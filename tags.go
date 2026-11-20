package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/dhowden/tag"
)

// TagStatus describes the outcome of reading tags for one audio object.
// A non-OK status is not fatal: the track is still indexed using filename and
// directory fallbacks so that it remains browsable.
type TagStatus string

const (
	TagStatusOK        TagStatus = "ok"
	TagStatusMissing   TagStatus = "missing"    // The file has no usable text tags.
	TagStatusCorrupt   TagStatus = "corrupt"    // The tag structure could not be parsed.
	TagStatusTooLarge  TagStatus = "too_large"  // The read budget was exhausted before the tag ended.
	TagStatusTimeout   TagStatus = "timeout"    // The object store did not answer in time.
	TagStatusReadError TagStatus = "read_error" // Any other object store error.
)

// TagInfo contains the tag fields Bsimp indexes. Empty fields mean that the
// tag was absent; callers are expected to apply filename/directory fallbacks.
type TagInfo struct {
	Title       string
	Artist      string
	Album       string
	AlbumArtist string
	Track       int
	TrackTotal  int
	Disc        int
	DiscTotal   int
	Format      string
}

// TagReadLimits bounds the amount of data and the number of requests used to
// read one object's tags. Zero values disable the corresponding limit.
type TagReadLimits struct {
	MaxBytes    int64
	MaxRequests int
	ChunkSize   int64
}

const defaultTagChunkSize = 32 << 10

var (
	// errTagBudget is returned when the per-object byte budget is exhausted.
	errTagBudget = errors.New("tag read byte budget exhausted")
	// errTagRequests is returned when the per-object request budget is exhausted.
	errTagRequests = errors.New("tag read request budget exhausted")
	// ErrNoUsableTags is returned when a readable file contains no text tags.
	ErrNoUsableTags = errors.New("no usable tags")
)

// tagFetchError wraps an object-store error so it is reported as a read error
// rather than a corrupt tag.
type tagFetchError struct{ err error }

func (e *tagFetchError) Error() string { return "read object: " + e.err.Error() }
func (e *tagFetchError) Unwrap() error { return e.err }

type tagFetchFunc func(ctx context.Context, start, length int64) ([]byte, error)

// tagReadSeeker adapts a ranged object fetch into the io.ReadSeeker required
// by the tag parser. Data is fetched in chunks and cached, so the parser's many
// small reads cost a small number of GetObject range requests. The total number
// of bytes and requests is capped, which also bounds memory and latency for
// oversized or malicious tags.
type tagReadSeeker struct {
	ctx      context.Context
	size     int64
	pos      int64
	chunk    int64
	maxBytes int64
	maxReqs  int
	fetch    tagFetchFunc

	bytesRead int64
	requests  int

	// limitErr records the first budget/transport failure. The tag parser
	// sometimes wraps errors with %v (dropping the chain), so readTags consults
	// this field to classify the outcome reliably.
	limitErr error

	buf      []byte
	bufStart int64
	bufEnd   int64
}

func newTagReadSeeker(ctx context.Context, size int64, fetch tagFetchFunc, limits TagReadLimits) *tagReadSeeker {
	chunk := limits.ChunkSize
	if chunk <= 0 {
		chunk = defaultTagChunkSize
	}
	return &tagReadSeeker{
		ctx:      ctx,
		size:     size,
		chunk:    chunk,
		maxBytes: limits.MaxBytes,
		maxReqs:  limits.MaxRequests,
		fetch:    fetch,
	}
}

func (r *tagReadSeeker) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.pos >= r.size {
		return 0, io.EOF
	}
	if r.pos < r.bufStart || r.pos >= r.bufEnd {
		if err := r.fill(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.buf[r.pos-r.bufStart:r.bufEnd-r.bufStart])
	r.pos += int64(n)
	return n, nil
}

func (r *tagReadSeeker) fill() error {
	if err := r.ctx.Err(); err != nil {
		r.limitErr = err
		return err
	}
	length := r.chunk
	if remaining := r.size - r.pos; length > remaining {
		length = remaining
	}
	if r.maxBytes > 0 {
		remaining := r.maxBytes - r.bytesRead
		if remaining <= 0 {
			r.limitErr = errTagBudget
			return errTagBudget
		}
		if length > remaining {
			length = remaining
		}
	}
	if r.maxReqs > 0 && r.requests >= r.maxReqs {
		r.limitErr = errTagRequests
		return errTagRequests
	}
	data, err := r.fetch(r.ctx, r.pos, length)
	r.requests++
	if err != nil {
		r.limitErr = &tagFetchError{err: err}
		return r.limitErr
	}
	if len(data) == 0 {
		return io.EOF
	}
	// Never trust a server to respect the requested length.
	if int64(len(data)) > length {
		data = data[:length]
	}
	r.bytesRead += int64(len(data))
	r.buf = data
	r.bufStart = r.pos
	r.bufEnd = r.pos + int64(len(data))
	return nil
}

