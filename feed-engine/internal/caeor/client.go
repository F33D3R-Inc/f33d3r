// Package caeor is feed-engine's client for Caeor, the one media-asset
// authority.
//
// Caeor owns every stored byte a person uploads: the private source, the
// watermarked derivatives every viewer is served, the HLS ladder, the
// downloadable copy and the forensic layer burned into a paid download. It owns
// who uploaded each asset, which exact-byte duplicates resolve to which
// original, and whether an asset has been withdrawn. feed-engine is its client
// and keeps only the work-side decisions: which works reference which assets,
// the wall in front of a work, and whether a work's author allows downloads.
//
// There is deliberately no local-disk fallback anywhere in this package or its
// callers. An upload Caeor cannot take is an upload that failed, and the person
// is told so; a byte stored around the authority is a byte nothing can withdraw,
// watermark or credit.
//
// Every call carries the service key. An unconfigured client fails every call
// with ErrNotConfigured, which is louder and safer than storing anything this
// brain has no right to store.
package caeor

import (
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

// headerServiceKey is how every call proves it comes from a platform service.
const headerServiceKey = "X-Service-Key"

// Kind is the closed vocabulary of asset kinds Caeor stores.
type Kind string

const (
	KindImage     Kind = "image"
	KindVideo     Kind = "video"
	KindVoice     Kind = "voice"
	KindAudio     Kind = "audio"
	KindAvatar    Kind = "avatar"
	KindHeader    Kind = "header"
	KindEncrypted Kind = "encrypted"
	KindClip      Kind = "clip"
)

// IsWorkMedia reports whether assets of this kind are governed by the wall of
// the works (or Visions) that reference them. Avatars and headers are profile
// furniture, and encrypted ciphertext is opened by its content key, not by
// the read path; neither has a work wall.
func (k Kind) IsWorkMedia() bool {
	switch k {
	case KindImage, KindVideo, KindVoice, KindAudio, KindClip:
		return true
	}
	return false
}

// Asset status words.
const (
	StatusReady      = "ready"
	StatusProcessing = "processing"
	StatusFailed     = "failed"
	StatusWithdrawn  = "withdrawn"
	// StatusHeld: the asset exists and nothing of it is servable, because
	// Abraxas, the integrity authority, has it under review or has not answered
	// yet. Caeor releases or withdraws it when the answer comes.
	StatusHeld = "held"
)

// Errors a caller is expected to branch on.
var (
	// ErrNotConfigured means CAEOR_URL or CAEOR_SERVICE_KEY is unset.
	ErrNotConfigured = errors.New("caeor: CAEOR_URL and CAEOR_SERVICE_KEY must both be configured")
	// ErrNotFound means Caeor has no asset (or no servable object) for the
	// question asked. For a path, that includes a private source: a private
	// object is never resolvable as servable.
	ErrNotFound = errors.New("caeor: no such asset")
	// ErrRefused means Caeor refused the upload itself (unreadable, wrong type,
	// too large). The person is told; nothing was stored.
	ErrRefused = errors.New("caeor: upload refused")
	// ErrBanned means the integrity authority refused the upload: the bytes
	// match banned content. The asset is withdrawn and kept as the record.
	ErrBanned = errors.New("caeor: upload refused by the integrity authority")
	// ErrBusy means Caeor has too much work queued to take this one now.
	ErrBusy = errors.New("caeor: too busy, try again shortly")
	// ErrWithdrawn means the asset has been taken off the platform.
	ErrWithdrawn = errors.New("caeor: asset withdrawn")
	// ErrNotReady means the asset (or its downloadable copy) is not finished.
	ErrNotReady = errors.New("caeor: asset not ready")
	// ErrNoDownload means the asset's kind has no downloadable copy of the
	// requested sort (profile pictures, ciphertext, a forensic copy of audio).
	ErrNoDownload = errors.New("caeor: no download for this asset")
)

// Uploader is the person an upload is put on record under.
type Uploader struct {
	PIAL    string
	Account string
	Handle  string
}

// URLs are the viewer-facing objects of one asset, relative to this site. For
// a visual asset every one of them is watermarked. Empty until the asset is
// ready, and after it is withdrawn.
type URLs struct {
	Thumb     string `json:"thumb,omitempty"`
	Feed      string `json:"feed,omitempty"`
	Full      string `json:"full,omitempty"`
	HLSMaster string `json:"hls_master,omitempty"`
	Poster    string `json:"poster,omitempty"`
}

// Duplicate names the original asset an exact-byte duplicate resolved to.
type Duplicate struct {
	AssetID      string `json:"asset_id"`
	ShortID      string `json:"short_id"`
	UploaderPIAL string `json:"uploader_pial"`
	Kind         Kind   `json:"kind"`
	Status       string `json:"status"`
}

// Credit names the original owner of an asset's bytes, when the integrity
// authority found them to be (or derive from) somebody else's. The original
// owner is credited wherever the asset is shown.
type Credit struct {
	OriginalAsset string `json:"original_asset,omitempty"`
	OriginalPIAL  string `json:"original_pial"`
	// Kind is exact | derivative | excerpt.
	Kind string `json:"kind"`
}

// Asset is one stored asset as Caeor describes it (its AssetView).
type Asset struct {
	AssetID         string   `json:"asset_id"`
	ShortID         string   `json:"short_id"`
	Kind            Kind     `json:"kind"`
	Status          string   `json:"status"`
	UploaderPIAL    string   `json:"uploader_pial"`
	UploaderAccount *string  `json:"uploader_account"`
	UploaderHandle  string   `json:"uploader_handle"`
	Withdrawn       bool     `json:"withdrawn"`
	Failure         string   `json:"failure,omitempty"`
	DurationSecs    *float64 `json:"duration_seconds"`
	Width           *int     `json:"width"`
	Height          *int     `json:"height"`
	// PrimaryURL is the one object a work names: the feed rendition of an
	// image, the HLS master of a video, the file itself for audio.
	PrimaryURL       *string `json:"primary_url"`
	URLs             URLs    `json:"urls"`
	Downloadable     bool    `json:"downloadable"`
	FullyWatermarked bool    `json:"fully_watermarked"`
	// CreditTo is the original owner to credit, when these bytes are somebody
	// else's. Nil when they are the uploader's own.
	CreditTo *Credit `json:"credit_to,omitempty"`

	// DuplicateOf is set by Upload, never by Caeor's view: the bytes were an
	// exact duplicate and this asset is the ORIGINAL, whose uploader keeps the
	// credit. Nothing new was stored.
	DuplicateOf *Duplicate `json:"-"`
}

// Held reports whether the integrity authority is still holding the asset.
func (a *Asset) Held() bool { return a != nil && a.Status == StatusHeld }

// CreditPIAL is whom the asset's bytes are credited to: the original owner
// the integrity authority named, or else the uploader.
func (a *Asset) CreditPIAL() string {
	if a.CreditTo != nil && a.CreditTo.OriginalPIAL != "" {
		return a.CreditTo.OriginalPIAL
	}
	return a.UploaderPIAL
}

// Ready reports whether the asset is finished and on the platform.
func (a *Asset) Ready() bool { return a != nil && a.Status == StatusReady && !a.Withdrawn }

// ViewerURL is the one URL a work (or a profile) stores for the asset: the
// watermarked object a viewer is served. "" until Caeor has produced it.
func (a *Asset) ViewerURL() string {
	if a.PrimaryURL == nil {
		return ""
	}
	return *a.PrimaryURL
}

// Duration, Dimensions: what Caeor measured; zero when it did not say.
func (a *Asset) Duration() float64 {
	if a.DurationSecs == nil {
		return 0
	}
	return *a.DurationSecs
}

func (a *Asset) Dimensions() (int, int) {
	w, h := 0, 0
	if a.Width != nil {
		w = *a.Width
	}
	if a.Height != nil {
		h = *a.Height
	}
	return w, h
}

// Resolved is what one served path is.
type Resolved struct {
	AssetID         string  `json:"asset_id"`
	ShortID         string  `json:"short_id"`
	UploaderPIAL    string  `json:"uploader_pial"`
	UploaderAccount *string `json:"uploader_account"`
	Kind            Kind    `json:"kind"`
	Status          string  `json:"status"`
	Withdrawn       bool    `json:"withdrawn"`
	// Role names which object of the asset the path is.
	Role        string `json:"role"`
	Variant     string `json:"variant,omitempty"`
	Watermarked bool   `json:"watermarked"`
	ContentType string `json:"content_type"`
	// Servable is Caeor's one answer to "may these bytes be served at all":
	// false when the asset is not ready or is withdrawn, the bytes were
	// removed, the object is the download file or a private source, or a
	// content object is not watermarked (legacy media awaiting its re-mark).
	Servable bool `json:"servable"`
}

// Client talks to Caeor over HTTP.
type Client struct {
	baseURL string
	key     string
	// calls carries the short questions: resolve, describe, withdraw.
	calls *http.Client
	// transfers carries bytes in either direction. No overall timeout: a
	// multi-gigabyte upload or download is bounded by its request context.
	transfers *http.Client
}

// New returns a client for the Caeor at baseURL, authenticating with key.
func New(baseURL, key string) *Client {
	return &Client{
		baseURL:   strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		key:       strings.TrimSpace(key),
		calls:     &http.Client{Timeout: 10 * time.Second},
		transfers: &http.Client{},
	}
}

// Configured reports whether the client can make any call at all.
func (c *Client) Configured() bool { return c != nil && c.baseURL != "" && c.key != "" }

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("caeor: building %s %s: %w", method, path, err)
	}
	req.Header.Set(headerServiceKey, c.key)
	return req, nil
}

