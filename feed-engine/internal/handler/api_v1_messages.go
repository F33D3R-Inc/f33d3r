package handler

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/gnosis"
	"github.com/f33d3r/feed-engine/internal/model"
)

// ── /api/v1/messages — Gnosis for the native clients ─────────────────────────
//
// The messaging brain is gnosis and it is unchanged: the same conversations,
// the same two modes, the same rows. What this file adds is the JSON twin of
// the Facet surface in messages.go — the conversation list, one thread, and
// the DTOs the SSE twin carries — so a phone reads exactly the state a browser
// reads and writes through exactly the same lane.
//
// The mode of a conversation decides what crosses this boundary:
//
//   plain  : the server holds the text, so the text is in the DTO.
//   sealed : the server holds ciphertext and the viewer's own wrapped key, and
//            that is all that is in the DTO. There is no plaintext to send and
//            no preview to build — the client decrypts and draws its own.
//
// sender_account and the members' `account` are users.id, the messaging
// anchor. They already cross to clients on /api/gnosis/directory, and they
// must: a sealed key set is addressed BY account, so a client that cannot name
// the accounts in a conversation cannot seal to them. No PIAL is ever here.

// ConversationDTO is one row of the viewer's conversation list.
//
// preview is what the server can honestly say about the last message: the
// plaintext for a plain conversation, and EMPTY for a sealed one. The web's
// lock placeholder is a rendered string for a template; a client that draws
// its own list is told `mode` and draws its own.
type ConversationDTO struct {
	ID           string    `json:"id"`
	Mode         string    `json:"mode"`
	IsGroup      bool      `json:"is_group"`
	Title        string    `json:"title,omitempty"`
	OtherHandle  string    `json:"other_handle,omitempty"`
	OtherDisplay string    `json:"other_display,omitempty"`
	OtherAvatar  *string   `json:"other_avatar,omitempty"`
	Preview      string    `json:"preview"`
	LastAt       time.Time `json:"last_at"`
	Unread       int       `json:"unread"`
}

