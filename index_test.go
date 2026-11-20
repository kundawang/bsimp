package main

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingStore wraps an S3Storage and counts object reads per public path so
// tests can prove that unchanged objects are never re-fetched.
type countingStore struct {
	*S3Storage
	mu     sync.Mutex
	ranges map[string]int
	gets   int64
}

func newCountingStore(t *testing.T) (*countingStore, func()) {
	t.Helper()
	cfg, closeS3 := newTestS3Config()
	store := &countingStore{
		S3Storage: NewS3Storage(cfg),
		ranges:    map[string]int{},
	}
	_, err := store.s3.CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: aws.String("test")})
	require.NoError(t, err)
	return store, closeS3
}

func (s *countingStore) Walk(ctx context.Context, p string, keep func(*ObjectMeta) bool, fn func(*ObjectMeta) error) error {
	return s.S3Storage.Walk(ctx, p, keep, fn)
}

func (s *countingStore) ReadRange(ctx context.Context, p string, offset, length int64) ([]byte, error) {
	atomic.AddInt64(&s.gets, 1)
	s.mu.Lock()
	s.ranges[p]++
	s.mu.Unlock()
	return s.S3Storage.ReadRange(ctx, p, offset, length)
}

func (s *countingStore) Count(p string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ranges[p]
}

func (s *countingStore) Total() int64 { return atomic.LoadInt64(&s.gets) }

func (s *countingStore) put(t *testing.T, key string, body []byte) {
	t.Helper()
	_, err := s.s3.PutObject(context.Background(), &s3.PutObjectInput{
		Body:   bytes.NewReader(body),
		Bucket: aws.String("test"),
		Key:    aws.String(key),
	})
	require.NoError(t, err)
}

func indexTestConfig() IndexConfig {
	return IndexConfig{
		MaxObjects:      1000,
		Concurrency:     4,
		TagTimeout:      5 * time.Second,
		ScanTimeout:     30 * time.Second,
		RefreshInterval: time.Hour,
		Limits:          TagReadLimits{MaxBytes: 1 << 20, MaxRequests: 64, ChunkSize: 1024},
	}
}

func TestTagIndex_GroupsAlbumsAndFallsBack(t *testing.T) {
	store, closeS3 := newCountingStore(t)
	defer closeS3()

	ctx := context.Background()
	store.put(t, "music/Aphex Twin/scrambled-a.mp3", readFixture(t, "mp3-v23.mp3"))
	store.put(t, "music/Aphex Twin/scrambled-b.mp3", readFixture(t, "mp3-v24.mp3"))
	store.put(t, "music/untagged/02 - Second Song.mp3", readFixture(t, "untagged.mp3"))
	store.put(t, "music/untagged/01 - First Song.mp3", readFixture(t, "untagged.mp3"))
	store.put(t, "music/untagged/notes.txt", []byte("not audio"))
	store.put(t, "music/Bjork/homogenic.flac", readFixture(t, "flac.flac"))

	ix := NewTagIndex(store, indexTestConfig())
	stats, err := ix.Refresh(ctx)
	require.NoError(t, err)
	assert.Equal(t, 5, stats.Scanned)
	assert.Equal(t, 5, stats.TagReads)
	assert.Equal(t, 3, stats.Tagged)
	assert.Equal(t, 2, stats.MissingTags)
	assert.False(t, stats.Truncated)

	albums, _, ready := ix.Snapshot()
	require.True(t, ready)
	require.Len(t, albums, 3)

	byTitle := map[string]*Album{}
	for _, a := range albums {
		byTitle[a.Title] = a
	}

	saw := byTitle["Selected Ambient Works 85-92"]
	require.NotNil(t, saw, "both files must group under the tagged album despite different filenames")
	assert.Equal(t, "Aphex Twin", saw.Artist)
	require.Len(t, saw.Tracks, 2)
	// Same tag track number (1) in both files: tie-break must be stable.
	assert.Equal(t, "Xtal", saw.Tracks[0].Title)
	assert.Equal(t, "scrambled-a.mp3", baseName(saw.Tracks[0].Path))
	assert.Equal(t, "scrambled-b.mp3", baseName(saw.Tracks[1].Path))

	homogenic := byTitle["Homogenic"]
	require.NotNil(t, homogenic)
	assert.Equal(t, "Björk", homogenic.Artist)

	untagged := byTitle["untagged"]
	require.NotNil(t, untagged, "untagged files group by directory")
	require.Len(t, untagged.Tracks, 2)
	// Filenames supply track numbers, so ordering is deterministic.
	assert.Equal(t, "01 - First Song", untagged.Tracks[0].Title)
	assert.Equal(t, 1, untagged.Tracks[0].TrackNumber)
	assert.Equal(t, "02 - Second Song", untagged.Tracks[1].Title)
	assert.Equal(t, 2, untagged.Tracks[1].TrackNumber)
	assert.Equal(t, TagStatusMissing, untagged.Tracks[0].TagStatus)
}

