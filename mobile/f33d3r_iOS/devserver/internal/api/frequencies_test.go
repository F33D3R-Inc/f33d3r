package api

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The Frequencies suite. Every case here is a rule from the brain's domain
// module — the lifecycle table, the authorization matrix, the admission
// decision — asked through the wire the app actually speaks.

func (h *harness) frequencyRoom(token, id string) FrequencyRoomDTO {
	h.t.Helper()
	status, body := h.do("GET", "/api/v1/frequencies/"+id, token, nil, "")
	if status != 200 {
		h.t.Fatalf("room %s: %d %s", id, status, body)
	}
	var room FrequencyRoomDTO
	if err := json.Unmarshal(body, &room); err != nil {
		h.t.Fatalf("room %s: %v", id, err)
	}
	return room
}

func (h *harness) frequencyList(token, lane string) FrequencyListDTO {
	h.t.Helper()
	status, body := h.do("GET", "/api/v1/frequencies?lane="+lane, token, nil, "")
	if status != 200 {
		h.t.Fatalf("lane %s: %d %s", lane, status, body)
	}
	var list FrequencyListDTO
	if err := json.Unmarshal(body, &list); err != nil {
		h.t.Fatalf("lane %s: %v", lane, err)
	}
	return list
}

// seededLive is the room @miiyazuko is hosting in the seed.
func (h *harness) seededLive(token string) FrequencySummaryDTO {
	h.t.Helper()
	list := h.frequencyList(token, "live")
	if len(list.Frequencies) != 1 {
		h.t.Fatalf("expected one live frequency, got %d", len(list.Frequencies))
	}
	return list.Frequencies[0]
}

// TestFrequencyLanes: the four discovery lanes, and the counts that come from
// rows rather than from a number somebody typed.
func TestFrequencyLanes(t *testing.T) {
	h := newHarness(t)
	devToken, _ := h.login("dev")
	miiToken, _ := h.login("miiyazuko")

	live := h.frequencyList(devToken, "live")
	if live.Lane != "live" || live.Count != 1 {
		t.Fatalf("live lane = %+v", live)
	}
	f := live.Frequencies[0]
	if f.Host.Handle != "miiyazuko" || f.State != "live" {
		t.Errorf("live room = %+v", f)
	}
	// host + cohost + one speaker on the stage, two listeners in the room.
	if f.SpeakerCount != 3 || f.ListenerCount != 2 || f.ParticipantCount != 5 {
		t.Errorf("counts = %d speakers / %d listeners / %d total", f.SpeakerCount, f.ListenerCount, f.ParticipantCount)
	}
	if f.AudioUnavailable == nil || *f.AudioUnavailable == "" {
		t.Errorf("a live room with no audio path must say so: %+v", f.AudioUnavailable)
	}
	if f.StartedAt == nil || f.EndedAt != nil || f.EndReason != nil {
		t.Errorf("live room timestamps = %+v", f)
	}

	scheduled := h.frequencyList(devToken, "scheduled")
	if scheduled.Count != 1 || scheduled.Frequencies[0].Host.Handle != "dev" {
		t.Fatalf("scheduled lane = %+v", scheduled)
	}
	if s := scheduled.Frequencies[0]; s.ScheduledAt == nil || !s.ScheduledAt.After(time.Now()) {
		t.Errorf("scheduled_at = %v", s.ScheduledAt)
	}
	if scheduled.Frequencies[0].AudioUnavailable != nil {
		t.Errorf("a room that has not started has no audio to be missing")
	}

	// `mine` is what the caller hosts, in any state; `own` is only what is
	// open right now, so the UI can offer "Return to your Frequency".
	mine := h.frequencyList(devToken, "mine")
	if mine.Count != 1 || mine.Frequencies[0].State != "scheduled" {
		t.Errorf("dev's mine lane = %+v", mine)
	}
	if mine.Own != nil {
		t.Errorf("dev has nothing open, own = %+v", mine.Own)
	}
	if own := h.frequencyList(miiToken, "live").Own; own == nil || own.ID != f.ID {
		t.Errorf("miiyazuko's own room = %+v", own)
	}
	if ended := h.frequencyList(devToken, "ended"); ended.Count != 0 {
		t.Errorf("nothing has ended yet: %+v", ended)
	}
	if status, _ := h.do("GET", "/api/v1/frequencies?lane=nowhere", devToken, nil, ""); status != 400 {
		t.Errorf("unknown lane: %d", status)
	}
}

