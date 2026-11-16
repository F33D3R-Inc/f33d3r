package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// ── The AET unit ──────────────────────────────────────────────────────────────
//
// Ain Soph owns the AET unit and publishes it at GET /v1/unit. Every amount
// this server sends to or reads from Themis and Ain Soph is an integer of those
// units (`*_units`), and this server never hard-codes the ratio: it learns it
// once and keeps it, because the unit is a constant of the ledger. A person
// types and reads decimal AET; the conversion happens here, on the server, with
// the ledger's own unit.

type aetUnit struct {
	UnitsPerAET int64 `json:"units_per_aet"`
	UAETPerUnit int64 `json:"uaet_per_unit"`
}

var aetUnitCache struct {
	sync.Mutex
	unit aetUnit
}

var errAETUnitUnavailable = errors.New("the AET unit is not known yet: Ain Soph has not answered")

// aetUnit returns the ledger's unit, asking Ain Soph the first time.
func (h *Handler) aetUnit(ctx context.Context) (aetUnit, error) {
	aetUnitCache.Lock()
	defer aetUnitCache.Unlock()
	if aetUnitCache.unit.UnitsPerAET > 0 {
		return aetUnitCache.unit, nil
	}
	if h.cfg == nil || h.cfg.AinSophURL == "" {
		return aetUnit{}, errAETUnitUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var u aetUnit
	if err := h.ainSophGet(ctx, "/v1/unit", &u); err != nil {
		return aetUnit{}, fmt.Errorf("%w: %v", errAETUnitUnavailable, err)
	}
	if u.UnitsPerAET <= 0 || u.UAETPerUnit <= 0 || aetDecimals(u.UnitsPerAET) < 0 {
		return aetUnit{}, fmt.Errorf("ain-soph published an unusable unit %+v", u)
	}
	aetUnitCache.unit = u
	return u, nil
}

// aetDecimals is the number of decimal places a unit has (100 → 2), or -1 when
// the unit is not a power of ten.
func aetDecimals(unitsPerAET int64) int {
	d := 0
	for n := unitsPerAET; n > 1; n /= 10 {
		if n%10 != 0 {
			return -1
		}
		d++
	}
	return d
}

// formatAETUnits writes units as a decimal AET amount ("12.50"), without the
// ticker.
func formatAETUnits(units int64, u aetUnit) string {
	d := aetDecimals(u.UnitsPerAET)
	sign := ""
	if units < 0 {
		sign = "-"
		units = -units
	}
	whole, frac := units/u.UnitsPerAET, units%u.UnitsPerAET
	if d <= 0 {
		return fmt.Sprintf("%s%d", sign, whole)
	}
	return fmt.Sprintf("%s%d.%0*d", sign, whole, d, frac)
}

// parseAETAmount reads a decimal AET amount a person typed ("5", "5.5",
// "5.50") into units, exactly: no floating point touches money. More decimals
// than the unit has is refused rather than rounded.
func parseAETAmount(s string, u aetUnit) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("enter an amount")
	}
	whole, frac, hasFrac := strings.Cut(s, ".")
	d := aetDecimals(u.UnitsPerAET)
	if whole == "" {
		whole = "0"
	}
	if hasFrac && len(frac) > d {
		return 0, fmt.Errorf("an amount has at most %d decimal places", d)
	}
	for _, part := range []string{whole, frac} {
		for _, c := range part {
			if c < '0' || c > '9' {
				return 0, errors.New("enter a number like 5 or 5.50")
			}
		}
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w > 1_000_000_000 {
		return 0, errors.New("that amount is too large")
	}
	frac += strings.Repeat("0", d-len(frac))
	var f int64
	if frac != "" {
		if f, err = strconv.ParseInt(frac, 10, 64); err != nil {
			return 0, errors.New("enter a number like 5 or 5.50")
		}
	}
	return w*u.UnitsPerAET + f, nil
}

// ── Ain Soph reads ────────────────────────────────────────────────────────────

