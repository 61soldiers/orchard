package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"orchard/internal/apple"
	"orchard/internal/stream"
	"orchard/internal/webplayback"
)

// handleSongVariants lists the audio renditions Apple offers for a track.
func (s *Server) handleSongVariants(w http.ResponseWriter, r *http.Request) {
	if !s.appleReady(w) {
		return
	}
	variants, err := s.stream.Variants(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.streamError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"variants": variants})
}

// handleSongStream decrypts a track and streams it as it arrives.
//
// The response is a fragmented MP4, not a plain .m4a: a progressive file needs a
// complete sample table up front, which is only known once the whole track has
// been processed. ffmpeg-based players handle fragmented MP4 fine; browsers
// using Media Source Extensions do not support ALAC at all.
func (s *Server) handleSongStream(w http.ResponseWriter, r *http.Request) {
	if !s.appleReady(w) {
		return
	}

	codec, err := stream.ParseCodec(r.URL.Query().Get("codec"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}

	id := chi.URLParam(r, "id")
	variants, err := s.stream.Variants(r.Context(), id)
	if err != nil {
		if errors.Is(err, stream.ErrNoFairPlayAsset) {
			s.streamAAC(w, r, id)
			return
		}
		s.streamError(w, r, err)
		return
	}
	variant, ok := stream.Pick(variants, codec)
	if !ok {
		writeError(w, http.StatusNotFound, "no_variant",
			"this track has no rendition in the requested codec")
		return
	}

	// A genuine seek (a range starting past byte 0, or an explicit end) would
	// mean re-decrypting from the start, so that's declined. But `bytes=0-` —
	// "give me everything, starting from the top" — is exactly what a 200
	// response already provides, and it's what ffmpeg/libmpv's HTTP protocol
	// sends by default on every open() to probe seekability, not just on an
	// actual seek. RFC 7233 explicitly permits a server to ignore Range and
	// answer 200 with the full body, so treat that one shape as no Range at
	// all instead of hard-failing every player that opens this URL.
	if rng := r.Header.Get("Range"); rng != "" && !isWholeBodyRange(rng) {
		w.Header().Set("Accept-Ranges", "none")
		writeError(w, http.StatusRequestedRangeNotSatisfiable, "range_unsupported",
			"this endpoint streams sequentially; download the track for seekable playback")
		return
	}

	w.Header().Set("Content-Type", "audio/mp4")
	w.Header().Set("Accept-Ranges", "none")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Orchard-Codec", string(variant.Codec))
	w.WriteHeader(http.StatusOK)

	// Headers are already sent, so a mid-stream failure can only be logged and
	// the connection dropped; the client sees a truncated file.
	if err := s.stream.Open(r.Context(), id, variant, w, nil); err != nil {
		if !errors.Is(err, r.Context().Err()) {
			writeInternalError(nopWriter{}, r, err)
		}
	}
}

// streamAAC serves a track that has no FairPlay HLS asset by way of Apple's
// Widevine web-playback AAC-256 asset (the same path download.Manager falls
// back to). The asset is fetched and decrypted whole in memory first — it is
// only a few MB — so any failure is reported cleanly before a response is
// committed.
//
// It deliberately behaves like the FairPlay /stream above: a plain 200 with
// the whole body, Accept-Ranges: none, and the same bytes=0- Range allowance.
// There is no cache here, so honouring a genuine seek would re-fetch and
// re-decrypt the entire track for every probe — and libmpv probes hard the
// moment it sees Accept-Ranges: bytes, which stalls playback entirely.
// Downloaded copies are the seekable option.
//
// Resolving the asset (web-playback lookup + license exchange + CDN download)
// takes a few seconds against Apple, and a media player abandons an open()
// that produces no response headers for ~5s. So the 200 and its headers are
// flushed to the client first — the open then succeeds and the player waits
// on the body under its far more lenient read timeout — and only then does
// PrepareAAC run. A failure after that point can only be logged and the
// connection dropped, exactly as the FairPlay stream above already accepts.
func (s *Server) streamAAC(w http.ResponseWriter, r *http.Request, id string) {
	if rng := r.Header.Get("Range"); rng != "" && !isWholeBodyRange(rng) {
		w.Header().Set("Accept-Ranges", "none")
		writeError(w, http.StatusRequestedRangeNotSatisfiable, "range_unsupported",
			"this endpoint streams sequentially; download the track for seekable playback")
		return
	}

	dev, mut, err := s.tokens(r.Context())
	if err != nil {
		s.streamError(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "audio/mp4")
	w.Header().Set("Accept-Ranges", "none")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Orchard-Codec", "aac")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	asset, keys, err := s.webplayback.PrepareAAC(r.Context(), id, dev, mut)
	if err != nil {
		if !errors.Is(err, r.Context().Err()) {
			writeInternalError(nopWriter{}, r, err)
		}
		return
	}
	if err := webplayback.DecryptAAC(asset, keys, w); err != nil {
		if !errors.Is(err, r.Context().Err()) {
			writeInternalError(nopWriter{}, r, err)
		}
	}
}

// isWholeBodyRange reports whether rng is functionally identical to no Range
// header at all — "bytes=0-", with no explicit end and no second range in a
// multi-range request. Anything else (a non-zero start, an explicit end, a
// unit other than bytes, multiple ranges, garbage) is a genuine partial
// request and stays refused.
func isWholeBodyRange(rng string) bool {
	spec, ok := strings.CutPrefix(strings.TrimSpace(rng), "bytes=")
	return ok && spec == "0-"
}

// appleReady rejects the request unless the daemon holds a live session.
func (s *Server) appleReady(w http.ResponseWriter) bool {
	if s.apple.Status().State != apple.StateReady {
		writeError(w, http.StatusServiceUnavailable, "apple_not_ready",
			"the Apple Music session is not signed in; run setup first")
		return false
	}
	return true
}

func (s *Server) streamError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, stream.ErrNoVariant):
		writeError(w, http.StatusNotFound, "no_variant", err.Error())
	case errors.Is(err, stream.ErrNoFairPlayAsset):
		// Only /variants reaches this — /stream falls back to the Widevine
		// AAC path. There are no selectable renditions on that path.
		writeError(w, http.StatusUnprocessableEntity, "no_fairplay_asset",
			"This track has no FairPlay HLS renditions (an older AAC-only "+
				"catalog item); /stream and downloads serve it over the "+
				"Widevine AAC path instead.")
	case errors.Is(err, apple.ErrNotReady):
		writeError(w, http.StatusServiceUnavailable, "apple_not_ready", err.Error())
	default:
		writeInternalError(w, r, err)
	}
}

// nopWriter lets writeInternalError log without touching a response that has
// already been committed.
type nopWriter struct{}

func (nopWriter) Header() http.Header         { return http.Header{} }
func (nopWriter) Write(b []byte) (int, error) { return len(b), nil }
func (nopWriter) WriteHeader(int)             {}
