package handler

// live_edge_auth.go — who may fetch a live stream's files.
//
// The live edge serves every playlist, segment and poster straight off disk,
// so the application server never carries video. That edge used to serve them
// to anyone who could guess a path. It now asks this function first, through
// nginx auth_request and the live control plane, and serves nothing it was not
// told to: the rules a viewer meets on the watch surface are the rules the
// bytes are behind.
//
// The edge caches each answer for a few seconds per viewer per stream (see
// infra/live/nginx.conf), so this runs a few times a minute per viewer, not on
// every segment.

import (
	"errors"
	"log"
	"net/http"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// AuthorizeLiveMedia applies a stream's audience and walls to the viewer who
// sent r, and answers with the status the live edge acts on:
//
//	204  serve
//	401  this stream needs a signed-in viewer and there is none
//	403  refused — blocked, walled, not in the audience, or no such stream
//	503  the answer could not be established; fail closed
//
// The broadcaster always sees their own stream. Everyone else is refused a
// blocked stream; refused an 18+ stream unless their account is cleared for
// adult content (the same wall the watch page renders); and, for a followers
// or subscribers broadcast, must be signed in and be one. An "everyone"
// broadcast is served to anyone, signed in or not, as the watch page is.
func (h *Handler) AuthorizeLiveMedia(r *http.Request, streamID string) int {
	if h.db == nil {
		return http.StatusServiceUnavailable
	}
	s, err := dbpkg.GetLiveStreamByID(h.db, streamID)
	if err != nil {
		if errors.Is(err, dbpkg.ErrLiveStreamNotFound) {
			// The same answer as a refusal: a path is not a way to learn
			// whether a stream exists.
			return http.StatusForbidden
		}
		log.Printf("[live-edge-auth] load %s: %v", streamID, err)
		return http.StatusServiceUnavailable
	}

	// No ResponseWriter: nothing this answer carries reaches the viewer but its
	// status, so there is no cookie to clear on an invalid session.
	viewer, err := h.viewer(nil, r)
	if err != nil {
		// The identity authority could not confirm the session. A signed-in
		// viewer cannot be told apart from a stranger, so nothing is served.
		return http.StatusServiceUnavailable
	}
	return liveMediaVerdict(h, s, liveActor(viewer))
}

// liveMediaVerdict is the rule itself, over a resolved stream and viewer
// (nil when signed out).
func liveMediaVerdict(h *Handler, s *model.LiveStream, viewer *model.User) int {
	if viewer != nil && viewer.ID == s.AuthorID {
		return http.StatusNoContent
	}
	if s.IsBlocked {
		return http.StatusForbidden
	}
	if s.IsNSFW && !frequencyAdultAllowed(viewer) {
		if viewer == nil {
			return http.StatusUnauthorized
		}
		return http.StatusForbidden
	}
	switch s.Audience {
	case "followers":
		if viewer == nil {
			return http.StatusUnauthorized
		}
		following, err := dbpkg.IsFollowing(h.db, viewer.ID, s.AuthorID)
		if err != nil {
			log.Printf("[live-edge-auth] follow state %s→%s: %v", viewer.ID, s.AuthorID, err)
			return http.StatusServiceUnavailable
		}
		if !following {
			return http.StatusForbidden
		}
		return http.StatusNoContent
	case "subscribers":
		if viewer == nil {
			return http.StatusUnauthorized
		}
		subscribed, err := dbpkg.IsSubscribed(h.db, viewer.ID, s.AuthorID)
		if err != nil {
			log.Printf("[live-edge] subscription of %s to %s unreadable: %v", viewer.ID, s.AuthorID, err)
			return http.StatusServiceUnavailable
		}
		if !subscribed {
			return http.StatusForbidden
		}
		return http.StatusNoContent
	default:
		return http.StatusNoContent
	}
}