// walletBalance reads the viewer's balance from Ain Soph. ok is false when
// there is no identity or Ain Soph did not answer.
func (h *Handler) walletBalance(ctx context.Context, walletID string) (units int64, display string, ok bool) {
	if walletID == "" {
		return 0, "", false
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var acc struct {
		BalanceUnits int64  `json:"balance_units"`
		BalanceAet   string `json:"balance_aet"`
	}
	if err := h.ainSophGet(ctx, "/v1/balance/"+walletID, &acc); err != nil {
		log.Printf("[wallet] balance: %v", err)
		return 0, "", false
	}
	return acc.BalanceUnits, acc.BalanceAet, true
}

type walletData struct {
	Balance string
	Amount  string // Balance without its unit, as wallet_hero shows it
	Address string
	Error   string
	Online  bool
}

func (h *Handler) walletDataFor(ctx context.Context, user *model.User) walletData {
	data := walletData{}
	// PIAL is the only thing a wallet may be keyed on. See walletIdentity.
	walletID := h.walletIdentity(user)
	if walletID == "" {
		data.Error = "Wallet unavailable — no identity for this account."
	}
	if _, display, ok := h.walletBalance(ctx, walletID); ok {
		data.Balance = display
		data.Online = true
	} else {
		data.Balance = "— AET"
	}
	data.Amount = strings.TrimSuffix(data.Balance, " AET")
	return data
}

func (h *Handler) walletPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	h.render(w, r, "wallet.html", h.withRail(map[string]interface{}{
		"User":       user,
		"Title":      "Wallet · F33D3R",
		"SessionID":  uuid.New().String(),
		"ShowScores": h.cfg.ShowScores,
		"Themes":     ThemesWithActive(user.ThemeID),
		"Wallet":     h.walletDataFor(r.Context(), user),
		"TipKey":     uuid.NewString(),
	}, user, "default"))
}

// ── Wallet panel partial handlers ─────────────────────────────────────────────
// Each handler renders the appropriate panel partial into #wallet-panel.

func (h *Handler) walletPanelData(w http.ResponseWriter, r *http.Request) map[string]interface{} {
	user := h.userFromRequest(w, r)
	return h.withRail(map[string]interface{}{
		"User":       user,
		"Title":      "Wallet · F33D3R",
		"SessionID":  uuid.New().String(),
		"ShowScores": h.cfg.ShowScores,
		"Themes":     ThemesWithActive(user.ThemeID),
		"Wallet":     h.walletDataFor(r.Context(), user),
		"TipKey":     uuid.NewString(),
	}, user, "default")
}

func (h *Handler) walletPanelOverview(w http.ResponseWriter, r *http.Request) {
	tplData := h.walletPanelData(w, r)
	tplData["ActiveSection"] = "overview"
	h.renderPartial(w, "wallet_panel_overview", tplData)
}

func (h *Handler) walletPanelHistory(w http.ResponseWriter, r *http.Request) {
	tplData := h.walletPanelData(w, r)
	tplData["ActiveSection"] = "history"
	h.renderPartial(w, "wallet_panel_history", tplData)
}

func (h *Handler) walletPanelGovernance(w http.ResponseWriter, r *http.Request) {
	tplData := h.walletPanelData(w, r)
	tplData["ActiveSection"] = "governance"
	h.renderPartial(w, "wallet_panel_governance", tplData)
}

func (h *Handler) walletPanelSend(w http.ResponseWriter, r *http.Request) {
	tplData := h.walletPanelData(w, r)
	tplData["ActiveSection"] = "send"
	h.renderPartial(w, "wallet_panel_send", tplData)
}

// ── Tips ──────────────────────────────────────────────────────────────────────

