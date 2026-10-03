package api

import (
	"errors"
	"net/http"
	"strings"

	"orchard/internal/apple"
)

// handleAppleStatus reports the Apple session and the wrapper install state.
func (s *Server) handleAppleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.apple.Status())
}

type appleLoginRequest struct {
	AppleID  string `json:"appleId"`
	Password string `json:"password"`
}

// handleAppleLogin starts the wrapper with credentials. It blocks until the
// session is ready or Apple asks for a 2FA code, so callers should read
// "state" from the body rather than assume 200 means logged in.
func (s *Server) handleAppleLogin(w http.ResponseWriter, r *http.Request) {
	var req appleLoginRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}

	req.AppleID = strings.TrimSpace(req.AppleID)
	if req.AppleID == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "invalid_body", "appleId and password are required")
		return
	}
	// Credentials reach wrapper as "user:password", so a colon in the Apple ID
	// would split in the wrong place.
	if strings.Contains(req.AppleID, ":") {
		writeError(w, http.StatusBadRequest, "invalid_body", "appleId must not contain a colon")
		return
	}

	status, err := s.apple.Login(r.Context(), req.AppleID, req.Password)
	switch {
	case errors.Is(err, apple.ErrBusy):
		writeError(w, http.StatusConflict, "login_in_progress", err.Error())
	case err != nil:
		// The status body carries the detail, so surface it instead of a 500.
		writeJSON(w, http.StatusBadGateway, status)
	default:
		writeJSON(w, http.StatusOK, status)
	}
}

type apple2FARequest struct {
	Code string `json:"code"`
}

// handleApple2FA delivers the verification code. Upstream polls for it for only
// 60 seconds after prompting, so a late call fails with 409.
func (s *Server) handleApple2FA(w http.ResponseWriter, r *http.Request) {
	var req apple2FARequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}

	status, err := s.apple.Submit2FA(r.Context(), req.Code)
	switch {
	case errors.Is(err, apple.ErrNot2FA):
		writeError(w, http.StatusConflict, "not_awaiting_2fa", "the daemon is not waiting for a verification code")
	case err != nil:
		writeJSON(w, http.StatusBadGateway, status)
	default:
		writeJSON(w, http.StatusOK, status)
	}
}

// handleAppleLogout stops the daemon and deletes the persisted session.
func (s *Server) handleAppleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.apple.Logout(r.Context()); err != nil {
		writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.apple.Status())
}