// MessageDTO is one message as its recipient may read it. In sealed mode the
// key fields are THIS viewer's own wrapped content key and nobody else's.
type MessageDTO struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversation_id"`
	SenderAccount  string    `json:"sender_account"`
	SenderHandle   string    `json:"sender_handle,omitempty"`
	SenderDisplay  string    `json:"sender_display,omitempty"`
	SenderAvatar   *string   `json:"sender_avatar,omitempty"`
	Mode           string    `json:"mode"`
	Body           string    `json:"body,omitempty"`
	BodyCtB64      string    `json:"body_ct_b64,omitempty"`
	BodyNonceB64   string    `json:"body_nonce_b64,omitempty"`
	EphPubB64      string    `json:"eph_pub_b64,omitempty"`
	SealedB64      string    `json:"sealed_b64,omitempty"`
	SealedNonceB64 string    `json:"sealed_nonce_b64,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	IsMine         bool      `json:"is_mine"`
}

// ConversationPageDTO is the whole list plus the badge number, so a client
// never has to add the rows up itself and never disagrees with the server.
type ConversationPageDTO struct {
	Conversations []ConversationDTO `json:"conversations"`
	UnreadTotal   int               `json:"unread_total"`
}

// ThreadMemberDTO is one participant of a conversation. `account` is what a
// sealed key is addressed to; it is the same id /api/gnosis/directory returns.
type ThreadMemberDTO struct {
	Handle  string  `json:"handle"`
	Display string  `json:"display"`
	Avatar  *string `json:"avatar,omitempty"`
	Account string  `json:"account"`
}

// ThreadDTO is one conversation opened: its row, a page of messages
// oldest-first, and the members a client needs to seal to.
//
// next_cursor walks BACKWARDS — it is the page of older messages before this
// one, because a thread is read from its end. Empty means the conversation
// begins here.
type ThreadDTO struct {
	Conversation ConversationDTO   `json:"conversation"`
	Messages     []MessageDTO      `json:"messages"`
	NextCursor   string            `json:"next_cursor,omitempty"`
	Members      []ThreadMemberDTO `json:"members"`
}

const (
	apiMessagesDefaultPage = 50
	apiMessagesMaxPage     = 200
)

// apiMessageLimit reads ?limit= for a thread page. A conversation is read in
// far bigger bites than a feed — the web renders 200 at once — so this lane
// has its own bound rather than the feed's.
func apiMessageLimit(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 {
		return apiMessagesDefaultPage
	}
	if n > apiMessagesMaxPage {
		return apiMessagesMaxPage
	}
	return n
}

// A thread cursor is the exact message the previous page began at, as
// created_at and id. The pair is the key because gnosis_messages has no
// sequence and two messages can share an instant; time alone either drops the
// messages that tie with the cursor or serves them twice.
func encodeThreadCursor(m gnosis.Message) string {
	return base64.RawURLEncoding.EncodeToString([]byte(m.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + m.ID))
}

func decodeThreadCursor(raw string) (time.Time, string, bool) {
	if raw == "" {
		return time.Time{}, "", true
	}
	blob, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return time.Time{}, "", false
	}
	at, id, ok := strings.Cut(string(blob), "|")
	if !ok {
		return time.Time{}, "", false
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return time.Time{}, "", false
	}
	// The id reaches SQL as a uuid, so it is proved to be one here rather than
	// letting a malformed cursor become a database error.
	if _, err := uuid.Parse(id); err != nil {
		return time.Time{}, "", false
	}
	return t, id, true
}

// conversationDTO projects one sidebar row. The preview is dropped for a
// sealed conversation: the server holds ciphertext, so it has nothing to
// preview, and the placeholder the web renders is a template's business.
func conversationDTO(row gnosis.ConvoListRow) ConversationDTO {
	d := ConversationDTO{
		ID:           row.ConversationID,
		Mode:         row.Mode,
		IsGroup:      row.IsGroup,
		Title:        row.Title,
		OtherHandle:  row.OtherHandle,
		OtherDisplay: row.OtherDisplay,
		OtherAvatar:  strPtr(row.OtherAvatar),
		Preview:      row.Preview,
		LastAt:       row.LastAt,
		Unread:       row.Unread,
	}
	if row.Mode == gnosis.ModeSealed || row.LastMode == gnosis.ModeSealed {
		d.Preview = ""
	}
	return d
}

// messageDTO projects one message for one viewer. `who` is the sender's
// display identity, resolved by the caller — the store returns an account id
// and this surface answers with a person, because a handler hydrates before it
// answers.
func messageDTO(m gnosis.Message, viewerAccount string, who gnosis.Participant) MessageDTO {
	return MessageDTO{
		ID:             m.ID,
		ConversationID: m.ConversationID,
		SenderAccount:  m.SenderAccount,
		SenderHandle:   who.Handle,
		SenderDisplay:  who.Display,
		SenderAvatar:   strPtr(who.Avatar),
		Mode:           m.Mode,
		Body:           m.Body,
		BodyCtB64:      m.BodyCtB64,
		BodyNonceB64:   m.BodyNonceB64,
		EphPubB64:      m.EphPubB64,
		SealedB64:      m.SealedB64,
		SealedNonceB64: m.SealedNonceB64,
		CreatedAt:      m.CreatedAt,
		IsMine:         m.SenderAccount == viewerAccount,
	}
}

// hydrateSenders resolves every distinct sender on a page of messages to a
// display identity, in one query.
func (h *Handler) hydrateSenders(msgs []gnosis.Message, known map[string]gnosis.Participant) map[string]gnosis.Participant {
	out := map[string]gnosis.Participant{}
	for k, v := range known {
		out[k] = v
	}
	var missing []string
	for _, m := range msgs {
		if m.SenderAccount == "" {
			continue
		}
		if _, ok := out[m.SenderAccount]; !ok {
			out[m.SenderAccount] = gnosis.Participant{AccountID: m.SenderAccount}
			missing = append(missing, m.SenderAccount)
		}
	}
	if len(missing) == 0 {
		return out
	}
	found, err := gnosis.DisplayForAccounts(h.db, missing)
	if err != nil {
		log.Printf("[api/v1] resolving message senders: %v", err)
		return out
	}
	for id, p := range found {
		out[id] = p
	}
	return out
}

// apiV1Messages — GET /api/v1/messages: the viewer's conversations, newest
// activity first, with the unread badge already totalled.
func (h *Handler) apiV1Messages(w http.ResponseWriter, r *http.Request, u *model.User) {
	rows, err := gnosis.ListConversationsForAccount(h.db, u.ID)
	if err != nil {
		apiServerError(w, err)
		return
	}
	out := ConversationPageDTO{Conversations: make([]ConversationDTO, 0, len(rows))}
	for _, row := range rows {
		out.Conversations = append(out.Conversations, conversationDTO(row))
		out.UnreadTotal += row.Unread
	}
	apiJSON(w, http.StatusOK, out)
}

// apiV1MessageThread — GET /api/v1/messages/{id}?limit=&cursor=: one
// conversation the viewer belongs to.
//
// Opening a thread does not mark it read. The web marks on render because a
// rendered thread IS the reading; a client that fetches a page and may not
// show it says so itself, with `message_read` on the event lane.
func (h *Handler) apiV1MessageThread(w http.ResponseWriter, r *http.Request, u *model.User) {
	convoID := r.PathValue("id")
	if _, err := uuid.Parse(convoID); err != nil {
		apiError(w, http.StatusNotFound, "not_found", "That conversation doesn't exist.")
		return
	}
	convo, err := gnosis.GetConversation(h.db, convoID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			apiError(w, http.StatusNotFound, "not_found", "That conversation doesn't exist.")
			return
		}
		apiServerError(w, err)
		return
	}
	member, err := gnosis.IsMember(h.db, convoID, u.ID)
	if err != nil {
		apiServerError(w, err)
		return
	}
	if !member {
		apiError(w, http.StatusForbidden, "not_a_member", "This conversation is not yours.")
		return
	}
	// `before` says what the cursor means on this lane — the page BEFORE this
	// one — and `cursor` is the name every other /api/v1 read uses. Both are
	// accepted so neither the surface nor the clients have to be renamed.
	raw := r.URL.Query().Get("before")
	if raw == "" {
		raw = r.URL.Query().Get("cursor")
	}
	beforeAt, beforeID, ok := decodeThreadCursor(raw)
	if !ok {
		apiError(w, http.StatusBadRequest, "bad_cursor", "That page cursor is not valid.")
		return
	}
	limit := apiMessageLimit(r)
	msgs, more, err := gnosis.ListMessagesBefore(h.db, convoID, u.ID, limit, beforeAt, beforeID)
	if err != nil {
		apiServerError(w, err)
		return
	}

	people, err := gnosis.Participants(h.db, convoID)
	if err != nil {
		apiServerError(w, err)
		return
	}
	byAccount := make(map[string]gnosis.Participant, len(people))
	members := make([]ThreadMemberDTO, 0, len(people))
	for _, p := range people {
		byAccount[p.AccountID] = p
		members = append(members, ThreadMemberDTO{
			Handle: p.Handle, Display: p.Display, Avatar: strPtr(p.Avatar), Account: p.AccountID,
		})
	}
	byAccount = h.hydrateSenders(msgs, byAccount)

	row, found, err := gnosis.ConversationRowForAccount(h.db, convoID, u.ID)
	if err != nil {
		apiServerError(w, err)
		return
	}
	if !found {
		// Membership was just proved, so this cannot happen unless the row was
		// removed between the two reads. Answer the conversation itself rather
		// than a contradiction.
		row = gnosis.ConvoListRow{
			ConversationID: convo.ID, Mode: convo.Mode, IsGroup: convo.IsGroup,
			Title: convo.Title, LastAt: convo.CreatedAt,
		}
	}
	out := ThreadDTO{
		Conversation: conversationDTO(row),
		Messages:     make([]MessageDTO, 0, len(msgs)),
		Members:      members,
	}
	for _, m := range msgs {
		out.Messages = append(out.Messages, messageDTO(m, u.ID, byAccount[m.SenderAccount]))
	}
	if more && len(msgs) > 0 {
		out.NextCursor = encodeThreadCursor(msgs[0])
	}
	apiJSON(w, http.StatusOK, out)
}

// ── The SSE twin ─────────────────────────────────────────────────────────────

// MessagePushDTO is the JSON twin of a gnosis_message event: the same message
// the browser receives as a rendered bubble, told to a client that draws its
// own.
//
// It carries no plaintext. A plain message's text is already on the wire as
// the rendered bubble for the browser; the twin says a message landed and
// where, and the client reads it through GET /api/v1/messages/{id} — one place
// that decides what a viewer may read, not two. A SEALED message is different:
// the ciphertext and the recipient's own wrapped key are opaque to the server
// and are what the client needs to decrypt, so they ride along and the push
// resolves without a round trip.
//
// Nothing here is a count. The unread badge is re-read from the database by
// the reader, exactly as the notify twin's count is: a number rendered for an
// earlier state must never be able to mis-badge a client.
type MessagePushDTO struct {
	ConversationID string    `json:"conversation_id"`
	MessageID      string    `json:"message_id"`
	Mode           string    `json:"mode"`
	SenderAccount  string    `json:"sender_account"`
	SenderHandle   string    `json:"sender_handle,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	BodyCtB64      string    `json:"body_ct_b64,omitempty"`
	BodyNonceB64   string    `json:"body_nonce_b64,omitempty"`
	EphPubB64      string    `json:"eph_pub_b64,omitempty"`
	SealedB64      string    `json:"sealed_b64,omitempty"`
	SealedNonceB64 string    `json:"sealed_nonce_b64,omitempty"`
}

