package handler

// analytics.go — the creator analytics page, the creator panel's analytics
// tab and the admin analytics panel. Every number is read from feed-engine's
// own rows (internal/db/analytics.go) and rendered here; nothing is fetched
// from another service.

import (
	"log"
	"net/http"
	"strings"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

// creatorAnalytics is everything an analytics surface renders for one creator.
type creatorAnalytics struct {
	Summary   *dbpkg.CreatorSummary
	Posts     *analyticsWorks
	Timeline  *dbpkg.CreatorTimeline
	Audience  *dbpkg.CreatorAudience
	Breakdown *dbpkg.CreatorBreakdown
}

// analyticsWorkRow is one row of the analytics_post_row Facet.
type analyticsWorkRow struct {
	dbpkg.CreatorWorkRow
	TimeAgo string
}

type analyticsWorks struct {
	Posts  []analyticsWorkRow
	SortBy string
	Period int
}

// analyticsPeriod reads ?period=7|30|90|all; anything else is the 30-day default.
func analyticsPeriod(r *http.Request) int {
	if d, ok := dbpkg.AnalyticsPeriods[r.URL.Query().Get("period")]; ok {
		return d
	}
	return 30
}

// analyticsSort reads ?sort=; anything not in the vocabulary is impressions.
func analyticsSort(r *http.Request) string {
	s := r.URL.Query().Get("sort")
	if _, ok := dbpkg.AnalyticsSorts[s]; ok {
		return s
	}
	return "impressions"
}

func (h *Handler) analyticsWorks(accountID string, days int, sortBy string) (*analyticsWorks, error) {
	cw, err := dbpkg.GetCreatorWorks(h.db, accountID, days, sortBy, 20)
	if err != nil {
		return nil, err
	}
	out := &analyticsWorks{Posts: make([]analyticsWorkRow, 0, len(cw.Posts)), SortBy: cw.SortBy, Period: cw.Period}
	for _, p := range cw.Posts {
		out.Posts = append(out.Posts, analyticsWorkRow{CreatorWorkRow: p, TimeAgo: TimeAgo(p.CreatedAt)})
	}
	return out, nil
}

// loadCreatorAnalytics reads every section. full adds the charts the creator
// panel does not draw.
func (h *Handler) loadCreatorAnalytics(accountID string, days int, sortBy string, full bool) (*creatorAnalytics, error) {
	var a creatorAnalytics
	var err error
	if a.Summary, err = dbpkg.GetCreatorSummary(h.db, accountID, days); err != nil {
		return nil, err
	}
	if a.Posts, err = h.analyticsWorks(accountID, days, sortBy); err != nil {
		return nil, err
	}
	if !full {
		return &a, nil
	}
	if a.Timeline, err = dbpkg.GetCreatorTimeline(h.db, accountID, days); err != nil {
		return nil, err
	}
	if a.Audience, err = dbpkg.GetCreatorAudience(h.db, accountID, days); err != nil {
		return nil, err
	}
	if a.Breakdown, err = dbpkg.GetCreatorBreakdown(h.db, accountID, days); err != nil {
		return nil, err
	}
	return &a, nil
}

// analyticsUnavailable answers a read that failed. The page is not drawn with
// sections missing: an empty section would read as "no activity", which is a
// different and false statement.
func analyticsUnavailable(w http.ResponseWriter, where string, err error) {
	log.Printf("[analytics] %s: %v", where, err)
	http.Error(w, "Analytics are unavailable right now. Try again shortly.", http.StatusServiceUnavailable)
}

// ── Analytics page ────────────────────────────────────────────────────────────

func (h *Handler) analyticsPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	days := analyticsPeriod(r)
	sortBy := analyticsSort(r)

	a, err := h.loadCreatorAnalytics(user.ID, days, sortBy, true)
	if err != nil {
		analyticsUnavailable(w, "analytics page for "+user.ID, err)
		return
	}

	data := h.baseData(user)
	data["Title"] = "Analytics · F33D3R"
	data["TargetHandle"] = user.Handle
	data["Period"] = days
	data["SortBy"] = sortBy
	data["Summary"] = a.Summary
	data["Posts"] = a.Posts
	data["Timeline"] = a.Timeline
	data["Audience"] = a.Audience
	data["Breakdown"] = a.Breakdown
	h.render(w, r, "analytics.html", h.withRail(data, user, "default"))
}

// creatorPanelAnalytics serves GET /create/partials/analytics: the creator
// panel's analytics tab, the last 30 days of the creator's own works.
func (h *Handler) creatorPanelAnalytics(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if h.denyCreatorPanel(w, user) {
		return
	}
	const days = 30
	sortBy := analyticsSort(r)

	a, err := h.loadCreatorAnalytics(user.ID, days, sortBy, false)
	if err != nil {
		analyticsUnavailable(w, "creator panel analytics for "+user.ID, err)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "creator_analytics", map[string]interface{}{
		"Summary":       a.Summary,
		"Posts":         a.Posts,
		"Period":        days,
		"SortBy":        sortBy,
		"User":          user,
		"ActiveSection": "analytics",
	})
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

	site, err := dbpkg.GetSiteAnalytics(h.db)
	if err != nil {
		analyticsUnavailable(w, "admin site analytics", err)
		return
	}

	handle := strings.TrimSpace(r.URL.Query().Get("handle"))
	data := map[string]interface{}{"SiteSummary": site, "QueriedHandle": handle, "LookupHandle": ""}
	if handle != "" {
		target, err := dbpkg.GetUserByHandle(h.db, handle)
		if err != nil {
			analyticsUnavailable(w, "admin analytics lookup of @"+handle, err)
			return
		}
		// An unknown handle renders the panel with nothing looked up, which the
		// Facet states as "no account".
		if target != nil {
			a, err := h.loadCreatorAnalytics(target.ID, 30, "impressions", false)
			if err != nil {
				analyticsUnavailable(w, "admin analytics of "+target.ID, err)
				return
			}
			data["LookupHandle"] = target.Handle
			data["UserSummary"] = a.Summary
			data["UserPosts"] = a.Posts
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "admin_analytics_panel", data)
}
