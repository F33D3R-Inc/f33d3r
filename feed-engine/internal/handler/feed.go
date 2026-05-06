package handler

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/f33d3r/feed-engine/internal/aethyr"
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
	var trending []dbpkg.TrendingTag
	var suggested []dbpkg.SuggestedUser
	if h.db != nil {
		trending, _ = dbpkg.GetTrendingTags(h.db, 6)
		if user != nil {
			suggested, _ = dbpkg.GetSuggestedUsers(h.db, user.ID, 3)
		}
	}
	h.render(w, "index.html", map[string]interface{}{
		"User":           user,
		"Surface":        "feed",
		"SessionID":      uuid.New().String(),
		"Title":          "Home",
		"ShowScores":     h.cfg.ShowScores,
		"Themes":         ThemesWithActive(user.ThemeID),
		"EngineOnline":   h.engineOnline(r.Context()),
		"TrendingTags":   trending,
		"SuggestedUsers": suggested,
	})
}

func (h *Handler) explorePage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	var trending []dbpkg.TrendingTag
	if h.db != nil {
		trending, _ = dbpkg.GetTrendingTags(h.db, 8)
	}
	h.render(w, "explore.html", map[string]interface{}{
		"User":         user,
		"Surface":      "explore",
		"SessionID":    uuid.New().String(),
		"Title":        "Explore",
		"ShowScores":   h.cfg.ShowScores,
		"Themes":       ThemesWithActive(user.ThemeID),
		"TrendingTags": trending,
	})
}

// ── HTMX: feed partial ────────────────────────────────────────────────────────

