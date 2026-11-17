// Package identity is feed-engine's client for elohim-veni, the one identity
// and authentication authority on the platform.
//
// elohim-veni owns every identity fact: the PIAL root, the accounts bound to
// it, handles, credentials, sessions, second factors, role, capabilities, age
// and KYC facts, and standing. feed-engine owns what a person publishes and how
// it is rendered, and keeps a read projection of identity (users.handle,
// users.pial_id, users.role, users.deactivated_at, identity_facts) so its SQL
// can join on it. Nothing in this brain decides an identity fact. It asks.
//
// Every route but the JWKS is service-to-service and carries X-Internal-Key.
// Every call has a deadline. Every deliberate refusal comes back typed (see
// errors.go); everything else is ErrUnavailable.
package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/f33d3r/feed-engine/internal/observ"
)

// Timeouts. Session resolution sits in front of every authenticated request,
// so it is held far below the generic bound: an authority that cannot resolve
// a session in a second and a half is an authority that is down, and the
// request fails closed rather than queueing behind it.
const (
	DefaultTimeout = 4 * time.Second
	ResolveTimeout = 1500 * time.Millisecond
	JWKSTimeout    = 3 * time.Second
)

// maxBody bounds what is read from any answer. The largest legitimate answer is
// a session list; a megabyte is far above it and far below harm.
const maxBody = 1 << 20

// Client talks to elohim-veni.
type Client struct {
	base    string
	key     string
	http    *http.Client
	timeout time.Duration
}

// New returns a client for the authority at baseURL. An empty baseURL or key is
// a configuration that cannot authenticate anyone; every call then fails with
// ErrUnavailable, which every caller already treats as fail-closed.
func New(baseURL, internalKey string) *Client {
	return &Client{
		base: strings.TrimRight(baseURL, "/"),
		key:  internalKey,
		// No client-wide timeout: each call carries its own deadline, so a
		// resolve and a password change are bounded differently.
		http:    observ.WrapClient(&http.Client{}),
		timeout: DefaultTimeout,
	}
}

// Configured reports whether the client has somewhere to send requests.
func (c *Client) Configured() bool { return c != nil && c.base != "" && c.key != "" }

// ── Contract types ───────────────────────────────────────────────────────────

// Facts are the identity facts every age and verification gate reads.
type Facts struct {
	AgeVerified bool   `json:"age_verified"`
	IsAdult     bool   `json:"is_adult"`
	IsMinor     bool   `json:"is_minor"`
	KYCTier     string `json:"kyc_tier"`
	// DOB is YYYY-MM-DD when the authority shares it, "" otherwise.
	DOB string `json:"dob,omitempty"`
	// KYCVerifiedAt is when the identity tier was recorded, nil when never.
	KYCVerifiedAt *time.Time `json:"kyc_verified_at,omitempty"`
}

// Birthday parses DOB. nil when absent or unreadable.
func (f Facts) Birthday() *time.Time {
	if f.DOB == "" {
		return nil
	}
	t, err := time.Parse("2006-01-02", f.DOB)
	if err != nil {
		return nil
	}
	return &t
}

// Account statuses.
const (
	StatusActive      = "active"
	StatusSuspended   = "suspended"
	StatusBanned      = "banned"
	StatusDeactivated = "deactivated"
	StatusPurged      = "purged"
)

// Session is a resolved session: who is behind a token right now.
type Session struct {
	AccountID   string   `json:"account_id"`
	PIALID      string   `json:"pial_id"`
	Handle      string   `json:"handle"`
	Role        string   `json:"role"`
	Caps        []string `json:"caps"`
	Facts       Facts    `json:"facts"`
	Status      string   `json:"status"`
	AccessToken string   `json:"access_token"`
	// SessionCreatedAt is when this session was opened.
	SessionCreatedAt time.Time `json:"session_created_at"`
	// SessionExpiresAt is when this session ends unless renewed.
	SessionExpiresAt time.Time `json:"session_expires_at"`
	// SessionID is filled from the verified access token's sid claim.
	SessionID string `json:"-"`
}

