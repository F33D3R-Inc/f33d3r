package handler

import (
	"bytes"
	"log"
	"net/http"
	"strings"
	"time"

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
	h.render(w, r, "messages.html", h.withRail(map[string]interface{}{
		"User":  user,
		"Title": "Messages",
	}, user, "default"))
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
// Shared by facetThread, handleCreateConvo and the Number path.
func (h *Handler) renderThread(w http.ResponseWriter, user *model.User, convoID string) {
	h.renderThreadWithDraft(w, user, convoID, "")
}

// renderThreadWithDraft is renderThread with an explicit undelivered message to
// render back into the composer.
//
// The draft is the server's, not the browser's: it lives in
// gnosis_pending_messages and is looked up here when the caller does not already
// hold it. It exists in exactly one situation — a conversation that opened
// SEALED while a plaintext message was still waiting to be delivered into it.
// Writing that plaintext into a sealed thread would be a silent downgrade of one
// message, so it is not written; it is handed back to the composer that can seal
// it, and it stays the server's until it is sent.
func (h *Handler) renderThreadWithDraft(w http.ResponseWriter, user *model.User, convoID, draft string) {
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
	if draft == "" {
		if p, found, perr := gnosis.FindPendingForConversationPeer(h.db, convoID, user.ID); perr == nil && found {
			draft = p.Body
		} else if perr != nil {
			log.Printf("[gnosis] reading a held message for conversation %s: %v", convoID, perr)
		}
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
		"Draft":        draft,
	})
}

// renderProspect draws the thread pane for a conversation that does not exist
// yet: who it is addressed to, the message the server is still holding for them,
// and — once an attempt has actually been made — the gate saying why it has not
// been delivered and what can still deliver it.
//
// decision is the outcome of the attempt just made. Empty means no attempt was
// made in this request, and the gate is drawn from what the server recorded last
// time, so re-opening the thread does not knock on the recipient a second time
// to find out what to draw.
func (h *Handler) renderProspect(w http.ResponseWriter, user *model.User, target *model.User, decision string) {
	data := map[string]interface{}{
		"ToHandle":  target.Handle,
		"ToDisplay": target.DisplayName,
		"ToAvatar":  target.AvatarURL,
		"Pending":   nil,
		"PendingID": "",
		"Decision":  decision,
	}
	p, found, err := gnosis.FindPending(h.db, user.ID, target.ID)
	if err != nil {
		log.Printf("[gnosis] reading a held message for %s: %v", target.Handle, err)
	}
	if found {
		// Rendered through the one canonical bubble, so a message that has not
		// been delivered still looks like a message.
		data["Pending"] = gnosis.Message{
			ID:            p.ID,
			SenderAccount: user.ID,
			Mode:          gnosis.ModePlain,
			Body:          p.Body,
			CreatedAt:     p.CreatedAt,
		}
		data["PendingID"] = p.ID
		if decision == "" {
			data["Decision"] = p.LastDecision
		}
	}
	h.renderPartial(w, "gnosis_prospect_thread", data)
}

// contactGateState maps what the contact authority answered to what the sender is
// shown, and it is deliberately total and deliberately lossy.
//
// Every refusal the authority can produce arrives here as one word — an unknown
// Number, a revoked one, an expired lease, a spent budget, a closed policy, a
// contact link not presented — because elohim-veni has already collapsed them.
// The default branch keeps them collapsed, so no decision this brain does not
// recognise can ever acquire a message of its own by being added upstream. That
// is the enumeration defence's last mile: the place it is easiest to lose is a
// well-meaning extra case in a switch.
//
// An empty result means this is not a gate at all: the conversation opens.
func contactGateState(decision string) string {
	switch decision {
	case "allow":
		return ""
	case "request":
		return gnosis.PendingRequest
	default:
		return gnosis.PendingDeny
	}
}

// contactNote bounds the note a knock carries.
//
// A NOTE IS NOT THE MESSAGE, and the difference is the whole meaning of a knock.
// The message is held here, undelivered, and the recipient decides BEFORE they
// receive it; a knock that carried its own payload would have delivered itself,
// and "accept" would be a formality after the fact. So the note is a separate,
// short, optional thing a sender writes to say why they are asking — and it is
// the only thing about them that crosses to the recipient before they agree.
//
// Truncation is by rune, not by byte, so the cut cannot land inside a character
// and hand the authority half of one.
func contactNote(body string) string {
	const maxNoteRunes = 280
	r := []rune(strings.TrimSpace(body))
	if len(r) > maxNoteRunes {
		return string(r[:maxNoteRunes])
	}
	return string(r)
}

