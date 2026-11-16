// Package abraxas is feed-engine's client for Abraxas, the platform's one
// integrity authority.
//
// Abraxas judges content: every upload (asked by Caeor before an asset may
// become ready), every work and Vision (through content.events), every live
// frame (asked here, by the live lane), every report a person files, and the
// review queue a moderator works — ML review first, then admin review. Its
// verdicts come back to this brain on Sitra Achra topic safety.events
// (internal/sitra/safety.go), and that is the only way a verdict is applied
// here.
//
// This package carries the calls feed-engine makes itself: the live frame
// check, report filing and handling, and the review queue's list and decide.
// Every call presents the internal service key as X-Internal-Key. An
// unconfigured client fails every call with ErrNotConfigured: a safety call
// that silently does nothing is the failure this package exists to prevent.
//
// PIALs travel on this wire because Abraxas keys people by their identity
// root. They never travel further: the admin Facets that render these answers
// resolve every PIAL to a handle through the identity projection.
package abraxas

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const headerInternalKey = "X-Internal-Key"

// ErrNotConfigured means ABRAXAS_URL or INTERNAL_API_KEY is unset.
var ErrNotConfigured = errors.New("abraxas: ABRAXAS_URL and INTERNAL_API_KEY must both be configured")

// ErrNotFound means Abraxas has no such report or review item.
var ErrNotFound = errors.New("abraxas: no such item")

// ErrConflict means the item has already been decided or closed.
var ErrConflict = errors.New("abraxas: the item is already closed")

// Subject names what a report, a review or a verdict is about.
type Subject struct {
	// Kind is work | vision | asset | stream | account.
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// BannedMatch is a match against the banned registry.
type BannedMatch struct {
	Category string `json:"category"`
	// Layer is the registry layer that matched: sha256 | phash | chromaprint | ...
	Layer string `json:"layer"`
	// Match is exact (the very bytes) or near (a perceptual match).
	Match    string  `json:"match"`
	SHA256   string  `json:"sha256,omitempty"`
	BannedID string  `json:"banned_id,omitempty"`
	Score    float64 `json:"score,omitempty"`
}

// Ownership is a finding that content is (or derives from) somebody else's.
type Ownership struct {
	MatchID       string   `json:"match_id,omitempty"`
	OriginalPIAL  string   `json:"original_pial,omitempty"`
	OriginalAsset string   `json:"original_asset,omitempty"`
	Kind          string   `json:"kind,omitempty"`
	Confidence    float64  `json:"confidence,omitempty"`
	OffsetSeconds *float64 `json:"offset_seconds,omitempty"`
	// State is ml_review | credited | rejected.
	State string `json:"state,omitempty"`
}

// Client talks to Abraxas over HTTP.
type Client struct {
	baseURL string
	key     string
	calls   *http.Client
}

// New returns a client for the Abraxas at baseURL, authenticating with key.
func New(baseURL, key string) *Client {
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		key:     strings.TrimSpace(key),
		calls:   &http.Client{Timeout: 20 * time.Second},
	}
}

// Configured reports whether the client can make any call at all.
func (c *Client) Configured() bool { return c != nil && c.baseURL != "" && c.key != "" }

// do sends one JSON request and decodes the JSON answer into out (when out is
// not nil). A non-2xx answer is an error carrying a bounded excerpt.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	if !c.Configured() {
		return ErrNotConfigured
	}
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("abraxas: encoding %s %s: %w", method, path, err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("abraxas: building %s %s: %w", method, path, err)
	}
	req.Header.Set(headerInternalKey, c.key)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.calls.Do(req)
	if err != nil {
		return fmt.Errorf("abraxas: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		excerpt, rerr := io.ReadAll(io.LimitReader(resp.Body, 512))
		detail := strings.TrimSpace(string(excerpt))
		if rerr != nil {
			detail = "body unreadable: " + rerr.Error()
		}
		cause := fmt.Errorf("abraxas: %s %s: HTTP %d: %s", method, path, resp.StatusCode, detail)
		switch resp.StatusCode {
		case http.StatusNotFound:
			return fmt.Errorf("%w: %v", ErrNotFound, cause)
		case http.StatusConflict:
			return fmt.Errorf("%w: %v", ErrConflict, cause)
		}
		return cause
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("abraxas: %s %s: unreadable answer: %w", method, path, err)
	}
	return nil
}

// ── Live frames ──────────────────────────────────────────────────────────────

// FrameCheck is one sampled live keyframe put to Abraxas. Abraxas reads the
// frame itself from the live-scan volume both services mount, at
// <LIVE_SCAN_DIR>/<stream_id>/<frame_name>; the sha256 is this side's hash of
// the same file, and Abraxas refuses a frame that does not match it.
type FrameCheck struct {
	StreamID  string `json:"stream_id"`
	FrameName string `json:"frame_name"`
	// PIAL is the broadcaster's identity root, so a banned match is reported
	// against the person on safety.events.
	PIAL   string `json:"pial,omitempty"`
	SHA256 string `json:"sha256"`
	// IsAdultCreator is the broadcaster's standing, which the policy reads.
	IsAdultCreator bool `json:"is_adult_creator"`
}

