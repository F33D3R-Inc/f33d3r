package api

import (
	"encoding/json"
	"testing"
)

// The quote chain and the list of quotes.
//
// The seed carries a chain three deep on purpose — @dev quotes @tehanibentley
// quoting @miiyazuko's four-frame roll — because that is what the card has to
// draw: the work, a bordered card for the work it quotes, and a compact rail
// under that for the work *that* one quotes. What is held here: the wire
// carries both levels under a work, never a third, and
// GET /api/v1/works/{id}/quotes answers the same page shape the replies route
// does.

// findWork returns the first work in the dev account's Following feed whose
// body matches, along with the raw JSON of that row.
func findWork(t *testing.T, h *harness, token, body string) map[string]any {
	t.Helper()
	status, raw := h.do("GET", "/api/v1/feed?surface=following&limit=50", token, nil, "")
	if status != 200 {
		t.Fatalf("feed: %d %s", status, raw)
	}
	var page struct {
		Works []map[string]any `json:"works"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	for _, w := range page.Works {
		if w["body"] == body {
			return w
		}
	}
	t.Fatalf("no work with body %q in the following feed", body)
	return nil
}

func TestSeededQuoteChainReachesTheWireThreeDeep(t *testing.T) {
	h := newHarness(t)
	token, _ := h.login("dev")

	outer := findWork(t, h, token, "Like that")
	quoted, ok := outer["quoted"].(map[string]any)
	if !ok {
		t.Fatalf("the outer work carries no quoted card: %v", keysOf(outer))
	}
	if author := quoted["author"].(map[string]any); author["handle"] != "tehanibentley" {
		t.Fatalf("level one should be @tehanibentley; got %v", author["handle"])
	}
	nested, ok := quoted["nested"].(map[string]any)
	if !ok {
		t.Fatalf("the quoted card carries no nested level: %v", keysOf(quoted))
	}
	if author := nested["author"].(map[string]any); author["handle"] != "miiyazuko" {
		t.Fatalf("level two should be @miiyazuko; got %v", author["handle"])
	}
	// The rail's thumbnail draws "+N" off this: four frames, so "+3".
	media, _ := nested["media_urls"].([]any)
	if len(media) != 4 {
		t.Fatalf("level two is the four-frame roll; got %d media", len(media))
	}
	if _, present := nested["nested"]; present {
		t.Fatalf("nothing draws a third level, so nothing may load one: %v", nested["nested"])
	}
	// Every key the first level has, the second has too — the app decodes both
	// with one model.
	for _, key := range []string{"id", "cid", "author", "body", "created_at", "media_urls", "is_nsfw", "is_gore"} {
		if _, ok := nested[key]; !ok {
			t.Errorf("nested level is missing %q", key)
		}
	}
}

func TestQuotesRouteAnswersThePageShape(t *testing.T) {
	h := newHarness(t)
	token, _ := h.login("dev")

	// The four-frame roll is quoted once, by @tehanibentley.
	outer := findWork(t, h, token, "Like that")
	middle := outer["quoted"].(map[string]any)
	original := middle["nested"].(map[string]any)

	status, raw := h.do("GET", "/api/v1/works/"+original["id"].(string)+"/quotes", token, nil, "")
	if status != 200 {
		t.Fatalf("quotes: %d %s", status, raw)
	}
	var page map[string]any
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	works, ok := page["works"].([]any)
	if !ok {
		t.Fatalf("a page of quotes is keyed `works`; got %s", raw)
	}
	if len(works) != 1 {
		t.Fatalf("the roll is quoted once; got %d", len(works))
	}
	row := works[0].(map[string]any)
	if row["id"] != middle["id"] {
		t.Fatalf("the quote in the list is @tehanibentley's; got %v", row["id"])
	}
	// A row in this list is an ordinary work, so it keeps its own quoted card.
	if _, ok := row["quoted"].(map[string]any); !ok {
		t.Fatalf("a quote in the list keeps the work it quotes, or the row draws no card")
	}
	if _, present := page["next_cursor"]; present {
		t.Fatalf("one row is not a full page, so there is no cursor; got %s", raw)
	}

	// The quote itself is quoted once too, by @dev — the chain is a chain in
	// both directions.
	status, raw = h.do("GET", "/api/v1/works/"+middle["id"].(string)+"/quotes", token, nil, "")
	if status != 200 {
		t.Fatalf("quotes of the quote: %d %s", status, raw)
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	works = page["works"].([]any)
	if len(works) != 1 || works[0].(map[string]any)["body"] != "Like that" {
		t.Fatalf("the quote should itself be quoted by @dev's \"Like that\"; got %s", raw)
	}
}

func TestQuotesRouteRejectsWhatTheRepliesRouteRejects(t *testing.T) {
	h := newHarness(t)
	token, _ := h.login("dev")

	if status, _ := h.do("GET", "/api/v1/works/not-a-uuid/quotes", token, nil, ""); status != 404 {
		t.Fatalf("a malformed id is a 404, as it is on replies; got %d", status)
	}
	if status, _ := h.do("GET", "/api/v1/works/00000000-0000-4000-8000-000000000000/quotes", "", nil, ""); status != 401 {
		t.Fatalf("the quotes list needs a viewer; got %d", status)
	}
}
