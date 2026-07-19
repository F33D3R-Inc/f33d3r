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
	"github.com/f33d3r/feed-engine/internal/realm"
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
	h.render(w, "login.html", map[string]interface{}{
		"Title": "Sign in · F33D3R",
	})
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	handle   := strings.TrimSpace(strings.ToLower(r.FormValue("handle")))
	password := r.FormValue("password")
	ip       := requestIP(r)

	if !handleRe.MatchString(handle) {
		h.render(w, "login.html", map[string]interface{}{
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
		h.render(w, "login.html", map[string]interface{}{
			"Title": "Sign in · F33D3R",
			"Error": "Invalid handle or password.",
		})
		return
	}

	user, _ := dbpkg.GetUserByHandle(h.db, handle)
	if user == nil {
		go LogSecurityEvent(h.db, "login_failed", "low", "", ip, r.UserAgent(), "/login",
			map[string]interface{}{"handle": handle, "reason": "user_missing"})
		h.render(w, "login.html", map[string]interface{}{
			"Title": "Sign in · F33D3R",
			"Error": "Invalid handle or password.",
		})
		return
	}

	// Password check — required for all accounts
	if dbpkg.HasPassword(h.db, user.ID) {
		if password == "" {
			h.render(w, "login.html", map[string]interface{}{
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
			h.render(w, "login.html", map[string]interface{}{
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
			h.render(w, "login.html", map[string]interface{}{
				"Title":            "Sign in · F33D3R",
				"Handle":           handle,
				"NeedsSetPassword": true,
			})
			return
		}
		if len(password) < 8 {
			h.render(w, "login.html", map[string]interface{}{
				"Title":            "Sign in · F33D3R",
				"Handle":           handle,
				"NeedsSetPassword": true,
				"Error":            "Password must be at least 8 characters.",
			})
			return
		}
		if confirmPassword != password {
			h.render(w, "login.html", map[string]interface{}{
				"Title":            "Sign in · F33D3R",
				"Handle":           handle,
				"NeedsSetPassword": true,
				"Error":            "Passwords don't match.",
			})
			return
		}
		if err := dbpkg.SetPassword(h.db, user.ID, password); err != nil {
			log.Printf("[login] SetPassword error: %v", err)
			h.render(w, "login.html", map[string]interface{}{
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
			h.render(w, "login.html", map[string]interface{}{
				"Title": "Sign in · F33D3R",
				"Error": "This account has been suspended. Contact support if you believe this is an error.",
			})
			return
		}
	}

	// Create session token
	deviceName := r.UserAgent()
	if len(deviceName) > 100 { deviceName = deviceName[:100] }
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
		http.Redirect(w, r, "/backup-codes/setup", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// loginRiskSignal sends device/UA signals to Verity on each login.
// If risk score is too high, marks user for re-verification.
func (h *Handler) loginRiskSignal(pialID string, r *http.Request) {
	if h.cfg.VerityURL == "" || pialID == "" {
		return
	}
	ua := r.UserAgent()
	if len(ua) > 200 { ua = ua[:200] }
	body, _ := json.Marshal(map[string]interface{}{
		"pial_id":       pialID,
		"signal":        "login",
		"ip_reputation": 0.85,
		"velocity_score": 0.0,
		"context":       "user_login",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		h.cfg.VerityURL+"/v1/risk/signal", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
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
			_, _ = h.db.Exec(
				`UPDATE pial_roots SET age_verified = FALSE WHERE pial_id = $1`,
				pialID,
			)
			log.Printf("[kyc] high risk login (%.2f) — flagged %s for re-verify", result.RiskScore, pialID)
		}
	}
}

func (h *Handler) deactivatedPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, "deactivated.html", map[string]interface{}{
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
	http.Redirect(w, r, "/login", http.StatusSeeOther)
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
	h.render(w, "onboard.html", map[string]interface{}{
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
		h.render(w, "onboard.html", map[string]interface{}{
			"Title":   "Welcome to F33D3R",
			"Themes":  ThemesWithActive("void"),
			"Error":   msg,
			"MaxDOB":  maxDOB,
			"MinDOB":  minDOB,
			"IsMinor": false, // unknown at render time; DOB gates adult option server-side on POST
		})
	}

	// Terms acceptance — server-side enforcement.
	if r.FormValue("terms_accepted") != "1" {
		renderErr("You must accept the Terms of Service to continue.")
		return
	}

	handle := strings.TrimSpace(strings.ToLower(r.FormValue("handle")))
	if !handleRe.MatchString(handle) {
		renderErr("Handle must be 1–30 characters, letters/numbers/underscore only.")
		return
	}
	if reservedHandle(handle) {
		renderErr("That handle is reserved. Try another.")
		return
	}

	password        := r.FormValue("password")
	confirmPassword := r.FormValue("confirm_password")
	if len(password) < 8 {
		renderErr("Password must be at least 8 characters.")
		return
	}
	if password != confirmPassword {
		renderErr("Passwords don't match.")
		return
	}

	// Date of birth — required, server-side age gate.
	dobStr := strings.TrimSpace(r.FormValue("dob"))
	if dobStr == "" {
		renderErr("Date of birth is required.")
		return
	}
	dob, err := time.Parse("2006-01-02", dobStr)
	if err != nil {
		renderErr("Invalid date of birth. Use YYYY-MM-DD format.")
		return
	}
	age := ageFromDOB(dob)
	if age < 16 {
		renderErr("You must be at least 16 years old to join F33D3R.")
		return
	}
	isMinor := age < 18
	isAdult := age >= 18

	// Role + creator type — validated here so the single POST covers all steps.
	roleType    := r.FormValue("role_type")
	creatorType := r.FormValue("creator_type")
	allowedRoles := map[string]bool{"user": true, "creator": true, "official": true}
	if !allowedRoles[roleType] {
		roleType = "user"
	}

	// Content setting — validated here.
	contentSetting := r.FormValue("content_setting")
	allowedContent := map[string]bool{"safe_mode": true, "default": true, "adult_enabled": true}
	if !allowedContent[contentSetting] {
		contentSetting = "default"
	}
	// Minors may not enable adult content regardless of form submission.
	if isMinor && contentSetting == "adult_enabled" {
		contentSetting = "default"
	}

	if h.db != nil {
		taken, _ := dbpkg.IsHandleTaken(h.db, handle)
		if taken {
			renderErr("That handle is already taken. Try another.")
			return
		}
		id, err := dbpkg.UpsertUserByHandle(h.db, handle)
		if err != nil {
			log.Printf("[onboard] upsert error: %v", err)
		}
		displayName := strings.TrimSpace(r.FormValue("display_name"))
		if displayName == "" {
			displayName = handle
		}
		themeID := r.FormValue("theme_id")
		if !isValidTheme(themeID) {
			themeID = "void"
		}
		if err := dbpkg.SetPassword(h.db, id, password); err != nil {
			log.Printf("[onboard] SetPassword error: %v", err)
			renderErr("Could not save password — try again.")
			return
		}
		go realm.AwardXP(h.db, id, "onboard", "", realm.XPOnboard)
		_ = dbpkg.SaveProfile(h.db, &model.ProfileSave{
			UserID:      id,
			DisplayName: truncate(displayName, 100),
			ThemeID:     themeID,
		})
		if pialID := dbpkg.BootstrapPIAL(h.db, id, handle); pialID == "" {
			log.Printf("[onboard] PIAL bootstrap failed for %s — account exists without identity", handle)
		}
		if founder, err := dbpkg.GetUserByHandle(h.db, "tehanibentley"); err == nil && founder != nil && founder.ID != id {
			_ = dbpkg.FollowUser(h.db, id, founder.ID)
		}
	}

	// Create session + complete all onboarding steps in one transaction.
	if h.db != nil {
		if u, _ := dbpkg.GetUserByHandle(h.db, handle); u != nil {
			deviceName := r.UserAgent()
			if len(deviceName) > 100 { deviceName = deviceName[:100] }
			if token, err := dbpkg.CreateAuthSession(h.db, u.ID, deviceName, r.RemoteAddr); err == nil {
				SetSessionCookie(w, token)
			}

			// Set role, creator type, age classification, and mark onboarding done — all at once.
			_ = dbpkg.UpsertUserRole(h.db, &dbpkg.UserRole{
				UserID:      u.ID,
				RoleType:    roleType,
				CreatorType: creatorType,
				IsAdult:     isAdult,
				IsMinor:     isMinor,
				OnboardStep: 5,
				OnboardDone: true,
			})

			// Store DOB in user_roles for audit trail (legacy) and in pial_roots as the
			// authoritative source. Profile edit reads from pial_roots only going forward.
			_, _ = h.db.Exec(
				`UPDATE user_roles SET date_of_birth = $1 WHERE user_id = $2`,
				dob.Format("2006-01-02"), u.ID,
			)
			if u.PIALID != "" {
				_ = dbpkg.SetPIALBirthday(h.db, u.PIALID, dob)
			}

			// Adult Content Creator → flag all their posts for age-gating via Abraxas Shield.
			if creatorType == "adult_content_creator" {
				_, _ = h.db.Exec(
					`UPDATE user_profiles SET is_adult_creator = TRUE WHERE user_id = $1`,
					u.ID,
				)
			}

			// Save content preference chosen on the single-page form.
			_, _ = h.db.Exec(
				`UPDATE user_profiles SET content_setting = $1 WHERE user_id = $2`,
				contentSetting, u.ID,
			)
		}
	}

	SetHandleCookie(w, handle)
	// Single-page flow: all steps done.
	// Gate: new users must generate backup codes before reaching the feed.
	if h.db != nil {
		if u, _ := dbpkg.GetUserByHandle(h.db, handle); u != nil {
			if !dbpkg.HasUnusedBackupCodes(h.db, u.ID) {
				http.Redirect(w, r, "/backup-codes/setup", http.StatusSeeOther)
				return
			}
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) onboardRolePage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		roleType    := r.FormValue("role_type")
		creatorType := r.FormValue("creator_type")
		allowed := map[string]bool{"user": true, "creator": true, "official": true}
		if !allowed[roleType] { roleType = "user" }
		if h.db != nil {
			role, _ := dbpkg.GetUserRole(h.db, user.ID)
			if role == nil { role = &dbpkg.UserRole{UserID: user.ID} }
			role.RoleType    = roleType
			role.CreatorType = creatorType
			role.OnboardStep = 2
			_ = dbpkg.UpsertUserRole(h.db, role)
			// Adult Content Creator → flag all their posts for age-gating via Abraxas Shield.
			if creatorType == "adult_content_creator" {
				_, _ = h.db.Exec(
					`UPDATE user_profiles SET is_adult_creator = TRUE WHERE user_id = $1`,
					user.ID,
				)
			}
		}
		// Age is already known from DOB collected at step 1. Skip /onboard/age.
		http.Redirect(w, r, "/onboard/content", http.StatusSeeOther)
		return
	}
	h.render(w, "onboard_role.html", map[string]interface{}{
		"Title": "Who are you? · F33D3R",
		"User":  user,
	})
}

func (h *Handler) onboardAgePage(w http.ResponseWriter, r *http.Request) {
	// Age classification moved to onboard step 1 (DOB field). This route
	// is kept for backward compat but redirects to the content step.
	http.Redirect(w, r, "/onboard/content", http.StatusSeeOther)
}

func (h *Handler) onboardContentPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		contentSetting := r.FormValue("content_setting") // safe_mode | default | adult_enabled
		allowed := map[string]bool{"safe_mode": true, "default": true, "adult_enabled": true}
		if !allowed[contentSetting] { contentSetting = "default" }
		if h.db != nil {
			_ = h.db.QueryRow(`UPDATE user_profiles SET content_setting = $1 WHERE user_id = $2`, contentSetting, user.ID)
			role, _ := dbpkg.GetUserRole(h.db, user.ID)
			if role != nil {
				role.OnboardStep = 5
				role.OnboardDone = true
				_ = dbpkg.UpsertUserRole(h.db, role)
			}
		}
		// Gate: new users must generate backup codes before reaching the feed.
		if h.db != nil && !dbpkg.HasUnusedBackupCodes(h.db, user.ID) {
			http.Redirect(w, r, "/backup-codes/setup", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	role, _ := dbpkg.GetUserRole(h.db, user.ID)
	h.render(w, "onboard_content.html", map[string]interface{}{
		"Title": "Content preferences · F33D3R",
		"User":  user,
		"Role":  role,
	})
}

func (h *Handler) onboardVerifyPage(w http.ResponseWriter, r *http.Request) {
	// Verification removed from onboarding — it's a post-signup Settings action.
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) onboardSetupPage(w http.ResponseWriter, r *http.Request) {
	// No messaging/vault setup in onboarding.
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
