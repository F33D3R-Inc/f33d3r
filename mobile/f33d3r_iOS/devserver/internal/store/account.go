package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Account-level reads and writes: profile edits, settings, sessions, the
// follow lists, tags, saves, and editing a work. Each has a twin in
// feed-engine's profile.go, settings handlers or works.go.

// ProfileUpdate is what the edit-profile screen sends. Nil means "leave it".
type ProfileUpdate struct {
	DisplayName *string
	Bio         *string
	Pronouns    *string
	Location    *string
	Website     *string
	AccentHex   *string
	ThemeID     *string
	AvatarURL   *string
	HeaderURL   *string

	IsAdultCreator         *bool
	BirthdayMdVisibility   *string
	BirthdayYearVisibility *string
	// CountryCode is ISO-3166 alpha-2; an empty string clears it.
	CountryCode *string
	// SocialLinks and ExternalTipLinks replace the whole map when set.
	SocialLinks      *map[string]string
	ExternalTipLinks *map[string]string
}

// The closed vocabularies the settings screen offers, mirrored from the web.
var (
	birthdayVisibilities = map[string]bool{"everyone": true, "followers": true, "mutual_followers": true, "only_me": true}
	socialLinkKeys       = map[string]bool{"youtube": true, "twitch": true, "spotify": true, "soundcloud": true, "kick": true, "other": true}
	tipLinkKeys          = map[string]bool{"cashapp": true, "venmo": true, "paypal": true, "kofi": true, "buymeacoffee": true, "bitcoin": true, "xrp": true}
	countryCodeRe        = regexp.MustCompile(`^[A-Z]{2}$`)
)

// UpdateProfile applies the non-nil fields.
func (s *Store) UpdateProfile(ctx context.Context, userID string, u ProfileUpdate) error {
	sets := []string{}
	args := []any{}
	add := func(col string, v *string, max int) error {
		if v == nil {
			return nil
		}
		val := strings.TrimSpace(*v)
		if len([]rune(val)) > max {
			return fmt.Errorf("%s is too long (max %d)", col, max)
		}
		sets = append(sets, col+" = ?")
		args = append(args, val)
		return nil
	}
	for _, f := range []struct {
		col string
		v   *string
		max int
	}{
		{"display_name", u.DisplayName, 50}, {"bio", u.Bio, 300}, {"pronouns", u.Pronouns, 30},
		{"location", u.Location, 60}, {"website", u.Website, 200}, {"accent_hex", u.AccentHex, 9},
		{"theme_id", u.ThemeID, 20}, {"avatar_url", u.AvatarURL, 300}, {"header_url", u.HeaderURL, 300},
	} {
		if err := add(f.col, f.v, f.max); err != nil {
			return err
		}
	}
	if u.AccentHex != nil && *u.AccentHex != "" && !accentRe.MatchString(*u.AccentHex) {
		return errors.New("accent_hex must be #rrggbb")
	}
	if u.ThemeID != nil && !themeIDs[strings.TrimSpace(*u.ThemeID)] {
		return errors.New("theme_id must be one of void, aurora, sakura, obsidian, moss, dusk")
	}
	if u.Website != nil && *u.Website != "" && !strings.HasPrefix(*u.Website, "http://") && !strings.HasPrefix(*u.Website, "https://") {
		return errors.New("website must start with http:// or https://")
	}
	if u.IsAdultCreator != nil {
		sets = append(sets, "is_adult_creator = ?")
		args = append(args, boolInt(*u.IsAdultCreator))
	}
	for _, f := range []struct {
		col string
		v   *string
	}{{"birthday_md_visibility", u.BirthdayMdVisibility}, {"birthday_year_visibility", u.BirthdayYearVisibility}} {
		if f.v == nil {
			continue
		}
		if !birthdayVisibilities[*f.v] {
			return fmt.Errorf("%s must be one of everyone, followers, mutual_followers, only_me", f.col)
		}
		sets = append(sets, f.col+" = ?")
		args = append(args, *f.v)
	}
	if u.CountryCode != nil {
		code := strings.ToUpper(strings.TrimSpace(*u.CountryCode))
		if code == "" {
			sets = append(sets, "country_code = NULL")
		} else {
			if !countryCodeRe.MatchString(code) {
				return errors.New("country_code must be an ISO-3166 alpha-2 code")
			}
			sets = append(sets, "country_code = ?")
			args = append(args, code)
		}
	}
	if u.SocialLinks != nil {
		clean, err := cleanLinks(*u.SocialLinks, socialLinkKeys, true)
		if err != nil {
			return fmt.Errorf("social_links: %w", err)
		}
		sets = append(sets, "social_links = ?")
		args = append(args, encodeLinkMap(clean))
	}
	if u.ExternalTipLinks != nil {
		clean, err := cleanLinks(*u.ExternalTipLinks, tipLinkKeys, false)
		if err != nil {
			return fmt.Errorf("external_tip_links: %w", err)
		}
		sets = append(sets, "external_tip_links = ?")
		args = append(args, encodeLinkMap(clean))
	}
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, Now(), userID)
	_, err := s.db.ExecContext(ctx, `UPDATE user_profiles SET `+strings.Join(sets, ", ")+` WHERE user_id = ?`, args...)
	return err
}