// CreateAccountRequest is a signup.
type CreateAccountRequest struct {
	Handle         string `json:"handle"`
	Password       string `json:"password"`
	DisplayName    string `json:"display_name,omitempty"`
	DOB            string `json:"dob"`
	RoleType       string `json:"role_type"`
	CreatorType    string `json:"creator_type"`
	ContentSetting string `json:"content_setting"`
	DeviceName     string `json:"device_name"`
	IP             string `json:"ip"`
}

// CreatedAccount is a signup that succeeded: the account, its identity, and a
// session already open on it.
type CreatedAccount struct {
	AccountID    string `json:"account_id"`
	PIALID       string `json:"pial_id"`
	Handle       string `json:"handle"`
	Founder      bool   `json:"founder"`
	SessionToken string `json:"session_token"`
	AccessToken  string `json:"access_token"`
}

// LoginRequest is a sign-in. TOTP or BackupCode is the second factor when the
// account has one.
type LoginRequest struct {
	Handle     string `json:"handle"`
	Password   string `json:"password"`
	TOTP       string `json:"totp,omitempty"`
	BackupCode string `json:"backup_code,omitempty"`
	DeviceName string `json:"device_name"`
	IP         string `json:"ip"`
}

// LoginResult is a sign-in that succeeded.
type LoginResult struct {
	AccountID        string `json:"account_id"`
	PIALID           string `json:"pial_id"`
	SessionToken     string `json:"session_token"`
	AccessToken      string `json:"access_token"`
	NeedsBackupCodes bool   `json:"needs_backup_codes"`
}

