package handler

import (
	"strings"
	"testing"
	"time"

	"github.com/f33d3r/feed-engine/internal/model"
)

// A poll work reaching the render with PollOptions but no projected ballot is
// the bug this file exists to keep closed: the card drew the question and
// nothing else, because work_core had no ballot to draw and nothing populated
// one.

func openPoll(userVote int) *model.Poll {
	ends := time.Now().Add(3 * time.Hour)
	p := &model.Poll{
		Options:  []string{"Rust", "Go", "Neither"},
		Votes:    []int{1, 2, 0},
		UserVote: userVote,
		EndsAt:   &ends,
	}
	p.Project(time.Now())
	return p
}

func TestPollProjectComputesEveryRenderedValue(t *testing.T) {
	p := openPoll(1)

	if p.TotalVotes != 3 {
		t.Fatalf("total votes: got %d, want 3", p.TotalVotes)
	}
	// Rounded, not truncated: two of three is 67%, and a ballot that reads 66%
	// is a ballot the server computed wrong.
	wantPct := []int{33, 67, 0}
	for i, w := range wantPct {
		if p.Results[i].Pct != w {
			t.Errorf("option %d pct: got %d, want %d", i, p.Results[i].Pct, w)
		}
	}
	if !p.Results[1].IsWinner || p.Results[0].IsWinner || p.Results[2].IsWinner {
		t.Errorf("winner marks: got %v, want only option 1", []bool{
			p.Results[0].IsWinner, p.Results[1].IsWinner, p.Results[2].IsWinner})
	}
	// Voted marks the viewer's own ballot and nothing else — it is not the winner.
	if !p.Results[1].Voted || p.Results[0].Voted {
		t.Error("Voted must mark exactly the option the viewer chose")
	}
	if p.Closed || p.Ended {
		t.Error("a poll ending in three hours is open")
	}
	if p.TimeLeft != "3 hours left" {
		t.Errorf("TimeLeft: got %q, want %q", p.TimeLeft, "3 hours left")
	}
}

func TestPollProjectClosesAnExpiredBallotAndNeverClosesAnEndlessOne(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	closed := &model.Poll{Options: []string{"a", "b"}, Votes: []int{0, 0}, UserVote: -1, EndsAt: &past}
	closed.Project(time.Now())
	if !closed.Closed || !closed.Ended {
		t.Error("a poll past its end time is closed")
	}
	if closed.TimeLeft != "Final results" {
		t.Errorf("closed TimeLeft: got %q", closed.TimeLeft)
	}
	// Zero votes must not produce a winner, or every option wins by default.
	for i, r := range closed.Results {
		if r.IsWinner {
			t.Errorf("option %d won an election with no votes", i)
		}
	}

	endless := &model.Poll{Options: []string{"a", "b"}, Votes: []int{1, 0}, UserVote: -1}
	endless.Project(time.Now())
	if endless.Closed || endless.TimeLeft != "" {
		t.Errorf("a poll with no end time never closes and carries no label: closed=%v label=%q",
			endless.Closed, endless.TimeLeft)
	}
}

func TestPollCardOffersTheBallotThenReportsIt(t *testing.T) {
	tmpl := partialSet(t, "_poll_card.html", "_poll_bar.html")

	voting := render(t, tmpl, "poll_card", map[string]interface{}{
		"PostID": "w1", "Poll": openPoll(-1), "Surface": "pcd", "ReadOnly": false,
	})
	if !strings.Contains(voting, `hx-post="/api/poll/vote"`) {
		t.Error("an open ballot the viewer has not cast offers the vote")
	}
	if strings.Count(voting, "poll-option-btn") != 3 {
		t.Errorf("three options, three buttons: %s", voting)
	}
	if strings.Contains(voting, "poll-bar-fill") {
		t.Error("results must not leak before the viewer votes")
	}

	voted := render(t, tmpl, "poll_card", map[string]interface{}{
		"PostID": "w1", "Poll": openPoll(1), "Surface": "pcd", "ReadOnly": false,
	})
	if strings.Contains(voted, "poll-option-btn") {
		t.Error("a cast ballot is reported, never re-offered")
	}
	if strings.Count(voted, "poll-bar-fill") != 3 {
		t.Errorf("three options, three bars: %s", voted)
	}
	if !strings.Contains(voted, "poll-voted") || !strings.Contains(voted, "poll-winner") {
		t.Error("the viewer's choice and the leading option are marked separately")
	}
	if !strings.Contains(voted, "3 votes · 3 hours left") {
		t.Errorf("the tally line is rendered by the server: %s", voted)
	}
}

