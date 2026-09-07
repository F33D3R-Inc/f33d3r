package api

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestVisionTray: the tray shows only followed accounts, unseen before seen,
// marks seen, takes a private reply, and a new vision appears for followers.
func TestVisionTray(t *testing.T) {
	h := newHarness(t)
	devToken, _ := h.login("dev")
	guestToken, _ := h.login("guest")

	status, body := h.do("GET", "/api/v1/visions", devToken, nil, "")
	if status != 200 {
		t.Fatalf("visions: %d %s", status, body)
	}
	var tray VisionTrayDTO
	json.Unmarshal(body, &tray)
	if tray.Own != nil {
		t.Errorf("dev has no vision yet, own = %+v", tray.Own)
	}
	handles := map[string]VisionRingDTO{}
	for _, r := range tray.Rings {
		handles[r.Author.Handle] = r
	}
	// dev follows tehanibentley, miiyazuko, admin — all three seeded visions.
	for _, want := range []string{"tehanibentley", "miiyazuko", "admin"} {
		if _, ok := handles[want]; !ok {
			t.Errorf("tray lacks @%s: %s", want, body)
		}
	}
	if _, ok := handles["guest"]; ok {
		t.Errorf("tray shows an account dev does not follow")
	}
	if tray.Rings[0].Author.Handle != "tehanibentley" || !tray.Rings[0].IsLive {
		t.Errorf("live author should lead the tray: %+v", tray.Rings[0].Author)
	}
	for _, r := range tray.Rings {
		if r.State != "unseen" || r.Provenance == nil {
			t.Errorf("ring %s state=%s provenance=%v", r.Author.Handle, r.State, r.Provenance)
		}
		for _, f := range r.Visions {
			if f.ViewCount != nil {
				t.Errorf("view_count leaked to a non-owner")
			}
			if f.Artboard.TypeScale == "auto" {
				t.Errorf("type_scale must be resolved, got auto")
			}
		}
	}

	// Seeing the founder's two visions flips the ring.
	tb := handles["tehanibentley"]
	for _, f := range tb.Visions {
		if status, _ := h.do("POST", "/events", devToken, map[string]string{"event_type": "vision_seen", "vision_id": f.ID}, ""); status != 204 {
			t.Fatalf("vision_seen: %d", status)
		}
	}
	_, body = h.do("GET", "/api/v1/visions", devToken, nil, "")
	json.Unmarshal(body, &tray)
	for _, r := range tray.Rings {
		if r.Author.Handle == "tehanibentley" && r.State != "seen" {
			t.Errorf("ring not seen after views: %s", r.State)
		}
	}

	// Author sees the view count.
	tbToken, _ := h.login("tehanibentley")
	_, body = h.do("GET", "/api/v1/visions", tbToken, nil, "")
	var own VisionTrayDTO
	json.Unmarshal(body, &own)
	if own.Own == nil || own.Own.Visions[0].ViewCount == nil || *own.Own.Visions[0].ViewCount != 1 {
		t.Errorf("owner view count: %s", body)
	}

	// Private reply lands in the author's inbox as vision_reply.
	status, body = h.do("POST", "/events", devToken, map[string]string{"event_type": "vision_reply", "vision_id": tb.Visions[0].ID, "body": "love this"}, "")
	if status != 204 {
		t.Fatalf("vision_reply: %d %s", status, body)
	}
	_, body = h.do("GET", "/api/v1/notifications", tbToken, nil, "")
	if !strings.Contains(string(body), `"kind":"vision_reply"`) || !strings.Contains(string(body), "love this") {
		t.Errorf("author inbox lacks the private reply: %s", body)
	}
	// Replies-off vision refuses.
	admin := handles["admin"]
	status, _ = h.do("POST", "/events", devToken, map[string]string{"event_type": "vision_reply", "vision_id": admin.Visions[0].ID, "body": "x"}, "")
	if status != 400 {
		t.Errorf("reply to replies-off vision: %d", status)
	}
	// Poll vote.
	status, _ = h.do("POST", "/events", devToken, map[string]any{"event_type": "vision_poll_vote", "vision_id": admin.Visions[0].ID, "option_idx": 1}, "")
	if status != 204 {
		t.Errorf("vision_poll_vote: %d", status)
	}
	_, body = h.do("GET", "/api/v1/visions", devToken, nil, "")
	json.Unmarshal(body, &tray)
	for _, r := range tray.Rings {
		if r.Author.Handle == "admin" {
			if r.Visions[0].Poll == nil || r.Visions[0].Poll.ViewerVote != 1 || r.Visions[0].Poll.TotalVotes != 1 {
				t.Errorf("poll after vote: %+v", r.Visions[0].Poll)
			}
		}
	}

	// Dev posts a text vision; guest follows tehanibentley only, so guest does
	// not see it; tehanibentley (follows dev) does.
	status, body = h.do("POST", "/events", devToken, map[string]any{
		"event_type": "vision", "content_type": "text", "body": "hello tray", "artboard_background": "bloom",
		"audience": "everyone", "ttl_hours": 12, "allow_replies": true,
	}, "")
	if status != 201 {
		t.Fatalf("vision create: %d %s", status, body)
	}
	_, body = h.do("GET", "/api/v1/visions", guestToken, nil, "")
	if strings.Contains(string(body), "hello tray") {
		t.Errorf("guest sees a vision from someone they do not follow")
	}
	_, body = h.do("GET", "/api/v1/visions", tbToken, nil, "")
	if !strings.Contains(string(body), "hello tray") {
		t.Errorf("follower does not see the new vision: %s", body)
	}
	_, body = h.do("GET", "/api/v1/visions", devToken, nil, "")
	json.Unmarshal(body, &tray)
	if tray.Own == nil || len(tray.Own.Visions) != 1 {
		t.Errorf("own ring after posting: %+v", tray.Own)
	}

	// Muting a creator's visions removes their ring.
	status, _ = h.do("POST", "/events", devToken, map[string]string{"event_type": "vision_mute", "target_handle": "admin"}, "")
	if status != 204 {
		t.Fatalf("vision_mute: %d", status)
	}
	_, body = h.do("GET", "/api/v1/visions", devToken, nil, "")
	if strings.Contains(string(body), `"handle":"admin"`) {
		t.Errorf("muted creator still in tray")
	}

	// A bad preset is refused.
	status, _ = h.do("POST", "/events", devToken, map[string]any{"event_type": "vision", "content_type": "text", "body": "x", "artboard_background": "neon"}, "")
	if status != 400 {
		t.Errorf("bad background accepted: %d", status)
	}
}

