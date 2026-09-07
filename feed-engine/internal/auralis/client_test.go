package auralis

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const pial = "c0ffee00-0000-4000-8000-000000000001"

// stub answers like Auralis and records what it was sent.
func stub(t *testing.T, status int, body string, seen *http.Request) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = *r.Clone(context.Background())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

const answerJSON = `{"frequency":{"frequency":{"id":"f1","version":3,"host_pial_id":"` + pial + `","title":"t","description":"","state":"live","visibility":"public","language":"en","adult_content":false,"speaker_verity_min_tier":0,"scheduled_at":null,"started_at":"2026-09-06T21:19:54Z","ended_at":null,"end_reason":null,"recording_enabled":false,"replay_status":"none","max_speakers":10,"max_listeners":1000,"requests_open":true,"locked":false,"media_node":"n1","created_at":"2026-09-06T21:19:54Z","updated_at":"2026-09-06T21:19:54Z"},"counts":{"listeners":1,"speakers":0,"participants":2},"speakers":[{"pial_id":"` + pial + `","role":"host","muted":false,"present":true,"joined_at":"2026-09-06T21:19:54Z"}],"cohost_pial_ids":[],"requests":[{"id":"r1","pial_id":"x","reason":"Lagos","upvotes":2,"created_at":"2026-09-06T21:19:55Z"}],"present_pial_ids":["` + pial + `","x"],"lease_node":"n1","viewer":{"pial_id":"x","role":"listener","muted":false,"present":true,"blocked":false,"request":null,"can_request_mic":true,"can_moderate":false,"can_end":false,"can_speak":false,"can_listen":true}},"session":{"session_id":"s1","token":"abc.def","expires_at":1,"signal_path":"/frequencies/signal/f1","role":"listener","permissions":["subscribe"]},"rejoined":false}`

func TestHeadersAndDecoding(t *testing.T) {
	var seen http.Request
	srv := stub(t, 200, answerJSON, &seen)
	defer srv.Close()
	c := New(srv.URL, "s3cret", time.Second)

	ans, err := c.TuneIn(context.Background(), "x", "idem-1", "f1")
	if err != nil {
		t.Fatalf("tune in: %v", err)
	}
	if seen.Header.Get("X-Internal-Key") != "s3cret" {
		t.Fatalf("internal key not sent: %q", seen.Header.Get("X-Internal-Key"))
	}
	if seen.Header.Get("X-Pial-Identity") != "x" {
		t.Fatalf("pial not sent: %q", seen.Header.Get("X-Pial-Identity"))
	}
	if seen.Header.Get("Idempotency-Key") != "idem-1" {
		t.Fatalf("idempotency key not sent")
	}
	if seen.URL.Path != "/v1/frequencies/f1/tune_in" || seen.Method != http.MethodPost {
		t.Fatalf("wrong route: %s %s", seen.Method, seen.URL.Path)
	}
	f := ans.Frequency.Frequency
	if f.ID != "f1" || f.State != "live" || !f.IsLive() || f.HostPialID != pial || f.StartedAt == nil {
		t.Fatalf("summary decoded wrong: %+v", f)
	}
	if ans.Frequency.Counts.Listeners != 1 || ans.Frequency.Counts.Participants != 2 {
		t.Fatalf("counts: %+v", ans.Frequency.Counts)
	}
	if len(ans.Frequency.Requests) != 1 || ans.Frequency.Requests[0].Reason != "Lagos" {
		t.Fatalf("requests: %+v", ans.Frequency.Requests)
	}
	if ans.Frequency.Viewer == nil || ans.Frequency.Viewer.Role == nil || *ans.Frequency.Viewer.Role != "listener" || !ans.Frequency.Viewer.CanRequestMic {
		t.Fatalf("viewer: %+v", ans.Frequency.Viewer)
	}
	if ans.Session == nil || ans.Session.Token != "abc.def" || ans.Session.Permissions[0] != "subscribe" {
		t.Fatalf("session: %+v", ans.Session)
	}
}

