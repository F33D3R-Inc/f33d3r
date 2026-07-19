package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/cron"
	"github.com/f33d3r/feed-engine/internal/model"
)

func (h *Handler) adminPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}

	var userCount, postCount, pialCount, activeSessions int
	if h.db != nil {
		h.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&userCount)
		h.db.QueryRow(`SELECT COUNT(*) FROM works WHERE deleted_at IS NULL`).Scan(&postCount)
		h.db.QueryRow(`SELECT COUNT(*) FROM pial_roots WHERE is_tombstoned = FALSE`).Scan(&pialCount)
		h.db.QueryRow(`SELECT COUNT(*) FROM user_sessions WHERE expires_at > NOW()`).Scan(&activeSessions)
	}

	pendingReports, _ := dbpkg.GetPendingReports(h.db, 50)
	siteAnalytics, _  := dbpkg.GetSiteAnalytics(h.db)

	var dmcaCount, orgCount int
	if h.db != nil {
		h.db.QueryRow(`SELECT COUNT(*) FROM dmca_requests WHERE status = 'pending'`).Scan(&dmcaCount)
		h.db.QueryRow(`SELECT COUNT(*) FROM org_verification_applications WHERE status = 'pending'`).Scan(&orgCount)
	}
	securityCount := PendingSecurityCount(h.db)

	// Detect section from URL path so back-button loads correct panel server-side.
	section := "overview"
	if s := r.PathValue("section"); s != "" {
		section = s
	} else if p := strings.TrimPrefix(r.URL.Path, "/admin/"); p != "" && !strings.Contains(p, "/") {
		section = p
	}

	h.render(w, "admin.html", map[string]interface{}{
		"User":                 user,
		"Themes":               ThemesWithActive(user.ThemeID),
		"ActiveSection":        section,
		"UserCount":            userCount,
		"PostCount":            postCount,
		"PIALCount":            pialCount,
		"ActiveSessions":       activeSessions,
		"PendingReports":       pendingReports,
		"PendingReportCount":   len(pendingReports),
		"PendingDMCACount":     dmcaCount,
		"PendingOrgCount":      orgCount,
		"PendingSecurityCount": securityCount,
		"SiteAnalytics":        siteAnalytics,
	})
}

// adminPanelOverview renders the Overview tab panel for HTMX swap.
func (h *Handler) adminPanelOverview(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}

	var userCount, postCount, pialCount, activeSessions int
	if h.db != nil {
		h.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&userCount)
		h.db.QueryRow(`SELECT COUNT(*) FROM works WHERE deleted_at IS NULL`).Scan(&postCount)
		h.db.QueryRow(`SELECT COUNT(*) FROM pial_roots WHERE is_tombstoned = FALSE`).Scan(&pialCount)
		h.db.QueryRow(`SELECT COUNT(*) FROM user_sessions WHERE expires_at > NOW()`).Scan(&activeSessions)
	}

	pendingReports, _ := dbpkg.GetPendingReports(h.db, 50)
	siteAnalytics, _  := dbpkg.GetSiteAnalytics(h.db)

	var dmcaCount, orgCount int
	if h.db != nil {
		h.db.QueryRow(`SELECT COUNT(*) FROM dmca_requests WHERE status = 'pending'`).Scan(&dmcaCount)
		h.db.QueryRow(`SELECT COUNT(*) FROM org_verification_applications WHERE status = 'pending'`).Scan(&orgCount)
	}
	secCount := PendingSecurityCount(h.db)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "admin_panel_overview", map[string]interface{}{
		"UserCount":            userCount,
		"PostCount":            postCount,
		"PIALCount":            pialCount,
		"ActiveSessions":       activeSessions,
		"PendingReportCount":   len(pendingReports),
		"PendingDMCACount":     dmcaCount,
		"PendingOrgCount":      orgCount,
		"PendingSecurityCount": secCount,
		"SiteAnalytics":        siteAnalytics,
	})
}

// adminPanelUsers renders the Users tab panel for HTMX swap.
// Accepts optional ?handle= param for inline user lookup.
func (h *Handler) adminPanelUsers(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}

	lookupHandle := strings.TrimSpace(r.URL.Query().Get("handle"))
	var lookupUser *model.User
	var lookupCaps model.PIALCapabilityMap
	if lookupHandle != "" && h.db != nil {
		if lu, _ := dbpkg.GetUserByHandle(h.db, lookupHandle); lu != nil {
			lookupUser = lu
			lookupCaps = dbpkg.GetCapabilities(h.db, lu.PIALID)
		}
	}

	recentUsers, _ := dbpkg.GetRecentUsers(h.db, 20)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "admin_panel_users", map[string]interface{}{
		"LookupUser":   lookupUser,
		"LookupCaps":   lookupCaps,
		"LookupHandle": lookupHandle,
		"AllCaps":      model.DefaultCapabilities,
		"RecentUsers":  recentUsers,
		"CurrentUser":  user,
	})
}

// adminPanelModeration renders the Moderation tab panel for HTMX swap.
func (h *Handler) adminPanelModeration(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}

	pendingReports, _ := dbpkg.GetPendingReports(h.db, 50)
	reviewQueue, _ := dbpkg.GetHumanReviewPosts(h.db, 50)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "admin_panel_moderation", map[string]interface{}{
		"PendingReports": pendingReports,
		"ReviewQueue":    reviewQueue,
	})
}

// adminPanelDMCA renders the DMCA tab panel for HTMX swap.
func (h *Handler) adminPanelDMCA(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}

	var requests []DMCARequest
	if h.db != nil {
		rows, err := h.db.Query(`
			SELECT ticket_id, claimant_name, claimant_email, infringing_url,
			       COALESCE(original_url,''), COALESCE(description,''), status, created_at
			FROM dmca_requests
			WHERE status = 'pending'
			ORDER BY created_at DESC LIMIT 100`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var req DMCARequest
				rows.Scan(&req.TicketID, &req.ClaimantName, &req.ClaimantEmail,
					&req.InfringingURL, &req.OriginalURL, &req.Description,
					&req.Status, &req.CreatedAt)
				requests = append(requests, req)
			}
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "admin_panel_dmca", map[string]interface{}{
		"DMCARequests": requests,
	})
}

// adminPanelDevtools renders the Dev Tools tab panel for HTMX swap.
func (h *Handler) adminPanelDevtools(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "admin_panel_devtools", map[string]interface{}{
		"User": user,
	})
}

