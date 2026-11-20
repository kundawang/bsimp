package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// IndexConfig controls a TagIndex scan. Zero values select conservative
// defaults; a negative MaxObjects means "no object limit".
type IndexConfig struct {
	// MaxObjects caps the number of audio objects indexed per snapshot.
	// The scan stops at the limit and reports a truncated index.
	MaxObjects int
	// Concurrency caps the number of concurrent tag reads.
	Concurrency int
	// TagTimeout caps how long a single object's tags may take to read.
	TagTimeout time.Duration
	// ScanTimeout caps a whole index scan.
	ScanTimeout time.Duration
	// RefreshInterval is how long a snapshot is considered fresh.
	RefreshInterval time.Duration
	// Limits bounds the bytes and requests spent reading one object's tags.
	Limits TagReadLimits
}

const (
	defaultMaxObjects  = 50000
	defaultConcurrency = 8
	defaultTagTimeout  = 10 * time.Second
	defaultScanTimeout = 15 * time.Minute

	defaultRefreshAfter = 5 * time.Minute

	defaultAlbumSearchLimit = 100
	maxAlbumSearchLimit     = 500
)

func (cfg IndexConfig) withDefaults() IndexConfig {
	if cfg.MaxObjects == 0 {
		cfg.MaxObjects = defaultMaxObjects
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = defaultConcurrency
	}
	if cfg.TagTimeout <= 0 {
		cfg.TagTimeout = defaultTagTimeout
	}
	if cfg.ScanTimeout <= 0 {
		cfg.ScanTimeout = defaultScanTimeout
	}
	// A zero value means "use the default cadence". Negative means "only
	// refresh on demand", so it is clamped to zero, which Fresh reports as
	// always current.
	if cfg.RefreshInterval == 0 {
		cfg.RefreshInterval = defaultRefreshAfter
	}
	if cfg.RefreshInterval < 0 {
		cfg.RefreshInterval = 0
	}
	if cfg.Limits.MaxBytes == 0 {
		cfg.Limits.MaxBytes = 1 << 20 // 1 MiB of tag data per object.
	}
	if cfg.Limits.MaxRequests == 0 {
		cfg.Limits.MaxRequests = 64
	}
	return cfg
}

// Track is one indexed audio object. Tag fields are already resolved: when a
// tag is absent, values fall back to the filename and directory structure so
// every track remains browsable.
type Track struct {
	Path        string
	Size        int64
	Title       string
	Artist      string
	Album       string
	AlbumArtist string
	TrackNumber int
	TrackTotal  int
	Disc        int
	DiscTotal   int

	// TagStatus is TagStatusOK when the title/artist/album came from tags. For
	// other statuses the fields above are fallbacks and TagError explains why.
	TagStatus TagStatus
	TagError  string

	albumKey   string
	searchText string
}

// URLPath returns the URL-escaped path suitable for embedding in templates.
func (t *Track) URLPath() string {
	return (&url.URL{Path: t.Path}).EscapedPath()
}

// HasTags reports whether usable text tags were read for the track.
func (t *Track) HasTags() bool { return t.TagStatus == TagStatusOK }

// Album is a group of tracks sharing an album title and album artist. Grouping
// by both means two different albums with the same title do not collapse.
type Album struct {
	ID     string
	Title  string
	Artist string
	Tracks []*Track

	albumKey   string
	searchText string
}

// IndexStats summarizes one refresh. It is safe to display to users: it makes
// partial results (truncation, unreadable tags, timeouts) explicit.
type IndexStats struct {
	Scanned      int
	Unchanged    int
	TagReads     int
	Removed      int
	Tagged       int
	MissingTags  int
	CorruptTags  int
	TooLargeTags int
	Timeouts     int
	ReadErrors   int

	// Truncated is true when MaxObjects stopped the scan. The snapshot still
	// contains the first MaxObjects audio objects in lexical key order.
	Truncated       bool
	TruncatedReason string

	StartedAt   time.Time
	CompletedAt time.Time
	Duration    time.Duration
}

// ProblemCount returns the number of tracks whose tags could not be indexed
// cleanly. Fallbacks still apply to those tracks.
func (s IndexStats) ProblemCount() int {
	return s.MissingTags + s.CorruptTags + s.TooLargeTags + s.Timeouts + s.ReadErrors
}

