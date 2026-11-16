package handler

import (
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/f33d3r/feed-engine/internal/config"
	"github.com/f33d3r/feed-engine/internal/model"
)

// Ain Soph is the wallet brain. It used to take the identity whose money moves
// straight out of the request body — `withdraw.user_id`, `transfer.from_user_id`
// — with no authentication of any kind on its router, so a request could name
// whose balance it drained. That is fixed inside Ain Soph, where the acting
// identity is now derived from the authenticated caller.
//
// These tests pin the half of that contract feed-engine owns. There is no
// browser proxy to Ain Soph any more: feed-engine makes every call itself, with
// the session's PIAL in `X-Pial-Identity`, and the body names no identity at all.
// The browser never learns a PIAL, so it has none to forge.

const injectionSessionToken = "test-session-ainsoph-injection"

// authedHandler builds a Handler that resolves exactly one session token to the
// given user, without touching a database. The session cache is consulted before
// any query, so a non-nil *sql.DB that is never dialled is enough.
func authedHandler(t *testing.T, upstreamURL string, user *model.User) *Handler {
	t.Helper()

	// sql.Open does not connect; it only validates the driver name. No query is
	// ever issued because the session cache short-circuits first.
	db, err := sql.Open("postgres", "postgres://unused@127.0.0.1:1/unused")
	if err != nil {
		t.Skipf("postgres driver unavailable in this build: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	h := &Handler{
		cfg: &config.Config{AinSophURL: upstreamURL, InternalAPIKey: "test-internal-key"},
		db:  db,
	}
	h.sessionCache.Store(injectionSessionToken, sessionEntry{
		user: user,
		exp:  time.Now().Add(time.Hour),
	})
	return h
}

// TestAinSophPostActsAsTheSession pins that a governance write reaches Ain Soph
// as the identity the caller passed (the session's), with no identity in the
// body for the brain to be tempted by.
func TestAinSophPostActsAsTheSession(t *testing.T) {
	const callerPIAL = "c0ffee00-0000-4000-8000-000000000001"

	var gotIdentity, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIdentity = r.Header.Get("X-Pial-Identity")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(upstream.Close)

	h := authedHandler(t, upstream.URL, &model.User{ID: "acct", PIALID: callerPIAL, Handle: "voter"})
	h.httpClient = upstream.Client()
	status, msg, err := h.ainSophPost(httptest.NewRequest(http.MethodPost, "/events", nil).Context(),
		callerPIAL, "/v1/proposals/00000000-0000-4000-8000-000000000001/vote",
		map[string]interface{}{"option": "yes"})
	if err != nil {
		t.Fatalf("ainSophPost: %v", err)
	}
	if status != http.StatusCreated || msg != "" {
		t.Fatalf("status=%d msg=%q, want 201 and no refusal", status, msg)
	}
	if gotIdentity != callerPIAL {
		t.Fatalf("X-Pial-Identity = %q, want the session's PIAL", gotIdentity)
	}
	if strings.Contains(gotBody, "voter_id") || strings.Contains(gotBody, callerPIAL) {
		t.Fatalf("the body names an identity: %s", gotBody)
	}
}

// TestAinSophPostReportsTheRefusal pins that a refusal is the brain's own
// message, never a silent success.
func TestAinSophPostReportsTheRefusal(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"already_voted","message":"You already voted."}`))
	}))
	t.Cleanup(upstream.Close)

	h := authedHandler(t, upstream.URL, &model.User{ID: "acct", PIALID: "p", Handle: "voter"})
	h.httpClient = upstream.Client()
	status, msg, err := h.ainSophPost(httptest.NewRequest(http.MethodPost, "/events", nil).Context(),
		"p", "/v1/proposals/x/vote", map[string]interface{}{"option": "yes"})
	if err != nil {
		t.Fatalf("ainSophPost: %v", err)
	}
	if status != http.StatusConflict || msg != "You already voted." {
		t.Fatalf("status=%d msg=%q", status, msg)
	}
}
