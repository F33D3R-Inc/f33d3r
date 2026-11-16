package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// denyCreatorPanel is the single admission check every creator-OS partial runs.
//
// It exists because the eight /create/partials/* endpoints each carried their
// own copy of the membership check and none of them carried the verification
// check at all — so /create rendered the gate while /create/partials/earnings
// served the earnings surface straight out to an unidentified account. One
// function means the next panel added cannot inherit half the rule.
//
// Two questions, in order: is this a creator surface you belong on at all
// (creator or admin), and has the person behind it been identified
// (model.CanMonetize — identity tier, admin bypass preserved).
//
// Returns true when the response has been written and the caller must return.
func (h *Handler) denyCreatorPanel(w http.ResponseWriter, user *model.User) bool {
	if !user.IsCreator && !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return true
	}
	if !user.CanMonetize() {
		http.Error(w, "403 Forbidden — creator features require identity verification", http.StatusForbidden)
		return true
	}
	return false
}

// creatorDashboardPage serves GET /create — the creator OS dashboard.
// Auth: must be creator or admin.
func (h *Handler) creatorDashboardPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsCreator && !user.IsAdmin() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	// The identity gate. creator_verification_required.html was built for exactly
	// this and registered as a page, but nothing ever rendered it, so the creator
	// OS was reachable by an account nobody had identified.
	//
	// This used to read !user.IsVerified. IsVerified is PIAL age_verified, which
	// SetPIALBirthday sets TRUE from the birthday typed into the signup form — so
	// that gate admitted every account that finished signup, and could not tell a
	// verified public figure from an anonymous one. Creator status is an identity
	// question, so it asks the identity predicate. See model.CanMonetize.
	if !user.CanMonetize() {
		h.render(w, r, "creator_verification_required.html", h.withRail(map[string]interface{}{
			"User":  user,
			"Title": "Verification Required · F33D3R",
		}, user, "default"))
		return
	}

	var artistStats dbpkg.ArtistStats
	var subCount int
	if h.db != nil {
		stats, err := dbpkg.GetArtistStats(h.db, user.ID)
		if err != nil {
			log.Printf("[creator] %v", err)
			http.Error(w, "the dashboard is unavailable right now", http.StatusInternalServerError)
			return
		}
		artistStats = stats
		subCount = dbpkg.GetSubscriberCount(h.db, user.ID)
	}

	h.render(w, r, "creator_dashboard.html", h.withRail(map[string]interface{}{
		"User":          user,
		"Title":         "Creator Dashboard · F33D3R",
		"SessionID":     uuid.New().String(),
		"ArtistStats":   artistStats,
		"SubCount":      subCount,
		"ActiveSection": "overview",
		"Themes":        ThemesWithActive(user.ThemeID),
	}, user, "default"))
}

// creatorPanelOverview serves GET /create/partials/overview
func (h *Handler) creatorPanelOverview(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if h.denyCreatorPanel(w, user) {
		return
	}

	var artistStats dbpkg.ArtistStats
	var subCount int
	var recentTracks []*model.Track
	if h.db != nil {
		tracks, stats, err := h.artistTracks(r.Context(), user, 5)
		if err != nil {
			log.Printf("[creator] tracks of %s: %v", user.ID, err)
			http.Error(w, "the dashboard is unavailable right now", http.StatusInternalServerError)
			return
		}
		recentTracks, artistStats = tracks, stats
		subCount = dbpkg.GetSubscriberCount(h.db, user.ID)
	}

	earningsDisplay := h.creatorEarningsDisplay(r.Context(), user)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "creator_overview", map[string]interface{}{
		"ArtistStats":     artistStats,
		"SubCount":        subCount,
		"EarningsDisplay": earningsDisplay,
		"RecentTracks":    recentTracks,
		"User":            user,
		"ActiveSection":   "overview",
	})
}

