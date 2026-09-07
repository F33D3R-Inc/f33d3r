package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type csrfCase struct {
	name    string
	method  string
	path    string
	origin  string
	referer string
	hx      bool
	key     string // X-Internal-Key, the service-to-service header
	host    string
	want    int
}

// TestCSRFOriginIsTheOnlyProof pins the contract: a mutating request passes only
// when the browser itself stamped a matching origin on it. A header the caller
// chose (HX-Request) proves nothing, and a hostile URL that merely contains the
// site host is not a same-origin Referer.
func TestCSRFOriginIsTheOnlyProof(t *testing.T) {
	const host = "f33d3r.com"

	cases := []csrfCase{
		{name: "get is not mutating", method: http.MethodGet, path: "/like", host: host, want: http.StatusOK},
		{name: "same origin post", method: http.MethodPost, path: "/like", origin: "https://f33d3r.com", host: host, want: http.StatusOK},
		{name: "same origin delete", method: http.MethodDelete, path: "/like", origin: "https://f33d3r.com", host: host, want: http.StatusOK},
		{name: "cross origin post", method: http.MethodPost, path: "/like", origin: "https://evil.example", host: host, want: http.StatusForbidden},
		{name: "suffix lookalike origin", method: http.MethodPost, path: "/like", origin: "https://f33d3r.com.evil.example", host: host, want: http.StatusForbidden},
		{name: "opaque origin", method: http.MethodPost, path: "/like", origin: "null", host: host, want: http.StatusForbidden},

		// The defect: HX-Request was an unconditional pass.
		{name: "hx header cannot vouch for a cross origin", method: http.MethodPost, path: "/like", origin: "https://evil.example", hx: true, host: host, want: http.StatusForbidden},
		{name: "hx header cannot vouch for a bare request", method: http.MethodPost, path: "/like", hx: true, host: host, want: http.StatusForbidden},

		// The defect: Referer was matched with strings.Contains.
		{name: "same origin referer", method: http.MethodPost, path: "/like", referer: "https://f33d3r.com/home", host: host, want: http.StatusOK},
		{name: "hostile referer containing the host", method: http.MethodPost, path: "/like", referer: "https://evil.example/?next=https://f33d3r.com/", host: host, want: http.StatusForbidden},

		{name: "no origin no referer in prod", method: http.MethodPost, path: "/like", host: host, want: http.StatusForbidden},
		{name: "no origin no referer on localhost dev", method: http.MethodPost, path: "/like", host: "localhost:8081", want: http.StatusOK},
		// Internal endpoints are exempt only when the service header is present:
		// a browser cannot attach a custom header without a CORS preflight, which
		// feed-engine never grants. The handler still verifies the value.
		{name: "internal service endpoint with service key", method: http.MethodPost, path: "/api/internal/balance-update", key: "service-key", host: host, want: http.StatusOK},
		{name: "internal service endpoint without service key", method: http.MethodPost, path: "/api/internal/balance-update", host: host, want: http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := CSRF(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			r := httptest.NewRequest(tc.method, "http://"+tc.host+tc.path, nil)
			r.Host = tc.host
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.referer != "" {
				r.Header.Set("Referer", tc.referer)
			}
			if tc.hx {
				r.Header.Set("HX-Request", "true")
			}
			if tc.key != "" {
				r.Header.Set("X-Internal-Key", tc.key)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
		})
	}
}
