package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/f33d3r/feed-engine/internal/abraxas"
	"github.com/f33d3r/feed-engine/internal/cron"
	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/identity"
	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/realm"
)

func (h *Handler) adminPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}

	userCount, postCount, pialCount, activeDevices := h.adminHeadlineCounts()

	pendingReportCount := h.openReportCount(r.Context())
	siteAnalytics, err := dbpkg.GetSiteAnalytics(h.db)
	if err != nil {
		analyticsUnavailable(w, "admin overview", err)
		return
	}

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

	h.render(w, r, "admin.html", map[string]interface{}{
		"User":                 user,
		"Themes":               ThemesWithActive(user.ThemeID),
		"ActiveSection":        section,
		"UserCount":            userCount,
		"PostCount":            postCount,
		"PIALCount":            pialCount,
		"ActiveDevices":        activeDevices,
		"PendingReportCount":   pendingReportCount,
		"PendingDMCACount":     dmcaCount,
		"PendingCSAMCount":     h.openCSAMReportCount(),
		"PendingOrgCount":      orgCount,
		"PendingSecurityCount": securityCount,
		"SiteAnalytics":        siteAnalytics,
	})
}

// adminHeadlineCounts are the overview's four headline numbers. Sessions are
// elohim-veni's and are not counted here; the devices this brain saw accounts
// sign in from in the last day stand in as the activity figure. A count that
// cannot be read is logged and shown as zero.
func (h *Handler) adminHeadlineCounts() (users, works, pials, devices int) {
	if h.db == nil {
		return
	}
	for _, q := range []struct {
		name string
		sql  string
		dst  *int
	}{
		{"users", `SELECT COUNT(*) FROM users`, &users},
		{"works", `SELECT COUNT(*) FROM works WHERE deleted_at IS NULL`, &works},
		{"identities", `SELECT COUNT(*) FROM identity_facts WHERE status <> 'purged'`, &pials},
		{"devices", `SELECT COUNT(*) FROM user_devices WHERE last_seen > NOW() - INTERVAL '24 hours'`, &devices},
	} {
		if err := h.db.QueryRow(q.sql).Scan(q.dst); err != nil {
			log.Printf("[admin] counting %s: %v", q.name, err)
		}
	}
	return
}

// adminPanelOverview renders the Overview tab panel for HTMX swap.
func (h *Handler) adminPanelOverview(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}

	userCount, postCount, pialCount, activeDevices := h.adminHeadlineCounts()

	pendingReportCount := h.openReportCount(r.Context())
	siteAnalytics, err := dbpkg.GetSiteAnalytics(h.db)
	if err != nil {
		analyticsUnavailable(w, "admin overview", err)
		return
	}

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
		"ActiveDevices":        activeDevices,
		"PendingReportCount":   pendingReportCount,
		"PendingDMCACount":     dmcaCount,
		"PendingCSAMCount":     h.openCSAMReportCount(),
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

	h.renderModerationPanel(w, r)
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

