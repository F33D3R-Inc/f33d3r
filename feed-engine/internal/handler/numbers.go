package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/gnosis"
	"github.com/f33d3r/feed-engine/internal/model"
)

// ── F33D3R Numbers ───────────────────────────────────────────────────────────
//
// A Number is a speakable, rotatable contact name: twelve decimal digits, the
// twelfth a Luhn check digit over the other eleven, rendered 0412 8837 2919.
// It is not a telephone number, not an identity, not a database id and not a
// key — and it carries no area code, no country code and no prefix of any kind,
// because a prefix that encoded where somebody was cannot be rotated away.
//
// Numbers minted before the digit format are thirteen Crockford base32 symbols
// and keep working unchanged; the two shapes are disjoint by length.
//
// Resolving one yields a POLICY DECISION and nothing else. Whoever obtains a
// Number may attempt contact subject to that Number's policy; they learn no
// email, no phone, no handle, no profile, no account id and no key material
// unless the policy allows contact.
//
// Allocation, rotation and revocation live in Manhattan. Policy, capabilities
// and the decision live in elohim-veni. This file is the HTTP/HTMX surface and
// owns neither.

// numberResolveFloor is the minimum wall-clock duration of a public resolve.
// Valid, revoked, unknown and malformed Numbers must be indistinguishable by
// clock as well as by status code and response shape.
const numberResolveFloor = 150 * time.Millisecond

// errPolicyAuthorityDown is returned when elohim-veni cannot be reached. Contact
// is refused rather than allowed: an unknown policy is not an open one.
var errPolicyAuthorityDown = errors.New("contact policy authority unavailable")

// elohimRefusalMax bounds how much of a refusal body is read. A refusal carries
// one short code; anything larger is not one.
const elohimRefusalMax = 4 << 10

// elohimRefusal is a refusal the policy authority took deliberately, carrying
// the code it stated. Code is empty for a failure that named no reason, which is
// treated as "unavailable" and never as a decision.
type elohimRefusal struct {
	Status int
	Code   string
	Path   string
}

func (e *elohimRefusal) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("elohim-veni %s: %d", e.Path, e.Status)
	}
	return fmt.Sprintf("elohim-veni %s: %d %s", e.Path, e.Status, e.Code)
}

// statedRefusal returns the code the authority stated, or "" when it stated
// none — a transport failure, or an error it did not intend as an answer.
func statedRefusal(err error) string {
	var refusal *elohimRefusal
	if errors.As(err, &refusal) {
		return refusal.Code
	}
	return ""
}

// numberDisplay renders the canonical hyphenated form as spoken groups: 4-4-4
// for a Number, 4-4-5 for one minted before the digit format. Anything that is
// not either shape is returned untouched rather than sliced.
func numberDisplay(number string) string {
	bare := strings.NewReplacer("-", "", " ", "", "_", "", ".", "").
		Replace(strings.ToUpper(strings.TrimSpace(number)))
	if len(bare) != numberTotalLen && len(bare) != numberLegacyTotalLen {
		return number
	}
	return bare[0:4] + " " + bare[4:8] + " " + bare[8:]
}

// ── The owner's own surface ──────────────────────────────────────────────────

type numberRow struct {
	Number  string
	Display string
	Live    bool
	Policy  string
	// Legacy marks one of the Crockford base32 Numbers minted before the digit
	// format. It is owner-private, like everything else on this row: it exists
	// so somebody still holding an older Number can see that it is older and
	// choose to mint a new one. Nothing a resolver ever sees carries it.
	Legacy bool
	// Label is owner-private. It exists so someone holding five Numbers knows
	// which one to retire when the conference ends and the class is still
	// running. It is never disclosed to anybody resolving the Number.
	Label string
	// Lease is what the authority says about this Number's expiry and use
	// budget, already in words. Owner-private for the same reason: how much of a
	// budget is left would tell a stranger how many other people hold the same
	// Number.
	Lease numberLease
}

type contactRequestRow struct {
	ID     string
	Handle string
	Name   string
	Avatar string
	Note   string
	When   string
}

type capabilityRow struct {
	ID      string
	Label   string
	Uses    int
	MaxUses int
	Revoked bool
}

// facetNumbers — GET /facets/identity/numbers
func (h *Handler) facetNumbers(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	h.renderNumbers(w, r, user.PIALID, "", "")
}