// SearchResult contains limited matches plus the total match counts.
type SearchResult struct {
	Query        string
	Albums       []*Album
	Tracks       []*Track
	TotalAlbums  int
	TotalTracks  int
	Truncated    bool
	SearchLimit  int
	IndexStats   IndexStats
	IndexReady   bool
	IndexFailure string
}

type cachedTags struct {
	size         int64
	etag         string
	lastModified time.Time
	track        *Track
}

// ObjectStore is the subset of the storage used to build the index. Keeping it
// small makes the index testable and lets callers wrap the S3 client.
type ObjectStore interface {
	Walk(ctx context.Context, p string, keep func(*ObjectMeta) bool, fn func(*ObjectMeta) error) error
	ReadRange(ctx context.Context, p string, offset, length int64) ([]byte, error)
}

// TagIndex keeps an in-memory tag index of the audio library. It never stores
// anything in the bucket. Refresh is incremental: an object is re-read only
// when its ETag, size or (when no ETag is available) last-modified time changes.
type TagIndex struct {
	store  ObjectStore
	cfg    IndexConfig
	limits TagReadLimits

	mu           sync.RWMutex
	tracks       map[string]*Track
	sortedTracks []*Track
	albums       []*Album
	byID         map[string]*Album
	cache        map[string]*cachedTags
	stats        IndexStats
	lastErr      string
	ready        bool

	refreshMu sync.Mutex
}

func NewTagIndex(store ObjectStore, cfg IndexConfig) *TagIndex {
	cfg = cfg.withDefaults()
	return &TagIndex{
		store:  store,
		cfg:    cfg,
		limits: cfg.Limits,
		tracks: map[string]*Track{},
		byID:   map[string]*Album{},
		cache:  map[string]*cachedTags{},
	}
}

type tagTaskResult struct {
	track     *Track
	status    TagStatus
	cacheable bool
}

// Refresh scans the bucket and replaces the snapshot. If the scan itself fails
// (listing error or overall deadline), the previous snapshot is kept intact.
func (ix *TagIndex) Refresh(ctx context.Context) (IndexStats, error) {
	ix.refreshMu.Lock()
	defer ix.refreshMu.Unlock()

	started := time.Now()
	stats := IndexStats{StartedAt: started}

	scanCtx := ctx
	var cancel context.CancelFunc
	if ix.cfg.ScanTimeout > 0 {
		scanCtx, cancel = context.WithTimeout(ctx, ix.cfg.ScanTimeout)
		defer cancel()
	}

	var objects []*ObjectMeta
	unchanged := map[string]*Track{}
	err := ix.store.Walk(scanCtx, "", func(m *ObjectMeta) bool { return isAudioPath(m.Path) }, func(m *ObjectMeta) error {
		if ix.cfg.MaxObjects > 0 && len(objects) >= ix.cfg.MaxObjects {
			stats.Truncated = true
			stats.TruncatedReason = fmt.Sprintf("object limit of %d audio files reached", ix.cfg.MaxObjects)
			return errStopWalk
		}
		objects = append(objects, m)
		if track, ok := ix.cachedTrack(m); ok {
			unchanged[m.Path] = track
		}
		return nil
	})
	if errors.Is(err, errStopWalk) {
		err = nil
	}
	if err != nil {
		stats.CompletedAt = time.Now()
		stats.Duration = stats.CompletedAt.Sub(started)
		ix.mu.Lock()
		ix.lastErr = err.Error()
		ix.mu.Unlock()
		return stats, err
	}

	results := make([]*tagTaskResult, len(objects))

	// Use a fixed worker pool rather than one goroutine per object, so a very
	// large library does not create tens of thousands of blocked goroutines.
	type job struct {
		index int
		meta  *ObjectMeta
	}
	workers := ix.cfg.Concurrency
	if workers > len(objects) && len(objects) > 0 {
		workers = len(objects)
	}
	jobs := make(chan job)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				results[j.index] = ix.readTrack(scanCtx, j.meta)
			}
		}()
	}
	for i, meta := range objects {
		if track, ok := unchanged[meta.Path]; ok {
			results[i] = &tagTaskResult{track: track, status: track.TagStatus, cacheable: true}
			continue
		}
		select {
		case jobs <- job{index: i, meta: meta}:
		case <-scanCtx.Done():
			// Stop feeding the pool; the deadline is reported below.
		}
		if scanCtx.Err() != nil {
			break
		}
	}
	close(jobs)
	wg.Wait()

	if err := scanCtx.Err(); err != nil {
		stats.CompletedAt = time.Now()
		stats.Duration = stats.CompletedAt.Sub(started)
		stats.TruncatedReason = "scan interrupted"
		ix.mu.Lock()
		ix.lastErr = err.Error()
		ix.mu.Unlock()
		return stats, err
	}

	newTracks := make(map[string]*Track, len(objects))
	newCache := make(map[string]*cachedTags, len(objects))
	stats.Scanned = len(objects)
	for i, meta := range objects {
		res := results[i]
		if res == nil {
			stats.ReadErrors++
			continue
		}
		newTracks[meta.Path] = res.track
		if res.cacheable {
			newCache[meta.Path] = &cachedTags{
				size:         meta.Size,
				etag:         meta.ETag,
				lastModified: meta.LastModified,
				track:        res.track,
			}
		}
		if _, ok := unchanged[meta.Path]; ok {
			stats.Unchanged++
		} else {
			stats.TagReads++
		}
		switch res.status {
		case TagStatusOK:
			stats.Tagged++
		case TagStatusMissing:
			stats.MissingTags++
		case TagStatusCorrupt:
			stats.CorruptTags++
		case TagStatusTooLarge:
			stats.TooLargeTags++
		case TagStatusTimeout:
			stats.Timeouts++
		default:
			stats.ReadErrors++
		}
	}
	stats.Removed = len(ix.tracks)
	for p := range newTracks {
		if _, ok := ix.tracks[p]; ok {
			stats.Removed--
		}
	}

	albums, byID := buildAlbums(newTracks)
	sortedTracks := make([]*Track, 0, len(newTracks))
	for _, t := range newTracks {
		sortedTracks = append(sortedTracks, t)
	}
	sortTracks(sortedTracks)
	stats.CompletedAt = time.Now()
	stats.Duration = stats.CompletedAt.Sub(started)

	ix.mu.Lock()
	ix.tracks = newTracks
	ix.sortedTracks = sortedTracks
	ix.cache = newCache
	ix.albums = albums
	ix.byID = byID
	ix.stats = stats
	ix.ready = true
	ix.lastErr = ""
	ix.mu.Unlock()

	return stats, nil
}

