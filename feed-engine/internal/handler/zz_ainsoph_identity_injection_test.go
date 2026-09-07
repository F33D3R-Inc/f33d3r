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
// These tests pin the half of that contract feed-engine owns. Ain Soph's
// authorization is only worth what this proxy's header injection is worth: if
// `X-Pial-Identity` were ever forgeable by the browser, or dropped, the brain
// would be back to trusting a body field.

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

func proxyRequest(t *testing.T, h *Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/ainsoph"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: injectionSessionToken})
	// A forged identity header, exactly as a hostile browser would send it.
	req.Header.Set("X-Pial-Identity", "c0ffee00-0000-4000-8000-000000000011")
	rec := httptest.NewRecorder()
	h.ainSophProxy(rec, req)
	return rec
}

// TestAinSophProxyInjectsCallerIdentity is the pin that makes Ain Soph's
// authorization meaningful: whatever the browser claims, the identity that
// reaches the wallet brain is the one this proxy derived from the authenticated
// session — and the session cookie never travels upstream, so the brain cannot
// be tricked into re-deriving a different one.
func TestAinSophProxyInjectsCallerIdentity(t *testing.T) {
	const callerPIAL = "c0ffee00-0000-4000-8000-000000000001"

	var (
		gotIdentity string
		gotCookie   string
		gotBody     string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIdentity = r.Header.Get("X-Pial-Identity")
		gotCookie = r.Header.Get("Cookie")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"completed"}`))
	}))
	defer upstream.Close()

	user := &model.User{
		Role: model.RoleUser, PIALID: callerPIAL, Handle: "audit_caller",
		KYCTier: model.KYCTierFull, IsVerified: true, IsAgeVerified: true,
	}
	h := authedHandler(t, upstream.URL, user)

	// The body names somebody else as the account to debit — the shape of the
	// original attack.
	rec := proxyRequest(t, h, "/v1/withdraw",
		`{"user_id":"c0ffee00-0000-4000-8000-000000000011","amount_cents":100,"idempotency_key":"k"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("proxy refused a permitted caller: got %d, body %q", rec.Code, rec.Body.String())
	}
	if gotIdentity != callerPIAL {
		t.Errorf("upstream saw X-Pial-Identity %q, want the session's PIAL %q — "+
			"a browser-supplied header reached the wallet brain", gotIdentity, callerPIAL)
	}
	if gotCookie != "" {
		t.Errorf("session cookie leaked upstream: %q", gotCookie)
	}
	// The body is forwarded unchanged; Ain Soph is what must refuse to believe
	// it. This assertion documents that division of responsibility rather than
	// asserting the proxy rewrites payloads.
	if !strings.Contains(gotBody, "c0ffee00-0000-4000-8000-000000000011") {
		t.Errorf("proxy altered the request body: %q", gotBody)
	}
}

// TestAinSophValueRoutesRequireIdentityVerification pins the first of the two
// layers: an account that has not completed identity verification cannot reach
// the on/off ramps at all, on either the legacy or the v1 path.
func TestAinSophValueRoutesRequireIdentityVerification(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request reached Ain Soph despite an unverified caller: %s", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	unverified := &model.User{
		Role: model.RoleUser, PIALID: "c0ffee00-0000-4000-8000-000000000001",
		Handle: "audit_unverified", KYCTier: model.KYCTierBasic,
	}
	if unverified.CanMonetize() {
		t.Fatal("test premise broken: a basic-tier user must not satisfy CanMonetize")
	}
	h := authedHandler(t, upstream.URL, unverified)

	for _, path := range []string{"/withdraw", "/v1/withdraw", "/deposit", "/v1/deposit"} {
		rec := proxyRequest(t, h, path, `{"amount_cents":100,"idempotency_key":"k"}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: got %d, want 403 — value route reachable without identity verification",
				path, rec.Code)
		}
	}
}

// TestAinSophValueRouteTableCoversBothPrefixes guards the gate's coverage. Ain
// Soph serves the money routes under two prefixes for backward compatibility,
// and a gate that listed only one would be trivially stepped around by using the
// other.
func TestAinSophValueRouteTableCoversBothPrefixes(t *testing.T) {
	for _, suffix := range []string{"/withdraw", "/v1/withdraw", "/deposit", "/v1/deposit"} {
		if !ainSophValueRoutes[suffix] {
			t.Errorf("%s moves value in or out of the ledger but is not gated", suffix)
		}
	}
}
