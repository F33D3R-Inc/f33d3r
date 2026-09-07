package db

import (
	"database/sql"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/lib/pq"

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

// blockedFilter excludes only confirmed-blocked posts, not-yet-published scheduled posts,
// and soft-deleted rows from all public feeds. Applied to every public feed/explore/search query.
// scan_state = 'pending_scan', 'human_review', 'age_gated' are all VISIBLE — Abraxas scoring
// affects ranking and flags for admin review only. Only 'blocked' hides a post.
const blockedFilter = "AND COALESCE(p.scan_state, 'clean') != 'blocked' AND (p.scheduled_at IS NULL OR p.scheduled_at <= NOW()) AND p.deleted_at IS NULL"

// blockedFilterForViewer extends blockedFilter to also hide pending_scan posts
// from users who are not the author. Authors always see their own pending posts.
// viewerID must be the UUID string of the authenticated user, or "" for anonymous.
func blockedFilterForViewer(viewerID string) string {
	// Validate viewerID is a UUID to prevent SQL injection before interpolating.
	// UUIDs contain only hex digits and hyphens — safe to embed.
	safeID := ""
	if isValidUUID(viewerID) {
		safeID = viewerID
	}
	if safeID == "" {
		// Anonymous viewers: also hide pending_scan posts.
		return blockedFilter + " AND COALESCE(p.scan_state, 'clean') != 'pending_scan'"
	}
	// Authenticated: hide pending_scan unless the viewer is the author.
	return blockedFilter + " AND (COALESCE(p.scan_state, 'clean') != 'pending_scan' OR p.author_id::text = '" + safeID + "')"
}

// isValidUUID returns true if s is a valid UUID (lowercase or uppercase hex + hyphens, correct length).
func isValidUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
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
	LEFT JOIN pial_roots plr ON plr.pial_id = u.pial_id
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

func CreateOrgMembership(database *sql.DB, userID, orgUserID, initiatedBy string) error {
	_, err := database.Exec(`
		INSERT INTO org_memberships (user_id, org_user_id, status, initiated_by)
		VALUES ($1, $2,
		  CASE WHEN $3='org' THEN 'pending_org' ELSE 'pending_employee' END,
		  $3)
		ON CONFLICT (user_id, org_user_id) DO NOTHING
	`, userID, orgUserID, initiatedBy)
	return err
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
		LEFT JOIN pial_roots plr ON plr.pial_id = u.pial_id
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

func SuspendOrgMemberships(database *sql.DB, orgUserID string) error {
	_, err := database.Exec(`
		UPDATE org_memberships SET status='suspended', updated_at=NOW()
		WHERE org_user_id=$1 AND status='approved'
	`, orgUserID)
	return err
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
		LEFT JOIN pial_roots plr ON plr.pial_id = u.pial_id
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
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	var userID string
	if err = tx.QueryRow(`UPDATE org_verification_applications SET status='approved', reviewed_at=NOW(), reviewed_by=$1 WHERE id=$2 RETURNING user_id`, adminUserID, appID).Scan(&userID); err != nil {
		tx.Rollback()
		return err
	}
	if _, err = tx.Exec(`INSERT INTO user_profiles (user_id, official_type) VALUES ($1,$2) ON CONFLICT (user_id) DO UPDATE SET official_type=$2, updated_at=NOW()`, userID, orgType); err != nil {
		tx.Rollback()
		return err
	}
	// Verified badge == PIAL kyc_tier='full' — an approved org verification is a
	// deliberate identity confirmation, same bar as eKYC/admin verify. age_verified
	// and dob_locked come along for the ride: an org account has a confirmed identity,
	// so treat it exactly like the other full-tier grant paths.
	if _, err = tx.Exec(`UPDATE pial_roots pr SET age_verified=TRUE, kyc_tier='full', kyc_verified_at=NOW(), dob_locked=TRUE, is_adult=TRUE, is_minor=FALSE FROM users u WHERE u.id=$1 AND u.pial_id=pr.pial_id`, userID); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
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

// GrantOrgBadge sets official_type on a user by handle and marks is_verified.
func GrantOrgBadge(database *sql.DB, handle, orgType string) error {
	if _, err := database.Exec(`
		INSERT INTO user_profiles (user_id, official_type)
		SELECT id, $1 FROM users WHERE handle = $2
		ON CONFLICT (user_id) DO UPDATE
		  SET official_type = EXCLUDED.official_type,
		      updated_at    = NOW()`,
		orgType, handle); err != nil {
		return err
	}
	// Verified badge == PIAL kyc_tier='full' — see ApproveOrgVerification for why.
	_, err := database.Exec(`UPDATE pial_roots pr SET age_verified=TRUE, kyc_tier='full', kyc_verified_at=NOW(), dob_locked=TRUE, is_adult=TRUE, is_minor=FALSE FROM users u WHERE u.handle=$1 AND u.pial_id=pr.pial_id`, handle)
	return err
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

func ClearUnreadCount(database *sql.DB, userID string) {
	database.Exec(`UPDATE users SET unread_count = 0 WHERE id = $1`, userID)
}

func IncrementUnreadCount(database *sql.DB, userID string) {
	database.Exec(`UPDATE users SET unread_count = unread_count + 1 WHERE id = $1`, userID)
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

// Canonical media records who first uploaded an asset. It records that as a
// PIAL and nothing else.
//
// The creator_handle column is deliberately left unwritten. A handle is
// registry-brain's grant, transferable and reclaimable, so a handle copied into
// a content row is a snapshot of who held it at insert time — it goes stale
// silently the moment the handle moves, and nothing in this brain would ever
// learn that it had. The durable name for the same person is the PIAL, which is
// already on the row, and the handle it currently answers to is resolved from
// the naming plane at the point of display.

// CreateCanonicalMedia inserts a new canonical media record and returns its UUID.
// ON CONFLICT on master_url: first uploader owns the record. Returns existing ID on conflict.
// creatorPIAL may be empty for legacy accounts without a PIAL root.
func CreateCanonicalMedia(database *sql.DB, creatorUserID, creatorPIAL, masterURL, posterURL string, durationSecs float32, width, height int) (string, error) {
	var id string
	err := database.QueryRow(`
		INSERT INTO canonical_media (creator_user_id, creator_pial_id, master_url, poster_url, duration_secs, width, height)
		VALUES ($1, NULLIF($2,'')::uuid, $3, $4, $5, $6, $7)
		ON CONFLICT (master_url) WHERE master_url != ''
		DO NOTHING
		RETURNING id`,
		creatorUserID, creatorPIAL, masterURL, posterURL, durationSecs, width, height,
	).Scan(&id)
	if err == sql.ErrNoRows {
		// Conflict: this video already has a canonical record. Return existing owner's ID.
		err = database.QueryRow(
			`SELECT id FROM canonical_media WHERE master_url = $1`, masterURL,
		).Scan(&id)
	}
	return id, err
}

// CreateCanonicalImage registers a new image asset with its original uploader.
// ON CONFLICT DO NOTHING so the first uploader always retains ownership.
func CreateCanonicalImage(database *sql.DB, creatorUserID, creatorPIAL, imageURL string) error {
	if database == nil || imageURL == "" {
		return nil
	}
	_, err := database.Exec(`
		INSERT INTO canonical_media (creator_user_id, creator_pial_id, image_url)
		VALUES ($1, NULLIF($2,'')::uuid, $3)
		ON CONFLICT (image_url) WHERE image_url != '' DO NOTHING`,
		creatorUserID, creatorPIAL, imageURL,
	)
	return err
}

// FindCanonicalImageOwnerPIAL returns the original uploader's PIAL for a known
// image URL, or "" when no canonical record exists for it. Callers that need a
// handle to show resolve it from this PIAL through Manhattan; they must not read
// the copy this table used to carry.
func FindCanonicalImageOwnerPIAL(database *sql.DB, imageURL string) (pial string, err error) {
	if database == nil || imageURL == "" {
		return "", nil
	}
	err = database.QueryRow(`
		SELECT COALESCE(creator_pial_id::TEXT, '')
		FROM canonical_media WHERE image_url = $1`, imageURL,
	).Scan(&pial)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return pial, err
}

// SetPostLineage stores the original uploader's PIAL and handle on a post when
// content-scan detects the video is a duplicate of an earlier upload.
// Called async after transcoding — safe to fire-and-forget.
func SetPostLineage(database *sql.DB, postID, lineagePIAL, lineageHandle string) error {
	if database == nil {
		return nil
	}
	_, err := database.Exec(
		`UPDATE works SET lineage_pial = $1, lineage_handle = $2 WHERE id = $3`,
		lineagePIAL, lineageHandle, postID,
	)
	return err
}

// RedirectDuplicatePost re-points a duplicate post's video at the original post's
// transcoded asset and canonical_media record. The uploader's post still exists and
// belongs to them (caption, tags, interactions are preserved) but the video served
// is the original — making the duplicate act like a repost wrapper around the
// canonical asset. Called when a duplicate is confirmed via dedup logic.
func RedirectDuplicatePost(database *sql.DB, duplicatePostID, originalPostID string) error {
	if database == nil || duplicatePostID == originalPostID {
		return nil
	}
	var masterURL, posterURL string
	var dur float32
	var w, h int
	err := database.QueryRow(`
		SELECT COALESCE(video_master_url,''), COALESCE(video_poster_url,''),
		       COALESCE(video_duration_secs,0)::REAL, COALESCE(video_width,0), COALESCE(video_height,0)
		FROM works WHERE id = $1 AND video_master_url IS NOT NULL AND video_master_url != ''
	`, originalPostID).Scan(&masterURL, &posterURL, &dur, &w, &h)
	if err != nil || masterURL == "" {
		return err
	}
	// Redirect duplicate to original's video asset (works has no canonical_media_id;
	// the served video URLs are the canonical link).
	_, err = database.Exec(`
		UPDATE works
		SET video_master_url    = $1,
		    video_poster_url    = $2,
		    video_duration_secs = $3,
		    video_width         = $4,
		    video_height        = $5
		WHERE id = $6
	`, masterURL, posterURL, dur, w, h, duplicatePostID)
	return err
}

// PublishScheduledPosts clears scheduled_at on every work whose publish time has
// passed, making it visible to the feed queries. Returns the number published.
func PublishScheduledPosts(database *sql.DB) (int, error) {
	res, err := database.Exec(`
		WITH published AS (
			UPDATE works
			SET scheduled_at = NULL
			WHERE scheduled_at IS NOT NULL
			  AND scheduled_at <= NOW()
			  AND deleted_at IS NULL
			RETURNING author_id
		)
		UPDATE user_profiles up
		SET post_count = post_count + cnt.n
		FROM (SELECT author_id, COUNT(*) AS n FROM published GROUP BY author_id) cnt
		WHERE up.user_id = cnt.author_id
	`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

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

// FollowUser records follower → following and returns whether a NEW follow was
// created (false means the follow already existed and nothing changed).
//
// The counters are NOT touched here. user_profiles.follower_count and
// following_count are maintained by the trg_follows_counters trigger (migration
// 0016), because this function is not the only thing that writes follows: the
// follower_id/following_id foreign keys are ON DELETE CASCADE, so deleting an
// account removes its follow edges inside the database with no Go code running.
// Maintaining the counters here left them permanently wrong every time an
// account was deleted. A trigger fires for cascades too, so it cannot be
// bypassed by any writer.
func FollowUser(database *sql.DB, followerID, followingID string) (bool, error) {
	res, err := database.Exec(`
		INSERT INTO follows (follower_id, following_id)
		VALUES ($1::uuid, $2::uuid)
		ON CONFLICT DO NOTHING`, followerID, followingID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// UnfollowUser removes follower → following and returns whether a follow was
// actually removed. Counters are the trigger's job — see FollowUser.
func UnfollowUser(database *sql.DB, followerID, followingID string) (bool, error) {
	res, err := database.Exec(`
		DELETE FROM follows
		WHERE follower_id = $1::uuid AND following_id = $2::uuid`, followerID, followingID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// FollowListEntry is a minimal user row for the followers/following list.
type FollowListEntry struct {
	ID            string
	Handle        string
	DisplayName   string
	AvatarURL     string
	PIALID        string
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
		       COALESCE(p.avatar_url, ''),
		       COALESCE(u.pial_id::text, '')
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
		if err := rows.Scan(&e.ID, &e.Handle, &e.DisplayName, &e.AvatarURL, &e.PIALID); err != nil {
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
		       COALESCE(p.avatar_url, ''),
		       COALESCE(u.pial_id::text, '')
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
		if err := rows.Scan(&e.ID, &e.Handle, &e.DisplayName, &e.AvatarURL, &e.PIALID); err != nil {
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

// ── Tracks ────────────────────────────────────────────────────────────────────

func InsertTrack(database *sql.DB, authorID, title, description, audioURL, coverURL string, durationSecs int, genre string, tags []string, priceCents int) (string, error) {
	isFree := priceCents == 0
	var id string
	err := database.QueryRow(`
		INSERT INTO tracks (author_id, title, description, audio_url, cover_url, duration_secs, genre, tags, price_cents, is_free)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		RETURNING id
	`, authorID, title, description, audioURL, coverURL, durationSecs, genre, pq.Array(tags), priceCents, isFree).Scan(&id)
	return id, err
}

func GetRecentTracks(database *sql.DB, genre string, limit int, afterID string) ([]*model.Track, error) {
	query := `
		SELECT t.id, t.author_id, u.handle,
		       COALESCE(NULLIF(TRIM(p.display_name),''), CASE WHEN LENGTH(u.handle)>=40 THEN 'User '||LEFT(u.handle,6) ELSE INITCAP(REPLACE(u.handle,'_',' ')) END),
		       COALESCE(p.avatar_url, ''),
		       (COALESCE(plr.kyc_tier, 'none') = 'full'),
		       t.title, t.description, t.audio_url, t.cover_url,
		       t.duration_secs, t.genre, t.tags, t.price_cents, t.is_free,
		       t.play_count, t.like_count, t.created_at
		FROM tracks t
		JOIN users u ON u.id = t.author_id
		LEFT JOIN user_profiles p ON p.user_id = t.author_id
		LEFT JOIN pial_roots plr ON plr.pial_id = u.pial_id
		WHERE ($1 = '' OR t.genre = $1)
		AND ($2::uuid IS NULL OR t.created_at < (SELECT created_at FROM tracks WHERE id=$2::uuid))
		ORDER BY t.created_at DESC LIMIT $3
	`
	var afterParam interface{}
	if afterID != "" {
		afterParam = afterID
	}
	rows, err := database.Query(query, genre, afterParam, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTracks(rows)
}

func GetTracksByAuthor(database *sql.DB, authorID string, limit int) ([]*model.Track, error) {
	rows, err := database.Query(`
		SELECT t.id, t.author_id, u.handle,
		       COALESCE(NULLIF(TRIM(p.display_name),''), CASE WHEN LENGTH(u.handle)>=40 THEN 'User '||LEFT(u.handle,6) ELSE INITCAP(REPLACE(u.handle,'_',' ')) END),
		       COALESCE(p.avatar_url, ''),
		       (COALESCE(plr.kyc_tier, 'none') = 'full'),
		       t.title, t.description, t.audio_url, t.cover_url,
		       t.duration_secs, t.genre, t.tags, t.price_cents, t.is_free,
		       t.play_count, t.like_count, t.created_at
		FROM tracks t
		JOIN users u ON u.id = t.author_id
		LEFT JOIN user_profiles p ON p.user_id = t.author_id
		LEFT JOIN pial_roots plr ON plr.pial_id = u.pial_id
		WHERE t.author_id = $1
		ORDER BY t.created_at DESC LIMIT $2
	`, authorID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTracks(rows)
}

func ToggleTrackLike(database *sql.DB, userID, trackID string) (bool, error) {
	var exists bool
	_ = database.QueryRow(`SELECT EXISTS(SELECT 1 FROM track_likes WHERE user_id=$1 AND track_id=$2)`, userID, trackID).Scan(&exists)
	if exists {
		_, err := database.Exec(`DELETE FROM track_likes WHERE user_id=$1 AND track_id=$2`, userID, trackID)
		_, _ = database.Exec(`UPDATE tracks SET like_count = GREATEST(0, like_count-1) WHERE id=$1`, trackID)
		return false, err
	}
	_, err := database.Exec(`INSERT INTO track_likes (user_id, track_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, userID, trackID)
	_, _ = database.Exec(`UPDATE tracks SET like_count = like_count+1 WHERE id=$1`, trackID)
	return true, err
}

func IncrementTrackPlay(database *sql.DB, trackID string) error {
	_, err := database.Exec(`UPDATE tracks SET play_count = play_count+1 WHERE id=$1`, trackID)
	return err
}

func IsTrackLiked(database *sql.DB, userID, trackID string) (bool, error) {
	var exists bool
	err := database.QueryRow(`SELECT EXISTS(SELECT 1 FROM track_likes WHERE user_id=$1 AND track_id=$2)`, userID, trackID).Scan(&exists)
	return exists, err
}

type ArtistStats struct {
	TotalTracks int
	TotalPlays  int
	TotalLikes  int
}

func GetArtistStats(database *sql.DB, authorID string) ArtistStats {
	var s ArtistStats
	_ = database.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(play_count),0), COALESCE(SUM(like_count),0)
		FROM tracks WHERE author_id = $1`, authorID).Scan(&s.TotalTracks, &s.TotalPlays, &s.TotalLikes)
	return s
}

func DeleteTrack(database *sql.DB, trackID, authorID string) (bool, error) {
	res, err := database.Exec(`DELETE FROM tracks WHERE id=$1 AND author_id=$2`, trackID, authorID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func scanTracks(rows *sql.Rows) ([]*model.Track, error) {
	var tracks []*model.Track
	for rows.Next() {
		t := &model.Track{}
		var tags pq.StringArray
		err := rows.Scan(
			&t.ID, &t.AuthorID, &t.AuthorHandle, &t.AuthorName, &t.AvatarURL, &t.IsVerified,
			&t.Title, &t.Description, &t.AudioURL, &t.CoverURL,
			&t.DurationSecs, &t.Genre, &tags, &t.PriceCents, &t.IsFree,
			&t.PlayCount, &t.LikeCount, &t.CreatedAt,
		)
		if err != nil {
			return nil, err
		}
		t.Tags = []string(tags)
		tracks = append(tracks, t)
	}
	return tracks, rows.Err()
}

// NOTE: content deletion is SoftDeleteWork (works.deleted_at), reached through
// the work_delete event. The hard DELETE against the posts table that used to
// live here is gone with the table.

// ── Sessions ──────────────────────────────────────────────────────────────────

func CreateSession(database *sql.DB, userID, surface string) (string, error) {
	var id string
	err := database.QueryRow(`INSERT INTO feed_sessions (user_id, surface) VALUES ($1, $2) RETURNING id`, userID, surface).Scan(&id)
	return id, err
}

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

// GetContentVelocitiesBatch returns normalised 0–1 velocity scores for a set of
// content IDs in a single query. windowHours controls the lookback window.
// Use this before building a rank request to pass real signal to AethyrRank.
func GetContentVelocitiesBatch(database *sql.DB, contentIDs []string, windowHours int) (map[string]float64, error) {
	if len(contentIDs) == 0 {
		return map[string]float64{}, nil
	}
	rows, err := database.Query(`
		SELECT content_id,
		       COUNT(*) FILTER (WHERE event_type IN ('like','share','comment','save','view_complete')) AS eng,
		       GREATEST(1, COUNT(*) FILTER (WHERE event_type = 'impression')) AS imp
		FROM feedback_events
		WHERE content_id = ANY($1)
		  AND created_at >= NOW() - ($2::int * interval '1 hour')
		GROUP BY content_id
	`, pq.Array(contentIDs), windowHours)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]float64, len(contentIDs))
	for rows.Next() {
		var cid string
		var eng, imp int
		if err := rows.Scan(&cid, &eng, &imp); err != nil {
			continue
		}
		v := float64(eng) / float64(imp)
		if v > 1.0 {
			v = 1.0
		}
		result[cid] = v
	}
	return result, nil
}

// GetContentVelocity returns a normalised 0–1 velocity score for a content item:
// engagement events (like/share/comment/save) in the last windowHours divided by
// total impressions in the same window (floored at 1 to avoid division by zero).
func GetContentVelocity(database *sql.DB, contentID string, windowHours int) (float64, error) {
	var engagements, impressions int
	err := database.QueryRow(`
		SELECT
			COUNT(*) FILTER (WHERE event_type IN ('like','share','comment','save','view_complete')) AS eng,
			GREATEST(1, COUNT(*) FILTER (WHERE event_type = 'impression')) AS imp
		FROM feedback_events
		WHERE content_id = $1
		  AND created_at >= NOW() - ($2::int * interval '1 hour')
	`, contentID, windowHours).Scan(&engagements, &impressions)
	if err != nil {
		return 0, err
	}
	v := float64(engagements) / float64(impressions)
	if v > 1.0 {
		v = 1.0
	}
	return v, nil
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
		LEFT JOIN pial_roots plr ON plr.pial_id = u.pial_id
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

// FilterBlockedPosts removes posts whose author is in the blocked set.
// Operates in-place on the slice header — callers should use the returned slice.
func FilterBlockedPosts(posts []*model.Post, blocked map[string]bool) []*model.Post {
	if len(blocked) == 0 {
		return posts
	}
	out := posts[:0]
	for _, p := range posts {
		if !blocked[p.AuthorID] {
			out = append(out, p)
		}
	}
	return out
}

// ── Reply restrictions ────────────────────────────────────────────────────────

// SetCommentGating updates a post's comment_gating. Verifies ownership.
// gating values: "open" | "followers" | "verified" | "none"
func SetCommentGating(database *sql.DB, postID, userID, gating string) error {
	res, err := database.Exec(`
		UPDATE works SET comment_gating = $1
		WHERE id = $2 AND author_id = $3`,
		gating, postID, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("work not found or not owned by user")
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
			PIALID:      u.PIALID,
		})
	}
	if err := markViewerFollows(database, viewerID, entries); err != nil {
		log.Printf("[follow-list] mark viewer follows: %v", err)
	}
	return entries
}

// RevokeAllSessions ends every session of userID — the account is leaving,
// by deactivation or by a queued deletion, so no device stays signed in.
func RevokeAllSessions(database *sql.DB, userID string) error {
	_, err := database.Exec(`DELETE FROM user_sessions WHERE user_id = $1`, userID)
	return err
}

// GetPIALKYCVerifiedAt returns the instant the identity tier on this PIAL root
// was recorded, or nil when it never was.
func GetPIALKYCVerifiedAt(database *sql.DB, pialID string) *time.Time {
	if database == nil || pialID == "" {
		return nil
	}
	var at sql.NullTime
	if err := database.QueryRow(
		`SELECT kyc_verified_at FROM pial_roots WHERE pial_id = $1::uuid`, pialID,
	).Scan(&at); err != nil || !at.Valid {
		return nil
	}
	t := at.Time
	return &t
}

// FilterMutedPosts removes posts whose author is in the muted set.
func FilterMutedPosts(posts []*model.Post, muted map[string]bool) []*model.Post {
	if len(muted) == 0 {
		return posts
	}
	out := posts[:0]
	for _, p := range posts {
		if !muted[p.AuthorID] {
			out = append(out, p)
		}
	}
	return out
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

// IsSubscribed returns true if subscriberID is currently subscribed to creatorID.
func IsSubscribed(database *sql.DB, subscriberID, creatorID string) bool {
	var exists bool
	_ = database.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM subscriptions
			WHERE subscriber_id = $1 AND creator_id = $2
			  AND status = 'active'
			  AND (expires_at IS NULL OR expires_at > NOW())
		)
	`, subscriberID, creatorID).Scan(&exists)
	return exists
}

type CreatorPlan struct {
	ID          string
	Name        string
	Description string
	PriceAet    int
}

func GetCreatorPlans(database *sql.DB, creatorID string) ([]CreatorPlan, error) {
	rows, err := database.Query(`
		SELECT id, name, description, price_aet
		FROM creator_plans
		WHERE creator_id = $1 AND active = TRUE
		ORDER BY price_aet ASC
	`, creatorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var plans []CreatorPlan
	for rows.Next() {
		var p CreatorPlan
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.PriceAet); err == nil {
			plans = append(plans, p)
		}
	}
	return plans, nil
}

func CreateSubscription(database *sql.DB, subscriberID, creatorID, planID string, priceAet int, durationDays int) error {
	var expiresAt interface{}
	if durationDays > 0 {
		expiresAt = time.Now().AddDate(0, 0, durationDays)
	}
	_, err := database.Exec(`
		INSERT INTO subscriptions (subscriber_id, creator_id, plan_id, price_aet, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (subscriber_id, creator_id) DO UPDATE SET
			status     = 'active',
			plan_id    = EXCLUDED.plan_id,
			price_aet  = EXCLUDED.price_aet,
			expires_at = EXCLUDED.expires_at,
			cancelled_at = NULL
	`, subscriberID, creatorID, planID, priceAet, expiresAt)
	return err
}

func CancelSubscription(database *sql.DB, subscriberID, creatorID string) error {
	_, err := database.Exec(`
		UPDATE subscriptions
		SET status = 'cancelled', cancelled_at = NOW()
		WHERE subscriber_id = $1 AND creator_id = $2 AND status = 'active'
	`, subscriberID, creatorID)
	return err
}

func GetSubscriberCount(database *sql.DB, creatorID string) int {
	var n int
	_ = database.QueryRow(`
		SELECT COUNT(*) FROM subscriptions
		WHERE creator_id = $1 AND status = 'active'
		  AND (expires_at IS NULL OR expires_at > NOW())
	`, creatorID).Scan(&n)
	return n
}

// GetSubscribers returns a creator's active subscribers as follow-list rows. It
// takes the viewer because those rows render a follow control: without the viewer's
// follow state a subscriber the viewer already follows would be offered "Follow".
func GetSubscribers(database *sql.DB, creatorID, viewerID string, limit int) ([]FollowListEntry, error) {
	rows, err := database.Query(`
		SELECT u.id::text,
		       u.handle,
		       COALESCE(NULLIF(TRIM(p.display_name),''), CASE WHEN LENGTH(u.handle)>=40 THEN 'User '||LEFT(u.handle,6) ELSE INITCAP(REPLACE(u.handle,'_',' ')) END),
		       COALESCE(p.avatar_url, ''),
		       COALESCE(u.pial_id::text, '')
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
		if err := rows.Scan(&e.ID, &e.Handle, &e.DisplayName, &e.AvatarURL, &e.PIALID); err != nil {
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

// ── Analytics queries (Astraon stub) ─────────────────────────────────────────
// TODO_ASTRAON: all functions in this section migrate to HTTP calls to
// Astraon (port 8088, f33d3r_analytics DB) once that brain is online.
// For now they query f33d3r_feed directly.

// CreatorAnalyticsSummary holds aggregate stats for a PIAL's posts.
type CreatorAnalyticsSummary struct {
	TotalPosts       int
	TotalImpressions int64
	TotalLikes       int64
	TotalReposts     int64
	TotalComments    int64
	TotalSaves       int64
	TotalViewSeconds int64
	EngagementRate   float64 // (likes+reposts+comments) / max(impressions,1) * 100
}

// AnalyticsPostRow holds per-post metrics for the analytics table.
type AnalyticsPostRow struct {
	PostID        string
	Body          string // truncated to 80 chars
	CreatedAt     time.Time
	Impressions   int64
	Likes         int64
	Reposts       int64
	Comments      int64
	Saves         int64
	EngagementPct float64 // (likes+reposts+comments) / max(impressions,1) * 100
	MaxPct        float64 // normalised bar width: row_total / max_total * 100
}

// SiteAnalytics holds platform-wide aggregate counters.
type SiteAnalytics struct {
	TotalUsers       int
	TotalPosts       int
	TotalImpressions int64
	TotalLikes       int64
	NewUsersWeek     int
	NewPostsWeek     int
}

// GetCreatorAnalyticsSummary returns aggregate metrics across all non-deleted
// posts for a given PIAL owner.
// TODO_ASTRAON: migrate to GET /v1/analytics/creator/{pial_id}/summary
func GetCreatorAnalyticsSummary(database *sql.DB, pialID string) (*CreatorAnalyticsSummary, error) {
	if database == nil || pialID == "" {
		return &CreatorAnalyticsSummary{}, nil
	}
	row := database.QueryRow(`
		SELECT
		    COUNT(w.id),
		    COALESCE(SUM(w.view_count), 0),
		    COALESCE(SUM((SELECT COUNT(*) FROM work_reactions wr WHERE wr.work_id = w.id AND wr.reaction_type='like')), 0),
		    COALESCE(SUM((SELECT COUNT(*) FROM work_reactions wr WHERE wr.work_id = w.id AND wr.reaction_type='repost')), 0),
		    COALESCE(SUM((SELECT COUNT(*) FROM work_citations wc WHERE wc.target_id = w.id AND wc.citation_type='reply')), 0),
		    COALESCE(SUM((SELECT COUNT(*) FROM work_reactions wr WHERE wr.work_id = w.id AND wr.reaction_type='bookmark')), 0),
		    0
		FROM works w
		JOIN users u ON u.id = w.author_id
		WHERE u.pial_id = $1::uuid
		  AND w.kind <> 'reply'
		  AND w.deleted_at IS NULL
	`, pialID)
	s := &CreatorAnalyticsSummary{}
	err := row.Scan(
		&s.TotalPosts,
		&s.TotalImpressions,
		&s.TotalLikes,
		&s.TotalReposts,
		&s.TotalComments,
		&s.TotalSaves,
		&s.TotalViewSeconds,
	)
	if err != nil {
		return s, err
	}
	if s.TotalImpressions > 0 {
		total := float64(s.TotalLikes + s.TotalReposts + s.TotalComments)
		s.EngagementRate = total / float64(s.TotalImpressions) * 100
	}
	return s, nil
}

// GetCreatorTopPosts returns up to limit posts for a PIAL owner ordered by
// total engagement (likes+reposts+comments) descending.
// TODO_ASTRAON: migrate to GET /v1/analytics/creator/{pial_id}/top-posts
func GetCreatorTopPosts(database *sql.DB, pialID string, limit int) ([]AnalyticsPostRow, error) {
	if database == nil || pialID == "" {
		return nil, nil
	}
	rows, err := database.Query(`
		SELECT * FROM (
		    SELECT
		        w.id,
		        w.body,
		        w.created_at,
		        COALESCE(w.view_count, 0) AS impressions,
		        (SELECT COUNT(*) FROM work_reactions wr WHERE wr.work_id = w.id AND wr.reaction_type='like')     AS likes,
		        (SELECT COUNT(*) FROM work_reactions wr WHERE wr.work_id = w.id AND wr.reaction_type='repost')   AS reposts,
		        (SELECT COUNT(*) FROM work_citations wc WHERE wc.target_id = w.id AND wc.citation_type='reply') AS comments,
		        (SELECT COUNT(*) FROM work_reactions wr WHERE wr.work_id = w.id AND wr.reaction_type='bookmark') AS saves
		    FROM works w
		    JOIN users u ON u.id = w.author_id
		    WHERE u.pial_id = $1::uuid
		      AND w.kind <> 'reply'
		      AND w.deleted_at IS NULL
		) t
		ORDER BY (likes + reposts + comments) DESC, created_at DESC
		LIMIT $2
	`, pialID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AnalyticsPostRow
	var maxEngagement int64
	for rows.Next() {
		var r AnalyticsPostRow
		if err := rows.Scan(
			&r.PostID, &r.Body, &r.CreatedAt,
			&r.Impressions, &r.Likes, &r.Reposts, &r.Comments, &r.Saves,
		); err != nil {
			return nil, err
		}
		// Truncate body to 80 chars
		runes := []rune(r.Body)
		if len(runes) > 80 {
			r.Body = string(runes[:80]) + "…"
		}
		eng := r.Likes + r.Reposts + r.Comments
		if eng > maxEngagement {
			maxEngagement = eng
		}
		imp := r.Impressions
		if imp < 1 {
			imp = 1
		}
		r.EngagementPct = float64(eng) / float64(imp) * 100
		if r.EngagementPct > 100 {
			r.EngagementPct = 100
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Normalise MaxPct so CSS bars are relative to the top post.
	for i := range out {
		eng := out[i].Likes + out[i].Reposts + out[i].Comments
		if maxEngagement > 0 {
			out[i].MaxPct = float64(eng) / float64(maxEngagement) * 100
		} else {
			out[i].MaxPct = 0
		}
	}
	return out, nil
}

// GetSiteAnalytics returns platform-wide aggregate counters for the admin panel.
// TODO_ASTRAON: migrate to GET /v1/analytics/site/summary
func GetSiteAnalytics(database *sql.DB) (*SiteAnalytics, error) {
	if database == nil {
		return &SiteAnalytics{}, nil
	}
	s := &SiteAnalytics{}
	database.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&s.TotalUsers)
	database.QueryRow(`SELECT COUNT(*) FROM works WHERE kind <> 'reply' AND deleted_at IS NULL`).Scan(&s.TotalPosts)
	database.QueryRow(`SELECT COALESCE(SUM(view_count),0) FROM works WHERE deleted_at IS NULL`).Scan(&s.TotalImpressions)
	database.QueryRow(`SELECT COUNT(*) FROM work_reactions WHERE reaction_type = 'like'`).Scan(&s.TotalLikes)
	database.QueryRow(`SELECT COUNT(*) FROM users WHERE created_at > NOW() - INTERVAL '7 days'`).Scan(&s.NewUsersWeek)
	database.QueryRow(`SELECT COUNT(*) FROM works WHERE kind <> 'reply' AND deleted_at IS NULL AND created_at > NOW() - INTERVAL '7 days'`).Scan(&s.NewPostsWeek)
	return s, nil
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
		LEFT JOIN pial_roots plr ON plr.pial_id = u.pial_id
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

func CleanExpiredSessions(database *sql.DB) (int64, error) {
	res, err := database.Exec(`DELETE FROM user_sessions WHERE expires_at < NOW()`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
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

// GetUserPIAL returns the PIAL ID for a given user ID, or empty string if not found.
func GetUserPIAL(database *sql.DB, userID string) string {
	var pial string
	database.QueryRow(`SELECT COALESCE(pial_id::text,'') FROM users WHERE id=$1`, userID).Scan(&pial)
	return pial
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
// actor should be "system", a moderator handle, or "zodacare".
//
// works is the only content table: this used to probe posts first and fall through,
// which meant a lookup miss on an empty table on every single moderation decision.
func SetPostScanState(database *sql.DB, postID, toState, reason, actor string) error {
	var fromState string
	err := database.QueryRow(`SELECT COALESCE(scan_state,'clean') FROM works WHERE id = $1`, postID).Scan(&fromState)
	if err != nil {
		return err
	}
	if _, err2 := database.Exec(`UPDATE works SET scan_state = $1 WHERE id = $2`, toState, postID); err2 != nil {
		return err2
	}
	// scan_state alone does not block or age-gate anything — is_blocked and is_nsfw
	// are what the read paths and the content gate actually key off. Letting either
	// fail while this function returns nil produces a work the moderation queue
	// shows as blocked and the feed still serves.
	if toState == "blocked" {
		if _, err2 := database.Exec(`UPDATE works SET is_blocked = TRUE WHERE id = $1`, postID); err2 != nil {
			return fmt.Errorf("work %s: scan_state set to blocked but is_blocked not set: %w", postID, err2)
		}
	}
	if toState == "age_gated" {
		if _, err2 := database.Exec(`UPDATE works SET is_nsfw = TRUE WHERE id = $1`, postID); err2 != nil {
			return fmt.Errorf("work %s: scan_state set to age_gated but is_nsfw not set: %w", postID, err2)
		}
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

// ScanResult holds the full output of a content-scan risk assessment.
type ScanResult struct {
	PostID          string
	ScanVersion     string
	NudityScore     float64
	GoreScore       float64
	ClickbaitScore  float64
	OCRText         string
	Transcript      string
	HateSignals     []string
	ViolenceSignals []string
	SelfHarmSignals []string
	RiskLevel       string
	Recommendation  string
	Signals         []string
	IsDuplicate     bool
	DuplicateType   string
	OriginalPostID  string
}

// StoreScanResult saves raw scan signals for a post (upsert).
func StoreScanResult(database *sql.DB, r *ScanResult) error {
	_, err := database.Exec(`
		INSERT INTO content_scan_results
		  (post_id, scan_version, nudity_score, gore_score, clickbait_score,
		   ocr_text, transcript, hate_signals, risk_level, recommendation, signals,
		   is_duplicate, duplicate_type, original_post_id, scanned_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,NOW())
		ON CONFLICT (post_id) DO UPDATE SET
		  scan_version     = EXCLUDED.scan_version,
		  nudity_score     = EXCLUDED.nudity_score,
		  gore_score       = EXCLUDED.gore_score,
		  clickbait_score  = EXCLUDED.clickbait_score,
		  ocr_text         = EXCLUDED.ocr_text,
		  transcript       = EXCLUDED.transcript,
		  hate_signals     = EXCLUDED.hate_signals,
		  risk_level       = EXCLUDED.risk_level,
		  recommendation   = EXCLUDED.recommendation,
		  signals          = EXCLUDED.signals,
		  is_duplicate     = EXCLUDED.is_duplicate,
		  duplicate_type   = EXCLUDED.duplicate_type,
		  original_post_id = EXCLUDED.original_post_id,
		  scanned_at       = NOW()`,
		r.PostID, r.ScanVersion, r.NudityScore, r.GoreScore, r.ClickbaitScore,
		r.OCRText, r.Transcript, pq.Array(r.HateSignals), r.RiskLevel, r.Recommendation,
		pq.Array(r.Signals), r.IsDuplicate, r.DuplicateType, r.OriginalPostID,
	)
	return err
}

// ReleaseStaleHumanReviewPosts auto-approves works that have been sitting in
// human_review longer than minAge — a human clearly hasn't reviewed them.
// Returns the IDs of every work that was released.
func ReleaseStaleHumanReviewPosts(database *sql.DB, minAge time.Duration) ([]string, error) {
	rows, err := database.Query(
		`UPDATE works SET scan_state = 'clean'
		 WHERE scan_state = 'human_review'
		   AND deleted_at IS NULL
		   AND created_at < NOW() - make_interval(secs => $1)
		 RETURNING id`,
		minAge.Seconds(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var released []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			released = append(released, id)
		}
	}

	// Write an audit log entry for each released work and close any reports that
	// pointed at it — clearing the work must clear its reports, or the report
	// cards resurrect on the next queue poll.
	for _, id := range released {
		if _, err := database.Exec(
			`INSERT INTO content_moderation_log (post_id, from_state, to_state, reason, actor)
			 VALUES ($1, 'human_review', 'clean', 'sweep:stale_human_review_timeout', 'system')`,
			id,
		); err != nil {
			log.Printf("[sweep] moderation log NOT written for released work %s: %v", id, err)
		}
		if _, err := ResolveReportsForContent(database, id, "resolved", "system"); err != nil {
			log.Printf("[sweep] reports NOT resolved for released work %s — its report cards will "+
				"resurrect on the next queue poll: %v", id, err)
		}
	}
	return released, nil
}

// GetHumanReviewPosts returns works in human_review state for the moderation queue.
// Includes scan signals, media thumbnail URL (first media asset if any), and author avatar.
func GetHumanReviewPosts(database *sql.DB, limit int) ([]map[string]interface{}, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	rows, err := database.Query(`
		SELECT w.id, w.body, u.handle, w.scan_state, w.created_at,
		       COALESCE(r.risk_level,'unknown'), COALESCE(r.nudity_score,0),
		       COALESCE(r.gore_score,0), COALESCE(r.clickbait_score,0),
		       COALESCE(array_to_string(r.signals,'|'),''),
		       COALESCE(w.media_urls, '{}'),
		       COALESCE(up.avatar_url, '')
		FROM works w
		JOIN users u ON u.id = w.author_id
		LEFT JOIN user_profiles up ON up.user_id = u.id
		LEFT JOIN content_scan_results r ON r.post_id = w.id::text
		WHERE w.scan_state IN ('human_review','flagged','pending')
		  AND w.deleted_at IS NULL
		ORDER BY created_at ASC
		LIMIT $1`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]interface{}
	for rows.Next() {
		var id, body, handle, state, riskLevel, signalStr, avatarURL string
		var createdAt time.Time
		var nudity, gore, clickbait float64
		var mediaURLs pq.StringArray
		if err := rows.Scan(&id, &body, &handle, &state, &createdAt,
			&riskLevel, &nudity, &gore, &clickbait, &signalStr,
			&mediaURLs, &avatarURL); err != nil {
			continue
		}
		// Surface the first media URL as a preview thumbnail.
		thumbURL := ""
		if len(mediaURLs) > 0 {
			thumbURL = mediaURLs[0]
		}
		out = append(out, map[string]interface{}{
			"post_id":         id,
			"body":            body,
			"author_handle":   handle,
			"author_avatar":   avatarURL,
			"scan_state":      state,
			"created_at":      createdAt,
			"risk_level":      riskLevel,
			"nudity_score":    nudity,
			"gore_score":      gore,
			"clickbait_score": clickbait,
			"signals":         signalStr,
			"thumb_url":       thumbURL,
		})
	}
	return out, nil
}

// GetAdminUserIDs returns the IDs of all users with role = 'admin'.
// Used to fan-out moderation notifications when a post enters human_review.
func GetAdminUserIDs(database *sql.DB) []string {
	rows, err := database.Query(`SELECT id FROM users WHERE role = 'admin'`)
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

// IsBannedHash checks whether a hash appears in the banned_content_hashes registry.
func IsBannedHash(database *sql.DB, hashType, hashValue string) (bool, string, error) {
	var category string
	err := database.QueryRow(
		`SELECT category FROM banned_content_hashes WHERE hash_type = $1 AND hash_value = $2 LIMIT 1`,
		hashType, hashValue,
	).Scan(&category)
	if err == sql.ErrNoRows {
		return false, "", nil
	}
	return err == nil, category, err
}

// AddBannedHash registers a hash in the banned_content_hashes registry.
func AddBannedHash(database *sql.DB, hashType, hashValue, category, addedBy, note string) error {
	_, err := database.Exec(
		`INSERT INTO banned_content_hashes (hash_type, hash_value, category, added_by, note)
		 VALUES ($1,$2,$3,$4,$5) ON CONFLICT (hash_type,hash_value) DO NOTHING`,
		hashType, hashValue, category, addedBy, note,
	)
	return err
}

// GetModerationLog returns the state transition history for a post.
func GetModerationLog(database *sql.DB, postID string) ([]map[string]interface{}, error) {
	rows, err := database.Query(`
		SELECT from_state, to_state, reason, actor, created_at
		FROM content_moderation_log
		WHERE post_id = $1
		ORDER BY created_at ASC`, postID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]interface{}
	for rows.Next() {
		var from, to, reason, actor string
		var createdAt time.Time
		if err := rows.Scan(&from, &to, &reason, &actor, &createdAt); err != nil {
			continue
		}
		out = append(out, map[string]interface{}{
			"from_state": from,
			"to_state":   to,
			"reason":     reason,
			"actor":      actor,
			"created_at": createdAt,
		})
	}
	return out, nil
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
		LEFT JOIN pial_roots plr ON plr.pial_id = u.pial_id
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

// StoreVideoRawHash records the SHA-256 of a raw uploaded video file.
// Called at TUS assembly time — before transcoding — so future uploads of the
// same file can be blocked client-side before a single byte is transferred.
// ON CONFLICT DO NOTHING: the first uploader's record is authoritative.
func StoreVideoRawHash(database *sql.DB, sha256, uploaderPIAL, uploaderHandle string) error {
	if sha256 == "" || database == nil {
		return nil
	}
	_, err := database.Exec(`
		INSERT INTO video_raw_hashes (sha256, uploader_pial, uploader_handle)
		VALUES ($1, $2, $3)
		ON CONFLICT (sha256) DO NOTHING`,
		sha256, uploaderPIAL, uploaderHandle)
	return err
}

// CheckVideoRawHash looks up a raw-file SHA-256 in the dedup store.
// Returns (originalHandle, postID, found, err).
// found=false means the file is new and upload may proceed.
func CheckVideoRawHash(database *sql.DB, sha256 string) (originalHandle, postID string, found bool, err error) {
	if sha256 == "" || database == nil {
		return "", "", false, nil
	}
	err = database.QueryRow(
		`SELECT uploader_handle, post_id FROM video_raw_hashes WHERE sha256 = $1`, sha256).
		Scan(&originalHandle, &postID)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return originalHandle, postID, true, nil
}

// ── Image raw-hash dedup store ────────────────────────────────────────────────

// CheckImageRawHash looks up a raw file SHA-256 in image_raw_hashes.
// Returns the canonical media URL, original uploader handle, and whether it was found.
func CheckImageRawHash(database *sql.DB, sha256 string) (mediaURL, uploaderHandle string, found bool, err error) {
	if sha256 == "" || database == nil {
		return "", "", false, nil
	}
	err = database.QueryRow(
		`SELECT media_url, uploader_handle FROM image_raw_hashes WHERE sha256 = $1`, sha256).
		Scan(&mediaURL, &uploaderHandle)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return mediaURL, uploaderHandle, true, nil
}

// StoreImageRawHash seeds image_raw_hashes after a successful image upload.
// ON CONFLICT DO NOTHING: first uploader's record is authoritative.
func StoreImageRawHash(database *sql.DB, sha256, uploaderPIAL, uploaderHandle, mediaURL string) error {
	if sha256 == "" || database == nil {
		return nil
	}
	_, err := database.Exec(`
		INSERT INTO image_raw_hashes (sha256, uploader_pial, uploader_handle, media_url)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (sha256) DO NOTHING`,
		sha256, uploaderPIAL, uploaderHandle, mediaURL)
	return err
}

// ── Video raw-hash dedup store ────────────────────────────────────────────────

// AssociateVideoRawHashPost links a video_raw_hashes row to its work once the
// work is created. The post_id column stores the work UUID — naming is legacy.
// Best-effort — callers should log but not fail on error.
func AssociateVideoRawHashPost(database *sql.DB, uploaderPIAL, workID string) {
	if database == nil || uploaderPIAL == "" || workID == "" {
		return
	}
	database.Exec(`
		UPDATE video_raw_hashes SET post_id = $1
		WHERE uploader_pial = $2 AND post_id = '' AND created_at > NOW() - INTERVAL '2 hours'`,
		workID, uploaderPIAL)
}

// VideoDuplicateResult holds the original work's video info returned when a
// duplicate raw-file hash is detected.
type VideoDuplicateResult struct {
	MasterURL     string
	PosterURL     string
	DurationSecs  float64
	Width         int
	Height        int
	WorkID        string
	CreatorHandle string
	CreatorName   string
	CreatorPIAL   string
}

// CheckVideoDuplicateByWork looks up a raw-file SHA-256 and, if found, returns
// the original work's video metadata so the compose box can reference it.
// Returns nil, nil when the hash is not in the store (i.e. the file is new).
// post_id in video_raw_hashes stores the work UUID (legacy column name).
func CheckVideoDuplicateByWork(database *sql.DB, rawSHA256 string) (*VideoDuplicateResult, error) {
	if rawSHA256 == "" || database == nil {
		return nil, nil
	}

	// Look up the work_id stored in the post_id column (legacy name).
	var workID string
	err := database.QueryRow(
		`SELECT post_id FROM video_raw_hashes WHERE sha256 = $1 AND post_id != '' LIMIT 1`,
		rawSHA256,
	).Scan(&workID)
	if err == sql.ErrNoRows {
		return nil, nil // not a duplicate, or hash not yet linked to a work
	}
	if err != nil {
		return nil, err
	}

	// Fetch the original work's video metadata, author handle, and display name.
	var res VideoDuplicateResult
	err = database.QueryRow(`
		SELECT
		    COALESCE(w.video_master_url, w.video_watermarked_url, ''),
		    COALESCE(w.video_poster_url, ''),
		    COALESCE(w.video_duration_secs, 0)::float8,
		    COALESCE(w.video_width, 0),
		    COALESCE(w.video_height, 0),
		    w.id::text,
		    u.handle,
		    COALESCE(up.display_name, u.handle),
		    w.author_pial::text
		FROM works w
		JOIN users u ON u.id = w.author_id
		LEFT JOIN user_profiles up ON up.user_id = w.author_id
		WHERE w.id = $1::uuid AND w.deleted_at IS NULL`,
		workID,
	).Scan(
		&res.MasterURL, &res.PosterURL,
		&res.DurationSecs, &res.Width, &res.Height,
		&res.WorkID, &res.CreatorHandle, &res.CreatorName, &res.CreatorPIAL,
	)
	if err == sql.ErrNoRows {
		// Work was deleted — treat as not a duplicate.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if res.MasterURL == "" {
		// Work exists but has no video — hash collision or race; treat as clean.
		return nil, nil
	}
	return &res, nil
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

// RecordScanFeedback writes an admin moderation decision alongside the signals
// that triggered the flag. This is the training signal for Abraxas.
func RecordScanFeedback(database *sql.DB, postID, decision, actionedBy string, signals []string) error {
	_, err := database.Exec(`
		INSERT INTO scan_feedback (post_id, signals, admin_decision, actioned_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING`,
		postID, pq.Array(signals), decision, actionedBy)
	return err
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

// GetPIALKYCTier returns the kyc_tier for a PIAL root. Returns "none" on any error.
func GetPIALKYCTier(db *sql.DB, pialID string) string {
	var tier string
	if err := db.QueryRow(`SELECT kyc_tier FROM pial_roots WHERE pial_id = $1::uuid`, pialID).Scan(&tier); err != nil {
		return "none"
	}
	return tier
}

// MintContentReceipt inserts a content_receipts row for a work. Idempotent via ON CONFLICT.
//
// The column is work_id: until migration 0006 it was post_id with a foreign key
// into the posts table, so passing a work id here violated that key and every
// mint failed. The receipt now references the row it is a receipt for.
func MintContentReceipt(db *sql.DB, pialID, workID, bodyHash, kycTier string) error {
	_, err := db.Exec(`
		INSERT INTO content_receipts (pial_id, work_id, body_hash, kyc_tier_at_mint)
		VALUES ($1::uuid, $2::uuid, $3, $4)
		ON CONFLICT DO NOTHING
	`, pialID, workID, bodyHash, kycTier)
	return err
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