// TestFrequencyJoinAndQueue: tuning in, the viewer block the client renders
// its controls from, and the microphone queue only moderators can read.
func TestFrequencyJoinAndQueue(t *testing.T) {
	h := newHarness(t)
	guestToken, _ := h.login("guest")
	tbToken, _ := h.login("tehanibentley")
	id := h.seededLive(guestToken).ID

	status, body := h.do("POST", "/events", guestToken, map[string]string{"event_type": "frequency_join", "frequency_id": id}, "")
	if status != 200 {
		t.Fatalf("frequency_join: %d %s", status, body)
	}
	var room FrequencyRoomDTO
	json.Unmarshal(body, &room)
	v := room.Viewer
	if v == nil || v.Role == nil || *v.Role != "listener" {
		t.Fatalf("guest joined as %+v", v)
	}
	if !v.CanListen || v.CanSpeak || v.CanModerate || v.CanEnd {
		t.Errorf("a listener may only listen: %+v", v)
	}
	// The seed left a question in the queue, so the hand is already up.
	if v.Request == nil || v.Request.Reason != "got a question about the drop" {
		t.Fatalf("guest's seeded request = %+v", v.Request)
	}
	if v.CanRequestMic {
		t.Errorf("a listener with a hand up cannot raise it again")
	}
	// The queue is the moderators': a listener never sees who else is asking.
	if len(room.Requests) != 0 {
		t.Errorf("listener sees the queue: %+v", room.Requests)
	}
	modRoom := h.frequencyRoom(tbToken, id)
	if len(modRoom.Requests) != 1 || modRoom.Requests[0].Author.Handle != "guest" {
		t.Fatalf("co-host's queue = %+v", modRoom.Requests)
	}
	if modRoom.Viewer == nil || !modRoom.Viewer.CanModerate || modRoom.Viewer.CanEnd {
		t.Errorf("a co-host moderates but does not end: %+v", modRoom.Viewer)
	}

	// Taking the hand down puts Raise Hand back.
	status, body = h.do("POST", "/events", guestToken, map[string]string{
		"event_type": "frequency_request_withdraw", "frequency_id": id, "request_id": v.Request.ID,
	}, "")
	if status != 204 {
		t.Fatalf("withdraw: %d %s", status, body)
	}
	after := h.frequencyRoom(guestToken, id).Viewer
	if after.Request != nil || !after.CanRequestMic {
		t.Errorf("after withdrawing: %+v", after)
	}

	// The stage is host first, then co-hosts, then speakers, and the muted
	// speaker says so.
	got := []string{}
	for _, sp := range modRoom.Speakers {
		got = append(got, sp.Author.Handle+":"+sp.Role)
		if sp.Author.Handle == "admin" && !sp.Muted {
			t.Errorf("the seeded speaker is muted")
		}
		if !sp.Present {
			t.Errorf("%s is joined and must read as present", sp.Author.Handle)
		}
	}
	want := "miiyazuko:host tehanibentley:cohost admin:speaker"
	if strings.Join(got, " ") != want {
		t.Errorf("stage order = %q, want %q", strings.Join(got, " "), want)
	}
}

