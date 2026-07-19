package handler

import (
	"fmt"
	"log"
	"net/http"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// visionCameraPage — GET /visions
// Renders the vision camera page (Layer 4 assembler).
func (h *Handler) visionCameraPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	h.render(w, "vision_camera.html", map[string]interface{}{
		"User": user,
	})
}

// visionsFeedPage — GET /visions
// Renders the Visions media feed page (Layer 4 assembler).
func (h *Handler) visionsFeedPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	h.render(w, "visions.html", map[string]interface{}{
		"User": user,
	})
}

// facetVisionsFeed — GET /facets/visions/feed
// Returns rendered vision_card HTML fragments for the Visions feed.
// Query params: surface=following|for_you, before=<RFC3339 cursor>
func (h *Handler) facetVisionsFeed(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	surface := r.URL.Query().Get("surface")
	before := r.URL.Query().Get("before")

	var works []*model.Work
	var err error
	const limit = 20
	if surface == "for_you" {
		works, err = dbpkg.GetVisionsWorksForYou(h.db, limit, before)
	} else {
		works, err = dbpkg.GetVisionsWorksFeed(h.db, user.ID, limit, before)
	}
	if err != nil {
		log.Printf("[visions-feed] %v", err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	dbpkg.EnrichWorksWithReactions(h.db, works, user.ID)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if len(works) == 0 {
		h.renderPartial(w, "stateEmpty", map[string]interface{}{
			"Title": "Nothing here yet",
			"Sub":   "Visions from people you follow will appear here",
		})
		return
	}
	for _, wk := range works {
		h.renderPartial(w, "vision_card", wk)
	}
	last := works[len(works)-1]
	fmt.Fprintf(w,
		`<div class="feed-sentinel" hx-get="/facets/visions/feed?surface=%s&before=%s" hx-trigger="revealed" hx-swap="outerHTML" aria-hidden="true"><div class="spinner" style="margin:24px auto"></div></div>`,
		surface,
		last.CreatedAt.Format(time.RFC3339),
	)
}
