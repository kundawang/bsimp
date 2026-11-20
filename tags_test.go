package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/tags/" + name)
	require.NoError(t, err)
	return b
}

// fixtureFetch serves an in-memory object through ranged fetches and counts
// bytes and requests, mirroring the S3 ReadRange contract.
func fixtureFetch(data []byte, bytesRead, requests *int64) tagFetchFunc {
	return func(_ context.Context, start, length int64) ([]byte, error) {
		*requests++
		if start >= int64(len(data)) {
			return nil, nil
		}
		end := start + length
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		b := data[start:end]
		*bytesRead += int64(len(b))
		return b, nil
	}
}

func TestReadTags_Formats(t *testing.T) {
	lim := TagReadLimits{MaxBytes: 1 << 20, MaxRequests: 64, ChunkSize: 512}
	testCases := []struct {
		file     string
		status   TagStatus
		title    string
		artist   string
		album    string
		track    int
		disc     int
		maxBytes int64
	}{
		{file: "mp3-v23.mp3", status: TagStatusOK, title: "Xtal", artist: "Aphex Twin", album: "Selected Ambient Works 85-92", track: 1},
		{file: "mp3-v24.mp3", status: TagStatusOK, title: "Xtal", artist: "Aphex Twin", album: "Selected Ambient Works 85-92", track: 1},
		{file: "id3v1.mp3", status: TagStatusOK, title: "Jericho", artist: "The Prodigy", album: "Experience", track: 1},
		{file: "flac.flac", status: TagStatusOK, title: "Hunter", artist: "Björk", album: "Homogenic", track: 1},
		{file: "ogg-vorbis.ogg", status: TagStatusOK, title: "Вечная мерзлота", artist: "Психохирурги", album: "Психохирурги", track: 2},
		{file: "ogg-opus.opus", status: TagStatusOK, title: "andata", artist: "坂本龍一", album: "async"},
		{file: "m4a.m4a", status: TagStatusOK, title: "Svefn-g-englar", artist: "Sigur Rós", album: "Ágætis byrjun", track: 1},
		{file: "untagged.mp3", status: TagStatusMissing},
		{file: "corrupt.mp3", status: TagStatusCorrupt},
	}
	for _, tc := range testCases {
		t.Run(tc.file, func(t *testing.T) {
			data := readFixture(t, tc.file)
			var bytesRead, requests int64
			info, status, _ := readTags(context.Background(), int64(len(data)),
				fixtureFetch(data, &bytesRead, &requests), lim)
			assert.Equal(t, tc.status, status)
			assert.Equal(t, tc.title, info.Title)
			assert.Equal(t, tc.artist, info.Artist)
			assert.Equal(t, tc.album, info.Album)
			assert.Equal(t, tc.track, info.Track)
			assert.Equal(t, tc.disc, info.Disc)
			// Tag data is a small prefix (or suffix); never the whole file.
			assert.LessOrEqual(t, bytesRead, int64(8<<10), "read too much of %s", tc.file)
			assert.LessOrEqual(t, requests, int64(16), "too many range requests")
		})
	}
}

func TestReadTags_ByteBudget(t *testing.T) {
	data := readFixture(t, "ogg-vorbis.ogg")
	var bytesRead, requests int64
	// The vorbis comment sits after the audio setup header; a tiny budget
	// must stop the read and report too_large instead of hanging or reading
	// the whole object.
	info, status, err := readTags(context.Background(), int64(len(data)),
		fixtureFetch(data, &bytesRead, &requests), TagReadLimits{MaxBytes: 16, MaxRequests: 64, ChunkSize: 16})
	require.Error(t, err)
	assert.Equal(t, TagStatusTooLarge, status)
	assert.True(t, info.isEmpty())
	assert.LessOrEqual(t, bytesRead, int64(16))
}

func TestReadTags_RequestBudget(t *testing.T) {
	data := readFixture(t, "flac.flac")
	var bytesRead, requests int64
	_, status, _ := readTags(context.Background(), int64(len(data)),
		fixtureFetch(data, &bytesRead, &requests), TagReadLimits{MaxBytes: 1 << 20, MaxRequests: 1, ChunkSize: 8})
	assert.Equal(t, TagStatusTooLarge, status)
	assert.Equal(t, int64(1), requests)
}

func TestReadTags_Timeout(t *testing.T) {
	data := readFixture(t, "mp3-v23.mp3")
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	<-ctx.Done()
	fetch := func(ctx context.Context, _, _ int64) ([]byte, error) {
		return nil, ctx.Err()
	}
	_, status, err := readTags(ctx, int64(len(data)), fetch, TagReadLimits{MaxBytes: 1 << 20, MaxRequests: 8})
	require.Error(t, err)
	assert.Equal(t, TagStatusTimeout, status)
}

func TestReadTags_ReadError(t *testing.T) {
	data := readFixture(t, "mp3-v23.mp3")
	fetch := func(context.Context, int64, int64) ([]byte, error) {
		return nil, assert.AnError
	}
	_, status, err := readTags(context.Background(), int64(len(data)), fetch, TagReadLimits{MaxBytes: 1 << 20, MaxRequests: 8})
	require.Error(t, err)
	assert.Equal(t, TagStatusReadError, status)
}

func TestTagReadSeeker_ShortReads(t *testing.T) {
	// A server that returns fewer bytes than asked must still yield the full
	// object to the parser.
	data := []byte(strings.Repeat("0123456789", 50))
	fetch := func(_ context.Context, start, length int64) ([]byte, error) {
		if length > 7 {
			length = 7
		}
		end := start + length
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		return append([]byte(nil), data[start:end]...), nil
	}
	rs := newTagReadSeeker(context.Background(), int64(len(data)), fetch, TagReadLimits{MaxBytes: 1 << 20, MaxRequests: 1000, ChunkSize: 32})
	var out []byte
	buf := make([]byte, 10)
	for {
		n, err := rs.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			break
		}
	}
	assert.Equal(t, string(data), string(out))
}

func TestReadTags_EmptyObject(t *testing.T) {
	info, status, err := readTags(context.Background(), 0,
		func(context.Context, int64, int64) ([]byte, error) { return nil, nil },
		TagReadLimits{})
	assert.Equal(t, TagStatusMissing, status)
	require.Error(t, err)
	assert.True(t, info.isEmpty())
}

func TestReadTags_TinyBudgetClassifiedAsTooLarge(t *testing.T) {
	// Budget smaller than the ID3 header: the parser wraps the read error with
	// %v, so classification must rely on the recorded limit failure.
	data := readFixture(t, "mp3-v23.mp3")
	var bytesRead, requests int64
	_, status, err := readTags(context.Background(), int64(len(data)),
		fixtureFetch(data, &bytesRead, &requests), TagReadLimits{MaxBytes: 4, MaxRequests: 64, ChunkSize: 4})
	require.Error(t, err)
	assert.Equal(t, TagStatusTooLarge, status)
	assert.LessOrEqual(t, bytesRead, int64(4))
}

func TestReadTags_CanceledContext(t *testing.T) {
	data := readFixture(t, "flac.flac")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fetch := func(_ context.Context, start, length int64) ([]byte, error) {
		return data[start : start+length], nil
	}
	_, status, _ := readTags(ctx, int64(len(data)), fetch, TagReadLimits{MaxBytes: 1 << 20, MaxRequests: 8})
	assert.Equal(t, TagStatusTimeout, status)
}
