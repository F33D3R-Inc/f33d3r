package middleware

import (
	"net/http"
	"net/url"
	"strings"
)

// CSRF enforces same-origin on every mutating request using the origin the
// browser stamps on the request itself.
//
// Rules:
//  1. GET/HEAD/OPTIONS are not mutating and pass unconditionally.
//  2. Every other request must prove same-origin: the Origin header must parse
//     and its host must equal the request Host. When Origin is absent (some
//     browsers omit it on same-origin form POSTs) the Referer host is used.
//  3. Internal service-to-service endpoints are authenticated by X-Internal-Key
//     rather than a browser session, so origin does not apply to them.
//
// There is deliberately no header-presence exemption. A request that merely
// carries a header the caller chose — HX-Request: true, say — proves nothing on
// its own: it is only unforgeable while no permissive CORS policy exists on any
// browser-facing origin, an assumption that lives in a different file and can be
// changed without anyone touching this one. FA Live requests are same-origin
// XMLHttpRequests, so they carry Origin on every mutating method and pass rule 2
// with nothing added to any template.
func CSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}

		// Static files — no mutation possible.
		if strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}

		// Internal service-to-service endpoints — authenticated via X-Internal-Key
		// header, not via browser session. CSRF does not apply to server→server calls.
		//
		// The exemption is granted only to a request that actually presents the
		// service header, whose value the handler then verifies. A path alone
		// proves nothing: a browser can POST a form to any path. It cannot attach
		// X-Internal-Key — a custom header forces a CORS preflight, and feed-engine
		// answers no Access-Control-Allow-* on any route — so the header's mere
		// presence is what separates a brain from a browser here.
		if strings.HasPrefix(r.URL.Path, "/api/internal/") {
			if r.Header.Get("X-Internal-Key") == "" {
				http.Error(w, "403 Forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		if origin := r.Header.Get("Origin"); origin != "" {
			if !sameOriginHost(origin, r.Host) {
				http.Error(w, "403 Forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		// Referer fallback (some browsers omit Origin on same-origin form POSTs).
		if referer := r.Header.Get("Referer"); referer != "" {
			if !sameOriginHost(referer, r.Host) {
				http.Error(w, "403 Forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		// No Origin and no Referer — allow only from localhost in dev, block otherwise.
		// This handles form submissions from file:// or odd browser contexts.
		if strings.HasPrefix(r.Host, "localhost") || strings.HasPrefix(r.Host, "127.") {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "403 Forbidden", http.StatusForbidden)
	})
}

// sameOriginHost reports whether raw is an absolute http(s) URL whose host is
// exactly the request host. Parsing is what makes this safe: substring matching
// accepts an attacker URL that merely contains the host ("https://evil.example/
// ?next=https://f33d3r.com/"), and prefix matching accepts "f33d3r.com.evil".
// An opaque or empty origin ("null") has no host and never matches.
func sameOriginHost(raw, host string) bool {
	if host == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return strings.EqualFold(u.Host, host)
}