func TestTagIndex_SameTitleDifferentAlbums(t *testing.T) {
	store, closeS3 := newCountingStore(t)
	defer closeS3()
	ctx := context.Background()

	store.put(t, "a/one.mp3", readFixture(t, "mp3-v23.mp3"))
	store.put(t, "b/two.mp3", readFixture(t, "flac.flac"))
	ix := NewTagIndex(store, indexTestConfig())
	_, err := ix.Refresh(ctx)
	require.NoError(t, err)

	res := ix.Search("homogenic", 10)
	require.Len(t, res.Albums, 1)
	assert.Equal(t, "Björk", res.Albums[0].Artist)

	res = ix.Search("selected ambient", 10)
	require.Len(t, res.Albums, 1)
	assert.Equal(t, "Aphex Twin", res.Albums[0].Artist)

	// Two files with the same album title and different artists must not merge.
	store.put(t, "c/three.mp3", readFixture(t, "m4a.m4a"))
	ix2 := NewTagIndex(store, indexTestConfig())
	_, err = ix2.Refresh(ctx)
	require.NoError(t, err)
	// No two albums should share an ID.
	albums, _, _ := ix2.Snapshot()
	seen := map[string]bool{}
	for _, a := range albums {
		assert.False(t, seen[a.ID], "duplicate album id %s", a.ID)
		seen[a.ID] = true
	}
}

func TestTagIndex_SameAlbumTitleDifferentArtistsDoNotMerge(t *testing.T) {
	// Two tagged objects with identical album titles but different artists
	// must stay separate albums.
	m1 := &ObjectMeta{Path: "x/1.mp3", Size: 1}
	m2 := &ObjectMeta{Path: "y/2.mp3", Size: 1}
	t1 := newTrackFromInfo(m1, TagInfo{Title: "Song A", Artist: "Artist One", Album: "Greatest Hits"})
	t2 := newTrackFromInfo(m2, TagInfo{Title: "Song B", Artist: "Artist Two", Album: "Greatest Hits"})
	assert.NotEqual(t, t1.albumKey, t2.albumKey)

	// Same album title and artist, different directories, must merge.
	m3 := &ObjectMeta{Path: "z/3.mp3", Size: 1}
	t3 := newTrackFromInfo(m3, TagInfo{Title: "Song C", Artist: "Artist One", Album: "Greatest Hits"})
	assert.Equal(t, t1.albumKey, t3.albumKey)

	albums, _ := buildAlbums(map[string]*Track{t1.Path: t1, t2.Path: t2, t3.Path: t3})
	require.Len(t, albums, 2)
}

func TestTagIndex_IncrementalScan(t *testing.T) {
	store, closeS3 := newCountingStore(t)
	defer closeS3()
	ctx := context.Background()

	store.put(t, "lib/album/a.mp3", readFixture(t, "mp3-v23.mp3"))
	store.put(t, "lib/album/b.mp3", readFixture(t, "flac.flac"))
	ix := NewTagIndex(store, indexTestConfig())

	first, err := ix.Refresh(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, first.TagReads)
	assert.Equal(t, 0, first.Unchanged)

	// Nothing changed: no object should be fetched from S3.
	before := store.Total()
	second, err := ix.Refresh(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, second.TagReads)
	assert.Equal(t, 2, second.Unchanged)
	assert.Equal(t, before, store.Total(), "unchanged objects must not be re-read")

	// Changing one object re-reads only that object.
	store.put(t, "lib/album/a.mp3", readFixture(t, "m4a.m4a"))
	before = store.Total()
	third, err := ix.Refresh(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, third.TagReads)
	assert.Equal(t, 1, third.Unchanged)
	assert.Greater(t, store.Total(), before)

	// Deleting an object removes it from the next snapshot.
	_, err = store.s3.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("test"), Key: aws.String("lib/album/b.mp3")})
	require.NoError(t, err)
	fourth, err := ix.Refresh(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, fourth.Removed)
	albums, _, _ := ix.Snapshot()
	require.Len(t, albums, 1)
	assert.Equal(t, "Ágætis byrjun", albums[0].Title)
}