// TestFrequencyRequestApproval: a hand goes up, a co-host calls them to the
// microphone, and the room fills.
func TestFrequencyRequestApproval(t *testing.T) {
	h := newHarness(t)
	devToken, _ := h.login("dev")
	guestToken, _ := h.login("guest")
	tbToken, _ := h.login("tehanibentley")
	id := h.seededLive(devToken).ID

	status, body := h.do("POST", "/events", devToken, map[string]string{
		"event_type": "frequency_request_mic", "frequency_id": id, "reason": "one thing about the fourth frame",
	}, "")
	if status != 201 {
		t.Fatalf("request_mic: %d %s", status, body)
	}
	var req FrequencyRequestDTO
	json.Unmarshal(body, &req)
	if req.Author.Handle != "dev" || req.Upvotes != 0 {
		t.Fatalf("request = %+v", req)
	}

	// The room can vote a question up the queue, but not its own.
	status, body = h.do("POST", "/events", devToken, map[string]any{
		"event_type": "frequency_request_upvote", "frequency_id": id, "request_id": req.ID,
	}, "")
	if status != 409 || !strings.Contains(string(body), "own_request") {
		t.Errorf("upvoting your own request: %d %s", status, body)
	}
	status, body = h.do("POST", "/events", guestToken, map[string]any{
		"event_type": "frequency_request_upvote", "frequency_id": id, "request_id": req.ID,
	}, "")
	var vote struct {
		Counted bool `json:"counted"`
		Upvotes int  `json:"upvotes"`
	}
	json.Unmarshal(body, &vote)
	if status != 200 || !vote.Counted || vote.Upvotes != 1 {
		t.Fatalf("upvote: %d %s", status, body)
	}
	// Twice from the same person is once.
	_, body = h.do("POST", "/events", guestToken, map[string]any{
		"event_type": "frequency_request_upvote", "frequency_id": id, "request_id": req.ID,
	}, "")
	json.Unmarshal(body, &vote)
	if vote.Counted || vote.Upvotes != 1 {
		t.Errorf("second upvote from the same person: %s", body)
	}

	// A listener cannot answer the queue.
	status, body = h.do("POST", "/events", guestToken, map[string]any{
		"event_type": "frequency_request_approve", "frequency_id": id, "request_id": req.ID,
	}, "")
	if status != 403 {
		t.Errorf("listener approving: %d %s", status, body)
	}

	// The co-host can.
	status, body = h.do("POST", "/events", tbToken, map[string]any{
		"event_type": "frequency_request_approve", "frequency_id": id, "request_id": req.ID,
	}, "")
	if status != 200 {
		t.Fatalf("approve: %d %s", status, body)
	}
	v := h.frequencyRoom(devToken, id).Viewer
	if v.Role == nil || *v.Role != "speaker" || !v.CanSpeak || v.CanRequestMic {
		t.Fatalf("dev after approval = %+v", v)
	}
	f := h.seededLive(devToken)
	if f.SpeakerCount != 4 || f.ListenerCount != 1 {
		t.Errorf("counts after approval = %d speakers / %d listeners", f.SpeakerCount, f.ListenerCount)
	}

	// max_speakers is 4 in the seed: the next approval has nowhere to put them.
	status, body = h.do("POST", "/events", guestToken, map[string]string{
		"event_type": "frequency_request_mic", "frequency_id": id, "reason": "one more",
	}, "")
	if status != 201 {
		t.Fatalf("second request_mic: %d %s", status, body)
	}
	var second FrequencyRequestDTO
	json.Unmarshal(body, &second)
	status, body = h.do("POST", "/events", tbToken, map[string]any{
		"event_type": "frequency_request_approve", "frequency_id": id, "request_id": second.ID,
	}, "")
	if status != 409 || !strings.Contains(string(body), "speakers_full") {
		t.Errorf("approving into a full stage: %d %s", status, body)
	}

	// Declining answers the room and leaves the queue empty.
	status, body = h.do("POST", "/events", tbToken, map[string]any{
		"event_type": "frequency_request_decline", "frequency_id": id, "request_id": second.ID,
	}, "")
	if status != 200 {
		t.Fatalf("decline: %d %s", status, body)
	}
	if q := h.frequencyRoom(tbToken, id).Requests; len(q) != 0 {
		t.Errorf("queue after decline = %+v", q)
	}
}

