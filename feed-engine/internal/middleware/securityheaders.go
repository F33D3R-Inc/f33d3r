package middleware

import (
	"net/http"
	"strings"

	"github.com/f33d3r/feed-engine/internal/config"
)

// ContentSecurityPolicy is the one policy every response carries. It is a
// package-level constant so that what the browser enforces can be read in one
// place, and so that every external origin on it is a deliberate decision with
// its reason written beside it.
//
// Same-origin covers the application itself: the FA Live stream (/api/events),
// the WHIP handshake (/live/{id}/whip, answered by this process and presented
// to the media server over the container network), the live ladder's playlists
// and segments (/live/…, same host), the MinIO proxy (/media), uploads, and the
// sealcore WASM core under /static.
//
// External origins, each earned by a real load in web/templates or web/static/js:
//
//	https://cdn.jsdelivr.net             script-src, connect-src — the MediaPipe
//	                                      selfie-segmentation library the react-
//	                                      video green screen lazy-loads, plus the
//	                                      wasm/model files it fetches from the
//	                                      same base (reactvideo/react-video-layouts.js).
//	https://nominatim.openstreetmap.org  connect-src — reverse geocoding for the
//	                                      profile location field (f33d3r.js,
//	                                      pages/settings.js, _edit_profile_modal).
//	https://www.youtube-nocookie.com     frame-src — the privacy-enhanced player the
//	                                      youtube_embed facet swaps in on click.
//	https: (img-src, media-src)          remote work media, avatars, YouTube
//	                                      thumbnails (img.youtube.com), link previews.
//
// Source expressions, each earned the same way:
//
//	'unsafe-inline'    script-src — on* handler attributes throughout the
//	                   templates; style-src — style="" attributes.
//	'unsafe-eval'      script-src — htmx 2 compiles every hx-on: attribute with
//	                   new Function (htmx.config.allowEval). Ten templates use
//	                   hx-on:; without this the handlers throw an EvalError.
//	'wasm-unsafe-eval' script-src — WebAssembly.instantiate of the sealcore core.
//	blob:              script-src / worker-src — hls.js runs its demuxer in a
//	                   blob: Worker; connect-src — blob: URLs read back by
//	                   fetch during media capture; media-src — recorded clips.
//	data:              img-src / font-src / media-src — inline assets.
//
// Not present, on purpose: frame-src 'self' (nothing on this origin is framed),
// any Google Fonts or CDN script origin (nothing loads them), and COEP/CORP
// (they break cross-origin media playback).
const ContentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self' 'unsafe-inline' 'unsafe-eval' 'wasm-unsafe-eval' blob: https://cdn.jsdelivr.net; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob: https:; " +
	"media-src 'self' blob: data: https:; " +
	"connect-src 'self' blob: https://nominatim.openstreetmap.org https://cdn.jsdelivr.net; " +
	"font-src 'self' data:; " +
	"worker-src 'self' blob:; " +
	"frame-src https://www.youtube-nocookie.com; " +
	"frame-ancestors 'none'; " +
	"object-src 'none'; " +
	"base-uri 'self'; " +
	"form-action 'self'"

// SecurityHeaders stamps the browser-side hardening headers on every response.
// It is the sole owner of these headers in the process: the values are set
// before the handler runs, so a handler that needs a different Cache-Control
// for its own response simply sets it.
//
// HSTS is emitted only when the request demonstrably arrived over TLS — the
// edge says so with X-Forwarded-Proto, or this process terminated TLS itself.
// A plain-HTTP dev listener must never teach a browser to pin HTTPS on
// localhost.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		// camera/microphone/display-capture are self-only rather than disabled:
		// the capture substrate and the live broadcast surface call getUserMedia
		// and getDisplayMedia on this origin, and an empty allowlist blocks them.
		h.Set("Permissions-Policy",
			"camera=(self), microphone=(self), display-capture=(self), geolocation=(self), payment=(), usb=(), bluetooth=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Content-Security-Policy", ContentSecurityPolicy)
		h.Set("Server", "f33d3r")

		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains; preload")
			// Onion-Location tells Tor Browser this page is also served at the
			// hidden service. Browsers only honour it on an HTTPS response, which
			// is exactly the primary site behind the edge: the tor daemon itself
			// reaches this process over plain HTTP and so never gets one, nor does
			// a direct dev listener. The hostname is whatever tor derived from the
			// hidden-service key (config.OnionHost), so it is never written down
			// here; empty until tor has exported it, and then this header appears.
			if onionSurface(r.URL.Path) {
				if host := config.OnionHost(); host != "" {
					h.Set("Onion-Location", "http://"+host+r.URL.RequestURI())
				}
			}
		}

		// Rendered surfaces are never cached: a facet is the server's view at the
		// moment it was rendered, and a back-button replay of a page from before
		// logout is a session leak. Static assets are versioned (?v=) and set
		// their own long-lived policy; the media proxy passes its upstream's.
		p := r.URL.Path
		if !strings.HasPrefix(p, "/static/") && !strings.HasPrefix(p, "/media/") &&
			!strings.HasSuffix(p, ".js") && !strings.HasSuffix(p, ".css") {
			h.Set("Cache-Control", "no-store")
		}

		next.ServeHTTP(w, r)
	})
}

// onionSurface reports whether a path is part of the site a visitor could
// carry over to the hidden service. Health, metrics and brain-to-brain
// internal routes are answered for machines on the internal network and are
// not mirrored, so they carry no Onion-Location.
func onionSurface(p string) bool {
	switch {
	case p == "/metrics", p == "/health", p == "/api/health":
		return false
	case strings.HasPrefix(p, "/internal/"), strings.HasPrefix(p, "/api/internal/"):
		return false
	}
	return true
}
