package handler

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// ── Pure-stdlib TOTP (RFC 6238 / RFC 4226) ───────────────────────────────────

func totpGenSecret() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b), nil
}

func totpCode(secret string, t time.Time) (string, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(
		strings.ToUpper(strings.TrimSpace(secret)),
	)
	if err != nil {
		return "", err
	}
	counter := uint64(math.Floor(float64(t.Unix()) / 30))
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(buf)
	h := mac.Sum(nil)
	offset := h[len(h)-1] & 0x0f
	code := (binary.BigEndian.Uint32(h[offset:offset+4]) & 0x7fffffff) % 1_000_000
	return fmt.Sprintf("%06d", code), nil
}

func totpVerify(secret, code string) bool {
	now := time.Now()
	for _, delta := range []int{-1, 0, 1} {
		t := now.Add(time.Duration(delta) * 30 * time.Second)
		if c, err := totpCode(secret, t); err == nil && c == code {
			return true
		}
	}
	return false
}

func totpProvisioningURI(secret, handle, issuer string) string {
	return fmt.Sprintf(
		"otpauth://totp/%s?secret=%s&issuer=%s&algorithm=SHA1&digits=6&period=30",
		url.PathEscape(issuer+":@"+handle),
		secret,
		url.PathEscape(issuer),
	)
}

// ── HTTP handlers ─────────────────────────────────────────────────────────────

// totpBeginSetup mints a fresh secret for an account that has not enabled 2FA
// yet and returns its provisioning URI. It is the rule, with no transport: the
// cookie route below and GET /api/v1/2fa/setup both call it, so the QR the web
// draws and the QR the phone draws are the same secret from the same check.
// A non-200 status carries the sentence to show.
func (h *Handler) totpBeginSetup(user *model.User) (uri, secret string, status int, msg string) {
	if h.db == nil {
		return "", "", http.StatusServiceUnavailable, "Database unavailable."
	}
	if _, enabled := dbpkg.GetTOTPSecret(h.db, user.ID); enabled {
		return "", "", http.StatusConflict, "2FA is already enabled."
	}
	secret, err := totpGenSecret()
	if err != nil {
		return "", "", http.StatusInternalServerError, "Could not generate a secret — try again."
	}
	if err := dbpkg.SetTOTPSecret(h.db, user.ID, secret); err != nil {
		return "", "", http.StatusInternalServerError, "Could not save the secret — try again."
	}
	return totpProvisioningURI(secret, user.Handle, "F33D3R"), secret, http.StatusOK, ""
}

// totpEnableCore verifies a code against the secret minted by totpBeginSetup
// and turns 2FA on. 200 means it is on; any other status carries the sentence.
func (h *Handler) totpEnableCore(user *model.User, code string) (int, string) {
	if h.db == nil {
		return http.StatusServiceUnavailable, "Database unavailable."
	}
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if code == "" {
		return http.StatusBadRequest, "Enter the 6-digit code."
	}
	if _, err := strconv.Atoi(code); err != nil || len(code) != 6 {
		return http.StatusBadRequest, "Enter a 6-digit code."
	}
	secret, enabled := dbpkg.GetTOTPSecret(h.db, user.ID)
	if enabled {
		return http.StatusConflict, "2FA is already enabled."
	}
	if secret == "" {
		return http.StatusBadRequest, "Set up 2FA first."
	}
	if !totpVerify(secret, code) {
		return http.StatusUnauthorized, "Invalid code."
	}
	if err := dbpkg.EnableTOTP(h.db, user.ID); err != nil {
		return http.StatusInternalServerError, "Could not enable 2FA — try again."
	}
	return http.StatusOK, ""
}

// totpDisableCore verifies a current code and turns 2FA off.
func (h *Handler) totpDisableCore(user *model.User, code string) (int, string) {
	if h.db == nil {
		return http.StatusServiceUnavailable, "Database unavailable."
	}
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	secret, enabled := dbpkg.GetTOTPSecret(h.db, user.ID)
	if !enabled {
		return http.StatusBadRequest, "2FA is not enabled."
	}
	if !totpVerify(secret, code) {
		return http.StatusUnauthorized, "Invalid code."
	}
	if err := dbpkg.DisableTOTP(h.db, user.ID); err != nil {
		return http.StatusInternalServerError, "Could not disable 2FA — try again."
	}
	return http.StatusOK, ""
}

// GET /api/2fa/setup — generates a new TOTP secret and returns a provisioning URI.
func (h *Handler) totpSetup(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	uri, secret, status, msg := h.totpBeginSetup(user)
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
	if status, msg := h.totpEnableCore(user, r.FormValue("code")); status != http.StatusOK {
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
	if status, msg := h.totpDisableCore(user, r.FormValue("code")); status != http.StatusOK {
		h.totpRefuse(w, r, msg, status)
		return
	}
	if h.totpRenderSecurity(w, r, user) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}
