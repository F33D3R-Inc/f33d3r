package handler

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// The quote chain on the wire. The loader attaches dbpkg.QuoteRenderDepth
// levels of quotes to a work and the web draws both — a bordered card for the
// first, a bare rail for the second. The JSON lane used to stop after the
// first, so a phone showed a chain the site did not. What is held here: the
// second level is present, a third never is, and a work with nothing under its
// quote does not carry an empty key for one.

// quoteChain builds n works each quoting the next: w1 quotes w2 quotes w3 …
func quoteChain(n int) *model.Work {
	var next *model.Work
	for i := n; i >= 1; i-- {
		next = &model.Work{
			ID:           fmt.Sprintf("w%d", i),
			CID:          fmt.Sprintf("sha256:%d", i),
			AuthorHandle: fmt.Sprintf("author%d", i),
			AuthorName:   fmt.Sprintf("Author %d", i),
			Body:         fmt.Sprintf("level %d", i),
			CreatedAt:    time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
			MediaURLs:    []string{"/media/a.webp", "/media/b.webp"},
			QuotedWork:   next,
		}
	}
	return next
}

// quotedLevels walks quoted → nested → nested … and returns the ids it finds,
// outermost first.
func quotedLevels(t *testing.T, w *model.Work) []string {
	t.Helper()
	raw, err := json.Marshal(workDTO(w, nil))
	if err != nil {
		t.Fatal(err)
	}
	var top struct {
		Quoted json.RawMessage `json:"quoted"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	var ids []string
	cur := top.Quoted
	for len(cur) > 0 {
		var level struct {
			ID     string          `json:"id"`
			Nested json.RawMessage `json:"nested"`
		}
		if err := json.Unmarshal(cur, &level); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, level.ID)
		cur = level.Nested
	}
	return ids
}

func TestQuotedWorkCarriesTheSecondLevel(t *testing.T) {
	got := quotedLevels(t, quoteChain(3))
	if len(got) != 2 || got[0] != "w2" || got[1] != "w3" {
		t.Fatalf("a quote of a quote should reach the wire as quoted=w2, nested=w3; got %v", got)
	}
}

func TestQuotedWorkStopsWhereTheLoaderDoes(t *testing.T) {
	// Longer than the loader ever attaches. Whatever is in memory, the wire
	// carries exactly QuoteRenderDepth levels under the work.
	got := quotedLevels(t, quoteChain(dbpkg.QuoteRenderDepth+3))
	if len(got) != dbpkg.QuoteRenderDepth {
		t.Fatalf("the loader attaches %d levels; the wire carried %d (%v)", dbpkg.QuoteRenderDepth, len(got), got)
	}
}

func TestQuotedWorkWithoutASecondLevelOmitsTheKey(t *testing.T) {
	raw, err := json.Marshal(workDTO(quoteChain(2), nil))
	if err != nil {
		t.Fatal(err)
	}
	var top struct {
		Quoted map[string]json.RawMessage `json:"quoted"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	if _, present := top.Quoted["nested"]; present {
		t.Fatalf("a quote with nothing under it must not carry a nested key; got %s", top.Quoted["nested"])
	}
	if got := quotedLevels(t, quoteChain(1)); len(got) != 0 {
		t.Fatalf("a work that quotes nothing has no quoted key; got %v", got)
	}
}

// The second level is the same shape as the first — the app decodes both with
// one model — so every key the first level has, the second has too.
func TestQuotedWorkLevelsShareOneShape(t *testing.T) {
	raw, err := json.Marshal(workDTO(quoteChain(3), nil))
	if err != nil {
		t.Fatal(err)
	}
	var top struct {
		Quoted struct {
			Nested map[string]json.RawMessage `json:"nested"`
		} `json:"quoted"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "cid", "author", "body", "created_at", "media_urls", "is_nsfw", "is_gore"} {
		if _, ok := top.Quoted.Nested[key]; !ok {
			t.Errorf("nested level is missing %q", key)
		}
	}
}

// ── The quotes list ───────────────────────────────────────────────────────────
//
// GET /api/v1/works/{id}/quotes answers the works that quote one work. It is a
// list of ordinary works, so it is the same WorkPageDTO the replies route
// answers with — the app decodes both with one type, and "View quotes" is a
// plain work list rather than a shape of its own. What is held here: the page
// keys are `works` and `next_cursor`, a full page carries a cursor and a short
// one does not, and a quote in the list still carries its own quote chain, so
// the row draws its bordered card the same way it draws in a feed.

func TestQuotesPageIsTheSameShapeAsReplies(t *testing.T) {
	page := WorkPageDTO{Works: workDTOs([]*model.Work{quoteChain(2)}, nil)}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["works"]; !ok {
		t.Fatalf("a page of quotes is keyed `works`; got %s", raw)
	}
	if _, present := got["next_cursor"]; present {
		t.Fatalf("a page with nothing after it carries no cursor; got %s", raw)
	}
	if len(page.Works) != 1 || page.Works[0].Quoted == nil {
		t.Fatalf("a quote in the list keeps the work it quotes, or the row draws no card")
	}
}

func TestQuotesPageCursorFollowsTheLastRow(t *testing.T) {
	// Two rows for a limit of two: there may be more, so the page names where
	// the next one starts. The cursor is the last row's time, exactly as the
	// replies route computes it.
	older := quoteChain(2)
	older.ID, older.CreatedAt = "w-older", time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC)
	newer := quoteChain(2)
	newer.ID, newer.CreatedAt = "w-newer", time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	works := []*model.Work{newer, older}

	if got := apiNextCursor(works, 2, time.Time{}); got != older.CreatedAt.Format(time.RFC3339Nano) {
		t.Fatalf("a full page points at the oldest row it returned; got %q", got)
	}
	if got := apiNextCursor(works, 20, time.Time{}); got != "" {
		t.Fatalf("a page short of the limit is the end of the list; got %q", got)
	}
}
