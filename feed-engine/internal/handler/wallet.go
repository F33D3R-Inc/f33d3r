package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

func (h *Handler) walletPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)

	type walletData struct {
		Balance  string
		Address  string
		Error    string
		Online   bool
	}
	data := walletData{}

	// Use PIAL ID as the canonical wallet identity (cross-brain anchor)
	walletID := user.PIALID
	if walletID == "" {
		walletID = user.ID
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		h.cfg.AinSophURL+"/v1/balance/"+walletID, nil)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		defer resp.Body.Close()
		var acc struct {
			BalanceUnits int64  `json:"balance_units"`
			BalanceAet   string `json:"balance_aet"`
		}
		if json.NewDecoder(resp.Body).Decode(&acc) == nil && acc.BalanceAet != "" {
			data.Balance = acc.BalanceAet
			data.Online  = true
		}
	}
	if !data.Online {
		data.Balance = "0.00 AET"
	}

	h.render(w, "wallet.html", map[string]interface{}{
		"User":    user,
		"Title":   "Wallet",
		"Themes":  ThemesWithActive(user.ThemeID),
		"Wallet":  data,
	})
}

// ── Ain Soph proxy — ETHRA/AET wallet + governance ────────────────────────────
// Path: /ainsoph/v1/... → http://ain-soph:8089/v1/...
// Injects X-Pial-Identity so ain-soph can bind operations to the caller's PIAL.
func (h *Handler) ainSophProxy(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user.PIALID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"pial_not_ready","message":"Identity anchor not ready."}`))
		return
	}

	upstreamURL, err := url.Parse(h.cfg.AinSophURL)
	if err != nil {
		http.Error(w, "proxy config error", http.StatusInternalServerError)
		return
	}

	pathSuffix := strings.TrimPrefix(r.URL.Path, "/ainsoph")
	if pathSuffix == "" {
		pathSuffix = "/"
	}

	pialID := user.PIALID
	handle := user.Handle

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = upstreamURL.Scheme
			req.URL.Host   = upstreamURL.Host
			req.URL.Path   = pathSuffix
			if r.URL.RawQuery != "" {
				req.URL.RawQuery = r.URL.RawQuery
			}
			req.Host = upstreamURL.Host
			req.Header.Set("X-Pial-Identity", pialID)
			req.Header.Set("X-Pial-Handle", handle)
			req.Header.Del("Cookie")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, `{"error":"ain_soph_unavailable"}`, http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}
