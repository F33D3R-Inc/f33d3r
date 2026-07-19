package handler

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
)

// balanceUpdateWebhook is called by Ain Soph after any transaction settles.
// It pushes a live balance SSE event to the user's active session immediately.
// POST /api/internal/balance-update
// Auth: X-Internal-Key header
func (h *Handler) balanceUpdateWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Internal-Key") != h.cfg.InternalAPIKey || h.cfg.InternalAPIKey == "" {
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
	PublishToUser(body.PialID, SSEEvent{Type: "balance", Data: body.BalanceAet})
	w.WriteHeader(http.StatusOK)
}

// themisPurchaseDeliveredWebhook is called by Themis when CEK wrapping is complete.
// It pushes a marketplace_purchase_delivered SSE event to the buyer's session.
// POST /api/internal/themis/purchase-delivered
// Auth: X-Internal-Key header
func (h *Handler) themisPurchaseDeliveredWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Internal-Key") != h.cfg.InternalAPIKey || h.cfg.InternalAPIKey == "" {
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
		"purchase_id":  body.PurchaseID,
		"content_url":  body.ContentURL,
		"content_hash": body.ContentHash,
		"cek_for_buyer": body.CekForBuyer,
	})
	PublishToUser(body.BuyerPial, SSEEvent{Type: "marketplace_purchase_delivered", Data: string(data)})
	w.WriteHeader(http.StatusOK)
}

// pialEcdhPubkeyInternal returns a PIAL's ECDH-P256 public key for internal use by Themis.
// Proxies to Elohim-veni first; falls back to local Nantar DB.
// GET /api/internal/pial/{pial_id}/ecdh-pubkey
// Auth: X-Internal-Key header
func (h *Handler) pialEcdhPubkeyInternal(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Internal-Key") != h.cfg.InternalAPIKey || h.cfg.InternalAPIKey == "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	pialID := r.PathValue("pial_id")
	if pialID == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// PIAL record (feed-engine) is authoritative — read local first.
	var pubKey string
	err := h.db.QueryRowContext(r.Context(),
		`SELECT public_key_b64 FROM pial_ecdh_keys WHERE pial_id::text = $1`, pialID).Scan(&pubKey)
	if err == nil {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"public_key_b64":%q}`, pubKey)
		return
	}
	if err != sql.ErrNoRows {
		http.Error(w, `{"error":"db_error"}`, http.StatusInternalServerError)
		return
	}

	// Fallback: Elohim-veni mirror (cross-brain replication / legacy).
	if h.cfg.ElohimVeniURL != "" {
		evReq, _ := http.NewRequestWithContext(r.Context(), "GET",
			h.cfg.ElohimVeniURL+"/v1/pial/"+pialID+"/ecdh-pubkey", nil)
		evReq.Header.Set("X-Internal-Key", h.cfg.InternalAPIKey)
		if resp, err := h.httpClient.Do(evReq); err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				w.Header().Set("Content-Type", "application/json")
				io.Copy(w, resp.Body)
				return
			}
		}
	}
	http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
}

// pialSigningPubkeyInternal returns a PIAL's ECDSA-P256 signing public key for Themis signature verification.
// PIAL record (feed-engine) is authoritative — reads local first, Elohim-veni key service as fallback.
// GET /api/internal/pial/{pial_id}/signing-pubkey
// Auth: X-Internal-Key header
func (h *Handler) pialSigningPubkeyInternal(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Internal-Key") != h.cfg.InternalAPIKey || h.cfg.InternalAPIKey == "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	pialID := r.PathValue("pial_id")
	if pialID == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Elohim-veni is the durable authority for PIAL signing keys — serve from there. (The local
	// pial_signing_keys table is now a write-through mirror only, never the read source of truth.)
	pub := h.fetchSigningPubkey(pialID)
	if pub == "" {
		http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"public_key_b64":%q}`, pub)
}

// registerPIALEcdhKey stores or updates the calling PIAL's ECDH-P256 public key.
// PIAL record (feed-engine) is authoritative — writes local first, then mirrors to
// Elohim-veni (a pipeline / cross-brain replica), which no longer owns the key.
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

	// Authoritative: write to the PIAL record (feed-engine) first.
	if h.db != nil {
		if _, err := h.db.ExecContext(r.Context(), `
			INSERT INTO pial_ecdh_keys (pial_id, public_key_b64)
			VALUES ($1::uuid, $2)
			ON CONFLICT (pial_id) DO UPDATE SET public_key_b64 = EXCLUDED.public_key_b64, updated_at = NOW()
		`, user.PIALID, req.PublicKeyB64); err != nil {
			http.Error(w, `{"error":"db_error"}`, http.StatusInternalServerError)
			return
		}
	}

	// Mirror to Elohim-veni (pipeline / cross-brain replica) — best effort.
	if h.cfg.ElohimVeniURL != "" {
		payload, _ := json.Marshal(map[string]string{
			"pial_id":        user.PIALID,
			"public_key_b64": req.PublicKeyB64,
		})
		evReq, _ := http.NewRequestWithContext(r.Context(), "POST",
			h.cfg.ElohimVeniURL+"/v1/pial/ecdh-key/register",
			bytes.NewReader(payload))
		evReq.Header.Set("Content-Type", "application/json")
		evReq.Header.Set("X-Internal-Key", h.cfg.InternalAPIKey)
		if resp, err := h.httpClient.Do(evReq); err == nil {
			resp.Body.Close()
		} else {
			log.Printf("[pial] ECDH key Elohim-veni mirror failed for PIAL %s (local record is source of truth)", user.PIALID)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}
