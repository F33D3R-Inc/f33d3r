package nexus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Client calls Verity's NEXUS endpoints.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewClient creates a new NEXUS Verity client.
func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL:    baseURL,
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	}
}

// TierResponse is returned by GET /v1/nexus/tier/:shard.
type TierResponse struct {
	PIALShardID string `json:"pial_shard_id"`
	Tier        int    `json:"tier"`
	TierSource  string `json:"tier_source"`
	IsSuspended bool   `json:"is_suspended"`
}

// GetTier calls GET /v1/nexus/tier/:shard and returns the tier for a PIAL.
// Returns tier=0 and no error if the PIAL has no NEXUS.
func (c *Client) GetTier(pialShard string) (TierResponse, error) {
	resp, err := c.HTTPClient.Get(fmt.Sprintf("%s/v1/nexus/tier/%s", c.BaseURL, pialShard))
	if err != nil {
		return TierResponse{Tier: 0}, err
	}
	defer resp.Body.Close()
	var tr TierResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return TierResponse{Tier: 0}, err
	}
	return tr, nil
}

// VerifyRequest is the payload sent to POST /v1/nexus/verify.
type VerifyRequest struct {
	PIALShardID   string  `json:"pial_shard_id"`
	BiometricHash string  `json:"biometric_hash"`
	DocumentHash  string  `json:"document_hash"`
	Confidence    float64 `json:"confidence"`
	Jurisdiction  string  `json:"jurisdiction"`
}

// VerifyResponse is returned by POST /v1/nexus/verify.
type VerifyResponse struct {
	Result    string `json:"result"`      // "created" | "already_linked" | "match_found"
	Tier      int    `json:"tier"`
	RequestID string `json:"request_id"`  // only when result == "match_found"
}

// Verify calls POST /v1/nexus/verify to create or find a NEXUS identity.
func (c *Client) Verify(req VerifyRequest) (VerifyResponse, error) {
	body, _ := json.Marshal(req)
	resp, err := c.HTTPClient.Post(c.BaseURL+"/v1/nexus/verify", "application/json", bytes.NewReader(body))
	if err != nil {
		return VerifyResponse{}, err
	}
	defer resp.Body.Close()
	var vr VerifyResponse
	json.NewDecoder(resp.Body).Decode(&vr)
	return vr, nil
}

// PersonaEntry is one linked persona from Verity's GET /v1/nexus/personas.
type PersonaEntry struct {
	PIALShardID  string `json:"pial_shard_id"`
	PersonaType  string `json:"persona_type"`
	DisplayLabel string `json:"display_label"`
	IsPrimary    bool   `json:"is_primary"`
}

// PersonasResponse is the response from GET /v1/nexus/personas.
type PersonasResponse struct {
	Personas []PersonaEntry `json:"personas"`
	Tier     int            `json:"tier"`
	Count    int            `json:"count"`
}

// ListPersonas calls GET /v1/nexus/personas?pial_shard_id={shard}.
// Returns nil, nil when the shard is not linked to any NEXUS (single-account).
func (c *Client) ListPersonas(pialShard string) (*PersonasResponse, error) {
	url := fmt.Sprintf("%s/v1/nexus/personas?pial_shard_id=%s", c.BaseURL, pialShard)
	resp, err := c.HTTPClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, nil
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("verity /v1/nexus/personas returned %d", resp.StatusCode)
	}
	var pr PersonasResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

// ConfirmLinkRequest is the payload for POST /v1/nexus/link/confirm.
type ConfirmLinkRequest struct {
	RequestID    string `json:"request_id"`
	PIALShardID  string `json:"pial_shard_id"`
}

// ConfirmLink calls POST /v1/nexus/link/confirm.
func (c *Client) ConfirmLink(requestID, pialShard string) error {
	body, _ := json.Marshal(ConfirmLinkRequest{RequestID: requestID, PIALShardID: pialShard})
	resp, err := c.HTTPClient.Post(c.BaseURL+"/v1/nexus/link/confirm", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("verity /v1/nexus/link/confirm returned %d", resp.StatusCode)
	}
	return nil
}

// DeclineLinkRequest is the payload for POST /v1/nexus/link/decline.
type DeclineLinkRequest struct {
	RequestID   string `json:"request_id"`
	PIALShardID string `json:"pial_shard_id"`
}

// DeclineLink calls POST /v1/nexus/link/decline.
func (c *Client) DeclineLink(requestID, pialShard string) error {
	body, _ := json.Marshal(DeclineLinkRequest{RequestID: requestID, PIALShardID: pialShard})
	resp, err := c.HTTPClient.Post(c.BaseURL+"/v1/nexus/link/decline", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("verity /v1/nexus/link/decline returned %d", resp.StatusCode)
	}
	return nil
}

// InitiateLinkRequest is the payload for POST /v1/nexus/link/initiate.
type InitiateLinkRequest struct {
	SourcePIALShardID string `json:"source_pial_shard_id"`
	TargetPIALShardID string `json:"target_pial_shard_id"`
	PersonaType       string `json:"persona_type"`
}

// InitiateLinkResponse is returned by POST /v1/nexus/link/initiate.
type InitiateLinkResponse struct {
	Result    string `json:"result"`     // "pending" | "conflict"
	RequestID string `json:"request_id"`
}

// InitiateLink calls POST /v1/nexus/link/initiate.
// Returns the pending request_id on success.
func (c *Client) InitiateLink(sourceShard, targetShard string) (InitiateLinkResponse, error) {
	body, _ := json.Marshal(InitiateLinkRequest{
		SourcePIALShardID: sourceShard,
		TargetPIALShardID: targetShard,
		PersonaType:       "personal",
	})
	resp, err := c.HTTPClient.Post(c.BaseURL+"/v1/nexus/link/initiate", "application/json", bytes.NewReader(body))
	if err != nil {
		return InitiateLinkResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 409 {
		return InitiateLinkResponse{Result: "conflict"}, nil
	}
	if resp.StatusCode != 200 {
		return InitiateLinkResponse{}, fmt.Errorf("verity /v1/nexus/link/initiate returned %d", resp.StatusCode)
	}
	var r InitiateLinkResponse
	json.NewDecoder(resp.Body).Decode(&r)
	return r, nil
}

// PendingLinkRequest is one incoming link request from Verity.
type PendingLinkRequest struct {
	RequestID          string `json:"request_id"`
	SourcePIALShardID  string `json:"source_pial_shard_id"`
	InitiatedAt        string `json:"initiated_at"`
	ExpiresAt          string `json:"expires_at"`
}

// PendingLinksResponse is returned by GET /v1/nexus/link/pending.
type PendingLinksResponse struct {
	Requests []PendingLinkRequest `json:"requests"`
	Count    int                  `json:"count"`
}

// ListPendingLinks calls GET /v1/nexus/link/pending?target_pial_shard_id={shard}.
// Returns incoming link requests the user has not yet accepted or declined.
func (c *Client) ListPendingLinks(targetShard string) (*PendingLinksResponse, error) {
	url := fmt.Sprintf("%s/v1/nexus/link/pending?target_pial_shard_id=%s", c.BaseURL, targetShard)
	resp, err := c.HTTPClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("verity /v1/nexus/link/pending returned %d", resp.StatusCode)
	}
	var pr PendingLinksResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, err
	}
	return &pr, nil
}