// handleCreateConvo opens the thread pane for a @handle. If a conversation with
// that person already exists it is rendered; if it does not, the PROSPECT thread
// is rendered — the peer, an empty message list and a composer.
//
// It deliberately asks the contact authority NOTHING. It used to ask here, which
// meant opening a conversation with somebody placed a knock in their inbox for a
// message that had not been written, and showed a refusal for a sentence nobody
// had typed. Wanting to write to somebody is not an event in their life. The
// decision belongs at the moment a message is actually sent, which is where
// handleSendMessage now takes it.
//
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
		h.renderProspect(w, user, target, "")
		return
	}
	// Refresh the sidebar list, then return the thread fragment.
	w.Header().Set("HX-Trigger", "gnosis-refresh")
	h.renderThread(w, user, convoID)
}

// sendToPerson is the send path for a message addressed to a PERSON rather than
// to a conversation — the first thing written to somebody there is no
// conversation with yet.
//
// This is where the contact decision is taken, and taking it here is the whole
// difference. The message exists before the decision does, so whatever the
// authority answers can be rendered underneath the sentence it was answered
// about, and the sentence is kept rather than thrown away. A knock carries what
// was written, so the person deciding is deciding about something real.
func (h *Handler) sendToPerson(w http.ResponseWriter, r *http.Request, user *model.User, handle, body string) {
	target, err := dbpkg.GetUserByHandle(h.db, handle)
	if err != nil || target == nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	if target.ID == user.ID {
		http.Error(w, "cannot message yourself", http.StatusBadRequest)
		return
	}
	// A conversation that is already open was already permitted. The gate is not
	// re-asked; the message goes straight in.
	if convoID, _ := gnosis.FindDirectConversation(h.db, user.ID, target.ID); convoID != "" {
		// A knock from before this conversation existed may still be held. The
		// sentence being sent now supersedes it, so it is carried in by id and
		// released on delivery. Without the id the row survives, and the very
		// next render of this thread hands it back as a draft — inviting the
		// sender to say the same thing twice.
		held := gnosis.Pending{Body: body}
		if p, found, ferr := gnosis.FindPending(h.db, user.ID, target.ID); ferr == nil && found {
			held.ID = p.ID
		} else if ferr != nil {
			log.Printf("[gnosis] reading a held message for %s: %v", handle, ferr)
		}
		w.Header().Set("HX-Trigger", "gnosis-refresh")
		h.deliverPendingInto(w, user, convoID, held)
		return
	}

	// Past this point the act is a contact INITIATION, not an ordinary write: it
	// holds a message for a stranger, asks the contact authority about them, and
	// can push a notification at them. That act has its own far stricter budget,
	// and it is charged here rather than on the route because the branch above —
	// writing into a conversation that already exists — is an ordinary write and
	// must keep the ordinary write rate.
	if !h.rlContactInit.Allow(r) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "Too many contact attempts — slow down", http.StatusTooManyRequests)
		return
	}

	// Held before it is judged, so nothing that happens next can lose it.
	pending, err := gnosis.UpsertPending(h.db, user.ID, target.ID, body)
	if err != nil {
		log.Printf("[gnosis] holding a message for %s: %v", handle, err)
		http.Error(w, "could not send", http.StatusInternalServerError)
		return
	}

	targetPIAL := dbpkg.ResolvePIAL(h.db, target.ID)
	if targetPIAL == "" {
		// No identity to ask about, so nobody was asked. Not a refusal by them.
		h.gateProspect(w, user, target, pending, gnosis.PendingUnavailable)
		return
	}

	// Knowing a handle is not permission to open a conversation. The contact
	// policy authority decides, and an unreachable authority refuses: an
	// unknown policy is not an open one.
	outcome, err := h.evaluateContact(r.Context(), user, contactQuery{TargetPIAL: targetPIAL})
	if err != nil {
		log.Printf("[gnosis] contact decision for %s: %v", handle, err)
		h.gateProspect(w, user, target, pending, gnosis.PendingUnavailable)
		return
	}
	// The one notification pipeline, reached the one way.
	h.notifyContactRequest(user, outcome)

	if state := contactGateState(outcome.Decision); state != "" {
		h.gateProspect(w, user, target, pending, state)
		return
	}

	convoID, err := h.ensureDirectConversation(user, target.ID, targetPIAL)
	if err != nil {
		log.Printf("[gnosis] create conversation: %v", err)
		h.gateProspect(w, user, target, pending, gnosis.PendingUnavailable)
		return
	}
	w.Header().Set("HX-Trigger", "gnosis-refresh")
	h.deliverPendingInto(w, user, convoID, pending)
}

