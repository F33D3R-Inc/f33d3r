package api

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"f33d3r.com/ios/devserver/internal/store"
)

// This file is feed-engine/internal/handler/work_event.go's admission sequence,
// carried over as literally as the dialect allows. The canonical struct, the
// hashing, the signature check, the kind vocabulary and every rejection string
// are the real server's, because the Swift client was tested against exactly
// these bytes and these words.

// workCanonicalPayload is marshalled to canonical JSON for CID verification.
// Field declaration order = JSON output order. Keys alphabetical by JSON name.
type workCanonicalPayload struct {
	AuthorPIAL        string      `json:"author_pial"`
	Body              string      `json:"body"`
	CommentGating     string      `json:"comment_gating"`
	IsRepost          bool        `json:"is_repost"`
	Kind              string      `json:"kind"`
	MediaURLs         []string    `json:"media_urls"`
	ParentCID         interface{} `json:"parent_cid"`
	PollEndsAt        interface{} `json:"poll_ends_at"`
	PollOptions       interface{} `json:"poll_options"`
	RepostSourceID    interface{} `json:"repost_source_id"`
	ScheduledAt       interface{} `json:"scheduled_at"`
	SubscriberOnly    bool        `json:"subscriber_only"`
	Tags              []string    `json:"tags"`
	TimestampMS       int64       `json:"timestamp_ms"`
	VideoDurationSecs interface{} `json:"video_duration_secs"`
	VideoMasterURL    interface{} `json:"video_master_url"`
	VideoPosterURL    interface{} `json:"video_poster_url"`
	VoiceDurationSecs interface{} `json:"voice_duration_secs"`
	VoiceURL          interface{} `json:"voice_url"`
}

// workKinds is the closed vocabulary. Mirrored by the CHECK constraint on
// works.kind in schema.sql.
var workKinds = map[string]bool{
	"post": true, "reply": true, "quote": true, "poll": true,
	"video": true, "voice": true, "thread_post": true, "react_video": true,
}

var hashtagRe = regexp.MustCompile(`(?m)#([A-Za-z0-9_]{1,100})`)

// canonicalBytes is the exact byte string the CID hashes.
func canonicalBytes(p workCanonicalPayload) ([]byte, error) {
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
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// verifyCID recomputes sha256 of the canonical payload and compares.
func verifyCID(claimed string, p workCanonicalPayload) bool {
	canonical, err := canonicalBytes(p)
	if err != nil {
		return false
	}
	sum := sha256.Sum256(canonical)
	return claimed == fmt.Sprintf("sha256:%x", sum)
}

// verifySignature checks an ECDSA-P256 P1363 (r||s) signature, base64url
// unpadded, over the UTF-8 bytes of the CID, against an SPKI base64 key.
// Steps 2–6 of verifyMalkuthSig; step 1 (fetching the key) is the caller's.
func verifySignature(pubKeyB64, cid, sigBase64URL string) bool {
	if pubKeyB64 == "" {
		return false
	}
	spki, err := base64.StdEncoding.DecodeString(pubKeyB64)
	if err != nil {
		spki, err = base64.RawStdEncoding.DecodeString(pubKeyB64)
		if err != nil {
			return false
		}
	}
	pub, err := x509.ParsePKIXPublicKey(spki)
	if err != nil {
		return false
	}
	ecPub, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigBase64URL)
	if err != nil || len(sig) != 64 {
		return false
	}
	digest := sha256.Sum256([]byte(cid))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	return ecdsa.Verify(ecPub, digest[:], r, s)
}