// creatorPanelMusic serves GET /create/partials/music
func (h *Handler) creatorPanelMusic(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if h.denyCreatorPanel(w, user) {
		return
	}

	var myTracks []*model.Track
	var artistStats dbpkg.ArtistStats
	if h.db != nil {
		tracks, stats, err := h.artistTracks(r.Context(), user, 100)
		if err != nil {
			log.Printf("[creator] tracks of %s: %v", user.ID, err)
			http.Error(w, "your tracks are unavailable right now", http.StatusInternalServerError)
			return
		}
		myTracks, artistStats = tracks, stats
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
	if h.denyCreatorPanel(w, user) {
		return
	}

	var subCount int
	plans := []model.ShopPlan{}
	if h.db != nil {
		var err error
		if plans, err = h.shopPlans(r.Context(), user.ID); err != nil {
			log.Printf("[creator] plans of %s: %v", user.ID, err)
			http.Error(w, "your plans are unavailable right now", http.StatusInternalServerError)
			return
		}
		subCount = dbpkg.GetSubscriberCount(h.db, user.ID)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "creator_memberships", map[string]interface{}{
		"Plans":         plans,
		"SubCount":      subCount,
		"User":          user,
		"ActiveSection": "memberships",
		"CreatorPIALID": user.PIALID,
	})
}

// creatorPanelEarnings serves GET /create/partials/earnings
func (h *Handler) creatorPanelEarnings(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if h.denyCreatorPanel(w, user) {
		return
	}

	var subCount int
	plans := []model.ShopPlan{}
	if h.db != nil {
		var err error
		if plans, err = h.shopPlans(r.Context(), user.ID); err != nil {
			log.Printf("[creator] plans of %s: %v", user.ID, err)
			http.Error(w, "your plans are unavailable right now", http.StatusInternalServerError)
			return
		}
		subCount = dbpkg.GetSubscriberCount(h.db, user.ID)
	}

	subsDisplay := fmt.Sprintf("%d active subscriber(s)", subCount)
	earningsDisplay := h.creatorEarningsDisplay(r.Context(), user)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "creator_earnings", map[string]interface{}{
		"SubCount":        subCount,
		"EarningsDisplay": earningsDisplay,
		"SubsDisplay":     subsDisplay,
		"Plans":           plans,
		"User":            user,
		"ActiveSection":   "earnings",
	})
}