// mintNumber — POST /identity/numbers/mint
// An identity may hold several Numbers at once — one per badge, per event, per
// campaign — and retire them independently.
func (h *Handler) mintNumber(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// The authority stores this Number's policy against the identity, so the
	// identity has to exist there first. Refusing here is the same refusal the
	// mint would produce a moment later, with the cause named.
	if err := h.assertPIAL(r.Context(), user.PIALID); err != nil {
		h.renderNumbers(w, r, user.PIALID, "Could not allocate a Number right now.", "")
		return
	}
	policy := strings.TrimSpace(r.FormValue("policy"))
	if policy == "" {
		policy = "number_only"
	}
	// The lease the owner chose: how long this Number keeps admitting people and
	// how many it may admit. Shape is checked here; the BOUNDS belong to
	// elohim-veni, which owns the columns.
	lease, err := parseNumberLease(r)
	if err != nil {
		h.renderNumbers(w, r, user.PIALID, "An expiry is a whole number of days and a budget is a whole number of people.", "")
		return
	}
	body := map[string]interface{}{
		"pial_id": user.PIALID,
		"policy":  policy,
		"label":   strings.TrimSpace(r.FormValue("label")),
	}
	// Asking for a Number back rather than being given a new one. The authority
	// decides whether this identity may have it; nothing here does.
	if reclaim := strings.TrimSpace(r.FormValue("reclaim")); reclaim != "" {
		body["reclaim"] = reclaim
	}
	lease.applyTo(body)
	var out struct {
		Number string `json:"number"`
	}
	if err := h.callElohim(r.Context(), http.MethodPost, "/v1/numbers/mint", body, &out); err != nil {
		log.Printf("[numbers] minting for pial %s: %v", user.PIALID, err)
		h.renderNumbers(w, r, user.PIALID, mintRefusalNotice(statedRefusal(err)), "")
		return
	}
	h.renderNumbers(w, r, user.PIALID, "", out.Number)
}

// mintRefusalNotice turns the authority's stated reason into something the owner
// can act on.
//
// Every one of these refusals has an action attached, and naming it is the whole
// point: an owner told "could not allocate a Number right now" retries and gets
// the same answer for ever, while an owner told they are holding five retires
// one and carries on. A refusal that stated no reason stays vague, because
// inventing a cause for it would be worse than admitting there is none.
func mintRefusalNotice(code string) string {
	switch code {
	case "number_cap_reached":
		return "You are already holding as many Numbers as one account may hold at once. " +
			"Retire one you have finished with — retiring is instant and never limited — and " +
			"this will work."
	case "number_mint_rate":
		return "You have created a lot of Numbers recently, so new ones are being paced. " +
			"Try again a little later. Retiring a Number is never paced and works right now."
	case "number_not_reclaimable":
		return "That Number is not one you can take back."
	default:
		return "Could not allocate a Number right now."
	}
}

// revokeNumber — POST /identity/numbers/revoke
// Rotation. The identity, its keys, its devices, its sessions and every open
// conversation are untouched; only future contact through this Number stops.
func (h *Handler) revokeNumber(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	number := strings.TrimSpace(r.FormValue("number"))
	if number == "" {
		http.Error(w, "number required", http.StatusBadRequest)
		return
	}
	body := map[string]interface{}{"pial_id": user.PIALID, "number": number}
	if err := h.callElohim(r.Context(), http.MethodPost, "/v1/numbers/revoke", body, nil); err != nil {
		log.Printf("[numbers] revoking %s: %v", number, err)
		h.renderNumbers(w, r, user.PIALID, "Could not retire that Number right now.", "")
		return
	}
	h.renderNumbers(w, r, user.PIALID, "", "")
}

// setNumberPolicy — POST /identity/numbers/policy
// A conference-badge Number and a business-card Number are different promises.
func (h *Handler) setNumberPolicy(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	number := strings.TrimSpace(r.FormValue("number"))
	policy := strings.TrimSpace(r.FormValue("policy"))
	if number == "" || policy == "" {
		http.Error(w, "number and policy required", http.StatusBadRequest)
		return
	}
	// A form that never mentioned the expiry or the budget sends neither, so the
	// authority leaves them exactly as they are. Changing who may reach you
	// through a Number must not silently discard when it stops working.
	lease, err := parseNumberLease(r)
	if err != nil {
		h.renderNumbers(w, r, user.PIALID, "An expiry is a whole number of days and a budget is a whole number of people.", "")
		return
	}
	body := map[string]interface{}{
		"pial_id": user.PIALID,
		"number":  number,
		"policy":  policy,
		"label":   strings.TrimSpace(r.FormValue("label")),
	}
	lease.applyTo(body)
	if err := h.callElohim(r.Context(), http.MethodPost, "/v1/numbers/policy", body, nil); err != nil {
		log.Printf("[numbers] policy for %s: %v", number, err)
		h.renderNumbers(w, r, user.PIALID, "Could not change that Number's policy right now.", "")
		return
	}
	h.renderNumbers(w, r, user.PIALID, "", "")
}