// adminMigrateWorks — migration is complete; posts table is empty, works has all data.
// This endpoint is now a no-op stub. POST /api/admin/migrate-works
func (h *Handler) adminMigrateWorks(w http.ResponseWriter, r *http.Request) {
	// TODO_WORKS: migration complete — endpoint can be removed in a future cleanup pass.
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true,"message":"Migration already complete. posts table is empty; works has all data."}`))
}

// adminSetCap is an HTMX-friendly endpoint to set a PIAL capability.
// Returns an HTML fragment confirming the action.
func (h *Handler) adminSetCap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", http.StatusMethodNotAllowed)
		return
	}
	caller := h.userFromRequest(w, r)
	if !caller.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	_ = r.ParseForm()
	targetHandle := strings.TrimSpace(r.FormValue("target_handle"))
	capability   := r.FormValue("capability")
	state        := r.FormValue("state")
	reason       := strings.TrimSpace(r.FormValue("reason"))

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if targetHandle == "" || capability == "" || state == "" {
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Missing fields.</span>`))
		return
	}

	target, _ := dbpkg.GetUserByHandle(h.db, targetHandle)
	if target == nil || target.PIALID == "" {
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">User not found.</span>`))
		return
	}

	if err := dbpkg.SetCapability(h.db, target.PIALID, capability, state, reason, caller.Handle, nil); err != nil {
		log.Printf("[admin] set-cap error for @%s: %v", targetHandle, err)
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">DB error — check logs.</span>`))
		return
	}

	dbpkg.LogPIALEvent(h.db, target.PIALID, target.ID, "capability_changed", map[string]interface{}{
		"capability": capability, "state": state, "reason": reason, "by": caller.Handle,
	}, "admin")

	// Evict the target's session cache so the new capability applies on their next request
	// instead of lagging up to the 30s TTL. (Token-keyed entries self-expire; the handle key
	// is evicted immediately.)
	h.sessionCache.Delete("__handle__" + targetHandle)

	log.Printf("[admin] %s set capability %s=%s on @%s (%s)", caller.Handle, capability, state, targetHandle, reason)
	fmt.Fprintf(w, `<span style="color:#22c55e;font-size:12px;font-weight:500">✓ %s → %s for @%s</span>`, capability, state, targetHandle)
}

// adminSetRole updates a user's platform role, identity badge, and permission flags.
// Form fields:
//   platform_role  — "user" | "admin" | "founder"
//   badge_type     — "" | "business" | "government"
//   is_verified    — "1" if checked
//   is_creator     — "1" if checked
func (h *Handler) adminSetRole(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", http.StatusMethodNotAllowed)
		return
	}
	caller := h.userFromRequest(w, r)
	if !caller.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	_ = r.ParseForm()
	targetHandle := strings.TrimSpace(r.FormValue("target_handle"))
	platformRole := r.FormValue("platform_role")
	badgeType    := r.FormValue("badge_type")
	isVerified   := r.FormValue("is_verified") == "1"
	isCreator    := r.FormValue("is_creator") == "1"

	// platform_role: only these three are valid — creator is a perm, not a role.
	allowedRoles := map[string]bool{"user": true, "admin": true, "founder": true}
	if !allowedRoles[platformRole] { platformRole = "user" }
	// Founder role can only be held by @tehanibentley — prevent accidental or malicious assignment.
	if platformRole == "founder" && targetHandle != "tehanibentley" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Founder role cannot be assigned to this account.</span>`))
		return
	}

	// badge_type: identity classification badge, stored in official_type column.
	allowedBadges := map[string]bool{"": true, "business": true, "government": true}
	if !allowedBadges[badgeType] { badgeType = "" }

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if h.db != nil && targetHandle != "" {
		// PIAL is the single record of truth for role + verification. The admin "Verified"
		// checkbox is the manual KYC override (a trusted human stands in for eKYC) — it
		// writes age_verified to PIAL with admin provenance, and is fully reversible.
		target, _ := dbpkg.GetUserByHandle(h.db, targetHandle)
		if target == nil || target.PIALID == "" {
			w.Write([]byte(`<span style="color:#ef4444;font-size:12px">User not found.</span>`))
			return
		}
		// Surface write failures instead of discarding them behind a green "✓ Updated".
		if err := dbpkg.SetPIALRole(h.db, target.PIALID, platformRole); err != nil {
			log.Printf("[admin] SetPIALRole failed for @%s: %v", targetHandle, err)
			w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Role write failed — check logs.</span>`))
			return
		}
		if isVerified {
			if err := dbpkg.SetPIALVerified(h.db, target.PIALID, "full", "admin:"+caller.Handle); err != nil {
				log.Printf("[admin] SetPIALVerified failed for @%s: %v", targetHandle, err)
				w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Verify write failed — check logs.</span>`))
				return
			}
		} else {
			// De-verify through the PIAL setter (records the reversal in the ledger and
			// recomputes the state hash — the raw UPDATE it replaces skipped both).
			if err := dbpkg.SetPIALUnverified(h.db, target.PIALID, "admin:"+caller.Handle); err != nil {
				log.Printf("[admin] SetPIALUnverified failed for @%s: %v", targetHandle, err)
				w.Write([]byte(`<span style="color:#ef4444;font-size:12px">De-verify write failed — check logs.</span>`))
				return
			}
		}
		// is_creator and official_type are separate axes (perm/badge), not part of the
		// verification/role fold — they stay on user_profiles. The is_verified and role
		// projections are kept in sync by the PIAL setters above.
		if _, err := h.db.Exec(`
			UPDATE user_profiles
			SET is_creator = $1, official_type = $2
			WHERE user_id = (SELECT id FROM users WHERE handle = $3)`,
			isCreator, badgeType, targetHandle); err != nil {
			log.Printf("[admin] user_profiles badge/creator write failed for @%s: %v", targetHandle, err)
		}
		// Evict the target's session cache so role/verification apply on their next request
		// instead of lagging up to the 30s TTL.
		h.sessionCache.Delete("__handle__" + targetHandle)
		log.Printf("[admin] %s set platform_role=%s badge=%q verified=%v creator=%v on @%s",
			caller.Handle, platformRole, badgeType, isVerified, isCreator, targetHandle)
	}

	fmt.Fprintf(w, `<span style="color:#22c55e;font-size:12px;font-weight:500">✓ Updated @%s</span>`, targetHandle)
}

func (h *Handler) resolveReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "405", 405); return }
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	_ = r.ParseForm()
	contentID := strings.TrimSpace(r.FormValue("content_id"))
	status    := r.FormValue("status") // "resolved" or "dismissed"
	if contentID == "" || (status != "resolved" && status != "dismissed") {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if h.db != nil {
		// Close every open report on this item, then unstick the post if it was
		// only in human_review because of the report (no independent AI hold).
		_, _ = dbpkg.ResolveReportsForContent(h.db, contentID, status, user.Handle)
		_ = dbpkg.ClearReportDrivenReview(h.db, contentID, user.Handle)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<div id="admin-report-%s" class="admin-report-resolved">✓ %s by @%s</div>`,
		contentID, status, user.Handle)
}