// deriveTags is the union of client tags and #hashtags in the body,
// deduplicated case-insensitively with the first spelling kept.
func deriveTags(clientTags []string, body string) []string {
	seen := map[string]bool{}
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

// workAccepted is the 201 body: the row id and the CID it was stored under.
type workAccepted struct {
	WorkID string `json:"work_id"`
	CID    string `json:"cid"`
}

// workEvent admits a signed work.
func (s *Server) workEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	if body.raw == nil {
		plainError(w, http.StatusBadRequest, "signed works are JSON")
		return
	}
	var (
		cid, signature, eventType, videoWatermarkedURL, quotedWorkID, reactLayout, tusUploadID string
		isNSFW                                                                                 bool
		videoWidth, videoHeight                                                                int
		payload                                                                                workCanonicalPayload
	)
	for key, val := range body.raw {
		switch key {
		case "event_type":
			json.Unmarshal(val, &eventType)
		case "cid":
			json.Unmarshal(val, &cid)
		case "signature":
			json.Unmarshal(val, &signature)
		case "payload":
			if err := json.Unmarshal(val, &payload); err != nil {
				plainError(w, http.StatusBadRequest, "malformed payload")
				return
			}
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
	_ = tusUploadID

	if !strings.HasPrefix(cid, "sha256:") {
		plainError(w, http.StatusBadRequest, "invalid cid format")
		return
	}
	if signature == "" {
		plainError(w, http.StatusBadRequest, "signature required")
		return
	}
	if !verifyCID(cid, payload) {
		plainError(w, http.StatusBadRequest, "cid mismatch")
		return
	}
	ctx := r.Context()
	if !verifySignature(s.store.SigningKey(ctx, u.PIALID), cid, signature) {
		log.Printf("[work] sig verification failed for session PIAL, cid %s (key registered: %v)", cid, s.store.SigningKey(ctx, u.PIALID) != "")
		plainError(w, http.StatusForbidden, "signature invalid")
		return
	}
	// The signed author must be the session's identity. feed-engine trusts the
	// session here and stores author_pial from it; refusing a mismatch is
	// stricter, and the only way a client can hit it is by signing as someone
	// it is not.
	if payload.AuthorPIAL != u.PIALID {
		plainError(w, http.StatusForbidden, "author_pial does not match the session")
		return
	}

	kind := strings.TrimSpace(payload.Kind)
	if kind == "" {
		kind = "post"
	}
	if !workKinds[kind] {
		plainError(w, http.StatusBadRequest, "unknown work kind")
		return
	}
	if payload.IsRepost {
		src, _ := payload.RepostSourceID.(string)
		if strings.TrimSpace(src) == "" {
			plainError(w, http.StatusBadRequest, "repost requires repost_source_id")
			return
		}
	}
	parentCID, _ := payload.ParentCID.(string)
	if kind == "reply" && strings.TrimSpace(parentCID) == "" {
		plainError(w, http.StatusBadRequest, "reply requires parent_cid")
		return
	}
	quotedCID := ""
	if quotedWorkID != "" {
		if q, err := s.store.GetWorkByID(ctx, quotedWorkID, ""); err == nil {
			quotedCID = q.CID
		}
	}

	// Reply gating is the parent author's rule, enforced where the reply lands.
	if kind == "reply" {
		parent, err := s.store.GetWorkByCID(ctx, parentCID, u.ID)
		if err != nil {
			plainError(w, http.StatusNotFound, "parent work not found")
			return
		}
		if !s.mayReply(r, parent, u) {
			plainError(w, http.StatusForbidden, "replies are restricted on this work")
			return
		}
	}

	repostSourceID, _ := payload.RepostSourceID.(string)
	voiceURL, _ := payload.VoiceURL.(string)
	videoMasterURL, _ := payload.VideoMasterURL.(string)
	videoPosterURL, _ := payload.VideoPosterURL.(string)

	var pollEndsAt, scheduledAt *time.Time
	if str, ok := payload.PollEndsAt.(string); ok && str != "" {
		if t, err := time.Parse(time.RFC3339, str); err == nil {
			pollEndsAt = &t
		}
	}
	if str, ok := payload.ScheduledAt.(string); ok && str != "" {
		if t, err := time.Parse(time.RFC3339, str); err == nil {
			scheduledAt = &t
		}
	}
	var voiceDurationSecs, videoDurationSecs *int
	if v, ok := payload.VoiceDurationSecs.(float64); ok {
		iv := int(v)
		voiceDurationSecs = &iv
	}
	if v, ok := payload.VideoDurationSecs.(float64); ok {
		iv := int(v)
		videoDurationSecs = &iv
	}
	var pollOptions []string
	if raw, ok := payload.PollOptions.([]interface{}); ok {
		pollOptions = []string{}
		for _, item := range raw {
			if str, ok := item.(string); ok {
				pollOptions = append(pollOptions, str)
			}
		}
	}
	commentGating := payload.CommentGating
	if commentGating == "" {
		commentGating = "everyone"
	}

	workID, err := s.store.InsertWork(ctx, store.InsertWorkParams{
		AuthorID: u.ID, AuthorPIAL: u.PIALID, CID: cid, Body: payload.Body, Kind: kind,
		MediaURLs: payload.MediaURLs, ParentCID: parentCID, QuotedCID: quotedCID,
		Tags:        deriveTags(payload.Tags, payload.Body),
		PollOptions: pollOptions, PollEndsAt: pollEndsAt, ScheduledAt: scheduledAt,
		CommentGating: commentGating, SubscriberOnly: payload.SubscriberOnly, IsNSFW: isNSFW,
		IsRepost: payload.IsRepost, RepostSourceID: repostSourceID,
		VoiceURL: voiceURL, VoiceDurationSecs: voiceDurationSecs,
		VideoMasterURL: videoMasterURL, VideoWatermarkedURL: videoWatermarkedURL, VideoPosterURL: videoPosterURL,
		VideoDurationSecs: videoDurationSecs, VideoWidth: videoWidth, VideoHeight: videoHeight,
		ReactLayout: reactLayout,
	})
	if err != nil {
		if errors.Is(err, store.ErrDuplicateCID) {
			// The same bytes were already stored. feed-engine answers 500 here;
			// naming the row instead lets a retry after a timeout resolve.
			if existing, err := s.store.GetWorkByCID(ctx, cid, ""); err == nil {
				writeJSON(w, http.StatusOK, workAccepted{WorkID: existing.ID, CID: cid})
				return
			}
		}
		log.Printf("[work] InsertWork: %v", err)
		plainError(w, http.StatusInternalServerError, "server error")
		return
	}

	s.notifyForWork(r, u, workID, kind, parentCID, quotedCID, payload.Body)
	if kind != "reply" {
		if followers, err := s.store.FollowerIDs(ctx, u.ID); err == nil {
			for _, fid := range followers {
				s.hub.publish(userTopic(fid), "new_post", map[string]string{"work_id": workID, "author_handle": u.Handle})
			}
		}
	}
	writeJSON(w, http.StatusCreated, workAccepted{WorkID: workID, CID: cid})
}

// mayReply is the server side of Work.viewerMayReply: the parent author's
// rule, enforced where the reply lands.
func (s *Server) mayReply(r *http.Request, parent *store.Work, u *store.User) bool {
	if parent.AuthorID == u.ID {
		return true
	}
	switch parent.CommentGating {
	case "none":
		return false
	case "verified":
		return u.IsVerified
	case "followers", "circle":
		// The author's circle: people the author follows. This is the rule
		// feed-engine's detail prompt states ("Only people the author follows
		// can reply").
		ok, err := s.store.IsFollowing(r.Context(), parent.AuthorID, u.ID)
		return err == nil && ok
	}
	return true
}

// notifyForWork raises the notifications a new work owes: the parent's author
// for a reply, the quoted author for a quote, and every @mention in the body.
func (s *Server) notifyForWork(r *http.Request, u *store.User, workID, kind, parentCID, quotedCID, body string) {
	ctx := r.Context()
	preview := strings.TrimSpace(body)
	if len(preview) > 120 {
		preview = preview[:120]
	}
	notified := map[string]bool{u.ID: true}
	if kind == "reply" && parentCID != "" {
		if parent, err := s.store.GetWorkByCID(ctx, parentCID, ""); err == nil && !notified[parent.AuthorID] {
			notified[parent.AuthorID] = true
			s.store.Notify(ctx, parent.AuthorID, "reply", u.ID, workID, "work", map[string]any{"preview": preview})
		}
	}
	if quotedCID != "" {
		if quoted, err := s.store.GetWorkByCID(ctx, quotedCID, ""); err == nil && !notified[quoted.AuthorID] {
			notified[quoted.AuthorID] = true
			s.store.Notify(ctx, quoted.AuthorID, "quote", u.ID, workID, "work", map[string]any{"preview": preview})
		}
	}
	for _, m := range mentionRe.FindAllStringSubmatch(body, -1) {
		target, err := s.store.GetUserByHandle(ctx, m[1])
		if err != nil || notified[target.ID] {
			continue
		}
		notified[target.ID] = true
		s.store.Notify(ctx, target.ID, "mention", u.ID, workID, "work", map[string]any{"preview": preview})
	}
	for id := range notified {
		if id != u.ID {
			s.signalNotify(r, id)
		}
	}
}

var mentionRe = regexp.MustCompile(`(?m)(?:^|[^\w@])@([A-Za-z0-9_\-]{1,30})`)