func TestTagIndex_CorruptAndOversizedTagsFallBack(t *testing.T) {
	store, closeS3 := newCountingStore(t)
	defer closeS3()
	ctx := context.Background()

	store.put(t, "bad/01 - Broken.mp3", readFixture(t, "corrupt.mp3"))
	store.put(t, "bad/02 - Oversized.mp3", oversizedTagObject(64<<10))
	cfg := indexTestConfig()
	cfg.Limits.MaxBytes = 4 << 10
	ix := NewTagIndex(store, cfg)
	stats, err := ix.Refresh(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.CorruptTags)
	assert.Equal(t, 1, stats.TooLargeTags)

	albums, _, _ := ix.Snapshot()
	require.Len(t, albums, 1)
	require.Len(t, albums[0].Tracks, 2)
	assert.Equal(t, "01 - Broken", albums[0].Tracks[0].Title)
	assert.Equal(t, 1, albums[0].Tracks[0].TrackNumber)
	assert.Equal(t, TagStatusCorrupt, albums[0].Tracks[0].TagStatus)
	assert.NotEmpty(t, albums[0].Tracks[0].TagError)
	assert.Equal(t, TagStatusTooLarge, albums[0].Tracks[1].TagStatus)
}

func TestTagIndex_ObjectLimitTruncates(t *testing.T) {
	store, closeS3 := newCountingStore(t)
	defer closeS3()
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		store.put(t, fmt.Sprintf("lib/album/%02d.mp3", i), readFixture(t, "mp3-v23.mp3"))
	}
	cfg := indexTestConfig()
	cfg.MaxObjects = 4
	ix := NewTagIndex(store, cfg)
	stats, err := ix.Refresh(ctx)
	require.NoError(t, err)
	assert.True(t, stats.Truncated)
	assert.NotEmpty(t, stats.TruncatedReason)
	assert.Equal(t, 4, stats.Scanned)

	albums, _, _ := ix.Snapshot()
	require.Len(t, albums, 1)
	require.Len(t, albums[0].Tracks, 4, "the snapshot is bounded, not silently partial")
	// Lexical order means the first four objects are kept deterministically.
	assert.Equal(t, "lib/album/00.mp3", albums[0].Tracks[0].Path)
}

func TestTagIndex_SearchUnicodeAndCase(t *testing.T) {
	store, closeS3 := newCountingStore(t)
	defer closeS3()
	ctx := context.Background()

	store.put(t, "lib/vorbis/track.ogg", readFixture(t, "ogg-vorbis.ogg"))
	store.put(t, "lib/opus/track.opus", readFixture(t, "ogg-opus.opus"))
	store.put(t, "lib/flac/track.flac", readFixture(t, "flac.flac"))
	ix := NewTagIndex(store, indexTestConfig())
	_, err := ix.Refresh(ctx)
	require.NoError(t, err)

	testCases := []struct {
		query   string
		albums  []string
		trackOf string
	}{
		{query: "психохирурги", albums: []string{"Психохирурги"}, trackOf: "Вечная мерзлота"},
		{query: "ПСИХОХИРУРГИ", albums: []string{"Психохирурги"}, trackOf: "Вечная мерзлота"},
		{query: "坂本", albums: []string{"async"}, trackOf: "andata"},
		{query: "björk", albums: []string{"Homogenic"}, trackOf: "Hunter"},
		{query: "BJORK", albums: nil}, // Ö is not an O: folded, not deaccented.
		{query: "sigur", albums: nil},
	}
	for _, tc := range testCases {
		t.Run(tc.query, func(t *testing.T) {
			res := ix.Search(tc.query, 10)
			var got []string
			for _, a := range res.Albums {
				got = append(got, a.Title)
			}
			assert.ElementsMatch(t, tc.albums, got)
			if tc.trackOf != "" {
				require.NotEmpty(t, res.Tracks)
				assert.Equal(t, tc.trackOf, res.Tracks[0].Title)
			}
		})
	}

	// A composed vs decomposed query must match the same way.
	composed := ix.Search("Björk", 10)
	decomposed := ix.Search("Björk", 10)
	assert.Equal(t, composed.TotalAlbums, decomposed.TotalAlbums)
	assert.Equal(t, 1, composed.TotalAlbums)
}