func (h *Handler) reportContent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "405", 405); return }
	_ = r.ParseForm()
	user := h.userFromRequest(w, r)

	contentID   := strings.TrimSpace(r.FormValue("content_id"))
	contentType := r.FormValue("content_type")
	reason      := r.FormValue("reason")
	detail      := truncate(r.FormValue("detail"), 500)

	if contentID == "" || reason == "" {
		http.Error(w, "missing fields", http.StatusBadRequest)
		return
	}
	if contentType == "" { contentType = "post" }

	if h.db != nil {
		// One open report per reporter per item — the partial-unique index
		// uq_reports_pending_per_reporter backs this ON CONFLICT so a user mashing
		// the report button can't stack duplicate pending cards into the queue.
		h.db.Exec(`
			INSERT INTO content_reports (reporter_id, content_id, content_type, reason, detail)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (reporter_id, content_id) WHERE status = 'pending' DO NOTHING`,
			user.ID, contentID, contentType, reason, detail)

		dbpkg.LogPIALEvent(h.db, user.PIALID, user.ID, "content_reported", map[string]interface{}{
			"content_id": contentID, "reason": reason,
		}, "elohim-veni")

		// Reports for adult content or hate speech go straight to the human review
		// queue — admin must explicitly approve, age-gate, or block them.
		if contentType == "post" && (reason == "nsfw" || reason == "hate") {
			_ = dbpkg.SetPostScanState(h.db, contentID, "human_review", "user report: "+reason, "report_system")
			for _, adminID := range dbpkg.GetAdminUserIDs(h.db) {
				h.notifyUser(adminID, "moderation", "", contentID, "post")
			}
		}
	}

	// Forward to Zodacare safety brain (non-blocking).
	if h.cfg.ZodacareURL != "" {
		go func() {
			payload, _ := json.Marshal(map[string]interface{}{
				"reporter_pial_id": user.PIALID,
				"content_id":       contentID,
				"content_type":     contentType,
				"reason":           reason,
				"detail":           detail,
			})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.cfg.ZodacareURL+"/v1/reports", bytes.NewReader(payload))
			if err == nil {
				req.Header.Set("Content-Type", "application/json")
				resp, err := http.DefaultClient.Do(req)
				if err == nil { resp.Body.Close() }
			}
		}()
	}

	// Troll achievement: check report-based achievements on the content author
	if h.db != nil && contentType == "post" {
		go func() {
			var authorID string
			h.db.QueryRow(`SELECT author_id FROM works WHERE id = $1::uuid`, contentID).Scan(&authorID)
			if authorID == "" {
				h.db.QueryRow(`SELECT author_id FROM posts WHERE id = $1::uuid`, contentID).Scan(&authorID)
			}
			if authorID != "" {
				dbpkg.TryAwardTrollOnReported(h.db, authorID)
			}
		}()
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(`<span style="font-size:12px;color:var(--accent)">Reported — thank you.</span>`))
}

// adminInjectAET credits AET to a user's wallet via ain-soph.
func (h *Handler) adminInjectAET(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "405", 405); return }
	caller := h.userFromRequest(w, r)
	if !caller.IsAdmin() { http.Error(w, "403 Forbidden", 403); return }
	_ = r.ParseForm()
	targetHandle := strings.TrimSpace(r.FormValue("target_handle"))
	amountStr    := strings.TrimSpace(r.FormValue("amount"))
	reason       := strings.TrimSpace(r.FormValue("reason"))
	if reason == "" { reason = "admin_inject" }

	amount, err := strconv.ParseFloat(amountStr, 64)
	if err != nil || amount <= 0 || amount > 1000000 {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Invalid amount.</span>`))
		return
	}

	target, _ := dbpkg.GetUserByHandle(h.db, targetHandle)
	if target == nil || target.PIALID == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">User not found.</span>`))
		return
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"pial_id": target.PIALID,
		"amount":  amount,
		"reason":  reason,
		"actor":   caller.Handle,
	})
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		h.cfg.AinSophURL+"/v1/admin/credit", bytes.NewReader(payload))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err != nil {
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Request error.</span>`))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.httpClient.Do(req)
	if err != nil || resp.StatusCode >= 400 {
		log.Printf("[admin] AET inject failed for @%s: %v", targetHandle, err)
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Ain-Soph error — check logs.</span>`))
		return
	}
	resp.Body.Close()
	log.Printf("[admin] %s injected %.2f AET to @%s (%s)", caller.Handle, amount, targetHandle, reason)
	fmt.Fprintf(w, `<span style="color:#22c55e;font-size:12px;font-weight:500">✓ %.2f AET credited to @%s</span>`, amount, targetHandle)
}

// adminResetPassword sets a new password for a user directly (admin only).
func (h *Handler) adminResetPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "405", 405); return }
	caller := h.userFromRequest(w, r)
	if !caller.IsAdmin() { http.Error(w, "403 Forbidden", 403); return }
	_ = r.ParseForm()
	targetHandle := strings.TrimSpace(r.FormValue("target_handle"))
	newPassword  := r.FormValue("new_password")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if len(newPassword) < 8 {
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Min 8 characters.</span>`))
		return
	}
	target, _ := dbpkg.GetUserByHandle(h.db, targetHandle)
	if target == nil {
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">User not found.</span>`))
		return
	}
	if err := dbpkg.SetPassword(h.db, target.ID, newPassword); err != nil {
		log.Printf("[admin] reset password failed for @%s: %v", targetHandle, err)
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">DB error.</span>`))
		return
	}
	log.Printf("[admin] %s reset password for @%s", caller.Handle, targetHandle)
	fmt.Fprintf(w, `<span style="color:#22c55e;font-size:12px;font-weight:500">✓ Password reset for @%s</span>`, targetHandle)
}

