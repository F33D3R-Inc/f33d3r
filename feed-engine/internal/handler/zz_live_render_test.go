package handler

import (
	"bytes"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/f33d3r/feed-engine/internal/config"
	"github.com/f33d3r/feed-engine/internal/live"
	"github.com/f33d3r/feed-engine/internal/model"
)

// liveTestHandler builds a Handler against the real template tree, the same way
// the follow and atlas render tests do.
func liveTestHandler(t *testing.T) *Handler {
	t.Helper()
	cwd, _ := os.Getwd()
	if err := os.Chdir("../.."); err != nil {
		t.Skipf("cannot chdir to repo root: %v", err)
	}
	t.Cleanup(func() { os.Chdir(cwd) })
	if _, err := os.Stat("web/templates/partials"); err != nil {
		t.Skipf("no web/templates: %v", err)
	}
	h := &Handler{cfg: &config.Config{}}
	h.loadTemplates()
	return h
}

func liveTestStream() *model.LiveStream {
	now := time.Now().Add(-12 * time.Minute)
	return &model.LiveStream{
		ID: "11111111-1111-1111-1111-111111111111", AuthorID: "a", AuthorPIAL: "p",
		Title: "Building the live lane", Status: model.LiveStatusLive,
		StartedAt: &now, ViewerCount: 12, PeakViewers: 40,
		PosterURL: "/live/x/poster.jpg", PlaylistURL: "/live/x/master.m3u8",
		SourceHeight: 1080, AuthorHandle: "nantar", AuthorName: "Nantar",
	}
}

func liveTestView() *liveStreamView {
	s := liveTestStream()
	return &liveStreamView{
		S: s, AuthorHandle: s.AuthorHandle, AuthorName: s.AuthorName,
		AuthorPIAL: s.AuthorPIAL, VerifiedType: "blue", IsVerified: true,
		Viewers: "12", ViewerCount: 12, TimeLabel: "live 12m",
		Ladder: []int{1080, 720, 480}, Heartbeat: true, ViewerToken: "tok",
		CanChat: true, ViewerHandle: "watcher",
	}
}

func TestLiveFacetsRender(t *testing.T) {
	h := liveTestHandler(t)
	v := liveTestView()
	msg := liveChatMessage{
		ID: "m1", StreamID: v.S.ID, Handle: "watcher", Name: "Watcher",
		Body: "hello", CreatedAt: time.Now(), TimeLabel: "12:00",
	}

	cases := []struct {
		name string
		data interface{}
		want string
	}{
		{"live_player", v, "facet:f33d3r:live:" + v.S.ID + ":player"},
		{"live_card", v, "facet:f33d3r:live:" + v.S.ID + ":card"},
		{"live_author_row", v, "facet:f33d3r:live:" + v.S.ID + ":author"},
		{"live_title_bar", map[string]interface{}{"StreamID": v.S.ID, "Title": v.S.Title, "Status": v.S.Status, "BackURL": "#back"}, ":title_bar"},
		{"live_chat_row", map[string]interface{}{"M": msg}, ":chat_msg_m1"},
		{"live_chat_panel", map[string]interface{}{"StreamID": v.S.ID, "Messages": []liveChatMessage{msg}, "CanChat": true, "Handle": "watcher"}, ":chat"},
		{"live_chat_panel", map[string]interface{}{"StreamID": v.S.ID, "Messages": nil, "CanChat": false, "Handle": ""}, ":chat_gate"},
		{"live_go_composer", map[string]interface{}{
			"Handle": "nantar", "CanNSFW": true, "Error": "",
			"Title": "", "Description": "", "Source": "browser",
		}, "facet:f33d3r:live:new:go_composer"},
		{"live_ended", map[string]interface{}{"S": v.S, "AuthorHandle": "nantar"}, ":ended"},
		{"live_broadcast_stage", map[string]interface{}{
			"S": v.S, "AuthorHandle": "nantar", "Facing": "environment", "Source": "browser",
			"Viewers": "12", "ViewerCount": 12, "ShareURL": "/live/x",
			"Encoder": encoderSetupData(v.S.ID, nil),
		}, ":broadcast"},
		{"live_broadcast_stage", map[string]interface{}{
			"S": v.S, "AuthorHandle": "nantar", "Facing": "environment", "Source": "encoder",
			"Viewers": "12", "ViewerCount": 12, "ShareURL": "/live/x",
			"Encoder": encoderSetupData(v.S.ID, nil),
		}, "facet:f33d3r:live:" + v.S.ID + ":encoder_setup"},
		{"live_encoder_setup", encoderSetupData(v.S.ID, nil), "facet:f33d3r:live:" + v.S.ID + ":encoder_setup"},
	}

	for _, c := range cases {
		var buf bytes.Buffer
		if err := h.partial.ExecuteTemplate(&buf, c.name, c.data); err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !strings.Contains(buf.String(), c.want) {
			t.Errorf("%s: fragment missing %q", c.name, c.want)
		}
		if strings.Contains(buf.String(), "<no value>") {
			t.Errorf("%s: fragment contains an unfilled key", c.name)
		}
	}
}

