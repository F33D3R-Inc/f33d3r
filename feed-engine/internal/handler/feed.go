package handler

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/realm"
)

func (h *Handler) indexPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	user := h.userFromRequest(w, r)
	rail := h.railData(user, "feed")
	var pinnedSurfaces []model.FeedSurface
	if h.db != nil && user != nil && user.ID != "" {
		pinnedSurfaces, _ = dbpkg.GetUserSurfaces(h.db, user.ID)
	}
	h.render(w, "index.html", map[string]interface{}{
		"User":            user,
		"Surface":         "feed",
		"SessionID":       uuid.New().String(),
		"Title":           "Home",
		"ShowScores":      h.cfg.ShowScores,
		"Themes":          ThemesWithActive(user.ThemeID),
		"EngineOnline":    h.engineOnline(r.Context()),
		"TrendingTags":    rail["TrendingTags"],
		"SuggestedUsers":  rail["SuggestedUsers"],
		"RailContext":     rail["RailContext"],
		"RailCreators":    rail["RailCreators"],
		"RailNewsItems":   rail["RailNewsItems"],
		"RailNewsLabel":   rail["RailNewsLabel"],
		"PinnedSurfaces":  pinnedSurfaces,
	})
}

func (h *Handler) explorePage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	rail := h.railData(user, "explore")
	h.render(w, "explore.html", map[string]interface{}{
		"User":           user,
		"Surface":        "explore",
		"SessionID":      uuid.New().String(),
		"Title":          "Explore",
		"ShowScores":     h.cfg.ShowScores,
		"Themes":         ThemesWithActive(user.ThemeID),
		"TrendingTags":   rail["TrendingTags"],
		"SuggestedUsers": rail["SuggestedUsers"],
		"RailContext":    rail["RailContext"],
		"RailCreators":   rail["RailCreators"],
		"RailNewsItems":  rail["RailNewsItems"],
		"RailNewsLabel":  rail["RailNewsLabel"],
	})
}

// ── HTMX: feed partial ────────────────────────────────────────────────────────