func TestTagIndex_SearchLimits(t *testing.T) {
	store, closeS3 := newCountingStore(t)
	defer closeS3()
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		store.put(t, fmt.Sprintf("lib/album/%02d.mp3", i), readFixture(t, "mp3-v23.mp3"))
	}
	ix := NewTagIndex(store, indexTestConfig())
	_, err := ix.Refresh(ctx)
	require.NoError(t, err)

	res := ix.Search("", 2)
	assert.Len(t, res.Albums, 1)
	assert.Len(t, res.Tracks, 2)
	assert.Equal(t, 5, res.TotalTracks)
	assert.True(t, res.Truncated)
	assert.Equal(t, 2, res.SearchLimit)

	res = ix.Search("", -1)
	assert.Equal(t, defaultAlbumSearchLimit, res.SearchLimit)
	res = ix.Search("", 1_000_000)
	assert.Equal(t, maxAlbumSearchLimit, res.SearchLimit)
}

func TestTagIndex_ScanFailureKeepsSnapshot(t *testing.T) {
	store, closeS3 := newCountingStore(t)
	defer closeS3()
	ctx := context.Background()
	store.put(t, "lib/album/a.mp3", readFixture(t, "mp3-v23.mp3"))
	ix := NewTagIndex(store, indexTestConfig())
	_, err := ix.Refresh(ctx)
	require.NoError(t, err)

	// Point the store at a bucket that does not exist.
	store.cfg.Bucket = "missing"
	_, err = ix.Refresh(ctx)
	require.Error(t, err)

	albums, _, ready := ix.Snapshot()
	assert.True(t, ready)
	require.Len(t, albums, 1)
	res := ix.Search("xtal", 10)
	assert.Equal(t, 1, res.TotalTracks)
	assert.NotEmpty(t, res.IndexFailure)
}

