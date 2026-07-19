package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

// astraonGet proxies a GET request to Astraon and decodes JSON into dest.
// Falls back gracefully when Astraon is unavailable.
func (h *Handler) astraonGet(ctx context.Context, path string, dest interface{}) error {
	if h.cfg.AstraonURL == "" {
		return fmt.Errorf("astraon not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.cfg.AstraonURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("astraon %d for %s", resp.StatusCode, path)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dest)
}

// ── Astraon response types mirrored from astraon/src/models.rs ────────────────

type AstraonSummary struct {
	PialID             string   `json:"pial_id"`
	PeriodDays         int      `json:"period_days"`
	TotalPosts         int64    `json:"total_posts"`
	Impressions        int64    `json:"impressions"`
	Likes              int64    `json:"likes"`
	Reposts            int64    `json:"reposts"`
	Replies            int64    `json:"replies"`
	Saves              int64    `json:"saves"`
	ProfileVisits      int64    `json:"profile_visits"`
	FollowersGained    int64    `json:"followers_gained"`
	FollowersLost      int64    `json:"followers_lost"`
	NetFollowers       int64    `json:"net_followers"`
	CurrentFollowers   int64    `json:"current_followers"`
	EngagementRate     float64  `json:"engagement_rate"`
	ViewTimeSecs       int64    `json:"view_time_secs"`
	ImpressionsPctChange *float64 `json:"impressions_pct_change"`
	LikesPctChange       *float64 `json:"likes_pct_change"`
	FollowersPctChange   *float64 `json:"followers_pct_change"`
}

type AstraonPostRow struct {
	PostID        string    `json:"post_id"`
	BodyPreview   string    `json:"body_preview"`
	ContentType   string    `json:"content_type"`
	CreatedAt     time.Time `json:"created_at"`
	TimeAgo       string    `json:"time_ago"`
	Impressions   int64     `json:"impressions"`
	Likes         int64     `json:"likes"`
	Reposts       int64     `json:"reposts"`
	Replies       int64     `json:"replies"`
	Saves         int64     `json:"saves"`
	EngagementPct float64   `json:"engagement_pct"`
	BarPct        float64   `json:"bar_pct"`
}

type AstraonCreatorPosts struct {
	Posts  []AstraonPostRow `json:"posts"`
	SortBy string           `json:"sort_by"`
	Period int              `json:"period"`
}

type AstraonTimelinePoint struct {
	Date        string  `json:"date"`
	Impressions int64   `json:"impressions"`
	Likes       int64   `json:"likes"`
	Reposts     int64   `json:"reposts"`
	Replies     int64   `json:"replies"`
	Posts       int64   `json:"posts"`
	Engagement  float64 `json:"engagement"`
}

type AstraonTimeline struct {
	Points          []AstraonTimelinePoint `json:"points"`
	PeriodDays      int                    `json:"period_days"`
	MaxImpressions  int64                  `json:"max_impressions"`
	MaxEngagement   float64                `json:"max_engagement"`
}

type AstraonAudiencePoint struct {
	Date         string `json:"date"`
	NewFollowers int64  `json:"new_followers"`
	Unfollows    int64  `json:"unfollows"`
	Net          int64  `json:"net"`
}

type AstraonAudience struct {
	Points         []AstraonAudiencePoint `json:"points"`
	TotalFollowers int64                  `json:"total_followers"`
	PeriodDays     int                    `json:"period_days"`
}

type AstraonBreakdownItem struct {
	ContentType   string  `json:"content_type"`
	Count         int64   `json:"count"`
	Impressions   int64   `json:"impressions"`
	AvgEngagement float64 `json:"avg_engagement"`
	PctOfPosts    float64 `json:"pct_of_posts"`
}

type AstraonBreakdown struct {
	Types []AstraonBreakdownItem `json:"types"`
}

type AstraonSiteSummary struct {
	TotalUsers       int64 `json:"total_users"`
	TotalPosts       int64 `json:"total_posts"`
	TotalImpressions int64 `json:"total_impressions"`
	TotalLikes       int64 `json:"total_likes"`
	TotalReposts     int64 `json:"total_reposts"`
	TotalComments    int64 `json:"total_comments"`
	NewUsersToday    int64 `json:"new_users_today"`
	NewUsersWeek     int64 `json:"new_users_week"`
	NewPostsToday    int64 `json:"new_posts_today"`
	NewPostsWeek     int64 `json:"new_posts_week"`
	ActiveUsersDay   int64 `json:"active_users_day"`
	ActiveUsersWeek  int64 `json:"active_users_week"`
}

// ── Analytics page ────────────────────────────────────────────────────────────

func (h *Handler) analyticsPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	days := 30
	switch r.URL.Query().Get("period") {
	case "7":   days = 7
	case "90":  days = 90
	case "all": days = 0
	}
	sortBy := r.URL.Query().Get("sort")
	if sortBy == "" {
		sortBy = "impressions"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// ── Astraon calls (with DB fallback) ─────────────────────────────────────
	var summary *AstraonSummary
	var posts   *AstraonCreatorPosts
	var timeline *AstraonTimeline
	var audience *AstraonAudience
	var breakdown *AstraonBreakdown

	pialID := user.PIALID
	daysStr := fmt.Sprintf("%d", days)

	var astrErr error

	// Summary
	var s AstraonSummary
	astrErr = h.astraonGet(ctx, fmt.Sprintf("/v1/analytics/creator/%s/summary?days=%s", pialID, daysStr), &s)
	if astrErr == nil {
		summary = &s
	} else {
		// DB fallback
		if dbSum, err := dbpkg.GetCreatorAnalyticsSummary(h.db, pialID); err == nil && dbSum != nil {
			summary = &AstraonSummary{
				PialID:          pialID,
				PeriodDays:      days,
				TotalPosts:      int64(dbSum.TotalPosts),
				Impressions:     dbSum.TotalImpressions,
				Likes:           dbSum.TotalLikes,
				Reposts:         dbSum.TotalReposts,
				Replies:         dbSum.TotalComments,
				Saves:           dbSum.TotalSaves,
				EngagementRate:  dbSum.EngagementRate,
				ViewTimeSecs:    dbSum.TotalViewSeconds,
			}
		}
	}

	// Posts table
	var cp AstraonCreatorPosts
	astrErr = h.astraonGet(ctx, fmt.Sprintf("/v1/analytics/creator/%s/posts?days=%s&sort_by=%s&limit=20", pialID, daysStr, sortBy), &cp)
	if astrErr == nil {
		posts = &cp
	} else {
		// DB fallback
		if dbPosts, err := dbpkg.GetCreatorTopPosts(h.db, pialID, 20); err == nil {
			rows := make([]AstraonPostRow, 0, len(dbPosts))
			for _, p := range dbPosts {
				rows = append(rows, AstraonPostRow{
					PostID:        p.PostID,
					BodyPreview:   p.Body,
					ContentType:   "text",
					CreatedAt:     p.CreatedAt,
					TimeAgo:       TimeAgo(p.CreatedAt),
					Impressions:   p.Impressions,
					Likes:         p.Likes,
					Reposts:       p.Reposts,
					Replies:       p.Comments,
					Saves:         p.Saves,
					EngagementPct: p.EngagementPct,
					BarPct:        p.MaxPct,
				})
			}
			posts = &AstraonCreatorPosts{Posts: rows, SortBy: sortBy, Period: days}
		}
	}

	// Timeline
	var tl AstraonTimeline
	if err := h.astraonGet(ctx, fmt.Sprintf("/v1/analytics/creator/%s/timeline?days=%s", pialID, daysStr), &tl); err == nil {
		timeline = &tl
	}

	// Audience
	var aud AstraonAudience
	if err := h.astraonGet(ctx, fmt.Sprintf("/v1/analytics/creator/%s/audience?days=%s", pialID, daysStr), &aud); err == nil {
		audience = &aud
	}

	// Content breakdown
	var bd AstraonBreakdown
	if err := h.astraonGet(ctx, fmt.Sprintf("/v1/analytics/creator/%s/breakdown?days=%s", pialID, daysStr), &bd); err == nil {
		breakdown = &bd
	}

	rail := h.railData(user, "default")
	data := h.baseData(user)
	data["Title"]        = "Analytics · F33D3R"
	data["SessionID"]    = uuid.New().String()
	data["TargetHandle"] = user.Handle
	data["Period"]       = days
	data["SortBy"]       = sortBy
	data["Summary"]      = summary
	data["Posts"]        = posts
	data["Timeline"]     = timeline
	data["Audience"]     = audience
	data["Breakdown"]    = breakdown
	data["AstraonOnline"]= astrErr == nil
	data["TrendingTags"]    = rail["TrendingTags"]
	data["SuggestedUsers"]  = rail["SuggestedUsers"]
	data["RailContext"]     = rail["RailContext"]
	data["RailNewsItems"]   = rail["RailNewsItems"]
	data["RailNewsLabel"]   = rail["RailNewsLabel"]
	h.render(w, "analytics.html", data)
}

// adminUserAnalytics — standalone page redirect for /admin/analytics (bookmarkable URL).
func (h *Handler) adminUserAnalytics(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// adminPanelAnalytics renders the Analytics tab panel for HTMX swap inside the admin panel.
func (h *Handler) adminPanelAnalytics(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}

	handle := strings.TrimSpace(r.URL.Query().Get("handle"))
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var siteSummary *AstraonSiteSummary
	var ss AstraonSiteSummary
	if err := h.astraonGet(ctx, "/v1/analytics/site/summary", &ss); err == nil {
		siteSummary = &ss
	} else {
		if dbSite, err := dbpkg.GetSiteAnalytics(h.db); err == nil {
			siteSummary = &AstraonSiteSummary{
				TotalUsers:       int64(dbSite.TotalUsers),
				TotalPosts:       int64(dbSite.TotalPosts),
				TotalImpressions: dbSite.TotalImpressions,
				TotalLikes:       dbSite.TotalLikes,
				NewUsersWeek:     int64(dbSite.NewUsersWeek),
				NewPostsWeek:     int64(dbSite.NewPostsWeek),
			}
		}
	}

	var userSummary *AstraonSummary
	var userPosts   *AstraonCreatorPosts
	var lookupHandle string
	if handle != "" {
		target, tErr := dbpkg.GetUserByHandle(h.db, handle)
		if tErr != nil {
			log.Printf("[analytics] target lookup %s: %v", handle, tErr)
		}
		if target != nil {
			lookupHandle = target.Handle
			var s AstraonSummary
			if err := h.astraonGet(ctx, fmt.Sprintf("/v1/analytics/creator/%s/summary?days=30", target.PIALID), &s); err == nil {
				userSummary = &s
			} else {
				if dbSum, err := dbpkg.GetCreatorAnalyticsSummary(h.db, target.PIALID); err == nil && dbSum != nil {
					userSummary = &AstraonSummary{
						PialID:         target.PIALID,
						PeriodDays:     30,
						TotalPosts:     int64(dbSum.TotalPosts),
						Impressions:    dbSum.TotalImpressions,
						Likes:          dbSum.TotalLikes,
						Reposts:        dbSum.TotalReposts,
						Replies:        dbSum.TotalComments,
						Saves:          dbSum.TotalSaves,
						EngagementRate: dbSum.EngagementRate,
					}
				}
			}
			var cp AstraonCreatorPosts
			if err := h.astraonGet(ctx, fmt.Sprintf("/v1/analytics/creator/%s/posts?days=30&limit=20", target.PIALID), &cp); err == nil {
				userPosts = &cp
			} else {
				if dbPosts, err := dbpkg.GetCreatorTopPosts(h.db, target.PIALID, 20); err == nil {
					rows := make([]AstraonPostRow, 0, len(dbPosts))
					for _, p := range dbPosts {
						rows = append(rows, AstraonPostRow{
							PostID:        p.PostID,
							BodyPreview:   p.Body,
							ContentType:   "text",
							CreatedAt:     p.CreatedAt,
							TimeAgo:       TimeAgo(p.CreatedAt),
							Impressions:   p.Impressions,
							Likes:         p.Likes,
							Reposts:       p.Reposts,
							Replies:       p.Comments,
							Saves:         p.Saves,
							EngagementPct: p.EngagementPct,
							BarPct:        p.MaxPct,
						})
					}
					userPosts = &AstraonCreatorPosts{Posts: rows, SortBy: "impressions", Period: 30}
				}
			}
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "admin_analytics_panel", map[string]interface{}{
		"SiteSummary":  siteSummary,
		"UserSummary":  userSummary,
		"UserPosts":    userPosts,
		"LookupHandle": lookupHandle,
	})
}
