package handler

import (
	"net/http"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// composeQuotePickerBookmarks serves GET /partials/compose/quote-picker/bookmarks
func (h *Handler) composeQuotePickerBookmarks(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	works, _ := dbpkg.GetWorksSaves(h.db, user.ID, 20, "")
	for _, wk := range works {
		wk.TimeAgo = TimeAgo(wk.CreatedAt)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderComposeQuoteWorkRows(w, works, "No bookmarks yet")
}

// composeQuotePickerLikes serves GET /partials/compose/quote-picker/likes
func (h *Handler) composeQuotePickerLikes(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	works, _ := dbpkg.GetWorksLikedByUser(h.db, user.ID, 20)
	for _, wk := range works {
		wk.TimeAgo = TimeAgo(wk.CreatedAt)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderComposeQuoteWorkRows(w, works, "No liked works yet")
}

// composeQuotePickerYours serves GET /partials/compose/quote-picker/yours
func (h *Handler) composeQuotePickerYours(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	works, _ := dbpkg.GetWorksForProfile(h.db, user.PIALID, 20, "")
	for _, wk := range works {
		wk.TimeAgo = TimeAgo(wk.CreatedAt)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderComposeQuoteWorkRows(w, works, "No posts yet")
}

func (h *Handler) renderComposeQuoteWorkRows(w http.ResponseWriter, works []*model.Work, emptyMsg string) {
	if len(works) == 0 {
		h.renderPartial(w, "compose_quote_empty", emptyMsg)
		return
	}
	for _, wk := range works {
		h.renderPartial(w, "compose_quote_row", wk)
	}
}