func TestTagIndex_RefreshTimeout(t *testing.T) {
	store, closeS3 := newCountingStore(t)
	defer closeS3()
	store.put(t, "lib/album/a.mp3", readFixture(t, "mp3-v23.mp3"))

	cfg := indexTestConfig()
	cfg.ScanTimeout = time.Nanosecond
	ix := NewTagIndex(store, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	_, err := ix.Refresh(ctx)
	require.Error(t, err)
	_, ready := ix.Stats()
	assert.False(t, ready)
}

func TestTagIndex_TrackLookupUsesIndex(t *testing.T) {
	store, closeS3 := newCountingStore(t)
	defer closeS3()
	store.put(t, "lib/album/a.mp3", readFixture(t, "mp3-v23.mp3"))
	ix := NewTagIndex(store, indexTestConfig())
	_, err := ix.Refresh(context.Background())
	require.NoError(t, err)

	tr, ok := ix.Track("lib/album/a.mp3")
	require.True(t, ok)
	assert.Equal(t, "Xtal", tr.Title)
	assert.Equal(t, "lib/album/a.mp3", tr.Path)
	assert.Equal(t, "lib/album/a.mp3", tr.URLPath())

	album, ok := ix.Album(albumID(tr.albumKey))
	require.True(t, ok)
	assert.Equal(t, "Selected Ambient Works 85-92", album.Title)
}

func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

// oversizedTagObject builds a syntactically valid MP3 ID3v2.3 object whose
// first frame claims a body far larger than the read budget, so a bounded
// reader must give up rather than buffer it.
func oversizedTagObject(size int) []byte {
	b := make([]byte, size)
	copy(b, []byte("ID3\x03\x00\x00"))
	// Syncsafe tag size covering the rest of the object.
	n := size - 10
	b[6] = byte((n >> 21) & 0x7f)
	b[7] = byte((n >> 14) & 0x7f)
	b[8] = byte((n >> 7) & 0x7f)
	b[9] = byte(n & 0x7f)
	// Frame header at offset 10: ID "APIC", size 32-bit big-endian, flags.
	copy(b[10:14], []byte("APIC"))
	frameSize := size - 20
	b[14] = byte(frameSize >> 24)
	b[15] = byte(frameSize >> 16)
	b[16] = byte(frameSize >> 8)
	b[17] = byte(frameSize)
	return b
}

func TestTagIndex_PaginatedListing(t *testing.T) {
	store, closeS3 := newCountingStore(t)
	defer closeS3()
	ctx := context.Background()

	// gofakes3 caps ListObjectsV2 pages at 1000 keys. Crossing that boundary
	// proves the walk follows continuation tokens instead of stopping at page
	// one.
	const total = 1005
	for i := 0; i < total; i++ {
		store.put(t, fmt.Sprintf("lib/album/%04d.mp3", i), readFixture(t, "mp3-v23.mp3"))
	}

	cfg := indexTestConfig()
	cfg.MaxObjects = -1 // no cap
	ix := NewTagIndex(store, cfg)
	stats, err := ix.Refresh(ctx)
	require.NoError(t, err)
	assert.Equal(t, total, stats.Scanned)
	assert.False(t, stats.Truncated)
	albums, _, _ := ix.Snapshot()
	require.Len(t, albums, 1)
	assert.Len(t, albums[0].Tracks, total)
}

func TestTagIndex_ScatteredAlbumAcrossDirectories(t *testing.T) {
	store, closeS3 := newCountingStore(t)
	defer closeS3()
	ctx := context.Background()

	// Same tagged album, files scattered across unrelated directories and with
	// arbitrary names. Tag indexing must still group them.
	store.put(t, "misc/track-zzz.mp3", readFixture(t, "mp3-v23.mp3"))
	store.put(t, "other/aaaa.mp3", readFixture(t, "mp3-v24.mp3"))
	store.put(t, "loose.mp3", readFixture(t, "mp3-v23.mp3"))

	ix := NewTagIndex(store, indexTestConfig())
	stats, err := ix.Refresh(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, stats.Tagged)

	res := ix.Search("selected ambient", 10)
	require.Len(t, res.Albums, 1)
	assert.Len(t, res.Albums[0].Tracks, 3)
}

func TestTagIndex_DirectoryListingOrder(t *testing.T) {
	store, closeS3 := newCountingStore(t)
	defer closeS3()
	ctx := context.Background()

	// Filenames sort in a deliberately wrong order; tag track numbers do not.
	store.put(t, "Album/z-last.mp3", readFixture(t, "mp3-v23.mp3"))
	store.put(t, "Album/a-first.mp3", readFixture(t, "mp3-v24.mp3"))

	ml := NewMediaLibrary(store.S3Storage)
	ix := NewTagIndex(store, indexTestConfig())
	_, err := ix.Refresh(ctx)
	require.NoError(t, err)
	ml.SetIndex(ix)

	listing, err := ml.List(ctx, "Album")
	require.NoError(t, err)
	require.Len(t, listing.AudioTracks, 2)
	// Both fixtures carry track number 1; ordering then falls back to title
	// (both "Xtal") and finally path, which is deterministic.
	assert.Equal(t, "Album/a-first.mp3", listing.AudioTracks[0].Path())
	assert.Equal(t, "Album/z-last.mp3", listing.AudioTracks[1].Path())
	assert.Equal(t, "Xtal", listing.DisplayName(listing.AudioTracks[0]))
}

func TestTagIndex_UnicodeFilenamesFallback(t *testing.T) {
	store, closeS3 := newCountingStore(t)
	defer closeS3()
	ctx := context.Background()

	// No tags; the directory and filename carry non-ASCII text. Grouping uses
	// the directory, and search must match decomposed input.
	store.put(t, "Психохирурги/Вечная мерзлота.mp3", readFixture(t, "untagged.mp3"))
	ix := NewTagIndex(store, indexTestConfig())
	_, err := ix.Refresh(ctx)
	require.NoError(t, err)

	res := ix.Search("психохирурги", 10)
	require.Len(t, res.Albums, 1)
	assert.Equal(t, "Психохирурги", res.Albums[0].Title)

	tracks, _, _ := ix.Snapshot()
	require.Len(t, tracks, 1)
	assert.Equal(t, "Вечная мерзлота", tracks[0].Tracks[0].Title)

	// Stream URL escapes the Cyrillic path exactly as before.
	assert.Contains(t, tracks[0].Tracks[0].URLPath(), "%D0%9F")
}

func TestTagIndex_TransientErrorsAreRetried(t *testing.T) {
	base, closeS3 := newCountingStore(t)
	defer closeS3()
	base.put(t, "lib/album/a.mp3", readFixture(t, "mp3-v23.mp3"))

	failing := &flakyStore{ObjectStore: base, failReads: true}
	ix := NewTagIndex(failing, indexTestConfig())
	stats, err := ix.Refresh(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Timeouts)
	albums, _, _ := ix.Snapshot()
	require.Len(t, albums, 1)
	assert.Equal(t, TagStatusTimeout, albums[0].Tracks[0].TagStatus)

	// A failed read must not be cached; the next scan retries and succeeds.
	failing.failReads = false
	stats, err = ix.Refresh(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, stats.TagReads)
	assert.Equal(t, 1, stats.Tagged)
}

type flakyStore struct {
	ObjectStore
	failReads bool
}

func (s *flakyStore) ReadRange(ctx context.Context, p string, offset, length int64) ([]byte, error) {
	if s.failReads {
		return nil, context.DeadlineExceeded
	}
	return s.ObjectStore.ReadRange(ctx, p, offset, length)
}

func TestNewTrackFromInfo_Fallbacks(t *testing.T) {
	testCases := []struct {
		name   string
		path   string
		info   TagInfo
		title  string
		artist string
		album  string
		track  int
		disk   int
	}{
		{
			name:  "full tags",
			path:  "Artist/Album/whatever.mp3",
			info:  TagInfo{Title: "Song", Artist: "Artist", Album: "Album", Track: 3},
			title: "Song", artist: "Artist", album: "Album", track: 3,
		},
		{
			name:  "no tags uses file and dir",
			path:  "Parent/Album/07. Song Name.mp3",
			info:  TagInfo{},
			title: "07. Song Name", artist: "Parent", album: "Album", track: 7,
		},
		{
			name:  "album tag but no artist",
			path:  "Artist/Album/song.mp3",
			info:  TagInfo{Title: "Song", Album: "Tagged Album"},
			title: "Song", artist: "Artist", album: "Tagged Album",
		},
		{
			name:  "disc-track filename",
			path:  "x/y/1-02 Song.mp3",
			info:  TagInfo{},
			title: "1-02 Song", artist: "x", album: "y", track: 2, disk: 1,
		},
		{
			name:  "root file",
			path:  "Song.mp3",
			info:  TagInfo{},
			title: "Song", artist: "Unknown Artist", album: "Unknown Album",
		},
		{
			name:  "track number without separator is not a track",
			path:  "a/b/2001.mp3",
			info:  TagInfo{},
			title: "2001", artist: "a", album: "b", track: 0,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTrackFromInfo(&ObjectMeta{Path: tc.path, Size: 1}, tc.info)
			assert.Equal(t, tc.title, tr.Title)
			assert.Equal(t, tc.artist, tr.Artist)
			assert.Equal(t, tc.album, tr.Album)
			assert.Equal(t, tc.track, tr.TrackNumber)
			assert.Equal(t, tc.disk, tr.Disc)
		})
	}
}

