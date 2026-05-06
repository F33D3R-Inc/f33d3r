package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

func (h *Handler) notificationsPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	var notifs []dbpkg.Notification
	if h.db != nil && user != nil {
		notifs, _ = dbpkg.GetNotifications(h.db, user.ID, 50)
		for i := range notifs {
			notifs[i].TimeAgo = TimeAgo(notifs[i].CreatedAt)
		}
		dbpkg.ClearUnreadCount(h.db, user.ID)
		user.UnreadCount = 0
	}
	h.render(w, "notifications.html", map[string]interface{}{
		"User":          user,
		"Title":         "Notifications",
		"Themes":        ThemesWithActive(user.ThemeID),
		"Notifications": notifs,
	})
}

func (h *Handler) notificationsCount(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	count := 0
	if h.db != nil && user != nil {
		_ = h.db.QueryRow(
			"SELECT COUNT(*) FROM notifications WHERE user_id = $1 AND is_read = false",
			user.ID,
		).Scan(&count)
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"count":%d}`, count)
}

// ── SSE /api/events — server-sent events, no polling, no JSON ─────────────────
// Pushes HTML fragments directly. Browser receives them; f33d3r.js swaps into DOM.
// Two event types:
//
//	notify  → HTML badge fragment for notification count
//	balance → plain text AET balance string
func (h *Handler) sseEvents(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering if present

	// Helpers
	sendNotify := func() {
		count := 0
		if h.db != nil && user != nil {
			_ = h.db.QueryRow(
				"SELECT COUNT(*) FROM notifications WHERE user_id = $1 AND is_read = false",
				user.ID,
			).Scan(&count)
		}
		var badge string
		if count > 0 {
			label := fmt.Sprintf("%d", count)
			if count > 9 { label = "9+" }
			badge = fmt.Sprintf(`<span class="notif-count">%s</span>`, label)
		}
		fmt.Fprintf(w, "event: notify\ndata: %s\n\n", badge)
		flusher.Flush()
	}

	sendBalance := func() {
		if h.cfg.AinSophURL == "" || user == nil || user.PIALID == "" { return }
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			h.cfg.AinSophURL+"/v1/balance/"+user.PIALID, nil)
		if err != nil { return }
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp == nil { return }
		defer resp.Body.Close()
		var acc struct {
			BalanceAet string `json:"balance_aet"`
		}
		if json.NewDecoder(resp.Body).Decode(&acc) == nil && acc.BalanceAet != "" {
			fmt.Fprintf(w, "event: balance\ndata: %s\n\n", acc.BalanceAet)
			flusher.Flush()
		}
	}

	// Send initial state immediately on connect
	sendNotify()
	sendBalance()

	// Mark user online while SSE connection is alive
	if user != nil && user.Handle != "" && user.Handle != "you" {
		h.onlineUsers.Store(user.Handle, time.Now())
		defer h.onlineUsers.Delete(user.Handle)
	}

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			// Refresh online timestamp
			if user != nil && user.Handle != "" && user.Handle != "you" {
				h.onlineUsers.Store(user.Handle, time.Now())
			}
			sendNotify()
			sendBalance()
		}
	}
}

// userOnlineStatus returns whether a handle is currently connected via SSE.
func (h *Handler) userOnlineStatus(w http.ResponseWriter, r *http.Request) {
	handle := strings.TrimPrefix(r.URL.Path, "/api/online/")
	handle = strings.TrimSpace(handle)
	online := false
	if handle != "" {
		if ts, ok := h.onlineUsers.Load(handle); ok {
			if t, ok := ts.(time.Time); ok {
				online = time.Since(t) < 45*time.Second
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"online":%v}`, online)
}

func (h *Handler) markNotificationsRead(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if h.db != nil {
		_ = dbpkg.MarkAllNotificationsRead(h.db, user.ID)
		dbpkg.ClearUnreadCount(h.db, user.ID)
	}
	w.WriteHeader(http.StatusOK)
}
