package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/gnosis"
	"github.com/f33d3r/feed-engine/internal/model"
)

// ── /api/v1 — F33D3R Numbers for the native clients ──────────────────────────
//
// A Number is a contact address somebody can be reached at without knowing
// their handle: twelve digits, eleven of payload and a Luhn check digit,
// grouped for reading aloud. It is minted, it is revocable, it carries its own
// policy, and it is how a stranger reaches somebody's priority inbox.
//
// This file is the JSON twin of the Facet surface in numbers.go and of the
// Number lane in messages_number.go. It decides NOTHING. Allocation and
// rotation belong to Manhattan, policy and the contact decision belong to
// elohim-veni, and both are reached through the one entry point this brain has
// for each — h.callElohim and h.evaluateContact. What this file owns is the
// shape of the answer.
//
// TWO PROPERTIES GOVERN EVERYTHING BELOW, and they are the reason the surface
// looks lopsided:
//
//  1. NO REVERSE LOOKUP. There is no route here that takes a Number and says
//     anything about who holds it. The owner's listing is addressed by the
//     caller's own PIAL and answers only about the caller. The one act that
//     touches somebody else's Number is message_number, and what comes back
//     from it is a conversation or the word "not_delivered".
//
//  2. ONE REFUSAL. Every reason a Number does not get through — no such
//     Number, a retired one, a spent budget, an expired lease, a policy that
//     refuses, a revoked capability, an authority that did not answer — is one
//     answer, in one shape, after one duration. A JSON lane that graded those
//     refusals would rebuild, in a form far easier to script against, exactly
//     the enumeration oracle the HTML gate is built to deny.
//
// The error envelope is apiError, not the plain text the shared event lane
// uses, because nothing in this file has a web caller: the browser reaches
// these acts through /identity/numbers/* and /identity/contact/*, which render
// Facets. Every reader here is JSON, so every refusal here is too — except
// message_number's, which is a 200 carrying a state, because a refusal that
// answered with a different status from an allow's would be told apart by the
// status.

// ── The owner's own surface ──────────────────────────────────────────────────

// NumberDTO is one Number as its owner sees it.
//
// `id` is the canonical Number — 0412-8837-2919 — and `number` is the same
// Number grouped the way it is read aloud. They are deliberately the same
// value in two dresses: a Number IS its own identifier, there is no second
// handle for one, and inventing an opaque id would mean a lookup from that id
// back to a Number on every retire and every policy change.
//
// The lease is NOT here. How long a Number lasts and how many people it may
// still admit are owner-private in a stronger sense than the rest of this row:
// what is left of a budget is a fact about how many other people hold the same
// Number. The clients do not draw it yet, and a field that is not drawn is a
// field that cannot leak.
type NumberDTO struct {
	ID        string `json:"id"`
	Number    string `json:"number"`
	Policy    string `json:"policy"`
	CreatedAt string `json:"created_at"`
	Revoked   bool   `json:"revoked"`
}

// NumbersDTO is the whole Numbers screen: the caller's own Numbers, the policy
// that governs their @handle, and the vocabulary both are drawn from.
//
// `policies` is served rather than assumed so the app renders the picker from
// the server. A client that hardcoded the six words would keep offering one
// the authority had stopped accepting, and the person would be told their
// change failed with nothing they could do about it.
type NumbersDTO struct {
	Numbers       []NumberDTO `json:"numbers"`
	ContactPolicy string      `json:"contact_policy"`
	Policies      []string    `json:"policies"`
}

// ContactRequestDTO is one person asking to reach the caller.
//
// A requester with no account on this brain stays in the list with an empty
// handle rather than being dropped: somebody is asking either way, and a row
// the recipient cannot see is a decision they cannot make.
//
// There is no note of WHICH Number they used, and no policy. That is the
// recipient's own business to know and not the requester's to be handed, and
// naming it here would tell whoever ends up seeing this screen which door was
// tried.
type ContactRequestDTO struct {
	ID        string  `json:"id"`
	Handle    string  `json:"handle"`
	Display   string  `json:"display"`
	Avatar    *string `json:"avatar,omitempty"`
	Note      string  `json:"note"`
	CreatedAt string  `json:"created_at"`
}

// ContactRequestsDTO is the pending knocks waiting on the caller.
type ContactRequestsDTO struct {
	Requests []ContactRequestDTO `json:"requests"`
}

// apiTimestamp renders a stamp the authority wrote in the one form every
// client on this surface parses: RFC 3339, UTC, whole seconds.
//
// elohim-veni writes these with the sub-second precision Postgres kept, and a
// date parser is entitled to be stricter about fractions than the parser for
// the rest of this surface is about anything else. Neither a Number's mint time
// nor a knock's arrival needs a fraction of a second, so the fraction is
// dropped here rather than becoming a decode failure that costs somebody a
// whole screen over a row that is perfectly good.
//
// A stamp this cannot read is passed through untouched and said out loud.
// Inventing a time for a row would be worse than handing over the one the
// authority actually wrote.
func apiTimestamp(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	log.Printf("[api/v1] a timestamp from the contact authority is not RFC 3339: %q", raw)
	return raw
}