func (h *Handler) feedPartial(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	surface   := r.URL.Query().Get("surface")
	if surface == "" { surface = h.cfg.DefaultSurface }
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" { sessionID = uuid.New().String() }
	after  := r.URL.Query().Get("after")
	userID := r.URL.Query().Get("user_id")
	user   := h.userFromRequest(w, r)

	var candidates []*model.Post

	// Profile-scoped surfaces
	if strings.HasPrefix(surface, "profile_") && h.db != nil {
		targetID := userID
		if targetID == "" { targetID = user.ID }
		switch surface {
		case "profile_posts":
			candidates, _ = dbpkg.GetUserPosts(h.db, targetID, h.cfg.FeedMaxCandidates, after)
		case "profile_replies":
			candidates, _ = dbpkg.GetUserReplies(h.db, targetID, h.cfg.FeedMaxCandidates, after)
		case "profile_media":
			candidates, _ = dbpkg.GetUserMedia(h.db, targetID, h.cfg.FeedMaxCandidates, after)
		case "profile_saves":
			candidates, _ = dbpkg.GetBookmarks(h.db, targetID, h.cfg.FeedMaxCandidates, after)
		}
		for _, p := range candidates { p.TimeAgo = TimeAgo(p.CreatedAt) }
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = h.partial.ExecuteTemplate(w, "feed_items.html", map[string]interface{}{
			"Posts":             candidates,
			"SessionID":         sessionID,
			"Surface":           surface,
			"After":             after,
			"ShowScores":        h.cfg.ShowScores,
			"CurrentUserHandle": user.Handle,
			"CurrentUserID":     user.ID,
		})
		return
	}

	// Latest surface — chronological, no ranking
	if surface == "latest" {
		var posts []*model.Post
		if h.db != nil {
			posts, _ = dbpkg.GetRecentPosts(h.db, "", h.cfg.FeedMaxCandidates, after)
		}
		for _, p := range posts { p.TimeAgo = TimeAgo(p.CreatedAt) }
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = h.partial.ExecuteTemplate(w, "feed_items.html", map[string]interface{}{
			"Posts":             posts,
			"SessionID":         sessionID,
			"Surface":           surface,
			"After":             after,
			"ShowScores":        false,
			"CurrentUserHandle": user.Handle,
			"CurrentUserID":     user.ID,
		})
		return
	}

	// Standard surfaces — real posts from DB first, mock fallback
	isMockFeed := false
	if h.db != nil {
		var dbPosts []*model.Post
		var err error
		switch surface {
		case "following":
			dbPosts, err = dbpkg.GetFollowingFeed(h.db, user.ID, h.cfg.FeedMaxCandidates, after)
		case "field", "focus":
			// For You: blended follower + trending candidates
			dbPosts, err = dbpkg.GetForYouFeed(h.db, user.ID, h.cfg.FeedMaxCandidates, after)
		default:
			dbPosts, err = dbpkg.GetRecentPostsForSurface(h.db, surface, h.cfg.FeedMaxCandidates, after)
		}
		if err != nil {
			log.Printf("[feed] db query: %v", err)
		} else if len(dbPosts) > 0 {
			candidates = dbPosts
		}
	}
	if len(candidates) == 0 {
		if surface == "following" {
			// No mock for following — show empty state
		} else {
			candidates = aethyr.GetCandidatesForSurface(surface, user.ID, h.cfg.FeedMaxCandidates)
			isMockFeed = true
		}
	}

	// Enrich candidates with real-time velocity from feedback_events (24h window).
	// One batch query replaces N per-post queries.
	if h.db != nil && len(candidates) > 0 && !isMockFeed {
		ids := make([]string, len(candidates))
		for i, p := range candidates { ids[i] = p.ContentID }
		if velocities, err := dbpkg.GetContentVelocitiesBatch(h.db, ids, 24); err == nil {
			for _, p := range candidates {
				if v, ok := velocities[p.ContentID]; ok {
					p.FeedVelocity = v
				}
			}
		}
	}

	// AethyrRank
	rankReq := h.buildRankRequest(user, candidates, surface, sessionID)
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.AethyrRankTimeout)
	defer cancel()

	var rankedPosts []*model.Post
	if resp, err := h.aethyr.Rank(ctx, rankReq); err == nil {
		postMap := map[string]*model.Post{}
		for _, p := range candidates { postMap[p.ContentID] = p }
		for _, ranked := range resp.RankedItems {
			if p, ok := postMap[ranked.ContentID]; ok {
				p.FinalScore      = ranked.FinalScore
				p.ExplorationSlot = ranked.ExplorationSlot
				p.AesqAlignment   = ranked.ScoreBreakdown.AesqAlignment
				p.VelocityBoost   = ranked.ScoreBreakdown.VelocityBoost
				p.Explanation     = ranked.Explanation
				p.TimeAgo         = TimeAgo(p.CreatedAt)
				rankedPosts = append(rankedPosts, p)
			}
		}
	} else {
		rankedPosts = aethyr.FallbackRankPosts(candidates)
		for _, p := range rankedPosts { p.TimeAgo = TimeAgo(p.CreatedAt) }
	}

	h.sendImpressionFeedback(user.ID, sessionID, surface, rankedPosts)

	// Attach poll data where posts have poll_options
	if h.db != nil {
		for _, p := range rankedPosts {
			if p.ContentType == "poll" {
				p.Poll, _ = dbpkg.GetPollResults(h.db, p.ID, user.ID)
			}
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.partial.ExecuteTemplate(w, "feed_items.html", map[string]interface{}{
		"Posts":             rankedPosts,
		"SessionID":         sessionID,
		"Surface":           surface,
		"After":             after,
		"ShowScores":        h.cfg.ShowScores,
		"IsMockFeed":        isMockFeed,
		"CurrentUserHandle": user.Handle,
		"CurrentUserID":     user.ID,
	}); err != nil {
		log.Printf("[feed] template error: %v", err)
	}
}

// ── HTMX: interaction actions ─────────────────────────────────────────────────

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
			// Notify post author
			go func() {
				if post, err := dbpkg.GetPostByID(h.db, cid); err == nil && post != nil {
					_ = dbpkg.CreateNotification(h.db, post.AuthorID, "like", user.ID, cid, "post")
					dbpkg.IncrementUnreadCount(h.db, post.AuthorID)
				}
			}()
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

	likedClass := ""
	if liked { likedClass = "liked" }
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<button hx-post="/feed/item/like" hx-target="this" hx-swap="outerHTML"
		hx-include="[name='session_id'],[name='surface']" onclick="event.stopPropagation()"
		name="content_id" value="%s" class="action-btn %s">
		<svg class="w-3.5 h-3.5" viewBox="0 0 24 24" fill="%s" stroke="currentColor" stroke-width="2"><path d="M20.84 4.61a5.5 5.5 0 0 0-7.78 0L12 5.67l-1.06-1.06a5.5 5.5 0 0 0-7.78 7.78l1.06 1.06L12 21.23l7.78-7.78 1.06-1.06a5.5 5.5 0 0 0 0-7.78z"/></svg>
		</button>`,
		cid, likedClass,
		func() string {
			if liked { return "currentColor" }
			return "none"
		}(),
	)
}

func (h *Handler) saveAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "405", 405); return }
	r.ParseForm()
	cid, sid, surf := r.FormValue("content_id"), r.FormValue("session_id"), r.FormValue("surface")
	user := h.userFromRequest(w, r)

	if h.db != nil {
		_ = dbpkg.AddBookmark(h.db, user.ID, cid)
		_ = dbpkg.IncrementSave(h.db, cid)
	}
	h.aethyr.SendFeedback(&model.AethyrFeedbackRequest{
		UserID: user.ID, SessionID: sid, Surface: surf,
		Events: []model.AethyrFeedbackEvent{{
			ContentID: cid, EventType: "save",
			PositionAtDisplay: 0, Timestamp: time.Now(),
		}},
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<button hx-post="/feed/item/save" hx-target="this" hx-swap="outerHTML"
		hx-include="[name='session_id'],[name='surface']" onclick="event.stopPropagation()"
		name="content_id" value="%s" class="action-btn saved">
		<svg class="w-3.5 h-3.5" viewBox="0 0 24 24" fill="currentColor"><path d="M19 21l-7-5-7 5V5a2 2 0 012-2h10a2 2 0 012 2z"/></svg>
		</button>`, cid)
}