// cachedTrack returns a previously indexed track when the object is unchanged.
func (ix *TagIndex) cachedTrack(meta *ObjectMeta) (*Track, bool) {
	ix.mu.RLock()
	cached, ok := ix.cache[meta.Path]
	ix.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return cached.track, cacheKeyMatches(cached, meta)
}

// cacheKeyMatches reports whether an object is unchanged since it was indexed.
// Preferring a non-empty ETag over timestamps avoids re-reading objects on
// stores whose LastModified changes spuriously.
func cacheKeyMatches(cached *cachedTags, meta *ObjectMeta) bool {
	if cached.size != meta.Size {
		return false
	}
	if cached.etag != "" && meta.ETag != "" {
		return cached.etag == meta.ETag
	}
	return cached.lastModified.Equal(meta.LastModified)
}

func (ix *TagIndex) readTrack(ctx context.Context, meta *ObjectMeta) *tagTaskResult {
	readCtx := ctx
	var cancel context.CancelFunc
	if ix.cfg.TagTimeout > 0 {
		readCtx, cancel = context.WithTimeout(ctx, ix.cfg.TagTimeout)
		defer cancel()
	}

	fetch := func(ctx context.Context, start, length int64) ([]byte, error) {
		return ix.store.ReadRange(ctx, meta.Path, start, length)
	}
	info, status, err := readTags(readCtx, meta.Size, fetch, ix.limits)

	track := newTrackFromInfo(meta, info)
	track.TagStatus = status
	res := &tagTaskResult{
		track:     track,
		status:    status,
		cacheable: true,
	}
	if err != nil {
		track.TagError = err.Error()
		switch status {
		case TagStatusTimeout, TagStatusReadError:
			// Transient: do not cache, so the next refresh retries.
			res.cacheable = false
		}
	} else {
		track.TagError = ""
	}
	return res
}

// Snapshot returns the current album list and statistics. The returned values
// must not be modified by callers.
func (ix *TagIndex) Snapshot() ([]*Album, IndexStats, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.albums, ix.stats, ix.ready
}

// Stats returns the last scan statistics and whether a snapshot is available.
func (ix *TagIndex) Stats() (IndexStats, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.stats, ix.ready
}

// LastError returns the error from the most recent failed scan, if any. A
// successful scan clears it.
func (ix *TagIndex) LastError() string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.lastErr
}