// gateProspect records what the authority answered and redraws the prospect
// thread with the gate under the held message. Every non-allow outcome of a send
// to a person ends here, so there is one place that decides what a person sees
// when their message did not get through.
func (h *Handler) gateProspect(w http.ResponseWriter, user *model.User, target *model.User, pending gnosis.Pending, decision string) {
	if err := gnosis.SetPendingDecision(h.db, pending.ID, decision); err != nil {
		log.Printf("[gnosis] recording a contact decision for %s: %v", target.Handle, err)
	}
	h.renderProspect(w, user, target, decision)
}

// deliverPendingInto puts a held message into a conversation that has just been
// permitted, and renders the conversation.
//
// A SEALED conversation is the one case where it is not written. Sealing needs
// the recipient's key bundle and that bundle is exactly what contact permission
// grants, so a message written before permission existed cannot have been
// sealed — and writing it in as plaintext would put one readable message inside
// an end-to-end encrypted thread. It stays the server's and is rendered back
// into the composer that can seal it, so nobody types it twice.
func (h *Handler) deliverPendingInto(w http.ResponseWriter, user *model.User, convoID string, pending gnosis.Pending) {
	if !h.deliverPending(user, convoID, pending) {
		// It stays the sender's, and the composer that can send it gets it back.
		h.renderThreadWithDraft(w, user, convoID, pending.Body)
		return
	}
	h.renderThread(w, user, convoID)
}

// deliverPending writes a held message into a conversation and releases the
// hold, reporting whether it landed. It is the whole of the act with none of
// the answer, so the browser and a client that draws its own thread deliver one
// message through one function and cannot come to differ about when a held
// message is released.
//
// It reports false, and leaves the hold ALONE, whenever the message did not
// land — including the one case that is not a failure at all: a conversation
// that opened SEALED. Sealing needs the sender's device key, which the server
// does not have and must not have, so writing the plaintext would be a silent
// downgrade of one message inside a thread both people believe is encrypted.
// The message stays the sender's, and the composer that can seal it gets it
// back the moment they open the thread.
func (h *Handler) deliverPending(user *model.User, convoID string, pending gnosis.Pending) bool {
	convo, err := gnosis.GetConversation(h.db, convoID)
	if err != nil {
		log.Printf("[gnosis] reading conversation %s: %v", convoID, err)
		return false
	}
	if convo.Mode == gnosis.ModeSealed {
		return false
	}
	msg, err := gnosis.InsertPlainMessage(h.db, convoID, user.ID, pending.Body)
	if err != nil {
		log.Printf("[gnosis] delivering a held message into %s: %v", convoID, err)
		return false
	}
	h.fanoutMessage(convoID, user.ID, msg)
	if pending.ID != "" {
		if err := gnosis.DeletePending(h.db, pending.ID); err != nil {
			log.Printf("[gnosis] releasing a delivered message %s: %v", pending.ID, err)
		}
	}
	return true
}

// holdUndelivered keeps a sentence that did not arrive, and names it.
//
// A message written into a thread that has stopped delivering is in exactly the
// position of a message written to somebody there is no conversation with yet:
// it was said, it did not land, and it must not be lost because of that. So it
// goes where every other undelivered sentence goes — one row per (sender,
// target), amended rather than queued — and the id comes back so the sender can
// hand it to the Number lane and have THIS message delivered rather than a
// second copy of it.
//
// It answers "" when it could not hold anything. The caller still reports that
// the message did not arrive, because it did not; what it must not do is claim
// to be holding something it is not.
func (h *Handler) holdUndelivered(user *model.User, convoID, body string) string {
	body = strings.TrimSpace(body)
	if body == "" || len(body) > maxMessageBytes {
		return ""
	}
	people, err := gnosis.Participants(h.db, convoID)
	if err != nil {
		log.Printf("[gnosis] reading members of %s: %v", convoID, err)
		return ""
	}
	for _, p := range people {
		if p.AccountID == "" || p.AccountID == user.ID {
			continue
		}
		pending, err := gnosis.UpsertPending(h.db, user.ID, p.AccountID, body)
		if err != nil {
			log.Printf("[gnosis] holding an undelivered message from %s: %v", user.ID, err)
			return ""
		}
		return pending.ID
	}
	return ""
}

