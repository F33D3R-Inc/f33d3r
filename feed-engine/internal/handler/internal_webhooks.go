package handler

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// internalCallerOK is the one check every brain-facing endpoint makes: the
// caller presented INTERNAL_API_KEY on X-Internal-Key.
//
// Fail-closed: an empty configured key matches nothing, so a deployment that
// forgot to set the secret exposes no endpoint rather than every endpoint
// (cmd/server refuses to start in that state; this holds even if that changes).
// Constant-time: a byte-wise != returns as soon as the first byte differs,
// which lets a caller on the same network recover the key one byte at a time
// from response timing. subtle.ConstantTimeCompare's cost depends only on
// length, and the length check that precedes it inside the function leaks
// nothing a caller could not learn from its own request.
func (h *Handler) internalCallerOK(r *http.Request) bool {
	want := []byte(h.cfg.InternalAPIKey)
	if len(want) == 0 {
		return false
	}
	got := []byte(r.Header.Get("X-Internal-Key"))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// balanceUpdateWebhook is called by Ain Soph after any transaction settles.
// It pushes a live balance SSE event to the user's active session immediately.
// POST /api/internal/balance-update
// Auth: X-Internal-Key header
func (h *Handler) balanceUpdateWebhook(w http.ResponseWriter, r *http.Request) {
	if !h.internalCallerOK(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var body struct {
		PialID     string `json:"pial_id"`
		BalanceAet string `json:"balance_aet"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.PialID == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	PublishToUser(body.PialID, SSEEvent{Type: "balance", Data: body.BalanceAet, JSON: balanceJSON(body.BalanceAet)})
	w.WriteHeader(http.StatusOK)
}

// themisPurchaseDeliveredWebhook is called by Themis when CEK wrapping is complete.
// It pushes a marketplace_purchase_delivered SSE event to the buyer's session.
// POST /api/internal/themis/purchase-delivered
// Auth: X-Internal-Key header
func (h *Handler) themisPurchaseDeliveredWebhook(w http.ResponseWriter, r *http.Request) {
	if !h.internalCallerOK(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var body struct {
		BuyerPial   string `json:"buyer_pial"`
		PurchaseID  string `json:"purchase_id"`
		ContentURL  string `json:"content_url"`
		ContentHash string `json:"content_hash"`
		CekForBuyer string `json:"cek_for_buyer"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.BuyerPial == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	data, _ := json.Marshal(map[string]string{
		"purchase_id":   body.PurchaseID,
		"content_url":   body.ContentURL,
		"content_hash":  body.ContentHash,
		"cek_for_buyer": body.CekForBuyer,
	})
	PublishToUser(body.BuyerPial, SSEEvent{Type: "marketplace_purchase_delivered", Data: string(data)})
	w.WriteHeader(http.StatusOK)
}

// pialEcdhPubkeyInternal returns a PIAL's ECDH-P256 public key for internal use by Themis.
//
// Manhattan gives the `key` kind to elohim-veni, so there is one place this
// answer can come from. It used to read a local table first and treat
// elohim-veni as a fallback, which meant the two stores could disagree and the
// caller would never learn which one it had been served.
//
// GET /api/internal/pial/{pial_id}/ecdh-pubkey
// Auth: X-Internal-Key header
func (h *Handler) pialEcdhPubkeyInternal(w http.ResponseWriter, r *http.Request) {
	if !h.internalCallerOK(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	pialID := r.PathValue("pial_id")
	if pialID == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	pub, err := h.fetchEcdhPubkey(r.Context(), pialID)
	if err != nil {
		if errors.Is(err, errKeyNotRegistered) {
			http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
			return
		}
		// The key authority being unreachable is not "this identity has no key".
		// Themis must not open a channel on the second answer.
		log.Printf("[pial] ECDH pubkey lookup for PIAL %s failed at the key authority: %v", pialID, err)
		http.Error(w, `{"error":"key_authority_unavailable"}`, http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"public_key_b64":%q}`, pub)
}

// errKeyNotRegistered distinguishes "the authority answered, and this identity
// has no such key" from "the authority did not answer". Collapsing the two is
// how a transport failure became a 404 that callers cached as fact.
var errKeyNotRegistered = errors.New("pial: key not registered")

// fetchEcdhPubkey reads a PIAL's ECDH-P256 public key from elohim-veni, the
// brain Manhattan names as the owner of the `key` kind.
func (h *Handler) fetchEcdhPubkey(ctx context.Context, pialID string) (string, error) {
	if h.cfg.ElohimVeniURL == "" {
		return "", errPolicyAuthorityDown
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		h.cfg.ElohimVeniURL+"/v1/pial/"+url.PathEscape(pialID)+"/ecdh-pubkey", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Internal-Key", h.cfg.InternalAPIKey)
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errPolicyAuthorityDown, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", errKeyNotRegistered
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("elohim-veni GET ecdh-pubkey: %s", resp.Status)
	}
	var out struct {
		PublicKeyB64 string `json:"public_key_b64"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.PublicKeyB64 == "" {
		return "", errKeyNotRegistered
	}
	return out.PublicKeyB64, nil
}

// pialSigningPubkeyInternal returns a PIAL's ECDSA-P256 signing public key for Themis signature verification.
// PIAL record (feed-engine) is authoritative — reads local first, Elohim-veni key service as fallback.
// GET /api/internal/pial/{pial_id}/signing-pubkey
// Auth: X-Internal-Key header
func (h *Handler) pialSigningPubkeyInternal(w http.ResponseWriter, r *http.Request) {
	if !h.internalCallerOK(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	pialID := r.PathValue("pial_id")
	if pialID == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Elohim-veni owns the `key` kind, so it is the only place this answer comes
	// from. There is no longer a local table to prefer or fall back to.
	pub := h.fetchSigningPubkey(pialID)
	if pub == "" {
		http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"public_key_b64":%q}`, pub)
}

// registerPIALEcdhKey hands the calling PIAL's ECDH-P256 public key to the brain
// that owns keys.
//
// Same rule as the signing key: Manhattan gives `key` to elohim-veni, so this is
// a registration and not a mirror. The previous shape wrote a local row, fired
// the mirror, discarded its status and answered {"ok":true} — a 403 from
// elohim-veni left the graph with no key node, no signs_for edge, and a log line
// asserting the local copy was the source of truth.
// POST /api/pial/ecdh-key/register
// Auth: session cookie (requireHandle middleware)
func (h *Handler) registerPIALEcdhKey(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		PublicKeyB64 string `json:"public_key_b64"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.PublicKeyB64 == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if err := h.callElohim(r.Context(), http.MethodPost, "/v1/pial/ecdh-key/register",
		map[string]string{
			"pial_id":        user.PIALID,
			"public_key_b64": req.PublicKeyB64,
		}, nil); err != nil {
		log.Printf("[pial] ECDH key registration for PIAL %s refused by the key authority: %v", user.PIALID, err)
		http.Error(w, `{"error":"key_authority_unavailable"}`, http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// balanceJSON is the native form of a balance Ain Soph reports as an AET
// string ("12.50"): settled µAET, and pending 0 — the webhook carries only
// the settled figure.
func balanceJSON(balanceAET string) string {
	f, err := strconv.ParseFloat(strings.TrimSpace(balanceAET), 64)
	if err != nil {
		return ""
	}
	return fmt.Sprintf(`{"balance_uaet":%d,"pending_uaet":0}`, int64(math.Round(f*1_000_000)))
}
