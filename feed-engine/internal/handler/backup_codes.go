package handler

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/f33d3r/feed-engine/internal/identity"
)

// Backup codes and password recovery.
//
// Backup codes are the only way back into an account — F33D3R holds no email
// or phone. They are issued, stored, checked and spent by elohim-veni. This
// file renders the reveal page and carries the recovery steps; it keeps no
// code, no hash, and no recovery token of its own.

// resetAttempts tracks per-handle recovery attempt timestamps for rate
// limiting, on top of the per-IP and per-account limiters on the route: a
// handle-keyed bound in front of the authority's own.
var resetAttempts sync.Map // handle → []time.Time

const (
	resetRateWindow   = 15 * time.Minute
	resetRateMaxTries = 3
)

// checkResetRateLimit returns true if the handle has exceeded the allowed number
// of recovery attempts in the sliding window. It prunes stale entries and
// records the current attempt.
func checkResetRateLimit(handle string) bool {
	now := time.Now()
	cutoff := now.Add(-resetRateWindow)
	raw, _ := resetAttempts.Load(handle)
	var times []time.Time
	if raw != nil {
		for _, t := range raw.([]time.Time) {
			if t.After(cutoff) {
				times = append(times, t)
			}
		}
	}
	if len(times) >= resetRateMaxTries {
		return true // limit exceeded — do not record this attempt
	}
	times = append(times, now)
	resetAttempts.Store(handle, times)
	return false
}

// ── Backup codes setup (mandatory gate after login) ───────────────────────────

// backupCodesSetupPage has fresh codes issued and shows them once. Any prior
// codes are replaced; the person is on this page because they have none, or
// have just spent one.
func (h *Handler) backupCodesSetupPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if viewerAccountID(user) == "" {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	codes, err := h.identity.Client().IssueBackupCodes(r.Context(), user.ID)
	if err != nil {
		log.Printf("[backup-codes] issue for @%s: %v", user.Handle, err)
		http.Error(w, "Your backup codes could not be generated right now — reload to try again.", http.StatusServiceUnavailable)
		return
	}
	h.render(w, r, "backup_codes_setup.html", map[string]interface{}{
		"Title": "Save your backup codes · F33D3R",
		"Codes": codes,
		"User":  user,
	})
}

// ── Forgot password — step 1: handle entry ────────────────────────────────────

func (h *Handler) forgotPasswordPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "forgot_password.html", map[string]interface{}{
		"Title": "Forgot password · F33D3R",
		"Step":  "handle",
	})
}

// forgotPasswordSubmit receives a handle and returns the backup-code entry
// Fragment. It answers the same way whether or not the handle exists.
func (h *Handler) forgotPasswordSubmit(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	handle := strings.TrimSpace(strings.ToLower(r.FormValue("handle")))
	w.Header().Set("Content-Type", "text/html")
	h.renderPartial(w, "password_reset_code_form", map[string]interface{}{
		"Handle": handle,
	})
}

// ── Forgot password — step 2: verify backup code ─────────────────────────────

// forgotPasswordVerifyCode has elohim-veni spend one backup code and hands the
// person the recovery token it returns, for the new-password step. The
// authority answers an unknown handle and a wrong code identically.
func (h *Handler) forgotPasswordVerifyCode(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	handle := strings.TrimSpace(strings.ToLower(r.FormValue("handle")))
	code := strings.TrimSpace(r.FormValue("code"))

	renderError := func(msg string) {
		w.Header().Set("Content-Type", "text/html")
		h.renderPartial(w, "password_reset_code_form", map[string]interface{}{
			"Handle": handle,
			"Error":  msg,
		})
	}
	if handle == "" || code == "" {
		renderError("Please enter your handle and a backup code.")
		return
	}
	if checkResetRateLimit(handle) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusTooManyRequests)
		h.renderPartial(w, "password_reset_code_form", map[string]interface{}{
			"Handle": handle,
			"Error":  "Too many reset attempts — try again later.",
		})
		return
	}

	rec, err := h.identity.Client().BeginRecovery(r.Context(), handle, code)
	switch {
	case err == nil:
	case errors.Is(err, identity.ErrBackupCodeInvalid), errors.Is(err, identity.ErrNotFound):
		renderError("Invalid handle or backup code.")
		return
	default:
		log.Printf("[recovery] begin for @%s: %v", handle, err)
		renderError("Recovery is briefly unavailable — try again in a moment.")
		return
	}

	w.Header().Set("Content-Type", "text/html")
	h.renderPartial(w, "password_reset_new_password_form", map[string]interface{}{
		"Handle":     handle,
		"ResetToken": rec.RecoveryToken,
	})
}

// ── Forgot password — step 3: set new password ───────────────────────────────

// forgotPasswordResetSubmit completes the recovery: elohim-veni sets the new
// password, ends every session on the account — whoever else was signed in may
// be the reason for this reset — and opens one for this device. A banned
// account is refused there: knowing a backup code does not undo a ban.
func (h *Handler) forgotPasswordResetSubmit(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	resetToken := strings.TrimSpace(r.FormValue("reset_token"))
	newPassword := r.FormValue("new_password")
	confirm := r.FormValue("confirm_password")

	renderError := func(msg string) {
		w.Header().Set("Content-Type", "text/html")
		h.renderPartial(w, "password_reset_new_password_form", map[string]interface{}{
			"ResetToken": resetToken,
			"Error":      msg,
		})
	}
	if len(newPassword) < 8 {
		renderError("Password must be at least 8 characters.")
		return
	}
	if newPassword != confirm {
		renderError("Passwords don't match.")
		return
	}

	deviceName := r.UserAgent()
	if len(deviceName) > 100 {
		deviceName = deviceName[:100]
	}
	res, err := h.identity.Client().CompleteRecovery(r.Context(), resetToken, newPassword, deviceName, requestIP(r))
	switch {
	case err == nil:
	case errors.Is(err, identity.ErrRecoveryExpired), errors.Is(err, identity.ErrNotFound):
		renderError("This reset link has expired. Please start over.")
		return
	case errors.Is(err, identity.ErrWeakPassword):
		renderError("Password must be at least 8 characters.")
		return
	case errors.Is(err, identity.ErrSuspended):
		hxAwareRedirect(w, r, "/login")
		return
	default:
		log.Printf("[recovery] complete: %v", err)
		renderError("Could not save password — try again in a moment.")
		return
	}

	h.evictCachedAccount(res.AccountID, "")
	h.openedSession(w, res.SessionToken)
	h.afterSignIn(r, res.AccountID, res.PIALID)
	// The spent code leaves one fewer; a fresh set is issued straight away.
	hxAwareRedirect(w, r, "/backup-codes/setup")
}
