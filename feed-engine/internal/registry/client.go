// Package registry is feed-engine's client for registry-brain, the handle
// authority.
//
// A handle is not a property of a user row. It is an allocation: a scarce
// string granted to one identity, transferable, auctionable, reclaimable, and
// reservable — and every one of those verbs already lives in registry-brain.
// Manhattan's authority map records the same thing at the wire, refusing any
// brain but the registrar that tries to bind a name in the `handle` namespace.
//
// feed-engine ran signup anyway, wrote `users.handle` on its own authority, and
// then tried to publish the binding itself. Two brains held one truth and only
// one of them was allowed to say it out loud, so the handle plane simply never
// resolved. This package is the missing edge: the allocation decision leaves
// this brain and is made where the authority actually sits, and the registrar's
// own outbox publishes the name that follows from it.
//
// There is deliberately no database handle here. The registrar's tables are the
// registrar's; this brain reaches them over the boundary or not at all.
package registry

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

const headerRequestID = "X-Request-Id"

// Errors a caller is expected to branch on. Everything else is a transport or
// server failure and is returned verbatim.
var (
	// ErrNotConfigured means REGISTRY_BRAIN_URL is unset. A handle cannot be
	// allocated without the authority that allocates it, so callers must treat
	// this as a failure and not as permission to proceed locally.
	ErrNotConfigured = errors.New("registry: REGISTRY_BRAIN_URL is not configured")

	// ErrTaken means the handle is already allocated to some identity.
	ErrTaken = errors.New("registry: handle already taken")

	// ErrReserved means the handle is held back and may not be allocated.
	ErrReserved = errors.New("registry: handle is reserved")

	// ErrNotFound means the registrar has no record of the handle.
	ErrNotFound = errors.New("registry: handle not found")
)

// Client talks to registry-brain over HTTP.
type Client struct {
	baseURL string
	http    *http.Client
}

// New returns a client for the registrar at baseURL. An empty baseURL yields a
// client that reports Configured() == false and fails every call with
// ErrNotConfigured, which is louder and safer than silently allocating handles
// this brain has no right to allocate.
func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *Client) Configured() bool { return c != nil && c.baseURL != "" }

// Handle is one allocation as the registrar sees it.
type Handle struct {
	ID     string `json:"id"`
	Handle string `json:"handle"`
	PIALID string `json:"pial_id"`
	Status string `json:"status"`
	Tier   string `json:"tier"`
}

// Claim allocates handle to pialID. This is the allocation decision itself, not
// a notification about one made elsewhere: if it returns an error the handle was
// NOT granted, and no local row may be written as though it had been.
//
// ErrTaken and ErrReserved are the registrar's refusals and are the caller's to
// render. Every other error means the authority could not be reached, which is
// equally a refusal — a handle allocated while the allocator is unreachable is
// exactly the split the authority map exists to prevent.
func (c *Client) Claim(ctx context.Context, handle, pialID string) (*Handle, error) {
	var out Handle
	body := map[string]string{
		"handle":  strings.ToLower(strings.TrimSpace(handle)),
		"pial_id": pialID,
		"tier":    "standard",
	}
	if err := c.do(ctx, http.MethodPost, "/v1/handles", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Resolve returns the registrar's record for handle, or ErrNotFound.
func (c *Client) Resolve(ctx context.Context, handle string) (*Handle, error) {
	var out Handle
	path := "/v1/handles/" + strings.ToLower(strings.TrimSpace(handle))
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ── Transport ────────────────────────────────────────────────────────────────

func (c *Client) do(ctx context.Context, method, path string, body interface{}, out interface{}) error {
	if !c.Configured() {
		return ErrNotConfigured
	}

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("registry: encoding %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("registry: building %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if rid, ok := ctx.Value(requestIDKey).(string); ok && rid != "" {
		req.Header.Set(headerRequestID, rid)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("registry: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		// The registrar's two refusals are distinguishable only by their
		// message, so they are decoded here once rather than string-matched at
		// every call site.
		if resp.StatusCode == http.StatusConflict {
			switch {
			case strings.Contains(string(detail), "reserved"):
				return ErrReserved
			case strings.Contains(string(detail), "taken"):
				return ErrTaken
			}
		}
		return fmt.Errorf("registry: %s %s: %s: %s",
			method, path, resp.Status, strings.TrimSpace(string(detail)))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("registry: decoding %s %s: %w", method, path, err)
	}
	return nil
}

type ctxKey string

const requestIDKey ctxKey = "request_id"

// WithRequestID propagates a request id across the brain boundary so one user
// action stays one traceable line through every service it touches.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}
