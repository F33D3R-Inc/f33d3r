package handler

import (
	"encoding/json"
	"net/http"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

// GET /api/referral/code
// Returns or creates a unique referral code for the current user.
// Response: { "code": "alice-a1b2c3", "link": "https://f33d3r.com/join?ref=alice-a1b2c3" }
func (h *Handler) referralCode(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.ID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	code, err := dbpkg.GetOrCreateReferralCode(h.db, user.ID, user.Handle)
	if err != nil {
		http.Error(w, "could not generate code", http.StatusInternalServerError)
		return
	}

	link := "https://f33d3r.com/join?ref=" + code
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"code": code,
		"link": link,
	})
}

// GET /api/referral/stats
// Returns referral statistics for the current user.
// Response: { "code": "alice-a1b2c3", "total_referred": 3, "verified_count": 1, "pending_count": 2 }
func (h *Handler) referralStats(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.ID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	stats, err := dbpkg.GetReferralStats(h.db, user.ID, user.Handle)
	if err != nil {
		http.Error(w, "could not fetch stats", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"code":           stats.Code,
		"total_referred": stats.TotalReferred,
		"verified_count": stats.VerifiedCount,
		"pending_count":  stats.PendingCount,
	})
}

// GET /facets/referral_panel
// Returns the referral panel HTML partial for the creator dashboard/settings.
func (h *Handler) facetReferralPanel(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil || user.ID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	stats, err := dbpkg.GetReferralStats(h.db, user.ID, user.Handle)
	if err != nil {
		http.Error(w, "could not fetch referral data", http.StatusInternalServerError)
		return
	}

	link := "https://f33d3r.com/join?ref=" + stats.Code
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "referral_panel", map[string]interface{}{
		"User":          user,
		"ReferralCode":  stats.Code,
		"ReferralLink":  link,
		"TotalReferred": stats.TotalReferred,
		"VerifiedCount": stats.VerifiedCount,
		"PendingCount":  stats.PendingCount,
	})
}
