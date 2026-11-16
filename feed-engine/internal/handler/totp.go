package handler

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"

	"github.com/f33d3r/feed-engine/internal/identity"
	"github.com/f33d3r/feed-engine/internal/model"
)

// ── Second factor ─────────────────────────────────────────────────────────────
//
// TOTP secrets are minted, stored and checked by elohim-veni. The functions
// below are the rule with no transport — the cookie routes and the /api/v1
// events both call them, so the web and the phones answer from one place. A
// non-200 status carries the sentence to show.

// totpBeginSetup mints a fresh secret for an account that has not enabled 2FA
// and returns its provisioning URI.
func (h *Handler) totpBeginSetup(ctx context.Context, user *model.User) (uri, secret string, status int, msg string) {
	enr, err := h.identity.Client().TOTPEnroll(ctx, user.ID)
	switch {
	case err == nil:
		return enr.URI, enr.Secret, http.StatusOK, ""
	case errors.Is(err, identity.ErrTOTPAlreadyEnabled):
		return "", "", http.StatusConflict, "2FA is already enabled."
	default:
		log.Printf("[2fa] enroll @%s: %v", user.Handle, err)
		return "", "", http.StatusServiceUnavailable, "Could not start 2FA setup — try again."
	}
}

// totpEnableCore confirms a code from the enrolled secret and turns 2FA on.
func (h *Handler) totpEnableCore(ctx context.Context, user *model.User, code string) (int, string) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if code == "" {
		return http.StatusBadRequest, "Enter the 6-digit code."
	}
	if !isTOTPCode(code) {
		return http.StatusBadRequest, "Enter a 6-digit code."
	}
	err := h.identity.Client().TOTPConfirm(ctx, user.ID, code)
	switch {
	case err == nil:
		return http.StatusOK, ""
	case errors.Is(err, identity.ErrTOTPAlreadyEnabled):
		return http.StatusConflict, "2FA is already enabled."
	case errors.Is(err, identity.ErrTOTPNotEnrolled):
		return http.StatusBadRequest, "Set up 2FA first."
	case errors.Is(err, identity.ErrTOTPInvalid):
		return http.StatusUnauthorized, "Invalid code."
	default:
		log.Printf("[2fa] confirm @%s: %v", user.Handle, err)
		return http.StatusServiceUnavailable, "Could not enable 2FA — try again."
	}
}

// totpDisableCore checks a current code and turns 2FA off.
func (h *Handler) totpDisableCore(ctx context.Context, user *model.User, code string) (int, string) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	err := h.identity.Client().TOTPDisable(ctx, user.ID, code)
	switch {
	case err == nil:
		return http.StatusOK, ""
	case errors.Is(err, identity.ErrTOTPNotEnabled):
		return http.StatusBadRequest, "2FA is not enabled."
	case errors.Is(err, identity.ErrTOTPInvalid):
		return http.StatusUnauthorized, "Invalid code."
	default:
		log.Printf("[2fa] disable @%s: %v", user.Handle, err)
		return http.StatusServiceUnavailable, "Could not disable 2FA — try again."
	}
}

// GET /api/2fa/setup — generates a new TOTP secret and returns a provisioning URI.
func (h *Handler) totpSetup(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	uri, secret, status, msg := h.totpBeginSetup(r.Context(), user)
	if status != http.StatusOK {
		http.Error(w, msg, status)
		return
	}
	if facetRequest(r) {
		// The setup step rendered: provisioning URI, secret and the confirm
		// form, swapped into #2fa-setup-wrap by the "Set up 2FA" control.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		h.renderPartial(w, "totp_setup", map[string]interface{}{
			"URI":    uri,
			"Secret": secret,
		})
		return
	}
	apiJSON(w, http.StatusOK, TwoFactorSetupDTO{URI: uri, Secret: secret})
}

// POST /api/2fa/enable — verifies a TOTP code and activates 2FA.
func (h *Handler) totpEnable(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if status, msg := h.totpEnableCore(r.Context(), user, r.FormValue("code")); status != http.StatusOK {
		h.totpRefuse(w, r, msg, status)
		return
	}
	if h.totpRenderSecurity(w, r, user) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// totpRefuse reports a refused 2FA change. An htmx caller sees the words inside
// the security Facet's result line (#2fa-result) with the rest of the section
// untouched — the setup QR stays on screen for another attempt. Any other
// caller gets the plain status.
func (h *Handler) totpRefuse(w http.ResponseWriter, r *http.Request, msg string, code int) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Retarget", "#2fa-result")
		w.Header().Set("HX-Reswap", "innerHTML")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, template.HTMLEscapeString(msg))
		return
	}
	http.Error(w, msg, code)
}

// totpRenderSecurity answers an htmx caller with the whole security section
// rendered from the server's current truth (the 2FA block flips state, the
// sessions block reloads). Returns false when the caller is not htmx.
func (h *Handler) totpRenderSecurity(w http.ResponseWriter, r *http.Request, user *model.User) bool {
	if r.Header.Get("HX-Request") != "true" {
		return false
	}
	data := h.settingsSectionData(user, "security", r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "settings_section_security", data)
	return true
}

// POST /api/2fa/disable — disables 2FA after verifying the current code.
func (h *Handler) totpDisable(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if status, msg := h.totpDisableCore(r.Context(), user, r.FormValue("code")); status != http.StatusOK {
		h.totpRefuse(w, r, msg, status)
		return
	}
	if h.totpRenderSecurity(w, r, user) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}