// TestLiveRoom: the seeded room is listed, its chat backlog reads, chat and
// tips arrive over the SSE stream, presence counts, and ending answers a
// summary and closes the stream.
func TestLiveRoom(t *testing.T) {
	h := newHarness(t)
	devToken, _ := h.login("dev")
	tbToken, _ := h.login("tehanibentley")

	status, body := h.do("GET", "/api/v1/live", devToken, nil, "")
	if status != 200 {
		t.Fatalf("live: %d %s", status, body)
	}
	var list struct {
		Streams []LiveStreamDTO `json:"streams"`
		Count   int             `json:"count"`
	}
	json.Unmarshal(body, &list)
	if list.Count != 1 || list.Streams[0].Author.Handle != "tehanibentley" || list.Streams[0].HasVideo {
		t.Fatalf("live list: %s", body)
	}
	room := list.Streams[0]
	if room.TipTotalUAET != 40_000_000 || room.TipGoalUAET != 200_000_000 || room.PinnedBody == "" {
		t.Errorf("room totals: %+v", room)
	}
	if room.TopTippers != nil {
		t.Errorf("top tippers leaked to a viewer")
	}

	// Feed pages carry the live count.
	_, body = h.do("GET", "/api/v1/feed?surface=following", devToken, nil, "")
	if !strings.Contains(string(body), `"live_count":1`) {
		t.Errorf("feed lacks live_count: %s", body[:200])
	}

	// Backlog, oldest first.
	_, body = h.do("GET", "/api/v1/live/"+room.ID, devToken, nil, "")
	var view LiveRoomDTO
	json.Unmarshal(body, &view)
	if len(view.Chat) != 4 || view.Chat[0].Body != "play the new one" || view.Chat[2].Kind != "tip" {
		t.Errorf("backlog: %+v", view.Chat)
	}

	// Owner sees top tippers.
	_, body = h.do("GET", "/api/v1/live/"+room.ID, tbToken, nil, "")
	json.Unmarshal(body, &view)
	if len(view.Stream.TopTippers) != 1 || view.Stream.TopTippers[0].Author.Handle != "admin" {
		t.Errorf("owner top tippers: %+v", view.Stream.TopTippers)
	}

	// Open the event stream as dev, then chat and tip as guest; both frames arrive.
	req, _ := http.NewRequest("GET", h.srv.URL+"/api/v1/live/"+room.ID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+devToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("sse: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	frames := make(chan [2]string, 16)
	go func() {
		sc := bufio.NewScanner(res.Body)
		var event, data string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			case line == "":
				if event != "" {
					frames <- [2]string{event, data}
				}
				event, data = "", ""
			}
		}
	}()
	next := func(want string) string {
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
	// Presence: dev is one viewer.
	if v := next("viewers"); v != `{"count":1}` {
		t.Errorf("viewers = %s", v)
	}

	guestToken, _ := h.login("guest")
	status, body = h.do("POST", "/events", guestToken, map[string]string{"event_type": "live_chat", "stream_id": room.ID, "body": "hello room"}, "")
	if status != 200 {
		t.Fatalf("live_chat: %d %s", status, body)
	}
	if c := next("chat"); !strings.Contains(c, "hello room") || !strings.Contains(c, `"handle":"guest"`) {
		t.Errorf("chat frame = %s", c)
	}
	status, body = h.do("POST", "/events", guestToken, "event_type=tip&stream_id="+room.ID+"&amount_aet=500", "application/x-www-form-urlencoded")
	if status != 204 {
		t.Fatalf("stream tip: %d %s", status, body)
	}
	if tip := next("tip"); !strings.Contains(tip, `"kind":"tip"`) || !strings.Contains(tip, `"amount_uaet":5000000`) {
		t.Errorf("tip frame = %s", tip)
	}
	if totals := next("tips"); !strings.Contains(totals, `"total_uaet":45000000`) {
		t.Errorf("tips frame = %s", totals)
	}
	status, _ = h.do("POST", "/events", guestToken, map[string]string{"event_type": "live_heart", "stream_id": room.ID}, "")
	if status != 204 {
		t.Fatalf("heart: %d", status)
	}
	if hearts := next("hearts"); hearts != `{"count":1}` {
		t.Errorf("hearts = %s", hearts)
	}
	// Only the broadcaster pins.
	if status, _ := h.do("POST", "/events", guestToken, map[string]string{"event_type": "live_pin", "stream_id": room.ID, "body": "nope"}, ""); status != 403 {
		t.Errorf("viewer pinned: %d", status)
	}
	if status, _ := h.do("POST", "/events", tbToken, map[string]string{"event_type": "live_pin", "stream_id": room.ID, "body": "goal met soon"}, ""); status != 204 {
		t.Errorf("owner pin: %d", status)
	}
	if p := next("pinned"); !strings.Contains(p, "goal met soon") {
		t.Errorf("pinned = %s", p)
	}

	// Second broadcast while one is open is refused.
	if status, _ := h.do("POST", "/events", tbToken, map[string]string{"event_type": "live_start", "title": "again"}, ""); status != 409 {
		t.Errorf("double live_start: %d", status)
	}

	// End: summary, and the stream says so.
	status, body = h.do("POST", "/events", tbToken, map[string]string{"event_type": "live_end", "stream_id": room.ID}, "")
	if status != 200 {
		t.Fatalf("live_end: %d %s", status, body)
	}
	var sum LiveSummaryDTO
	json.Unmarshal(body, &sum)
	if sum.TipsUAET != 45_000_000 || sum.ChatLines != 4 || sum.Hearts != 1 || sum.PeakViewers < 1 {
		t.Errorf("summary = %+v", sum)
	}
	if st := next("status"); !strings.Contains(st, "ended") {
		t.Errorf("status frame = %s", st)
	}
	// Chat after the end is refused; the lane is empty.
	if status, _ := h.do("POST", "/events", guestToken, map[string]string{"event_type": "live_chat", "stream_id": room.ID, "body": "late"}, ""); status != 409 {
		t.Errorf("chat after end: %d", status)
	}
	_, body = h.do("GET", "/api/v1/live", devToken, nil, "")
	if !strings.Contains(string(body), `"count":0`) {
		t.Errorf("ended room still listed: %s", body)
	}

	// dev goes live with the sheet's choices and gets a room back.
	status, body = h.do("POST", "/events", devToken, map[string]any{
		"event_type": "live_start", "title": "Build log", "audience": "everyone", "lane": "music",
		"tip_goal_aet": 50, "notify_followers": true, "save_replay": false,
	}, "")
	if status != 201 {
		t.Fatalf("live_start: %d %s", status, body)
	}
	var mine LiveStreamDTO
	json.Unmarshal(body, &mine)
	if mine.Status != "live" || mine.TipGoalUAET != 50_000_000 || mine.Lane != "music" {
		t.Errorf("started room = %+v", mine)
	}
	// Followers were told.
	_, body = h.do("GET", "/api/v1/notifications", tbToken, nil, "")
	if !strings.Contains(string(body), `"kind":"live_start"`) {
		t.Errorf("follower not notified of live_start: %s", body)
	}
}

// TestAccountSurface: search with tags, tag feed, follow lists, saves,
// profile update, settings, edit window, sessions, and the user stream.
func TestAccountSurface(t *testing.T) {
	h := newHarness(t)
	devToken, _ := h.login("dev")

	status, body := h.do("GET", "/api/v1/search?q=fil", devToken, nil, "")
	if status != 200 || !strings.Contains(string(body), `"tag":"film"`) {
		t.Errorf("search tags: %d %s", status, body)
	}
	status, body = h.do("GET", "/api/v1/feed?surface=tag&tag=film", devToken, nil, "")
	if status != 200 || !strings.Contains(string(body), "four frames") {
		t.Errorf("tag feed: %d %s", status, body[:200])
	}
	status, body = h.do("GET", "/api/v1/users/dev/followers", devToken, nil, "")
	var page UserPageDTO
	json.Unmarshal(body, &page)
	if status != 200 || len(page.Users) != 3 {
		t.Errorf("followers: %d %s", status, body)
	}
	// Saves are the owner's.
	status, body = h.do("GET", "/api/v1/users/dev/works?tab=saves", devToken, nil, "")
	if status != 200 || !strings.Contains(string(body), "four frames") {
		t.Errorf("saves: %d %s", status, body[:200])
	}
	tbToken, _ := h.login("tehanibentley")
	if status, _ := h.do("GET", "/api/v1/users/dev/works?tab=saves", tbToken, nil, ""); status != 403 {
		t.Errorf("saves visible to others: %d", status)
	}
	// Profile update answers the fresh me.
	status, body = h.do("POST", "/events", devToken, map[string]any{"event_type": "profile_update", "bio": "Ships things.", "accent_hex": "#E05080"}, "")
	if status != 200 || !strings.Contains(string(body), `"bio":"Ships things."`) || !strings.Contains(string(body), `"accent_hex":"#E05080"`) {
		t.Errorf("profile_update: %d %s", status, body)
	}
	if status, _ := h.do("POST", "/events", devToken, map[string]any{"event_type": "profile_update", "accent_hex": "red"}, ""); status != 400 {
		t.Errorf("bad accent accepted: %d", status)
	}
	// Settings.
	if status, _ := h.do("POST", "/events", devToken, map[string]any{"event_type": "settings.privacy.show_sensitive", "value": "1"}, ""); status != 204 {
		t.Errorf("show_sensitive: %d", status)
	}
	_, body = h.do("GET", "/api/v1/me", devToken, nil, "")
	if !strings.Contains(string(body), `"show_sensitive":true`) {
		t.Errorf("setting did not stick: %s", body)
	}
	// Edit within the window, then the window is enforced on an old work.
	dev := newSigner(t)
	h.do("POST", "/api/pial/signing-key/register", devToken, map[string]string{"public_key_b64": dev.spkiB64()}, "")
	var me map[string]any
	json.Unmarshal(body, &me)
	p := workCanonicalPayload{AuthorPIAL: me["pial_id"].(string), Body: "typo hear", CommentGating: "everyone", Kind: "post", MediaURLs: []string{}, Tags: []string{}, TimestampMS: time.Now().UnixMilli()}
	_, body = h.do("POST", "/events", devToken, envelope(t, dev, p, nil), "application/json")
	var acc workAccepted
	json.Unmarshal(body, &acc)
	if status, body := h.do("POST", "/events", devToken, map[string]any{"event_type": "work_edit", "work_id": acc.WorkID, "body": "typo here"}, ""); status != 204 {
		t.Errorf("work_edit: %d %s", status, body)
	}
	_, body = h.do("GET", "/api/v1/works/"+acc.WorkID, devToken, nil, "")
	if !strings.Contains(string(body), `"body":"typo here"`) || !strings.Contains(string(body), `"is_edited":true`) {
		t.Errorf("edit not applied: %s", body[:300])
	}
	// Seeded works are hours old: the window is closed.
	_, body = h.do("GET", "/api/v1/users/dev/works", devToken, nil, "")
	var works WorkPageDTO
	json.Unmarshal(body, &works)
	var old string
	for _, w := range works.Works {
		if w.ID != acc.WorkID {
			old = w.ID
		}
	}
	if status, _ := h.do("POST", "/events", devToken, map[string]any{"event_type": "work_edit", "work_id": old, "body": "late"}, ""); status != 409 {
		t.Errorf("edit window not enforced: %d", status)
	}
	// Sessions: two logins, revoke the other.
	_, body = h.do("GET", "/api/v1/sessions", devToken, nil, "")
	var sess struct{ Sessions []SessionInfoDTO }
	json.Unmarshal(body, &sess)
	if len(sess.Sessions) < 1 || !sess.Sessions[0].IsCurrent {
		t.Errorf("sessions: %s", body)
	}
	other, _ := h.login("dev")
	_, body = h.do("GET", "/api/v1/sessions", devToken, nil, "")
	json.Unmarshal(body, &sess)
	if len(sess.Sessions) != 2 {
		t.Fatalf("expected 2 sessions: %s", body)
	}
	if status, _ := h.do("POST", "/events", devToken, map[string]any{"event_type": "session_revoke", "session_id": sess.Sessions[1].ID}, ""); status != 204 {
		t.Errorf("session_revoke: %d", status)
	}
	if status, _ := h.do("GET", "/api/v1/me", other, nil, ""); status != 401 {
		t.Errorf("revoked session still works: %d", status)
	}

	// User stream: a like from someone else pushes the unread count.
	req, _ := http.NewRequest("GET", h.srv.URL+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+devToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	frames := make(chan [2]string, 16)
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
	next("notify")
	next("balance")
	h.do("POST", "/events", tbToken, map[string]string{"event_type": "work_like", "work_id": acc.WorkID}, "")
	if n := next("notify"); !strings.Contains(n, `"unread"`) {
		t.Errorf("notify = %s", n)
	}
	// A new work from someone dev follows arrives as new_post.
	tb := newSigner(t)
	h.do("POST", "/api/pial/signing-key/register", tbToken, map[string]string{"public_key_b64": tb.spkiB64()}, "")
	_, body = h.do("GET", "/api/v1/me", tbToken, nil, "")
	var tbMe map[string]any
	json.Unmarshal(body, &tbMe)
	tp := workCanonicalPayload{AuthorPIAL: tbMe["pial_id"].(string), Body: "fresh", CommentGating: "everyone", Kind: "post", MediaURLs: []string{}, Tags: []string{}, TimestampMS: time.Now().UnixMilli()}
	h.do("POST", "/events", tbToken, envelope(t, tb, tp, nil), "application/json")
	if np := next("new_post"); !strings.Contains(np, `"author_handle":"tehanibentley"`) {
		t.Errorf("new_post = %s", np)
	}
}