func TestLivePageRenders(t *testing.T) {
	h := liveTestHandler(t)
	v := liveTestView()
	msg := liveChatMessage{ID: "m1", StreamID: v.S.ID, Handle: "w", Name: "W", Body: "hi", TimeLabel: "12:00"}

	modes := []map[string]interface{}{
		{"Mode": "watch", "V": v, "ChatMessages": []liveChatMessage{msg}, "PageTitle": "t",
			"OGTitle": "t", "OGDesc": "d", "OGImage": "i", "OGUrl": "u"},
		{"Mode": "compose", "ComposerHandle": "nantar", "CanNSFW": false, "Error": "",
			"Title": "", "Description": "", "Source": "browser", "PageTitle": "t"},
		{"Mode": "broadcast", "B": map[string]interface{}{
			"S": v.S, "AuthorHandle": "nantar", "Facing": "user", "Source": "browser",
			"Viewers": "0", "ViewerCount": 0, "ShareURL": "/live/x",
			"Encoder": encoderSetupData(v.S.ID, nil)}, "PageTitle": "t"},
		{"Mode": "broadcast", "B": map[string]interface{}{
			"S": v.S, "AuthorHandle": "nantar", "Facing": "user", "Source": "encoder",
			"Viewers": "0", "ViewerCount": 0, "ShareURL": "/live/x",
			"Encoder": encoderSetupData(v.S.ID, nil)}, "PageTitle": "t"},
		{"Mode": "missing", "PageTitle": "t"},
	}
	for _, data := range modes {
		data["User"] = (*model.User)(nil)
		data["RailContext"] = "default"
		// base.html itself reaches the database through celebrationClass, which
		// this template-only harness has no handle for. The page's own blocks are
		// what this file owns, so those are what it renders.
		for _, block := range []string{"title", "head-extra", "shell-class", "sidebar-hidden", "mobile-nav-hidden", "body", "right-panel", "page-scripts"} {
			var buf bytes.Buffer
			if err := h.pages["live.html"].ExecuteTemplate(&buf, block, data); err != nil {
				t.Errorf("mode %v block %s: %v", data["Mode"], block, err)
			}
			if strings.Contains(buf.String(), "<no value>") {
				t.Errorf("mode %v block %s: unfilled key", data["Mode"], block)
			}
		}
	}
}

