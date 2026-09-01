package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"orchard/internal/catalog"
)

// The `/v1/me/library/*` routes expose the signed-in account's own Apple Music
// library — added songs/albums/artists and personal playlists — distinct from
// `/v1/library/*`, which is Orchard's own index of downloaded files. Every
// item comes back keyed by its catalog id, so it can be opened, streamed or
// downloaded through the existing catalog routes.

func (s *Server) handleLibraryPlaylistsMe(w http.ResponseWriter, r *http.Request) {
	if !s.appleReady(w) {
		return
	}
	playlists, err := s.catalog.LibraryPlaylists(r.Context())
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	if playlists == nil {
		playlists = []catalog.LibraryPlaylist{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"playlists": playlists})
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

func (s *Server) handleLibrarySongsMe(w http.ResponseWriter, r *http.Request) {
	if !s.appleReady(w) {
		return
	}
	songs, err := s.catalog.LibrarySongs(r.Context())
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	if songs == nil {
		songs = []catalog.Song{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"songs": songs})
}

func (s *Server) handleLibraryAlbumsMe(w http.ResponseWriter, r *http.Request) {
	if !s.appleReady(w) {
		return
	}
	albums, err := s.catalog.LibraryAlbums(r.Context())
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	if albums == nil {
		albums = []catalog.Album{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"albums": albums})
}

func (s *Server) handleLibraryArtistsMe(w http.ResponseWriter, r *http.Request) {
	if !s.appleReady(w) {
		return
	}
	artists, err := s.catalog.LibraryArtists(r.Context())
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	if artists == nil {
		artists = []catalog.Artist{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"artists": artists})
}
