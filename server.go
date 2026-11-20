package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

//go:embed templates static
var embedFS embed.FS

type Server struct {
	mediaLib      *MediaLibrary
	tmpl          *template.Template
	staticVersion string

	// backgroundRefresh is non-zero while a background index refresh runs, so
	// request storms do not launch concurrent full scans.
	backgroundRefresh atomic.Int32
}

func httpError(r *http.Request, w http.ResponseWriter, err error, code int) {
	http.Error(w, err.Error(), code)
	slog.Error("failed request",
		slog.Any("err", err),
		slog.String("url", r.URL.String()),
		slog.Int("code", code),
	)
}

// ValidatePath provides a basic protection from the path traversal vulnerability.
func ValidatePath(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "./") || strings.Contains(r.URL.Path, ".\\") {
			httpError(r, w, errors.New("invalid path"), http.StatusBadRequest)
			return
		}
		h(w, r)
	}
}

// NormalizePath normalizes the request URL by removing the delimeter suffix.
func NormalizePath(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = strings.TrimRight(r.URL.Path, Delimiter)
		h(w, r)
	}
}

// DisableFileListing disables file listing under directories. It can be used with the built-in http.FileServer.
func DisableFileListing(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// TemplateData is the view model shared by all pages.
type TemplateData struct {
	StaticVersion string
	Nav           Nav
	*MediaListing
}

// SearchPageData is the view model for the tag-index search page.
type SearchPageData struct {
	StaticVersion string
	Nav           Nav
	Result        SearchResult
}

// AlbumPageData is the view model for an album grouped by tags.
type AlbumPageData struct {
	StaticVersion string
	Nav           Nav
	Listing       *AlbumListing
	ProblemCount  int
}

// Nav carries the search box state.
type Nav struct {
	Query string
	Home  bool
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		httpError(r, w, err, http.StatusInternalServerError)
		return
	}
}

func (s *Server) ListingHandler(w http.ResponseWriter, r *http.Request) {
	listing, err := s.mediaLib.List(r.Context(), r.URL.Path)
	if err != nil {
		httpError(r, w, err, http.StatusInternalServerError)
		return
	}
	// Keep the index warm as the user browses; this never blocks the request.
	s.ensureIndex(r.Context())
	s.render(w, r, "listing.gohtml", TemplateData{
		StaticVersion: s.staticVersion,
		MediaListing:  listing,
	})
}

func (s *Server) StreamHandler(w http.ResponseWriter, r *http.Request) {
	url, err := s.mediaLib.ContentURL(r.Context(), r.URL.Path)
	if err != nil {
		httpError(r, w, err, http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, url, http.StatusFound)
}

// SearchHandler serves tag-based album and track search.
func (s *Server) SearchHandler(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	// When no snapshot exists yet there is nothing to show, so wait for the
	// first scan. Afterwards, refreshes happen in the background.
	if s.mediaLib.index != nil && !s.mediaLib.index.Ready() {
		if _, err := s.mediaLib.RefreshIndex(r.Context()); err != nil {
			slog.Error("failed building tag index", slog.Any("err", err))
		}
	} else {
		s.ensureIndex(r.Context())
	}
	result := s.mediaLib.Search(query, limit)
	s.render(w, r, "search.gohtml", SearchPageData{
		StaticVersion: s.staticVersion,
		Nav:           Nav{Query: query},
		Result:        result,
	})
}

// ensureIndex refreshes the tag index in the background when it is missing or
// stale, so browsing never blocks behind a full library scan. At most one
// background refresh runs at a time.
func (s *Server) ensureIndex(ctx context.Context) {
	if s.mediaLib.index == nil {
		return
	}
	ix := s.mediaLib.index
	if ix.Fresh() {
		return
	}
	if !s.backgroundRefresh.CompareAndSwap(0, 1) {
		return
	}
	go func() {
		defer s.backgroundRefresh.Store(0)
		if _, err := ix.Refresh(context.WithoutCancel(ctx)); err != nil {
			slog.Error("failed refreshing tag index", slog.Any("err", err))
		}
	}()
}

// AlbumHandler serves an album grouped by tags.
func (s *Server) AlbumHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/")
	listing, err := s.mediaLib.Album(r.Context(), id)
	if errors.Is(err, errNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		httpError(r, w, err, http.StatusInternalServerError)
		return
	}
	stats := listing.Stats
	s.render(w, r, "album.gohtml", AlbumPageData{
		StaticVersion: s.staticVersion,
		Nav:           Nav{Query: listing.Album.Artist},
		Listing:       listing,
		ProblemCount:  stats.ProblemCount(),
	})
}

