package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// ErrNotFound is returned when a lookup names nothing.
var ErrNotFound = errors.New("not found")

// HandleRe is feed-engine's handle rule, verbatim.
var HandleRe = regexp.MustCompile(`^[a-zA-Z0-9_\-]{1,30}$`)

var accentRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// User is one account with its profile and PIAL standing joined in — the
// shape the handlers need, assembled once here.
type User struct {
	ID      string
	Handle  string
	PIALID  string
	Tier    string
	Created time.Time

	// Profile.
	DisplayName         string
	Bio                 string
	Pronouns            string
	Location            string
	Website             string
	AvatarURL           string
	HeaderURL           string
	ThemeID             string
	AccentHex           string
	OfficialType        string
	IsCreator           bool
	IsAdultCreator      bool
	IsPrivate           bool
	ContentSetting      string
	ShowSensitive       bool
	CelebrationsEnabled bool
	FollowerCount       int
	FollowingCount      int
	PostCount           int
	PinnedWorkID        string

	// PIAL standing.
	Role          string
	XP            int64
	IsVerified    bool
	IsAdult       bool
	IsMinor       bool
	IsAgeVerified bool
	// KYCTier is where identity verification stands: none | basic | soft | full.
	// Full is the payout gate, as in feed-engine's CanMonetize.
	KYCTier        string
	KYCSubmittedAt *time.Time

	// Account fields the settings screen edits.
	BirthdayMdVisibility   string
	BirthdayYearVisibility string
	CountryCode            string
	SocialLinks            map[string]string
	ExternalTipLinks       map[string]string

	// Credentials.
	TwoFAEnabled     bool
	NeedsBackupCodes bool
	// HasPassword is false for an account created through a channel that set
	// none; the change-password form then asks for the new one only.
	HasPassword bool
}

const userSelectSQL = `
SELECT u.id, u.handle, u.pial_id, u.tier, u.created_at,
       p.display_name, p.bio, p.pronouns, p.location, p.website, p.avatar_url, p.header_url,
       p.theme_id, p.accent_hex, p.official_type, p.is_creator, p.is_adult_creator, p.is_private,
       p.content_setting, p.show_sensitive, p.celebrations_enabled,
       p.follower_count, p.following_count, p.post_count, COALESCE(p.pinned_work_id, ''),
       r.role, r.xp, r.is_verified, r.is_adult, r.is_minor, r.age_verified,
       COALESCE(c.totp_enabled, 0), COALESCE(c.backup_codes_set, 1),
       r.kyc_tier, COALESCE(r.kyc_submitted_at, ''),
       p.birthday_md_visibility, p.birthday_year_visibility, COALESCE(p.country_code, ''),
       p.social_links, p.external_tip_links, COALESCE(c.password_hash, '') <> ''
FROM users u
JOIN user_profiles p ON p.user_id = u.id
JOIN pial_roots r ON r.pial_id = u.pial_id
LEFT JOIN user_credentials c ON c.user_id = u.id
`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var created string
	var isCreator, isAdultCreator, isPrivate, showSensitive, celebrations, isVerified, isAdult, isMinor, ageVerified, totp, backup, hasPassword int
	var kycSubmitted, socialLinks, tipLinks string
	err := row.Scan(
		&u.ID, &u.Handle, &u.PIALID, &u.Tier, &created,
		&u.DisplayName, &u.Bio, &u.Pronouns, &u.Location, &u.Website, &u.AvatarURL, &u.HeaderURL,
		&u.ThemeID, &u.AccentHex, &u.OfficialType, &isCreator, &isAdultCreator, &isPrivate,
		&u.ContentSetting, &showSensitive, &celebrations,
		&u.FollowerCount, &u.FollowingCount, &u.PostCount, &u.PinnedWorkID,
		&u.Role, &u.XP, &isVerified, &isAdult, &isMinor, &ageVerified,
		&totp, &backup,
		&u.KYCTier, &kycSubmitted,
		&u.BirthdayMdVisibility, &u.BirthdayYearVisibility, &u.CountryCode,
		&socialLinks, &tipLinks, &hasPassword,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.Created = ParseTime(created)
	if kycSubmitted != "" {
		at := ParseTime(kycSubmitted)
		u.KYCSubmittedAt = &at
	}
	u.SocialLinks = decodeLinkMap(socialLinks)
	u.ExternalTipLinks = decodeLinkMap(tipLinks)
	u.HasPassword = hasPassword == 1
	u.IsCreator = isCreator == 1
	u.IsAdultCreator = isAdultCreator == 1
	u.IsPrivate = isPrivate == 1
	u.ShowSensitive = showSensitive == 1
	u.CelebrationsEnabled = celebrations == 1
	u.IsVerified = isVerified == 1
	u.IsAdult = isAdult == 1
	u.IsMinor = isMinor == 1
	u.IsAgeVerified = ageVerified == 1
	u.TwoFAEnabled = totp == 1
	u.NeedsBackupCodes = backup == 0
	if u.DisplayName == "" {
		u.DisplayName = u.Handle
	}
	return &u, nil
}

