// Package auralis is Nantar's client for the Frequencies brain.
//
// Auralis owns the Frequency: its lifecycle, who is in it and in what role,
// the speaker queue, moderation, the events every other brain hears. Nantar
// owns none of that. It authenticates the browser, resolves the PIAL, asks
// Auralis, and renders what Auralis answers. Nothing here caches a Frequency,
// and nothing here decides a rule — a refusal Auralis gives is the answer.
//
// Every call carries X-Internal-Key (the shared brain secret). A call made for
// a person carries X-Pial-Identity with their bare PIAL uuid; Auralis resolves
// it through Manhattan before it writes anything. Mutations may carry an
// Idempotency-Key so a retried request is one effect.
//
// The http.Client is wrapped by observ.WrapClient so X-Request-ID travels
// from the browser's request through this brain into Auralis's logs.
package auralis

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

	"github.com/f33d3r/feed-engine/internal/observ"
)

// headerInternalKey is the brain-to-brain shared secret header, the same one
// every internal endpoint in the estate checks.
const headerInternalKey = "X-Internal-Key"

// ErrNotConfigured is returned by every call when AURALIS_URL is empty.
var ErrNotConfigured = errors.New("auralis: AURALIS_URL is not set")

// StatusError is an answer FROM Auralis that refused the request: a 4xx with
// the machine code and message Auralis put in the body. It is a fact about
// this request, not about the deployment — the caller renders it.
type StatusError struct {
	Method  string
	Path    string
	Status  int
	Code    string
	Message string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("auralis: %s %s: http %d %s: %s", e.Method, e.Path, e.Status, e.Code, e.Message)
}

// transportError wraps a failure to get any usable answer: a refused
// connection, a timeout, a body that would not decode.
type transportError struct {
	Method string
	Path   string
	Err    error
}

func (e *transportError) Error() string {
	return fmt.Sprintf("auralis: %s %s: %v", e.Method, e.Path, e.Err)
}

func (e *transportError) Unwrap() error { return e.Err }

// Code returns the machine code of a refusal, or "" for anything else.
func Code(err error) string {
	var s *StatusError
	if errors.As(err, &s) {
		return s.Code
	}
	return ""
}

func statusIs(err error, status int) bool {
	var s *StatusError
	return errors.As(err, &s) && s.Status == status
}

// IsConflict: 409 — the Frequency's state refused this (not_live, locked,
// full, blocked, requests_closed, speakers_full, host_already_live, ...).
func IsConflict(err error) bool { return statusIs(err, http.StatusConflict) }

// IsForbidden: 403 — the person's role does not permit it.
func IsForbidden(err error) bool { return statusIs(err, http.StatusForbidden) }

// IsNotFound: 404.
func IsNotFound(err error) bool { return statusIs(err, http.StatusNotFound) }

// IsRateLimited: 429.
func IsRateLimited(err error) bool { return statusIs(err, http.StatusTooManyRequests) }

// IsBadRequest: 400 — a field Auralis would not accept.
func IsBadRequest(err error) bool { return statusIs(err, http.StatusBadRequest) }

// IsUnavailable: Auralis answered 503 (its naming plane or Redis is down).
func IsUnavailable(err error) bool { return statusIs(err, http.StatusServiceUnavailable) }

// IsOutage reports whether err says nothing about the request: the brain
// could not be reached, it faulted, it is not configured, or it rejected this
// brain's key. Rendered to a person as "Frequencies are unavailable", never
// as a refusal of what they asked.
func IsOutage(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNotConfigured) {
		return true
	}
	var s *StatusError
	if errors.As(err, &s) {
		return s.Status >= 500 || s.Status == http.StatusUnauthorized
	}
	var t *transportError
	return errors.As(err, &t)
}

// ── Wire types — mirror Auralis's JSON exactly ───────────────────────────────