// NOTE: POST /api/admin/migrate-works (adminMigrateWorks) was removed. It moved
// posts rows into works; once that was done it became a stub that answered
// {"ok":true} to a request that did nothing. The posts table it migrated from no
// longer exists, so the endpoint has nothing left to be a stub for.

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
	capability := r.FormValue("capability")
	state := r.FormValue("state")
	reason := strings.TrimSpace(r.FormValue("reason"))

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

	switch state {
	case model.CapStateGranted, model.CapStateRestricted, model.CapStateRevoked:
	default:
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Unknown capability state.</span>`))
		return
	}
	// Capabilities are elohim-veni's. The admin sees whether it accepted.
	if err := h.identity.Client().PutCapability(r.Context(), target.PIALID, capability, identity.CapabilityWrite{
		State: state, Reason: reason, By: "admin:" + caller.Handle,
	}); err != nil {
		log.Printf("[admin] set-cap for @%s: %v", targetHandle, err)
		fmt.Fprintf(w, `<span style="color:#ef4444;font-size:12px">%s</span>`, template.HTMLEscapeString(adminIdentityRefusal(err)))
		return
	}

	dbpkg.LogPIALEvent(h.db, target.PIALID, target.ID, "capability_changed", map[string]interface{}{
		"capability": capability, "state": state, "reason": reason, "by": caller.Handle,
	}, "admin")

	// Evict the target's cached sessions so the new capability applies on their next request
	// instead of lagging up to the 30s TTL.
	h.evictCachedAccount(target.ID, "")

	log.Printf("[admin] %s set capability %s=%s on @%s (%s)", caller.Handle, capability, state, targetHandle, reason)
	fmt.Fprintf(w, `<span style="color:#22c55e;font-size:12px;font-weight:500">✓ %s → %s for @%s</span>`, capability, state, targetHandle)
}

// adminIdentityRefusal is the sentence an admin sees when elohim-veni did not
// accept an identity write.
func adminIdentityRefusal(err error) string {
	if identity.IsUnavailable(err) {
		return "The identity authority is unreachable — nothing was changed. Try again."
	}
	if e := identity.Refusal(err); e != nil {
		if e.Message != "" {
			return "Refused by the identity authority: " + e.Message
		}
		return "Refused by the identity authority (" + e.Code + ")."
	}
	return "Identity write failed — check logs."
}

// adminVerificationChange is the facts write the admin Verified checkbox calls
// for, if any: verify an identity not yet at the identity tier, or withdraw a
// verification from one that is. Withdrawing drops the tier to basic, not none —
// a self-reported date of birth is still on file; only the documented-identity
// claim goes.
//
// It is an override of identity only. It never satisfies 18 U.S.C. § 2257: an
// 18+ work is published only when Verity holds a passing creator verification
// with an ID copy (adultPublishGate), which no admin write creates. The reason
// is required and recorded in elohim-veni's identity ledger with the change.
func adminVerificationChange(database *sql.DB, pialID string, verify bool, by, reason string) (identity.FactsUpdate, bool) {
	current := dbpkg.LoadPIALState(database, pialID)
	isVerified := current != nil && model.KYCTierIsIdentity(current.KYCTier)
	switch {
	case verify && !isVerified:
		tier, yes := model.KYCTierFull, true
		return identity.FactsUpdate{KYCTier: &tier, AgeVerified: &yes, DOBLocked: &yes, Source: by, Reason: reason}, true
	case !verify && isVerified:
		tier, no := model.KYCTierBasic, false
		return identity.FactsUpdate{KYCTier: &tier, AgeVerified: &no, Source: by, Reason: reason}, true
	default:
		return identity.FactsUpdate{}, false
	}
}

// adminSetRole updates a user's platform role, identity badge, and permission flags.
// Form fields:
//
//	platform_role  — "user" | "admin" to assign; "founder" only as the founder's own
//	                 current role, re-posted by the badge and permission forms
//	badge_type     — "" | "business" | "government"
//	is_verified    — "1" if checked
//	is_creator     — "1" if checked
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
	badgeType := r.FormValue("badge_type")
	isVerified := r.FormValue("is_verified") == "1"
	isCreator := r.FormValue("is_creator") == "1"
	// The reason for a manual verification override; required when the
	// Verified box changes what is recorded.
	verificationReason := strings.TrimSpace(r.FormValue("verification_reason"))
	if len(verificationReason) > 500 {
		verificationReason = verificationReason[:500]
	}

	// platform_role names the three roles; creator is a perm, not a role. Anything
	// else is refused rather than read as "user", which would silently demote an
	// admin on a malformed form.
	switch platformRole {
	case model.RoleUser, model.RoleAdmin, model.RoleFounder:
	default:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Unknown platform role.</span>`))
		return
	}

	// badge_type: identity classification badge, stored in official_type column.
	allowedBadges := map[string]bool{"": true, "business": true, "government": true}
	if !allowedBadges[badgeType] {
		badgeType = ""
	}

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
		// A verification override names its reason, and the form is refused
		// before anything is written when it does not.
		verificationUpdate, verificationChanged := adminVerificationChange(h.db, target.PIALID, isVerified, "admin:"+caller.Handle, verificationReason)
		if verificationChanged && verificationReason == "" {
			w.Write([]byte(`<span class="admin-action-error">Changing Verified needs a reason. Nothing was changed.</span>`))
			return
		}
		// Founder is never assigned here and never taken away: it belongs to the
		// first account created on the database (db.CreateAccount). The badge and
		// permission forms re-post the account's current role, so the founder's own
		// "founder" passes through with no role write; every other combination
		// involving it is refused.
		switch {
		case target.IsFounder() && platformRole != model.RoleFounder:
			w.Write([]byte(`<span style="color:#ef4444;font-size:12px">The founder role is permanent.</span>`))
			return
		case !target.IsFounder() && platformRole == model.RoleFounder:
			w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Founder cannot be assigned — it belongs to the first account created.</span>`))
			return
		case !target.IsFounder() && target.Role != platformRole:
			// Roles are elohim-veni's, and so is the founder rule: it refuses to
			// touch the founder role in either direction, which also covers an
			// account whose identity holds it.
			if err := h.identity.Client().PutRole(r.Context(), target.PIALID, platformRole, "admin:"+caller.Handle); err != nil {
				log.Printf("[admin] role %s for @%s: %v", platformRole, targetHandle, err)
				if errors.Is(err, identity.ErrFounderPermanent) {
					w.Write([]byte(`<span style="color:#ef4444;font-size:12px">This account belongs to the founder's identity; its role is permanent.</span>`))
					return
				}
				fmt.Fprintf(w, `<span style="color:#ef4444;font-size:12px">%s</span>`, template.HTMLEscapeString(adminIdentityRefusal(err)))
				return
			}
		}
		// The admin "Verified" checkbox is the manual KYC override: a trusted human
		// stands in for eKYC. It is an identity fact, written to elohim-veni with
		// admin provenance, and only when it changes what is recorded — the badge
		// and permission forms re-post it unchanged on every save.
		if verificationChanged {
			if err := h.identity.Client().PutFacts(r.Context(), target.PIALID, verificationUpdate); err != nil {
				log.Printf("[admin] verification=%v for @%s: %v", isVerified, targetHandle, err)
				fmt.Fprintf(w, `<span style="color:#ef4444;font-size:12px">%s</span>`, template.HTMLEscapeString(adminIdentityRefusal(err)))
				return
			}
		}
		// is_creator and official_type are separate axes (perm/badge) this brain
		// owns on user_profiles. The role and verification projections follow from
		// identity.events, which also restates the realm a role change moves.
		if _, err := h.db.Exec(`
			UPDATE user_profiles
			SET is_creator = $1, official_type = $2
			WHERE user_id = $3`,
			isCreator, badgeType, target.ID); err != nil {
			log.Printf("[admin] user_profiles badge/creator write failed for @%s: %v", targetHandle, err)
			w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Badge write failed — check logs.</span>`))
			return
		}
		// Evict the target's cached sessions so role/verification apply on their next request
		// instead of lagging up to the 30s TTL.
		h.evictCachedAccount(target.ID, "")
		log.Printf("[admin] %s set platform_role=%s badge=%q verified=%v creator=%v on @%s",
			caller.Handle, platformRole, badgeType, isVerified, isCreator, targetHandle)
	}

	fmt.Fprintf(w, `<span style="color:#22c55e;font-size:12px;font-weight:500">✓ Updated @%s</span>`, targetHandle)
}

// realmStandingLabel renders, on the server, the one sentence the admin surface
// shows about an account's standing: what it wears, where that came from, and
// what it would fall back to if the floor were removed. The facet interpolates
// the result — it never compares a realm to a number or works out for itself
// whether standing was earned.
func realmStandingLabel(xp int64, grant int, role string) string {
	effective := realm.Effective(xp, grant, role)
	earned := realm.ComputeRealm(xp)
	worn := fmt.Sprintf("R%d %s", effective, realm.RealmName(effective))

	if !realm.IsGranted(xp, grant, role) {
		return fmt.Sprintf("%s — earned on %d XP", worn, xp)
	}
	source := fmt.Sprintf("a granted R%d floor", realm.Clamp(grant))
	if role == model.RoleFounder && realm.FounderFloor > realm.Clamp(grant) {
		source = fmt.Sprintf("the permanent R%d floor the founder role carries", realm.FounderFloor)
	}
	return fmt.Sprintf("%s — held by %s; earned R%d %s on %d XP",
		worn, source, earned, realm.RealmName(earned), xp)
}

// adminSetRealmGrant places an account on a granted realm floor, or clears one.
//
// Form fields:
//
//	target_handle — the account being placed
//	realm_grant   — "0" to clear, else "1".."5" (realm.Min..realm.Max)
//	reason        — required; it is what the ledger entry is for
//
// FOUNDER ONLY, deliberately stricter than the role control next to it.
// Role assignment is gated on IsAdmin() with founder restricted to one handle;
// a realm grant mints the platform's scarcest public signal — the ring every
// surface draws from the account's handle — so it is held at IsFounder(), the
// same gate as the treasury sweep and the hard purge. The card that posts here
// is likewise rendered for the founder alone, so an admin is never shown a
// control the server will refuse.
//
// The grant is a FLOOR (see package realm): it cannot be erased by a later XP
// award, it cannot cap what the account goes on to earn, and it cannot demote.
// Clearing it returns the account to exactly what its XP has earned.
func (h *Handler) adminSetRealmGrant(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", http.StatusMethodNotAllowed)
		return
	}
	caller := h.userFromRequest(w, r)
	if !caller.IsFounder() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	_ = r.ParseForm()
	targetHandle := strings.TrimSpace(r.FormValue("target_handle"))
	rawGrant := strings.TrimSpace(r.FormValue("realm_grant"))
	reason := strings.TrimSpace(r.FormValue("reason"))

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if targetHandle == "" || rawGrant == "" {
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Missing fields.</span>`))
		return
	}
	// A grant is a privilege change on a public surface. It is never recorded
	// without a stated reason, because an unexplained one is exactly the entry
	// nobody can reconstruct six months later.
	if reason == "" {
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">A reason is required — it is what the audit entry says.</span>`))
		return
	}
	level, err := strconv.Atoi(rawGrant)
	if err != nil {
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Grant must be a realm level.</span>`))
		return
	}

	target, _ := dbpkg.GetUserByHandle(h.db, targetHandle)
	if target == nil || target.PIALID == "" {
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">User not found.</span>`))
		return
	}

	// realm.SetGrant is the one writer of realm_grant and one of only two
	// writers of users.realm. It restates the standing in the same transaction
	// and republishes the account to the realm index, so the ring is correct on
	// the very next render — no restart, no reconcile wait.
	effective, err := realm.SetGrant(h.db, target.ID, level)
	if err != nil {
		log.Printf("[admin] realm grant R%d for @%s failed: %v", level, targetHandle, err)
		fmt.Fprintf(w, `<span style="color:#ef4444;font-size:12px">%s</span>`, template.HTMLEscapeString(err.Error()))
		return
	}

	// Audit trail: the append-only PIAL ledger, the same shape the capability
	// surface uses. Who granted it, to whom, what tier, when (created_at) and
	// why — a realm grant is never untraceable.
	event := "realm_granted"
	if level == realm.GrantNone {
		event = "realm_grant_cleared"
	}
	dbpkg.LogPIALEvent(h.db, target.PIALID, target.ID, event, map[string]interface{}{
		"grant":     level,
		"effective": effective,
		"earned":    realm.ComputeRealm(target.XP),
		"xp":        target.XP,
		"reason":    reason,
		"by":        caller.Handle,
	}, "admin")

	// Evict the target's cached session so the standing on their own next
	// request is the new one rather than lagging the 30s TTL.
	h.evictCachedAccount(target.ID, "")

	log.Printf("[admin] %s set realm_grant=%d on @%s → effective R%d (%s)",
		caller.Handle, level, targetHandle, effective, reason)

	if level == realm.GrantNone {
		fmt.Fprintf(w, `<span style="color:#22c55e;font-size:12px;font-weight:500">✓ Grant cleared for @%s — now R%d, earned</span>`,
			template.HTMLEscapeString(targetHandle), effective)
		return
	}
	fmt.Fprintf(w, `<span style="color:#22c55e;font-size:12px;font-weight:500">✓ @%s granted R%d — now wearing R%d (%s)</span>`,
		template.HTMLEscapeString(targetHandle), level, effective, realm.RealmName(effective))
}

