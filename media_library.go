package main

import (
	"context"
	"errors"
	"path"
	"sort"
)

var errNotFound = errors.New("not found")

type MediaListing struct {
	CurrentDirectory *StorageDirectory
	Directories      []*StorageDirectory
	Files            []*StorageFile
	Cover            *StorageFile
	AudioTracks      []*StorageFile

	// IndexedTracks holds tag-based display names for the audio tracks above,
	// keyed by public path. It is empty when the tag index is not available.
	IndexedTracks map[string]*Track
}

// DisplayName returns the tag title for an audio file when known, falling back
// to the file name without its extension.
func (l *MediaListing) DisplayName(f *StorageFile) string {
	if t, ok := l.IndexedTracks[f.Path()]; ok && t != nil && t.Title != "" {
		return t.Title
	}
	return f.FriendlyName()
}

// Track returns the indexed track for a file, if available.
func (l *MediaListing) Track(f *StorageFile) *Track {
	return l.IndexedTracks[f.Path()]
}

type MediaLibrary struct {
	store *S3Storage
	index *TagIndex
}

func NewMediaLibrary(store *S3Storage) *MediaLibrary {
	return &MediaLibrary{
		store: store,
	}
}

// SetIndex attaches the tag index used to enrich directory listings.
func (ml *MediaLibrary) SetIndex(ix *TagIndex) {
	ml.index = ix
}

func (ml *MediaLibrary) findCover(files []*StorageFile) *StorageFile {
	candidates := ScoreCovers(files)
	if len(candidates) == 0 {
		return nil
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})
	return candidates[0].StorageFile
}

func (ml *MediaLibrary) listArtworkFiles(ctx context.Context, dirs []*StorageDirectory) ([]*StorageFile, error) {
	var candidates []*StorageFile
	for _, dir := range dirs {
		if !IsArtworkDir(dir) {
			continue
		}
		_, files, err := ml.store.List(ctx, dir.path)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, files...)
	}
	return candidates, nil
}

// List returns directory listing under the provided path.
func (ml *MediaLibrary) List(ctx context.Context, p string) (*MediaListing, error) {
	dirs, files, err := ml.store.List(ctx, p)
	if err != nil {
		return nil, err
	}

	// Find album cover in the current directory.
	cover := ml.findCover(files)

	if cover == nil {
		// Scan nested artwork directories for covers.
		artworkFiles, err := ml.listArtworkFiles(ctx, dirs)
		if err != nil {
			return nil, err
		}
		cover = ml.findCover(artworkFiles)
	}

	// Find audio tracks and separate all other files.
	var tracks []*StorageFile
	var otherFiles []*StorageFile
	for _, f := range files {
		if IsAudioFile(f) {
			tracks = append(tracks, f)
		} else if cover == nil || f.path != cover.path {
			otherFiles = append(otherFiles, f)
		}
	}

	var indexed map[string]*Track
	if ml.index != nil {
		indexed = map[string]*Track{}
		for _, f := range tracks {
			if t, ok := ml.index.Track(f.Path()); ok {
				indexed[f.Path()] = t
			}
		}
	}
	sortTracksByIndex(indexed, tracks)

	listing := &MediaListing{
		CurrentDirectory: NewStorageDirectory(p),
		Directories:      dirs,
		Files:            otherFiles,
		Cover:            cover,
		AudioTracks:      tracks,
		IndexedTracks:    indexed,
	}
	return listing, nil
}

// sortTracksByIndex orders audio tracks by disc and track number when the index
// knows them. It only reorders when every indexed track in the directory
// belongs to a single album; a directory mixing albums keeps the storage order
// so unrelated tracks are not interleaved by their per-album track numbers.
func sortTracksByIndex(indexed map[string]*Track, files []*StorageFile) {
	if len(indexed) < 2 {
		return
	}
	albumKey := ""
	for _, t := range indexed {
		if albumKey == "" {
			albumKey = t.albumKey
		} else if t.albumKey != albumKey {
			return
		}
	}
	pos := make(map[string]int, len(files))
	for i, f := range files {
		pos[f.Path()] = i
	}
	indexedTracks := make([]*Track, 0, len(indexed))
	for _, t := range indexed {
		indexedTracks = append(indexedTracks, t)
	}
	sortTracks(indexedTracks)
	rank := make(map[string]int, len(indexedTracks))
	for i, t := range indexedTracks {
		rank[t.Path] = i
	}
	sort.SliceStable(files, func(i, j int) bool {
		ri, oki := rank[files[i].Path()]
		rj, okj := rank[files[j].Path()]
		switch {
		case oki && okj:
			return ri < rj
		case oki:
			return true
		case okj:
			return false
		default:
			return pos[files[i].Path()] < pos[files[j].Path()]
		}
	})
}

// AlbumListing returns a listing for one indexed album. It uses the index data
// instead of listing a directory, so tracks scattered across the bucket group
// correctly. Stream URLs continue to use each track's public path.
type AlbumListing struct {
	Album    *Album
	Cover    *StorageFile
	Stats    IndexStats
	Tracks   []*Track
	Searched string
}

// Album returns the album listing for the given ID.
func (ml *MediaLibrary) Album(ctx context.Context, id string) (*AlbumListing, error) {
	if ml.index == nil {
		return nil, errNotFound
	}
	a, ok := ml.index.Album(id)
	if !ok {
		return nil, errNotFound
	}
	stats, _ := ml.index.Stats()
	listing := &AlbumListing{Album: a, Stats: stats, Tracks: a.Tracks}
	// Best effort: use the cover art found in the album's first track's
	// directory. Album grouping is tag-based, so tracks may live in several
	// directories; the first one is a reasonable representative.
	if len(a.Tracks) > 0 {
		dir := path.Dir(a.Tracks[0].Path)
		if dir == "." {
			dir = ""
		}
		dirs, files, err := ml.store.List(ctx, dir)
		if err == nil {
			cover := ml.findCover(files)
			if cover == nil {
				artworkFiles, err := ml.listArtworkFiles(ctx, dirs)
				if err == nil {
					cover = ml.findCover(artworkFiles)
				}
			}
			listing.Cover = cover
		}
	}
	return listing, nil
}

// Search returns tag-index search results. A missing index yields an empty
// result with IndexReady=false rather than an error.
func (ml *MediaLibrary) Search(query string, limit int) SearchResult {
	if ml.index == nil {
		return SearchResult{Query: query, IndexReady: false, IndexFailure: "tag index is not configured"}
	}
	return ml.index.Search(query, limit)
}

// RefreshIndex rebuilds the tag index.
func (ml *MediaLibrary) RefreshIndex(ctx context.Context) (IndexStats, error) {
	if ml.index == nil {
		return IndexStats{}, nil
	}
	return ml.index.Refresh(ctx)
}

// ContentURL returns a public URL to a file under the given path.
func (ml *MediaLibrary) ContentURL(ctx context.Context, p string) (string, error) {
	return ml.store.FileContentURL(ctx, p)
}
