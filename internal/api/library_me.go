package api

import (
	"errors"
	"net/http"
	"strconv"
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

// libraryOptionsFrom reads the optional `limit` (page size) and `sort=recent`
// (newest added first) query parameters. `sort` only matters on the first
// request, since the cursor carries it on; `limit` must be repeated on every
// request, because Apple drops it from its `next` links.
func libraryOptionsFrom(r *http.Request) []catalog.LibraryOption {
	var opts []catalog.LibraryOption
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil {
		opts = append(opts, catalog.WithLibraryLimit(n))
	}
	if r.URL.Query().Get("sort") == "recent" {
		opts = append(opts, catalog.WithLibraryRecent())
	}
	return opts
}

func (s *Server) handleLibrarySongsMe(w http.ResponseWriter, r *http.Request) {
	if !s.appleReady(w) {
		return
	}
	cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	songs, next, err := s.catalog.LibrarySongs(r.Context(), cursor, libraryOptionsFrom(r)...)
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
	albums, next, err := s.catalog.LibraryAlbums(r.Context(), cursor, libraryOptionsFrom(r)...)
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

// ---------------------------------------------------------------------------
// Library playlist writes
// ---------------------------------------------------------------------------
//
// The four routes below are the only ones in `/v1/me/library/*` that change
// anything on Apple's side. They exist so a client can manage the account's
// own playlists — create, rename, add to, reorder/remove — instead of only
// reading them; `elbert` uses them for the same playlist editing it already
// offers for local and Subsonic playlists.
//
// Track ids are the **catalog** ids every other endpoint here hands out, so a
// client can take a song straight from a search result or an album and put it
// in a playlist without a second lookup. Reordering and removing are both
// PUT: Apple has no "move" or "remove one" operation, so the client sends the
// full list in its intended order (see catalog.SetLibraryPlaylistTracks).

// handleCreateLibraryPlaylistMe creates a playlist in the account's library.
func (s *Server) handleCreateLibraryPlaylistMe(w http.ResponseWriter, r *http.Request) {
	if !s.appleReady(w) {
		return
	}
	var body struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		TrackIDs    []string `json:"trackIds"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeError(w, http.StatusBadRequest, "invalid_body", "name is required")
		return
	}

	playlist, err := s.catalog.CreateLibraryPlaylist(r.Context(), body.Name, body.Description, body.TrackIDs)
	if err != nil {
		s.libraryWriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, playlist)
}

// handleUpdateLibraryPlaylistMe renames and/or re-describes a playlist.
func (s *Server) handleUpdateLibraryPlaylistMe(w http.ResponseWriter, r *http.Request) {
	if !s.appleReady(w) {
		return
	}
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if strings.TrimSpace(body.Name) == "" && strings.TrimSpace(body.Description) == "" {
		writeError(w, http.StatusBadRequest, "invalid_body", "name or description is required")
		return
	}

	if err := s.catalog.UpdateLibraryPlaylist(r.Context(), chi.URLParam(r, "id"), body.Name, body.Description); err != nil {
		s.libraryWriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDeleteLibraryPlaylistMe removes a playlist from the library.
func (s *Server) handleDeleteLibraryPlaylistMe(w http.ResponseWriter, r *http.Request) {
	if !s.appleReady(w) {
		return
	}
	if err := s.catalog.DeleteLibraryPlaylist(r.Context(), chi.URLParam(r, "id")); err != nil {
		s.libraryWriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAddLibraryPlaylistTracksMe appends tracks to a playlist.
func (s *Server) handleAddLibraryPlaylistTracksMe(w http.ResponseWriter, r *http.Request) {
	if !s.appleReady(w) {
		return
	}
	var body struct {
		TrackIDs []string `json:"trackIds"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if len(body.TrackIDs) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_body", "trackIds is required")
		return
	}

	if err := s.catalog.AddLibraryPlaylistTracks(r.Context(), chi.URLParam(r, "id"), body.TrackIDs); err != nil {
		s.libraryWriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSetLibraryPlaylistTracksMe replaces a playlist's contents with
// exactly the ids given, in that order — the reorder and remove operation.
//
// Emptying a playlist needs `"allowEmpty": true` alongside an empty list, so
// a client bug that loses its track list can't silently wipe one.
func (s *Server) handleSetLibraryPlaylistTracksMe(w http.ResponseWriter, r *http.Request) {
	if !s.appleReady(w) {
		return
	}
	var body struct {
		TrackIDs   []string `json:"trackIds"`
		AllowEmpty bool     `json:"allowEmpty"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}

	err := s.catalog.SetLibraryPlaylistTracks(r.Context(), chi.URLParam(r, "id"), body.TrackIDs, body.AllowEmpty)
	if err != nil {
		s.libraryWriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// libraryWriteError adds the two failures only a write can produce to the
// shared catalog error mapping: an empty track list (a client bug, 400) and
// Apple refusing to edit a playlist the account doesn't own (403).
func (s *Server) libraryWriteError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, catalog.ErrEmptyTrackList) {
		writeError(w, http.StatusBadRequest, "invalid_body",
			"track list is empty; pass allowEmpty to clear a playlist")
		return
	}
	var apiErr *catalog.APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusForbidden {
		writeError(w, http.StatusForbidden, "playlist_not_editable",
			"Apple refused this edit; only playlists the signed-in account owns can be changed")
		return
	}
	s.catalogError(w, r, err)
}