// ensureDirectConversation finds or creates the 1:1 conversation between two
// accounts. The contact policy gate is the CALLER's responsibility — every path
// that reaches here has already been authorised.
func (h *Handler) ensureDirectConversation(user *model.User, targetAccount, targetPIAL string) (string, error) {
	convoID, _ := gnosis.FindDirectConversation(h.db, user.ID, targetAccount)
	if convoID != "" {
		return convoID, nil
	}
	mode := gnosis.ModeForParticipants(h.db, []string{user.PIALID, targetPIAL})
	members := []gnosis.Member{
		{AccountID: user.ID, PIALID: user.PIALID},
		{AccountID: targetAccount, PIALID: targetPIAL},
	}
	return gnosis.CreateConversation(h.db, user.ID, members, mode)
}

// handleSendMessage posts a message. Plain conversations store server-readable text
// and fan out server-rendered bubbles over SSE. Sealed conversations are refused
// here — they are sent through the encrypted client path so the server never sees
// plaintext (wired in sealed-mode).
//
// It takes two shapes, and the second is what makes the contact gate possible:
//
//	c=<conversation>  an ordinary message into a conversation that exists.
//	to=<handle>       the first message to a person there is no conversation with
//	                  yet. The contact decision is taken there, AFTER something has
//	                  been written, so the outcome can be rendered under the words
//	                  it was about and the words are kept either way.
//
// POST /messages/send  (form: c | to, body)
func (h *Handler) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	convoID := r.FormValue("c")
	body := strings.TrimSpace(r.FormValue("body"))
	if body == "" {
		http.Error(w, "message required", http.StatusBadRequest)
		return
	}
	if len(body) > maxMessageBytes {
		http.Error(w, "message too long", http.StatusRequestEntityTooLarge)
		return
	}
	if convoID == "" {
		handle := strings.TrimPrefix(strings.TrimSpace(r.FormValue("to")), "@")
		if handle == "" {
			http.Error(w, "message required", http.StatusBadRequest)
			return
		}
		h.sendToPerson(w, r, user, handle, body)
		return
	}
	msg, status, reason := h.sendIntoConversation(user, convoID, body)
	if status != http.StatusOK {
		http.Error(w, reason, status)
		return
	}

	// Optimistic: return the sender's own out-bubble for HTMX append.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "gnosis_bubble", map[string]interface{}{
		"M": msg, "Variant": "out",
	})
}

// sendIntoConversation is one plaintext message written into a conversation
// that already exists — the whole of the act, with none of the answer.
//
// It is deliberately shaped as (result, status, reason) rather than as a
// handler, because two clients ask for the same act and want different
// answers: the browser wants its bubble, the app wants the message. Anything
// that decided BOTH here would be two send paths pretending to be one, and the
// mode check is exactly the rule that must never be able to differ between
// them — a sealed conversation refuses plaintext, whoever asks.
//
// status is http.StatusOK when the message was written. Any other status is a
// refusal, and reason is what to say about it.
// sendNotDelivered is the whole of what a send that did not arrive says. It is
// one string on purpose: both surfaces read this exact value — the browser
// shows it, the JSON lane turns it into StartNotDelivered — and neither may
// grow a second sentence for a second cause.
const sendNotDelivered = "not delivered"

// undeliverableTo reports whether a message written into this conversation
// would not reach the person it is addressed to.
//
// It asks ONE thing: whether there is a block between these two accounts, in
// either direction. In particular the contact authority is NOT re-asked, and
// that is deliberate three times over. A conversation that exists passed the
// gate once, when it was opened; re-asking it on every sentence would knock on
// somebody each time their friend typed one, would put an HTTP hop on the
// hot path of the messaging surface, and — because the authority is asked about
// a target identity rather than about a Number — would judge a conversation
// that was opened through a Number against the @handle policy instead, quietly
// closing threads whose owners never closed them.
//
// A block is different in kind. It is this brain's own fact, about accounts
// this brain owns, read locally, and it is the one way a permitted conversation
// stops being permitted here.
//
// A GROUP is never undeliverable for one member's block. A room is not two
// people, and one person's block does not silence the room for everybody.
func (h *Handler) undeliverableTo(user *model.User, convo *gnosis.Conversation) bool {
	if h.db == nil || user == nil || convo == nil || convo.IsGroup {
		return false
	}
	people, err := gnosis.Participants(h.db, convo.ID)
	if err != nil {
		// Fail closed. "Nobody has blocked you" and "I could not find out" are
		// different answers and only one of them may quietly deliver.
		log.Printf("[gnosis] reading members of %s: %v", convo.ID, err)
		return true
	}
	for _, p := range people {
		if p.AccountID == "" || p.AccountID == user.ID {
			continue
		}
		if dbpkg.IsBlocked(h.db, p.AccountID, user.ID) || dbpkg.IsBlocked(h.db, user.ID, p.AccountID) {
			return true
		}
	}
	return false
}