// messagePushJSON renders one recipient's twin. m must already carry THAT
// recipient's sealed key, because a sealed message is a different object to
// every member.
func messagePushJSON(m gnosis.Message, senderHandle string) string {
	push := MessagePushDTO{
		ConversationID: m.ConversationID,
		MessageID:      m.ID,
		Mode:           m.Mode,
		SenderAccount:  m.SenderAccount,
		SenderHandle:   senderHandle,
		CreatedAt:      m.CreatedAt,
	}
	if m.Mode == gnosis.ModeSealed {
		push.BodyCtB64 = m.BodyCtB64
		push.BodyNonceB64 = m.BodyNonceB64
		push.EphPubB64 = m.EphPubB64
		push.SealedB64 = m.SealedB64
		push.SealedNonceB64 = m.SealedNonceB64
	}
	raw, err := json.Marshal(push)
	if err != nil {
		log.Printf("[gnosis] message twin for %s: %v", m.ID, err)
		return ""
	}
	return string(raw)
}

// ── The event lane ───────────────────────────────────────────────────────────

// apiWantsJSON reports whether the caller asked for JSON rather than a
// rendered fragment. HTMX sends `Accept: text/html…`, so the web never
// matches and its bubble is never taken away from it.
func apiWantsJSON(r *http.Request) bool {
	accept := strings.ToLower(r.Header.Get("Accept"))
	if strings.Contains(accept, "application/json") {
		return true
	}
	if strings.Contains(accept, "text/html") {
		return false
	}
	return strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/json")
}

