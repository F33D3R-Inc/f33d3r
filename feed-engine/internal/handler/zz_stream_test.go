package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/f33d3r/feed-engine/internal/model"
)

// The stream's promises, held where a database is not needed to hold them.
//
// A live stream fails quietly by nature: nobody sees a frame that was never
// written, and the symptom of a broken one is a screen that is merely out of
// date. So the things pinned here are the ones with no other witness — that a
// frame says where it sits in the sequence, that a client which says where it
// left off is handed exactly what it missed and is refused when the server
// cannot honestly say, that a reply is not announced as a post, and that the
// read-receipt lane reads the body the apps actually send.

// streamPIAL gives each test its own person, since the sequence and the ring
// are per-PIAL and the registry is process-wide.
func streamPIAL(t *testing.T) (account, pial string) {
	t.Helper()
	return fmt.Sprintf("a0000000-0000-4000-8000-%012x", uniqueStreamID(t)),
		fmt.Sprintf("b0000000-0000-4000-8000-%012x", uniqueStreamID(t))
}

var streamIDSeq int

func uniqueStreamID(t *testing.T) int {
	t.Helper()
	streamIDSeq++
	return streamIDSeq
}

// TestWriteSSEFrameCarriesID pins the field the whole resume story rests on.
// `id:` is the only thing an SSE client sends back after a drop; a frame
// written without one is a frame nobody can resume past.
func TestWriteSSEFrameCarriesID(t *testing.T) {
	var b strings.Builder
	writeSSEFrameID(&b, 7, "new_post", `{"work_id":"w1"}`)
	got := b.String()

	if !strings.HasPrefix(got, "id: 7\n") {
		t.Fatalf("id: must be the first field of the frame, got:\n%q", got)
	}
	if !strings.Contains(got, "event: new_post\n") {
		t.Fatalf("event name missing:\n%q", got)
	}
	if !strings.Contains(got, "data: {\"work_id\":\"w1\"}\n") {
		t.Fatalf("payload missing:\n%q", got)
	}
	if !strings.HasSuffix(got, "\n\n") {
		t.Fatalf("frame must end with the blank line that dispatches it:\n%q", got)
	}

	// A multi-line payload still gets one data: line each, id or no id.
	b.Reset()
	writeSSEFrameID(&b, 2, "notify", "<div>\n  <b>1</b>\n</div>")
	if n := strings.Count(b.String(), "data: "); n != 3 {
		t.Fatalf("want one data: line per payload line, got %d in:\n%q", n, b.String())
	}

	// The unnumbered form is the connect-time snapshot: no id, so a client
	// cannot resume from a position no other connection shares.
	b.Reset()
	writeSSEFrame(&b, "notify", `{"unread":3}`)
	if strings.Contains(b.String(), "id:") {
		t.Fatalf("snapshot frame must carry no id:\n%q", b.String())
	}
}

// recvLive reads one event from a session or fails.
func recvLive(t *testing.T, ch <-chan SSEEvent) SSEEvent {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(time.Second):
		t.Fatal("event was never delivered")
		return SSEEvent{}
	}
}

// TestStreamSequenceAndReplay is the gap contract: numbers increase, and a
// reconnect quoting a number is handed everything after it and nothing else.
func TestStreamSequenceAndReplay(t *testing.T) {
	account, pial := streamPIAL(t)
	ch, _, cleanup := RegisterSSESession(account, pial)
	defer cleanup()

	for i := 1; i <= 3; i++ {
		PublishToUser(pial, SSEEvent{Type: "new_post", JSON: fmt.Sprintf(`{"work_id":"w%d"}`, i)})
	}
	var ids []int64
	for i := 0; i < 3; i++ {
		ev := recvLive(t, ch)
		if len(ids) > 0 && ev.ID <= ids[len(ids)-1] {
			t.Fatalf("event %d arrived numbered %d after %d: numbers must increase", i, ev.ID, ids[len(ids)-1])
		}
		ids = append(ids, ev.ID)
	}

	missed, ok := ReplayAfter(account, pial, ids[0])
	if !ok {
		t.Fatal("a resume point inside the log must be honoured")
	}
	if len(missed) != 2 || missed[0].ID != ids[1] || missed[1].ID != ids[2] {
		t.Fatalf("replay must be oldest-first and gapless, got %v", missed)
	}
	if missed[0].Event.JSON != `{"work_id":"w2"}` {
		t.Fatalf("replayed frame lost its payload: %q", missed[0].Event.JSON)
	}

	// Caught up: honoured, and nothing to say.
	if frames, ok := ReplayAfter(account, pial, ids[2]); !ok || len(frames) != 0 {
		t.Fatalf("a client already at the head must be told so, got ok=%v frames=%d", ok, len(frames))
	}

	// A number no replica could have issued is not a resume point.
	if _, ok := ReplayAfter(account, pial, time.Now().Add(time.Hour).UnixMicro()); ok {
		t.Fatal("a resume point in the future must be refused")
	}
	if _, ok := ReplayAfter(account, pial, 0); ok {
		t.Fatal("no resume point is not a resume point")
	}
}

