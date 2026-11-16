package handler

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/f33d3r/feed-engine/internal/identity"
	"github.com/f33d3r/feed-engine/internal/model"
)

// Sessions are elohim-veni's. This file renders them and carries a person's
// requests to end them; it holds none.

// sessionsPartial handles GET /api/sessions.
// Returns the sessions_list Fragment rendered on the server.
func (h *Handler) sessionsPartial(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if viewerAccountID(user) == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sessions, err := h.accountSessions(r, user)
	if err != nil {
		log.Printf("[sessions] listing for @%s: %v", user.Handle, err)
		htmxError(w, r, "Your devices could not be listed right now — try again.", http.StatusServiceUnavailable)
		return
	}
	h.renderPartial(w, "sessions_list", map[string]interface{}{
		"Sessions":         sessions,
		"CurrentSessionID": user.SessionID,
	})
}

// accountSessions lists the viewer's open sessions, the one this request
// arrived on marked current and first.
func (h *Handler) accountSessions(r *http.Request, user *model.User) ([]identity.SessionInfo, error) {
	list, err := h.identity.Client().ListSessions(r.Context(), user.ID)
	if err != nil {
		return nil, err
	}
	out := make([]identity.SessionInfo, 0, len(list))
	for _, s := range list {
		s.IsCurrent = user.SessionID != "" && s.ID == user.SessionID
		if s.IsCurrent {
			out = append([]identity.SessionInfo{s}, out...)
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// sessionRevokeEvent handles event_type=session.revoke dispatched from the /events gateway.
// Returns an empty string — HTMX outerHTML swap removes the revoked session card.
func (h *Handler) sessionRevokeEvent(w http.ResponseWriter, r *http.Request, rawBodyMap map[string]json.RawMessage) {
	user := h.userFromRequest(w, r)
	if viewerAccountID(user) == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sessionID := r.FormValue("session_id")
	if sessionID == "" && rawBodyMap != nil {
		if err := json.Unmarshal(rawBodyMap["session_id"], &sessionID); err != nil {
			http.Error(w, "session_id must be a string", http.StatusBadRequest)
			return
		}
	}
	if sessionID == "" {
		http.Error(w, "session_id required", http.StatusBadRequest)
		return
	}
	if err := h.revokeOneSession(r, user, sessionID); err != nil {
		log.Printf("[sessions] revoke %s for @%s: %v", sessionID, user.Handle, err)
		status := http.StatusBadRequest
		if identity.IsUnavailable(err) {
			status = http.StatusServiceUnavailable
		}
		htmxError(w, r, "Could not revoke session", status)
		return
	}
	// Empty body — hx-swap="outerHTML" on the card collapses it to nothing.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

// revokeOneSession ends one of the viewer's sessions. The authority refuses a
// session that is not this account's.
func (h *Handler) revokeOneSession(r *http.Request, user *model.User, sessionID string) error {
	if err := h.identity.Client().DeleteSession(r.Context(), user.ID, sessionID); err != nil {
		return err
	}
	// A revoked session must not keep resolving from this process's cache.
	h.evictCachedAccount(user.ID, GetSessionToken(r))
	return nil
}

// errSessionsNotEnded says sign-out-everywhere could not be confirmed.
var errSessionsNotEnded = errors.New("sessions could not be ended")

// endAccountSessions signs out every device on the account, except the one
// keepToken opens. A password change is often someone locking a thief out,
// which only works if sessions opened with the old password end with it, and a
// ban means nothing while the banned person is still signed in. Cached copies
// go too, so a revoked session does not keep working until its cache entry
// expires.
func (h *Handler) endAccountSessions(r *http.Request, userID, keepToken string) error {
	h.evictCachedAccount(userID, keepToken)
	if err := h.identity.Client().RevokeAccountSessions(r.Context(), userID, keepToken); err != nil {
		return errors.Join(errSessionsNotEnded, err)
	}
	return nil
}

// evictCachedAccount drops every cached resolution of the account on every
// device, except the entry for keepToken when one is named. Anything that
// changes what an account is — role, capability, verification, standing,
// profile, interest vector — calls it, so the change applies on the account's
// next request rather than when the cache TTL runs out.
func (h *Handler) evictCachedAccount(userID, keepToken string) {
	if userID == "" {
		return
	}
	h.sessionCache.Range(func(key, value any) bool {
		entry, ok := value.(sessionEntry)
		if ok && entry.user != nil && entry.user.ID == userID && key != keepToken {
			h.sessionCache.Delete(key)
		}
		return true
	})
}
