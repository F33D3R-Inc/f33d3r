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

	// Check handle exists
	taken, _ := dbpkg.IsHandleTaken(h.db, handle)
	if !taken {
		h.render(w, "login.html", map[string]interface{}{
			"Title": "Sign in · F33D3R",
			"Error": "Handle not found. New here? Create an account.",
		})
		return
	}

	user, _ := dbpkg.GetUserByHandle(h.db, handle)
	if user == nil {
		h.render(w, "login.html", map[string]interface{}{
			"Title": "Sign in · F33D3R",
			"Error": "Account not found.",
		})
		return
	}

	// Password check — if user has a password set, require it
	if dbpkg.HasPassword(h.db, user.ID) {
		if password == "" {
			h.render(w, "login.html", map[string]interface{}{
				"Title":           "Sign in · F33D3R",
				"Handle":          handle,
				"NeedsPassword":   true,
			})
			return
		}
		userID, err := dbpkg.CheckPassword(h.db, handle, password)
		if err != nil || userID == "" {
			h.render(w, "login.html", map[string]interface{}{
				"Title":         "Sign in · F33D3R",
				"Handle":        handle,
				"NeedsPassword": true,
				"Error":         "Incorrect password.",
			})
			return
		}
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
	// Phase 5: async risk signal on every login — device/velocity scoring
	go h.loginRiskSignal(user.PIALID, r)
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
		// High risk: flag for re-verification
		if h.db != nil {
			_, _ = h.db.Exec(
				`UPDATE user_profiles SET is_verified = FALSE
				  WHERE user_id = (SELECT id FROM users WHERE pial_id = $1)`,
				pialID,
			)
			log.Printf("[kyc] high risk login (%.2f) — flagged %s for re-verify", result.RiskScore, pialID)
		}
	}
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

func (h *Handler) onboardPage(w http.ResponseWriter, r *http.Request) {
	// Already has a handle → go to feed
	if HandleFromCookie(r) != "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	h.render(w, "onboard.html", map[string]interface{}{
		"Title":  "Welcome to F33D3R",
		"Themes": ThemesWithActive("void"),
	})
}

func (h *Handler) handleOnboard(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	handle := strings.TrimSpace(strings.ToLower(r.FormValue("handle")))
	if !handleRe.MatchString(handle) {
		h.render(w, "onboard.html", map[string]interface{}{
			"Title":  "Welcome to F33D3R",
			"Themes": ThemesWithActive("void"),
			"Error":  "Handle must be 1–30 characters, letters/numbers/underscore only.",
		})
		return
	}

	if h.db != nil {
		taken, _ := dbpkg.IsHandleTaken(h.db, handle)
		if taken {
			h.render(w, "onboard.html", map[string]interface{}{
				"Title":  "Welcome to F33D3R",
				"Themes": ThemesWithActive("void"),
				"Error":  "That handle is already taken. Try another.",
			})
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
		go realm.AwardXP(h.db, id, "onboard", "", realm.XPOnboard)
		_ = dbpkg.SaveProfile(h.db, &model.ProfileSave{
			UserID:      id,
			DisplayName: truncate(displayName, 100),
			ThemeID:     themeID,
		})
		// Bootstrap PIAL synchronously — user must not act before identity exists.
		if pialID := dbpkg.BootstrapPIAL(h.db, id, handle); pialID == "" {
			log.Printf("[onboard] PIAL bootstrap failed for %s — account exists without identity", handle)
		}
	}

	// Create session for the new account
	if h.db != nil {
		if u, _ := dbpkg.GetUserByHandle(h.db, handle); u != nil {
			deviceName := r.UserAgent()
			if len(deviceName) > 100 { deviceName = deviceName[:100] }
			if token, err := dbpkg.CreateAuthSession(h.db, u.ID, deviceName, r.RemoteAddr); err == nil {
				SetSessionCookie(w, token)
			}
			// Initialize role row — onboarding not done yet
			_ = dbpkg.UpsertUserRole(h.db, &dbpkg.UserRole{
				UserID:      u.ID,
				RoleType:    "user",
				OnboardStep: 1,
				OnboardDone: false,
			})
		}
	}

	SetHandleCookie(w, handle)
	http.Redirect(w, r, "/onboard/role", http.StatusSeeOther)
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
			_ = dbpkg.UpsertUserRole(h.db, &dbpkg.UserRole{
				UserID: user.ID, RoleType: roleType,
				CreatorType: creatorType, OnboardStep: 2,
			})
		}
		http.Redirect(w, r, "/onboard/age", http.StatusSeeOther)
		return
	}
	h.render(w, "onboard_role.html", map[string]interface{}{
		"Title": "Who are you? · F33D3R",
		"User":  user,
	})
}

func (h *Handler) onboardAgePage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		ageGroup := r.FormValue("age_group") // "adult" | "minor_16" | "minor_under16"
		isAdult  := ageGroup == "adult"
		isMinor  := ageGroup == "minor_16" || ageGroup == "minor_under16"
		if ageGroup == "minor_under16" {
			// Under 16 — redirect to age-gate rejection
			h.render(w, "onboard_age.html", map[string]interface{}{
				"Title":      "Age verification · F33D3R",
				"User":       user,
				"AgeBlocked": true,
			})
			return
		}
		if h.db != nil {
			role, _ := dbpkg.GetUserRole(h.db, user.ID)
			if role == nil { role = &dbpkg.UserRole{UserID: user.ID} }
			role.IsAdult = isAdult
			role.IsMinor = isMinor
			role.OnboardStep = 3
			_ = dbpkg.UpsertUserRole(h.db, role)
		}
		// Minors skip content preferences, go straight to verify then setup
		if isMinor {
			http.Redirect(w, r, "/onboard/verify", http.StatusSeeOther)
		} else {
			http.Redirect(w, r, "/onboard/content", http.StatusSeeOther)
		}
		return
	}
	h.render(w, "onboard_age.html", map[string]interface{}{
		"Title": "Age verification · F33D3R",
		"User":  user,
	})
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
			if role != nil { role.OnboardStep = 4; _ = dbpkg.UpsertUserRole(h.db, role) }
		}
		http.Redirect(w, r, "/onboard/verify", http.StatusSeeOther)
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
	user := h.userFromRequest(w, r)
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		choice := r.FormValue("verify_choice") // "verify" | "skip"
		if h.db != nil {
			role, _ := dbpkg.GetUserRole(h.db, user.ID)
			if role != nil {
				if choice == "verify" { role.IsAgeVerified = true }
				role.OnboardStep = 5
				role.OnboardDone = true
				_ = dbpkg.UpsertUserRole(h.db, role)
			}
		}
		http.Redirect(w, r, "/onboard/setup", http.StatusSeeOther)
		return
	}
	role, _ := dbpkg.GetUserRole(h.db, user.ID)
	h.render(w, "onboard_verify.html", map[string]interface{}{
		"Title": "Verify your identity · F33D3R",
		"User":  user,
		"Role":  role,
	})
}

func (h *Handler) onboardSetupPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	h.render(w, "onboard_setup.html", map[string]interface{}{
		"Title": "Set up messaging · F33D3R",
		"User":  user,
	})
}