// setContactPolicy — POST /identity/contact/policy
// The identity-wide defaults: who may reach this person by @handle, and the
// policy a freshly minted Number inherits.
func (h *Handler) setContactPolicy(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := h.assertPIAL(r.Context(), user.PIALID); err != nil {
		h.renderNumbers(w, r, user.PIALID, "Could not save your contact policy right now.", "")
		return
	}
	body := map[string]interface{}{"pial_id": user.PIALID}
	if v := strings.TrimSpace(r.FormValue("handle_policy")); v != "" {
		body["handle_policy"] = v
	}
	if v := strings.TrimSpace(r.FormValue("default_number_policy")); v != "" {
		body["default_number_policy"] = v
	}
	if err := h.callElohim(r.Context(), http.MethodPost, "/v1/contact/policy", body, nil); err != nil {
		log.Printf("[numbers] contact policy for pial %s: %v", user.PIALID, err)
		h.renderNumbers(w, r, user.PIALID, "Could not save your contact policy right now.", "")
		return
	}
	h.renderNumbers(w, r, user.PIALID, "", "")
}

// mintContactCapability — POST /identity/contact/capability/mint
// The bearer half of contact: 160 bits, never spoken, handed over as a link.
// Returned once and never again — only its hash is stored.
func (h *Handler) mintContactCapability(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := h.assertPIAL(r.Context(), user.PIALID); err != nil {
		h.renderNumbers(w, r, user.PIALID, "Could not create a contact link right now.", "")
		return
	}
	body := map[string]interface{}{
		"pial_id": user.PIALID,
		"label":   strings.TrimSpace(r.FormValue("label")),
	}
	if v := strings.TrimSpace(r.FormValue("number")); v != "" {
		body["number"] = v
	}
	if v := strings.TrimSpace(r.FormValue("max_uses")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			http.Error(w, "max_uses must be a positive whole number", http.StatusBadRequest)
			return
		}
		body["max_uses"] = n
	}
	var out struct {
		Capability string `json:"capability"`
	}
	if err := h.callElohim(r.Context(), http.MethodPost, "/v1/contact/capabilities/mint", body, &out); err != nil {
		log.Printf("[numbers] minting capability for pial %s: %v", user.PIALID, err)
		h.renderNumbers(w, r, user.PIALID, "Could not create a contact link right now.", "")
		return
	}
	h.renderNumbersWithCapability(w, r, user.PIALID, out.Capability)
}

// revokeContactCapability — POST /identity/contact/capability/revoke
func (h *Handler) revokeContactCapability(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := strings.TrimSpace(r.FormValue("capability_id"))
	if id == "" {
		http.Error(w, "capability_id required", http.StatusBadRequest)
		return
	}
	body := map[string]interface{}{"pial_id": user.PIALID, "capability_id": id}
	if err := h.callElohim(r.Context(), http.MethodPost, "/v1/contact/capabilities/revoke", body, nil); err != nil {
		log.Printf("[numbers] revoking capability %s: %v", id, err)
		h.renderNumbers(w, r, user.PIALID, "Could not revoke that contact link right now.", "")
		return
	}
	h.renderNumbers(w, r, user.PIALID, "", "")
}

// decideContactRequest — POST /identity/contact/requests/decide
func (h *Handler) decideContactRequest(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := strings.TrimSpace(r.FormValue("request_id"))
	if id == "" {
		http.Error(w, "request_id required", http.StatusBadRequest)
		return
	}
	accept := r.FormValue("accept") == "1"
	body := map[string]interface{}{
		"pial_id":    user.PIALID,
		"request_id": id,
		"accept":     accept,
	}
	var out struct {
		RequesterPIAL string `json:"requester_pial"`
	}
	if err := h.callElohim(r.Context(), http.MethodPost, "/v1/contact/requests/decide", body, &out); err != nil {
		log.Printf("[numbers] deciding request %s: %v", id, err)
		h.renderNumbers(w, r, user.PIALID, "Could not answer that request right now.", "")
		return
	}
	if accept && out.RequesterPIAL != "" {
		if requester := dbpkg.AccountForPIAL(h.db, out.RequesterPIAL); requester != "" {
			h.deliverHeldOnAccept(requester, user)
			h.notifyUser(requester, "contact_accepted", user.ID, user.ID, "user")
		}
	}
	h.renderNumbers(w, r, user.PIALID, "", "")
}

