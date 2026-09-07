// Package zior is the client for the Zior audio brain (zior-engine, Rust).
//
// Zior listens to an audio file and places it on the same eight Jung axes
// that internal/jung computes for text (the axis order is a shared contract:
// zior-engine/src/jung/axes.rs == internal/jung/vector.go). For a music track
// or a voice work the audio is the work, so Zior's vector is authoritative
// and replaces the text mapping; the text mapping remains the immediate
// fallback so no audio work is ever vector-less while Zior is still
// listening or unreachable.
//
// Every call here is made from a detached goroutine at a publish seam with
// its own deadline. Nothing in this package blocks a request.
package zior

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"
	"strings"
	"time"

	"github.com/f33d3r/feed-engine/internal/jung"
)

const (
	// UploadTimeout bounds one /upload round trip: Zior decodes, extracts
	// features and fingerprints the whole file before answering.
	UploadTimeout = 8 * time.Second
	// FastTimeout bounds /signal and /events, which are store lookups.
	FastTimeout = 2 * time.Second
	// maxResponseBytes caps what is read back from Zior so a misbehaving peer
	// cannot hold a goroutine on an unbounded body.
	maxResponseBytes = 1 << 20
)

// ErrUnprocessable is returned when Zior accepted the request but could not
// analyse the audio (undecodable, or shorter than its minimum duration). It
// is a property of the file, not a transient fault, so callers keep the text
// mapping and do not retry.
var ErrUnprocessable = errors.New("zior: audio not analysable")

// ErrTooLarge is returned when the file exceeds Zior's configured upload
// limit. Like ErrUnprocessable it is a property of the file.
var ErrTooLarge = errors.New("zior: audio exceeds upload limit")

// ErrNotFound is returned by Signal when Zior holds no signal for the id.
var ErrNotFound = errors.New("zior: no signal for track")

type Client struct {
	baseURL string
	upload  *http.Client
	fast    *http.Client
}

// NewClient builds a client for the Zior base URL. An empty URL yields a
// client whose Enabled reports false and whose calls return an error
// immediately, so a deployment without Zior degrades to the text mapper
// with one log line per attempt and no network traffic.
func NewClient(baseURL string) *Client {
	transport := &http.Transport{
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		upload:  &http.Client{Timeout: UploadTimeout, Transport: transport},
		fast:    &http.Client{Timeout: FastTimeout, Transport: transport},
	}
}

// Enabled reports whether a Zior URL is configured.
func (c *Client) Enabled() bool {
	return c != nil && c.baseURL != ""
}

// Analysis is what Zior says about one piece of audio.
type Analysis struct {
	// TrackID is the key Zior stored the signal under — the id the caller
	// supplied, so /signal/{id} and /similar/{id} are addressable by it.
	TrackID string
	// Vector is the audio's position on the Jung axes, normalised the way
	// internal/jung stores every vector (unit length, every coordinate
	// strictly positive).
	Vector jung.Vector
	// Mood is Zior's one-word affect label (euphoric, melancholic, ...).
	Mood string
	// ContextTags are the listening contexts Zior infers (workout, late
	// night, ...).
	ContextTags []string
	// Descriptor is Zior's genre-free display string ("128 BPM · A minor ·
	// intense · ...").
	Descriptor string
	// BPM and Key are the tempo and key Zior heard.
	BPM float32
	Key string
	// ProcessingMs is how long Zior spent on the file.
	ProcessingMs uint64
}

// uploadResponse mirrors zior-engine/src/api/types.rs UploadResponse.
type uploadResponse struct {
	TrackID       string     `json:"track_id"`
	Fingerprint   string     `json:"fingerprint"`
	Status        string     `json:"status"`
	ProcessingMs  uint64     `json:"processing_ms"`
	TopicVector   []float32  `json:"topic_vector"`
	SignalPreview signalPeek `json:"signal_preview"`
}

type signalPeek struct {
	BPM           float32  `json:"bpm"`
	Key           string   `json:"key"`
	Mood          string   `json:"mood"`
	Descriptor    string   `json:"descriptor"`
	VelocityScore float64  `json:"velocity_score"`
	ContextTags   []string `json:"context_tags"`
}