// FrameVerdict is Abraxas's answer for one frame.
type FrameVerdict struct {
	// Verdict is allow | age_gate | review | block.
	Verdict     string             `json:"verdict"`
	BannedMatch *BannedMatch       `json:"banned_match,omitempty"`
	Signals     []string           `json:"signals,omitempty"`
	RiskLevel   string             `json:"risk_level,omitempty"`
	Scores      map[string]float64 `json:"scores,omitempty"`
	SHA256      string             `json:"sha256,omitempty"`
}

// CheckFrame asks Abraxas about one live frame. Any failure to get a readable
// verdict is an error: the caller holds the frame unjudged, never clean.
func (c *Client) CheckFrame(ctx context.Context, f FrameCheck) (*FrameVerdict, error) {
	var v FrameVerdict
	if err := c.do(ctx, http.MethodPost, "/v1/check/frame", f, &v); err != nil {
		return nil, err
	}
	switch v.Verdict {
	case "allow", "age_gate", "review":
	case "block":
		if v.BannedMatch != nil && v.BannedMatch.Category == "" {
			return nil, fmt.Errorf("abraxas: frame %s: banned match without a category", f.FrameName)
		}
	default:
		return nil, fmt.Errorf("abraxas: frame %s: unknown verdict %q", f.FrameName, v.Verdict)
	}
	return &v, nil
}

// ── Reports ──────────────────────────────────────────────────────────────────

// Report kinds.
const (
	ReportKindContent        = "content"
	ReportKindOwnershipClaim = "ownership_claim"
)

