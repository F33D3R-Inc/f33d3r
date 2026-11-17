package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/identity"
	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/realm"
)

// ── /api/v1/auth — the JSON form of /login, /onboard and /logout ─────────────
//
// Credentials, sessions and accounts are elohim-veni's. These handlers carry a
// person's request to it and answer with what it decided: a SessionDTO instead
// of a redirect, an error envelope instead of a re-rendered form. The web forms
// in onboard.go call the same signIn and createAccount.

type apiLoginRequest struct {
	Handle     string `json:"handle"`
	Password   string `json:"password"`
	DeviceName string `json:"device_name"`
	// Code is the second factor for an account that has TOTP enabled: the
	// six-digit code, or one of the account's single-use backup codes.
	Code string `json:"code"`
}

// signInOutcome is what a sign-in attempt came to, in the terms both the form
// and the JSON endpoint render.
type signInOutcome struct {
	Result *identity.LoginResult
	// Exactly one of the following is set when Result is nil.
	NeedsCode   bool // the account has a second factor and none was given
	BadCode     bool // a second factor was given and was wrong
	Invalid     bool // unknown handle or wrong password — never told apart
	Suspended   bool
	NoPassword  bool // the account has never set a password
	Unavailable bool // the authority could not be asked
}

// signIn asks elohim-veni to open a session. code is the second factor when
// the person gave one: six digits are an authenticator code, anything else a
// backup code. Every failure is accounted for here, once, for both surfaces.
func (h *Handler) signIn(r *http.Request, handle, password, code, deviceName, surface string) signInOutcome {
	ip := requestIP(r)
	req := identity.LoginRequest{Handle: handle, Password: password, DeviceName: deviceName, IP: ip}
	if code = strings.ReplaceAll(strings.TrimSpace(code), " ", ""); code != "" {
		if isTOTPCode(code) {
			req.TOTP = code
		} else {
			req.BackupCode = code
		}
	}
	res, err := h.identity.Client().Login(r.Context(), req)
	switch {
	case err == nil:
		return signInOutcome{Result: res}
	case errors.Is(err, identity.ErrInvalidCredentials):
		go LogSecurityEvent(h.db, "login_failed", "medium", "", ip, r.UserAgent(), surface,
			map[string]interface{}{"handle": handle, "reason": "invalid_credentials"})
		go CheckBruteForce(h.db, ip)
		go CheckBruteForceHandle(h.db, handle, ip)
		return signInOutcome{Invalid: true}
	case errors.Is(err, identity.ErrTOTPRequired):
		return signInOutcome{NeedsCode: true}
	case errors.Is(err, identity.ErrTOTPInvalid), errors.Is(err, identity.ErrBackupCodeInvalid):
		go LogSecurityEvent(h.db, "login_2fa_failed", "medium", "", ip, r.UserAgent(), surface,
			map[string]interface{}{"handle": handle})
		return signInOutcome{BadCode: true}
	case errors.Is(err, identity.ErrSuspended):
		return signInOutcome{Suspended: true}
	case errors.Is(err, identity.ErrPasswordNotSet):
		return signInOutcome{NoPassword: true}
	default:
		log.Printf("[login] @%s: %v", handle, err)
		return signInOutcome{Unavailable: true}
	}
}

