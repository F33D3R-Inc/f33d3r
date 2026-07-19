package handler

import (
	"net/http"
	"strings"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

// renderOrgSection writes the settings_section_org Facet HTML after a mutation.
// This lets the HTMX POST response directly update #settings-panel with fresh data.
func (h *Handler) renderOrgSection(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	data := h.settingsSectionData(user, "org", r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "settings_section_org", data)
}

// orgEvent handles all org-related mutations via POST /events.
func (h *Handler) orgEvent(w http.ResponseWriter, r *http.Request, eventType string) {
	user := h.userFromRequest(w, r)
	if user == nil || user.ID == "" {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	if h.db == nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	switch eventType {

	case "org_apply":
		orgHandle := strings.TrimPrefix(strings.TrimSpace(r.FormValue("org_handle")), "@")
		if orgHandle == "" {
			htmxError(w, r, "org_handle required", http.StatusBadRequest)
			return
		}
		org, err := dbpkg.GetOrgByHandle(h.db, orgHandle)
		if err != nil || org == nil {
			htmxError(w, r, "Organization not found", http.StatusNotFound)
			return
		}
		if org.ID == user.ID {
			htmxError(w, r, "You can't apply to your own organization", http.StatusBadRequest)
			return
		}
		if dbpkg.GetUserApprovedOrgCount(h.db, user.ID) >= 3 {
			htmxError(w, r, "You've reached the 3-organization limit", http.StatusBadRequest)
			return
		}
		if dbpkg.OrgMembershipExists(h.db, user.ID, org.ID) {
			htmxError(w, r, "Application already exists", http.StatusConflict)
			return
		}
		if err := dbpkg.CreateOrgMembership(h.db, user.ID, org.ID, "employee"); err != nil {
			htmxError(w, r, "Could not submit application", http.StatusInternalServerError)
			return
		}
		go h.notifyUser(org.ID, "org_apply", user.ID, "", "org")
		h.renderOrgSection(w, r)

	case "org_invite":
		if user.OfficialType == "" {
			htmxError(w, r, "Only verified organizations can invite members", http.StatusForbidden)
			return
		}
		targetHandle := strings.TrimPrefix(strings.TrimSpace(r.FormValue("target_handle")), "@")
		if targetHandle == "" {
			htmxError(w, r, "target_handle required", http.StatusBadRequest)
			return
		}
		target, err := dbpkg.GetUserByHandle(h.db, targetHandle)
		if err != nil || target == nil {
			htmxError(w, r, "User not found", http.StatusNotFound)
			return
		}
		if target.ID == user.ID {
			htmxError(w, r, "Cannot invite yourself", http.StatusBadRequest)
			return
		}
		if dbpkg.GetUserApprovedOrgCount(h.db, target.ID) >= 3 {
			htmxError(w, r, "That user has reached the 3-organization limit", http.StatusBadRequest)
			return
		}
		if dbpkg.OrgMembershipExists(h.db, target.ID, user.ID) {
			htmxError(w, r, "Invite already sent", http.StatusConflict)
			return
		}
		if err := dbpkg.CreateOrgMembership(h.db, target.ID, user.ID, "org"); err != nil {
			htmxError(w, r, "Could not send invite", http.StatusInternalServerError)
			return
		}
		go h.notifyUser(target.ID, "org_invite", user.ID, "", "org")
		h.renderOrgSection(w, r)

	case "org_approve":
		membershipID := r.FormValue("membership_id")
		if user.OfficialType == "" {
			htmxError(w, r, "Only org accounts can approve applications", http.StatusForbidden)
			return
		}
		m, err := dbpkg.GetOrgMembership(h.db, membershipID)
		if err != nil || m.Org.ID != user.ID {
			htmxError(w, r, "Not found or not authorized", http.StatusNotFound)
			return
		}
		// m.Org.ID is the org; we need the employee's user_id — re-query membership
		var employeeUserID string
		h.db.QueryRow(`SELECT user_id FROM org_memberships WHERE id=$1`, membershipID).Scan(&employeeUserID)
		if dbpkg.GetUserApprovedOrgCount(h.db, employeeUserID) >= 3 {
			htmxError(w, r, "That user has reached the 3-organization limit", http.StatusBadRequest)
			return
		}
		if err := dbpkg.SetMembershipStatus(h.db, membershipID, "approved"); err != nil {
			htmxError(w, r, "Could not approve", http.StatusInternalServerError)
			return
		}
		h.renderOrgSection(w, r)

	case "org_accept_invite":
		membershipID := r.FormValue("membership_id")
		m, err := dbpkg.GetOrgMembership(h.db, membershipID)
		if err != nil || m.Status != "pending_org" {
			htmxError(w, r, "Invite not found", http.StatusNotFound)
			return
		}
		if dbpkg.GetUserApprovedOrgCount(h.db, user.ID) >= 3 {
			htmxError(w, r, "You've reached the 3-organization limit", http.StatusBadRequest)
			return
		}
		if err := dbpkg.SetMembershipStatus(h.db, membershipID, "approved"); err != nil {
			htmxError(w, r, "Could not accept invite", http.StatusInternalServerError)
			return
		}
		h.renderOrgSection(w, r)

	case "org_reject":
		membershipID := r.FormValue("membership_id")
		m, err := dbpkg.GetOrgMembership(h.db, membershipID)
		if err != nil {
			htmxError(w, r, "Not found", http.StatusNotFound)
			return
		}
		// Allow: org owner rejecting an applicant, or employee declining an invite
		if m.Org.ID != user.ID && m.Status != "pending_org" {
			htmxError(w, r, "Not authorized", http.StatusForbidden)
			return
		}
		dbpkg.SetMembershipStatus(h.db, membershipID, "rejected")
		h.renderOrgSection(w, r)

	case "org_revoke":
		membershipID := r.FormValue("membership_id")
		m, err := dbpkg.GetOrgMembership(h.db, membershipID)
		if err != nil {
			htmxError(w, r, "Not found", http.StatusNotFound)
			return
		}
		// Allow: org owner revoking, or cancelling a sent invite
		if m.Org.ID != user.ID {
			htmxError(w, r, "Not authorized", http.StatusForbidden)
			return
		}
		dbpkg.SetMembershipStatus(h.db, membershipID, "revoked")
		h.renderOrgSection(w, r)

	case "org_leave":
		membershipID := r.FormValue("membership_id")
		m, err := dbpkg.GetOrgMembership(h.db, membershipID)
		if err != nil {
			htmxError(w, r, "Not found", http.StatusNotFound)
			return
		}
		_ = m
		dbpkg.SetMembershipStatus(h.db, membershipID, "revoked")
		h.renderOrgSection(w, r)

	case "org_set_primary":
		membershipID := r.FormValue("membership_id")
		if err := dbpkg.SetPrimaryOrg(h.db, user.ID, membershipID); err != nil {
			htmxError(w, r, "Could not set primary org", http.StatusInternalServerError)
			return
		}
		h.renderOrgSection(w, r)
	}
}

// orgPanelPage renders the org owner management panel.
// Only visible to accounts with official_type set.
func (h *Handler) orgPanelPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user.OfficialType == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	data := h.baseData(user)
	if h.db != nil {
		data["PendingApplications"] = dbpkg.GetOrgPendingApplications(h.db, user.ID)
		data["PendingInvites"]      = dbpkg.GetOrgPendingInvites(h.db, user.ID)
		data["ApprovedMembers"]     = dbpkg.GetOrgApprovedMembers(h.db, user.ID)
	}
	data["Title"] = "Organization Panel"
	h.render(w, "org_panel.html", data)
}

// facetOrgSearch returns org search result rows for #sorg-search-results target.
func (h *Handler) facetOrgSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if h.db == nil || len(q) < 2 {
		return
	}
	orgs := dbpkg.SearchOrgs(h.db, q)
	h.renderPartial(w, "org_search_results", orgs)
}

