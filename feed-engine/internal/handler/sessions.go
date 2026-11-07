package handler

import (
	"encoding/json"
	"log"
	"net/http"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

// sessionsPartial handles GET /api/sessions.
// Returns the sessions_list partial rendered directly as HTML (Lane 2 — read).
func (h *Handler) sessionsPartial(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.ID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	sessions, err := dbpkg.ListUserSessions(h.db, user.ID)
	if err != nil {
		log.Printf("[sessions] ListUserSessions error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Resolve the current session ID to mark IsCurrent on the matching card.
	currentSessionID := ""
	rawToken := GetSessionToken(r)
	if rawToken != "" && h.db != nil {
		hash := dbpkg.HashToken(rawToken)
		h.db.QueryRow(`SELECT id FROM user_sessions WHERE token_hash = $1`, hash).
			Scan(&currentSessionID)
	}

	for i := range sessions {
		sessions[i].IsCurrent = sessions[i].ID == currentSessionID
	}

	h.renderPartial(w, "sessions_list", map[string]interface{}{
		"Sessions":         sessions,
		"CurrentSessionID": currentSessionID,
	})
}

// sessionRevokeEvent handles event_type=session.revoke dispatched from the /events gateway.
// Returns an empty string — HTMX outerHTML swap removes the revoked session card.
func (h *Handler) sessionRevokeEvent(w http.ResponseWriter, r *http.Request, rawBodyMap map[string]json.RawMessage) {
	user := h.userFromRequest(w, r)
	if user == nil || user.ID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	sessionID := r.FormValue("session_id")
	if sessionID == "" && rawBodyMap != nil {
		json.Unmarshal(rawBodyMap["session_id"], &sessionID)
	}
	if sessionID == "" {
		http.Error(w, "session_id required", http.StatusBadRequest)
		return
	}

	if err := dbpkg.RevokeSession(h.db, sessionID, user.ID); err != nil {
		log.Printf("[sessions] RevokeSession error: %v", err)
		htmxError(w, r, "Could not revoke session", http.StatusBadRequest)
		return
	}

	// Return empty HTML — hx-swap="outerHTML" on the card collapses it to nothing.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

// endSessionsAfterPasswordChange signs out every device on the account, except the one
// keepToken names. A password change is often someone locking a thief out, which only
// works if sessions opened with the old password end with it. Cached copies go too, so
// a revoked session doesn't keep working until its cache entry expires.
func (h *Handler) endSessionsAfterPasswordChange(userID, keepToken string) error {
	var err error
	if keepToken == "" {
		err = dbpkg.RevokeAllSessions(h.db, userID)
	} else {
		err = dbpkg.RevokeOtherSessions(h.db, userID, keepToken)
	}
	h.sessionCache.Range(func(key, value any) bool {
		entry, ok := value.(sessionEntry)
		if ok && entry.user != nil && entry.user.ID == userID && key != keepToken {
			h.sessionCache.Delete(key)
		}
		return true
	})
	return err
}