// TestReplayRefusesBelowTheFloor pins the honest failure: a client whose gap
// starts before what this replica can vouch for must re-read, not believe a
// partial replay was the whole of it. A restart produces exactly this.
func TestReplayRefusesBelowTheFloor(t *testing.T) {
	account, pial := streamPIAL(t)
	before := nextLiveID()
	liveEvents.raiseFloor(nextLiveID())
	if _, ok := ReplayAfter(account, pial, before); ok {
		t.Fatal("a gap older than the floor must be refused")
	}
}

// TestPublishRecordsWithoutAListener pins why the log is not per session: the
// moment a person has no connection is the moment they are about to reconnect.
// That holds for account events too, which is how an offline message is replayed.
func TestPublishRecordsWithoutAListener(t *testing.T) {
	account, pial := streamPIAL(t)
	start := nextLiveID()
	PublishToUser(pial, SSEEvent{Type: "notify", JSON: `{"unread":1}`})
	PublishToAccount(account, SSEEvent{Type: "gnosis_message", JSON: `{"id":"m"}`})
	frames, ok := ReplayAfter(account, pial, start)
	if !ok || len(frames) != 2 {
		t.Fatalf("events for a disconnected person must be replayable, got ok=%v %v", ok, frames)
	}
}

// TestBulkEventsCannotCrowdOutOthers pins the two buffers: a burst of high-volume
// events must not cost a person an update addressed to them.
func TestBulkEventsCannotCrowdOutOthers(t *testing.T) {
	account, pial := streamPIAL(t)
	ch, _, cleanup := RegisterSSESession(account, pial)
	defer cleanup()
	for i := 0; i < sseBufferSize*2; i++ {
		PublishToUser(pial, SSEEvent{Type: "new_post", Data: "p"})
	}
	PublishToUser(pial, SSEEvent{Type: "balance", Data: "1"})

	for i := 0; i < sseBufferSize+3; i++ {
		if recvLive(t, ch).Type == "balance" {
			return
		}
	}
	t.Fatal("an update addressed to the person was lost behind a burst of bulk events")
}

// TestDroppedDeliveryAsksForResync pins that a lost event is not a silent gap.
func TestDroppedDeliveryAsksForResync(t *testing.T) {
	account, pial := streamPIAL(t)
	ch, _, cleanup := RegisterSSESession(account, pial)
	defer cleanup()
	for i := 0; i < sseBufferSize+5; i++ {
		PublishToUser(pial, SSEEvent{Type: "new_post", Data: "p"})
	}
	for i := 0; i < sseBufferSize+5; i++ {
		if recvLive(t, ch).Type == sseResync {
			return
		}
	}
	t.Fatal("a dropped delivery must tell the client to re-read")
}

// TestFullConnectionDropsAreCounted pins that a delivery lost to a full buffer
// is on the record. Silence here was the reason a dropped push looked like a
// server that never sent one.
func TestFullConnectionDropsAreCounted(t *testing.T) {
	account, pial := streamPIAL(t)
	_, _, cleanup := RegisterSSESession(account, pial)
	defer cleanup()

	before := sseDropCount.Load()
	// The buffer holds sseBufferSize and nothing is reading it.
	for i := 0; i < 70; i++ {
		PublishToUser(pial, SSEEvent{Type: "new_post", JSON: `{"work_id":"w"}`})
	}
	if got := sseDropCount.Load() - before; got == 0 {
		t.Fatal("a delivery dropped on a full connection must be counted, not discarded silently")
	}
}