// cleanLinks keeps the known keys with non-empty values, trimmed; a URL-valued
// map must hold http(s) URLs. An unknown key is refused rather than dropped, so
// a typo on the client is an error it hears about.
func cleanLinks(in map[string]string, known map[string]bool, urls bool) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range in {
		if !known[k] {
			return nil, fmt.Errorf("unknown key %q", k)
		}
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if len(v) > 300 {
			return nil, fmt.Errorf("%s is too long (max 300)", k)
		}
		if urls && !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
			return nil, fmt.Errorf("%s must start with http:// or https://", k)
		}
		out[k] = v
	}
	return out, nil
}

// themeIDs is the web's closed set of accent themes (`body[data-theme]`).
var themeIDs = map[string]bool{"void": true, "aurora": true, "sakura": true, "obsidian": true, "moss": true, "dusk": true}

// Settings the app can change. Names follow the web's event types.
func (s *Store) SetContentSetting(ctx context.Context, userID, value string) error {
	switch value {
	case "safe_mode", "default", "adult_enabled":
	default:
		return errors.New("unknown content_setting")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE user_profiles SET content_setting = ?, updated_at = ? WHERE user_id = ?`, value, Now(), userID)
	return err
}

func (s *Store) SetShowSensitive(ctx context.Context, userID string, on bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE user_profiles SET show_sensitive = ?, updated_at = ? WHERE user_id = ?`, boolInt(on), Now(), userID)
	return err
}

func (s *Store) SetCelebrations(ctx context.Context, userID string, on bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE user_profiles SET celebrations_enabled = ?, updated_at = ? WHERE user_id = ?`, boolInt(on), Now(), userID)
	return err
}

func (s *Store) SetPrivate(ctx context.Context, userID string, on bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE user_profiles SET is_private = ?, updated_at = ? WHERE user_id = ?`, boolInt(on), Now(), userID)
	return err
}

// ChangePassword verifies the current password and stores the new one.
func (s *Store) ChangePassword(ctx context.Context, userID, current, next string) error {
	if len(next) < 8 {
		return errors.New("passwords are at least 8 characters")
	}
	var hash string
	if err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM user_credentials WHERE user_id = ?`, userID).Scan(&hash); err != nil {
		return err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(current)) != nil {
		return errors.New("current password is wrong")
	}
	fresh, err := bcrypt.GenerateFromPassword([]byte(next), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE user_credentials SET password_hash = ?, updated_at = ? WHERE user_id = ?`, string(fresh), Now(), userID)
	return err
}

