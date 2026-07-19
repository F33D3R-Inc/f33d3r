package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
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
		"KYCMode":       r.URL.Query().Get("mode"),
		"Title":         "Verification · F33D3R",
		"SessionID":     uuid.New().String(),
		"ShowScores":    h.cfg.ShowScores,
		"Themes":        ThemesWithActive(page.User.ThemeID),
	})
}

// ── MRZ age-only verification (Lane 1) ────────────────────────────────────────
// POST /kyc/age-verify
// Takes a government-ID photo, reads the MRZ zone, confirms 18+.
// No face match. No liveness. Image discarded by eKYC immediately.
func (h *Handler) handleMRZAgeVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", 405)
		return
	}
	user := h.userFromRequest(w, r)
	_ = r.ParseForm()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	docImageB64 := r.FormValue("document_image")
	if docImageB64 == "" {
		w.Write([]byte(`<span style="color:#ef4444;font-size:13px">No image received — please capture your ID document.</span>`))
		return
	}

	// Strip data URI prefix if present (data:image/jpeg;base64,...)
	if idx := strings.Index(docImageB64, ","); idx != -1 {
		docImageB64 = docImageB64[idx+1:]
	}
	imgBytes, err := base64.StdEncoding.DecodeString(docImageB64)
	if err != nil {
		w.Write([]byte(`<span style="color:#ef4444;font-size:13px">Invalid image data — please retake the photo.</span>`))
		return
	}

	if h.cfg.EKYCUrl == "" {
		// Dev mode: grant age verification without calling eKYC.
		if h.db != nil && user.PIALID != "" {
			// Single record: write verification to PIAL (sets age_verified, tier, locks DOB).
			_ = dbpkg.SetPIALVerified(h.db, user.PIALID, "soft", "ekyc:dev")
			if tok := GetSessionToken(r); tok != "" && !strings.HasPrefix(tok, "__handle__") {
				h.sessionCache.Delete(tok)
			}
			h.sessionCache.Delete("__handle__" + user.Handle)
		}
		w.Write([]byte(`<span style="color:#22c55e;font-size:13px;font-weight:500">✓ Age verified — reload to continue.</span>`))
		return
	}

	// Build multipart body for eKYC /v1/mrz/verify
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("image", "document.jpg")
	if err != nil {
		w.Write([]byte(`<span style="color:#ef4444;font-size:13px">Could not process image — please try again.</span>`))
		return
	}
	if _, err = fw.Write(imgBytes); err != nil {
		mw.Close()
		w.Write([]byte(`<span style="color:#ef4444;font-size:13px">Could not process image — please try again.</span>`))
		return
	}
	mw.Close()

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		h.cfg.EKYCUrl+"/v1/mrz/verify", &buf)
	if err != nil {
		w.Write([]byte(`<span style="color:#ef4444;font-size:13px">Verification service unavailable — try again shortly.</span>`))
		return
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := h.httpClient.Do(req)
	if err != nil {
		log.Printf("[kyc/mrz] ekyc error: %v", err)
		w.Write([]byte(`<span style="color:#ef4444;font-size:13px">Verification service unavailable — try again shortly.</span>`))
		return
	}
	defer resp.Body.Close()

	var result struct {
		Verified bool   `json:"verified"`
		Is18Plus bool   `json:"is_18_plus"`
		AgeBand  string `json:"age_band"`
		DocType  string `json:"doc_type"`
		Country  string `json:"country"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || !result.Verified {
		w.Write([]byte(`<span style="color:#ef4444;font-size:13px">We could not read your ID. Please use a passport or national ID card — a library card or gym membership will not work.</span>`))
		return
	}
	if !result.Is18Plus {
		w.Write([]byte(`<span style="color:#ef4444;font-size:13px">You must be 18 or older to continue.</span>`))
		return
	}

	if h.db != nil && user.PIALID != "" {
		// MRZ document verified 18+ (no liveness) → soft tier on PIAL.
		_ = dbpkg.SetPIALVerified(h.db, user.PIALID, "soft", "ekyc:mrz")
		if tok := GetSessionToken(r); tok != "" && !strings.HasPrefix(tok, "__handle__") {
			h.sessionCache.Delete(tok)
		}
		h.sessionCache.Delete("__handle__" + user.Handle)
	}

	w.Write([]byte(`<span style="color:#22c55e;font-size:13px;font-weight:500">✓ Age verified — reload to continue.</span>`))
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

// ── Liveness challenge proxy ──────────────────────────────────────────────────
// Issues a server-side challenge session from the eKYC service.
// JS calls this before starting the face scan to get a session_id + challenge order.
func (h *Handler) kycLivenessChallenge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", 405)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	if h.cfg.EKYCUrl == "" {
		// Dev mode — return a fixed challenge so the flow still works without the service
		w.Write([]byte(`{"session_id":"dev-session","challenges":["blink","look_up","smile"],"ttl":120}`))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.cfg.EKYCUrl+"/v1/liveness/challenge", nil)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":"challenge_unavailable"}`))
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":"challenge_unavailable"}`))
		return
	}
	defer resp.Body.Close()
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
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
	docImage            := r.FormValue("document_image")
	faceImage           := r.FormValue("face_image")
	livenessSessionID   := r.FormValue("liveness_session_id")
	livenessFramesJSON  := r.FormValue("liveness_challenge_frames")
	livenessPassed      := r.FormValue("liveness_passed")
	docHash             := r.FormValue("document_hash")
	ageBand             := r.FormValue("age_band")
	if ageBand == "" { ageBand = "unknown" }

	// ── Phase 2-4: eKYC service ───────────────────────────────────────────────
	var ekyc *ekycResult

	if h.cfg.EKYCUrl != "" && docImage != "" && faceImage != "" {
		var livenessFrames []string
		if livenessFramesJSON != "" {
			_ = json.Unmarshal([]byte(livenessFramesJSON), &livenessFrames)
		}

		ekycPayload := map[string]interface{}{
			"document_image":           docImage,
			"face_image":               faceImage,
			"liveness_session_id":      livenessSessionID,
			"liveness_challenge_frames": livenessFrames,
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

	// ── Update PIAL: verification + age classification (the single record) ─────
	if h.db != nil && user.PIALID != "" {
		// Invalidate session cache so the next request immediately sees the new state.
		// Without this the feed gate stays stale for up to 30 seconds after KYC.
		if tok := GetSessionToken(r); tok != "" && !strings.HasPrefix(tok, "__handle__") {
			h.sessionCache.Delete(tok)
		}
		h.sessionCache.Delete("__handle__" + user.Handle)

		// Propagate the eKYC result to PIAL — the single record. A confirmed minor is
		// never marked age_verified (is_minor is derived from the DOB we lock here);
		// an adult gets full-tier verification.
		if ekyc != nil {
			isMinorConfirmed := ekyc.AgeBand == "minor"
			// OCR DOB is authoritative — record + lock it so is_adult/is_minor derive from it.
			if ekyc.OCR.DOB != "" {
				_, _ = h.db.Exec(`
					UPDATE pial_roots
					SET date_of_birth = $1, dob_locked = TRUE,
					    is_adult = (date_part('year', age($1::date)) >= 18),
					    is_minor = (date_part('year', age($1::date)) <  18)
					WHERE pial_id = $2`,
					ekyc.OCR.DOB, user.PIALID,
				)
			}
			if !isMinorConfirmed {
				_ = dbpkg.SetPIALVerified(h.db, user.PIALID, "full", "ekyc")
			}
		} else {
			// Dev mode: still mark age_verified so the flag is set.
			_ = dbpkg.SetPIALVerified(h.db, user.PIALID, "full", "ekyc:dev")
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

// ── Creator statement ─────────────────────────────────────────────────────────
// GET /kyc/creator/statement
// Returns an HTML fragment containing the 18 USC 2257 record-keeping statement.
// Fetched live from Verity; falls back to hardcoded statutory text if unavailable.
func (h *Handler) kycCreatorStatement(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	const fallback = "In accordance with 18 U.S.C. § 2257, all models, actors, actresses and other persons who appear in any visual depiction of actual or simulated sexually explicit conduct appearing on this platform were 18 years of age or older at the time of the creation of such depictions. Records required to be maintained pursuant to 18 U.S.C. § 2257 are kept by F33D3R Platform as custodian of records. F33D3R Platform, online at f33d3r.com."

	var statement string

	if h.cfg.VerityURL != "" && user.PIALID != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			h.cfg.VerityURL+"/v1/compliance/2257/statement/"+user.PIALID, nil)
		if err == nil {
			resp, err := h.httpClient.Do(req)
			if err == nil {
				defer resp.Body.Close()
				var v struct {
					Statement string `json:"statement"`
				}
				if json.NewDecoder(resp.Body).Decode(&v) == nil && v.Statement != "" {
					statement = v.Statement
				}
			}
		}
	}

	if statement == "" {
		statement = fallback
	}

	fmt.Fprintf(w, `<div class="kyc-agreement-text">%s</div>`, template.HTMLEscapeString(statement))
}

// ── Creator accept ────────────────────────────────────────────────────────────
// POST /kyc/creator/accept
// Activates adult creator mode after verifying KYC prerequisites and recording
// the 2257 compliance record in Verity (fire-and-forget).
func (h *Handler) kycCreatorAccept(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", 405)
		return
	}
	user := h.userFromRequest(w, r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// Require both is_age_verified and is_verified before activating.
	if !user.IsAgeVerified {
		w.Write([]byte(`<span style="color:#ef4444;font-size:13px">Age verification must be completed before activating adult creator mode.</span>`))
		return
	}
	if !user.IsVerified {
		w.Write([]byte(`<span style="color:#ef4444;font-size:13px">Identity verification must be completed before activating adult creator mode.</span>`))
		return
	}

	// Fire-and-forget: record the 2257 compliance entry in Verity.
	if h.cfg.VerityURL != "" && user.PIALID != "" {
		go func(pialID string) {
			body, _ := json.Marshal(map[string]string{
				"pial_id":  pialID,
				"age_band": "18+",
			})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost,
				h.cfg.VerityURL+"/v1/compliance/2257/record", bytes.NewReader(body))
			if err != nil {
				return
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := h.httpClient.Do(req)
			if err != nil {
				return
			}
			resp.Body.Close()
		}(user.PIALID)
	}

	// Update local DB.
	if h.db != nil && user.ID != "" {
		// Becoming an adult creator implies wanting to see adult content directly —
		// promote content_setting to adult_enabled so they aren't stuck on the
		// "standard" 18+ click-to-reveal gate. KYC-approved here, so never a minor.
		_, err := h.db.Exec(`
			UPDATE user_profiles
			SET is_adult_creator = TRUE, adult_creator_pending = FALSE,
			    content_setting = 'adult_enabled'
			WHERE user_id = $1
		`, user.ID)
		if err != nil {
			log.Printf("[kyc/creator/accept] db update: %v", err)
		}

		// Invalidate session cache so the next request sees the updated flags.
		if tok := GetSessionToken(r); tok != "" && !strings.HasPrefix(tok, "__handle__") {
			h.sessionCache.Delete(tok)
		}
		h.sessionCache.Delete("__handle__" + user.Handle)
	}

	w.Write([]byte(`<span style="color:#22c55e;font-size:13px;font-weight:500">✓ Adult Creator activated — <a href="/shop" style="color:var(--accent)">Set up your shop →</a></span>`))
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

	// Minors (is_minor=true) can never enable adult content regardless of
	// what they claim their age is. This blocks the minor_16 path from
	// flipping to adult_enabled via KYC.
	if user.IsMinor {
		w.Write([]byte(`<span style="color:#ef4444;font-size:13px">Adult content cannot be enabled on this account.</span>`))
		return
	}

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
