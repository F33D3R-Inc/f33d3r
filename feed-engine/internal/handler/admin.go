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
	"github.com/f33d3r/feed-engine/internal/model"
)

func (h *Handler) adminPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}

	// Handle capability set action
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		targetHandle := strings.TrimSpace(r.FormValue("target_handle"))
		capability  := r.FormValue("capability")
		state       := r.FormValue("state")
		reason      := strings.TrimSpace(r.FormValue("reason"))
		if targetHandle != "" && capability != "" && state != "" {
			if target, _ := dbpkg.GetUserByHandle(h.db, targetHandle); target != nil && target.PIALID != "" {
				_ = dbpkg.SetCapability(h.db, target.PIALID, capability, state, reason, user.Handle, nil)
				dbpkg.LogPIALEvent(h.db, target.PIALID, target.ID, "capability_changed", map[string]interface{}{
					"capability": capability, "state": state, "reason": reason, "by": user.Handle,
				}, "admin")
			}
		}
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}

	var userCount, postCount, pialCount int
	if h.db != nil {
		h.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&userCount)
		h.db.QueryRow(`SELECT COUNT(*) FROM posts`).Scan(&postCount)
		h.db.QueryRow(`SELECT COUNT(*) FROM pial_roots WHERE is_tombstoned = FALSE`).Scan(&pialCount)
	}

	// Lookup target handle if provided
	lookupHandle := strings.TrimSpace(r.URL.Query().Get("lookup"))
	var lookupCaps model.PIALCapabilityMap
	var lookupUser *model.User
	if lookupHandle != "" && h.db != nil {
		if lu, _ := dbpkg.GetUserByHandle(h.db, lookupHandle); lu != nil {
			lookupUser = lu
			lookupCaps = dbpkg.GetCapabilities(h.db, lu.PIALID)
		}
	}

	var pendingReports []dbpkg.ContentReport
	if h.db != nil {
		pendingReports, _ = dbpkg.GetPendingReports(h.db, 50)
	}

	recentUsers, _ := dbpkg.GetRecentUsers(h.db, 20)

	h.render(w, "admin.html", map[string]interface{}{
		"User":           user,
		"Themes":         ThemesWithActive(user.ThemeID),
		"UserCount":      userCount,
		"PostCount":      postCount,
		"PIALCount":      pialCount,
		"LookupUser":     lookupUser,
		"LookupCaps":     lookupCaps,
		"LookupHandle":   lookupHandle,
		"AllCaps":        model.DefaultCapabilities,
		"PendingReports": pendingReports,
		"RecentUsers":    recentUsers,
	})
}

// adminSetRole sets a user's platform role and creator/verified flags.
// Only accessible to admin-role accounts.
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
	role         := r.FormValue("role")
	isVerified   := r.FormValue("is_verified") == "1"
	isCreator    := r.FormValue("is_creator") == "1"

	allowed := map[string]bool{"user": true, "creator": true, "admin": true, "founder": true}
	if !allowed[role] { role = "user" }

	if h.db != nil && targetHandle != "" {
		h.db.Exec(`UPDATE users SET role = $1 WHERE handle = $2`, role, targetHandle)
		h.db.Exec(`UPDATE user_profiles SET is_verified = $1, is_creator = $2 WHERE user_id = (SELECT id FROM users WHERE handle = $3)`,
			isVerified, isCreator, targetHandle)
		h.db.Exec(`UPDATE user_roles SET role_type = $1 WHERE user_id = (SELECT id FROM users WHERE handle = $2)`,
			role, targetHandle)
		log.Printf("[admin] %s set role=%s verified=%v creator=%v on @%s", caller.Handle, role, isVerified, isCreator, targetHandle)
	}
	http.Redirect(w, r, "/admin?lookup="+targetHandle, http.StatusSeeOther)
}

func (h *Handler) resolveReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "405", 405); return }
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	_ = r.ParseForm()
	reportID := strings.TrimSpace(r.FormValue("report_id"))
	status   := r.FormValue("status") // "resolved" or "dismissed"
	if reportID == "" || (status != "resolved" && status != "dismissed") {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if h.db != nil {
		_ = dbpkg.ResolveReport(h.db, reportID, status, user.Handle)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<span class="text-xs text-accent">%s</span>`, status)
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
		h.db.Exec(`
			INSERT INTO content_reports (reporter_id, content_id, content_type, reason, detail)
			VALUES ($1, $2, $3, $4, $5)`,
			user.ID, contentID, contentType, reason, detail)

		dbpkg.LogPIALEvent(h.db, user.PIALID, user.ID, "content_reported", map[string]interface{}{
			"content_id": contentID, "reason": reason,
		}, "elohim-veni")
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