// Summary is the durable Frequency row as Auralis serves it. Identities are
// bare PIAL uuids; Nantar resolves them to people before anything renders.
type Summary struct {
	ID                   string     `json:"id"`
	Version              int64      `json:"version"`
	HostPialID           string     `json:"host_pial_id"`
	Title                string     `json:"title"`
	Description          string     `json:"description"`
	State                string     `json:"state"`
	Visibility           string     `json:"visibility"`
	Language             string     `json:"language"`
	AdultContent         bool       `json:"adult_content"`
	SpeakerVerityMinTier int        `json:"speaker_verity_min_tier"`
	ScheduledAt          *time.Time `json:"scheduled_at"`
	StartedAt            *time.Time `json:"started_at"`
	EndedAt              *time.Time `json:"ended_at"`
	EndReason            *string    `json:"end_reason"`
	RecordingEnabled     bool       `json:"recording_enabled"`
	ReplayStatus         string     `json:"replay_status"`
	MaxSpeakers          int        `json:"max_speakers"`
	MaxListeners         int        `json:"max_listeners"`
	RequestsOpen         bool       `json:"requests_open"`
	Locked               bool       `json:"locked"`
	MediaNode            *string    `json:"media_node"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

// IsLive reports whether listeners may Tune In.
func (s *Summary) IsLive() bool { return s != nil && s.State == "live" }

// IsOver reports a Frequency that has ended one way or another.
func (s *Summary) IsOver() bool {
	if s == nil {
		return false
	}
	switch s.State {
	case "ended", "processing_replay", "archived", "cancelled", "failed", "moderation_terminated":
		return true
	}
	return false
}

// Counts: listeners and speakers exclude the host; participants is everyone
// present including the host.
type Counts struct {
	Listeners    int `json:"listeners"`
	Speakers     int `json:"speakers"`
	Participants int `json:"participants"`
}

type Speaker struct {
	PialID   string    `json:"pial_id"`
	Role     string    `json:"role"`
	Muted    bool      `json:"muted"`
	Present  bool      `json:"present"`
	JoinedAt time.Time `json:"joined_at"`
}

type Request struct {
	ID        string    `json:"id"`
	PialID    string    `json:"pial_id"`
	Reason    string    `json:"reason"`
	Upvotes   int       `json:"upvotes"`
	CreatedAt time.Time `json:"created_at"`
}

// Viewer is the calling person's own standing, present when the call named
// one. Role is nil when they are not in the Frequency.
type Viewer struct {
	PialID        string   `json:"pial_id"`
	Role          *string  `json:"role"`
	Muted         bool     `json:"muted"`
	Present       bool     `json:"present"`
	Blocked       bool     `json:"blocked"`
	Request       *Request `json:"request"`
	CanRequestMic bool     `json:"can_request_mic"`
	CanModerate   bool     `json:"can_moderate"`
	CanEnd        bool     `json:"can_end"`
	CanSpeak      bool     `json:"can_speak"`
	CanListen     bool     `json:"can_listen"`
}

// View is the authoritative read model of one Frequency.
type View struct {
	Frequency      Summary   `json:"frequency"`
	Counts         Counts    `json:"counts"`
	Speakers       []Speaker `json:"speakers"`
	CohostPialIDs  []string  `json:"cohost_pial_ids"`
	Requests       []Request `json:"requests"`
	PresentPialIDs []string  `json:"present_pial_ids"`
	LeaseNode      *string   `json:"lease_node"`
	Viewer         *Viewer   `json:"viewer"`
}

// Session is the short-lived media bootstrap a browser gets on Tune In and
// Start. The token is the only credential that crosses the browser boundary.
type Session struct {
	SessionID   string   `json:"session_id"`
	Token       string   `json:"token"`
	ExpiresAt   int64    `json:"expires_at"`
	SignalPath  string   `json:"signal_path"`
	Role        string   `json:"role"`
	Permissions []string `json:"permissions"`
}

// Answer is what every Frequency mutation and read returns.
type Answer struct {
	Frequency View     `json:"frequency"`
	Session   *Session `json:"session"`
	Rejoined  bool     `json:"rejoined"`
}

type ListItem struct {
	Frequency Summary `json:"frequency"`
	Counts    Counts  `json:"counts"`
}

type ListAnswer struct {
	Lane  string     `json:"lane"`
	Items []ListItem `json:"items"`
}

type RequestAnswer struct {
	Request Request `json:"request"`
	Created bool    `json:"created"`
}

type HostOpenAnswer struct {
	Frequency *Summary `json:"frequency"`
}

type HeartbeatAnswer struct {
	State   string  `json:"state"`
	Present bool    `json:"present"`
	Role    string  `json:"role"`
	Muted   bool    `json:"muted"`
	Counts  *Counts `json:"counts"`
}

// Event is one row of the Frequency's event log, in the taxonomy envelope.
type Event struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	SchemaVersion string          `json:"schema_version"`
	PialID        string          `json:"pial_id"`
	Timestamp     time.Time       `json:"timestamp"`
	FrequencyID   string          `json:"frequency_id"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// CreateInput is what Start Frequency collects.
type CreateInput struct {
	Title                string     `json:"title"`
	Description          string     `json:"description,omitempty"`
	Visibility           string     `json:"visibility,omitempty"`
	Language             string     `json:"language,omitempty"`
	AdultContent         bool       `json:"adult_content"`
	RecordingEnabled     bool       `json:"recording_enabled"`
	ScheduledAt          *time.Time `json:"scheduled_at,omitempty"`
	MaxSpeakers          *int       `json:"max_speakers,omitempty"`
	MaxListeners         *int       `json:"max_listeners,omitempty"`
	SpeakerVerityMinTier *int       `json:"speaker_verity_min_tier,omitempty"`
}

// UpdateInput: nil fields are left alone.
type UpdateInput struct {
	Title                *string `json:"title,omitempty"`
	Description          *string `json:"description,omitempty"`
	Visibility           *string `json:"visibility,omitempty"`
	Language             *string `json:"language,omitempty"`
	AdultContent         *bool   `json:"adult_content,omitempty"`
	RecordingEnabled     *bool   `json:"recording_enabled,omitempty"`
	MaxSpeakers          *int    `json:"max_speakers,omitempty"`
	MaxListeners         *int    `json:"max_listeners,omitempty"`
	SpeakerVerityMinTier *int    `json:"speaker_verity_min_tier,omitempty"`
}

// ── Client ───────────────────────────────────────────────────────────────────

type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// New returns a client for the Auralis brain at baseURL. An empty baseURL
// yields a client whose every call returns ErrNotConfigured, so a deployment
// without the brain fails loudly at the call site rather than pretending.
func New(baseURL, internalKey string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  internalKey,
		http:    observ.WrapClient(&http.Client{Timeout: timeout}),
	}
}