// feedPartial — GET /feed
// Now handles ONLY: articles (global) and profile_articles (by handle/user_id).
// All post/work surfaces have moved to GET /facets/works/feed.
func (h *Handler) feedPartial(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	surface   := r.URL.Query().Get("surface")
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" { sessionID = uuid.New().String() }
	after  := r.URL.Query().Get("after")
	userID := r.URL.Query().Get("user_id")
	user   := h.userFromRequest(w, r)

	// Global published articles feed.
	if surface == "articles" && h.db != nil {
		articles, err := dbpkg.GetPublishedArticles(h.db, h.cfg.FeedPageSize+1, after)
		if err != nil {
			log.Printf("[feed] published articles: %v", err)
		}
		var nextAfter string
		if len(articles) > h.cfg.FeedPageSize {
			articles = articles[:h.cfg.FeedPageSize]
			nextAfter = articles[len(articles)-1].ID
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		h.renderPartial(w, "article_feed", map[string]interface{}{
			"Articles":  articles,
			"After":     nextAfter,
			"Surface":   surface,
			"SessionID": sessionID,
		})
		return
	}

	// Profile articles — user's published articles by handle or user_id.
	if surface == "profile_articles" && h.db != nil {
		targetID := userID
		if targetID == "" && user != nil { targetID = user.ID }
		handle := strings.TrimSpace(r.URL.Query().Get("handle"))
		if targetID == "" && handle != "" {
			if tu, err := dbpkg.GetUserByHandle(h.db, handle); err == nil && tu != nil {
				targetID = tu.ID
			}
		}
		articles, err := dbpkg.GetUserArticles(h.db, targetID, h.cfg.FeedPageSize+1, after)
		if err != nil {
			log.Printf("[feed] user articles: %v", err)
		}
		var nextAfter string
		if len(articles) > h.cfg.FeedPageSize {
			articles = articles[:h.cfg.FeedPageSize]
			nextAfter = articles[len(articles)-1].ID
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		h.renderPartial(w, "article_feed", map[string]interface{}{
			"Articles":  articles,
			"After":     nextAfter,
			"Surface":   surface,
			"SessionID": sessionID,
		})
		return
	}

	http.NotFound(w, r)
}

// ── HTMX: interaction actions ─────────────────────────────────────────────────

// publishPostEngagementSSE broadcasts a post_engagement SSE event to all sessions watching a post.
// TODO_WORKS: migrate to GetWorkByID + works-based action_bar_detail template.
func (h *Handler) publishPostEngagementSSE(postID, actorUserID string) {
	// no-op until action_bar_detail is ported to model.Work
}

func (h *Handler) likeAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "405", 405); return }
	r.ParseForm()
	cid, sid, surf := r.FormValue("content_id"), r.FormValue("session_id"), r.FormValue("surface")
	user := h.userFromRequest(w, r)

	liked := false
	if h.db != nil {
		liked, _ = dbpkg.ToggleLike(h.db, user.ID, cid)
		if liked {
			go realm.AwardXP(h.db, user.ID, "like", cid, realm.XPLike)
		}
	} else {
		_ = dbpkg.IncrementLike(h.db, cid)
		liked = true
	}

	h.aethyr.SendFeedback(&model.AethyrFeedbackRequest{
		UserID: user.ID, SessionID: sid, Surface: surf,
		Events: []model.AethyrFeedbackEvent{{
			ContentID: cid, EventType: "like",
			PositionAtDisplay: 0, Timestamp: time.Now(),
		}},
	})

	// FA Live: push full action_bar_detail fragment to all sessions watching this post.
	go h.publishPostEngagementSSE(cid, user.ID)
	// Sitra Achra: publish ranking event (fire-and-forget).
	if liked {
		go h.publishRankingEvent("like", cid, user.PIALID)
	} else {
		go h.publishRankingEvent("unlike", cid, user.PIALID)
	}

	// Re-render the canonical like_btn facet markup (see _action_bar.html) so the
	// filled state, count, AND celebration effects (which key off .action.like.is-on)
	// survive the HTMX swap. Previously this returned a drifted `action-btn liked`
	// fragment, which broke the celebrate-* like animation.
	activeClass := ""
	fillVal := "none"
	if liked {
		activeClass = " is-on"
		fillVal = "currentColor"
	}
	count := 0
	if h.db != nil {
		count = dbpkg.GetLikeCount(h.db, cid)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<button class="action action-btn like%s" hx-post="/feed/item/like" hx-target="this" hx-swap="outerHTML" hx-include="[name='session_id'],[name='surface']" onclick="event.stopPropagation()" name="content_id" value="%s" title="Like" aria-label="Like" aria-pressed="%t"><svg viewBox="0 0 24 24" fill="%s" stroke="currentColor" stroke-width="1.8"><path d="M20.84 4.61a5.5 5.5 0 0 0-7.78 0L12 5.67l-1.06-1.06a5.5 5.5 0 0 0-7.78 7.78l1.06 1.06L12 21.23l7.78-7.78 1.06-1.06a5.5 5.5 0 0 0 0-7.78z"/></svg><span id="post-like-count-%s">%d</span></button>`,
		activeClass, cid, liked, fillVal, cid, count)
}

func (h *Handler) saveAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "405", 405); return }
	r.ParseForm()
	cid, sid, surf := r.FormValue("content_id"), r.FormValue("session_id"), r.FormValue("surface")
	user := h.userFromRequest(w, r)

	saved := true
	if h.db != nil {
		already, _ := dbpkg.IsBookmarked(h.db, user.ID, cid)
		if already {
			_ = dbpkg.RemoveBookmark(h.db, user.ID, cid)
			saved = false
		} else {
			_ = dbpkg.AddBookmark(h.db, user.ID, cid)
		}
	}
	if saved {
		h.aethyr.SendFeedback(&model.AethyrFeedbackRequest{
			UserID: user.ID, SessionID: sid, Surface: surf,
			Events: []model.AethyrFeedbackEvent{{
				ContentID: cid, EventType: "save",
				PositionAtDisplay: 0, Timestamp: time.Now(),
			}},
		})
	}
	// FA Live: push full action_bar_detail fragment to all sessions watching this post.
	go h.publishPostEngagementSSE(cid, user.ID)
	count := 0
	if h.db != nil {
		_ = h.db.QueryRow(`SELECT COALESCE(saves,0) FROM post_metrics WHERE post_id=$1`, cid).Scan(&count)
	}
	svg := `<svg viewBox="0 0 24 24" stroke="currentColor" stroke-width="1.8"><path d="M19 21l-7-5-7 5V5a2 2 0 012-2h10a2 2 0 012 2z"/></svg>`
	svgFilled := `<svg viewBox="0 0 24 24" fill="currentColor" stroke="currentColor" stroke-width="1.8"><path d="M19 21l-7-5-7 5V5a2 2 0 012-2h10a2 2 0 012 2z"/></svg>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if saved {
		fmt.Fprintf(w, `<button class="action action-btn bookmark saved is-on" hx-post="/feed/item/save" hx-target="this" hx-swap="outerHTML" hx-include="[name='session_id'],[name='surface']" onclick="event.stopPropagation()" name="content_id" value="%s" title="Bookmark">%s<span>%d</span></button>`,
			cid, svgFilled, count)
	} else {
		fmt.Fprintf(w, `<button class="action action-btn bookmark" hx-post="/feed/item/save" hx-target="this" hx-swap="outerHTML" hx-include="[name='session_id'],[name='surface']" onclick="event.stopPropagation()" name="content_id" value="%s" title="Bookmark">%s<span>%d</span></button>`,
			cid, svg, count)
	}
}

