// Package scanclient is feed-engine's only route to the content-scan brain's
// perceptual hashing.
//
// feed-engine never computes a perceptual hash of its own. A perceptual hash
// matches only when the same algorithm produced both sides of the comparison:
// the values in banned_content_hashes were produced by content-scan's
// imagehash/chromaprint pipeline, so a second implementation in Go would
// disagree with every one of them and the gate would look enforced while
// catching nothing. One implementation, one algorithm.
//
// The exact-byte sha256 layer is deliberately NOT here. It is a local database
// query, it must run on every upload, and it must fail closed — it cannot
// depend on a service that is optional in this deployment.
package scanclient

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
	"os"
	"strings"
	"sync"
	"time"
)

// ErrNotConfigured is returned when CONTENT_SCAN_URL is unset, which is a
// supported deployment: the perceptual layer is absent by configuration rather
// than broken, and the caller is told so explicitly instead of being handed a
// clean-looking verdict nothing produced.
var ErrNotConfigured = errors.New("scanclient: CONTENT_SCAN_URL is unset")

// Media classes content-scan accepts. The class decides which perceptual layers
// apply: an image has no audio fingerprint, an audio file has no frame hash.
const (
	KindImage = "image"
	KindVideo = "video"
	KindAudio = "audio"
)

// kindTimeout bounds one perceptual check. A video hash is eight ffmpeg seeks
// plus a chromaprint pass, so it is allowed far longer than a still image; no
// class is allowed to block an upload indefinitely.
var kindTimeout = map[string]time.Duration{
	KindImage: 30 * time.Second,
	KindAudio: 120 * time.Second,
	KindVideo: 300 * time.Second,
}

// outageGrace is how long content-scan may be unreachable before the outage is
// logged at error severity. Below it the log stays a warning so a container
// restart does not read like an incident.
const outageGrace = 5 * time.Minute

// outageLogInterval rate-limits the outage log so a long outage does not drown
// every other line.
const outageLogInterval = time.Minute

// Hash types content-scan reports a near match on. A phash near match is a
// Hamming distance in the outer radius; an audio_fp near match is a chromaprint
// bit-similarity score in the band below the block cut.
const (
	HashTypePHash   = "phash"
	HashTypeAudioFP = "audio_fp"
)

// NearMatch is banned content close to — but not equal to — this media. It is a
// review signal, never a block: perceptual hashes of low-detail media collide
// and audio below the block cut is derived rather than identical, and a false
// refusal on this gate silences a legitimate creator.
//
// Which measurement is carried depends on the layer that found it: Distance for
// phash, Score for audio_fp. Never read one without checking HashType — read
// them through Detail and Describe instead.
type NearMatch struct {
	HashType string  `json:"hash_type"`
	Distance int     `json:"distance"`
	Score    float64 `json:"score"`
	Category string  `json:"category"`
}

// Detail renders the measurement behind a near match for a signal string: "d6"
// for a phash Hamming distance, "s0.8532" for an audio similarity score. One
// vocabulary across both registries, so a moderator reads the same shape
// whichever layer found it.
//
// A hash type this build does not know carries both numbers rather than being
// rendered as a distance it may not have: a newer content-scan must never have
// its finding silently mislabelled here.
func (n NearMatch) Detail() string {
	switch n.HashType {
	case HashTypeAudioFP:
		return fmt.Sprintf("s%.4f", n.Score)
	case HashTypePHash:
		return fmt.Sprintf("d%d", n.Distance)
	default:
		return fmt.Sprintf("d%d:s%.4f", n.Distance, n.Score)
	}
}

// Describe renders a near match for a log line.
func (n NearMatch) Describe() string {
	switch n.HashType {
	case HashTypeAudioFP:
		return fmt.Sprintf("audio at similarity %.4f of banned %s content", n.Score, n.Category)
	case HashTypePHash:
		return fmt.Sprintf("phash within distance %d of banned %s content", n.Distance, n.Category)
	default:
		return fmt.Sprintf("%s near banned %s content (%s)", n.HashType, n.Category, n.Detail())
	}
}

// MediaVerdict is content-scan's answer from /v1/hashes/check-media.
//
// Checked lists the layers that actually ran; Degraded lists the layers that
// applied to this media and could not be computed. A verdict with a non-empty
// Degraded is not a clean verdict — part of the gate did not run.
type MediaVerdict struct {
	Banned         bool        `json:"banned"`
	HashType       string      `json:"hash_type"`
	Category       string      `json:"category"`
	SHA256         string      `json:"sha256"`
	PHash          string      `json:"phash"`
	AudioFPPresent bool        `json:"audio_fp_present"`
	Checked        []string    `json:"checked"`
	Degraded       []string    `json:"degraded"`
	NearMatch      *NearMatch  `json:"near_match"`
	NearMatches    []NearMatch `json:"near_matches"`
	Kind           string      `json:"kind"`
}

// Nears returns every near match in the verdict. One piece of media can be near
// banned content on more than one layer — a video's frames and its audio are
// hashed separately — and each of those is evidence a moderator has to see, so
// none of them is dropped. NearMatch is the single most specific finding and is
// used on its own only when the list is absent.
func (v *MediaVerdict) Nears() []NearMatch {
	if len(v.NearMatches) > 0 {
		return v.NearMatches
	}
	if v.NearMatch != nil {
		return []NearMatch{*v.NearMatch}
	}
	return nil
}

// Client talks to one content-scan instance and tracks whether it is answering.
type Client struct {
	base string
	http *http.Client

	mu            sync.Mutex
	outageSince   time.Time
	outageReason  string
	lastOutageLog time.Time
}

