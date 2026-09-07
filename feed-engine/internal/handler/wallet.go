package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

func (h *Handler) walletPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)

	type walletData struct {
		Balance string
		Address string
		Error   string
		Online  bool
	}
	data := walletData{}

	// PIAL is the only thing a wallet may be keyed on. See walletIdentity.
	walletID := h.walletIdentity(user)
	if walletID == "" {
		data.Error = "Wallet unavailable — no identity for this account."
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	var req *http.Request
	if walletID != "" {
		req, _ = http.NewRequestWithContext(ctx, http.MethodGet,
			h.cfg.AinSophURL+"/v1/balance/"+walletID, nil)
	}
	if resp, err := doIfSet(req); err == nil && resp != nil {
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

	h.render(w, r, "wallet.html", h.withRail(map[string]interface{}{
		"User":       user,
		"Title":      "Wallet · F33D3R",
		"SessionID":  uuid.New().String(),
		"ShowScores": h.cfg.ShowScores,
		"Themes":     ThemesWithActive(user.ThemeID),
		"Wallet":     data,
	}, user, "default"))
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

	walletID := h.walletIdentity(user)
	if walletID == "" {
		data.Error = "Wallet unavailable — no identity for this account."
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	var req *http.Request
	if walletID != "" {
		req, _ = http.NewRequestWithContext(ctx, http.MethodGet,
			h.cfg.AinSophURL+"/v1/balance/"+walletID, nil)
	}
	if resp, err := doIfSet(req); err == nil && resp != nil {
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

	tplData := h.withRail(map[string]interface{}{
		"User":       user,
		"Title":      "Wallet · F33D3R",
		"SessionID":  uuid.New().String(),
		"ShowScores": h.cfg.ShowScores,
		"Themes":     ThemesWithActive(user.ThemeID),
		"Wallet":     data,
	}, user, "default")
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

// ainSophValueRoutes are the Ain Soph routes that move value out of the AET
// ledger rather than around inside it. Ain Soph registers /withdraw and
// /deposit under both a legacy and a /v1 path, so both spellings are listed —
// a gate that covers only the spelling the UI happens to use is not a gate.
//
// Transfers and tips are absent: those are a person spending their own balance,
// which is paying rather than being paid. Reads (balance, transactions,
// proposals) are absent for the same reason /marketplace stays an age gate —
// looking at your own wallet is not a payout.
var ainSophValueRoutes = map[string]bool{
	"/withdraw":    true,
	"/v1/withdraw": true,
	"/deposit":     true,
	"/v1/deposit":  true,
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

	// This is a blanket reverse proxy: every route Ain Soph serves is reachable
	// through it, including the two that move value out of the ledger. Ain Soph
	// itself asks no identity question, so an account that had done nothing but
	// type a birthday into the signup form could POST /ainsoph/v1/withdraw and
	// cash out. Value-moving routes ask the identity predicate here, at the only
	// layer that holds a session.
	if ainSophValueRoutes[pathSuffix] && !user.CanMonetize() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"identity_verification_required","message":"Moving value in or out of the AET ledger requires identity verification. Complete verification at /kyc."}`))
		return
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

	targetHandle := strings.TrimPrefix(strings.TrimSpace(r.FormValue("target_handle")), "@")
	streamID := strings.TrimSpace(r.FormValue("stream_id"))
	workID := strings.TrimSpace(r.FormValue("work_id"))
	amountStr := r.FormValue("amount_aet")

	// A tip names its target one of two ways: a handle, or the broadcast the
	// tipper is watching, whose author is the target.
	var stream *model.LiveStream
	if targetHandle == "" && streamID != "" {
		if h.db == nil {
			h.tipEventError(w, r, "", "Tip service unavailable", http.StatusServiceUnavailable)
			return
		}
		s, err := dbpkg.GetLiveStreamByID(h.db, streamID)
		if err != nil {
			if errors.Is(err, dbpkg.ErrLiveStreamNotFound) {
				h.tipEventError(w, r, "", "stream not found", http.StatusNotFound)
				return
			}
			log.Printf("tip: load stream %s: %v", streamID, err)
			h.tipEventError(w, r, "", "stream unavailable", http.StatusInternalServerError)
			return
		}
		if s.Status != model.LiveStatusLive {
			h.tipEventError(w, r, "", "this stream has ended", http.StatusConflict)
			return
		}
		if s.AuthorID == user.ID {
			h.tipEventError(w, r, "", "cannot tip yourself", http.StatusBadRequest)
			return
		}
		stream = s
		targetHandle = s.AuthorHandle
	}
	if targetHandle == "" {
		h.tipEventError(w, r, targetHandle, "target_handle required", http.StatusBadRequest)
		return
	}
	var amountAet int
	if amountStr != "" {
		n, err := strconv.Atoi(amountStr)
		if err != nil || n <= 0 {
			h.tipEventError(w, r, targetHandle, "Enter a valid amount", http.StatusBadRequest)
			return
		}
		amountAet = n
	} else {
		aet, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue("amount")), 64)
		if err != nil || aet <= 0 || aet > 10000 {
			h.tipEventError(w, r, targetHandle, "Enter a valid amount", http.StatusBadRequest)
			return
		}
		amountAet = int(math.Round(aet * 100))
	}
	if amountAet <= 0 {
		h.tipEventError(w, r, targetHandle, "Enter a valid amount", http.StatusBadRequest)
		return
	}

	var targetPIAL, targetID string
	if h.db != nil {
		_ = h.db.QueryRow(
			`SELECT COALESCE(pial_id::text, ''), id::text FROM users WHERE handle = $1`,
			targetHandle,
		).Scan(&targetPIAL, &targetID)
	}
	if targetPIAL == "" {
		h.tipEventError(w, r, targetHandle, "User not found", http.StatusNotFound)
		return
	}

	status, msg := h.settleTip(r.Context(), user, targetPIAL, amountAet)
	if status != http.StatusOK {
		h.tipEventError(w, r, targetHandle, msg, status)
		return
	}

	// The ledger said yes. What follows is the record of it on this side:
	// the inbox row, and for a broadcast the room's own account of the tip.
	amountUAET := int64(amountAet) * 10_000
	if stream != nil {
		h.recordLiveTip(stream, user, amountUAET)
	} else if targetID != "" {
		targetType := "profile"
		if workID != "" {
			targetType = "work"
		}
		h.notifyUserWithPayload(targetID, "tip", user.ID, workID, targetType, map[string]interface{}{"amount_uaet": amountUAET})
	}

	aetAmount := float64(amountAet) / 100.0
	if facetRequest(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		h.renderPartial(w, "tip_result", map[string]interface{}{
			"OK":      true,
			"Handle":  targetHandle,
			"Amount":  fmt.Sprintf("%.2f", aetAmount),
			"Message": "",
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"ok":true}`)
}

// settleTip moves amountAet (AET hundredths) from the tipper to targetPIAL
// through Ain Soph, the ledger brain — F33D3R never holds the money. On
// success it returns 200 and tells the recipient (Herald push, and the toast
// on their web sessions); otherwise the status and message the ledger gave.
func (h *Handler) settleTip(ctx context.Context, from *model.User, targetPIAL string, amountAet int) (int, string) {
	if from == nil || from.PIALID == "" || targetPIAL == "" || amountAet <= 0 {
		return http.StatusBadRequest, "tip failed"
	}
	aetAmount := float64(amountAet) / 100.0
	payload := map[string]interface{}{
		"from_pial_id":    from.PIALID,
		"to_pial_id":      targetPIAL,
		"amount_aet":      aetAmount,
		"idempotency_key": "",
	}
	body, _ := json.Marshal(payload)

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.cfg.AinSophURL+"/v1/tip", bytes.NewReader(body))
	if err != nil {
		return http.StatusBadGateway, "Tip service unavailable"
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Pial-Identity", from.PIALID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("tip: ain-soph unavailable: %v", err)
		return http.StatusBadGateway, "Tip service unavailable"
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		go h.HeraldNotify(targetPIAL, "tip",
			"@"+from.Handle+" sent you a tip",
			fmt.Sprintf("%.2f AET", aetAmount),
			"", "/wallet")
		go PublishToUser(targetPIAL, SSEEvent{
			Type: "tip_received",
			Data: fmt.Sprintf(`<div class="toast toast--success" role="alert">@%s sent you %.2f AET</div>`,
				from.Handle, aetAmount),
		})
		return http.StatusOK, ""
	}

	var errBody struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	json.NewDecoder(resp.Body).Decode(&errBody)
	msg := errBody.Error
	if msg == "" {
		msg = errBody.Message
	}
	if msg == "" {
		msg = "tip failed"
	}
	return resp.StatusCode, msg
}

func (h *Handler) tipEventError(w http.ResponseWriter, r *http.Request, handle, msg string, code int) {
	if facetRequest(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		h.renderPartial(w, "tip_result", map[string]interface{}{
			"OK":      false,
			"Handle":  handle,
			"Amount":  "",
			"Message": msg,
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	fmt.Fprintf(w, `{"error":%q}`, msg)
}

// walletIdentity returns the PIAL a wallet is keyed on, or "" if the viewer has
// no identity and one cannot be created.
//
// It exists because the two wallet pages used to do this instead:
//
//	walletID := user.PIALID
//	if walletID == "" {
//	    walletID = user.ID
//	}
//
// which keyed a wallet on a feed-engine row id whenever a PIAL was missing. A
// PIAL and a users.id are both UUIDs, so nothing downstream could tell them
// apart — Ain Soph opened an account for whichever string arrived and the money
// went to an identifier this brain is free to renumber. On this database that
// produced four wallet accounts for two people, half of them keyed on the wrong
// thing.
//
// A wallet is keyed on PIAL or it is not opened. There is no fallback, because a
// fallback here is indistinguishable from correctness until someone's balance is
// in the wrong place. If the viewer somehow has no PIAL, one is minted — that is
// what a PIAL is for — and only a genuine failure to mint yields "".
func (h *Handler) walletIdentity(user *model.User) string {
	if user == nil {
		return ""
	}
	if user.PIALID != "" {
		return user.PIALID
	}
	if h.db == nil || user.ID == "" {
		return ""
	}
	// No PIAL yet: create the identity rather than borrowing an account id.
	pial := dbpkg.BootstrapPIAL(h.db, user.ID, user.Handle)
	if pial == "" {
		log.Printf("[wallet] cannot open a wallet for account %s: no PIAL and bootstrap failed", user.ID)
		return ""
	}
	// The handle predates this identity. Tell the authority.
	h.ensureHandleAllocated(context.Background(), user.Handle, pial)
	user.PIALID = pial
	return pial
}

// doIfSet performs req, or returns (nil, nil) when there is no request to make.
// Keeps the "no identity, no call" decision in one place rather than nesting the
// whole balance lookup twice.
func doIfSet(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, nil
	}
	return http.DefaultClient.Do(req)
}
