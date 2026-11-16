// Runs a Swift-built `POST /events` body through everything `createWork` does
// before it touches the database or the media registry.
//
// workCanonicalPayload, workKinds, cidRe, verifyCID, parseWorkEnvelope's field
// list, the timestamp, text, repost, gating, citation and poll rules are COPIED
// from feed-engine/internal/handler/work_event.go, and the text rules from
// feed-engine/internal/worktext/worktext.go (normalizer v1). Three
// substitutions, each because the dependency is a service:
//
//   - the session account is the vector's `account_id` (the server reads it
//     from the session);
//   - the signing key is the vector's (the server asks Elohim Veni);
//   - "now" is the payload's own timestamp_ms, so a recorded vector stays
//     decidable — the freshness window itself is the server clock's to judge.
//
// Media is not resolved (that is Caeor's), and nothing is inserted.
//
//	go run ./envelope envelopes_from_swift.json
package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ─── COPIED from work_event.go ───────────────────────────────────────────────

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

var workKinds = map[string]bool{
	"post": true, "reply": true, "quote": true, "poll": true, "video": true,
	"voice": true, "thread_post": true, "react_video": true, "track": true,
}

var commentGatings = []string{"everyone", "followers", "circle", "verified", "none"}

var cidRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func verifyCID(claimed string, p workCanonicalPayload) bool {
	if p.MediaURLs == nil {
		p.MediaURLs = []string{}
	}
	// The server's line here is append([]string(nil), ...), which turns the
	// empty list back into nil and hashes "media_urls":null — contradicting its
	// own comment and malkuth.js, which hash []. That is a feed-engine defect;
	// this copy hashes the contract, [], so the fixtures say what every client
	// must send.
	p.MediaURLs = append([]string{}, p.MediaURLs...)
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
	canonical := bytes.TrimRight(buf.Bytes(), "\n")
	sum := sha256.Sum256(canonical)
	return claimed == fmt.Sprintf("sha256:%x", sum)
}

// verifyMalkuthSig with the key supplied instead of fetched.
func verifySig(pubKeyB64, cid, sigBase64URL string) bool {
	spkiBytes, err := base64.StdEncoding.DecodeString(pubKeyB64)
	if err != nil {
		if spkiBytes, err = base64.RawStdEncoding.DecodeString(pubKeyB64); err != nil {
			return false
		}
	}
	pub, err := x509.ParsePKIXPublicKey(spkiBytes)
	if err != nil {
		return false
	}
	ecPub, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return false
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(sigBase64URL)
	if err != nil || len(sigBytes) != 64 {
		return false
	}
	digest := sha256.Sum256([]byte(cid))
	r := new(big.Int).SetBytes(sigBytes[:32])
	s := new(big.Int).SetBytes(sigBytes[32:])
	return ecdsa.Verify(ecPub, digest[:], r, s)
}

// ─── COPIED from worktext.go (normalizer v1) ─────────────────────────────────

const maxWeightedLength = 2000
const maxRunes = 10000
const urlWeight = 23

var urlRe = regexp.MustCompile(`https?://[^\s"'<>\x00-\x1f]+`)

const urlTrailing = ".,;:!?)'\""

func stripped(r rune) bool {
	switch {
	case r == '\n' || r == '\t':
		return false
	case r < 0x20, r == 0x7F, r >= 0x80 && r <= 0x9F:
		return true
	case r == 0x061C, r == 0x200E, r == 0x200F,
		r >= 0x202A && r <= 0x202E,
		r >= 0x2066 && r <= 0x2069:
		return true
	case r == 0x00AD, r == 0x180E, r == 0x200B,
		r >= 0x2060 && r <= 0x2064,
		r >= 0x206A && r <= 0x206F,
		r == 0x3164, r == 0xFFA0,
		r == 0xFEFF,
		r >= 0xFFF9 && r <= 0xFFFB:
		return true
	}
	return false
}

func normalize(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.Map(func(r rune) rune {
		if stripped(r) {
			return -1
		}
		return r
	}, s)
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := false
	for _, line := range lines {
		if strings.TrimFunc(line, unicode.IsSpace) == "" {
			if blank {
				continue
			}
			blank = true
			out = append(out, "")
			continue
		}
		blank = false
		out = append(out, line)
	}
	return strings.TrimFunc(strings.Join(out, "\n"), unicode.IsSpace)
}

func isNormalized(s string) bool { return normalize(s) == s }

func isBlank(s string) bool {
	for _, r := range s {
		if unicode.In(r, unicode.L, unicode.N, unicode.P, unicode.S) {
			return false
		}
	}
	return true
}

