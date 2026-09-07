package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

func (h *Handler) loginPage(w http.ResponseWriter, r *http.Request) {
	// Only redirect if there's a valid session token (not just an old handle cookie)
	if h.db != nil {
		if sess := GetSessionToken(r); sess != "" && !strings.HasPrefix(sess, "__handle__") {
			if uid, _ := dbpkg.GetUserIDFromSession(h.db, sess); uid != "" {
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
		}
	}
	h.render(w, r, "login.html", map[string]interface{}{
		"Title": "Sign in · F33D3R",
	})
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	handle := strings.TrimSpace(strings.ToLower(r.FormValue("handle")))
	password := r.FormValue("password")
	ip := requestIP(r)

	if !handleRe.MatchString(handle) {
		h.render(w, r, "login.html", map[string]interface{}{
			"Title": "Sign in · F33D3R",
			"Error": "Invalid handle — letters, numbers, underscore, 1–30 chars.",
		})
		return
	}

	if h.db == nil {
		// Dev mode — no DB, just set handle cookie
		SetHandleCookie(w, handle)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	// Check handle exists. Generic error keeps handle enumeration impossible —
	// an attacker must not be able to discover which handles exist by probing login.
	taken, _ := dbpkg.IsHandleTaken(h.db, handle)
	if !taken {
		go LogSecurityEvent(h.db, "login_failed", "low", "", ip, r.UserAgent(), "/login",
			map[string]interface{}{"handle": handle, "reason": "unknown_handle"})
		go CheckBruteForce(h.db, ip)
		h.render(w, r, "login.html", map[string]interface{}{
			"Title": "Sign in · F33D3R",
			"Error": "Invalid handle or password.",
		})
		return
	}

	user, _ := dbpkg.GetUserByHandle(h.db, handle)
	if user == nil {
		go LogSecurityEvent(h.db, "login_failed", "low", "", ip, r.UserAgent(), "/login",
			map[string]interface{}{"handle": handle, "reason": "user_missing"})
		h.render(w, r, "login.html", map[string]interface{}{
			"Title": "Sign in · F33D3R",
			"Error": "Invalid handle or password.",
		})
		return
	}

	// Password check — required for all accounts
	if dbpkg.HasPassword(h.db, user.ID) {
		if password == "" {
			h.render(w, r, "login.html", map[string]interface{}{
				"Title":         "Sign in · F33D3R",
				"Handle":        handle,
				"NeedsPassword": true,
			})
			return
		}
		userID, err := dbpkg.CheckPassword(h.db, handle, password)
		if err != nil || userID == "" {
			go LogSecurityEvent(h.db, "login_failed", "medium", user.ID, ip, r.UserAgent(), "/login",
				map[string]interface{}{"handle": handle, "reason": "bad_password"})
			go CheckBruteForce(h.db, ip)
			go CheckBruteForceHandle(h.db, handle, ip)
			h.render(w, r, "login.html", map[string]interface{}{
				"Title":         "Sign in · F33D3R",
				"Handle":        handle,
				"NeedsPassword": true,
				"Error":         "Incorrect password.",
			})
			return
		}
	} else {
		// No password set yet — require the user to create one before granting access.
		// Without this, any visitor who knows a handle can log in without credentials.
		confirmPassword := r.FormValue("confirm_password")
		if password == "" {
			h.render(w, r, "login.html", map[string]interface{}{
				"Title":            "Sign in · F33D3R",
				"Handle":           handle,
				"NeedsSetPassword": true,
			})
			return
		}
		if len(password) < 8 {
			h.render(w, r, "login.html", map[string]interface{}{
				"Title":            "Sign in · F33D3R",
				"Handle":           handle,
				"NeedsSetPassword": true,
				"Error":            "Password must be at least 8 characters.",
			})
			return
		}
		if confirmPassword != password {
			h.render(w, r, "login.html", map[string]interface{}{
				"Title":            "Sign in · F33D3R",
				"Handle":           handle,
				"NeedsSetPassword": true,
				"Error":            "Passwords don't match.",
			})
			return
		}
		if err := dbpkg.SetPassword(h.db, user.ID, password); err != nil {
			log.Printf("[login] SetPassword error: %v", err)
			h.render(w, r, "login.html", map[string]interface{}{
				"Title": "Sign in · F33D3R",
				"Error": "Could not save password — try again.",
			})
			return
		}
	}

	// Deactivated accounts: allow reactivation by logging back in within 30 days.
	if dbpkg.IsDeactivated(h.db, user.ID) {
		// Reactivate the account on login — clear the deactivated_at timestamp.
		h.db.Exec(`UPDATE users SET deactivated_at = NULL, deactivated_reason = '' WHERE id = $1`, user.ID)
	}

	// PIAL capability check — suspended accounts blocked
	if user.PIALID != "" {
		caps := dbpkg.GetCapabilities(h.db, user.PIALID)
		if !caps.Can(model.CapPosting) && !caps.Can(model.CapMessaging) {
			h.render(w, r, "login.html", map[string]interface{}{
				"Title": "Sign in · F33D3R",
				"Error": "This account has been suspended. Contact support if you believe this is an error.",
			})
			return
		}
	}

	// Create session token
	deviceName := r.UserAgent()
	if len(deviceName) > 100 {
		deviceName = deviceName[:100]
	}
	token, err := dbpkg.CreateAuthSession(h.db, user.ID, deviceName, r.RemoteAddr)
	if err != nil {
		log.Printf("[login] session create: %v", err)
		// Fall back to legacy handle cookie
		SetHandleCookie(w, handle)
	} else {
		SetSessionCookie(w, token)
		SetHandleCookie(w, handle) // keep for backward compat
	}
	// Device fingerprinting — logs new_device_login security event on first login from this browser.
	go RegisterDevice(h.db, user.ID, r.UserAgent(), ip)
	// Anomaly detection — alert the user if this device/IP has never been seen before.
	go func() {
		if !dbpkg.IsKnownDevice(h.db, user.ID, deviceName, ip) {
			h.sendNewDeviceAlert(user, r)
		}
	}()
	// Phase 5: async risk signal on every login — device/velocity scoring
	go h.loginRiskSignal(user.PIALID, r)

	// Backup codes are mandatory. Gate every login until codes are generated.
	if !dbpkg.HasUnusedBackupCodes(h.db, user.ID) {
		hxAwareRedirect(w, r, "/backup-codes/setup")
		return
	}
	// Signing in changes the Shell (nav, account row, theme): a document load.
	hxAwareRedirect(w, r, "/")
}

// loginRiskSignal sends device/UA signals to Verity on each login.
// If risk score is too high, marks user for re-verification.
func (h *Handler) loginRiskSignal(pialID string, r *http.Request) {
	if h.cfg.VerityURL == "" || pialID == "" {
		return
	}
	ua := r.UserAgent()
	if len(ua) > 200 {
		ua = ua[:200]
	}
	body, _ := json.Marshal(map[string]interface{}{
		"pial_id":        pialID,
		"signal":         "login",
		"ip_reputation":  0.85,
		"velocity_score": 0.0,
		"context":        "user_login",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		h.cfg.VerityURL+"/v1/risk/signal", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	// Verity's risk lane is service-to-service — an account may not clear its own
	// score. feed-engine is the peer, so it holds the shared secret.
	req.Header.Set("X-Internal-Key", h.cfg.InternalAPIKey)
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var result struct {
		RiskScore float64 `json:"risk_score"`
	}
	if json.NewDecoder(resp.Body).Decode(&result) == nil && result.RiskScore >= 0.8 {
		// High risk: flag for re-verification — a moderation pipeline writing PIAL (the record).
		if h.db != nil {
			// An identity write. If it fails the account keeps a verification this
			// pipeline just decided it should not have, so the log must not claim
			// the flag was applied.
			if _, err := h.db.Exec(
				`UPDATE pial_roots SET age_verified = FALSE WHERE pial_id = $1::uuid`,
				pialID,
			); err != nil {
				log.Printf("[kyc] high risk login (%.2f) for %s but the re-verify flag FAILED to apply: %v",
					result.RiskScore, pialID, err)
				return
			}
			log.Printf("[kyc] high risk login (%.2f) — flagged %s for re-verify", result.RiskScore, pialID)
		}
	}
}

func (h *Handler) deactivatedPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "deactivated.html", map[string]interface{}{
		"Title": "Account Deactivated · F33D3R",
	})
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if tok := GetSessionToken(r); tok != "" && len(tok) < 100 {
		h.sessionCache.Delete(tok)
		if h.db != nil {
			go dbpkg.DeleteSession(h.db, tok)
		}
	}
	ClearSessionCookie(w)
	// Signing out changes the Shell: a document load, for every kind of caller.
	hxAwareRedirect(w, r, "/login")
}

// ── Onboarding ────────────────────────────────────────────────────────────────

// dobBounds returns the max and min date strings for the DOB input.
// max = today − 16 years (youngest allowed), min = today − 120 years.
func dobBounds() (maxDOB, minDOB string) {
	now := time.Now().UTC()
	maxDOB = now.AddDate(-16, 0, 0).Format("2006-01-02")
	minDOB = now.AddDate(-120, 0, 0).Format("2006-01-02")
	return
}

// ageFromDOB returns the age in completed years for a given birth date.
func ageFromDOB(dob time.Time) int {
	now := time.Now().UTC()
	age := now.Year() - dob.Year()
	if now.Month() < dob.Month() || (now.Month() == dob.Month() && now.Day() < dob.Day()) {
		age--
	}
	return age
}

func (h *Handler) onboardPage(w http.ResponseWriter, r *http.Request) {
	// Already has a handle → go to feed
	if HandleFromCookie(r) != "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	maxDOB, minDOB := dobBounds()
	h.render(w, r, "onboard.html", map[string]interface{}{
		"Title":   "Welcome to F33D3R",
		"Themes":  ThemesWithActive("void"),
		"MaxDOB":  maxDOB,
		"MinDOB":  minDOB,
		"IsMinor": false, // unknown until DOB entered; adult option shown, server enforces on POST
	})
}

func (h *Handler) handleOnboard(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	maxDOB, minDOB := dobBounds()

	renderErr := func(msg string) {
		h.render(w, r, "onboard.html", map[string]interface{}{
			"Title":   "Welcome to F33D3R",
			"Themes":  ThemesWithActive("void"),
			"Error":   msg,
			"MaxDOB":  maxDOB,
			"MinDOB":  minDOB,
			"IsMinor": false, // unknown at render time; DOB gates adult option server-side on POST
		})
	}

	// The dev listener without a database has no identity plane to write;
	// it only remembers the handle, as it always did.
	if h.db == nil {
		handle := strings.TrimSpace(strings.ToLower(r.FormValue("handle")))
		if !handleRe.MatchString(handle) {
			renderErr("Handle must be 1–30 characters, letters/numbers/underscore only.")
			return
		}
		SetHandleCookie(w, handle)
		hxAwareRedirect(w, r, "/")
		return
	}

	// One writer of new accounts: the same sequence the JSON signup runs.
	// Refusals come back as the sentence the person should read.
	user, token, aerr := h.createAccount(r, accountParams{
		Handle:          r.FormValue("handle"),
		Password:        r.FormValue("password"),
		ConfirmPassword: r.FormValue("confirm_password"),
		DisplayName:     r.FormValue("display_name"),
		ThemeID:         r.FormValue("theme_id"),
		DateOfBirth:     r.FormValue("dob"),
		RoleType:        r.FormValue("role_type"),
		CreatorType:     r.FormValue("creator_type"),
		ContentSetting:  r.FormValue("content_setting"),
		TermsAccepted:   r.FormValue("terms_accepted") == "1",
		DeviceName:      r.UserAgent(),
	})
	if aerr != nil {
		renderErr(aerr.Message)
		return
	}
	handle := user.Handle
	SetSessionCookie(w, token)

	SetHandleCookie(w, handle)
	// Single-page flow: all steps done.
	// Gate: new users must generate backup codes before reaching the feed.
	if h.db != nil {
		if u, _ := dbpkg.GetUserByHandle(h.db, handle); u != nil {
			if !dbpkg.HasUnusedBackupCodes(h.db, u.ID) {
				hxAwareRedirect(w, r, "/backup-codes/setup")
				return
			}
		}
	}
	// A new account changes the Shell: a document load.
	hxAwareRedirect(w, r, "/")
}

// The retired multi-step onboarding.
//
// Signup is one POST to /onboard: role, creator type, date of birth and content
// preference are all collected and written there. The step pages that used to
// carry them (/onboard/role, /onboard/age, /onboard/content, /onboard/verify,
// /onboard/setup) had their templates deleted when the single-page form landed,
// but two of their handlers kept calling h.render for pages that no longer
// existed — so those routes could only ever answer "unknown page", and their
// POST halves were a second, unreachable writer of content_setting and of the
// onboarding-complete flag.
//
// What remains is redirects. A person who still has one of these URLs open is
// sent to the feed; nothing here writes anything, because there is exactly one
// place that completes an onboarding and it is handleOnboard.

func (h *Handler) onboardAgePage(w http.ResponseWriter, r *http.Request) {
	// Age classification is collected at signup (the DOB field).
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) onboardVerifyPage(w http.ResponseWriter, r *http.Request) {
	// Verification removed from onboarding — it's a post-signup Settings action.
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) onboardSetupPage(w http.ResponseWriter, r *http.Request) {
	// No messaging/vault setup in onboarding.
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