// Configured reports whether this client can reach a Frequencies brain at all.
func (c *Client) Configured() bool { return c != nil && c.baseURL != "" }

// call performs one request. pial is the bare PIAL uuid of the person acted
// for, or "" for a service read. idem is an Idempotency-Key, or "".
func (c *Client) call(ctx context.Context, method, path, pial, idem string, body, out interface{}) error {
	if !c.Configured() {
		return ErrNotConfigured
	}
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return &transportError{Method: method, Path: path, Err: err}
		}
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, payload)
	if err != nil {
		return &transportError{Method: method, Path: path, Err: err}
	}
	req.Header.Set(headerInternalKey, c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if pial != "" {
		req.Header.Set(observ.HeaderPialIdentity, pial)
	}
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return &transportError{Method: method, Path: path, Err: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return &transportError{Method: method, Path: path, Err: err}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var wire struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &wire)
		if wire.Message == "" {
			wire.Message = strings.TrimSpace(string(raw))
		}
		return &StatusError{Method: method, Path: path, Status: resp.StatusCode, Code: wire.Error, Message: wire.Message}
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &transportError{Method: method, Path: path, Err: fmt.Errorf("decoding answer: %w", err)}
	}
	return nil
}

func seg(s string) string { return url.PathEscape(s) }

// ── Reads ────────────────────────────────────────────────────────────────────

