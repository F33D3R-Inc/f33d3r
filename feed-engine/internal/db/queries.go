package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/f33d3r/feed-engine/internal/model"
)

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

func GetUserByID(database *sql.DB, id string) (*model.User, error) {
	return scanUser(database.QueryRow(userSelectSQL+"WHERE u.id = $1", id))
}

const userSelectSQL = `
	SELECT u.id, u.handle,
	       COALESCE(NULLIF(TRIM(p.display_name),''), CASE WHEN LENGTH(u.handle)>=40 THEN 'User '||LEFT(u.handle,6) ELSE INITCAP(REPLACE(u.handle,'_',' ')) END),
	       COALESCE(p.bio, ''),
	       COALESCE(p.pronouns, ''),
	       COALESCE(p.location, ''),
	       COALESCE(p.website, ''),
	       COALESCE(p.avatar_url, ''),
	       COALESCE(p.avatar_animated, FALSE),
	       COALESCE(p.header_url, ''),
	       COALESCE(p.theme_id, 'void'),
	       COALESCE(p.accent_hex, ''),
	       COALESCE(p.jung_archetype, ''),
	       COALESCE(p.pinned_track_id, ''),
	       COALESCE(p.social_links::text, '{}'),
	       COALESCE(p.is_creator, FALSE),
	       COALESCE(p.is_verified, FALSE),
	       COALESCE(p.follower_count, 0),
	       COALESCE(p.following_count, 0),
	       COALESCE(p.post_count, 0),
	       COALESCE(u.tier, 'free'),
	       COALESCE(p.content_setting, 'default'),
	       COALESCE(u.realm, 1),
	       COALESCE(u.xp, 0),
	       COALESCE(u.unread_count, 0),
	       COALESCE(u.role, 'user'),
	       COALESCE(u.pial_id::text, '')
	FROM users u
	LEFT JOIN user_profiles p ON p.user_id = u.id
`