// creatorPanelContent serves GET /create/partials/content
func (h *Handler) creatorPanelContent(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if h.denyCreatorPanel(w, user) {
		return
	}

	var works []*model.Work
	if h.db != nil {
		works, _ = dbpkg.GetWorksForProfile(h.db, user.PIALID, user.ID, 20, "")
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
	if h.denyCreatorPanel(w, user) {
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
	if h.denyCreatorPanel(w, user) {
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "creator_settings", map[string]interface{}{
		"User":          user,
		"ActiveSection": "settings",
	})
}

// ── Creator plan mutation events ─────────────────────────────────────────────
// Themis owns plans: each event below is a call to Themis made for the session's
// identity (X-Pial-Identity), with the price a person typed as decimal AET
// turned into AET units with the ledger's own unit. The memberships panel is
// redrawn from the projection, which commerce.events keeps current.

// creatorEarningsDisplay is what Themis records the creator as having earned,
// or a dash when Themis cannot say.
func (h *Handler) creatorEarningsDisplay(ctx context.Context, user *model.User) string {
	if user == nil || user.PIALID == "" {
		return "—"
	}
	status, body, err := h.themisGet(ctx, "/v1/creator/"+user.PIALID+"/earnings", user)
	if err != nil || status != http.StatusOK {
		log.Printf("[creator] earnings of %s: status=%d err=%v", user.ID, status, err)
		return "—"
	}
	var e struct {
		TotalDisplay string `json:"total_display"`
	}
	if err := json.Unmarshal(body, &e); err != nil || e.TotalDisplay == "" {
		log.Printf("[creator] earnings of %s unreadable: %v", user.ID, err)
		return "—"
	}
	return e.TotalDisplay
}

// planFormValue reads one plan form field from either the form or a JSON body.
func planFormValue(r *http.Request, raw map[string]json.RawMessage, key string) string {
	v := r.FormValue(key)
	if v == "" && raw != nil {
		_ = json.Unmarshal(raw[key], &v)
	}
	return strings.TrimSpace(v)
}

// planPriceUnits reads the plan form's decimal AET price into AET units.
func (h *Handler) planPriceUnits(r *http.Request, raw map[string]json.RawMessage) (int64, string) {
	unit, err := h.aetUnit(r.Context())
	if err != nil {
		log.Printf("[creator] plan price: %v", err)
		return 0, "Plans are unavailable right now — please try again"
	}
	units, err := parseAETAmount(planFormValue(r, raw, "price"), unit)
	if err != nil || units <= 0 {
		return 0, "Enter a monthly price above zero, like 5 or 5.50"
	}
	return units, ""
}

// creatorPlanCreateEvent handles event_type=creator.plan.create.
func (h *Handler) creatorPlanCreateEvent(w http.ResponseWriter, r *http.Request, raw map[string]json.RawMessage) {
	user := h.userFromRequest(w, r)
	if user == nil || (!user.IsCreator && !user.IsAdmin()) || user.PIALID == "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	name := planFormValue(r, raw, "name")
	if name == "" {
		htmxError(w, r, "Plan name is required", http.StatusBadRequest)
		return
	}
	units, problem := h.planPriceUnits(r, raw)
	if problem != "" {
		htmxError(w, r, problem, http.StatusBadRequest)
		return
	}
	status, body, err := h.themisCall(r.Context(), http.MethodPost, "/v1/plans", user, map[string]interface{}{
		"creator_account": user.ID,
		"name":            name,
		"price_units":     units,
		"description":     planFormValue(r, raw, "description"),
	})
	if err != nil || status != http.StatusOK {
		log.Printf("[creator] plan create for %s: status=%d err=%v", user.ID, status, err)
		htmxError(w, r, themisRefusal(body, "Could not create plan — please try again"), http.StatusBadGateway)
		return
	}
	var plan struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &plan) == nil && plan.ID != "" {
		h.awaitPlanProjection(r.Context(), plan.ID)
	}
	h.creatorPanelMemberships(w, r)
}

// creatorPlanUpdateEvent handles event_type=creator.plan.update.
func (h *Handler) creatorPlanUpdateEvent(w http.ResponseWriter, r *http.Request, raw map[string]json.RawMessage) {
	user := h.userFromRequest(w, r)
	if user == nil || (!user.IsCreator && !user.IsAdmin()) || user.PIALID == "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	planID := planFormValue(r, raw, "plan_id")
	if _, err := uuid.Parse(planID); err != nil {
		http.Error(w, "plan_id required", http.StatusBadRequest)
		return
	}
	units, problem := h.planPriceUnits(r, raw)
	if problem != "" {
		htmxError(w, r, problem, http.StatusBadRequest)
		return
	}
	status, body, err := h.themisCall(r.Context(), http.MethodPut, "/v1/plans/"+planID, user, map[string]interface{}{
		"name":        planFormValue(r, raw, "name"),
		"price_units": units,
		"description": planFormValue(r, raw, "description"),
	})
	if err != nil || status != http.StatusOK {
		log.Printf("[creator] plan update %s for %s: status=%d err=%v", planID, user.ID, status, err)
		htmxError(w, r, themisRefusal(body, "Could not update plan — please try again"), http.StatusBadGateway)
		return
	}
	h.awaitPlanProjection(r.Context(), planID)
	h.creatorPanelMemberships(w, r)
}

// awaitPlanProjection waits briefly until the projection holds the plan as
// Themis just answered, so the redrawn panel reads its own write.
func (h *Handler) awaitPlanProjection(ctx context.Context, planID string) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for {
		seen, err := dbpkg.PlanProjectedSince(ctx, h.db, planID, time.Now().Add(-5*time.Second))
		if err == nil && seen {
			return
		}
		select {
		case <-ctx.Done():
			log.Printf("[creator] projection of plan %s did not catch up within 3s", planID)
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// creatorPlanDeleteEvent handles event_type=creator.plan.delete: the plan is
// retired in Themis. Its subscribers run to the end of their paid period.
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

	status, body, err := h.themisCall(r.Context(), http.MethodDelete, "/v1/plans/"+planID, user, nil)
	if err != nil || status != http.StatusOK {
		log.Printf("[creator] plan retire %s for %s: status=%d err=%v", planID, user.ID, status, err)
		htmxError(w, r, themisRefusal(body, "Could not delete plan — please try again"), http.StatusBadGateway)
		return
	}
	h.awaitPlanProjection(r.Context(), planID)
	h.creatorPanelMemberships(w, r)
}