// TestLiveEncoderKeySealed is the standing guard on the one credential in the
// live lane. The sealed Facet is what every broadcast page ships with; if a
// stream key ever reaches it, this fails.
func TestLiveEncoderKeySealed(t *testing.T) {
	h := liveTestHandler(t)
	streamID := liveTestStream().ID
	const key = "sealedkeymustnotappearanywhere"
	targets := live.TargetsFor(streamID, key)

	var sealed bytes.Buffer
	if err := h.partial.ExecuteTemplate(&sealed, "live_encoder_setup", encoderSetupData(streamID, nil)); err != nil {
		t.Fatalf("sealed encoder facet: %v", err)
	}
	for _, secret := range []string{key, targets.RTMPStreamKey, targets.SRTURL} {
		if strings.Contains(sealed.String(), secret) {
			t.Fatalf("sealed encoder facet leaked a publish credential")
		}
	}
	if !strings.Contains(sealed.String(), `hx-post="/live/`+streamID+`/encoder"`) {
		t.Errorf("sealed encoder facet has no reveal control")
	}

	// The revealed state is the answer to the owner's own POST, and is the only
	// place the key is allowed to be.
	var revealed bytes.Buffer
	if err := h.partial.ExecuteTemplate(&revealed, "live_encoder_setup", encoderSetupData(streamID, &targets)); err != nil {
		t.Fatalf("revealed encoder facet: %v", err)
	}
	if !strings.Contains(revealed.String(), key) {
		t.Errorf("revealed encoder facet does not carry the stream key")
	}
	if !strings.Contains(revealed.String(), targets.RTMPServer) {
		t.Errorf("revealed encoder facet does not carry the server URL")
	}
	// The browser leg never needs a credential — it goes through the same-origin
	// WHIP proxy — so no WHIP publish URL exists to be rendered, here or
	// anywhere. The key must not appear in any URL shape at all.
	if strings.Contains(revealed.String(), "/whip?") {
		t.Errorf("revealed encoder facet rendered a WHIP publish URL")
	}
}

// TestLiveViewerSurfacesCarryNoKey proves the credential cannot reach a viewer
// through any Facet a viewer is served.
func TestLiveViewerSurfacesCarryNoKey(t *testing.T) {
	h := liveTestHandler(t)
	v := liveTestView()
	const key = "viewerfacingsurfacesmustnothaveit"
	targets := live.TargetsFor(v.S.ID, key)

	for _, name := range []string{"live_player", "live_card", "live_author_row"} {
		var buf bytes.Buffer
		if err := h.partial.ExecuteTemplate(&buf, name, v); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		out := buf.String()
		for _, secret := range []string{key, targets.RTMPStreamKey, targets.SRTURL} {
			if strings.Contains(out, secret) {
				t.Errorf("%s leaked a publish credential to a viewer surface", name)
			}
		}
	}
}

// TestLiveIdleWallBeats pins the channel that lets an open watch page learn
// that the broadcast started.
//
// The idle wall renders no tally, so it cannot carry live_viewer_count's beat;
// before live_wait_beat it carried nothing, and a viewer who opened the page
// before the broadcaster went live was never told. The wall's beat states the
// status it was rendered for, swaps nothing on its own, and is addressed by
// its own Facet id inside the player root — so the player answering it
// replaces the beat along with the wall.
func TestLiveIdleWallBeats(t *testing.T) {
	h := liveTestHandler(t)
	v := liveTestView()
	v.S.Status = model.LiveStatusIdle
	v.Heartbeat = livePresenceBeats(v.S, true)
	v.ViewerToken = "waiting-token"

	var wall bytes.Buffer
	if err := h.partial.ExecuteTemplate(&wall, "live_player", v); err != nil {
		t.Fatalf("live_player (idle): %v", err)
	}
	out := wall.String()
	if !strings.Contains(out, "is not live right now") {
		t.Fatal("idle player is not the wall")
	}
	if !strings.Contains(out, `data-facet-id="facet:f33d3r:live:`+v.S.ID+`:wait"`) {
		t.Error("idle wall carries no wait beat Facet")
	}
	if !strings.Contains(out, `hx-post="/live/`+v.S.ID+`/heartbeat"`) {
		t.Error("idle wall's beat does not post presence")
	}
	if !strings.Contains(out, `hx-swap="none"`) {
		t.Error("idle wall's beat swaps something on its own; the server names the swap")
	}
	if !strings.Contains(out, `"status":"idle"`) {
		t.Error("idle wall's beat does not state the status it was rendered for")
	}
	if !strings.Contains(out, `"vt":"waiting-token"`) {
		t.Error("idle wall's beat does not carry the page's viewer token")
	}
	// The wall still holds no tally and no video: waiting is not watching.
	if strings.Contains(out, "live-viewers") {
		t.Error("idle wall renders a viewer tally")
	}
	if strings.Contains(out, "data-f33d-live-hls") {
		t.Error("idle wall renders a decoder")
	}

	// A running broadcast's player carries the tally's beat and not the wall's.
	v.S.Status = model.LiveStatusLive
	v.Heartbeat = livePresenceBeats(v.S, true)
	var running bytes.Buffer
	if err := h.partial.ExecuteTemplate(&running, "live_player", v); err != nil {
		t.Fatalf("live_player (live): %v", err)
	}
	if strings.Contains(running.String(), ":wait\"") {
		t.Error("a running broadcast's player still carries the wait beat")
	}
}