// TestFrequencyAuthority: the matrix, on the wire. A co-host moderates but
// does not end, delegate, lock or toggle the queue, and nobody acts upward.
func TestFrequencyAuthority(t *testing.T) {
	h := newHarness(t)
	tbToken, _ := h.login("tehanibentley")
	miiToken, _ := h.login("miiyazuko")
	guestToken, _ := h.login("guest")
	id := h.seededLive(tbToken).ID

	for _, ev := range []map[string]any{
		{"event_type": "frequency_end", "frequency_id": id},
		{"event_type": "frequency_lock", "frequency_id": id, "locked": true},
		{"event_type": "frequency_requests_open", "frequency_id": id, "open": false},
		{"event_type": "frequency_cohost", "frequency_id": id, "handle": "guest", "cohost": true},
	} {
		status, body := h.do("POST", "/events", tbToken, ev, "")
		if status != 403 {
			t.Errorf("co-host %s: %d %s", ev["event_type"], status, body)
		}
	}

	// A co-host may mute a speaker below them, and may not touch the host.
	status, body := h.do("POST", "/events", tbToken, map[string]any{
		"event_type": "frequency_mute", "frequency_id": id, "handle": "admin", "muted": false,
	}, "")
	if status != 200 {
		t.Fatalf("co-host unmuting a speaker: %d %s", status, body)
	}
	status, body = h.do("POST", "/events", tbToken, map[string]any{
		"event_type": "frequency_remove", "frequency_id": id, "handle": "miiyazuko",
	}, "")
	if status != 409 || !strings.Contains(string(body), "is_host") {
		t.Errorf("co-host removing the host: %d %s", status, body)
	}
	// A listener moderates nothing.
	status, body = h.do("POST", "/events", guestToken, map[string]any{
		"event_type": "frequency_mute", "frequency_id": id, "handle": "admin", "muted": true,
	}, "")
	if status != 403 {
		t.Errorf("listener muting a speaker: %d %s", status, body)
	}

	// The host takes the co-host badge back, and it demotes to speaker rather
	// than throwing them off the stage.
	status, body = h.do("POST", "/events", miiToken, map[string]any{
		"event_type": "frequency_cohost", "frequency_id": id, "handle": "tehanibentley", "cohost": false,
	}, "")
	if status != 200 {
		t.Fatalf("uncohost: %d %s", status, body)
	}
	var room FrequencyRoomDTO
	json.Unmarshal(body, &room)
	if len(room.Cohosts) != 0 {
		t.Errorf("co-hosts after uncohost = %+v", room.Cohosts)
	}
	v := h.frequencyRoom(tbToken, id).Viewer
	if v.Role == nil || *v.Role != "speaker" || v.CanModerate {
		t.Errorf("former co-host = %+v", v)
	}
	if h.seededLive(tbToken).SpeakerCount != 3 {
		t.Errorf("uncohost must not empty the stage")
	}
}

// TestFrequencyLockAndAdmission: a locked room keeps listeners out and lets
// the host back in, and a blocked person is refused before anything else.
func TestFrequencyLockAndAdmission(t *testing.T) {
	h := newHarness(t)
	miiToken, _ := h.login("miiyazuko")
	id := h.seededLive(miiToken).ID

	// Somebody who has never been in the room.
	status, body := h.do("POST", "/api/v1/auth/signup", "", map[string]string{
		"handle": "newcomer", "password": "f33d3rdev", "display_name": "Newcomer",
	}, "")
	if status != 201 && status != 200 {
		t.Fatalf("signup: %d %s", status, body)
	}
	var session struct {
		Token string `json:"token"`
	}
	json.Unmarshal(body, &session)
	newToken := session.Token

	status, body = h.do("POST", "/events", miiToken, map[string]any{
		"event_type": "frequency_lock", "frequency_id": id, "locked": true,
	}, "")
	if status != 200 {
		t.Fatalf("lock: %d %s", status, body)
	}
	var room FrequencyRoomDTO
	json.Unmarshal(body, &room)
	if !room.Frequency.Locked {
		t.Errorf("room did not lock: %+v", room.Frequency)
	}

	status, body = h.do("POST", "/events", newToken, map[string]string{"event_type": "frequency_join", "frequency_id": id}, "")
	if status != 403 || !strings.Contains(string(body), `"locked"`) {
		t.Errorf("listener joining a locked room: %d %s", status, body)
	}
	// The host coming back is what unlocks a stuck session, so the host is
	// never refused by their own lock.
	if status, body := h.do("POST", "/events", miiToken, map[string]string{"event_type": "frequency_leave", "frequency_id": id}, ""); status != 204 {
		t.Fatalf("host leave: %d %s", status, body)
	}
	if status, body := h.do("POST", "/events", miiToken, map[string]string{"event_type": "frequency_join", "frequency_id": id}, ""); status != 200 {
		t.Errorf("host rejoining their locked room: %d %s", status, body)
	}

	// Unlock, then block: blocked is refused before lock or capacity.
	h.do("POST", "/events", miiToken, map[string]any{"event_type": "frequency_lock", "frequency_id": id, "locked": false}, "")
	status, body = h.do("POST", "/events", miiToken, map[string]any{
		"event_type": "frequency_block", "frequency_id": id, "handle": "guest", "reason": "spam",
	}, "")
	if status != 200 {
		t.Fatalf("block: %d %s", status, body)
	}
	guestToken, _ := h.login("guest")
	status, body = h.do("POST", "/events", guestToken, map[string]string{"event_type": "frequency_join", "frequency_id": id}, "")
	if status != 403 || !strings.Contains(string(body), `"blocked"`) {
		t.Errorf("blocked join: %d %s", status, body)
	}
	if status, body := h.do("POST", "/events", miiToken, map[string]any{
		"event_type": "frequency_unblock", "frequency_id": id, "handle": "guest",
	}, ""); status != 200 {
		t.Fatalf("unblock: %d %s", status, body)
	}
	if status, body := h.do("POST", "/events", guestToken, map[string]string{"event_type": "frequency_join", "frequency_id": id}, ""); status != 200 {
		t.Errorf("join after unblock: %d %s", status, body)
	}
}

