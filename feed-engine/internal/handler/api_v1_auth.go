package handler

import (
	"log"
	"net/http"
	"strings"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/realm"
)

// ── /api/v1/auth — the JSON form of /login, /onboard and /logout ─────────────
//
// Same rules as the HTML forms, same rows written, same session minted. What
// differs is only the answer: a SessionDTO instead of a redirect, an error
// envelope instead of a re-rendered form. The credential checks, the brute
// force accounting, the device registration and the risk signal are the ones
// handleLogin runs; account creation is createAccount, the single writer both
// handleOnboard and apiV1Signup call.

type apiLoginRequest struct {
	Handle     string `json:"handle"`
	Password   string `json:"password"`
	DeviceName string `json:"device_name"`
	// Code is the second factor for an account that has TOTP enabled: the
	// six-digit code, or one of the account's single-use backup codes.
	Code string `json:"code"`
}

// apiV1Login — POST /api/v1/auth/login.
//
// One error for an unknown handle and a wrong password, so the endpoint
// cannot be used to discover which handles exist.
func (h *Handler) apiV1Login(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		apiError(w, http.StatusServiceUnavailable, "db_unavailable", "F33D3R is having trouble right now. Try again in a moment.")
		return
	}
	var req apiLoginRequest
	if err := apiReadJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, "bad_request", "The request could not be read.")
		return
	}
	handle := strings.TrimSpace(strings.ToLower(strings.TrimPrefix(req.Handle, "@")))
	ip := requestIP(r)
	invalid := func(reason, severity, userID string) {
		go LogSecurityEvent(h.db, "login_failed", severity, userID, ip, r.UserAgent(), "/api/v1/auth/login",
			map[string]interface{}{"handle": handle, "reason": reason})
		go CheckBruteForce(h.db, ip)
		if userID != "" {
			go CheckBruteForceHandle(h.db, handle, ip)
		}
		apiError(w, http.StatusUnauthorized, "invalid_credentials", "Invalid handle or password.")
	}
	if !handleRe.MatchString(handle) || req.Password == "" {
		apiError(w, http.StatusUnauthorized, "invalid_credentials", "Invalid handle or password.")
		return
	}
	taken, _ := dbpkg.IsHandleTaken(h.db, handle)
	if !taken {
		invalid("unknown_handle", "low", "")
		return
	}
	user, _ := dbpkg.GetUserByHandle(h.db, handle)
	if user == nil {
		invalid("user_missing", "low", "")
		return
	}
	// An account that has never set a password cannot be entered from a device
	// that cannot show the set-password form: the web login is where that
	// happens, and a guessable "first password wins" rule here would let any
	// caller who knows a passwordless handle claim it.
	if !dbpkg.HasPassword(h.db, user.ID) {
		apiError(w, http.StatusForbidden, "password_not_set", "This account has no password yet. Sign in on the web once to set one.")
		return
	}
	if userID, err := dbpkg.CheckPassword(h.db, handle, req.Password); err != nil || userID == "" {
		invalid("bad_password", "medium", user.ID)
		return
	}
	// The second factor, checked only after the password is right: asking for
	// a code before that would tell a stranger which handles have 2FA on.
	if secret, twoFA := dbpkg.GetTOTPSecret(h.db, user.ID); twoFA {
		code := strings.ReplaceAll(strings.TrimSpace(req.Code), " ", "")
		if code == "" {
			apiError(w, http.StatusUnauthorized, "two_fa_required", "Enter the code from your authenticator app.")
			return
		}
		if !totpVerify(secret, code) {
			// A backup code is the other way in, and it is spent by using it.
			ok, err := dbpkg.VerifyAndConsumeBackupCode(h.db, user.ID, code)
			if err != nil {
				apiServerError(w, err)
				return
			}
			if !ok {
				go LogSecurityEvent(h.db, "login_2fa_failed", "medium", user.ID, ip, r.UserAgent(), "/api/v1/auth/login",
					map[string]interface{}{"handle": handle})
				apiError(w, http.StatusUnauthorized, "two_fa_invalid", "That code is not right. Try again.")
				return
			}
		}
	}
	if dbpkg.IsDeactivated(h.db, user.ID) {
		// Reactivation on login, as the web does.
		h.db.Exec(`UPDATE users SET deactivated_at = NULL, deactivated_reason = '' WHERE id = $1`, user.ID)
	}
	if user.PIALID != "" {
		caps := dbpkg.GetCapabilities(h.db, user.PIALID)
		if !caps.Can(model.CapPosting) && !caps.Can(model.CapMessaging) {
			apiError(w, http.StatusForbidden, "account_suspended", "This account has been suspended. Contact support if you believe this is an error.")
			return
		}
	}
	h.apiIssueSession(w, r, user, http.StatusOK, req.DeviceName)
}