// NOTE: legacy repostAction (/feed/item/repost → ToggleRepost on the posts
// table) was removed. Reposts are works-native: work card → toggleWorkReaction
// → POST /events work_repost → work_reactions, surfaced into feeds by
// collectFeedWithReposts with "<name> reposted" attribution.

func (h *Handler) dislikeAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "405", 405); return }
	r.ParseForm()
	cid, sid, surf := r.FormValue("content_id"), r.FormValue("session_id"), r.FormValue("surface")
	user := h.userFromRequest(w, r)

	disliked := true
	if h.db != nil {
		var err error
		disliked, err = dbpkg.ToggleDislike(h.db, user.ID, cid)
		if err != nil {
			log.Printf("[dislike] toggle error: %v", err)
		}
	}
	if disliked {
		h.aethyr.SendFeedback(&model.AethyrFeedbackRequest{
			UserID: user.ID, SessionID: sid, Surface: surf,
			Events: []model.AethyrFeedbackEvent{{
				ContentID: cid, EventType: "dislike",
				PositionAtDisplay: 0, Timestamp: time.Now(),
			}},
		})
	}
	count := 0
	if h.db != nil {
		count = dbpkg.GetDislikeCount(h.db, cid)
	}
	// Toggle fill so the icon fills solid when active (matches _action_bar.html);
	// previously fill was omitted, so disliking only recolored the outline.
	dislikeFill := "none"
	if disliked {
		dislikeFill = "currentColor"
	}
	svg := fmt.Sprintf(`<svg viewBox="0 0 24 24" fill="%s" stroke="currentColor" stroke-width="1.8"><path d="M10 15v4a3 3 0 003 3l4-9V2H5.72a2 2 0 00-2 1.7l-1.38 9a2 2 0 002 2.3zm10-13h2.67A2.31 2.31 0 0125 4v7a2.31 2.31 0 01-2.33 2H20"/></svg>`, dislikeFill)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	layout := r.FormValue("layout")
	if layout == "focus" {
		activeClass := ""
		if disliked { activeClass = " is-on" }
		fmt.Fprintf(w, `<button class="f-act dislike%s" hx-post="/feed/item/dislike" hx-target="this" hx-swap="outerHTML" hx-include="[name='session_id'],[name='surface']" hx-vals='{"layout":"focus"}' onclick="event.stopPropagation()" name="content_id" value="%s" title="Dislike">%s<span>%d</span></button>`,
			activeClass, cid, svg, count)
	} else {
		activeClass := ""
		if disliked { activeClass = " is-on" }
		fmt.Fprintf(w, `<button class="action action-btn dislike%s" hx-post="/feed/item/dislike" hx-target="this" hx-swap="outerHTML" hx-include="[name='session_id'],[name='surface']" onclick="event.stopPropagation()" name="content_id" value="%s" title="Dislike">%s<span>%d</span></button>`,
			activeClass, cid, svg, count)
	}
}


func (h *Handler) engineOnline(ctx context.Context) bool {
	c, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	return h.aethyr.Health(c)
}

// nsfwFeedPage serves the NSFW surface — exclusively Adult Creator content.
// Hard-gated: returns 403 for minors and any user without adult_enabled.
func (h *Handler) nsfwFeedPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if hideAdultCreators(user) {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	sessionID := uuid.New().String()
	h.render(w, "index.html", map[string]interface{}{
		"User":      user,
		"Title":     "NSFW · F33D3R",
		"Surface":   "nsfw",
		"SessionID": sessionID,
		"Themes":    ThemesWithActive(user.ThemeID),
	})
}