// deliverHeldOnAccept keeps the promise the contact gate made.
//
// The gate tells a sender, in these words: "Nothing here was delivered yet; if
// they accept, this message goes with it." Accepting therefore has to deliver
// it. Without this, accepting opened nothing, the held message stayed held, and
// the next thing the sender typed at that person overwrote it — so the sentence
// the recipient actually said yes to was destroyed by the reply to it.
//
// A held message is the sender's until it lands, so every failure here leaves
// the row alone: the message stays waiting and is offered back in the sender's
// composer rather than being lost to a bad moment on the network.
func (h *Handler) deliverHeldOnAccept(requesterAccount string, accepter *model.User) {
	held, found, err := gnosis.FindPending(h.db, requesterAccount, accepter.ID)
	if err != nil {
		log.Printf("[gnosis] reading a held message from %s: %v", requesterAccount, err)
		return
	}
	if !found {
		return
	}

	requester, err := dbpkg.GetUserByID(h.db, requesterAccount)
	if err != nil || requester == nil {
		log.Printf("[gnosis] resolving the sender %s of a held message: %v", requesterAccount, err)
		return
	}

	convoID, err := h.ensureDirectConversation(requester, accepter.ID, accepter.PIALID)
	if err != nil {
		log.Printf("[gnosis] opening a conversation for an accepted request: %v", err)
		return
	}

	convo, err := gnosis.GetConversation(h.db, convoID)
	if err != nil {
		log.Printf("[gnosis] reading conversation %s: %v", convoID, err)
		return
	}
	// A SEALED conversation cannot be written into by the server: sealing needs
	// the sender's device key, which the server does not have and must not have.
	// The message stays the sender's, and the thread hands it back to the
	// composer that can seal it the moment they open it — the one case a held
	// message legitimately outlives the conversation opening. See
	// FindPendingForConversationPeer.
	if convo.Mode == gnosis.ModeSealed {
		return
	}

	msg, err := gnosis.InsertPlainMessage(h.db, convoID, requesterAccount, held.Body)
	if err != nil {
		log.Printf("[gnosis] delivering an accepted message into %s: %v", convoID, err)
		return
	}
	h.fanoutMessage(convoID, requesterAccount, msg)
	if err := gnosis.DeletePending(h.db, held.ID); err != nil {
		log.Printf("[gnosis] releasing delivered message %s: %v", held.ID, err)
	}
}

// ── The public resolver ──────────────────────────────────────────────────────

// resolveNumber — POST /contact/resolve
// The only public entry point for a Number. It answers with a policy decision,
// rendered server-side. A hit reveals who the Number belongs to only when that
// Number's policy allows contact; every other outcome — unknown, retired,
// malformed, closed — renders the identical fragment after an identical minimum
// duration, so the endpoint is not an existence oracle.
func (h *Handler) resolveNumber(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	number := strings.TrimSpace(r.FormValue("number"))
	capability := strings.TrimSpace(r.FormValue("capability"))
	note := strings.TrimSpace(r.FormValue("note"))
	if len(note) > 280 {
		note = note[:280]
	}

	outcome, err := h.evaluateContact(r.Context(), user, contactQuery{
		Number:     number,
		Capability: capability,
		Note:       note,
	})
	if err != nil {
		holdFloor(started, numberResolveFloor)
		log.Printf("[numbers] resolving for pial %s: %v", user.PIALID, err)
		h.renderPartial(w, "number_resolve", map[string]interface{}{
			"PIALID":      user.PIALID,
			"Unavailable": true,
		})
		return
	}

	// A request that nobody is told about is a request nobody answers, so the
	// person it landed on is notified through the one notification pipeline this
	// brain has. Nothing about them is rendered back to the caller.
	h.notifyContactRequest(user, outcome)

	// Deliberately no handle, no display name, no avatar, no account id. A
	// resolve yields the decision and the Number the caller already typed. The
	// identity behind it surfaces only once a conversation actually opens.
	holdFloor(started, numberResolveFloor)
	h.renderPartial(w, "number_resolve", map[string]interface{}{
		"PIALID":   user.PIALID,
		"Decision": outcome.Decision,
		"Number":   number,
	})
}