// IndexHandler reports index status and refreshes it on demand.
func (s *Server) IndexHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		stats, err := s.mediaLib.RefreshIndex(r.Context())
		if err != nil {
			httpError(r, w, err, http.StatusInternalServerError)
			return
		}
		slog.Info("refreshed tag index", slog.Int("tracks", stats.Scanned), slog.Int("read", stats.TagReads))
		http.Redirect(w, r, "/index/", http.StatusSeeOther)
		return
	}
	stats, ready := IndexStats{}, false
	failure := ""
	if s.mediaLib.index != nil {
		stats, ready = s.mediaLib.index.Stats()
		failure = s.mediaLib.index.LastError()
	}
	s.render(w, r, "index.gohtml", IndexPageData{
		StaticVersion: s.staticVersion,
		Stats:         stats,
		Ready:         ready,
		Failure:       failure,
	})
}

// IndexPageData is the view model for the index status page.
type IndexPageData struct {
	StaticVersion string
	Stats         IndexStats
	Ready         bool
	Failure       string
}

// Don't include sprig just for one function.
var templateFunctions = map[string]any{
	"defaultString": func(s string, def string) string {
		if s == "" {
			return def
		}
		return s
	},
	"statusText": func(s TagStatus) string {
		switch s {
		case TagStatusOK:
			return "read from tags"
		case TagStatusMissing:
			return "no tags; using filename"
		case TagStatusCorrupt:
			return "damaged tags; using filename"
		case TagStatusTooLarge:
			return "tag too large; using filename"
		case TagStatusTimeout:
			return "tag read timed out; using filename"
		case TagStatusReadError:
			return "tag read failed; using filename"
		default:
			return "using filename"
		}
	},
	"duration": func(d time.Duration) string {
		return d.Round(time.Millisecond).String()
	},
}

// StartServer starts HTTP server.
func StartServer(mediaLib *MediaLibrary, addr string) error {
	tmpl, err := template.New("").Funcs(templateFunctions).ParseFS(embedFS, "templates/*.gohtml")
	if err != nil {
		return err
	}

	mux := http.NewServeMux()

	mux.Handle("/", http.RedirectHandler("/library/", http.StatusMovedPermanently))

	staticVersion := fmt.Sprintf("%x", rand.Uint64())
	staticFS, err := fs.Sub(embedFS, "static")
	if err != nil {
		return err
	}
	staticPath := fmt.Sprintf("/static/%s/", staticVersion)
	mux.Handle(staticPath, DisableFileListing(http.StripPrefix(staticPath, http.FileServer(http.FS(staticFS)))))

	s := Server{
		mediaLib:      mediaLib,
		tmpl:          tmpl,
		staticVersion: staticVersion,
	}
	mux.Handle("/library/", http.StripPrefix("/library/", ValidatePath(NormalizePath(s.ListingHandler))))
	mux.Handle("/stream/", http.StripPrefix("/stream/", ValidatePath(NormalizePath(s.StreamHandler))))
	mux.Handle("/search", ValidatePath(s.SearchHandler))
	mux.Handle("/album/", http.StripPrefix("/album/", ValidatePath(s.AlbumHandler)))
	mux.Handle("/index/", ValidatePath(s.IndexHandler))

	return http.ListenAndServe(addr, mux)
}
