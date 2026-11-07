package handler

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/f33d3r/feed-engine/internal/auralis"
	"github.com/f33d3r/feed-engine/internal/config"
	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/nexus"
)

const (
	freqTestHostPIAL     = "c0ffee00-0000-4000-8000-00000000a001"
	freqTestListenerPIAL = "c0ffee00-0000-4000-8000-00000000a002"
	freqTestID           = "11111111-2222-4333-8444-555555555555"
)

// auralisStub plays Auralis: it answers by route and records every call.
type auralisStub struct {
	mu    sync.Mutex
	calls []string
	// status overrides by path suffix, e.g. "/approve": 403
	refuse map[string]int
	view   string
	// heartbeat, when set, answers POST .../heartbeat; otherwise the view does.
	heartbeat string
}

func (s *auralisStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.calls = append(s.calls, r.Method+" "+r.URL.Path+" pial="+r.Header.Get("X-Pial-Identity"))
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("X-Internal-Key") != "k" {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"unauthenticated","message":"no key"}`))
		return
	}
	for suffix, status := range s.refuse {
		if strings.HasSuffix(r.URL.Path, suffix) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"forbidden","message":"listener may not ApproveSpeaker"}`))
			return
		}
	}
	switch {
	case s.heartbeat != "" && strings.HasSuffix(r.URL.Path, "/heartbeat"):
		w.WriteHeader(200)
		_, _ = w.Write([]byte(s.heartbeat))
	case strings.HasSuffix(r.URL.Path, "/request_mic"):
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"request":{"id":"r1","pial_id":"` + freqTestListenerPIAL + `","reason":"Lagos","upvotes":0,"created_at":"2026-09-06T21:19:55Z"},"created":true}`))
	case r.Method == http.MethodPost && r.URL.Path == "/v1/frequencies":
		w.WriteHeader(201)
		_, _ = w.Write([]byte(s.view))
	default:
		w.WriteHeader(200)
		_, _ = w.Write([]byte(s.view))
	}
}

func freqViewJSON(state string) string {
	return `{"frequency":{"frequency":{"id":"` + freqTestID + `","version":3,"host_pial_id":"` + freqTestHostPIAL + `","title":"Late Night Tech Talk","description":"fees","state":"` + state + `","visibility":"public","language":"en","adult_content":false,"speaker_verity_min_tier":0,"scheduled_at":null,"started_at":"2026-09-06T21:19:54Z","ended_at":null,"end_reason":null,"recording_enabled":false,"replay_status":"none","max_speakers":10,"max_listeners":1000,"requests_open":true,"locked":false,"media_node":"n1","created_at":"2026-09-06T21:19:54Z","updated_at":"2026-09-06T21:19:54Z"},"counts":{"listeners":1,"speakers":0,"participants":2},"speakers":[{"pial_id":"` + freqTestHostPIAL + `","role":"host","muted":false,"present":true,"joined_at":"2026-09-06T21:19:54Z"}],"cohost_pial_ids":[],"requests":[{"id":"r1","pial_id":"` + freqTestListenerPIAL + `","reason":"I run a node in Lagos","upvotes":1,"created_at":"2026-09-06T21:19:55Z"}],"present_pial_ids":["` + freqTestHostPIAL + `","` + freqTestListenerPIAL + `"],"lease_node":"n1","viewer":{"pial_id":"` + freqTestListenerPIAL + `","role":"listener","muted":false,"present":true,"blocked":false,"request":null,"can_request_mic":true,"can_moderate":false,"can_end":false,"can_speak":false,"can_listen":true}},"session":{"session_id":"s1","token":"abc.def","expires_at":1,"signal_path":"/frequencies/signal/` + freqTestID + `","role":"listener","permissions":["subscribe"]},"rejoined":false}`
}

// frequencyTestHandler builds a Handler against the real template tree with a
// stub Auralis. The database handle is a lazy, unreachable connection: the
// session cache path needs a non-nil handle, and the people lookup tolerates
// a failed query by rendering a nameless person.
func frequencyTestHandler(t *testing.T, stub *auralisStub) (*Handler, *httptest.Server) {
	t.Helper()
	cwd, _ := os.Getwd()
	if err := os.Chdir("../.."); err != nil {
		t.Skipf("cannot chdir to repo root: %v", err)
	}
	t.Cleanup(func() { os.Chdir(cwd) })
	if _, err := os.Stat("web/templates/partials"); err != nil {
		t.Skipf("no web/templates: %v", err)
	}
	lazy, err := sql.Open("postgres", "host=127.0.0.1 port=1 user=x dbname=x sslmode=disable connect_timeout=1")
	if err != nil {
		t.Fatalf("lazy db: %v", err)
	}
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	h := &Handler{cfg: &config.Config{}, db: lazy}
	h.loadTemplates()
	h.auralis = auralis.New(srv.URL, "k", time.Second)
	// A full-page render asks NEXUS for the persona session; point it at the
	// stub so the composer re-render path has a client to fail softly against.
	h.nexusClient = nexus.NewClient(srv.URL, "k")
	return h, srv
}

// as injects a signed-in actor through the session cache, the same object the
// DB path fills, so no database is needed to be somebody.
func (h *Handler) as(t *testing.T, r *http.Request, handle, pial string) {
	t.Helper()
	tok := "tok-" + handle
	h.sessionCache.Store(tok, sessionEntry{
		user: &model.User{ID: "u-" + handle, Handle: handle, DisplayName: handle, PIALID: pial, ContentSetting: "default"},
		exp:  time.Now().Add(time.Minute),
	})
	r.Header.Set("Authorization", "Bearer "+tok)
}

func eventRequest(fields url.Values) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(fields.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("HX-Request", "true")
	return r
}

func requirePartials(t *testing.T, h *Handler, names ...string) {
	t.Helper()
	for _, n := range names {
		if h.partial == nil || h.partial.Lookup(n) == nil {
			t.Skipf("partial %q not present yet (Facets fork owns it); render path untested here", n)
		}
	}
}

func TestFrequencyFacetIDFormat(t *testing.T) {
	got := frequencyFacetID(freqTestID, "controls")
	want := "facet:f33d3r:frequency:" + freqTestID + ":controls"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	for _, slot := range []string{"card", "stage", "header", "speakers", "listener_count", "live_badge", "controls", "request_button", "requests", "status", "recording"} {
		if _, ok := frequencySlotPartial(slot); !ok {
			t.Fatalf("slot %q has no partial", slot)
		}
	}
	if _, ok := frequencySlotPartial("role"); ok {
		t.Fatal("unknown slot must not resolve")
	}
}

// The derived standing (fan-out path) must agree with Auralis's own rules.
func TestFrequencyStandingDerivation(t *testing.T) {
	var ans auralis.Answer
	if err := json.Unmarshal([]byte(freqViewJSON("live")), &ans); err != nil {
		t.Fatal(err)
	}
	v := &ans.Frequency
	host := frequencyStanding(v, freqTestHostPIAL)
	if host.Role != "host" || !host.CanEnd || !host.CanModerate || !host.CanSpeak || host.CanRequestMic {
		t.Fatalf("host standing wrong: %+v", host)
	}
	// Listener with a pending request: Auralis's viewer block says so.
	listener := frequencyStanding(v, freqTestListenerPIAL)
	if listener.Role != "listener" || listener.CanEnd || listener.CanModerate || listener.CanSpeak || !listener.CanRequestMic {
		t.Fatalf("listener standing (auralis viewer) wrong: %+v", listener)
	}
	// Same person, derived locally (no viewer block): the pending request in
	// the queue means they may not ask again.
	v.Viewer = nil
	derived := frequencyStanding(v, freqTestListenerPIAL)
	if derived.Role != "listener" || !derived.InFrequency || derived.CanRequestMic || derived.Request == nil || derived.Request.RequestID != "r1" {
		t.Fatalf("derived listener standing wrong: %+v", derived)
	}
	nobody := frequencyStanding(v, "c0ffee00-0000-4000-8000-00000000ffff")
	if nobody.InFrequency || nobody.Role != "" || nobody.CanRequestMic {
		t.Fatalf("outsider standing wrong: %+v", nobody)
	}
}

func TestFrequencyErrorMapping(t *testing.T) {
	cases := []struct {
		err  error
		code int
	}{
		{&auralis.StatusError{Status: 403, Code: "forbidden"}, 403},
		{&auralis.StatusError{Status: 409, Code: "requests_closed"}, 409},
		{&auralis.StatusError{Status: 429, Code: "rate_limited"}, 429},
		{&auralis.StatusError{Status: 404, Code: "not_found"}, 404},
		{&auralis.StatusError{Status: 500, Code: "db_error"}, 503},
		{auralis.ErrNotConfigured, 503},
	}
	for _, c := range cases {
		msg, code := frequencyErrorMessage(c.err)
		if code != c.code || msg == "" {
			t.Fatalf("%v → %d %q, want %d", c.err, code, msg, c.code)
		}
	}
}

func TestFrequencyTuneInAnswersDockOrStageAndFansOut(t *testing.T) {
	stub := &auralisStub{view: freqViewJSON("live")}
	h, _ := frequencyTestHandler(t, stub)
	requirePartials(t, h, "frequency_stage", "frequency_dock", "frequency_controls", "frequency_request_button", "frequency_request_queue", "frequency_header", "frequency_speaker_grid", "frequency_listener_count", "frequency_live_badge", "frequency_status", "frequency_recording_badge", "frequency_card")

	hostCh, _, hostDone := RegisterSSESession("u-host", freqTestHostPIAL)
	defer hostDone()

	// The answer is empty wherever the form was. An overlay clears on it, and the
	// stage and dock arrive over FA Live.
	r := eventRequest(url.Values{"event_type": {"frequency.tune_in"}, "frequency_id": {freqTestID}})
	h.as(t, r, "listener", freqTestListenerPIAL)
	w := httptest.NewRecorder()
	h.eventsPost(w, r)
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("tune in must answer an empty 200: %d %s", w.Code, w.Body.String())
	}

	stub.mu.Lock()
	joined := strings.Join(stub.calls, "\n")
	stub.mu.Unlock()
	if !strings.Contains(joined, "POST /v1/frequencies/"+freqTestID+"/tune_in pial="+freqTestListenerPIAL) {
		t.Fatalf("Auralis was not asked to admit the listener:\n%s", joined)
	}

	// The host, present, receives the fan-out over FA Live.
	deadline := time.After(3 * time.Second)
	gotControls := false
	for !gotControls {
		select {
		case ev := <-hostCh:
			// One publish, two projections: the fragments for the web and
			// the JSON room for a phone. Anything else is a defect.
			if ev.Type != sseFacetMutate && ev.Type != sseFrequencyJSON {
				t.Fatalf("wrong SSE type %q", ev.Type)
			}
			if ev.Type == sseFacetMutate && strings.Contains(ev.Data, frequencyFacetID(freqTestID, "controls")) {
				gotControls = true
			}
		case <-deadline:
			t.Fatal("host never received the controls fragment over FA Live")
		}
	}
}

func TestFrequencyLeaveAnswersEmpty(t *testing.T) {
	stub := &auralisStub{view: freqViewJSON("live")}
	h, _ := frequencyTestHandler(t, stub)
	requirePartials(t, h, "frequency_dock")
	r := eventRequest(url.Values{"event_type": {"frequency.leave"}, "frequency_id": {freqTestID}})
	h.as(t, r, "listener", freqTestListenerPIAL)
	w := httptest.NewRecorder()
	h.eventsPost(w, r)
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("leave must answer an empty 200, got %d %q", w.Code, w.Body.String())
	}
}

func TestFrequencyApproveByListenerIsForbidden(t *testing.T) {
	stub := &auralisStub{view: freqViewJSON("live"), refuse: map[string]int{"/approve": 403}}
	h, _ := frequencyTestHandler(t, stub)

	r := eventRequest(url.Values{"event_type": {"frequency.approve"}, "frequency_id": {freqTestID}, "request_id": {"r1"}})
	h.as(t, r, "listener", freqTestListenerPIAL)
	w := httptest.NewRecorder()
	h.eventsPost(w, r)
	// A page can't show an error status, so the refusal goes out as a toast.
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204 with the refusal on the stream, got %d %s", w.Code, w.Body.String())
	}
}

func TestFrequencyGoLiveRedirectsToTheStage(t *testing.T) {
	stub := &auralisStub{view: freqViewJSON("live")}
	h, _ := frequencyTestHandler(t, stub)

	r := eventRequest(url.Values{"event_type": {"frequency.go_live"}, "title": {"Late Night Tech Talk"}})
	r.Header.Del("HX-Request")
	h.as(t, r, "host", freqTestHostPIAL)
	w := httptest.NewRecorder()
	h.eventsPost(w, r)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/frequencies/"+freqTestID {
		t.Fatalf("expected 303 to the stage, got %d %q", w.Code, w.Header().Get("Location"))
	}
	stub.mu.Lock()
	joined := strings.Join(stub.calls, "\n")
	stub.mu.Unlock()
	if !strings.Contains(joined, "POST /v1/frequencies pial="+freqTestHostPIAL) || !strings.Contains(joined, "/start pial="+freqTestHostPIAL) {
		t.Fatalf("go_live must create then start:\n%s", joined)
	}

	// The htmx composer (targets #main-col) gets the stage body and the URL
	// to push, with the dock riding along out-of-band.
	requirePartials(t, h, "frequency_stage", "frequency_dock")
	r2 := eventRequest(url.Values{"event_type": {"frequency.go_live"}, "title": {"Again"}})
	h.as(t, r2, "host", freqTestHostPIAL)
	w2 := httptest.NewRecorder()
	h.eventsPost(w2, r2)
	if w2.Header().Get("HX-Push-Url") != "/frequencies/"+freqTestID {
		t.Fatalf("htmx go_live must push the stage URL, got %d %v", w2.Code, w2.Header())
	}
	b2 := w2.Body.String()
	if !strings.Contains(b2, `id="main-col"`) || !strings.Contains(b2, frequencyFacetID(freqTestID, "stage")) || !strings.Contains(b2, `id="frequency-dock"`) {
		t.Fatalf("htmx go_live must answer the stage body + dock: %s", b2[:min(400, len(b2))])
	}
}

// The templates are real: every Frequency Facet renders from this brain's
// types under the real funcMap, so a contract drift fails here, not on a page.
func TestFrequencyTemplatesRenderFromGoTypes(t *testing.T) {
	stub := &auralisStub{view: freqViewJSON("live")}
	h, _ := frequencyTestHandler(t, stub)
	requirePartials(t, h, "frequency_stage", "frequency_dock", "frequency_dock_mini", "frequency_preview", "frequency_card", "frequency_lanes", "live_go_composer")

	var ans auralis.Answer
	if err := json.Unmarshal([]byte(freqViewJSON("live")), &ans); err != nil {
		t.Fatal(err)
	}
	viewer := &model.User{ID: "u-l", Handle: "listener", PIALID: freqTestListenerPIAL, ContentSetting: "default"}
	stage := h.buildFrequencyStage(&ans.Frequency, freqTestListenerPIAL, viewer, map[string]FrequencyPerson{
		freqTestHostPIAL:     {Handle: "host", DisplayName: "Host"},
		freqTestListenerPIAL: {Handle: "listener", DisplayName: "Listener"},
	})
	if stage.PeopleCount != 2 || stage.RequestCount != 1 || !stage.Viewer.InFrequency || stage.Captions == nil {
		t.Fatalf("stage counts: %+v", stage)
	}
	for _, slot := range []string{"stage", "dock", "dock_mini", "preview", "header", "speakers", "listener_count", "live_badge", "controls", "request_button", "requests", "status", "recording"} {
		frag, err := h.renderFrequencySlot(slot, stage)
		if err != nil {
			t.Fatalf("slot %s: %v", slot, err)
		}
		idSlot := slot
		if slot == "dock_mini" {
			idSlot = "dock" // the mini dock keeps the dock's id so mutations land on either
		}
		if !strings.Contains(frag, frequencyFacetID(freqTestID, idSlot)) && slot != "requests" && slot != "request_button" && slot != "speakers" && slot != "header" && slot != "status" && slot != "recording" && slot != "listener_count" && slot != "live_badge" {
			t.Fatalf("slot %s does not carry its facet id: %s", slot, frag[:min(200, len(frag))])
		}
		if strings.Contains(frag, freqTestHostPIAL) || strings.Contains(frag, freqTestListenerPIAL) {
			t.Fatalf("slot %s leaks a PIAL", slot)
		}
	}
	card := h.buildFrequencyCard(&ans.Frequency.Frequency, ans.Frequency.Counts, FrequencyPerson{Handle: "host", DisplayName: "Host"}, viewer)
	if _, err := h.renderFrequencyFragment("frequency_card", card); err != nil {
		t.Fatalf("card: %v", err)
	}
	if _, err := h.renderFrequencyFragment("frequency_lanes", map[string]interface{}{"Live": []FrequencyCardData{card}, "Scheduled": []FrequencyCardData{}, "Ended": []FrequencyCardData{}, "Ctx": h.workCardCtx(viewer, "frequencies")}); err != nil {
		t.Fatalf("lanes: %v", err)
	}
	// The composer in Frequency mode, blank and after a rejection.
	for _, goData := range []map[string]interface{}{nil, frequencyGoData(viewer, frequencyGoDraft{Title: "x", Error: "no"})} {
		if _, err := h.renderFrequencyFragment("live_go_composer", map[string]interface{}{
			"Handle": "host", "CanNSFW": false, "Error": "", "Title": "", "Description": "", "Source": "browser",
			"FrequencyMode": true, "FrequencyGo": goData, "FrequenciesLive": []FrequencyCardData{card},
		}); err != nil {
			t.Fatalf("composer (FrequencyGo=%v): %v", goData != nil, err)
		}
	}
	// The Shell dock helper: a live Frequency renders the dock, nothing renders the mount.
	if dock := h.frequencyDock(httptest.NewRequest("GET", "/", nil).Context(), viewer); !strings.Contains(string(dock), frequencyFacetID(freqTestID, "dock")) {
		t.Fatalf("frequencyDock did not render the live dock: %s", dock)
	}
	if dock := h.frequencyDock(httptest.NewRequest("GET", "/", nil).Context(), nil); string(dock) != "" {
		t.Fatalf("frequencyDock for nobody must be empty: %s", dock)
	}
}

func TestFrequencyGoLiveRefusesUntitled(t *testing.T) {
	stub := &auralisStub{view: freqViewJSON("live")}
	h, _ := frequencyTestHandler(t, stub)
	requirePartials(t, h, "live_go_composer")
	r := eventRequest(url.Values{"event_type": {"frequency.go_live"}, "title": {"   "}})
	h.as(t, r, "host", freqTestHostPIAL)
	w := httptest.NewRecorder()
	h.eventsPost(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("untitled must be refused with the composer re-rendered, got %d", w.Code)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	for _, c := range stub.calls {
		if strings.Contains(c, "POST /v1/frequencies") {
			t.Fatalf("Auralis must not be asked to create an untitled Frequency: %s", c)
		}
	}
}

func TestPublishFrequencyStateRendersPerViewer(t *testing.T) {
	stub := &auralisStub{view: freqViewJSON("live")}
	h, _ := frequencyTestHandler(t, stub)
	requirePartials(t, h, "frequency_controls", "frequency_request_button", "frequency_request_queue", "frequency_header", "frequency_speaker_grid", "frequency_listener_count", "frequency_live_badge", "frequency_status", "frequency_recording_badge", "frequency_card")

	hostCh, _, hostDone := RegisterSSESession("u-host", freqTestHostPIAL)
	defer hostDone()
	lisCh, _, lisDone := RegisterSSESession("u-lis", freqTestListenerPIAL)
	defer lisDone()

	h.PublishFrequencyState(httptest.NewRequest("GET", "/", nil).Context(), freqTestID)

	drain := func(ch <-chan SSEEvent) []string {
		var out []string
		for {
			select {
			case ev := <-ch:
				out = append(out, ev.Data)
			case <-time.After(300 * time.Millisecond):
				return out
			}
		}
	}
	hostFrags, lisFrags := drain(hostCh), drain(lisCh)
	find := func(frags []string, slot string) string {
		for _, f := range frags {
			if strings.Contains(f, frequencyFacetID(freqTestID, slot)) {
				return f
			}
		}
		return ""
	}
	hc, lc := find(hostFrags, "controls"), find(lisFrags, "controls")
	if hc == "" || lc == "" {
		t.Fatalf("both present participants must receive controls: host=%d listener=%d fragments", len(hostFrags), len(lisFrags))
	}
	if hc == lc {
		t.Fatal("controls must be rendered per viewer; host and listener got identical fragments")
	}
	ht, lt := find(hostFrags, "listener_count"), find(lisFrags, "listener_count")
	if ht == "" || lt == "" {
		t.Fatal("the tally must reach everyone present")
	}
	for _, f := range append(hostFrags, lisFrags...) {
		if strings.Contains(f, freqTestHostPIAL) || strings.Contains(f, freqTestListenerPIAL) {
			t.Fatal("a PIAL leaked into a pushed fragment")
		}
	}
}

// The presence beat rides on its own leaf, never on a dock root. htmx hands
// hx-vals, hx-post and hx-trigger down to every descendant: a beat on the
// root rewrote the event_type of every form inside the dock — Mute, Lock, End
// Frequency, Leave — into a heartbeat, and the tally the server answered with
// replaced the whole dock in the Shell, leaving a bare "nobody listening yet"
// standing as a fourth column on every page.
func TestFrequencyDockRootsCarryNoBeat(t *testing.T) {
	stub := &auralisStub{view: freqViewJSON("live")}
	h, _ := frequencyTestHandler(t, stub)
	requirePartials(t, h, "frequency_dock", "frequency_dock_mini", "frequency_listener_count")

	var ans auralis.Answer
	if err := json.Unmarshal([]byte(freqViewJSON("live")), &ans); err != nil {
		t.Fatal(err)
	}
	people := map[string]FrequencyPerson{
		freqTestHostPIAL:     {Handle: "host", DisplayName: "Host"},
		freqTestListenerPIAL: {Handle: "listener", DisplayName: "Listener"},
	}
	for _, who := range []struct{ handle, pial string }{{"host", freqTestHostPIAL}, {"listener", freqTestListenerPIAL}} {
		viewer := &model.User{ID: "u-" + who.handle, Handle: who.handle, PIALID: who.pial, ContentSetting: "default"}
		stage := h.buildFrequencyStage(&ans.Frequency, who.pial, viewer, people)
		for _, slot := range []string{freqSlotDock, freqSlotDockMini} {
			frag, err := h.renderFrequencySlot(slot, stage)
			if err != nil {
				t.Fatalf("%s %s: %v", who.handle, slot, err)
			}
			root := frag[:strings.IndexByte(frag, '>')]
			for _, attr := range []string{"hx-vals", "hx-post", "hx-get", "hx-trigger", "hx-swap", "hx-target"} {
				if strings.Contains(root, attr) {
					t.Fatalf("%s %s: the root carries %s, which every form inside would inherit:\n%s", who.handle, slot, attr, root)
				}
			}
			if !strings.Contains(frag, `"event_type":"frequency.heartbeat"`) || strings.Contains(frag[strings.Index(frag, "freq-listeners"):], `hx-trigger="every 15s"`) {
				t.Fatalf("%s %s: a participant's dock must beat from a leaf outside the tally:\n%s", who.handle, slot, frag)
			}
		}
	}
}

// A beat swaps nothing. Once a person is gone it clears their dock over FA Live.
func TestFrequencyHeartbeatSwapsNothing(t *testing.T) {
	beat := func(name string, stub *auralisStub, wantDockGone bool) {
		t.Run(name, func(t *testing.T) {
			h, _ := frequencyTestHandler(t, stub)
			ch, _, done := RegisterSSESession("u-lis", freqTestListenerPIAL)
			defer done()
			r := eventRequest(url.Values{"event_type": {"frequency.heartbeat"}, "frequency_id": {freqTestID}})
			h.as(t, r, "listener", freqTestListenerPIAL)
			w := httptest.NewRecorder()
			h.eventsPost(w, r)
			if w.Code != http.StatusNoContent {
				t.Fatalf("heartbeat answered %d: %s", w.Code, w.Body.String())
			}
			gotDock := false
			for drained := false; !drained; {
				select {
				case ev := <-ch:
					if strings.Contains(ev.Data, frequencyFacetID(freqTestID, freqSlotDock)) {
						gotDock = true
					}
				case <-time.After(200 * time.Millisecond):
					drained = true
				}
			}
			if gotDock != wantDockGone {
				t.Fatalf("dock removal published = %v, want %v", gotDock, wantDockGone)
			}
		})
	}
	beat("present", &auralisStub{view: freqViewJSON("live"), heartbeat: `{"state":"live","present":true,"role":"listener","muted":false,"counts":{"listeners":3,"speakers":1,"participants":4}}`}, false)
	beat("gone", &auralisStub{view: freqViewJSON("ended"), heartbeat: `{"state":"ended","present":false,"role":"listener","muted":false,"counts":null}`}, true)
}