// reportReasons are the reasons a person may give. "stolen" is an ownership
// claim: the reporter says the content is theirs.
var reportReasons = map[string]bool{
	"spam": true, "harassment": true, "hate": true, "nsfw": true, "violence": true,
	"illegal": true, "misinformation": true, "stolen": true, "other": true,
}

// reportContent — POST /api/report, the web report sheet.
func (h *Handler) reportContent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	user := h.userFromRequest(w, r)

	contentID := strings.TrimSpace(r.FormValue("content_id"))
	contentType := r.FormValue("content_type")
	reason := r.FormValue("reason")
	detail := truncate(r.FormValue("detail"), 500)

	if contentID == "" || !reportReasons[reason] {
		http.Error(w, "missing fields", http.StatusBadRequest)
		return
	}
	if contentType == "" {
		contentType = "post"
	}

	if err := h.fileContentReport(user, contentID, contentType, reason, detail); err != nil {
		http.Error(w, "could not file report", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if reason == "stolen" {
		w.Write([]byte(`<span style="font-size:12px;color:var(--accent)">Claim sent — an admin will review it.</span>`))
		return
	}
	w.Write([]byte(`<span style="font-size:12px;color:var(--accent)">Reported — thank you.</span>`))
}

// reportSubject names a reported item the way Abraxas names subjects: a work
// by its id, a person by their PIAL (server to server only — Abraxas keys
// people by identity root), a Frequency by its host. A Frequency and its
// participants are not subjects of their own, so the Frequency they happened
// in is carried into the report's detail.
func (h *Handler) reportSubject(ctx context.Context, contentID, contentType string) (abraxas.Subject, string, error) {
	pialOf := func(accountID string) (string, error) {
		if h.db == nil {
			return "", errors.New("report: no database")
		}
		u, err := dbpkg.GetUserByID(h.db, accountID)
		if err != nil {
			return "", fmt.Errorf("report: account %s: %w", accountID, err)
		}
		if u.PIALID == "" {
			return "", fmt.Errorf("report: account %s has no identity", accountID)
		}
		return u.PIALID, nil
	}
	switch contentType {
	case "post", "work":
		return abraxas.Subject{Kind: "work", ID: contentID}, "", nil
	case "user", "profile":
		pial, err := pialOf(contentID)
		return abraxas.Subject{Kind: "account", ID: pial}, "", err
	case "frequency":
		ans, err := h.auralis.Get(ctx, contentID, "")
		if err != nil {
			return abraxas.Subject{}, "", fmt.Errorf("report: frequency %s: %w", contentID, err)
		}
		host := ans.Frequency.Frequency.HostPialID
		if host == "" {
			return abraxas.Subject{}, "", fmt.Errorf("report: frequency %s has no host", contentID)
		}
		return abraxas.Subject{Kind: "account", ID: host}, "Frequency " + contentID + " (host): ", nil
	case "frequency_participant":
		freq, account, ok := strings.Cut(contentID, ":")
		if !ok {
			return abraxas.Subject{}, "", fmt.Errorf("report: frequency participant %q is not <frequency>:<account>", contentID)
		}
		pial, err := pialOf(account)
		return abraxas.Subject{Kind: "account", ID: pial}, "In Frequency " + freq + ": ", err
	}
	return abraxas.Subject{}, "", fmt.Errorf("report: content type %q names nothing Abraxas judges", contentType)
}

// fileContentReport is the one way a report is filed, whichever surface took
// it: the web report sheet (reportContent), the `report` event the native
// clients send, and a Frequency's report. The report goes to Abraxas, the
// integrity authority, which keeps it, decides whether the content is held,
// and puts it on the moderators' queue. Reason "stolen" is filed as an
// ownership claim, which Abraxas sends straight to admin review with the
// reporter's own matching uploads attached when it finds them. A report
// Abraxas did not take is an error the caller answers: the reporter is never
// thanked for a report nobody holds.
func (h *Handler) fileContentReport(user *model.User, contentID, contentType, reason, detail string) error {
	if user == nil || user.PIALID == "" {
		return errors.New("report: the reporter has no identity")
	}
	kind := abraxas.ReportKindContent
	if reason == "stolen" {
		kind = abraxas.ReportKindOwnershipClaim
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	subject, where, err := h.reportSubject(ctx, contentID, contentType)
	if err != nil {
		log.Printf("[report] @%s reporting %s %s: %v", user.Handle, contentType, contentID, err)
		return err
	}
	filed, err := h.abraxas.CreateReport(ctx, abraxas.NewReport{
		ReporterPIAL: user.PIALID,
		Subject:      subject,
		Kind:         kind,
		Reason:       reason,
		Detail:       strings.TrimSpace(where + detail),
	})
	if err != nil {
		log.Printf("[report] @%s reporting %s %s (%s) NOT filed: %v", user.Handle, contentType, contentID, reason, err)
		return err
	}
	if h.db != nil {
		dbpkg.LogPIALEvent(h.db, user.PIALID, user.ID, "content_reported", map[string]interface{}{
			"content_id": contentID, "reason": reason, "report_id": filed.ReportID,
		}, "abraxas")
		// An ownership claim opens straight at admin review: the admins are
		// told now, through the same notices every review item uses.
		if kind == abraxas.ReportKindOwnershipClaim && filed.Created {
			if err := dbpkg.NotifyReviewers(h.db, "report-"+filed.ReportID); err != nil {
				log.Printf("[report] claim %s filed; telling the admins failed: %v", filed.ReportID, err)
			}
		}
	}
	return nil
}

// adminInjectAET credits platform AET to a user's wallet. AET is
// non-redeemable platform credit, and this — the founder's grant, made through
// Themis to Ain Soph's service-only mint — is the only way it comes into being.
//
// The form's amount is a decimal AET amount; grant_key is rendered with the
// form, so a double submit is one grant.
func (h *Handler) adminInjectAET(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", http.StatusMethodNotAllowed)
		return
	}
	caller := h.userFromRequest(w, r)
	if !caller.IsFounder() {
		http.Error(w, "403 Forbidden — founder only", http.StatusForbidden)
		return
	}
	_ = r.ParseForm()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fail := func(msg string) {
		fmt.Fprintf(w, `<span style="color:#ef4444;font-size:12px">%s</span>`, template.HTMLEscapeString(msg))
	}
	targetHandle := strings.TrimPrefix(strings.TrimSpace(r.FormValue("target_handle")), "@")
	reason := strings.TrimSpace(r.FormValue("reason"))
	if reason == "" {
		reason = "admin_inject"
	}
	grantKey := strings.TrimSpace(r.FormValue("grant_key"))
	if _, err := uuid.Parse(grantKey); err != nil {
		grantKey = uuid.NewString()
	}
	unit, err := h.aetUnit(r.Context())
	if err != nil {
		log.Printf("[admin] AET inject: %v", err)
		fail("The wallet is unavailable — nothing was credited.")
		return
	}
	units, err := parseAETAmount(r.FormValue("amount"), unit)
	if err != nil || units <= 0 {
		fail("Invalid amount.")
		return
	}
	target, _ := dbpkg.GetUserByHandle(h.db, targetHandle)
	if target == nil || target.PIALID == "" {
		fail("User not found.")
		return
	}
	status, body, err := h.themisPeerCall(r.Context(), "/v1/admin/aet/credit", caller, map[string]interface{}{
		"target_pial":     target.PIALID,
		"amount_units":    units,
		"reason":          reason,
		"idempotency_key": grantKey,
	})
	if err != nil {
		log.Printf("[admin] AET inject for @%s: themis unreachable: %v", targetHandle, err)
		fail("Themis unreachable — nothing was credited.")
		return
	}
	if status != http.StatusOK {
		// A value path must never fail quietly: the status and Themis's own
		// reason go to the operator.
		msg := themisRefusal(body, "refused")
		log.Printf("[admin] AET inject for @%s refused: themis %d: %s", targetHandle, status, msg)
		fail(fmt.Sprintf("The credit was refused (%d): %s", status, msg))
		return
	}
	log.Printf("[admin] %s credited %d units to @%s (%s)", caller.Handle, units, targetHandle, reason)
	fmt.Fprintf(w, `<span style="color:#22c55e;font-size:12px;font-weight:500">✓ %s AET credited to @%s (net of the deposit fee)</span>`,
		formatAETUnits(units, unit), template.HTMLEscapeString(targetHandle))
}

// adminResetPassword sets a new password for a user directly (admin only).
func (h *Handler) adminResetPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", 405)
		return
	}
	caller := h.userFromRequest(w, r)
	if !caller.IsAdmin() {
		http.Error(w, "403 Forbidden", 403)
		return
	}
	_ = r.ParseForm()
	targetHandle := strings.TrimSpace(r.FormValue("target_handle"))
	newPassword := r.FormValue("new_password")
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
	// elohim-veni sets the password and ends every session on the account.
	if err := h.identity.Client().ResetPassword(r.Context(), target.ID, newPassword, "admin:"+caller.Handle); err != nil {
		log.Printf("[admin] reset password for @%s: %v", targetHandle, err)
		fmt.Fprintf(w, `<span style="color:#ef4444;font-size:12px">%s</span>`, template.HTMLEscapeString(adminIdentityRefusal(err)))
		return
	}
	h.evictCachedAccount(target.ID, "")
	log.Printf("[admin] %s reset password for @%s", caller.Handle, targetHandle)
	fmt.Fprintf(w, `<span style="color:#22c55e;font-size:12px;font-weight:500">✓ Password reset for @%s</span>`, targetHandle)
}