var (
	sharedOnce sync.Once
	shared     *Client
)

// Shared returns the process-wide client, built from CONTENT_SCAN_URL on first
// use. The absence of a classifier is announced once, loudly, so no operator
// believes the perceptual layer is running when it is not.
func Shared() *Client {
	sharedOnce.Do(func() {
		shared = New(os.Getenv("CONTENT_SCAN_URL"))
		if shared.base == "" {
			log.Printf("[scan] CONTENT_SCAN_URL is unset — uploads are gated on the local sha256 " +
				"banned-hash registry only. The perceptual layer (phash, audio_fp) does not run, " +
				"so a re-encoded or re-containered copy of banned content is not caught.")
			return
		}
		log.Printf("[scan] content-scan: %s — perceptual banned-hash layer active", shared.base)
	})
	return shared
}

// New builds a client for an explicit base URL. Request deadlines come from the
// per-call context, so the http.Client carries no timeout of its own.
func New(base string) *Client {
	return &Client{
		base: strings.TrimRight(strings.TrimSpace(base), "/"),
		http: &http.Client{},
	}
}

// Configured reports whether a content-scan address is known.
func (c *Client) Configured() bool { return c != nil && c.base != "" }

// Base returns the configured address, for log lines that must name it.
func (c *Client) Base() string {
	if c == nil {
		return ""
	}
	return c.base
}

// CheckMedia asks content-scan whether these bytes match the banned registry on
// any hash type it can compute. An error means no verdict was reached: the
// caller must treat that as unjudged, never as clean.
func (c *Client) CheckMedia(ctx context.Context, kind, filename string, data []byte) (*MediaVerdict, error) {
	if len(data) == 0 {
		return nil, errors.New("scanclient: no bytes to check")
	}
	return c.check(ctx, kind, filename, int64(len(data)), func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	})
}

// CheckMediaFile is CheckMedia for media that is already on disk. A video is
// gigabytes and is never pulled into memory to be asked about: the file streams
// straight into the request body.
func (c *Client) CheckMediaFile(ctx context.Context, kind, filename, path string) (*MediaVerdict, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() == 0 {
		return nil, fmt.Errorf("scanclient: %s is empty", path)
	}
	return c.check(ctx, kind, filename, info.Size(), func() (io.ReadCloser, error) {
		return os.Open(path)
	})
}

// check posts one multipart request and decodes the verdict.
//
// The body is assembled as [part headers][payload][closing boundary] so its
// exact length is known before a byte is sent. That keeps the request off
// chunked transfer encoding while still streaming a payload of any size.
func (c *Client) check(ctx context.Context, kind, filename string, size int64,
	open func() (io.ReadCloser, error)) (*MediaVerdict, error) {

	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	timeout, ok := kindTimeout[kind]
	if !ok {
		return nil, fmt.Errorf("scanclient: unknown media kind %q", kind)
	}
	if filename == "" {
		filename = "upload"
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	if err := mw.WriteField("kind", kind); err != nil {
		return nil, err
	}
	// CreateFormFile writes the part headers into head; the payload is streamed
	// after them rather than through the writer.
	if _, err := mw.CreateFormFile("file", filename); err != nil {
		return nil, err
	}
	tail := "\r\n--" + mw.Boundary() + "--\r\n"

	payload, err := open()
	if err != nil {
		return nil, err
	}
	defer payload.Close()

	body := io.MultiReader(bytes.NewReader(head.Bytes()), payload, strings.NewReader(tail))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/hashes/check-media", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.ContentLength = int64(head.Len()) + size + int64(len(tail))

	resp, err := c.http.Do(req)
	if err != nil {
		c.noteOutage(err)
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// The body carries the reason content-scan refused — a detector that is
		// not loaded, or a registry it could not read. Carrying it into the
		// error is what makes the degraded state diagnosable from the log.
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		statusErr := fmt.Errorf("content-scan status %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
		if resp.StatusCode >= http.StatusInternalServerError {
			c.noteOutage(statusErr)
		}
		return nil, statusErr
	}

	var out MediaVerdict
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		c.noteOutage(err)
		return nil, err
	}
	c.noteHealthy()
	return &out, nil
}

// noteOutage opens or extends the outage window and logs it at a bounded rate,
// escalating to error severity once the outage outlives the grace window.
func (c *Client) noteOutage(cause error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.outageSince.IsZero() {
		c.outageSince = time.Now()
		c.outageReason = cause.Error()
		c.lastOutageLog = time.Time{}
	}
	if time.Since(c.lastOutageLog) < outageLogInterval {
		return
	}
	c.lastOutageLog = time.Now()

	down := time.Since(c.outageSince).Round(time.Second)
	if down >= outageGrace {
		log.Printf("[scan] ERROR content-scan %s unreachable for %s (%s) — the perceptual "+
			"banned-hash layer has not run for that long. Uploads are still gated on sha256 and "+
			"every unjudged upload is recorded in csam_scan_log at result='pending_scan'.",
			c.base, down, c.outageReason)
		return
	}
	log.Printf("[scan] content-scan %s unreachable for %s (%s) — perceptual banned-hash layer degraded",
		c.base, down, c.outageReason)
}

// noteHealthy closes an open outage window.
func (c *Client) noteHealthy() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.outageSince.IsZero() {
		return
	}
	log.Printf("[scan] content-scan %s recovered after %s (was: %s) — perceptual banned-hash layer active again",
		c.base, time.Since(c.outageSince).Round(time.Second), c.outageReason)
	c.outageSince = time.Time{}
	c.outageReason = ""
	c.lastOutageLog = time.Time{}
}
