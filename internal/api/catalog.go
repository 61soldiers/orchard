package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"orchard/internal/apple"
	"orchard/internal/catalog"
)

// catalogError maps client errors onto HTTP responses. Apple's own detail is
// safe to pass through; it never contains tokens.
func (s *Server) catalogError(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *catalog.APIError
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such item in the Apple Music catalog")
	case errors.Is(err, apple.ErrNotReady):
		writeError(w, http.StatusServiceUnavailable, "apple_not_ready",
			"the Apple Music session is not signed in; run setup first")
	case errors.Is(err, catalog.ErrUnauthorized):
		writeError(w, http.StatusBadGateway, "apple_unauthorized",
			"Apple rejected the stored session; sign in again")
	case errors.As(err, &apiErr):
		writeError(w, http.StatusBadGateway, "apple_error", apiErr.Error())
	default:
		writeInternalError(w, r, err)
	}
}

func (s *Server) handleStorefront(w http.ResponseWriter, r *http.Request) {
	sf, err := s.catalog.Storefront(r.Context())
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"storefront": sf})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	term := strings.TrimSpace(q.Get("term"))
	if term == "" {
		writeError(w, http.StatusBadRequest, "invalid_query", "term is required")
		return
	}

	var types []string
	if raw := strings.TrimSpace(q.Get("types")); raw != "" {
		for _, t := range strings.Split(raw, ",") {
			if t = strings.TrimSpace(t); t != "" {
				types = append(types, t)
			}
		}
	}

	limit := 0
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "invalid_query", "limit must be a positive number")
			return
		}
		limit = n
	}

	res, err := s.catalog.Search(r.Context(), term, types, limit)
	if err != nil {
		// A bad type is the caller's fault, not Apple's.
		if strings.HasPrefix(err.Error(), "unknown search type") {
			writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		s.catalogError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleAlbum(w http.ResponseWriter, r *http.Request) {
	album, err := s.catalog.Album(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, album)
}

func (s *Server) handleArtist(w http.ResponseWriter, r *http.Request) {
	artist, err := s.catalog.Artist(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, artist)
}

func (s *Server) handlePlaylist(w http.ResponseWriter, r *http.Request) {
	playlist, err := s.catalog.Playlist(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, playlist)
}

func (s *Server) handleSong(w http.ResponseWriter, r *http.Request) {
	song, err := s.catalog.Song(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, song)
}

func (s *Server) handleLyrics(w http.ResponseWriter, r *http.Request) {
	lyrics, err := s.catalog.Lyrics(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, lyrics)
}
