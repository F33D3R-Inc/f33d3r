package db

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/f33d3r/feed-engine/internal/identity"
	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/realm"
)

// YouTube video ID extraction — compiled regexes covering all URL forms.
// Compiled once at package level (never inside the function).
var (
	// youtu.be/VIDEO_ID or youtu.be/VIDEO_ID?si=...
	ytShortenedRe = regexp.MustCompile(`youtu\.be/([A-Za-z0-9_-]{11})`)
	// youtube.com/watch?v=VIDEO_ID (any subdomain, any trailing params)
	ytWatchRe = regexp.MustCompile(`youtube\.com/watch[^"'\s]*[?&]v=([A-Za-z0-9_-]{11})`)
	// youtube.com/embed/VIDEO_ID
	ytEmbedRe = regexp.MustCompile(`youtube\.com/embed/([A-Za-z0-9_-]{11})`)
	// youtube.com/shorts/VIDEO_ID
	ytShortsRe = regexp.MustCompile(`youtube\.com/shorts/([A-Za-z0-9_-]{11})`)
	// youtube.com/live/VIDEO_ID
	ytLiveRe = regexp.MustCompile(`youtube\.com/live/([A-Za-z0-9_-]{11})`)
	// list= playlist ID (any YouTube URL form)
	ytPlaylistRe = regexp.MustCompile(`[?&]list=([A-Za-z0-9_-]+)`)
)

// extractYouTubeID returns the first YouTube video ID found in src, or "".
// Handles: watch?v=, youtu.be/, /shorts/, /live/, /embed/
func extractYouTubeID(src string) string {
	for _, re := range []*regexp.Regexp{ytShortenedRe, ytWatchRe, ytEmbedRe, ytShortsRe, ytLiveRe} {
		if m := re.FindStringSubmatch(src); m != nil {
			return m[1]
		}
	}
	return ""
}

// extractYouTubePlaylistID returns the list= playlist ID from a YouTube URL, or "".
func extractYouTubePlaylistID(src string) string {
	if m := ytPlaylistRe.FindStringSubmatch(src); m != nil {
		return m[1]
	}
	return ""
}

// ── Users ─────────────────────────────────────────────────────────────────────

func UpsertUser(database *sql.DB, handle, emailHash string) (string, error) {
	var id string
	err := database.QueryRow(`
		INSERT INTO users (handle, email_hash)
		VALUES ($1, $2)
		ON CONFLICT (handle) DO UPDATE SET updated_at = NOW()
		RETURNING id
	`, handle, emailHash).Scan(&id)
	return id, err
}

func UpsertUserByHandle(database *sql.DB, handle string) (string, error) {
	return UpsertUser(database, handle, "")
}

func IsHandleTaken(database *sql.DB, handle string) (bool, error) {
	var exists bool
	err := database.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE handle = $1)`, handle).Scan(&exists)
	return exists, err
}

func GetUserByHandle(database *sql.DB, handle string) (*model.User, error) {
	return scanUser(database.QueryRow(userSelectSQL+"WHERE u.handle = $1", handle))
}

// GetUserIDFromPIAL returns the users.id for a given PIAL UUID.
func GetUserIDFromPIAL(database *sql.DB, pialID string) (string, error) {
	var userID string
	err := database.QueryRow(`SELECT id FROM users WHERE pial_id = $1::uuid`, pialID).Scan(&userID)
	return userID, err
}

func GetUserByID(database *sql.DB, id string) (*model.User, error) {
	return scanUser(database.QueryRow(userSelectSQL+"WHERE u.id = $1", id))
}

const userSelectSQL = `
	SELECT u.id, u.handle,
	       COALESCE(NULLIF(TRIM(p.display_name),''), CASE WHEN LENGTH(u.handle)>=40 THEN 'User '||LEFT(u.handle,6) ELSE INITCAP(REPLACE(u.handle,'_',' ')) END),
	       COALESCE(p.bio, ''),
	       COALESCE(p.pronouns, ''),
	       COALESCE(p.location, ''),
	       COALESCE(p.country_code, ''),
	       COALESCE(p.website, ''),
	       COALESCE(p.avatar_url, ''),
	       COALESCE(p.avatar_animated, FALSE),
	       COALESCE(p.header_url, ''),
	       COALESCE(p.theme_id, 'void'),
	       COALESCE(p.accent_hex, ''),
	       COALESCE(p.jung_archetype, ''),
	       COALESCE(p.pinned_track_id, ''),
	       COALESCE(p.social_links::text, '{}'),
	       COALESCE(p.external_tip_links::text, '{}'),
	       COALESCE(p.is_creator, FALSE),
	       (COALESCE(plr.kyc_tier, 'none') = 'full'),
	       COALESCE(p.is_adult_creator, FALSE),
	       COALESCE(p.adult_creator_pending, FALSE),
	       COALESCE(p.follower_count, 0),
	       COALESCE(p.following_count, 0),
	       COALESCE(p.post_count, 0),
	       COALESCE(u.tier, 'free'),
	       COALESCE(p.content_setting, 'default'),
	       COALESCE(u.realm, 1),
	       COALESCE(u.realm_grant, 0),
	       COALESCE(u.xp, 0),
	       COALESCE(u.unread_count, 0),
	       COALESCE(u.role, 'user'),
	       COALESCE(u.pial_id::text, ''),
	       COALESCE(p.official_type, ''),
	       plr.date_of_birth,
	       COALESCE(p.show_birthday, TRUE),
	       COALESCE(p.birthday_md_visibility, 'everyone'),
	       COALESCE(p.birthday_year_visibility, 'only_me'),
	       COALESCE(p.mobile_feed_view, 'standard'),
	       COALESCE(p.show_sensitive, FALSE),
	       COALESCE(p.celebrations_enabled, TRUE),
	       COALESCE(p.is_private, FALSE)
	FROM users u
	LEFT JOIN user_profiles p ON p.user_id = u.id
	LEFT JOIN identity_facts plr ON plr.pial_id = u.pial_id
`

// userRowScanner is *sql.Row or *sql.Rows: scanUser reads one account the same
// way whether it came back alone or as one row of a batch.
type userRowScanner interface {
	Scan(dest ...interface{}) error
}

func scanUser(row userRowScanner) (*model.User, error) {
	u := &model.User{}
	var birthday sql.NullTime
	err := row.Scan(
		&u.ID, &u.Handle,
		&u.DisplayName, &u.Bio, &u.Pronouns,
		&u.Location, &u.CountryCode, &u.Website,
		&u.AvatarURL, &u.AvatarAnimated, &u.HeaderURL,
		&u.ThemeID, &u.AccentHex, &u.JungArchetype,
		&u.PinnedTrackID, &u.SocialLinksRaw, &u.ExternalTipLinksRaw,
		&u.IsCreator, &u.IsVerified,
		&u.IsAdultCreator, &u.AdultCreatorPending,
		&u.FollowerCount, &u.FollowingCount, &u.PostCount,
		&u.Tier, &u.ContentSetting,
		&u.Realm, &u.RealmGrant, &u.XP,
		&u.UnreadCount, &u.Role, &u.PIALID,
		&u.OfficialType,
		&birthday, &u.ShowBirthday,
		&u.BirthdayMdVisibility, &u.BirthdayYearVisibility,
		&u.MobileFeedView,
		&u.ShowSensitive,
		&u.CelebrationsEnabled,
		&u.IsPrivate,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if birthday.Valid {
		u.Birthday = &birthday.Time
	}
	// NOTE: verified status is resolved from PIAL kyc_tier=='full' in userFromRequest —
	// the single system of record for a documented identity. age_verified stays the
	// separate age-gate fact (self-reported DOB is enough for that); the old force-OR
	// (admin/founder/creator/official ⇒ verified) was a drift source and is removed —
	// role-holders get kyc_tier='full' set explicitly on PIAL by the admin/eKYC
	// pipelines instead.
	return u, err
}

// SetBirthdayVisibility updates who can see the birthday on the user's profile.
// The birthday date itself is owned by PIAL and cannot be changed here.
func SetBirthdayVisibility(database *sql.DB, userID string, show bool, mdVis, yearVis string) error {
	validVis := map[string]bool{"everyone": true, "followers": true, "mutual_followers": true, "only_me": true}
	if !validVis[mdVis] {
		mdVis = "everyone"
	}
	if !validVis[yearVis] {
		yearVis = "only_me"
	}
	_, err := database.Exec(`
		INSERT INTO user_profiles (user_id, show_birthday, birthday_md_visibility, birthday_year_visibility, updated_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (user_id) DO UPDATE SET
		    show_birthday              = EXCLUDED.show_birthday,
		    birthday_md_visibility     = EXCLUDED.birthday_md_visibility,
		    birthday_year_visibility   = EXCLUDED.birthday_year_visibility,
		    updated_at                 = NOW()
	`, userID, show, mdVis, yearVis)
	return err
}

// ── Org membership queries ────────────────────────────────────────────────────

func GetUserOrgMemberships(database *sql.DB, userID string) []model.OrgMembership {
	rows, err := database.Query(`
		SELECT om.id, om.status, om.is_primary, om.initiated_by, om.created_at,
		       orgu.id, orgu.handle,
		       COALESCE(NULLIF(TRIM(orgp.display_name),''), orgu.handle),
		       COALESCE(orgp.avatar_url,''), COALESCE(orgp.official_type,'')
		FROM org_memberships om
		JOIN users orgu ON orgu.id = om.org_user_id
		LEFT JOIN user_profiles orgp ON orgp.user_id = om.org_user_id
		WHERE om.user_id = $1
		ORDER BY om.is_primary DESC, om.created_at ASC
	`, userID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []model.OrgMembership
	for rows.Next() {
		var m model.OrgMembership
		if err := rows.Scan(&m.ID, &m.Status, &m.IsPrimary, &m.InitiatedBy, &m.CreatedAt,
			&m.Org.ID, &m.Org.Handle, &m.Org.DisplayName, &m.Org.AvatarURL, &m.Org.OrgType); err == nil {
			out = append(out, m)
		}
	}
	return out
}

func GetUserApprovedOrgCount(database *sql.DB, userID string) int {
	var n int
	database.QueryRow(`SELECT COUNT(*) FROM org_memberships WHERE user_id=$1 AND status='approved'`, userID).Scan(&n)
	return n
}

func OrgMembershipExists(database *sql.DB, userID, orgUserID string) bool {
	var n int
	database.QueryRow(`SELECT COUNT(*) FROM org_memberships WHERE user_id=$1 AND org_user_id=$2`, userID, orgUserID).Scan(&n)
	return n > 0
}

func createOrgMembership(q DBTX, userID, orgUserID, initiatedBy string) (sql.Result, error) {
	return q.Exec(`
		INSERT INTO org_memberships (user_id, org_user_id, status, initiated_by)
		VALUES ($1, $2,
		  CASE WHEN $3='org' THEN 'pending_org' ELSE 'pending_employee' END,
		  $3)
		ON CONFLICT (user_id, org_user_id) DO NOTHING
	`, userID, orgUserID, initiatedBy)
}

func GetOrgMembership(database *sql.DB, membershipID string) (model.OrgMembership, error) {
	var m model.OrgMembership
	err := database.QueryRow(`
		SELECT om.id, om.user_id, om.org_user_id, om.status, om.is_primary, om.initiated_by, om.created_at,
		       orgu.id, orgu.handle,
		       COALESCE(NULLIF(TRIM(orgp.display_name),''), orgu.handle),
		       COALESCE(orgp.avatar_url,''), COALESCE(orgp.official_type,'')
		FROM org_memberships om
		JOIN users orgu ON orgu.id = om.org_user_id
		LEFT JOIN user_profiles orgp ON orgp.user_id = om.org_user_id
		WHERE om.id = $1
	`, membershipID).Scan(&m.ID, new(string), new(string), &m.Status, &m.IsPrimary, &m.InitiatedBy, &m.CreatedAt,
		&m.Org.ID, &m.Org.Handle, &m.Org.DisplayName, &m.Org.AvatarURL, &m.Org.OrgType)
	return m, err
}

func GetOrgPendingApplications(database *sql.DB, orgUserID string) []model.OrgApplicationRow {
	return orgMemberRows(database, orgUserID, "pending_employee")
}
func GetOrgPendingInvites(database *sql.DB, orgUserID string) []model.OrgApplicationRow {
	return orgMemberRows(database, orgUserID, "pending_org")
}
func GetOrgApprovedMembers(database *sql.DB, orgUserID string) []model.OrgApplicationRow {
	return orgMemberRows(database, orgUserID, "approved")
}

func orgMemberRows(database *sql.DB, orgUserID, status string) []model.OrgApplicationRow {
	rows, err := database.Query(`
		SELECT om.id, om.status, om.initiated_by, om.created_at,
		       u.id, u.handle,
		       COALESCE(NULLIF(TRIM(p.display_name),''), u.handle),
		       COALESCE(p.avatar_url,''), (COALESCE(plr.kyc_tier,'none') = 'full')
		FROM org_memberships om
		JOIN users u ON u.id = om.user_id
		LEFT JOIN user_profiles p ON p.user_id = om.user_id
		LEFT JOIN identity_facts plr ON plr.pial_id = u.pial_id
		WHERE om.org_user_id = $1 AND om.status = $2
		ORDER BY om.created_at ASC
	`, orgUserID, status)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []model.OrgApplicationRow
	for rows.Next() {
		var r model.OrgApplicationRow
		if err := rows.Scan(&r.MembershipID, &r.Status, &r.InitiatedBy, &r.CreatedAt,
			&r.UserID, &r.Handle, &r.DisplayName, &r.AvatarURL, &r.IsVerified); err == nil {
			out = append(out, r)
		}
	}
	return out
}

func SetMembershipStatus(database *sql.DB, membershipID, status string) error {
	_, err := database.Exec(`UPDATE org_memberships SET status=$1, updated_at=NOW() WHERE id=$2`, status, membershipID)
	return err
}

func SetPrimaryOrg(database *sql.DB, userID, membershipID string) error {
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE org_memberships SET is_primary=FALSE, updated_at=NOW() WHERE user_id=$1`, userID)
	if err != nil {
		tx.Rollback()
		return err
	}
	_, err = tx.Exec(`UPDATE org_memberships SET is_primary=TRUE, updated_at=NOW() WHERE id=$1 AND user_id=$2 AND status='approved'`, membershipID, userID)
	if err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func SearchOrgs(database *sql.DB, query string) []model.OrgInfo {
	if query == "" {
		return nil
	}
	rows, err := database.Query(`
		SELECT u.id, u.handle,
		       COALESCE(NULLIF(TRIM(p.display_name),''), u.handle),
		       COALESCE(p.avatar_url,''), COALESCE(p.official_type,'')
		FROM users u
		JOIN user_profiles p ON p.user_id = u.id
		LEFT JOIN identity_facts plr ON plr.pial_id = u.pial_id
		WHERE p.official_type IN ('business','government')
		  AND (u.handle ILIKE '%'||$1||'%' OR p.display_name ILIKE '%'||$1||'%')
		  AND COALESCE(plr.age_verified, FALSE) = TRUE
		ORDER BY p.follower_count DESC
		LIMIT 20
	`, query)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []model.OrgInfo
	for rows.Next() {
		var o model.OrgInfo
		if err := rows.Scan(&o.ID, &o.Handle, &o.DisplayName, &o.AvatarURL, &o.OrgType); err == nil {
			out = append(out, o)
		}
	}
	return out
}