// SessionInfo is one open session, as the device list shows it. The field
// names are the ones the session_card Facet reads.
type SessionInfo struct {
	ID         string    `json:"id"`
	DeviceName string    `json:"device_name"`
	IPAddress  string    `json:"ip"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	IsCurrent  bool      `json:"-"`
}

// Account is one account as the authority holds it.
type Account struct {
	AccountID            string     `json:"account_id"`
	PIALID               string     `json:"pial_id"`
	Handle               string     `json:"handle"`
	Role                 string     `json:"role"`
	Status               string     `json:"status"`
	TOTPEnabled          bool       `json:"totp_enabled"`
	HasPassword          bool       `json:"has_password"`
	BackupCodesRemaining int        `json:"backup_codes_remaining"`
	CreatedAt            time.Time  `json:"created_at"`
	DeactivatedAt        *time.Time `json:"deactivated_at,omitempty"`
}

// PIAL is one identity root as the authority holds it.
type PIAL struct {
	PIALID     string   `json:"pial_id"`
	Role       string   `json:"role"`
	Status     string   `json:"status"`
	Caps       []string `json:"caps"`
	Facts      Facts    `json:"facts"`
	AccountIDs []string `json:"account_ids"`
}

// FactsUpdate is a change to identity facts. Only the fields set are sent; the
// authority derives is_adult and is_minor from the date of birth itself.
type FactsUpdate struct {
	KYCTier     *string `json:"kyc_tier,omitempty"`
	AgeVerified *bool   `json:"age_verified,omitempty"`
	DOB         *string `json:"dob,omitempty"`
	DOBLocked   *bool   `json:"dob_locked,omitempty"`
	// Source is the provenance recorded with the change: "ekyc:<attempt_id>"
	// (a verification Verity accepted), "admin:<handle>", "org_verification",
	// "risk_signal", "signup".
	Source string `json:"source"`
	// Reason is why the change was made, recorded in the identity ledger.
	// elohim-veni requires it when Source is "admin:<handle>": a person
	// overriding a verification says why.
	Reason string `json:"reason,omitempty"`
}

// CapabilityWrite sets one capability's state on an identity.
type CapabilityWrite struct {
	State     string     `json:"state"`
	Reason    string     `json:"reason"`
	By        string     `json:"by"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// TOTPEnrollment is the enrolment step: the otpauth:// URI a QR is drawn from
// and the same secret in the form a person can type.
type TOTPEnrollment struct {
	URI    string `json:"uri"`
	Secret string `json:"secret"`
}

// Recovery is a verified backup code exchanged for a short-lived right to set a
// new password.
type Recovery struct {
	RecoveryToken string `json:"recovery_token"`
	AccountID     string `json:"account_id"`
}

// AddedAccount is a further account opened on an existing identity.
type AddedAccount struct {
	AccountID string `json:"account_id"`
	PIALID    string `json:"pial_id"`
	Handle    string `json:"handle"`
}

// ── Accounts and sign-in ─────────────────────────────────────────────────────

// CreateAccount opens a new identity, its first account, and a session on it.
func (c *Client) CreateAccount(ctx context.Context, req CreateAccountRequest) (*CreatedAccount, error) {
	var out CreatedAccount
	if err := c.do(ctx, "create account", http.MethodPost, "/v1/accounts", req, &out, c.timeout); err != nil {
		return nil, err
	}
	if out.AccountID == "" || out.PIALID == "" || out.SessionToken == "" {
		return nil, fmt.Errorf("%w: create account answered without account, identity or session", ErrUnavailable)
	}
	return &out, nil
}

// AddAccount opens a further account on an identity that already exists. The
// new account shares the identity's facts, role and capabilities.
func (c *Client) AddAccount(ctx context.Context, pialID string, req CreateAccountRequest) (*AddedAccount, error) {
	var out AddedAccount
	if err := c.do(ctx, "add account", http.MethodPost, "/v1/pials/"+url.PathEscape(pialID)+"/accounts", req, &out, c.timeout); err != nil {
		return nil, err
	}
	if out.AccountID == "" {
		return nil, fmt.Errorf("%w: add account answered without an account", ErrUnavailable)
	}
	return &out, nil
}

// LinkAccount moves an independently created account onto pialID. Ownership is
// proved by one of the target account's backup codes, which the authority
// spends.
func (c *Client) LinkAccount(ctx context.Context, pialID, handle, backupCode string) (*AddedAccount, error) {
	var out AddedAccount
	body := map[string]string{"handle": handle, "backup_code": backupCode}
	if err := c.do(ctx, "link account", http.MethodPost, "/v1/pials/"+url.PathEscape(pialID)+"/accounts/link", body, &out, c.timeout); err != nil {
		return nil, err
	}
	if out.AccountID == "" {
		return nil, fmt.Errorf("%w: link account answered without an account", ErrUnavailable)
	}
	return &out, nil
}

// Login signs in with a handle and password and, when the account has one, a
// second factor.
func (c *Client) Login(ctx context.Context, req LoginRequest) (*LoginResult, error) {
	var out LoginResult
	if err := c.do(ctx, "login", http.MethodPost, "/v1/auth/login", req, &out, c.timeout); err != nil {
		return nil, err
	}
	if out.AccountID == "" || out.SessionToken == "" {
		return nil, fmt.Errorf("%w: login answered without account or session", ErrUnavailable)
	}
	return &out, nil
}

// GetAccount reads one account.
func (c *Client) GetAccount(ctx context.Context, accountID string) (*Account, error) {
	var out Account
	if err := c.do(ctx, "get account", http.MethodGet, "/v1/accounts/"+url.PathEscape(accountID), nil, &out, c.timeout); err != nil {
		return nil, err
	}
	return &out, nil
}

// AccountByHandle reads the account a handle names.
func (c *Client) AccountByHandle(ctx context.Context, handle string) (*Account, error) {
	var out Account
	if err := c.do(ctx, "account by handle", http.MethodGet, "/v1/accounts/by-handle/"+url.PathEscape(handle), nil, &out, c.timeout); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetPIAL reads one identity root.
func (c *Client) GetPIAL(ctx context.Context, pialID string) (*PIAL, error) {
	var out PIAL
	if err := c.do(ctx, "get pial", http.MethodGet, "/v1/pials/"+url.PathEscape(pialID), nil, &out, c.timeout); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeactivateAccount takes an account off the platform at its holder's request.
// The authority ends every session on it; signing in again reactivates it.
func (c *Client) DeactivateAccount(ctx context.Context, accountID, reason string) error {
	return c.do(ctx, "deactivate account", http.MethodPost, "/v1/accounts/"+url.PathEscape(accountID)+"/deactivate",
		map[string]string{"reason": reason}, nil, c.timeout)
}

// PurgeAccount erases an account at the authority and tombstones its identity
// when it was the identity's last account.
func (c *Client) PurgeAccount(ctx context.Context, accountID, by string) error {
	return c.do(ctx, "purge account", http.MethodPost, "/v1/accounts/"+url.PathEscape(accountID)+"/purge",
		map[string]string{"by": by}, nil, c.timeout)
}

// ── Sessions ─────────────────────────────────────────────────────────────────

// ResolveSession says who is behind a session token. ErrSessionInvalid means
// the token opens nothing; ErrUnavailable means nobody could say.
func (c *Client) ResolveSession(ctx context.Context, token string) (*Session, error) {
	var out Session
	err := c.do(ctx, "resolve session", http.MethodPost, "/v1/sessions/resolve",
		map[string]string{"session_token": token}, &out, ResolveTimeout)
	if err != nil {
		if e := Refusal(err); e != nil && e.Status == http.StatusUnauthorized {
			// A 401 here means exactly one thing whatever its body says.
			return nil, &Error{Status: e.Status, Code: ErrSessionInvalid.Code, Message: e.Message, Op: e.Op}
		}
		return nil, err
	}
	if out.AccountID == "" || out.PIALID == "" || out.AccessToken == "" {
		return nil, fmt.Errorf("%w: resolve answered without account, identity or access token", ErrUnavailable)
	}
	return &out, nil
}

// RevokeSession ends the session a token opens.
func (c *Client) RevokeSession(ctx context.Context, token string) error {
	return c.do(ctx, "revoke session", http.MethodPost, "/v1/sessions/revoke",
		map[string]string{"session_token": token}, nil, c.timeout)
}

// RevokeAccountSessions ends every session on an account except the one
// keepToken opens, when it names one.
func (c *Client) RevokeAccountSessions(ctx context.Context, accountID, keepToken string) error {
	body := map[string]interface{}{"account_id": accountID, "all": true}
	if keepToken != "" {
		body["keep_session_token"] = keepToken
	}
	return c.do(ctx, "revoke account sessions", http.MethodPost, "/v1/sessions/revoke", body, nil, c.timeout)
}

// SwitchSessionAccount moves a session onto another account of the same
// identity.
func (c *Client) SwitchSessionAccount(ctx context.Context, token, accountID string) error {
	return c.do(ctx, "switch session account", http.MethodPost, "/v1/sessions/switch",
		map[string]string{"session_token": token, "account_id": accountID}, nil, c.timeout)
}

// ListSessions lists the open sessions on an account, most recently used first.
func (c *Client) ListSessions(ctx context.Context, accountID string) ([]SessionInfo, error) {
	var out struct {
		Sessions []SessionInfo `json:"sessions"`
	}
	if err := c.do(ctx, "list sessions", http.MethodGet, "/v1/sessions?account_id="+url.QueryEscape(accountID), nil, &out, c.timeout); err != nil {
		return nil, err
	}
	return out.Sessions, nil
}

// DeleteSession ends one session of an account. The authority refuses with
// ErrNotFound when the session is not that account's.
func (c *Client) DeleteSession(ctx context.Context, accountID, sessionID string) error {
	return c.do(ctx, "delete session", http.MethodDelete,
		"/v1/sessions/"+url.PathEscape(sessionID)+"?account_id="+url.QueryEscape(accountID), nil, nil, c.timeout)
}

// ── Credentials ──────────────────────────────────────────────────────────────

// ChangePassword replaces an account's password. current is "" only for an
// account that has never set one. Every session but keepToken's ends.
func (c *Client) ChangePassword(ctx context.Context, accountID, current, next, keepToken string) error {
	body := map[string]string{"current": current, "new": next, "keep_session_token": keepToken}
	return c.do(ctx, "change password", http.MethodPost, "/v1/accounts/"+url.PathEscape(accountID)+"/password", body, nil, c.timeout)
}

// ResetPassword is an administrator setting an account's password. Every
// session on the account ends.
func (c *Client) ResetPassword(ctx context.Context, accountID, next, by string) error {
	body := map[string]string{"new": next, "by": by}
	return c.do(ctx, "reset password", http.MethodPost, "/v1/accounts/"+url.PathEscape(accountID)+"/password/reset", body, nil, c.timeout)
}

// BeginRecovery spends one backup code of the account a handle names and
// returns a short-lived recovery token. The authority answers
// ErrBackupCodeInvalid for an unknown handle and a wrong code alike.
func (c *Client) BeginRecovery(ctx context.Context, handle, backupCode string) (*Recovery, error) {
	var out Recovery
	body := map[string]string{"handle": handle, "backup_code": backupCode}
	if err := c.do(ctx, "begin recovery", http.MethodPost, "/v1/recovery/verify", body, &out, c.timeout); err != nil {
		return nil, err
	}
	if out.RecoveryToken == "" {
		return nil, fmt.Errorf("%w: recovery answered without a token", ErrUnavailable)
	}
	return &out, nil
}

// CompleteRecovery sets a new password with a recovery token, ends every
// session on the account, and opens a new one for this device.
func (c *Client) CompleteRecovery(ctx context.Context, recoveryToken, next, deviceName, ip string) (*LoginResult, error) {
	var out LoginResult
	body := map[string]string{"recovery_token": recoveryToken, "new_password": next, "device_name": deviceName, "ip": ip}
	if err := c.do(ctx, "complete recovery", http.MethodPost, "/v1/recovery/complete", body, &out, c.timeout); err != nil {
		return nil, err
	}
	if out.SessionToken == "" {
		return nil, fmt.Errorf("%w: recovery answered without a session", ErrUnavailable)
	}
	return &out, nil
}

// TOTPEnroll mints a fresh, not yet active, second-factor secret.
func (c *Client) TOTPEnroll(ctx context.Context, accountID string) (*TOTPEnrollment, error) {
	var out TOTPEnrollment
	if err := c.do(ctx, "totp enroll", http.MethodPost, "/v1/accounts/"+url.PathEscape(accountID)+"/totp/enroll", nil, &out, c.timeout); err != nil {
		return nil, err
	}
	return &out, nil
}

// TOTPConfirm activates the enrolled secret with a code it produced.
func (c *Client) TOTPConfirm(ctx context.Context, accountID, code string) error {
	return c.do(ctx, "totp confirm", http.MethodPost, "/v1/accounts/"+url.PathEscape(accountID)+"/totp/confirm",
		map[string]string{"code": code}, nil, c.timeout)
}

// TOTPDisable turns the second factor off with a current code.
func (c *Client) TOTPDisable(ctx context.Context, accountID, code string) error {
	return c.do(ctx, "totp disable", http.MethodPost, "/v1/accounts/"+url.PathEscape(accountID)+"/totp/disable",
		map[string]string{"code": code}, nil, c.timeout)
}

// IssueBackupCodes replaces an account's recovery codes and returns them in the
// clear — the one moment they exist in the clear.
func (c *Client) IssueBackupCodes(ctx context.Context, accountID string) ([]string, error) {
	var out struct {
		Codes []string `json:"codes"`
	}
	if err := c.do(ctx, "issue backup codes", http.MethodPost, "/v1/accounts/"+url.PathEscape(accountID)+"/backup-codes", nil, &out, c.timeout); err != nil {
		return nil, err
	}
	if len(out.Codes) == 0 {
		return nil, fmt.Errorf("%w: backup codes answered empty", ErrUnavailable)
	}
	return out.Codes, nil
}

// ── Identity writes ──────────────────────────────────────────────────────────

// PutFacts changes identity facts.
func (c *Client) PutFacts(ctx context.Context, pialID string, update FactsUpdate) error {
	return c.do(ctx, "put facts", http.MethodPut, "/v1/pials/"+url.PathEscape(pialID)+"/facts", update, nil, c.timeout)
}

// PutRole sets an identity's platform role. The founder role is refused in
// both directions with ErrFounderPermanent.
func (c *Client) PutRole(ctx context.Context, pialID, role, by string) error {
	return c.do(ctx, "put role", http.MethodPut, "/v1/pials/"+url.PathEscape(pialID)+"/role",
		map[string]string{"role": role, "by": by}, nil, c.timeout)
}

// PutCapability sets one capability on an identity.
func (c *Client) PutCapability(ctx context.Context, pialID, capability string, w CapabilityWrite) error {
	return c.do(ctx, "put capability", http.MethodPut,
		"/v1/pials/"+url.PathEscape(pialID)+"/capabilities/"+url.PathEscape(capability), w, nil, c.timeout)
}

// Ban ends an identity: every account, every capability, every session.
func (c *Client) Ban(ctx context.Context, pialID, reason, by string) error {
	return c.do(ctx, "ban", http.MethodPost, "/v1/pials/"+url.PathEscape(pialID)+"/ban",
		map[string]string{"reason": reason, "by": by}, nil, c.timeout)
}

// Unban restores an identity a ban ended.
func (c *Client) Unban(ctx context.Context, pialID, reason, by string) error {
	return c.do(ctx, "unban", http.MethodPost, "/v1/pials/"+url.PathEscape(pialID)+"/unban",
		map[string]string{"reason": reason, "by": by}, nil, c.timeout)
}

// ── Transport ────────────────────────────────────────────────────────────────

// do sends one request and decodes its answer into out. A 2xx is success; a
// 4xx with a typed body is an *Error; anything else is ErrUnavailable.
func (c *Client) do(ctx context.Context, op, method, path string, in, out interface{}, timeout time.Duration) error {
	if !c.Configured() {
		return fmt.Errorf("%w: ELOHIM_VENI_URL or INTERNAL_API_KEY is not configured", ErrUnavailable)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var body io.Reader
	if in != nil {
		payload, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("identity: %s: encoding request: %w", op, err)
		}
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return fmt.Errorf("identity: %s: building request: %w", op, err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Internal-Key", c.key)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrUnavailable, op, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("%w: %s: reading answer: %v", ErrUnavailable, op, err)
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if out == nil || resp.StatusCode == http.StatusNoContent {
			return nil
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%w: %s: undecodable answer: %v", ErrUnavailable, op, err)
		}
		return nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return decodeRefusal(op, resp.StatusCode, raw)
	default:
		return fmt.Errorf("%w: %s: HTTP %d", ErrUnavailable, op, resp.StatusCode)
	}
}

// decodeRefusal turns a 4xx answer into an *Error. A 4xx without a code is
// still a refusal — the authority answered — and is named after its status, so
// errors.Is against a sentinel stays false rather than guessing.
func decodeRefusal(op string, status int, raw []byte) error {
	var stated struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &stated) // an untyped body is handled below, not ignored
	code := stated.Error
	if code == "" {
		switch status {
		case http.StatusNotFound:
			code = ErrNotFound.Code
		default:
			code = fmt.Sprintf("http_%d", status)
		}
	}
	return &Error{Status: status, Code: code, Message: stated.Message, Op: op}
}

// IsUnavailable reports whether err means the authority could not answer.
func IsUnavailable(err error) bool { return errors.Is(err, ErrUnavailable) }
