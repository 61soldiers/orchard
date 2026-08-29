// Package api exposes Orchard's HTTP surface.
package api

import (
	"crypto/sha256"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"orchard/internal/apple"
	"orchard/internal/catalog"
	"orchard/internal/config"
	"orchard/internal/download"
	"orchard/internal/ratelimit"
	"orchard/internal/store"
	"orchard/internal/stream"
	"orchard/internal/webplayback"
)

// Server holds the API dependencies and its router.
type Server struct {
	cfg         *config.Config
	store       *store.Store
	apple       *apple.Manager
	catalog     *catalog.Client
	stream      *stream.Client
	webplayback *webplayback.Client
	tokens      catalog.TokenSource
	downloads   *download.Manager
	keyDigest   [32]byte
	limiter     *ratelimit.Limiter
	router      chi.Router
}

// New wires up the router. tokens and wp power the Widevine AAC fallback for
// tracks with no FairPlay HLS asset (see handleSongStream).
func New(cfg *config.Config, st *store.Store, appleMgr *apple.Manager, cat *catalog.Client,
	str *stream.Client, tokens catalog.TokenSource, wp *webplayback.Client, dl *download.Manager) *Server {
	s := &Server{
		cfg:         cfg,
		store:       st,
		apple:       appleMgr,
		catalog:     cat,
		stream:      str,
		webplayback: wp,
		tokens:      tokens,
		downloads:   dl,
		keyDigest:   sha256.Sum256([]byte(cfg.APIKey)),
		limiter:     ratelimit.New(cfg.RateLimit, cfg.RateBurst),
	}
	s.router = s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

func (s *Server) routes() chi.Router {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	if s.cfg.TrustProxyHeaders {
		r.Use(middleware.RealIP)
	}
	r.Use(middleware.Recoverer)
	r.Use(requestLogger)

	r.Get("/healthz", s.handleHealth)

	r.Route("/v1", func(r chi.Router) {
		r.Use(s.requireAPIKey)
		r.Use(s.rateLimit)

		r.Get("/apple/status", s.handleAppleStatus)
		r.Post("/apple/login", s.handleAppleLogin)
		r.Post("/apple/2fa", s.handleApple2FA)
		r.Delete("/apple/session", s.handleAppleLogout)

		r.Get("/storefront", s.handleStorefront)
		r.Get("/search", s.handleSearch)
		r.Get("/albums/{id}", s.handleAlbum)
		r.Get("/artists/{id}", s.handleArtist)
		r.Get("/playlists/{id}", s.handlePlaylist)
		r.Get("/songs/{id}", s.handleSong)
		r.Get("/songs/{id}/lyrics", s.handleLyrics)
		r.Get("/songs/{id}/variants", s.handleSongVariants)
		r.Get("/songs/{id}/stream", s.handleSongStream)

		r.Get("/me/recommendations", s.handleRecommendations)
		r.Get("/me/recent/played", s.handleRecentlyPlayed)

		r.Post("/downloads", s.handleCreateDownload)
		r.Get("/downloads", s.handleListDownloads)
		r.Get("/downloads/{id}", s.handleGetDownload)
		r.Get("/downloads/{id}/events", s.handleDownloadEvents)
		r.Delete("/downloads/{id}", s.handleCancelDownload)

		r.Get("/tracks/{id}/file", s.handleTrackFile)
		r.Get("/library/tracks", s.handleLibraryTracks)
		r.Get("/library/albums", s.handleLibraryAlbums)
		r.Get("/library/artists", s.handleLibraryArtists)
		r.Get("/library/stats", s.handleLibraryStats)
	})

	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed for this endpoint")
	})

	return r
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