// adminSetEmail sets a user's email address (for password reset capability).
func (h *Handler) adminSetEmail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "405", 405); return }
	caller := h.userFromRequest(w, r)
	if !caller.IsAdmin() { http.Error(w, "403 Forbidden", 403); return }
	_ = r.ParseForm()
	targetHandle := strings.TrimSpace(r.FormValue("target_handle"))
	email        := strings.TrimSpace(strings.ToLower(r.FormValue("email")))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	target, _ := dbpkg.GetUserByHandle(h.db, targetHandle)
	if target == nil {
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">User not found.</span>`))
		return
	}
	if err := dbpkg.SetUserEmail(h.db, target.ID, email); err != nil {
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">DB error.</span>`))
		return
	}
	log.Printf("[admin] %s set email for @%s", caller.Handle, targetHandle)
	fmt.Fprintf(w, `<span style="color:#22c55e;font-size:12px;font-weight:500">✓ Email set for @%s</span>`, targetHandle)
}

const ainSophFeesAccount  = "ain_soph_fees"
const tehanibentleyPIAL   = "93d563af-856c-4bf8-a69b-4f7da08cd18e"

type adminFeeTx struct {
	FromUserID  string `json:"from_user_id"`
	ToUserID    string `json:"to_user_id"`
	AmountUnits int64  `json:"amount_units"`
	AmountAet   string `json:"amount_aet"`
	TxMeta      string `json:"tx_meta"`
	CreatedAt   string `json:"created_at"`
}

// adminPanelTreasury renders the Treasury tab — fee vault balance + recent collections.
func (h *Handler) adminPanelTreasury(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	var balanceUnits int64
	balanceAet := "0.00 AET"
	if req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		h.cfg.AinSophURL+"/v1/balance/"+ainSophFeesAccount, nil); err == nil {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			var body struct {
				BalanceUnits int64  `json:"balance_units"`
				BalanceAet   string `json:"balance_aet"`
			}
			if json.NewDecoder(resp.Body).Decode(&body) == nil && body.BalanceAet != "" {
				balanceUnits = body.BalanceUnits
				balanceAet   = body.BalanceAet
			}
			resp.Body.Close()
		}
	}

	var txs []adminFeeTx
	if req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		h.cfg.AinSophURL+"/v1/transactions/"+ainSophFeesAccount+"?limit=50", nil); err == nil {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			var body struct {
				Entries []adminFeeTx `json:"entries"`
			}
			if json.NewDecoder(resp.Body).Decode(&body) == nil {
				txs = body.Entries
			}
			resp.Body.Close()
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "admin_panel_treasury", map[string]interface{}{
		"FeeBalance":      balanceAet,
		"FeeBalanceUnits": balanceUnits,
		"FeeTxs":          txs,
	})
}