func (h *Handler) sendIntoConversation(user *model.User, convoID, body string) (gnosis.Message, int, string) {
	body = strings.TrimSpace(body)
	if body == "" {
		return gnosis.Message{}, http.StatusBadRequest, "message required"
	}
	if len(body) > maxMessageBytes {
		return gnosis.Message{}, http.StatusRequestEntityTooLarge, "message too long"
	}
	if ok, _ := gnosis.IsMember(h.db, convoID, user.ID); !ok {
		return gnosis.Message{}, http.StatusForbidden, "forbidden"
	}
	convo, err := gnosis.GetConversation(h.db, convoID)
	if err != nil {
		return gnosis.Message{}, http.StatusNotFound, "not found"
	}
	if convo.Mode == gnosis.ModeSealed {
		return gnosis.Message{}, http.StatusConflict, "sealed conversation: send via the encrypted client path"
	}
	// A thread can outlive the permission that opened it. When it has, the
	// message does not arrive and is not written — and nothing here says which
	// of the reasons it was.
	if h.undeliverableTo(user, convo) {
		return gnosis.Message{}, http.StatusForbidden, sendNotDelivered
	}
	msg, err := gnosis.InsertPlainMessage(h.db, convoID, user.ID, body)
	if err != nil {
		log.Printf("[gnosis] writing into %s: %v", convoID, err)
		return gnosis.Message{}, http.StatusInternalServerError, "send failed"
	}
	h.fanoutMessage(convoID, user.ID, msg)
	// This conversation is in use, so a sentence written before it existed has
	// been overtaken and must not resurface in its composer.
	if err := gnosis.DeletePendingForConversationPeer(h.db, convoID, user.ID); err != nil {
		log.Printf("[gnosis] releasing a held message for conversation %s: %v", convoID, err)
	}
	return msg, http.StatusOK, ""
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
//
// One publish, two projections: Data is the rendered bubble the browser swaps
// in, JSON is the same event for a client that draws its own. A plain message
// is the same object to every recipient, so both are rendered once.
func (h *Handler) fanoutMessage(convoID, senderID string, msg gnosis.Message) {
	accounts, _ := gnosis.MemberAccounts(h.db, convoID)
	var buf bytes.Buffer
	if err := h.partial.ExecuteTemplate(&buf, "gnosis_bubble", map[string]interface{}{
		"M": msg, "Variant": "in",
	}); err != nil {
		return
	}
	event := SSEEvent{
		Type: "gnosis_message",
		Data: buf.String(),
		JSON: messagePushJSON(msg, h.senderHandle(senderID)),
	}
	for _, acct := range accounts {
		if acct == senderID {
			continue
		}
		PublishToAccount(acct, event)
	}
}

// senderHandle resolves an account to its @handle for the message twin, or ""
// when it cannot be read. A twin without a name is still a usable event; a
// failed lookup must not cost the recipient the message.
func (h *Handler) senderHandle(accountID string) string {
	if accountID == "" {
		return ""
	}
	who, err := gnosis.DisplayForAccounts(h.db, []string{accountID})
	if err != nil {
		log.Printf("[gnosis] resolving sender %s: %v", accountID, err)
		return ""
	}
	return who[accountID].Handle
}

// ── The Number, entered where it is needed ───────────────────────────────────

// handleNumberContact is the one entry point a F33D3R Number takes into the
// messaging surface. It has two shapes, told apart by whether the caller is
// holding an undelivered message:
//
//	p=<held message>  the contact gate inside a conversation. Somebody wrote to a
//	                  person, it did not get through, and they have been given
//	                  that person's Number. The message they already wrote is
//	                  what gets sent.
//	no p              a Number and nothing else — somebody was handed a Number
//	                  and does not know whose it is.
//
// POST /messages/new-number
func (h *Handler) handleNumberContact(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(r.FormValue("p")) == "" {
		h.handleCreateConvoByNumber(w, r)
		return
	}
	h.handleGateNumber(w, r)
}

// handleGateNumber resolves a Number entered against a message that is already
// waiting, and delivers that message if the Number opens the door.
//
// WHAT THIS SURFACE MUST NOT BECOME. A Number is guessable in principle, and the
// only thing standing between a guesser and an identity is that a wrong guess
// tells them nothing. So every outcome that is not an allow renders the SAME
// fragment after the SAME minimum duration:
//
//   - a Number nobody holds
//   - a Number that has been revoked
//   - a Number whose lease has expired
//   - a Number whose admission budget is spent
//   - a Number that is somebody else's, not this person's
//   - a policy that refuses this caller
//
// The authority already collapses the first four into one `deny` with one
// reason. The fifth is collapsed here, because a Number that resolves to a
// different person must not confirm that it resolves to anybody. The clock is
// held to numberResolveFloor on every one of them, so the work actually done is
// not readable from the timing either.
//
// Only a malformed Number is named, and only because whether something is shaped
// like a Number is arithmetic the caller can do offline — canonicalNumber
// decides it here, nothing about the format is repeated in this file, and the
// answer discloses nothing except that they mistyped.
func (h *Handler) handleGateNumber(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	pending, found, err := gnosis.GetPendingForSender(h.db, strings.TrimSpace(r.FormValue("p")), user.ID)
	if err != nil {
		log.Printf("[gnosis] reading a held message for pial %s: %v", user.PIALID, err)
		http.Error(w, "could not send", http.StatusInternalServerError)
		return
	}
	if !found {
		// Either it was delivered already or it was never this caller's. Neither
		// is a contact decision, so neither renders a gate.
		http.Error(w, "no message is waiting", http.StatusNotFound)
		return
	}
	target, err := dbpkg.GetUserByID(h.db, pending.TargetAccount)
	if err != nil || target == nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}

	// Every non-allow return below passes through here. A Number lookup that
	// answered faster because it did less work would say what it did.
	gate := func(decision string) {
		if err := gnosis.SetPendingDecision(h.db, pending.ID, decision); err != nil {
			log.Printf("[gnosis] recording a contact decision for %s: %v", target.Handle, err)
		}
		holdFloor(started, numberResolveFloor)
		h.renderProspect(w, user, target, decision)
	}

	number, wellFormed := canonicalNumber(r.FormValue("number"))
	if !wellFormed {
		// Not stored: what somebody typed is not something the recipient did,
		// so it is not a decision to remember about them.
		holdFloor(started, numberResolveFloor)
		h.renderProspect(w, user, target, "malformed")
		return
	}

	outcome, err := h.evaluateContact(r.Context(), user, contactQuery{
		Number:     number,
		Capability: strings.TrimSpace(r.FormValue("capability")),
		Note:       contactNote(r.FormValue("note")),
	})
	if err != nil {
		log.Printf("[gnosis] contact decision by Number for pial %s: %v", user.PIALID, err)
		gate(gnosis.PendingUnavailable)
		return
	}
	// The one notification pipeline, reached the one way.
	h.notifyContactRequest(user, outcome)

	if state := contactGateState(outcome.Decision); state != "" {
		gate(state)
		return
	}
	if outcome.TargetPIAL == "" {
		// An allow that names nobody cannot open a conversation, and saying so
		// would say more than a refusal does.
		gate(gnosis.PendingDeny)
		return
	}
	account := dbpkg.AccountForPIAL(h.db, outcome.TargetPIAL)
	if account == "" || account != pending.TargetAccount {
		// A working Number belonging to somebody else does not deliver a message
		// written to this person, and does not say that is what happened.
		gate(gnosis.PendingDeny)
		return
	}
	convoID, err := h.ensureDirectConversation(user, account, outcome.TargetPIAL)
	if err != nil {
		log.Printf("[gnosis] opening conversation by Number for pial %s: %v", user.PIALID, err)
		gate(gnosis.PendingUnavailable)
		return
	}

	holdFloor(started, numberResolveFloor)
	w.Header().Set("HX-Trigger", "gnosis-refresh")
	h.deliverPendingInto(w, user, convoID, pending)
}
