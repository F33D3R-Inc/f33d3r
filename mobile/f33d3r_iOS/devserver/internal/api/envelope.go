package api

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// DeviceKey is one signing key, as a device holds it.
type DeviceKey struct{ Key *ecdsa.PrivateKey }

// SPKIBase64 is the form the key authority registers.
func (k DeviceKey) SPKIBase64() string {
	der, _ := x509.MarshalPKIXPublicKey(&k.Key.PublicKey)
	return base64.StdEncoding.EncodeToString(der)
}

// SignCID signs the UTF-8 bytes of a CID: ECDSA P-256 over SHA-256, P1363
// r||s, base64url without padding — the same bytes the Swift signer produces.
func (k DeviceKey) SignCID(cid string) string {
	digest := sha256.Sum256([]byte(cid))
	r, s, _ := ecdsa.Sign(rand.Reader, k.Key, digest[:])
	out := make([]byte, 64)
	r.FillBytes(out[:32])
	s.FillBytes(out[32:])
	return base64.RawURLEncoding.EncodeToString(out)
}

// WorkDraft is the part of a work a tool or test chooses.
type WorkDraft struct {
	AuthorPIAL string
	Kind       string
	Body       string
	Tags       []string
	MediaURLs  []string
	ParentCID  string

	// A voice note: the uploaded path and the recorder's whole seconds. Both
	// are signed. Setting them makes the kind `voice`, as the app's composer does.
	VoiceURL          string
	VoiceDurationSecs int

	// A video: the signed half (master, poster, whole seconds) and the
	// unsigned half that rides on the envelope (watermarked file, frame size).
	// Setting the master makes the kind `video`.
	VideoMasterURL      string
	VideoPosterURL      string
	VideoDurationSecs   int
	VideoWatermarkedURL string
	VideoWidth          int
	VideoHeight         int
}

// BuildEnvelope produces the `POST /events` body the app sends for a work:
// the canonical payload bytes spliced in verbatim, the CID over them, and the
// signature over the CID. Shared by the contract tests and `cmd/postas`, so
// the tooling posts exactly what a device posts.
func BuildEnvelope(k DeviceKey, d WorkDraft) (body []byte, cid string, err error) {
	if d.Kind == "" {
		d.Kind = "post"
	}
	if d.Tags == nil {
		d.Tags = []string{}
	}
	if d.MediaURLs == nil {
		d.MediaURLs = []string{}
	}
	p := workCanonicalPayload{
		AuthorPIAL: d.AuthorPIAL, Body: d.Body, CommentGating: "everyone", Kind: d.Kind,
		MediaURLs: d.MediaURLs, Tags: d.Tags, TimestampMS: time.Now().UnixMilli(),
	}
	if d.ParentCID != "" {
		p.ParentCID = d.ParentCID
	}
	if d.VoiceURL != "" {
		p.Kind = "voice"
		p.VoiceURL = d.VoiceURL
		p.VoiceDurationSecs = d.VoiceDurationSecs
	} else if d.VideoMasterURL != "" {
		p.Kind = "video"
		p.VideoMasterURL = d.VideoMasterURL
		if d.VideoPosterURL != "" {
			p.VideoPosterURL = d.VideoPosterURL
		}
		p.VideoDurationSecs = d.VideoDurationSecs
	}
	canonical, err := canonicalBytes(p)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(canonical)
	cid = fmt.Sprintf("sha256:%x", sum)
	envelope := map[string]any{
		"event_type": p.Kind, "cid": cid, "signature": k.SignCID(cid), "is_nsfw": false,
	}
	if d.VideoMasterURL != "" {
		if d.VideoWatermarkedURL != "" {
			envelope["video_watermarked_url"] = d.VideoWatermarkedURL
		}
		if d.VideoWidth > 0 && d.VideoHeight > 0 {
			envelope["video_width"] = d.VideoWidth
			envelope["video_height"] = d.VideoHeight
		}
	}
	outer, _ := json.Marshal(envelope)
	outer = outer[:len(outer)-1]
	outer = append(outer, []byte(`,"payload":`)...)
	outer = append(outer, canonical...)
	outer = append(outer, '}')
	return outer, cid, nil
}