// adminSweepFees transfers the entire ain_soph_fees balance to @tehanibentley's PIAL.
func (h *Handler) adminSweepFees(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", http.StatusMethodNotAllowed)
		return
	}
	caller := h.userFromRequest(w, r)
	if !caller.IsFounder() {
		http.Error(w, "403 Forbidden — founder only", http.StatusForbidden)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		h.cfg.AinSophURL+"/v1/balance/"+ainSophFeesAccount, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Ain Soph unavailable.</span>`))
		return
	}
	var balResp struct {
		BalanceUnits int64  `json:"balance_units"`
		BalanceAet   string `json:"balance_aet"`
	}
	json.NewDecoder(resp.Body).Decode(&balResp)
	resp.Body.Close()

	if balResp.BalanceUnits <= 0 {
		w.Write([]byte(`<span style="font-size:12px;color:var(--text-muted)">Fee vault is empty — nothing to sweep.</span>`))
		return
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"from_user_id":    ainSophFeesAccount,
		"to_user_id":      tehanibentleyPIAL,
		"amount_cents":    balResp.BalanceUnits,
		"idempotency_key": fmt.Sprintf("sweep-%d", time.Now().Unix()),
		"tx_meta":         "admin_sweep",
	})
	xferReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		h.cfg.AinSophURL+"/v1/transfer", bytes.NewReader(payload))
	if err != nil {
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Request error.</span>`))
		return
	}
	xferReq.Header.Set("Content-Type", "application/json")
	xferResp, err := http.DefaultClient.Do(xferReq)
	if err != nil || xferResp.StatusCode >= 400 {
		log.Printf("[admin] fee sweep failed: %v", err)
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Sweep failed — check logs.</span>`))
		if xferResp != nil { xferResp.Body.Close() }
		return
	}
	xferResp.Body.Close()
	log.Printf("[admin] %s swept %d units (%s) to tehanibentley PIAL", caller.Handle, balResp.BalanceUnits, balResp.BalanceAet)
	fmt.Fprintf(w, `<span style="color:#22c55e;font-size:12px;font-weight:500">✓ Swept %s to @tehanibentley — vault now empty.</span>`, balResp.BalanceAet)
}

// adminScanSweep manually rescues posts stuck in either pending_scan (>15 min) or
// human_review (>2 hours). The pending_scan path re-triggers the content scanner
// or falls back to clean. The human_review path auto-approves to clean — a human
// clearly hasn't reviewed them in that window.
func (h *Handler) adminScanSweep(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", http.StatusMethodNotAllowed)
		return
	}
	caller := h.userFromRequest(w, r)
	if !caller.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}

	// Count stale pending_scan posts before sweep so we can report accurately.
	var stalePendingScan int
	if h.db != nil {
		h.db.QueryRow(
			`SELECT COUNT(*) FROM works WHERE scan_state = 'pending' AND deleted_at IS NULL AND created_at < NOW() - INTERVAL '15 minutes'`,
		).Scan(&stalePendingScan)
	}

	// Auto-release human_review posts older than 2 hours.
	var releasedIDs []string
	if h.db != nil {
		var err error
		releasedIDs, err = dbpkg.ReleaseStaleHumanReviewPosts(h.db, 2*time.Hour)
		if err != nil {
			log.Printf("[admin] %s scan sweep — human_review release error: %v", caller.Handle, err)
		}
	}

	log.Printf("[admin] %s triggered rescue sweep: %d pending_scan stale, %d human_review released",
		caller.Handle, stalePendingScan, len(releasedIDs))

	// Run the existing pending_scan sweep logic.
	cron.SweepPendingScanOnce(h.db, h.cfg, h.httpClient)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	switch {
	case stalePendingScan == 0 && len(releasedIDs) == 0:
		w.Write([]byte(`<span style="color:var(--text-muted);font-size:12px">✓ No stuck posts found — feeds are clear.</span>`))
	case stalePendingScan > 0 && len(releasedIDs) == 0:
		fmt.Fprintf(w, `<span style="color:#22c55e;font-size:12px;font-weight:500">✓ Re-triggered scan for %d pending_scan post(s). No stale human_review posts.</span>`, stalePendingScan)
	case stalePendingScan == 0 && len(releasedIDs) > 0:
		fmt.Fprintf(w, `<span style="color:#22c55e;font-size:12px;font-weight:500">✓ Released %d human_review post(s) to clean. No pending_scan backlog.</span>`, len(releasedIDs))
	default:
		fmt.Fprintf(w, `<span style="color:#22c55e;font-size:12px;font-weight:500">✓ Re-triggered scan for %d pending_scan post(s) and released %d human_review post(s) to clean.</span>`, stalePendingScan, len(releasedIDs))
	}
}

// adminHumanReviewCount returns a small inline HTML fragment showing the current
// number of posts in human_review state, polled every 30s by the devtools panel.
func (h *Handler) adminHumanReviewCount(w http.ResponseWriter, r *http.Request) {
	caller := h.userFromRequest(w, r)
	if !caller.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	var count int
	if h.db != nil {
		h.db.QueryRow(`SELECT COUNT(*) FROM works WHERE scan_state = 'human_review' AND deleted_at IS NULL`).Scan(&count)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if count == 0 {
		fmt.Fprintf(w, `<span style="font-size:11px;color:var(--text-muted)" hx-get="/api/admin/human-review-count" hx-trigger="every 30s" hx-swap="outerHTML">human_review queue: 0</span>`)
	} else {
		fmt.Fprintf(w, `<span style="font-size:11px;color:#f59e0b;font-weight:600" hx-get="/api/admin/human-review-count" hx-trigger="every 30s" hx-swap="outerHTML">human_review queue: %d stuck</span>`, count)
	}
}

// adminBulkApproveReview approves ALL posts currently in human_review — used when
// the admin wants to clear the queue in one click from the moderation panel.
func (h *Handler) adminBulkApproveReview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", http.StatusMethodNotAllowed)
		return
	}
	caller := h.userFromRequest(w, r)
	if !caller.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}

	// Release all human_review posts immediately (0 age threshold = all of them).
	released, err := dbpkg.ReleaseStaleHumanReviewPosts(h.db, 0)
	if err != nil {
		log.Printf("[admin] %s bulk-approve-review error: %v", caller.Handle, err)
		http.Error(w, "db_error", http.StatusInternalServerError)
		return
	}
	log.Printf("[admin] %s bulk-approved %d human_review posts", caller.Handle, len(released))

	// Return a fresh moderation panel so the queue shows as empty immediately.
	pendingReports, _ := dbpkg.GetPendingReports(h.db, 50)
	reviewQueue, _ := dbpkg.GetHumanReviewPosts(h.db, 50)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "admin_panel_moderation", map[string]interface{}{
		"PendingReports": pendingReports,
		"ReviewQueue":    reviewQueue,
	})
}

// adminUserAction applies a moderation action to a user account.
// POST /api/admin/user-action  form fields: target_handle, action, reason
//
// action values:
//   warn      — sets enforcement_state = 'warning' in trust_scores; sends violation notification
//   suspend   — sets enforcement_state = 'suspended', cooldown_until = NOW()+7d; revokes posting cap
//   ban       — sets users.role = 'banned', enforcement_state = 'terminated'; revokes all caps
func (h *Handler) adminUserAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", http.StatusMethodNotAllowed)
		return
	}
	caller := h.userFromRequest(w, r)
	if !caller.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	_ = r.ParseForm()
	targetHandle := strings.TrimSpace(r.FormValue("target_handle"))
	action       := r.FormValue("action")
	reason       := strings.TrimSpace(r.FormValue("reason"))
	if reason == "" {
		reason = "admin: " + action
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if targetHandle == "" || action == "" {
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Missing handle or action.</span>`))
		return
	}

	target, _ := dbpkg.GetUserByHandle(h.db, targetHandle)
	if target == nil || target.PIALID == "" {
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">User not found.</span>`))
		return
	}

	// Protect against self-action and founder account.
	if target.Handle == caller.Handle {
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Cannot action your own account.</span>`))
		return
	}
	if target.Role == "founder" {
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Cannot action the founder account.</span>`))
		return
	}

	switch action {
	case "warn":
		// Set enforcement_state = warning; send violation notification.
		_, nsfwTier, _ := dbpkg.GetTrustScore(h.db, target.PIALID)
		if err := dbpkg.SetTrustScore(h.db, target.PIALID, 40, nsfwTier, "warning"); err != nil {
			log.Printf("[admin] user-action warn error for @%s: %v", targetHandle, err)
			w.Write([]byte(`<span style="color:#ef4444;font-size:12px">DB error — check logs.</span>`))
			return
		}
		h.notifyUser(target.ID, "violation", caller.ID, "", "moderation")
		dbpkg.LogEnforcementAction(h.db, target.PIALID, "warning", reason, caller.Handle, nil)
		dbpkg.LogPIALEvent(h.db, target.PIALID, target.ID, "account_warned", map[string]interface{}{
			"reason": reason, "by": caller.Handle,
		}, "moderation")
		log.Printf("[admin] %s warned @%s: %s", caller.Handle, targetHandle, reason)
		fmt.Fprintf(w, `<span style="color:#f59e0b;font-size:12px;font-weight:500">⚠ @%s warned — violation notification sent.</span>`, targetHandle)

	case "suspend":
		// Set enforcement_state = suspended, cooldown_until = 7 days, revoke posting cap.
		_, nsfwTier, _ := dbpkg.GetTrustScore(h.db, target.PIALID)
		if err := dbpkg.SetTrustScore(h.db, target.PIALID, 20, nsfwTier, "suspended"); err != nil {
			log.Printf("[admin] user-action suspend error for @%s: %v", targetHandle, err)
			w.Write([]byte(`<span style="color:#ef4444;font-size:12px">DB error — check logs.</span>`))
			return
		}
		h.db.Exec(`
			UPDATE trust_scores SET cooldown_until = NOW() + INTERVAL '7 days', updated_at = NOW()
			WHERE pial_id = $1`, target.PIALID)
		_ = dbpkg.SetCapability(h.db, target.PIALID, "posting", "revoked", reason, caller.Handle, nil)
		h.notifyUser(target.ID, "violation", caller.ID, "", "moderation")
		dbpkg.LogEnforcementAction(h.db, target.PIALID, "suspension_7d", reason, caller.Handle, nil)
		dbpkg.LogPIALEvent(h.db, target.PIALID, target.ID, "account_suspended", map[string]interface{}{
			"duration_days": 7, "reason": reason, "by": caller.Handle,
		}, "moderation")
		log.Printf("[admin] %s suspended @%s (7d): %s", caller.Handle, targetHandle, reason)
		fmt.Fprintf(w, `<span style="color:#ef4444;font-size:12px;font-weight:500">⛔ @%s suspended for 7 days — posting revoked.</span>`, targetHandle)

	case "ban":
		// Set role = banned, enforcement_state = terminated, revoke all caps.
		if h.db != nil {
			h.db.Exec(`UPDATE users SET role = 'banned' WHERE handle = $1`, targetHandle)
		}
		_, nsfwTier, _ := dbpkg.GetTrustScore(h.db, target.PIALID)
		_ = dbpkg.SetTrustScore(h.db, target.PIALID, 0, nsfwTier, "terminated")
		for _, cap := range []string{"posting", "messaging", "tipping", "subscribing", "live_streaming"} {
			_ = dbpkg.SetCapability(h.db, target.PIALID, cap, "revoked", reason, caller.Handle, nil)
		}
		dbpkg.LogEnforcementAction(h.db, target.PIALID, "ban", reason, caller.Handle, nil)
		dbpkg.LogPIALEvent(h.db, target.PIALID, target.ID, "account_banned", map[string]interface{}{
			"reason": reason, "by": caller.Handle,
		}, "moderation")
		log.Printf("[admin] %s banned @%s: %s", caller.Handle, targetHandle, reason)
		fmt.Fprintf(w, `<span style="color:#ef4444;font-size:12px;font-weight:500">✕ @%s banned — all capabilities revoked.</span>`, targetHandle)

	default:
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Unknown action. Valid: warn, suspend, ban.</span>`))
	}
}

// adminResyncFollowCounts recomputes every user's follower/following count
// directly from the follows table, fixing drift caused by the duplicate-follow
// counter bug (FollowUser was incrementing even on ON CONFLICT DO NOTHING).
func (h *Handler) adminResyncFollowCounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", http.StatusMethodNotAllowed)
		return
	}
	caller := h.userFromRequest(w, r)
	if !caller.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}

	res, err := h.db.Exec(`
		UPDATE user_profiles p
		SET
		  follower_count  = (SELECT COUNT(*) FROM follows f WHERE f.following_id = p.user_id),
		  following_count = (SELECT COUNT(*) FROM follows f WHERE f.follower_id  = p.user_id)
	`)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err != nil {
		log.Printf("[admin] resync-follow-counts error: %v", err)
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">DB error — check logs.</span>`))
		return
	}
	n, _ := res.RowsAffected()
	log.Printf("[admin] %s resynced follow counts for %d profiles", caller.Handle, n)
	fmt.Fprintf(w, `<span style="color:#22c55e;font-size:12px;font-weight:500">✓ Resynced follower/following counts for %d profiles from live data.</span>`, n)
}