// The three outcomes of writing to a PERSON, and the whole vocabulary this
// lane has for it.
//
// There are three because there are three things that can honestly be said,
// not because there are three things that can happen. Six distinct causes —
// a handle nobody holds, a revoked Number, an expired lease, a spent budget, a
// closed policy, an authority that did not answer — all end at
// StartNotDelivered, in one shape, after one duration. Adding a fourth state to
// be helpful about which of them it was would rebuild, in JSON, exactly the
// enumeration oracle the HTML gate is built to deny.
const (
	// StartOpened: there is a conversation, and it is in the answer.
	StartOpened = "opened"
	// StartHeld: the message is stored and the recipient has been asked. It is
	// delivered if and when they accept, and never before.
	StartHeld = "held"
	// StartNotDelivered: it did not get through. Nothing about why.
	StartNotDelivered = "not_delivered"
)

// ConversationStartDTO is the answer to message_start. The same shape carries
// all three outcomes; only `state` differs, and `conversation` is present only
// when there is one to give.
type ConversationStartDTO struct {
	State        string           `json:"state"`
	Conversation *ConversationDTO `json:"conversation,omitempty"`
	// PendingID names the message the server is now holding for this sender,
	// so a client can hand it back — with a Number — and have THAT sentence
	// delivered rather than typing a second one. It is the JSON twin of the
	// web's `p`, and it is what makes the Number keypad inside a thread release
	// the message the person actually wrote.
	//
	// It is present only where the answer has ALREADY said the person exists:
	// with `held`, which means somebody was asked, and on the ordinary send
	// path, where the caller and the recipient already share a conversation.
	// It is never attached to a `not_delivered` from message_start or
	// message_number, because there its presence or absence would be exactly
	// the existence oracle the single state refuses to be — a handle nobody
	// holds can have nothing held for it, and an id would say so.
	PendingID string `json:"pending_id,omitempty"`
}