// Analyze sends one audio file to Zior as multipart/form-data and returns
// its analysis. The request shape is Zior's /upload contract:
//
//	POST {ZIOR_URL}/upload
//	Content-Type: multipart/form-data
//	  track_id   — the caller's id for the audio (feed-engine's work id or
//	               track id), which Zior stores the signal under
//	  creator_id — the author's id, carried into Zior's signal
//	  audio      — the file, with its filename so Zior reads the extension
//
// trackID must be non-empty: Zior would otherwise mint its own id and the
// signal would be unaddressable from here.
func (c *Client) Analyze(ctx context.Context, trackID, creatorID, filename string, audio io.Reader) (*Analysis, error) {
	if !c.Enabled() {
		return nil, errors.New("zior: not configured")
	}
	trackID = strings.TrimSpace(trackID)
	if trackID == "" {
		return nil, errors.New("zior: track id required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, has := ctx.Deadline(); !has {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, UploadTimeout)
		defer cancel()
	}

	// The body is assembled in memory: Zior needs the whole file before it
	// can answer, and the caller has already bounded the file's size at the
	// upload gate.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("track_id", trackID); err != nil {
		return nil, fmt.Errorf("zior upload form: %w", err)
	}
	if creatorID = strings.TrimSpace(creatorID); creatorID != "" {
		if err := mw.WriteField("creator_id", creatorID); err != nil {
			return nil, fmt.Errorf("zior upload form: %w", err)
		}
	}
	part, err := mw.CreatePart(audioPartHeader(filename))
	if err != nil {
		return nil, fmt.Errorf("zior upload form: %w", err)
	}
	if _, err := io.Copy(part, audio); err != nil {
		return nil, fmt.Errorf("zior upload copy: %w", err)
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("zior upload form: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/upload", &body)
	if err != nil {
		return nil, fmt.Errorf("zior upload request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.ContentLength = int64(body.Len())

	start := time.Now()
	resp, err := c.upload.Do(req)
	if err != nil {
		return nil, fmt.Errorf("zior upload do: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnprocessableEntity:
		return nil, ErrUnprocessable
	case http.StatusRequestEntityTooLarge:
		return nil, ErrTooLarge
	default:
		return nil, fmt.Errorf("zior upload status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("zior upload read: %w", err)
	}
	var out uploadResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("zior upload unmarshal: %w", err)
	}
	vec, ok := jung.FromSlice(out.TopicVector)
	if !ok {
		return nil, fmt.Errorf("zior upload: topic_vector has %d dims, want %d", len(out.TopicVector), jung.Dim)
	}
	if out.TrackID != trackID {
		// Zior stored the signal under a different key: the contract is
		// broken and nothing later could find it, so this is a failure.
		return nil, fmt.Errorf("zior upload: stored as %q, sent %q", out.TrackID, trackID)
	}
	log.Printf("[zior] analysed %s: mood=%s bpm=%.0f engine=%dms roundtrip=%dms",
		trackID, out.SignalPreview.Mood, out.SignalPreview.BPM, out.ProcessingMs, time.Since(start).Milliseconds())
	return &Analysis{
		TrackID:      out.TrackID,
		Vector:       vec.Normalise(),
		Mood:         out.SignalPreview.Mood,
		ContextTags:  out.SignalPreview.ContextTags,
		Descriptor:   out.SignalPreview.Descriptor,
		BPM:          out.SignalPreview.BPM,
		Key:          out.SignalPreview.Key,
		ProcessingMs: out.ProcessingMs,
	}, nil
}

// audioPartHeader builds the part header for the audio field. The filename
// is reduced to its base name and quoted so a name containing quotes or
// path separators cannot break the form; Zior only reads its extension.
func audioPartHeader(filename string) textproto.MIMEHeader {
	base := filepath.Base(strings.TrimSpace(filename))
	if base == "." || base == "/" || base == "" {
		base = "audio"
	}
	base = strings.NewReplacer("\\", "_", "\"", "_", "\r", "_", "\n", "_").Replace(base)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="audio"; filename="%s"`, base))
	h.Set("Content-Type", "application/octet-stream")
	return h
}

// Signal is the stored signal for one track, the subset of Zior's
// AethyrSignal this brain reads. Behavioural fields move as listeners play.
type Signal struct {
	TrackID        string    `json:"track_id"`
	CreatorID      string    `json:"creator_id"`
	ComputedAt     time.Time `json:"computed_at"`
	TopicVector    []float32 `json:"topic_vector"`
	VelocityScore  float64   `json:"velocity_score"`
	EarlyRetention float64   `json:"early_retention"`
	CompletionRate float64   `json:"completion_rate"`
	ReplayRate     float64   `json:"replay_rate"`
	ShareVelocity  float64   `json:"share_velocity"`
	ExposureCount  uint64    `json:"exposure_count"`
	DurationSecs   float64   `json:"duration_secs"`
	BPM            float32   `json:"bpm"`
	Mood           string    `json:"mood"`
	ContextTags    []string  `json:"context_tags"`
	Descriptor     string    `json:"descriptor"`
	HasVocals      bool      `json:"has_vocals"`
}

// Vector returns the signal's topic vector as a jung.Vector, normalised the
// way this brain stores vectors. The second result is false when Zior sent
// something that is not an 8-axis vector.
func (s *Signal) Vector() (jung.Vector, bool) {
	if s == nil {
		return jung.Vector{}, false
	}
	v, ok := jung.FromSlice(s.TopicVector)
	if !ok {
		return v, false
	}
	return v.Normalise(), true
}

// Signal fetches the stored signal for a track id (GET /signal/{id}).
func (c *Client) Signal(ctx context.Context, trackID string) (*Signal, error) {
	if !c.Enabled() {
		return nil, errors.New("zior: not configured")
	}
	trackID = strings.TrimSpace(trackID)
	if trackID == "" || strings.ContainsAny(trackID, "/?#") {
		return nil, errors.New("zior: invalid track id")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, has := ctx.Deadline(); !has {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, FastTimeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/signal/"+trackID, nil)
	if err != nil {
		return nil, fmt.Errorf("zior signal request: %w", err)
	}
	resp, err := c.fast.Do(req)
	if err != nil {
		return nil, fmt.Errorf("zior signal do: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, ErrNotFound
	default:
		return nil, fmt.Errorf("zior signal status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("zior signal read: %w", err)
	}
	var s Signal
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("zior signal unmarshal: %w", err)
	}
	return &s, nil
}

// PlayEventType is one of Zior's behavioural event kinds
// (zior-engine/src/behavioral/events.rs PlayEventType, snake_case).
type PlayEventType string

const (
	EventPlay     PlayEventType = "play"
	EventSkip     PlayEventType = "skip"
	EventComplete PlayEventType = "complete"
	EventReplay   PlayEventType = "replay"
	EventShare    PlayEventType = "share"
	EventSave     PlayEventType = "save"
	EventNegative PlayEventType = "negative"
	EventPlaylist PlayEventType = "playlist"
)

// ParsePlayEventType maps the names the browser posts to Zior's kinds. Both
// the bare kind ("skip") and the platform's prefixed event name
// ("track_skip") are accepted; anything else is rejected.
func ParsePlayEventType(name string) (PlayEventType, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.TrimPrefix(name, "track_")
	switch t := PlayEventType(name); t {
	case EventPlay, EventSkip, EventComplete, EventReplay, EventShare, EventSave, EventNegative, EventPlaylist:
		return t, true
	}
	return "", false
}

// PlayEvent is one listener interaction, the shape of Zior's IngestEvent.
type PlayEvent struct {
	TrackID      string        `json:"track_id"`
	UserID       string        `json:"user_id"`
	EventType    PlayEventType `json:"event_type"`
	PositionSecs float64       `json:"position_secs"`
	DurationSecs float64       `json:"duration_secs"`
	SessionID    string        `json:"session_id"`
	// Timestamp is when the event happened; nil lets Zior stamp receipt.
	Timestamp *time.Time `json:"timestamp,omitempty"`
}

type ingestRequest struct {
	Events []PlayEvent `json:"events"`
}

type ingestResponse struct {
	Ingested int      `json:"ingested"`
	TrackIDs []string `json:"track_ids"`
}

// IngestEvents forwards behavioural events to Zior (POST /events), which
// folds them into the track's behavioural vector and velocity. Events with
// no track id or user id are dropped before sending: Zior would attribute
// them to nothing.
func (c *Client) IngestEvents(ctx context.Context, events []PlayEvent) error {
	if !c.Enabled() {
		return errors.New("zior: not configured")
	}
	clean := make([]PlayEvent, 0, len(events))
	for _, ev := range events {
		ev.TrackID = strings.TrimSpace(ev.TrackID)
		ev.UserID = strings.TrimSpace(ev.UserID)
		if ev.TrackID == "" || ev.UserID == "" || ev.EventType == "" {
			continue
		}
		if ev.PositionSecs < 0 {
			ev.PositionSecs = 0
		}
		if ev.DurationSecs < 0 {
			ev.DurationSecs = 0
		}
		clean = append(clean, ev)
	}
	if len(clean) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, has := ctx.Deadline(); !has {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, FastTimeout)
		defer cancel()
	}
	body, err := json.Marshal(ingestRequest{Events: clean})
	if err != nil {
		return fmt.Errorf("zior events marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/events", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("zior events request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.fast.Do(req)
	if err != nil {
		return fmt.Errorf("zior events do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("zior events status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("zior events read: %w", err)
	}
	var out ingestResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return fmt.Errorf("zior events unmarshal: %w", err)
	}
	if out.Ingested != len(clean) {
		return fmt.Errorf("zior events: sent %d, ingested %d", len(clean), out.Ingested)
	}
	return nil
}

// SendEvents is IngestEvents detached: it runs on its own goroutine with
// its own deadline and only logs a failure. It is the call a request
// handler makes, since no listener interaction should wait on Zior.
func (c *Client) SendEvents(events []PlayEvent) {
	if !c.Enabled() || len(events) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), FastTimeout)
		defer cancel()
		if err := c.IngestEvents(ctx, events); err != nil {
			log.Printf("[zior] events: %v", err)
		}
	}()
}

// Health reports whether Zior answers on /health.
func (c *Client) Health(ctx context.Context) bool {
	if !c.Enabled() {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := c.fast.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
