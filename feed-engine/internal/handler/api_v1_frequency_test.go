package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/f33d3r/feed-engine/internal/auralis"
	"github.com/f33d3r/feed-engine/internal/model"
)

// apiFreqStub is the Auralis stub with the two routes the JSON lane reads
// that the Facet tests do not: the lane list and the host's open Frequency.
type apiFreqStub struct {
	*auralisStub
	listJSON     string
	hostOpenJSON string
}

func (s *apiFreqStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/frequencies":
		s.auralisStub.mu.Lock()
		s.auralisStub.calls = append(s.auralisStub.calls, "GET /v1/frequencies?"+r.URL.RawQuery)
		s.auralisStub.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(s.listJSON))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/hosts/"):
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(s.hostOpenJSON))
	default:
		s.auralisStub.ServeHTTP(w, r)
	}
}

func freqListJSON() string {
	return `{"lane":"live","items":[{"frequency":{"id":"` + freqTestID + `","version":3,"host_pial_id":"` + freqTestHostPIAL + `","title":"Late Night Tech Talk","description":"fees","state":"live","visibility":"public","language":"en","adult_content":false,"speaker_verity_min_tier":0,"scheduled_at":null,"started_at":"2026-09-06T21:19:54Z","ended_at":null,"end_reason":null,"recording_enabled":false,"replay_status":"none","max_speakers":10,"max_listeners":1000,"requests_open":true,"locked":false,"media_node":"n1","created_at":"2026-09-06T21:19:54Z","updated_at":"2026-09-06T21:19:54Z"},"counts":{"listeners":1,"speakers":0,"participants":2}}]}`
}

func apiFrequencyTestHandler(t *testing.T, view string) (*Handler, *apiFreqStub) {
	t.Helper()
	inner := &auralisStub{view: view}
	stub := &apiFreqStub{auralisStub: inner, listJSON: freqListJSON(), hostOpenJSON: `{"frequency":null}`}
	// The real constructor gives templates, the lazy DB and NEXUS wiring;
	// then the Auralis client is pointed at this lane's wider stub.
	h, _ := frequencyTestHandler(t, inner)
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	h.auralis = auralis.New(srv.URL, "k", time.Second)
	return h, stub
}

// asAPI signs a request in as an account the JSON lane accepts: requireAPIUser
// insists the account id is a uuid (viewerAccountID), where the web lane's
// test actor uses a readable one.
func (h *Handler) asAPI(t *testing.T, r *http.Request, handle, pial string) {
	t.Helper()
	tok := "api-tok-" + handle
	id := "a0000000-0000-4000-8000-00000000000" + string(rune('1'+len(handle)%8))
	h.sessionCache.Store(tok, sessionEntry{
		user: &model.User{ID: id, Handle: handle, DisplayName: handle, PIALID: pial, ContentSetting: "default"},
		exp:  time.Now().Add(time.Minute),
	})
	r.Header.Set("Authorization", "Bearer "+tok)
}

func apiGet(t *testing.T, h *Handler, path, handle, pial string, fn func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Accept", "application/json")
	h.asAPI(t, r, handle, pial)
	w := httptest.NewRecorder()
	fn(w, r)
	return w
}

