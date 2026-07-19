package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// creatorDashboardPage serves GET /create — the creator OS dashboard.
// Auth: must be creator or admin.
func (h *Handler) creatorDashboardPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsCreator && !user.IsAdmin() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	var artistStats dbpkg.ArtistStats
	var subCount int
	if h.db != nil {
		artistStats = dbpkg.GetArtistStats(h.db, user.ID)
		subCount = dbpkg.GetSubscriberCount(h.db, user.ID)
	}

	rail := h.railData(user, "default")
	h.render(w, "creator_dashboard.html", map[string]interface{}{
		"User":           user,
		"Title":          "Creator Dashboard · F33D3R",
		"SessionID":      uuid.New().String(),
		"ArtistStats":    artistStats,
		"SubCount":       subCount,
		"ActiveSection":  "overview",
		"Themes":         ThemesWithActive(user.ThemeID),
		"TrendingTags":   rail["TrendingTags"],
		"SuggestedUsers": rail["SuggestedUsers"],
		"RailContext":    rail["RailContext"],
		"RailNewsItems":  rail["RailNewsItems"],
		"RailNewsLabel":  rail["RailNewsLabel"],
	})
}

// creatorPanelOverview serves GET /create/partials/overview
func (h *Handler) creatorPanelOverview(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsCreator && !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}

	var artistStats dbpkg.ArtistStats
	var subCount int
	var rawPlans []dbpkg.CreatorPlan
	var recentTracks []*model.Track
	if h.db != nil {
		artistStats = dbpkg.GetArtistStats(h.db, user.ID)
		subCount = dbpkg.GetSubscriberCount(h.db, user.ID)
		rawPlans, _ = dbpkg.GetCreatorPlans(h.db, user.ID)
		recentTracks, _ = dbpkg.GetTracksByAuthor(h.db, user.ID, 5)
		for _, t := range recentTracks {
			t.TimeAgo = TimeAgo(t.CreatedAt)
		}
	}

	totalAET := 0
	for _, p := range rawPlans {
		totalAET += p.PriceAet * subCount
	}
	earningsDisplay := fmt.Sprintf("%.2f AET", float64(totalAET)/100.0)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "creator_overview", map[string]interface{}{
		"ArtistStats":     artistStats,
		"SubCount":        subCount,
		"EarningsDisplay": earningsDisplay,
		"RecentTracks":    recentTracks,
		"Plans":           rawPlans,
		"User":            user,
		"ActiveSection":   "overview",
	})
}

// creatorPanelMusic serves GET /create/partials/music
func (h *Handler) creatorPanelMusic(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsCreator && !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}

	var myTracks []*model.Track
	var artistStats dbpkg.ArtistStats
	if h.db != nil {
		myTracks, _ = dbpkg.GetTracksByAuthor(h.db, user.ID, 100)
		for _, t := range myTracks {
			t.TimeAgo = TimeAgo(t.CreatedAt)
		}
		artistStats = dbpkg.GetArtistStats(h.db, user.ID)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "creator_music", map[string]interface{}{
		"MyTracks":      myTracks,
		"ArtistStats":   artistStats,
		"User":          user,
		"ActiveSection": "music",
	})
}

// creatorPanelMemberships serves GET /create/partials/memberships
func (h *Handler) creatorPanelMemberships(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsCreator && !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}

	var rawPlans []dbpkg.CreatorPlan
	var subCount int
	if h.db != nil {
		rawPlans, _ = dbpkg.GetCreatorPlans(h.db, user.ID)
		subCount = dbpkg.GetSubscriberCount(h.db, user.ID)
	}

	plans := make([]model.ShopPlan, 0, len(rawPlans))
	for _, p := range rawPlans {
		plans = append(plans, model.ShopPlan{
			ID:           p.ID,
			Name:         p.Name,
			Description:  p.Description,
			PriceAET:     p.PriceAet,
			PriceDisplay: fmt.Sprintf("%.2f", float64(p.PriceAet)/100.0),
			IsActive:     true,
		})
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "creator_memberships", map[string]interface{}{
		"Plans":          plans,
		"SubCount":       subCount,
		"User":           user,
		"ActiveSection":  "memberships",
		"CreatorPIALID":  user.PIALID,
	})
}