// startStateFor maps a contact decision onto this lane's vocabulary. It is
// contactGateState's mapping and nothing else — one place decides what a
// contact outcome means, and both surfaces read it there.
func startStateFor(decision string) string {
	switch contactGateState(decision) {
	case "":
		return StartOpened
	case gnosis.PendingRequest:
		return StartHeld
	default:
		return StartNotDelivered
	}
}

// apiV1MessageEvent is the messaging vocabulary on POST /events:
//
//	message_start handle, body   write to a PERSON — open, knock, or nothing
//	message_send  c, body        write into a conversation that already exists
//	message_read  c              this member has read up to now
//
// message_send addresses a CONVERSATION and never a person; message_start is
// the one that addresses a person, and it takes the contact decision exactly
// where sendToPerson takes it — after something has been written, so whatever
// the authority answers is answered about a real sentence and the sentence is
// kept either way.
//
// Returns false for an event type it does not own, so the caller carries on.
func (h *Handler) apiV1MessageEvent(w http.ResponseWriter, r *http.Request, eventType string, rawBodyMap map[string]json.RawMessage) bool {
	switch eventType {
	case "message_send", "message_read", "message_start":
	default:
		return false
	}
	if h.db == nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return true
	}
	user := h.userFromRequest(w, r)
	if user == nil || user.ID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return true
	}
	if eventType == "message_start" {
		h.messageStartEvent(w, r, user, rawBodyMap)
		return true
	}
	convoID := eventField(r, rawBodyMap, "c", "conversation_id")
	if convoID == "" {
		http.Error(w, "conversation required", http.StatusBadRequest)
		return true
	}

	switch eventType {
	case "message_send":
		started := time.Now()
		msg, status, msgText := h.sendIntoConversation(user, convoID, eventField(r, rawBodyMap, "body"))
		if status != http.StatusOK {
			// A thread can outlive the permission that opened it, and when it
			// does the ordinary send is where that is discovered — which is
			// exactly where a client learns to offer the Number keypad. So a
			// message that did not arrive answers the SAME three-state shape
			// message_start and message_number answer, through the same
			// function, after the same floor. It is not an error: nothing was
			// wrong with the request, and the sentence simply did not land.
			//
			// Every other refusal here is a fact about the CALLER's own
			// request — an empty message, one too long, a conversation that is
			// not theirs, a sealed thread that needs the encrypted path — and
			// each keeps its own plain, immediate answer, because none of them
			// says anything about the person addressed.
			if apiWantsJSON(r) && msgText == sendNotDelivered {
				// The sentence is kept, exactly as it is on every other path
				// that could not deliver one, and it is NAMED — so the keypad
				// this answer is what makes the client offer can hand the same
				// message back with a Number and have that message delivered
				// rather than a second one typed over it.
				answerStartRefusal(w, started, StartNotDelivered,
					h.holdUndelivered(user, convoID, eventField(r, rawBodyMap, "body")))
				return true
			}
			http.Error(w, msgText, status)
			return true
		}
		if apiWantsJSON(r) {
			who := gnosis.Participant{Handle: user.Handle, Display: user.DisplayName, Avatar: user.AvatarURL}
			apiJSON(w, http.StatusCreated, messageDTO(msg, user.ID, who))
			return true
		}
		// The web's own answer, unchanged: the sender's out-bubble to append.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		h.renderPartial(w, "gnosis_bubble", map[string]interface{}{"M": msg, "Variant": "out"})

	case "message_read":
		if ok, err := gnosis.IsMember(h.db, convoID, user.ID); err != nil {
			log.Printf("[gnosis] membership for %s: %v", convoID, err)
			http.Error(w, "server error", http.StatusInternalServerError)
			return true
		} else if !ok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return true
		}
		if err := gnosis.MarkRead(h.db, convoID, user.ID); err != nil {
			log.Printf("[gnosis] marking %s read for %s: %v", convoID, user.ID, err)
			http.Error(w, "server error", http.StatusInternalServerError)
			return true
		}
		w.WriteHeader(http.StatusNoContent)
	}
	return true
}

