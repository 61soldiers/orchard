package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// requireAPIKey rejects any request that does not present the server's key.
func (s *Server) requireAPIKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.keyMatches(presentedKey(r)) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="orchard"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid API key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// rateLimit throttles the /v1 surface to a steady requests/sec budget, so one
// misbehaving client can't hammer Orchard, and by extension Apple, into a
// throttling or ban response. It runs after requireAPIKey so a flood of wrong
// keys is rejected outright rather than eating into the budget.
func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.limiter.Allow() {
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests; slow down")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// presentedKey pulls the key from the Authorization header, falling back to the
// api_key query parameter that browser EventSource clients are stuck with.
func presentedKey(r *http.Request) string {
	if header := r.Header.Get("Authorization"); header != "" {
		if key, ok := strings.CutPrefix(header, "Bearer "); ok {
			return strings.TrimSpace(key)
		}
		return ""
	}
	return r.URL.Query().Get("api_key")
}

// keyMatches compares digests so the check leaks neither content nor length.
func (s *Server) keyMatches(candidate string) bool {
	got := sha256.Sum256([]byte(candidate))
	return subtle.ConstantTimeCompare(got[:], s.keyDigest[:]) == 1
}

// requestLogger logs one line per request. It deliberately omits the query
// string and all headers, either of which can carry the API key.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

		next.ServeHTTP(ww, r)

		slog.LogAttrs(r.Context(), slog.LevelInfo, "request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", ww.Status()),
			slog.Int("bytes", ww.BytesWritten()),
			slog.Duration("duration", time.Since(start)),
			slog.String("request_id", middleware.GetReqID(r.Context())),
		)
	})
}
