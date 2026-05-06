package handler

import (
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

func (h *Handler) messagesPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	h.render(w, "messages.html", map[string]interface{}{
		"User":          user,
		"Themes":        ThemesWithActive(user.ThemeID),
		"Conversations": nil,
	})
}

// ── Vovin proxy ───────────────────────────────────────────────────────────────
// Forwards authenticated requests to aethyr-msg, injecting the caller's identity.
// Path: /vovin/v1/... → http://aethyr-msg:8092/v1/...
func (h *Handler) vovinProxy(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)

	// PIAL must be resolved before any messaging operation.
	// userFromRequest auto-bootstraps, but if it still fails, block early.
	if user.PIALID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"pial_not_ready","message":"Your identity anchor is still being set up. Please wait a moment and try again."}`))
		return
	}

	// Strip /vovin prefix
	target := strings.TrimPrefix(r.URL.Path, "/vovin")
	if target == "" { target = "/" }

	upstream := h.cfg.VovinURL + target
	if r.URL.RawQuery != "" {
		upstream += "?" + r.URL.RawQuery
	}

	// Read body for forwarding
	var body io.Reader
	if r.Body != nil {
		body = io.LimitReader(r.Body, 1<<20) // 1 MB cap
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstream, body)
	if err != nil {
		http.Error(w, "proxy error", http.StatusBadGateway)
		return
	}

	// Forward relevant headers
	req.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	req.Header.Set("Accept", r.Header.Get("Accept"))
	// Inject PIAL root as the messaging identity — immutable cross-brain anchor
	req.Header.Set("X-Vovin-Identity", user.PIALID)
	req.Header.Set("X-Vovin-Handle", user.Handle)

	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "vovin unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response
	for k, vv := range resp.Header {
		for _, v := range vv { w.Header().Add(k, v) }
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// ── Vovin WebSocket push proxy ────────────────────────────────────────────────
// Proxies WebSocket upgrade to aethyr-msg /v1/push/:identity.
// Uses httputil.ReverseProxy which handles the HTTP→WS upgrade and bidirectional pipe.
func (h *Handler) vovinWSProxy(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user.PIALID == "" {
		http.Error(w, `{"error":"pial_not_ready"}`, http.StatusServiceUnavailable)
		return
	}

	upstreamURL, err := url.Parse(h.cfg.VovinURL)
	if err != nil {
		http.Error(w, "proxy config error", http.StatusInternalServerError)
		return
	}

	pathSuffix := strings.TrimPrefix(r.URL.Path, "/vovin")
	if pathSuffix == "" {
		pathSuffix = "/"
	}

	pialID := user.PIALID
	handle := user.Handle

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = upstreamURL.Scheme
			req.URL.Host = upstreamURL.Host
			req.URL.Path = pathSuffix
			if r.URL.RawQuery != "" {
				req.URL.RawQuery = r.URL.RawQuery
			}
			req.Host = upstreamURL.Host
			req.Header.Set("X-Vovin-Identity", pialID)
			req.Header.Set("X-Vovin-Handle", handle)
			req.Header.Del("Cookie")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("vovin ws proxy: %v", err)
		},
	}
	proxy.ServeHTTP(w, r)
}
