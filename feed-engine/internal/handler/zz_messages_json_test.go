package handler

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/f33d3r/feed-engine/internal/gnosis"
)

// The JSON messaging surface has two properties that are not about shape, and
// both of them are load-bearing:
//
//   - a sealed conversation crosses this boundary as ciphertext and nothing
//     else — no plaintext, no preview standing in for plaintext;
//   - the web keeps its rendered answer, whatever the app asks for.
//
// The tests below hold those, and the pagination key that makes a thread
// readable without losing or repeating a message.

func TestThreadCursorRoundTrip(t *testing.T) {
	m := gnosis.Message{
		ID:        "6f1f0e2a-64f6-4b3e-9a0d-8a4a0a3f1c11",
		CreatedAt: time.Date(2026, 9, 5, 12, 0, 0, 123456789, time.UTC),
	}
	at, id, ok := decodeThreadCursor(encodeThreadCursor(m))
	if !ok {
		t.Fatal("a cursor this package wrote was refused by it")
	}
	if id != m.ID {
		t.Errorf("id: got %q want %q", id, m.ID)
	}
	if !at.Equal(m.CreatedAt) {
		t.Errorf("instant: got %s want %s — nanoseconds are the tie-break, they cannot be rounded away", at, m.CreatedAt)
	}
	// An empty cursor is the first page, not an error.
	if at, id, ok := decodeThreadCursor(""); !ok || !at.IsZero() || id != "" {
		t.Errorf("empty cursor: %v %q %v", at, id, ok)
	}
	// Anything else is refused here rather than reaching SQL as a cast.
	for _, bad := range []string{
		"!!!not base64!!!",
		encodeCursorParts("2026-09-05T12:00:00Z", "not-a-uuid"),
		encodeCursorParts("yesterday", "6f1f0e2a-64f6-4b3e-9a0d-8a4a0a3f1c11"),
		encodeCursorParts("2026-09-05T12:00:00Z", ""),
	} {
		if _, _, ok := decodeThreadCursor(bad); ok {
			t.Errorf("%q was accepted as a cursor", bad)
		}
	}
}

// encodeCursorParts builds a cursor out of parts that need not be valid, so
// the decoder can be shown a malformed one.
func encodeCursorParts(at, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at + "|" + id))
}

func TestConversationDTOSaysNothingAboutASealedConversation(t *testing.T) {
	sealed := conversationDTO(gnosis.ConvoListRow{
		ConversationID: "c1",
		Mode:           gnosis.ModeSealed,
		LastMode:       gnosis.ModeSealed,
		Preview:        "🔒 Encrypted message",
		OtherHandle:    "miiyazuko",
	})
	if sealed.Preview != "" {
		t.Errorf("sealed preview crossed as %q; the server holds ciphertext and has nothing to preview", sealed.Preview)
	}
	if sealed.Mode != gnosis.ModeSealed {
		t.Errorf("mode: %q — the client draws its own placeholder from this", sealed.Mode)
	}

	plain := conversationDTO(gnosis.ConvoListRow{
		ConversationID: "c2",
		Mode:           gnosis.ModePlain,
		LastMode:       gnosis.ModePlain,
		Preview:        "see you at eight",
	})
	if plain.Preview != "see you at eight" {
		t.Errorf("plain preview: %q", plain.Preview)
	}
}

func TestMessagePushCarriesNoPlaintext(t *testing.T) {
	plain := messagePushJSON(gnosis.Message{
		ID: "m1", ConversationID: "c1", SenderAccount: "a1",
		Mode: gnosis.ModePlain, Body: "the-secret-sentence",
		CreatedAt: time.Now(),
	}, "dev")
	if strings.Contains(plain, "the-secret-sentence") {
		t.Errorf("the push twin carried the message body: %s", plain)
	}
	var got MessagePushDTO
	if err := json.Unmarshal([]byte(plain), &got); err != nil {
		t.Fatalf("twin is not an object: %v", err)
	}
	if got.MessageID != "m1" || got.ConversationID != "c1" || got.SenderHandle != "dev" {
		t.Errorf("twin does not say what landed and where: %+v", got)
	}

	// A sealed message is opaque to the server, so it rides whole: the client
	// decrypts from the push without asking for anything back.
	sealed := messagePushJSON(gnosis.Message{
		ID: "m2", ConversationID: "c1", SenderAccount: "a1", Mode: gnosis.ModeSealed,
		BodyCtB64: "CT", BodyNonceB64: "NONCE", EphPubB64: "EPH", SealedB64: "K", SealedNonceB64: "KN",
		CreatedAt: time.Now(),
	}, "dev")
	if err := json.Unmarshal([]byte(sealed), &got); err != nil {
		t.Fatalf("sealed twin: %v", err)
	}
	if got.BodyCtB64 != "CT" || got.SealedB64 != "K" || got.EphPubB64 != "EPH" {
		t.Errorf("sealed twin lost the envelope: %+v", got)
	}
}

