package handler

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
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

// GET /api/2fa/setup — generates a new TOTP secret and returns a provisioning URI.
func (h *Handler) totpSetup(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	_, enabled := dbpkg.GetTOTPSecret(h.db, user.ID)
	if enabled {
		http.Error(w, "2FA already enabled", http.StatusConflict)
		return
	}
	secret, err := totpGenSecret()
	if err != nil {
		http.Error(w, "failed to generate secret", http.StatusInternalServerError)
		return
	}
	if err := dbpkg.SetTOTPSecret(h.db, user.ID, secret); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	uri := totpProvisioningURI(secret, user.Handle, "F33D3R")
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"uri":%q,"secret":%q}`, uri, secret)
}

// POST /api/2fa/enable — verifies a TOTP code and activates 2FA.
func (h *Handler) totpEnable(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	code := strings.TrimSpace(r.FormValue("code"))
	if code == "" {
		http.Error(w, "code required", http.StatusBadRequest)
		return
	}
	code = strings.ReplaceAll(code, " ", "")
	if _, err := strconv.Atoi(code); err != nil || len(code) != 6 {
		http.Error(w, "invalid code format", http.StatusBadRequest)
		return
	}
	secret, enabled := dbpkg.GetTOTPSecret(h.db, user.ID)
	if enabled {
		http.Error(w, "2FA already enabled", http.StatusConflict)
		return
	}
	if secret == "" {
		http.Error(w, "setup not started — call /api/2fa/setup first", http.StatusBadRequest)
		return
	}
	if !totpVerify(secret, code) {
		http.Error(w, "invalid code", http.StatusUnauthorized)
		return
	}
	if err := dbpkg.EnableTOTP(h.db, user.ID); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// POST /api/2fa/disable — disables 2FA after verifying the current code.
func (h *Handler) totpDisable(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	code := strings.TrimSpace(r.FormValue("code"))
	secret, enabled := dbpkg.GetTOTPSecret(h.db, user.ID)
	if !enabled {
		http.Error(w, "2FA not enabled", http.StatusBadRequest)
		return
	}
	if !totpVerify(secret, code) {
		http.Error(w, "invalid code", http.StatusUnauthorized)
		return
	}
	if err := dbpkg.DisableTOTP(h.db, user.ID); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}
