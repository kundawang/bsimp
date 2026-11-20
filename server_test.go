package main

import (
	"context"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestServer(t *testing.T) (*Server, func()) {
	t.Helper()
	store, closeS3 := newCountingStore(t)
	store.put(t, "Aphex Twin/a.mp3", readFixture(t, "mp3-v23.mp3"))
	store.put(t, "Aphex Twin/b.mp3", readFixture(t, "mp3-v24.mp3"))
	store.put(t, "Aphex Twin/Cover.jpg", []byte("image"))
	store.put(t, "Untagged/01 - Song.mp3", readFixture(t, "untagged.mp3"))

	tmpl, err := template.New("").Funcs(templateFunctions).ParseFS(embedFS, "templates/*.gohtml")
	require.NoError(t, err)

	ml := NewMediaLibrary(store.S3Storage)
	ix := NewTagIndex(store, indexTestConfig())
	_, err = ix.Refresh(context.Background())
	require.NoError(t, err)
	ml.SetIndex(ix)

	return &Server{mediaLib: ml, tmpl: tmpl, staticVersion: "test"}, closeS3
}

func serve(s *Server, method, target string) *httptest.ResponseRecorder {
	safeTarget := strings.ReplaceAll(target, " ", "%20")
	req := httptest.NewRequest(method, safeTarget, nil)
	rec := httptest.NewRecorder()
	switch {
	case target == "/search" || strings.HasPrefix(target, "/search?"):
		s.SearchHandler(rec, req)
	case strings.HasPrefix(target, "/album/"):
		req.URL.Path = strings.TrimPrefix(target, "/album")
		s.AlbumHandler(rec, req)
	case strings.HasPrefix(target, "/index/"):
		s.IndexHandler(rec, req)
	default:
		decoded, err := url.PathUnescape(target)
		if err == nil {
			req.URL.Path = decoded
		} else {
			req.URL.Path = target
		}
		s.ListingHandler(rec, req)
	}
	return rec
}

func TestServer_ListingUsesTagTitles(t *testing.T) {
	s, closeS3 := newTestServer(t)
	defer closeS3()

	rec := serve(s, http.MethodGet, "/Aphex Twin")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Xtal", "tag title replaces the scrambled filename")
	assert.Contains(t, body, `data-artist="Aphex Twin"`)
	assert.Contains(t, body, `data-album="Selected Ambient Works 85-92"`)
	// Stream URLs keep the original object path.
	assert.Contains(t, body, `/stream/Aphex%20Twin/a.mp3`)
	assert.Contains(t, body, `/stream/Aphex%20Twin/b.mp3`)
	// The cover is still detected.
	assert.Contains(t, body, `/stream/Aphex%20Twin/Cover.jpg`)
}

func TestServer_ListingUntaggedFallback(t *testing.T) {
	s, closeS3 := newTestServer(t)
	defer closeS3()

	rec := serve(s, http.MethodGet, "/Untagged")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "01 - Song")
	assert.Contains(t, body, "untagged")
}

func TestServer_SearchPage(t *testing.T) {
	s, closeS3 := newTestServer(t)
	defer closeS3()

	rec := serve(s, http.MethodGet, "/search?q=aphex")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Selected Ambient Works 85-92")
	assert.Contains(t, body, "Xtal")
	assert.Contains(t, body, "/album/")

	rec = serve(s, http.MethodGet, "/search?q=")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "album(s)")

	rec = serve(s, http.MethodGet, "/search?q=no-such-thing")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "No albums or tracks matched")
}

func TestServer_AlbumPage(t *testing.T) {
	s, closeS3 := newTestServer(t)
	defer closeS3()

	res := s.mediaLib.Search("selected ambient", 10)
	require.Len(t, res.Albums, 1)
	album := res.Albums[0]

	rec := serve(s, http.MethodGet, "/album/"+album.ID)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Selected Ambient Works 85-92")
	assert.Contains(t, body, "Aphex Twin")
	assert.Contains(t, body, "Xtal")
	assert.Contains(t, body, `/stream/Aphex%20Twin/a.mp3`)
	assert.Contains(t, body, `/stream/Aphex%20Twin/b.mp3`)

	rec = serve(s, http.MethodGet, "/album/does-not-exist")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestServer_IndexPage(t *testing.T) {
	s, closeS3 := newTestServer(t)
	defer closeS3()

	rec := serve(s, http.MethodGet, "/index/")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Indexed 3 track(s)")
	assert.Contains(t, body, "Read from tags")

	rec = serve(s, http.MethodPost, "/index/")
	assert.Equal(t, http.StatusSeeOther, rec.Code, "POST redirects")
}

func TestServer_ListingWithoutIndex(t *testing.T) {
	store, closeS3 := newCountingStore(t)
	defer closeS3()
	store.put(t, "album/01 - Song.mp3", readFixture(t, "untagged.mp3"))
	ml := NewMediaLibrary(store.S3Storage)

	tmpl, err := template.New("").Funcs(templateFunctions).ParseFS(embedFS, "templates/*.gohtml")
	require.NoError(t, err)
	s := &Server{mediaLib: ml, tmpl: tmpl, staticVersion: "test"}

	rec := serve(s, http.MethodGet, "/album/01%20-%20Song")
	assert.Contains(t, []int{http.StatusOK, http.StatusNotFound}, rec.Code)

	rec = serve(s, http.MethodGet, "/search?q=x")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "not ready")
}

func TestNormalizePathAndValidatePath(t *testing.T) {
	handler := ValidatePath(NormalizePath(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(r.URL.Path))
	}))

	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/a/b/", nil))
	assert.Equal(t, "/a/b", rec.Body.String())

	rec = httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/../etc", nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