// failure reads an error answer into an error, keeping a bounded excerpt of
// what Caeor said.
func failure(op string, resp *http.Response) error {
	excerpt, err := io.ReadAll(io.LimitReader(resp.Body, 512))
	if err != nil {
		return fmt.Errorf("caeor: %s: HTTP %d (body unreadable: %v)", op, resp.StatusCode, err)
	}
	return fmt.Errorf("caeor: %s: HTTP %d: %s", op, resp.StatusCode, strings.TrimSpace(string(excerpt)))
}

func decode(op string, resp *http.Response, into any) error {
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return fmt.Errorf("caeor: %s: unreadable answer: %w", op, err)
	}
	return nil
}

// Upload streams one file to Caeor as an asset of kind, on record under up.
// filename supplies the extension Caeor needs for a raw body; size is the
// exact byte count of body (sent as Content-Length, so a truncated stream fails
// rather than storing a partial asset). body is read once and never buffered.
//
// An exact-byte duplicate of an existing asset stores nothing: Caeor answers
// 409 naming the original, and Upload returns that ORIGINAL asset with
// DuplicateOf set — its uploader keeps the credit, and the work that uses it
// is a credited reference.
func (c *Client) Upload(ctx context.Context, kind Kind, up Uploader, filename string, body io.Reader, size int64) (*Asset, error) {
	if up.PIAL == "" || up.Account == "" || up.Handle == "" {
		return nil, fmt.Errorf("caeor: upload of %s: uploader PIAL, account and handle are all required", kind)
	}
	if size <= 0 {
		return nil, fmt.Errorf("caeor: upload of %s: empty file", kind)
	}
	if kind == KindClip {
		return nil, fmt.Errorf("caeor: a clip is made with Clip, not uploaded")
	}
	q := url.Values{}
	q.Set("kind", string(kind))
	q.Set("uploader_pial", up.PIAL)
	q.Set("uploader_account", up.Account)
	q.Set("uploader_handle", up.Handle)
	q.Set("filename", filename)
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/assets?"+q.Encode(), body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.transfers.Do(req)
	if err != nil {
		return nil, fmt.Errorf("caeor: upload of %s: %w", kind, err)
	}
	defer resp.Body.Close()
	op := "upload of " + string(kind)
	switch {
	case resp.StatusCode == http.StatusConflict:
		var dup struct {
			Status      string     `json:"status"`
			DuplicateOf *Duplicate `json:"duplicate_of"`
		}
		if err := decode(op, resp, &dup); err != nil {
			return nil, err
		}
		if dup.Status != "duplicate" || dup.DuplicateOf == nil || dup.DuplicateOf.AssetID == "" {
			return nil, fmt.Errorf("caeor: %s: conflict that names no original", op)
		}
		original, err := c.Get(ctx, dup.DuplicateOf.AssetID)
		if err != nil {
			return nil, fmt.Errorf("caeor: %s: reading the original %s: %w", op, dup.DuplicateOf.AssetID, err)
		}
		original.DuplicateOf = dup.DuplicateOf
		return original, nil
	case resp.StatusCode == http.StatusUnprocessableEntity:
		// 422 is either the integrity authority's refusal ({"error":"banned"})
		// or bytes Caeor could not process. The body says which.
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 512))
		if err != nil {
			return nil, fmt.Errorf("caeor: %s: HTTP 422 (body unreadable: %v)", op, err)
		}
		var refusal struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &refusal) == nil && refusal.Error == "banned" {
			return nil, ErrBanned
		}
		return nil, fmt.Errorf("%w: caeor: %s: HTTP 422: %s", ErrRefused, op, strings.TrimSpace(string(raw)))
	case resp.StatusCode == http.StatusBadRequest,
		resp.StatusCode == http.StatusRequestEntityTooLarge, resp.StatusCode == http.StatusUnsupportedMediaType:
		return nil, fmt.Errorf("%w: %v", ErrRefused, failure(op, resp))
	case resp.StatusCode == http.StatusServiceUnavailable:
		return nil, fmt.Errorf("%w: %v", ErrBusy, failure(op, resp))
	case resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK:
		return nil, failure(op, resp)
	}
	return decodeAsset(op, resp)
}

