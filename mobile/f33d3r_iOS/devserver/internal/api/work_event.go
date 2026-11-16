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
	"math"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"f33d3r.com/ios/devserver/internal/store"
	"f33d3r.com/ios/devserver/internal/worktext"
)

// This file is feed-engine/internal/handler/work_event.go and work_create.go —
// the create path — carried over as literally as the dialect allows. The
// canonical struct, the hashing, the signature check, the kind and gating
// vocabularies, the text rules (internal/worktext, copied verbatim), the
// bounds, the state vocabulary, its statuses and its sentences are the real
// server's, because the clients are written against exactly these. What this
// server does not have it says so where it stands: no rate limit, and the
// integrity authority is the media_assets table.

// workCanonicalPayload is marshalled to canonical JSON for CID verification.
// Field declaration order = JSON output order. Keys alphabetical by JSON name.
// is_repost and repost_source_id stay in the wire form because every client's
// content address is computed over them; the server refuses any envelope that
// sets them.
type workCanonicalPayload struct {
	AuthorAccount     string      `json:"author_account"`
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
// works.kind in schema.sql. (feed-engine also has `track`, the music lane's
// kind; this server has no track works.)
var workKinds = map[string]bool{
	"post": true, "reply": true, "quote": true, "poll": true,
	"video": true, "voice": true, "thread_post": true, "react_video": true,
}

// Bounds of a work, all checked before anything is written.
const (
	workSignedMaxAge     = time.Hour
	workSignedMaxAhead   = 5 * time.Minute
	workMaxScheduleAhead = 366 * 24 * time.Hour
	workPollMinOptions   = 2
	workPollMaxOptions   = 4
	workPollOptionRunes  = 80
	workPollMinDuration  = 5 * time.Minute
	workPollMaxDuration  = 7 * 24 * time.Hour
	workMaxClientTags    = 10
	workMaxTags          = 30
	workTagRunes         = 64
	workMaxMedia         = 4
	workMaxDurationSecs  = 4 * 60 * 60
	workMaxVideoSide     = 16384
	// workDedupWindow is how long the same content by the same author is a
	// Duplicate rather than a new work (dbpkg.WorkDedupWindow).
	workDedupWindow = 12 * time.Hour
)

var reactLayouts = map[string]bool{
	"presenter": true, "pip": true, "split": true, "card_over": true, "media_only": true, "green_screen": true,
}

var cidRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

var hashtagRe = regexp.MustCompile(`(?m)#([A-Za-z0-9_]{1,100})`)

// canonicalBytes is the exact byte string the CID hashes. An empty media list
// is [] — the contract malkuth.js and every client hash.
func canonicalBytes(p workCanonicalPayload) ([]byte, error) {
	if p.MediaURLs == nil {
		p.MediaURLs = []string{}
	}
	p.MediaURLs = append([]string{}, p.MediaURLs...)
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
func verifyCID(claimed string, p workCanonicalPayload) (bool, error) {
	canonical, err := canonicalBytes(p)
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256(canonical)
	return claimed == fmt.Sprintf("sha256:%x", sum), nil
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

// ── The outcome of a create (work_create.go) ─────────────────────────────────

// workCreateState is the closed vocabulary of create outcomes.
type workCreateState string

const (
	workStateOk                workCreateState = "ok"
	workStateDuplicate         workCreateState = "duplicate"
	workStateTextTooLong       workCreateState = "text_too_long"
	workStateTextBlank         workCreateState = "text_blank"
	workStateInvalidField      workCreateState = "invalid_field"
	workStateCitedNotFound     workCreateState = "cited_not_found"
	workStateRepliesRestricted workCreateState = "replies_restricted"
	workStatePostingRestricted workCreateState = "posting_restricted"
	workStateRateLimited       workCreateState = "rate_limited"
	workStateServerError       workCreateState = "server_error"
)

func (st workCreateState) status() int {
	switch st {
	case workStateOk:
		return http.StatusCreated
	case workStateDuplicate:
		return http.StatusConflict
	case workStateTextTooLong, workStateTextBlank:
		return http.StatusUnprocessableEntity
	case workStateInvalidField:
		return http.StatusBadRequest
	case workStateCitedNotFound:
		return http.StatusNotFound
	case workStateRepliesRestricted, workStatePostingRestricted:
		return http.StatusForbidden
	case workStateRateLimited:
		return http.StatusTooManyRequests
	}
	return http.StatusInternalServerError
}

// workCreateAnswer is one decided outcome.
type workCreateAnswer struct {
	State    workCreateState
	Field    string
	Detail   string
	WorkID   string
	CID      string
	Replayed bool
	Over     int
}

func refuse(state workCreateState) *workCreateAnswer { return &workCreateAnswer{State: state} }

func invalidField(field, detail string) *workCreateAnswer {
	return &workCreateAnswer{State: workStateInvalidField, Field: field, Detail: detail}
}

// message is what the poster reads — work_create.go's sentences.
func (a *workCreateAnswer) message() string {
	switch a.State {
	case workStateOk:
		if a.Replayed {
			return "Already posted."
		}
		return "Posted."
	case workStateDuplicate:
		return "You already posted this."
	case workStateTextTooLong:
		return fmt.Sprintf("Your post is %d over the limit.", a.Over)
	case workStateTextBlank:
		return "Write something, or add media, a poll or a voice note."
	case workStateInvalidField:
		return "This post could not be sent: " + a.Field + " — " + a.Detail + "."
	case workStateCitedNotFound:
		return "That post is no longer available."
	case workStateRepliesRestricted:
		return "The author has limited who can reply."
	case workStatePostingRestricted:
		return "Posting is restricted on this account."
	case workStateRateLimited:
		return "You have reached the posting limit. Try again later."
	}
	return "Something went wrong. Your post was not sent."
}

// workAccepted is the 201 body: the row id and the CID it was stored under.
type workAccepted struct {
	WorkID string `json:"work_id"`
	CID    string `json:"cid"`
}

// answerWorkCreate writes an outcome: the receipt on success, the sentence
// otherwise, and the state's name in X-Work-Create-State either way.
func answerWorkCreate(w http.ResponseWriter, a *workCreateAnswer) {
	if a.State == workStateServerError || a.State == workStateInvalidField {
		log.Printf("[work] %s: %s %s", a.State, a.Field, a.Detail)
	}
	w.Header().Set("X-Work-Create-State", string(a.State))
	if a.State != workStateOk {
		plainError(w, a.State.status(), a.message())
		return
	}
	writeJSON(w, http.StatusCreated, workAccepted{WorkID: a.WorkID, CID: a.CID})
}

// ── Admission (work_event.go) ────────────────────────────────────────────────

// workEnvelope is the signed envelope and the unsigned fields beside it.
type workEnvelope struct {
	CID              string
	Signature        string
	Payload          workCanonicalPayload
	IsNSFW           bool
	VideoWidth       int
	VideoHeight      int
	QuotedWorkID     string
	ReactLayout      string
	DownloadsAllowed *bool
}

// parseWorkEnvelope reads every field the create path uses. Unknown keys are
// ignored, video_watermarked_url and payload.tus_upload_id among them.
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

func optString(v interface{}, field string) (string, *workCreateAnswer) {
	switch t := v.(type) {
	case nil:
		return "", nil
	case string:
		return t, nil
	}
	return "", invalidField(field, "must be a string or null")
}

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

// workEvent admits a signed work.
func (s *Server) workEvent(w http.ResponseWriter, r *http.Request, u *store.User, body *eventBody) {
	if body.raw == nil {
		answerWorkCreate(w, invalidField("payload", "signed works are JSON"))
		return
	}
	answerWorkCreate(w, s.createWork(r, u, body.raw))
}

// createWork decides one create. Every refusal is typed and synchronous.
func (s *Server) createWork(r *http.Request, u *store.User, raw map[string]json.RawMessage) *workCreateAnswer {
	ctx := r.Context()
	env, bad := parseWorkEnvelope(raw)
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
	if p.AuthorAccount != u.ID {
		return invalidField("author_account", "is not the signed-in account")
	}
	matches, err := verifyCID(env.CID, p)
	if err != nil {
		return &workCreateAnswer{State: workStateServerError, Detail: err.Error()}
	}
	if !matches {
		return invalidField("cid", "does not match the payload")
	}
	if !verifySignature(s.store.SigningKey(ctx, u.PIALID), env.CID, env.Signature) {
		return invalidField("signature", "does not verify against this identity's key")
	}

	// A re-delivered envelope is answered with the work it already created,
	// before any rule that depends on the clock.
	if prior, err := s.store.GetWorkByCID(ctx, env.CID, ""); err == nil {
		if prior.AuthorID != u.ID {
			return invalidField("cid", "names a work this account cannot create; sign the work again")
		}
		return &workCreateAnswer{State: workStateOk, WorkID: prior.ID, CID: env.CID, Replayed: true}
	} else if !errors.Is(err, store.ErrNotFound) {
		return &workCreateAnswer{State: workStateServerError, Detail: err.Error()}
	}

	params, fingerprint, bad := s.validateWork(r, u, env)
	if bad != nil {
		return bad
	}
	if prior, err := s.store.FindWorkByFingerprint(ctx, u.ID, fingerprint, time.Now().Add(-workDedupWindow)); err == nil {
		return &workCreateAnswer{State: workStateDuplicate, WorkID: prior}
	} else if !errors.Is(err, store.ErrNotFound) {
		return &workCreateAnswer{State: workStateServerError, Detail: err.Error()}
	}
	params.Fingerprint = fingerprint

	workID, err := s.store.InsertWork(ctx, *params)
	if err != nil {
		if errors.Is(err, store.ErrDuplicateCID) {
			if existing, err := s.store.GetWorkByCID(ctx, env.CID, ""); err == nil && existing.AuthorID == u.ID {
				return &workCreateAnswer{State: workStateOk, WorkID: existing.ID, CID: env.CID, Replayed: true}
			}
		}
		return &workCreateAnswer{State: workStateServerError, Detail: err.Error()}
	}

	s.notifyForWork(r, u, workID, params.Kind, params.ParentCID, params.QuotedCID, params.Body)
	if params.Kind != "reply" && params.ParentCID == "" {
		if followers, err := s.store.FollowerIDs(ctx, u.ID); err == nil {
			for _, fid := range followers {
				s.hub.publish(userTopic(fid), "new_post", map[string]string{"work_id": workID, "author_handle": u.Handle})
			}
		}
	}
	return &workCreateAnswer{State: workStateOk, WorkID: workID, CID: env.CID}
}

// validateWork turns a verified envelope into insert parameters and the
// content fingerprint, or the first reason it cannot.
func (s *Server) validateWork(r *http.Request, u *store.User, env *workEnvelope) (*store.InsertWorkParams, string, *workCreateAnswer) {
	ctx := r.Context()
	p := env.Payload
	now := time.Now()

	signed := time.UnixMilli(p.TimestampMS)
	if p.TimestampMS <= 0 || now.Sub(signed) > workSignedMaxAge || signed.Sub(now) > workSignedMaxAhead {
		return nil, "", invalidField("timestamp_ms", "the work must be signed within the last hour")
	}
	kind := p.Kind
	if !workKinds[kind] {
		return nil, "", invalidField("kind", "is not a work kind")
	}
	body := p.Body
	if !worktext.IsNormalized(body) {
		return nil, "", invalidField("body", fmt.Sprintf("is not normalized (text normalizer v%d)", worktext.NormalizerVersion))
	}
	if n := utf8.RuneCountInString(body); n > worktext.MaxRunes {
		return nil, "", &workCreateAnswer{State: workStateTextTooLong, Over: n - worktext.MaxRunes}
	}
	if n := worktext.WeightedLength(body); n > worktext.MaxWeightedLength {
		return nil, "", &workCreateAnswer{State: workStateTextTooLong, Over: n - worktext.MaxWeightedLength}
	}
	if p.IsRepost || p.RepostSourceID != nil {
		return nil, "", invalidField("is_repost", "a repost is a reaction, not a signed work")
	}
	gating := p.CommentGating
	if gating == "" {
		gating = "everyone"
	}
	if !store.ValidCommentGating(gating) {
		return nil, "", invalidField("comment_gating", "must be one of "+strings.Join(store.CommentGatings, ", "))
	}

	parentCID, bad := optString(p.ParentCID, "parent_cid")
	if bad != nil {
		return nil, "", bad
	}
	if parentCID != "" && !cidRe.MatchString(parentCID) {
		return nil, "", invalidField("parent_cid", "is not a content address")
	}
	if kind == "reply" && parentCID == "" {
		return nil, "", invalidField("parent_cid", "a reply names the work it answers")
	}
	parentWorkID := ""
	if parentCID != "" {
		parent, err := s.store.GetWorkByCID(ctx, parentCID, u.ID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, "", &workCreateAnswer{State: workStateCitedNotFound, Field: "parent_cid"}
		}
		if err != nil {
			return nil, "", &workCreateAnswer{State: workStateServerError, Detail: err.Error()}
		}
		if !s.mayReply(r, parent, u) {
			return nil, "", refuse(workStateRepliesRestricted)
		}
		parentWorkID = parent.ID
	}
	quotedCID := ""
	if env.QuotedWorkID != "" {
		q, err := s.store.GetWorkByID(ctx, env.QuotedWorkID, "")
		if errors.Is(err, store.ErrNotFound) {
			return nil, "", &workCreateAnswer{State: workStateCitedNotFound, Field: "quoted_work_id"}
		}
		if err != nil {
			return nil, "", &workCreateAnswer{State: workStateServerError, Detail: err.Error()}
		}
		quotedCID = q.CID
	}
	if kind == "quote" && env.QuotedWorkID == "" {
		return nil, "", invalidField("quoted_work_id", "a quote names the work it quotes")
	}

	scheduledAt, bad := optTime(p.ScheduledAt, "scheduled_at")
	if bad != nil {
		return nil, "", bad
	}
	if scheduledAt != nil {
		if !scheduledAt.After(now) {
			return nil, "", invalidField("scheduled_at", "must be in the future")
		}
		if scheduledAt.Sub(now) > workMaxScheduleAhead {
			return nil, "", invalidField("scheduled_at", "must be within a year")
		}
	}
	pollOptions, pollEndsAt, bad := validatePoll(kind, p, scheduledAt, now)
	if bad != nil {
		return nil, "", bad
	}
	tags, bad := validateTags(p.Tags, body)
	if bad != nil {
		return nil, "", bad
	}
	media, bad := s.resolveWorkMedia(r, u, kind, p)
	if bad != nil {
		return nil, "", bad
	}
	if worktext.IsBlank(body) && !media.any() && len(pollOptions) == 0 {
		return nil, "", refuse(workStateTextBlank)
	}
	if env.VideoWidth < 0 || env.VideoWidth > workMaxVideoSide || env.VideoHeight < 0 || env.VideoHeight > workMaxVideoSide {
		return nil, "", invalidField("video_width", fmt.Sprintf("dimensions must be between 0 and %d", workMaxVideoSide))
	}
	if env.ReactLayout != "" && (kind != "react_video" || !reactLayouts[env.ReactLayout]) {
		return nil, "", invalidField("react_layout", "is a layout of a react_video work")
	}

	fingerprint, err := worktext.Fingerprint(worktext.FingerprintInput{
		Body: body, Kind: kind, ParentWorkID: parentWorkID, QuotedWorkID: env.QuotedWorkID,
		MediaURLs: media.images, VoiceURL: media.voice, VideoURL: media.videoMaster, PollOptions: pollOptions,
	})
	if err != nil {
		return nil, "", &workCreateAnswer{State: workStateServerError, Detail: err.Error()}
	}
	return &store.InsertWorkParams{
		AuthorID: u.ID, AuthorPIAL: u.PIALID, CID: env.CID, Body: body, Kind: kind,
		MediaURLs: media.images, ParentCID: parentCID, QuotedCID: quotedCID,
		Tags: tags, PollOptions: pollOptions, PollEndsAt: pollEndsAt, ScheduledAt: scheduledAt,
		CommentGating: gating, SubscriberOnly: p.SubscriberOnly, IsNSFW: env.IsNSFW,
		VoiceURL: media.voice, VoiceDurationSecs: media.voiceSecs,
		VideoMasterURL: media.videoMaster, VideoPosterURL: media.videoPoster,
		VideoDurationSecs: media.videoSecs, VideoWidth: env.VideoWidth, VideoHeight: env.VideoHeight,
		ReactLayout: env.ReactLayout, LineageHandle: media.creditHandle,
	}, fingerprint, nil
}

// validatePoll checks a poll's options and its end, as feed-engine does.
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

// workMedia is a work's media as the asset registry vouches for it.
type workMedia struct {
	images       []string
	voice        string
	voiceSecs    *int
	videoMaster  string
	videoPoster  string
	videoSecs    *int
	creditHandle string
}

func (m workMedia) any() bool {
	return len(m.images) > 0 || m.voice != "" || m.videoMaster != ""
}

// workAsset resolves one media path a work names. A path this server issued
// must be a ready asset of the right kind; a path under /media/ is media an
// older database or the seed holds, kept at its path as feed-engine keeps
// imported legacy media. Anything else is not media this server issued.
func (s *Server) workAsset(r *http.Request, field, raw, kind string) (*store.MediaAsset, *workCreateAnswer) {
	if strings.HasPrefix(raw, "/media/") && !strings.Contains(raw, "..") {
		return nil, nil
	}
	id := assetIDFromPath(raw)
	if id == "" {
		return nil, invalidField(field, "is not a media object this server issued")
	}
	asset, err := s.store.MediaAsset(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, invalidField(field, "is not an upload this server knows")
	}
	if err != nil {
		return nil, &workCreateAnswer{State: workStateServerError, Field: field, Detail: err.Error()}
	}
	switch asset.Status {
	case store.AssetBanned:
		return nil, invalidField(field, "has been removed from the platform")
	case store.AssetHeld:
		return nil, invalidField(field, "is being reviewed; post it once the review is done")
	}
	if asset.Kind != kind {
		return nil, invalidField(field, "names media of the wrong kind")
	}
	return asset, nil
}

// credit records the first asset in the work that the author did not upload.
func (m *workMedia) credit(author *store.User, asset *store.MediaAsset) {
	if asset == nil || asset.UploaderID == author.ID || m.creditHandle != "" {
		return
	}
	m.creditHandle = asset.UploaderHandle
}

func gifProviderHost(host string) bool {
	host = strings.ToLower(host)
	return host == "klipy.com" || strings.HasSuffix(host, ".klipy.com")
}

// resolveWorkMedia accepts media only as assets this server issued (or a GIF
// from the picker's provider), recording the original uploader's credit.
func (s *Server) resolveWorkMedia(r *http.Request, u *store.User, kind string, p workCanonicalPayload) (workMedia, *workCreateAnswer) {
	m := workMedia{images: []string{}}
	if len(p.MediaURLs) > workMaxMedia {
		return m, invalidField("media_urls", fmt.Sprintf("at most %d", workMaxMedia))
	}
	seen := map[string]bool{}
	for _, raw := range p.MediaURLs {
		if seen[raw] {
			return m, invalidField("media_urls", "lists the same media twice")
		}
		seen[raw] = true
		if strings.HasPrefix(raw, "https://") || strings.HasPrefix(raw, "http://") {
			parsed, err := url.Parse(raw)
			if err != nil || parsed.Scheme != "https" || !gifProviderHost(parsed.Hostname()) {
				return m, invalidField("media_urls", "external media must be a GIF from the GIF picker")
			}
			m.images = append(m.images, raw)
			continue
		}
		asset, bad := s.workAsset(r, "media_urls", raw, "image")
		if bad != nil {
			return m, bad
		}
		m.credit(u, asset)
		m.images = append(m.images, raw)
	}

	voice, bad := optString(p.VoiceURL, "voice_url")
	if bad != nil {
		return m, bad
	}
	if voice != "" {
		asset, bad := s.workAsset(r, "voice_url", voice, "voice")
		if bad != nil {
			return m, bad
		}
		if m.voiceSecs, bad = optSeconds(p.VoiceDurationSecs, "voice_duration_secs"); bad != nil {
			return m, bad
		}
		m.credit(u, asset)
		m.voice = voice
	} else if p.VoiceDurationSecs != nil {
		return m, invalidField("voice_duration_secs", "only a voice recording has a voice duration")
	}
	if kind == "voice" && m.voice == "" {
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
	asset, bad := s.workAsset(r, "video_master_url", master, "video")
	if bad != nil {
		return m, bad
	}
	if m.videoSecs, bad = optSeconds(p.VideoDurationSecs, "video_duration_secs"); bad != nil {
		return m, bad
	}
	m.credit(u, asset)
	m.videoMaster = master
	m.videoPoster = poster
	return m, nil
}

// mayReply is the server side of Work.viewerMayReply: the parent author's
// rule, enforced where the reply lands.
func (s *Server) mayReply(r *http.Request, parent *store.Work, u *store.User) bool {
	if parent.AuthorID == u.ID {
		return true
	}
	// feed-engine's replyAllowed: followers are accounts that follow the
	// author; circle is both ways; verified is a verified account.
	switch parent.CommentGating {
	case "everyone":
		return true
	case "followers":
		ok, err := s.store.IsFollowing(r.Context(), u.ID, parent.AuthorID)
		return err == nil && ok
	case "circle":
		posterFollows, err := s.store.IsFollowing(r.Context(), u.ID, parent.AuthorID)
		if err != nil || !posterFollows {
			return false
		}
		authorFollows, err := s.store.IsFollowing(r.Context(), parent.AuthorID, u.ID)
		return err == nil && authorFollows
	case "verified":
		return u.IsVerified
	}
	return false // "none", and anything outside the vocabulary
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
