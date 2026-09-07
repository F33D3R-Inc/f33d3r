package handler

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/f33d3r/feed-engine/internal/config"
	"github.com/f33d3r/feed-engine/internal/model"
)

// Proves a quote OF a quote renders as one nested column: the outer quote keeps
// its bordered card, the twice-quoted work hangs off the quoted_work_nested rail
// (never a second card), and its media collapses to a thumbnail with the +N
// overflow count. Regression guard for the two-level quote render.
func TestQuotedWorkRendersNestedQuoteAsRail(t *testing.T) {
	t.Chdir("../..")

	h := &Handler{cfg: &config.Config{}}
	h.loadTemplates()

	now := time.Now()
	inner := &model.Work{
		ID:           "22222222-2222-4222-8222-222222222222",
		Kind:         "post",
		Body:         "F40 — stay tuned",
		AuthorHandle: "lewishamilton",
		AuthorName:   "Lewis Hamilton",
		IsVerified:   true,
		TimeAgo:      "3d",
		CreatedAt:    now.Add(-72 * time.Hour),
		MediaURLs:    []string{"/m/a.jpg", "/m/b.jpg", "/m/c.jpg", "/m/d.jpg"},
	}
	outer := &model.Work{
		ID:           "11111111-1111-4111-8111-111111111111",
		Kind:         "quote",
		Body:         "I don't follow f1 but I fuck wit this",
		AuthorHandle: "schassis_eddi",
		AuthorName:   "Eddi",
		IsVerified:   true,
		TimeAgo:      "2d",
		CreatedAt:    now.Add(-48 * time.Hour),
		QuotedWork:   inner,
	}

	var buf bytes.Buffer
	if err := h.partial.ExecuteTemplate(&buf, "quoted_work", outer); err != nil {
		t.Fatalf("execute quoted_work: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"quoted-post-card",     // level 1 keeps the bordered card
		"@schassis_eddi", "2d", // level 1 identity line carries the time
		"quoted-nested", // level 2 is the rail, drawn inside level 1
		"@lewishamilton", "3d",
		"quoted-nested-thumb", // level 2 media is a thumbnail, not a player
		"+3",                  // 4 media → 1 shown, 3 more
		"badge-verified",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("nested quote render missing %q\n---\n%s", want, out)
		}
	}
	if n := strings.Count(out, "quoted-post-card"); n != 1 {
		t.Fatalf("a quote of a quote must not open a second card: got %d cards\n---\n%s", n, out)
	}
	if strings.Contains(out, "post-media-grid") {
		t.Fatalf("level-2 quote must not re-draw the full media container\n---\n%s", out)
	}
}
