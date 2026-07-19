package handler

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// workCanonicalPayload is marshalled to canonical JSON for CID verification.
// Field declaration order = JSON output order (Go marshals struct fields in declaration order).
// This order MUST match the JS object literal key order in malkuth.js computeCID exactly.
// Keys are in alphabetical order by JSON name so both Go and JS produce identical output.
type workCanonicalPayload struct {
	AuthorPIAL        string      `json:"author_pial"`
	Body              string      `json:"body"`
	CommentGating     string      `json:"comment_gating"`  // "everyone"|"followers"|"circle"|"none"
	IsRepost          bool        `json:"is_repost"`
	Kind              string      `json:"kind"`
	MediaURLs         []string    `json:"media_urls"`
	ParentCID         interface{} `json:"parent_cid"`          // null or string
	PollEndsAt        interface{} `json:"poll_ends_at"`        // null or RFC3339 string
	PollOptions       interface{} `json:"poll_options"`        // null or []string
	RepostSourceID    interface{} `json:"repost_source_id"`    // null or UUID string
	ScheduledAt       interface{} `json:"scheduled_at"`        // null or RFC3339 string
	SubscriberOnly    bool        `json:"subscriber_only"`
	Tags              []string    `json:"tags"`                // may be empty []
	TimestampMS       int64       `json:"timestamp_ms"`
	VideoDurationSecs interface{} `json:"video_duration_secs"` // null or number
	VideoMasterURL    interface{} `json:"video_master_url"`    // null or string
	VideoPosterURL    interface{} `json:"video_poster_url"`    // null or string
	VoiceDurationSecs interface{} `json:"voice_duration_secs"` // null or number
	VoiceURL          interface{} `json:"voice_url"`           // null or string
}

// workEnvelope is the full signed envelope from the browser.
type workEnvelope struct {
	EventType string               `json:"event_type"`
	CID       string               `json:"cid"`       // "sha256:{hex}"
	Signature string               `json:"signature"` // base64url ECDSA-P256 sig over CID bytes
	Payload   workCanonicalPayload `json:"payload"`
}

// verifyCID recomputes sha256 of the canonical JSON payload and compares to claimed CID.
// Nil slices are normalised to empty slices so they serialise as [] not null,
// matching the JS behaviour of (arr || []).
// media_urls is sorted to match computeCID()'s .slice().sort() in malkuth.js.
// SetEscapeHTML(false) is required so & < > in body text produce identical bytes
// to JavaScript's JSON.stringify, which never escapes those characters.
func verifyCID(claimed string, p workCanonicalPayload) bool {
	if p.MediaURLs == nil {
		p.MediaURLs = []string{}
	}
	sort.Strings(p.MediaURLs)
	if p.Tags == nil {
		p.Tags = []string{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p); err != nil {
		return false
	}
	// Encode appends a trailing newline — strip it before hashing.
	canonical := bytes.TrimRight(buf.Bytes(), "\n")
	sum := sha256.Sum256(canonical)
	expected := fmt.Sprintf("sha256:%x", sum)
	return claimed == expected
}

// deriveTags returns the union of client-supplied tags and #hashtags extracted from the
// body, deduplicated case-insensitively (original case preserved for display / /tag links).
// The server owns the tag index so hashtags typed in a work are always searchable.
func deriveTags(clientTags []string, body string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(clientTags))
	add := func(t string) {
		t = strings.TrimSpace(t)
		if t == "" {
			return
		}
		k := strings.ToLower(t)
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, t)
	}
	for _, t := range clientTags {
		add(t)
	}
	for _, m := range hashtagRe.FindAllStringSubmatch(body, -1) {
		add(m[1])
	}
	return out
}

