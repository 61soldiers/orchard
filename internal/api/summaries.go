package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"orchard/internal/catalog"
)

// handleSummaries lists the Replay periods Apple has computed for the account
// — every year on record, or one year's months with ?period=month&year=YYYY.
// The listing carries each period's totals but none of its rankings; those
// come from handleSummary, which is one request per period a client opens.
func (s *Server) handleSummaries(w http.ResponseWriter, r *http.Request) {
	if !s.appleReady(w) {
		return
	}
	year, _ := strconv.Atoi(r.URL.Query().Get("year"))
	idx, err := s.catalog.Summaries(r.Context(), r.URL.Query().Get("period"), year)
	if err != nil {
		s.summaryError(w, r, err)
		return
	}
	if idx.Summaries == nil {
		idx.Summaries = []catalog.Summary{}
	}
	writeJSON(w, http.StatusOK, idx)
}

// handleSummary returns one Replay period with every ranking Apple computed
// for it: top songs, albums, artists, genres, playlists, stations, and the
// milestones passed within it.
func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	if !s.appleReady(w) {
		return
	}
	sum, err := s.catalog.Summary(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.summaryError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

// summaryError adds the one failure mode the shared catalog mapping doesn't
// know about: a period Apple doesn't compute is the caller's mistake, not a
// bad gateway.
func (s *Server) summaryError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, catalog.ErrInvalidPeriod) {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	s.catalogError(w, r, err)
}