// NewReport is one report a person files.
type NewReport struct {
	ReporterPIAL string  `json:"reporter_pial"`
	Subject      Subject `json:"subject"`
	// Kind is content | ownership_claim. An ownership claim goes straight to
	// admin review, with the reporter's own matching assets attached when
	// Abraxas finds them.
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// FiledReport is Abraxas's answer to a filed report.
type FiledReport struct {
	ReportID string `json:"report_id"`
	Stage    string `json:"stage,omitempty"`
	// Created is false when the filing joined a case already open.
	Created bool `json:"created"`
}

// CreateReport files a report.
func (c *Client) CreateReport(ctx context.Context, r NewReport) (*FiledReport, error) {
	var out FiledReport
	if err := c.do(ctx, http.MethodPost, "/v1/reports", r, &out); err != nil {
		return nil, err
	}
	if out.ReportID == "" {
		return nil, errors.New("abraxas: report filed but no report_id came back")
	}
	return &out, nil
}

// MatchedAsset is one of a claimant's own assets that matches the reported
// content.
type MatchedAsset struct {
	AssetID    string  `json:"asset_id"`
	PIAL       string  `json:"pial"`
	Kind       string  `json:"kind,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
}

// Report is one open (or recently closed) report as Abraxas holds it.
type Report struct {
	ReportID      string   `json:"report_id"`
	Subject       Subject  `json:"subject"`
	Kind          string   `json:"kind"`
	Reason        string   `json:"reason"`
	Detail        string   `json:"detail,omitempty"`
	Status        string   `json:"status"`
	Stage         string   `json:"stage,omitempty"`
	ReporterPIALs []string `json:"reporter_pials,omitempty"`
	// ReportedPIAL is the subject's owner, when Abraxas knows it.
	ReportedPIAL  string         `json:"reported_pial,omitempty"`
	ReportCount   int            `json:"report_count,omitempty"`
	MatchedAssets []MatchedAsset `json:"matched_assets,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
}

// ListReports lists reports in a status (open | resolved | dismissed).
func (c *Client) ListReports(ctx context.Context, status string, limit int) ([]Report, error) {
	q := url.Values{}
	q.Set("status", status)
	q.Set("limit", strconv.Itoa(limit))
	var out struct {
		Reports []Report `json:"reports"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/reports?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.Reports, nil
}

// Report resolution actions.
const (
	ReportActionNone = "none"
	ReportActionBan  = "ban"
)

// GetReport reads one report.
func (c *Client) GetReport(ctx context.Context, reportID string) (*Report, error) {
	var r Report
	if err := c.do(ctx, http.MethodGet, "/v1/reports/"+url.PathEscape(reportID), nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ReportAction is a moderator closing or escalating a report.
type ReportAction struct {
	ActorPIAL string `json:"actor_pial"`
	// Status is resolved | dismissed, for resolve.
	Status string `json:"status,omitempty"`
	Note   string `json:"note,omitempty"`
	// Action is none | ban, for resolve. A ban is handed to elohim-veni by
	// Abraxas in the same transaction as the resolution.
	Action string `json:"action,omitempty"`
	// BanPIAL is whom a ban falls on.
	BanPIAL string `json:"ban_pial,omitempty"`
}

// ResolveReport closes a report as resolved or dismissed.
func (c *Client) ResolveReport(ctx context.Context, reportID string, a ReportAction) error {
	return c.do(ctx, http.MethodPost, "/v1/reports/"+url.PathEscape(reportID)+"/resolve", a, nil)
}

// EscalateReport moves a report to admin review.
func (c *Client) EscalateReport(ctx context.Context, reportID string, a ReportAction) error {
	return c.do(ctx, http.MethodPost, "/v1/reports/"+url.PathEscape(reportID)+"/escalate", a, nil)
}

// ── The review queue ─────────────────────────────────────────────────────────

// Review stages.
const (
	StageMLReview    = "ml_review"
	StageAdminReview = "admin_review"
)

// Review decisions.
const (
	DecisionApprove = "approve"
	DecisionAgeGate = "age_gate"
	DecisionBlock   = "block"
	DecisionCredit  = "credit"
	DecisionReject  = "reject"
)

// DecisionsFor are the decisions a review item of a kind takes; any other is
// refused by Abraxas.
func DecisionsFor(itemKind string) []string {
	switch itemKind {
	case "content", "unjudged":
		return []string{DecisionApprove, DecisionAgeGate, DecisionBlock}
	case "banned_near", "report":
		return []string{DecisionApprove, DecisionBlock}
	case "ownership", "ownership_claim":
		return []string{DecisionCredit, DecisionReject}
	}
	return nil
}

// ReviewItem is one item waiting for a decision.
type ReviewItem struct {
	ReviewID string `json:"review_id"`
	// ItemKind is content | unjudged | banned_near | report | ownership |
	// ownership_claim; it decides which decisions the item takes.
	ItemKind  string             `json:"item_kind"`
	Stage     string             `json:"stage"`
	Subject   Subject            `json:"subject"`
	AssetID   string             `json:"asset_id,omitempty"`
	PIAL      string             `json:"pial,omitempty"`
	Reason    string             `json:"reason,omitempty"`
	Scores    map[string]float64 `json:"scores,omitempty"`
	Signals   []string           `json:"signals,omitempty"`
	Ownership *Ownership         `json:"ownership,omitempty"`
	ReportID  string             `json:"report_id,omitempty"`
	// Category is the banned category of a banned_near item.
	Category string `json:"category,omitempty"`
	// Evidence and MLOutcome are Abraxas's records of why the item exists
	// and what the ML stage made of it. They can name PIALs: read named
	// fields out of them, never render them whole.
	Evidence       json.RawMessage `json:"evidence,omitempty"`
	MLOutcome      json.RawMessage `json:"ml_outcome,omitempty"`
	CurrentVerdict string          `json:"current_verdict,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

// ListReview lists the items waiting at one stage.
func (c *Client) ListReview(ctx context.Context, stage string, limit int) ([]ReviewItem, error) {
	q := url.Values{}
	q.Set("stage", stage)
	q.Set("limit", strconv.Itoa(limit))
	var out struct {
		Items []ReviewItem `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/review?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// Decision is a moderator's decision on one review item.
type Decision struct {
	// Decision is approve | block | credit | reject.
	Decision  string `json:"decision"`
	ActorPIAL string `json:"actor_pial"`
	Note      string `json:"note,omitempty"`
}

// ── The banned registry ──────────────────────────────────────────────────────

// BannedHash is one value added to the banned registry.
type BannedHash struct {
	// HashType is sha256 | phash | chromaprint | audio_fp.
	HashType  string `json:"hash_type"`
	HashValue string `json:"hash_value"`
	Category  string `json:"category"`
	Note      string `json:"note,omitempty"`
	// AddedBy is the admin's PIAL.
	AddedBy string `json:"added_by"`
}

// AddBanned registers a value in the banned registry. Registering a value
// already there is not an error.
func (c *Client) AddBanned(ctx context.Context, b BannedHash) error {
	return c.do(ctx, http.MethodPost, "/v1/banned", b, nil)
}

// BanFromAsset bans an asset's material on every layer its fingerprint has,
// and blocks the asset.
func (c *Client) BanFromAsset(ctx context.Context, assetID, category, note, addedBy string) error {
	return c.do(ctx, http.MethodPost, "/v1/banned/from-asset", map[string]string{
		"asset_id": assetID, "category": category, "note": note, "added_by": addedBy,
	}, nil)
}

// RevokeBanned takes a value out of the banned registry.
func (c *Client) RevokeBanned(ctx context.Context, hashType, hashValue, revokedBy, reason string) error {
	return c.do(ctx, http.MethodPost, "/v1/banned/revoke", map[string]string{
		"hash_type": hashType, "hash_value": hashValue, "revoked_by": revokedBy, "reason": reason,
	}, nil)
}

// Decide records a decision. Abraxas carries it out and publishes
// review_decided on safety.events, which is how it reaches the content.
func (c *Client) Decide(ctx context.Context, reviewID string, d Decision) error {
	switch d.Decision {
	case DecisionApprove, DecisionAgeGate, DecisionBlock, DecisionCredit, DecisionReject:
	default:
		return fmt.Errorf("abraxas: unknown decision %q", d.Decision)
	}
	return c.do(ctx, http.MethodPost, "/v1/review/"+url.PathEscape(reviewID)+"/decide", d, nil)
}
