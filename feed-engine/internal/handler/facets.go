package handler

// facets.go — D-070 read lane: GET /facets/{name}
// Each handler returns a rendered Facet HTML fragment.  Never JSON, never a full page shell.

import (
	"net/http"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// facetLinkPreview — GET /facets/link_preview?url=...
// Fetches Open Graph metadata for the given URL (cache-first) and returns the
// rendered link_preview Facet HTML so the compose modal can preview link cards
// before the user posts.
func (h *Handler) facetLinkPreview(w http.ResponseWriter, r *http.Request) {
	rawURL := r.URL.Query().Get("url")
	if rawURL == "" {
		http.Error(w, "", http.StatusBadRequest)
		return
	}

	// Serve from cache when fresh (7-day TTL in DB)
	if cached, err := dbpkg.GetCachedLinkPreview(h.db, rawURL); err == nil && cached != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		h.renderPartial(w, "link_preview", cached)
		return
	}

	// Live fetch
	preview, err := fetchOGMetadata(rawURL)
	if err != nil || preview == nil {
		// 204: compose JS treats no-content as "no preview available"
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Persist for future requests
	_ = dbpkg.UpsertLinkPreview(h.db, preview)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	h.renderPartial(w, "link_preview", preview)
}

// facetQuotedPost — GET /facets/quoted_post?post_id=...
// Returns the rendered quoted_work embed Facet so the compose modal can preview
// the work being quoted — the full, playable render, identical to the in-feed
// quote. (Route/param keep the legacy "post" names; the payload is a Work.)
func (h *Handler) facetQuotedPost(w http.ResponseWriter, r *http.Request) {
	postID := r.URL.Query().Get("post_id")
	if postID == "" {
		http.Error(w, "", http.StatusBadRequest)
		return
	}

	work, err := dbpkg.GetWorkByID(h.db, postID, "")
	if err != nil || work == nil {
		http.Error(w, "", http.StatusNotFound)
		return
	}
	// The work being quoted may itself be a quote. Recall its citation chain so
	// the preview draws the same nested rail the posted work will draw — the
	// composer must never show a quote the feed renders differently.
	dbpkg.EnrichWorksWithQuotes(h.db, []*model.Work{work})

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=60")
	h.renderPartial(w, "quoted_work", work)
}

// facetReportSheet — GET /facets/report_sheet?post_id=<uuid>
// The report overlay for one work: a form whose submission is POST /api/report.
// Opened into #overlay-slot by btn_report and the work menu.
func (h *Handler) facetReportSheet(w http.ResponseWriter, r *http.Request) {
	postID := r.URL.Query().Get("post_id")
	if _, err := uuid.Parse(postID); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	h.renderPartial(w, "report_flow_sheet", map[string]interface{}{"PostID": postID})
}

// facetTranslateSlot — GET /facets/work/{id}/translate_slot
// The empty translation slot of a work card. A translation renders into the
// slot's place (translate_result, translate_error); hiding it asks for the
// empty slot back, so the card returns to exactly what the server first drew.
func (h *Handler) facetTranslateSlot(w http.ResponseWriter, r *http.Request) {
	postID := r.PathValue("id")
	if _, err := uuid.Parse(postID); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	h.renderPartial(w, "translate_slot", map[string]interface{}{"PostID": postID})
}