// adminSetEmail sets a user's email address (for password reset capability).
func (h *Handler) adminSetEmail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", 405)
		return
	}
	caller := h.userFromRequest(w, r)
	if !caller.IsAdmin() {
		http.Error(w, "403 Forbidden", 403)
		return
	}
	_ = r.ParseForm()
	targetHandle := strings.TrimSpace(r.FormValue("target_handle"))
	email := strings.TrimSpace(strings.ToLower(r.FormValue("email")))
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

const ainSophFeesAccount = "ain_soph_fees"

// adminFeeTx is one fee float movement as the treasury shows it. No PIAL is
// rendered: the other party is not named.
type adminFeeTx struct {
	Direction string
	Kind      string
	Amount    string
	When      string
}

// adminPanelTreasury renders the Treasury tab — fee vault balance + recent movements.
func (h *Handler) adminPanelTreasury(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	data := map[string]interface{}{"Notice": ""}
	var bal struct {
		BalanceUnits int64  `json:"balance_units"`
		BalanceAet   string `json:"balance_aet"`
	}
	if err := h.ainSophGet(ctx, "/v1/balance/"+ainSophFeesAccount, &bal); err != nil {
		log.Printf("[admin] treasury balance: %v", err)
		data["Notice"] = "The fee vault could not be read."
	}
	var hist struct {
		Entries []struct {
			TxType    string    `json:"tx_type"`
			Direction string    `json:"direction"`
			NetAet    string    `json:"net_aet"`
			CreatedAt time.Time `json:"created_at"`
		} `json:"entries"`
	}
	if err := h.ainSophGet(ctx, "/v1/transactions/"+ainSophFeesAccount+"?limit=50", &hist); err != nil {
		log.Printf("[admin] treasury history: %v", err)
		data["Notice"] = "The fee vault could not be read."
	}
	txs := make([]adminFeeTx, 0, len(hist.Entries))
	for _, e := range hist.Entries {
		txs = append(txs, adminFeeTx{Direction: e.Direction, Kind: e.TxType, Amount: e.NetAet, When: TimeAgo(e.CreatedAt)})
	}

	// The sweep pays the founder, so the panel names who that is. A database
	// with no founder account renders the panel without a sweep destination.
	founder, ferr := dbpkg.GetFounder(h.db)
	if ferr != nil {
		log.Printf("[admin] treasury: founder lookup: %v", ferr)
		http.Error(w, "Could not load the treasury — check logs.", http.StatusInternalServerError)
		return
	}
	founderHandle := ""
	if founder != nil {
		founderHandle = founder.Handle
	}

	data["FeeBalance"] = bal.BalanceAet
	data["FeeBalanceUnits"] = bal.BalanceUnits
	data["FeeTxs"] = txs
	data["FounderHandle"] = founderHandle
	data["SweepKey"] = uuid.NewString()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "admin_panel_treasury", data)
}