// videoFeedPage — video-only surface
func (h *Handler) videoFeedPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	h.render(w, "index.html", map[string]interface{}{
		"User":      user,
		"Title":     "Video · F33D3R",
		"Surface":   "video",
		"SessionID": uuid.New().String(),
		"Themes":    ThemesWithActive(user.ThemeID),
	})
}

// comingSoonPage renders a placeholder for features under development.
func (h *Handler) comingSoonPage(title, subtitle string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := h.userFromRequest(w, r)
		h.render(w, "index.html", map[string]interface{}{
			"User":       user,
			"Title":      title + " · F33D3R",
			"Surface":    "feed",
			"SessionID":  uuid.New().String(),
			"Themes":     ThemesWithActive(user.ThemeID),
			"ComingSoon": map[string]string{"Title": title, "Subtitle": subtitle},
		})
	}
}

// pinSurfaceEvent handles POST /events event_type=pin_surface|unpin_surface.
// Returns the updated pinned-tab fragment for HTMX to swap into #feed-surface-tabs.
func (h *Handler) pinSurfaceEvent(w http.ResponseWriter, r *http.Request, eventType string) {
	user := h.userFromRequest(w, r)
	if user == nil || user.ID == "" {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	surfaceID := strings.TrimSpace(r.FormValue("surface_id"))
	if surfaceID == "" {
		http.Error(w, "surface_id required", http.StatusBadRequest)
		return
	}
	if h.db != nil {
		if eventType == "pin_surface" {
			_ = dbpkg.PinSurface(h.db, user.ID, surfaceID)
		} else {
			_ = dbpkg.UnpinSurface(h.db, user.ID, surfaceID)
		}
	}
	// Return updated tab fragment for HTMX to swap into #feed-surface-tabs
	var surfaces []model.FeedSurface
	if h.db != nil {
		surfaces, _ = dbpkg.GetUserSurfaces(h.db, user.ID)
	}
	sessionID := r.FormValue("session_id")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "feed_surface_tabs", map[string]interface{}{
		"Surfaces":  surfaces,
		"SessionID": sessionID,
	})
}

// feedSurfacesPartial serves GET /feed/surfaces.
// Returns the rendered tab fragment for the user's pinned interest surfaces.
// Called on DOMContentLoaded and after pin/unpin events to refresh the tab bar.
func (h *Handler) feedSurfacesPartial(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	var surfaces []model.FeedSurface
	if h.db != nil && user != nil && user.ID != "" {
		surfaces, _ = dbpkg.GetUserSurfaces(h.db, user.ID)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "feed_surface_tabs", map[string]interface{}{
		"Surfaces":  surfaces,
		"SessionID": r.URL.Query().Get("session_id"),
	})
}

// surfaceSheetPartial serves GET /partials/surface-sheet.
// Returns the discovery sheet with all interest surfaces and pin state for the current user.
func (h *Handler) surfaceSheetPartial(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	var surfaces []model.FeedSurface
	if h.db != nil && user != nil {
		surfaces, _ = dbpkg.GetAllSurfaces(h.db, user.ID)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "surface_sheet_content", map[string]interface{}{
		"Surfaces": surfaces,
	})
}

// sidebarRightPartial serves GET /partials/sidebar-right.
// Returns the EventType(sidebar_right_content) fragment for HTMX load.
func (h *Handler) sidebarRightPartial(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	var trending []dbpkg.TrendingTag
	var suggested []dbpkg.SuggestedUser
	if h.db != nil {
		trending, _ = dbpkg.GetTrendingTags(h.db, 6)
		if user != nil {
			suggested, _ = dbpkg.GetSuggestedUsersFiltered(h.db, user.ID, 3, user.IsMinor)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "sidebar_right", map[string]interface{}{
		"TrendingTags":   trending,
		"SuggestedUsers": suggested,
	})
}

// replyPreviewPartial serves GET /partials/reply-preview/{post_id}.
// Returns the EventType(reply_preview) fragment for HTMX swap after a new reply is posted.
func (h *Handler) replyPreviewPartial(w http.ResponseWriter, r *http.Request) {
	postID := r.PathValue("post_id")
	if postID == "" {
		http.NotFound(w, r)
		return
	}
	work, err := dbpkg.GetWorkByID(h.db, postID, "")
	if err != nil || work == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "reply_preview", work)
}
