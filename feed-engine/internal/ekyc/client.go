// Package ekyc is feed-engine's client for ekyc, the service that measures one
// identity-verification attempt and decides whether it passes.
//
// ekyc is stateless except for liveness challenge sessions. It reads the
// document (barcode or MRZ, cross-checked against the OCR'd front), checks
// expiry and age, matches the selfie against the document portrait, runs
// presentation-attack detection and verifies the frames answering a
// server-issued liveness challenge. Its answer is a decision and the measured
// facts; the raw document number never leaves it, only doc_hmac does.
//
// Fail closed: an unconfigured client, a transport failure, a non-2xx answer or
// an answer that does not decode is an error, and an error is never a pass.
package ekyc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrNotConfigured is every call's answer when EKYC_URL is unset.
var ErrNotConfigured = errors.New("ekyc: EKYC_URL is not configured")

// ErrBusy is ekyc's 503: every verification slot is in use. Nothing was
// decided; the person tries again shortly.
var ErrBusy = errors.New("ekyc: busy")

// maxAnswer bounds what is read from an answer. A verdict is a few kilobytes.
const maxAnswer = 1 << 20

// Purposes of a verification attempt. Both run the same checks; creator also
// obliges Verity to keep a copy of the ID for 18 U.S.C. § 2257.
const (
	PurposeAge     = "age"
	PurposeCreator = "creator"
)

// ValidPurpose reports whether p is one of the two purposes.
func ValidPurpose(p string) bool { return p == PurposeAge || p == PurposeCreator }

// Client calls ekyc.
type Client struct {
	base string
	key  string
	http *http.Client
}

// New returns a client. base may be empty: every call then fails with
// ErrNotConfigured.
func New(base, internalKey string) *Client {
	return &Client{
		base: strings.TrimRight(strings.TrimSpace(base), "/"),
		key:  internalKey,
		// The verify call runs OCR, barcode decoding, face embedding and PAD on
		// several images; it is given a minute.
		http: &http.Client{Timeout: 60 * time.Second},
	}
}

// Configured reports whether there is an ekyc to ask.
func (c *Client) Configured() bool { return c != nil && c.base != "" }

// Challenge is a server-issued liveness challenge session.
type Challenge struct {
	SessionID  string   `json:"session_id"`
	Challenges []string `json:"challenges"`
	ExpiresAt  string   `json:"expires_at"`
}

// NewChallenge opens a liveness challenge session.
func (c *Client) NewChallenge(ctx context.Context) (*Challenge, error) {
	var ch Challenge
	if err := c.post(ctx, "/v1/liveness/challenge", map[string]string{}, &ch); err != nil {
		return nil, err
	}
	if ch.SessionID == "" || len(ch.Challenges) == 0 {
		return nil, fmt.Errorf("ekyc: liveness challenge: answer carried no session or no challenges")
	}
	return &ch, nil
}

// VerifyRequest is one attempt's evidence. Images are base64 without a data:
// prefix. DocumentBack is empty for a passport.
type VerifyRequest struct {
	Purpose                 string   `json:"purpose"`
	DocumentFront           string   `json:"document_front"`
	DocumentBack            string   `json:"document_back"`
	FaceImage               string   `json:"face_image"`
	LivenessSessionID       string   `json:"liveness_session_id"`
	LivenessChallengeFrames []string `json:"liveness_challenge_frames"`
}

// DocumentChecks are the per-check results ekyc reports for the document.
type DocumentChecks struct {
	Barcode          string `json:"barcode"`       // ok | absent | invalid
	MRZChecksums     string `json:"mrz_checksums"` // ok | absent | invalid
	FieldsConsistent bool   `json:"fields_consistent"`
	Expired          bool   `json:"expired"`
	Tamper           string `json:"tamper"` // ok | suspect
}

// Document is what ekyc read from the document.
type Document struct {
	Type           string         `json:"type"` // passport | id_card | drivers_license
	IssuingCountry string         `json:"issuing_country"`
	IssuingRegion  string         `json:"issuing_region"`
	DocHMAC        string         `json:"doc_hmac"`
	GivenNames     string         `json:"given_names"`
	Surname        string         `json:"surname"`
	DOB            string         `json:"dob"`    // YYYY-MM-DD
	Expiry         string         `json:"expiry"` // YYYY-MM-DD
	Checks         DocumentChecks `json:"checks"`
}