func TestPollCardReadOnlySurfaceShowsTheBallotWithoutTakingAVote(t *testing.T) {
	tmpl := partialSet(t, "_poll_card.html", "_poll_bar.html")
	out := render(t, tmpl, "poll_card", map[string]interface{}{
		"PostID": "w1", "Poll": openPoll(-1), "Surface": "embed", "ReadOnly": true,
	})
	if strings.Contains(out, "hx-post") {
		t.Error("a quoted ballot is shown, never taken")
	}
	if strings.Count(out, "poll-bar-fill") != 3 {
		t.Errorf("a read-only ballot still shows its results: %s", out)
	}
	// Nothing swaps it, and the same work can be quoted twice on one page, so it
	// claims no id — only the facet id the stream addresses.
	if strings.Contains(out, `id="poll-`) {
		t.Errorf("a read-only ballot must not claim a swap-target id: %s", out)
	}
	if !strings.Contains(out, `data-facet-id="facet:f33d3r:work:w1:poll_embed"`) {
		t.Errorf("every fragment carries its facet id: %s", out)
	}
}

func TestPollCardNamespacesItsSwapTargetBySurface(t *testing.T) {
	// The desktop card and the mobile focus card both draw the same work into
	// the same DOM. Without a per-surface id, a vote on one swaps the other.
	tmpl := partialSet(t, "_poll_card.html", "_poll_bar.html")
	seen := map[string]bool{}
	for _, surface := range []string{"pcd", "pcf"} {
		out := render(t, tmpl, "poll_card", map[string]interface{}{
			"PostID": "w1", "Poll": openPoll(-1), "Surface": surface, "ReadOnly": false,
		})
		id := `id="poll-` + surface + `-w1"`
		if !strings.Contains(out, id) {
			t.Errorf("surface %s: missing %s", surface, id)
		}
		if !strings.Contains(out, `hx-target="#poll-`+surface+`-w1"`) {
			t.Errorf("surface %s: vote must swap its own ballot", surface)
		}
		if !strings.Contains(out, `data-facet-id="facet:f33d3r:work:w1:poll_`+surface+`"`) {
			t.Errorf("surface %s: fragment must carry its facet id: %s", surface, out)
		}
		if seen[id] {
			t.Errorf("surface %s reuses another surface's DOM id", surface)
		}
		seen[id] = true
	}
}

func TestPollSurfaceRejectsAnUnknownSurface(t *testing.T) {
	for _, s := range []string{"pcd", "pcf"} {
		if got := pollSurface(s); got != s {
			t.Errorf("pollSurface(%q) = %q", s, got)
		}
	}
	// An unknown value must never reach a DOM id.
	for _, s := range []string{"", "embed", `x" onload="alert(1)`} {
		if got := pollSurface(s); got != "pcd" {
			t.Errorf("pollSurface(%q) = %q, want pcd", s, got)
		}
	}
}

func TestWorkCoreDrawsTheBallotForAPollWork(t *testing.T) {
	tmpl := partialSet(t, "_work_core.html", "_poll_card.html", "_poll_bar.html")
	// html/template walks every branch when it escapes, including the ones this
	// poll work never takes, so the facets work_core can reach must exist in the
	// set. They are stubbed rather than parsed: what is under test is whether the
	// ballot is drawn, not what a video player looks like.
	if _, err := tmpl.Parse(`
{{define "react_video_player"}}{{end}}
{{define "content_gate"}}{{end}}
{{define "media_container"}}{{end}}
{{define "quoted_work"}}{{end}}
{{define "youtube_embed"}}{{end}}
{{define "link_preview"}}{{end}}
{{define "post_voice_player"}}{{end}}
`); err != nil {
		t.Fatalf("stubbing work_core's children: %v", err)
	}

	w := &model.Work{
		ID:          "w1",
		Kind:        "poll",
		Body:        "Rust or Go?",
		PollOptions: []string{"Rust", "Go", "Neither"},
		Poll:        openPoll(-1),
	}
	feed := render(t, tmpl, "work_core", map[string]interface{}{"W": w, "Ctx": nil, "Variant": "feed"})
	if !strings.Contains(feed, "poll-wrap") || !strings.Contains(feed, "poll-option-btn") {
		t.Fatalf("a poll work must draw its ballot in the feed: %s", feed)
	}

	embed := render(t, tmpl, "work_core", map[string]interface{}{"W": w, "Variant": "embed"})
	if !strings.Contains(embed, "poll-wrap") {
		t.Fatalf("a quoted poll must draw its ballot: %s", embed)
	}
	if strings.Contains(embed, "hx-post") {
		t.Error("a quoted ballot must not take a vote")
	}

	// A work with no poll draws no ballot — the facet is conditional, not always-on.
	plain := &model.Work{ID: "w2", Kind: "post", Body: "hello"}
	out := render(t, tmpl, "work_core", map[string]interface{}{"W": plain, "Ctx": nil, "Variant": "feed"})
	if strings.Contains(out, "poll-wrap") {
		t.Errorf("a non-poll work drew a ballot: %s", out)
	}
}