func TestAPIV1FrequencyListShapesLaneAndOwn(t *testing.T) {
	h, stub := apiFrequencyTestHandler(t, freqViewJSON("live"))
	stub.hostOpenJSON = `{"frequency":{"id":"` + freqTestID + `","version":3,"host_pial_id":"` + freqTestHostPIAL + `","title":"Late Night Tech Talk","description":"fees","state":"live","visibility":"public","language":"en","adult_content":false,"speaker_verity_min_tier":0,"scheduled_at":null,"started_at":"2026-09-06T21:19:54Z","ended_at":null,"end_reason":null,"recording_enabled":false,"replay_status":"none","max_speakers":10,"max_listeners":1000,"requests_open":true,"locked":false,"media_node":"n1","created_at":"2026-09-06T21:19:54Z","updated_at":"2026-09-06T21:19:54Z"}}`

	w := apiGet(t, h, "/api/v1/frequencies?lane=live", "host", freqTestHostPIAL, h.requireAPIUser(h.apiV1Frequencies))
	if w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var out FrequencyListDTO
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Count != 1 || len(out.Frequencies) != 1 || out.Frequencies[0].ID != freqTestID {
		t.Fatalf("list shape wrong: %s", w.Body.String())
	}
	f := out.Frequencies[0]
	if f.State != "live" || f.ListenerCount != 1 || f.PeopleCount != 2 || f.Visibility != "public" {
		t.Fatalf("frequency dto wrong: %+v", f)
	}
	if out.Own == nil || out.Own.ID != freqTestID {
		t.Fatalf("own must be the host's open frequency: %s", w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, freqTestHostPIAL) || strings.Contains(body, "pial") {
		t.Fatalf("a PIAL leaked into the JSON lane: %s", body)
	}
	for _, key := range []string{`"host":`, `"listener_count":`, `"speaker_count":`, `"people_count":`, `"is_nsfw":`, `"requests_open":`} {
		if !strings.Contains(body, key) {
			t.Fatalf("missing key %s in %s", key, body)
		}
	}

	bad := apiGet(t, h, "/api/v1/frequencies?lane=trending", "host", freqTestHostPIAL, h.requireAPIUser(h.apiV1Frequencies))
	if bad.Code != 400 {
		t.Fatalf("unknown lane must be 400, got %d", bad.Code)
	}
}

func TestAPIV1FrequencyRoomHidesQueueFromListenerShowsHost(t *testing.T) {
	h, _ := apiFrequencyTestHandler(t, freqViewJSON("live"))

	// The stub's view says the viewer is the listener with a pending request
	// in the queue (r1). A listener does not see the queue, but sees their own
	// request on viewer.request.
	w := apiGet(t, h, "/api/v1/frequencies/"+freqTestID, "listener", freqTestListenerPIAL, func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("id", freqTestID)
		h.requireAPIUser(h.apiV1FrequencyRoom)(w, r)
	})
	if w.Code != 200 {
		t.Fatalf("room: %d %s", w.Code, w.Body.String())
	}
	var room FrequencyRoomDTO
	if err := json.Unmarshal(w.Body.Bytes(), &room); err != nil {
		t.Fatal(err)
	}
	if len(room.Requests) != 0 {
		t.Fatalf("a listener must not see the queue: %+v", room.Requests)
	}
	if room.Viewer.Role == nil || *room.Viewer.Role != "listener" || !room.Viewer.InFrequency || room.Viewer.CanModerate {
		t.Fatalf("listener standing wrong: %+v", room.Viewer)
	}
	if room.Session != nil {
		t.Fatal("a GET must not mint a session")
	}
	if len(room.Speakers) != 1 || room.Speakers[0].Role != "host" || room.Speakers[0].Author.Handle == "" && room.Speakers[0].Author.DisplayName != "" {
		t.Fatalf("speakers wrong: %+v", room.Speakers)
	}

	// The host sees the queue. The stub's viewer block names the listener, so
	// the host's standing is derived — and derivation says host.
	w2 := apiGet(t, h, "/api/v1/frequencies/"+freqTestID, "host", freqTestHostPIAL, func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("id", freqTestID)
		h.requireAPIUser(h.apiV1FrequencyRoom)(w, r)
	})
	var hostRoom FrequencyRoomDTO
	if err := json.Unmarshal(w2.Body.Bytes(), &hostRoom); err != nil {
		t.Fatal(err)
	}
	if !hostRoom.Viewer.CanModerate || !hostRoom.Viewer.CanEnd || len(hostRoom.Requests) != 1 || hostRoom.Requests[0].Reason != "I run a node in Lagos" || hostRoom.Requests[0].Upvotes != 1 {
		t.Fatalf("host must see the queue with reasons: %s", w2.Body.String())
	}
	if strings.Contains(w2.Body.String(), freqTestListenerPIAL) {
		t.Fatal("a PIAL leaked into the room DTO")
	}
}