// tipEvent handles POST /events {event_type:tip, target_handle | stream_id,
// amount, tip_key}. amount is a decimal AET amount as the person typed it;
// tip_key is the form's own idempotency key, rendered with the form, so a
// double submit is one tip.
func (h *Handler) tipEvent(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	targetHandle := strings.TrimPrefix(strings.TrimSpace(r.FormValue("target_handle")), "@")
	streamID := strings.TrimSpace(r.FormValue("stream_id"))
	workID := strings.TrimSpace(r.FormValue("work_id"))
	tipKey := strings.TrimSpace(r.FormValue("tip_key"))
	if _, err := uuid.Parse(tipKey); err != nil {
		// A form rendered before tip keys existed still tips once per press.
		tipKey = uuid.NewString()
	}

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

	unit, err := h.aetUnit(r.Context())
	if err != nil {
		log.Printf("tip: %v", err)
		h.tipEventError(w, r, targetHandle, "The wallet is unavailable right now. Nothing was sent.", http.StatusServiceUnavailable)
		return
	}
	units, err := parseAETAmount(r.FormValue("amount"), unit)
	if err != nil || units <= 0 {
		h.tipEventError(w, r, targetHandle, "Enter a valid amount", http.StatusBadRequest)
		return
	}

	target, err := dbpkg.GetUserByHandle(h.db, targetHandle)
	if err != nil || target == nil || target.PIALID == "" {
		h.tipEventError(w, r, targetHandle, "User not found", http.StatusNotFound)
		return
	}

	tipTargetID, tipTargetType := workID, "profile"
	if workID != "" {
		tipTargetType = "work"
	} else if stream != nil {
		tipTargetID, tipTargetType = stream.ID, "live"
	}
	status, msg := h.settleTip(r.Context(), user, target, tipTargetID, tipTargetType, units, tipKey)
	if status != http.StatusOK {
		h.tipEventError(w, r, targetHandle, msg, status)
		return
	}

	// The ledger moved the tip and Themis has told the recipient. A broadcast
	// also keeps its own account of the tip, in the native clients' µAET.
	if stream != nil {
		h.recordLiveTip(stream, user, units*unit.UAETPerUnit)
	}

	amount := formatAETUnits(units, unit)
	if facetRequest(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		// The wallet's history Facet re-requests itself on this.
		w.Header().Set("HX-Trigger", "fa:wallet-moved")
		h.renderPartial(w, "tip_result", map[string]interface{}{
			"OK":      true,
			"Handle":  targetHandle,
			"Amount":  amount,
			"Message": "",
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"ok":true}`)
}

// settleTip asks Themis — the only door through which AET moves — to move
// units from the tipper to target. The tipper is the session's identity, sent
// in X-Pial-Identity; the body never names who pays. On success it shows the
// toast on the recipient's web sessions (Themis writes their notification with
// the payment). Otherwise the status and message Themis gave.
func (h *Handler) settleTip(ctx context.Context, from, target *model.User, tipTargetID, tipTargetType string, units int64, tipKey string) (int, string) {
	if from == nil || from.PIALID == "" || target == nil || target.PIALID == "" || units <= 0 {
		return http.StatusBadRequest, "tip failed"
	}
	status, body, err := h.themisCall(ctx, http.MethodPost, "/v1/tips", from, map[string]interface{}{
		"recipient_pial_id": target.PIALID,
		"amount_units":      units,
		"idempotency_key":   tipKey,
		"from_account":      from.ID,
		"from_handle":       from.Handle,
		"to_account":        target.ID,
		"target_id":         tipTargetID,
		"target_type":       tipTargetType,
	})
	if err != nil {
		log.Printf("tip: themis unavailable: %v", err)
		return http.StatusBadGateway, "Tip service unavailable"
	}
	if status == http.StatusOK {
		var ok struct {
			AmountDisplay string `json:"amount_display"`
		}
		_ = json.Unmarshal(body, &ok)
		go PublishToUser(target.PIALID, SSEEvent{
			Type: "tip_received",
			Data: fmt.Sprintf(`<div class="toast toast--success" role="alert">@%s sent you %s</div>`,
				from.Handle, ok.AmountDisplay),
		})
		return http.StatusOK, ""
	}
	return status, themisRefusal(body, "tip failed")
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

// walletIdentity is the identity a wallet belongs to: the viewer's PIAL. Every
// account has one from the moment elohim-veni creates it; an account without
// one is not a wallet holder, and "" says so — a wallet is never opened under
// an account id instead.
func (h *Handler) walletIdentity(user *model.User) string {
	if user == nil || user.PIALID == "" {
		return ""
	}
	return user.PIALID
}