// answerStartRefusal writes the one answer every non-allow outcome of
// message_start gets: the same status, the same shape, and never before the
// floor. It is one function on purpose — a refusal that took a second route out
// of the handler is a refusal that can be told apart from the first.
func answerStartRefusal(w http.ResponseWriter, started time.Time, state, pendingID string) {
	holdFloor(started, numberResolveFloor)
	apiJSON(w, http.StatusOK, ConversationStartDTO{State: state, PendingID: pendingID})
}

// answerAuthorityUnavailable is the one answer for a contact authority that did
// not answer at all — the URL is not configured, or the round trip failed.
//
// It is NOT one of the three states, and that is the point. The three states
// are what happened to a message; this is what happened to the request. Telling
// somebody their message did not get through when nothing was ever attempted is
// untrue, and it is the one thing the web's own gate refuses to do: its
// unavailable fragment says "Nobody was contacted".
//
// It discloses nothing, because it is not a fact about anybody. An authority
// that is down is down for every caller and every Number at once, so it
// separates no Number from any other. A refusal the authority DID take — a
// non-2xx it answered deliberately — is not this: that stays collapsed into the
// one refusal, because that one can depend on who was asked about.
//
// The floor is held anyway, so "every exit that is not an allow takes at least
// numberResolveFloor" stays true as one rule with no exceptions to remember.
func answerAuthorityUnavailable(w http.ResponseWriter, started time.Time) {
	holdFloor(started, numberResolveFloor)
	apiError(w, http.StatusServiceUnavailable, "contact_unavailable",
		"Nobody was contacted. Nothing was sent — try again in a moment.")
}

// answerStartOpened writes the one answer an ALLOW gets: the conversation that
// now exists, as the viewer's own sidebar row describes it.
//
// It is one function for the same reason answerStartRefusal is: message_start
// and message_number are two ways of naming a person and one act, and an
// "opened" that was assembled twice is an answer that can start differing
// between the lane that knew the handle and the lane that did not.
//
// It holds no floor. An allow has already disclosed everything it could — there
// is a conversation, and it is right here — so there is nothing left for the
// clock to give away, and slowing it down would only make the surface worse.
func (h *Handler) answerStartOpened(w http.ResponseWriter, user *model.User, convoID string) {
	h.answerStartOpenedHolding(w, user, convoID, "")
}

