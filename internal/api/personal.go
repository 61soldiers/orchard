package api

import (
	"net/http"
	"strconv"

	"orchard/internal/catalog"
)

// handleRecommendations returns Apple's "Made For You" personalization —
// the signed-in account's named personal playlists, themed collections and
// station groups.
func (s *Server) handleRecommendations(w http.ResponseWriter, r *http.Request) {
	if !s.appleReady(w) {
		return
	}
	groups, err := s.catalog.Recommendations(r.Context())
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	if groups == nil {
		groups = []catalog.RecommendationGroup{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
}

// handleRecentlyPlayed lists what the signed-in account played most recently.
func (s *Server) handleRecentlyPlayed(w http.ResponseWriter, r *http.Request) {
	if !s.appleReady(w) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := s.catalog.RecentlyPlayed(r.Context(), limit)
	if err != nil {
		s.catalogError(w, r, err)
		return
	}
	if items == nil {
		items = []catalog.Item{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
