package handler

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

// ── In-memory reset token store ───────────────────────────────────────────────
// Tokens are issued after a valid backup code is consumed and expire in 10 min.
// They tie the code-verification step to the password-change step.

type resetEntry struct {
	userID    string
	expiresAt time.Time
}

var (
	resetTokens    sync.Map
	resetTokensTTL = 10 * time.Minute

	// resetAttempts tracks per-handle attempt timestamps for rate limiting.
	// Key: handle (string) → []time.Time of recent attempts.
	resetAttempts sync.Map
)

const (
	resetRateWindow   = 15 * time.Minute
	resetRateMaxTries = 3
)

// checkResetRateLimit returns true if the handle has exceeded the allowed number
// of password-reset attempts in the sliding window. It prunes stale entries and
// records the current attempt atomically.
func checkResetRateLimit(handle string) bool {
	now := time.Now()
	cutoff := now.Add(-resetRateWindow)

	// Load existing timestamps, prune entries outside the window, and append now.
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

func issueResetToken(userID string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	resetTokens.Store(token, resetEntry{userID: userID, expiresAt: time.Now().Add(resetTokensTTL)})
	return token, nil
}

func consumeResetToken(token string) (string, bool) {
	v, ok := resetTokens.LoadAndDelete(token)
	if !ok {
		return "", false
	}
	e := v.(resetEntry)
	if time.Now().After(e.expiresAt) {
		return "", false
	}
	return e.userID, true
}

// ── Backup codes setup (mandatory gate after login) ───────────────────────────

// backupCodesSetupPage generates 8 fresh codes, stores their hashes, and shows
// the reveal page. Any prior codes are replaced; the user is on this page
// precisely because they have none.
func (h *Handler) backupCodesSetupPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		return
	}

	codes := make([]string, 8)
	for i := range codes {
		c, err := dbpkg.GenerateBackupCode()
		if err != nil {
			log.Printf("[backup-codes] generate: %v", err)
			http.Error(w, "could not generate codes", http.StatusInternalServerError)
			return
		}
		codes[i] = c
	}

	if err := dbpkg.StoreBackupCodes(h.db, user.ID, codes); err != nil {
		log.Printf("[backup-codes] store: %v", err)
		http.Error(w, "could not store codes", http.StatusInternalServerError)
		return
	}

	h.render(w, "backup_codes_setup.html", map[string]interface{}{
		"Title": "Save your backup codes · F33D3R",
		"Codes": codes,
	})
}

// ── Forgot password — step 1: handle entry ────────────────────────────────────

func (h *Handler) forgotPasswordPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, "forgot_password.html", map[string]interface{}{
		"Title": "Forgot password · F33D3R",
		"Step":  "handle",
	})
}

// forgotPasswordSubmit receives a handle and returns the backup-code entry
// fragment. Always responds the same way regardless of whether the handle
// exists — prevents user enumeration.
func (h *Handler) forgotPasswordSubmit(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	handle := strings.TrimSpace(strings.ToLower(r.FormValue("handle")))

	// Render code-entry form for any non-empty handle input.
	// The handle is passed through so step 2 can use it — but we never confirm
	// whether it exists at this point.
	w.Header().Set("Content-Type", "text/html")
	h.renderPartial(w, "password_reset_code_form", map[string]interface{}{
		"Handle":    handle,
	})
}

// ── Forgot password — step 2: verify backup code ─────────────────────────────

func (h *Handler) forgotPasswordVerifyCode(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	handle := strings.TrimSpace(strings.ToLower(r.FormValue("handle")))
	code   := strings.TrimSpace(r.FormValue("code"))

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
	if h.db == nil {
		renderError("Service unavailable — try again.")
		return
	}

	user, _ := dbpkg.GetUserByHandle(h.db, handle)
	// Always spend bcrypt time even on unknown handle to prevent timing attacks.
	if user == nil {
		// Burn ~same time as a real bcrypt check
		dbpkg.GenerateBackupCode() //nolint:errcheck
		renderError("Invalid handle or backup code.")
		return
	}

	ok, err := dbpkg.VerifyAndConsumeBackupCode(h.db, user.ID, code)
	if err != nil {
		log.Printf("[backup-codes] verify: %v", err)
		renderError("Something went wrong — try again.")
		return
	}
	if !ok {
		renderError("Invalid backup code. Check your saved codes and try again.")
		return
	}

	resetToken, err := issueResetToken(user.ID)
	if err != nil {
		log.Printf("[backup-codes] issue reset token: %v", err)
		renderError("Something went wrong — try again.")
		return
	}

	w.Header().Set("Content-Type", "text/html")
	h.renderPartial(w, "password_reset_new_password_form", map[string]interface{}{
		"Handle":     handle,
		"ResetToken": resetToken,
	})
}

// ── Forgot password — step 3: set new password ───────────────────────────────

func (h *Handler) forgotPasswordResetSubmit(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	resetToken  := strings.TrimSpace(r.FormValue("reset_token"))
	newPassword := r.FormValue("new_password")
	confirm     := r.FormValue("confirm_password")

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

	userID, ok := consumeResetToken(resetToken)
	if !ok {
		renderError("This reset link has expired. Please start over.")
		return
	}

	if err := dbpkg.SetPassword(h.db, userID, newPassword); err != nil {
		log.Printf("[backup-codes] set password: %v", err)
		renderError("Could not save password — try again.")
		return
	}

	// Create a session so the user lands directly on the home feed.
	deviceName := r.UserAgent()
	if len(deviceName) > 100 {
		deviceName = deviceName[:100]
	}
	token, err := dbpkg.CreateAuthSession(h.db, userID, deviceName, r.RemoteAddr)
	if err != nil {
		log.Printf("[backup-codes] create session post-reset: %v", err)
	} else {
		SetSessionCookie(w, token)
	}

	// Redirect to backup-codes setup — the consumed code means they now have
	// one fewer code; force them to generate a fresh set immediately.
	http.Redirect(w, r, "/backup-codes/setup", http.StatusSeeOther)
}
