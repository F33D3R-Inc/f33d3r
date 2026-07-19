package handler

import (
	"bytes"
	"log"
	"net/http"
	"strings"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/gnosis"
	"github.com/f33d3r/feed-engine/internal/model"
)

// maxMessageBytes caps a single plaintext message body (plain mode). Generous for
// text, but bounds the unauthenticated-storage surface.
const maxMessageBytes = 8000

// messagesPage renders the Gnosis messaging shell — a conversation-list pane and a
// thread pane, both populated by HTMX. Each conversation is either 'plain' (server-
// readable, rendered here) or 'sealed' (E2EE, decrypted client-side).
func (h *Handler) messagesPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	rail := h.railData(user, "default")
	h.render(w, "messages.html", map[string]interface{}{
		"User":           user,
		"Title":          "Messages",
		"TrendingTags":   rail["TrendingTags"],
		"SuggestedUsers": rail["SuggestedUsers"],
		"RailContext":    rail["RailContext"],
		"RailNewsItems":  rail["RailNewsItems"],
		"RailNewsLabel":  rail["RailNewsLabel"],
	})
}

// facetConvoList returns the viewer's conversation rows for HTMX.
// GET /facets/messages_convos
func (h *Handler) facetConvoList(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if user == nil || h.db == nil {
		h.renderPartial(w, "stateEmpty", map[string]interface{}{
			"Title": "No messages", "Sub": "Start a conversation"})
		return
	}
	rows, _ := gnosis.ListConversationsForAccount(h.db, user.ID)

	// Server-authoritative search: when the sidebar search box sends ?q=, filter the
	// rows here so the browser only ever receives rendered matches (no client state).
	if q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q"))); q != "" {
		filtered := rows[:0]
		for _, row := range rows {
			if strings.Contains(strings.ToLower(row.OtherDisplay), q) ||
				strings.Contains(strings.ToLower(row.OtherHandle), q) ||
				strings.Contains(strings.ToLower(row.Title), q) {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
		if len(rows) == 0 {
			h.renderPartial(w, "stateEmpty", map[string]interface{}{
				"Title": "No matches", "Sub": "No conversations match your search"})
			return
		}
	} else if len(rows) == 0 {
		h.renderPartial(w, "stateEmpty", map[string]interface{}{
			"Title": "No messages yet", "Sub": "Start a conversation below"})
		return
	}
	for _, row := range rows {
		h.renderPartial(w, "gnosis_convo_item", row)
	}
}

// facetThread returns the message list + composer for one conversation.
// GET /facets/messages_thread?c=<id>
func (h *Handler) facetThread(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	convoID := r.URL.Query().Get("c")
	if convoID == "" {
		h.renderPartial(w, "gnosis_thread_empty", nil)
		return
	}
	h.renderThread(w, user, convoID)
}

// renderThread renders the thread fragment for a conversation the user belongs to.
// Shared by facetThread and handleCreateConvo.
func (h *Handler) renderThread(w http.ResponseWriter, user *model.User, convoID string) {
	ok, _ := gnosis.IsMember(h.db, convoID, user.ID)
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	convo, err := gnosis.GetConversation(h.db, convoID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	msgs, _ := gnosis.ListMessages(h.db, convoID, user.ID, 200)
	_ = gnosis.MarkRead(h.db, convoID, user.ID)
	handle, display, avatar := gnosis.OtherParticipant(h.db, convoID, user.ID)
	sealed := convo.Mode == gnosis.ModeSealed
	h.renderPartial(w, "gnosis_thread", map[string]interface{}{
		"Convo":        convo,
		"Messages":     msgs,
		"ViewerID":     user.ID,
		"Sealed":       sealed,
		"OtherHandle":  handle,
		"OtherDisplay": display,
		"OtherAvatar":  avatar,
	})
}

// handleCreateConvo starts (or reuses) a 1:1 conversation with a @handle. The mode
// is decided from both participants' PIAL 18+ status and fixed at creation.
// POST /messages/new  (form: handle)
func (h *Handler) handleCreateConvo(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	handle := strings.TrimPrefix(strings.TrimSpace(r.FormValue("handle")), "@")
	if handle == "" {
		http.Error(w, "handle required", http.StatusBadRequest)
		return
	}
	target, err := dbpkg.GetUserByHandle(h.db, handle)
	if err != nil || target == nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	if target.ID == user.ID {
		http.Error(w, "cannot message yourself", http.StatusBadRequest)
		return
	}
	convoID, _ := gnosis.FindDirectConversation(h.db, user.ID, target.ID)
	if convoID == "" {
		targetPIAL := dbpkg.ResolvePIAL(h.db, target.ID)
		mode := gnosis.ModeForParticipants(h.db, []string{user.PIALID, targetPIAL})
		members := []gnosis.Member{
			{AccountID: user.ID, PIALID: user.PIALID},
			{AccountID: target.ID, PIALID: targetPIAL},
		}
		convoID, err = gnosis.CreateConversation(h.db, user.ID, members, mode)
		if err != nil {
			log.Printf("[gnosis] create conversation: %v", err)
			http.Error(w, "could not create conversation", http.StatusInternalServerError)
			return
		}
	}
	// Refresh the sidebar list, then return the thread fragment.
	w.Header().Set("HX-Trigger", "gnosis-refresh")
	h.renderThread(w, user, convoID)
}

// handleSendMessage posts a message. Plain conversations store server-readable text
// and fan out server-rendered bubbles over SSE. Sealed conversations are refused
// here — they are sent through the encrypted client path so the server never sees
// plaintext (wired in sealed-mode).
// POST /messages/send  (form: c, body)
func (h *Handler) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	convoID := r.FormValue("c")
	body := strings.TrimSpace(r.FormValue("body"))
	if convoID == "" || body == "" {
		http.Error(w, "message required", http.StatusBadRequest)
		return
	}
	if len(body) > maxMessageBytes {
		http.Error(w, "message too long", http.StatusRequestEntityTooLarge)
		return
	}
	ok, _ := gnosis.IsMember(h.db, convoID, user.ID)
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	convo, err := gnosis.GetConversation(h.db, convoID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if convo.Mode == gnosis.ModeSealed {
		http.Error(w, "sealed conversation: send via the encrypted client path", http.StatusConflict)
		return
	}

	msg, err := gnosis.InsertPlainMessage(h.db, convoID, user.ID, body)
	if err != nil {
		http.Error(w, "send failed", http.StatusInternalServerError)
		return
	}
	h.fanoutMessage(convoID, user.ID, msg)

	// Optimistic: return the sender's own out-bubble for HTMX append.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "gnosis_bubble", map[string]interface{}{
		"M": msg, "Variant": "out",
	})
}

