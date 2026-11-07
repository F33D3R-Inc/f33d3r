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
	if pathSuffix == "" {
		pathSuffix = "/"
	}
	pialID := user.PIALID

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = upstreamURL.Scheme
			req.URL.Host = upstreamURL.Host
			req.URL.Path = pathSuffix
			if r.URL.RawQuery != "" {
				req.URL.RawQuery = r.URL.RawQuery
			}
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
	if pathSuffix == "" {
		pathSuffix = "/"
	}
	pialID := user.PIALID

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = upstreamURL.Scheme
			req.URL.Host = upstreamURL.Host
			req.URL.Path = pathSuffix
			if r.URL.RawQuery != "" {
				req.URL.RawQuery = r.URL.RawQuery
			}
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

// thessalonAllowed lists what a browser may reach through this proxy. Everything else
// on Themis, including its events route, metrics and other people's records, stays
// behind it. Reads about a person are for that person only.
func thessalonAllowed(method, path, pial string, query url.Values) bool {
	parts := strings.Split(strings.TrimPrefix(path, "/v1/"), "/")
	if !strings.HasPrefix(path, "/v1/") {
		return false
	}
	switch {
	case method == http.MethodPost && len(parts) == 1:
		switch parts[0] {
		case "plans", "subscribe", "ppv", "tips":
			return true
		}
	case method == http.MethodPost && len(parts) == 2 && parts[0] == "creator" && parts[1] == "enable":
		return true
	case method == http.MethodPost && len(parts) == 3 && parts[0] == "ppv" && parts[2] == "purchase":
		return true
	case (method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete) && len(parts) == 2 && parts[0] == "plans":
		return true
	case method == http.MethodDelete && len(parts) == 1 && parts[0] == "subscribe":
		return true
	case method == http.MethodGet && len(parts) == 3 && parts[0] == "creator":
		switch parts[2] {
		case "status", "plans", "ppv":
			return true
		case "earnings", "subscribers":
			return parts[1] == pial
		}
	case method == http.MethodGet && len(parts) == 3 && parts[0] == "subscriber" && parts[2] == "subscriptions":
		return parts[1] == pial
	case method == http.MethodGet && len(parts) == 2 && parts[0] == "access" && parts[1] == "subscription":
		return query.Get("subscriber") == pial
	case method == http.MethodGet && len(parts) == 2 && parts[0] == "access" && parts[1] == "ppv":
		return query.Get("buyer") == pial
	}
	return false
}

// ── Thessalon proxy: /thessalon/v1/... → themis:8100/v1/... (Thessalon merged into Themis) ───
func (h *Handler) thessalonProxy(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user.PIALID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"pial_not_ready"}`))
		return
	}

	upstreamURL, err := url.Parse(h.themisURL)
	if err != nil {
		http.Error(w, "proxy config error", http.StatusInternalServerError)
		return
	}

	pathSuffix := strings.TrimPrefix(r.URL.Path, "/thessalon")
	if pathSuffix == "" {
		pathSuffix = "/"
	}
	pialID := user.PIALID
	if !thessalonAllowed(r.Method, pathSuffix, pialID, r.URL.Query()) {
		http.NotFound(w, r)
		return
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = upstreamURL.Scheme
			req.URL.Host = upstreamURL.Host
			req.URL.Path = pathSuffix
			if r.URL.RawQuery != "" {
				req.URL.RawQuery = r.URL.RawQuery
			}
			req.Host = upstreamURL.Host
			req.Header.Set("X-Pial-Identity", pialID)
			req.Header.Set("X-Pial-Handle", user.Handle)
			req.Header.Del("Cookie")
			req.Header.Set(serviceKeyHeader, h.cfg.InternalAPIKey)
			req.Header.Del("X-Internal-Key")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("thessalon proxy: %v", err)
			http.Error(w, `{"error":"themis_unavailable"}`, http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}