func runeWeight(r rune) int {
	switch {
	case unicode.In(r, unicode.Mn, unicode.Me),
		r == 0x200C, r == 0x200D,
		r >= 0xFE00 && r <= 0xFE0F,
		r >= 0xE0100 && r <= 0xE01EF,
		r >= 0x1F3FB && r <= 0x1F3FF,
		r >= 0xE0020 && r <= 0xE007F:
		return 0
	case r <= 0x10FF,
		r >= 0x2000 && r <= 0x200D,
		r >= 0x2010 && r <= 0x201F,
		r >= 0x2032 && r <= 0x2037:
		return 1
	}
	return 2
}

func textWeight(s string) int {
	n := 0
	for _, r := range s {
		n += runeWeight(r)
	}
	return n
}

func weightedLength(s string) int {
	n := 0
	last := 0
	for _, m := range urlRe.FindAllStringIndex(s, -1) {
		link := strings.TrimRight(s[m[0]:m[1]], urlTrailing)
		n += textWeight(s[last:m[0]]) + urlWeight
		last = m[0] + len(link)
	}
	return n + textWeight(s[last:])
}

// ─── createWork's checks, in its order, up to the database ───────────────────

type verdict struct {
	Accepted bool   `json:"accepted"`
	Status   int    `json:"status"`
	Reason   string `json:"reason"`
	// What the server would have derived and stored.
	Kind         string `json:"kind"`
	ParentCID    string `json:"parent_cid"`
	IsNSFW       bool   `json:"is_nsfw"`
	QuotedWorkID string `json:"quoted_work_id"`
}

func refuse(status int, reason string) verdict { return verdict{Status: status, Reason: reason} }
func invalid(field string) verdict             { return refuse(400, "invalid_field "+field) }

func optString(v interface{}) (string, bool) {
	switch t := v.(type) {
	case nil:
		return "", true
	case string:
		return t, true
	}
	return "", false
}

func admit(bodyBytes []byte, pubKeyB64, accountID string) verdict {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(bodyBytes, &raw); err != nil {
		return refuse(400, "body is not a JSON object")
	}
	// eventsPost routes a body with a sha256: cid to workEvent.
	var cid string
	if v, ok := raw["cid"]; !ok || json.Unmarshal(v, &cid) != nil || !strings.HasPrefix(cid, "sha256:") {
		return refuse(404, "would not route to workEvent")
	}

	// parseWorkEnvelope.
	var p workCanonicalPayload
	var signature, quoted string
	var isNSFW bool
	fields := []struct {
		key      string
		into     interface{}
		required bool
	}{
		{"cid", &cid, true},
		{"signature", &signature, true},
		{"payload", &p, true},
		{"is_nsfw", &isNSFW, false},
		{"quoted_work_id", &quoted, false},
	}
	for _, f := range fields {
		val, ok := raw[f.key]
		if !ok || bytes.Equal(bytes.TrimSpace(val), []byte("null")) {
			if f.required {
				return invalid(f.key)
			}
			continue
		}
		if err := json.Unmarshal(val, f.into); err != nil {
			return invalid(f.key)
		}
	}
	for _, removed := range []string{"video_watermarked_url"} {
		if _, ok := raw[removed]; ok {
			return refuse(400, "client sent removed field "+removed)
		}
	}
	var payloadKeys map[string]json.RawMessage
	_ = json.Unmarshal(raw["payload"], &payloadKeys)
	if _, ok := payloadKeys["tus_upload_id"]; ok {
		return refuse(400, "client sent removed field tus_upload_id")
	}

	if !cidRe.MatchString(cid) {
		return invalid("cid")
	}
	if signature == "" {
		return invalid("signature")
	}
	if p.AuthorAccount != accountID {
		return invalid("author_account")
	}
	if !verifyCID(cid, p) {
		return invalid("cid")
	}
	if !verifySig(pubKeyB64, cid, signature) {
		return invalid("signature")
	}

	// validateWork, with now = the signing time.
	now := time.UnixMilli(p.TimestampMS)
	if p.TimestampMS <= 0 {
		return invalid("timestamp_ms")
	}
	kind := p.Kind
	if !workKinds[kind] {
		return invalid("kind")
	}
	if !isNormalized(p.Body) {
		return invalid("body")
	}
	if utf8.RuneCountInString(p.Body) > maxRunes || weightedLength(p.Body) > maxWeightedLength {
		return refuse(422, "text_too_long")
	}
	if p.IsRepost || p.RepostSourceID != nil {
		return invalid("is_repost")
	}
	gating := p.CommentGating
	if gating == "" {
		gating = "everyone"
	}
	ok := false
	for _, g := range commentGatings {
		ok = ok || g == gating
	}
	if !ok {
		return invalid("comment_gating")
	}
	parent, good := optString(p.ParentCID)
	if !good || (parent != "" && !cidRe.MatchString(parent)) || parent == cid {
		return invalid("parent_cid")
	}
	if kind == "reply" && parent == "" {
		return invalid("parent_cid")
	}
	if kind == "quote" && quoted == "" {
		return invalid("quoted_work_id")
	}
	if bad := validatePoll(kind, p, now); bad != "" {
		return invalid(bad)
	}
	return verdict{
		Accepted: true, Status: 201, Reason: "would insert",
		Kind: kind, ParentCID: parent, IsNSFW: isNSFW, QuotedWorkID: quoted,
	}
}

