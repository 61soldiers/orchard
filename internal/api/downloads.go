package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"orchard/internal/download"
	"orchard/internal/store"
)

type downloadRequest struct {
	Type     string   `json:"type"`
	ID       string   `json:"id"`
	Codec    string   `json:"codec"`
	TrackIDs []string `json:"trackIds"`
}

func (s *Server) handleCreateDownload(w http.ResponseWriter, r *http.Request) {
	if !s.appleReady(w) {
		return
	}

	var req downloadRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		writeError(w, http.StatusBadRequest, "invalid_body", "id is required")
		return
	}

	job, err := s.downloads.Enqueue(r.Context(), download.Request{
		Type:      strings.ToLower(strings.TrimSpace(req.Type)),
		CatalogID: strings.TrimSpace(req.ID),
		Codec:     req.Codec,
		TrackIDs:  req.TrackIDs,
	})
	switch {
	case errors.Is(err, download.ErrUnknownType):
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
	case err != nil:
		s.catalogError(w, r, err)
	default:
		writeJSON(w, http.StatusAccepted, job)
	}
}

func (s *Server) handleListDownloads(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	jobs, err := s.store.Jobs(r.Context(), limit)
	if err != nil {
		writeInternalError(w, r, err)
		return
	}
	if jobs == nil {
		jobs = []store.Job{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

func (s *Server) handleGetDownload(w http.ResponseWriter, r *http.Request) {
	job, err := s.store.Job(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such job")
		return
	}
	if err != nil {
		writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleCancelDownload(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	err := s.downloads.Cancel(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such job")
	case err != nil:
		writeError(w, http.StatusConflict, "not_cancellable", err.Error())
	default:
		// Cancellation is asynchronous: the worker has to unwind before the
		// state lands. Give it a moment so callers see the settled job.
		job, _ := s.store.Job(r.Context(), id)
		for i := 0; i < 20 && !isTerminal(job.State); i++ {
			time.Sleep(100 * time.Millisecond)
			job, _ = s.store.Job(r.Context(), id)
		}
		writeJSON(w, http.StatusOK, job)
	}
}

// handleDownloadEvents streams job progress as server-sent events. It replays
// the job's current state first so a client that connects late is not stuck
// waiting for the next change.
func (s *Server) handleDownloadEvents(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	job, err := s.store.Job(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such job")
		return
	}
	if err != nil {
		writeInternalError(w, r, err)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal_error", "streaming is unavailable")
		return
	}

	events, unsubscribe := s.downloads.Subscribe(id)
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(e download.Event) bool {
		payload, err := json.Marshal(e)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	send(download.Event{JobID: id, State: job.State})
	for _, t := range job.Tracks {
		send(download.Event{
			JobID: id, TrackID: t.TrackID, Phase: t.Phase,
			BytesDone: t.BytesDone, BytesTotal: t.BytesTotal, Error: t.Error,
		})
	}
	if isTerminal(job.State) {
		return
	}

	// Comments keep proxies from closing an idle connection.
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case e, open := <-events:
			if !open || !send(e) {
				return
			}
			if e.State != "" && isTerminal(e.State) {
				return
			}
		}
	}
}

func isTerminal(state string) bool {
	return state == store.JobDone || state == store.JobFailed || state == store.JobCancelled
}

// handleTrackFile serves a downloaded file, with range support so players can
// seek. Unlike the streaming endpoint this is a finished progressive .m4a.
func (s *Server) handleTrackFile(w http.ResponseWriter, r *http.Request) {
	track, err := s.store.Track(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "that track has not been downloaded")
		return
	}
	if err != nil {
		writeInternalError(w, r, err)
		return
	}

	// Paths come from our own database, but resolve and re-check anyway so a
	// bad row can never escape the library root.
	full := filepath.Join(s.cfg.LibraryDir, filepath.Clean("/"+track.Path))
	if !strings.HasPrefix(full, filepath.Clean(s.cfg.LibraryDir)+string(os.PathSeparator)) {
		writeInternalError(w, r, fmt.Errorf("track %s has an out-of-root path", track.ID))
		return
	}

	f, err := os.Open(full)
	if err != nil {
		writeError(w, http.StatusNotFound, "file_missing",
			"the file is recorded but missing from disk; download it again")
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		writeInternalError(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "audio/mp4")
	w.Header().Set("Accept-Ranges", "bytes")
	http.ServeContent(w, r, filepath.Base(full), info.ModTime(), f)
}

// handleLibraryTracks lists downloaded tracks. since+limit alone reproduces
// the plain sync listing; q/artist/album/albumId/codec/sort/order/offset let
// a client browse and filter its own copy instead of mirroring the whole thing.
func (s *Server) handleLibraryTracks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	f := store.TrackFilter{
		Q:       strings.TrimSpace(q.Get("q")),
		Artist:  strings.TrimSpace(q.Get("artist")),
		Album:   strings.TrimSpace(q.Get("album")),
		AlbumID: strings.TrimSpace(q.Get("albumId")),
		Codec:   strings.TrimSpace(q.Get("codec")),
		Sort:    strings.TrimSpace(q.Get("sort")),
	}

	if raw := q.Get("since"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_query",
				"since must be an RFC3339 timestamp")
			return
		}
		f.Since = t
	}
	switch strings.ToLower(strings.TrimSpace(q.Get("order"))) {
	case "", "asc":
	case "desc":
		f.Desc = true
	default:
		writeError(w, http.StatusBadRequest, "invalid_query", "order must be asc or desc")
		return
	}
	switch f.Sort {
	case "", "downloadedAt", "title", "artist", "album", "duration", "size":
	default:
		writeError(w, http.StatusBadRequest, "invalid_query",
			"sort must be one of downloadedAt, title, artist, album, duration, size")
		return
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "invalid_query", "limit must be a positive number")
			return
		}
		f.Limit = n
	}
	if raw := q.Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid_query", "offset must not be negative")
			return
		}
		f.Offset = n
	}

	tracks, err := s.store.Tracks(r.Context(), f)
	if err != nil {
		writeInternalError(w, r, err)
		return
	}
	if tracks == nil {
		tracks = []store.Track{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tracks": tracks})
}

// handleLibraryAlbums lists the albums the library holds, grouped by catalog
// album id where one was recorded and by name otherwise.
func (s *Server) handleLibraryAlbums(w http.ResponseWriter, r *http.Request) {
	albums, err := s.store.Albums(r.Context())
	if err != nil {
		writeInternalError(w, r, err)
		return
	}
	if albums == nil {
		albums = []store.AlbumSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"albums": albums})
}

// handleLibraryArtists lists the artists the library holds.
func (s *Server) handleLibraryArtists(w http.ResponseWriter, r *http.Request) {
	artists, err := s.store.Artists(r.Context())
	if err != nil {
		writeInternalError(w, r, err)
		return
	}
	if artists == nil {
		artists = []store.ArtistSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"artists": artists})
}

// handleLibraryStats summarises the whole library, for a client's storage or
// settings view.
func (s *Server) handleLibraryStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.Stats(r.Context())
	if err != nil {
		writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}