// handleAddMember adds a participant to a conversation, enforcing no-silent-downgrade:
// an unverified account cannot join a sealed conversation.
// POST /messages/add-member  (form: c, handle)
func (h *Handler) handleAddMember(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	convoID := r.FormValue("c")
	handle := strings.TrimPrefix(strings.TrimSpace(r.FormValue("handle")), "@")
	if convoID == "" || handle == "" {
		http.Error(w, "missing fields", http.StatusBadRequest)
		return
	}
	if ok, _ := gnosis.IsMember(h.db, convoID, user.ID); !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	// A direct message is strictly 1:1 — never silently expand it into a group.
	if convo, err := gnosis.GetConversation(h.db, convoID); err != nil || !convo.IsGroup {
		http.Error(w, "cannot add members to a direct message", http.StatusConflict)
		return
	}
	target, err := dbpkg.GetUserByHandle(h.db, handle)
	if err != nil || target == nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	targetPIAL := dbpkg.ResolvePIAL(h.db, target.ID)
	if err := gnosis.AddMember(h.db, convoID, target.ID, targetPIAL); err != nil {
		if err == gnosis.ErrCannotDowngradeSealed {
			http.Error(w, "This conversation is end-to-end encrypted; only 18+ verified accounts can join.", http.StatusConflict)
			return
		}
		http.Error(w, "could not add member", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// fanoutMessage renders the in-bubble once and pushes it to every other member's
// live SSE connection.
func (h *Handler) fanoutMessage(convoID, senderID string, msg gnosis.Message) {
	accounts, _ := gnosis.MemberAccounts(h.db, convoID)
	var buf bytes.Buffer
	if err := h.partial.ExecuteTemplate(&buf, "gnosis_bubble", map[string]interface{}{
		"M": msg, "Variant": "in",
	}); err != nil {
		return
	}
	html := buf.String()
	for _, acct := range accounts {
		if acct == senderID {
			continue
		}
		PublishToAccount(acct, SSEEvent{Type: "gnosis_message", Data: html})
	}
}