// adminMediaFingerprintSweep fingerprints all video posts that have never been indexed,
// and fixes lineage attribution for posts sharing the same video URL.
// Processes in created_at ASC order so the oldest post is always the canonical original.
// Returns immediately — the sweep runs in a background goroutine so the browser isn't
// blocked while content-scan processes each video (which can take several seconds each).
// POST /api/admin/media-fingerprint-sweep
func (h *Handler) adminMediaFingerprintSweep(w http.ResponseWriter, r *http.Request) {
	caller := h.userFromRequest(w, r)
	if !caller.IsAdmin() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if h.db == nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}

	type postRow struct {
		ID             string
		Handle         string
		PIAL           string
		VideoMasterURL string
	}

	// All video posts not yet in media_fingerprints, oldest-first.
	dbRows, err := h.db.Query(`
		SELECT p.id::text, u.handle, u.pial_id::text, p.video_master_url
		FROM works p
		JOIN users u ON u.id = p.author_id
		LEFT JOIN media_fingerprints mf ON mf.post_id = p.id::text
		WHERE p.video_master_url IS NOT NULL AND p.video_master_url != ''
		  AND mf.id IS NULL
		  AND p.deleted_at IS NULL
		ORDER BY p.created_at ASC
	`)
	if err != nil {
		http.Error(w, "db_error", http.StatusInternalServerError)
		return
	}
	var posts []postRow
	for dbRows.Next() {
		var p postRow
		dbRows.Scan(&p.ID, &p.Handle, &p.PIAL, &p.VideoMasterURL)
		posts = append(posts, p)
	}
	dbRows.Close()

	if len(posts) == 0 {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<span style="color:#22c55e;font-size:12px;font-weight:500">✓ All videos already indexed — nothing to sweep.</span>`))
		return
	}

	// Run the sweep in a background goroutine so the browser gets an immediate response.
	// Each content-scan /scan/video call can take 5-30s; 32 videos = minutes of wall time.
	callerHandle := caller.Handle
	go func() {
		type origInfo struct{ PostID, PIAL, Handle string }
		canonicals := map[string]*origInfo{}
		var fingerprintedCount, lineageFixed, errCount int

		for _, p := range posts {
			if orig, ok := canonicals[p.VideoMasterURL]; ok {
				if setErr := dbpkg.SetPostLineage(h.db, p.ID, orig.PIAL, orig.Handle); setErr != nil {
					log.Printf("[sweep] SetPostLineage error post=%s: %v", p.ID, setErr)
					errCount++
				} else {
					lineageFixed++
					log.Printf("[sweep] lineage fixed: post=%s → @%s", p.ID, orig.Handle)
				}
				continue
			}

			// Video fingerprint sweep via content-scan HTTP is removed.
			// Abraxas Shield Phase 3.1 will handle media hash events via Kafka.
			canonicals[p.VideoMasterURL] = &origInfo{PostID: p.ID, PIAL: p.PIAL, Handle: p.Handle}
		}
		log.Printf("[admin] %s media-fingerprint-sweep complete: fingerprinted=%d lineage_fixed=%d errors=%d",
			callerHandle, fingerprintedCount, lineageFixed, errCount)
	}()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w,
		`<span style="color:#22c55e;font-size:12px;font-weight:500">✓ Sweep started — processing %d videos in background. Watch server logs for completion.</span>`,
		len(posts))
}

// adminNSFWToggle sets is_adult_creator on a user's profile (admin only).
// POST /api/admin/user/nsfw-toggle  form: target_handle, is_adult_creator ("1"|"0")
func (h *Handler) adminNSFWToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", http.StatusMethodNotAllowed)
		return
	}
	caller := h.userFromRequest(w, r)
	if !caller.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	_ = r.ParseForm()
	targetHandle  := strings.TrimSpace(r.FormValue("target_handle"))
	isAdultCreator := r.FormValue("is_adult_creator") == "1"

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if targetHandle == "" {
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Missing target_handle.</span>`))
		return
	}

	target, _ := dbpkg.GetUserByHandle(h.db, targetHandle)
	if target == nil {
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">User not found.</span>`))
		return
	}

	if h.db != nil {
		_, err := h.db.Exec(
			`UPDATE user_profiles SET is_adult_creator = $1 WHERE user_id = $2`,
			isAdultCreator, target.ID,
		)
		if err != nil {
			log.Printf("[admin] nsfw-toggle db error for @%s: %v", targetHandle, err)
			w.Write([]byte(`<span style="color:#ef4444;font-size:12px">DB error — check logs.</span>`))
			return
		}
		// Granting adult-creator implies they should see adult content directly —
		// promote content_setting to adult_enabled (unless a minor). Disabling does
		// not downgrade: the account may still want adult content shown.
		if isAdultCreator {
			if role, _ := dbpkg.GetUserRole(h.db, target.ID); role == nil || !role.IsMinor {
				_ = dbpkg.SetContentSetting(h.db, target.ID, "adult_enabled")
			}
		}
	}

	dbpkg.LogPIALEvent(h.db, target.PIALID, target.ID, "adult_creator_toggled", map[string]interface{}{
		"is_adult_creator": isAdultCreator, "by": caller.Handle,
	}, "admin")
	log.Printf("[admin] %s set is_adult_creator=%v on @%s", caller.Handle, isAdultCreator, targetHandle)

	label := "18+ / Adult Content Account: enabled"
	colour := "#22c55e"
	if !isAdultCreator {
		label = "18+ / Adult Content Account: disabled"
		colour = "#6b7280"
	}
	fmt.Fprintf(w, `<span style="color:%s;font-size:12px;font-weight:500">✓ %s for @%s</span>`, colour, label, targetHandle)
}