// Get returns the read model. viewerPIAL may be "" for an anonymous read.
func (c *Client) Get(ctx context.Context, id, viewerPIAL string) (*Answer, error) {
	var out Answer
	if err := c.call(ctx, http.MethodGet, "/v1/frequencies/"+seg(id), viewerPIAL, "", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// List: lane is live | scheduled | ended | mine.
func (c *Client) List(ctx context.Context, lane, viewerPIAL string, limit int) (*ListAnswer, error) {
	var out ListAnswer
	path := fmt.Sprintf("/v1/frequencies?lane=%s&limit=%d", url.QueryEscape(lane), limit)
	if err := c.call(ctx, http.MethodGet, path, viewerPIAL, "", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Events(ctx context.Context, id string) ([]Event, error) {
	var out struct {
		Events []Event `json:"events"`
	}
	if err := c.call(ctx, http.MethodGet, "/v1/frequencies/"+seg(id)+"/events", "", "", nil, &out); err != nil {
		return nil, err
	}
	return out.Events, nil
}

// Current returns the live Frequency this person is joined to, rendered for
// them (viewer-specific), or nil. The Shell asks this once per full page so
// the dock renders on load and survives navigation.
func (c *Client) Current(ctx context.Context, pial string) (*View, error) {
	var out struct {
		Frequency *View `json:"frequency"`
	}
	if err := c.call(ctx, http.MethodGet, "/v1/participants/"+seg(pial)+"/current", "", "", nil, &out); err != nil {
		return nil, err
	}
	return out.Frequency, nil
}

// HostOpen returns the host's open Frequency, or nil.
func (c *Client) HostOpen(ctx context.Context, hostPIAL string) (*Summary, error) {
	var out HostOpenAnswer
	if err := c.call(ctx, http.MethodGet, "/v1/hosts/"+seg(hostPIAL)+"/open", "", "", nil, &out); err != nil {
		return nil, err
	}
	return out.Frequency, nil
}

// ── Host mutations ───────────────────────────────────────────────────────────

func (c *Client) Create(ctx context.Context, pial, idem string, in CreateInput) (*Answer, error) {
	var out Answer
	if err := c.call(ctx, http.MethodPost, "/v1/frequencies", pial, idem, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Update(ctx context.Context, pial, id string, in UpdateInput) (*Answer, error) {
	return c.answer(ctx, pial, "", "/v1/frequencies/"+seg(id)+"/update", in)
}

func (c *Client) Schedule(ctx context.Context, pial, id string, at *time.Time) (*Answer, error) {
	return c.answer(ctx, pial, "", "/v1/frequencies/"+seg(id)+"/schedule", map[string]interface{}{"scheduled_at": at})
}

func (c *Client) Start(ctx context.Context, pial, idem, id string) (*Answer, error) {
	return c.answer(ctx, pial, idem, "/v1/frequencies/"+seg(id)+"/start", nil)
}

func (c *Client) End(ctx context.Context, pial, idem, id string) (*Answer, error) {
	return c.answer(ctx, pial, idem, "/v1/frequencies/"+seg(id)+"/end", nil)
}

func (c *Client) Cancel(ctx context.Context, pial, id string) (*Answer, error) {
	return c.answer(ctx, pial, "", "/v1/frequencies/"+seg(id)+"/cancel", nil)
}

func (c *Client) Lock(ctx context.Context, pial, id string, locked bool) (*Answer, error) {
	return c.answer(ctx, pial, "", "/v1/frequencies/"+seg(id)+"/lock", map[string]bool{"locked": locked})
}

func (c *Client) RequestsOpen(ctx context.Context, pial, id string, open bool) (*Answer, error) {
	return c.answer(ctx, pial, "", "/v1/frequencies/"+seg(id)+"/requests_open", map[string]bool{"open": open})
}

// ── Participant mutations ────────────────────────────────────────────────────

func (c *Client) TuneIn(ctx context.Context, pial, idem, id string) (*Answer, error) {
	return c.answer(ctx, pial, idem, "/v1/frequencies/"+seg(id)+"/tune_in", nil)
}

func (c *Client) Leave(ctx context.Context, pial, id string) (*Answer, error) {
	return c.answer(ctx, pial, "", "/v1/frequencies/"+seg(id)+"/leave", nil)
}

func (c *Client) Heartbeat(ctx context.Context, pial, id string) (*HeartbeatAnswer, error) {
	var out HeartbeatAnswer
	if err := c.call(ctx, http.MethodPost, "/v1/frequencies/"+seg(id)+"/heartbeat", pial, "", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RequestMic: reason is the one line the host reads; verityTier is what
// Nantar read off the person's PIAL state, never a browser claim.
func (c *Client) RequestMic(ctx context.Context, pial, id, reason string, verityTier int) (*RequestAnswer, error) {
	var out RequestAnswer
	body := map[string]interface{}{"reason": reason, "verity_tier": verityTier}
	if err := c.call(ctx, http.MethodPost, "/v1/frequencies/"+seg(id)+"/request_mic", pial, "", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Withdraw(ctx context.Context, pial, id, requestID string) error {
	return c.call(ctx, http.MethodPost, "/v1/frequencies/"+seg(id)+"/requests/"+seg(requestID)+"/withdraw", pial, "", nil, nil)
}

func (c *Client) Upvote(ctx context.Context, pial, id, requestID string) (int, error) {
	var out struct {
		Counted bool `json:"counted"`
		Upvotes int  `json:"upvotes"`
	}
	if err := c.call(ctx, http.MethodPost, "/v1/frequencies/"+seg(id)+"/requests/"+seg(requestID)+"/upvote", pial, "", nil, &out); err != nil {
		return 0, err
	}
	return out.Upvotes, nil
}

// ── Moderation ───────────────────────────────────────────────────────────────

func (c *Client) Approve(ctx context.Context, pial, id, requestID string) (*Answer, error) {
	return c.answer(ctx, pial, "", "/v1/frequencies/"+seg(id)+"/requests/"+seg(requestID)+"/approve", nil)
}

func (c *Client) Decline(ctx context.Context, pial, id, requestID string) (*Answer, error) {
	return c.answer(ctx, pial, "", "/v1/frequencies/"+seg(id)+"/requests/"+seg(requestID)+"/decline", nil)
}

func (c *Client) Mute(ctx context.Context, pial, id, targetPIAL string) (*Answer, error) {
	return c.participant(ctx, pial, id, targetPIAL, "mute", nil)
}

func (c *Client) Unmute(ctx context.Context, pial, id, targetPIAL string) (*Answer, error) {
	return c.participant(ctx, pial, id, targetPIAL, "unmute", nil)
}

func (c *Client) Demote(ctx context.Context, pial, id, targetPIAL string) (*Answer, error) {
	return c.participant(ctx, pial, id, targetPIAL, "demote", nil)
}

func (c *Client) Remove(ctx context.Context, pial, id, targetPIAL, reason string) (*Answer, error) {
	return c.participant(ctx, pial, id, targetPIAL, "remove", map[string]string{"reason": reason})
}

func (c *Client) Block(ctx context.Context, pial, id, targetPIAL, reason string) (*Answer, error) {
	return c.participant(ctx, pial, id, targetPIAL, "block", map[string]string{"reason": reason})
}

func (c *Client) Unblock(ctx context.Context, pial, id, targetPIAL string) (*Answer, error) {
	return c.participant(ctx, pial, id, targetPIAL, "unblock", nil)
}

func (c *Client) AddCohost(ctx context.Context, pial, id, targetPIAL string) (*Answer, error) {
	return c.participant(ctx, pial, id, targetPIAL, "cohost", nil)
}

func (c *Client) RemoveCohost(ctx context.Context, pial, id, targetPIAL string) (*Answer, error) {
	return c.participant(ctx, pial, id, targetPIAL, "uncohost", nil)
}

// Terminate is the platform's action (§23): service key only, no person.
func (c *Client) Terminate(ctx context.Context, id, reason string) (*Answer, error) {
	return c.answer(ctx, "", "", "/internal/v1/frequencies/"+seg(id)+"/terminate", map[string]string{"reason": reason})
}

func (c *Client) participant(ctx context.Context, pial, id, target, action string, body interface{}) (*Answer, error) {
	return c.answer(ctx, pial, "", "/v1/frequencies/"+seg(id)+"/participants/"+seg(target)+"/"+action, body)
}

func (c *Client) answer(ctx context.Context, pial, idem, path string, body interface{}) (*Answer, error) {
	var out Answer
	if err := c.call(ctx, http.MethodPost, path, pial, idem, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
