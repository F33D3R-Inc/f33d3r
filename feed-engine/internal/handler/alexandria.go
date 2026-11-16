package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// alexandriaTitlePayload is the JSON body sent to Alexandria POST /v1/titles.
type alexandriaTitlePayload struct {
	AuthorPIALID string   `json:"author_pial_id"`
	SectionSlug  string   `json:"section_slug,omitempty"`
	Body         string   `json:"body"`
	MediaArea    string   `json:"media_area"`
	Visibility   string   `json:"visibility"`
	IsNSFW       bool     `json:"is_nsfw,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	LegacyWorkID string   `json:"legacy_work_id,omitempty"`
}

// alexandriaCreateTitle is the alexandria_catalog obligation: the work's entry
// in the Alexandria catalog. A brain with no Alexandria configured owes it
// nothing. Any answer but 201 is a failure the outbox retries.
//
// Alexandria's create is not yet idempotent on legacy_work_id, so a retry after
// a request that succeeded but whose answer was lost can catalog a work twice;
// X-Idempotency-Key names the work so Alexandria can refuse the second one.
func (h *Handler) alexandriaCreateTitle(ctx context.Context, p alexandriaTitlePayload) error {
	if h.alexandriaURL == "" {
		return nil
	}
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("alexandria: encoding title for work %s: %w", p.LegacyWorkID, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.alexandriaURL+"/v1/titles", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("alexandria: building request for work %s: %w", p.LegacyWorkID, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Pial-Identity", p.AuthorPIALID)
	req.Header.Set("X-Idempotency-Key", "work:"+p.LegacyWorkID)

	resp, err := alexandriaClient.Do(req)
	if err != nil {
		return fmt.Errorf("alexandria: creating title for work %s: %w", p.LegacyWorkID, err)
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return fmt.Errorf("alexandria: reading answer for work %s: %w", p.LegacyWorkID, err)
	}
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("alexandria: creating title for work %s: HTTP %d: %s", p.LegacyWorkID, resp.StatusCode, strings.TrimSpace(string(answer)))
	}
	return nil
}

// alexandriaClient carries catalog writes; the outbox gives each attempt its own
// deadline as well.
var alexandriaClient = &http.Client{Timeout: 10 * time.Second}