func TestAPIV1FrequencyNativeTuneInReturnsSession(t *testing.T) {
	h, stub := apiFrequencyTestHandler(t, freqViewJSON("live"))

	body := `{"event_type":"frequency.tune_in","frequency_id":"` + freqTestID + `"}`
	r := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")
	h.as(t, r, "listener", freqTestListenerPIAL)
	w := httptest.NewRecorder()
	h.eventsPost(w, r)
	if w.Code != 200 {
		t.Fatalf("native tune in: %d %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("native answer must be JSON, got %q: %s", ct, w.Body.String())
	}
	var room FrequencyRoomDTO
	if err := json.Unmarshal(w.Body.Bytes(), &room); err != nil {
		t.Fatalf("decode: %v: %s", err, w.Body.String())
	}
	if room.Session == nil || room.Session.Token != "abc.def" || room.Session.SignalPath != "/frequencies/signal/"+freqTestID || len(room.Session.Permissions) != 1 {
		t.Fatalf("session missing from native tune in: %s", w.Body.String())
	}
	if room.Frequency.ID != freqTestID || !room.Viewer.InFrequency {
		t.Fatalf("room wrong: %s", w.Body.String())
	}
	stub.auralisStub.mu.Lock()
	calls := strings.Join(stub.auralisStub.calls, "\n")
	stub.auralisStub.mu.Unlock()
	if !strings.Contains(calls, "POST /v1/frequencies/"+freqTestID+"/tune_in pial="+freqTestListenerPIAL) {
		t.Fatalf("Auralis was not asked to admit the listener:\n%s", calls)
	}

	// A refusal comes back in the JSON envelope with Auralis's code.
	stub.auralisStub.refuse = map[string]int{"/approve": 403}
	r2 := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(`{"event_type":"frequency.approve","frequency_id":"`+freqTestID+`","request_id":"r1"}`))
	r2.Header.Set("Content-Type", "application/json")
	r2.Header.Set("Accept", "application/json")
	h.as(t, r2, "listener", freqTestListenerPIAL)
	w2 := httptest.NewRecorder()
	h.eventsPost(w2, r2)
	if w2.Code != 403 || !strings.Contains(w2.Body.String(), `"code":"forbidden"`) {
		t.Fatalf("native refusal must be the JSON envelope: %d %s", w2.Code, w2.Body.String())
	}
}

func TestAPIV1FrequencyEventsStreamEmitsRoomOnPublish(t *testing.T) {
	h, _ := apiFrequencyTestHandler(t, freqViewJSON("live"))
	requirePartials(t, h, "frequency_stage", "frequency_dock", "frequency_controls", "frequency_request_button", "frequency_request_queue", "frequency_header", "frequency_speaker_grid", "frequency_listener_count", "frequency_live_badge", "frequency_status", "frequency_recording_badge", "frequency_card")

	// The host's channel, as a phone with the room open would hold it.
	hostCh, _, hostDone := RegisterSSESession("u-host", freqTestHostPIAL)
	defer hostDone()

	var ans auralis.Answer
	if err := json.Unmarshal([]byte(freqViewJSON("live")), &ans); err != nil {
		t.Fatal(err)
	}
	h.publishFrequencyView(&ans.Frequency)

	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-hostCh:
			if ev.Type != sseFrequencyJSON {
				continue
			}
			var frame frequencyJSONFrame
			if err := json.Unmarshal([]byte(ev.JSON), &frame); err != nil {
				t.Fatalf("bad frame: %v: %s", err, ev.JSON)
			}
			if frame.FrequencyID != freqTestID || frame.Event != sseFrequencyJSON {
				t.Fatalf("frame wrong: %+v", frame)
			}
			var room FrequencyRoomDTO
			if err := json.Unmarshal(frame.Data, &room); err != nil {
				t.Fatal(err)
			}
			if room.Viewer.Role == nil || *room.Viewer.Role != "host" || len(room.Requests) != 1 {
				t.Fatalf("host's room frame wrong: %s", string(frame.Data))
			}
			return
		case <-deadline:
			t.Fatal("host never received frequency_json on the native channel")
		}
	}
}

func TestAPIV1FrequencyEndedFrameOnOver(t *testing.T) {
	h, _ := apiFrequencyTestHandler(t, freqViewJSON("ended"))
	requirePartials(t, h, "frequency_stage", "frequency_dock", "frequency_card")

	hostCh, _, hostDone := RegisterSSESession("u-host", freqTestHostPIAL)
	defer hostDone()

	var ans auralis.Answer
	if err := json.Unmarshal([]byte(freqViewJSON("ended")), &ans); err != nil {
		t.Fatal(err)
	}
	h.publishFrequencyView(&ans.Frequency)

	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-hostCh:
			if ev.Type != sseFrequencyJSON {
				continue
			}
			var frame frequencyJSONFrame
			if err := json.Unmarshal([]byte(ev.JSON), &frame); err != nil {
				t.Fatal(err)
			}
			if frame.Event == sseFrequencyEnded {
				return
			}
		case <-deadline:
			t.Fatal("no frequency_ended frame for an ended Frequency")
		}
	}
}