func TestMessageDTOIsViewerRelative(t *testing.T) {
	m := gnosis.Message{ID: "m1", ConversationID: "c1", SenderAccount: "a1", Mode: gnosis.ModePlain, Body: "hi"}
	if !messageDTO(m, "a1", gnosis.Participant{Handle: "dev"}).IsMine {
		t.Error("the sender's own message is not marked as theirs")
	}
	if messageDTO(m, "a2", gnosis.Participant{Handle: "dev"}).IsMine {
		t.Error("somebody else's message is marked as the viewer's")
	}
	if got := messageDTO(m, "a2", gnosis.Participant{Handle: "dev", Display: "Dev"}).SenderDisplay; got != "Dev" {
		t.Errorf("sender not hydrated: %q", got)
	}
}

func TestWantsJSONNeverTakesTheWebsFragment(t *testing.T) {
	cases := []struct {
		accept, contentType string
		want                bool
	}{
		// What HTMX sends.
		{"text/html", "application/x-www-form-urlencoded", false},
		{"text/html, */*; q=0.01", "application/x-www-form-urlencoded", false},
		{"*/*", "application/x-www-form-urlencoded", false},
		{"", "application/x-www-form-urlencoded", false},
		// What the app sends.
		{"application/json", "application/json", true},
		{"", "application/json", true},
		{"application/json, text/plain", "", true},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "/events", strings.NewReader("{}"))
		if c.accept != "" {
			r.Header.Set("Accept", c.accept)
		}
		if c.contentType != "" {
			r.Header.Set("Content-Type", c.contentType)
		}
		if got := apiWantsJSON(r); got != c.want {
			t.Errorf("Accept=%q Content-Type=%q: got %v want %v", c.accept, c.contentType, got, c.want)
		}
	}
}

// ── message_start: the contact gate, told in JSON ────────────────────────────
//
// The HTML gate collapses six causes into one answer in one duration. A JSON
// lane onto the same act that graded its refusals would rebuild the
// enumeration oracle the HTML one denies, in a shape that is far easier to
// script against. These pin that it does not.

func TestEveryStartRefusalCauseCollapsesToOneState(t *testing.T) {
	// The same table as TestEveryRefusalCauseCollapsesToOneState, because it is
	// the same set of causes reaching a second surface.
	causes := map[string]string{
		"a handle nobody holds":            gnosis.PendingDeny,
		"an unknown Number":                "deny",
		"a revoked Number":                 "deny",
		"an expired lease":                 "deny",
		"an exhausted admission budget":    "deny",
		"a closed policy":                  "deny",
		"a contact link never presented":   "deny",
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

func TestStartRefusalIsOneShapeThatNamesNoCause(t *testing.T) {
	body := func(state string) (string, int, time.Duration) {
		rec := httptest.NewRecorder()
		started := time.Now()
		answerStartRefusal(rec, started, state, "")
		return rec.Body.String(), rec.Code, time.Since(started)
	}
	first, status, took := body(StartNotDelivered)
	second, _, _ := body(StartNotDelivered)
	if first != second {
		t.Fatalf("two refusals rendered differently: %s vs %s", first, second)
	}
	if status != 200 {
		t.Fatalf("a refusal answered %d; a status code that differs from an allow's is itself the cause", status)
	}
	if took < numberResolveFloor {
		t.Fatalf("a refusal answered in %v, under the %v floor", took, numberResolveFloor)
	}
	// It says nothing but the one word, and it hands back no conversation.
	var got ConversationStartDTO
	if err := json.Unmarshal([]byte(first), &got); err != nil {
		t.Fatalf("refusal is not the one shape: %v", err)
	}
	if got.State != StartNotDelivered || got.Conversation != nil {
		t.Fatalf("a refusal carried more than its state: %+v", got)
	}
	lower := strings.ToLower(first)
	for _, leak := range []string{
		"unknown", "revoked", "retired", "expired", "exhausted", "spent",
		"no longer", "does not exist", "never existed", "wrong person",
		"belongs to", "not accepting", "closed", "blocked", "policy",
		"authority", "unavailable", "found",
	} {
		if strings.Contains(lower, leak) {
			t.Fatalf("the refusal leaked why it refused: %q in %s", leak, first)
		}
	}

	// A knock is a different answer, and it is allowed to be: the recipient was
	// genuinely asked, and the sender is owed the truth that their message is
	// waiting rather than gone. It still names no cause and still holds the floor.
	knock, kstatus, ktook := body(StartHeld)
	if kstatus != status {
		t.Fatalf("a knock answered %d and a refusal %d; the status told them apart", kstatus, status)
	}
	if ktook < numberResolveFloor {
		t.Fatalf("a knock answered in %v, under the %v floor", ktook, numberResolveFloor)
	}
	if knock == first {
		t.Fatal("a knock and a refusal are the same answer; a held message would look lost")
	}
	if strings.Count(knock, `"`) != strings.Count(first, `"`) {
		t.Fatalf("a knock is a different shape from a refusal, not a different word: %s vs %s", knock, first)
	}
}