// validatePoll, returning the refused field or "".
func validatePoll(kind string, p workCanonicalPayload, now time.Time) string {
	if p.PollOptions == nil {
		if kind == "poll" {
			return "poll_options"
		}
		if p.PollEndsAt != nil {
			return "poll_ends_at"
		}
		return ""
	}
	if kind != "poll" {
		return "poll_options"
	}
	raw, ok := p.PollOptions.([]interface{})
	if !ok || len(raw) < 2 || len(raw) > 4 {
		return "poll_options"
	}
	seen := map[string]bool{}
	for _, item := range raw {
		o, ok := item.(string)
		if !ok || !isNormalized(o) || strings.ContainsRune(o, '\n') || isBlank(o) || utf8.RuneCountInString(o) > 80 {
			return "poll_options"
		}
		k := strings.ToLower(o)
		if seen[k] {
			return "poll_options"
		}
		seen[k] = true
	}
	s, ok := optString(p.PollEndsAt)
	if !ok || s == "" {
		return "poll_ends_at"
	}
	ends, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return "poll_ends_at"
	}
	opens := now
	if at, ok := optString(p.ScheduledAt); ok && at != "" {
		if t, err := time.Parse(time.RFC3339, at); err == nil {
			opens = t
		}
	}
	if d := ends.Sub(opens); d < 5*time.Minute || d > 7*24*time.Hour {
		return "poll_ends_at"
	}
	return ""
}

type caseIn struct {
	Name             string `json:"name"`
	PublicKeySPKIB64 string `json:"public_key_spki_b64"`
	AccountID        string `json:"account_id"`
	BodyB64          string `json:"body_b64"`
	ExpectAccepted   bool   `json:"expect_accepted"`
	ExpectReason     string `json:"expect_reason"`
	ExpectKind       string `json:"expect_kind"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: envelope <file>")
		os.Exit(2)
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	var file struct {
		Cases []caseIn `json:"cases"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		panic(err)
	}
	if len(file.Cases) == 0 {
		panic("no cases")
	}

	results := []map[string]any{}
	failures := 0
	for _, c := range file.Cases {
		body, err := base64.StdEncoding.DecodeString(c.BodyB64)
		if err != nil {
			panic(err)
		}
		v := admit(body, c.PublicKeySPKIB64, c.AccountID)

		problems := []string{}
		if v.Accepted != c.ExpectAccepted {
			problems = append(problems, fmt.Sprintf("accepted=%v want %v", v.Accepted, c.ExpectAccepted))
		}
		if c.ExpectReason != "" && v.Reason != c.ExpectReason {
			problems = append(problems, fmt.Sprintf("reason=%q want %q", v.Reason, c.ExpectReason))
		}
		if c.ExpectKind != "" && v.Kind != c.ExpectKind {
			problems = append(problems, fmt.Sprintf("kind=%q want %q", v.Kind, c.ExpectKind))
		}
		status := "ok"
		if len(problems) > 0 {
			status = "MISMATCH: " + strings.Join(problems, "; ")
			failures++
		}
		fmt.Printf("%-34s %-3d %-34s %s\n", c.Name, v.Status, v.Reason, status)

		results = append(results, map[string]any{
			"name":            c.Name,
			"accepted":        v.Accepted,
			"status":          v.Status,
			"reason":          v.Reason,
			"kind":            v.Kind,
			"parent_cid":      v.ParentCID,
			"is_nsfw":         v.IsNSFW,
			"quoted_work_id":  v.QuotedWorkID,
			"expect_accepted": c.ExpectAccepted,
		})
	}

	out, _ := json.MarshalIndent(map[string]any{
		"go_version": runtime.Version(),
		"results":    results,
	}, "", "  ")
	_ = os.WriteFile(os.Args[1]+".verdicts", out, 0o644)
	fmt.Printf("\n%d/%d as expected\n", len(file.Cases)-failures, len(file.Cases))
	if failures > 0 {
		os.Exit(1)
	}
}
