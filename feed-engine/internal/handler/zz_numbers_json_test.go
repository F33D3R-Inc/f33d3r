package handler

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/f33d3r/feed-engine/internal/config"
	"github.com/f33d3r/feed-engine/internal/gnosis"
)

// The JSON Numbers surface has one property that is not about shape, and it is
// the whole reason the design exists: a Number cannot be used to find out
// whether somebody exists, who they are, or how they have set their door.
//
// Every test below holds some part of that. They are the JSON twins of
// TestEveryRefusalCauseCollapsesToOneState and
// TestNumberGateCannotDistinguishAnUnknownNumberFromARetiredOne, which hold the
// same property on the surface the browser sees — the same set of causes
// reaching a second surface, in a shape that is far easier to script against.

// ── One refusal ──────────────────────────────────────────────────────────────

// Every reason a Number does not get through is one state. This is the same
// table as TestEveryRefusalCauseCollapsesToOneState, read through the mapping
// the JSON lane answers with, because a second mapping is a second vocabulary
// waiting to say more than the first.
func TestEveryNumberRefusalCauseCollapsesToOneState(t *testing.T) {
	causes := map[string]string{
		"a Number nobody holds":            "deny",
		"a retired Number":                 "deny",
		"an expired lease":                 "deny",
		"an exhausted admission budget":    "deny",
		"a closed policy":                  "deny",
		"a revoked contact capability":     "deny",
		"a contact link never presented":   "deny",
		"a check digit that got this far":  "deny",
		"an authority that did not answer": gnosis.PendingUnavailable,
		"a word this brain does not know":  "some_future_decision",
		"an empty answer":                  "",
	}
	for name, decision := range causes {
		if got := startStateFor(decision); got != StartNotDelivered {
			t.Fatalf("%s produced state %q; every refusal must be the one refusal", name, got)
		}
	}
	if got := startStateFor("request"); got != StartHeld {
		t.Fatalf("a knock answered %q; a knock is not a refusal", got)
	}
	if got := startStateFor("allow"); got != StartOpened {
		t.Fatalf("an allow answered %q; an allow opens the conversation", got)
	}
}

// The bytes themselves. An unknown Number and a retired one arrive here as the
// same state and must leave as the same answer, after the same minimum
// duration, with nothing in it that names which it was.
func TestNumberGateJSONCannotDistinguishAnUnknownNumberFromARetiredOne(t *testing.T) {
	answer := func() (string, int, time.Duration) {
		rec := httptest.NewRecorder()
		started := time.Now()
		answerStartRefusal(rec, started, startStateFor("deny"), "")
		return rec.Body.String(), rec.Code, time.Since(started)
	}
	unknown, status, took := answer()
	retired, retiredStatus, retiredTook := answer()
	if unknown != retired {
		t.Fatalf("an unknown Number and a retired one answered differently: %s vs %s", unknown, retired)
	}
	if status != retiredStatus {
		t.Fatalf("the status told them apart: %d vs %d", status, retiredStatus)
	}
	if status != 200 {
		t.Fatalf("a refusal answered %d; a status that differs from an allow's is itself the cause", status)
	}
	for _, d := range []time.Duration{took, retiredTook} {
		if d < numberResolveFloor {
			t.Fatalf("a refusal answered in %v, under the %v floor; the clock said what the body would not", d, numberResolveFloor)
		}
	}
	var got ConversationStartDTO
	if err := json.Unmarshal([]byte(unknown), &got); err != nil {
		t.Fatalf("the refusal is not the one shape: %v", err)
	}
	if got.State != StartNotDelivered || got.Conversation != nil {
		t.Fatalf("a refusal carried more than its state: %+v", got)
	}
	lower := strings.ToLower(unknown)
	for _, leak := range []string{
		"unknown", "revoked", "retired", "expired", "exhausted", "spent",
		"no longer", "does not exist", "never existed", "wrong person",
		"belongs to", "not accepting", "closed", "blocked", "policy",
		"authority", "unavailable", "found", "@",
	} {
		if strings.Contains(lower, leak) {
			t.Fatalf("the refusal leaked why it refused: %q in %s", leak, unknown)
		}
	}
}