func TestAlbumGrouping_CompilationViaAlbumArtist(t *testing.T) {
	// A compilation keeps one album even though each track has a different
	// ARTIST, because ALBUMARTIST is shared. This is the standard way to tag
	// multi-artist albums.
	t1 := newTrackFromInfo(&ObjectMeta{Path: "v/1.mp3"}, TagInfo{
		Title: "A", Artist: "Artist One", Album: "Mix", AlbumArtist: "Various Artists",
	})
	t2 := newTrackFromInfo(&ObjectMeta{Path: "v/2.mp3"}, TagInfo{
		Title: "B", Artist: "Artist Two", Album: "Mix", AlbumArtist: "Various Artists",
	})
	assert.Equal(t, t1.albumKey, t2.albumKey)

	albums, _ := buildAlbums(map[string]*Track{t1.Path: t1, t2.Path: t2})
	require.Len(t, albums, 1)
	assert.Equal(t, "Various Artists", albums[0].Artist)
	assert.Len(t, albums[0].Tracks, 2)

	// Without ALBUMARTIST, per-track ARTIST drives grouping, which splits a
	// multi-artist album. Documenting this makes the limitation explicit.
	t3 := newTrackFromInfo(&ObjectMeta{Path: "w/1.mp3"}, TagInfo{Title: "A", Artist: "Artist One", Album: "Mix"})
	t4 := newTrackFromInfo(&ObjectMeta{Path: "w/2.mp3"}, TagInfo{Title: "B", Artist: "Artist Two", Album: "Mix"})
	assert.NotEqual(t, t3.albumKey, t4.albumKey)
}