// GetUserByID loads one account. ErrNotFound when there is none.
func (s *Store) GetUserByID(ctx context.Context, id string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, userSelectSQL+`WHERE u.id = ?`, id))
}

// GetUserByHandle loads one account by handle, case-insensitively.
func (s *Store) GetUserByHandle(ctx context.Context, handle string) (*User, error) {
	handle = NormalizeHandle(handle)
	return scanUser(s.db.QueryRowContext(ctx, userSelectSQL+`WHERE u.handle = ?`, handle))
}

// GetUserByPIAL resolves a PIAL to its account.
func (s *Store) GetUserByPIAL(ctx context.Context, pial string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, userSelectSQL+`WHERE u.pial_id = ?`, pial))
}

// GetUsersByID loads many accounts at once, keyed by id.
func (s *Store) GetUsersByID(ctx context.Context, ids []string) (map[string]*User, error) {
	out := make(map[string]*User, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	q, args := inClause(ids)
	rows, err := s.db.QueryContext(ctx, userSelectSQL+`WHERE u.id IN (`+q+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out[u.ID] = u
	}
	return out, rows.Err()
}

// NormalizeHandle strips a leading @, trims, and lower-cases — feed-engine
// stores handles lower-case and every lookup does the same.
func NormalizeHandle(h string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "@")))
}

// NewUserParams is what CreateUser needs. Everything not set takes the same
// default the real schema gives it.
type NewUserParams struct {
	Handle      string
	Password    string
	DisplayName string
	Bio         string
	Role        string // user | admin | founder
	Tier        string // free | subscriber | creator
	IsCreator   bool
	IsVerified  bool
	XP          int64
	AvatarURL   string
	HeaderURL   string
	Pronouns    string
	Location    string
	Website     string
	AccentHex   string
	CreatedAt   *time.Time
	// KYCTier seeds the identity standing; empty is none.
	KYCTier string
}

// CreateUser creates a PIAL root, the account bound to it, its profile and
// its credentials, in one transaction — the same four rows signup writes in
// feed-engine, minus the Elohim Veni bootstrap call because this server *is*
// the key authority here.
func (s *Store) CreateUser(ctx context.Context, p NewUserParams) (*User, error) {
	handle := NormalizeHandle(p.Handle)
	if !HandleRe.MatchString(handle) {
		return nil, fmt.Errorf("invalid handle %q", p.Handle)
	}
	if p.Password == "" {
		return nil, errors.New("password required")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(p.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	now := Now()
	if p.CreatedAt != nil {
		now = FormatTime(*p.CreatedAt)
	}
	if p.Role == "" {
		p.Role = "user"
	}
	if p.Tier == "" {
		p.Tier = "free"
	}
	if p.DisplayName == "" {
		p.DisplayName = handle
	}
	kycTier := p.KYCTier
	if kycTier == "" {
		kycTier = "none"
	}
	var kycSubmitted any
	if kycTier != "none" {
		kycSubmitted = now
	}
	pial := NewID()
	id := NewID()
	err = s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO pial_roots (pial_id, role, xp, is_verified, is_adult, is_minor, age_verified, created_at, kyc_tier, kyc_submitted_at)
			VALUES (?, ?, ?, ?, 1, 0, ?, ?, ?, ?)`,
			pial, p.Role, p.XP, boolInt(p.IsVerified), boolInt(p.IsVerified), now, kycTier, kycSubmitted); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO users (id, handle, pial_id, tier, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			id, handle, pial, p.Tier, now, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO user_profiles (user_id, display_name, bio, pronouns, location, website, avatar_url, header_url,
			                           accent_hex, is_creator, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, p.DisplayName, p.Bio, p.Pronouns, p.Location, p.Website, p.AvatarURL, p.HeaderURL,
			p.AccentHex, boolInt(p.IsCreator), now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO user_credentials (user_id, password_hash, updated_at) VALUES (?, ?, ?)`,
			id, string(hash), now)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetUserByID(ctx, id)
}

// IsHandleTaken reports whether a handle is in use.
func (s *Store) IsHandleTaken(ctx context.Context, handle string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE handle = ?`, NormalizeHandle(handle)).Scan(&n)
	return n > 0, err
}

// CheckPassword verifies a handle/password pair and returns the account, or
// ErrNotFound for either an unknown handle or a wrong password — one answer
// for both, so a caller cannot enumerate handles through the login form.
func (s *Store) CheckPassword(ctx context.Context, handle, password string) (*User, error) {
	u, err := s.GetUserByHandle(ctx, handle)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// Burn the same time a real comparison would, so a missing handle
			// is not visibly faster than a wrong password.
			bcrypt.CompareHashAndPassword([]byte("$2a$10$000000000000000000000uGxbz7Zq7Zt0Z0m6X9Q5Q5Q5Q5Q5Q5Q5Q"), []byte(password))
		}
		return nil, ErrNotFound
	}
	var hash string
	if err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM user_credentials WHERE user_id = ?`, u.ID).Scan(&hash); err != nil {
		return nil, ErrNotFound
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return nil, ErrNotFound
	}
	return u, nil
}

