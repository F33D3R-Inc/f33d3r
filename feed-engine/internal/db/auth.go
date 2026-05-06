package db

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
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

// CreateAuthSession inserts a new auth session and returns the raw token for the cookie.
func CreateAuthSession(db *sql.DB, userID, deviceName, ipAddress string) (string, error) {
	token, hash, err := GenerateSessionToken()
	if err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	deviceID := hex.EncodeToString(func() []byte { b := make([]byte, 8); rand.Read(b); return b }())
	_, err = db.Exec(`
		INSERT INTO user_sessions (user_id, token_hash, device_id, device_name, ip_address)
		VALUES ($1, $2, $3, $4, $5)
	`, userID, hash, deviceID, deviceName, ipAddress)
	if err != nil {
		return "", fmt.Errorf("insert session: %w", err)
	}
	return token, nil
}

// GetUserIDFromSession looks up a session by raw token. Returns ("", nil) if not found or expired.
func GetUserIDFromSession(db *sql.DB, rawToken string) (string, error) {
	hash := HashToken(rawToken)
	var userID string
	err := db.QueryRow(`
		SELECT user_id FROM user_sessions
		WHERE token_hash = $1 AND expires_at > NOW()
	`, hash).Scan(&userID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	// Update last_seen_at async — don't fail request on error
	go db.Exec(`UPDATE user_sessions SET last_seen_at = NOW() WHERE token_hash = $1`, hash)
	return userID, nil
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
		SELECT device_name, ip_address, created_at, last_seen_at
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
		if err := rows.Scan(&s.DeviceName, &s.IPAddress, &s.CreatedAt, &s.LastSeenAt); err != nil {
			continue
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

type SessionInfo struct {
	DeviceName string
	IPAddress  string
	CreatedAt  time.Time
	LastSeenAt time.Time
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

// ── User roles ────────────────────────────────────────────────────────────────

type UserRole struct {
	UserID        string
	RoleType      string // user | creator | official | minor_user | minor_creator
	CreatorType   string // musician | streamer | artist | podcaster | athlete | journalist | other
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
	err := db.QueryRow(`
		SELECT role_type, creator_type, is_adult, is_minor,
		       is_id_verified, is_age_verified, content_prefs,
		       onboard_step, onboard_done
		FROM user_roles WHERE user_id = $1
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

func UpsertUserRole(db *sql.DB, r *UserRole) error {
	_, err := db.Exec(`
		INSERT INTO user_roles
		  (user_id, role_type, creator_type, is_adult, is_minor,
		   is_id_verified, is_age_verified, onboard_step, onboard_done)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (user_id) DO UPDATE SET
		  role_type      = EXCLUDED.role_type,
		  creator_type   = EXCLUDED.creator_type,
		  is_adult       = EXCLUDED.is_adult,
		  is_minor       = EXCLUDED.is_minor,
		  is_id_verified = EXCLUDED.is_id_verified,
		  is_age_verified= EXCLUDED.is_age_verified,
		  onboard_step   = EXCLUDED.onboard_step,
		  onboard_done   = EXCLUDED.onboard_done,
		  updated_at     = NOW()
	`,
		r.UserID, r.RoleType, r.CreatorType, r.IsAdult, r.IsMinor,
		r.IsIDVerified, r.IsAgeVerified, r.OnboardStep, r.OnboardDone,
	)
	return err
}

// ── Notifications ─────────────────────────────────────────────────────────────

type Notification struct {
	ID         string
	Type       string // like | follow | reply | mention | message | system | tip | milestone
	ActorID    string
	ActorName  string
	ActorHandle string
	ActorAvatar string
	TargetID   string
	TargetType string
	Payload    map[string]interface{}
	IsRead     bool
	TimeAgo    string
	CreatedAt  time.Time
}

func CreateNotification(db *sql.DB, userID, nType, actorID, targetID, targetType string) error {
	if userID == actorID {
		return nil // never self-notify
	}
	_, err := db.Exec(`
		INSERT INTO notifications (user_id, type, actor_id, target_id, target_type)
		VALUES ($1, $2, $3, $4, $5)
	`, userID, nType, nullIfEmpty(actorID), targetID, targetType)
	return err
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
