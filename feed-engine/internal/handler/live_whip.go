package handler

// live_whip.go — same-origin WebRTC ingest for the phone browser.
//
// The phone is the primary capture device, and a browser cannot speak RTMP or
// SRT. It speaks WebRTC, and WHIP is the standard way to publish one: POST an
// SDP offer, receive an SDP answer.
//
// The browser is never given a publish credential. It posts its offer here; the
// server checks the session owns the stream, mints a single-use publish ticket
// that lives only in this process, and presents that ticket to the media server
// on the broadcaster's behalf. The stream's long-lived ingest key — the one an
// OBS user configured — is not touched and never reaches a Facet.
//
// Only the SDP handshake passes through this process. The media itself goes
// browser -> media server over WebRTC and never touches the application server.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/live"
	"github.com/f33d3r/feed-engine/internal/model"
)

// maxSDPBytes caps one offer. A large SDP is a malformed SDP.
const maxSDPBytes = 256 << 10

var whipClient = &http.Client{Timeout: 15 * time.Second}

// liveWhipPublish proxies a WHIP offer for a stream the session owns.
// POST /live/{id}/whip   Content-Type: application/sdp
func (h *Handler) liveWhipPublish(w http.ResponseWriter, r *http.Request) {
	stream, ok := h.publishableLiveStream(w, r)
	if !ok {
		return
	}

	offer, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSDPBytes))
	if err != nil {
		http.Error(w, "offer too large or unreadable", http.StatusBadRequest)
		return
	}
	if len(bytes.TrimSpace(offer)) == 0 {
		http.Error(w, "empty SDP offer", http.StatusBadRequest)
		return
	}

	ticket, err := live.MintPublishTicket(stream.ID)
	if err != nil {
		log.Printf("[live-whip] minting ticket for %s: %v", stream.ID, err)
		http.Error(w, "could not authorise the broadcast", http.StatusInternalServerError)
		return
	}

	target := fmt.Sprintf("%s?user=%s&pass=%s",
		live.InternalWHIPURL(stream.ID), url.QueryEscape(stream.ID), url.QueryEscape(ticket))

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(offer))
	if err != nil {
		http.Error(w, "could not reach the media server", http.StatusBadGateway)
		return
	}
	req.Header.Set("Content-Type", "application/sdp")

	resp, err := whipClient.Do(req)
	if err != nil {
		log.Printf("[live-whip] media server unreachable for %s: %v", stream.ID, err)
		http.Error(w, "the media server is not reachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxSDPBytes))
	if err != nil {
		http.Error(w, "could not read the media server answer", http.StatusBadGateway)
		return
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		log.Printf("[live-whip] media server refused %s: status %d: %s",
			stream.ID, resp.StatusCode, strings.TrimSpace(string(answer)))
		http.Error(w, "the media server refused the broadcast", http.StatusBadGateway)
		return
	}

	// The WHIP resource lives at the media server. Rewrite its Location to a
	// same-origin path so the browser's teardown stays same-origin too.
	if loc := resp.Header.Get("Location"); loc != "" {
		if id := whipSessionID(loc); id != "" {
			w.Header().Set("Location", "/live/"+stream.ID+"/whip/"+id)
		}
	}
	if etag := resp.Header.Get("ETag"); etag != "" {
		w.Header().Set("ETag", etag)
	}
	w.Header().Set("Content-Type", "application/sdp")
	w.WriteHeader(http.StatusCreated)
	if _, err := w.Write(answer); err != nil {
		log.Printf("[live-whip] writing answer for %s: %v", stream.ID, err)
	}
}

// liveWhipTeardown ends a browser broadcast's WebRTC session.
// DELETE /live/{id}/whip/{session}
// Teardown is deliberately not gated on the stream still being publishable. The
// moment this request matters most is the moment the broadcast is already over:
// the server ended the row, or the media server dropped the publisher, and the
// browser is releasing the WebRTC session it still holds. Refusing that with a
// 409 leaves a live peer connection at the media server with nothing to do,
// which is the opposite of what the request asked for. Ownership is the whole
// question here, and ownership does not expire when a broadcast does.
func (h *Handler) liveWhipTeardown(w http.ResponseWriter, r *http.Request) {
	stream, ok := h.ownedLiveStream(w, r)
	if !ok {
		return
	}
	session := r.PathValue("session")
	if session == "" {
		http.Error(w, "session required", http.StatusBadRequest)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodDelete,
		live.InternalWHIPURL(stream.ID)+"/"+url.PathEscape(session), nil)
	if err != nil {
		http.Error(w, "could not reach the media server", http.StatusBadGateway)
		return
	}
	resp, err := whipClient.Do(req)
	if err != nil {
		log.Printf("[live-whip] teardown for %s unreachable: %v", stream.ID, err)
		http.Error(w, "the media server is not reachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	w.WriteHeader(http.StatusNoContent)
}

// publishableLiveStream is the gate for every surface that starts or renews a
// publish: the WHIP proxy and the encoder credential reveal alike. It is
// ownership plus the two conditions that make a broadcast publishable at all —
// it has not ended, and it is not blocked.
func (h *Handler) publishableLiveStream(w http.ResponseWriter, r *http.Request) (*model.LiveStream, bool) {
	stream, ok := h.ownedLiveStream(w, r)
	if !ok {
		return nil, false
	}
	if stream.Status == model.LiveStatusEnded {
		http.Error(w, "this broadcast has ended", http.StatusConflict)
		return nil, false
	}
	if stream.IsBlocked || stream.ScanState == "blocked" {
		http.Error(w, "this broadcast is blocked", http.StatusForbidden)
		return nil, false
	}
	return stream, true
}

// ownedLiveStream resolves the stream named in the path and proves the session
// owns it. It answers exactly one question — is this the broadcaster's own
// stream — because that is the only question every caller shares. Whether the
// broadcast may still be published to is a separate question, asked separately
// by publishableLiveStream, so that releasing a finished broadcast's resources
// is never refused on the grounds that the broadcast is finished.
func (h *Handler) ownedLiveStream(w http.ResponseWriter, r *http.Request) (*model.LiveStream, bool) {
	if h.db == nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return nil, false
	}
	user := liveActor(h.userFromRequest(w, r))
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil, false
	}
	stream, err := dbpkg.GetLiveStreamByID(h.db, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, dbpkg.ErrLiveStreamNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return nil, false
		}
		log.Printf("[live-whip] loading stream: %v", err)
		http.Error(w, "stream lookup failed", http.StatusInternalServerError)
		return nil, false
	}
	if stream.AuthorID != user.ID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return nil, false
	}
	return stream, true
}

// whipSessionID pulls the resource id out of the media server's Location header,
// which may be absolute or relative.
func whipSessionID(location string) string {
	trimmed := strings.TrimRight(location, "/")
	if i := strings.LastIndex(trimmed, "/"); i >= 0 {
		return trimmed[i+1:]
	}
	return trimmed
}