// TestLivePlayerShapeGate pins which renders may replace the player root.
//
// The root holds the decoder. A status change is the only render allowed to
// replace it; a render that changes the ladder and nothing else — a
// backpressure report, a capacity note — must reach the surface some other
// way, or every such report restarts every viewer's decode from nothing.
func TestLivePlayerShapeGate(t *testing.T) {
	s := liveTestStream()
	liveRuntime.forget(s.ID)
	t.Cleanup(func() { liveRuntime.forget(s.ID) })

	if !liveRuntime.shapeMoved(s.ID, playerShape(s)) {
		t.Fatal("the first render of a stream must count as a change of shape")
	}
	if liveRuntime.shapeMoved(s.ID, playerShape(s)) {
		t.Error("a render under the same status replaced the player root")
	}
	s.Ladder = "720p,480p"
	if liveRuntime.shapeMoved(s.ID, playerShape(s)) {
		t.Error("a ladder change replaced the player root")
	}
	s.Status = model.LiveStatusEnded
	if !liveRuntime.shapeMoved(s.ID, playerShape(s)) {
		t.Error("ending the broadcast did not replace the player root")
	}
	s.IsBlocked = true
	if !liveRuntime.shapeMoved(s.ID, playerShape(s)) {
		t.Error("blocking the broadcast did not replace the player root")
	}
	liveRuntime.forget(s.ID)
	if !liveRuntime.shapeMoved(s.ID, playerShape(s)) {
		t.Error("a forgotten stream's next render must replace the player root")
	}
}

// TestResolvePublishSource pins the order the server trusts its evidence in.
func TestResolvePublishSource(t *testing.T) {
	s := liveTestStream()
	liveRuntime.forget(s.ID)
	t.Cleanup(func() { liveRuntime.forget(s.ID) })

	if got := resolvePublishSource("", s); got != liveSourceBrowser {
		t.Errorf("no evidence: got %q, want browser", got)
	}
	s.IngestProto = "rtmp"
	if got := resolvePublishSource("", s); got != liveSourceEncoder {
		t.Errorf("observed rtmp: got %q, want encoder", got)
	}
	liveRuntime.setPublishSource(s.ID, liveSourceBrowser)
	if got := resolvePublishSource("", s); got != liveSourceBrowser {
		t.Errorf("recorded choice must outrank observed protocol: got %q", got)
	}
	if got := resolvePublishSource("encoder", s); got != liveSourceEncoder {
		t.Errorf("explicit request must win: got %q", got)
	}
	if got := resolvePublishSource("nonsense", s); got != liveSourceBrowser {
		t.Errorf("unknown request falls back to the recorded choice: got %q", got)
	}
}

// TestLivePresenceBeatIsServerOwned pins the rule that ends a presence beat.
//
// A surface beats only while the server says the broadcast is running. Nothing
// in the browser decides this: the Fragment either carries the beat or does not,
// and an ended stream's Fragment never does. Without this the watch surface of a
// finished broadcast beats every 20 s forever.
func TestLivePresenceBeatIsServerOwned(t *testing.T) {
	s := liveTestStream()

	cases := []struct {
		status string
		want   bool
	}{
		{model.LiveStatusLive, true},
		{model.LiveStatusIdle, false},
		{model.LiveStatusEnded, false},
	}
	for _, c := range cases {
		s.Status = c.status
		if got := livePresenceBeats(s, true); got != c.want {
			t.Errorf("status %q: a wanting surface beats = %v, want %v", c.status, got, c.want)
		}
		if livePresenceBeats(s, false) {
			t.Errorf("status %q: a surface that asked for no beat was given one", c.status)
		}
	}
	if livePresenceBeats(nil, true) {
		t.Error("no stream must never beat")
	}
}