type apiSignupRequest struct {
	Handle         string `json:"handle"`
	Password       string `json:"password"`
	DisplayName    string `json:"display_name"`
	DeviceName     string `json:"device_name"`
	DateOfBirth    string `json:"date_of_birth"` // YYYY-MM-DD
	RoleType       string `json:"role_type"`     // user | creator | official; default user
	CreatorType    string `json:"creator_type"`  // creator sub-type; only read when role_type is creator
	ContentSetting string `json:"content_setting"`
	TermsAccepted  bool   `json:"terms_accepted"`
}

// apiV1Signup — POST /api/v1/auth/signup.
//
// Handle and password make an account — F33D3R takes no email or phone — but
// the date of birth is not optional on an adult platform: every age gate reads
// it from the identity plane, so an account without one cannot exist.
func (h *Handler) apiV1Signup(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		apiError(w, http.StatusServiceUnavailable, "db_unavailable", "F33D3R is having trouble right now. Try again in a moment.")
		return
	}
	var req apiSignupRequest
	if err := apiReadJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, "bad_request", "The request could not be read.")
		return
	}
	user, token, aerr := h.createAccount(r, accountParams{
		Handle:          strings.TrimPrefix(req.Handle, "@"),
		Password:        req.Password,
		ConfirmPassword: req.Password,
		DisplayName:     req.DisplayName,
		DateOfBirth:     req.DateOfBirth,
		RoleType:        req.RoleType,
		CreatorType:     req.CreatorType,
		ContentSetting:  req.ContentSetting,
		TermsAccepted:   req.TermsAccepted,
		DeviceName:      apiDeviceName(r, req.DeviceName),
	})
	if aerr != nil {
		apiError(w, aerr.Status, aerr.Code, aerr.Message)
		return
	}
	h.apiWriteSession(w, r, user, token, http.StatusCreated)
}

// apiIssueSession mints a session for an authenticated login and answers
// with it, running the same post-login accounting the web form does.
func (h *Handler) apiIssueSession(w http.ResponseWriter, r *http.Request, user *model.User, status int, deviceName string) {
	deviceName = apiDeviceName(r, deviceName)
	token, err := dbpkg.CreateAuthSession(h.db, user.ID, deviceName, r.RemoteAddr)
	if err != nil {
		apiServerError(w, err)
		return
	}
	ip := requestIP(r)
	go RegisterDevice(h.db, user.ID, r.UserAgent(), ip)
	go func() {
		if !dbpkg.IsKnownDevice(h.db, user.ID, deviceName, ip) {
			h.sendNewDeviceAlert(user, r)
		}
	}()
	go h.loginRiskSignal(user.PIALID, r)
	h.apiWriteSession(w, r, user, token, status)
}

// apiWriteSession answers with the SessionDTO for a freshly minted token. The
// user is re-resolved through the session so the DTO carries what every later
// /me will carry — PIAL standing folded on, not the bare row.
func (h *Handler) apiWriteSession(w http.ResponseWriter, r *http.Request, user *model.User, token string, status int) {
	r.Header.Set("Authorization", "Bearer "+token)
	h.sessionCache.Delete(token)
	if resolved := h.userFromRequest(w, r); viewerAccountID(resolved) != "" {
		user = resolved
	}
	expires := dbpkg.GetSessionExpiry(h.db, token)
	if expires.IsZero() {
		expires = time.Now().Add(30 * 24 * time.Hour)
	}
	apiJSON(w, status, SessionDTO{
		Token:            token,
		ExpiresAt:        expires,
		User:             h.meDTOFor(user),
		NeedsBackupCodes: !dbpkg.HasUnusedBackupCodes(h.db, user.ID),
	})
}

// apiDeviceName is the device label stored on the session: what the client
// said, or its user agent, bounded the way the web bounds it.
func apiDeviceName(r *http.Request, given string) string {
	name := strings.TrimSpace(given)
	if name == "" {
		name = r.UserAgent()
	}
	if len(name) > 100 {
		name = name[:100]
	}
	return name
}