// ── Sessions ──────────────────────────────────────────────────────────────────

// SessionTTL is feed-engine's 30-day session life.
const SessionTTL = 30 * 24 * time.Hour

// HashToken is feed-engine's token hashing, verbatim: the token is hex, so
// the hash covers the decoded bytes.
func HashToken(token string) string {
	raw, _ := hex.DecodeString(token)
	if len(raw) == 0 {
		raw = []byte(token)
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

// CreateSession mints a bearer token for the account and returns it with its
// expiry. Only the SHA-256 of the token is stored.
func (s *Store) CreateSession(ctx context.Context, u *User, deviceName, ip string) (token string, expires time.Time, err error) {
	token, err = RandomHex(32)
	if err != nil {
		return "", time.Time{}, err
	}
	deviceID, _ := RandomHex(8)
	now := time.Now()
	expires = now.Add(SessionTTL)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO user_sessions (id, user_id, token_hash, device_id, device_name, ip_address, pial_id, active_account_id,
		                           created_at, expires_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		NewID(), u.ID, HashToken(token), deviceID, deviceName, ip, u.PIALID, u.ID,
		FormatTime(now), FormatTime(expires), FormatTime(now))
	if err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

// UserForToken resolves a bearer token to its account. ErrNotFound when the
// token is unknown or expired.
func (s *Store) UserForToken(ctx context.Context, token string) (*User, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	hash := HashToken(token)
	var userID string
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(active_account_id, user_id) FROM user_sessions
		WHERE token_hash = ? AND expires_at > ?`, hash, Now()).Scan(&userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	// last_seen_at is bookkeeping; a failure to write it is not a failure to
	// authenticate.
	s.db.ExecContext(ctx, `UPDATE user_sessions SET last_seen_at = ? WHERE token_hash = ?`, Now(), hash)
	return s.GetUserByID(ctx, userID)
}

// DeleteSession revokes one token. Unknown tokens are not an error.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_sessions WHERE token_hash = ?`, HashToken(token))
	return err
}

// ── Follow graph ──────────────────────────────────────────────────────────────

// Follow records follower → following. Returns whether anything changed, so a
// repeat follow does not re-notify or re-count.
func (s *Store) Follow(ctx context.Context, followerID, followingID string) (bool, error) {
	if followerID == followingID {
		return false, errors.New("cannot follow yourself")
	}
	var changed bool
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO follows (follower_id, following_id, created_at) VALUES (?, ?, ?)`,
			followerID, followingID, Now())
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		changed = n > 0
		if !changed {
			return nil
		}
		return recountFollows(ctx, tx, followerID, followingID)
	})
	return changed, err
}

// Unfollow removes the edge. Returns whether it existed.
func (s *Store) Unfollow(ctx context.Context, followerID, followingID string) (bool, error) {
	var changed bool
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM follows WHERE follower_id = ? AND following_id = ?`, followerID, followingID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		changed = n > 0
		if !changed {
			return nil
		}
		return recountFollows(ctx, tx, followerID, followingID)
	})
	return changed, err
}

