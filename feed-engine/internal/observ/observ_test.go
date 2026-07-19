package observ

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestIDMiddleware_GeneratesAndPropagates(t *testing.T) {
	Init("nantar-test", "info", false)
	defer Shutdown()

	var seen string
	h := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestID(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	h.ServeHTTP(rec, req)

	if seen == "" {
		t.Fatal("expected request id to be generated and placed on context")
	}
	if got := rec.Header().Get(HeaderRequestID); got != seen {
		t.Fatalf("expected response header %q, got %q", seen, got)
	}
}

func TestRequestIDMiddleware_RespectsInbound(t *testing.T) {
	Init("nantar-test", "info", false)
	defer Shutdown()

	const id = "abc-123-def"
	var seen string
	h := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestID(r.Context())
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(HeaderRequestID, id)
	h.ServeHTTP(rec, req)

	if seen != id {
		t.Fatalf("expected request id %q to be preserved; got %q", id, seen)
	}
}

func TestPathNormalisation(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/post/abc-123", "/post/:id"},
		{"/u/edd", "/u/:handle"},
		{"/u/edd/followers", "/u/:handle"},
		{"/static/css/styles.css", "/static/*"},
		{"/cdn/posts/x.webp", "/cdn/*"},
		{"/api/post/like", "/api/post/:action"},
		{"/feed/item/like", "/feed/item/:action"},
		{"/shop/edd", "/shop/:handle"},
		{"/shop/edd/items", "/shop/:handle"},
		{"/api/track/play", "/api/track/:action"},
		{"/api/poll/vote", "/api/poll/:action"},
		{"/kyc/submit", "/kyc/*"},
		{"/admin/users", "/admin/*"},
		{"/settings/profile", "/settings/*"},
		{"/ledger/v1/blocks/42", "/ledger/*"},
		{"/thessalon/v1/tips", "/thessalon/*"},
		{"/nonexistent/path/with/12345678-1234-5678-9abc-123456789abc/embedded", "/nonexistent/path/with/:uuid/embedded"},
		{"/", ""},
		{"/explore", "/explore"},
	}
	for _, c := range cases {
		if got := normalisePath(c.in); got != c.want {
			t.Errorf("normalisePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMetricsHandler_RegistersAndExports(t *testing.T) {
	Init("nantar-test", "info", false)
	defer Shutdown()

	// Drive a single request through the full middleware chain to populate metrics.
	app := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/explore", nil)
		app.ServeHTTP(rec, req)
	}

	// Scrape /metrics
	mrec := httptest.NewRecorder()
	mreq := httptest.NewRequest("GET", "/metrics", nil)
	MetricsHandler().ServeHTTP(mrec, mreq)

	body := mrec.Body.String()
	checks := []string{
		`http_requests_total{`,
		`brain="nantar-test"`,
		`method="GET"`,
		`path="/explore"`,
		`status="200"`,
		`http_request_duration_seconds_bucket{`,
	}
	for _, want := range checks {
		if !strings.Contains(body, want) {
			t.Errorf("expected metrics to contain %q; full body:\n%s", want, body)
		}
	}
}

func TestL_ReturnsLoggerEvenBeforeInit(t *testing.T) {
	// Reset state — simulate a never-initialised process.
	rootLogger.Store(nil)
	defer Init("nantar-test", "info", false)

	if L(context.Background()) == nil {
		t.Fatal("L must never return nil")
	}
}
