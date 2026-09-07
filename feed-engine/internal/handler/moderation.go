package handler

// moderation.go — content moderation handlers and admin review queue.
//
// Lifecycle:
//   1. Post created → scan_state = 'pending_scan'
//   2. Abraxas Shield consumes content.events from Sitra Achra (Kafka)
//   3. Abraxas Shield writes to content_scan_results and transitions scan_state directly
//   4. scan_complete events (legacy HTTP path) still supported via scanCompleteEvent
//   5. Moderator reviews 'human_review' queue in admin panel
//   6. Moderator approves/blocks → SetPostScanState

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/bits"
	"net/http"
	"strconv"
	"strings"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

// ScanResult payload from content-scan brain.
// Sent via POST /api/internal/scan-complete with X-Internal-Key header.
type scanCompletePayload struct {
	PostID          string   `json:"post_id"`
	NudityScore     float64  `json:"nudity_score"`
	GoreScore       float64  `json:"gore_score"`
	ClickbaitScore  float64  `json:"clickbait_score"`
	OCRText         string   `json:"ocr_text"`
	Transcript      string   `json:"transcript"`
	HateSignals     []string `json:"hate_signals"`
	ViolenceSignals []string `json:"violence_signals"`
	SelfHarmSignals []string `json:"self_harm_signals"`
	RiskLevel       string   `json:"risk_level"`     // clean | age_gate | review | block
	Recommendation  string   `json:"recommendation"` // approve | age_gate | human_review | auto_block
	Signals         []string `json:"signals"`
	IsDuplicate     bool     `json:"is_duplicate"`
	DuplicateType   string   `json:"duplicate_type"`
	OriginalPostID  string   `json:"original_post_id"`
	OriginalHandle  string   `json:"original_handle"`
	OriginalPIAL    string   `json:"original_pial"`
	ScanVersion     string   `json:"scan_version"`
}