func TestServiceReadSendsNoIdentity(t *testing.T) {
	var seen http.Request
	srv := stub(t, 200, answerJSON, &seen)
	defer srv.Close()
	c := New(srv.URL, "k", time.Second)
	if _, err := c.Get(context.Background(), "f1", ""); err != nil {
		t.Fatal(err)
	}
	if _, present := seen.Header["X-Pial-Identity"]; present {
		t.Fatal("anonymous read must not carry X-Pial-Identity")
	}
}

func TestRefusalsCarryTheCode(t *testing.T) {
	var seen http.Request
	srv := stub(t, 409, `{"error":"requests_closed","message":"conflict: requests_closed"}`, &seen)
	defer srv.Close()
	c := New(srv.URL, "k", time.Second)
	_, err := c.RequestMic(context.Background(), "x", "f1", "why", 0)
	if !IsConflict(err) || Code(err) != "requests_closed" || IsOutage(err) {
		t.Fatalf("got %v", err)
	}
	var body map[string]interface{}
	_ = json.NewDecoder(seen.Body).Decode(&body)
}

func TestForbiddenAndNotFound(t *testing.T) {
	var seen http.Request
	srv := stub(t, 403, `{"error":"forbidden","message":"listener may not ApproveSpeaker"}`, &seen)
	defer srv.Close()
	c := New(srv.URL, "k", time.Second)
	_, err := c.Approve(context.Background(), "x", "f1", "r1")
	if !IsForbidden(err) || IsConflict(err) || IsOutage(err) {
		t.Fatalf("got %v", err)
	}
	srv2 := stub(t, 404, `{"error":"not_found","message":"not found"}`, &seen)
	defer srv2.Close()
	_, err = New(srv2.URL, "k", time.Second).Get(context.Background(), "nope", "")
	if !IsNotFound(err) {
		t.Fatalf("got %v", err)
	}
}

func TestOutages(t *testing.T) {
	if _, err := New("", "k", time.Second).Get(context.Background(), "f1", ""); !IsOutage(err) {
		t.Fatalf("unconfigured must be an outage: %v", err)
	}
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	if _, err := New(dead.URL, "k", 200*time.Millisecond).Get(context.Background(), "f1", ""); !IsOutage(err) {
		t.Fatalf("refused connection must be an outage: %v", err)
	}
	var seen http.Request
	srv := stub(t, 500, `{"error":"db_error","message":"x"}`, &seen)
	defer srv.Close()
	if _, err := New(srv.URL, "k", time.Second).Get(context.Background(), "f1", ""); !IsOutage(err) {
		t.Fatalf("500 must be an outage: %v", err)
	}
	srv401 := stub(t, 401, `{"error":"unauthenticated","message":"x"}`, &seen)
	defer srv401.Close()
	if _, err := New(srv401.URL, "wrong", time.Second).Get(context.Background(), "f1", ""); !IsOutage(err) {
		t.Fatalf("rejected key is a deployment fault, not a refusal: %v", err)
	}
}

func TestListAndHostOpen(t *testing.T) {
	var seen http.Request
	srv := stub(t, 200, `{"lane":"live","items":[{"frequency":{"id":"f1","host_pial_id":"h","title":"t","state":"live","created_at":"2026-09-06T21:19:54Z","updated_at":"2026-09-06T21:19:54Z"},"counts":{"listeners":3,"speakers":1,"participants":5}}]}`, &seen)
	defer srv.Close()
	c := New(srv.URL, "k", time.Second)
	l, err := c.List(context.Background(), "live", "", 24)
	if err != nil || len(l.Items) != 1 || l.Items[0].Counts.Listeners != 3 {
		t.Fatalf("list: %v %+v", err, l)
	}
	if seen.URL.Query().Get("lane") != "live" || seen.URL.Query().Get("limit") != "24" {
		t.Fatalf("query: %s", seen.URL.RawQuery)
	}
	srv2 := stub(t, 200, `{"frequency":null}`, &seen)
	defer srv2.Close()
	open, err := New(srv2.URL, "k", time.Second).HostOpen(context.Background(), "h")
	if err != nil || open != nil {
		t.Fatalf("host open: %v %+v", err, open)
	}
	if seen.URL.Path != "/v1/hosts/h/open" {
		t.Fatalf("path %s", seen.URL.Path)
	}
}
