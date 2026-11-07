// Package herald is feed-engine's client for the notification inbox Herald owns.
package herald

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

// ErrNotFound means Herald holds no such notification for this account.
var ErrNotFound = errors.New("herald: notification not found")

const requestTimeout = 3 * time.Second

var (
	baseURL     string
	internalKey string
	httpClient  = &http.Client{Timeout: requestTimeout}
)

// Configure sets where Herald is. It is called once at startup.
func Configure(url, key string) {
	baseURL = strings.TrimRight(url, "/")
	internalKey = key
}

// Row is one stored notification.
type Row struct {
	ID           string    `json:"id"`
	Kind         string    `json:"kind"`
	ActorAccount string    `json:"actor_account"`
	TargetID     string    `json:"target_id"`
	TargetType   string    `json:"target_type"`
	Payload      string    `json:"payload"`
	IsRead       bool      `json:"is_read"`
	CreatedAt    time.Time `json:"created_at"`
}

func call(ctx context.Context, method, path string, body, out interface{}) error {
	if baseURL == "" {
		return errors.New("herald: HERALD_URL is not configured")
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("X-Internal-Key", internalKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("herald: %s %s answered %d", method, path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// List returns an account's notifications newest first, optionally only some kinds.
func List(ctx context.Context, account string, limit int, before time.Time, kinds ...string) ([]Row, error) {
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))
	q.Set("before", before.UTC().Format(time.RFC3339Nano))
	if len(kinds) > 0 {
		q.Set("kinds", strings.Join(kinds, ","))
	}
	var out struct {
		Notifications []Row `json:"notifications"`
	}
	err := call(ctx, http.MethodGet, "/v1/inbox/"+url.PathEscape(account)+"?"+q.Encode(), nil, &out)
	return out.Notifications, err
}

// Unread returns an account's unread count.
func Unread(ctx context.Context, account string) (int, error) {
	var out struct {
		Unread int `json:"unread"`
	}
	err := call(ctx, http.MethodGet, "/v1/inbox/"+url.PathEscape(account)+"/unread", nil, &out)
	return out.Unread, err
}

// Group returns a notification's kind and the rows of its group.
func Group(ctx context.Context, account, id string) (string, []string, error) {
	var out struct {
		Kind string   `json:"kind"`
		IDs  []string `json:"ids"`
	}
	err := call(ctx, http.MethodGet, "/v1/inbox/"+url.PathEscape(account)+"/group/"+url.PathEscape(id), nil, &out)
	return out.Kind, out.IDs, err
}

// MarkRead marks the given rows read, or all of them.
func MarkRead(ctx context.Context, account string, ids []string, all bool) error {
	body := map[string]interface{}{"ids": ids, "all": all}
	return call(ctx, http.MethodPost, "/v1/inbox/"+url.PathEscape(account)+"/read", body, nil)
}

// DistinctActors counts who sent an account one kind of notification in recent days.
func DistinctActors(ctx context.Context, account, kind string, days int) (int, error) {
	q := url.Values{}
	q.Set("kind", kind)
	q.Set("days", strconv.Itoa(days))
	var out struct {
		Actors int `json:"actors"`
	}
	err := call(ctx, http.MethodGet, "/v1/inbox/"+url.PathEscape(account)+"/actors?"+q.Encode(), nil, &out)
	return out.Actors, err
}

// Purge removes an account's inbox and every row naming it to other people.
func Purge(ctx context.Context, account string) error {
	return call(ctx, http.MethodPost, "/v1/inbox/purge", map[string]string{"account": account}, nil)
}

// Missing returns which of these event IDs Herald has not stored.
func Missing(ctx context.Context, eventIDs []string) ([]string, error) {
	var out struct {
		Missing []string `json:"missing"`
	}
	err := call(ctx, http.MethodPost, "/v1/inbox/missing", map[string][]string{"event_ids": eventIDs}, &out)
	return out.Missing, err
}