// numberDTO projects one row of the owner's own listing.
func numberDTO(n numberEntry) NumberDTO {
	return NumberDTO{
		ID:        n.Number,
		Number:    numberDisplay(n.Number),
		Policy:    n.Policy,
		CreatedAt: apiTimestamp(n.CreatedAt),
		// One word for every way a Number has stopped working. The authority
		// answers "active" or it does not.
		Revoked: n.Status != "active",
	}
}

// apiV1Numbers — GET /api/v1/numbers: the caller's own Numbers and the contact
// settings that govern them.
//
// Addressed by the caller's own PIAL and by nothing else. There is no
// ?number= on this route and there must never be one.
func (h *Handler) apiV1Numbers(w http.ResponseWriter, r *http.Request, u *model.User) {
	if u.PIALID == "" {
		apiIdentityUnavailable(w)
		return
	}
	entries, err := h.listNumbers(r.Context(), u.PIALID)
	if err != nil {
		log.Printf("[api/v1] numbers for pial %s: %v", u.PIALID, err)
		apiError(w, http.StatusServiceUnavailable, "numbers_unavailable",
			"F33D3R Numbers are unavailable right now. Try again in a moment.")
		return
	}
	// The contact policy is not decoration on this screen — it is the setting
	// the screen exists to show, and `contact_policy` is a key the client
	// reads. So a listing that succeeded while the policy could not be read is
	// answered as unavailable rather than as an identity with no policy: an
	// empty string here would be drawn as a choice nobody made.
	policy, err := h.contactPolicyFor(r.Context(), u.PIALID)
	if err != nil {
		log.Printf("[api/v1] contact policy for pial %s: %v", u.PIALID, err)
		apiError(w, http.StatusServiceUnavailable, "numbers_unavailable",
			"F33D3R Numbers are unavailable right now. Try again in a moment.")
		return
	}
	out := NumbersDTO{
		Numbers:       make([]NumberDTO, 0, len(entries)),
		ContactPolicy: policy.HandlePolicy,
		Policies:      policy.Policies,
	}
	if len(out.Policies) == 0 {
		out.Policies = contactPolicies()
	}
	for _, n := range entries {
		out.Numbers = append(out.Numbers, numberDTO(n))
	}
	apiJSON(w, http.StatusOK, out)
}

// apiV1ContactRequests — GET /api/v1/contact/requests: the people asking to
// reach the caller, waiting on their answer.
func (h *Handler) apiV1ContactRequests(w http.ResponseWriter, r *http.Request, u *model.User) {
	if u.PIALID == "" {
		apiIdentityUnavailable(w)
		return
	}
	// The one reader, shared with both Facet surfaces. It is what names the
	// requesters this brain knows.
	rows, err := h.pendingContactRequests(r.Context(), u.PIALID)
	if err != nil {
		log.Printf("[api/v1] contact requests for pial %s: %v", u.PIALID, err)
		apiError(w, http.StatusServiceUnavailable, "requests_unavailable",
			"Contact requests are unavailable right now. Try again in a moment.")
		return
	}
	out := ContactRequestsDTO{Requests: make([]ContactRequestDTO, 0, len(rows))}
	for _, row := range rows {
		out.Requests = append(out.Requests, ContactRequestDTO{
			ID:        row.ID,
			Handle:    row.Handle,
			Display:   row.Name,
			Avatar:    strPtr(row.Avatar),
			Note:      row.Note,
			CreatedAt: apiTimestamp(row.When),
		})
	}
	apiJSON(w, http.StatusOK, out)
}

// apiIdentityUnavailable is the one answer for a caller whose PIAL is not yet
// in place. userFromRequest bootstraps one for any account that lacks it, so
// this is a transient state and is answered as one.
func apiIdentityUnavailable(w http.ResponseWriter) {
	apiError(w, http.StatusServiceUnavailable, "identity_unavailable",
		"Your identity is still being set up. Try again in a moment.")
}

// ── The event lane ───────────────────────────────────────────────────────────

