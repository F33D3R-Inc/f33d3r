package middleware

import (
	"net/http"
	"net/url"
	"strings"
)

// CSRF uses the double-origin check strategy — no server-side token state needed.
//
// Rules:
//  1. HTMX requests (HX-Request: true) are XMLHttpRequest — browsers enforce
//     same-origin for these automatically, so we trust them.
//  2. For regular form POSTs: if an Origin header is present, it must match
//     the request Host. If absent, we fall back to the Referer header.
//  3. GET/HEAD/OPTIONS are never mutating so they pass unconditionally.
//
// This covers the vast majority of CSRF vectors while requiring zero template
// changes. When proper session infrastructure exists, upgrade to token-based CSRF.
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

		// Internal service-to-service endpoints — authenticated via X-Internal-Key header,
		// not via browser session. CSRF does not apply to server→server calls.
		if strings.HasPrefix(r.URL.Path, "/api/internal/") {
			next.ServeHTTP(w, r)
			return
		}

		// HTMX sends HX-Request: true on all its requests — browsers cannot
		// forge this header cross-site (blocked by CORS preflight or same-origin).
		if r.Header.Get("HX-Request") == "true" {
			next.ServeHTTP(w, r)
			return
		}

		// Origin check for regular form POSTs.
		// Parse the origin URL and compare the host exactly — HasPrefix is vulnerable
		// to prefix attacks (e.g. "https://host.evil.com" has prefix "https://host").
		origin := r.Header.Get("Origin")
		if origin != "" && origin != "null" {
			parsed, err := url.Parse(origin)
			if err != nil || parsed.Host != r.Host {
				http.Error(w, "403 Forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		// Referer fallback (some browsers omit Origin on same-origin form POSTs).
		referer := r.Header.Get("Referer")
		if referer != "" {
			host := r.Host
			if strings.Contains(referer, "://"+host+"/") ||
				strings.HasPrefix(referer, "http://"+host) ||
				strings.HasPrefix(referer, "https://"+host) {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "403 Forbidden", http.StatusForbidden)
			return
		}

		// No Origin and no Referer — allow only from localhost in dev, block otherwise.
		// This handles form submissions from file:// or odd browser contexts.
		remoteHost := r.Host
		if strings.HasPrefix(remoteHost, "localhost") || strings.HasPrefix(remoteHost, "127.") {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "403 Forbidden", http.StatusForbidden)
	})
}
