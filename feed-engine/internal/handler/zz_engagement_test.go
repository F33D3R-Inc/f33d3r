package handler

import (
	"encoding/json"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/f33d3r/feed-engine/internal/model"
)

// engagementTestWork is the work every case here publishes: counts that are all
// different from one another, so a twin that swapped two of them is caught, and
// viewer state set to true so a fragment that leaked it would be visible.
func engagementTestWork() *model.Work {
	return &model.Work{
		ID:               "3f2b9c14-6a7d-4c2e-9b31-0d8e5a71c4f0",
		CID:              "bafy2bzacedengagementtest",
		AuthorHandle:     "miiyazuko",
		AuthorName:       "Miiyazuko",
		Body:             "the tally moves for everyone watching",
		CreatedAt:        time.Now().Add(-3 * time.Minute),
		LikeCount:        43,
		DislikeCount:     2,
		ReplyCount:       11,
		RepostCount:      7,
		QuoteCount:       3,
		BookmarkCount:    5,
		ViewCount:        1902,
		LikedByUser:      true,
		BookmarkedByUser: true,
		RepostedByUser:   true,
		DislikedByUser:   true,
	}
}

// TestWorkEngagementTwinKeys pins the JSON half of the event: the counts, under
// the names WorkDTO already uses for them, and nothing that is true of one
// viewer and false for another.
//
// The key names are asserted against WorkDTO itself rather than a copied list,
// so renaming a count on the DTO fails here instead of quietly leaving the
// stream speaking an older vocabulary than the reads.
func TestWorkEngagementTwinKeys(t *testing.T) {
	raw, err := json.Marshal(newWorkEngagementTwin(engagementTestWork()))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	want := map[string]float64{
		"like_count":     43,
		"dislike_count":  2,
		"reply_count":    11,
		"repost_count":   7,
		"quote_count":    3,
		"bookmark_count": 5,
		"view_count":     1902,
	}
	for k, v := range want {
		raw, ok := got[k]
		if !ok {
			t.Errorf("twin is missing %q", k)
			continue
		}
		var n float64
		if err := json.Unmarshal(raw, &n); err != nil || n != v {
			t.Errorf("twin %q = %s, want %v", k, raw, v)
		}
	}
	var id string
	if err := json.Unmarshal(got["work_id"], &id); err != nil || id != engagementTestWork().ID {
		t.Errorf("twin work_id = %s, want the work's id", got["work_id"])
	}
	if len(got) != len(want)+1 {
		t.Errorf("twin carries %d keys, want %d (work_id plus the counts): %s", len(got), len(want)+1, raw)
	}

	// Every count key must be a WorkDTO key, so the client applies the event to
	// the model it already holds without translating.
	dtoKeys := jsonKeys(t, &WorkDTO{})
	for k := range want {
		if !dtoKeys[k] {
			t.Errorf("twin key %q is not a WorkDTO key", k)
		}
	}

	// Viewer-relative state must never ride a fan-out event.
	for _, forbidden := range []string{
		"liked_by_viewer", "disliked_by_viewer", "reposted_by_viewer",
		"bookmarked_by_viewer", "viewer_follows_author", "liked", "viewer",
	} {
		if _, bad := got[forbidden]; bad {
			t.Errorf("twin carries viewer-relative key %q", forbidden)
		}
	}
}

// TestPublishWorkEngagementReachesWatchers is the whole L1 path below the
// database: a session that has said it is watching a work receives both
// projections of one mutation, and a session that has not says nothing.
func TestPublishWorkEngagementReachesWatchers(t *testing.T) {
	h := liveTestHandler(t)
	work := engagementTestWork()

	const (
		watcherAcct = "acct-engagement-watcher"
		watcherPIAL = "pial-engagement-watcher"
		idlerAcct   = "acct-engagement-idler"
		idlerPIAL   = "pial-engagement-idler"
	)
	watcher, _, closeWatcher := RegisterSSESession(watcherAcct, watcherPIAL)
	defer closeWatcher()
	idler, _, closeIdler := RegisterSSESession(idlerAcct, idlerPIAL)
	defer closeIdler()

	AddPostWatcher(watcherPIAL, work.ID)
	defer RemovePostWatcher(watcherPIAL, work.ID)

	h.publishWorkEngagementFor(work)

	events := drainEvents(t, watcher, 2)
	if len(events) != 2 {
		t.Fatalf("watcher received %d events, want 2 (work_engagement + post_engagement)", len(events))
	}
	byType := map[string]SSEEvent{}
	for _, ev := range events {
		byType[ev.Type] = ev
	}

	// The app's half: a twin, no fragment.
	app, ok := byType["work_engagement"]
	if !ok {
		t.Fatalf("no work_engagement event: got %v", events)
	}
	if app.JSON == "" {
		t.Error("work_engagement carries no JSON twin — the app's stream drops events without one")
	}
	if app.Data != "" {
		t.Errorf("work_engagement carries an HTML fragment the app cannot use: %q", app.Data)
	}
	var twin map[string]interface{}
	if err := json.Unmarshal([]byte(app.JSON), &twin); err != nil {
		t.Fatalf("work_engagement twin is not an object: %v", err)
	}
	if twin["like_count"] != float64(43) || twin["work_id"] != work.ID {
		t.Errorf("work_engagement twin = %s", app.JSON)
	}

	// The web's half: a rendered Facet under the name f33d3r.js listens for,
	// and no twin (so the app's whitelist never has to reject it).
	web, ok := byType["post_engagement"]
	if !ok {
		t.Fatalf("no post_engagement event: got %v", events)
	}
	if web.JSON != "" {
		t.Errorf("post_engagement carries a JSON twin it should not: %q", web.JSON)
	}
	assertCountsFragment(t, web.Data, work)

	// A session that never said it was watching hears nothing.
	select {
	case ev := <-idler:
		t.Errorf("a non-watching session received %q", ev.Type)
	default:
	}
}

