package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/f33d3r/feed-engine/internal/identity"
)

func (h *Handler) loginPage(w http.ResponseWriter, r *http.Request) {
	// Only redirect when the presented session is confirmed live. A session
	// that cannot be confirmed is shown the form: signing in again is the one
	// thing that works when resolution is down.
	if u, err := h.viewer(w, r); err == nil && viewerAccountID(u) != "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	h.render(w, r, "login.html", map[string]interface{}{
		"Title": "Sign in · F33D3R",
	})
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	handle := strings.TrimSpace(strings.ToLower(r.FormValue("handle")))
	password := r.FormValue("password")

	page := func(data map[string]interface{}) {
		data["Title"] = "Sign in · F33D3R"
		h.render(w, r, "login.html", data)
	}
	if !handleRe.MatchString(handle) {
		page(map[string]interface{}{"Error": "Invalid handle — letters, numbers, underscore, 1–30 chars."})
		return
	}
	if password == "" {
		page(map[string]interface{}{"Handle": handle})
		return
	}

	deviceName := r.UserAgent()
	if len(deviceName) > 100 {
		deviceName = deviceName[:100]
	}
	out := h.signIn(r, handle, password, r.FormValue("code"), deviceName, "/login")
	switch {
	case out.Result != nil:
	case out.NeedsCode:
		page(map[string]interface{}{"Handle": handle, "NeedsCode": true})
		return
	case out.BadCode:
		page(map[string]interface{}{"Handle": handle, "NeedsCode": true, "Error": "That code is not right. Try again."})
		return
	case out.Suspended:
		page(map[string]interface{}{"Error": "This account has been suspended. Contact support if you believe this is an error."})
		return
	case out.NoPassword:
		page(map[string]interface{}{"Handle": handle, "Error": "This account has no password yet. Use “Forgot password?” with one of your backup codes to set one."})
		return
	case out.Unavailable:
		page(map[string]interface{}{"Handle": handle, "Error": "Sign-in is briefly unavailable. Try again in a moment."})
		return
	default:
		page(map[string]interface{}{"Handle": handle, "Error": "Invalid handle or password."})
		return
	}

	h.openedSession(w, out.Result.SessionToken)
	h.afterSignIn(r, out.Result.AccountID, out.Result.PIALID)

	// Backup codes are mandatory. Gate every login until codes are generated.
	if out.Result.NeedsBackupCodes {
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
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("[kyc] risk signal answer for %s unreadable: %v", pialID, err)
		return
	}
	if result.RiskScore < 0.8 {
		return
	}
	// High risk: the age verification is withdrawn until the person verifies
	// again. That is an identity fact, so elohim-veni records it. If it does
	// not, the account keeps a verification this pipeline decided it should
	// not have, so the log must not claim the flag was applied.
	unverified := false
	if err := h.identity.Client().PutFacts(context.Background(), pialID, identity.FactsUpdate{
		AgeVerified: &unverified, Source: "risk_signal",
	}); err != nil {
		log.Printf("[kyc] high risk login (%.2f) for %s but the re-verify flag FAILED to apply: %v",
			result.RiskScore, pialID, err)
		return
	}
	log.Printf("[kyc] high risk login (%.2f) — flagged %s for re-verify", result.RiskScore, pialID)
}

func (h *Handler) deactivatedPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "deactivated.html", map[string]interface{}{
		"Title": "Account Deactivated · F33D3R",
	})
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if tok := GetSessionToken(r); tok != "" && len(tok) < 200 {
		h.sessionCache.Delete(tok)
		// The cookie is cleared whatever the authority answers: this device is
		// signed out either way. A session the authority could not be told to
		// end is said in the log, because it stays usable until it expires.
		if err := h.identity.Client().RevokeSession(r.Context(), tok); err != nil && !errors.Is(err, identity.ErrSessionInvalid) {
			log.Printf("[auth] logout could not end the session at elohim-veni: %v", err)
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
	// Already signed in to a real account → go to feed
	if viewerAccountID(h.userFromRequest(w, r)) != "" {
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

	// One writer of new accounts: the same sequence the JSON signup runs.
	// Refusals come back as the sentence the person should read.
	_, token, aerr := h.createAccount(r, accountParams{
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
	h.openedSession(w, token)
	// Gate: a new account has no backup codes, and must generate them before
	// reaching the feed.
	hxAwareRedirect(w, r, "/backup-codes/setup")
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