// startContactByNumber — POST /contact/start
// Completes the purpose chain: Number → policy decision → conversation. The
// decision is taken again here rather than trusted from the resolve, so a client
// replaying a form cannot skip the gate.
func (h *Handler) startContactByNumber(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	number := strings.TrimSpace(r.FormValue("number"))
	if number == "" {
		http.Error(w, "number required", http.StatusBadRequest)
		return
	}

	outcome, err := h.evaluateContact(r.Context(), user, contactQuery{
		Number:     number,
		Capability: strings.TrimSpace(r.FormValue("capability")),
	})
	if err != nil {
		holdFloor(started, numberResolveFloor)
		log.Printf("[numbers] starting contact for pial %s: %v", user.PIALID, err)
		h.renderPartial(w, "number_resolve", map[string]interface{}{
			"PIALID": user.PIALID, "Unavailable": true})
		return
	}
	h.notifyContactRequest(user, outcome)
	if outcome.Decision != "allow" || outcome.TargetPIAL == "" {
		holdFloor(started, numberResolveFloor)
		h.renderPartial(w, "number_resolve", map[string]interface{}{
			"PIALID": user.PIALID, "Decision": outcome.Decision, "Number": number})
		return
	}

	account := dbpkg.AccountForPIAL(h.db, outcome.TargetPIAL)
	if account == "" || account == user.ID {
		holdFloor(started, numberResolveFloor)
		h.renderPartial(w, "number_resolve", map[string]interface{}{
			"PIALID": user.PIALID, "Decision": "deny", "Number": number})
		return
	}
	if _, err := h.ensureDirectConversation(user, account, outcome.TargetPIAL); err != nil {
		holdFloor(started, numberResolveFloor)
		log.Printf("[numbers] opening conversation for pial %s: %v", user.PIALID, err)
		h.renderPartial(w, "number_resolve", map[string]interface{}{
			"PIALID": user.PIALID, "Unavailable": true})
		return
	}

	holdFloor(started, numberResolveFloor)
	w.Header().Set("HX-Redirect", "/messages")
	h.renderPartial(w, "number_resolve", map[string]interface{}{
		"PIALID": user.PIALID, "Decision": "allow", "Number": number})
}

// holdFloor keeps a handler alive to a fixed minimum duration, so the work it
// actually did is not readable from the clock.
func holdFloor(started time.Time, floor time.Duration) {
	if elapsed := time.Since(started); elapsed < floor {
		time.Sleep(floor - elapsed)
	}
}

// contactQuery is one contact attempt: by Number, or by target identity on the
// @handle path.
type contactQuery struct {
	Number     string
	TargetPIAL string
	Capability string
	Note       string
}

// contactOutcome is what the authority decided, and the little it discloses
// about whom. TargetPIAL is populated only on allow. RequestOwnerPIAL is
// populated only when a contact request was actually opened, so the person whose
// inbox it landed in can be told about it; neither ever reaches the browser.
type contactOutcome struct {
	Decision         string
	TargetPIAL       string
	RequestOwnerPIAL string
	RequestID        string
	// BudgetAlert is what the OWNER of the Number should be told about what is
	// left of its budget: "low", "spent", or empty for nothing to say. The
	// authority sends it only alongside an allow, where the target is already
	// disclosed, so it never travels with a refusal and never reaches the
	// person who typed the Number. BudgetOwnerPIAL is whose inbox it belongs in.
	BudgetAlert     string
	BudgetOwnerPIAL string
}

// evaluateContact asks elohim-veni whether contact may proceed.
//
// This call carries no follow-graph facts, and that is the point. It used to
// carry `follows` and `mutual` as plain booleans, which meant a security
// decision taken in one brain rested on an assertion the deciding brain could
// not verify — and which any caller holding the internal key could simply set.
//
// It also could not work. On the Number path the target is not disclosed before
// the decision, so there was nobody to compute the facts about, so the fields
// were absent and elohim-veni's `followers` and `mutuals` policies degraded to a
// contact request for people who genuinely were mutual follows. Both problems
// have the same root: the wrong brain was answering the question.
//
// This brain publishes the follow graph to Manhattan's association plane
// (migration 0017), and elohim-veni — which knows the target because it resolved
// the Number itself — reads the relationship there. The privacy property is
// untouched: nothing on this path learns who a Number belongs to unless the
// policy already allowed contact.
func (h *Handler) evaluateContact(ctx context.Context, user *model.User, q contactQuery) (contactOutcome, error) {
	body := map[string]interface{}{"initiator_pial": user.PIALID}
	if q.Number != "" {
		// The check digit, before any I/O — and here that means before the HTTP
		// hop to the policy authority, not merely before its database read.
		// Nine of every ten blind guesses die on this line, inside this process,
		// having cost a guesser one function call and this stack nothing.
		//
		// A refusal here is the SAME refusal an unresolved Number produces, with
		// the same decision and the same empty target, because the surfaces above
		// hold their own floor and render deny identically however it arose. The
		// one surface that distinguishes a mistyped Number does its own
		// canonicalisation first and never reaches this line.
		canonical, wellFormed := canonicalNumber(q.Number)
		if !wellFormed {
			return contactOutcome{Decision: "deny"}, nil
		}
		body["number"] = canonical
	}
	if q.TargetPIAL != "" {
		body["target_pial"] = q.TargetPIAL
	}
	if q.Capability != "" {
		body["capability"] = q.Capability
	}
	if q.Note != "" {
		body["note"] = q.Note
	}

	var out struct {
		Decision         string `json:"decision"`
		TargetPIAL       string `json:"target_pial"`
		RequestOwnerPIAL string `json:"request_owner_pial"`
		RequestID        string `json:"request_id"`
		BudgetAlert      string `json:"budget_alert"`
		BudgetOwnerPIAL  string `json:"budget_owner_pial"`
	}
	if err := h.callElohim(ctx, http.MethodPost, "/v1/contact/evaluate", body, &out); err != nil {
		return contactOutcome{}, err
	}
	if out.Decision == "" {
		return contactOutcome{}, errors.New("contact authority returned no decision")
	}
	return contactOutcome{
		Decision:         out.Decision,
		TargetPIAL:       out.TargetPIAL,
		RequestOwnerPIAL: out.RequestOwnerPIAL,
		RequestID:        out.RequestID,
		BudgetAlert:      out.BudgetAlert,
		BudgetOwnerPIAL:  out.BudgetOwnerPIAL,
	}, nil
}

