package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
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

	rail := h.railData(user, "default")
	h.render(w, "wallet.html", map[string]interface{}{
		"User":           user,
		"Title":          "Wallet · F33D3R",
		"SessionID":      uuid.New().String(),
		"ShowScores":     h.cfg.ShowScores,
		"Themes":         ThemesWithActive(user.ThemeID),
		"Wallet":         data,
		"TrendingTags":   rail["TrendingTags"],
		"SuggestedUsers": rail["SuggestedUsers"],
		"RailContext":    rail["RailContext"],
		"RailNewsItems":  rail["RailNewsItems"],
		"RailNewsLabel":  rail["RailNewsLabel"],
	})
}

// ── Wallet panel partial handlers ─────────────────────────────────────────────
// Each handler fetches the same balance data as walletPage and renders
// the appropriate panel partial into #wallet-panel via HTMX swap.

func (h *Handler) walletPanelData(w http.ResponseWriter, r *http.Request) (map[string]interface{}, bool) {
	user := h.userFromRequest(w, r)

	type walletData struct {
		Balance string
		Address string
		Error   string
		Online  bool
	}
	data := walletData{}

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
			data.Online = true
		}
	}
	if !data.Online {
		data.Balance = "0.00 AET"
	}

	rail := h.railData(user, "default")
	tplData := map[string]interface{}{
		"User":           user,
		"Title":          "Wallet · F33D3R",
		"SessionID":      uuid.New().String(),
		"ShowScores":     h.cfg.ShowScores,
		"Themes":         ThemesWithActive(user.ThemeID),
		"Wallet":         data,
		"TrendingTags":   rail["TrendingTags"],
		"SuggestedUsers": rail["SuggestedUsers"],
		"RailContext":    rail["RailContext"],
		"RailNewsItems":  rail["RailNewsItems"],
		"RailNewsLabel":  rail["RailNewsLabel"],
	}
	return tplData, true
}

func (h *Handler) walletPanelOverview(w http.ResponseWriter, r *http.Request) {
	tplData, ok := h.walletPanelData(w, r)
	if !ok {
		return
	}
	tplData["ActiveSection"] = "overview"
	h.renderPartial(w, "wallet_panel_overview", tplData)
}

func (h *Handler) walletPanelHistory(w http.ResponseWriter, r *http.Request) {
	tplData, ok := h.walletPanelData(w, r)
	if !ok {
		return
	}
	tplData["ActiveSection"] = "history"
	h.renderPartial(w, "wallet_panel_history", tplData)
}

func (h *Handler) walletPanelGovernance(w http.ResponseWriter, r *http.Request) {
	tplData, ok := h.walletPanelData(w, r)
	if !ok {
		return
	}
	tplData["ActiveSection"] = "governance"
	h.renderPartial(w, "wallet_panel_governance", tplData)
}

func (h *Handler) walletPanelSend(w http.ResponseWriter, r *http.Request) {
	tplData, ok := h.walletPanelData(w, r)
	if !ok {
		return
	}
	tplData["ActiveSection"] = "send"
	h.renderPartial(w, "wallet_panel_send", tplData)
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

// tipEvent handles POST /events {event_type:tip, target_handle, amount_aet}.
// amount_aet is in cents of AET (100 = 1.00 AET).
// Resolves target_handle → target PIAL, then proxies to Thessalon /v1/tips.
func (h *Handler) tipEvent(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	targetHandle := strings.TrimPrefix(r.FormValue("target_handle"), "@")
	amountStr := r.FormValue("amount_aet")
	if targetHandle == "" || amountStr == "" {
		http.Error(w, "target_handle and amount_aet required", http.StatusBadRequest)
		return
	}
	amountAet, err := strconv.Atoi(amountStr)
	if err != nil || amountAet <= 0 {
		http.Error(w, "invalid amount", http.StatusBadRequest)
		return
	}

	// Resolve target handle → PIAL. users.pial_id is the direct FK to pial_roots.
	var targetPIAL string
	if h.db != nil {
		_ = h.db.QueryRow(
			`SELECT pial_id FROM users WHERE handle = $1`,
			targetHandle,
		).Scan(&targetPIAL)
	}
	if targetPIAL == "" {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}

	// Route tip through Ain Soph /v1/tip — the actual ledger brain that debits
	// the sender and credits the recipient atomically.
	aetAmount := float64(amountAet) / 100.0
	payload := map[string]interface{}{
		"from_pial_id": user.PIALID,
		"to_pial_id":   targetPIAL,
		"amount_aet":   aetAmount,
		"idempotency_key": "",
	}
	body, _ := json.Marshal(payload)

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.cfg.AinSophURL+"/v1/tip", bytes.NewReader(body))
	if err != nil {
		http.Error(w, "tip service unavailable", http.StatusBadGateway)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Pial-Identity", user.PIALID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("tip: ain-soph unavailable: %v", err)
		http.Error(w, `{"error":"tip service unavailable"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		go h.HeraldNotify(targetPIAL, "tip",
			"@"+user.Handle+" sent you a tip",
			fmt.Sprintf("%.2f AET", aetAmount),
			"", "/wallet")
		// FA Live: push tip_received toast to recipient immediately.
		go PublishToUser(targetPIAL, SSEEvent{
			Type: "tip_received",
			Data: fmt.Sprintf(`<div class="toast toast--success" role="alert">@%s sent you %.2f AET</div>`,
				user.Handle, aetAmount),
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"ok":true}`)
		return
	}

	var errBody struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	json.NewDecoder(resp.Body).Decode(&errBody)
	msg := errBody.Error
	if msg == "" { msg = errBody.Message }
	if msg == "" { msg = "tip failed" }
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	fmt.Fprintf(w, `{"error":%q}`, msg)
}
