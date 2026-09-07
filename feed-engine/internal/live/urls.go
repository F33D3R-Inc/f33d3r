package live

// urls.go — every URL in the live lane, in one place.
//
// Two families live here and they never mix:
//
//   Playback URLs are public, carry no secret, and are served by the live edge.
//   Publish  URLs carry the ingest key and are shown once to the broadcaster.
//
// The ingest key is never the stream id, and a publish URL is never rendered
// into a Facet that a viewer can reach.

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// ── playback ─────────────────────────────────────────────────────────────────

// MasterPlaylistURL is the ABR master playlist for a stream.
func MasterPlaylistURL(streamID string) string {
	return "/live/" + streamID + "/master.m3u8"
}

// VariantPlaylistURL is one rung of the ladder.
func VariantPlaylistURL(streamID string, height int) string {
	return fmt.Sprintf("/live/%s/%dp/index.m3u8", streamID, height)
}

// PosterURL is the periodically refreshed still frame for a stream.
func PosterURL(streamID string) string {
	return "/live/" + streamID + "/poster.jpg"
}

// ── publish ──────────────────────────────────────────────────────────────────

// endpoints describes where a broadcaster sends video. These are the media
// server's addresses as reached from outside the compose network, which is not
// the same as the address this process uses to reach its API.
//
// There is no public WHIP endpoint here. The browser leg posts its offer to
// this process, same-origin, and this process presents it to the media server
// with a ticket (InternalWHIPURL, ticket.go); a WHIP URL that carried the
// ingest key in its query string would put the key in access logs and
// Referer headers, which is why it is never minted.
type endpoints struct {
	rtmpBase string // e.g. rtmp://live.f33d3r.com:1935
	srtHost  string // e.g. live.f33d3r.com:8890
}

func loadEndpoints() endpoints {
	return endpoints{
		rtmpBase: envOr("LIVE_RTMP_BASE", "rtmp://localhost:1935"),
		srtHost:  envOr("LIVE_SRT_HOST", "localhost:8890"),
	}
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// mediaPathPrefix is the media server's path namespace for live ingest. The
// path is the stream id under this prefix; the ingest key is never in a path.
const mediaPathPrefix = "live"

// mediaPath is the media server path for a stream.
func mediaPath(streamID string) string { return mediaPathPrefix + "/" + streamID }

// RTMPServerURL is the "Server" field of an OBS custom stream target.
func RTMPServerURL() string {
	return strings.TrimRight(loadEndpoints().rtmpBase, "/") + "/" + mediaPathPrefix
}

// RTMPStreamKey is the "Stream Key" field of an OBS custom stream target. The
// path is the stream id and the secret rides as query credentials, which every
// RTMP encoder passes through unchanged.
func RTMPStreamKey(streamID, ingestKey string) string {
	return fmt.Sprintf("%s?user=%s&pass=%s",
		streamID, url.QueryEscape(streamID), url.QueryEscape(ingestKey))
}

// SRTPublishURL is a single-field SRT caller URL for a low-latency hardware or
// mobile encoder. MediaMTX reads the credentials out of the streamid.
func SRTPublishURL(streamID, ingestKey string) string {
	e := loadEndpoints()
	sid := fmt.Sprintf("publish:%s:%s:%s", mediaPath(streamID), streamID, ingestKey)
	return fmt.Sprintf("srt://%s?streamid=%s&latency=120000", e.srtHost, url.QueryEscape(sid))
}

// InternalWHIPURL is the media server's own WHIP endpoint for a stream, reached
// over the container network. Only the server calls this: the browser talks to
// a same-origin proxy so no publish credential ever reaches a page.
func InternalWHIPURL(streamID string) string {
	base := strings.TrimRight(envOr("LIVE_MEDIA_WHIP_URL", "http://mediamtx:8889"), "/")
	return base + "/" + mediaPath(streamID) + "/whip"
}

// PublishTargets is the complete set of ways to broadcast into one stream.
// It carries the ingest key and must only ever be rendered to the stream owner.
type PublishTargets struct {
	RTMPServer    string
	RTMPStreamKey string
	SRTURL        string
}

// TargetsFor builds the publish set for a freshly minted or rotated key.
func TargetsFor(streamID, ingestKey string) PublishTargets {
	return PublishTargets{
		RTMPServer:    RTMPServerURL(),
		RTMPStreamKey: RTMPStreamKey(streamID, ingestKey),
		SRTURL:        SRTPublishURL(streamID, ingestKey),
	}
}