func GetOrgByHandle(database *sql.DB, handle string) (*model.OrgInfo, error) {
	var o model.OrgInfo
	err := database.QueryRow(`
		SELECT u.id, u.handle,
		       COALESCE(NULLIF(TRIM(p.display_name),''), u.handle),
		       COALESCE(p.avatar_url,''), COALESCE(p.official_type,'')
		FROM users u
		JOIN user_profiles p ON p.user_id = u.id
		WHERE u.handle = $1 AND p.official_type IN ('business','government')
	`, handle).Scan(&o.ID, &o.Handle, &o.DisplayName, &o.AvatarURL, &o.OrgType)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// ── Org verification applications ─────────────────────────────────────────────

func CreateOrgVerificationApplication(database *sql.DB, userID, orgType, orgName, orgWebsite, description, evidenceURL string) error {
	_, err := database.Exec(`
		INSERT INTO org_verification_applications (user_id, org_type, org_name, org_website, description, evidence_url)
		VALUES ($1,$2,$3,$4,$5,$6)
	`, userID, orgType, orgName, orgWebsite, description, evidenceURL)
	return err
}

func GetPendingOrgVerifications(database *sql.DB) []model.OrgVerificationApplication {
	rows, err := database.Query(`
		SELECT a.id, a.user_id, u.handle,
		       COALESCE(NULLIF(TRIM(p.display_name),''), u.handle),
		       COALESCE(p.avatar_url,''),
		       a.org_type, a.org_name, a.org_website, a.description, a.evidence_url,
		       a.status, a.admin_notes, a.created_at
		FROM org_verification_applications a
		JOIN users u ON u.id = a.user_id
		LEFT JOIN user_profiles p ON p.user_id = a.user_id
		WHERE a.status = 'pending'
		ORDER BY a.created_at ASC
	`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []model.OrgVerificationApplication
	for rows.Next() {
		var a model.OrgVerificationApplication
		if err := rows.Scan(&a.ID, &a.UserID, &a.UserHandle, &a.DisplayName, &a.AvatarURL,
			&a.OrgType, &a.OrgName, &a.OrgWebsite, &a.Description, &a.EvidenceURL,
			&a.Status, &a.AdminNotes, &a.CreatedAt); err == nil {
			out = append(out, a)
		}
	}
	return out
}

func ApproveOrgVerification(database *sql.DB, appID, adminUserID, orgType string) error {
	return WithTx(database, func(tx *sql.Tx) error {
		var userID string
		if err := tx.QueryRow(`UPDATE org_verification_applications SET status='approved', reviewed_at=NOW(), reviewed_by=$1 WHERE id=$2 RETURNING user_id`, adminUserID, appID).Scan(&userID); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO user_profiles (user_id, official_type) VALUES ($1,$2) ON CONFLICT (user_id) DO UPDATE SET official_type=$2, updated_at=NOW()`, userID, orgType); err != nil {
			return err
		}
		return enqueueOrgIdentityConfirmation(tx, `SELECT COALESCE(pial_id::text,'') FROM users WHERE id = $1`, userID)
	})
}

// enqueueOrgIdentityConfirmation records, in the badge's own transaction, the
// identity confirmation an approved organisation carries: the verified badge
// is kyc_tier 'full', the same bar as eKYC and an admin verify, and the badge
// must never stand without it. elohim-veni owns the fact; the identity
// command relay delivers it after commit.
func enqueueOrgIdentityConfirmation(tx DBTX, lookup string, arg interface{}) error {
	var pialID string
	if err := tx.QueryRow(lookup, arg).Scan(&pialID); err != nil {
		return fmt.Errorf("org verification: account lookup: %w", err)
	}
	if pialID == "" {
		return fmt.Errorf("org verification: account %v has no identity", arg)
	}
	tier, verified, locked := model.KYCTierFull, true, true
	return EnqueueIdentityCommand(tx, identity.CommandFacts, pialID, identity.FactsUpdate{
		KYCTier: &tier, AgeVerified: &verified, DOBLocked: &locked, Source: "org_verification",
	})
}

func RejectOrgVerification(database *sql.DB, appID, adminUserID, notes string) error {
	_, err := database.Exec(`UPDATE org_verification_applications SET status='rejected', admin_notes=$1, reviewed_at=NOW(), reviewed_by=$2 WHERE id=$3`, notes, adminUserID, appID)
	return err
}

// GetAllVerifiedOrgs returns all accounts with an official_type badge.
func GetAllVerifiedOrgs(database *sql.DB) []model.OrgInfo {
	rows, err := database.Query(`
		SELECT u.id, u.handle,
		       COALESCE(NULLIF(TRIM(p.display_name),''), u.handle),
		       COALESCE(p.avatar_url,''), COALESCE(p.official_type,'')
		FROM users u
		JOIN user_profiles p ON p.user_id = u.id
		WHERE p.official_type IN ('business','government')
		ORDER BY p.official_type, u.handle
	`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []model.OrgInfo
	for rows.Next() {
		var o model.OrgInfo
		rows.Scan(&o.ID, &o.Handle, &o.DisplayName, &o.AvatarURL, &o.OrgType)
		out = append(out, o)
	}
	return out
}

// GrantOrgBadge sets official_type on an account by handle and records the
// identity confirmation the badge carries, in one transaction.
func GrantOrgBadge(database *sql.DB, handle, orgType string) error {
	return WithTx(database, func(tx *sql.Tx) error {
		res, err := tx.Exec(`
			INSERT INTO user_profiles (user_id, official_type)
			SELECT id, $1 FROM users WHERE handle = $2
			ON CONFLICT (user_id) DO UPDATE
			  SET official_type = EXCLUDED.official_type,
			      updated_at    = NOW()`,
			orgType, handle)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return fmt.Errorf("org badge for @%s: no such account (%d rows, %v)", handle, n, err)
		}
		return enqueueOrgIdentityConfirmation(tx, `SELECT COALESCE(pial_id::text,'') FROM users WHERE handle = $1`, handle)
	})
}

// RevokeOrgBadge clears official_type from a user by handle.
func RevokeOrgBadge(database *sql.DB, handle string) error {
	_, err := database.Exec(`
		UPDATE user_profiles SET official_type = '', updated_at = NOW()
		WHERE user_id = (SELECT id FROM users WHERE handle = $1)`, handle)
	return err
}

// SetMobileFeedView persists the user's mobile feed preference ("standard" or "reels").
func SetMobileFeedView(database *sql.DB, userID, view string) error {
	if view != "standard" && view != "reels" {
		return fmt.Errorf("invalid mobile_feed_view value: %q", view)
	}
	_, err := database.Exec(`
		INSERT INTO user_profiles (user_id, mobile_feed_view, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (user_id) DO UPDATE SET
		    mobile_feed_view = EXCLUDED.mobile_feed_view,
		    updated_at       = NOW()
	`, userID, view)
	return err
}

// SetShowSensitive persists the 18+ "show sensitive content" (gore) preference.
func SetShowSensitive(database *sql.DB, userID string, show bool) error {
	_, err := database.Exec(`
		INSERT INTO user_profiles (user_id, show_sensitive, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (user_id) DO UPDATE SET
		    show_sensitive = EXCLUDED.show_sensitive,
		    updated_at     = NOW()
	`, userID, show)
	return err
}

// SetCelebrationsEnabled persists the per-user celebration opt-out (TRUE = show
// admin-scheduled celebrations, the default; FALSE = this user hides them).
func SetCelebrationsEnabled(database *sql.DB, userID string, on bool) error {
	_, err := database.Exec(`
		INSERT INTO user_profiles (user_id, celebrations_enabled, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (user_id) DO UPDATE SET
		    celebrations_enabled = EXCLUDED.celebrations_enabled,
		    updated_at           = NOW()
	`, userID, on)
	return err
}

// SetContentSetting persists the porn-axis content_setting (safe_mode|default|
// adult_enabled). Same validated value set as onboarding; caller is responsible
// for the minor / age-verification guard before allowing adult_enabled.
func SetContentSetting(database *sql.DB, userID, setting string) error {
	allowed := map[string]bool{"safe_mode": true, "default": true, "adult_enabled": true}
	if !allowed[setting] {
		setting = "default"
	}
	_, err := database.Exec(`
		INSERT INTO user_profiles (user_id, content_setting, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (user_id) DO UPDATE SET
		    content_setting = EXCLUDED.content_setting,
		    updated_at      = NOW()
	`, userID, setting)
	return err
}

// SetIsPrivate persists the account privacy flag. TRUE = profile and posts are
// hidden from public (logged-out / non-follower) lookup; FALSE = public (default).
func SetIsPrivate(database *sql.DB, userID string, private bool) error {
	_, err := database.Exec(`
		INSERT INTO user_profiles (user_id, is_private, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (user_id) DO UPDATE SET
		    is_private = EXCLUDED.is_private,
		    updated_at = NOW()
	`, userID, private)
	return err
}

// SetUnreadCount records the unread count Herald announced, for rendering pages
// without a call to Herald.
func SetUnreadCount(database *sql.DB, userID string, count int) error {
	_, err := database.Exec(`UPDATE users SET unread_count = $2 WHERE id = $1`, userID, count)
	return err
}

func SaveProfile(database *sql.DB, p *model.ProfileSave) error {
	socialJSON := p.SocialLinksJSON
	if socialJSON == "" {
		socialJSON = "{}"
	}
	tipLinksJSON := p.ExternalTipLinksJSON
	if tipLinksJSON == "" {
		tipLinksJSON = "{}"
	}
	_, err := database.Exec(`
		INSERT INTO user_profiles
		    (user_id, display_name, bio, pronouns, location, country_code, website,
		     avatar_url, avatar_animated, header_url,
		     theme_id, accent_hex, jung_archetype,
		     pinned_track_id, social_links, external_tip_links, is_adult_creator, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15::jsonb,$16::jsonb,$17,NOW())
		ON CONFLICT (user_id) DO UPDATE SET
		    display_name        = EXCLUDED.display_name,
		    bio                 = EXCLUDED.bio,
		    pronouns            = EXCLUDED.pronouns,
		    location            = EXCLUDED.location,
		    country_code        = CASE WHEN EXCLUDED.country_code = '' THEN user_profiles.country_code ELSE EXCLUDED.country_code END,
		    website             = EXCLUDED.website,
		    avatar_url          = CASE WHEN EXCLUDED.avatar_url = '' THEN user_profiles.avatar_url ELSE EXCLUDED.avatar_url END,
		    avatar_animated     = EXCLUDED.avatar_animated,
		    header_url          = CASE WHEN EXCLUDED.header_url = '' THEN user_profiles.header_url ELSE EXCLUDED.header_url END,
		    theme_id            = EXCLUDED.theme_id,
		    accent_hex          = EXCLUDED.accent_hex,
		    jung_archetype      = EXCLUDED.jung_archetype,
		    pinned_track_id     = EXCLUDED.pinned_track_id,
		    social_links        = EXCLUDED.social_links,
		    external_tip_links  = EXCLUDED.external_tip_links,
		    is_adult_creator    = EXCLUDED.is_adult_creator,
		    updated_at          = NOW()
	`,
		p.UserID, p.DisplayName, p.Bio, p.Pronouns,
		p.Location, p.CountryCode, p.Website,
		p.AvatarURL, p.AvatarAnimated, p.HeaderURL,
		p.ThemeID, p.AccentHex, p.JungArchetype,
		p.PinnedTrackID, socialJSON, tipLinksJSON, p.IsAdultCreator,
	)
	return err
}

// AwardXP adds xp_delta to a user's XP and updates their realm.
//
// It delegates to realm.AwardXP rather than restating the thresholds in SQL.
// Two implementations of "what realm is this XP worth" is exactly how a ring
// and a badge end up disagreeing, and how a realm change happens without the
// realm index that draws the rings ever hearing about it.
func AwardXP(database *sql.DB, userID, reason string, delta int) {
	realm.AwardXP(database, userID, reason, "", delta)
}

// ── Posts ─────────────────────────────────────────────────────────────────────

// NOTE: all content creation lives in InsertWork (works table). Polls included:
// the compose surface builds a Malkuth-signed work carrying poll_options and
// poll_ends_at, so there is one create path and one table.

// CastPollVote records a voter's ballot on a work's poll. Returns (alreadyVoted, error).
//
// work_poll_votes is the only place a ballot exists — one row per (work, voter),
// enforced by the primary key, with tallies counted from those rows at read time.
// There is no counter to increment and therefore none to drift.
func CastPollVote(database *sql.DB, workID, voterID string, optionIdx int) (bool, error) {
	res, err := database.Exec(`
		INSERT INTO work_poll_votes (work_id, voter_id, option_idx)
		VALUES ($1::uuid, $2::uuid, $3)
		ON CONFLICT (work_id, voter_id) DO NOTHING
	`, workID, voterID, optionIdx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 0, nil
}

// GetPollResults returns the rendered ballot for one work — the tallies, the
// viewer's own vote, and every display value the ballot facet draws.
//
// This is the single-poll read used right after a vote. It builds the same
// model.Poll the feed enrichment builds and hands it to the same
// model.Poll.Project, so the ballot swapped in over a vote is identical to the
// ballot the feed rendered.
func GetPollResults(database *sql.DB, postID, voterID string) (*model.Poll, error) {
	var options pq.StringArray
	var endsAt *time.Time
	err := database.QueryRow(`SELECT poll_options, poll_ends_at FROM works WHERE id = $1::uuid`, postID).
		Scan(&options, &endsAt)
	if err != nil || options == nil {
		return nil, err
	}

	poll := &model.Poll{
		Options:  []string(options),
		Votes:    make([]int, len(options)),
		UserVote: -1,
		EndsAt:   endsAt,
	}

	rows, err := database.Query(`
		SELECT option_idx, COUNT(*)::int FROM work_poll_votes WHERE work_id = $1::uuid GROUP BY option_idx
	`, postID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var idx, count int
		if err := rows.Scan(&idx, &count); err != nil {
			return nil, err
		}
		if idx >= 0 && idx < len(poll.Votes) {
			poll.Votes[idx] = count
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if voterID != "" {
		var voted int
		switch err := database.QueryRow(
			`SELECT option_idx FROM work_poll_votes WHERE work_id = $1::uuid AND voter_id = $2::uuid`,
			postID, voterID).Scan(&voted); err {
		case nil:
			poll.UserVote = voted
		case sql.ErrNoRows:
			// viewer has not voted — UserVote stays -1
		default:
			return nil, err
		}
	}

	poll.Project(time.Now())
	return poll, nil
}

// Canonical media records who uploaded an asset — the uploader registry the
// media gate decides from (media_assets.go). It records that as an account and a
// PIAL and nothing else.
//
// The creator_handle column is deliberately left unwritten. A handle is
// registry-brain's grant, transferable and reclaimable, so a handle copied into
// a content row is a snapshot of who held it at insert time — it goes stale
// silently the moment the handle moves, and nothing in this brain would ever
// learn that it had. The durable name for the same person is the PIAL, which is
// already on the row, and the handle it currently answers to is resolved from
// the naming plane at the point of display.

// ── Follows ───────────────────────────────────────────────────────────────────

// uuidShapeRe matches the canonical 8-4-4-4-12 UUID text form. Viewer identifiers
// reaching this package are not always accounts: the anonymous viewer is the
// "demo_user" sentinel and dev builds carry non-UUID handles. Sending one of those
// to Postgres as a uuid parameter aborts the whole statement, which is how a
// logged-out visitor used to get an EMPTY followers list instead of the list with
// no follow marks. Everything viewer-relative below tests the shape first and
// degrades to "this viewer follows nobody".
var uuidShapeRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// FollowStateFor returns, for one viewer, which of the given target user ids they
// follow. One query, no N+1, and the ONLY definition of "does this viewer follow
// that person" in the service — profile headers, work cards, follower/following
// lists, the who-to-follow rail and the POST /events response all resolve follow
// state through here, so no two surfaces can disagree about it.
//
// A viewer with no account, an empty target set, or a target set with no real
// account ids resolves to an empty map: nobody is followed. The map only ever
// contains ids that ARE followed, so a missing key reads false.
func FollowStateFor(database *sql.DB, viewerID string, targetIDs []string) (map[string]bool, error) {
	out := make(map[string]bool, len(targetIDs))
	if database == nil || !uuidShapeRe.MatchString(viewerID) || len(targetIDs) == 0 {
		return out, nil
	}
	// De-duplicate and drop anything that is not an account id; a work feed
	// commonly repeats the same author many times.
	unique := make([]string, 0, len(targetIDs))
	seen := make(map[string]bool, len(targetIDs))
	for _, id := range targetIDs {
		if id == "" || seen[id] || !uuidShapeRe.MatchString(id) {
			continue
		}
		seen[id] = true
		unique = append(unique, id)
	}
	if len(unique) == 0 {
		return out, nil
	}
	rows, err := database.Query(`
		SELECT following_id::text
		FROM follows
		WHERE follower_id = $1::uuid
		  AND following_id = ANY($2::uuid[])`, viewerID, pq.Array(unique))
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return out, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// UnfollowUser removes follower → following and returns whether a follow was
// actually removed.
//
// The counters are NOT touched here. user_profiles.follower_count and
// following_count are maintained by the trg_follows_counters trigger (migration
// 0016), because this function is not the only thing that writes follows: the
// follower_id/following_id foreign keys are ON DELETE CASCADE, so deleting an
// account removes its follow edges inside the database with no Go code running.
// A trigger fires for cascades too, so it cannot be bypassed by any writer.
//
// The unfollow and the ranking signal that takes the follow back commit together.
func UnfollowUser(database *sql.DB, followerID, followingID string) (bool, error) {
	changed := false
	err := WithTx(database, func(tx *sql.Tx) error {
		res, err := tx.Exec(`
			DELETE FROM follows
			WHERE follower_id = $1::uuid AND following_id = $2::uuid`, followerID, followingID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		changed = n > 0
		if !changed {
			return nil
		}
		return EnqueueRankingSignal(tx, RankingSignal{
			Action: RankUnfollow, Viewer: followerID, TargetAccount: followingID,
		})
	})
	return changed, err
}

// FollowListEntry is a minimal user row for the followers/following list.
type FollowListEntry struct {
	ID            string
	Handle        string
	DisplayName   string
	AvatarURL     string
	ViewerFollows bool // viewer already follows this user — drives FOLLOW vs UNFOLLOW button
}

// markViewerFollows fills ViewerFollows on a list of rows from the one follow-state
// owner. Kept out of the list SQL on purpose: the per-row EXISTS subquery it
// replaces made the whole query viewer-typed, so an anonymous viewer ("demo_user")
// failed the uuid cast and the visitor saw an empty list instead of an unmarked one.
func markViewerFollows(database *sql.DB, viewerID string, entries []FollowListEntry) error {
	if len(entries) == 0 {
		return nil
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	state, err := FollowStateFor(database, viewerID, ids)
	if err != nil {
		return err
	}
	for i := range entries {
		entries[i].ViewerFollows = state[entries[i].ID]
	}
	return nil
}

func GetFollowers(database *sql.DB, userID, viewerID string, limit int) ([]FollowListEntry, error) {
	rows, err := database.Query(`
		SELECT u.id::text,
		       u.handle,
		       COALESCE(NULLIF(TRIM(p.display_name),''), CASE WHEN LENGTH(u.handle)>=40 THEN 'User '||LEFT(u.handle,6) ELSE INITCAP(REPLACE(u.handle,'_',' ')) END),
		       COALESCE(p.avatar_url, '')
		FROM follows f
		JOIN users u ON u.id = f.follower_id
		LEFT JOIN user_profiles p ON p.user_id = u.id
		WHERE f.following_id = $1
		ORDER BY f.created_at DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FollowListEntry
	for rows.Next() {
		var e FollowListEntry
		if err := rows.Scan(&e.ID, &e.Handle, &e.DisplayName, &e.AvatarURL); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, markViewerFollows(database, viewerID, out)
}

func GetFollowing(database *sql.DB, userID, viewerID string, limit int) ([]FollowListEntry, error) {
	rows, err := database.Query(`
		SELECT u.id::text,
		       u.handle,
		       COALESCE(NULLIF(TRIM(p.display_name),''), CASE WHEN LENGTH(u.handle)>=40 THEN 'User '||LEFT(u.handle,6) ELSE INITCAP(REPLACE(u.handle,'_',' ')) END),
		       COALESCE(p.avatar_url, '')
		FROM follows f
		JOIN users u ON u.id = f.following_id
		LEFT JOIN user_profiles p ON p.user_id = u.id
		WHERE f.follower_id = $1
		ORDER BY f.created_at DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FollowListEntry
	for rows.Next() {
		var e FollowListEntry
		if err := rows.Scan(&e.ID, &e.Handle, &e.DisplayName, &e.AvatarURL); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, markViewerFollows(database, viewerID, out)
}

// IsFollowing answers the single-target question through the one follow-state
// owner, so a page that asks about one person and a page that asks about fifty
// cannot disagree.
func IsFollowing(database *sql.DB, followerID, followingID string) (bool, error) {
	state, err := FollowStateFor(database, followerID, []string{followingID})
	if err != nil {
		return false, err
	}
	return state[followingID], nil
}

// ── Mention helpers ───────────────────────────────────────────────────────────

// AccountsByPIAL maps PIALs to the local accounts that hold them.
//
// This is the local half of resolving an @mention, and it deliberately takes
// PIALs rather than handles. Turning "@someone" into an identity is registry-
// brain's grant resolved through Manhattan, not a LOWER(users.handle) join
// against this brain's copy of that grant — a copy which answers with whoever
// held the handle when the row was written. What is left for this brain to
// answer is the only part it actually owns: which of its own accounts belongs to
// an identity it has already been told the name of.
//
// A PIAL with no local account is simply absent from the result.
func AccountsByPIAL(database *sql.DB, pials []string) (map[string]MentionTarget, error) {
	if database == nil || len(pials) == 0 {
		return map[string]MentionTarget{}, nil
	}
	rows, err := database.Query(`
		SELECT u.pial_id::text,
		       u.id::text,
		       COALESCE(NULLIF(TRIM(p.display_name),''), INITCAP(REPLACE(u.handle,'_',' ')))
		FROM users u
		LEFT JOIN user_profiles p ON p.user_id = u.id
		WHERE u.pial_id::text = ANY($1)
	`, pq.Array(pials))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]MentionTarget, len(pials))
	for rows.Next() {
		var t MentionTarget
		var pial string
		if err := rows.Scan(&pial, &t.UserID, &t.DisplayName); err != nil {
			return nil, err
		}
		out[pial] = t
	}
	return out, rows.Err()
}

// MentionTarget is one account a mention can be delivered to.
type MentionTarget struct {
	UserID      string
	DisplayName string
}

// ── Viewer interaction state ─────────────────────────────────────────────────
//
// Likes, dislikes, reposts and bookmarks are rows in work_reactions, written by
// the /events reaction handler and counted in worksSelectSQL at read time. The
// per-viewer flags come back on the same SELECT (EnrichWorksWithReactions), so
// there is no second query and no second owner.
//
// The bookmarks / post_likes / user_dislikes / post_metrics helpers that used to
// live here were deleted in the posts→works cutover. They wrote to tables the
// render never read, so every one of them returned success while changing
// nothing a viewer could see — a failure that looked exactly like a feature.

// NOTE: content deletion is SoftDeleteWork (works.deleted_at), reached through
// the work_delete event. The hard DELETE against the posts table that used to
// live here is gone with the table.

// ── Sessions ──────────────────────────────────────────────────────────────────

// ── Feedback events ───────────────────────────────────────────────────────────

// InsertFeedbackEvents persists a batch of feedback events to the audit table.
// Called fire-and-forget from the feedbackAPI handler before forwarding to AethyrRank.
func InsertFeedbackEvents(database *sql.DB, userID, sessionID, surface string, events []model.AethyrFeedbackEvent) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`
		INSERT INTO feedback_events (user_id, session_id, surface, content_id, event_type, position, dwell_ms, is_explore)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT DO NOTHING
	`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, ev := range events {
		dwellMs := int64(0)
		if ev.DwellMs != nil {
			dwellMs = *ev.DwellMs
		}
		if _, err := stmt.Exec(userID, sessionID, surface, ev.ContentID, ev.EventType, ev.PositionAtDisplay, dwellMs, ev.ExplorationSlot); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func timeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return t.Format("Jan 2")
	}
}

// ── Trending tags (real data from posts) ──────────────────────────────────────

type TrendingTag struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// SearchTags returns hashtags matching a prefix, most-used first — powers the compose
// hashtag autocomplete. Prefix match is case-insensitive. Same source as GetTrendingTags.
func SearchTags(database *sql.DB, prefix string, limit int) ([]TrendingTag, error) {
	// Tags live on works (posts table is legacy/empty after the works migration).
	rows, err := database.Query(`
		SELECT tag, COUNT(*) AS cnt
		FROM works, unnest(tags) AS tag
		WHERE tag ILIKE $1 || '%'
		  AND tag <> ''
		  AND deleted_at IS NULL
		  AND (expires_at IS NULL OR expires_at > NOW())
		GROUP BY tag
		ORDER BY cnt DESC
		LIMIT $2
	`, prefix, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrendingTag
	for rows.Next() {
		var t TrendingTag
		if err := rows.Scan(&t.Tag, &t.Count); err != nil {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

func GetTrendingTags(database *sql.DB, limit int) ([]TrendingTag, error) {
	// Tags live on works (posts table is legacy/empty after the works migration).
	rows, err := database.Query(`
		SELECT tag, COUNT(*) AS cnt
		FROM works, unnest(tags) AS tag
		WHERE created_at > NOW() - INTERVAL '7 days'
		  AND tag <> ''
		  AND deleted_at IS NULL
		  AND (expires_at IS NULL OR expires_at > NOW())
		GROUP BY tag
		ORDER BY cnt DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrendingTag
	for rows.Next() {
		var t TrendingTag
		if err := rows.Scan(&t.Tag, &t.Count); err != nil {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// ── User search (@ mention autocomplete) ──────────────────────────────────────

type MentionResult struct {
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name"`
	AvatarURL   string `json:"avatar_url"`
	IsVerified  bool   `json:"is_verified"`
}

func SearchHandles(database *sql.DB, prefix string, limit int) ([]MentionResult, error) {
	return SearchHandlesFiltered(database, prefix, limit, false, false)
}

// SearchHandlesFiltered searches users by handle/display name with optional minor isolation.
// When viewerIsMinor=true, only minor accounts are returned.
// When viewerIsMinor=false, minor accounts are excluded from results.
func SearchHandlesFiltered(database *sql.DB, prefix string, limit int, viewerIsMinor bool, showAdultCreators bool) ([]MentionResult, error) {
	minorClause := ""
	if viewerIsMinor {
		minorClause = "AND COALESCE(plr.is_minor, FALSE) = TRUE"
	} else {
		minorClause = "AND COALESCE(plr.is_minor, FALSE) = FALSE"
	}
	adultClause := ""
	if !showAdultCreators {
		adultClause = "AND COALESCE(pr.is_adult_creator, FALSE) = FALSE"
	}
	rows, err := database.Query(`
		SELECT u.handle,
		       COALESCE(NULLIF(TRIM(pr.display_name),''), CASE WHEN LENGTH(u.handle)>=40 THEN 'User '||LEFT(u.handle,6) ELSE INITCAP(REPLACE(u.handle,'_',' ')) END),
		       COALESCE(pr.avatar_url, ''),
		       (COALESCE(plr.kyc_tier, 'none') = 'full')
		FROM users u
		LEFT JOIN user_profiles pr ON pr.user_id = u.id
		LEFT JOIN identity_facts plr ON plr.pial_id = u.pial_id
		WHERE (u.handle ILIKE $1 OR COALESCE(pr.display_name,'') ILIKE $2)
		`+minorClause+` `+adultClause+`
		ORDER BY COALESCE(pr.follower_count, 0) DESC, u.handle
		LIMIT $3
	`, prefix+"%", prefix+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MentionResult
	for rows.Next() {
		var m MentionResult
		if err := rows.Scan(&m.Handle, &m.DisplayName, &m.AvatarURL, &m.IsVerified); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// SetCreatorMode sets is_creator on user_profiles for the given user.
func SetCreatorMode(database *sql.DB, userID string, enabled bool) error {
	_, err := database.Exec(
		`UPDATE user_profiles SET is_creator = $1 WHERE user_id = $2`,
		enabled, userID,
	)
	return err
}

// ── Blocks ────────────────────────────────────────────────────────────────────

// BlockUser records a block. Idempotent.
func BlockUser(database *sql.DB, blockerID, blockedID string) error {
	_, err := database.Exec(
		`INSERT INTO blocks (blocker_id, blocked_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		blockerID, blockedID,
	)
	return err
}

// UnblockUser removes a block.
func UnblockUser(database *sql.DB, blockerID, blockedID string) error {
	_, err := database.Exec(
		`DELETE FROM blocks WHERE blocker_id = $1 AND blocked_id = $2`,
		blockerID, blockedID,
	)
	return err
}

// GetBlockedUserIDs returns a set of user IDs that the viewer has blocked or
// who have blocked the viewer. Both directions are invisible to each other.
func GetBlockedUserIDs(database *sql.DB, viewerID string) (map[string]bool, error) {
	rows, err := database.Query(`
		SELECT blocked_id  FROM blocks WHERE blocker_id = $1
		UNION
		SELECT blocker_id  FROM blocks WHERE blocked_id = $1
	`, viewerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out, nil
}

// ── Reply restrictions ────────────────────────────────────────────────────────

// ErrWorkNotOwned reports a change to a work that does not exist, is deleted,
// or belongs to somebody else.
var ErrWorkNotOwned = errors.New("work not found or not owned by user")

// SetCommentGating changes who may reply to a work its author owns. gating must
// be in CommentGatings; the column's CHECK refuses anything else.
func SetCommentGating(database *sql.DB, workID, userID, gating string) error {
	if !ValidCommentGating(gating) {
		return fmt.Errorf("comment gating %q is not in the vocabulary", gating)
	}
	res, err := database.Exec(`
		UPDATE works SET comment_gating = $1
		 WHERE id = $2::uuid AND author_id = $3::uuid AND deleted_at IS NULL`,
		gating, workID, userID)
	if err != nil {
		return fmt.Errorf("setting comment gating of work %s: %w", workID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("setting comment gating of work %s: %w", workID, err)
	}
	if n == 0 {
		return ErrWorkNotOwned
	}
	return nil
}

// ── Mutes ─────────────────────────────────────────────────────────────────────

// MuteUser silently hides another user's posts from the muter's feed. Idempotent.
func MuteUser(database *sql.DB, muterID, mutedID string) error {
	_, err := database.Exec(
		`INSERT INTO user_mutes (muter_id, muted_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		muterID, mutedID,
	)
	return err
}

// UnmuteUser removes a mute.
func UnmuteUser(database *sql.DB, muterID, mutedID string) error {
	_, err := database.Exec(
		`DELETE FROM user_mutes WHERE muter_id = $1 AND muted_id = $2`,
		muterID, mutedID,
	)
	return err
}

// GetMutedUserIDs returns the set of user IDs that viewerID has muted.
func GetMutedUserIDs(database *sql.DB, viewerID string) (map[string]bool, error) {
	rows, err := database.Query(
		`SELECT muted_id FROM user_mutes WHERE muter_id = $1`, viewerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out, nil
}

// ListBlockedUsers returns the accounts viewerID has blocked, newest block
// first, resolved as whole accounts. One source for both surfaces that ask the
// question: the /blocked page and GET /api/v1/blocks.
func ListBlockedUsers(database *sql.DB, viewerID string) ([]*model.User, error) {
	return listRelatedUsers(database, `
		JOIN blocks b ON b.blocked_id = u.id
		WHERE b.blocker_id = $1
		ORDER BY b.created_at DESC`, viewerID)
}

// ListMutedUsers returns the accounts viewerID has muted, newest mute first.
func ListMutedUsers(database *sql.DB, viewerID string) ([]*model.User, error) {
	return listRelatedUsers(database, `
		JOIN user_mutes m ON m.muted_id = u.id
		WHERE m.muter_id = $1
		ORDER BY m.created_at DESC`, viewerID)
}

// listRelatedUsers runs userSelectSQL with one extra join and predicate, so a
// relation list reads accounts exactly as GetUserByID does and no surface has
// to re-describe what a user row is.
func listRelatedUsers(database *sql.DB, tail, viewerID string) ([]*model.User, error) {
	if database == nil || viewerID == "" {
		return nil, nil
	}
	rows, err := database.Query(userSelectSQL+tail, viewerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// FollowListEntriesFor turns resolved accounts into the row shape the follow
// list templates read, with the viewer's own follow state marked on each.
func FollowListEntriesFor(database *sql.DB, viewerID string, users []*model.User) []FollowListEntry {
	entries := make([]FollowListEntry, 0, len(users))
	for _, u := range users {
		entries = append(entries, FollowListEntry{
			ID:          u.ID,
			Handle:      u.Handle,
			DisplayName: u.DisplayName,
			AvatarURL:   u.AvatarURL,
		})
	}
	if err := markViewerFollows(database, viewerID, entries); err != nil {
		log.Printf("[follow-list] mark viewer follows: %v", err)
	}
	return entries
}

// ── Post pinning ──────────────────────────────────────────────────────────────

// PinPost sets the user's pinned_work_id. Verifies the work belongs to the user.
func PinPost(database *sql.DB, userID, workID string) error {
	var owner string
	err := database.QueryRow(`SELECT author_id FROM works WHERE id = $1`, workID).Scan(&owner)
	if err != nil {
		return err
	}
	if owner != userID {
		return fmt.Errorf("work does not belong to user")
	}
	_, err = database.Exec(
		`UPDATE user_profiles SET pinned_work_id = $1 WHERE user_id = $2`,
		workID, userID,
	)
	return err
}

// UnpinPost clears the user's pinned_work_id.
func UnpinPost(database *sql.DB, userID string) error {
	_, err := database.Exec(
		`UPDATE user_profiles SET pinned_work_id = NULL WHERE user_id = $1`, userID)
	return err
}

// GetPinnedPostID returns the pinned_work_id for a user, or "" if none.
func GetPinnedPostID(database *sql.DB, userID string) string {
	var id sql.NullString
	_ = database.QueryRow(`SELECT COALESCE(pinned_work_id::text,'') FROM user_profiles WHERE user_id = $1`, userID).Scan(&id)
	if id.Valid {
		return id.String
	}
	return ""
}

// ── Creator subscriptions ─────────────────────────────────────────────────────

// IsSubscribed, the plan reads and the subscriber count live in commerce.go,
// beside the projection that feeds them.

// GetSubscribers returns a creator's active subscribers as follow-list rows. It
// takes the viewer because those rows render a follow control: without the viewer's
// follow state a subscriber the viewer already follows would be offered "Follow".
func GetSubscribers(database *sql.DB, creatorID, viewerID string, limit int) ([]FollowListEntry, error) {
	rows, err := database.Query(`
		SELECT u.id::text,
		       u.handle,
		       COALESCE(NULLIF(TRIM(p.display_name),''), CASE WHEN LENGTH(u.handle)>=40 THEN 'User '||LEFT(u.handle,6) ELSE INITCAP(REPLACE(u.handle,'_',' ')) END),
		       COALESCE(p.avatar_url, '')
		FROM subscriptions s
		JOIN users u ON u.id = s.subscriber_id
		LEFT JOIN user_profiles p ON p.user_id = u.id
		WHERE s.creator_id = $1 AND s.status = 'active'
		  AND (s.expires_at IS NULL OR s.expires_at > NOW())
		ORDER BY s.created_at DESC LIMIT $2`, creatorID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FollowListEntry
	for rows.Next() {
		var e FollowListEntry
		if err := rows.Scan(&e.ID, &e.Handle, &e.DisplayName, &e.AvatarURL); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, markViewerFollows(database, viewerID, out)
}

// ── AethyrRank signal helpers ─────────────────────────────────────────────────

// BatchGetSelfReplyFirst30m returns the set of post IDs where the author replied
// to their own post within 30 minutes of publishing. Used as the
// self_reply_cadence signal in AethyrRank velocity scoring.
func BatchGetSelfReplyFirst30m(database *sql.DB, postIDs []string) map[string]bool {
	out := make(map[string]bool)
	if len(postIDs) == 0 {
		return out
	}
	placeholders := make([]string, len(postIDs))
	args := make([]interface{}, len(postIDs))
	for i, id := range postIDs {
		placeholders[i] = fmt.Sprintf("$%d::uuid", i+1)
		args[i] = id
	}
	q := fmt.Sprintf(`
		SELECT DISTINCT p.id::text
		FROM works p
		WHERE p.id IN (%s)
		  AND EXISTS (
		    SELECT 1 FROM work_citations wc
		    JOIN works r ON r.id = wc.work_id
		    WHERE wc.target_id = p.id
		      AND wc.citation_type = 'reply'
		      AND r.author_id = p.author_id
		      AND r.created_at <= p.created_at + INTERVAL '30 minutes'
		  )`,
		strings.Join(placeholders, ","))
	rows, err := database.Query(q, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

func SetUserEmail(database *sql.DB, userID, email string) error {
	_, err := database.Exec(`UPDATE users SET email = $1 WHERE id = $2`, email, userID)
	return err
}

// ── Admin queries ─────────────────────────────────────────────────────────────

type AdminUserRow struct {
	ID          string
	Handle      string
	DisplayName string
	Role        string
	IsVerified  bool
	IsCreator   bool
	CreatedAt   time.Time
}

func GetRecentUsers(database *sql.DB, limit int) ([]AdminUserRow, error) {
	rows, err := database.Query(`
		SELECT u.id, u.handle,
		       COALESCE(NULLIF(TRIM(p.display_name),''), INITCAP(REPLACE(u.handle,'_',' '))),
		       COALESCE(u.role, 'user'),
		       (COALESCE(plr.kyc_tier, 'none') = 'full'),
		       COALESCE(p.is_creator, FALSE),
		       u.created_at
		FROM users u
		LEFT JOIN user_profiles p ON p.user_id = u.id
		LEFT JOIN identity_facts plr ON plr.pial_id = u.pial_id
		ORDER BY u.created_at DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminUserRow
	for rows.Next() {
		var r AdminUserRow
		rows.Scan(&r.ID, &r.Handle, &r.DisplayName, &r.Role, &r.IsVerified, &r.IsCreator, &r.CreatedAt)
		out = append(out, r)
	}
	return out, nil
}

// ── Feed surfaces ─────────────────────────────────────────────────────────────

// GetUserSurfaces returns the surfaces pinned by userID, ordered by pin sort_order.
func GetUserSurfaces(database *sql.DB, userID string) ([]model.FeedSurface, error) {
	rows, err := database.Query(`
		SELECT fs.id, fs.label, fs.emoji, fs.surface_type, fs.tags, fs.content_type, usp.sort_order
		FROM user_surface_pins usp
		JOIN feed_surfaces fs ON fs.id = usp.surface_id
		WHERE usp.user_id = $1 AND fs.is_active = TRUE
		ORDER BY usp.sort_order ASC, usp.pinned_at ASC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSurfaces(rows, true)
}

// GetAllSurfaces returns all active interest surfaces with IsPinned set for userID.
func GetAllSurfaces(database *sql.DB, userID string) ([]model.FeedSurface, error) {
	rows, err := database.Query(`
		SELECT fs.id, fs.label, fs.emoji, fs.surface_type, fs.tags, fs.content_type, fs.sort_order,
		       EXISTS(SELECT 1 FROM user_surface_pins usp WHERE usp.user_id=$1 AND usp.surface_id=fs.id) AS is_pinned
		FROM feed_surfaces fs
		WHERE fs.is_active = TRUE AND fs.surface_type = 'interest'
		ORDER BY fs.sort_order ASC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.FeedSurface
	for rows.Next() {
		var s model.FeedSurface
		if err := rows.Scan(&s.ID, &s.Label, &s.Emoji, &s.SurfaceType, pq.Array(&s.Tags), &s.ContentType, &s.SortOrder, &s.IsPinned); err != nil {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// GetSurfaceByID returns a single surface definition for feed routing.
func GetSurfaceByID(database *sql.DB, surfaceID string) (*model.FeedSurface, error) {
	var s model.FeedSurface
	err := database.QueryRow(`
		SELECT id, label, emoji, surface_type, tags, content_type, sort_order
		FROM feed_surfaces WHERE id=$1 AND is_active=TRUE
	`, surfaceID).Scan(&s.ID, &s.Label, &s.Emoji, &s.SurfaceType, pq.Array(&s.Tags), &s.ContentType, &s.SortOrder)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// PinSurface adds a surface to the user's tab bar. Safe to call if already pinned.
func PinSurface(database *sql.DB, userID, surfaceID string) error {
	_, err := database.Exec(`
		INSERT INTO user_surface_pins (user_id, surface_id, sort_order)
		VALUES ($1, $2, (SELECT COALESCE(MAX(sort_order)+1, 0) FROM user_surface_pins WHERE user_id=$1))
		ON CONFLICT (user_id, surface_id) DO NOTHING
	`, userID, surfaceID)
	return err
}

// UnpinSurface removes a surface from the user's tab bar.
func UnpinSurface(database *sql.DB, userID, surfaceID string) error {
	_, err := database.Exec(`DELETE FROM user_surface_pins WHERE user_id=$1 AND surface_id=$2`, userID, surfaceID)
	return err
}

// ── AethyrRank signal gaps ────────────────────────────────────────────────────

// GetFollowedUserIDSet returns a set of user IDs that viewerID follows.
// Used to mark in-network candidates in buildRankRequest.
func GetFollowedUserIDSet(database *sql.DB, userID string) map[string]bool {
	rows, err := database.Query(
		`SELECT following_id::text FROM follows WHERE follower_id = $1::uuid`, userID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	set := make(map[string]bool)
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			set[id] = true
		}
	}
	return set
}

// BatchGetDwellSecondsPerContent returns average view-time seconds (from view_time
// feedback events in the last 7 days) for a set of content IDs.
func BatchGetDwellSecondsPerContent(database *sql.DB, contentIDs []string) map[string]float64 {
	if len(contentIDs) == 0 {
		return nil
	}
	placeholders := make([]string, len(contentIDs))
	args := make([]interface{}, len(contentIDs))
	for i, id := range contentIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}
	q := fmt.Sprintf(`
		SELECT content_id, AVG(dwell_ms)::float8 / 1000.0
		FROM feedback_events
		WHERE content_id IN (%s)
		AND event_type = 'view_time'
		AND dwell_ms > 0
		AND created_at > NOW() - INTERVAL '7 days'
		GROUP BY content_id
	`, strings.Join(placeholders, ","))
	rows, err := database.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make(map[string]float64)
	for rows.Next() {
		var cid string
		var secs float64
		if rows.Scan(&cid, &secs) == nil {
			result[cid] = secs
		}
	}
	return result
}

// GetRecentReactionWorkIDs returns the ids of the works a person most recently
// reacted to (any reaction type), newest first. It is the interaction history
// AethyrRank uses for fatigue: a work already reacted to is one already seen.
func GetRecentReactionWorkIDs(database *sql.DB, userID string, limit int) []string {
	rows, err := database.Query(`
		SELECT work_id::text
		FROM work_reactions
		WHERE reactor_id = $1::uuid
		ORDER BY created_at DESC
		LIMIT $2
	`, userID, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// LoadInterestVector returns the stored interest vector for a user, or nil if unset.
func LoadInterestVector(database *sql.DB, userID string) []float32 {
	var vec pq.Float32Array
	err := database.QueryRow(
		`SELECT interest_vector FROM user_profiles WHERE user_id = $1::uuid AND interest_vector IS NOT NULL`,
		userID,
	).Scan(&vec)
	if err != nil {
		return nil
	}
	return []float32(vec)
}

// SaveInterestVector persists the normalised interest vector for a user and
// stamps when it moved.
func SaveInterestVector(database *sql.DB, userID string, vec []float32) error {
	_, err := database.Exec(
		`UPDATE user_profiles SET interest_vector = $1, interest_updated_at = NOW() WHERE user_id = $2::uuid`,
		pq.Float32Array(vec), userID,
	)
	return err
}

// ── AethyrRank candidate signals ──────────────────────────────────────────────
//
// Each of these is one batched query over the candidate window of a single
// feed request. They exist so the rank request carries real numbers in every
// field the ranker reads, never a constant standing in for a signal.

// BatchGetCreatorPostCounts24h returns, per author id, how many works that
// author published in the last 24 hours. AethyrRank dilutes per-post weight
// for burst posters.
func BatchGetCreatorPostCounts24h(database *sql.DB, authorIDs []string) map[string]int {
	out := make(map[string]int)
	if len(authorIDs) == 0 {
		return out
	}
	rows, err := database.Query(`
		SELECT author_id::text, COUNT(*)::int
		FROM works
		WHERE author_id = ANY($1::uuid[])
		  AND deleted_at IS NULL
		  AND created_at > NOW() - INTERVAL '24 hours'
		GROUP BY author_id
	`, pq.Array(authorIDs))
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if rows.Scan(&id, &n) == nil {
			out[id] = n
		}
	}
	return out
}

// BatchGetCreatorExposure returns, per author id, the total views their works
// have drawn in the last 30 days. It is the creator_exposure the ranker's
// shadow term reads: the smaller it is, the more the creator is lifted.
func BatchGetCreatorExposure(database *sql.DB, authorIDs []string) map[string]uint64 {
	out := make(map[string]uint64)
	if len(authorIDs) == 0 {
		return out
	}
	rows, err := database.Query(`
		SELECT author_id::text, COALESCE(SUM(view_count), 0)::bigint
		FROM works
		WHERE author_id = ANY($1::uuid[])
		  AND deleted_at IS NULL
		  AND created_at > NOW() - INTERVAL '30 days'
		GROUP BY author_id
	`, pq.Array(authorIDs))
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int64
		if rows.Scan(&id, &n) == nil && n > 0 {
			out[id] = uint64(n)
		}
	}
	return out
}

// BatchGetRecentReactionCounts returns, per work id, how many reactions of
// any type the work received inside the trailing window. It is the raw
// d(engagement)/dt the velocity score is built from.
func BatchGetRecentReactionCounts(database *sql.DB, workIDs []string, window time.Duration) map[string]int {
	out := make(map[string]int)
	if len(workIDs) == 0 {
		return out
	}
	rows, err := database.Query(`
		SELECT work_id::text, COUNT(*)::int
		FROM work_reactions
		WHERE work_id = ANY($1::uuid[])
		  AND created_at > NOW() - $2::interval
		GROUP BY work_id
	`, pq.Array(workIDs), fmt.Sprintf("%d seconds", int64(window.Seconds())))
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if rows.Scan(&id, &n) == nil {
			out[id] = n
		}
	}
	return out
}

func scanSurfaces(rows *sql.Rows, isPinned bool) ([]model.FeedSurface, error) {
	var out []model.FeedSurface
	for rows.Next() {
		var s model.FeedSurface
		s.IsPinned = isPinned
		if err := rows.Scan(&s.ID, &s.Label, &s.Emoji, &s.SurfaceType, pq.Array(&s.Tags), &s.ContentType, &s.SortOrder); err != nil {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// ── Trust & Safety: Content Moderation ───────────────────────────────────────

// SetPostScanState transitions a work's scan_state and appends to the moderation log.
// actor should be "system", "abraxas", or a moderator handle; byPerson is whether a
// person made the decision (it guards the state from later automation).
//
// works is the only content table: this used to probe posts first and fall through,
// which meant a lookup miss on an empty table on every single moderation decision.
func SetPostScanState(database DBTX, postID, toState, reason, actor string, byPerson bool) error {
	var fromState string
	err := database.QueryRow(`SELECT COALESCE(scan_state,'clean') FROM works WHERE id = $1`, postID).Scan(&fromState)
	if err != nil {
		return err
	}
	setBy := "auto"
	if byPerson {
		setBy = "person"
	}
	// One statement keeps the state and what the read paths key off together:
	//   is_blocked — a block sets it (only a person lifts it; applyWorkVerdict);
	//   is_nsfw    — 18+ when the creator marked the work 18+ or the work is
	//                age-gated, and nothing else, so leaving age_gated returns
	//                the work to exactly what its creator declared;
	//   scan_set_by — whether automation or a person put it in this state.
	if _, err2 := database.Exec(`
		UPDATE works
		   SET scan_state  = $1,
		       is_blocked  = is_blocked OR $1 = 'blocked',
		       is_nsfw     = creator_marked_nsfw OR $1 = 'age_gated',
		       scan_set_by = $3
		 WHERE id = $2`, toState, postID, setBy); err2 != nil {
		return fmt.Errorf("work %s: setting scan state %s: %w", postID, toState, err2)
	}
	if _, err2 := database.Exec(
		`INSERT INTO content_moderation_log (post_id, from_state, to_state, reason, actor) VALUES ($1,$2,$3,$4,$5)`,
		postID, fromState, toState, reason, actor,
	); err2 != nil {
		return fmt.Errorf("work %s: moderation decision %s->%s applied but not logged: %w",
			postID, fromState, toState, err2)
	}
	return nil
}

// GetAdminUserIDs returns the IDs of all users with role = 'admin'.
// Used to fan-out moderation notifications when a post enters human_review.
func GetAdminUserIDs(database DBTX) []string {
	rows, err := database.Query(`SELECT id FROM users WHERE role IN ('admin','founder')`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// ── Link preview ──────────────────────────────────────────────────────────────

// GetCachedLinkPreview returns the cached preview if it was fetched within the last 7 days.
func GetCachedLinkPreview(database *sql.DB, rawURL string) (*model.LinkPreview, error) {
	lp := &model.LinkPreview{}
	err := database.QueryRow(`
		SELECT url, title, description, image_url, site_name
		FROM link_previews
		WHERE url = $1
		  AND fetched_at > NOW() - INTERVAL '7 days'
	`, rawURL).Scan(&lp.URL, &lp.Title, &lp.Description, &lp.ImageURL, &lp.SiteName)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return lp, nil
}

// UpsertLinkPreview stores or refreshes Open Graph metadata for a URL.
func UpsertLinkPreview(database *sql.DB, lp *model.LinkPreview) error {
	_, err := database.Exec(`
		INSERT INTO link_previews (url, title, description, image_url, site_name, fetched_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		ON CONFLICT (url) DO UPDATE SET
			title       = EXCLUDED.title,
			description = EXCLUDED.description,
			image_url   = EXCLUDED.image_url,
			site_name   = EXCLUDED.site_name,
			fetched_at  = NOW()
	`, lp.URL, lp.Title, lp.Description, lp.ImageURL, lp.SiteName)
	return err
}

// ── Topic subscriptions ──────────────────────────────────────────────────────

func FollowTopic(database *sql.DB, userID, tag string) error {
	_, err := database.Exec(
		`INSERT INTO user_topic_subscriptions (user_id, tag) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		userID, tag)
	return err
}

func UnfollowTopic(database *sql.DB, userID, tag string) error {
	_, err := database.Exec(
		`DELETE FROM user_topic_subscriptions WHERE user_id = $1 AND tag = $2`,
		userID, tag)
	return err
}

func IsFollowingTopic(database *sql.DB, userID, tag string) bool {
	var exists bool
	_ = database.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM user_topic_subscriptions WHERE user_id=$1 AND tag=$2)`,
		userID, tag).Scan(&exists)
	return exists
}

func GetSubscribedTopics(database *sql.DB, userID string) []string {
	rows, err := database.Query(
		`SELECT tag FROM user_topic_subscriptions WHERE user_id=$1 ORDER BY tag`, userID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var tags []string
	for rows.Next() {
		var t string
		if rows.Scan(&t) == nil {
			tags = append(tags, t)
		}
	}
	return tags
}

// ── Lists ────────────────────────────────────────────────────────────────────

type List struct {
	ID          string
	OwnerID     string
	OwnerHandle string
	Name        string
	Description string
	IsPublic    bool
	MemberCount int
	CreatedAt   time.Time
}

func CreateList(database *sql.DB, ownerID, name, description string, isPublic bool) (string, error) {
	var id string
	err := database.QueryRow(`
		INSERT INTO user_lists (owner_id, name, description, is_public)
		VALUES ($1, $2, $3, $4) RETURNING id`,
		ownerID, name, description, isPublic).Scan(&id)
	return id, err
}

func DeleteList(database *sql.DB, listID, ownerID string) error {
	res, err := database.Exec(`DELETE FROM user_lists WHERE id=$1 AND owner_id=$2`, listID, ownerID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("list not found or not owned")
	}
	return nil
}

func GetUserLists(database *sql.DB, ownerID string) ([]List, error) {
	rows, err := database.Query(`
		SELECT l.id, l.owner_id, u.handle, l.name, l.description, l.is_public,
		       (SELECT COUNT(*) FROM list_members lm WHERE lm.list_id=l.id)::int, l.created_at
		FROM user_lists l
		JOIN users u ON u.id = l.owner_id
		WHERE l.owner_id = $1
		ORDER BY l.created_at DESC`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanLists(rows)
}

func GetPublicList(database *sql.DB, listID string) (*List, error) {
	rows, err := database.Query(`
		SELECT l.id, l.owner_id, u.handle, l.name, l.description, l.is_public,
		       (SELECT COUNT(*) FROM list_members lm WHERE lm.list_id=l.id)::int, l.created_at
		FROM user_lists l
		JOIN users u ON u.id = l.owner_id
		WHERE l.id = $1 AND l.is_public = TRUE`, listID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	lists, err := scanLists(rows)
	if err != nil || len(lists) == 0 {
		return nil, err
	}
	return &lists[0], nil
}

func scanLists(rows *sql.Rows) ([]List, error) {
	var out []List
	for rows.Next() {
		var l List
		if err := rows.Scan(&l.ID, &l.OwnerID, &l.OwnerHandle, &l.Name, &l.Description, &l.IsPublic, &l.MemberCount, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func AddListMember(database *sql.DB, listID, ownerID, targetUserID string) error {
	// verify ownership
	var oid string
	if err := database.QueryRow(`SELECT owner_id FROM user_lists WHERE id=$1`, listID).Scan(&oid); err != nil {
		return fmt.Errorf("list not found")
	}
	if oid != ownerID {
		return fmt.Errorf("not list owner")
	}
	_, err := database.Exec(`INSERT INTO list_members (list_id, user_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, listID, targetUserID)
	return err
}

func RemoveListMember(database *sql.DB, listID, ownerID, targetUserID string) error {
	var oid string
	if err := database.QueryRow(`SELECT owner_id FROM user_lists WHERE id=$1`, listID).Scan(&oid); err != nil {
		return fmt.Errorf("list not found")
	}
	if oid != ownerID {
		return fmt.Errorf("not list owner")
	}
	_, err := database.Exec(`DELETE FROM list_members WHERE list_id=$1 AND user_id=$2`, listID, targetUserID)
	return err
}

func GetListMembers(database *sql.DB, listID string) ([]model.User, error) {
	rows, err := database.Query(`
		SELECT u.id, u.handle, COALESCE(p.display_name,''), COALESCE(p.avatar_url,''),
		       COALESCE(plr.age_verified,FALSE), COALESCE(p.is_creator,FALSE),
		       COALESCE(p.follower_count,0), COALESCE(p.following_count,0)
		FROM list_members lm
		JOIN users u ON u.id = lm.user_id
		LEFT JOIN user_profiles p ON p.user_id = u.id
		LEFT JOIN identity_facts plr ON plr.pial_id = u.pial_id
		WHERE lm.list_id = $1
		ORDER BY lm.added_at`, listID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.User
	for rows.Next() {
		var u model.User
		var isVerified, isCreator bool
		if err := rows.Scan(&u.ID, &u.Handle, &u.DisplayName, &u.AvatarURL, &isVerified, &isCreator, &u.FollowerCount, &u.FollowingCount); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ── Post analytics ───────────────────────────────────────────────────────────

type PostAnalytics struct {
	PostID      string
	Impressions int64
	Likes       int64
	Dislikes    int64
	Reposts     int64
	Comments    int64
	Saves       int64
	ViewSeconds int64
}

// ── Leaderboard queries ────────────────────────────────────────────────────────
//
// Daily period: current calendar day in Asia/Tokyo (UTC+9), resets at 15:00 UTC.
// All-time: cumulative users.xp, no time filter.
//
// Both use LEFT JOIN user_profiles to get display_name and avatar_url, since
// those columns live in user_profiles, not users.

const leaderboardPeriodCTE = `
WITH period AS (
    SELECT date_trunc('day', NOW() AT TIME ZONE 'Asia/Tokyo') AT TIME ZONE 'Asia/Tokyo' AS start
)`

func scanLeaderboardRows(rows *sql.Rows) ([]model.LeaderboardEntry, error) {
	defer rows.Close()
	var out []model.LeaderboardEntry
	for rows.Next() {
		var e model.LeaderboardEntry
		if err := rows.Scan(&e.UserID, &e.Handle, &e.DisplayName, &e.AvatarURL, &e.Realm, &e.XPToday, &e.Rank); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func GetGlobalLeaderboard(database *sql.DB) ([]model.LeaderboardEntry, error) {
	rows, err := database.Query(leaderboardPeriodCTE + `,
scores AS (
    SELECT user_id, SUM(xp_delta) AS xp_today
    FROM xp_events
    WHERE created_at >= (SELECT start FROM period) AND xp_delta > 0
    GROUP BY user_id
)
SELECT u.id, u.handle,
       COALESCE(NULLIF(p.display_name,''), u.handle),
       COALESCE(p.avatar_url, ''),
       COALESCE(u.realm, 1),
       s.xp_today,
       RANK() OVER (ORDER BY s.xp_today DESC, u.realm DESC) AS rank
FROM scores s
JOIN users u ON u.id = s.user_id
LEFT JOIN user_profiles p ON p.user_id = u.id
ORDER BY s.xp_today DESC, u.realm DESC, u.handle
LIMIT 10`)
	if err != nil {
		return nil, err
	}
	return scanLeaderboardRows(rows)
}

func GetCountryLeaderboard(database *sql.DB, countryCode string) ([]model.LeaderboardEntry, error) {
	if countryCode == "" {
		return nil, nil
	}
	rows, err := database.Query(leaderboardPeriodCTE+`,
scores AS (
    SELECT xe.user_id, SUM(xe.xp_delta) AS xp_today
    FROM xp_events xe
    JOIN user_profiles up ON up.user_id = xe.user_id
    WHERE xe.created_at >= (SELECT start FROM period)
      AND xe.xp_delta > 0
      AND up.country_code = $1
    GROUP BY xe.user_id
)
SELECT u.id, u.handle,
       COALESCE(NULLIF(p.display_name,''), u.handle),
       COALESCE(p.avatar_url, ''),
       COALESCE(u.realm, 1),
       s.xp_today,
       RANK() OVER (ORDER BY s.xp_today DESC, u.realm DESC) AS rank
FROM scores s
JOIN users u ON u.id = s.user_id
LEFT JOIN user_profiles p ON p.user_id = u.id
ORDER BY s.xp_today DESC, u.realm DESC, u.handle
LIMIT 10`, countryCode)
	if err != nil {
		return nil, err
	}
	return scanLeaderboardRows(rows)
}

func GetGlobalAllTimeLeaderboard(database *sql.DB) ([]model.LeaderboardEntry, error) {
	rows, err := database.Query(`
SELECT u.id, u.handle,
       COALESCE(NULLIF(p.display_name,''), u.handle),
       COALESCE(p.avatar_url, ''),
       COALESCE(u.realm, 1),
       u.xp,
       RANK() OVER (ORDER BY u.xp DESC, u.realm DESC) AS rank
FROM users u
LEFT JOIN user_profiles p ON p.user_id = u.id
WHERE u.xp > 0
ORDER BY u.xp DESC, u.realm DESC, u.handle
LIMIT 10`)
	if err != nil {
		return nil, err
	}
	return scanLeaderboardRows(rows)
}

func GetCountryAllTimeLeaderboard(database *sql.DB, countryCode string) ([]model.LeaderboardEntry, error) {
	if countryCode == "" {
		return nil, nil
	}
	rows, err := database.Query(`
SELECT u.id, u.handle,
       COALESCE(NULLIF(p.display_name,''), u.handle),
       COALESCE(p.avatar_url, ''),
       COALESCE(u.realm, 1),
       u.xp,
       RANK() OVER (ORDER BY u.xp DESC, u.realm DESC) AS rank
FROM users u
LEFT JOIN user_profiles p ON p.user_id = u.id
WHERE u.xp > 0
  AND p.country_code = $1
ORDER BY u.xp DESC, u.realm DESC, u.handle
LIMIT 10`, countryCode)
	if err != nil {
		return nil, err
	}
	return scanLeaderboardRows(rows)
}

// GetUserDailyRank returns the current user's rank and XP earned today.
// rank = 0 when the user has earned 0 XP (not on the board).
func GetUserDailyRank(database *sql.DB, userID string) (rank int, xpToday int64, err error) {
	err = database.QueryRow(leaderboardPeriodCTE+`,
my_score AS (
    SELECT COALESCE(SUM(xp_delta), 0) AS xp
    FROM xp_events
    WHERE user_id = $1 AND created_at >= (SELECT start FROM period) AND xp_delta > 0
)
SELECT m.xp,
       (SELECT COUNT(*)+1
        FROM (
            SELECT user_id FROM xp_events
            WHERE created_at >= (SELECT start FROM period) AND xp_delta > 0
            GROUP BY user_id
            HAVING SUM(xp_delta) > m.xp
        ) better)
FROM my_score m`, userID).Scan(&xpToday, &rank)
	if err != nil {
		rank, xpToday = 0, 0
		err = nil
	}
	return
}

func GetPostAnalytics(database *sql.DB, postID, ownerID string) (*PostAnalytics, error) {
	var a PostAnalytics
	var oid string
	// Content lives in `works`, not the legacy `posts` table.
	if err := database.QueryRow(`SELECT author_id FROM works WHERE id=$1`, postID).Scan(&oid); err != nil {
		return nil, fmt.Errorf("work not found")
	}
	if oid != ownerID {
		return nil, fmt.Errorf("not work owner")
	}
	// Same live counters the work card renders (works.go work-select): reaction
	// tallies from work_reactions, replies from work_citations, views from the works row.
	err := database.QueryRow(`
		SELECT w.id::text,
		       COALESCE(w.view_count, 0),
		       (SELECT COUNT(*) FROM work_reactions wr WHERE wr.work_id = w.id AND wr.reaction_type='like')::int,
		       (SELECT COUNT(*) FROM work_reactions wr WHERE wr.work_id = w.id AND wr.reaction_type='dislike')::int,
		       (SELECT COUNT(*) FROM work_reactions wr WHERE wr.work_id = w.id AND wr.reaction_type='repost')::int,
		       (SELECT COUNT(*) FROM work_citations wc WHERE wc.target_id = w.id AND wc.citation_type='reply')::int,
		       (SELECT COUNT(*) FROM work_reactions wr WHERE wr.work_id = w.id AND wr.reaction_type='bookmark')::int
		FROM works w
		WHERE w.id = $1`, postID).Scan(
		&a.PostID, &a.Impressions, &a.Likes, &a.Dislikes,
		&a.Reposts, &a.Comments, &a.Saves)
	if err != nil {
		return nil, err
	}
	a.ViewSeconds = 0 // works don't track cumulative view-time; template hides the row at 0
	return &a, nil
}

// ── Articles ──────────────────────────────────────────────────────────────────

const articleSelectSQL = `
	SELECT a.id, a.author_id, u.handle, COALESCE(up.display_name,''), COALESCE(up.avatar_url,''),
	       a.slug, a.title, a.excerpt, a.cover_url, a.status, a.published_at,
	       a.view_count, a.created_at, a.updated_at
	FROM articles a
	JOIN users u ON u.id = a.author_id
	LEFT JOIN user_profiles up ON up.user_id = a.author_id
`

func scanArticleRows(rows *sql.Rows) ([]model.Article, error) {
	defer rows.Close()
	var out []model.Article
	for rows.Next() {
		var a model.Article
		var pubAt *time.Time
		if err := rows.Scan(
			&a.ID, &a.AuthorID, &a.AuthorHandle, &a.AuthorDisplay, &a.AuthorAvatar,
			&a.Slug, &a.Title, &a.Excerpt, &a.CoverURL, &a.Status, &pubAt,
			&a.ViewCount, &a.CreatedAt, &a.UpdatedAt,
		); err != nil {
			return nil, err
		}
		a.PublishedAt = pubAt
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetPublishedArticles returns paginated published articles ordered by published_at DESC.
func GetPublishedArticles(database *sql.DB, limit int, after string) ([]model.Article, error) {
	q := articleSelectSQL + `WHERE a.status = 'published'`
	args := []interface{}{}
	if after != "" {
		q += ` AND a.published_at < (SELECT published_at FROM articles WHERE id = $1)`
		args = append(args, after)
		q += fmt.Sprintf(` ORDER BY a.published_at DESC LIMIT $%d`, len(args)+1)
	} else {
		q += ` ORDER BY a.published_at DESC LIMIT $1`
	}
	args = append(args, limit)
	rows, err := database.Query(q, args...)
	if err != nil {
		return nil, err
	}
	return scanArticleRows(rows)
}

// GetUserArticles returns a user's published articles ordered by published_at DESC.
// Used for the profile articles tab.
func GetUserArticles(database *sql.DB, userID string, limit int, after string) ([]model.Article, error) {
	q := articleSelectSQL + `WHERE a.author_id = $1 AND a.status = 'published'`
	args := []interface{}{userID}
	if after != "" {
		q += ` AND a.published_at < (SELECT published_at FROM articles WHERE id = $2)`
		args = append(args, after)
		q += fmt.Sprintf(` ORDER BY a.published_at DESC LIMIT $%d`, len(args)+1)
	} else {
		q += ` ORDER BY a.published_at DESC LIMIT $2`
		args = append(args, limit)
	}
	if after != "" {
		args = append(args, limit)
	}
	rows, err := database.Query(q, args...)
	if err != nil {
		return nil, err
	}
	return scanArticleRows(rows)
}

// SaveArticleVersion inserts a version snapshot into article_versions.
// Called after every successful article save (create or update).
func SaveArticleVersion(database *sql.DB, articleID, authorID, title, body, bodyHTML, excerpt, coverURL, reason string) error {
	var nextNum int
	database.QueryRow(
		`SELECT COALESCE(MAX(version_num), 0) + 1 FROM article_versions WHERE article_id = $1`,
		articleID,
	).Scan(&nextNum)
	_, err := database.Exec(`
		INSERT INTO article_versions
		    (article_id, author_id, version_num, title, body, body_html, excerpt, cover_url, reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		articleID, authorID, nextNum, title, body, bodyHTML, excerpt, coverURL, reason,
	)
	return err
}

// ── Celebrations (admin-scheduled seasonal themes) ───────────────────────────

// Celebration is one admin-scheduled seasonal theme. Theme drives the
// body.celebrate-<theme> class and its CSS (static/css/celebrations.css).
type Celebration struct {
	Theme     string    `json:"theme"`
	Name      string    `json:"name"`
	StartDate time.Time `json:"start_date"`
	EndDate   time.Time `json:"end_date"`
	Enabled   bool      `json:"enabled"`
}

// ListCelebrations returns every celebration row for the admin panel.
func ListCelebrations(database *sql.DB) ([]Celebration, error) {
	rows, err := database.Query(`
		SELECT theme, name, start_date, end_date, enabled
		FROM celebrations ORDER BY start_date`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Celebration
	for rows.Next() {
		var c Celebration
		if err := rows.Scan(&c.Theme, &c.Name, &c.StartDate, &c.EndDate, &c.Enabled); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetActiveCelebration returns the enabled celebration whose date range includes
// today, preferring the narrowest range (so a single-day holiday beats an
// overlapping month). Returns (nil, nil) when none is active.
func GetActiveCelebration(database *sql.DB) (*Celebration, error) {
	var c Celebration
	err := database.QueryRow(`
		SELECT theme, name, start_date, end_date, enabled
		FROM celebrations
		WHERE enabled = TRUE AND CURRENT_DATE BETWEEN start_date AND end_date
		ORDER BY (end_date - start_date) ASC, start_date DESC
		LIMIT 1`).Scan(&c.Theme, &c.Name, &c.StartDate, &c.EndDate, &c.Enabled)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// UpdateCelebration persists an admin edit to one celebration (dates + on/off).
func UpdateCelebration(database *sql.DB, theme string, start, end time.Time, enabled bool, updatedBy string) error {
	res, err := database.Exec(`
		UPDATE celebrations
		SET start_date = $2, end_date = $3, enabled = $4, updated_at = NOW(), updated_by = $5
		WHERE theme = $1`, theme, start, end, enabled, updatedBy)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("unknown celebration theme %q", theme)
	}
	return nil
}

// ── Abraxas Shield config ────────────────────────────────────────────────────

// AbraxasConfig holds the admin-tunable Abraxas Shield settings.
type AbraxasConfig struct {
	NudityBlockThreshold     float64   `json:"nudity_block_threshold"`
	NudityReviewThreshold    float64   `json:"nudity_review_threshold"`
	ClickbaitBlockThreshold  float64   `json:"clickbait_block_threshold"`
	ClickbaitReviewThreshold float64   `json:"clickbait_review_threshold"`
	GoreBlockThreshold       float64   `json:"gore_block_threshold"`
	GoreReviewThreshold      float64   `json:"gore_review_threshold"`
	CustomHateKeywords       []string  `json:"custom_hate_keywords"`
	CustomViolenceKeywords   []string  `json:"custom_violence_keywords"`
	CustomSpamKeywords       []string  `json:"custom_spam_keywords"`
	EnableBotDetection       bool      `json:"enable_bot_detection"`
	EnableSpamDetection      bool      `json:"enable_spam_detection"`
	AutoApproveUnknown       bool      `json:"auto_approve_unknown"`
	UpdatedAt                time.Time `json:"updated_at"`
	UpdatedBy                string    `json:"updated_by"`
}

// GetAbraxasConfig returns the current Abraxas Shield configuration.
func GetAbraxasConfig(database *sql.DB) (*AbraxasConfig, error) {
	cfg := &AbraxasConfig{}
	err := database.QueryRow(`
		SELECT nudity_block_threshold, nudity_review_threshold,
		       clickbait_block_threshold, clickbait_review_threshold,
		       gore_block_threshold, gore_review_threshold,
		       COALESCE(custom_hate_keywords, '{}'),
		       COALESCE(custom_violence_keywords, '{}'),
		       COALESCE(custom_spam_keywords, '{}'),
		       enable_bot_detection, enable_spam_detection,
		       auto_approve_unknown, updated_at, updated_by
		FROM abraxas_config WHERE id = 1`).Scan(
		&cfg.NudityBlockThreshold, &cfg.NudityReviewThreshold,
		&cfg.ClickbaitBlockThreshold, &cfg.ClickbaitReviewThreshold,
		&cfg.GoreBlockThreshold, &cfg.GoreReviewThreshold,
		pq.Array(&cfg.CustomHateKeywords),
		pq.Array(&cfg.CustomViolenceKeywords),
		pq.Array(&cfg.CustomSpamKeywords),
		&cfg.EnableBotDetection, &cfg.EnableSpamDetection,
		&cfg.AutoApproveUnknown, &cfg.UpdatedAt, &cfg.UpdatedBy,
	)
	return cfg, err
}

// SignalAccuracy describes how accurate a signal has been across admin decisions.
type SignalAccuracy struct {
	Signal         string
	TruePositives  int // times admin blocked
	FalsePositives int // times admin approved
	AgeGated       int
	Total          int
	FPRate         float64 // false positive rate 0.0–1.0
}

// GetSignalAccuracy returns per-signal accuracy stats from admin feedback.
func GetSignalAccuracy(database *sql.DB) ([]SignalAccuracy, error) {
	rows, err := database.Query(`
		SELECT
			unnest(signals)                                              AS signal,
			COUNT(*) FILTER (WHERE admin_decision = 'block')            AS true_positives,
			COUNT(*) FILTER (WHERE admin_decision = 'approve')          AS false_positives,
			COUNT(*) FILTER (WHERE admin_decision = 'age_gate')         AS age_gated,
			COUNT(*)                                                     AS total
		FROM scan_feedback
		GROUP BY signal
		ORDER BY total DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SignalAccuracy
	for rows.Next() {
		var a SignalAccuracy
		if err := rows.Scan(&a.Signal, &a.TruePositives, &a.FalsePositives, &a.AgeGated, &a.Total); err != nil {
			continue
		}
		if a.Total > 0 {
			a.FPRate = float64(a.FalsePositives) / float64(a.Total)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SaveAbraxasConfig writes updated Abraxas Shield settings (upsert on id=1).
func SaveAbraxasConfig(database *sql.DB, cfg *AbraxasConfig, updatedBy string) error {
	_, err := database.Exec(`
		INSERT INTO abraxas_config (
			id, nudity_block_threshold, nudity_review_threshold,
			clickbait_block_threshold, clickbait_review_threshold,
			gore_block_threshold, gore_review_threshold,
			custom_hate_keywords, custom_violence_keywords, custom_spam_keywords,
			enable_bot_detection, enable_spam_detection,
			auto_approve_unknown, updated_at, updated_by
		) VALUES (1,$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,NOW(),$13)
		ON CONFLICT (id) DO UPDATE SET
			nudity_block_threshold     = EXCLUDED.nudity_block_threshold,
			nudity_review_threshold    = EXCLUDED.nudity_review_threshold,
			clickbait_block_threshold  = EXCLUDED.clickbait_block_threshold,
			clickbait_review_threshold = EXCLUDED.clickbait_review_threshold,
			gore_block_threshold       = EXCLUDED.gore_block_threshold,
			gore_review_threshold      = EXCLUDED.gore_review_threshold,
			custom_hate_keywords       = EXCLUDED.custom_hate_keywords,
			custom_violence_keywords   = EXCLUDED.custom_violence_keywords,
			custom_spam_keywords       = EXCLUDED.custom_spam_keywords,
			enable_bot_detection       = EXCLUDED.enable_bot_detection,
			enable_spam_detection      = EXCLUDED.enable_spam_detection,
			auto_approve_unknown       = EXCLUDED.auto_approve_unknown,
			updated_at                 = NOW(),
			updated_by                 = EXCLUDED.updated_by`,
		cfg.NudityBlockThreshold, cfg.NudityReviewThreshold,
		cfg.ClickbaitBlockThreshold, cfg.ClickbaitReviewThreshold,
		cfg.GoreBlockThreshold, cfg.GoreReviewThreshold,
		pq.Array(cfg.CustomHateKeywords),
		pq.Array(cfg.CustomViolenceKeywords),
		pq.Array(cfg.CustomSpamKeywords),
		cfg.EnableBotDetection, cfg.EnableSpamDetection,
		cfg.AutoApproveUnknown, updatedBy,
	)
	return err
}
