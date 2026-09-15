package api

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"orchard/internal/catalog"
)

// The `/v1/me/library/*` routes expose the signed-in account's own Apple Music
// library — added songs/albums/artists and personal playlists — distinct from
// `/v1/library/*`, which is Orchard's own index of downloaded files. Every
// item comes back keyed by its catalog id, so it can be opened, streamed or
// downloaded through the existing catalog routes.
//
// Every list here returns one page per call, alongside a `nextCursor` that is
// empty once the list is exhausted. A cursor must be echoed back exactly as
// received — it is not a page number, and passing anything else fails with
// `invalid_query`.

func (s *Server) handleLibraryPlaylistsMe(w http.ResponseWriter, r *http.Request) {
	if !s.appleReady(w) {
		return
	}
	cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	playlists, next, err := s.catalog.LibraryPlaylists(r.Context(), cursor)
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	if playlists == nil {
		playlists = []catalog.LibraryPlaylist{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"playlists": playlists, "nextCursor": next})
}

func (s *Server) handleLibraryPlaylistMe(w http.ResponseWriter, r *http.Request) {
	if !s.appleReady(w) {
		return
	}
	playlist, err := s.catalog.LibraryPlaylist(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, playlist)
}

// handleLibraryPlaylistTracksMe returns the next page of a library
// playlist's tracks. cursor must be a tracksNextCursor from the playlist
// itself or a previous call here.
func (s *Server) handleLibraryPlaylistTracksMe(w http.ResponseWriter, r *http.Request) {
	if !s.appleReady(w) {
		return
	}
	cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	if cursor == "" {
		writeError(w, http.StatusBadRequest, "invalid_query", "cursor is required")
		return
	}
	songs, next, err := s.catalog.LibraryPlaylistTracks(r.Context(), chi.URLParam(r, "id"), cursor)
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	if songs == nil {
		songs = []catalog.Song{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"songs": songs, "nextCursor": next})
}

func (s *Server) handleLibrarySongsMe(w http.ResponseWriter, r *http.Request) {
	if !s.appleReady(w) {
		return
	}
	cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	songs, next, err := s.catalog.LibrarySongs(r.Context(), cursor)
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	if songs == nil {
		songs = []catalog.Song{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"songs": songs, "nextCursor": next})
}

func (s *Server) handleLibraryAlbumsMe(w http.ResponseWriter, r *http.Request) {
	if !s.appleReady(w) {
		return
	}
	cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	albums, next, err := s.catalog.LibraryAlbums(r.Context(), cursor)
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	if albums == nil {
		albums = []catalog.Album{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"albums": albums, "nextCursor": next})
}

func (s *Server) handleLibraryArtistsMe(w http.ResponseWriter, r *http.Request) {
	if !s.appleReady(w) {
		return
	}
	cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	artists, next, err := s.catalog.LibraryArtists(r.Context(), cursor)
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	if artists == nil {
		artists = []catalog.Artist{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"artists": artists, "nextCursor": next})
}

// handlePinsMe returns the items the user pinned to the top of their Apple
// Music library. Ids come back resolved to the catalog wherever Apple has an
// equivalent, same as every other library read here.
func (s *Server) handlePinsMe(w http.ResponseWriter, r *http.Request) {
	if !s.appleReady(w) {
		return
	}
	pins, err := s.catalog.Pins(r.Context())
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	if pins == nil {
		pins = []catalog.Pin{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"pins": pins})
}