func TestTagIndex_CaseInsensitiveAlbumGrouping(t *testing.T) {
	// Tag text differing only by case or Unicode composition must group.
	t1 := newTrackFromInfo(&ObjectMeta{Path: "a/1.mp3"}, TagInfo{Title: "A", Artist: "Björk", Album: "Homogenic"})
	t2 := newTrackFromInfo(&ObjectMeta{Path: "b/2.mp3"}, TagInfo{Title: "B", Artist: "BJÖRK", Album: "homogenic"})
	assert.Equal(t, t1.albumKey, t2.albumKey)

	albums, _ := buildAlbums(map[string]*Track{t1.Path: t1, t2.Path: t2})
	require.Len(t, albums, 1)
}

func TestMediaLibrary_ListOrdering(t *testing.T) {
	ctx := context.Background()

	t.Run("mixed albums keep storage order", func(t *testing.T) {
		store, closeS3 := newCountingStore(t)
		defer closeS3()
		// Two different albums in one directory.
		store.put(t, "Mixed/01.mp3", readFixture(t, "mp3-v23.mp3"))
		store.put(t, "Mixed/02.mp3", readFixture(t, "flac.flac"))

		ml := NewMediaLibrary(store.S3Storage)
		ix := NewTagIndex(store, indexTestConfig())
		_, err := ix.Refresh(ctx)
		require.NoError(t, err)
		ml.SetIndex(ix)

		listing, err := ml.List(ctx, "Mixed")
		require.NoError(t, err)
		require.Len(t, listing.AudioTracks, 2)
		// Storage order is lexical: 01 then 02.
		assert.Equal(t, "Mixed/01.mp3", listing.AudioTracks[0].Path())
		assert.Equal(t, "Mixed/02.mp3", listing.AudioTracks[1].Path())
	})

	t.Run("single album sorted by track number", func(t *testing.T) {
		// Filenames reversed relative to tag track numbers within one album.
		t1 := newTrackFromInfo(&ObjectMeta{Path: "Album/02-second.mp3", Size: 1},
			TagInfo{Title: "Second", Artist: "A", Album: "Same", Track: 2})
		t2 := newTrackFromInfo(&ObjectMeta{Path: "Album/01-first.mp3", Size: 1},
			TagInfo{Title: "First", Artist: "A", Album: "Same", Track: 1})
		indexed := map[string]*Track{t1.Path: t1, t2.Path: t2}
		files := []*StorageFile{
			NewStorageFile("Album/02-second.mp3", 1),
			NewStorageFile("Album/01-first.mp3", 1),
		}
		sortTracksByIndex(indexed, files)
		assert.Equal(t, "Album/01-first.mp3", files[0].Path())
		assert.Equal(t, "Album/02-second.mp3", files[1].Path())
	})
}