// meDTOFor assembles the owner's view: the resolved user plus the standing
// that lives off the User model — the unread count, whether 2FA and a password
// are set, and when the identity tier on their PIAL root was recorded.
func (h *Handler) meDTOFor(u *model.User) MeDTO {
	_, twoFA := dbpkg.GetTOTPSecret(h.db, u.ID)
	return meDTO(u, meStanding{
		Unread:         dbpkg.CountUnreadNotifications(h.db, u.ID),
		TwoFA:          twoFA,
		HasPassword:    dbpkg.HasPassword(h.db, u.ID),
		KYCSubmittedAt: dbpkg.GetPIALKYCVerifiedAt(h.db, u.PIALID),
	})
}

// apiV1Logout — POST /api/v1/auth/logout. Revokes the presented token; 204.
func (h *Handler) apiV1Logout(w http.ResponseWriter, r *http.Request, _ *model.User) {
	tok := GetSessionToken(r)
	if tok != "" && !strings.HasPrefix(tok, "__handle__") {
		h.sessionCache.Delete(tok)
		if err := dbpkg.DeleteSession(h.db, tok); err != nil {
			apiServerError(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// apiV1Me — GET /api/v1/me.
func (h *Handler) apiV1Me(w http.ResponseWriter, r *http.Request, u *model.User) {
	apiJSON(w, http.StatusOK, h.meDTOFor(u))
}

// ── Account creation ──────────────────────────────────────────────────────────

// accountParams is what a signup supplies, whichever form it arrived on.
type accountParams struct {
	Handle          string
	Password        string
	ConfirmPassword string
	DisplayName     string
	ThemeID         string
	DateOfBirth     string // YYYY-MM-DD
	RoleType        string // user | creator | official
	CreatorType     string
	ContentSetting  string // safe_mode | default | adult_enabled
	TermsAccepted   bool
	DeviceName      string
}

// accountError is a refusal the caller can show: the HTML form renders
// Message, the JSON form sends Code and Status with it.
type accountError struct {
	Status  int
	Code    string
	Message string
}

func (e *accountError) Error() string { return e.Message }

func accountRefused(status int, code, msg string) *accountError {
	return &accountError{Status: status, Code: code, Message: msg}
}

// createAccount is the one writer of a new account. It validates the supplied
// facts, mints the identity, allocates the handle with the authority, writes
// the profile and the age record, and only then issues a session — in that
// order, deliberately: every age gate on this platform reads the PIAL root, so
// an account must not be able to browse before its date of birth is there.
//
// On success it returns the account as re-read from the database and the raw
// session token. On refusal it returns an *accountError with the message the
// person should see; the HTML and JSON handlers differ only in how they show
// it.
func (h *Handler) createAccount(r *http.Request, p accountParams) (*model.User, string, *accountError) {
	if !p.TermsAccepted {
		return nil, "", accountRefused(http.StatusBadRequest, "terms_required", "You must accept the Terms of Service to continue.")
	}
	handle := strings.TrimSpace(strings.ToLower(p.Handle))
	if !handleRe.MatchString(handle) {
		return nil, "", accountRefused(http.StatusBadRequest, "invalid_handle", "Handle must be 1–30 characters, letters/numbers/underscore only.")
	}
	if reservedHandle(handle) {
		return nil, "", accountRefused(http.StatusBadRequest, "handle_reserved", "That handle is reserved. Try another.")
	}
	if len(p.Password) < 8 {
		return nil, "", accountRefused(http.StatusBadRequest, "weak_password", "Password must be at least 8 characters.")
	}
	if p.Password != p.ConfirmPassword {
		return nil, "", accountRefused(http.StatusBadRequest, "password_mismatch", "Passwords don't match.")
	}

	// Date of birth — required, server-side age gate.
	dobStr := strings.TrimSpace(p.DateOfBirth)
	if dobStr == "" {
		return nil, "", accountRefused(http.StatusBadRequest, "date_of_birth_required", "Date of birth is required.")
	}
	dob, err := time.Parse("2006-01-02", dobStr)
	if err != nil {
		return nil, "", accountRefused(http.StatusBadRequest, "invalid_date_of_birth", "Invalid date of birth. Use YYYY-MM-DD format.")
	}
	age := ageFromDOB(dob)
	if age < 16 {
		return nil, "", accountRefused(http.StatusBadRequest, "too_young", "You must be at least 16 years old to join F33D3R.")
	}
	isMinor := age < 18
	isAdult := age >= 18

	roleType := p.RoleType
	allowedRoles := map[string]bool{"user": true, "creator": true, "official": true}
	if !allowedRoles[roleType] {
		roleType = "user"
	}
	creatorType := p.CreatorType

	contentSetting := p.ContentSetting
	allowedContent := map[string]bool{"safe_mode": true, "default": true, "adult_enabled": true}
	if !allowedContent[contentSetting] {
		contentSetting = "default"
	}
	// Minors may not enable adult content regardless of what was submitted.
	if isMinor && contentSetting == "adult_enabled" {
		contentSetting = "default"
	}

	if h.db == nil {
		return nil, "", accountRefused(http.StatusServiceUnavailable, "db_unavailable", "Could not create your account — try again.")
	}

	taken, _ := dbpkg.IsHandleTaken(h.db, handle)
	if taken {
		return nil, "", accountRefused(http.StatusConflict, "handle_taken", "That handle is already taken. Try another.")
	}

	// The identity comes first, because the handle is allocated TO one. A
	// PIAL with no account attached yet is harmless; a handle granted to
	// nothing is not.
	pialID, perr := dbpkg.CreatePIAL(h.db)
	if perr != nil {
		log.Printf("[onboard] CreatePIAL error for %s: %v", handle, perr)
		return nil, "", accountRefused(http.StatusInternalServerError, "server_error", "Could not create your identity — try again.")
	}

	// The allocation itself. registry-brain decides whether this handle is
	// this identity's, and the local row is written only after it says yes
	// — the moment this brain writes users.handle on its own authority is
	// the moment there are two answers to who owns a handle.
	if aerr := h.allocateHandle(r, handle, pialID); aerr != nil {
		return nil, "", accountRefused(http.StatusConflict, "handle_unavailable", aerr.Error())
	}

	id, err := dbpkg.UpsertUserByHandle(h.db, handle)
	if err != nil {
		log.Printf("[onboard] upsert error: %v", err)
	}
	displayName := strings.TrimSpace(p.DisplayName)
	if displayName == "" {
		displayName = handle
	}
	themeID := p.ThemeID
	if !isValidTheme(themeID) {
		themeID = "void"
	}
	if err := dbpkg.SetPassword(h.db, id, p.Password); err != nil {
		log.Printf("[onboard] SetPassword error: %v", err)
		return nil, "", accountRefused(http.StatusInternalServerError, "server_error", "Could not save password — try again.")
	}
	go realm.AwardXP(h.db, id, "onboard", "", realm.XPOnboard)
	// This is the INSERT that brings user_profiles into existence. Every later
	// write below — the adult-creator flag, the content preference — is an
	// UPDATE against the row it creates, so a failure here is not cosmetic: it
	// silently turns those into updates of nothing.
	if serr := dbpkg.SaveProfile(h.db, &model.ProfileSave{
		UserID:      id,
		DisplayName: truncate(displayName, 100),
		ThemeID:     themeID,
	}); serr != nil {
		log.Printf("[onboard] SaveProfile error for %s: %v", handle, serr)
		return nil, "", accountRefused(http.StatusInternalServerError, "server_error", "Could not save your profile — try again.")
	}
	if dbpkg.AttachPIAL(h.db, pialID, id, handle) == "" {
		log.Printf("[onboard] PIAL attach failed for %s — account exists without identity", handle)
	}
	if founder, err := dbpkg.GetUserByHandle(h.db, "tehanibentley"); err == nil && founder != nil && founder.ID != id {
		if _, ferr := dbpkg.FollowUser(h.db, id, founder.ID); ferr != nil {
			log.Printf("[onboard] founder auto-follow failed for %s: %v", handle, ferr)
		}
	}

	// Complete onboarding, then hand out the session — in that order.
	u, uerr := dbpkg.GetUserByHandle(h.db, handle)
	if uerr != nil || u == nil {
		log.Printf("[onboard] cannot re-read account %s after creation: %v", handle, uerr)
		return nil, "", accountRefused(http.StatusInternalServerError, "server_error", "Could not complete your signup — try again.")
	}

	// creator_type + onboarding progress — the user_roles columns that stay
	// local to this brain. role / is_adult / is_minor live on PIAL and are
	// written below; UpsertUserRole deliberately ignores them.
	if rerr := dbpkg.UpsertUserRole(h.db, &dbpkg.UserRole{
		UserID:      u.ID,
		RoleType:    roleType,
		CreatorType: creatorType,
		IsAdult:     isAdult,
		IsMinor:     isMinor,
		OnboardStep: 5,
		OnboardDone: true,
	}); rerr != nil {
		log.Printf("[onboard] UpsertUserRole error for %s: %v", handle, rerr)
		return nil, "", accountRefused(http.StatusInternalServerError, "server_error", "Could not save your account type — try again.")
	}

	// A declared creator is a creator. Without this the choice made on the
	// signup form lands nowhere any creator surface reads.
	if roleType == "creator" {
		if cerr := dbpkg.SetCreatorMode(h.db, u.ID, true); cerr != nil {
			log.Printf("[onboard] SetCreatorMode error for %s: %v", handle, cerr)
			return nil, "", accountRefused(http.StatusInternalServerError, "server_error", "Could not save your account type — try again.")
		}
	}

	// The date of birth is recorded twice on purpose, and this order matters.
	// user_roles.date_of_birth is the audit trail of what the person actually
	// supplied; pial_roots is the identity plane the gates read. Writing the
	// audit trail first means that if the identity write fails, the supplied
	// value still survives and the reconciler can finish the job — nothing
	// has to be re-asked, and nothing may ever be invented.
	if _, derr := h.db.Exec(
		`UPDATE user_roles SET date_of_birth = $1 WHERE user_id = $2`,
		dob.Format("2006-01-02"), u.ID,
	); derr != nil {
		log.Printf("[onboard] date of birth audit write failed for %s: %v", handle, derr)
		return nil, "", accountRefused(http.StatusInternalServerError, "server_error", "Could not record your date of birth — try again.")
	}

	if berr := dbpkg.SetPIALBirthday(h.db, u.PIALID, dob); berr != nil {
		// Loud, specific, and terminal. This is the compliance record for an
		// adult platform: it is never allowed to fail quietly, and the account
		// is never allowed to proceed with age facts the gates would read wrong.
		log.Printf("[onboard] CRITICAL: date of birth not recorded on PIAL %q for %s (account %s): %v "+
			"— no session issued; every age gate would treat this account as non-adult",
			u.PIALID, handle, u.ID, berr)
		dbpkg.LogPIALEvent(h.db, u.PIALID, u.ID, "age_record_failed", map[string]interface{}{
			"handle": handle,
			"reason": berr.Error(),
		}, "onboard")
		return nil, "", accountRefused(http.StatusInternalServerError, "age_record_failed",
			"We could not record your date of birth, so your account was not completed. "+
				"Please try again in a moment — if it keeps happening, contact support.")
	}

	// Adult Content Creator → flag all their posts for age-gating.
	if creatorType == "adult_content_creator" {
		if _, aerr := h.db.Exec(
			`UPDATE user_profiles SET is_adult_creator = TRUE WHERE user_id = $1`,
			u.ID,
		); aerr != nil {
			log.Printf("[onboard] adult-creator flag failed for %s: %v", handle, aerr)
			return nil, "", accountRefused(http.StatusInternalServerError, "server_error", "Could not save your creator type — try again.")
		}
	}

	if _, cerr := h.db.Exec(
		`UPDATE user_profiles SET content_setting = $1 WHERE user_id = $2`,
		contentSetting, u.ID,
	); cerr != nil {
		log.Printf("[onboard] content setting write failed for %s: %v", handle, cerr)
		return nil, "", accountRefused(http.StatusInternalServerError, "server_error", "Could not save your content preference — try again.")
	}

	// The account is complete and its age is on record. Only now does it get
	// a way in.
	deviceName := p.DeviceName
	if deviceName == "" {
		deviceName = r.UserAgent()
	}
	if len(deviceName) > 100 {
		deviceName = deviceName[:100]
	}
	token, serr := dbpkg.CreateAuthSession(h.db, u.ID, deviceName, r.RemoteAddr)
	if serr != nil {
		log.Printf("[onboard] CreateAuthSession error for %s: %v", handle, serr)
		return nil, "", accountRefused(http.StatusInternalServerError, "session_failed", "Your account was created but we could not sign you in — please sign in.")
	}
	return u, token, nil
}