// scanCompleteEvent receives the async content-scan result and transitions scan_state.
// Called only by the content-scan brain (validated via X-Internal-Key in eventsPost dispatcher).
func (h *Handler) scanCompleteEvent(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	var payload scanCompletePayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if payload.PostID == "" {
		http.Error(w, "post_id required", http.StatusBadRequest)
		return
	}

	// Store raw scan signals for audit trail.
	scanResult := &dbpkg.ScanResult{
		PostID:          payload.PostID,
		ScanVersion:     orDefault(payload.ScanVersion, "2.1"),
		NudityScore:     payload.NudityScore,
		GoreScore:       payload.GoreScore,
		ClickbaitScore:  payload.ClickbaitScore,
		OCRText:         payload.OCRText,
		Transcript:      payload.Transcript,
		HateSignals:     payload.HateSignals,
		ViolenceSignals: payload.ViolenceSignals,
		SelfHarmSignals: payload.SelfHarmSignals,
		RiskLevel:       payload.RiskLevel,
		Recommendation:  payload.Recommendation,
		Signals:         payload.Signals,
		IsDuplicate:     payload.IsDuplicate,
		DuplicateType:   payload.DuplicateType,
		OriginalPostID:  payload.OriginalPostID,
	}
	// The evidence behind the state transition below. Storing the state without
	// the result it was derived from leaves a moderated work whose reason nobody
	// can reconstruct, so the scanner is told to retry instead.
	if err := dbpkg.StoreScanResult(h.db, scanResult); err != nil {
		log.Printf("[scan-complete] scan result NOT stored for post=%s: %v", payload.PostID, err)
		http.Error(w, "db_error", http.StatusInternalServerError)
		return
	}

	// Map recommendation to scan_state transition.
	var nextState string
	switch payload.Recommendation {
	case "approve":
		nextState = "clean"
	case "age_gate":
		nextState = "age_gated"
	case "human_review":
		nextState = "human_review"
	case "auto_block":
		nextState = "blocked"
	default:
		// Unknown recommendation — send to human review rather than auto-clear.
		nextState = "human_review"
	}

	if err := dbpkg.SetPostScanState(h.db, payload.PostID, nextState, "content-scan: "+payload.RiskLevel, "system"); err != nil {
		http.Error(w, "db_error", http.StatusInternalServerError)
		return
	}

	// Notify all admin users when a post enters human_review so they don't have
	// to manually poll the admin panel to discover the queue has items.
	if nextState == "human_review" {
		for _, adminID := range dbpkg.GetAdminUserIDs(h.db) {
			h.notifyUser(adminID, "moderation", "", payload.PostID, "post")
		}
	}

	// If age_gated, also set is_nsfw = true on the post/work so the content_gate
	// overlay fires without needing a separate check.
	if nextState == "age_gated" {
		h.db.Exec(`UPDATE works SET is_nsfw = TRUE WHERE id = $1`, payload.PostID)
	}

	// Duplicate lineage: set attribution and redirect the duplicate post to serve
	// the original creator's canonical video asset. Only fires for non-self duplicates
	// (original_handle will equal the poster's handle for self-uploads, which are
	// handled by the direct syncCheckDuplicate path in createPost).
	if payload.IsDuplicate && payload.OriginalHandle != "" {
		if err := dbpkg.SetPostLineage(h.db, payload.PostID, payload.OriginalPIAL, payload.OriginalHandle); err != nil {
			log.Printf("[scan-complete] SetPostLineage error post=%s: %v", payload.PostID, err)
		}
		if payload.OriginalPostID != "" {
			if err := dbpkg.RedirectDuplicatePost(h.db, payload.PostID, payload.OriginalPostID); err != nil {
				log.Printf("[scan-complete] RedirectDuplicatePost error post=%s: %v", payload.PostID, err)
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true,"state":"` + nextState + `"}`))
}

// ModerationQueueGet returns the human_review + flagged posts for admin panel.
// GET /admin/moderation/queue
func (h *Handler) ModerationQueueGet(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if h.db == nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}
	queue, err := dbpkg.GetHumanReviewPosts(h.db, 100)
	if err != nil {
		http.Error(w, "db_error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"queue": queue,
		"count": len(queue),
	})
}

// ModerationActionPost sets scan_state on a post (admin action: approve/block/age_gate).
// POST /admin/moderation/action  {post_id, action: "approve"|"block"|"age_gate", reason}
func (h *Handler) ModerationActionPost(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if h.db == nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}

	// HTMX sends hx-vals as form-encoded data, not JSON.
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	req := struct {
		PostID string
		Action string
		Reason string
	}{
		PostID: r.FormValue("post_id"),
		Action: r.FormValue("action"),
		Reason: r.FormValue("reason"),
	}
	if req.PostID == "" || req.Action == "" {
		http.Error(w, "post_id and action required", http.StatusBadRequest)
		return
	}

	var nextState string
	switch req.Action {
	case "approve":
		nextState = "clean"
	case "block":
		nextState = "blocked"
	case "age_gate":
		nextState = "age_gated"
		h.db.Exec(`UPDATE works SET is_nsfw = TRUE WHERE id = $1`, req.PostID)
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}

	reason := req.Reason
	if reason == "" {
		reason = "moderator: " + req.Action
	}

	if err := dbpkg.SetPostScanState(h.db, req.PostID, nextState, reason, user.Handle); err != nil {
		http.Error(w, "db_error", http.StatusInternalServerError)
		return
	}

	// A moderator decision on the content closes every open user report pointing
	// at it. Without this the reports stay pending and the card resurrects on the
	// next queue poll — the "stuck post" bug. One decision, both pipelines cleared.
	if n, _ := dbpkg.ResolveReportsForContent(h.db, req.PostID, "resolved", user.Handle); n > 0 {
		log.Printf("[admin] %s %s on post %s also resolved %d report(s)", user.Handle, req.Action, req.PostID, n)
	}

	// Record admin decision as training signal for Abraxas.
	// Fetch the signals that caused this post to be flagged.
	var signals []string
	rows, _ := h.db.Query(`SELECT unnest(signals) FROM content_scan_results WHERE post_id = $1`, req.PostID)
	if rows != nil {
		for rows.Next() {
			var sig string
			rows.Scan(&sig)
			signals = append(signals, sig)
		}
		rows.Close()
	}
	if len(signals) > 0 {
		// Feedback into the scanner's training signal. Best-effort by design — the
		// moderator's decision has already been applied — but reported when lost.
		if err := dbpkg.RecordScanFeedback(h.db, req.PostID, req.Action, user.Handle, signals); err != nil {
			log.Printf("[moderation] scan feedback not recorded for post=%s: %v", req.PostID, err)
		}
	}

	// TODO_WORKS: push approved work SSE event to author via works-based renderer.

	// Notify the author when a post is blocked or age-gated — both are violations.
	// age_gate also flags the account: the author posted explicit content without
	// marking it 18+, which violates the content policy on sign-up.
	if (nextState == "blocked" || nextState == "age_gated") && h.db != nil {
		var authorID string
		if err := h.db.QueryRow(`SELECT author_id FROM works WHERE id = $1`, req.PostID).Scan(&authorID); err == nil && authorID != "" {
			h.notifyUser(authorID, "violation", "", req.PostID, "post")
			if nextState == "age_gated" {
				var pialID string
				h.db.QueryRow(`SELECT pial_id FROM users WHERE id = $1`, authorID).Scan(&pialID)
				if pialID != "" {
					dbpkg.LogPIALEvent(h.db, pialID, authorID, "content_violation_age_gate", map[string]interface{}{
						"post_id": req.PostID, "reason": req.Reason, "actioned_by": user.Handle,
					}, "moderation")
				}
			}
		}
	}

	// Return an HTML confirmation fragment — HTMX swaps this into the card slot.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var label, colour string
	switch nextState {
	case "clean":
		label, colour = "✓ Approved — post is now visible", "#22c55e"
	case "age_gated":
		label, colour = "✓ Age-gated — 18+ gate applied", "#a855f7"
	case "blocked":
		label, colour = "✕ Blocked — post hidden, author notified", "#ef4444"
	default:
		label, colour = "✓ Action applied", "#22c55e"
	}
	log.Printf("[admin] %s → %s on post %s", user.Handle, nextState, req.PostID)
	w.Write([]byte(`<div style="padding:10px 16px;font-size:13px;font-weight:500;color:` + colour + `;border-bottom:1px solid var(--border-soft)">` + label + `</div>`))
}

// ModerationHashBan adds a SHA-256 or phash to the banned content registry.
// POST /admin/moderation/hash  {hash_type, hash_value, category, note}
func (h *Handler) ModerationHashBan(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if h.db == nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		HashType  string `json:"hash_type"`
		HashValue string `json:"hash_value"`
		Category  string `json:"category"`
		Note      string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	validTypes := map[string]bool{"sha256": true, "phash": true, "audio_fp": true}
	validCats := map[string]bool{"csam": true, "gore": true, "hate": true, "spam": true}
	if !validTypes[req.HashType] || !validCats[req.Category] {
		http.Error(w, "invalid hash_type or category", http.StatusBadRequest)
		return
	}

	hashValue := strings.TrimSpace(req.HashValue)
	if err := validateBannedHashValue(req.HashType, hashValue); err != nil {
		// A malformed entry does not fail loudly later — it simply never matches
		// anything, which is the silent-empty-gate failure this registry keeps
		// producing. It is refused here instead.
		log.Printf("[admin] %s offered an unusable %s value: %v", user.Handle, req.HashType, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := dbpkg.AddBannedHash(h.db, req.HashType, hashValue, req.Category, user.Handle, req.Note); err != nil {
		log.Printf("[admin] %s could not register %s %s: %v", user.Handle, req.HashType, hashPrefix(hashValue), err)
		http.Error(w, "db_error", http.StatusInternalServerError)
		return
	}
	log.Printf("[admin] %s registered banned %s %s (%s)", user.Handle, req.HashType, hashPrefix(hashValue), req.Category)

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// validateBannedHashValue refuses a registry entry that could not do its job.
//
// The two failure modes are opposite and both silent:
//
//   - A malformed value (wrong length, not hex) matches nothing. The gate then
//     reads as enforced while catching nothing, which is how this registry sat
//     empty and unnoticed in the first place.
//   - A degenerate perceptual hash matches far too much. content-scan compares
//     a phash by Hamming distance, so the hash of a near-uniform frame — nearly
//     all zero or nearly all one bits — sits inside the block radius of a large
//     share of ordinary dark or blank media. Registering one would refuse
//     innocent uploads across the platform.
//
// An operator who cannot register a frame's phash can still register its
// sha256, which carries no such risk.
func validateBannedHashValue(hashType, value string) error {
	switch hashType {
	case "sha256":
		if len(value) != 64 || !isLowerHex(value) {
			return errors.New("sha256 must be 64 lowercase hex characters")
		}
	case "phash":
		if len(value) != 16 || !isLowerHex(value) {
			return errors.New("phash must be 16 lowercase hex characters (a 64-bit imagehash phash)")
		}
		value64, err := strconv.ParseUint(value, 16, 64)
		if err != nil {
			return errors.New("phash is not a 64-bit hex value")
		}
		ones := bits.OnesCount64(value64)
		if ones < 12 || ones > 52 {
			return fmt.Errorf("phash %s has %d of 64 bits set — that is the hash of a near-uniform "+
				"image, and matching it by distance would block ordinary blank or dark media; "+
				"register the sha256 instead", value, ones)
		}
	case "audio_fp":
		// A chromaprint fingerprint is base64 of compressed data; its length
		// varies with duration. Only an obviously truncated value is refused.
		if len(value) < 16 {
			return errors.New("audio_fp is too short to be a chromaprint fingerprint")
		}
	default:
		return fmt.Errorf("unknown hash type %q", hashType)
	}
	return nil
}

// isLowerHex reports whether every character is a lowercase hex digit. Case
// matters because the registry is compared with string equality, so an
// upper-case duplicate would be a second row that never matches.
func isLowerHex(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