// apiV1NumberEvent is the Numbers vocabulary on POST /events:
//
//	number_mint                        allocate a Number
//	number_revoke   number_id          retire one
//	number_policy   number_id, policy  change who a Number admits
//	contact_policy  policy             change who may reach this identity by handle
//	contact_decide  request_id, decision   answer a knock
//	message_number  number, body       write to whoever holds a Number
//
// Returns false for an event type it does not own, so the caller carries on.
func (h *Handler) apiV1NumberEvent(w http.ResponseWriter, r *http.Request, eventType string, raw map[string]json.RawMessage) bool {
	switch eventType {
	case "number_mint", "number_revoke", "number_policy",
		"contact_policy", "contact_decide", "message_number":
	default:
		return false
	}
	if h.db == nil {
		apiError(w, http.StatusServiceUnavailable, "db_unavailable",
			"F33D3R is having trouble right now. Try again in a moment.")
		return true
	}
	user := h.userFromRequest(w, r)
	if user == nil || user.ID == "" {
		apiError(w, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
		return true
	}
	// Every act here is addressed to or from an identity on the naming plane,
	// so every one of them needs the caller's PIAL. None of them ever put it
	// on the wire.
	if user.PIALID == "" {
		apiIdentityUnavailable(w)
		return true
	}

	switch eventType {
	case "number_mint":
		h.apiNumberMintEvent(w, r, user, raw)
	case "number_revoke":
		h.apiNumberRevokeEvent(w, r, user, raw)
	case "number_policy":
		h.apiNumberPolicyEvent(w, r, user, raw)
	case "contact_policy":
		h.apiContactPolicyEvent(w, r, user, raw)
	case "contact_decide":
		h.apiContactDecideEvent(w, r, user, raw)
	case "message_number":
		h.apiMessageNumberEvent(w, r, user, raw)
	}
	return true
}

// ── Minting ──────────────────────────────────────────────────────────────────

// apiNumberMintEvent allocates a Number and answers with it.
//
// NOTHING IS CAPPED HERE, and that is deliberate rather than an omission. The
// caps are Manhattan's, because the address space is Manhattan's: five live
// Numbers per identity at once, and a rolling twelve-month minting ladder that
// paces — never refuses — past the first ten. A second copy of either rule in
// this brain would be a second answer waiting to disagree with the first, and
// it would disagree silently. So this handler makes the same call the Facet
// path makes and reports the same three stated refusals, in the same words.
//
// Naming those refusals leaks nothing: they are facts about the caller's own
// identity, and the caller can act on every one of them. An owner told "could
// not allocate a Number right now" retries for ever; an owner told they are
// holding five retires one and carries on.
func (h *Handler) apiNumberMintEvent(w http.ResponseWriter, r *http.Request, user *model.User, raw map[string]json.RawMessage) {
	// The authority stores this Number's policy against the identity, so the
	// identity has to exist there first — the same assertion the Facet path
	// makes, for the same reason.
	if err := h.assertPIAL(r.Context(), user.PIALID); err != nil {
		log.Printf("[api/v1] asserting pial %s before a mint: %v", user.PIALID, err)
		apiError(w, http.StatusServiceUnavailable, "mint_unavailable", mintRefusalNotice(""))
		return
	}
	policy := strings.TrimSpace(eventField(r, raw, "policy"))
	if policy == "" {
		// What a Number is for: reachable by whoever was told the Number, and
		// by nobody else. It is elohim-veni's own default for a Number.
		policy = "number_only"
	}
	if !isContactPolicy(policy) {
		apiError(w, http.StatusBadRequest, "unknown_policy", "That is not a contact policy.")
		return
	}
	// No label and no lease cross this lane, so the authority applies its own
	// default budget and no expiry — exactly what the mint form produces when
	// the owner chooses neither.
	body := map[string]interface{}{"pial_id": user.PIALID, "policy": policy}
	var out struct {
		Number string `json:"number"`
	}
	if err := h.callElohim(r.Context(), http.MethodPost, "/v1/numbers/mint", body, &out); err != nil {
		log.Printf("[api/v1] minting for pial %s: %v", user.PIALID, err)
		code := statedRefusal(err)
		apiError(w, mintRefusalStatus(code), mintRefusalCode(code), mintRefusalNotice(code))
		return
	}
	if out.Number == "" {
		log.Printf("[api/v1] mint for pial %s returned no Number", user.PIALID)
		apiError(w, http.StatusServiceUnavailable, "mint_unavailable", mintRefusalNotice(""))
		return
	}
	apiJSON(w, http.StatusCreated, h.mintedNumberDTO(r.Context(), user.PIALID, out.Number, policy))
}

// mintRefusalStatus and mintRefusalCode carry the authority's stated reason
// onto this lane without translating it. They sit beside mintRefusalNotice,
// which turns the same code into the sentence a person reads, so one refusal
// is described in one place in three registers and cannot drift between them.
func mintRefusalStatus(code string) int {
	switch code {
	case "number_cap_reached", "number_not_reclaimable":
		return http.StatusConflict
	case "number_mint_rate":
		return http.StatusTooManyRequests
	default:
		return http.StatusServiceUnavailable
	}
}

func mintRefusalCode(code string) string {
	switch code {
	case "number_cap_reached", "number_mint_rate", "number_not_reclaimable":
		return code
	default:
		// A refusal that stated no reason stays vague here too. Inventing a
		// cause for it would be worse than admitting there is none.
		return "mint_unavailable"
	}
}

// mintedNumberDTO answers with the Number that was just allocated, as the
// authority itself describes it: the listing is re-read so `created_at` is the
// stamp the authority wrote and not this brain's guess at one.
//
// If that read fails the mint still happened, and a mint reported as a failure
// would leave a live Number nobody knows they hold — the `number` namespace
// never reissues, so it would be burned for good. So the answer is assembled
// from what is certainly true: the Number, the policy just asked for, and the
// fact that it is live as of now.
func (h *Handler) mintedNumberDTO(ctx context.Context, pialID, number, policy string) NumberDTO {
	entries, err := h.listNumbers(ctx, pialID)
	if err != nil {
		log.Printf("[api/v1] reading back a minted Number for pial %s: %v", pialID, err)
	}
	for _, n := range entries {
		if strings.EqualFold(n.Number, number) {
			return numberDTO(n)
		}
	}
	return NumberDTO{
		ID:        number,
		Number:    numberDisplay(number),
		Policy:    policy,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Revoked:   false,
	}
}

// ── Retiring and repolicing ──────────────────────────────────────────────────

// apiNumberRevokeEvent retires one of the caller's own Numbers. The identity,
// its keys, its devices, its sessions and every open conversation are
// untouched; only future contact through this Number stops.
//
// EVERY WAY THIS CAN FAIL IS ONE ANSWER. A Number that is not a Number, one
// nobody holds, one that belongs to somebody else, an authority that did not
// answer: the same code, the same status, the same sentence. The authority
// itself distinguishes them — 404 for a Number it cannot resolve, 403 for one
// owned by another identity — and passing that distinction on would turn the
// retire endpoint into the ownership oracle the rest of this surface refuses
// to be. The Facet path collapses them into one notice for the same reason.
func (h *Handler) apiNumberRevokeEvent(w http.ResponseWriter, r *http.Request, user *model.User, raw map[string]json.RawMessage) {
	number, ok := canonicalNumber(eventField(r, raw, "number_id", "number"))
	if !ok {
		h.answerNumberUnchanged(w, "retire")
		return
	}
	body := map[string]interface{}{"pial_id": user.PIALID, "number": number}
	if err := h.callElohim(r.Context(), http.MethodPost, "/v1/numbers/revoke", body, nil); err != nil {
		log.Printf("[api/v1] retiring a Number for pial %s: %v", user.PIALID, err)
		h.answerNumberUnchanged(w, "retire")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// apiNumberPolicyEvent changes who one Number admits. A conference-badge
// Number and a business-card Number are different promises.
//
// The Number's LABEL is read and written back unchanged. The authority stores
// policy and label as one row and replaces both, and this lane carries no
// label — so without reading it first, changing who may reach you through a
// Number would silently erase the note that says which Number it is. Reading
// it also means a Number the caller does not hold never reaches the authority,
// and that costs nothing in disclosure because the answer is the same either
// way.
func (h *Handler) apiNumberPolicyEvent(w http.ResponseWriter, r *http.Request, user *model.User, raw map[string]json.RawMessage) {
	policy := strings.TrimSpace(eventField(r, raw, "policy"))
	if !isContactPolicy(policy) {
		apiError(w, http.StatusBadRequest, "unknown_policy", "That is not a contact policy.")
		return
	}
	number, ok := canonicalNumber(eventField(r, raw, "number_id", "number"))
	if !ok {
		h.answerNumberUnchanged(w, "policy")
		return
	}
	entries, err := h.listNumbers(r.Context(), user.PIALID)
	if err != nil {
		log.Printf("[api/v1] reading Numbers before a policy change for pial %s: %v", user.PIALID, err)
		h.answerNumberUnchanged(w, "policy")
		return
	}
	label := ""
	held := false
	for _, n := range entries {
		if strings.EqualFold(n.Number, number) {
			label, held = n.Label, true
			break
		}
	}
	if !held {
		h.answerNumberUnchanged(w, "policy")
		return
	}
	// The lease is not mentioned, so the authority leaves the expiry and the
	// budget exactly as they are. Changing who may reach you through a Number
	// must not silently discard when it stops working.
	body := map[string]interface{}{
		"pial_id": user.PIALID,
		"number":  number,
		"policy":  policy,
		"label":   label,
	}
	if err := h.callElohim(r.Context(), http.MethodPost, "/v1/numbers/policy", body, nil); err != nil {
		log.Printf("[api/v1] policy change for pial %s: %v", user.PIALID, err)
		h.answerNumberUnchanged(w, "policy")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// answerNumberUnchanged is the one refusal a retire or a policy change gets.
// The act names which act did not happen — the caller chose it, so it
// discloses nothing about anybody — and nothing else.
func (h *Handler) answerNumberUnchanged(w http.ResponseWriter, act string) {
	message := "Could not retire that Number right now."
	if act == "policy" {
		message = "Could not change that Number's policy right now."
	}
	apiError(w, http.StatusConflict, "number_unchanged", message)
}

// apiContactPolicyEvent sets who may reach this identity by @handle — the
// identity-wide default, the one a stranger who knows the handle meets.
//
// It deliberately does NOT touch the policy a freshly minted Number inherits.
// The two are separate settings on the authority, this lane carries one word,
// and quietly writing it to both would change a setting the caller never
// named.
func (h *Handler) apiContactPolicyEvent(w http.ResponseWriter, r *http.Request, user *model.User, raw map[string]json.RawMessage) {
	policy := strings.TrimSpace(eventField(r, raw, "policy", "handle_policy"))
	if !isContactPolicy(policy) {
		apiError(w, http.StatusBadRequest, "unknown_policy", "That is not a contact policy.")
		return
	}
	if err := h.assertPIAL(r.Context(), user.PIALID); err != nil {
		log.Printf("[api/v1] asserting pial %s before a contact policy change: %v", user.PIALID, err)
		apiError(w, http.StatusServiceUnavailable, "policy_unchanged",
			"Could not save your contact policy right now.")
		return
	}
	body := map[string]interface{}{"pial_id": user.PIALID, "handle_policy": policy}
	if err := h.callElohim(r.Context(), http.MethodPost, "/v1/contact/policy", body, nil); err != nil {
		log.Printf("[api/v1] contact policy for pial %s: %v", user.PIALID, err)
		apiError(w, http.StatusServiceUnavailable, "policy_unchanged",
			"Could not save your contact policy right now.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// apiContactDecideEvent answers one knock.
//
// Accepting keeps the promise the gate made to the person who knocked: it
// opens the conversation and delivers the sentence that has been held all
// along, through deliverHeldOnAccept — the same function the Facet path calls,
// so there is one place a held message is released. Declining tells them
// nothing at all, which is the whole reason a knock is safe to answer.
func (h *Handler) apiContactDecideEvent(w http.ResponseWriter, r *http.Request, user *model.User, raw map[string]json.RawMessage) {
	id := eventField(r, raw, "request_id")
	if id == "" {
		apiError(w, http.StatusBadRequest, "request_required", "Which request?")
		return
	}
	var accept bool
	switch strings.ToLower(strings.TrimSpace(eventField(r, raw, "decision"))) {
	case "accept":
		accept = true
	case "decline":
		accept = false
	default:
		apiError(w, http.StatusBadRequest, "unknown_decision",
			`A request is answered with "accept" or "decline".`)
		return
	}
	body := map[string]interface{}{
		"pial_id":    user.PIALID,
		"request_id": id,
		"accept":     accept,
	}
	var out struct {
		RequesterPIAL string `json:"requester_pial"`
	}
	if err := h.callElohim(r.Context(), http.MethodPost, "/v1/contact/requests/decide", body, &out); err != nil {
		log.Printf("[api/v1] deciding request %s: %v", id, err)
		apiError(w, http.StatusConflict, "request_undecided",
			"Could not answer that request right now.")
		return
	}
	if accept && out.RequesterPIAL != "" {
		if requester := dbpkg.AccountForPIAL(h.db, out.RequesterPIAL); requester != "" {
			h.deliverHeldOnAccept(requester, user)
			h.notifyUser(requester, "contact_accepted", user.ID, user.ID, "user")
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Writing to a Number ──────────────────────────────────────────────────────

// apiMessageNumberEvent is the one entry point a F33D3R Number takes into the
// messaging surface, and it has the two shapes the web's own entry point has,
// told apart the same way — by whether the caller is holding an undelivered
// message. It is handleNumberContact's dispatch, in JSON:
//
//	pending_id present  the contact gate inside a thread. Somebody wrote to a
//	                    person, it did not arrive, and they have since been
//	                    given that person's Number. THE MESSAGE THEY ALREADY
//	                    WROTE is what gets delivered — nothing is retyped, so
//	                    nothing is duplicated and nothing is left held.
//	no pending_id       a Number and nothing else, with a message typed against
//	                    it. Somebody was handed a Number and does not know
//	                    whose it is.
//
// The two are not variants of one act, which is why they are two functions.
// The first is addressed to a PERSON and the Number is a key offered for that
// door; the second is addressed to whoever the Number opens. Only the first can
// enforce the rule that makes it safe — a working Number belonging to SOMEBODY
// ELSE must not deliver a message written to this person, and must not say that
// is what happened — and routing the thread case through the second would lose
// that rule entirely.
func (h *Handler) apiMessageNumberEvent(w http.ResponseWriter, r *http.Request, user *model.User, raw map[string]json.RawMessage) {
	if pendingID := eventField(r, raw, "pending_id", "p"); pendingID != "" {
		h.releaseHeldByNumber(w, r, user, pendingID, raw)
		return
	}
	h.startByNumber(w, r, user, raw)
}

// startByNumber writes to whoever holds a Number: the first thing said to
// somebody there is no conversation with, reached by the twelve digits they
// gave out rather than by a handle.
//
// It is message_start with a Number where the handle goes, and it is
// deliberately the same act in the same order, because the order IS the
// security property:
//
//  1. everything settled from the CALLER's own request is settled first, before
//     one fact about anybody else is read;
//  2. the contact-initiation budget is charged BEFORE the Number is looked up,
//     so probing a Number nobody holds costs exactly what reaching a real one
//     does — a budget that only charged for real Numbers would itself be the
//     thing that says which Numbers are real;
//  3. every outcome that is not an allow answers in ONE shape after ONE
//     duration, through the same answerStartRefusal and the same startStateFor
//     mapping message_start uses, so what actually happened is readable neither
//     from the answer nor from the clock.
//
// The three states are message_start's three states and are not extended here.
// Six causes reach StartNotDelivered by six routes and leave as one word.
func (h *Handler) startByNumber(w http.ResponseWriter, r *http.Request, user *model.User, raw map[string]json.RawMessage) {
	started := time.Now()

	// ── Settled from the caller's own request ────────────────────────────────
	//
	// These answer plainly and fast, and they are allowed to, because every one
	// of them is decided before anything about another person has been read.
	// An empty message and a Number that is not a Number disclose nothing: the
	// length, the alphabet and the check digit are a pure function of what the
	// caller typed, and anyone can compute them offline without this service.
	body := strings.TrimSpace(eventField(r, raw, "body"))
	if body == "" {
		apiError(w, http.StatusBadRequest, "body_required", "Write something to send.")
		return
	}
	if len(body) > maxMessageBytes {
		apiError(w, http.StatusRequestEntityTooLarge, "body_too_long", "That message is too long.")
		return
	}
	number, ok := h.readNumberField(w, r, raw)
	if !ok {
		return
	}
	// The person the caller believes they are writing to, when they had one on
	// screen. It is used for exactly two things, and both are checks rather
	// than lookups: refusing to let somebody message themselves, and refusing
	// to deliver this message to a DIFFERENT person when the Number turns out
	// to be somebody else's.
	//
	// That second check is what handleGateNumber enforces with the held
	// message's target, and it is safe for the same reason: a mismatch is the
	// same answer as an unknown Number, so asking it tells the caller nothing
	// they could not already have learned by simply sending — an allow
	// discloses the identity either way, and every refusal is the one refusal.
	toHandle := strings.TrimPrefix(strings.TrimSpace(eventField(r, raw, "handle", "to")), "@")
	if toHandle != "" && strings.EqualFold(toHandle, user.Handle) {
		apiError(w, http.StatusBadRequest, "self_message", "You cannot message yourself.")
		return
	}

	refuse := func(state, pendingID string) { answerStartRefusal(w, started, state, pendingID) }

	// Past this point the act is a contact INITIATION: it asks the contact
	// authority about a stranger, it can push a notification at them, and it
	// can hold a message for them. It has its own far stricter budget, and it
	// is charged here — before the Number has been looked at by anybody — so
	// that an attempt against a Number nobody holds costs exactly what an
	// attempt against a real one does.
	if !h.chargeContactInitiation(w, r) {
		return
	}

	// The decision is the authority's, taken through the one entry point this
	// brain has for it. The note is what the recipient sees before they agree;
	// the message itself is held and is not part of the knock.
	outcome, err := h.evaluateContact(r.Context(), user, contactQuery{
		Number: number,
		Note:   contactNote(body),
	})
	if err != nil {
		log.Printf("[gnosis] contact decision by Number for pial %s: %v", user.PIALID, err)
		if errors.Is(err, errPolicyAuthorityDown) {
			answerAuthorityUnavailable(w, started)
			return
		}
		refuse(StartNotDelivered, "")
		return
	}
	// The one notification pipeline, reached the one way: if a request was
	// opened, the person it landed on hears about it here and nowhere else.
	h.notifyContactRequest(user, outcome)

	switch startStateFor(outcome.Decision) {
	case StartHeld:
		// A knock names what is held. It has already said somebody was asked,
		// so the id adds nothing to what the answer discloses — and without it
		// the sentence they are waiting on could never be released by Number.
		refuse(h.holdForContactRequest(user, body, outcome))
		return
	case StartNotDelivered:
		refuse(StartNotDelivered, "")
		return
	}

	// An allow, and only now does the identity behind the Number surface at
	// all — as an account to open a conversation with, never as a name sent
	// back on its own.
	account := dbpkg.AccountForPIAL(h.db, outcome.TargetPIAL)
	if outcome.TargetPIAL == "" || account == "" || account == user.ID {
		refuse(StartNotDelivered, "")
		return
	}
	// A working Number belonging to somebody else does not deliver a message
	// written to this person, and does not say that is what happened.
	if toHandle != "" {
		target, terr := dbpkg.GetUserByHandle(h.db, toHandle)
		if terr != nil || target == nil || target.ID != account {
			refuse(StartNotDelivered, "")
			return
		}
	}
	convoID, err := h.ensureDirectConversation(user, account, outcome.TargetPIAL)
	if err != nil {
		log.Printf("[gnosis] opening a conversation by Number for pial %s: %v", user.PIALID, err)
		refuse(StartNotDelivered, "")
		return
	}
	// The message goes in only if the conversation is PLAIN. A sealed thread
	// never takes plaintext: sealing needs the key bundle that permission has
	// only just granted, so a message written before it cannot have been
	// sealed, and writing it as plaintext would put one readable message inside
	// an end-to-end encrypted thread. It stays with the client to seal.
	if convo, cerr := gnosis.GetConversation(h.db, convoID); cerr == nil && convo.Mode == gnosis.ModePlain {
		if _, status, reason := h.sendIntoConversation(user, convoID, body); status != http.StatusOK {
			log.Printf("[gnosis] delivering into %s after a Number allowed contact: %s", convoID, reason)
		}
	}
	h.answerStartOpened(w, user, convoID)
}

// releaseHeldByNumber resolves a Number entered against a message that is
// already waiting, and delivers THAT message if the Number opens the door. It
// is handleGateNumber with a JSON answer.
//
// WHAT THIS MUST NOT BECOME. A Number is guessable in principle, and the only
// thing standing between a guesser and an identity is that a wrong guess tells
// them nothing. So every outcome that is not an allow answers the SAME state
// after the SAME minimum duration:
//
//   - a Number nobody holds
//   - a Number that has been retired
//   - a Number whose lease has expired
//   - a Number whose admission budget is spent
//   - a Number that is somebody else's, not this person's
//   - a policy that refuses this caller
//
// The authority already collapses the first four into one deny. The fifth is
// collapsed here, because a Number that resolves to a different person must not
// confirm that it resolves to anybody.
//
// The held message is named back in every one of those answers. That discloses
// nothing: the caller supplied the id, it is their own message, and it says
// only that it is still theirs and still waiting.
func (h *Handler) releaseHeldByNumber(w http.ResponseWriter, r *http.Request, user *model.User, pendingID string, raw map[string]json.RawMessage) {
	started := time.Now()

	// The caller's own held message, read as the caller's. A row that is not
	// theirs is not found, so there is no way to ask about anybody else's.
	pending, found, err := gnosis.GetPendingForSender(h.db, pendingID, user.ID)
	if err != nil {
		log.Printf("[gnosis] reading a held message for %s: %v", user.ID, err)
		apiError(w, http.StatusInternalServerError, "server_error",
			"F33D3R is having trouble right now. Try again in a moment.")
		return
	}
	if !found {
		// Either it was delivered already or it was never this caller's.
		// Neither is a contact decision, so neither is answered as one — and
		// both are facts about the caller's own state, so both may be plain.
		apiError(w, http.StatusNotFound, "nothing_held", "No message is waiting.")
		return
	}
	number, ok := h.readNumberField(w, r, raw)
	if !ok {
		// Not recorded: what somebody typed is not something the recipient did,
		// so it is not a decision to remember about them.
		return
	}

	// Every non-allow return below passes through here, and each one records
	// what was answered, so re-opening this thread does not knock on the
	// recipient a second time to find out what to say.
	gate := func(decision string) {
		if err := gnosis.SetPendingDecision(h.db, pending.ID, decision); err != nil {
			log.Printf("[gnosis] recording a contact decision for %s: %v", pending.ID, err)
		}
		answerStartRefusal(w, started, startStateFor(decision), pending.ID)
	}

	if !h.chargeContactInitiation(w, r) {
		return
	}
	outcome, err := h.evaluateContact(r.Context(), user, contactQuery{
		Number: number,
		Note:   contactNote(pending.Body),
	})
	if err != nil {
		log.Printf("[gnosis] contact decision by Number for pial %s: %v", user.PIALID, err)
		if errors.Is(err, errPolicyAuthorityDown) {
			if derr := gnosis.SetPendingDecision(h.db, pending.ID, gnosis.PendingUnavailable); derr != nil {
				log.Printf("[gnosis] recording a contact decision for %s: %v", pending.ID, derr)
			}
			answerAuthorityUnavailable(w, started)
			return
		}
		gate(gnosis.PendingUnavailable)
		return
	}
	h.notifyContactRequest(user, outcome)

	if state := contactGateState(outcome.Decision); state != "" {
		// Recorded as one of the three words the recipient's own surfaces
		// know, and never as whatever the authority happened to say: a
		// decision this build does not recognise must not become a stored
		// state nothing can render.
		gate(state)
		return
	}
	account := dbpkg.AccountForPIAL(h.db, outcome.TargetPIAL)
	if outcome.TargetPIAL == "" || account == "" || account != pending.TargetAccount {
		// The fifth cause, collapsed: a Number that works but is not this
		// person's delivers nothing and says nothing about whose it is.
		gate(gnosis.PendingDeny)
		return
	}
	convoID, err := h.ensureDirectConversation(user, account, outcome.TargetPIAL)
	if err != nil {
		log.Printf("[gnosis] opening a conversation by Number for pial %s: %v", user.PIALID, err)
		gate(gnosis.PendingUnavailable)
		return
	}
	// The one function that releases a held message, shared with the web. It
	// leaves the hold alone when the conversation opened SEALED — the client
	// holds what was written and seals it — so the answer says the conversation
	// is open AND that the message is still waiting to be sent into it.
	held := ""
	if !h.deliverPending(user, convoID, pending) {
		held = pending.ID
	}
	h.answerStartOpenedHolding(w, user, convoID, held)
}

// readNumberField reads and canonicalises the Number the caller typed, or
// answers for it and reports false.
//
// A malformed Number is NAMED as the typing mistake it is, rather than folded
// into the silent refusal, and it is answered immediately. That is safe for the
// same reason the web names it: length, alphabet and check digit are a pure
// function of what the caller typed, and anyone can compute them offline
// without this service. What must never be named is why a WELL-FORMED Number
// did not reach anyone, and nothing on either shape names that.
func (h *Handler) readNumberField(w http.ResponseWriter, r *http.Request, raw map[string]json.RawMessage) (string, bool) {
	typed := eventField(r, raw, "number")
	if typed == "" {
		apiError(w, http.StatusBadRequest, "number_required", "Enter a F33D3R Number.")
		return "", false
	}
	number, wellFormed := canonicalNumber(typed)
	if !wellFormed {
		apiError(w, http.StatusBadRequest, "bad_number",
			"That is not a F33D3R Number. A Number is twelve digits, like 0412 8837 2919.")
		return "", false
	}
	return number, true
}

// chargeContactInitiation charges the contact-initiation budget, answering for
// itself when there is nothing left. It is one function so that both shapes
// charge the same bucket at the same point in the act — before anything about
// the Number has been looked up by anybody.
func (h *Handler) chargeContactInitiation(w http.ResponseWriter, r *http.Request) bool {
	if h.rlContactInit.Allow(r) {
		return true
	}
	w.Header().Set("Retry-After", "60")
	apiError(w, http.StatusTooManyRequests, "rate_limited", "Too many contact attempts — slow down.")
	return false
}

// holdForContactRequest keeps the promise "held" makes, and names what it kept.
//
// "Held" says the message is stored and the recipient has been asked, and it is
// delivered if and when they accept. That has to be true, so the message is
// stored here against the identity the authority has just told us the request
// landed on — the same identity it disclosed so the knock could be notified,
// and never anything the caller learns. deliverHeldOnAccept releases it if the
// answer is yes, and the id it returns lets the sender release it themselves
// with a Number.
//
// A hold that could not be written answers as a refusal instead, in the same
// shape after the same duration. A message nobody kept is not held, and saying
// it was would be the one lie this lane could tell that costs somebody a
// sentence they thought was waiting.
func (h *Handler) holdForContactRequest(user *model.User, body string, o contactOutcome) (string, string) {
	if o.RequestOwnerPIAL == "" {
		// A knock the authority did not open, because one from this identity is
		// already waiting on that person. The recipient HAS been asked and the
		// message they will see is the one held from the first attempt, so
		// "held" is the truth. There is no identity to address a new hold to,
		// and asking for one would be a reverse lookup.
		return StartHeld, ""
	}
	owner := dbpkg.AccountForPIAL(h.db, o.RequestOwnerPIAL)
	if owner == "" || owner == user.ID {
		return StartNotDelivered, ""
	}
	pending, err := gnosis.UpsertPending(h.db, user.ID, owner, body)
	if err != nil {
		log.Printf("[gnosis] holding a message behind a Number for %s: %v", user.ID, err)
		return StartNotDelivered, ""
	}
	// What was answered is recorded, so re-opening this does not knock on the
	// recipient a second time to find out what to say.
	if err := gnosis.SetPendingDecision(h.db, pending.ID, gnosis.PendingRequest); err != nil {
		log.Printf("[gnosis] recording a contact decision for %s: %v", pending.ID, err)
	}
	return StartHeld, pending.ID
}
