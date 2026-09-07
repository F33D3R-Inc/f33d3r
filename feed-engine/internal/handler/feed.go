package handler

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

func (h *Handler) indexPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	user := h.userFromRequest(w, r)
	var pinnedSurfaces []model.FeedSurface
	if h.db != nil && user != nil && user.ID != "" {
		pinnedSurfaces, _ = dbpkg.GetUserSurfaces(h.db, user.ID)
	}
	// A feed tab pushes /?surface=<id>; the page drawn for that URL opens on
	// that tab, so a history restore or a shared link shows what it names.
	active := homeTabSurface(strings.TrimSpace(r.URL.Query().Get("surface")))
	if active == "" {
		active = "following"
	}
	h.render(w, r, "index.html", h.withRail(map[string]interface{}{
		"User":           user,
		"Surface":        "feed",
		"ActiveSurface":  active,
		"SessionID":      uuid.New().String(),
		"Title":          "Home",
		"ShowScores":     h.cfg.ShowScores,
		"Themes":         ThemesWithActive(user.ThemeID),
		"EngineOnline":   h.engineOnline(r.Context()),
		"PinnedSurfaces": pinnedSurfaces,
	}, user, "feed"))
}

// exploreTrendingLimit sizes the Explore surface's trending grid, chip row and
// tag browser — a Playground surface, not the rail.
//
// It is Explore's own number and it is stated here because the surface is where
// the decision belongs: this is a full-width canvas whose entire job on the
// Trending tab is to show what is trending, so it can afford rows the 300px rail
// beside it cannot. Explore used to read the rail's list, which meant sizing one
// silently resized the other — cutting the rail's panel would have quietly
// halved this grid. The two now ask the same query their own question.
const exploreTrendingLimit = 6

// exploreSurfaces maps an Explore tab to the works surface it shows. The "tags"
// tab shows the tag browser and no works surface. A tab absent here is not a
// tab, and the canvas answers with the Trending tab for it.
var exploreSurfaces = map[string]string{
	"trending": "trending",
	"you":      "for_you",
	"news":     "trending",
	"live":     "trending",
	"paid":     "for_you",
	"tags":     "",
}

// exploreCanvasData is the input of the explore_canvas Facet for one tab: the
// tab strip with that tab active, the trending strip on Trending, the tag
// browser on Tags, and the results container addressed at the tab's surface.
func (h *Handler) exploreCanvasData(surf string) map[string]interface{} {
	feedSurface, ok := exploreSurfaces[surf]
	if !ok {
		surf = "trending"
		feedSurface = exploreSurfaces[surf]
	}
	var trending []dbpkg.TrendingTag
	if h.db != nil {
		var err error
		trending, err = dbpkg.GetTrendingTags(h.db, exploreTrendingLimit)
		if err != nil {
			log.Printf("[explore] trending tags: %v", err)
		}
	}
	return map[string]interface{}{
		"Surf":         surf,
		"FeedSurface":  feedSurface,
		"TrendingTags": trending,
	}
}

// facetExploreCanvas — GET /facets/explore/canvas?surf=<tab>
// An Explore tab is a request for the canvas drawn for that tab; the tab
// strip, the sections and the results container arrive together.
func (h *Handler) facetExploreCanvas(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	h.renderPartial(w, "explore_canvas", h.exploreCanvasData(r.URL.Query().Get("surf")))
}

func (h *Handler) explorePage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	canvas := h.exploreCanvasData(r.URL.Query().Get("surf"))
	// The page owns the canvas inputs by name: TrendingTags is Explore's own
	// list (exploreTrendingLimit), never the rail's.
	h.render(w, r, "explore.html", h.withRail(map[string]interface{}{
		"User":         user,
		"Surface":      "explore",
		"SessionID":    uuid.New().String(),
		"Title":        "Explore",
		"ShowScores":   h.cfg.ShowScores,
		"Themes":       ThemesWithActive(user.ThemeID),
		"Surf":         canvas["Surf"],
		"FeedSurface":  canvas["FeedSurface"],
		"TrendingTags": canvas["TrendingTags"],
	}, user, "explore"))
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
	surface := r.URL.Query().Get("surface")
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		sessionID = uuid.New().String()
	}
	after := r.URL.Query().Get("after")
	userID := r.URL.Query().Get("user_id")
	user := h.userFromRequest(w, r)

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
		if targetID == "" && user != nil {
			targetID = user.ID
		}
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

// ── Interaction actions ───────────────────────────────────────────────────────
//
// /feed/item/like, /feed/item/save and /feed/item/dislike were removed with the
// posts lane. They wrote post_likes / bookmarks / user_dislikes and read counts
// back out of post_metrics — four tables the render stopped consulting when work
// cards took over, so each one answered 200 with a button whose count came from
// nowhere. No template had emitted those hx-posts for some time; the routes were
// reachable only by hand.
//
// Every reaction is now one row in work_reactions: the button calls
// toggleWorkReaction, which POSTs /events {work_like|work_unlike|work_repost|
// work_unrepost|work_bookmark|work_unbookmark|work_dislike|work_undislike}, and
// the tallies are counted from those rows inside worksSelectSQL at read time.

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
	h.render(w, r, "index.html", map[string]interface{}{
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
	h.render(w, r, "index.html", map[string]interface{}{
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
		h.render(w, r, "index.html", map[string]interface{}{
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
		"Active":    "",
	})
}

// feedSurface resolves a surface id to its server-side definition, or nil when
// the id names no active interest surface.
//
// The definition is the authority on what a pinned tab means. The tab strip, the
// discovery sheet and the feed query all read the same row, so a surface cannot
// be labelled one thing in the sheet and select another thing in the feed.
func (h *Handler) feedSurface(id string) *model.FeedSurface {
	id = strings.TrimSpace(strings.ToLower(id))
	if h.db == nil || id == "" {
		return nil
	}
	s, err := dbpkg.GetSurfaceByID(h.db, id)
	if err != nil || s == nil || s.SurfaceType != "interest" {
		return nil
	}
	return s
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
		"Active":    r.URL.Query().Get("surface"),
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
//
// It renders from railData, the same dataset the page render uses, so the rail
// fetched on its own and the rail delivered with a page are the same rail. A
// second, hand-rolled query here is how the two drift apart.
func (h *Handler) sidebarRightPartial(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "sidebar_right", h.railData(user, "default"))
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