// creatorPanelEarnings serves GET /create/partials/earnings
func (h *Handler) creatorPanelEarnings(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsCreator && !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}

	var rawPlans []dbpkg.CreatorPlan
	var subCount int
	if h.db != nil {
		rawPlans, _ = dbpkg.GetCreatorPlans(h.db, user.ID)
		subCount = dbpkg.GetSubscriberCount(h.db, user.ID)
	}

	plans := make([]model.ShopPlan, 0, len(rawPlans))
	totalAET := 0
	for _, p := range rawPlans {
		plans = append(plans, model.ShopPlan{
			ID:           p.ID,
			Name:         p.Name,
			Description:  p.Description,
			PriceAET:     p.PriceAet,
			PriceDisplay: fmt.Sprintf("%.2f", float64(p.PriceAet)/100.0),
			IsActive:     true,
		})
		totalAET += p.PriceAet * subCount
	}

	subsDisplay := fmt.Sprintf("%d active subscriber(s)", subCount)
	earningsDisplay := fmt.Sprintf("%.2f AET", float64(totalAET)/100.0)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "creator_earnings", map[string]interface{}{
		"SubCount":        subCount,
		"TotalAET":        totalAET,
		"EarningsDisplay": earningsDisplay,
		"SubsDisplay":     subsDisplay,
		"Plans":           plans,
		"User":            user,
		"ActiveSection":   "earnings",
	})
}

// creatorPanelAnalytics serves GET /create/partials/analytics
// Uses the same Astraon fetch logic as analyticsPage, scoped to the creator's PIAL.
func (h *Handler) creatorPanelAnalytics(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsCreator && !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}

	days := 30
	pialID := user.PIALID
	daysStr := fmt.Sprintf("%d", days)
	sortBy := "impressions"

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var summary *AstraonSummary
	var posts *AstraonCreatorPosts
	var timeline *AstraonTimeline
	var audience *AstraonAudience
	var breakdown *AstraonBreakdown

	var astrErr error

	// Summary
	var s AstraonSummary
	astrErr = h.astraonGet(ctx, fmt.Sprintf("/v1/analytics/creator/%s/summary?days=%s", pialID, daysStr), &s)
	if astrErr == nil {
		summary = &s
	} else {
		if dbSum, err := dbpkg.GetCreatorAnalyticsSummary(h.db, pialID); err == nil && dbSum != nil {
			summary = &AstraonSummary{
				PialID:         pialID,
				PeriodDays:     days,
				TotalPosts:     int64(dbSum.TotalPosts),
				Impressions:    dbSum.TotalImpressions,
				Likes:          dbSum.TotalLikes,
				Reposts:        dbSum.TotalReposts,
				Replies:        dbSum.TotalComments,
				Saves:          dbSum.TotalSaves,
				EngagementRate: dbSum.EngagementRate,
				ViewTimeSecs:   dbSum.TotalViewSeconds,
			}
		}
	}

	// Posts table
	var cp AstraonCreatorPosts
	astrErr = h.astraonGet(ctx, fmt.Sprintf("/v1/analytics/creator/%s/posts?days=%s&sort_by=%s&limit=20", pialID, daysStr, sortBy), &cp)
	if astrErr == nil {
		posts = &cp
	} else {
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

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "creator_analytics", map[string]interface{}{
		"Summary":       summary,
		"Posts":         posts,
		"Timeline":      timeline,
		"Audience":      audience,
		"Breakdown":     breakdown,
		"AstraonOnline": astrErr == nil,
		"Period":        days,
		"SortBy":        sortBy,
		"User":          user,
		"ActiveSection": "analytics",
	})
}

// creatorPanelContent serves GET /create/partials/content
func (h *Handler) creatorPanelContent(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsCreator && !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}

	var works []*model.Work
	if h.db != nil {
		works, _ = dbpkg.GetWorksForProfile(h.db, user.PIALID, 20, "")
		for _, wk := range works {
			wk.TimeAgo = TimeAgo(wk.CreatedAt)
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "creator_content", map[string]interface{}{
		"Works":         works,
		"User":          user,
		"ActiveSection": "content",
	})
}

// creatorPanelAudience serves GET /create/partials/audience
func (h *Handler) creatorPanelAudience(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsCreator && !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}

	var subCount, followerCount, followingCount int
	if h.db != nil {
		subCount = dbpkg.GetSubscriberCount(h.db, user.ID)
		_ = h.db.QueryRow(
			`SELECT COUNT(*) FROM follows WHERE following_id = $1 AND unfollowed_at IS NULL`,
			user.ID,
		).Scan(&followerCount)
		_ = h.db.QueryRow(
			`SELECT COUNT(*) FROM follows WHERE follower_id = $1 AND unfollowed_at IS NULL`,
			user.ID,
		).Scan(&followingCount)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "creator_audience", map[string]interface{}{
		"SubCount":       subCount,
		"FollowerCount":  followerCount,
		"FollowingCount": followingCount,
		"User":           user,
		"ActiveSection":  "audience",
	})
}

// creatorPanelSettings serves GET /create/partials/settings
func (h *Handler) creatorPanelSettings(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsCreator && !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "creator_settings", map[string]interface{}{
		"User":          user,
		"ActiveSection": "settings",
	})
}

// ── D-070 creator plan mutation events ───────────────────────────────────────
// All three forward to POST /thessalon/v1/plans (→ Themis) with PIAL auth.

