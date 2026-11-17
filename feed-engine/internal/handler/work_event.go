package handler

// work_event.go — creating a work.
//
// In Facet Architecture order:
//
//	triggering event  POST /events carrying a Malkuth-signed envelope (eventsPost
//	                  routes any body with a sha256: cid here)
//	handler           workEvent: parse every field, check the signed author is
//	                  the session's account, verify the signature against the
//	                  session's PIAL key, validate synchronously, then one call to
//	                  dbpkg.InsertWork — the transaction that is the
//	                  acknowledgement, holding the work, edition 1, its citations,
//	                  its receipt and every downstream obligation
//	Facet renderer    renderComposeStatus (work_create.go), one per outcome
//	Fragment          compose_status, facet:f33d3r:compose:<account>:status
//	stream mutation   PublishFacetToAccount to the posting account's connections; the
//	                  obligations are drained by StartWorkOutbox (work_outbox.go)
//
// Nothing runs after the commit except the answer.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/f33d3r/feed-engine/internal/caeor"
	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/worktext"
	"github.com/f33d3r/feed-engine/internal/zior"
)

// workCanonicalPayload is marshalled to canonical JSON for CID verification.
// Field declaration order = JSON output order (Go marshals struct fields in declaration order).
// This order MUST match the JS object literal key order in malkuth.js computeCID exactly.
// Keys are in alphabetical order by JSON name so both Go and JS produce identical output.
//
// is_repost and repost_source_id stay in the wire form because every client's
// content address is computed over them; the signed-repost lane itself is retired
// (migration 0035) and the server refuses any envelope that sets them.
//
// author_account is the author's assertion of which account (persona) the work
// is published as, and it is signed like every other field. The author's PIAL is
// deliberately not in the payload: a PIAL is a person's root identity and is
// never handed to a client, so no client can name it. It does not need to be
// named to be bound: the signature verifies only against the key registered to
// the session's PIAL, and the server records that PIAL as the work's author from
// the session, never from the payload.
type workCanonicalPayload struct {
	AuthorAccount     string      `json:"author_account"`
	Body              string      `json:"body"`
	CommentGating     string      `json:"comment_gating"` // dbpkg.CommentGatings
	IsRepost          bool        `json:"is_repost"`
	Kind              string      `json:"kind"`
	MediaURLs         []string    `json:"media_urls"`
	ParentCID         interface{} `json:"parent_cid"`       // null or string
	PollEndsAt        interface{} `json:"poll_ends_at"`     // null or RFC3339 string
	PollOptions       interface{} `json:"poll_options"`     // null or []string
	RepostSourceID    interface{} `json:"repost_source_id"` // always null
	ScheduledAt       interface{} `json:"scheduled_at"`     // null or RFC3339 string
	SubscriberOnly    bool        `json:"subscriber_only"`
	Tags              []string    `json:"tags"` // may be empty []
	TimestampMS       int64       `json:"timestamp_ms"`
	VideoDurationSecs interface{} `json:"video_duration_secs"` // null or number
	VideoMasterURL    interface{} `json:"video_master_url"`    // null or string
	VideoPosterURL    interface{} `json:"video_poster_url"`    // null or string
	VoiceDurationSecs interface{} `json:"voice_duration_secs"` // null or number
	VoiceURL          interface{} `json:"voice_url"`           // null or string
}

// workKinds is the closed, server-owned vocabulary of Work kinds. The kind is
// the author's label for what the work is made of; whether a work is a reply is
// not the label's to say — that is works.is_reply, derived from the signed
// parent citation.
//
// Mirrored by the works_kind_vocabulary CHECK constraint (latest: migration 0036) —
// change one and the other must change with it.
var workKinds = map[string]bool{
	"post":        true,
	"reply":       true,
	"quote":       true,
	"poll":        true,
	"video":       true,
	"voice":       true,
	"thread_post": true,
	"react_video": true,
	"track":       true,
}

// workBodyMaxRunes is the character limit the edit paths (editPost,
// applyWorkEdit) and the tag search still apply. Creation measures with
// worktext.WeightedLength against worktext.MaxWeightedLength; the edit paths
// must adopt the same measure when they are rebuilt.
const workBodyMaxRunes = 2000

// Bounds of a work, all checked before anything is written.
const (
	// A signed work must reach the server within this long of being signed,
	// and may not claim to be signed in the future beyond clock skew.
	workSignedMaxAge   = time.Hour
	workSignedMaxAhead = 5 * time.Minute

	workMaxScheduleAhead = 366 * 24 * time.Hour

	workPollMinOptions  = 2
	workPollMaxOptions  = 4
	workPollOptionRunes = 80
	workPollMinDuration = 5 * time.Minute
	workPollMaxDuration = 7 * 24 * time.Hour
	workMaxClientTags   = 10
	workMaxTags         = 30
	workTagRunes        = 64
	workMaxMedia        = 4
	workMaxDurationSecs = 4 * 60 * 60
	workMaxVideoSide    = 16384
)