func decodeAsset(op string, resp *http.Response) (*Asset, error) {
	var a Asset
	if err := decode(op, resp, &a); err != nil {
		return nil, err
	}
	if a.AssetID == "" {
		return nil, fmt.Errorf("caeor: %s: answer carries no asset_id", op)
	}
	return &a, nil
}

// ClipRequest is one Frequency clip: a span of a replay, drawn as a
// watermarked video asset of kind clip on record under the clipper.
type ClipRequest struct {
	ReplayFile     string `json:"replay_file"`
	StartSeconds   int    `json:"start_seconds"`
	LengthSeconds  int    `json:"length_seconds"`
	Title          string `json:"title"`
	HostHandle     string `json:"host_handle"`
	ClipperPIAL    string `json:"clipper_pial"`
	ClipperAccount string `json:"clipper_account,omitempty"`
	ClipperHandle  string `json:"clipper_handle"`
}

// Clip asks Caeor to make a clip. The answer is the clip asset, processing;
// poll Get until it is ready. ErrBusy when Caeor has too many clips waiting.
func (c *Client) Clip(ctx context.Context, r ClipRequest) (*Asset, error) {
	body, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("caeor: clip: %w", err)
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/assets/clip", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.calls.Do(req)
	if err != nil {
		return nil, fmt.Errorf("caeor: clip: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusServiceUnavailable:
		return nil, fmt.Errorf("%w: %v", ErrBusy, failure("clip", resp))
	case resp.StatusCode == http.StatusBadRequest:
		return nil, fmt.Errorf("%w: %v", ErrRefused, failure("clip", resp))
	case resp.StatusCode != http.StatusCreated:
		return nil, failure("clip", resp)
	}
	return decodeAsset("clip", resp)
}

// Get describes one asset. ErrNotFound when Caeor has no such asset.
func (c *Client) Get(ctx context.Context, assetID string) (*Asset, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/assets/"+url.PathEscape(assetID), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.calls.Do(req)
	if err != nil {
		return nil, fmt.Errorf("caeor: describing %s: %w", assetID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, failure("describing "+assetID, resp)
	}
	return decodeAsset("describing "+assetID, resp)
}

// Resolve says what the object at a served path is. ErrNotFound for a path
// that names no servable object of any asset — including any private source.
func (c *Client) Resolve(ctx context.Context, path string) (*Resolved, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/assets/resolve?path="+url.QueryEscape(path), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.calls.Do(req)
	if err != nil {
		return nil, fmt.Errorf("caeor: resolving %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, failure("resolving "+path, resp)
	}
	var r Resolved
	if err := decode("resolving "+path, resp, &r); err != nil {
		return nil, err
	}
	if r.AssetID == "" {
		return nil, fmt.Errorf("caeor: resolving %s: answer carries no asset_id", path)
	}
	return &r, nil
}

// Withdraw takes an asset off the platform for good. Withdrawing an asset that
// is already withdrawn succeeds; an asset Caeor has never heard of is
// ErrNotFound, which a caller with nothing left to withdraw treats as done. A
// withdrawal Caeor could not complete (some bytes not removed) is an error, and
// the caller retries it.
func (c *Client) Withdraw(ctx context.Context, assetID, reason string) error {
	body, err := json.Marshal(map[string]string{"reason": reason})
	if err != nil {
		return fmt.Errorf("caeor: withdrawing %s: %w", assetID, err)
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/assets/"+url.PathEscape(assetID)+"/withdraw", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.calls.Do(req)
	if err != nil {
		return fmt.Errorf("caeor: withdrawing %s: %w", assetID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode >= 300 {
		return failure("withdrawing "+assetID, resp)
	}
	return nil
}

// Recheck asks Caeor to put a held asset to the integrity authority again now,
// rather than at its next scheduled check. It decides nothing; Caeor fetches the
// answer from Abraxas. An asset that is not held is not an error.
func (c *Client) Recheck(ctx context.Context, assetID string) error {
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/assets/"+url.PathEscape(assetID)+"/recheck", nil)
	if err != nil {
		return err
	}
	resp, err := c.calls.Do(req)
	if err != nil {
		return fmt.Errorf("caeor: recheck of %s: %w", assetID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return failure("recheck of "+assetID, resp)
	}
	return nil
}

// Download opens the downloadable, watermarked copy of an asset for one
// viewer. forensic burns the per-subscriber layer naming the viewer into it.
// The caller owns the returned body and must close it.
func (c *Client) Download(ctx context.Context, assetID, viewerHandle string, forensic bool) (*http.Response, error) {
	q := url.Values{}
	q.Set("viewer_handle", viewerHandle)
	q.Set("forensic", strconv.FormatBool(forensic))
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/assets/"+url.PathEscape(assetID)+"/download?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.transfers.Do(req)
	if err != nil {
		return nil, fmt.Errorf("caeor: download of %s: %w", assetID, err)
	}
	if resp.StatusCode == http.StatusOK {
		return resp, nil
	}
	defer resp.Body.Close()
	op := "download of " + assetID
	switch resp.StatusCode {
	case http.StatusNotFound:
		return nil, ErrNotFound
	case http.StatusGone:
		return nil, fmt.Errorf("%w: %v", ErrWithdrawn, failure(op, resp))
	case http.StatusConflict:
		return nil, fmt.Errorf("%w: %v", ErrNotReady, failure(op, resp))
	case http.StatusUnprocessableEntity:
		return nil, fmt.Errorf("%w: %v", ErrNoDownload, failure(op, resp))
	}
	return nil, failure(op, resp)
}