// TestNewPostFanoutSkipsReplies is the rule the following feed has always had
// and the stream never did: a reply is not a post. Fanning one out put a card
// on every follower's screen that the feed itself would never show them.
func TestNewPostFanoutSkipsReplies(t *testing.T) {
	if newPostFanoutKind("reply") {
		t.Fatal("a reply must never be announced as a new post")
	}
	for _, kind := range []string{"post", "quote", "poll", "video", "voice", "thread_post", "react_video"} {
		if !newPostFanoutKind(kind) {
			t.Fatalf("%s is a feed item and must be announced", kind)
		}
	}
}

// TestUserStreamWhitelist pins what the app's stream forwards. A kind the app
// cannot decode is noise; a kind with no structured twin is a rendered fragment
// meant for the web, and handing it to the app is handing it markup.
func TestUserStreamWhitelist(t *testing.T) {
	for _, kind := range []string{
		"notify", "new_post", "post_deleted", "balance", "live_start",
		"gnosis_message", "work_engagement", "frequency_start",
	} {
		if !apiStreamable(SSEEvent{Type: kind, JSON: "{}"}) {
			t.Fatalf("%s is in the app's contract and must be forwarded", kind)
		}
	}
	if apiStreamable(SSEEvent{Type: "work_engagement", Data: "<div/>"}) {
		t.Fatal("an event with no twin must not reach a native client")
	}
	if apiStreamable(SSEEvent{Type: "price_update", JSON: "{}"}) {
		t.Fatal("a kind the app does not decode must not be forwarded")
	}
}

// TestLastEventIDHeader pins the resume point's reading. Anything that is not a
// positive number is no resume point, and must not be mistaken for id 0.
func TestLastEventIDHeader(t *testing.T) {
	cases := map[string]int64{"": 0, "12": 12, " 12 ": 12, "abc": 0, "-3": 0, "0": 0}
	for header, want := range cases {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
		if header != "" {
			r.Header.Set("Last-Event-ID", header)
		}
		if got := lastEventID(r); got != want {
			t.Fatalf("Last-Event-ID %q read as %d, want %d", header, got, want)
		}
	}
}

// notifReadRequest posts one notif_read the way the apps do: a JSON body, no
// form encoding anywhere.
func notifReadRequest(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: injectionSessionToken})
	rec := httptest.NewRecorder()
	h.eventsPost(rec, req)
	return rec
}

// TestNotifReadAcceptsJSONBody pins the lane the app talks on. notif_read read
// r.FormValue("notif_id") and nothing else, so every read receipt the app sent
// was a 204 that marked nothing — the badge stayed up and came back on the next
// launch, which is what a read receipt exists to prevent.
func TestNotifReadAcceptsJSONBody(t *testing.T) {
	h := authedHandler(t, "", &model.User{
		ID:     "2f6c1d0e-9b3a-4c8d-8e7f-6a5b4c3d2e1f",
		Handle: "dev",
		PIALID: "7b2c8a2e-1c7d-4d5e-9a0b-0a1b2c3d4e5f",
	})

	// No id at all is the one refusal: without one there is nothing to mark.
	if rec := notifReadRequest(t, h, `{"event_type":"notif_read"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("notif_read with no id must be refused, got %d", rec.Code)
	}

	// With an id in the JSON body the field is found and the handler proceeds
	// to the rows. It cannot reach them here — this package has no database —
	// so what is pinned is that it got past reading the body, which is exactly
	// where it used to stop.
	for _, body := range []string{
		`{"event_type":"notif_read","notif_id":"b1000000-0001-4a00-9c11-000000000001"}`,
		`{"event_type":"notif_read","id":"b1000000-0001-4a00-9c11-000000000001"}`,
	} {
		rec := notifReadRequest(t, h, body)
		if rec.Code == http.StatusBadRequest {
			t.Fatalf("notif_read did not read the id out of %s", body)
		}
		if rec.Code == http.StatusUnauthorized {
			t.Fatalf("notif_read rejected an authenticated caller: %s", rec.Body.String())
		}
	}
}