// adminSetContentSetting sets a user's porn-axis content_setting (admin only).
// POST /api/admin/user/content-setting  form: target_handle, content_setting
// Lets an admin move an account between safe_mode | default | adult_enabled —
// the control that was previously only reachable at onboarding. adult_enabled is
// refused for minors / non-age-verified accounts (same rule as the user path).
func (h *Handler) adminSetContentSetting(w http.ResponseWriter, r *http.Request) {
	caller := h.userFromRequest(w, r)
	if !caller.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	_ = r.ParseForm()
	targetHandle := strings.TrimSpace(r.FormValue("target_handle"))
	setting      := r.FormValue("content_setting")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if targetHandle == "" || setting == "" {
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Missing fields.</span>`))
		return
	}
	target, _ := dbpkg.GetUserByHandle(h.db, targetHandle)
	if target == nil {
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">User not found.</span>`))
		return
	}
	if setting == "adult_enabled" {
		role, _ := dbpkg.GetUserRole(h.db, target.ID)
		ageOK := target.IsVerified || (role != nil && role.IsAgeVerified)
		isMinor := role != nil && role.IsMinor
		if isMinor || !ageOK {
			w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Cannot enable adult content — account is not age-verified.</span>`))
			return
		}
	}
	if err := dbpkg.SetContentSetting(h.db, target.ID, setting); err != nil {
		log.Printf("[admin] content-setting db error for @%s: %v", targetHandle, err)
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">DB error — check logs.</span>`))
		return
	}
	dbpkg.LogPIALEvent(h.db, target.PIALID, target.ID, "content_setting_changed", map[string]interface{}{
		"content_setting": setting, "by": caller.Handle,
	}, "admin")
	log.Printf("[admin] %s set content_setting=%s on @%s", caller.Handle, setting, targetHandle)
	fmt.Fprintf(w, `<span style="color:#22c55e;font-size:12px;font-weight:500">✓ content sensitivity → %s for @%s</span>`, setting, targetHandle)
}

// ── Abraxas Shield config handlers ──────────────────────────────────────────

// abraxasConfigGet renders the Abraxas Shield config panel fragment.
// GET /admin/partials/abraxas
func (h *Handler) abraxasConfigGet(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	cfg, err := dbpkg.GetAbraxasConfig(h.db)
	if err != nil {
		http.Error(w, "db_error", http.StatusInternalServerError)
		return
	}
	accuracy, _ := dbpkg.GetSignalAccuracy(h.db)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "admin_panel_abraxas", map[string]interface{}{
		"Cfg":            cfg,
		"SignalAccuracy": accuracy,
	})
}