// ── The order the act is performed in ────────────────────────────────────────

// bodyOf returns one function's body from a source file with its line comments
// stripped, so a test about the ORDER of the calls cannot be satisfied — or
// broken — by prose that happens to mention them.
func bodyOf(t *testing.T, file, signature string) string {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	src := string(raw)
	start := strings.Index(src, signature)
	if start < 0 {
		t.Fatalf("%s does not declare %s", file, signature)
	}
	rest := src[start+len(signature):]
	if end := strings.Index(rest, "\nfunc "); end >= 0 {
		rest = rest[:end]
	}
	var kept []string
	for _, line := range strings.Split(rest, "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "//") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// withoutClosureDefinitions drops the lines that DECLARE a closure, so a test
// about when an answer is written is not satisfied — or broken — by where the
// function that writes it was defined.
func withoutClosureDefinitions(body string) string {
	var kept []string
	depth, inDef := 0, false
	for _, line := range strings.Split(body, "\n") {
		if !inDef && strings.Contains(line, ":= func(") {
			inDef = true
			depth = 0
		}
		if inDef {
			depth += strings.Count(line, "{") - strings.Count(line, "}")
			if depth <= 0 {
				inDef = false
			}
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// The rate limiter is charged BEFORE the Number is looked up, and this is the
// difference between a budget and an oracle. A probe against a Number nobody
// holds must cost exactly what reaching a real one costs; a limiter charged
// only once a target turned out to exist would make the limiter itself the
// thing that answers "is this Number real?".
//
// The same tightening was made for message_start, and it is asserted the same
// way: the order is the property, and the order is what is checked.
func TestMessageNumberChargesTheContactBudgetBeforeAnyLookup(t *testing.T) {
	// Nothing is ANSWERED about the person addressed until the budget has been
	// charged, so an attempt against somebody who is not there costs what an
	// attempt against somebody who is costs.
	for _, c := range []struct{ file, signature string }{
		{"api_v1_numbers.go", "func (h *Handler) startByNumber("},
		{"api_v1_numbers.go", "func (h *Handler) releaseHeldByNumber("},
		{"api_v1_messages.go", "func (h *Handler) messageStartEvent("},
	} {
		// Declaring the closure that answers is not answering, so the lines
		// that define one are dropped before the order is read.
		body := withoutClosureDefinitions(bodyOf(t, c.file, c.signature))
		charge := strings.Index(body, "chargeContactInitiation")
		if charge < 0 {
			charge = strings.Index(body, "rlContactInit.Allow")
		}
		if charge < 0 {
			t.Fatalf("%s never charges the contact-initiation budget", c.signature)
		}
		for _, answer := range []string{"refuse(Start", "refuse(h.hold", "gate(gnosis.", "answerStartRefusal(w"} {
			if at := strings.Index(body, answer); at >= 0 && at < charge {
				t.Fatalf("%s answers about the person addressed before charging the contact "+
					"budget; a probe that costs nothing because nobody was there is an oracle",
					c.signature)
			}
		}
	}
	// And on both Number shapes the LOOKUP itself is after the charge too.
	// There is no conversation to find first here — a Number names nobody this
	// brain can resolve — so the first thing that touches the Number is the
	// decision, and it is paid for before it is asked.
	for _, sig := range []string{
		"func (h *Handler) startByNumber(",
		"func (h *Handler) releaseHeldByNumber(",
	} {
		body := bodyOf(t, "api_v1_numbers.go", sig)
		charge := strings.Index(body, "chargeContactInitiation")
		for _, lookup := range []string{"evaluateContact", "AccountForPIAL", "ensureDirectConversation", "GetUserByHandle"} {
			if at := strings.Index(body, lookup); at >= 0 && at < charge {
				t.Fatalf("%s reads %s before charging the contact budget", sig, lookup)
			}
		}
	}
}

// Every way out of either Number shape that is not an allow leaves through the
// one function that holds the floor and writes the one shape. A second exit is
// a refusal that can be told apart from the first — by its bytes, by its
// status, or by how long it took.
//
// The single exception is an authority that did not answer AT ALL, which is not
// one of the three states because it is not a fact about anybody: it is the
// same for every caller and every Number at once, and reporting it as "your
// message did not get through" would be untrue.
func TestMessageNumberHasOneRefusalExit(t *testing.T) {
	for _, sig := range []string{
		"func (h *Handler) startByNumber(",
		"func (h *Handler) releaseHeldByNumber(",
	} {
		body := bodyOf(t, "api_v1_numbers.go", sig)
		// From the decision onward, nothing about the caller's own request is
		// left to answer: everything past this point is a fact about somebody
		// else.
		gate := strings.Index(body, "h.evaluateContact(")
		if gate < 0 {
			t.Fatalf("%s never asks the contact authority", sig)
		}
		after := body[gate:]
		if strings.Count(after, "apiError(") != 0 {
			t.Fatalf("%s writes an error after the gate; every outcome past that point must "+
				"go through the shared refusal, the shared allow, or the authority-down answer", sig)
		}
		for _, forbidden := range []string{"http.Error(", "WriteHeader(", "apiJSON("} {
			if strings.Contains(after, forbidden) {
				t.Fatalf("%s writes %s after the gate", sig, forbidden)
			}
		}
		if !strings.Contains(after, "answerAuthorityUnavailable(w, started)") {
			t.Fatalf("%s reports an unreachable authority as a delivery failure", sig)
		}
		if !strings.Contains(after, "answerStartRefusal") && !strings.Contains(after, "gate(") &&
			!strings.Contains(after, "refuse(") {
			t.Fatalf("%s never refuses; the refusal is the whole point of the lane", sig)
		}
		if !strings.Contains(after, "answerStartOpened") {
			t.Fatalf("%s never opens a conversation through the shared answer", sig)
		}
	}
	// And the refusal they use is the one that holds the floor — as does the
	// authority-down answer, so "every exit that is not an allow takes at least
	// the floor" stays one rule with no exceptions.
	for _, sig := range []string{"func answerStartRefusal(", "func answerAuthorityUnavailable("} {
		if !strings.Contains(bodyOf(t, "api_v1_messages.go", sig), "holdFloor(started, numberResolveFloor)") {
			t.Fatalf("%s no longer holds the timing floor", sig)
		}
	}
}

// The two shapes are told apart the way the web tells them apart — by whether
// the caller is holding an undelivered message — and the held shape enforces
// what only it can: a working Number belonging to SOMEBODY ELSE must not
// deliver a message written to this person, and must not say so.
func TestTheHeldShapeRefusesSomebodyElsesNumber(t *testing.T) {
	dispatch := bodyOf(t, "api_v1_numbers.go", "func (h *Handler) apiMessageNumberEvent(")
	if !strings.Contains(dispatch, `eventField(r, raw, "pending_id", "p")`) {
		t.Fatal("message_number does not dispatch on the held message the way the web does")
	}
	if !strings.Contains(dispatch, "h.releaseHeldByNumber(") || !strings.Contains(dispatch, "h.startByNumber(") {
		t.Fatal("message_number does not have both shapes")
	}

	held := bodyOf(t, "api_v1_numbers.go", "func (h *Handler) releaseHeldByNumber(")
	// The held message is read as the CALLER's, so there is no way to ask about
	// anybody else's.
	if !strings.Contains(held, "gnosis.GetPendingForSender(h.db, pendingID, user.ID)") {
		t.Fatal("a held message is not read as the caller's own")
	}
	// The Number must resolve to the person the message was written to.
	if !strings.Contains(held, "account != pending.TargetAccount") {
		t.Fatal("a Number belonging to somebody else would deliver a message written to this person")
	}
	// And that mismatch is the ordinary refusal, not one of its own.
	idx := strings.Index(held, "account != pending.TargetAccount")
	if !strings.Contains(held[idx:idx+220], "gate(gnosis.PendingDeny)") {
		t.Fatal("a Number that is somebody else's is answered differently from one nobody holds")
	}
	// Nothing is retyped, so nothing is duplicated: the held message is what is
	// delivered, through the one function that releases one.
	if !strings.Contains(held, "h.deliverPending(user, convoID, pending)") {
		t.Fatal("the held shape does not deliver the held message")
	}
	if strings.Contains(held, `eventField(r, raw, "body")`) {
		t.Fatal("the held shape reads a typed body; it must deliver the message already written")
	}
}

// pending_id is present only where the answer has already said the person
// exists. Attached to a `not_delivered` on the lanes that address a stranger it
// would be exactly the existence oracle the single state refuses to be: a
// handle nobody holds can have nothing held for it, so an id would say so.
func TestAHeldMessageIsNamedOnlyWhereNamingItDisclosesNothing(t *testing.T) {
	start := bodyOf(t, "api_v1_messages.go", "func (h *Handler) messageStartEvent(")
	if !strings.Contains(start, `answerStartRefusal(w, started, state, "")`) &&
		!strings.Contains(start, `refuse := func(state string) { answerStartRefusal(w, started, state, "") }`) {
		t.Fatal("message_start's refusal closure no longer withholds the held id")
	}
	if !strings.Contains(start, "if state == StartHeld {") {
		t.Fatal("message_start names a held message on an answer other than held")
	}
	begin := bodyOf(t, "api_v1_numbers.go", "func (h *Handler) startByNumber(")
	if strings.Count(begin, `refuse(StartNotDelivered, "")`) < 4 {
		t.Fatalf("a Number refusal names a held message: %d unnamed refusals",
			strings.Count(begin, `refuse(StartNotDelivered, "")`))
	}
	// The ordinary send path is the one place a not_delivered names one, and it
	// is safe there because the caller and the recipient already share a
	// conversation.
	event := bodyOf(t, "api_v1_messages.go", "func (h *Handler) apiV1MessageEvent(")
	if !strings.Contains(event, "h.holdUndelivered(user, convoID,") {
		t.Fatal("a send that did not arrive drops the sentence instead of holding it")
	}
}

// ── No reverse lookup ────────────────────────────────────────────────────────

// The surface is addressed by the caller's own identity and by nothing else.
// A route that took a Number in its path or its query would answer, by
// existing, the one question this whole design refuses: who is this?
func TestNumbersJSONSurfaceOffersNoReverseLookup(t *testing.T) {
	raw, err := os.ReadFile("api_v1.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.Contains(line, "mux.HandleFunc(") || !strings.Contains(line, "/api/v1/") {
			continue
		}
		if strings.Contains(line, "{number") || strings.Contains(line, "numbers/{") {
			t.Fatalf("a route resolves a Number to whoever holds it: %s", strings.TrimSpace(line))
		}
	}
	// Nor does the surface hand back the identity behind one. The DTOs a
	// resolve could reach carry no PIAL and no account id.
	for _, dto := range []interface{}{
		NumberDTO{ID: "0412-8837-2919", Number: "0412 8837 2919", Policy: "number_only"},
		ContactRequestDTO{ID: "r1", Handle: "dana", Display: "Dana"},
		ConversationStartDTO{State: StartNotDelivered},
	} {
		blob, err := json.Marshal(dto)
		if err != nil {
			t.Fatal(err)
		}
		lower := strings.ToLower(string(blob))
		for _, leak := range []string{"pial", "account", "target", "owner", "requester"} {
			if strings.Contains(lower, leak) {
				t.Fatalf("%T puts %q on the wire: %s", dto, leak, blob)
			}
		}
	}
}

// ── The owner's own listing ──────────────────────────────────────────────────

func TestNumberDTOSaysWhatTheAuthoritySaid(t *testing.T) {
	live := numberDTO(numberEntry{
		Number: "0098-7754-8744", Status: "active", Policy: "number_only",
		CreatedAt: "2026-09-05T12:00:00Z", Label: "conference badge",
	})
	if live.ID != "0098-7754-8744" {
		t.Errorf("id: %q — the Number is its own identifier, so a retire needs no second lookup", live.ID)
	}
	if live.Number != "0098 7754 8744" {
		t.Errorf("number: %q — a Number is grouped for reading aloud", live.Number)
	}
	if live.Revoked {
		t.Error("an active Number was reported as retired")
	}
	if live.CreatedAt != "2026-09-05T12:00:00Z" {
		t.Errorf("created_at: %q — it is the authority's stamp, not this brain's guess", live.CreatedAt)
	}
	// The label and the lease are owner-private in a stronger sense than the
	// rest of the row: what is left of a budget is a fact about how many other
	// people hold the same Number.
	blob, _ := json.Marshal(live)
	for _, leak := range []string{"conference badge", "label", "lease", "admission", "expires"} {
		if strings.Contains(strings.ToLower(string(blob)), leak) {
			t.Errorf("the Number DTO carries %q: %s", leak, blob)
		}
	}
	// Every way a Number has stopped working is the one word.
	for _, status := range []string{"revoked", "quarantined", "", "expired"} {
		if !numberDTO(numberEntry{Number: "0098-7754-8744", Status: status}).Revoked {
			t.Errorf("status %q was reported as live", status)
		}
	}
}

func TestNumbersDTOKeysAreTheContract(t *testing.T) {
	keys := func(v interface{}) map[string]bool {
		blob, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(blob, &m); err != nil {
			t.Fatalf("%T is not an object: %v", v, err)
		}
		out := map[string]bool{}
		for k := range m {
			out[k] = true
		}
		return out
	}
	want := func(got map[string]bool, names ...string) {
		t.Helper()
		for _, n := range names {
			if !got[n] {
				t.Errorf("key %q is missing; the app decodes it", n)
			}
			delete(got, n)
		}
		for extra := range got {
			t.Errorf("key %q is emitted and the app does not read it", extra)
		}
	}
	want(keys(NumbersDTO{}), "numbers", "contact_policy", "policies")
	want(keys(NumberDTO{}), "id", "number", "policy", "created_at", "revoked")
	want(keys(ContactRequestsDTO{}), "requests")
	// avatar is omitted rather than sent empty for a requester with no picture.
	want(keys(ContactRequestDTO{}), "id", "handle", "display", "note", "created_at")
	avatar := "https://example.test/a.png"
	want(keys(ContactRequestDTO{Avatar: &avatar}), "id", "handle", "display", "avatar", "note", "created_at")
}

// The picker the app draws comes from the server. This mirror is the fallback
// for when the authority did not state its own list, so it has to BE the
// authority's list — elohim-veni's contact::POLICIES, in order.
func TestContactPolicyVocabularyMatchesTheAuthority(t *testing.T) {
	want := []string{"open", "followers", "mutuals", "number_only", "capability_only", "closed"}
	got := contactPolicies()
	if len(got) != len(want) {
		t.Fatalf("the policy vocabulary is %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("policy %d is %q, want %q", i, got[i], want[i])
		}
		if !isContactPolicy(want[i]) {
			t.Fatalf("%q is offered to the owner and then refused", want[i])
		}
	}
	for _, no := range []string{"", "OPEN", "everyone", "nobody", "number", "public", "closed "} {
		if isContactPolicy(no) {
			t.Fatalf("%q was accepted as a contact policy", no)
		}
	}
}

// ── Minting ──────────────────────────────────────────────────────────────────

// The mint caps are Manhattan's — five live Numbers at once, and a rolling
// twelve-month ladder that paces rather than refuses. This brain copies none of
// them; it carries the authority's stated reason across, in three registers
// that must agree about which refusals a person can act on.
func TestMintRefusalsCarryTheAuthoritysOwnReason(t *testing.T) {
	actionable := map[string]int{
		"number_cap_reached":     409,
		"number_mint_rate":       429,
		"number_not_reclaimable": 409,
	}
	for code, status := range actionable {
		if got := mintRefusalStatus(code); got != status {
			t.Errorf("%s answered %d, want %d", code, got, status)
		}
		if got := mintRefusalCode(code); got != code {
			t.Errorf("%s was renamed to %q on the way out", code, got)
		}
		notice := mintRefusalNotice(code)
		if notice == "" || notice == mintRefusalNotice("") {
			t.Errorf("%s reads as the vague refusal (%q); an owner who cannot see the "+
				"cause retries for ever", code, notice)
		}
	}
	// A refusal that stated no reason stays vague. Inventing a cause for it
	// would be worse than admitting there is none.
	for _, unknown := range []string{"", "db_error", "some_future_code"} {
		if got := mintRefusalCode(unknown); got != "mint_unavailable" {
			t.Errorf("an unexplained refusal was reported as %q", got)
		}
		if got := mintRefusalStatus(unknown); got != 503 {
			t.Errorf("an unexplained refusal answered %d, want 503", got)
		}
	}
	// The cap is named as something the owner can clear, because it is.
	if !strings.Contains(strings.ToLower(mintRefusalNotice("number_cap_reached")), "retire") {
		t.Error("the cap notice does not say which action clears it")
	}
}

// A Number the mint allocated is answered for even when the read-back fails.
// The `number` namespace never reissues, so a mint reported as a failure leaves
// a live Number nobody knows they hold — burned for good.
func TestAMintedNumberIsAnsweredForEvenWhenTheListingFails(t *testing.T) {
	// No authority configured, so the read-back cannot answer.
	h := &Handler{cfg: &config.Config{}}
	got := h.mintedNumberDTO(context.Background(), "pial", "0098-7754-8744", "mutuals")
	if got.ID != "0098-7754-8744" || got.Number != "0098 7754 8744" {
		t.Fatalf("the minted Number was lost: %+v", got)
	}
	if got.Policy != "mutuals" || got.Revoked {
		t.Fatalf("the Number was answered for wrongly: %+v", got)
	}
	if _, err := time.Parse(time.RFC3339Nano, got.CreatedAt); err != nil {
		t.Fatalf("created_at is not a time: %q", got.CreatedAt)
	}
}

// ── Retiring and repolicing ──────────────────────────────────────────────────

// A retire either happened or it did not, and there is ONE way to say that it
// did not. The authority tells this brain 404 for a Number it cannot resolve
// and 403 for one held by somebody else; passing that difference on would make
// the retire route answer "does this Number exist, and is it mine?" for any
// twelve digits somebody cared to type.
func TestRetireAndRepoliceRefuseWithOneAnswer(t *testing.T) {
	body := func(act string) (string, int) {
		rec := httptest.NewRecorder()
		(&Handler{}).answerNumberUnchanged(rec, act)
		return rec.Body.String(), rec.Code
	}
	retire, status := body("retire")
	policy, policyStatus := body("policy")
	if status != policyStatus || status != 409 {
		t.Fatalf("statuses: retire %d, policy %d", status, policyStatus)
	}
	var got map[string]apiErrorBody
	if err := json.Unmarshal([]byte(retire), &got); err != nil {
		t.Fatalf("the refusal is not the error envelope: %v", err)
	}
	if got["error"].Code != "number_unchanged" {
		t.Fatalf("code: %q", got["error"].Code)
	}
	var pol map[string]apiErrorBody
	json.Unmarshal([]byte(policy), &pol)
	if pol["error"].Code != got["error"].Code {
		t.Fatalf("two acts refused with two codes: %q and %q", got["error"].Code, pol["error"].Code)
	}
	for _, out := range []string{retire, policy} {
		lower := strings.ToLower(out)
		for _, leak := range []string{
			"not found", "does not exist", "unknown", "forbidden", "not yours",
			"belongs to", "somebody else", "already", "malformed", "invalid",
		} {
			if strings.Contains(lower, leak) {
				t.Fatalf("the refusal named its cause: %q in %s", leak, out)
			}
		}
	}
	// The two sentences differ only in which act the CALLER asked for, which
	// they already know.
	if retire == policy {
		t.Fatal("a retire and a policy change report the same act")
	}
}

// A policy change reads the Number's label and writes it back. The authority
// stores policy and label as one row and replaces both, and this lane carries
// no label — so a change that did not read it first would silently erase the
// note that says which Number is which.
func TestAPolicyChangeKeepsTheLabelItDoesNotCarry(t *testing.T) {
	body := bodyOf(t, "api_v1_numbers.go", "func (h *Handler) apiNumberPolicyEvent(")
	list := strings.Index(body, "h.listNumbers(")
	write := strings.Index(body, `"/v1/numbers/policy"`)
	if list < 0 || write < 0 || list > write {
		t.Fatal("a policy change writes without reading the label back first")
	}
	if !strings.Contains(body, `"label":   label`) {
		t.Fatal("a policy change does not send the label it read; the owner's label would be erased")
	}
}

// ── The ordinary send path ───────────────────────────────────────────────────
//
// A thread can outlive the permission that opened it, and the ordinary send is
// where that is discovered — which is where a client learns to offer the
// keypad. So message_send can answer the same three states, and the collapse
// rule follows it there: one answer, one shape, one floor, no cause named.

func TestAnUndeliveredSendSaysNothingAboutWhy(t *testing.T) {
	if strings.TrimSpace(sendNotDelivered) == "" {
		t.Fatal("a send that did not arrive says nothing at all")
	}
	lower := strings.ToLower(sendNotDelivered)
	for _, leak := range []string{
		"block", "blocked", "policy", "closed", "revoked", "retired", "left",
		"deleted", "unknown", "forbidden", "not allowed", "refus",
	} {
		if strings.Contains(lower, leak) {
			t.Fatalf("the send refusal names its cause: %q in %q", leak, sendNotDelivered)
		}
	}
	// The same string is what the browser is shown and what the JSON lane keys
	// on, so a second sentence for a second cause cannot be added to one
	// surface without the other noticing.
	send := bodyOf(t, "messages.go", "func (h *Handler) sendIntoConversation(")
	if strings.Count(send, "sendNotDelivered") != 1 {
		t.Fatalf("the send path has %d ways to say a message did not arrive; it must have one",
			strings.Count(send, "sendNotDelivered"))
	}
	event := bodyOf(t, "api_v1_messages.go", "func (h *Handler) apiV1MessageEvent(")
	if !strings.Contains(event, "msgText == sendNotDelivered") {
		t.Fatal("the JSON lane does not recognise a send that did not arrive")
	}
	if !strings.Contains(event, "answerStartRefusal(w, started, StartNotDelivered,") {
		t.Fatal("an undelivered send does not answer through the one refusal that holds the floor")
	}
}

// The gate is not re-asked on every sentence. Re-asking would knock on somebody
// each time their friend typed one, would put an HTTP hop on the messaging hot
// path, and — because the authority is asked about a target and not a Number —
// would judge a conversation opened by Number against the @handle policy
// instead, closing threads their owners never closed.
func TestAnOrdinarySendDoesNotReAskTheContactAuthority(t *testing.T) {
	for _, sig := range []string{
		"func (h *Handler) sendIntoConversation(",
		"func (h *Handler) undeliverableTo(",
	} {
		body := bodyOf(t, "messages.go", sig)
		for _, remote := range []string{"evaluateContact", "callElohim", "notifyContactRequest"} {
			if strings.Contains(body, remote) {
				t.Fatalf("%s calls %s; the gate for an open conversation was passed once, "+
					"when it was opened", sig, remote)
			}
		}
	}
	// And what it does ask is local, in both directions, and never about a room.
	deliver := bodyOf(t, "messages.go", "func (h *Handler) undeliverableTo(")
	if !strings.Contains(deliver, "convo.IsGroup") {
		t.Fatal("one member's block would silence a whole room")
	}
	if strings.Count(deliver, "dbpkg.IsBlocked(") != 2 {
		t.Fatal("a block is read in one direction only; a thread must not deliver either way")
	}
	if !strings.Contains(deliver, "return true") {
		t.Fatal("a membership read that failed must fail closed, not deliver")
	}
}
