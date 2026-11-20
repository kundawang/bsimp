package main

import (
	"context"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEndToEndFakeS3 boots the full HTTP mux over a fake S3 backend and checks
// that directory browsing, tag-based albums, search and streaming all coexist.
func TestEndToEndFakeS3(t *testing.T) {
	store, closeS3 := newCountingStore(t)
	defer closeS3()
	ctx := context.Background()

	store.put(t, "Aphex Twin/zz-scrambled.mp3", readFixture(t, "mp3-v23.mp3"))
	store.put(t, "Aphex Twin/aa-jumbled.mp3", readFixture(t, "mp3-v24.mp3"))
	store.put(t, "Aphex Twin/Cover.jpg", []byte("jpegdata"))
	store.put(t, "Bjork/x.flac", readFixture(t, "flac.flac"))
	store.put(t, "Loose/01 - Untagged Track.mp3", readFixture(t, "untagged.mp3"))
	store.put(t, "Loose/readme.txt", []byte("hi"))
	store.put(t, "Bad/broken.mp3", readFixture(t, "corrupt.mp3"))

	ml := NewMediaLibrary(store.S3Storage)
	ix := NewTagIndex(store, indexTestConfig())
	ml.SetIndex(ix)
	_, err := ix.Refresh(ctx)
	require.NoError(t, err)

	tmpl := mustParseTemplates(t)
	s := &Server{mediaLib: ml, tmpl: tmpl, staticVersion: "v1"}

	// Build a mux mirroring StartServer's routes so URL escaping is exercised.
	mux := http.NewServeMux()
	mux.Handle("/library/", http.StripPrefix("/library/", ValidatePath(NormalizePath(s.ListingHandler))))
	mux.Handle("/stream/", http.StripPrefix("/stream/", ValidatePath(NormalizePath(s.StreamHandler))))
	mux.Handle("/search", ValidatePath(s.SearchHandler))
	mux.Handle("/album/", http.StripPrefix("/album/", ValidatePath(s.AlbumHandler)))
	mux.Handle("/index/", ValidatePath(s.IndexHandler))

	srv := httptest.NewServer(mux)
	defer srv.Close()

	get := func(path string) (int, string) {
		resp, err := http.Get(srv.URL + path)
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, string(body)
	}

	// Directory listing still works and shows tag titles.
	code, body := get("/library/Aphex%20Twin")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "Xtal")
	assert.Contains(t, body, "/stream/Aphex%20Twin/zz-scrambled.mp3")
	assert.Contains(t, body, "/stream/Aphex%20Twin/aa-jumbled.mp3")

	// Search groups them into one album despite scrambled names.
	code, body = get("/search?q=aphex")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "Selected Ambient Works 85-92")
	assert.Contains(t, body, "1 album(s)")

	// Album page links to the original S3 paths via /stream.
	res := ix.Search("aphex", 10)
	require.Len(t, res.Albums, 1)
	code, body = get("/album/" + res.Albums[0].ID)
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "/stream/Aphex%20Twin/zz-scrambled.mp3")
	assert.Contains(t, body, "/stream/Aphex%20Twin/aa-jumbled.mp3")

	// Streaming still redirects to a presigned URL for the original object.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(srv.URL + "/stream/Aphex%20Twin/zz-scrambled.mp3")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusFound, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Location"), "zz-scrambled.mp3")

	// Untagged track is browsable via fallback.
	code, body = get("/library/Loose")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "01 - Untagged Track")
	assert.Contains(t, body, "untagged")

	// Corrupt tag is reported on the index page but still browsable.
	code, body = get("/index/")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "Indexed 5 track(s)")
	assert.Contains(t, body, "Corrupt tags")

	// Unknown album is a 404.
	code, _ = get("/album/deadbeef")
	assert.Equal(t, http.StatusNotFound, code)

	// Refresh endpoint is idempotent.
	code, _ = get("/search?q=bjork")
	require.Equal(t, http.StatusOK, code)
}

func mustParseTemplates(t *testing.T) *template.Template {
	t.Helper()
	tmpl, err := template.New("").Funcs(templateFunctions).ParseFS(embedFS, "templates/*.gohtml")
	require.NoError(t, err)
	return tmpl
}