func scanUser(row *sql.Row) (*model.User, error) {
	u := &model.User{}
	err := row.Scan(
		&u.ID, &u.Handle,
		&u.DisplayName, &u.Bio, &u.Pronouns,
		&u.Location, &u.Website,
		&u.AvatarURL, &u.AvatarAnimated, &u.HeaderURL,
		&u.ThemeID, &u.AccentHex, &u.JungArchetype,
		&u.PinnedTrackID, &u.SocialLinksRaw,
		&u.IsCreator, &u.IsVerified,
		&u.FollowerCount, &u.FollowingCount, &u.PostCount,
		&u.Tier, &u.ContentSetting,
		&u.Realm, &u.XP,
		&u.UnreadCount, &u.Role, &u.PIALID,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return u, err
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
	_, err := database.Exec(`
		INSERT INTO user_profiles
		    (user_id, display_name, bio, pronouns, location, website,
		     avatar_url, avatar_animated, header_url,
		     theme_id, accent_hex, jung_archetype,
		     pinned_track_id, social_links, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14::jsonb,NOW())
		ON CONFLICT (user_id) DO UPDATE SET
		    display_name    = EXCLUDED.display_name,
		    bio             = EXCLUDED.bio,
		    pronouns        = EXCLUDED.pronouns,
		    location        = EXCLUDED.location,
		    website         = EXCLUDED.website,
		    avatar_url      = CASE WHEN EXCLUDED.avatar_url = '' THEN user_profiles.avatar_url ELSE EXCLUDED.avatar_url END,
		    avatar_animated = EXCLUDED.avatar_animated,
		    header_url      = CASE WHEN EXCLUDED.header_url = '' THEN user_profiles.header_url ELSE EXCLUDED.header_url END,
		    theme_id        = EXCLUDED.theme_id,
		    accent_hex      = EXCLUDED.accent_hex,
		    jung_archetype  = EXCLUDED.jung_archetype,
		    pinned_track_id = EXCLUDED.pinned_track_id,
		    social_links    = EXCLUDED.social_links,
		    updated_at      = NOW()
	`,
		p.UserID, p.DisplayName, p.Bio, p.Pronouns,
		p.Location, p.Website,
		p.AvatarURL, p.AvatarAnimated, p.HeaderURL,
		p.ThemeID, p.AccentHex, p.JungArchetype,
		p.PinnedTrackID, socialJSON,
	)
	return err
}

// AwardXP adds xp_delta to a user's XP, updates their realm level if thresholds
// are crossed, and logs an immutable xp_events row. Safe to call fire-and-forget.
func AwardXP(database *sql.DB, userID, reason string, delta int) {
	if database == nil || userID == "" || delta == 0 {
		return
	}
	database.Exec(`
		WITH updated AS (
			UPDATE users
			SET xp = GREATEST(0, COALESCE(xp, 0) + $1),
			    realm = CASE
			        WHEN COALESCE(xp,0) + $1 >= 20000 THEN 5
			        WHEN COALESCE(xp,0) + $1 >= 7500  THEN 4
			        WHEN COALESCE(xp,0) + $1 >= 2000  THEN 3
			        WHEN COALESCE(xp,0) + $1 >= 500   THEN 2
			        ELSE 1
			    END
			WHERE id = $2
			RETURNING id
		)
		INSERT INTO xp_events (user_id, reason, xp_delta, created_at)
		SELECT $2, $3, $1, NOW()
		WHERE EXISTS (SELECT 1 FROM updated)`,
		delta, userID, reason)
}

// ── Posts ─────────────────────────────────────────────────────────────────────

func InsertPost(database *sql.DB, authorID, body, contentType string, tags []string) (string, error) {
	return insertPostInternal(database, authorID, "", false, body, contentType, tags, []string{}, "open")
}

func InsertPostWithMedia(database *sql.DB, authorID, body, contentType string, tags, mediaURLs []string, commentGating string) (string, error) {
	return insertPostInternal(database, authorID, "", false, body, contentType, tags, mediaURLs, commentGating)
}

// InsertPoll creates a post with poll options attached.
func InsertPoll(database *sql.DB, authorID, body string, options []string, endsAt *time.Time) (string, error) {
	var id string
	var endsAtParam interface{}
	if endsAt != nil {
		endsAtParam = *endsAt
	}
	err := database.QueryRow(`
		INSERT INTO posts (author_id, body, content_type, poll_options, poll_ends_at, comment_gating)
		VALUES ($1, $2, 'poll', $3, $4, 'open')
		RETURNING id
	`, authorID, body, pq.Array(options), endsAtParam).Scan(&id)
	if err != nil {
		return "", err
	}
	_, _ = database.Exec(`INSERT INTO post_metrics (post_id) VALUES ($1) ON CONFLICT DO NOTHING`, id)
	return id, nil
}

// CastPollVote records a user's vote. Returns (alreadyVoted, error).
func CastPollVote(database *sql.DB, postID, voterID string, optionIdx int) (bool, error) {
	_, err := database.Exec(`
		INSERT INTO poll_votes (post_id, voter_id, option_idx)
		VALUES ($1, $2, $3)
		ON CONFLICT (post_id, voter_id) DO NOTHING
	`, postID, voterID, optionIdx)
	if err != nil {
		return false, err
	}
	return false, nil
}

// GetPollResults returns vote counts per option for a post.
func GetPollResults(database *sql.DB, postID, voterID string) (*model.Poll, error) {
	// Get poll metadata
	var options pq.StringArray
	var endsAt *time.Time
	err := database.QueryRow(`SELECT poll_options, poll_ends_at FROM posts WHERE id = $1`, postID).
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
	if endsAt != nil && endsAt.Before(time.Now()) {
		poll.Ended = true
	}

	// Get vote counts per option
	rows, err := database.Query(`
		SELECT option_idx, COUNT(*) FROM poll_votes WHERE post_id = $1 GROUP BY option_idx
	`, postID)
	if err != nil {
		return poll, nil
	}
	defer rows.Close()
	for rows.Next() {
		var idx, count int
		if rows.Scan(&idx, &count) == nil && idx >= 0 && idx < len(poll.Votes) {
			poll.Votes[idx] = count
			poll.TotalVotes += count
		}
	}

	// Get user's vote
	if voterID != "" {
		var voted int
		err = database.QueryRow(`SELECT option_idx FROM poll_votes WHERE post_id = $1 AND voter_id = $2`,
			postID, voterID).Scan(&voted)
		if err == nil {
			poll.UserVote = voted
		}
	}

	// Precompute display results
	poll.Results = make([]model.PollResult, len(poll.Options))
	for i, opt := range poll.Options {
		pct := 0
		if poll.TotalVotes > 0 {
			pct = int(float64(poll.Votes[i]) / float64(poll.TotalVotes) * 100)
		}
		poll.Results[i] = model.PollResult{
			Label: opt,
			Votes: poll.Votes[i],
			Pct:   pct,
			Voted: poll.UserVote == i,
		}
	}
	return poll, nil
}

// SetPostVideo persists the HLS asset on a post AFTER Caeor finishes transcoding.
// Idempotent — safe to call from a background poller or Kafka consumer.
func SetPostVideo(database *sql.DB, postID, masterURL, posterURL string, dur float32, w, h int) error {
	if database == nil {
		return nil
	}
	_, err := database.Exec(`
		UPDATE posts
		   SET video_master_url    = $2,
		       video_poster_url    = $3,
		       video_duration_secs = $4,
		       video_width         = $5,
		       video_height        = $6,
		       content_type        = CASE WHEN content_type = 'text' THEN 'video' ELSE content_type END
		 WHERE id = $1
	`, postID, masterURL, posterURL, dur, w, h)
	return err
}

func InsertReply(database *sql.DB, authorID, parentID, body string) (string, error) {
	id, err := insertPostInternal(database, authorID, parentID, true, body, "text", []string{}, []string{}, "open")
	if err != nil {
		return "", err
	}
	// increment comment count on parent
	_, _ = database.Exec(`UPDATE post_metrics SET comments = comments + 1 WHERE post_id = $1`, parentID)
	return id, nil
}

func insertPostInternal(database *sql.DB, authorID, parentID string, isReply bool, body, contentType string, tags, mediaURLs []string, commentGating string) (string, error) {
	if tags == nil {
		tags = []string{}
	}
	if mediaURLs == nil {
		mediaURLs = []string{}
	}
	if commentGating == "" {
		commentGating = "open"
	}
	var parentParam interface{}
	if parentID != "" {
		parentParam = parentID
	}
	var id string
	err := database.QueryRow(`
		INSERT INTO posts (author_id, parent_id, is_reply, body, content_type, tags, media_urls, comment_gating)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id
	`, authorID, parentParam, isReply, body, contentType, pq.Array(tags), pq.Array(mediaURLs), commentGating).Scan(&id)
	if err != nil {
		return "", err
	}
	_, _ = database.Exec(`INSERT INTO post_metrics (post_id) VALUES ($1) ON CONFLICT DO NOTHING`, id)
	_, _ = database.Exec(`UPDATE user_profiles SET post_count = post_count + 1 WHERE user_id = $1`, authorID)
	return id, nil
}

func GetPostByID(database *sql.DB, postID string) (*model.Post, error) {
	rows, err := database.Query(postSelectSQL+`WHERE p.id = $1`, postID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	posts, err := scanPosts(rows)
	if err != nil || len(posts) == 0 {
		return nil, err
	}
	return posts[0], nil
}

func GetReplies(database *sql.DB, parentID string, limit int) ([]*model.Post, error) {
	rows, err := database.Query(postSelectSQL+`WHERE p.parent_id = $1 ORDER BY p.created_at ASC LIMIT $2`, parentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPosts(rows)
}

func GetRecentPosts(database *sql.DB, contentType string, limit int, afterID string) ([]*model.Post, error) {
	var afterParam interface{}
	if afterID != "" {
		afterParam = afterID
	}
	query := postSelectSQL + `
		WHERE p.is_reply = FALSE
		AND ($1::uuid IS NULL OR p.created_at < (SELECT created_at FROM posts WHERE id = $1::uuid))
		AND ($2 = '' OR p.content_type = $2)
		ORDER BY p.created_at DESC LIMIT $3`
	rows, err := database.Query(query, afterParam, contentType, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPosts(rows)
}

func GetRecentPostsForSurface(database *sql.DB, surface string, limit int, afterID string) ([]*model.Post, error) {
	contentType := ""
	if surface == "music" {
		contentType = "audio"
	}
	return GetRecentPosts(database, contentType, limit, afterID)
}

// GetFollowingFeed returns posts from users that viewerID follows.
func GetFollowingFeed(database *sql.DB, viewerID string, limit int, afterID string) ([]*model.Post, error) {
	var afterParam interface{}
	if afterID != "" {
		afterParam = afterID
	}
	rows, err := database.Query(postSelectSQL+`
		WHERE p.author_id IN (SELECT following_id FROM follows WHERE follower_id = $1)
		AND p.is_reply = FALSE
		AND ($2::uuid IS NULL OR p.created_at < (SELECT created_at FROM posts WHERE id = $2::uuid))
		ORDER BY p.created_at DESC LIMIT $3
	`, viewerID, afterParam, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPosts(rows)
}

// GetForYouFeed returns a blended "For You" candidate pool:
// 60% high-engagement posts from users the viewer follows (last 48h),
// 40% broadly trending posts (last 24h) for discovery.
func GetForYouFeed(database *sql.DB, viewerID string, limit int, afterID string) ([]*model.Post, error) {
	var afterParam interface{}
	if afterID != "" {
		afterParam = afterID
	}
	followLimit := (limit * 60) / 100
	trendLimit := limit - followLimit

	// Viewer's own posts + followed users — last 48h, ranked by engagement
	followRows, err := database.Query(postSelectSQL+`
		WHERE (p.author_id = $1 OR p.author_id IN (SELECT following_id FROM follows WHERE follower_id = $1))
		AND p.is_reply = FALSE
		AND p.created_at > NOW() - INTERVAL '48 hours'
		AND ($2::uuid IS NULL OR p.created_at < (SELECT created_at FROM posts WHERE id = $2::uuid))
		ORDER BY COALESCE(m.likes,0) + COALESCE(m.reposts,0)*2 + COALESCE(m.comments,0) DESC, p.created_at DESC
		LIMIT $3
	`, viewerID, afterParam, followLimit)
	if err != nil {
		return GetRecentPosts(database, "", limit, afterID)
	}
	defer followRows.Close()
	posts, _ := scanPosts(followRows)

	seen := map[string]bool{}
	for _, p := range posts { seen[p.ID] = true }

	// Trending posts from community — last 24h by engagement
	trendRows, err := database.Query(postSelectSQL+`
		WHERE p.is_reply = FALSE
		AND p.created_at > NOW() - INTERVAL '24 hours'
		AND p.author_id NOT IN (SELECT following_id FROM follows WHERE follower_id = $1)
		AND ($2::uuid IS NULL OR p.created_at < (SELECT created_at FROM posts WHERE id = $2::uuid))
		ORDER BY COALESCE(m.likes,0) + COALESCE(m.reposts,0)*2 + COALESCE(m.comments,0) DESC, p.created_at DESC
		LIMIT $3
	`, viewerID, afterParam, trendLimit)
	if err == nil {
		defer trendRows.Close()
		trending, _ := scanPosts(trendRows)
		for _, p := range trending {
			if !seen[p.ID] {
				posts = append(posts, p)
				seen[p.ID] = true
			}
		}
	}

	// Fill remainder from recent posts if we're short
	if len(posts) < limit/2 {
		recent, _ := GetRecentPosts(database, "", limit, afterID)
		for _, p := range recent {
			if !seen[p.ID] {
				posts = append(posts, p)
			}
		}
		if len(posts) > limit { posts = posts[:limit] }
	}

	return posts, nil
}

func GetUserPosts(database *sql.DB, userID string, limit int, afterID string) ([]*model.Post, error) {
	var afterParam interface{}
	if afterID != "" {
		afterParam = afterID
	}
	rows, err := database.Query(postSelectSQL+`
		WHERE p.author_id = $1 AND p.is_reply = FALSE
		AND ($2::uuid IS NULL OR p.created_at < (SELECT created_at FROM posts WHERE id = $2::uuid))
		ORDER BY p.created_at DESC LIMIT $3`, userID, afterParam, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPosts(rows)
}

func GetUserReplies(database *sql.DB, userID string, limit int, afterID string) ([]*model.Post, error) {
	var afterParam interface{}
	if afterID != "" {
		afterParam = afterID
	}
	rows, err := database.Query(postSelectSQL+`
		WHERE p.author_id = $1 AND p.is_reply = TRUE
		AND ($2::uuid IS NULL OR p.created_at < (SELECT created_at FROM posts WHERE id = $2::uuid))
		ORDER BY p.created_at DESC LIMIT $3`, userID, afterParam, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPosts(rows)
}

func GetUserMedia(database *sql.DB, userID string, limit int, afterID string) ([]*model.Post, error) {
	var afterParam interface{}
	if afterID != "" {
		afterParam = afterID
	}
	rows, err := database.Query(postSelectSQL+`
		WHERE p.author_id = $1 AND (p.content_type IN ('audio','image','video') OR array_length(p.media_urls,1) > 0)
		AND ($2::uuid IS NULL OR p.created_at < (SELECT created_at FROM posts WHERE id = $2::uuid))
		ORDER BY p.created_at DESC LIMIT $3`, userID, afterParam, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPosts(rows)
}

func SearchPosts(database *sql.DB, query string, limit int) ([]*model.Post, error) {
	rows, err := database.Query(postSelectSQL+`
		WHERE p.is_reply = FALSE
		AND to_tsvector('english', p.body) @@ plainto_tsquery('english', $1)
		ORDER BY ts_rank(to_tsvector('english', p.body), plainto_tsquery('english', $1)) DESC, p.created_at DESC
		LIMIT $2`, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPosts(rows)
}

const postSelectSQL = `
	SELECT
	    p.id, p.author_id, p.body, p.content_type, p.tags, p.media_urls,
	    p.is_edited, p.is_reply, p.created_at,
	    COALESCE(p.comment_gating, 'open'),
	    u.handle,
	    COALESCE(NULLIF(TRIM(pr.display_name),''), CASE WHEN LENGTH(u.handle)>=40 THEN 'User '||LEFT(u.handle,6) ELSE INITCAP(REPLACE(u.handle,'_',' ')) END),
	    COALESCE(pr.avatar_url, ''),
	    COALESCE(pr.avatar_animated, FALSE),
	    COALESCE(pr.is_verified, FALSE),
	    COALESCE(ur.role_type, 'user'),
	    COALESCE(m.likes, 0),
	    COALESCE(m.reposts, 0),
	    COALESCE(m.comments, 0),
	    COALESCE(m.saves, 0),
	    COALESCE(m.impressions, 0),
	    COALESCE(p.is_nsfw, FALSE),
	    COALESCE(p.video_master_url, ''),
	    COALESCE(p.video_poster_url, ''),
	    COALESCE(p.video_duration_secs, 0)::REAL,
	    COALESCE(p.video_width, 0),
	    COALESCE(p.video_height, 0),
	    (SELECT COUNT(*) FROM posts t WHERE t.parent_id = p.id AND t.author_id = p.author_id)::INT AS thread_count
	FROM posts p
	JOIN users u ON u.id = p.author_id
	LEFT JOIN user_profiles pr ON pr.user_id = p.author_id
	LEFT JOIN user_roles ur ON ur.user_id = p.author_id
	LEFT JOIN post_metrics m ON m.post_id = p.id
`

func scanPosts(rows *sql.Rows) ([]*model.Post, error) {
	var posts []*model.Post
	for rows.Next() {
		p := &model.Post{}
		var createdAt time.Time
		var tagsArr, mediaArr pq.StringArray
		if err := rows.Scan(
			&p.ID, &p.AuthorID, &p.Body, &p.ContentType, &tagsArr, &mediaArr,
			&p.IsEdited, &p.IsReply, &createdAt,
			&p.CommentGating,
			&p.AuthorHandle, &p.AuthorName,
			&p.AvatarURL, &p.AvatarAnimated, &p.IsVerified, &p.AuthorRole,
			&p.Likes, &p.Reposts, &p.Comments, &p.Saves, &p.Impressions,
			&p.IsNSFW,
			&p.VideoMasterURL, &p.VideoPosterURL,
			&p.VideoDurationSecs, &p.VideoWidth, &p.VideoHeight,
			&p.ThreadCount,
		); err != nil {
			return nil, err
		}
		p.Tags      = []string(tagsArr)
		p.MediaURLs = []string(mediaArr)
		p.CreatedAt = createdAt
		p.TimeAgo   = timeAgo(createdAt)
		p.ContentID = p.ID
		p.AvatarSeed = p.AuthorHandle
		posts = append(posts, p)
	}
	return posts, rows.Err()
}

// ── Follows ───────────────────────────────────────────────────────────────────

func FollowUser(database *sql.DB, followerID, followingID string) error {
	_, err := database.Exec(`
		INSERT INTO follows (follower_id, following_id) VALUES ($1, $2)
		ON CONFLICT DO NOTHING
	`, followerID, followingID)
	if err != nil {
		return err
	}
	_, _ = database.Exec(`UPDATE user_profiles SET following_count = following_count + 1 WHERE user_id = $1`, followerID)
	_, _ = database.Exec(`UPDATE user_profiles SET follower_count  = follower_count  + 1 WHERE user_id = $1`, followingID)
	return nil
}

func UnfollowUser(database *sql.DB, followerID, followingID string) error {
	res, err := database.Exec(`DELETE FROM follows WHERE follower_id = $1 AND following_id = $2`, followerID, followingID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		_, _ = database.Exec(`UPDATE user_profiles SET following_count = GREATEST(0, following_count - 1) WHERE user_id = $1`, followerID)
		_, _ = database.Exec(`UPDATE user_profiles SET follower_count  = GREATEST(0, follower_count  - 1) WHERE user_id = $1`, followingID)
	}
	return nil
}

// FollowListEntry is a minimal user row for the followers/following list.
type FollowListEntry struct {
	Handle      string
	DisplayName string
	AvatarURL   string
}

func GetFollowers(database *sql.DB, userID string, limit int) ([]FollowListEntry, error) {
	rows, err := database.Query(`
		SELECT u.handle, COALESCE(NULLIF(TRIM(p.display_name),''), CASE WHEN LENGTH(u.handle)>=40 THEN 'User '||LEFT(u.handle,6) ELSE INITCAP(REPLACE(u.handle,'_',' ')) END), COALESCE(p.avatar_url, '')
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
		rows.Scan(&e.Handle, &e.DisplayName, &e.AvatarURL)
		out = append(out, e)
	}
	return out, nil
}

func GetFollowing(database *sql.DB, userID string, limit int) ([]FollowListEntry, error) {
	rows, err := database.Query(`
		SELECT u.handle, COALESCE(NULLIF(TRIM(p.display_name),''), CASE WHEN LENGTH(u.handle)>=40 THEN 'User '||LEFT(u.handle,6) ELSE INITCAP(REPLACE(u.handle,'_',' ')) END), COALESCE(p.avatar_url, '')
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
		rows.Scan(&e.Handle, &e.DisplayName, &e.AvatarURL)
		out = append(out, e)
	}
	return out, nil
}

func IsFollowing(database *sql.DB, followerID, followingID string) (bool, error) {
	var exists bool
	err := database.QueryRow(`SELECT EXISTS(SELECT 1 FROM follows WHERE follower_id=$1 AND following_id=$2)`, followerID, followingID).Scan(&exists)
	return exists, err
}

// SuggestedUser is a minimal row for the "Who to follow" widget.
type SuggestedUser struct {
	ID            string
	Handle        string
	DisplayName   string
	AvatarURL     string
	FollowerCount int
}

// GetSuggestedUsers returns up to limit users the viewer is not already following.
func GetSuggestedUsers(database *sql.DB, viewerID string, limit int) ([]SuggestedUser, error) {
	rows, err := database.Query(`
		SELECT u.id,
		       u.handle,
		       COALESCE(NULLIF(TRIM(p.display_name),''), INITCAP(REPLACE(u.handle,'_',' '))),
		       COALESCE(p.avatar_url, ''),
		       COALESCE(p.follower_count, 0)
		FROM users u
		LEFT JOIN user_profiles p ON p.user_id = u.id
		WHERE u.id != $1
		  AND u.id NOT IN (SELECT following_id FROM follows WHERE follower_id = $1)
		ORDER BY COALESCE(p.follower_count, 0) DESC
		LIMIT $2
	`, viewerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SuggestedUser
	for rows.Next() {
		var s SuggestedUser
		rows.Scan(&s.ID, &s.Handle, &s.DisplayName, &s.AvatarURL, &s.FollowerCount)
		out = append(out, s)
	}
	return out, nil
}

// ── Threads ───────────────────────────────────────────────────────────────────

// GetThreadChain returns the full self-reply chain rooted at rootPostID.
// A thread is a series of posts where each reply's author_id == the root's author_id.
// Returns posts in chronological order (root first).
func GetThreadChain(database *sql.DB, rootPostID string, viewerID string) ([]*model.Post, error) {
	rows, err := database.Query(`
		WITH RECURSIVE chain AS (
			SELECT p.id, p.author_id, 0 AS depth
			FROM posts p
			WHERE p.id = $1
		UNION ALL
			SELECT p.id, p.author_id, c.depth + 1
			FROM posts p
			JOIN chain c ON p.parent_id = c.id AND p.author_id = c.author_id
			WHERE c.depth < 50
		)
		`+postSelectSQL+`
		JOIN chain ch ON ch.id = p.id
		ORDER BY ch.depth ASC
	`, rootPostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	posts, err := scanPosts(rows)
	if err != nil {
		return nil, err
	}
	// Annotate viewer like/bookmark state if viewer is known
	if viewerID != "" {
		for _, p := range posts {
			p.LikedByUser, _      = IsLiked(database, viewerID, p.ID)
			p.BookmarkedByUser, _ = IsBookmarked(database, viewerID, p.ID)
		}
	}
	return posts, nil
}

// ── Mention helpers ───────────────────────────────────────────────────────────

// ResolveMentionHandles takes a slice of lowercase handles and returns a map
// of handle → (userID, displayName). Missing handles are simply absent from the map.
func ResolveMentionHandles(database *sql.DB, handles []string) (map[string][2]string, error) {
	if len(handles) == 0 {
		return nil, nil
	}
	rows, err := database.Query(`
		SELECT u.handle,
		       u.id,
		       COALESCE(NULLIF(TRIM(p.display_name),''), INITCAP(REPLACE(u.handle,'_',' ')))
		FROM users u
		LEFT JOIN user_profiles p ON p.user_id = u.id
		WHERE LOWER(u.handle) = ANY($1)
	`, pq.Array(handles))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string][2]string)
	for rows.Next() {
		var handle, uid, name string
		if err := rows.Scan(&handle, &uid, &name); err == nil {
			out[strings.ToLower(handle)] = [2]string{uid, name}
		}
	}
	return out, nil
}

// ── Bookmarks ─────────────────────────────────────────────────────────────────

func AddBookmark(database *sql.DB, userID, postID string) error {
	_, err := database.Exec(`INSERT INTO bookmarks (user_id, post_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, userID, postID)
	if err == nil {
		_, _ = database.Exec(`UPDATE post_metrics SET saves = saves + 1 WHERE post_id = $1`, postID)
	}
	return err
}

func RemoveBookmark(database *sql.DB, userID, postID string) error {
	res, err := database.Exec(`DELETE FROM bookmarks WHERE user_id=$1 AND post_id=$2`, userID, postID)
	if err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			_, _ = database.Exec(`UPDATE post_metrics SET saves = GREATEST(0, saves - 1) WHERE post_id = $1`, postID)
		}
	}
	return err
}

func IsBookmarked(database *sql.DB, userID, postID string) (bool, error) {
	var exists bool
	err := database.QueryRow(`SELECT EXISTS(SELECT 1 FROM bookmarks WHERE user_id=$1 AND post_id=$2)`, userID, postID).Scan(&exists)
	return exists, err
}

func GetBookmarks(database *sql.DB, userID string, limit int, afterID string) ([]*model.Post, error) {
	var afterParam interface{}
	if afterID != "" {
		afterParam = afterID
	}
	rows, err := database.Query(`
		SELECT
		    p.id, p.author_id, p.body, p.content_type, p.tags, p.media_urls,
		    p.is_edited, p.is_reply, p.created_at,
		    COALESCE(p.comment_gating, 'open'),
		    u.handle,
		    COALESCE(NULLIF(TRIM(pr.display_name), ''), INITCAP(REPLACE(u.handle, '_', ' '))),
		    COALESCE(pr.avatar_url, ''),
		    COALESCE(pr.avatar_animated, FALSE),
		    COALESCE(pr.is_verified, FALSE),
		    COALESCE(ur.role_type, 'user'),
		    COALESCE(m.likes, 0),
		    COALESCE(m.reposts, 0),
		    COALESCE(m.comments, 0),
		    COALESCE(m.saves, 0),
		    COALESCE(m.impressions, 0)
		FROM bookmarks b
		JOIN posts p ON p.id = b.post_id
		JOIN users u ON u.id = p.author_id
		LEFT JOIN user_profiles pr ON pr.user_id = p.author_id
		LEFT JOIN user_roles ur ON ur.user_id = p.author_id
		LEFT JOIN post_metrics m ON m.post_id = p.id
		WHERE b.user_id = $1
		AND ($2::uuid IS NULL OR b.created_at < (SELECT created_at FROM bookmarks WHERE user_id=$1 AND post_id=$2::uuid))
		ORDER BY b.created_at DESC LIMIT $3
	`, userID, afterParam, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPosts(rows)
}

// ── Likes ─────────────────────────────────────────────────────────────────────

func ToggleLike(database *sql.DB, userID, postID string) (liked bool, err error) {
	var exists bool
	_ = database.QueryRow(`SELECT EXISTS(SELECT 1 FROM post_likes WHERE user_id=$1 AND post_id=$2)`, userID, postID).Scan(&exists)
	if exists {
		_, err = database.Exec(`DELETE FROM post_likes WHERE user_id=$1 AND post_id=$2`, userID, postID)
		_, _ = database.Exec(`UPDATE post_metrics SET likes = GREATEST(0, likes-1) WHERE post_id=$1`, postID)
		return false, err
	}
	_, err = database.Exec(`INSERT INTO post_likes (user_id, post_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, userID, postID)
	_, _ = database.Exec(`UPDATE post_metrics SET likes = likes+1 WHERE post_id=$1`, postID)
	return true, err
}

func IsLiked(database *sql.DB, userID, postID string) (bool, error) {
	var exists bool
	err := database.QueryRow(`SELECT EXISTS(SELECT 1 FROM post_likes WHERE user_id=$1 AND post_id=$2)`, userID, postID).Scan(&exists)
	return exists, err
}

// ── Legacy metric helpers (kept for non-per-user paths) ───────────────────────

func IncrementLike(database *sql.DB, postID string) error {
	_, err := database.Exec(`UPDATE post_metrics SET likes = likes + 1, updated_at = NOW() WHERE post_id = $1`, postID)
	return err
}

func IncrementRepost(database *sql.DB, postID string) error {
	_, err := database.Exec(`UPDATE post_metrics SET reposts = reposts + 1, updated_at = NOW() WHERE post_id = $1`, postID)
	return err
}

func IncrementSave(database *sql.DB, postID string) error {
	_, err := database.Exec(`UPDATE post_metrics SET saves = saves + 1, updated_at = NOW() WHERE post_id = $1`, postID)
	return err
}

func IncrementImpression(database *sql.DB, postID string) error {
	_, err := database.Exec(`UPDATE post_metrics SET impressions = impressions + 1, updated_at = NOW() WHERE post_id = $1`, postID)
	return err
}

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
		       COALESCE(p.is_verified, FALSE),
		       t.title, t.description, t.audio_url, t.cover_url,
		       t.duration_secs, t.genre, t.tags, t.price_cents, t.is_free,
		       t.play_count, t.like_count, t.created_at
		FROM tracks t
		JOIN users u ON u.id = t.author_id
		LEFT JOIN user_profiles p ON p.user_id = t.author_id
		WHERE ($1 = '' OR t.genre = $1)
		AND ($2::uuid IS NULL OR t.created_at < (SELECT created_at FROM tracks WHERE id=$2::uuid))
		ORDER BY t.created_at DESC LIMIT $3
	`
	var afterParam interface{}
	if afterID != "" { afterParam = afterID }
	rows, err := database.Query(query, genre, afterParam, limit)
	if err != nil { return nil, err }
	defer rows.Close()
	return scanTracks(rows)
}

func GetTracksByAuthor(database *sql.DB, authorID string, limit int) ([]*model.Track, error) {
	rows, err := database.Query(`
		SELECT t.id, t.author_id, u.handle,
		       COALESCE(NULLIF(TRIM(p.display_name),''), CASE WHEN LENGTH(u.handle)>=40 THEN 'User '||LEFT(u.handle,6) ELSE INITCAP(REPLACE(u.handle,'_',' ')) END),
		       COALESCE(p.avatar_url, ''),
		       COALESCE(p.is_verified, FALSE),
		       t.title, t.description, t.audio_url, t.cover_url,
		       t.duration_secs, t.genre, t.tags, t.price_cents, t.is_free,
		       t.play_count, t.like_count, t.created_at
		FROM tracks t
		JOIN users u ON u.id = t.author_id
		LEFT JOIN user_profiles p ON p.user_id = t.author_id
		WHERE t.author_id = $1
		ORDER BY t.created_at DESC LIMIT $2
	`, authorID, limit)
	if err != nil { return nil, err }
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
	if err != nil { return false, err }
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
		if err != nil { return nil, err }
		t.Tags = []string(tags)
		tracks = append(tracks, t)
	}
	return tracks, rows.Err()
}

// ── Post management ───────────────────────────────────────────────────────────

// DeletePost removes a post only if the caller is the author. Returns false if not found/not owner.
func DeletePost(database *sql.DB, postID, authorID string) (bool, error) {
	res, err := database.Exec(`DELETE FROM posts WHERE id=$1 AND author_id=$2`, postID, authorID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

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
		if v > 1.0 { v = 1.0 }
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

func GetTrendingTags(database *sql.DB, limit int) ([]TrendingTag, error) {
	rows, err := database.Query(`
		SELECT tag, COUNT(*) AS cnt
		FROM posts, unnest(tags) AS tag
		WHERE created_at > NOW() - INTERVAL '7 days'
		  AND tag <> ''
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
}

func SearchHandles(database *sql.DB, prefix string, limit int) ([]MentionResult, error) {
	rows, err := database.Query(`
		SELECT u.handle, COALESCE(NULLIF(TRIM(pr.display_name),''), CASE WHEN LENGTH(u.handle)>=40 THEN 'User '||LEFT(u.handle,6) ELSE INITCAP(REPLACE(u.handle,'_',' ')) END), COALESCE(pr.avatar_url, '')
		FROM users u
		LEFT JOIN user_profiles pr ON pr.user_id = u.id
		WHERE u.handle ILIKE $1 OR COALESCE(pr.display_name,'') ILIKE $2
		ORDER BY u.handle
		LIMIT $3
	`, prefix+"%", prefix+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MentionResult
	for rows.Next() {
		var m MentionResult
		if err := rows.Scan(&m.Handle, &m.DisplayName, &m.AvatarURL); err != nil {
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
	if err != nil { return nil, err }
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

// ── Password reset ────────────────────────────────────────────────────────────

func SetUserEmail(database *sql.DB, userID, email string) error {
	_, err := database.Exec(`UPDATE users SET email = $1 WHERE id = $2`, email, userID)
	return err
}

func GetUserByEmail(database *sql.DB, email string) (*model.User, error) {
	if email == "" {
		return nil, sql.ErrNoRows
	}
	return scanUser(database.QueryRow(userSelectSQL+"WHERE u.email = $1 AND u.email != ''", email))
}

func CreatePasswordResetToken(database *sql.DB, userID string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.URLEncoding.EncodeToString(b)
	_, err := database.Exec(`
		INSERT INTO password_reset_tokens (user_id, token, expires_at)
		VALUES ($1, $2, NOW() + INTERVAL '1 hour')
	`, userID, token)
	return token, err
}

func GetUserByResetToken(database *sql.DB, token string) (*model.User, error) {
	var userID string
	err := database.QueryRow(`
		SELECT user_id FROM password_reset_tokens
		WHERE token = $1 AND used_at IS NULL AND expires_at > NOW()
	`, token).Scan(&userID)
	if err != nil {
		return nil, err
	}
	return GetUserByID(database, userID)
}

func MarkResetTokenUsed(database *sql.DB, token string) error {
	_, err := database.Exec(`UPDATE password_reset_tokens SET used_at = NOW() WHERE token = $1`, token)
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
		       COALESCE(p.is_verified, FALSE),
		       COALESCE(p.is_creator, FALSE),
		       u.created_at
		FROM users u
		LEFT JOIN user_profiles p ON p.user_id = u.id
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