// TestFrequencyStream: the room's SSE — the opening frame, the frames a
// mutation pushes, and the state that closes it.
func TestFrequencyStream(t *testing.T) {
	h := newHarness(t)
	guestToken, _ := h.login("guest")
	miiToken, _ := h.login("miiyazuko")
	devToken, _ := h.login("dev")
	id := h.seededLive(guestToken).ID

	req, _ := http.NewRequest("GET", h.srv.URL+"/api/v1/frequencies/"+id+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+guestToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("stream: %d", res.StatusCode)
	}
	frames := make(chan [2]string, 64)
	go func() {
		sc := bufio.NewScanner(res.Body)
		var ev string
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "event: ") {
				ev = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				frames <- [2]string{ev, strings.TrimPrefix(line, "data: ")}
			}
		}
	}()
	next := func(want string) string {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case f := <-frames:
				if f[0] == want {
					return f[1]
				}
			case <-deadline:
				t.Fatalf("no %s frame", want)
			}
		}
	}

	// The first frame is the whole room, rendered for this reader: a listener
	// must not be handed the host's controls.
	first := next("room")
	var opening FrequencyRoomDTO
	if err := json.Unmarshal([]byte(first), &opening); err != nil {
		t.Fatalf("opening frame: %v", err)
	}
	if opening.Viewer == nil || opening.Viewer.CanEnd || opening.Frequency.ID != id {
		t.Fatalf("opening frame = %s", first)
	}

	// A speaker leaving the stage, and the counts that follow.
	if status, body := h.do("POST", "/events", miiToken, map[string]any{
		"event_type": "frequency_demote", "frequency_id": id, "handle": "admin",
	}, ""); status != 200 {
		t.Fatalf("demote: %d %s", status, body)
	}
	if left := next("speaker_left"); !strings.Contains(left, `"handle":"admin"`) {
		t.Errorf("speaker_left = %s", left)
	}
	if counts := next("counts"); !strings.Contains(counts, `"speakers":2`) {
		t.Errorf("counts = %s", counts)
	}

	// A mute is its own frame.
	if status, body := h.do("POST", "/events", miiToken, map[string]any{
		"event_type": "frequency_mute", "frequency_id": id, "handle": "tehanibentley", "muted": true,
	}, ""); status != 200 {
		t.Fatalf("mute: %d %s", status, body)
	}
	if m := next("muted"); !strings.Contains(m, `"handle":"tehanibentley"`) || !strings.Contains(m, `"muted":true`) {
		t.Errorf("muted = %s", m)
	}

	// Closing the queue is a flags frame.
	if status, body := h.do("POST", "/events", miiToken, map[string]any{
		"event_type": "frequency_requests_open", "frequency_id": id, "open": false,
	}, ""); status != 200 {
		t.Fatalf("requests_open: %d %s", status, body)
	}
	if fl := next("flags"); !strings.Contains(fl, `"requests_open":false`) {
		t.Errorf("flags = %s", fl)
	}

	// And a hand going up reaches the moderators only. dev is a listener on
	// this stream's room; the frame must not arrive here.
	h.do("POST", "/events", miiToken, map[string]any{"event_type": "frequency_requests_open", "frequency_id": id, "open": true}, "")
	next("flags")
	if status, body := h.do("POST", "/events", devToken, map[string]string{
		"event_type": "frequency_request_mic", "frequency_id": id, "reason": "quick one",
	}, ""); status != 201 {
		t.Fatalf("request_mic: %d %s", status, body)
	}
	drain := time.After(300 * time.Millisecond)
	for done := false; !done; {
		select {
		case f := <-frames:
			if f[0] == "request" {
				t.Errorf("a listener was sent the microphone queue: %s", f[1])
			}
		case <-drain:
			done = true
		}
	}

	// Ending closes the room, and the reason travels with it.
	if status, body := h.do("POST", "/events", miiToken, map[string]any{"event_type": "frequency_end", "frequency_id": id}, ""); status != 200 {
		t.Fatalf("end: %d %s", status, body)
	}
	st := next("state")
	if !strings.Contains(st, `"state":"ended"`) || !strings.Contains(st, `"end_reason":"host_ended"`) {
		t.Errorf("state frame = %s", st)
	}

	// After the end: nothing live, the room is in the ended lane, and joining
	// is refused by state.
	if list := h.frequencyList(guestToken, "live"); list.Count != 0 {
		t.Errorf("ended room still live: %+v", list)
	}
	if list := h.frequencyList(guestToken, "ended"); list.Count != 1 || list.Frequencies[0].ID != id {
		t.Errorf("ended lane = %+v", list)
	}
	status, body := h.do("POST", "/events", guestToken, map[string]string{"event_type": "frequency_join", "frequency_id": id}, "")
	if status != 409 || !strings.Contains(string(body), "not_live") {
		t.Errorf("joining an ended room: %d %s", status, body)
	}
	// A new session is a new Frequency: the old one never starts again.
	status, body = h.do("POST", "/events", miiToken, map[string]any{"event_type": "frequency_start", "frequency_id": id}, "")
	if status != 409 || !strings.Contains(string(body), `"over"`) {
		t.Errorf("restarting an ended room: %d %s", status, body)
	}
}