// notifyContactRequest delivers everything one contact decision owes to somebody
// other than the person who caused it, through the one notification pipeline
// this brain has.
//
// It is called on every path that evaluates contact, which is why both deliveries
// live behind it rather than beside it: a second call somebody has to remember to
// add is a notification somebody eventually does not get. Each part is a no-op
// unless the authority actually said something, so a repeated resolve against a
// still-pending request does not re-notify.
//
//	a contact request → the person it landed on
//	a budget warning  → the owner of the Number that is running out
func (h *Handler) notifyContactRequest(user *model.User, o contactOutcome) {
	h.notifyNumberBudget(o)
	if o.RequestID == "" || o.RequestOwnerPIAL == "" {
		return
	}
	owner := dbpkg.AccountForPIAL(h.db, o.RequestOwnerPIAL)
	if owner == "" || owner == user.ID {
		return
	}
	h.notifyUser(owner, "contact_request", user.ID, user.ID, "user")
}

// notifyNumberBudget tells the owner of a Number that its budget is running out,
// or has just run out, through the one notification pipeline this brain has.
//
// This is the other half of a property that would otherwise be a trap. A spent
// budget is INVISIBLE to whoever presents the Number — byte-identical and
// time-identical to unknown, retired and expired — which is exactly right, and
// which means that without this the owner's Number would stop working and
// nobody, on either side, would be able to tell why. So the moment it stops is
// the moment its owner is told, and they are warned once on the way down.
//
// The alert names nobody. It is a fact about the owner's own Number, and who
// spent the last of it is not the owner's business to be handed unasked — the
// person admitted is now a conversation they can see for themselves.
func (h *Handler) notifyNumberBudget(o contactOutcome) {
	if o.BudgetAlert == "" || o.BudgetOwnerPIAL == "" {
		return
	}
	kind := ""
	switch o.BudgetAlert {
	case "low":
		kind = "number_budget_low"
	case "spent":
		kind = "number_budget_spent"
	default:
		// An alert this build does not recognise is not guessed at. A wrong
		// sentence in somebody's notifications is worse than a missing one.
		log.Printf("[numbers] unrecognised budget alert %q", o.BudgetAlert)
		return
	}
	owner := dbpkg.AccountForPIAL(h.db, o.BudgetOwnerPIAL)
	if owner == "" {
		return
	}
	// The notification has NO actor. Nothing about the person who was admitted
	// travels here, and naming the owner as their own actor would be a
	// self-notification — which is dropped, which is how this alert came to be
	// written and never delivered. It is the platform stating a fact about the
	// owner's own Number, and it points at the surface that can act on it.
	h.notifyUser(owner, kind, "", owner, "user")
}

// ── Rendering ────────────────────────────────────────────────────────────────

func (h *Handler) renderNumbers(w http.ResponseWriter, r *http.Request, pialID, notice, highlight string) {
	h.renderNumbersFacet(w, r, pialID, notice, highlight, "")
}

func (h *Handler) renderNumbersWithCapability(w http.ResponseWriter, r *http.Request, pialID, capability string) {
	h.renderNumbersFacet(w, r, pialID, "", "", capability)
}

// renderNumbersFacet draws the f33d3r_numbers Facet. Every mutation above ends
// here, so the fragment the browser swaps in is always the server's current
// account of this identity's Numbers, policy, links and pending requests.
func (h *Handler) renderNumbersFacet(w http.ResponseWriter, r *http.Request, pialID, notice, highlight, capability string) {
	data := h.numbersData(r.Context(), pialID)
	data["Notice"] = notice
	if highlight != "" {
		data["NewNumber"] = numberDisplay(highlight)
	}
	if capability != "" {
		data["NewCapability"] = capability
	}
	h.renderPartial(w, "f33d3r_numbers", data)
}