func isTOTPCode(code string) bool {
	if len(code) != 6 {
		return false
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// afterSignIn runs this brain's own accounting of a sign-in: the device
// record and the new-device alert. None of it gates the
// sign-in, which the authority has already granted.
func (h *Handler) afterSignIn(r *http.Request, accountID string) {
	ip := requestIP(r)
	ua := r.UserAgent()
	go func() {
		isNew, err := RegisterDevice(h.db, accountID, ua, ip)
		if err != nil {
			log.Printf("[login] %v", err)
			return
		}
		if !isNew {
			return
		}
		u, err := dbpkg.GetUserByID(h.db, accountID)
		if err != nil || u == nil {
			log.Printf("[login] new-device alert for %s: account unreadable: %v", accountID, err)
			return
		}
		h.sendNewDeviceAlert(u, r)
	}()
}

// apiV1Login — POST /api/v1/auth/login.
//
// One error for an unknown handle and a wrong password, so the endpoint
// cannot be used to discover which handles exist.
func (h *Handler) apiV1Login(w http.ResponseWriter, r *http.Request) {
	var req apiLoginRequest
	if err := apiReadJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, "bad_request", "The request could not be read.")
		return
	}
	handle := strings.TrimSpace(strings.ToLower(strings.TrimPrefix(req.Handle, "@")))
	if !handleRe.MatchString(handle) || req.Password == "" {
		apiError(w, http.StatusUnauthorized, "invalid_credentials", "Invalid handle or password.")
		return
	}
	out := h.signIn(r, handle, req.Password, req.Code, apiDeviceName(r, req.DeviceName), "/api/v1/auth/login")
	switch {
	case out.Result != nil:
		h.afterSignIn(r, out.Result.AccountID)
		h.apiWriteSession(w, r, out.Result.SessionToken, http.StatusOK, out.Result.NeedsBackupCodes)
	case out.NeedsCode:
		apiError(w, http.StatusUnauthorized, "two_fa_required", "Enter the code from your authenticator app.")
	case out.BadCode:
		apiError(w, http.StatusUnauthorized, "two_fa_invalid", "That code is not right. Try again.")
	case out.Suspended:
		apiError(w, http.StatusForbidden, "account_suspended", "This account has been suspended. Contact support if you believe this is an error.")
	case out.NoPassword:
		apiError(w, http.StatusForbidden, "password_not_set", "This account has no password. Recover it with a backup code.")
	case out.Unavailable:
		apiError(w, http.StatusServiceUnavailable, "identity_unavailable", "Sign-in is briefly unavailable. Try again in a moment.")
	default:
		apiError(w, http.StatusUnauthorized, "invalid_credentials", "Invalid handle or password.")
	}
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
// it, so an account without one cannot exist.
func (h *Handler) apiV1Signup(w http.ResponseWriter, r *http.Request) {
	var req apiSignupRequest
	if err := apiReadJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, "bad_request", "The request could not be read.")
		return
	}
	_, token, aerr := h.createAccount(r, accountParams{
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
	// A new account has no recovery codes yet.
	h.apiWriteSession(w, r, token, http.StatusCreated, true)
}

// apiWriteSession answers with the SessionDTO for a session the authority has
// just opened. The account is resolved through the session, so the DTO carries
// what every later /me will carry.
func (h *Handler) apiWriteSession(w http.ResponseWriter, r *http.Request, token string, status int, needsBackupCodes bool) {
	h.sessionCache.Delete(token)
	user, err := h.sessionUser(r.Context(), token)
	if err != nil {
		// The session exists at the authority; this brain could not confirm it
		// yet. The client signs in again rather than holding a token it was
		// never shown to be good.
		log.Printf("[auth] confirming a just-opened session: %v", err)
		apiError(w, http.StatusServiceUnavailable, "identity_unavailable", "Signed in, but your session could not be confirmed yet. Try again in a moment.")
		return
	}
	me, err := h.meDTOFor(r.Context(), user)
	if err != nil {
		apiError(w, http.StatusServiceUnavailable, "identity_unavailable", "Your account could not be read right now. Try again in a moment.")
		return
	}
	apiJSON(w, status, SessionDTO{
		Token:            token,
		ExpiresAt:        user.SessionExpiresAt,
		User:             me,
		NeedsBackupCodes: needsBackupCodes,
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
// that lives off the User model — the unread count from Herald, whether 2FA
// and a password are set from elohim-veni, and when the identity tier was
// recorded from the projection.
func (h *Handler) meDTOFor(ctx context.Context, u *model.User) (MeDTO, error) {
	acct, err := h.identity.Client().GetAccount(ctx, u.ID)
	if err != nil {
		log.Printf("[me] account standing for @%s: %v", u.Handle, err)
		return MeDTO{}, err
	}
	return meDTO(u, meStanding{
		Unread:         dbpkg.CountUnreadNotifications(h.db, u.ID),
		TwoFA:          acct.TOTPEnabled,
		HasPassword:    acct.HasPassword,
		KYCSubmittedAt: dbpkg.GetPIALKYCVerifiedAt(h.db, u.PIALID),
	}), nil
}

// apiMe answers with the account's own view, or 503 when its standing cannot
// be read.
func (h *Handler) apiMe(w http.ResponseWriter, r *http.Request, u *model.User) {
	me, err := h.meDTOFor(r.Context(), u)
	if err != nil {
		apiError(w, http.StatusServiceUnavailable, "identity_unavailable", "Your account could not be read right now. Try again in a moment.")
		return
	}
	apiJSON(w, http.StatusOK, me)
}

// apiV1Logout — POST /api/v1/auth/logout. Revokes the presented token; 204.
func (h *Handler) apiV1Logout(w http.ResponseWriter, r *http.Request, _ *model.User) {
	tok := GetSessionToken(r)
	if tok != "" {
		h.sessionCache.Delete(tok)
		if err := h.identity.Client().RevokeSession(r.Context(), tok); err != nil && !errors.Is(err, identity.ErrSessionInvalid) {
			log.Printf("[auth] logout: %v", err)
			apiError(w, http.StatusServiceUnavailable, "identity_unavailable", "Could not sign out right now. Try again in a moment.")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// apiV1Me — GET /api/v1/me.
func (h *Handler) apiV1Me(w http.ResponseWriter, r *http.Request, u *model.User) {
	h.apiMe(w, r, u)
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

// identityRefusal renders an authority's refusal of an account write as the
// sentence a person should read.
func identityRefusal(err error) *accountError {
	switch {
	case errors.Is(err, identity.ErrHandleTaken):
		return accountRefused(http.StatusConflict, "handle_taken", "That handle is already taken. Try another.")
	case errors.Is(err, identity.ErrHandleReserved):
		return accountRefused(http.StatusConflict, "handle_reserved", "That handle is reserved. Try another.")
	case errors.Is(err, identity.ErrHandleInvalid):
		return accountRefused(http.StatusBadRequest, "invalid_handle", "Handle must be 1–30 characters, letters/numbers/underscore only.")
	case errors.Is(err, identity.ErrWeakPassword):
		return accountRefused(http.StatusBadRequest, "weak_password", "Password must be at least 8 characters.")
	case errors.Is(err, identity.ErrTooYoung):
		return accountRefused(http.StatusBadRequest, "too_young", "You must be at least 16 years old to join F33D3R.")
	case errors.Is(err, identity.ErrInvalidDOB):
		return accountRefused(http.StatusBadRequest, "invalid_date_of_birth", "Invalid date of birth. Use YYYY-MM-DD format.")
	case identity.IsUnavailable(err):
		return accountRefused(http.StatusServiceUnavailable, "identity_unavailable", "Account creation is briefly unavailable — please try again in a moment.")
	}
	if e := identity.Refusal(err); e != nil && e.Message != "" {
		return accountRefused(e.Status, e.Code, e.Message)
	}
	return accountRefused(http.StatusInternalServerError, "server_error", "Could not create your account — try again.")
}

// createAccount is the one path that creates an account. elohim-veni creates
// the identity, the account, the handle, the credential, the age record and the
// session — and decides whether this is the platform's first account, the
// founder. This brain then sets up what it owns: the account projection, the
// profile and theme, the creator and onboarding record, and the content
// preference, keyed by the account id the authority returned.
//
// On success it returns the account and the raw session token. On refusal it
// returns an *accountError with the message the person should see.
func (h *Handler) createAccount(r *http.Request, p accountParams) (*model.User, string, *accountError) {
	if !p.TermsAccepted {
		return nil, "", accountRefused(http.StatusBadRequest, "terms_required", "You must accept the Terms of Service to continue.")
	}
	handle := strings.TrimSpace(strings.ToLower(p.Handle))
	if !handleRe.MatchString(handle) {
		return nil, "", accountRefused(http.StatusBadRequest, "invalid_handle", "Handle must be 1–30 characters, letters/numbers/underscore only.")
	}
	// Handles that collide with this brain's own routes are this brain's rule.
	if reservedHandle(handle) {
		return nil, "", accountRefused(http.StatusBadRequest, "handle_reserved", "That handle is reserved. Try another.")
	}
	if len(p.Password) < 8 {
		return nil, "", accountRefused(http.StatusBadRequest, "weak_password", "Password must be at least 8 characters.")
	}
	if p.Password != p.ConfirmPassword {
		return nil, "", accountRefused(http.StatusBadRequest, "password_mismatch", "Passwords don't match.")
	}

	// Date of birth — required. The authority records and enforces it; it is
	// read here only to keep a minor's content preference safe.
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

	roleType := p.RoleType
	if !map[string]bool{"user": true, "creator": true, "official": true}[roleType] {
		roleType = "user"
	}
	creatorType := p.CreatorType
	contentSetting := p.ContentSetting
	if !map[string]bool{"safe_mode": true, "default": true, "adult_enabled": true}[contentSetting] {
		contentSetting = "default"
	}
	// Minors may not enable adult content regardless of what was submitted.
	if isMinor && contentSetting == "adult_enabled" {
		contentSetting = "default"
	}
	displayName := strings.TrimSpace(p.DisplayName)
	if displayName == "" {
		displayName = handle
	}
	deviceName := p.DeviceName
	if deviceName == "" {
		deviceName = r.UserAgent()
	}
	if len(deviceName) > 100 {
		deviceName = deviceName[:100]
	}

	created, err := h.identity.Client().CreateAccount(r.Context(), identity.CreateAccountRequest{
		Handle:         handle,
		Password:       p.Password,
		DisplayName:    truncate(displayName, 100),
		DOB:            dob.Format("2006-01-02"),
		RoleType:       roleType,
		CreatorType:    creatorType,
		ContentSetting: contentSetting,
		DeviceName:     deviceName,
		IP:             requestIP(r),
	})
	if err != nil {
		log.Printf("[onboard] @%s: %v", handle, err)
		return nil, "", identityRefusal(err)
	}
	if created.Founder {
		log.Printf("[onboard] @%s is the first account on the platform and holds the founder role", handle)
	}

	themeID := p.ThemeID
	if !isValidTheme(themeID) {
		themeID = "void"
	}
	if serr := h.setUpNewAccount(created, handle, dob, roleType, creatorType, contentSetting, displayName, themeID); serr != nil {
		// The account exists at the authority and cannot be created twice, so
		// this is not a refusal of the signup. What did not land is this
		// brain's part of it; the person is told plainly and signs in.
		log.Printf("[onboard] CRITICAL: account %s (@%s) created at elohim-veni but its feed-engine setup failed: %v",
			created.AccountID, handle, serr)
		if rerr := h.identity.Client().RevokeSession(r.Context(), created.SessionToken); rerr != nil {
			log.Printf("[onboard] ending the session of half-set-up account @%s: %v", handle, rerr)
		}
		return nil, "", accountRefused(http.StatusInternalServerError, "setup_incomplete",
			"Your account was created, but we could not finish setting it up. Sign in to continue.")
	}

	u, uerr := dbpkg.GetUserByID(h.db, created.AccountID)
	if uerr != nil || u == nil {
		log.Printf("[onboard] cannot re-read account %s after creation: %v", created.AccountID, uerr)
		return nil, "", accountRefused(http.StatusInternalServerError, "setup_incomplete",
			"Your account was created, but we could not finish setting it up. Sign in to continue.")
	}
	h.afterSignIn(r, created.AccountID)
	return u, created.SessionToken, nil
}

// setUpNewAccount writes what this brain owns about a new account: the
// projection rows its queries join on, then the profile, the onboarding and
// creator record, and the content preference. Every step's error is returned.
func (h *Handler) setUpNewAccount(c *identity.CreatedAccount, handle string, dob time.Time,
	roleType, creatorType, contentSetting, displayName, themeID string) error {
	age := ageFromDOB(dob)
	role := model.RoleUser
	if c.Founder {
		role = model.RoleFounder
	}
	// The projection as the signup answer established it. identity.events
	// confirms and corrects it; until then these are the facts the gates read.
	if err := dbpkg.SeedIdentity(h.db, dbpkg.IdentitySnapshot{
		PIALID: c.PIALID, AccountID: c.AccountID, Handle: handle, Role: role, Status: identity.StatusActive,
		Caps: model.DefaultCapabilities,
		Facts: dbpkg.IdentityFacts{
			AgeVerified: true, IsAdult: age >= 18, IsMinor: age < 18,
			KYCTier: model.KYCTierBasic, DateOfBirth: &dob,
		},
	}); err != nil {
		return err
	}
	// This is the INSERT that brings user_profiles into existence. Every later
	// write below is an UPDATE against the row it creates.
	if err := dbpkg.SaveProfile(h.db, &model.ProfileSave{
		UserID:      c.AccountID,
		DisplayName: truncate(displayName, 100),
		ThemeID:     themeID,
	}); err != nil {
		return err
	}
	go realm.AwardXP(h.db, c.AccountID, "onboard", "", realm.XPOnboard)
	if err := dbpkg.UpsertUserRole(h.db, &dbpkg.UserRole{
		UserID:      c.AccountID,
		RoleType:    roleType,
		CreatorType: creatorType,
		OnboardStep: 5,
		OnboardDone: true,
	}); err != nil {
		return err
	}
	// A declared creator is a creator. Without this the choice made on the
	// signup form lands nowhere any creator surface reads.
	if roleType == "creator" {
		if err := dbpkg.SetCreatorMode(h.db, c.AccountID, true); err != nil {
			return err
		}
	}
	// Adult Content Creator → every post of theirs is age-gated.
	if creatorType == "adult_content_creator" {
		if _, err := h.db.Exec(`UPDATE user_profiles SET is_adult_creator = TRUE WHERE user_id = $1`, c.AccountID); err != nil {
			return err
		}
	}
	_, err := h.db.Exec(`UPDATE user_profiles SET content_setting = $1 WHERE user_id = $2`, contentSetting, c.AccountID)
	return err
}