// Album returns the album with the given ID, if it exists.
func (ix *TagIndex) Album(id string) (*Album, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	a, ok := ix.byID[id]
	return a, ok
}

// Track returns the indexed track for the given public path.
func (ix *TagIndex) Track(p string) (*Track, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	t, ok := ix.tracks[p]
	return t, ok
}

// Fresh reports whether a snapshot exists and is younger than RefreshInterval.
func (ix *TagIndex) Fresh() bool {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if !ix.ready {
		return false
	}
	if ix.cfg.RefreshInterval <= 0 {
		return true
	}
	return time.Since(ix.stats.CompletedAt) < ix.cfg.RefreshInterval
}

// Ready reports whether at least one successful scan has completed.
func (ix *TagIndex) Ready() bool {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.ready
}

// Search matches albums and tracks. All whitespace-separated query terms must
// appear somewhere in the folded Unicode text of an item. Empty queries match
// everything. Limits are clamped to [1, maxAlbumSearchLimit].
func (ix *TagIndex) Search(query string, limit int) SearchResult {
	if limit <= 0 {
		limit = defaultAlbumSearchLimit
	}
	if limit > maxAlbumSearchLimit {
		limit = maxAlbumSearchLimit
	}
	terms := foldTerms(query)

	ix.mu.RLock()
	albums := ix.albums
	allTracks := ix.sortedTracks
	stats, ready := ix.stats, ix.ready
	lastErr := ix.lastErr
	ix.mu.RUnlock()

	res := SearchResult{
		Query:        query,
		SearchLimit:  limit,
		IndexStats:   stats,
		IndexReady:   ready,
		IndexFailure: lastErr,
	}
	for _, a := range albums {
		if !matchesAll(a.searchText, terms) {
			continue
		}
		res.TotalAlbums++
		if len(res.Albums) < limit {
			res.Albums = append(res.Albums, a)
		}
	}
	for _, t := range allTracks {
		if !matchesAll(t.searchText, terms) {
			continue
		}
		res.TotalTracks++
		if len(res.Tracks) < limit {
			res.Tracks = append(res.Tracks, t)
		}
	}
	res.Truncated = res.TotalAlbums > len(res.Albums) || res.TotalTracks > len(res.Tracks)
	return res
}

func matchesAll(haystack string, terms []string) bool {
	for _, term := range terms {
		if !strings.Contains(haystack, term) {
			return false
		}
	}
	return true
}

func foldTerms(query string) []string {
	folded := foldText(query)
	if folded == "" {
		return nil
	}
	return strings.Fields(folded)
}

var foldCaser = cases.Fold()

// foldText normalizes Unicode (NFC) and case-folds it, so composed and
// decomposed forms and any letter case compare equal.
func foldText(s string) string {
	return foldCaser.String(norm.NFC.String(s))
}

func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// newTrackFromInfo resolves tag fields with filename/directory fallbacks.
func newTrackFromInfo(meta *ObjectMeta, info TagInfo) *Track {
	name := path.Base(meta.Path)
	stem, _ := splitNameExt(name)
	dir := path.Dir(meta.Path)
	dirName := ""
	if dir != "." && dir != "/" {
		dirName = path.Base(dir)
	}
	parentName := ""
	if dir != "." && dir != "/" {
		parent := path.Dir(dir)
		if parent != "." && parent != "/" {
			parentName = path.Base(parent)
		}
	}

	t := &Track{
		Path:        meta.Path,
		Size:        meta.Size,
		Title:       info.Title,
		Artist:      info.Artist,
		Album:       info.Album,
		AlbumArtist: info.AlbumArtist,
		TrackNumber: info.Track,
		TrackTotal:  info.TrackTotal,
		Disc:        info.Disc,
		DiscTotal:   info.DiscTotal,
		TagStatus:   TagStatusOK,
	}

	tagAlbum := t.Album != ""

	if t.Album == "" {
		t.Album = dirName
	}
	if t.Album == "" {
		t.Album = "Unknown Album"
	}
	if t.AlbumArtist == "" {
		if t.Artist != "" {
			t.AlbumArtist = t.Artist
		} else if parentName != "" {
			t.AlbumArtist = parentName
		} else {
			t.AlbumArtist = "Unknown Artist"
		}
	}
	if t.Artist == "" {
		t.Artist = t.AlbumArtist
	}
	if t.Title == "" {
		t.Title = stem
	}
	if t.Title == "" {
		t.Title = name
	}
	if t.TrackNumber == 0 {
		t.TrackNumber, t.Disc = parseFilenameTrack(stem)
	}

	if tagAlbum {
		// A tagged album is identified by its title and album artist. This
		// keeps "Greatest Hits" by two different artists in separate groups.
		t.albumKey = "tag\x00" + collapseSpace(foldText(t.Album)) + "\x00" + collapseSpace(foldText(t.AlbumArtist))
	} else {
		// Untagged tracks cannot be grouped reliably by name, so use the
		// containing directory.
		t.albumKey = "dir\x00" + collapseSpace(foldText(dir))
	}

	t.searchText = foldText(strings.Join([]string{
		t.Title, t.Artist, t.Album, t.AlbumArtist, stem, meta.Path,
	}, " "))
	return t
}

