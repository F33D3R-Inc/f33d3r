package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

func (h *Handler) notificationsPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	// Shell only — notification list is populated by HTMX on load
	// and refreshed live whenever the SSE notify event fires.
	h.render(w, r, "notifications.html", h.withRail(map[string]interface{}{
		"User":       user,
		"Title":      "Notifications",
		"Filter":     notifFilterName(r.URL.Query().Get("filter")),
		"SessionID":  uuid.New().String(),
		"ShowScores": h.cfg.ShowScores,
		"Themes":     ThemesWithActive(user.ThemeID),
	}, user, "default"))
}

// facetNotifications returns the notification list HTML for HTMX swap.
// Called on page load and whenever the SSE notify event fires.
// GET /facets/notifications
func (h *Handler) facetNotifications(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.writeNotificationList(w, user, r.URL.Query().Get("filter"))
}

// writeNotificationList writes the viewer's notification items — the body of
// #notification-list — for the load, the live refresh and the mark-all-read
// answer alike.
func (h *Handler) writeNotificationList(w http.ResponseWriter, user *model.User, filter string) {
	if user == nil || h.db == nil {
		h.renderPartial(w, "stateEmpty", map[string]interface{}{
			"Title": "All caught up", "Sub": "No notifications yet",
		})
		return
	}
	groups, _, err := dbpkg.ListNotificationGroups(h.db, user.ID, 50, time.Now(), notifFilterKinds[filter]...)
	if err != nil {
		// Saying "All caught up" here would be a false statement about the inbox.
		log.Printf("[notifications] fetch for %s: %v", user.ID, err)
		h.renderPartial(w, "stateEmpty", map[string]interface{}{
			"Title": "Notifications are unavailable", "Sub": "They are safe. Try again in a moment.",
		})
		return
	}
	if len(groups) == 0 {
		h.renderPartial(w, "stateEmpty", map[string]interface{}{
			"Title": "All caught up", "Sub": "No notifications yet",
		})
		return
	}
	var actorIDs []string
	for _, g := range groups {
		if len(g.ActorIDs) > 0 {
			actorIDs = append(actorIDs, g.ActorIDs[0])
		}
	}
	actors, err := dbpkg.GetUsersByIDs(h.db, actorIDs)
	if err != nil {
		log.Printf("[notifications] actors for %s: %v", user.ID, err)
	}
	for _, g := range groups {
		h.renderPartial(w, "notif_item", notificationEntry(h, g, actors))
	}
}

// notifFilterKinds are the kinds each notification tab shows. No entry means all.
var notifFilterKinds = map[string][]string{
	"mention": {"mention"},
	"reply":   {"reply", "quote"},
	"like":    {"like"},
	"follow":  {"follow"},
	"repost":  {"repost"},
	"tip":     {"tip"},
}

// notificationEntry shapes one grouped notification for the notif_item Facet.
func notificationEntry(h *Handler, g *dbpkg.NotificationGroup, actors map[string]*model.User) dbpkg.Notification {
	n := dbpkg.Notification{
		ID: g.ID, Type: g.Kind, TargetID: g.TargetID, TargetType: g.TargetType,
		IsRead: g.IsRead, CreatedAt: g.CreatedAt, TimeAgo: TimeAgo(g.CreatedAt),
		Others: g.ActorCount - 1, Preview: g.Preview,
	}
	if len(g.ActorIDs) > 0 {
		if a, ok := actors[g.ActorIDs[0]]; ok && a != nil {
			n.ActorID, n.ActorHandle, n.ActorName, n.ActorAvatar = a.ID, a.Handle, a.DisplayName, a.AvatarURL
		}
	}
	if g.AmountUAET > 0 {
		n.AmountAET = fmt.Sprintf("%.2f", float64(g.AmountUAET)/1_000_000)
	}
	switch g.TargetType {
	case "work", "post":
		n.Href = "/work/" + g.TargetID
		if n.Preview == "" && g.TargetID != "" {
			n.Preview = strings.TrimSpace(dbpkg.GetWorkBodyPreview(h.db, g.TargetID))
		}
	case "live":
		n.Href = "/live/" + g.TargetID
	case "user":
		if n.ActorHandle != "" {
			n.Href = "/" + n.ActorHandle
		}
	}
	return n
}