// fetchSigningPubkey returns a PIAL's ECDSA-P256 (SPKI base64) signing public key from the
// Elohim-veni key service — the durable authority for PIAL identity keys. Returns "" on any
// failure. This is the single source for work-signature verification across the app;
// never read a local mirror that a migration can wipe.
func (h *Handler) fetchSigningPubkey(pialID string) string {
	if h.cfg.ElohimVeniURL == "" {
		return ""
	}
	req, err := http.NewRequest("GET", h.cfg.ElohimVeniURL+"/v1/pial/"+pialID+"/signing-pubkey", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("X-Internal-Key", h.cfg.InternalAPIKey)
	resp, err := h.httpClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return ""
	}
	defer resp.Body.Close()
	var result struct {
		PublicKeyB64 string `json:"public_key_b64"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ""
	}
	return result.PublicKeyB64
}

// verifyMalkuthSig verifies an ECDSA-P256 signature from WebCrypto (IEEE P1363 format: r||s, 64 bytes).
// Fetches the signing public key from the Elohim-veni key service (key custody only).
// The signature covers the UTF-8 bytes of the CID string.
func (h *Handler) verifyMalkuthSig(pialID, cid, sigBase64URL string) bool {
	// 1. Fetch SPKI-encoded public key from Elohim-veni (the durable key authority).
	pubKeyB64 := h.fetchSigningPubkey(pialID)
	if pubKeyB64 == "" {
		return false
	}

	// 2. Decode SPKI bytes
	spkiBytes, err := base64.StdEncoding.DecodeString(pubKeyB64)
	if err != nil {
		// Try standard and raw variants
		spkiBytes, err = base64.RawStdEncoding.DecodeString(pubKeyB64)
		if err != nil {
			return false
		}
	}

	// 3. Parse ECDSA public key
	pub, err := x509.ParsePKIXPublicKey(spkiBytes)
	if err != nil {
		return false
	}
	ecPub, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return false
	}

	// 4. Decode signature (base64url, no padding)
	sigBytes, err := base64.RawURLEncoding.DecodeString(sigBase64URL)
	if err != nil {
		return false
	}
	if len(sigBytes) != 64 {
		return false
	}

	// 5. Compute SHA-256 of CID string (WebCrypto signs the CID bytes via ECDSA+SHA-256,
	//    which means the digest is computed internally. Go's ecdsa.Verify expects the
	//    pre-hashed digest.)
	digest := sha256.Sum256([]byte(cid))

	// 6. Split P1363 sig into r and s (each 32 bytes)
	r := new(big.Int).SetBytes(sigBytes[:32])
	s := new(big.Int).SetBytes(sigBytes[32:])

	return ecdsa.Verify(ecPub, digest[:], r, s)
}

// workEvent handles Malkuth-signed works from POST /events when the `cid` field is present.
func (h *Handler) workEvent(w http.ResponseWriter, r *http.Request, rawBody map[string]json.RawMessage) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if h.db != nil && !dbpkg.HasCapability(h.db, user.PIALID, model.CapPosting) {
		htmxError(w, r, "Posting is restricted on this account", http.StatusForbidden)
		return
	}

	// Parse the full envelope
	var env workEnvelope
	var isNSFW bool
	var videoWatermarkedURL string
	var quotedWorkID string
	var videoWidth, videoHeight int
	var tusUploadID string
	var reactLayout string
	for key, val := range rawBody {
		switch key {
		case "event_type":
			json.Unmarshal(val, &env.EventType)
		case "cid":
			json.Unmarshal(val, &env.CID)
		case "signature":
			json.Unmarshal(val, &env.Signature)
		case "payload":
			json.Unmarshal(val, &env.Payload)
			// tus_upload_id is in payload but intentionally excluded from the CID-verified
			// canonical struct so it never breaks signature verification.
			var extra struct {
				TusUploadID string `json:"tus_upload_id"`
			}
			if json.Unmarshal(val, &extra) == nil {
				tusUploadID = extra.TusUploadID
			}
		case "is_nsfw":
			json.Unmarshal(val, &isNSFW)
		case "video_watermarked_url":
			json.Unmarshal(val, &videoWatermarkedURL)
		case "video_width":
			json.Unmarshal(val, &videoWidth)
		case "video_height":
			json.Unmarshal(val, &videoHeight)
		case "quoted_work_id":
			json.Unmarshal(val, &quotedWorkID)
		case "react_layout":
			json.Unmarshal(val, &reactLayout)
		}
	}

	if !strings.HasPrefix(env.CID, "sha256:") {
		http.Error(w, "invalid cid format", http.StatusBadRequest)
		return
	}
	if env.Signature == "" {
		http.Error(w, "signature required", http.StatusBadRequest)
		return
	}

	// Verify CID — server recomputes sha256 of canonical payload.
	if !verifyCID(env.CID, env.Payload) {
		http.Error(w, "cid mismatch", http.StatusBadRequest)
		return
	}

	// Verify Malkuth signature — requires registered key in Elohim-veni.
	if !h.verifyMalkuthSig(user.PIALID, env.CID, env.Signature) {
		log.Printf("[work] sig verification failed for PIAL %s cid %s", user.PIALID, env.CID)
		http.Error(w, "signature invalid", http.StatusForbidden)
		return
	}

	if h.db == nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}

	// Determine kind: use payload.Kind, fall back to event_type.
	kind := env.Payload.Kind
	if kind == "" {
		kind = env.EventType
	}
	if kind == "" {
		kind = "post"
	}

	// Validate repost: must carry a non-empty repost_source_id.
	if env.Payload.IsRepost {
		srcID, _ := env.Payload.RepostSourceID.(string)
		if strings.TrimSpace(srcID) == "" {
			http.Error(w, "repost requires repost_source_id", http.StatusBadRequest)
			return
		}
	}

	// Resolve parentCID / quotedCID strings.
	parentCID := ""
	quotedCID := ""
	if s, ok := env.Payload.ParentCID.(string); ok {
		parentCID = s
	}
	// Resolve quotedCID from the outer quoted_work_id (UUID → CID lookup).
	if quotedCID == "" && quotedWorkID != "" {
		var qCID string
		if err := h.db.QueryRow(
			`SELECT cid FROM works WHERE id = $1::uuid AND deleted_at IS NULL`,
			quotedWorkID,
		).Scan(&qCID); err == nil {
			quotedCID = qCID
		}
	}

	// Validate reply: must carry a non-empty parent_cid.
	if kind == "reply" && strings.TrimSpace(parentCID) == "" {
		http.Error(w, "reply requires parent_cid", http.StatusBadRequest)
		return
	}

	// Resolve optional string fields from interface{}.
	repostSourceID, _ := env.Payload.RepostSourceID.(string)
	voiceURL, _       := env.Payload.VoiceURL.(string)
	videoMasterURL, _ := env.Payload.VideoMasterURL.(string)
	videoPosterURL, _ := env.Payload.VideoPosterURL.(string)

	// Resolve optional time fields.
	var pollEndsAt  *time.Time
	var scheduledAt *time.Time
	if s, ok := env.Payload.PollEndsAt.(string); ok && s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			pollEndsAt = &t
		}
	}
	if s, ok := env.Payload.ScheduledAt.(string); ok && s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			scheduledAt = &t
		}
	}

	// Resolve optional numeric fields from interface{} (JSON numbers unmarshal as float64).
	var voiceDurationSecs  *int
	var videoDurationSecs  *int
	if v, ok := env.Payload.VoiceDurationSecs.(float64); ok {
		iv := int(v)
		voiceDurationSecs = &iv
	}
	if v, ok := env.Payload.VideoDurationSecs.(float64); ok {
		iv := int(v)
		videoDurationSecs = &iv
	}

	// Resolve poll options: interface{} may be nil or []interface{} after JSON unmarshal.
	var pollOptions []string
	if raw, ok := env.Payload.PollOptions.([]interface{}); ok {
		for _, item := range raw {
			if s, ok := item.(string); ok {
				pollOptions = append(pollOptions, s)
			}
		}
	}

	// comment_gating: default to "everyone" when empty.
	commentGating := env.Payload.CommentGating
	if commentGating == "" {
		commentGating = "everyone"
	}

	// Derive the searchable tag index server-side: union of any client-supplied tags
	// and #hashtags extracted from the body. The server owns this index, so hashtags
	// typed in a work become searchable (trending, /tag pages, compose autocomplete)
	// regardless of what the client sends.
	tags := deriveTags(env.Payload.Tags, env.Payload.Body)

	// Insert work.
	workID, err := dbpkg.InsertWork(
		h.db,
		user.ID, user.PIALID,
		env.CID,
		env.Payload.Body,
		kind,
		env.Payload.MediaURLs,
		parentCID, quotedCID,
		tags,
		pollOptions,
		pollEndsAt,
		scheduledAt,
		commentGating,
		env.Payload.SubscriberOnly,
		isNSFW,
		env.Payload.IsRepost,
		repostSourceID,
		voiceURL,
		voiceDurationSecs,
		videoMasterURL, videoWatermarkedURL, videoPosterURL,
		videoDurationSecs,
		videoWidth, videoHeight,
		reactLayout,
	)
	if err != nil {
		log.Printf("[work] InsertWork error: %v", err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}

	// Link video hash to this work so the orphan sweep won't delete it.
	if tusUploadID != "" && h.db != nil {
		globalTusStore.mu.RLock()
		upload, ok := globalTusStore.uploads[tusUploadID]
		rawSHA := ""
		if ok {
			rawSHA = upload.RawSHA256
		}
		globalTusStore.mu.RUnlock()
		if rawSHA != "" {
			h.db.Exec(`UPDATE video_raw_hashes SET post_id = $1 WHERE sha256 = $2`, workID, rawSHA)
		}
	}

	// Dual-write to Alexandria catalog (fire-and-forget).
	go func(workID, pialID, body, kind string, tags []string, nsfw bool) {
		mediaArea := "text"
		switch kind {
		case "vision":
			mediaArea = "video"
		case "audio":
			mediaArea = "audio"
		}
		h.alexandriaCreateTitle(alexandriaTitlePayload{
			AuthorPIALID: pialID,
			SectionSlug:  "feed",
			Body:         body,
			MediaArea:    mediaArea,
			Visibility:   "public",
			IsNSFW:       nsfw,
			Tags:         tags,
			LegacyWorkID: workID,
		})
	}(workID, user.PIALID, env.Payload.Body, kind, tags, isNSFW)

	// Link the raw video hash to this work so future uploads of the same file
	// are resolved back to the original work by CheckVideoDuplicateByWork.
	// Only runs when this work carries a video master URL.
	// Best-effort: uses uploader_pial + recent created_at window (2h).
	if videoMasterURL != "" {
		go dbpkg.AssociateVideoRawHashPost(h.db, user.PIALID, workID)
	}

	// Insert first edition.
	if err := dbpkg.InsertEdition(h.db, workID, env.CID, env.Payload.Body, 1); err != nil {
		log.Printf("[work] InsertEdition error: %v", err)
		// Non-fatal: work was inserted; edition is historical record.
	}

	// Mint content receipt — permanent PIAL-anchored provenance record.
	// Do not mint for duplicate content: if lineage_pial is set, the original
	// already has a receipt and minting again would anchor it to the re-uploader.
	go func(workID, pialID, body string) {
		if h.db == nil {
			return
		}
		// Do not mint a receipt for duplicate content — the original already has one.
		var lineagePIAL string
		h.db.QueryRow(`SELECT COALESCE(lineage_pial::text,'') FROM works WHERE id = $1::uuid`, workID).Scan(&lineagePIAL)
		if lineagePIAL != "" {
			return
		}
		sum := sha256.Sum256([]byte(body))
		bodyHash := fmt.Sprintf("%x", sum)
		kycTier := dbpkg.GetPIALKYCTier(h.db, pialID)
		if err := dbpkg.MintContentReceipt(h.db, pialID, workID, bodyHash, kycTier); err != nil {
			log.Printf("[work] MintContentReceipt error work=%s: %v", workID, err)
		}
	}(workID, user.PIALID, env.Payload.Body)

	// Only fire content event for immediately-published works.
	// Scheduled works stay unpublished until the scheduler processes them.
	// Pass workID as both postID and workID so Abraxas Shield queries the works table.
	if scheduledAt == nil || !scheduledAt.After(time.Now()) {
		go h.publishContentEvent(workID, workID, user.PIALID, env.Payload.Body, true, false)
		go h.triggerWorkLinkPreview(env.Payload.Body)
	}

	if kind == "vision" {
		go dbpkg.TryAwardVisionAchievements(h.db, user.ID)
	}

	// Troll achievement triggers on post creation
	go dbpkg.TryAwardTrollOnPost(h.db, user.ID, user.PIALID, workID, env.Payload.Body)
	go dbpkg.TryAwardTrollForumDemon(h.db, user.ID, user.PIALID)
	if kind == "reply" && env.Payload.ParentCID != "" {
		parentCIDCopy := env.Payload.ParentCID
		replierID := user.ID
		replyBody := env.Payload.Body
		replyWorkID := workID
		go func() {
			if h.db == nil {
				return
			}
			var parentAuthorID, parentAuthorPIAL, parentPostID string
			h.db.QueryRow(
				`SELECT w.author_id, w.author_pial_id, w.id FROM works w
				 WHERE w.cid = $1 LIMIT 1`, parentCIDCopy,
			).Scan(&parentAuthorID, &parentAuthorPIAL, &parentPostID)
			if parentAuthorID != "" && parentAuthorID != replierID {
				dbpkg.TryAwardTrollOnReplyReceived(h.db, parentAuthorID, parentAuthorPIAL, parentPostID, replyBody)
				dbpkg.TryAwardTrollCartmanMethod(h.db, parentAuthorID, parentAuthorPIAL, parentPostID)
			}
			_ = replyWorkID
		}()
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{
		"work_id": workID,
		"cid":     env.CID,
	})
}