// parseFilenameTrack extracts a leading track number (and optional disc) from
// a filename stem such as "01. Xtal", "3 - Jericho" or "1-02 Song".
func parseFilenameTrack(stem string) (track, disc int) {
	s := strings.TrimSpace(stem)
	i := 0
	for i < len(s) && i < 3 && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, 0
	}
	first := atoiSafe(s[:i])
	if i < len(s) && s[i] == '-' {
		// Disc-track form, e.g. "1-02 Song".
		j := i + 1
		k := j
		for k < len(s) && k-j < 3 && s[k] >= '0' && s[k] <= '9' {
			k++
		}
		if k > j && (k == len(s) || isTrackSeparator(s[k])) {
			return atoiSafe(s[j:k]), first
		}
	}
	if i == len(s) || isTrackSeparator(s[i]) {
		return first, 0
	}
	return 0, 0
}

func isTrackSeparator(b byte) bool {
	switch b {
	case ' ', '\t', '.', '_', ')', '-':
		return true
	}
	return false
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func isAudioPath(p string) bool {
	_, ext := splitNameExt(strings.ToLower(path.Base(p)))
	return audioExtensions.Contains(ext)
}

func buildAlbums(tracks map[string]*Track) ([]*Album, map[string]*Album) {
	groups := map[string]*Album{}
	for _, t := range tracks {
		a, ok := groups[t.albumKey]
		if !ok {
			a = &Album{
				Title:    t.Album,
				albumKey: t.albumKey,
			}
			groups[t.albumKey] = a
		}
		a.Tracks = append(a.Tracks, t)
	}

	albums := make([]*Album, 0, len(groups))
	byID := make(map[string]*Album, len(groups))
	for key, a := range groups {
		a.Artist = albumDisplayArtist(a.Tracks)
		sortTracks(a.Tracks)
		a.ID = albumID(key)
		a.searchText = foldText(strings.Join([]string{a.Title, a.Artist}, " "))
		albums = append(albums, a)
		byID[a.ID] = a
	}

	sort.SliceStable(albums, func(i, j int) bool {
		ai, aj := foldText(albums[i].Artist), foldText(albums[j].Artist)
		if ai != aj {
			return ai < aj
		}
		ti, tj := foldText(albums[i].Title), foldText(albums[j].Title)
		if ti != tj {
			return ti < tj
		}
		return albums[i].ID < albums[j].ID
	})
	return albums, byID
}

func albumDisplayArtist(tracks []*Track) string {
	seen := map[string]struct{}{}
	for _, t := range tracks {
		seen[foldText(t.Artist)] = struct{}{}
	}
	if len(seen) == 1 {
		return tracks[0].Artist
	}
	// Prefer an explicit album-artist tag.
	for _, t := range tracks {
		if t.AlbumArtist != t.Artist && t.AlbumArtist != "" {
			return t.AlbumArtist
		}
	}
	return "Various Artists"
}

func sortTracks(tracks []*Track) {
	sort.SliceStable(tracks, func(i, j int) bool {
		a, b := tracks[i], tracks[j]
		if a.Disc != b.Disc {
			return a.Disc < b.Disc
		}
		// Numbered tracks sort ahead of unnumbered ones.
		if (a.TrackNumber == 0) != (b.TrackNumber == 0) {
			return a.TrackNumber != 0
		}
		if a.TrackNumber != b.TrackNumber {
			return a.TrackNumber < b.TrackNumber
		}
		at, bt := foldText(a.Title), foldText(b.Title)
		if at != bt {
			return at < bt
		}
		return a.Path < b.Path
	})
}

func albumID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:16])
}

// errStopWalk is used internally to end a Walk early without an error.
var errStopWalk = errors.New("stop walk")
