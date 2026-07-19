package handler

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"strings"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

// resolveHandleToPIAL resolves a @handle to its PIAL, bootstrapping one if the user has none.
// Used by the PIAL signing-key directory (by-handle lookup). Lives here because the messaging
// layer that originally defined it was removed.
func (h *Handler) resolveHandleToPIAL(handle string) string {
	handle = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(handle, "@")))
	if handle == "" {
		return ""
	}
	var pial, uid string
	_ = h.db.QueryRow(`SELECT COALESCE(pial_id::text,''), id::text FROM users WHERE LOWER(handle) = LOWER($1)`, handle).Scan(&pial, &uid)
	if pial == "" && uid != "" {
		pial = dbpkg.BootstrapPIAL(h.db, uid, handle)
	}
	return pial
}

// registerPIALSigningKey stores the browser-generated ECDSA-P256 public key for a PIAL.
// The PIAL record (feed-engine) is authoritative — writes local first, then mirrors to the
// Elohim-veni key service. Private key never leaves the device.
// POST /api/pial/signing-key/register  JSON: {"public_key_b64":"...","algorithm":"ECDSA-P256"}
func (h *Handler) registerPIALSigningKey(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.PIALID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		PublicKeyB64 string `json:"public_key_b64"`
		Algorithm    string `json:"algorithm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.PublicKeyB64 == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	algo := req.Algorithm
	if algo == "" {
		algo = "ECDSA-P256"
	}

	// Authoritative: write to the PIAL record (feed-engine) first.
	if err := dbpkg.RegisterPIALSigningKey(h.db, user.PIALID, req.PublicKeyB64, algo); err != nil {
		log.Printf("[pial] signing key local write failed for PIAL %s: %v", user.PIALID, err)
		http.Error(w, `{"error":"db_error"}`, http.StatusInternalServerError)
		return
	}

	// Mirror to Elohim-veni key service (best effort).
	if h.cfg.ElohimVeniURL != "" {
		payload, _ := json.Marshal(map[string]string{
			"pial_id":        user.PIALID,
			"public_key_b64": req.PublicKeyB64,
			"algorithm":      algo,
		})
		evReq, _ := http.NewRequestWithContext(r.Context(), "POST",
			h.cfg.ElohimVeniURL+"/v1/pial/signing-key/register",
			bytes.NewReader(payload))
		evReq.Header.Set("Content-Type", "application/json")
		evReq.Header.Set("X-Internal-Key", h.cfg.InternalAPIKey)
		if resp, err := h.httpClient.Do(evReq); err == nil {
			resp.Body.Close()
		} else {
			log.Printf("[pial] signing key Elohim-veni mirror failed for PIAL %s (local record is source of truth)", user.PIALID)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// pialByHandle resolves a @handle to its PIAL UUID for the wallet send/tip flow.
// GET /api/pial/by-handle/{handle} → {"identity":"<pial>","handle":"<h>"} or 404.
// Replaces a retired external identity-service lookup.
func (h *Handler) pialByHandle(w http.ResponseWriter, r *http.Request) {
	if user := h.userFromRequest(w, r); user == nil || user.PIALID == "" {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	handle := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(r.PathValue("handle"), "@")))
	if handle == "" {
		http.Error(w, `{"error":"handle_required"}`, http.StatusBadRequest)
		return
	}
	pial := h.resolveHandleToPIAL(handle)
	if pial == "" {
		http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"identity": pial, "handle": handle})
}