// TestFrequencyLifecycle: create, schedule, start, and the one-open-room rule
// — plus the followers who are told when a Frequency begins.
func TestFrequencyLifecycle(t *testing.T) {
	h := newHarness(t)
	devToken, _ := h.login("dev")
	tbToken, _ := h.login("tehanibentley")

	status, body := h.do("POST", "/events", devToken, map[string]any{
		"event_type": "frequency_create", "title": "Office hours", "description": "Ask anything.",
		"max_speakers": 3,
	}, "")
	if status != 201 {
		t.Fatalf("create: %d %s", status, body)
	}
	var created FrequencySummaryDTO
	json.Unmarshal(body, &created)
	if created.State != "draft" || created.Host.Handle != "dev" || created.MaxSpeakers != 3 {
		t.Fatalf("created = %+v", created)
	}
	if created.RequestsOpen != true || created.Locked != false {
		t.Errorf("defaults = %+v", created)
	}

	// Scheduling and un-scheduling walk the transition table both ways.
	at := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	status, body = h.do("POST", "/events", devToken, map[string]any{
		"event_type": "frequency_schedule", "frequency_id": created.ID, "scheduled_at": at,
	}, "")
	if status != 200 {
		t.Fatalf("schedule: %d %s", status, body)
	}
	var room FrequencyRoomDTO
	json.Unmarshal(body, &room)
	if room.Frequency.State != "scheduled" || room.Frequency.ScheduledAt == nil {
		t.Fatalf("scheduled = %+v", room.Frequency)
	}
	status, body = h.do("POST", "/events", devToken, map[string]any{
		"event_type": "frequency_schedule", "frequency_id": created.ID, "scheduled_at": "",
	}, "")
	json.Unmarshal(body, &room)
	if status != 200 || room.Frequency.State != "draft" || room.Frequency.ScheduledAt != nil {
		t.Fatalf("unscheduled = %d %+v", status, room.Frequency)
	}

	// An update changes one field and leaves the rest.
	status, body = h.do("POST", "/events", devToken, map[string]any{
		"event_type": "frequency_update", "frequency_id": created.ID, "title": "Office hours, take two",
	}, "")
	json.Unmarshal(body, &room)
	if status != 200 || room.Frequency.Title != "Office hours, take two" ||
		room.Frequency.Description == nil || *room.Frequency.Description != "Ask anything." {
		t.Fatalf("update = %d %+v", status, room.Frequency)
	}
	// Nobody else's to edit.
	if status, _ := h.do("POST", "/events", tbToken, map[string]any{
		"event_type": "frequency_update", "frequency_id": created.ID, "title": "mine now",
	}, ""); status != 403 {
		t.Errorf("someone else's Frequency: %d", status)
	}

	// Starting joins the host and tells their followers.
	status, body = h.do("POST", "/events", devToken, map[string]any{"event_type": "frequency_start", "frequency_id": created.ID}, "")
	if status != 200 {
		t.Fatalf("start: %d %s", status, body)
	}
	json.Unmarshal(body, &room)
	if room.Frequency.State != "live" || room.Frequency.StartedAt == nil {
		t.Fatalf("started = %+v", room.Frequency)
	}
	if len(room.Speakers) != 1 || room.Speakers[0].Author.Handle != "dev" || room.Speakers[0].Role != "host" {
		t.Fatalf("the host is on the stage: %+v", room.Speakers)
	}
	if room.Viewer == nil || !room.Viewer.CanEnd || !room.Viewer.CanModerate {
		t.Errorf("host viewer = %+v", room.Viewer)
	}
	_, body = h.do("GET", "/api/v1/notifications", tbToken, nil, "")
	if !strings.Contains(string(body), `"kind":"frequency_start"`) {
		t.Errorf("follower not told a Frequency started: %s", body)
	}

	// One open Frequency per host: the scheduled one cannot start on top of it.
	scheduled := h.frequencyList(devToken, "scheduled")
	if len(scheduled.Frequencies) != 1 {
		t.Fatalf("expected the seeded scheduled room: %+v", scheduled)
	}
	status, body = h.do("POST", "/events", devToken, map[string]any{
		"event_type": "frequency_start", "frequency_id": scheduled.Frequencies[0].ID,
	}, "")
	if status != 409 || !strings.Contains(string(body), "host_already_live") {
		t.Errorf("second open room: %d %s", status, body)
	}
	// It can be called off, though, and cancelling twice says so.
	status, body = h.do("POST", "/events", devToken, map[string]any{
		"event_type": "frequency_cancel", "frequency_id": scheduled.Frequencies[0].ID,
	}, "")
	if status != 200 {
		t.Fatalf("cancel: %d %s", status, body)
	}
	status, body = h.do("POST", "/events", devToken, map[string]any{
		"event_type": "frequency_cancel", "frequency_id": scheduled.Frequencies[0].ID,
	}, "")
	if status != 409 || !strings.Contains(string(body), "already_cancelled") {
		t.Errorf("cancelling twice: %d %s", status, body)
	}
}

