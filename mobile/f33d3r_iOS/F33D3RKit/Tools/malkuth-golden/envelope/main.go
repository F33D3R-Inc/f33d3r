// Runs a Swift-built `POST /events` body through everything `workEvent` does
// before it touches the database.
//
// workCanonicalPayload, workKinds, workEnvelope and verifyCID are COPIED
// VERBATIM from feed-engine/internal/handler/work_event.go. The body parsing
// mirrors workEvent's own loop over rawBody. The only substitution is the
// signing key: verifyMalkuthSig's step 1 fetches it from Elohim Veni, which is
// not running, so the vector supplies the key the registration would have given
// it. Everything after that is unchanged.
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
	"runtime"
	"sort"
	"strings"
)

// ─── VERBATIM ────────────────────────────────────────────────────────────────

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

var workKinds = map[string]bool{
	"post": true, "reply": true, "quote": true, "poll": true,
	"video": true, "voice": true, "thread_post": true, "react_video": true,
}

type workEnvelope struct {
	EventType string               `json:"event_type"`
	CID       string               `json:"cid"`
	Signature string               `json:"signature"`
	Payload   workCanonicalPayload `json:"payload"`
}

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
	canonical := bytes.TrimRight(buf.Bytes(), "\n")
	sum := sha256.Sum256(canonical)
	expected := fmt.Sprintf("sha256:%x", sum)
	return claimed == expected
}

// verifyMalkuthSig steps 2-6, verbatim; step 1 replaced by the supplied key.
func verifySig(pubKeyB64, cid, sigBase64URL string) bool {
	spkiBytes, err := base64.StdEncoding.DecodeString(pubKeyB64)
	if err != nil {
		spkiBytes, err = base64.RawStdEncoding.DecodeString(pubKeyB64)
		if err != nil {
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
	if err != nil {
		return false
	}
	if len(sigBytes) != 64 {
		return false
	}
	digest := sha256.Sum256([]byte(cid))
	r := new(big.Int).SetBytes(sigBytes[:32])
	s := new(big.Int).SetBytes(sigBytes[32:])
	return ecdsa.Verify(ecPub, digest[:], r, s)
}

// ─── workEvent's own admission checks, in its order ──────────────────────────

type verdict struct {
	Accepted bool   `json:"accepted"`
	Status   int    `json:"status"`
	Reason   string `json:"reason"`
	// What the server would have derived and stored.
	Kind        string `json:"kind"`
	ParentCID   string `json:"parent_cid"`
	TusUploadID string `json:"tus_upload_id"`
	IsNSFW      bool   `json:"is_nsfw"`
	QuotedWorkID string `json:"quoted_work_id"`
}

func admit(bodyBytes []byte, pubKeyB64 string) verdict {
	var rawBody map[string]json.RawMessage
	if err := json.Unmarshal(bodyBytes, &rawBody); err != nil {
		return verdict{Status: 400, Reason: "body is not a JSON object: " + err.Error()}
	}

	// eventsPost: event_type is required, and a cid routes to workEvent.
	var eventType string
	if v, ok := rawBody["event_type"]; ok {
		json.Unmarshal(v, &eventType)
	}
	if eventType == "" {
		return verdict{Status: 400, Reason: "event_type required"}
	}
	cidRaw, hasCID := rawBody["cid"]
	if !hasCID {
		return verdict{Status: 404, Reason: "no cid: would not route to workEvent"}
	}
	var cidStr string
	if json.Unmarshal(cidRaw, &cidStr) != nil || !strings.HasPrefix(cidStr, "sha256:") {
		return verdict{Status: 404, Reason: "cid present but not sha256:-prefixed: would not route to workEvent"}
	}

	// workEvent's parse loop.
	var env workEnvelope
	var isNSFW bool
	var quotedWorkID, tusUploadID string
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
			var extra struct {
				TusUploadID string `json:"tus_upload_id"`
			}
			if json.Unmarshal(val, &extra) == nil {
				tusUploadID = extra.TusUploadID
			}
		case "is_nsfw":
			json.Unmarshal(val, &isNSFW)
		case "quoted_work_id":
			json.Unmarshal(val, &quotedWorkID)
		}
	}

	if !strings.HasPrefix(env.CID, "sha256:") {
		return verdict{Status: 400, Reason: "invalid cid format"}
	}
	if env.Signature == "" {
		return verdict{Status: 400, Reason: "signature required"}
	}
	if !verifyCID(env.CID, env.Payload) {
		return verdict{Status: 400, Reason: "cid mismatch"}
	}
	if !verifySig(pubKeyB64, env.CID, env.Signature) {
		return verdict{Status: 403, Reason: "signature invalid"}
	}

	kind := strings.TrimSpace(env.Payload.Kind)
	if kind == "" {
		kind = "post"
	}
	if !workKinds[kind] {
		return verdict{Status: 400, Reason: "unknown work kind"}
	}
	if env.Payload.IsRepost {
		srcID, _ := env.Payload.RepostSourceID.(string)
		if strings.TrimSpace(srcID) == "" {
			return verdict{Status: 400, Reason: "repost requires repost_source_id"}
		}
	}
	parentCID := ""
	if s, ok := env.Payload.ParentCID.(string); ok {
		parentCID = s
	}
	if kind == "reply" && strings.TrimSpace(parentCID) == "" {
		return verdict{Status: 400, Reason: "reply requires parent_cid"}
	}

	return verdict{
		Accepted: true, Status: 201, Reason: "would insert",
		Kind: kind, ParentCID: parentCID, TusUploadID: tusUploadID,
		IsNSFW: isNSFW, QuotedWorkID: quotedWorkID,
	}
}

type caseIn struct {
	Name             string `json:"name"`
	PublicKeySPKIB64 string `json:"public_key_spki_b64"`
	BodyB64          string `json:"body_b64"`
	ExpectAccepted   bool   `json:"expect_accepted"`
	ExpectReason     string `json:"expect_reason"`
	ExpectKind       string `json:"expect_kind"`
	ExpectTusUploadID string `json:"expect_tus_upload_id"`
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
		v := admit(body, c.PublicKeySPKIB64)

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
		if c.ExpectTusUploadID != "" && v.TusUploadID != c.ExpectTusUploadID {
			problems = append(problems, fmt.Sprintf("tus=%q want %q", v.TusUploadID, c.ExpectTusUploadID))
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
			"tus_upload_id":   v.TusUploadID,
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
