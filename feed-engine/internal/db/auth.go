package db

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// ── Session tokens ────────────────────────────────────────────────────────────

// GenerateSessionToken creates a cryptographically secure 32-byte token.
// Returns the raw token (for cookie) and the SHA-256 hash (for DB storage).
func GenerateSessionToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return
	}
	token = hex.EncodeToString(b)
	h := sha256.Sum256(b)
	hash = hex.EncodeToString(h[:])
	return
}

// HashToken returns the SHA-256 hex hash of a raw token string.
func HashToken(token string) string {
	raw, _ := hex.DecodeString(token)
	if len(raw) == 0 {
		raw = []byte(token)
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

// CreateAuthSession inserts a new PIAL-bound session and returns the raw token for the cookie.
// pialID is the PIAL root; userID is the active account within that PIAL.
func CreateAuthSession(db *sql.DB, userID, deviceName, ipAddress string) (string, error) {
	// Resolve PIAL for this account — required for PIAL-bound sessions.
	pialID := ResolvePIAL(db, userID)
	return CreateAuthSessionWithPIAL(db, pialID, userID, deviceName, ipAddress)
}

// CreateAuthSessionWithPIAL creates a session with an explicit PIAL ID.
// Use this when the PIAL is already known (e.g., after BootstrapPIAL).
func CreateAuthSessionWithPIAL(db *sql.DB, pialID, accountID, deviceName, ipAddress string) (string, error) {
	token, hash, err := GenerateSessionToken()
	if err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	deviceID := hex.EncodeToString(func() []byte { b := make([]byte, 8); rand.Read(b); return b }())
	var pialParam interface{}
	if pialID != "" {
		pialParam = pialID
	}
	_, err = db.Exec(`
		INSERT INTO user_sessions (user_id, token_hash, device_id, device_name, ip_address, pial_id, active_account_id)
		VALUES ($1, $2, $3, $4, $5, $6, $1)
	`, accountID, hash, deviceID, deviceName, ipAddress, pialParam)
	if err != nil {
		return "", fmt.Errorf("insert session: %w", err)
	}
	return token, nil
}

// SessionIdentity holds the resolved identity for an authenticated session.
type SessionIdentity struct {
	UserID string // active account ID (= active_account_id, falls back to user_id)
	PIALID string // PIAL root UUID — empty for legacy sessions not yet backfilled
}

// GetSessionIdentity looks up a session by raw token.
// Returns (zero SessionIdentity, nil) when not found or expired — never errors on missing row.
func GetSessionIdentity(db *sql.DB, rawToken string) (SessionIdentity, error) {
	hash := HashToken(rawToken)
	var s SessionIdentity
	// active_account_id supersedes user_id; pial_id may be NULL for old sessions.
	err := db.QueryRow(`
		SELECT COALESCE(active_account_id, user_id), COALESCE(pial_id::TEXT, '')
		FROM user_sessions
		WHERE token_hash = $1 AND expires_at > NOW()
	`, hash).Scan(&s.UserID, &s.PIALID)
	if err == sql.ErrNoRows {
		return SessionIdentity{}, nil
	}
	if err != nil {
		return SessionIdentity{}, err
	}
	go func() {
		db.Exec(`UPDATE user_sessions SET last_seen_at = NOW() WHERE token_hash = $1`, hash)
		// Check Unemployed Final Boss — 16 consecutive hours in one session
		var sessionID string
		db.QueryRow(`SELECT id FROM user_sessions WHERE token_hash = $1`, hash).Scan(&sessionID)
		if sessionID != "" && s.UserID != "" {
			TryAwardTrollOnSessionUpdate(db, s.UserID, sessionID)
		}
	}()
	return s, nil
}

// GetUserIDFromSession is kept for call sites not yet migrated to GetSessionIdentity.
func GetUserIDFromSession(db *sql.DB, rawToken string) (string, error) {
	ident, err := GetSessionIdentity(db, rawToken)
	return ident.UserID, err
}

// DeleteSession removes a session (logout).
func DeleteSession(db *sql.DB, rawToken string) error {
	hash := HashToken(rawToken)
	_, err := db.Exec(`DELETE FROM user_sessions WHERE token_hash = $1`, hash)
	return err
}

// DeleteAllUserSessions removes all sessions for a user (security logout).
func DeleteAllUserSessions(db *sql.DB, userID string) error {
	_, err := db.Exec(`DELETE FROM user_sessions WHERE user_id = $1`, userID)
	return err
}

// ListUserSessions returns active sessions for a user (device management).
func ListUserSessions(db *sql.DB, userID string) ([]SessionInfo, error) {
	rows, err := db.Query(`
		SELECT id, device_name, ip_address, created_at, last_seen_at
		FROM user_sessions
		WHERE user_id = $1 AND expires_at > NOW()
		ORDER BY last_seen_at DESC
		LIMIT 10
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []SessionInfo
	for rows.Next() {
		var s SessionInfo
		if err := rows.Scan(&s.ID, &s.DeviceName, &s.IPAddress, &s.CreatedAt, &s.LastSeenAt); err != nil {
			continue
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

type SessionInfo struct {
	ID         string
	DeviceName string
	IPAddress  string
	CreatedAt  time.Time
	LastSeenAt time.Time
	IsCurrent  bool // hydrated by the handler, not from DB
}

// RevokeSession deletes a specific session by id AND user_id so users can only revoke their own.
func RevokeSession(db *sql.DB, sessionID, userID string) error {
	res, err := db.Exec(`DELETE FROM user_sessions WHERE id = $1 AND user_id = $2`, sessionID, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("session not found or not owned by user")
	}
	return nil
}

// ── Password credentials ──────────────────────────────────────────────────────

// SetPassword hashes and stores a password for a user.
func SetPassword(db *sql.DB, userID, plaintext string) error {
	if len(plaintext) < 8 {
		return fmt.Errorf("password must be at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plaintext), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	_, err = db.Exec(`
		INSERT INTO user_credentials (user_id, password_hash)
		VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE SET password_hash = EXCLUDED.password_hash, updated_at = NOW()
	`, userID, string(hash))
	return err
}

// CheckPassword verifies a plaintext password against the stored hash.
// Returns (userID, nil) on success, ("", nil) if wrong, ("", err) on DB error.
func CheckPassword(db *sql.DB, handle, plaintext string) (string, error) {
	var userID, hash string
	err := db.QueryRow(`
		SELECT u.id, COALESCE(c.password_hash, '')
		FROM users u
		LEFT JOIN user_credentials c ON c.user_id = u.id
		WHERE u.handle = $1
	`, handle).Scan(&userID, &hash)
	if err == sql.ErrNoRows {
		return "", nil // user not found
	}
	if err != nil {
		return "", err
	}
	if hash == "" {
		return "", nil // no password set
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(plaintext)); err != nil {
		return "", nil // wrong password
	}
	return userID, nil
}

// HasPassword returns true if a user has a password set.
func HasPassword(db *sql.DB, userID string) bool {
	var hash string
	db.QueryRow(`SELECT password_hash FROM user_credentials WHERE user_id = $1`, userID).Scan(&hash)
	return hash != ""
}

// ── Backup codes ─────────────────────────────────────────────────────────────

// GenerateBackupCode returns a single formatted code: XXXX-XXXX-XXXX (base32, ~60 bits).
func GenerateBackupCode() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	s = strings.ToUpper(s[:12])
	return s[:4] + "-" + s[4:8] + "-" + s[8:12], nil
}

// normalizeCode strips formatting from user input before comparison.
func normalizeCode(code string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(code, "-", ""), " ", ""))
}

// StoreBackupCodes atomically replaces all backup codes for a user.
// codes must be plaintext; they are bcrypt-hashed before storage.
func StoreBackupCodes(db *sql.DB, userID string, codes []string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM backup_codes WHERE user_id = $1`, userID); err != nil {
		return err
	}
	for _, code := range codes {
		hash, err := bcrypt.GenerateFromPassword([]byte(normalizeCode(code)), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(
			`INSERT INTO backup_codes (user_id, code_hash) VALUES ($1, $2)`,
			userID, string(hash),
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// HasUnusedBackupCodes returns true if the user has at least one unused backup code.
func HasUnusedBackupCodes(db *sql.DB, userID string) bool {
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM backup_codes WHERE user_id = $1 AND NOT used`, userID).Scan(&count)
	return count > 0
}

// VerifyAndConsumeBackupCode checks code against all unused codes for userID.
// On match it marks the code used (atomic) and returns true.
func VerifyAndConsumeBackupCode(db *sql.DB, userID, code string) (bool, error) {
	normalized := normalizeCode(code)
	rows, err := db.Query(`SELECT id, code_hash FROM backup_codes WHERE user_id = $1 AND NOT used`, userID)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	var matchID string
	for rows.Next() {
		var id, hash string
		if err := rows.Scan(&id, &hash); err != nil {
			continue
		}
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(normalized)) == nil {
			matchID = id
			break
		}
	}
	rows.Close()

	if matchID == "" {
		return false, nil
	}
	_, err = db.Exec(`UPDATE backup_codes SET used = TRUE WHERE id = $1`, matchID)
	return err == nil, err
}

// ── TOTP 2FA ──────────────────────────────────────────────────────────────────

// GetTOTPSecret returns the stored TOTP secret and enabled flag for a user.
func GetTOTPSecret(db *sql.DB, userID string) (secret string, enabled bool) {
	db.QueryRow(`SELECT totp_secret, totp_enabled FROM user_credentials WHERE user_id = $1`, userID).
		Scan(&secret, &enabled)
	return
}

// SetTOTPSecret stores an unverified TOTP secret (not yet enabled).
func SetTOTPSecret(db *sql.DB, userID, secret string) error {
	_, err := db.Exec(`
		INSERT INTO user_credentials (user_id, totp_secret, totp_enabled)
		VALUES ($1, $2, FALSE)
		ON CONFLICT (user_id) DO UPDATE SET totp_secret = EXCLUDED.totp_secret, totp_enabled = FALSE
	`, userID, secret)
	return err
}

// EnableTOTP marks TOTP as enabled for a user (call after the user verifies the first code).
func EnableTOTP(db *sql.DB, userID string) error {
	_, err := db.Exec(`
		INSERT INTO user_credentials (user_id, totp_enabled)
		VALUES ($1, TRUE)
		ON CONFLICT (user_id) DO UPDATE SET totp_enabled = TRUE
	`, userID)
	return err
}

// DisableTOTP clears the TOTP secret and disables 2FA.
func DisableTOTP(db *sql.DB, userID string) error {
	_, err := db.Exec(`
		UPDATE user_credentials SET totp_secret = '', totp_enabled = FALSE WHERE user_id = $1`, userID)
	return err
}

// ── User roles ────────────────────────────────────────────────────────────────

type UserRole struct {
	UserID        string
	RoleType      string // user | creator | official | minor_user | minor_creator
	CreatorType   string // musician | streamer | artist | podcaster | athlete | journalist | educator | adult_content_creator | other
	IsAdult       bool
	IsMinor       bool
	IsIDVerified  bool
	IsAgeVerified bool
	ContentPrefs  map[string]bool
	OnboardStep   int
	OnboardDone   bool
}

func GetUserRole(db *sql.DB, userID string) (*UserRole, error) {
	r := &UserRole{UserID: userID, RoleType: "user", ContentPrefs: map[string]bool{}}
	var prefsJSON []byte
	// role / is_adult / is_minor / is_age_verified are sourced from PIAL (the record);
	// creator_type / is_id_verified / content_prefs / onboard_* remain on user_roles.
	err := db.QueryRow(`
		SELECT COALESCE(pr.role,'user'), ur.creator_type,
		       COALESCE(pr.is_adult,FALSE), COALESCE(pr.is_minor,FALSE),
		       ur.is_id_verified, COALESCE(pr.age_verified,FALSE), ur.content_prefs,
		       ur.onboard_step, ur.onboard_done
		FROM user_roles ur
		JOIN users u ON u.id = ur.user_id
		LEFT JOIN pial_roots pr ON pr.pial_id = u.pial_id
		WHERE ur.user_id = $1
	`, userID).Scan(
		&r.RoleType, &r.CreatorType, &r.IsAdult, &r.IsMinor,
		&r.IsIDVerified, &r.IsAgeVerified, &prefsJSON,
		&r.OnboardStep, &r.OnboardDone,
	)
	if err == sql.ErrNoRows {
		return r, nil // return default
	}
	return r, err
}

// UpsertUserRole writes the user_roles columns that remain local to feed-engine.
// role / is_adult / is_minor / is_age_verified now live on PIAL (the record) and are
// written via the PIAL setters / SetPIALBirthday at onboarding — never here.
func UpsertUserRole(db *sql.DB, r *UserRole) error {
	_, err := db.Exec(`
		INSERT INTO user_roles
		  (user_id, creator_type, is_id_verified, onboard_step, onboard_done)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (user_id) DO UPDATE SET
		  creator_type   = EXCLUDED.creator_type,
		  is_id_verified = EXCLUDED.is_id_verified,
		  onboard_step   = EXCLUDED.onboard_step,
		  onboard_done   = EXCLUDED.onboard_done,
		  updated_at     = NOW()
	`,
		r.UserID, r.CreatorType, r.IsIDVerified, r.OnboardStep, r.OnboardDone,
	)
	return err
}

// ── Notifications ─────────────────────────────────────────────────────────────

type Notification struct {
	ID          string
	Type        string // like | follow | reply | mention | message | system | tip | milestone
	ActorID     string
	ActorName   string
	ActorHandle string
	ActorAvatar string
	TargetID    string
	TargetType  string
	Payload     map[string]interface{}
	IsRead      bool
	TimeAgo     string
	CreatedAt   time.Time
}

// CreateNotification writes one notification, reporting whether a row was
// actually created.
//
// The bool is not decoration. A notification about somebody's own action is
// dropped here, and a caller that then went on to raise the unread badge would
// be pointing a person at a notification that does not exist. The badge is a
// count of rows, so only a caller told a row landed may raise it.
//
// An actorID of "" means the notification has no actor: it is the platform
// telling this account a fact about itself. That is not a self-notification and
// is not dropped — the self-notify rule exists so a person is not told they
// liked their own post, not to prevent the system from speaking.
func CreateNotification(db *sql.DB, userID, nType, actorID, targetID, targetType string) (bool, error) {
	if actorID != "" && userID == actorID {
		return false, nil // never self-notify
	}
	_, err := db.Exec(`
		INSERT INTO notifications (user_id, type, actor_id, target_id, target_type)
		VALUES ($1, $2, $3, $4, $5)
	`, userID, nType, nullIfEmpty(actorID), targetID, targetType)
	if err != nil {
		return false, err
	}
	return true, nil
}

func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func GetNotifications(db *sql.DB, userID string, limit int) ([]Notification, error) {
	rows, err := db.Query(`
		SELECT
		    n.id, n.type, n.target_id, n.target_type, n.is_read, n.created_at,
		    COALESCE(u.id::text, ''), COALESCE(p.display_name, COALESCE(u.handle, '')),
		    COALESCE(u.handle, ''), COALESCE(p.avatar_url, '')
		FROM notifications n
		LEFT JOIN users u      ON u.id = n.actor_id
		LEFT JOIN user_profiles p ON p.user_id = n.actor_id
		WHERE n.user_id = $1
		ORDER BY n.created_at DESC
		LIMIT $2
	`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var notifs []Notification
	for rows.Next() {
		var n Notification
		if err := rows.Scan(
			&n.ID, &n.Type, &n.TargetID, &n.TargetType, &n.IsRead, &n.CreatedAt,
			&n.ActorID, &n.ActorName, &n.ActorHandle, &n.ActorAvatar,
		); err != nil {
			continue
		}
		notifs = append(notifs, n)
	}
	return notifs, rows.Err()
}

func MarkAllNotificationsRead(db *sql.DB, userID string) error {
	_, err := db.Exec(`UPDATE notifications SET is_read = TRUE WHERE user_id = $1`, userID)
	return err
}

func CountUnreadNotifications(db *sql.DB, userID string) int {
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM notifications WHERE user_id = $1 AND is_read = FALSE`, userID).Scan(&count)
	return count
}

// IsKnownDevice returns true if this user has previously logged in from this
// device name or IP address. Fails open (returns true) on DB error.
func IsKnownDevice(db *sql.DB, userID, deviceName, ipAddress string) bool {
	var exists bool
	err := db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM user_sessions
			WHERE user_id = $1
			  AND (device_name = $2 OR ip_address = $3)
			  AND created_at < NOW() - INTERVAL '1 minute'
		)`, userID, deviceName, ipAddress).Scan(&exists)
	if err != nil {
		return true // fail open — don't spam alerts on DB issues
	}
	return exists
}

// GetUserEmail returns the email for a user, or "" if unset.
func GetUserEmail(db *sql.DB, userID string) string {
	var email string
	db.QueryRow(`SELECT COALESCE(email, '') FROM users WHERE id = $1`, userID).Scan(&email)
	return email
}