// abraxasConfigPost saves updated Abraxas Shield thresholds and keyword lists.
// POST /api/admin/abraxas-config
func (h *Handler) abraxasConfigPost(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	parseFloat := func(key string, fallback float64) float64 {
		v, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue(key)), 64)
		if err != nil || v < 0 || v > 1 {
			return fallback
		}
		return v
	}
	parseKeywords := func(key string) []string {
		raw := strings.TrimSpace(r.FormValue(key))
		if raw == "" {
			return []string{}
		}
		var out []string
		for _, kw := range strings.Split(raw, ",") {
			kw = strings.ToLower(strings.TrimSpace(kw))
			if kw != "" {
				out = append(out, kw)
			}
		}
		return out
	}
	existing, err := dbpkg.GetAbraxasConfig(h.db)
	if err != nil {
		http.Error(w, "db_error", http.StatusInternalServerError)
		return
	}
	cfg := &dbpkg.AbraxasConfig{
		NudityBlockThreshold:     parseFloat("nudity_block", existing.NudityBlockThreshold),
		NudityReviewThreshold:    parseFloat("nudity_review", existing.NudityReviewThreshold),
		ClickbaitBlockThreshold:  parseFloat("clickbait_block", existing.ClickbaitBlockThreshold),
		ClickbaitReviewThreshold: parseFloat("clickbait_review", existing.ClickbaitReviewThreshold),
		GoreBlockThreshold:       parseFloat("gore_block", existing.GoreBlockThreshold),
		GoreReviewThreshold:      parseFloat("gore_review", existing.GoreReviewThreshold),
		CustomHateKeywords:       parseKeywords("hate_keywords"),
		CustomViolenceKeywords:   parseKeywords("violence_keywords"),
		CustomSpamKeywords:       parseKeywords("spam_keywords"),
		EnableBotDetection:       r.FormValue("enable_bot_detection") == "1",
		EnableSpamDetection:      r.FormValue("enable_spam_detection") == "1",
		AutoApproveUnknown:       r.FormValue("auto_approve_unknown") == "1",
	}
	if err := dbpkg.SaveAbraxasConfig(h.db, cfg, user.Handle); err != nil {
		log.Printf("[admin] abraxas config save error: %v", err)
		http.Error(w, "db_error", http.StatusInternalServerError)
		return
	}
	log.Printf("[admin] %s updated Abraxas Shield config", user.Handle)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<div style="padding:10px 16px;font-size:13px;font-weight:500;color:#22c55e;border-bottom:1px solid var(--border-soft)">✓ Abraxas Shield config saved</div>`)
}

// adminPanelCelebrations renders the Celebrations scheduler panel fragment.
// GET /admin/partials/celebrations
func (h *Handler) adminPanelCelebrations(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	cels, err := dbpkg.ListCelebrations(h.db)
	if err != nil {
		http.Error(w, "db_error", http.StatusInternalServerError)
		return
	}
	activeTheme := ""
	if active, _ := dbpkg.GetActiveCelebration(h.db); active != nil {
		activeTheme = active.Theme
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "admin_panel_celebrations", map[string]interface{}{
		"Celebrations": cels,
		"ActiveTheme":  activeTheme,
	})
}

// celebrationsConfigPost saves one celebration's dates + on/off.
// POST /api/admin/celebrations
func (h *Handler) celebrationsConfigPost(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	theme := strings.TrimSpace(r.FormValue("theme"))
	start, errS := time.Parse("2006-01-02", r.FormValue("start"))
	end, errE := time.Parse("2006-01-02", r.FormValue("end"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if theme == "" || errS != nil || errE != nil {
		fmt.Fprint(w, `<span style="color:var(--accent-warn)">Invalid date</span>`)
		return
	}
	if end.Before(start) {
		fmt.Fprint(w, `<span style="color:var(--accent-warn)">End is before start</span>`)
		return
	}
	enabled := r.FormValue("enabled") == "1"
	if err := dbpkg.UpdateCelebration(h.db, theme, start, end, enabled, user.Handle); err != nil {
		log.Printf("[admin] celebration save error: %v", err)
		fmt.Fprint(w, `<span style="color:var(--accent-warn)">Save failed</span>`)
		return
	}
	log.Printf("[admin] %s updated celebration %q enabled=%v %s→%s", user.Handle, theme, enabled, r.FormValue("start"), r.FormValue("end"))
	fmt.Fprint(w, `<span style="color:#22c55e">✓ Saved</span>`)
}

// adminGrantFoundingCreator grants the Founding Creator badge + 500 XP to a user.
func (h *Handler) adminGrantFoundingCreator(w http.ResponseWriter, r *http.Request) {
	admin := h.userFromRequest(w, r)
	if !admin.IsAdmin() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var body struct {
		Handle string `json:"handle"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Handle) == "" {
		http.Error(w, "handle required", http.StatusBadRequest)
		return
	}
	handle := strings.TrimSpace(strings.ToLower(body.Handle))
	target, err := dbpkg.GetUserByHandle(h.db, handle)
	if err != nil || target == nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	if err := dbpkg.GrantFoundingCreator(h.db, target.ID); err != nil {
		log.Printf("[admin] grant-founding-creator: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	go dbpkg.AwardXP(h.db, target.ID, "founding_creator_badge", 500)
	log.Printf("[admin] %s granted Founding Creator badge to @%s", admin.Handle, handle)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"handle":%q,"message":"Founding Creator badge granted + 500 XP awarded"}`, handle)
}

// adminClearOrphanHashes deletes video_raw_hashes rows older than 2 hours with no linked post.
// These are left behind when an upload gets stuck, times out, or the browser closes before posting.
func (h *Handler) adminClearOrphanHashes(w http.ResponseWriter, r *http.Request) {
	caller := h.userFromRequest(w, r)
	if !caller.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	result, err := h.db.Exec(`
		DELETE FROM video_raw_hashes
		WHERE (post_id IS NULL OR post_id = '')
		  AND created_at < NOW() - INTERVAL '2 hours'
	`)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err != nil {
		log.Printf("[admin] clear-orphan-hashes error: %v", err)
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">DB error — check logs.</span>`))
		return
	}
	n, _ := result.RowsAffected()
	log.Printf("[admin] %s cleared %d orphan video hashes", caller.Handle, n)
	fmt.Fprintf(w, `<span style="color:#22c55e;font-size:12px;font-weight:500">✓ %d orphan hash(es) cleared</span>`, n)
}

// adminDeletePost hard-deletes a work by ID without checking author ownership.
// Also purges associated video/image hashes so the media can be re-uploaded.
func (h *Handler) adminDeletePost(w http.ResponseWriter, r *http.Request) {
	caller := h.userFromRequest(w, r)
	if !caller.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	_ = r.ParseForm()
	postID := strings.TrimSpace(r.FormValue("post_id"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if postID == "" {
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">post_id required</span>`))
		return
	}
	if err := dbpkg.AdminDeleteWork(h.db, postID); err != nil {
		log.Printf("[admin] delete-post %s error: %v", postID, err)
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Not found or DB error.</span>`))
		return
	}
	log.Printf("[admin] %s hard-deleted post %s", caller.Handle, postID)
	fmt.Fprintf(w, `<span style="color:#22c55e;font-size:12px;font-weight:500">✓ Post %s deleted</span>`, postID)
}

// adminPurgeUser hard-deletes a user account and all associated data.
// Founder-only. Irreversible.
func (h *Handler) adminPurgeUser(w http.ResponseWriter, r *http.Request) {
	caller := h.userFromRequest(w, r)
	if caller == nil || caller.Role != "founder" {
		http.Error(w, "403 Forbidden — founder only", http.StatusForbidden)
		return
	}
	_ = r.ParseForm()
	targetHandle := strings.TrimSpace(r.FormValue("target_handle"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if targetHandle == "" {
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">target_handle required</span>`))
		return
	}
	target, _ := dbpkg.GetUserByHandle(h.db, targetHandle)
	if target == nil {
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">User not found.</span>`))
		return
	}
	if target.Handle == caller.Handle {
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Cannot purge your own account.</span>`))
		return
	}
	if err := dbpkg.PurgeUser(h.db, target.ID, target.PIALID); err != nil {
		log.Printf("[admin] purge-user @%s error: %v", targetHandle, err)
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">DB error — check logs.</span>`))
		return
	}
	log.Printf("[admin] PURGE: %s hard-deleted account @%s (user_id=%s)", caller.Handle, targetHandle, target.ID)
	fmt.Fprintf(w, `<span style="color:#22c55e;font-size:12px;font-weight:500">✓ @%s and all associated data permanently purged.</span>`, targetHandle)
}