// recountFollows keeps the denormalised counters honest from the edges — the
// job feed-engine gives a trigger in migration 0016.
func recountFollows(ctx context.Context, tx *sql.Tx, followerID, followingID string) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE user_profiles SET following_count = (SELECT COUNT(*) FROM follows WHERE follower_id = ?) WHERE user_id = ?`,
		followerID, followerID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE user_profiles SET follower_count = (SELECT COUNT(*) FROM follows WHERE following_id = ?) WHERE user_id = ?`,
		followingID, followingID)
	return err
}

// IsFollowing reports whether a follows b.
func (s *Store) IsFollowing(ctx context.Context, a, b string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM follows WHERE follower_id = ? AND following_id = ?`, a, b).Scan(&n)
	return n > 0, err
}

// FollowStateFor answers "does viewer follow each of these accounts" in one
// query — feed-engine's FollowStateFor, the single owner of follow state.
func (s *Store) FollowStateFor(ctx context.Context, viewerID string, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if viewerID == "" || len(ids) == 0 {
		return out, nil
	}
	q, args := inClause(ids)
	rows, err := s.db.QueryContext(ctx, `SELECT following_id FROM follows WHERE follower_id = ? AND following_id IN (`+q+`)`,
		append([]any{viewerID}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// SetMuted adds or removes a mute.
func (s *Store) SetMuted(ctx context.Context, muterID, mutedID string, muted bool) error {
	if muted {
		_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO user_mutes (muter_id, muted_id, created_at) VALUES (?, ?, ?)`, muterID, mutedID, Now())
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_mutes WHERE muter_id = ? AND muted_id = ?`, muterID, mutedID)
	return err
}

// SetBlocked adds or removes a block. Blocking also severs follows both ways,
// as the real platform does.
func (s *Store) SetBlocked(ctx context.Context, blockerID, blockedID string, blocked bool) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if !blocked {
			_, err := tx.ExecContext(ctx, `DELETE FROM blocks WHERE blocker_id = ? AND blocked_id = ?`, blockerID, blockedID)
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO blocks (blocker_id, blocked_id, created_at) VALUES (?, ?, ?)`, blockerID, blockedID, Now()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM follows WHERE (follower_id = ? AND following_id = ?) OR (follower_id = ? AND following_id = ?)`,
			blockerID, blockedID, blockedID, blockerID); err != nil {
			return err
		}
		return recountFollows(ctx, tx, blockerID, blockedID)
	})
}

// IsBlocked reports whether blocker has blocked blocked.
func (s *Store) IsBlocked(ctx context.Context, blockerID, blockedID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM blocks WHERE blocker_id = ? AND blocked_id = ?`, blockerID, blockedID).Scan(&n)
	return n > 0, err
}

// SetPinnedWork pins (or with "" unpins) a work on the owner's profile.
func (s *Store) SetPinnedWork(ctx context.Context, userID, workID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE user_profiles SET pinned_work_id = ?, updated_at = ? WHERE user_id = ?`, nullable(workID), Now(), userID)
	return err
}

// AwardXP adds to a PIAL's standing. Realm is derived from XP at read time.
func (s *Store) AwardXP(ctx context.Context, pial string, amount int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE pial_roots SET xp = xp + ? WHERE pial_id = ?`, amount, pial)
	return err
}

// inClause builds "?, ?, ?" and its args for an IN list.
func inClause(ids []string) (string, []any) {
	marks := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		marks[i] = "?"
		args[i] = id
	}
	return strings.Join(marks, ","), args
}

// decodeLinkMap reads a JSON object of string → string, the shape social and
// tip links are stored in. Anything unreadable is an empty map, not an error:
// a profile is still a profile with no links.
func decodeLinkMap(raw string) map[string]string {
	out := map[string]string{}
	if raw == "" {
		return out
	}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

// encodeLinkMap is the inverse, with keys sorted so the same links are the
// same bytes.
func encodeLinkMap(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// canMonetize is feed-engine's CanMonetize: full identity verification, or an
// administrator. The one rule every payout surface asks.
func (u *User) CanMonetize() bool {
	return u != nil && (u.KYCTier == "full" || u.Role == "admin" || u.Role == "founder")
}