// reactLayouts is the closed vocabulary of React With Video layouts.
var reactLayouts = map[string]bool{
	"presenter": true, "pip": true, "split": true, "card_over": true, "media_only": true, "green_screen": true,
}

var cidRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// gifProviderHost reports whether an external media URL is a GIF from the
// provider this server's GIF search hands out (KLIPY, tag.go). It is the one
// kind of media a work may carry that is not an upload.
func gifProviderHost(host string) bool {
	host = strings.ToLower(host)
	return host == "klipy.com" || strings.HasSuffix(host, ".klipy.com")
}

// verifyCID recomputes sha256 of the canonical JSON payload and compares to claimed CID.
// Nil slices are normalised to empty slices so they serialise as [] not null,
// matching the JS behaviour of (arr || []).
// media_urls is sorted to match computeCID()'s .slice().sort() in malkuth.js.
// SetEscapeHTML(false) is required so & < > in body text produce identical bytes
// to JavaScript's JSON.stringify, which never escapes those characters.
func verifyCID(claimed string, p workCanonicalPayload) (bool, error) {
	// A sorted copy, so the caller's slice is not reordered. It must be a
	// non-nil slice even when empty: clients hash "media_urls":[], and a nil
	// slice would encode as null and fail every media-less work's CID check.
	media := make([]string, len(p.MediaURLs))
	copy(media, p.MediaURLs)
	sort.Strings(media)
	p.MediaURLs = media
	if p.Tags == nil {
		p.Tags = []string{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p); err != nil {
		return false, fmt.Errorf("encoding canonical payload: %w", err)
	}
	// Encode appends a trailing newline — strip it before hashing.
	canonical := bytes.TrimRight(buf.Bytes(), "\n")
	sum := sha256.Sum256(canonical)
	return claimed == fmt.Sprintf("sha256:%x", sum), nil
}

// deriveTags returns the union of client-supplied tags and #hashtags extracted from the
// body, deduplicated case-insensitively (original case preserved for display / /tag links).
// The server owns the tag index so hashtags typed in a work are always searchable.
func deriveTags(clientTags []string, body string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(clientTags))
	add := func(t string) {
		k := strings.ToLower(t)
		if t == "" || seen[k] {
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

// errKeyAuthority is the key service failing to answer, which is not the same as
// a signature that does not verify.
var errKeyAuthority = errors.New("signing key authority unavailable")

// fetchSigningPubkey returns a PIAL's ECDSA-P256 (SPKI base64) signing public key from the
// Elohim-veni key service — the durable authority for PIAL identity keys. "" with a nil
// error means the PIAL has no key; an error means the authority could not be asked.
// This is the single source for work-signature verification across the app;
// never read a local mirror that a migration can wipe.
func (h *Handler) fetchSigningPubkey(ctx context.Context, pialID string) (string, error) {
	if h.cfg.ElohimVeniURL == "" {
		return "", fmt.Errorf("%w: ELOHIM_VENI_URL is not set", errKeyAuthority)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", h.cfg.ElohimVeniURL+"/v1/pial/"+url.PathEscape(pialID)+"/signing-pubkey", nil)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errKeyAuthority, err)
	}
	req.Header.Set("X-Internal-Key", h.cfg.InternalAPIKey)
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errKeyAuthority, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", nil
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("%w: HTTP %d", errKeyAuthority, resp.StatusCode)
	}
	var result struct {
		PublicKeyB64 string `json:"public_key_b64"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("%w: unreadable answer: %v", errKeyAuthority, err)
	}
	return result.PublicKeyB64, nil
}

// verifyMalkuthSig verifies an ECDSA-P256 signature from WebCrypto (IEEE P1363 format: r||s, 64 bytes)
// over the UTF-8 bytes of the CID string, against the PIAL's key at Elohim-veni.
// false with a nil error is a signature that does not verify; an error is the
// key authority failing to answer.
func (h *Handler) verifyMalkuthSig(ctx context.Context, pialID, cid, sigBase64URL string) (bool, error) {
	pubKeyB64, err := h.fetchSigningPubkey(ctx, pialID)
	if err != nil {
		return false, err
	}
	if pubKeyB64 == "" {
		return false, nil
	}
	spkiBytes, err := base64.StdEncoding.DecodeString(pubKeyB64)
	if err != nil {
		if spkiBytes, err = base64.RawStdEncoding.DecodeString(pubKeyB64); err != nil {
			return false, fmt.Errorf("%w: stored key for %s is not base64", errKeyAuthority, pialID)
		}
	}
	pub, err := x509.ParsePKIXPublicKey(spkiBytes)
	if err != nil {
		return false, fmt.Errorf("%w: stored key for %s does not parse: %v", errKeyAuthority, pialID, err)
	}
	ecPub, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return false, fmt.Errorf("%w: stored key for %s is not ECDSA", errKeyAuthority, pialID)
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(sigBase64URL)
	if err != nil || len(sigBytes) != 64 {
		return false, nil
	}
	// WebCrypto signs with ECDSA+SHA-256, so the digest is the SHA-256 of the
	// CID bytes; Go's ecdsa.Verify takes the pre-hashed digest.
	digest := sha256.Sum256([]byte(cid))
	r := new(big.Int).SetBytes(sigBytes[:32])
	s := new(big.Int).SetBytes(sigBytes[32:])
	return ecdsa.Verify(ecPub, digest[:], r, s), nil
}

// workEnvelope is the signed envelope and the unsigned fields that travel with it.
type workEnvelope struct {
	CID          string
	Signature    string
	Payload      workCanonicalPayload
	IsNSFW       bool
	VideoWidth   int
	VideoHeight  int
	QuotedWorkID string
	ReactLayout  string
	// DownloadsAllowed is the author's download switch for the work; nil (not
	// sent) means the default, allowed.
	DownloadsAllowed *bool
}

// parseWorkEnvelope reads every field the create path uses and reports the first
// that does not decode. Unknown keys are ignored. Two known keys are ignored on
// purpose: video_watermarked_url (the downloadable file is the registry's, never
// the client's) and payload.tus_upload_id (the registry binds the raw upload to
// the asset). event_type is not read: the signed kind names the work.
func parseWorkEnvelope(raw map[string]json.RawMessage) (*workEnvelope, *workCreateAnswer) {
	var env workEnvelope
	fields := []struct {
		key      string
		into     interface{}
		required bool
	}{
		{"cid", &env.CID, true},
		{"signature", &env.Signature, true},
		{"payload", &env.Payload, true},
		{"is_nsfw", &env.IsNSFW, false},
		{"video_width", &env.VideoWidth, false},
		{"video_height", &env.VideoHeight, false},
		{"quoted_work_id", &env.QuotedWorkID, false},
		{"react_layout", &env.ReactLayout, false},
		{"downloads_allowed", &env.DownloadsAllowed, false},
	}
	for _, f := range fields {
		val, ok := raw[f.key]
		if !ok || bytes.Equal(bytes.TrimSpace(val), []byte("null")) {
			if f.required {
				return nil, invalidField(f.key, "required")
			}
			continue
		}
		if err := json.Unmarshal(val, f.into); err != nil {
			return nil, invalidField(f.key, "malformed: "+err.Error())
		}
	}
	return &env, nil
}

// optString reads a null-or-string payload field.
func optString(v interface{}, field string) (string, *workCreateAnswer) {
	switch t := v.(type) {
	case nil:
		return "", nil
	case string:
		return t, nil
	}
	return "", invalidField(field, "must be a string or null")
}

// optTime reads a null-or-RFC3339 payload field. A malformed time is a refusal,
// never a missing one.
func optTime(v interface{}, field string) (*time.Time, *workCreateAnswer) {
	s, bad := optString(v, field)
	if bad != nil || s == "" {
		return nil, bad
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, invalidField(field, "must be an RFC 3339 time")
	}
	return &t, nil
}

// optSeconds reads a null-or-number duration in seconds, bounded.
func optSeconds(v interface{}, field string) (*int, *workCreateAnswer) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case float64:
		if math.IsNaN(t) || t <= 0 || t > workMaxDurationSecs {
			return nil, invalidField(field, fmt.Sprintf("must be between 0 and %d seconds", workMaxDurationSecs))
		}
		secs := int(math.Round(t))
		return &secs, nil
	}
	return nil, invalidField(field, "must be a number or null")
}

// workEvent handles Malkuth-signed works from POST /events when the `cid` field is present.
func (h *Handler) workEvent(w http.ResponseWriter, r *http.Request, rawBody map[string]json.RawMessage) {
	user := h.userFromRequest(w, r)
	if user == nil || user.ID == "" || user.ID == "demo_user" || user.PIALID == "" {
		// No poster, so no compose status Facet to render: the session is the
		// precondition of every state in the vocabulary.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	h.answerWorkCreate(w, user, h.createWork(r.Context(), user, rawBody))
}

// createWork decides one create. Every refusal is typed and synchronous.
func (h *Handler) createWork(ctx context.Context, user *model.User, rawBody map[string]json.RawMessage) *workCreateAnswer {
	env, bad := parseWorkEnvelope(rawBody)
	if bad != nil {
		return bad
	}
	p := env.Payload
	if !cidRe.MatchString(env.CID) {
		return invalidField("cid", "must be sha256: followed by 64 lowercase hex digits")
	}
	if env.Signature == "" {
		return invalidField("signature", "required")
	}
	if p.AuthorAccount != user.ID {
		return invalidField("author_account", "is not the signed-in account")
	}
	matches, err := verifyCID(env.CID, p)
	if err != nil {
		return &workCreateAnswer{State: WorkStateServerError, Detail: err.Error()}
	}
	if !matches {
		return invalidField("cid", "does not match the payload")
	}
	valid, err := h.verifyMalkuthSig(ctx, user.PIALID, env.CID, env.Signature)
	if err != nil {
		return &workCreateAnswer{State: WorkStateServerError, Field: "signature", Detail: err.Error()}
	}
	if !valid {
		return invalidField("signature", "does not verify against this identity's key")
	}

	// A re-delivered envelope is answered with the work it already created,
	// however long ago it was signed, before any rule that depends on the clock
	// or on the account's present standing: it creates nothing.
	if prior, found, err := dbpkg.LookupWorkCID(h.db, env.CID, user.ID); err != nil {
		return &workCreateAnswer{State: WorkStateServerError, Detail: err.Error()}
	} else if found {
		return h.answerFromOutcome(prior, env.CID, nil)
	}

	// The capability elohim-veni resolved for this session.
	if !h.mayPublish(user) {
		return refuse(WorkStatePostingRestricted)
	}

	draft, bad := h.validateWork(ctx, user, env)
	if bad != nil {
		return bad
	}
	if draft.IsNSFW {
		if refused := h.adultPublishGate(ctx, user); refused != nil {
			return refused
		}
	}
	outcome, err := dbpkg.InsertWork(h.db, *draft)
	if err != nil {
		return &workCreateAnswer{State: WorkStateServerError, Detail: err.Error()}
	}
	return h.answerFromOutcome(outcome, env.CID, draft.ScheduledAt)
}

// answerFromOutcome maps the transaction's decision onto the state vocabulary.
func (h *Handler) answerFromOutcome(o dbpkg.WorkCreateOutcome, cid string, scheduledAt *time.Time) *workCreateAnswer {
	switch o.Result {
	case dbpkg.WorkCreated:
		return &workCreateAnswer{State: WorkStateOk, WorkID: o.WorkID, CID: cid, ScheduledAt: scheduledAt}
	case dbpkg.WorkReplayed:
		return &workCreateAnswer{State: WorkStateOk, WorkID: o.WorkID, CID: cid, Replayed: true}
	case dbpkg.WorkDuplicate:
		return &workCreateAnswer{State: WorkStateDuplicate, WorkID: o.WorkID}
	case dbpkg.WorkCIDTaken:
		return invalidField("cid", "names a work this account cannot create; sign the work again")
	case dbpkg.WorkCitedNotFound:
		return &workCreateAnswer{State: WorkStateCitedNotFound, Field: o.Field}
	case dbpkg.WorkRepliesRestricted:
		return refuse(WorkStateRepliesRestricted)
	case dbpkg.WorkRateLimited:
		return refuse(WorkStateRateLimited)
	}
	return &workCreateAnswer{State: WorkStateServerError, Detail: fmt.Sprintf("unknown create result %d", o.Result)}
}

// validateWork turns a verified envelope into a draft, or the first reason it
// cannot be one.
func (h *Handler) validateWork(ctx context.Context, user *model.User, env *workEnvelope) (*dbpkg.WorkDraft, *workCreateAnswer) {
	p := env.Payload
	now := time.Now()

	signed := time.UnixMilli(p.TimestampMS)
	if p.TimestampMS <= 0 || now.Sub(signed) > workSignedMaxAge || signed.Sub(now) > workSignedMaxAhead {
		return nil, invalidField("timestamp_ms", "the work must be signed within the last hour")
	}

	kind := p.Kind
	if !workKinds[kind] {
		return nil, invalidField("kind", "is not a work kind")
	}

	body := p.Body
	if !worktext.IsNormalized(body) {
		return nil, invalidField("body", fmt.Sprintf("is not normalized (text normalizer v%d)", worktext.NormalizerVersion))
	}
	if utf8.RuneCountInString(body) > worktext.MaxRunes {
		return nil, &workCreateAnswer{State: WorkStateTextTooLong, Over: utf8.RuneCountInString(body) - worktext.MaxRunes}
	}
	if n := worktext.WeightedLength(body); n > worktext.MaxWeightedLength {
		return nil, &workCreateAnswer{State: WorkStateTextTooLong, Over: n - worktext.MaxWeightedLength}
	}

	if p.IsRepost || p.RepostSourceID != nil {
		return nil, invalidField("is_repost", "a repost is a reaction, not a signed work")
	}

	gating := p.CommentGating
	if gating == "" {
		gating = "everyone"
	}
	if !dbpkg.ValidCommentGating(gating) {
		return nil, invalidField("comment_gating", "must be one of "+strings.Join(dbpkg.CommentGatings, ", "))
	}

	parentCID, bad := optString(p.ParentCID, "parent_cid")
	if bad != nil {
		return nil, bad
	}
	if parentCID != "" && !cidRe.MatchString(parentCID) {
		return nil, invalidField("parent_cid", "is not a content address")
	}
	if parentCID == env.CID {
		return nil, invalidField("parent_cid", "a work cannot answer itself")
	}
	if kind == "reply" && parentCID == "" {
		return nil, invalidField("parent_cid", "a reply names the work it answers")
	}
	quoted := env.QuotedWorkID
	if quoted != "" {
		id, err := uuid.Parse(quoted)
		if err != nil {
			return nil, invalidField("quoted_work_id", "is not a work id")
		}
		quoted = id.String()
	}
	if kind == "quote" && quoted == "" {
		return nil, invalidField("quoted_work_id", "a quote names the work it quotes")
	}

	scheduledAt, bad := optTime(p.ScheduledAt, "scheduled_at")
	if bad != nil {
		return nil, bad
	}
	if scheduledAt != nil {
		if !scheduledAt.After(now) {
			return nil, invalidField("scheduled_at", "must be in the future")
		}
		if scheduledAt.Sub(now) > workMaxScheduleAhead {
			return nil, invalidField("scheduled_at", "must be within a year")
		}
	}

	pollOptions, pollEndsAt, bad := validatePoll(kind, p, scheduledAt, now)
	if bad != nil {
		return nil, bad
	}

	tags, bad := validateTags(p.Tags, body)
	if bad != nil {
		return nil, bad
	}

	media, bad := h.resolveWorkMedia(ctx, user, kind, p)
	if bad != nil {
		return nil, bad
	}

	if worktext.IsBlank(body) && !media.any() && len(pollOptions) == 0 {
		return nil, refuse(WorkStateTextBlank)
	}

	if env.VideoWidth < 0 || env.VideoWidth > workMaxVideoSide || env.VideoHeight < 0 || env.VideoHeight > workMaxVideoSide {
		return nil, invalidField("video_width", fmt.Sprintf("dimensions must be between 0 and %d", workMaxVideoSide))
	}
	if env.ReactLayout != "" && (kind != "react_video" || !reactLayouts[env.ReactLayout]) {
		return nil, invalidField("react_layout", "is a layout of a react_video work")
	}

	d := &dbpkg.WorkDraft{
		AuthorID:          user.ID,
		AuthorPIAL:        user.PIALID,
		CID:               env.CID,
		Body:              body,
		Kind:              kind,
		MediaURLs:         media.images,
		ParentCID:         parentCID,
		QuotedWorkID:      quoted,
		Tags:              tags,
		PollOptions:       pollOptions,
		PollEndsAt:        pollEndsAt,
		ScheduledAt:       scheduledAt,
		CommentGating:     gating,
		SubscriberOnly:    p.SubscriberOnly,
		IsNSFW:            env.IsNSFW,
		VoiceURL:          media.voice,
		VoiceDurationSecs: media.voiceSecs,
		VideoMasterURL:    media.videoMaster,
		VideoPosterURL:    media.videoPoster,
		VideoDurationSecs: media.videoSecs,
		VideoWidth:        env.VideoWidth,
		VideoHeight:       env.VideoHeight,
		ReactLayout:       env.ReactLayout,
		Assets:            media.assets,
		LineagePIAL:       media.creditPIAL,
		LineageHandle:     media.creditHandle,
		DownloadsAllowed:  env.DownloadsAllowed == nil || *env.DownloadsAllowed,
		HideAdult:         excludeNSFW(user),
	}
	d.FingerprintOf = func(parentWorkID, quotedWorkID string) (string, error) {
		return worktext.Fingerprint(worktext.FingerprintInput{
			Body:         body,
			Kind:         kind,
			ParentWorkID: parentWorkID,
			QuotedWorkID: quotedWorkID,
			MediaURLs:    media.images,
			VoiceURL:     media.voice,
			VideoURL:     media.videoMaster,
			PollOptions:  pollOptions,
		})
	}
	return d, nil
}

// validatePoll checks a poll's options and its end. Poll options and the poll
// kind come together or not at all.
func validatePoll(kind string, p workCanonicalPayload, scheduledAt *time.Time, now time.Time) ([]string, *time.Time, *workCreateAnswer) {
	if p.PollOptions == nil {
		if kind == "poll" {
			return nil, nil, invalidField("poll_options", "a poll needs options")
		}
		if p.PollEndsAt != nil {
			return nil, nil, invalidField("poll_ends_at", "only a poll ends")
		}
		return nil, nil, nil
	}
	if kind != "poll" {
		return nil, nil, invalidField("poll_options", "only a poll has options")
	}
	raw, ok := p.PollOptions.([]interface{})
	if !ok {
		return nil, nil, invalidField("poll_options", "must be a list of strings")
	}
	if len(raw) < workPollMinOptions || len(raw) > workPollMaxOptions {
		return nil, nil, invalidField("poll_options", fmt.Sprintf("a poll has %d to %d options", workPollMinOptions, workPollMaxOptions))
	}
	seen := map[string]bool{}
	options := make([]string, 0, len(raw))
	for _, item := range raw {
		o, ok := item.(string)
		if !ok {
			return nil, nil, invalidField("poll_options", "must be a list of strings")
		}
		if !worktext.IsNormalized(o) || !worktext.IsSingleLine(o) || worktext.IsBlank(o) {
			return nil, nil, invalidField("poll_options", "each option is one normalized, non-blank line")
		}
		if utf8.RuneCountInString(o) > workPollOptionRunes {
			return nil, nil, invalidField("poll_options", fmt.Sprintf("each option is at most %d characters", workPollOptionRunes))
		}
		k := strings.ToLower(o)
		if seen[k] {
			return nil, nil, invalidField("poll_options", "options must differ")
		}
		seen[k] = true
		options = append(options, o)
	}
	endsAt, bad := optTime(p.PollEndsAt, "poll_ends_at")
	if bad != nil {
		return nil, nil, bad
	}
	if endsAt == nil {
		return nil, nil, invalidField("poll_ends_at", "a poll needs an end")
	}
	opens := now
	if scheduledAt != nil {
		opens = *scheduledAt
	}
	if d := endsAt.Sub(opens); d < workPollMinDuration || d > workPollMaxDuration {
		return nil, nil, invalidField("poll_ends_at", "a poll runs from 5 minutes to 7 days")
	}
	return options, endsAt, nil
}

// validateTags bounds the client's tags and the derived tag index.
func validateTags(clientTags []string, body string) ([]string, *workCreateAnswer) {
	if len(clientTags) > workMaxClientTags {
		return nil, invalidField("tags", fmt.Sprintf("at most %d", workMaxClientTags))
	}
	for _, t := range clientTags {
		if t == "" || !worktext.IsNormalized(t) || utf8.RuneCountInString(t) > workTagRunes ||
			strings.IndexFunc(t, unicode.IsSpace) >= 0 {
			return nil, invalidField("tags", fmt.Sprintf("each tag is 1 to %d characters with no spaces", workTagRunes))
		}
	}
	tags := deriveTags(clientTags, body)
	if len(tags) > workMaxTags {
		return nil, invalidField("tags", fmt.Sprintf("a work carries at most %d tags, hashtags included", workMaxTags))
	}
	return tags, nil
}

// workMedia is a work's media as Caeor vouches for it.
type workMedia struct {
	images      []string
	voice       string
	voiceSecs   *int
	videoMaster string
	videoPoster string
	videoSecs   *int
	// assets are the work's media references, one per slot.
	assets []dbpkg.WorkAssetRef
	// creditPIAL and creditHandle name the uploader of media in the work that
	// the author did not upload ("Media by"); "" when every asset is the
	// author's own.
	creditPIAL   string
	creditHandle string
}

func (m workMedia) any() bool {
	return len(m.images) > 0 || m.voice != "" || m.videoMaster != ""
}

// workAsset resolves one media path a work names through Caeor. The path must
// be in canonical form (it is signed and stored exactly as sent) and must
// belong to a ready, non-withdrawn asset of one of kinds. Imported legacy media
// keeps its path, so the path itself is never judged by its shape — only by
// what Caeor says it is. The asset is described as well, so the caller can require the path to
// be the exact viewer-facing object of its slot.
func (h *Handler) workAsset(ctx context.Context, field, raw string, kinds ...caeor.Kind) (*caeor.Asset, *workCreateAnswer) {
	canonical := canonicalMediaPath(raw)
	if canonical == "" || canonical != raw || !strings.HasPrefix(canonical, "/") {
		return nil, invalidField(field, "is not a media object this server issued")
	}
	obj, err := h.caeor.Resolve(ctx, canonical)
	if errors.Is(err, caeor.ErrNotFound) {
		return nil, invalidField(field, "is not an upload this server knows")
	}
	if err != nil {
		return nil, &workCreateAnswer{State: WorkStateServerError, Field: field, Detail: err.Error()}
	}
	asset, err := h.caeor.Get(ctx, obj.AssetID)
	if errors.Is(err, caeor.ErrNotFound) {
		return nil, invalidField(field, "is not an upload this server knows")
	}
	if err != nil {
		return nil, &workCreateAnswer{State: WorkStateServerError, Field: field, Detail: err.Error()}
	}
	if asset.Withdrawn || obj.Withdrawn {
		return nil, invalidField(field, "has been removed from the platform")
	}
	if asset.Held() {
		return nil, invalidField(field, "is being reviewed; post it once the review is done")
	}
	if asset.Status != caeor.StatusReady {
		return nil, invalidField(field, "is still being processed; post it when it is ready")
	}
	if asset.UploaderPIAL == "" {
		asset.UploaderPIAL = obj.UploaderPIAL
	}
	if asset.Kind == "" {
		asset.Kind = obj.Kind
	}
	for _, k := range kinds {
		if asset.Kind == k {
			return asset, nil
		}
	}
	return nil, invalidField(field, "names media of the wrong kind")
}

// credit records whom an asset's bytes are credited to as the work's "Media
// by" credit when it is not the author: the original owner the integrity
// authority named (Asset.CreditTo), else the uploader. The first such owner is
// the credit.
func (h *Handler) credit(m *workMedia, author *model.User, asset *caeor.Asset) *workCreateAnswer {
	owner := asset.CreditPIAL()
	if owner == author.PIALID || m.creditPIAL != "" {
		return nil
	}
	handle, _, err := dbpkg.UploaderCredit(h.db, owner)
	if err != nil {
		return &workCreateAnswer{State: WorkStateServerError, Detail: err.Error()}
	}
	m.creditPIAL, m.creditHandle = owner, handle
	return nil
}

// resolveWorkMedia accepts media only as Caeor objects this server issued:
// ready, not withdrawn, the exact watermarked object of their slot. An asset
// somebody else uploaded may stand as a credited reference — the media gate
// governs an asset by its uploader's works only, so it unlocks nothing — and
// the work records the original uploader for its "Media by" credit.
func (h *Handler) resolveWorkMedia(ctx context.Context, user *model.User, kind string, p workCanonicalPayload) (workMedia, *workCreateAnswer) {
	// works.media_urls is NOT NULL: a work with no images stores an empty list.
	m := workMedia{images: []string{}}
	if len(p.MediaURLs) > workMaxMedia {
		return m, invalidField("media_urls", fmt.Sprintf("at most %d", workMaxMedia))
	}
	if kind == "track" && len(p.MediaURLs) > 1 {
		return m, invalidField("media_urls", "a track carries at most one cover image")
	}
	seen := map[string]bool{}
	for i, raw := range p.MediaURLs {
		if seen[raw] {
			return m, invalidField("media_urls", "lists the same media twice")
		}
		seen[raw] = true
		if strings.HasPrefix(raw, "https://") || strings.HasPrefix(raw, "http://") {
			u, err := url.Parse(raw)
			if err != nil || u.Scheme != "https" || !gifProviderHost(u.Hostname()) {
				return m, invalidField("media_urls", "external media must be a GIF from the GIF picker")
			}
			m.images = append(m.images, raw)
			continue
		}
		asset, bad := h.workAsset(ctx, "media_urls", raw, caeor.KindImage)
		if bad != nil {
			return m, bad
		}
		if raw != asset.ViewerURL() {
			return m, invalidField("media_urls", "must name the image's watermarked rendition")
		}
		if bad := h.credit(&m, user, asset); bad != nil {
			return m, bad
		}
		m.images = append(m.images, raw)
		m.assets = append(m.assets, dbpkg.WorkAssetRef{Slot: dbpkg.SlotImage, Position: i, AssetID: asset.AssetID, UploaderPIAL: asset.UploaderPIAL})
	}

	voice, bad := optString(p.VoiceURL, "voice_url")
	if bad != nil {
		return m, bad
	}
	if voice != "" {
		asset, bad := h.workAsset(ctx, "voice_url", voice, caeor.KindVoice, caeor.KindAudio)
		if bad != nil {
			return m, bad
		}
		if voice != asset.ViewerURL() {
			return m, invalidField("voice_url", "must name the recording's audio object")
		}
		if m.voiceSecs, bad = optSeconds(p.VoiceDurationSecs, "voice_duration_secs"); bad != nil {
			return m, bad
		}
		if bad := h.credit(&m, user, asset); bad != nil {
			return m, bad
		}
		m.voice = voice
		m.assets = append(m.assets, dbpkg.WorkAssetRef{Slot: dbpkg.SlotVoice, AssetID: asset.AssetID, UploaderPIAL: asset.UploaderPIAL})
	} else if p.VoiceDurationSecs != nil {
		return m, invalidField("voice_duration_secs", "only a voice recording has a voice duration")
	}
	if (kind == "voice" || kind == "track") && m.voice == "" {
		return m, invalidField("voice_url", "a "+kind+" work carries audio")
	}

	master, bad := optString(p.VideoMasterURL, "video_master_url")
	if bad != nil {
		return m, bad
	}
	poster, bad := optString(p.VideoPosterURL, "video_poster_url")
	if bad != nil {
		return m, bad
	}
	if master == "" {
		if poster != "" || p.VideoDurationSecs != nil {
			return m, invalidField("video_poster_url", "only a video has a poster or a duration")
		}
		if kind == "video" || kind == "react_video" {
			return m, invalidField("video_master_url", "a video work carries a video")
		}
		return m, nil
	}
	if kind == "track" {
		return m, invalidField("video_master_url", "a track carries audio, not video")
	}
	asset, bad := h.workAsset(ctx, "video_master_url", master, caeor.KindVideo, caeor.KindClip)
	if bad != nil {
		return m, bad
	}
	if master != asset.URLs.HLSMaster {
		return m, invalidField("video_master_url", "must name the video's watermarked HLS master")
	}
	if poster != "" && poster != asset.URLs.Poster {
		return m, invalidField("video_poster_url", "must be the poster of the same video")
	}
	if m.videoSecs, bad = optSeconds(p.VideoDurationSecs, "video_duration_secs"); bad != nil {
		return m, bad
	}
	if bad := h.credit(&m, user, asset); bad != nil {
		return m, bad
	}
	m.videoMaster = master
	m.videoPoster = poster
	m.assets = append(m.assets, dbpkg.WorkAssetRef{Slot: dbpkg.SlotVideo, AssetID: asset.AssetID, UploaderPIAL: asset.UploaderPIAL})
	if poster != "" {
		m.assets = append(m.assets, dbpkg.WorkAssetRef{Slot: dbpkg.SlotPoster, AssetID: asset.AssetID, UploaderPIAL: asset.UploaderPIAL})
	}
	return m, nil
}

// ziorAnalyzeVoiceWork is the zior_voice obligation: it sends an audio work
// (a voice work or a track) to Zior and stores the whole answer on the work —
// the psych_vector, which replaces the text mapping because the audio is the
// work, and the mood, descriptor, listening contexts, tempo and key (migration
// 0043). Zior keeps nothing, so this stored copy is the only one. A brain with
// Zior switched off owes nothing; a work deleted meanwhile is owed nothing.
func (h *Handler) ziorAnalyzeVoiceWork(ctx context.Context, workID, creatorID, voiceURL string) error {
	client := h.zior()
	if !client.Enabled() {
		return nil
	}
	// The audio object Caeor serves lives on the shared media volume; Zior hears
	// the same bytes a listener does.
	path, ok := localMediaFile(voiceURL)
	if !ok {
		return fmt.Errorf("zior: audio url %q of work %s is not a Caeor media object", voiceURL, workID)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("zior: opening voice file of work %s: %w", workID, err)
	}
	defer f.Close()
	ctx, cancel := context.WithTimeout(ctx, zior.UploadTimeout)
	defer cancel()
	a, err := client.Analyze(ctx, h.cfg.InternalAPIKey, filepath.Base(path), f)
	if errors.Is(err, zior.ErrUnprocessable) || errors.Is(err, zior.ErrTooLarge) {
		// A property of the file, not a fault: the text mapping stays, and
		// retrying would only hear the same file again.
		log.Printf("[zior] work %s by %s keeps its text mapping: %v", workID, creatorID, err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("zior: analysing work %s: %w", workID, err)
	}
	err = dbpkg.SetWorkAudioAnalysis(h.db, workID, dbpkg.WorkAudioAnalysis{
		Vector:      a.Vector.Slice(),
		Mood:        a.Mood,
		Descriptor:  a.Descriptor,
		ContextTags: a.ContextTags,
		BPM:         a.BPM,
		Key:         a.Key,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil // deleted while Zior listened
	}
	return err
}