// LegalName is the name on the document, given names first.
func (d Document) LegalName() string {
	return strings.TrimSpace(strings.TrimSpace(d.GivenNames) + " " + strings.TrimSpace(d.Surname))
}

// Face is the selfie-to-portrait match.
type Face struct {
	Match  bool    `json:"match"`
	Cosine float64 `json:"cosine"`
	Model  string  `json:"model"`
}

// PAD is presentation-attack detection on the selfie.
type PAD struct {
	Live  bool    `json:"live"`
	Score float64 `json:"score"`
	Model string  `json:"model"`
}

// Liveness is the server-verified answer to the challenge.
type Liveness struct {
	Passed bool    `json:"passed"`
	Score  float64 `json:"score"`
}

// Verdict is ekyc's decision on one attempt.
type Verdict struct {
	Decision      string   `json:"decision"` // pass | fail
	Reasons       []string `json:"reasons"`
	Document      Document `json:"document"`
	Age           int      `json:"age"`
	Face          Face     `json:"face"`
	PAD           PAD      `json:"pad"`
	Liveness      Liveness `json:"liveness"`
	EvidenceTypes []string `json:"evidence_types"`
}

// Passed reports whether the verdict is a pass that carries every fact a pass
// requires. A "pass" missing any of them is not believed: ekyc's word is
// necessary, not sufficient. The reasons a claimed pass is refused are
// returned so the attempt's record says why.
func (v *Verdict) Passed() (bool, []string) {
	if v == nil || v.Decision != "pass" {
		return false, nil
	}
	var why []string
	d := v.Document
	if d.DocHMAC == "" {
		why = append(why, "pass_without_doc_hmac")
	}
	if d.DOB == "" {
		why = append(why, "pass_without_dob")
	} else if _, err := time.Parse("2006-01-02", d.DOB); err != nil {
		why = append(why, "pass_with_unparseable_dob")
	}
	if d.Checks.Expired {
		why = append(why, "pass_with_expired_document")
	}
	if d.Checks.Barcode != "ok" && d.Checks.MRZChecksums != "ok" {
		why = append(why, "pass_without_machine_readable_source")
	}
	if v.Age < 18 {
		why = append(why, "pass_under_18")
	}
	if !v.Face.Match {
		why = append(why, "pass_without_face_match")
	}
	if !v.PAD.Live {
		why = append(why, "pass_without_pad_live")
	}
	if !v.Liveness.Passed {
		why = append(why, "pass_without_liveness")
	}
	return len(why) == 0, why
}

// Verify submits one attempt for a decision. The decision is the verdict's;
// an error means there is no decision at all.
func (c *Client) Verify(ctx context.Context, req VerifyRequest) (*Verdict, error) {
	var v Verdict
	if err := c.post(ctx, "/v1/verify", req, &v); err != nil {
		return nil, err
	}
	if v.Decision != "pass" && v.Decision != "fail" {
		return nil, fmt.Errorf("ekyc: verify: decision %q is neither pass nor fail", v.Decision)
	}
	return &v, nil
}

func (c *Client) post(ctx context.Context, path string, in, out interface{}) error {
	if !c.Configured() {
		return ErrNotConfigured
	}
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("ekyc: %s: encoding request: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("ekyc: %s: building request: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.key != "" {
		req.Header.Set("X-Internal-Key", c.key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("ekyc: %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return fmt.Errorf("ekyc: %s: reading answer: %w", path, err)
	}
	if resp.StatusCode == http.StatusServiceUnavailable {
		return fmt.Errorf("%w: %s: %s", ErrBusy, path, snippet(raw))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ekyc: %s: status %d: %s", path, resp.StatusCode, snippet(raw))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("ekyc: %s: undecodable answer: %w", path, err)
	}
	return nil
}

// snippet is the start of an answer body, for a log line.
func snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
