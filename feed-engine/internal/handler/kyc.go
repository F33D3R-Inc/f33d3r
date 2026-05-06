package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/f33d3r/feed-engine/internal/model"
)

// ── KYC page ──────────────────────────────────────────────────────────────────
func (h *Handler) kycPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	page := model.KYCPage{User: user}

	if h.cfg.VerityURL != "" && user.PIALID != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			h.cfg.VerityURL+"/v1/kyc/status?pial_id="+user.PIALID, nil)
		if err == nil {
			if resp, err := h.httpClient.Do(req); err == nil {
				defer resp.Body.Close()
				var v struct {
					Tier          int    `json:"tier"`
					AgeBand       string `json:"age_band"`
					PayoutEnabled bool   `json:"payout_enabled"`
					NSFWAccess    bool   `json:"nsfw_access"`
					Has2257       bool   `json:"has_2257"`
				}
				if json.NewDecoder(resp.Body).Decode(&v) == nil {
					page.VerityTier    = v.Tier
					page.AgeBand       = v.AgeBand
					page.PayoutEnabled = v.PayoutEnabled
					page.NSFWAccess    = v.NSFWAccess
					page.Has2257       = v.Has2257
				}
			}
		}
	}

	h.render(w, "kyc.html", map[string]interface{}{
		"User":          page.User,
		"VerityTier":    page.VerityTier,
		"AgeBand":       page.AgeBand,
		"PayoutEnabled": page.PayoutEnabled,
		"NSFWAccess":    page.NSFWAccess,
		"Has2257":       page.Has2257,
		"Title":         "Verification · F33D3R",
		"SessionID":     uuid.New().String(),
		"ShowScores":    h.cfg.ShowScores,
		"Themes":        ThemesWithActive(page.User.ThemeID),
	})
}

// ekycResult is the response from the ekyc Python service /v1/verify
type ekycResult struct {
	Pass        bool    `json:"pass"`
	Confidence  float64 `json:"confidence"`
	AgeBand     string  `json:"age_band"`
	AgeVerified bool    `json:"age_verified"`
	Reason      string  `json:"reason"`
	Liveness    struct {
		Passed bool    `json:"passed"`
		Score  float64 `json:"score"`
	} `json:"liveness"`
	FaceMatch struct {
		Verified bool    `json:"verified"`
		Score    float64 `json:"score"`
	} `json:"face_match"`
	OCR struct {
		DOB  string `json:"dob"`
		Age  *int   `json:"age"`
		Name string `json:"name"`
	} `json:"ocr"`
	Antispoof struct {
		LikelyReal bool    `json:"likely_real"`
		Score      float64 `json:"score"`
	} `json:"antispoof"`
}