func (r *tagReadSeeker) Seek(offset int64, whence int) (int64, error) {
	var pos int64
	switch whence {
	case io.SeekStart:
		pos = offset
	case io.SeekCurrent:
		pos = r.pos + offset
	case io.SeekEnd:
		pos = r.size + offset
	default:
		return 0, fmt.Errorf("invalid whence %d", whence)
	}
	if pos < 0 {
		return 0, fmt.Errorf("negative seek position %d", pos)
	}
	// Seeking past the end is allowed, matching os.File; reads return io.EOF.
	r.pos = pos
	return pos, nil
}

// maxTagFieldLength bounds a single text field so a malicious or malformed tag
// cannot inflate memory or the rendered page.
const maxTagFieldLength = 1024

// readTags reads text tags from an object of the given size using fetch to
// access its bytes. It always returns a classified status; the error is
// informational and is also non-nil when status is not TagStatusOK.
func readTags(ctx context.Context, size int64, fetch tagFetchFunc, limits TagReadLimits) (info TagInfo, status TagStatus, err error) {
	if size <= 0 {
		return TagInfo{}, TagStatusMissing, ErrNoUsableTags
	}
	// The tag parser handles untrusted bytes; a panic in it must be reported
	// as a corrupt tag, not crash the scan.
	defer func() {
		if r := recover(); r != nil {
			info = TagInfo{}
			status = TagStatusCorrupt
			err = fmt.Errorf("panic while parsing tags: %v", r)
		}
	}()

	rs := newTagReadSeeker(ctx, size, fetch, limits)
	m, err := tag.ReadFrom(rs)
	if err != nil {
		// Prefer the recorded limit/transport failure: the parser may have
		// wrapped it with %v, and that failure explains the outcome better than
		// a generic parse error.
		if rs.limitErr != nil {
			return TagInfo{}, classifyTagError(rs.limitErr), rs.limitErr
		}
		return TagInfo{}, classifyTagError(err), err
	}

	info = tagInfoFromMetadata(m)

	// Some files carry an empty ID3v2 tag in front of a populated ID3v1 tag.
	// The parser stops at the first recognised format, so give ID3v1 a chance
	// when the text fields are empty.
	if info.isEmpty() {
		if m1, err1 := tag.ReadID3v1Tags(rs); err1 == nil {
			info = mergeTagInfo(info, tagInfoFromMetadata(m1))
		}
	}
	if info.isEmpty() {
		return info, TagStatusMissing, ErrNoUsableTags
	}
	return info, TagStatusOK, nil
}

func (t TagInfo) isEmpty() bool {
	return t.Title == "" && t.Artist == "" && t.Album == "" && t.AlbumArtist == ""
}

func mergeTagInfo(dst, src TagInfo) TagInfo {
	if dst.Title == "" {
		dst.Title = src.Title
	}
	if dst.Artist == "" {
		dst.Artist = src.Artist
	}
	if dst.Album == "" {
		dst.Album = src.Album
	}
	if dst.AlbumArtist == "" {
		dst.AlbumArtist = src.AlbumArtist
	}
	if dst.Track == 0 {
		dst.Track, dst.TrackTotal = src.Track, src.TrackTotal
	}
	if dst.Disc == 0 {
		dst.Disc, dst.DiscTotal = src.Disc, src.DiscTotal
	}
	if dst.Format == "" {
		dst.Format = src.Format
	}
	return dst
}

func tagInfoFromMetadata(m tag.Metadata) TagInfo {
	track, trackTotal := m.Track()
	disc, discTotal := m.Disc()
	return TagInfo{
		Title:       cleanTagString(m.Title()),
		Artist:      cleanTagString(m.Artist()),
		Album:       cleanTagString(m.Album()),
		AlbumArtist: cleanTagString(m.AlbumArtist()),
		Track:       track,
		TrackTotal:  trackTotal,
		Disc:        disc,
		DiscTotal:   discTotal,
		Format:      string(m.Format()),
	}
}

func cleanTagString(s string) string {
	s = strings.TrimPrefix(s, "\ufeff")
	s = strings.TrimSpace(strings.Trim(s, "\x00"))
	if len(s) > maxTagFieldLength {
		// Truncate on a rune boundary.
		s = strings.ToValidUTF8(s[:maxTagFieldLength], "")
	}
	return s
}

func classifyTagError(err error) TagStatus {
	var fetchErr *tagFetchError
	switch {
	case errors.Is(err, errTagBudget), errors.Is(err, errTagRequests):
		return TagStatusTooLarge
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return TagStatusTimeout
	case errors.As(err, &fetchErr):
		if errors.Is(fetchErr.err, context.DeadlineExceeded) || errors.Is(fetchErr.err, context.Canceled) {
			return TagStatusTimeout
		}
		return TagStatusReadError
	case errors.Is(err, tag.ErrNoTagsFound):
		return TagStatusMissing
	case errors.Is(err, tag.ErrNotID3v1):
		return TagStatusMissing
	default:
		return TagStatusCorrupt
	}
}