// creatorPlanCreateEvent handles event_type=creator.plan.create (Lane 1).
// Forwards to POST /v1/plans on Themis with {creator_pial_id, name, price_aet, description}.
// On success: re-renders the memberships panel so the new plan appears instantly.
func (h *Handler) creatorPlanCreateEvent(w http.ResponseWriter, r *http.Request, raw map[string]json.RawMessage) {
	user := h.userFromRequest(w, r)
	if user == nil || (!user.IsCreator && !user.IsAdmin()) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	name := r.FormValue("name")
	priceStr := r.FormValue("price_aet")
	description := r.FormValue("description")
	if raw != nil {
		if name == "" { json.Unmarshal(raw["name"], &name) }
		if priceStr == "" { json.Unmarshal(raw["price_aet"], &priceStr) }
		if description == "" { json.Unmarshal(raw["description"], &description) }
	}
	if strings.TrimSpace(name) == "" {
		htmxError(w, r, "Plan name is required", http.StatusBadRequest)
		return
	}

	var priceAet int
	fmt.Sscanf(priceStr, "%d", &priceAet)
	if priceAet < 0 {
		htmxError(w, r, "Price cannot be negative", http.StatusBadRequest)
		return
	}

	if h.themisURL != "" && user.PIALID != "" {
		payload := map[string]interface{}{
			"creator_pial_id": user.PIALID,
			"name":            name,
			"price_aet":       priceAet,
			"description":     description,
		}
		body, _ := json.Marshal(payload)
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.themisURL+"/v1/plans", bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Pial-Identity", user.PIALID)
			req.Header.Set("X-Pial-Handle", user.Handle)
			resp, respErr := h.httpClient.Do(req)
			if respErr != nil || resp.StatusCode >= 400 {
				htmxError(w, r, "Could not create plan — please try again", http.StatusBadGateway)
				return
			}
			resp.Body.Close()
		}
	}

	// Re-render the memberships panel with the updated plan list.
	h.creatorPanelMemberships(w, r)
}

// creatorPlanUpdateEvent handles event_type=creator.plan.update (Lane 1).
// Forwards to PUT /v1/plans/{id} on Themis.
func (h *Handler) creatorPlanUpdateEvent(w http.ResponseWriter, r *http.Request, raw map[string]json.RawMessage) {
	user := h.userFromRequest(w, r)
	if user == nil || (!user.IsCreator && !user.IsAdmin()) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	planID := r.FormValue("plan_id")
	name := r.FormValue("name")
	priceStr := r.FormValue("price_aet")
	description := r.FormValue("description")
	if raw != nil {
		if planID == "" { json.Unmarshal(raw["plan_id"], &planID) }
		if name == "" { json.Unmarshal(raw["name"], &name) }
		if priceStr == "" { json.Unmarshal(raw["price_aet"], &priceStr) }
		if description == "" { json.Unmarshal(raw["description"], &description) }
	}
	if planID == "" {
		http.Error(w, "plan_id required", http.StatusBadRequest)
		return
	}

	var priceAet int
	fmt.Sscanf(priceStr, "%d", &priceAet)

	if h.themisURL != "" && user.PIALID != "" {
		payload := map[string]interface{}{
			"name":        name,
			"price_aet":   priceAet,
			"description": description,
		}
		body, _ := json.Marshal(payload)
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, h.themisURL+"/v1/plans/"+planID, bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Pial-Identity", user.PIALID)
			req.Header.Set("X-Pial-Handle", user.Handle)
			resp, respErr := h.httpClient.Do(req)
			if respErr != nil || resp.StatusCode >= 400 {
				htmxError(w, r, "Could not update plan — please try again", http.StatusBadGateway)
				return
			}
			resp.Body.Close()
		}
	}

	h.creatorPanelMemberships(w, r)
}

// creatorPlanDeleteEvent handles event_type=creator.plan.delete (Lane 1).
// Forwards to DELETE /v1/plans/{id} on Themis.
func (h *Handler) creatorPlanDeleteEvent(w http.ResponseWriter, r *http.Request, raw map[string]json.RawMessage) {
	user := h.userFromRequest(w, r)
	if user == nil || (!user.IsCreator && !user.IsAdmin()) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	planID := r.FormValue("plan_id")
	if planID == "" && raw != nil {
		json.Unmarshal(raw["plan_id"], &planID)
	}
	if planID == "" {
		http.Error(w, "plan_id required", http.StatusBadRequest)
		return
	}

	if h.themisURL != "" && user.PIALID != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, h.themisURL+"/v1/plans/"+planID, nil)
		if err == nil {
			req.Header.Set("X-Pial-Identity", user.PIALID)
			req.Header.Set("X-Pial-Handle", user.Handle)
			resp, respErr := h.httpClient.Do(req)
			if respErr != nil || resp.StatusCode >= 400 {
				htmxError(w, r, "Could not delete plan — please try again", http.StatusBadGateway)
				return
			}
			resp.Body.Close()
		}
	}

	h.creatorPanelMemberships(w, r)
}