// TestLiveEndedSurfaceCarriesNoBeat is the render-level guard on the same rule:
// the tally Facet the server renders for a finished broadcast asks for nothing,
// and the player it replaces that surface with holds no tally at all.
func TestLiveEndedSurfaceCarriesNoBeat(t *testing.T) {
	h := liveTestHandler(t)
	v := liveTestView()
	v.S.Status = model.LiveStatusEnded
	v.Heartbeat = livePresenceBeats(v.S, true)

	var tally bytes.Buffer
	if err := h.partial.ExecuteTemplate(&tally, "live_viewer_count", map[string]interface{}{
		"StreamID": v.S.ID, "Slot": "viewers", "Display": v.Viewers, "Count": v.ViewerCount,
		"Heartbeat": v.Heartbeat, "Token": "tok",
	}); err != nil {
		t.Fatalf("live_viewer_count: %v", err)
	}
	for _, forbidden := range []string{"hx-post", "hx-trigger", "every 20s"} {
		if strings.Contains(tally.String(), forbidden) {
			t.Errorf("an ended stream's tally still carries %q", forbidden)
		}
	}

	// The answer a beat against an ended stream receives: the whole player,
	// which is the terminal live_ended Facet and has no tally to beat.
	var player bytes.Buffer
	if err := h.partial.ExecuteTemplate(&player, "live_player", v); err != nil {
		t.Fatalf("live_player: %v", err)
	}
	if !strings.Contains(player.String(), "facet:f33d3r:live:"+v.S.ID+":ended") {
		t.Error("an ended stream's player is not the terminal Facet")
	}
	if strings.Contains(player.String(), "/heartbeat") {
		t.Error("an ended stream's player still asks for a presence beat")
	}

	// The same Facet while the broadcast runs must still beat, or the tally
	// stops updating for everyone.
	v.S.Status = model.LiveStatusLive
	v.Heartbeat = livePresenceBeats(v.S, true)
	var running bytes.Buffer
	if err := h.partial.ExecuteTemplate(&running, "live_player", v); err != nil {
		t.Fatalf("live_player (live): %v", err)
	}
	if !strings.Contains(running.String(), `hx-post="/live/`+v.S.ID+`/heartbeat"`) {
		t.Error("a running broadcast's player lost its presence beat")
	}
}

// TestLiveBadgeOOBIsAddressedByItsOwnFacetID pins the delivery shape that closes
// the stale title badge.
//
// A logged-out watcher's only channel is the presence beat, so the answer to a
// beat has to carry the badge alongside the player. htmx matches a plain
// hx-swap-oob="true" against the id ATTRIBUTE, which these Facets do not have —
// that form would silently swap nothing. The selector form addressing the
// Facet's own data-facet-id is the mechanism, and this is the guard that the
// address rendered into the Fragment is byte-for-byte the address the Fragment
// itself carries, so the two can never drift.
func TestLiveBadgeOOBIsAddressedByItsOwnFacetID(t *testing.T) {
	h := liveTestHandler(t)
	s := liveTestStream()
	s.Status = model.LiveStatusEnded
	facetID := "facet:f33d3r:live:" + s.ID + ":title_badge"

	var oob bytes.Buffer
	if err := h.partial.ExecuteTemplate(&oob, "live_badge", map[string]interface{}{
		"StreamID": s.ID, "Slot": "title_badge", "Status": s.Status, "OOB": true,
	}); err != nil {
		t.Fatalf("live_badge (oob): %v", err)
	}
	out := oob.String()
	if !strings.Contains(out, `data-facet-id="`+facetID+`"`) {
		t.Fatalf("oob badge does not carry its own facet id: %s", out)
	}
	// The rendered attribute is HTML-escaped; the browser hands htmx the decoded
	// value, so assert on the decoded form the selector will actually be.
	if !strings.Contains(html.UnescapeString(out),
		`hx-swap-oob="outerHTML:[data-facet-id='`+facetID+`']"`) {
		t.Errorf("oob badge is not addressed by its data-facet-id: %s", out)
	}
	if strings.Contains(out, `hx-swap-oob="true"`) {
		t.Error("oob badge uses the id-attribute form, which matches nothing here")
	}
	if !strings.Contains(out, "ENDED") {
		t.Error("oob badge does not carry the status it was rendered for")
	}

	// The same Facet written into a surface must carry no delivery address at
	// all, or it would swap itself out of any htmx response that contains it.
	for _, slot := range []string{"badge", "title_badge", "card_badge", "bcast_badge"} {
		var plain bytes.Buffer
		if err := h.partial.ExecuteTemplate(&plain, "live_badge", map[string]interface{}{
			"StreamID": s.ID, "Slot": slot, "Status": s.Status, "OOB": false,
		}); err != nil {
			t.Fatalf("live_badge %s: %v", slot, err)
		}
		if strings.Contains(plain.String(), "hx-swap-oob") {
			t.Errorf("in-surface badge %s carries an out-of-band address", slot)
		}
	}
}