// orgVerifyApply handles POST /api/org/verify/apply — submit org verification application.
func (h *Handler) orgVerifyApply(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	orgType := r.FormValue("org_type")
	if orgType != "business" && orgType != "government" {
		http.Error(w, "invalid org_type", http.StatusBadRequest)
		return
	}
	orgName := strings.TrimSpace(r.FormValue("org_name"))
	if orgName == "" {
		http.Error(w, "org_name required", http.StatusBadRequest)
		return
	}
	if h.db != nil {
		dbpkg.CreateOrgVerificationApplication(h.db, user.ID,
			orgType, orgName,
			strings.TrimSpace(r.FormValue("org_website")),
			strings.TrimSpace(r.FormValue("description")),
			strings.TrimSpace(r.FormValue("evidence_url")),
		)
	}
	http.Redirect(w, r, "/settings/org?applied=1", http.StatusSeeOther)
}

// adminOrgVerificationsPanel returns the admin section for reviewing org verification apps,
// granting badges manually, and seeing all currently verified orgs.
func (h *Handler) adminOrgVerificationsPanel(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	data := map[string]interface{}{"User": user}
	if h.db != nil {
		data["Applications"]   = dbpkg.GetPendingOrgVerifications(h.db)
		data["VerifiedOrgs"]   = dbpkg.GetAllVerifiedOrgs(h.db)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "admin_panel_org_verifications", data)
}

// adminGrantOrgBadge manually grants a business/government badge to any account.
// POST /api/admin/org/grant  fields: handle, org_type
func (h *Handler) adminGrantOrgBadge(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	r.ParseForm()
	handle  := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(r.FormValue("handle")), "@"))
	orgType := strings.TrimSpace(r.FormValue("org_type"))
	if handle == "" || (orgType != "business" && orgType != "government") {
		http.Error(w, "handle and org_type (business|government) required", http.StatusBadRequest)
		return
	}
	if h.db != nil {
		if err := dbpkg.GrantOrgBadge(h.db, handle, orgType); err != nil {
			http.Error(w, "could not grant badge: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	// Re-render the full panel so the verified list updates.
	h.adminOrgVerificationsPanel(w, r)
}

// adminRevokeOrgBadge removes the business/government badge from an account.
// POST /api/admin/org/revoke  fields: handle
func (h *Handler) adminRevokeOrgBadge(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	r.ParseForm()
	handle := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(r.FormValue("handle")), "@"))
	if handle == "" {
		http.Error(w, "handle required", http.StatusBadRequest)
		return
	}
	if h.db != nil {
		dbpkg.RevokeOrgBadge(h.db, handle)
	}
	h.adminOrgVerificationsPanel(w, r)
}

// adminApproveOrgVerification approves an org verification application.
func (h *Handler) adminApproveOrgVerification(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	r.ParseForm()
	appID := r.FormValue("app_id")
	orgType := r.FormValue("org_type")
	if h.db != nil {
		if err := dbpkg.ApproveOrgVerification(h.db, appID, user.ID, orgType); err != nil {
			http.Error(w, "could not approve: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`<div class="org-verify-card org-verify-card--done">✓ Approved</div>`))
}

// adminRejectOrgVerification rejects an org verification application.
func (h *Handler) adminRejectOrgVerification(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if !user.IsAdmin() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	r.ParseForm()
	appID := r.FormValue("app_id")
	notes := strings.TrimSpace(r.FormValue("notes"))
	if h.db != nil {
		dbpkg.RejectOrgVerification(h.db, appID, user.ID, notes)
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`<div class="org-verify-card org-verify-card--done">✗ Rejected</div>`))
}
