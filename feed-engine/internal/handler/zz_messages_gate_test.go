package handler

import (
	"html/template"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/f33d3r/feed-engine/internal/gnosis"
	"github.com/f33d3r/feed-engine/internal/realm"
)

// gateSet parses the facets that make up the in-thread contact gate, with the
// funcs the real renderer supplies to them.
func gateSet(t *testing.T, names ...string) *template.Template {
	t.Helper()
	paths := make([]string, 0, len(names))
	for _, n := range names {
		paths = append(paths, filepath.Join("..", "..", "web", "templates", "partials", n))
	}
	funcs := template.FuncMap{
		"avatarColors": func(string) []string { return []string{"#000", "#fff"} },
		"firstChar":    func(s string) string { return s },
		"realmRing":    realm.RingClass,
		"timeAgo":      func(time.Time) string { return "now" },
		"dict": func(values ...interface{}) (map[string]interface{}, error) {
			m := make(map[string]interface{}, len(values)/2)
			for i := 0; i+1 < len(values); i += 2 {
				m[values[i].(string)] = values[i+1]
			}
			return m, nil
		},
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFiles(paths...)
	if err != nil {
		t.Fatalf("parsing %v: %v", names, err)
	}
	return tmpl
}

// ── The property the whole lane rests on ─────────────────────────────────────

// A Number is guessable in principle, and the only thing between a guesser and
// an identity is that a wrong guess tells them nothing. Every cause of failure
// must therefore arrive at ONE state, and that state must render ONE fragment.
//
// This pins the mapping. It is total on purpose: a decision word this brain has
// never seen — one added to elohim-veni later — must land in the same refusal as
// every other, not acquire a sentence of its own by being unrecognised.
func TestEveryRefusalCauseCollapsesToOneState(t *testing.T) {
	causes := map[string]string{
		"an unknown Number":               "deny",
		"a revoked Number":                "deny",
		"an expired lease":                "deny",
		"an exhausted admission budget":   "deny",
		"a closed policy":                 "deny",
		"a contact link never presented":  "deny",
		"a word this brain does not know": "some_future_decision",
		"an empty answer":                 "",
	}
	for name, decision := range causes {
		if got := contactGateState(decision); got != gnosis.PendingDeny {
			t.Fatalf("%s produced state %q; every refusal must be the one refusal", name, got)
		}
	}
	if got := contactGateState("request"); got != gnosis.PendingRequest {
		t.Fatalf("a knock rendered as %q; a knock is not a refusal", got)
	}
	if got := contactGateState("allow"); got != "" {
		t.Fatalf("an allow produced gate state %q; an allow opens the conversation", got)
	}
}

// And the fragment itself. Two different causes reach the gate as the same
// state, so the bytes are identical by construction — this fails the moment
// somebody adds a branch, a cause parameter, or a helpful clarifying word.
func TestInThreadGateRefusalIsOneFragmentThatNamesNoCause(t *testing.T) {
	tmpl := gateSet(t, "_contact_gate.html")

	first := render(t, tmpl, "contact_gate", map[string]interface{}{
		"Handle": "dana", "Decision": gnosis.PendingDeny, "PendingID": "p1"})
	second := render(t, tmpl, "contact_gate", map[string]interface{}{
		"Handle": "dana", "Decision": gnosis.PendingDeny, "PendingID": "p1"})
	if first != second {
		t.Fatal("two refusals rendered differently")
	}
	lower := strings.ToLower(first)
	for _, leak := range []string{
		"unknown", "revoked", "retired", "expired", "exhausted", "spent",
		"used up", "no longer", "does not exist", "never existed", "wrong person",
		"belongs to", "not accepting", "closed", "blocked",
	} {
		if strings.Contains(lower, leak) {
			t.Fatalf("the refusal leaked why it refused: %q", leak)
		}
	}
	// Nor which wall was met. Naming the policy discloses how somebody has
	// locked their door to a person they did not let in.
	for _, policy := range []string{
		"followers", "mutuals", "number_only", "capability_only", "mutual follow",
		"does not follow", "doesn't follow", "not following",
	} {
		if strings.Contains(lower, policy) {
			t.Fatalf("the refusal disclosed the policy behind it: %q", policy)
		}
	}
}

// The 150 ms floor is the other half of indistinguishability: a lookup that
// answered faster because it did less work would say what it did. This pins the
// mechanism the handler applies on every resolving return.
func TestHoldFloorMakesUnequalWorkTakeEqualTime(t *testing.T) {
	var measured []time.Duration
	for _, work := range []time.Duration{0, 5 * time.Millisecond, 40 * time.Millisecond} {
		started := time.Now()
		time.Sleep(work)
		holdFloor(started, numberResolveFloor)
		measured = append(measured, time.Since(started))
	}
	for i, d := range measured {
		if d < numberResolveFloor {
			t.Fatalf("case %d finished in %v, under the %v floor", i, d, numberResolveFloor)
		}
	}
	spread := measured[0]
	for _, d := range measured {
		if d > spread {
			spread = d
		}
	}
	// Scheduler jitter is real; a cause being readable from the clock is not.
	if spread-numberResolveFloor > 40*time.Millisecond {
		t.Fatalf("the slowest case ran %v over the floor; the work is readable from the clock", spread-numberResolveFloor)
	}
}

// ── The gate, where it belongs ───────────────────────────────────────────────

// A knock is not a rejection, and the message that caused it is still here.
func TestInThreadGateSaysAKnockIsNotARefusal(t *testing.T) {
	tmpl := gateSet(t, "_contact_gate.html")
	knock := render(t, tmpl, "contact_gate", map[string]interface{}{
		"Handle": "dana", "Decision": gnosis.PendingRequest, "PendingID": "p1"})
	if !strings.Contains(knock, "not a refusal") {
		t.Fatal("a knock must say plainly that it is not a refusal")
	}
	if !strings.Contains(knock, "theirs to answer") {
		t.Fatal("a knock must say whose decision it is")
	}
	for _, rejection := range []string{"not reachable", "not accepting", "denied", "declined", "rejected"} {
		if strings.Contains(strings.ToLower(knock), rejection) {
			t.Fatalf("a knock was worded as a rejection: %q", rejection)
		}
	}
}

// A mistyped Number may be named as mistyped — and only that.
func TestInThreadGateNamesAMalformedNumberAndNothingElse(t *testing.T) {
	tmpl := gateSet(t, "_contact_gate.html")
	malformed := render(t, tmpl, "contact_gate", map[string]interface{}{
		"Handle": "dana", "Decision": "malformed", "PendingID": "p1"})
	if !strings.Contains(malformed, "not a F33D3R Number") {
		t.Fatal("a mistyped Number must be named as one, not shown as a refusal")
	}
	refused := render(t, tmpl, "contact_gate", map[string]interface{}{
		"Handle": "dana", "Decision": gnosis.PendingDeny, "PendingID": "p1"})
	if malformed == refused {
		t.Fatal("a typo and a refusal render identically; a person cannot tell they mistyped")
	}
}

// The affordance is the point of the surface: every state that has a held
// message offers the Number, and it carries the held message rather than the
// text of it, so the browser is holding no draft.
func TestInThreadGateOffersTheNumberAndCarriesNoDraft(t *testing.T) {
	tmpl := gateSet(t, "_contact_gate.html")
	for _, state := range []string{gnosis.PendingRequest, gnosis.PendingDeny, gnosis.PendingUnavailable, "malformed"} {
		out := render(t, tmpl, "contact_gate", map[string]interface{}{
			"Handle": "dana", "Decision": state, "PendingID": "p1"})
		if !strings.Contains(out, `name="number"`) {
			t.Fatalf("state %q offered no way to enter a Number", state)
		}
		if !strings.Contains(out, `name="p" value="p1"`) {
			t.Fatalf("state %q did not carry the held message", state)
		}
		if !strings.Contains(out, `name="capability"`) {
			t.Fatalf("state %q offered no contact link; capability_only is unreachable without one", state)
		}
		if !strings.Contains(out, `action="/messages/new-number"`) {
			t.Fatalf("state %q posted somewhere other than the one Number entry point", state)
		}
		// The composed message must not travel through the browser. It is the
		// server's, addressed by id.
		if strings.Contains(out, `name="body"`) {
			t.Fatalf("state %q put the composed message in a form field; the server owns it", state)
		}
		if !strings.Contains(out, `data-facet-id="facet:f33d3r:contact:dana:gate"`) {
			t.Fatalf("state %q is missing its facet id", state)
		}
	}

	// With nothing held there is nothing to send, so nothing is offered.
	bare := render(t, tmpl, "contact_gate", map[string]interface{}{
		"Handle": "dana", "Decision": gnosis.PendingDeny})
	if strings.Contains(bare, `name="number"`) {
		t.Fatal("a gate with no held message offered to send one")
	}
}

// The Number field must not encode the format. Numbers are twelve digits today,
// were thirteen Crockford symbols yesterday, and both still resolve — so a
// pattern, a maxlength or type=number in this facet would turn a valid Number
// into an unenterable one the day the format moves again. canonicalNumber is the
// only thing that decides.
func TestInThreadGateHardcodesNoNumberFormat(t *testing.T) {
	tmpl := gateSet(t, "_contact_gate.html")
	out := render(t, tmpl, "contact_gate", map[string]interface{}{
		"Handle": "dana", "Decision": gnosis.PendingDeny, "PendingID": "p1"})

	numberField := out[strings.Index(out, `name="number"`):]
	numberField = numberField[:strings.Index(numberField, ">")]
	for _, encoded := range []string{"pattern=", "maxlength=", `type="number"`, "minlength="} {
		if strings.Contains(numberField, encoded) {
			t.Fatalf("the Number field encodes the format with %q", encoded)
		}
	}
	// inputmode is presentation, not validation: it asks a phone for the keypad
	// without refusing a single character.
	if !strings.Contains(numberField, `inputmode="numeric"`) {
		t.Fatal("the Number field does not ask a phone for its keypad")
	}
}

// ── The thread that does not exist yet ───────────────────────────────────────

// The whole feature in one render: the message that did not land, still there,
// with the gate underneath it and a composer that addresses a PERSON, because
// there is no conversation to address.
func TestProspectThreadKeepsTheMessageAndPutsTheGateUnderIt(t *testing.T) {
	tmpl := gateSet(t, "_gnosis_prospect_thread.html", "_gnosis_thread.html",
		"_gnosis_bubble.html", "_contact_gate.html", "_user_avatar.html", "_status_dot.html")

	out := render(t, tmpl, "gnosis_prospect_thread", map[string]interface{}{
		"ToHandle": "dana", "ToDisplay": "Dana", "ToAvatar": "",
		"Pending": gnosis.Message{
			ID: "p1", SenderAccount: "me", Mode: gnosis.ModePlain,
			Body: "are you playing the fair on saturday", CreatedAt: time.Now(),
		},
		"PendingID": "p1",
		"Decision":  gnosis.PendingRequest,
	})

	if !strings.Contains(out, `data-facet-id="facet:f33d3r:contact:dana:prospect_thread"`) {
		t.Fatal("facet id missing or malformed")
	}
	if !strings.Contains(out, "are you playing the fair on saturday") {
		t.Fatal("the composed message was lost; preserving it is the point of this surface")
	}
	if !strings.Contains(out, "gn-undelivered") {
		t.Fatal("an undelivered message rendered as though it had been delivered")
	}
	// The gate is INSIDE the thread, under the message, not instead of it.
	msgAt := strings.Index(out, "are you playing the fair on saturday")
	gateAt := strings.Index(out, `data-facet-id="facet:f33d3r:contact:dana:gate"`)
	composerAt := strings.Index(out, `name="to"`)
	if msgAt < 0 || gateAt < 0 || composerAt < 0 {
		t.Fatal("the thread is missing the message, the gate or the composer")
	}
	if !(msgAt < gateAt && gateAt < composerAt) {
		t.Fatal("the gate must sit between the message that failed and the composer")
	}
	// A conversation that does not exist has no mode, so it claims neither.
	if strings.Contains(out, "Encrypted") || strings.Contains(out, "Standard") {
		t.Fatal("a conversation that has not been created claimed an encryption mode")
	}
	if !strings.Contains(out, `value="dana"`) {
		t.Fatal("the composer does not address the person it is written to")
	}
}

// Before anything has been written there is no failure, so there is no gate —
// opening a conversation with somebody must cost them nothing and tell them
// nothing.
func TestProspectThreadIsQuietBeforeAnythingIsWritten(t *testing.T) {
	tmpl := gateSet(t, "_gnosis_prospect_thread.html", "_gnosis_thread.html",
		"_gnosis_bubble.html", "_contact_gate.html", "_user_avatar.html", "_status_dot.html")

	out := render(t, tmpl, "gnosis_prospect_thread", map[string]interface{}{
		"ToHandle": "dana", "ToDisplay": "Dana", "ToAvatar": "",
		"Pending": nil, "PendingID": "", "Decision": "",
	})
	if strings.Contains(out, "gn-gate") {
		t.Fatal("a gate was drawn before any message had failed to send")
	}
	if !strings.Contains(out, `name="to" value="dana"`) {
		t.Fatal("there is no way to write to the person the pane is open on")
	}
}

// The composer renders the server's held message back into the field only where
// it cannot be delivered automatically — a sealed conversation, which plaintext
// must never enter. The browser stored nothing; it is displaying what arrived.
func TestSealedComposerCarriesTheHeldMessageBackFromTheServer(t *testing.T) {
	tmpl := gateSet(t, "_gnosis_thread.html", "_user_avatar.html")
	out := render(t, tmpl, "gnosis_composer", map[string]interface{}{
		"ConvoID": "c1", "Sealed": true, "ToHandle": "", "Draft": "met you at the fair",
	})
	if !strings.Contains(out, `value="met you at the fair"`) {
		t.Fatal("a held message was dropped instead of being handed to the composer that can seal it")
	}
	if !strings.Contains(out, `data-sealed="1"`) {
		t.Fatal("the sealed composer stopped being the sealed composer")
	}
}

// A knock is a decision taken BEFORE the message is received. The gate must
// therefore offer a note as its own field and must never put the composed
// message where the recipient could read it before they have agreed to.
func TestInThreadGateOffersANoteThatIsNotTheMessage(t *testing.T) {
	tmpl := gateSet(t, "_contact_gate.html")
	out := render(t, tmpl, "contact_gate", map[string]interface{}{
		"Handle": "dana", "Decision": gnosis.PendingRequest, "PendingID": "p1"})
	if !strings.Contains(out, `name="note"`) {
		t.Fatal("no note field: a sender under a reviewing policy cannot say why they are asking")
	}
	if strings.Contains(out, `name="body"`) {
		t.Fatal("the composed message was put in the form; it is the server's and it is not the note")
	}
}

// contactNote bounds what crosses to a recipient before they decide, and it must
// cut on a character boundary — half a rune is not a note.
func TestContactNoteIsBoundedAndCutsOnCharacters(t *testing.T) {
	if got := contactNote("  met you at the fair  "); got != "met you at the fair" {
		t.Fatalf("a plain note came back as %q", got)
	}
	long := strings.Repeat("é", 400)
	got := contactNote(long)
	if len([]rune(got)) != 280 {
		t.Fatalf("a long note kept %d runes, want 280", len([]rune(got)))
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncation split a character")
	}
}
