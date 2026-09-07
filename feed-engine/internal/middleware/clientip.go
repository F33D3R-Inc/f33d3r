package middleware

import (
	"log"
	"net"
	"net/http"
	"strings"
)

// ClientIP resolves the address a request should be attributed to for every
// security decision made in this process: rate limiting, IP blocks, audit logs.
//
// The proxy headers are honoured only when the TCP peer is the edge. Caddy sits
// on the container network and stamps X-Real-IP on the way through; a request
// whose peer is a public address did not come through Caddy, and any X-Real-IP
// or X-Forwarded-For it carries was written by the client itself. Trusting such
// a header unconditionally hands every caller a free choice of identity — one
// spoofed value per request defeats the limiter, and a block on the admin panel
// stops nothing.
//
// X-Real-IP is the edge's single-value verdict and is preferred. X-Forwarded-For
// is consulted only when X-Real-IP is absent, and only its last hop is taken:
// the last entry is the one the trusted proxy appended, every earlier entry was
// supplied by whoever was upstream of it.
func ClientIP(r *http.Request) string {
	peer := hostOnly(r.RemoteAddr)
	if !IsInternalAddr(peer) {
		return peer
	}
	if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
		return xri
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if last := strings.TrimSpace(parts[len(parts)-1]); last != "" {
			return last
		}
	}
	return peer
}

// IsInternalAddr reports whether addr (an IP, or host:port) is a loopback,
// RFC 1918 / ULA private, or link-local address — the container network and
// the host itself, never the internet.
func IsInternalAddr(addr string) bool {
	ip := net.ParseIP(hostOnly(addr))
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// FromEdge reports whether a request arrived through the reverse proxy — or
// claims to have. Any forwarding header is proof the request crossed a proxy
// hop; a public peer address is proof it did not originate on the container
// network. Either way the caller is outside.
func FromEdge(r *http.Request) bool {
	if r.Header.Get("X-Forwarded-Proto") != "" ||
		r.Header.Get("X-Forwarded-For") != "" ||
		r.Header.Get("X-Real-IP") != "" {
		return true
	}
	return !IsInternalAddr(r.RemoteAddr)
}

// InternalOnly answers 404 to any request that came through the edge. It is for
// routes that exist for peers on the container network — the Prometheus
// scraper, brain-to-brain webhooks — and have no business being addressable
// from f33d3r.com at all. 404 rather than 403: the route's existence is not
// something the outside needs confirmed.
//
// This is a reachability wall, not authentication. Routes behind it that carry
// a shared secret still verify that secret.
func InternalOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if FromEdge(r) {
			log.Printf("[internal-only] %s %s refused: reached from the edge (peer %s)",
				r.Method, r.URL.Path, r.RemoteAddr)
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func hostOnly(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}
