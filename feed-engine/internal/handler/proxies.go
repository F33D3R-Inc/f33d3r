package handler

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// ── Verity proxy — KYC/compliance ────────────────────────────────────────────
// Path: /verity/v1/... → http://verity:8095/v1/...
func (h *Handler) verityProxy(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user.PIALID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"pial_not_ready"}`))
		return
	}

	upstreamURL, err := url.Parse(h.cfg.VerityURL)
	if err != nil {
		http.Error(w, "proxy config error", http.StatusInternalServerError)
		return
	}

	pathSuffix := strings.TrimPrefix(r.URL.Path, "/verity")
	if pathSuffix == "" { pathSuffix = "/" }
	pialID := user.PIALID

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = upstreamURL.Scheme
			req.URL.Host   = upstreamURL.Host
			req.URL.Path   = pathSuffix
			if r.URL.RawQuery != "" { req.URL.RawQuery = r.URL.RawQuery }
			req.Host = upstreamURL.Host
			req.Header.Set("X-Pial-Identity", pialID)
			req.Header.Set("X-Pial-Handle", user.Handle)
			req.Header.Del("Cookie")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("verity proxy: %v", err)
			http.Error(w, `{"error":"verity_unavailable"}`, http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// ── Aethyr Ledger proxy — AET settlement ──────────────────────────────────────
// Path: /ledger/v1/... → http://aethyr-ledger:8096/v1/...
func (h *Handler) ledgerProxy(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user.PIALID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"pial_not_ready"}`))
		return
	}

	upstreamURL, err := url.Parse(h.cfg.LedgerURL)
	if err != nil {
		http.Error(w, "proxy config error", http.StatusInternalServerError)
		return
	}

	pathSuffix := strings.TrimPrefix(r.URL.Path, "/ledger")
	if pathSuffix == "" { pathSuffix = "/" }
	pialID := user.PIALID

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = upstreamURL.Scheme
			req.URL.Host   = upstreamURL.Host
			req.URL.Path   = pathSuffix
			if r.URL.RawQuery != "" { req.URL.RawQuery = r.URL.RawQuery }
			req.Host = upstreamURL.Host
			req.Header.Set("X-Pial-Identity", pialID)
			req.Header.Set("X-Pial-Handle", user.Handle)
			req.Header.Del("Cookie")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("ledger proxy: %v", err)
			http.Error(w, `{"error":"ledger_unavailable"}`, http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// ── Thessalon proxy: /thessalon/v1/... → thessalon:8084/v1/... ───────────────
func (h *Handler) thessalonProxy(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user.PIALID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"pial_not_ready"}`))
		return
	}

	upstreamURL, err := url.Parse(h.thessalonURL)
	if err != nil {
		http.Error(w, "proxy config error", http.StatusInternalServerError)
		return
	}

	pathSuffix := strings.TrimPrefix(r.URL.Path, "/thessalon")
	if pathSuffix == "" { pathSuffix = "/" }
	pialID := user.PIALID

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = upstreamURL.Scheme
			req.URL.Host   = upstreamURL.Host
			req.URL.Path   = pathSuffix
			if r.URL.RawQuery != "" { req.URL.RawQuery = r.URL.RawQuery }
			req.Host = upstreamURL.Host
			req.Header.Set("X-Pial-Identity", pialID)
			req.Header.Set("X-Pial-Handle",   user.Handle)
			req.Header.Del("Cookie")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("thessalon proxy: %v", err)
			http.Error(w, `{"error":"thessalon_unavailable"}`, http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}