func (h *Handler) notificationsCount(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	count := 0
	if h.db != nil && user != nil {
		count = dbpkg.CountUnreadNotifications(h.db, user.ID)
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

	// Register with the FA Live session registry so publishers can push
	// immediately when mutations happen (likes, tips, follows, etc.).
	// Register under BOTH the ACCOUNT (persona — messaging pipe) and the PIAL (person — notify/
	// wallet/likes pipe). The account index makes a DM reach only the addressed persona's devices;
	// the PIAL index keeps person-level signals reaching every open persona. One connection, two keys.
	var registryCh <-chan SSEEvent
	var registryDone <-chan struct{}
	var registryCleanup func()
	if user != nil && user.ID != "" {
		registryCh, registryDone, registryCleanup = RegisterSSESession(user.ID, user.PIALID)
	}
	if registryCleanup == nil {
		registryCleanup = func() {}
	}

	sendNotify := func() {
		if h.db == nil || user == nil {
			return
		}
		count := dbpkg.CountUnreadNotifications(h.db, user.ID)
		if h.partial != nil {
			var buf bytes.Buffer
			if err := h.partial.ExecuteTemplate(&buf, "notif_badge", map[string]interface{}{
				"PIALID": user.PIALID, "UnreadCount": count,
			}); err != nil {
				log.Printf("[notify] rendering notif_badge: %v", err)
				return
			}
			writeSSEFrame(w, "notify", buf.String())
			flusher.Flush()
		}
	}

	sendBalance := func() {
		if h.cfg.AinSophURL == "" || user == nil || user.PIALID == "" {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			h.cfg.AinSophURL+"/v1/balance/"+user.PIALID, nil)
		if err != nil {
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp == nil {
			return
		}
		defer resp.Body.Close()
		var acc struct {
			BalanceAet string `json:"balance_aet"`
		}
		if json.NewDecoder(resp.Body).Decode(&acc) == nil && acc.BalanceAet != "" {
			writeSSEFrame(w, "balance", acc.BalanceAet)
			flusher.Flush()
		}
	}

	// Send initial state immediately on connect
	sendNotify()
	sendBalance()

	replayed := map[int64]bool{}
	if user != nil && user.ID != "" {
		replayed = resumeWebStream(w, r, user.ID, user.PIALID)
		flusher.Flush()
	}

	// Mark user online while SSE connection is alive
	if user != nil && user.Handle != "" && user.Handle != "you" {
		h.onlineUsers.Store(user.Handle, time.Now())
		defer h.onlineUsers.Delete(user.Handle)
	}

	ticker := time.NewTicker(15 * time.Second)
	defer registryCleanup()
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-registryDone:
			// Evicted by a newer connection for this account — release this stale one cleanly.
			return
		case <-ticker.C:
			// Refresh online timestamp
			if user != nil && user.Handle != "" && user.Handle != "you" {
				h.onlineUsers.Store(user.Handle, time.Now())
			}
			// All events now pushed from mutation sources (FA Live).
			// balance: pushed by Ain Soph via /api/internal/balance-update.
			// notify:  pushed when Herald announces a new unread count.
			// new_post: fan-out from post publish handler.
			//
			// The tick still has to put bytes on the wire. A stream that writes
			// nothing between events is indistinguishable from a stream that has
			// died: the proxy in front of it times the connection out on idle and
			// the browser only finds out when it next expects something. A comment
			// line is the protocol's own keep-alive — it dispatches no event, so
			// nothing on the page reacts to it.
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case event, ok := <-registryCh:
			if !ok {
				return
			}
			if replayed[event.ID] {
				continue
			}
			// A JSON-only event has nothing for a web page, and a blank frame would
			// wipe whatever its listener swaps.
			if event.Data == "" && event.Type != sseResync {
				continue
			}
			// Immediately-pushed FA Live event from a mutation handler. Data is pre-rendered,
			// multi-line HTML — writeSSEFrameID emits one `data:` line per line so the browser
			// reassembles the exact fragment (a raw single-line write delivers it EMPTY; see
			// writeSSEFrame for why).
			writeSSEFrameID(w, event.ID, event.Type, event.Data)
			flusher.Flush()
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

// profilesMini returns [{handle, avatar_url, realm_level}] for a batch of handles.
// Used by messages.js to render real avatars + realm rings in the conversation list.
func (h *Handler) profilesMini(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("handles")
	if raw == "" || h.db == nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
		return
	}
	parts := strings.Split(raw, ",")
	handles := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			handles = append(handles, p)
		}
	}
	if len(handles) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
		return
	}
	if len(handles) > 50 {
		handles = handles[:50]
	}

	type mini struct {
		Handle     string `json:"handle"`
		AvatarURL  string `json:"avatar_url"`
		RealmLevel int    `json:"realm_level"`
	}

	rows, err := h.db.Query(`
		SELECT u.handle, COALESCE(p.avatar_url,''), COALESCE(u.realm,1)
		FROM users u
		LEFT JOIN user_profiles p ON p.user_id = u.id
		WHERE u.handle = ANY($1)
	`, pq.Array(handles))
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
		return
	}
	defer rows.Close()

	out := []mini{}
	for rows.Next() {
		var m mini
		if rows.Scan(&m.Handle, &m.AvatarURL, &m.RealmLevel) == nil {
			out = append(out, m)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (h *Handler) markNotificationsRead(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if h.db != nil && user != nil {
		if err := dbpkg.MarkAllNotificationsRead(h.db, user.ID); err != nil {
			log.Printf("[notifications] mark all read for %s: %v", user.ID, err)
			http.Error(w, "could not mark notifications read", http.StatusBadGateway)
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.writeNotificationList(w, user, "")
}

// notifFilterName keeps only a filter this page knows, because the value is written
// back into the page.
func notifFilterName(filter string) string {
	if _, ok := notifFilterKinds[filter]; ok {
		return filter
	}
	return ""
}
