package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// marketplaceSellerEvents lists the marketplace events that put a person on the
// receiving end of money: opening a shop registers the BTC/XRP address proceeds
// are paid to, and creating a listing puts priced content behind it. Those two
// are gated on model.CanMonetize.
//
// shop.close, listing.delete and listing.edit are deliberately absent. They
// shrink an existing shop rather than create one, and a gate that refuses them
// would trap a seller inside a shop they are no longer allowed to open — a gate
// must never be one-way. Buyer-side events (purchase.*) are absent for the
// opposite reason: paying is not being paid.
var marketplaceSellerEvents = map[string]bool{
	"marketplace.shop.open":      true,
	"marketplace.listing.create": true,
}

// marketplaceEvent forwards marketplace.* events to Themis via HTTP POST.
// For purchase.initiate, a successful response renders the purchase modal HTML
// instead of returning raw JSON (HTMX targets #overlay-slot expecting HTML).
func (h *Handler) marketplaceEvent(w http.ResponseWriter, r *http.Request, rawBody map[string]json.RawMessage) {
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	if h.themisURL == "" {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"marketplace_unavailable"}`, http.StatusServiceUnavailable)
		return
	}

	// Determine event type and build the upstream body.
	// rawBody is non-nil only when the browser sent JSON (Content-Type: application/json).
	// HTMX hx-vals sends form-encoded data, so rawBody is nil and we reconstruct from
	// parsed form values (r.ParseForm was already called by eventsPost → r.FormValue).
	eventType := ""
	var body []byte

	if rawBody != nil {
		if et, ok := rawBody["event_type"]; ok {
			json.Unmarshal(et, &eventType)
		}
		body, _ = json.Marshal(rawBody)
	} else {
		// Form-encoded path: rebuild JSON from all form fields so Themis gets a valid body.
		r.ParseForm()
		eventType = r.FormValue("event_type")
		m := map[string]interface{}{"event_type": eventType}
		for k, vs := range r.Form {
			if k == "event_type" || len(vs) == 0 {
				continue
			}
			m[k] = vs[0]
		}
		body, _ = json.Marshal(m)
	}

	// Themis authorises these events on the PIAL this proxy injects and never
	// asks what tier it holds, so this is the layer that has to.
	if marketplaceSellerEvents[eventType] && !user.CanMonetize() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"identity_verification_required","message":"Selling on the marketplace requires identity verification. Complete verification at /kyc."}`))
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), "POST", h.themisURL+"/v1/events", bytes.NewReader(body))
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Pial-Identity", user.PIALID)
	req.Header.Set("X-Pial-Handle", user.Handle)

	resp, err := h.httpClient.Do(req)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"themis_unavailable"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	// For purchase.initiate, render the purchase modal HTML on success.
	if eventType == "marketplace.purchase.initiate" && resp.StatusCode == http.StatusOK {
		var result struct {
			OK          bool    `json:"ok"`
			PurchaseID  string  `json:"purchase_id"`
			Currency    string  `json:"currency"`
			BtcAddress  string  `json:"btc_address"`
			AmountSats  int64   `json:"amount_sats"`
			XrpAddress  string  `json:"xrp_address"`
			AmountDrops int64   `json:"amount_drops"`
			AmountXRP   float64 `json:"amount_xrp"`
			ExpiresAt   string  `json:"expires_at"`
		}
		if err := json.Unmarshal(respBody, &result); err == nil && result.OK && result.PurchaseID != "" {
			// Format AmountXRP as a clean decimal string for the template
			amountXRPStr := formatXRP(result.AmountXRP)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			h.renderPartial(w, "marketplace_purchase_modal", map[string]interface{}{
				"PurchaseID": result.PurchaseID,
				"Currency":   result.Currency,
				"BtcAddress": result.BtcAddress,
				"AmountSats": result.AmountSats,
				"XrpAddress": result.XrpAddress,
				"AmountXRP":  amountXRPStr,
				"ExpiresAt":  result.ExpiresAt,
				"Status":     "pending",
			})
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

// formatXRP formats a float64 XRP amount as a trimmed decimal string (e.g. "1.5", "10", "0.000001").
func formatXRP(xrp float64) string {
	s := fmt.Sprintf("%.6f", xrp)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	return s
}