// adminSweepFees moves the entire fee float to the founder's identity, through
// Themis. sweep_key is rendered with the panel, so a double submit is one sweep.
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

	// The destination is looked up by role, never named. The caller is the
	// founder, but the founder's identity is resolved through the one function
	// that answers "who is the founder" so the payee cannot drift from it.
	founder, ferr := dbpkg.GetFounder(h.db)
	if ferr != nil || founder == nil || founder.PIALID == "" {
		log.Printf("[admin] fee sweep: founder account unresolved (account found: %v): %v", founder != nil, ferr)
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Founder account could not be resolved — nothing swept.</span>`))
		return
	}
	sweepKey := strings.TrimSpace(r.FormValue("sweep_key"))
	if _, err := uuid.Parse(sweepKey); err != nil {
		sweepKey = uuid.NewString()
	}
	status, body, err := h.themisPeerCall(r.Context(), "/v1/admin/fees/sweep", caller, map[string]string{
		"to_pial":         founder.PIALID,
		"idempotency_key": sweepKey,
	})
	if err != nil {
		log.Printf("[admin] fee sweep: themis unreachable: %v", err)
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Themis unreachable — nothing swept.</span>`))
		return
	}
	if status != http.StatusOK {
		msg := themisRefusal(body, "refused")
		log.Printf("[admin] fee sweep refused: themis %d: %s", status, msg)
		fmt.Fprintf(w, `<span style="color:#ef4444;font-size:12px">Sweep refused (%d): %s</span>`, status, template.HTMLEscapeString(msg))
		return
	}
	var out struct {
		AmountUnits   int64  `json:"amount_units"`
		AmountDisplay string `json:"amount_display"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		log.Printf("[admin] fee sweep answer unreadable: %v", err)
	}
	if out.AmountUnits <= 0 {
		w.Write([]byte(`<span style="font-size:12px;color:var(--text-muted)">Fee vault is empty — nothing to sweep.</span>`))
		return
	}
	log.Printf("[admin] %s swept %d units to founder @%s", caller.Handle, out.AmountUnits, founder.Handle)
	fmt.Fprintf(w, `<span style="color:#22c55e;font-size:12px;font-weight:500">✓ Swept %s to @%s — vault now empty.</span>`,
		template.HTMLEscapeString(out.AmountDisplay), template.HTMLEscapeString(founder.Handle))
}

// adminScanSweep reports works still waiting on a verdict from Abraxas. It
// writes nothing: a verdict is Abraxas's to give and a moderator's to decide
// in the review queue, never a timer's. A stale backlog means Abraxas is not
// answering, and the operator alert (cron.SweepPendingScanOnce) says so.
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

	log.Printf("[admin] %s checked the verdict backlog: %d work(s) pending over 15 minutes", caller.Handle, stalePendingScan)
	cron.SweepPendingScanOnce(h.db, h.cfg, h.httpClient)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if stalePendingScan == 0 {
		w.Write([]byte(`<span style="color:var(--text-muted);font-size:12px">✓ No work is waiting on a verdict.</span>`))
		return
	}
	fmt.Fprintf(w, `<span style="color:#f59e0b;font-size:12px;font-weight:500">%d work(s) have waited over 15 minutes for a verdict from Abraxas — they stay visible to their authors only until it answers.</span>`, stalePendingScan)
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

// adminUserAction applies a moderation action to a user account.
// POST /api/admin/user-action  form fields: target_handle, action, reason
//
// action values:
//
//	warn      — sets enforcement_state = 'warning' in trust_scores; sends violation notification
//	suspend   — sets enforcement_state = 'suspended', cooldown_until = NOW()+7d; queues a
//	            7-day posting cooldown for elohim-veni
//	ban       — sets enforcement_state = 'terminated'; queues the ban for elohim-veni, which
//	            revokes every capability and session and reports role 'banned' back
//	            through identity.events
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
	action := r.FormValue("action")
	reason := strings.TrimSpace(r.FormValue("reason"))
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
		err := dbpkg.WithTx(h.db, func(tx *sql.Tx) error {
			if err := dbpkg.SetTrustScore(tx, target.PIALID, 40, nsfwTier, "warning"); err != nil {
				return err
			}
			if _, err := dbpkg.WriteNotice(tx, dbpkg.Notice{Recipient: target.ID, Kind: "violation", TargetType: "moderation"}); err != nil {
				return err
			}
			dbpkg.LogEnforcementAction(tx, target.PIALID, "warning", reason, caller.Handle, nil)
			dbpkg.LogPIALEvent(tx, target.PIALID, target.ID, "account_warned", map[string]interface{}{
				"reason": reason, "by": caller.Handle,
			}, "moderation")
			return nil
		})
		if err != nil {
			log.Printf("[admin] user-action warn error for @%s: %v", targetHandle, err)
			w.Write([]byte(`<span style="color:#ef4444;font-size:12px">DB error — check logs.</span>`))
			return
		}
		log.Printf("[admin] %s warned @%s: %s", caller.Handle, targetHandle, reason)
		fmt.Fprintf(w, `<span style="color:#f59e0b;font-size:12px;font-weight:500">⚠ @%s warned — violation notification sent.</span>`, targetHandle)

	case "suspend":
		// Set enforcement_state = suspended, cooldown_until = 7 days, and stop posting for
		// that long. The cooldown IS the suspension, so the whole action commits or none of it.
		_, nsfwTier, _ := dbpkg.GetTrustScore(h.db, target.PIALID)
		err := dbpkg.WithTx(h.db, func(tx *sql.Tx) error {
			if err := dbpkg.SetTrustScore(tx, target.PIALID, 20, nsfwTier, "suspended"); err != nil {
				return err
			}
			if _, err := tx.Exec(`
				UPDATE trust_scores SET cooldown_until = NOW() + INTERVAL '7 days', updated_at = NOW()
				WHERE pial_id = $1`, target.PIALID); err != nil {
				return err
			}
			// The posting cooldown is elohim-veni's to apply; it is queued on this
			// commit so the suspension and its record stand or fall together.
			until := time.Now().Add(7 * 24 * time.Hour)
			if err := dbpkg.EnqueueIdentityCommand(tx, identity.CommandCapability, target.PIALID, identity.CapabilityCommand{
				Capability: model.CapPosting,
				Write:      identity.CapabilityWrite{State: model.CapStateCooldown, Reason: reason, By: "admin:" + caller.Handle, ExpiresAt: &until},
			}); err != nil {
				return err
			}
			if _, err := dbpkg.WriteNotice(tx, dbpkg.Notice{Recipient: target.ID, Kind: "violation", TargetType: "moderation"}); err != nil {
				return err
			}
			dbpkg.LogEnforcementAction(tx, target.PIALID, "suspension_7d", reason, caller.Handle, nil)
			dbpkg.LogPIALEvent(tx, target.PIALID, target.ID, "account_suspended", map[string]interface{}{
				"duration_days": 7, "reason": reason, "by": caller.Handle,
			}, "moderation")
			return nil
		})
		if err != nil {
			log.Printf("[admin] user-action suspend failed for @%s: %v", targetHandle, err)
			w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Suspension failed — check logs.</span>`))
			return
		}
		h.identityRelay.Kick()
		h.evictCachedAccount(target.ID, "")
		log.Printf("[admin] %s suspended @%s (7d): %s", caller.Handle, targetHandle, reason)
		fmt.Fprintf(w, `<span style="color:#ef4444;font-size:12px;font-weight:500">⛔ @%s suspended for 7 days — posting revoked.</span>`, targetHandle)

	case "ban":
		err := dbpkg.WithTx(h.db, func(tx *sql.Tx) error {
			return dbpkg.BanAccount(tx, target.PIALID, reason, caller.Handle, nil)
		})
		if err != nil {
			log.Printf("[admin] ban failed for @%s: %v", targetHandle, err)
			w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Ban failed — check logs.</span>`))
			return
		}
		accounts, aerr := dbpkg.AccountsOfPIAL(h.db, target.PIALID)
		if aerr != nil {
			log.Printf("[admin] ban of @%s: listing accounts to sign out: %v", targetHandle, aerr)
		}
		h.applyQueuedIdentityAction(accounts)
		log.Printf("[admin] %s banned @%s: %s", caller.Handle, targetHandle, reason)
		fmt.Fprintf(w, `<span style="color:#ef4444;font-size:12px;font-weight:500">✕ @%s banned — all capabilities revoked.</span>`, targetHandle)

	default:
		w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Unknown action. Valid: warn, suspend, ban.</span>`))
	}
}