// ── What the authority says about one identity's Numbers ─────────────────────
//
// The three readers below are the ONE place this brain decodes elohim-veni's
// account of an identity's Numbers and contact settings. Both surfaces read
// them: the Facet the browser swaps in, and the JSON the native clients draw
// their own screens from. A second copy of these shapes is a second place for
// them to drift away from the brain that writes them.

// contactPolicies is the contact-policy vocabulary, mirroring elohim-veni's
// contact::POLICIES in the order that brain declares it.
//
// It is a fallback, not a second authority: every surface prefers the list the
// authority states on its own contact-policy response, and falls back here only
// when that call did not answer. Nothing on either surface may offer a policy
// that is not in the list it was handed, because the authority refuses one.
func contactPolicies() []string {
	return []string{"open", "followers", "mutuals", "number_only", "capability_only", "closed"}
}

// isContactPolicy reports whether a policy is one of the six. The authority
// checks this too and is the one that decides; checking here means a caller who
// typed a word nobody defined is told so, instead of being told the allocation
// failed.
func isContactPolicy(policy string) bool {
	for _, p := range contactPolicies() {
		if p == policy {
			return true
		}
	}
	return false
}

// numberEntry is one Number as its owner's own listing describes it. Every
// field on it is owner-private: a resolve carries none of them, and no refusal
// ever names one.
type numberEntry struct {
	Number string `json:"number"`
	Status string `json:"status"`
	Policy string `json:"policy"`
	Label  string `json:"label"`
	// The lease, as the authority that enforces it reports it. The countdown is
	// measured against elohim-veni's own clock — the same one that stamped the
	// lease — so nothing here is worked out twice.
	Legacy           bool   `json:"legacy"`
	CreatedAt        string `json:"created_at"`
	ExpiresInSeconds *int64 `json:"expires_in_seconds"`
	MaxAdmissions    *int   `json:"max_admissions"`
	Admissions       int    `json:"admissions"`
	LeaseState       string `json:"lease_state"`
}

// listNumbers reads one identity's own Numbers from the brain that allocated
// them. It is addressed by PIAL and answers only about that identity — there is
// no call here, and must never be one, that goes the other way from a Number to
// whoever holds it.
func (h *Handler) listNumbers(ctx context.Context, pialID string) ([]numberEntry, error) {
	var out struct {
		Numbers []numberEntry `json:"numbers"`
	}
	if err := h.callElohim(ctx, http.MethodGet, "/v1/numbers/"+url.PathEscape(pialID), nil, &out); err != nil {
		return nil, err
	}
	return out.Numbers, nil
}

// contactPolicyView is one identity's contact settings: who may reach it by
// @handle, what a freshly minted Number inherits, and the vocabulary both are
// drawn from.
type contactPolicyView struct {
	HandlePolicy        string   `json:"handle_policy"`
	DefaultNumberPolicy string   `json:"default_number_policy"`
	Policies            []string `json:"policies"`
}

func (h *Handler) contactPolicyFor(ctx context.Context, pialID string) (contactPolicyView, error) {
	var out contactPolicyView
	if err := h.callElohim(ctx, http.MethodGet,
		"/v1/contact/policy/"+url.PathEscape(pialID), nil, &out); err != nil {
		return contactPolicyView{}, err
	}
	return out, nil
}

