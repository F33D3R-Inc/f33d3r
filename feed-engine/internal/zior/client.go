// Package zior is the client for Zior (zior-engine, Rust), the audio-analysis
// service: music, mood and tempo. It is not an identity or ownership service —
// Abraxas owns that — and it judges no content.
//
// Zior listens to an audio file and places it on the same eight Jung axes
// that internal/jung computes for text (the axis order is a shared contract:
// zior-engine/src/jung/axes.rs == internal/jung/vector.go). For a music track
// or a voice work the audio is the work, so Zior's vector is authoritative
// and replaces the text mapping; the text mapping remains the immediate
// fallback so no audio work is ever vector-less while Zior is still
// listening or unreachable.
//
// Zior is stateless. It answers once per file and keeps nothing; the answer
// is stored on the work (db.SetWorkAudioAnalysis) and never read back from it.
// The one caller is the zior_voice work obligation, which retries.
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
	// UploadTimeout bounds one /analyze round trip: Zior decodes and extracts
	// features from the whole file — up to a full track (trackUploadMax in
	// handler/music.go) — before answering.
	UploadTimeout = 90 * time.Second
	// FastTimeout bounds /health.
	FastTimeout = 2 * time.Second
	// InternalKeyHeader authenticates every call but /health to Zior.
	InternalKeyHeader = "X-Internal-Key"
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

// analyzeResponse mirrors zior-engine/src/api/types.rs AnalyzeResponse.
type analyzeResponse struct {
	ProcessingMs uint64    `json:"processing_ms"`
	DurationSecs float64   `json:"duration_secs"`
	TopicVector  []float32 `json:"topic_vector"`
	BPM          float32   `json:"bpm"`
	Key          string    `json:"key"`
	Mood         string    `json:"mood"`
	Descriptor   string    `json:"descriptor"`
	ContextTags  []string  `json:"context_tags"`
}

// Analyze sends one audio file to Zior as multipart/form-data and returns
// its analysis. The request shape is Zior's /analyze contract:
//
//	POST {ZIOR_URL}/analyze
//	X-Internal-Key: the shared INTERNAL_API_KEY
//	Content-Type: multipart/form-data
//	  audio — the file, with its filename so Zior reads the extension
func (c *Client) Analyze(ctx context.Context, internalKey, filename string, audio io.Reader) (*Analysis, error) {
	if !c.Enabled() {
		return nil, errors.New("zior: not configured")
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
	part, err := mw.CreatePart(audioPartHeader(filename))
	if err != nil {
		return nil, fmt.Errorf("zior analyze form: %w", err)
	}
	if _, err := io.Copy(part, audio); err != nil {
		return nil, fmt.Errorf("zior analyze copy: %w", err)
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("zior analyze form: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/analyze", &body)
	if err != nil {
		return nil, fmt.Errorf("zior analyze request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set(InternalKeyHeader, internalKey)
	req.ContentLength = int64(body.Len())

	start := time.Now()
	resp, err := c.upload.Do(req)
	if err != nil {
		return nil, fmt.Errorf("zior analyze do: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnprocessableEntity:
		return nil, ErrUnprocessable
	case http.StatusRequestEntityTooLarge:
		return nil, ErrTooLarge
	default:
		return nil, fmt.Errorf("zior analyze status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("zior analyze read: %w", err)
	}
	var out analyzeResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("zior analyze unmarshal: %w", err)
	}
	vec, ok := jung.FromSlice(out.TopicVector)
	if !ok {
		return nil, fmt.Errorf("zior analyze: topic_vector has %d dims, want %d", len(out.TopicVector), jung.Dim)
	}
	log.Printf("[zior] analysed %s: mood=%s bpm=%.0f engine=%dms roundtrip=%dms",
		filename, out.Mood, out.BPM, out.ProcessingMs, time.Since(start).Milliseconds())
	return &Analysis{
		Vector:       vec.Normalise(),
		Mood:         out.Mood,
		ContextTags:  out.ContextTags,
		Descriptor:   out.Descriptor,
		BPM:          out.BPM,
		Key:          out.Key,
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