// adminResyncFollowCounts recomputes every user's follower/following count
// directly from the follows table, fixing drift caused by the duplicate-follow
// counter bug (the old follow writer incremented even on ON CONFLICT DO NOTHING).
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
	targetHandle := strings.TrimSpace(r.FormValue("target_handle"))
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
				if err := dbpkg.SetContentSetting(h.db, target.ID, "adult_enabled"); err != nil {
					log.Printf("[admin] adult-creator grant for @%s: content_setting NOT promoted to adult_enabled: %v",
						targetHandle, err)
					w.Write([]byte(`<span style="color:#ef4444;font-size:12px">Flag set, but content setting was not promoted — check logs.</span>`))
					return
				}
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
	setting := r.FormValue("content_setting")

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

// adminDeletePost hard-deletes a work by ID without checking author ownership.
// AdminDeleteWork withdraws its author's own media from Caeor; ownership credit
// and the raw-byte record stay with Caeor, so the media is never freed for re-upload.
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
	// The identity erasure was queued with the purge; deliver it now.
	h.identityRelay.Kick()
	h.evictCachedAccount(target.ID, "")
	log.Printf("[admin] PURGE: %s hard-deleted account @%s (user_id=%s)", caller.Handle, targetHandle, target.ID)
	fmt.Fprintf(w, `<span style="color:#22c55e;font-size:12px;font-weight:500">✓ @%s and all associated data permanently purged.</span>`, targetHandle)
}