// TestLiveSurfacesStateBadgeDelivery is the drift guard on the input every
// live_badge call site has to state. A site that forgets it renders "<no value>"
// under the default template options and hard-fails under TEMPLATE_STRICT, so
// the badge is rendered through each of its four surfaces here.
func TestLiveSurfacesStateBadgeDelivery(t *testing.T) {
	h := liveTestHandler(t)
	v := liveTestView()

	cases := []struct {
		name string
		data interface{}
	}{
		{"live_player", v},
		{"live_card", v},
		{"live_title_bar", map[string]interface{}{
			"StreamID": v.S.ID, "Title": v.S.Title, "Status": v.S.Status, "BackURL": "#back"}},
		{"live_broadcast_stage", map[string]interface{}{
			"S": v.S, "AuthorHandle": "nantar", "Facing": "environment", "Source": "browser",
			"Viewers": "12", "ViewerCount": 12, "ShareURL": "/live/x",
			"Encoder": encoderSetupData(v.S.ID, nil)}},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		if err := h.partial.ExecuteTemplate(&buf, c.name, c.data); err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !strings.Contains(buf.String(), "live-badge") {
			t.Errorf("%s renders no live_badge", c.name)
			continue
		}
		if strings.Contains(buf.String(), "hx-swap-oob") {
			t.Errorf("%s wrote an out-of-band address into the surface", c.name)
		}
		if strings.Contains(buf.String(), "<no value>") {
			t.Errorf("%s has a live_badge call site that states no delivery shape", c.name)
		}
	}
}

// TestLiveWatchSurfaceBeat pins which surface is owed the title badge.
//
// live_player beats from two surfaces — the watch page and the author's profile
// hero — and only the watch page has a title bar above it. The server sends an
// address it knows exists and no other.
func TestLiveWatchSurfaceBeat(t *testing.T) {
	id := liveTestStream().ID
	cases := []struct {
		current string
		want    bool
	}{
		{"https://f33d3r.com/live/" + id, true},
		{"https://f33d3r.com/live/" + id + "/", true},
		{"https://f33d3r.com/live/" + id + "?x=1", true},
		{"https://f33d3r.com/nantar", false}, // profile live hero
		{"https://f33d3r.com/live/" + id + "/broadcast", false},
		{"https://f33d3r.com/live/22222222-2222-2222-2222-222222222222", false},
		{"", false},
		{"://nonsense", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodPost, "/live/"+id+"/heartbeat", nil)
		if c.current != "" {
			r.Header.Set("HX-Current-URL", c.current)
		}
		if got := liveWatchSurfaceBeat(r, id); got != c.want {
			t.Errorf("HX-Current-URL %q: got %v, want %v", c.current, got, c.want)
		}
	}
}
