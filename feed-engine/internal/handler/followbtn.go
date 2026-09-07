package handler

import (
	"bytes"
	"fmt"
	"html/template"
	"log"
)

// ── btn_follow — single render entry point ────────────────────────────────────
//
// A follow control is a Facet: the stream transports its rendered fragment,
// never a state delta. Before this file existed, three places produced that
// control — the btn_follow template, six fmt.Fprintf strings in followEvent,
// and a string-concatenating branch in the browser runtime — and they disagreed
// about the class name, the state class, and the hx-vals payload. The page drew
// one button and a click swapped in a different one, which is exactly the
// "follow does not stick" the owner reported. Everything below exists so that
// cannot happen: page loads, the POST /events response, and the FA Live
// fan-out all render through renderFollowButton, so the button a page draws and
// the button a click swaps in are the same button.

// followSurface names one place a follow control is drawn. A surface is a STATE
// of the one btn_follow Facet (it selects a modifier class), never a second
// Facet: profile is the full-size pill, compact the list/rail row pill, card the
// inline pill in a work_card author row, dot the overlay marker on the work
// focus avatar.
const (
	followSurfaceProfile = "profile"
	followSurfaceCompact = "compact"
	followSurfaceCard    = "card"
	followSurfaceDot     = "dot"
)

// followSurfaces is every surface the FA Live fan-out must re-render. A viewer
// can have the same person on screen in more than one shape at once (profile
// header plus a "who to follow" rail row), so the stream carries one fragment
// per shape and the runtime replaces only the elements drawn in that shape.
var followSurfaces = []string{
	followSurfaceProfile,
	followSurfaceCompact,
	followSurfaceCard,
	followSurfaceDot,
}

// normalizeFollowSurface maps the `source` form value a click reports back to a
// known surface. Unknown and empty values resolve to the profile surface — the
// full-size default — so a caller that forgets to report its surface degrades to
// a correct button instead of an unstyled one. The three legacy source values
// that predate the Facet (follow_list, suggested, post_detail) are mapped here
// so markup already rendered into an open page keeps working after deploy.
func normalizeFollowSurface(source string) string {
	switch source {
	case followSurfaceCompact, "follow_list", "suggested":
		return followSurfaceCompact
	case followSurfaceCard:
		return followSurfaceCard
	case followSurfaceDot:
		return followSurfaceDot
	default:
		return followSurfaceProfile
	}
}

// renderFollowButton produces the btn_follow fragment. This is the ONLY place a
// follow control is rendered — page loads reach it through the btn_follow
// template, and the POST /events response and the stream fan-out reach it
// through this function, so every path emits identical markup.
//
// targetPIAL is the primary address; targetHandle is carried for the aria-label
// and is the fallback address for the surfaces that only know a handle. surface
// selects the modifier class (see followSurface constants).
func (h *Handler) renderFollowButton(targetPIAL, targetHandle string, isFollowing bool, surface string) (template.HTML, error) {
	if h.partial == nil {
		return "", fmt.Errorf("btn_follow: no template set")
	}
	if targetPIAL == "" && targetHandle == "" {
		return "", fmt.Errorf("btn_follow: no target")
	}
	var buf bytes.Buffer
	err := h.partial.ExecuteTemplate(&buf, "btn_follow", map[string]interface{}{
		"TargetPIAL":   targetPIAL,
		"TargetHandle": targetHandle,
		"IsFollowing":  isFollowing,
		"Surface":      normalizeFollowSurface(surface),
	})
	if err != nil {
		return "", fmt.Errorf("btn_follow render for %s/%s: %w", targetPIAL, targetHandle, err)
	}
	if buf.Len() == 0 {
		return "", fmt.Errorf("btn_follow produced an empty fragment for %s/%s", targetPIAL, targetHandle)
	}
	return template.HTML(buf.String()), nil
}

// publishFollowState pushes the re-rendered btn_follow fragment onto the acting
// viewer's own FA Live stream, once per surface, on follow AND on unfollow.
//
// Scope is deliberate and is the whole reason this is not a broadcast: a follow
// button states "do *I* follow this person", so its fragment is true for exactly
// one viewer — the one who clicked. Sending it to the followed person, or to
// anyone else watching that person, would assert the acting viewer's
// relationship on someone else's screen. Other viewers' panels are their own
// fragments; nothing here can render them, so nothing here tries. The person
// being followed gets a notification (notifyUser), not this fragment.
//
// One event per surface, not one event carrying four fragments: the event model
// is one event = one facet mutation, and the same person can be on screen in two
// shapes at once (profile header plus a rail row). Each fragment carries the
// surface it was rendered for, and the runtime replaces only the elements drawn
// in that shape.
func (h *Handler) publishFollowState(actorPIAL, targetPIAL, targetHandle string, isFollowing bool) {
	if actorPIAL == "" {
		return
	}
	for _, surface := range followSurfaces {
		fragment, err := h.renderFollowButton(targetPIAL, targetHandle, isFollowing, surface)
		if err != nil {
			log.Printf("[follow-sse] %v", err)
			continue
		}
		PublishToUser(actorPIAL, SSEEvent{Type: "follow_state", Data: string(fragment)})
	}
}

// renderTopicFollowButton produces the btn_follow_topic fragment. Same rule as
// renderFollowButton: one entry point, so the button the tag page draws and the
// button a click swaps in are the same button.
func (h *Handler) renderTopicFollowButton(tag string, isFollowing bool) (template.HTML, error) {
	if h.partial == nil {
		return "", fmt.Errorf("btn_follow_topic: no template set")
	}
	if tag == "" {
		return "", fmt.Errorf("btn_follow_topic: no tag")
	}
	var buf bytes.Buffer
	err := h.partial.ExecuteTemplate(&buf, "btn_follow_topic", map[string]interface{}{
		"Tag":         tag,
		"IsFollowing": isFollowing,
	})
	if err != nil {
		return "", fmt.Errorf("btn_follow_topic render for #%s: %w", tag, err)
	}
	if buf.Len() == 0 {
		return "", fmt.Errorf("btn_follow_topic produced an empty fragment for #%s", tag)
	}
	return template.HTML(buf.String()), nil
}