// answerStartOpenedHolding is answerStartOpened for the one case where a
// conversation is open AND a message is still waiting to go into it: a thread
// that opened SEALED with a plaintext message held for it. The server cannot
// seal, so it does not write; it says the door is open and names what is still
// in the sender's hands, which is the only account of that moment that is true.
func (h *Handler) answerStartOpenedHolding(w http.ResponseWriter, user *model.User, convoID, pendingID string) {
	out := ConversationStartDTO{State: StartOpened, PendingID: pendingID}
	row, found, err := gnosis.ConversationRowForAccount(h.db, convoID, user.ID)
	if err != nil {
		log.Printf("[gnosis] reading conversation %s for %s: %v", convoID, user.ID, err)
	}
	if found {
		d := conversationDTO(row)
		out.Conversation = &d
	}
	apiJSON(w, http.StatusOK, out)
}

// messageStartEvent writes to a PERSON: the first thing said to somebody there
// is no conversation with yet, and the only way a native client opens one.
//
// It is sendToPerson with a JSON answer, and it is deliberately the same act in
// the same order, because the order IS the security property:
//
//  1. the message is written before anything is decided, so a refusal can be
//     answered about a real sentence and the sentence is kept either way;
//  2. the message is HELD before the authority is asked, so nothing that
//     happens next can lose it;
//  3. every outcome that is not an allow answers in ONE shape after ONE
//     duration, so what actually happened is not readable from the answer or
//     from the clock.
//
// Three refusals reach this function by three different routes — a handle
// nobody holds, a policy that said no, an authority that did not answer — and
// all three leave it as StartNotDelivered after numberResolveFloor. The floor
// is held for the outcomes that disclose nothing; an allow already discloses
// everything it could, so it is not slowed down to hide from itself.
//
// What this lane does NOT do is send into a conversation that already exists.
// That is message_send, and it is a different act: the gate for it was passed
// once, when the conversation was opened, and re-asking it would knock on
// somebody every time their friend typed a sentence.
func (h *Handler) messageStartEvent(w http.ResponseWriter, r *http.Request, user *model.User, raw map[string]json.RawMessage) {
	started := time.Now()

	// Everything decided from the CALLER's own request is decided here, before
	// a single fact about anybody else is read. A malformed request and a
	// message to oneself disclose nothing about the person addressed, because
	// nothing about them has been looked at yet.
	body := strings.TrimSpace(eventField(r, raw, "body"))
	if body == "" {
		http.Error(w, "message required", http.StatusBadRequest)
		return
	}
	if len(body) > maxMessageBytes {
		http.Error(w, "message too long", http.StatusRequestEntityTooLarge)
		return
	}
	handle := strings.TrimPrefix(strings.TrimSpace(eventField(r, raw, "handle", "to")), "@")
	if handle == "" {
		http.Error(w, "handle required", http.StatusBadRequest)
		return
	}
	if strings.EqualFold(handle, user.Handle) {
		http.Error(w, "cannot message yourself", http.StatusBadRequest)
		return
	}

	// One shape, one duration, for everything that is not an allow.
	// A refusal on the HANDLE lane never names a held message, and that is the
	// whole reason this closure takes no id. A handle nobody holds can have
	// nothing held for it, so an id attached to `not_delivered` would say, by
	// being there, that the person exists — rebuilding in one field the oracle
	// the single state exists to deny. `held` is different: it has already said
	// somebody was asked, so naming what they were asked about adds nothing.
	refuse := func(state string) { answerStartRefusal(w, started, state, "") }
	opened := func(convoID string) { h.answerStartOpened(w, user, convoID) }

	target, err := dbpkg.GetUserByHandle(h.db, handle)
	if err != nil || target == nil || target.ID == user.ID {
		// Answered below, with the contact-initiation budget charged first: a
		// probe that cost nothing because nobody holds the handle would make
		// the budget itself the thing that says who exists.
		target = nil
	}

	// A conversation that is already open was already permitted. The gate is
	// not re-asked, and this is not a contact initiation, so it is not charged
	// as one.
	if target != nil {
		if convoID, _ := gnosis.FindDirectConversation(h.db, user.ID, target.ID); convoID != "" {
			// The plaintext goes in only if the conversation is plain. A sealed
			// thread never takes plaintext — the client holds what it typed and
			// seals it through /api/gnosis/send-sealed, which is the one path
			// that can.
			if convo, cerr := gnosis.GetConversation(h.db, convoID); cerr == nil && convo.Mode == gnosis.ModePlain {
				if _, status, reason := h.sendIntoConversation(user, convoID, body); status != http.StatusOK {
					http.Error(w, reason, status)
					return
				}
			}
			opened(convoID)
			return
		}
	}

	// Past this point the act is a contact INITIATION: it holds a message for a
	// stranger, asks the contact authority about them, and can push a
	// notification at them. It has its own far stricter budget, and it is
	// charged BEFORE the target is known to exist so that an attempt against a
	// handle nobody holds costs exactly what an attempt against a real one does.
	if !h.rlContactInit.Allow(r) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "Too many contact attempts — slow down", http.StatusTooManyRequests)
		return
	}
	if target == nil {
		refuse(StartNotDelivered)
		return
	}

	// Held before it is judged, so nothing that happens next can lose it.
	pending, err := gnosis.UpsertPending(h.db, user.ID, target.ID, body)
	if err != nil {
		log.Printf("[gnosis] holding a message for %s: %v", handle, err)
		refuse(StartNotDelivered)
		return
	}
	// Every refusal records what was answered, so re-opening this does not
	// knock on the recipient a second time to find out what to say.
	gate := func(decision string) {
		if err := gnosis.SetPendingDecision(h.db, pending.ID, decision); err != nil {
			log.Printf("[gnosis] recording a contact decision for %s: %v", target.Handle, err)
		}
		state := startStateFor(decision)
		held := ""
		if state == StartHeld {
			held = pending.ID
		}
		answerStartRefusal(w, started, state, held)
	}

	targetPIAL := dbpkg.ResolvePIAL(h.db, target.ID)
	if targetPIAL == "" {
		gate(gnosis.PendingUnavailable)
		return
	}
	// Knowing a handle is not permission to open a conversation. The contact
	// policy authority decides, and an unreachable authority refuses: an
	// unknown policy is not an open one.
	outcome, err := h.evaluateContact(r.Context(), user, contactQuery{
		TargetPIAL: targetPIAL,
		Note:       contactNote(body),
	})
	if err != nil {
		log.Printf("[gnosis] contact decision for %s: %v", handle, err)
		if errors.Is(err, errPolicyAuthorityDown) {
			// Nothing was attempted, so nothing is reported as having failed to
			// arrive. The message stays held and the caller may try again.
			if derr := gnosis.SetPendingDecision(h.db, pending.ID, gnosis.PendingUnavailable); derr != nil {
				log.Printf("[gnosis] recording a contact decision for %s: %v", target.Handle, derr)
			}
			answerAuthorityUnavailable(w, started)
			return
		}
		gate(gnosis.PendingUnavailable)
		return
	}
	// The one notification pipeline, reached the one way.
	h.notifyContactRequest(user, outcome)

	if state := contactGateState(outcome.Decision); state != "" {
		gate(state)
		return
	}
	convoID, err := h.ensureDirectConversation(user, target.ID, targetPIAL)
	if err != nil {
		log.Printf("[gnosis] create conversation: %v", err)
		gate(gnosis.PendingUnavailable)
		return
	}
	// Permitted: the held message is delivered, unless the conversation opened
	// SEALED — sealing needs the key bundle that permission has only just
	// granted, so a message written before it cannot have been sealed, and
	// writing it as plaintext would put one readable message inside an
	// end-to-end encrypted thread. It stays held for the client to seal.
	if convo, cerr := gnosis.GetConversation(h.db, convoID); cerr == nil && convo.Mode == gnosis.ModePlain {
		if _, status, reason := h.sendIntoConversation(user, convoID, pending.Body); status != http.StatusOK {
			log.Printf("[gnosis] delivering a held message into %s: %s", convoID, reason)
		}
	}
	opened(convoID)
}