// numbersData hydrates the Numbers surface for one identity. Every field is
// fetched from the brain that owns it; nothing is cached here.
func (h *Handler) numbersData(ctx context.Context, pialID string) map[string]interface{} {
	data := map[string]interface{}{
		"PIALID":   pialID,
		"Policies": contactPolicies(),
		// The lease vocabulary the owner picks from. Server-rendered like
		// everything else: the browser is handed a list of choices, not the rules
		// behind them.
		"ExpiryOptions":       numberExpiryOptions(),
		"ExpiryChangeOptions": numberExpiryChangeOptions(),
		"ExpiryDaysMax":       numberExpiryDaysMax,
		"BudgetMax":           numberBudgetMax,
		"BudgetOptions":       numberBudgetOptions(),
	}
	if h.cfg.ElohimVeniURL == "" {
		data["Unavailable"] = true
		data["Notice"] = "F33D3R Numbers are not available on this deployment yet."
		return data
	}

	numbers, err := h.listNumbers(ctx, pialID)
	if err != nil {
		log.Printf("[numbers] listing for pial %s: %v", pialID, err)
		data["Unavailable"] = true
		data["Notice"] = "Could not load your Numbers right now."
		return data
	}
	rows := make([]numberRow, 0, len(numbers))
	for _, n := range numbers {
		rows = append(rows, numberRow{
			Number:  n.Number,
			Display: numberDisplay(n.Number),
			Live:    n.Status == "active",
			Policy:  n.Policy,
			Legacy:  n.Legacy,
			Label:   n.Label,
			Lease: numberLeaseFrom(
				n.LeaseState, n.ExpiresInSeconds, n.MaxAdmissions, n.Admissions),
		})
	}
	data["Numbers"] = rows

	if policy, err := h.contactPolicyFor(ctx, pialID); err != nil {
		log.Printf("[numbers] contact policy for pial %s: %v", pialID, err)
	} else {
		data["HandlePolicy"] = policy.HandlePolicy
		data["DefaultNumberPolicy"] = policy.DefaultNumberPolicy
		// The authority's own vocabulary when it states one, so the picker the
		// owner sees is the set the authority will actually accept.
		if len(policy.Policies) > 0 {
			data["Policies"] = policy.Policies
		}
	}

	var caps struct {
		Capabilities []struct {
			ID      string `json:"id"`
			Label   string `json:"label"`
			Uses    int    `json:"uses"`
			MaxUses *int   `json:"max_uses"`
			Revoked bool   `json:"revoked"`
		} `json:"capabilities"`
	}
	if err := h.callElohim(ctx, http.MethodGet, "/v1/contact/capabilities/"+url.PathEscape(pialID), nil, &caps); err != nil {
		log.Printf("[numbers] capabilities for pial %s: %v", pialID, err)
	} else {
		capRows := make([]capabilityRow, 0, len(caps.Capabilities))
		for _, c := range caps.Capabilities {
			row := capabilityRow{ID: c.ID, Label: c.Label, Uses: c.Uses, Revoked: c.Revoked}
			if c.MaxUses != nil {
				row.MaxUses = *c.MaxUses
			}
			capRows = append(capRows, row)
		}
		data["Capabilities"] = capRows
	}

	// One reader for pending contact requests, shared with the messaging
	// surface. Two copies of this block is two places for the shape to drift.
	if reqRows, err := h.pendingContactRequests(ctx, pialID); err != nil {
		log.Printf("[numbers] contact requests for pial %s: %v", pialID, err)
	} else {
		data["Requests"] = reqRows
	}

	return data
}

// ── Transport ────────────────────────────────────────────────────────────────

// callElohim sends an internal-authenticated request to elohim-veni and decodes
// the response. A non-2xx is an error, never a quietly empty result.
func (h *Handler) callElohim(ctx context.Context, method, path string, body interface{}, out interface{}) error {
	if h.cfg.ElohimVeniURL == "" {
		return errPolicyAuthorityDown
	}
	var reader *bytes.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.cfg.ElohimVeniURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Key", h.cfg.InternalAPIKey)
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", errPolicyAuthorityDown, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The authority states a machine-readable reason for every refusal it
		// takes deliberately. Throwing that away and reporting an English
		// sentence built from a status line is how "you are holding five
		// Numbers, retire one" became "could not allocate a Number right now"
		// — a true statement that helps nobody. The body is read here so a
		// caller can branch on what was actually said.
		var stated struct {
			Error string `json:"error"`
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, elohimRefusalMax))
		_ = json.Unmarshal(body, &stated)
		return &elohimRefusal{Status: resp.StatusCode, Code: stated.Error, Path: path}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// numberKeyBundle — POST /contact/keys
// The last link in the chain: a Number is exchanged for the target's messaging
// key bundle, and ONLY when that Number's policy allows contact. A Number alone
// never yields key material; the policy decision is what does.
func (h *Handler) numberKeyBundle(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	number := strings.TrimSpace(r.FormValue("number"))
	if number == "" {
		http.Error(w, "number required", http.StatusBadRequest)
		return
	}

	outcome, err := h.evaluateContact(r.Context(), user, contactQuery{
		Number:     number,
		Capability: strings.TrimSpace(r.FormValue("capability")),
	})
	if err != nil {
		holdFloor(started, numberResolveFloor)
		log.Printf("[numbers] key bundle decision for pial %s: %v", user.PIALID, err)
		http.Error(w, "contact policy authority unavailable", http.StatusServiceUnavailable)
		return
	}
	h.notifyContactRequest(user, outcome)
	if outcome.Decision != "allow" || outcome.TargetPIAL == "" {
		// Identical for unknown, retired, malformed and refused.
		holdFloor(started, numberResolveFloor)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unavailable"})
		return
	}

	bundle, err := h.contactBundleFor(r.Context(), outcome.TargetPIAL)
	if err != nil {
		holdFloor(started, numberResolveFloor)
		log.Printf("[numbers] key bundle for pial %s: %v", outcome.TargetPIAL, err)
		http.Error(w, "no key material for that identity", http.StatusConflict)
		return
	}
	holdFloor(started, numberResolveFloor)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(bundle)
}