// ── KYC submit — full pipeline: eKYC → Verity → local DB ─────────────────────
func (h *Handler) kycSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", 405)
		return
	}
	user := h.userFromRequest(w, r)
	_ = r.ParseForm()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// Collect client payload fields
	docImage   := r.FormValue("document_image")
	faceImage  := r.FormValue("face_image")
	livenessJSON := r.FormValue("liveness_results")
	livenessPassed := r.FormValue("liveness_passed")
	docHash    := r.FormValue("document_hash")
	ageBand    := r.FormValue("age_band")
	if ageBand == "" { ageBand = "unknown" }

	// ── Phase 2-4: eKYC service ───────────────────────────────────────────────
	var ekyc *ekycResult

	if h.cfg.EKYCUrl != "" && docImage != "" && faceImage != "" {
		var livenessArr []map[string]interface{}
		if livenessJSON != "" {
			_ = json.Unmarshal([]byte(livenessJSON), &livenessArr)
		}

		ekycPayload := map[string]interface{}{
			"document_image":   docImage,
			"face_image":       faceImage,
			"liveness_results": livenessArr,
		}
		body, _ := json.Marshal(ekycPayload)

		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			h.cfg.EKYCUrl+"/v1/verify", bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			resp, err := h.httpClient.Do(req)
			if err != nil {
				log.Printf("[kyc] ekyc service error: %v", err)
				w.Write([]byte(`<span style="color:#ef4444;font-size:13px">Verification service unavailable — try again shortly.</span>`))
				return
			}
			defer resp.Body.Close()
			var result ekycResult
			if json.NewDecoder(resp.Body).Decode(&result) == nil {
				ekyc = &result
			}
		}

		if ekyc == nil {
			w.Write([]byte(`<span style="color:#ef4444;font-size:13px">Verification could not be processed — try again.</span>`))
			return
		}

		if !ekyc.Pass {
			msg := friendlyKYCError(ekyc.Reason)
			fmt.Fprintf(w, `<span style="color:#ef4444;font-size:13px">%s</span>`, msg)
			return
		}

		// Use ekyc-extracted age band if better than client-supplied
		if ekyc.AgeBand != "" && ekyc.AgeBand != "unknown" {
			ageBand = ekyc.AgeBand
		}
	} else if h.cfg.EKYCUrl == "" {
		// Dev mode: trust client liveness count
		passed := 0
		fmt.Sscanf(livenessPassed, "%d", &passed)
		if passed < 1 {
			w.Write([]byte(`<span style="color:#ef4444;font-size:13px">Liveness check failed — please retry.</span>`))
			return
		}
	}

	// ── Forward to Verity for record-keeping + tier update ────────────────────
	confidence := 0.0
	if ekyc != nil {
		confidence = ekyc.Confidence
	} else {
		confidence = 0.75 // dev mode: no ekyc, grant pass
	}

	if h.cfg.VerityURL != "" && user.PIALID != "" {
		verityPayload := map[string]interface{}{
			"pial_id":          user.PIALID,
			"submission_type":  "gov_id",
			"age_band":         ageBand,
			"confidence":       confidence,
			"provider":         "ekyc_internal",
			"document_hash":    docHash,
		}
		if ekyc != nil && ekyc.OCR.Name != "" {
			verityPayload["metadata"] = map[string]interface{}{
				"face_score":    ekyc.FaceMatch.Score,
				"spoof_score":   ekyc.Antispoof.Score,
				"liveness_score": ekyc.Liveness.Score,
				"ocr_name":      ekyc.OCR.Name,
				"ocr_dob":       ekyc.OCR.DOB,
			}
		}
		vBody, _ := json.Marshal(verityPayload)
		ctx2, cancel2 := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel2()
		vReq, err := http.NewRequestWithContext(ctx2, http.MethodPost,
			h.cfg.VerityURL+"/v1/kyc/submit", bytes.NewReader(vBody))
		if err == nil {
			vReq.Header.Set("Content-Type", "application/json")
			vResp, err := h.httpClient.Do(vReq)
			if err == nil {
				vResp.Body.Close()
			}
		}
	}

	// ── Update local user_profiles.is_verified ────────────────────────────────
	if h.db != nil && user.ID != "" {
		_, err := h.db.Exec(
			`UPDATE user_profiles SET is_verified = TRUE WHERE user_id = $1`,
			user.ID,
		)
		if err != nil {
			log.Printf("[kyc] mark verified: %v", err)
		}
	}

	// ── Phase 5: send risk signal (baseline for continuous re-scoring) ─────────
	go h.kycRiskSignal(user.PIALID, r)

	w.Write([]byte(`<span style="color:#22c55e;font-size:13px;font-weight:500">✓ Identity verified — reload to see your badge.</span>`))
}

func friendlyKYCError(reason string) string {
	switch reason {
	case "liveness_failed":
		return "Liveness check failed — please ensure good lighting and try again."
	case "presentation_attack":
		return "Anti-spoof check failed — please use your real face, not a photo or screen."
	case "face_mismatch":
		return "Your face does not match the ID document photo. Ensure the document is the one with your photo."
	case "age_verification_failed":
		return "Age verification failed. You must be 18 or older to continue."
	case "missing_images":
		return "Document or face image missing — please complete all steps."
	default:
		return "Verification could not be completed. Ensure good lighting, a clear ID document, and try again."
	}
}

// kycRiskSignal sends a baseline risk signal to Verity after successful KYC.
func (h *Handler) kycRiskSignal(pialID string, r *http.Request) {
	if h.cfg.VerityURL == "" || pialID == "" {
		return
	}
	body, _ := json.Marshal(map[string]interface{}{
		"pial_id":       pialID,
		"signal":        "kyc_completed",
		"ip_reputation": 0.9,
		"velocity_score": 0.0,
		"context":       "kyc_success",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		h.cfg.VerityURL+"/v1/risk/signal", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// ── Adult enable ──────────────────────────────────────────────────────────────
func (h *Handler) kycAdultEnable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", 405)
		return
	}
	user := h.userFromRequest(w, r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if h.cfg.VerityURL == "" || user.PIALID == "" {
		w.Write([]byte(`<span style="color:#22c55e;font-size:13px">Adult creator mode enabled.</span>`))
		return
	}

	body, _ := json.Marshal(map[string]string{"pial_id": user.PIALID})
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		h.cfg.VerityURL+"/v1/kyc/adult/enable", bytes.NewReader(body))
	if err != nil {
		w.Write([]byte(`<span style="color:#ef4444;font-size:13px">Failed — please try again.</span>`))
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.httpClient.Do(req)
	if err != nil || resp.StatusCode >= 500 {
		w.Write([]byte(`<span style="color:#ef4444;font-size:13px">Service unavailable — try again.</span>`))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 300 {
		w.Write([]byte(`<span style="color:#22c55e;font-size:13px;font-weight:500">✓ Adult creator mode enabled. Reload to see your updated status.</span>`))
	} else {
		w.Write([]byte(`<span style="color:#ef4444;font-size:13px">Could not enable — complete Tier 2 verification first.</span>`))
	}
}
