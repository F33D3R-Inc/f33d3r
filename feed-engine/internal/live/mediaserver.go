package live

// mediaserver.go — a thin client for the media server's control API.
//
// This is a control plane only. No video byte ever crosses this client: it asks
// the media server what is publishing and tells it to drop a publisher. The
// pixels go ingest -> media server -> transcoder -> edge and never through this
// process, which is the difference between working at 5 viewers and 5,000.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// MediaServer talks to the MediaMTX control API over the internal network.
type MediaServer struct {
	baseURL string
	client  *http.Client
}

// NewMediaServer builds a client for the address in LIVE_MEDIA_API_URL.
func NewMediaServer() *MediaServer {
	return &MediaServer{
		baseURL: strings.TrimRight(envOr("LIVE_MEDIA_API_URL", "http://mediamtx:9997"), "/"),
		client:  &http.Client{Timeout: 5 * time.Second},
	}
}

// PathSource identifies whatever is currently publishing into a path.
type PathSource struct {
	Type string `json:"type"` // rtmpConn | webRTCSession | srtConn | rtspSession
	ID   string `json:"id"`
}

// Path is one ingest path as the media server sees it.
type Path struct {
	Name          string     `json:"name"`
	Ready         bool       `json:"ready"`
	ReadyTime     *time.Time `json:"readyTime"`
	Source        PathSource `json:"source"`
	BytesReceived int64      `json:"bytesReceived"`
	Tracks        []string   `json:"tracks"`
}

type pathList struct {
	ItemCount int    `json:"itemCount"`
	PageCount int    `json:"pageCount"`
	Items     []Path `json:"items"`
}

// Paths returns every path the media server currently holds.
func (m *MediaServer) Paths(ctx context.Context) ([]Path, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.baseURL+"/v3/paths/list?itemsPerPage=1000", nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("live: media server paths: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("live: media server paths: status %d", resp.StatusCode)
	}
	var out pathList
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("live: media server paths decode: %w", err)
	}
	return out.Items, nil
}

// PublishingIDs returns the set of path names that currently have a live
// publisher, so the database's idea of "live" can be reconciled against the
// media server's.
func (m *MediaServer) PublishingIDs(ctx context.Context) (map[string]bool, error) {
	paths, err := m.Paths(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(paths))
	for _, p := range paths {
		if p.Ready && p.Source.ID != "" {
			// Media server paths are namespaced ("live/<id>"); the database
			// knows streams by bare id. Compare on the same shape or every
			// live stream looks orphaned.
			out[strings.TrimPrefix(p.Name, mediaPathPrefix+"/")] = true
		}
	}
	return out, nil
}

// kickEndpoint maps a publisher's connection type to the API collection that
// can drop it. An unknown type is an error, never a silent no-op.
func kickEndpoint(sourceType string) (string, error) {
	switch sourceType {
	case "rtmpConn":
		return "rtmpconns", nil
	case "rtspSession":
		return "rtspsessions", nil
	case "srtConn":
		return "srtconns", nil
	case "webRTCSession":
		return "webrtcsessions", nil
	default:
		return "", fmt.Errorf("live: unknown publisher type %q", sourceType)
	}
}

// KickPublisher drops whatever is publishing into a path. This is how a
// moderation block actually stops a broadcast: the decision is taken here and
// enforced at the media server, not by hiding a Facet.
func (m *MediaServer) KickPublisher(ctx context.Context, streamID string) error {
	paths, err := m.Paths(ctx)
	if err != nil {
		return err
	}
	for _, p := range paths {
		if p.Name != mediaPath(streamID) {
			continue
		}
		if p.Source.ID == "" {
			return nil // nothing publishing; already stopped
		}
		collection, err := kickEndpoint(p.Source.Type)
		if err != nil {
			return err
		}
		url := fmt.Sprintf("%s/v3/%s/kick/%s", m.baseURL, collection, p.Source.ID)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
		if err != nil {
			return err
		}
		resp, err := m.client.Do(req)
		if err != nil {
			return fmt.Errorf("live: kick publisher: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("live: kick publisher %s: status %d", streamID, resp.StatusCode)
		}
		return nil
	}
	return nil // path not present; nothing to kick
}
