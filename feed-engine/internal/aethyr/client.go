package aethyr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/f33d3r/feed-engine/internal/model"
)

type Client struct {
	baseURL string
	http    *http.Client
}

func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{
		baseURL: baseURL,
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

type RankRequest = model.AethyrRankRequest

// InternalKeyHeader authenticates every call but /health to AethyrRank.
const InternalKeyHeader = "X-Internal-Key"

// Rank asks AethyrRank to order one candidate window. internalKey is the
// shared INTERNAL_API_KEY; AethyrRank refuses /rank without it. Ranking
// signals do not go through this client: they are ranking.events (relay.go).
func (c *Client) Rank(ctx context.Context, internalKey string, req *RankRequest) (*model.AethyrRankResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("aethyr rank marshal: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/rank", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("aethyr rank request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(InternalKeyHeader, internalKey)

	start := time.Now()
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("aethyr rank do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("aethyr rank status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("aethyr rank read: %w", err)
	}
	var result model.AethyrRankResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("aethyr rank unmarshal: %w", err)
	}
	log.Printf("[aethyr] rank ok: %d items, engine=%dms, roundtrip=%dms",
		len(result.RankedItems), result.LatencyMs, time.Since(start).Milliseconds())
	return &result, nil
}

func (c *Client) Health(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
