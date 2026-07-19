package handler

// facets.go — D-070 read lane: GET /facets/{name}
// Each handler returns a rendered Facet HTML fragment.  Never JSON, never a full page shell.

import (
	"net/http"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
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

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=60")
	h.renderPartial(w, "quoted_work", work)
}
