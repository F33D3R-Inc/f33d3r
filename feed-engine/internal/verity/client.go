// Package verity is feed-engine's client for Verity, the owner of identity
// EVIDENCE and LEGAL RECORDS: each verification attempt's submission and audit
// rows, the document-reuse index, 18 U.S.C. § 2257 records with the encrypted
// copy of a creator's ID, and their retention.
//
// Verity does not store or decide a verification tier; elohim-veni does. This
// client never reads a tier from Verity.
//
// Every call carries the internal key. Fail closed: an unconfigured client, a
// transport failure or an answer outside the documented ones is an error, and
// an error is never an acceptance.
package verity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrNotConfigured is every call's answer when VERITY_URL is unset.
var ErrNotConfigured = errors.New("verity: VERITY_URL is not configured")

// ErrNoCreatorPass is Verity's 409 on a 2257 record: the PIAL has no passing
// purpose=creator submission with an ID copy.
var ErrNoCreatorPass = errors.New("verity: no passing creator verification with an ID copy")

const maxAnswer = 1 << 20

// Client calls Verity.
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
		http: &http.Client{Timeout: 20 * time.Second},
	}
}

// Configured reports whether there is a Verity to ask.
func (c *Client) Configured() bool { return c != nil && c.base != "" && c.key != "" }

// Scores are the measured scores kept with the attempt. The face-match score is
// kept; no biometric template is.
type Scores struct {
	FaceCosine float64 `json:"face_cosine"`
	PAD        float64 `json:"pad"`
	Liveness   float64 `json:"liveness"`
}

// Consent is the person's consent to the collection, recorded with the attempt.
type Consent struct {
	Version    string `json:"version"`
	AcceptedAt string `json:"accepted_at"` // RFC 3339
	IP         string `json:"ip"`
	UserAgent  string `json:"user_agent"`
}

// IDCopy is the copy of the ID 2257 requires of a creator. Images are base64.
type IDCopy struct {
	Front    string `json:"front"`
	Back     string `json:"back,omitempty"`
	Portrait string `json:"portrait"`
}

// Submission is one attempt's evidence. Optional facts are omitted when
// empty: Verity refuses a malformed date even on a fail, and reads an absent
// one as absent.
type Submission struct {
	AttemptID      string   `json:"attempt_id"`
	PIALID         string   `json:"pial_id"`
	Purpose        string   `json:"purpose"`
	Decision       string   `json:"decision"`
	Reasons        []string `json:"reasons"`
	EvidenceTypes  []string `json:"evidence_types"`
	DocHMAC        string   `json:"doc_hmac,omitempty"`
	DocType        string   `json:"doc_type,omitempty"`
	IssuingCountry string   `json:"issuing_country,omitempty"`
	IssuingRegion  string   `json:"issuing_region,omitempty"`
	LegalName      string   `json:"legal_name,omitempty"`
	DOB            string   `json:"dob,omitempty"`
	DocExpiry      string   `json:"doc_expiry,omitempty"`
	Scores         Scores   `json:"scores"`
	Consent        Consent  `json:"consent"`
	// IDCopy is set only for a creator pass.
	IDCopy *IDCopy `json:"id_copy,omitempty"`
}

// SubmissionResult is Verity's answer to a submission.
type SubmissionResult int

const (
	// SubmissionAccepted — 201: a pass, recorded.
	SubmissionAccepted SubmissionResult = iota + 1
	// SubmissionRecordedFail — 200 {"accepted": false}: a fail, recorded.
	SubmissionRecordedFail
	// SubmissionDocumentInUse — 409 document_in_use: the document already
	// passed for a different PIAL.
	SubmissionDocumentInUse
)

// Submit records one attempt. Every answer other than the three documented
// ones is an error.
func (c *Client) Submit(ctx context.Context, s Submission) (SubmissionResult, error) {
	status, raw, err := c.do(ctx, http.MethodPost, "/v1/kyc/submissions", s)
	if err != nil {
		return 0, err
	}
	var ans struct {
		Accepted *bool  `json:"accepted"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &ans); err != nil {
		return 0, fmt.Errorf("verity: submission %s: status %d, undecodable answer: %w", s.AttemptID, status, err)
	}
	switch {
	case status == http.StatusCreated && ans.Accepted != nil && *ans.Accepted:
		return SubmissionAccepted, nil
	case status == http.StatusOK && ans.Accepted != nil && !*ans.Accepted:
		return SubmissionRecordedFail, nil
	case status == http.StatusConflict && ans.Reason == "document_in_use":
		return SubmissionDocumentInUse, nil
	}
	return 0, fmt.Errorf("verity: submission %s: unexpected answer %d: %s", s.AttemptID, status, snippet(raw))
}

// Eligible2257 reports whether Verity holds a passing purpose=creator
// verification with an ID copy for the PIAL: the only thing that lets an 18+
// work be published.
func (c *Client) Eligible2257(ctx context.Context, pialID string) (bool, error) {
	status, raw, err := c.do(ctx, http.MethodGet, "/v1/compliance/2257/"+url.PathEscape(pialID), nil)
	if err != nil {
		return false, err
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("verity: 2257 eligibility of %s: unexpected answer %d: %s", pialID, status, snippet(raw))
	}
	var ans struct {
		Eligible *bool `json:"eligible"`
	}
	if err := json.Unmarshal(raw, &ans); err != nil || ans.Eligible == nil {
		return false, fmt.Errorf("verity: 2257 eligibility of %s: undecodable answer: %v", pialID, err)
	}
	return *ans.Eligible, nil
}

// Record2257 links a published 18+ work to the creator's 2257 submission.
// ErrNoCreatorPass when Verity refuses because there is none.
func (c *Client) Record2257(ctx context.Context, pialID, contentID, recordType string) error {
	status, raw, err := c.do(ctx, http.MethodPost, "/v1/compliance/2257/record", map[string]string{
		"pial_id": pialID, "content_id": contentID, "record_type": recordType,
	})
	if err != nil {
		return err
	}
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusConflict:
		return fmt.Errorf("%w (content %s): %s", ErrNoCreatorPass, contentID, snippet(raw))
	}
	return fmt.Errorf("verity: 2257 record for content %s: unexpected answer %d: %s", contentID, status, snippet(raw))
}

// ContentRemoved2257 tells Verity a work with a 2257 record was removed: the
// retention of the creator's ID copy is measured from the last removal.
func (c *Client) ContentRemoved2257(ctx context.Context, contentID string, removedAt time.Time) error {
	status, raw, err := c.do(ctx, http.MethodPost, "/v1/compliance/2257/content-removed", map[string]string{
		"content_id": contentID, "removed_at": removedAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	if status >= 200 && status < 300 {
		return nil
	}
	return fmt.Errorf("verity: 2257 removal of content %s: unexpected answer %d: %s", contentID, status, snippet(raw))
}

// do sends one request and returns the status and body. Transport failures are
// errors; the status is the caller's to judge.
func (c *Client) do(ctx context.Context, method, path string, in interface{}) (int, []byte, error) {
	if !c.Configured() {
		return 0, nil, ErrNotConfigured
	}
	var body io.Reader
	if in != nil {
		payload, err := json.Marshal(in)
		if err != nil {
			return 0, nil, fmt.Errorf("verity: %s %s: encoding request: %w", method, path, err)
		}
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return 0, nil, fmt.Errorf("verity: %s %s: building request: %w", method, path, err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Internal-Key", c.key)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("verity: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return 0, nil, fmt.Errorf("verity: %s %s: reading answer: %w", method, path, err)
	}
	return resp.StatusCode, raw, nil
}

func snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
