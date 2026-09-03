package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"orchard/internal/apple"
	"orchard/internal/catalog"
	"orchard/internal/playactivity"
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

// playActivityRequest is one reported moment of playback. It is deliberately
// player-shaped rather than Apple-shaped — a client says what it did, and
// internal/playactivity translates that into Apple's wire format.
type playActivityRequest struct {
	// Event is "start" or "end".
	Event string `json:"event"`
	// SongID is the catalog adam id of the track played.
	SongID string `json:"song_id"`
	// DurationMs is the track's full length.
	DurationMs int `json:"duration_ms"`
	// StartPositionMs is where this play segment began, EndPositionMs where it
	// stopped. Both default to 0, which is right for a track played whole.
	StartPositionMs int `json:"start_position_ms"`
	EndPositionMs   int `json:"end_position_ms"`
	// EndReason is why an "end" event stopped: natural, skipped_forward,
	// skipped_backward, paused, replaced, failed, exited or other. Defaults to
	// natural.
	EndReason string `json:"end_reason"`
	// ContainerType is "album", "playlist", "artist" or "radio", and
	// ContainerID that container's catalog id. Apple keys Recently Played on
	// the container, so sending them is what makes an album or playlist (not
	// just a loose song) appear there.
	ContainerType string `json:"container_type"`
	ContainerID   string `json:"container_id"`
}

// handlePlayActivity reports a play to Apple so it lands in the account's
// Recently Played and feeds its personalization. This is fire-and-forget by
// nature — it never affects whether the audio played — but the status is
// returned honestly rather than swallowed, so a client can log a failure.
func (s *Server) handlePlayActivity(w http.ResponseWriter, r *http.Request) {
	if !s.appleReady(w) {
		return
	}

	var req playActivityRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}

	start := false
	switch strings.ToLower(strings.TrimSpace(req.Event)) {
	case "start":
		start = true
	case "end", "":
		start = false
	default:
		writeError(w, http.StatusBadRequest, "invalid_body",
			`event must be "start" or "end"`)
		return
	}

	reason, err := playactivity.ParseEndReason(req.EndReason)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}

	ev := playactivity.Event{
		Start:           start,
		SongID:          req.SongID,
		DurationMs:      req.DurationMs,
		StartPositionMs: req.StartPositionMs,
		EndPositionMs:   req.EndPositionMs,
		EndReason:       reason,
	}
	if strings.TrimSpace(req.ContainerID) != "" {
		ev.Container = &playactivity.Container{
			Kind: req.ContainerType,
			ID:   req.ContainerID,
		}
	}

	if err := s.playActivity.Report(r.Context(), ev); err != nil {
		s.playActivityError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"reported": true})
}

func (s *Server) playActivityError(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *playactivity.APIError
	switch {
	case errors.Is(err, playactivity.ErrInvalidEvent):
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
	case errors.Is(err, apple.ErrNotReady):
		writeError(w, http.StatusServiceUnavailable, "apple_not_ready",
			"the Apple Music session is not signed in; run setup first")
	case errors.As(err, &apiErr):
		writeError(w, http.StatusBadGateway, "apple_error", apiErr.Error())
	default:
		writeInternalError(w, r, err)
	}
}