// TestWorkCountsRowIsViewerNeutral: the Fragment goes to every watcher at once,
// so it must not contain a single mark of who caused it. The work it renders has
// every viewer flag set — none of them may appear.
func TestWorkCountsRowIsViewerNeutral(t *testing.T) {
	h := liveTestHandler(t)
	work := engagementTestWork()
	frag := h.renderWorkCountsRow(work)
	assertCountsFragment(t, frag, work)

	for _, leak := range []string{"aria-pressed", "is-on", "wab-like", "toggleWorkReaction"} {
		if strings.Contains(frag, leak) {
			t.Errorf("counts fragment leaks viewer state (%q): %s", leak, frag)
		}
	}
}

// assertCountsFragment checks the one thing the browser needs: a single root
// element whose id names an element the post page already draws, carrying the
// new tallies. f33d3r.js reads firstElementChild.id and swaps by that id, so a
// fragment without one is silently dropped.
func assertCountsFragment(t *testing.T, frag string, work *model.Work) {
	t.Helper()
	if strings.TrimSpace(frag) == "" {
		t.Fatal("counts fragment is empty")
	}
	wantID := "work-counts-" + work.ID
	root := regexp.MustCompile(`<div[^>]*\sid="([^"]+)"`).FindStringSubmatch(frag)
	if root == nil {
		t.Fatalf("counts fragment has no id on its root element: %s", frag)
	}
	if root[1] != wantID {
		t.Errorf("counts fragment root id = %q, want %q", root[1], wantID)
	}
	for _, want := range []string{"43", "7", "3"} { // likes, reposts, quotes
		if !strings.Contains(frag, ">"+want+"<") {
			t.Errorf("counts fragment does not show %s: %s", want, frag)
		}
	}
}

func drainEvents(t *testing.T, ch <-chan SSEEvent, n int) []SSEEvent {
	t.Helper()
	var out []SSEEvent
	deadline := time.After(2 * time.Second)
	for len(out) < n {
		select {
		case ev := <-ch:
			out = append(out, ev)
		case <-deadline:
			return out
		}
	}
	return out
}

// TestReactionWantsJSON: the browser's form post keeps the 204 it has always
// had; a JSON body asks for the work back.
func TestReactionWantsJSON(t *testing.T) {
	cases := []struct {
		contentType string
		want        bool
	}{
		{"application/json", true},
		{"application/json; charset=utf-8", true},
		{"Application/JSON", true},
		{"application/x-www-form-urlencoded", false},
		{"multipart/form-data; boundary=x", false},
		{"", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "/events", strings.NewReader(""))
		if c.contentType != "" {
			r.Header.Set("Content-Type", c.contentType)
		}
		if got := reactionWantsJSON(r); got != c.want {
			t.Errorf("Content-Type %q: reactionWantsJSON = %v, want %v", c.contentType, got, c.want)
		}
	}
}

// TestRoomSessionsDoNotEvictUserStream is L6: walking into rooms must not cost
// the user the stream their notifications arrive on. Six rooms — one past the
// per-account bound — are opened on top of one account stream, and the account
// stream is still there and still delivered to.
func TestRoomSessionsDoNotEvictUserStream(t *testing.T) {
	const acct = "acct-room-cap"
	const pial = "pial-room-cap"

	userCh, userDone, closeUser := RegisterSSESession(acct, pial)
	defer closeUser()

	for i := 0; i < maxSSEConnsPerUser+1; i++ {
		_, _, closeRoom := RegisterRoomSSESession(acct, pial)
		defer closeRoom()
	}

	select {
	case <-userDone:
		t.Fatal("opening rooms evicted the account stream")
	default:
	}

	PublishToAccount(acct, SSEEvent{Type: "notify", Data: "1"})
	select {
	case ev := <-userCh:
		if ev.Type != "notify" {
			t.Errorf("account stream received %q, want notify", ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("account stream is registered but no longer delivered to")
	}
}

// TestAccountStreamsStillEvict: the bound is not gone, only narrowed. Opening
// more account streams than it allows still reaps the oldest, which is what
// keeps a refresh from leaving a dead connection behind.
func TestAccountStreamsStillEvict(t *testing.T) {
	const acct = "acct-cap-still-holds"
	const pial = "pial-cap-still-holds"

	_, firstDone, closeFirst := RegisterSSESession(acct, pial)
	defer closeFirst()
	for i := 0; i < maxSSEConnsPerUser; i++ {
		_, _, closeNext := RegisterSSESession(acct, pial)
		defer closeNext()
	}

	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("the oldest account stream was not evicted once the bound was passed")
	}
}