// TestFrequencyNoIdentityLeak: the room is a public surface, and a PIAL is
// never public. Every identity on it is a handle.
func TestFrequencyNoIdentityLeak(t *testing.T) {
	h := newHarness(t)
	miiToken, me := h.login("miiyazuko")
	guestToken, _ := h.login("guest")
	id := h.seededLive(miiToken).ID

	rows, err := h.store.DB().Query(`SELECT pial_id FROM pial_roots`)
	if err != nil {
		t.Fatal(err)
	}
	var pials []string
	for rows.Next() {
		var p string
		rows.Scan(&p)
		pials = append(pials, p)
	}
	rows.Close()
	if me["pial_id"] == nil {
		t.Fatal("/me must carry pial_id")
	}

	for _, token := range []string{miiToken, guestToken} {
		for _, path := range []string{"/api/v1/frequencies?lane=live", "/api/v1/frequencies/" + id} {
			status, body := h.do("GET", path, token, nil, "")
			if status != 200 {
				t.Fatalf("%s: %d %s", path, status, body)
			}
			lower := strings.ToLower(string(body))
			for _, forbidden := range []string{"jung", "archetype", "aesq", "shadow", "pial"} {
				if strings.Contains(lower, forbidden) {
					t.Errorf("%s: response contains %q", path, forbidden)
				}
			}
			for _, p := range pials {
				if strings.Contains(lower, p) {
					t.Errorf("%s: response contains a PIAL", path)
				}
			}
		}
	}
}