// ── Sessions ──────────────────────────────────────────────────────────────────

type SessionInfo struct {
	ID         string
	DeviceName string
	IPAddress  string
	CreatedAt  time.Time
	LastSeenAt time.Time
	IsCurrent  bool
}

// Sessions lists the account's live sessions, current first.
func (s *Store) Sessions(ctx context.Context, userID, currentToken string) ([]SessionInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, device_name, ip_address, created_at, last_seen_at, token_hash FROM user_sessions
		WHERE user_id = ? AND expires_at > ? ORDER BY last_seen_at DESC LIMIT 20`, userID, Now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	current := HashToken(currentToken)
	var out []SessionInfo
	for rows.Next() {
		var si SessionInfo
		var created, seen, hash string
		if err := rows.Scan(&si.ID, &si.DeviceName, &si.IPAddress, &created, &seen, &hash); err != nil {
			return nil, err
		}
		si.CreatedAt = ParseTime(created)
		si.LastSeenAt = ParseTime(seen)
		si.IsCurrent = hash == current
		if si.IsCurrent {
			out = append([]SessionInfo{si}, out...)
		} else {
			out = append(out, si)
		}
	}
	if out == nil {
		out = []SessionInfo{}
	}
	return out, rows.Err()
}

// RevokeSession ends one of the account's own sessions.
func (s *Store) RevokeSession(ctx context.Context, userID, sessionID string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM user_sessions WHERE id = ? AND user_id = ?`, sessionID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeOtherSessions ends every session but the current one.
func (s *Store) RevokeOtherSessions(ctx context.Context, userID, currentToken string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_sessions WHERE user_id = ? AND token_hash <> ?`, userID, HashToken(currentToken))
	return err
}

// ── Follow lists ──────────────────────────────────────────────────────────────

// Followers lists who follows userID, newest follow first.
func (s *Store) Followers(ctx context.Context, userID string, limit int, cursor string) ([]*User, string, error) {
	return s.followList(ctx, `JOIN follows f ON f.follower_id = u.id WHERE f.following_id = ?`, userID, limit, cursor)
}

// Following lists who userID follows.
func (s *Store) Following(ctx context.Context, userID string, limit int, cursor string) ([]*User, string, error) {
	return s.followList(ctx, `JOIN follows f ON f.following_id = u.id WHERE f.follower_id = ?`, userID, limit, cursor)
}

func (s *Store) followList(ctx context.Context, join, userID string, limit int, cursor string) ([]*User, string, error) {
	before, err := cursorOrNow(cursor)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.db.QueryContext(ctx, userSelectSQL+join+` AND f.created_at < ? ORDER BY f.created_at DESC LIMIT ?`, userID, before, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []*User
	var stamps []string
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, u)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		// The cursor is the follow time of the last row; look it up.
		last := out[len(out)-1]
		var col, other string
		if strings.Contains(join, "f.follower_id = u.id") {
			col, other = "follower_id", "following_id"
		} else {
			col, other = "following_id", "follower_id"
		}
		s.db.QueryRowContext(ctx, `SELECT created_at FROM follows WHERE `+col+` = ? AND `+other+` = ?`, last.ID, userID).Scan(&next)
	}
	_ = stamps
	if out == nil {
		out = []*User{}
	}
	return out, next, nil
}

// ── Tags ──────────────────────────────────────────────────────────────────────

type TagCount struct {
	Tag   string
	Count int
}

// SearchTags finds tags by prefix with how many live works carry each.
func (s *Store) SearchTags(ctx context.Context, prefix string, limit int) ([]TagCount, error) {
	p := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(prefix, "#")))
	if p == "" {
		return []TagCount{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT lower(tg.value) AS tag, COUNT(*) AS n
		FROM works w, json_each(w.tags) tg
		WHERE w.deleted_at IS NULL AND w.is_blocked = 0 AND lower(tg.value) LIKE ?
		GROUP BY tag ORDER BY n DESC, tag LIMIT ?`, escapeLike(p)+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TagCount{}
	for rows.Next() {
		var t TagCount
		if err := rows.Scan(&t.Tag, &t.Count); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TrendingTags: the tags most used in the last two days.
func (s *Store) TrendingTags(ctx context.Context, limit int) ([]TagCount, error) {
	since := FormatTime(time.Now().Add(-48 * time.Hour))
	rows, err := s.db.QueryContext(ctx, `
		SELECT lower(tg.value) AS tag, COUNT(*) AS n
		FROM works w, json_each(w.tags) tg
		WHERE w.deleted_at IS NULL AND w.is_blocked = 0 AND w.created_at > ?
		GROUP BY tag ORDER BY n DESC, tag LIMIT ?`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TagCount{}
	for rows.Next() {
		var t TagCount
		if err := rows.Scan(&t.Tag, &t.Count); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ── Saves ─────────────────────────────────────────────────────────────────────

// SavedWorks is the viewer's bookmarks, newest save first. Owner-only at the API.
func (s *Store) SavedWorks(ctx context.Context, userID string, limit int, cursor string) ([]*Work, string, error) {
	before, err := cursorOrNow(cursor)
	if err != nil {
		return nil, "", err
	}
	now := Now()
	rows, err := s.db.QueryContext(ctx, worksSelectSQL+`
		JOIN work_reactions bk ON bk.work_id = w.id AND bk.reactor_id = ? AND bk.reaction_type = 'bookmark'
		WHERE w.deleted_at IS NULL AND `+workNotBlockedSQL+` AND `+workLiveSQL+`
		  AND bk.created_at < ?
		ORDER BY bk.created_at DESC LIMIT ?`, userID, now, before, limit+1)
	if err != nil {
		return nil, "", err
	}
	works, err := collectWorks(rows)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(works) > limit {
		works = works[:limit]
		s.db.QueryRowContext(ctx, `SELECT created_at FROM work_reactions WHERE work_id = ? AND reactor_id = ? AND reaction_type = 'bookmark'`,
			works[len(works)-1].ID, userID).Scan(&next)
	}
	if works == nil {
		works = []*Work{}
	}
	return works, next, s.Hydrate(ctx, works, userID)
}

// ── Editing ───────────────────────────────────────────────────────────────────

// EditWindow is how long after posting a work's body may change.
const EditWindow = 60 * time.Minute

var ErrEditWindowClosed = errors.New("works can be edited for 60 minutes after posting")

// EditWork replaces the body of the caller's own recent work and records an
// edition, as feed-engine's editWork does.
func (s *Store) EditWork(ctx context.Context, workID, authorID, body string) error {
	body = strings.TrimSpace(body)
	if body == "" {
		return errors.New("a work needs words")
	}
	var owner, created string
	if err := s.db.QueryRowContext(ctx, `SELECT author_id, created_at FROM works WHERE id = ? AND deleted_at IS NULL`, workID).Scan(&owner, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if owner != authorID {
		return errors.New("forbidden")
	}
	if time.Since(ParseTime(created)) > EditWindow {
		return ErrEditWindowClosed
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		now := Now()
		if _, err := tx.ExecContext(ctx, `UPDATE works SET body = ?, is_edited = 1, edited_at = ? WHERE id = ?`, body, now, workID); err != nil {
			return err
		}
		var n int
		tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM editions WHERE work_id = ?`, workID).Scan(&n)
		_, err := tx.ExecContext(ctx, `INSERT INTO editions (id, work_id, cid, body, edition_number, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			NewID(), workID, "edit:"+NewID(), body, n+1, now)
		return err
	})
}

// FollowerIDs lists who follows an account, for fan-out.
func (s *Store) FollowerIDs(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT follower_id FROM follows WHERE following_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		out = append(out, id)
	}
	return out, rows.Err()
}