func (h *Handler) repostAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "405", 405); return }
	r.ParseForm()
	cid, sid, surf := r.FormValue("content_id"), r.FormValue("session_id"), r.FormValue("surface")
	user := h.userFromRequest(w, r)

	if h.db != nil { _ = dbpkg.IncrementRepost(h.db, cid) }
	h.aethyr.SendFeedback(&model.AethyrFeedbackRequest{
		UserID: user.ID, SessionID: sid, Surface: surf,
		Events: []model.AethyrFeedbackEvent{{
			ContentID: cid, EventType: "share",
			PositionAtDisplay: 0, Timestamp: time.Now(),
		}},
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<button class="action-btn reposted">
		<svg class="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M17 1l4 4-4 4"/><path d="M3 11V9a4 4 0 014-4h14M7 23l-4-4 4-4"/><path d="M21 13v2a4 4 0 01-4 4H3"/></svg>
		<span>Reposted</span></button>`)
}

func (h *Handler) dislikeAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "405", 405); return }
	r.ParseForm()
	cid, sid, surf := r.FormValue("content_id"), r.FormValue("session_id"), r.FormValue("surface")
	user := h.userFromRequest(w, r)

	h.aethyr.SendFeedback(&model.AethyrFeedbackRequest{
		UserID: user.ID, SessionID: sid, Surface: surf,
		Events: []model.AethyrFeedbackEvent{{
			ContentID: cid, EventType: "dislike",
			PositionAtDisplay: 0, Timestamp: time.Now(),
		}},
	})

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	layout := r.FormValue("layout")
	if layout == "focus" {
		fmt.Fprintf(w, `<button class="f-act disliked" hx-post="/feed/item/dislike" hx-target="this" hx-swap="outerHTML"
			hx-include="[name='session_id'],[name='surface']" hx-vals='{"layout":"focus"}' onclick="event.stopPropagation()"
			name="content_id" value="%s" title="Not for me" style="opacity:.35">
			<svg fill="currentColor" stroke="none" viewBox="0 0 24 24" style="width:22px;height:22px"><path d="M10 15v4a3 3 0 003 3l4-9V2H5.72a2 2 0 00-2 1.7l-1.38 9a2 2 0 002 2.3zm10-13h2.67A2.31 2.31 0 0125 4v7a2.31 2.31 0 01-2.33 2H20"/></svg>
			</button>`, cid)
	} else {
		fmt.Fprintf(w, `<button class="action-btn disliked" hx-post="/feed/item/dislike" hx-target="this" hx-swap="outerHTML"
			hx-include="[name='session_id'],[name='surface']" onclick="event.stopPropagation()"
			name="content_id" value="%s" title="Not for me">
			<svg class="w-3.5 h-3.5" fill="currentColor" stroke="none" viewBox="0 0 24 24"><path d="M10 15v4a3 3 0 003 3l4-9V2H5.72a2 2 0 00-2 1.7l-1.38 9a2 2 0 002 2.3zm10-13h2.67A2.31 2.31 0 0125 4v7a2.31 2.31 0 01-2.33 2H20"/></svg>
			</button>`, cid)
	}
}

func (h *Handler) buildRankRequest(user *model.User, candidates []*model.Post, surface, sessionID string) *model.AethyrRankRequest {
	pool := make([]model.AethyrContent, len(candidates))
	for i, p := range candidates {
		pool[i] = aethyr.PostToAethyrContent(p)
	}
	history := user.RecentContentIDs
	if len(history) > 50 { history = history[len(history)-50:] }
	return &model.AethyrRankRequest{
		UserState: model.AethyrUserState{
			UserID:             user.ID,
			InterestVector:     user.InterestVector,
			InteractionHistory: history,
			IsColdStart:        user.IsColdStart,
			CreatorAffinities:  user.CreatorAffinities,
			SafetyEpsilon:      user.SafetyEpsilon,
			UserTier:           user.Tier,
			RealmLevel:         user.Realm,
		},
		ContentPool: pool,
		SessionContext: model.AethyrSession{
			Surface:   surface,
			RequestID: uuid.New().String(),
			SessionID: sessionID,
			MaxItems:  h.cfg.FeedPageSize,
			Timestamp: time.Now(),
		},
	}
}

func (h *Handler) sendImpressionFeedback(userID, sessionID, surface string, posts []*model.Post) {
	if len(posts) == 0 { return }
	events := make([]model.AethyrFeedbackEvent, len(posts))
	for i, p := range posts {
		events[i] = model.AethyrFeedbackEvent{
			ContentID: p.ContentID, EventType: "impression",
			PositionAtDisplay: i + 1, Timestamp: time.Now(),
			ExplorationSlot: p.ExplorationSlot,
		}
	}
	h.aethyr.SendFeedback(&model.AethyrFeedbackRequest{
		UserID: userID, SessionID: sessionID, Surface: surface, Events: events,
	})
}

func (h *Handler) engineOnline(ctx context.Context) bool {
	c, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	return h.aethyr.Health(c)
}
