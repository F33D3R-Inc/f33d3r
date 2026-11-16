package api

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"os"
	"strings"

	"f33d3r.com/ios/devserver/internal/store"
)

type loginRequest struct {
	Handle     string `json:"handle"`
	Password   string `json:"password"`
	DeviceName string `json:"device_name"`
	// Code is the second factor for an account with TOTP on: six digits, or a
	// backup code. As feed-engine's apiLoginRequest.
	Code string `json:"code"`
}

// devSecondFactor is the one code this server accepts for an account with
// TOTP on. It holds no authenticator secrets (there is no enrolment route
// here), so the code a tester types is the one they configured in
// F33D3R_DEV_TOTP_CODE; with none configured, every code is wrong.
func devSecondFactor(code string) bool {
	want := os.Getenv("F33D3R_DEV_TOTP_CODE")
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	return want != "" && subtle.ConstantTimeCompare([]byte(code), []byte(want)) == 1
}

// login — POST /api/v1/auth/login.
//
// One error for an unknown handle and a wrong password, so the form cannot be
// used to discover which handles exist.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "The request could not be read.")
		return
	}
	handle := store.NormalizeHandle(req.Handle)
	if !store.HandleRe.MatchString(handle) || req.Password == "" {
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "Invalid handle or password.")
		return
	}
	u, err := s.store.CheckPassword(r.Context(), handle, req.Password)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "invalid_credentials", "Invalid handle or password.")
			return
		}
		serverError(w, err)
		return
	}
	// The second factor, answered as feed-engine's /api/v1/auth/login does.
	if u.TwoFAEnabled {
		if strings.TrimSpace(req.Code) == "" {
			writeError(w, http.StatusUnauthorized, "two_fa_required", "Enter the code from your authenticator app.")
			return
		}
		if !devSecondFactor(req.Code) {
			writeError(w, http.StatusUnauthorized, "two_fa_invalid", "That code is not right. Try again.")
			return
		}
	}
	s.issueSession(w, r, u, http.StatusOK, req.DeviceName)
}

type signupRequest struct {
	Handle      string `json:"handle"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
	DeviceName  string `json:"device_name"`
}

// signup — POST /api/v1/auth/signup. Handle and password only: F33D3R takes no
// email or phone.
func (s *Server) signup(w http.ResponseWriter, r *http.Request) {
	var req signupRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "The request could not be read.")
		return
	}
	handle := store.NormalizeHandle(req.Handle)
	if !store.HandleRe.MatchString(handle) {
		writeError(w, http.StatusBadRequest, "invalid_handle", "Handles are 1–30 letters, numbers, underscores or dashes.")
		return
	}
	if len(req.Password) < 8 {
		writeError(w, http.StatusBadRequest, "weak_password", "Passwords are at least 8 characters.")
		return
	}
	taken, err := s.store.IsHandleTaken(r.Context(), handle)
	if err != nil {
		serverError(w, err)
		return
	}
	if taken {
		writeError(w, http.StatusConflict, "handle_taken", "That handle is taken.")
		return
	}
	u, err := s.store.CreateUser(r.Context(), store.NewUserParams{
		Handle:      handle,
		Password:    req.Password,
		DisplayName: strings.TrimSpace(req.DisplayName),
	})
	if err != nil {
		serverError(w, err)
		return
	}
	s.issueSession(w, r, u, http.StatusCreated, req.DeviceName)
}

func (s *Server) issueSession(w http.ResponseWriter, r *http.Request, u *store.User, status int, deviceName string) {
	token, expires, err := s.store.CreateSession(r.Context(), u, deviceName, clientIP(r))
	if err != nil {
		serverError(w, err)
		return
	}
	unread, _ := s.store.UnreadCount(r.Context(), u.ID)
	writeJSON(w, status, SessionDTO{
		Token:            token,
		ExpiresAt:        expires,
		User:             meDTO(u, unread),
		NeedsBackupCodes: u.NeedsBackupCodes,
	})
}

// logout — POST /api/v1/auth/logout. Revokes the presented token; 204.
func (s *Server) logout(w http.ResponseWriter, r *http.Request, _ *store.User) {
	if err := s.store.DeleteSession(r.Context(), bearerToken(r)); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// me — GET /api/v1/me.
func (s *Server) me(w http.ResponseWriter, r *http.Request, u *store.User) {
	unread, err := s.store.UnreadCount(r.Context(), u.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, meDTO(u, unread))
}
