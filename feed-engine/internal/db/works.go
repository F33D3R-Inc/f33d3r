package db

import (
	"database/sql"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/f33d3r/feed-engine/internal/model"
)

var workBodyURLRe = regexp.MustCompile(`https?://[^\s"'<>\x00-\x1f]+`)

// cashtagBodyRe matches the first $TICKER in a body (same pattern the renderer/compose
// use). Extracted at render time so a posted work can lazy-load the cashtag card facet.
var cashtagBodyRe = regexp.MustCompile(`\$([A-Z]{1,5})\b`)

func extractCashtagTicker(body string) string {
	if m := cashtagBodyRe.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}

// bareBodyURLRe matches bare domain URLs without a protocol prefix (e.g. "apple.com" or "apple.com/path").
// Applied only when workBodyURLRe finds nothing.
var bareBodyURLRe = regexp.MustCompile(`(?:^|[\s\n])([a-zA-Z0-9][a-zA-Z0-9.\-]*\.[a-zA-Z]{2,}(?:/[^\s"'<>]*)?)`)

func extractFirstBodyURL(body string) string {
	if m := workBodyURLRe.FindString(body); m != "" {
		return strings.TrimRight(m, ".,;:!?)'\"")
	}
	// Bare domain fallback — normalise to https:// so the link_previews cache key matches.
	if m := bareBodyURLRe.FindStringSubmatch(body); m != nil {
		candidate := strings.TrimRight(m[1], ".,;:!?)'\"")
		if !strings.HasPrefix(candidate, ".") && strings.Contains(candidate, ".") {
			return "https://" + candidate
		}
	}
	return ""
}

// InsertWork inserts a new content-addressed work and any citations.
// parentCID / quotedCID are empty strings when not applicable.
// The extended fields (tags, pollOptions, pollEndsAt, scheduledAt,
// commentGating, subscriberOnly, isRepost, repostSourceID, voiceURL,
// voiceDurationSecs, videoMasterURL, videoPosterURL, videoDurationSecs)
// map directly onto the columns added by the works schema extension.
// Returns the new work UUID.
func InsertWork(
	db *sql.DB,
	authorID, authorPIAL, cid, body, kind string,
	mediaURLs []string,
	parentCID, quotedCID string,
	tags []string,
	pollOptions []string,
	pollEndsAt *time.Time,
	scheduledAt *time.Time,
	commentGating string,
	subscriberOnly bool,
	isNSFW bool,
	isRepost bool,
	repostSourceID string,
	voiceURL string,
	voiceDurationSecs *int,
	videoMasterURL, videoWatermarkedURL, videoPosterURL string,
	videoDurationSecs *int,
	videoWidth, videoHeight int,
	reactLayout string,
) (string, error) {
	// Normalise optional UUID: empty string → NULL.
	var repostSourceIDParam interface{}
	if repostSourceID != "" {
		repostSourceIDParam = repostSourceID
	}
	// Normalise optional string → NULL.
	var voiceURLParam interface{}
	if voiceURL != "" {
		voiceURLParam = voiceURL
	}
	var videoMasterURLParam interface{}
	if videoMasterURL != "" {
		videoMasterURLParam = videoMasterURL
	}
	var videoWatermarkedURLParam interface{}
	if videoWatermarkedURL != "" {
		videoWatermarkedURLParam = videoWatermarkedURL
	}
	var videoPosterURLParam interface{}
	if videoPosterURL != "" {
		videoPosterURLParam = videoPosterURL
	}
	var reactLayoutParam interface{}
	if reactLayout != "" {
		reactLayoutParam = reactLayout
	}
	if commentGating == "" {
		commentGating = "open"
	}

	var workID string
	err := db.QueryRow(`
		INSERT INTO works (
			cid, author_id, author_pial, body, kind, media_urls,
			tags, poll_options, poll_ends_at, scheduled_at,
			comment_gating, subscriber_only, is_nsfw, is_repost, repost_source_id,
			voice_url, voice_duration_secs,
			video_master_url, video_watermarked_url, video_poster_url, video_duration_secs,
			video_width, video_height, react_layout,
			scan_state, expires_at
		) VALUES (
			$1, $2::uuid, $3::uuid, $4, $5, $6,
			$7, $8, $9, $10,
			$11, $12, $13, $14, $15::uuid,
			$16, $17,
			$18, $19, $20, $21,
			$22, $23, $24,
			'clean',
			CASE WHEN $5 = 'vision' THEN NOW() + INTERVAL '24 hours' ELSE NULL END
		)
		RETURNING id
	`,
		cid, authorID, authorPIAL, body, kind, pq.Array(mediaURLs),
		pq.Array(tags), pq.Array(pollOptions), pollEndsAt, scheduledAt,
		commentGating, subscriberOnly, isNSFW, isRepost, repostSourceIDParam,
		voiceURLParam, voiceDurationSecs,
		videoMasterURLParam, videoWatermarkedURLParam, videoPosterURLParam, videoDurationSecs,
		videoWidth, videoHeight, reactLayoutParam,
	).Scan(&workID)
	if err != nil {
		return "", err
	}

	if parentCID != "" {
		var targetID string
		err := db.QueryRow(`SELECT id FROM works WHERE cid = $1`, parentCID).Scan(&targetID)
		if err != nil {
			if err != sql.ErrNoRows {
				log.Printf("works: resolving parentCID %q: %v", parentCID, err)
			}
			// citation skipped — parent not found
		} else {
			_, err = db.Exec(`
				INSERT INTO work_citations (work_id, target_id, citation_type)
				VALUES ($1::uuid, $2::uuid, 'reply')
			`, workID, targetID)
			if err != nil {
				log.Printf("works: inserting reply citation for work %s: %v", workID, err)
			} else {
				db.Exec(`UPDATE works SET reply_count = reply_count + 1 WHERE id = $1::uuid`, targetID)
			}
		}
	}

	if quotedCID != "" {
		var targetID string
		err := db.QueryRow(`SELECT id FROM works WHERE cid = $1`, quotedCID).Scan(&targetID)
		if err != nil {
			if err != sql.ErrNoRows {
				log.Printf("works: resolving quotedCID %q: %v", quotedCID, err)
			}
			// citation skipped — quoted work not found
		} else {
			_, err = db.Exec(`
				INSERT INTO work_citations (work_id, target_id, citation_type)
				VALUES ($1::uuid, $2::uuid, 'quote')
			`, workID, targetID)
			if err != nil {
				log.Printf("works: inserting quote citation for work %s: %v", workID, err)
			}
		}
	}

	return workID, nil
}

// InsertEdition inserts an edition record. Call immediately after InsertWork with editionNumber=1.
func InsertEdition(db *sql.DB, workID, cid, body string, editionNumber int) error {
	_, err := db.Exec(`
		INSERT INTO editions (work_id, cid, body, edition_number)
		VALUES ($1::uuid, $2, $3, $4)
	`, workID, cid, body, editionNumber)
	return err
}

// GetWorkEditions returns all editions of a work ordered oldest-first.
func GetWorkEditions(db *sql.DB, workID string) ([]model.Edition, error) {
	rows, err := db.Query(`
		SELECT id, work_id::text, cid, body, edition_number, created_at
		FROM editions
		WHERE work_id = $1::uuid
		ORDER BY edition_number ASC`, workID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var editions []model.Edition
	for rows.Next() {
		var e model.Edition
		if err := rows.Scan(&e.ID, &e.WorkID, &e.CID, &e.Body, &e.EditionNumber, &e.CreatedAt); err != nil {
			continue
		}
		editions = append(editions, e)
	}
	return editions, nil
}

// worksSelectSQL is the shared SELECT for all work feed queries.
// Caller appends WHERE, ORDER BY, LIMIT.
//
// Column order must exactly match the Scan call in scanWork — any addition here
// requires a matching addition there, and vice versa.
const worksSelectSQL = `
SELECT
    w.id, w.cid, w.author_id, w.author_pial::text, w.body, w.kind,
    w.media_urls, w.is_nsfw, w.is_gore, w.is_blocked, w.expires_at, w.created_at,
    u.handle, COALESCE(up.display_name,''), COALESCE(up.avatar_url,''),
    COALESCE(plr.age_verified, FALSE), COALESCE(u.role,'user'), COALESCE(u.realm, 1),
    COALESCE(up.official_type,''), COALESCE(up.is_creator, FALSE),
    (SELECT COUNT(*) FROM work_reactions wr WHERE wr.work_id = w.id AND wr.reaction_type='like')::int,
    (SELECT COUNT(*) FROM work_reactions wr WHERE wr.work_id = w.id AND wr.reaction_type='repost')::int,
    (SELECT COUNT(*) FROM work_reactions wr WHERE wr.work_id = w.id AND wr.reaction_type='bookmark')::int,
    (SELECT COUNT(*) FROM work_reactions wr WHERE wr.work_id = w.id AND wr.reaction_type='dislike')::int,
    COALESCE(w.is_repost, FALSE),
    COALESCE(w.repost_source_id::text, ''),
    COALESCE(w.content_type, 'text'),
    COALESCE(w.tags, '{}'),
    COALESCE(w.comment_gating, 'open'),
    COALESCE(w.subscriber_only, FALSE),
    COALESCE(w.scan_state, 'pending'),
    COALESCE(w.score_band, 'steady'),
    COALESCE(w.legacy_post_id::text, ''),
    w.voice_url,
    w.voice_duration_secs,
    w.video_master_url,
    w.video_watermarked_url,
    w.video_poster_url,
    w.video_duration_secs,
    w.video_width,
    w.video_height,
    COALESCE(w.react_layout, ''),
    COALESCE(w.lineage_pial::text, ''),
    COALESCE(w.lineage_handle, ''),
    w.poll_options,
    w.poll_ends_at,
    COALESCE(w.reply_count, 0),
    COALESCE(w.quote_count, 0),
    COALESCE(w.is_edited, FALSE),
    w.edited_at,
    COALESCE(w.view_count, 0),
    COALESCE(up.is_adult_creator, FALSE)
FROM works w
JOIN users u ON u.id = w.author_id
LEFT JOIN user_profiles up ON up.user_id = w.author_id
LEFT JOIN pial_roots plr ON plr.pial_id = u.pial_id
`

// scanWork scans one row from worksSelectSQL into a model.Work.
// Column count and order must stay in sync with worksSelectSQL above.
func scanWork(rows *sql.Rows) (*model.Work, error) {
	var w model.Work
	var mediaURLs pq.StringArray
	var tags pq.StringArray
	var pollOptions pq.StringArray
	var expiresAt sql.NullTime
	var pollEndsAt sql.NullTime
	var createdAt time.Time
	var voiceURL sql.NullString
	var voiceDuration sql.NullFloat64
	var videoMasterURL      sql.NullString
	var videoWatermarkedURL sql.NullString
	var videoPosterURL      sql.NullString
	var videoDuration       sql.NullFloat64
	var videoWidth          sql.NullInt64
	var videoHeight         sql.NullInt64
	var editedAt            sql.NullTime

	err := rows.Scan(
		// core
		&w.ID, &w.CID, &w.AuthorID, &w.AuthorPIAL, &w.Body, &w.Kind,
		&mediaURLs, &w.IsNSFW, &w.IsGore, &w.IsBlocked, &expiresAt, &createdAt,
		// author display
		&w.AuthorHandle, &w.AuthorName, &w.AvatarURL,
		&w.IsVerified, &w.AuthorRole, &w.AuthorRealm,
		&w.AuthorOfficialType, &w.AuthorIsCreator,
		// reaction counts
		&w.LikeCount, &w.RepostCount, &w.BookmarkCount, &w.DislikeCount,
		// extended fields
		&w.IsRepost, &w.RepostSourceID, &w.ContentType, &tags,
		&w.CommentGating, &w.SubscriberOnly, &w.ScanState, &w.ScoreBand, &w.LegacyPostID,
		// voice
		&voiceURL, &voiceDuration,
		// video
		&videoMasterURL, &videoWatermarkedURL, &videoPosterURL, &videoDuration, &videoWidth, &videoHeight,
		// react-with-video layout
		&w.ReactLayout,
		// lineage
		&w.LineagePIAL, &w.LineageHandle,
		// poll
		&pollOptions, &pollEndsAt,
		// counters
		&w.ReplyCount, &w.QuoteCount,
		// edit state
		&w.IsEdited, &editedAt,
		// view count
		&w.ViewCount,
		// author adult-creator flag (feed filtering)
		&w.AuthorIsAdultCreator,
	)
	if err != nil {
		return nil, err
	}

	w.MediaURLs = []string(mediaURLs)
	w.Tags = []string(tags)
	w.CreatedAt = createdAt
	w.TimeAgo = timeAgo(createdAt)

	if expiresAt.Valid {
		t := expiresAt.Time
		w.ExpiresAt = &t
	}
	if pollEndsAt.Valid {
		t := pollEndsAt.Time
		w.PollEndsAt = &t
	}
	if len(pollOptions) > 0 {
		w.PollOptions = []string(pollOptions)
	}
	if voiceURL.Valid {
		w.VoiceURL = voiceURL.String
	}
	if voiceDuration.Valid {
		w.VoiceDurationSecs = float32(voiceDuration.Float64)
	}
	if videoMasterURL.Valid {
		w.VideoMasterURL = videoMasterURL.String
	}
	if videoWatermarkedURL.Valid {
		w.VideoWatermarkedURL = videoWatermarkedURL.String
	}
	if videoPosterURL.Valid {
		w.VideoPosterURL = videoPosterURL.String
	}
	if videoDuration.Valid {
		w.VideoDurationSecs = float32(videoDuration.Float64)
	}
	if videoWidth.Valid {
		w.VideoWidth = int(videoWidth.Int64)
	}
	if videoHeight.Valid {
		w.VideoHeight = int(videoHeight.Int64)
	}

	if editedAt.Valid {
		t := editedAt.Time
		w.EditedAt = &t
	}

	w.YouTubeID = extractYouTubeID(w.Body)
	w.YouTubePlaylistID = extractYouTubePlaylistID(w.Body)
	if t := extractCashtagTicker(w.Body); t != "" {
		w.Cashtag = &model.CashtagEmbed{Ticker: t}
	}

	return &w, nil
}

// parseBefore converts an RFC3339 cursor string to a time.Time.
// Empty string returns time.Now().
func parseBefore(before string) (time.Time, error) {
	if before == "" {
		return time.Now(), nil
	}
	return time.Parse(time.RFC3339, before)
}

// GetWorksFeedFollowing returns works from accounts the viewer follows, cursor-paginated by created_at.
// before is RFC3339; empty string = start from now.
func GetWorksFeedFollowing(db *sql.DB, viewerID string, limit int, before string) ([]*model.Work, error) {
	cursor, err := parseBefore(before)
	if err != nil {
		return nil, err
	}

	// Originals from you + people you follow, UNION reposts (work_reactions) by
	// you + people you follow. Each work appears once at its newest surfacing,
	// ordered by the time it hit the feed (repost time for reposts) so a fresh
	// repost rises to the top — reposts are first-class feed items.
	const idQuery = `
		SELECT id, reposter_handle, reposter_name, feed_ts FROM (
		  SELECT DISTINCT ON (id) id, reposter_handle, reposter_name, feed_ts FROM (
		    SELECT w.id AS id, NULL::text AS reposter_handle, NULL::text AS reposter_name, w.created_at AS feed_ts
		    FROM works w
		    WHERE (w.author_id = $1::uuid OR w.author_id IN (SELECT following_id FROM follows WHERE follower_id = $1::uuid))
		      AND w.deleted_at IS NULL AND NOT w.is_blocked AND w.kind <> 'reply'
		      AND (w.kind <> 'vision' OR w.expires_at IS NULL OR w.expires_at > NOW())
		      AND (NOT w.subscriber_only OR w.author_id = $1::uuid
		           OR EXISTS (SELECT 1 FROM subscriptions WHERE subscriber_id = $1::uuid AND creator_id = w.author_id AND status = 'active'))
		    UNION ALL
		    SELECT wr.work_id AS id, ru.handle AS reposter_handle,
		           COALESCE(NULLIF(TRIM(rp.display_name),''), ru.handle) AS reposter_name, wr.created_at AS feed_ts
		    FROM work_reactions wr
		    JOIN works w ON w.id = wr.work_id
		    JOIN users ru ON ru.id = wr.reactor_id
		    LEFT JOIN user_profiles rp ON rp.user_id = wr.reactor_id
		    WHERE wr.reaction_type = 'repost'
		      AND (wr.reactor_id = $1::uuid OR wr.reactor_id IN (SELECT following_id FROM follows WHERE follower_id = $1::uuid))
		      AND w.deleted_at IS NULL AND NOT w.is_blocked AND w.kind <> 'reply'
		      AND (NOT w.subscriber_only OR w.author_id = $1::uuid
		           OR EXISTS (SELECT 1 FROM subscriptions WHERE subscriber_id = $1::uuid AND creator_id = w.author_id AND status = 'active'))
		  ) feed
		  WHERE feed_ts < $2
		  ORDER BY id, feed_ts DESC
		) d
		ORDER BY feed_ts DESC
		LIMIT $3`
	return collectFeedWithReposts(db, idQuery, viewerID, cursor, limit)
}

// GetWorksForProfile returns works authored by the given PIAL, newest first.
func GetWorksForProfile(db *sql.DB, authorPIAL string, limit int, before string) ([]*model.Work, error) {
	cursor, err := parseBefore(before)
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(worksSelectSQL+`
		WHERE w.author_pial = $1::uuid
		AND w.deleted_at IS NULL
		AND NOT w.is_blocked
		AND w.kind != 'reply'
		AND (w.kind != 'vision' OR w.expires_at IS NULL OR w.expires_at > NOW())
		AND w.created_at < $2
		ORDER BY w.created_at DESC
		LIMIT $3
	`, authorPIAL, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return collectWorks(rows)
}

// SoftDeleteWork marks a work as deleted (sets deleted_at = NOW()).
// It verifies that the requesting user is the author (authorID = users.id) before deleting.
// Returns sql.ErrNoRows when the work does not exist or has already been deleted;
// returns a sentinel error when the user is not the owner.
// Also purges video_raw_hashes and image_raw_hashes so the same media can be re-uploaded.
func SoftDeleteWork(db *sql.DB, workID, authorID string) error {
	result, err := db.Exec(`
		UPDATE works
		SET deleted_at = NOW()
		WHERE id = $1::uuid
		  AND author_id = $2::uuid
		  AND deleted_at IS NULL
	`, workID, authorID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	// Purge hashes so the media can be freely re-uploaded by anyone.
	db.Exec(`DELETE FROM video_raw_hashes WHERE post_id = $1`, workID)
	db.Exec(`DELETE FROM image_raw_hashes WHERE media_url IN (
		SELECT unnest(media_urls) FROM works WHERE id = $1::uuid
	)`, workID)
	return nil
}

// AdminDeleteWork hard-deletes a work and its associated hashes without author ownership check.
// Used by admins to remove content regardless of who posted it.
func AdminDeleteWork(db *sql.DB, workID string) error {
	// Delete child rows first.
	for _, q := range []string{
		`DELETE FROM work_reactions  WHERE work_id = $1::uuid`,
		`DELETE FROM work_citations  WHERE work_id = $1::uuid`,
		`DELETE FROM editions        WHERE work_id = $1::uuid`,
		`DELETE FROM work_scores     WHERE work_id = $1::uuid`,
		`DELETE FROM video_raw_hashes WHERE post_id = $1`,
	} {
		db.Exec(q, workID)
	}
	// Clear pinned reference.
	db.Exec(`UPDATE user_profiles SET pinned_work_id = NULL WHERE pinned_work_id = $1::uuid`, workID)
	// Purge image hashes linked to this work's media URLs.
	db.Exec(`DELETE FROM image_raw_hashes WHERE media_url IN (
		SELECT unnest(media_urls) FROM works WHERE id = $1::uuid
	)`, workID)
	// Hard delete.
	result, err := db.Exec(`DELETE FROM works WHERE id = $1::uuid`, workID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// PurgeUser hard-deletes a user account and all associated data. Irreversible.
func PurgeUser(db *sql.DB, userID, pialID string) error {
	// Child rows for all the user's works.
	db.Exec(`DELETE FROM work_reactions WHERE work_id IN (SELECT id FROM works WHERE author_id = $1::uuid)`, userID)
	db.Exec(`DELETE FROM work_citations  WHERE work_id IN (SELECT id FROM works WHERE author_id = $1::uuid)`, userID)
	db.Exec(`DELETE FROM editions        WHERE work_id IN (SELECT id FROM works WHERE author_id = $1::uuid)`, userID)
	db.Exec(`DELETE FROM work_scores     WHERE work_id IN (SELECT id FROM works WHERE author_id = $1::uuid)`, userID)
	// Hashes.
	if pialID != "" {
		db.Exec(`DELETE FROM video_raw_hashes WHERE uploader_pial = $1`, pialID)
		db.Exec(`DELETE FROM image_raw_hashes WHERE uploader_pial = $1`, pialID)
	}
	// Clear pinned work references.
	db.Exec(`UPDATE user_profiles SET pinned_work_id = NULL WHERE user_id = $1::uuid`, userID)
	// Works.
	db.Exec(`DELETE FROM works WHERE author_id = $1::uuid`, userID)
	// User social graph & sessions.
	db.Exec(`DELETE FROM follows       WHERE follower_id = $1::uuid OR following_id = $1::uuid`, userID)
	db.Exec(`DELETE FROM subscriptions WHERE subscriber_id = $1::uuid OR creator_id = $1::uuid`, userID)
	db.Exec(`DELETE FROM user_sessions WHERE user_id = $1::uuid`, userID)
	// Profile & identity.
	db.Exec(`DELETE FROM user_profiles WHERE user_id = $1::uuid`, userID)
	db.Exec(`DELETE FROM user_roles    WHERE user_id = $1::uuid`, userID)
	if pialID != "" {
		db.Exec(`DELETE FROM pial_roots WHERE pial_id = $1::uuid`, pialID)
	}
	_, err := db.Exec(`DELETE FROM users WHERE id = $1::uuid`, userID)
	return err
}

// GetWorkByID fetches a single work by its UUID string. viewerID is used for reaction state.
// Deleted works (deleted_at IS NOT NULL) are excluded.
func GetWorkByID(db *sql.DB, workID, viewerID string) (*model.Work, error) {
	rows, err := db.Query(worksSelectSQL+`
		WHERE w.id = $1::uuid AND w.deleted_at IS NULL
	`, workID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, sql.ErrNoRows
	}

	w, err := scanWork(rows)
	if err != nil {
		return nil, err
	}

	// Populate citations
	citRows, err := db.Query(`
		SELECT wc.citation_type, w2.cid AS target_cid
		FROM work_citations wc JOIN works w2 ON w2.id = wc.target_id
		WHERE wc.work_id = $1::uuid
	`, workID)
	if err != nil {
		log.Printf("works: querying citations for %s: %v", workID, err)
	} else {
		defer citRows.Close()
		for citRows.Next() {
			var citType, targetCID string
			if err := citRows.Scan(&citType, &targetCID); err != nil {
				log.Printf("works: scanning citation: %v", err)
				continue
			}
			switch citType {
			case "reply":
				w.ParentCID = targetCID
			case "quote":
				w.QuotedCID = targetCID
			}
		}
	}

	// Populate viewer reaction state
	if viewerID != "" {
		EnrichWorksWithReactions(db, []*model.Work{w}, viewerID)
	}
	EnrichWorksWithQuotes(db, []*model.Work{w})
	EnrichWorksWithLinkPreviews(db, []*model.Work{w})

	return w, nil
}

// EnrichWorksWithReactions sets reaction state and follow state on each work for the viewer.
func EnrichWorksWithReactions(db *sql.DB, works []*model.Work, viewerID string) {
	if len(works) == 0 || viewerID == "" {
		return
	}

	ids := make([]string, len(works))
	index := make(map[string]*model.Work, len(works))
	for i, w := range works {
		ids[i] = w.ID
		index[w.ID] = w
	}

	// Reaction state (like/repost/bookmark)
	rows, err := db.Query(`
		SELECT work_id::text, reaction_type
		FROM work_reactions
		WHERE reactor_id = $1::uuid
		AND work_id = ANY($2::uuid[])
	`, viewerID, pq.Array(ids))
	if err != nil {
		log.Printf("works: EnrichWorksWithReactions: %v", err)
	} else {
		defer rows.Close()
		for rows.Next() {
			var workID, reactionType string
			if err := rows.Scan(&workID, &reactionType); err != nil {
				log.Printf("works: EnrichWorksWithReactions scan: %v", err)
				continue
			}
			w, ok := index[workID]
			if !ok {
				continue
			}
			switch reactionType {
			case "like":
				w.LikedByUser = true
			case "repost":
				w.RepostedByUser = true
			case "bookmark":
				w.BookmarkedByUser = true
			case "dislike":
				w.DislikedByUser = true
			}
		}
	}

	// Follow state: does viewer follow each work's author?
	authorIDs := make([]string, 0, len(works))
	seen := make(map[string]bool, len(works))
	for _, w := range works {
		if !seen[w.AuthorID] {
			authorIDs = append(authorIDs, w.AuthorID)
			seen[w.AuthorID] = true
		}
	}
	frows, ferr := db.Query(`
		SELECT following_id::text
		FROM follows
		WHERE follower_id = $1::uuid
		AND following_id = ANY($2::uuid[])
	`, viewerID, pq.Array(authorIDs))
	if ferr != nil {
		log.Printf("works: EnrichWorksWithReactions follow: %v", ferr)
		return
	}
	defer frows.Close()
	followed := make(map[string]bool)
	for frows.Next() {
		var fid string
		if frows.Scan(&fid) == nil {
			followed[fid] = true
		}
	}
	for _, w := range works {
		w.ViewerFollowsAuthor = followed[w.AuthorID]
	}
}

// EnrichWorksWithRepliers batch-loads up to 3 recent replier handles + avatars per work.
func EnrichWorksWithRepliers(db *sql.DB, works []*model.Work) {
	if len(works) == 0 {
		return
	}
	ids := make([]string, len(works))
	index := make(map[string]*model.Work, len(works))
	for i, w := range works {
		ids[i] = w.ID
		index[w.ID] = w
	}
	rows, err := db.Query(`
		SELECT wc.target_id::text, u.handle, COALESCE(up.avatar_url, '')
		FROM work_citations wc
		JOIN works w ON w.id = wc.work_id
		JOIN users u ON u.id = w.author_id
		LEFT JOIN user_profiles up ON up.user_id = w.author_id
		WHERE wc.target_id = ANY($1::uuid[])
		  AND wc.citation_type = 'reply'
		  AND w.deleted_at IS NULL
		ORDER BY w.created_at DESC
	`, pq.Array(ids))
	if err != nil {
		log.Printf("works: EnrichWorksWithRepliers: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var targetID, handle, avatar string
		if err := rows.Scan(&targetID, &handle, &avatar); err != nil {
			continue
		}
		w, ok := index[targetID]
		if !ok || len(w.LatestReplierHandles) >= 3 {
			continue
		}
		w.LatestReplierHandles = append(w.LatestReplierHandles, handle)
		w.LatestReplierAvatars = append(w.LatestReplierAvatars, avatar)
	}
}

// EnrichWorksWithQuotes batch-loads the QuotedWork for any work with kind='quote'.
// Uses two queries: one for citations, one for the quoted works themselves.
func EnrichWorksWithQuotes(db *sql.DB, works []*model.Work) {
	if len(works) == 0 {
		return
	}

	var quoteIDs []string
	index := make(map[string]*model.Work, len(works))
	for _, w := range works {
		// react_video works also embed an original via a 'quote' citation.
		if w.Kind == "quote" || w.Kind == "react_video" {
			quoteIDs = append(quoteIDs, w.ID)
			index[w.ID] = w
		}
	}
	if len(quoteIDs) == 0 {
		return
	}

	// Step 1: fetch citation targets for all quoting works.
	crows, err := db.Query(`
		SELECT wc.work_id::text, wc.target_id::text
		FROM work_citations wc
		WHERE wc.work_id = ANY($1::uuid[])
		  AND wc.citation_type = 'quote'
	`, pq.Array(quoteIDs))
	if err != nil {
		log.Printf("works: EnrichWorksWithQuotes citations: %v", err)
		return
	}
	defer crows.Close()

	var targetIDs []string
	citMap := make(map[string]string, len(quoteIDs))
	for crows.Next() {
		var wID, tID string
		if crows.Scan(&wID, &tID) == nil {
			citMap[wID] = tID
			targetIDs = append(targetIDs, tID)
		}
	}
	if len(targetIDs) == 0 {
		return
	}

	// Step 2: batch-fetch the quoted works.
	trows, err := db.Query(worksSelectSQL+`
		WHERE w.id = ANY($1::uuid[]) AND w.deleted_at IS NULL
	`, pq.Array(targetIDs))
	if err != nil {
		log.Printf("works: EnrichWorksWithQuotes fetch: %v", err)
		return
	}
	defer trows.Close()

	quotedMap := make(map[string]*model.Work, len(targetIDs))
	for trows.Next() {
		qw, err := scanWork(trows)
		if err == nil {
			quotedMap[qw.ID] = qw
		}
	}

	// Step 3: assign quoted works.
	for _, w := range works {
		if tID, ok := citMap[w.ID]; ok {
			w.QuotedWork = quotedMap[tID]
		}
	}
}

// EnrichWorksWithLinkPreviews batch-loads OG link preview metadata for works whose body
// contains an external URL but no YouTube video (YouTube uses the embed player instead).
// One DB query for all distinct URLs; no N+1.
func EnrichWorksWithLinkPreviews(db *sql.DB, works []*model.Work) {
	if len(works) == 0 {
		return
	}
	urlSet := map[string]struct{}{}
	for _, w := range works {
		if w.YouTubeID != "" {
			continue
		}
		if u := extractFirstBodyURL(w.Body); u != "" {
			urlSet[u] = struct{}{}
		}
	}
	if len(urlSet) == 0 {
		return
	}
	urls := make([]string, 0, len(urlSet))
	for u := range urlSet {
		urls = append(urls, u)
	}
	rows, err := db.Query(
		`SELECT url, title, description, image_url, site_name
		 FROM link_previews
		 WHERE url = ANY($1::text[])`,
		pq.Array(urls),
	)
	if err != nil {
		return
	}
	defer rows.Close()
	previews := map[string]*model.LinkPreview{}
	for rows.Next() {
		lp := &model.LinkPreview{}
		if err := rows.Scan(&lp.URL, &lp.Title, &lp.Description, &lp.ImageURL, &lp.SiteName); err == nil {
			previews[lp.URL] = lp
		}
	}
	for _, w := range works {
		if w.YouTubeID != "" {
			continue
		}
		if u := extractFirstBodyURL(w.Body); u != "" {
			if lp, ok := previews[u]; ok {
				w.LinkPreview = lp
			}
		}
	}
}

// GetWorksForYou returns recent works from all users (global discovery feed), newest first.
// before is RFC3339; empty = start from now.
func GetWorksForYou(db *sql.DB, limit int, before string) ([]*model.Work, error) {
	t, err := parseBefore(before)
	if err != nil {
		return nil, err
	}
	// Global feed: all public works UNION reposts (work_reactions) by anyone,
	// each work once at its newest surfacing, ordered by feed time — so a repost
	// lifts the original back to the top of the default home feed.
	const idQuery = `
		SELECT id, reposter_handle, reposter_name, feed_ts FROM (
		  SELECT DISTINCT ON (id) id, reposter_handle, reposter_name, feed_ts FROM (
		    SELECT w.id AS id, NULL::text AS reposter_handle, NULL::text AS reposter_name, w.created_at AS feed_ts
		    FROM works w
		    WHERE w.deleted_at IS NULL AND NOT w.is_blocked AND NOT w.subscriber_only AND w.kind <> 'reply'
		      AND (w.kind <> 'vision' OR w.expires_at IS NULL OR w.expires_at > NOW())
		    UNION ALL
		    SELECT wr.work_id AS id, ru.handle AS reposter_handle,
		           COALESCE(NULLIF(TRIM(rp.display_name),''), ru.handle) AS reposter_name, wr.created_at AS feed_ts
		    FROM work_reactions wr
		    JOIN works w ON w.id = wr.work_id
		    JOIN users ru ON ru.id = wr.reactor_id
		    LEFT JOIN user_profiles rp ON rp.user_id = wr.reactor_id
		    WHERE wr.reaction_type = 'repost'
		      AND w.deleted_at IS NULL AND NOT w.is_blocked AND NOT w.subscriber_only AND w.kind <> 'reply'
		  ) feed
		  WHERE feed_ts < $1
		  ORDER BY id, feed_ts DESC
		) d
		ORDER BY feed_ts DESC
		LIMIT $2`
	return collectFeedWithReposts(db, idQuery, t, limit)
}

// GetWorksNSFW returns works flagged as sensitive content, newest first. Hard-gated by caller.
func GetWorksNSFW(db *sql.DB, limit int, before string) ([]*model.Work, error) {
	t, err := parseBefore(before)
	if err != nil {
		return nil, err
	}
	// 18+ feed: a work is adult if it is flagged NSFW or gore at the work level,
	// or its author is an adult-content creator. (The old filter used
	// w.is_sensitive, a column nothing ever sets — so the feed was always empty.)
	rows, err := db.Query(worksSelectSQL+`
		WHERE w.deleted_at IS NULL
		  AND NOT w.is_blocked
		  AND NOT w.subscriber_only
		  AND w.kind != 'reply'
		  AND (w.kind != 'vision' OR w.expires_at IS NULL OR w.expires_at > NOW())
		  AND (w.is_nsfw = TRUE OR w.is_gore = TRUE OR COALESCE(up.is_adult_creator, FALSE) = TRUE)
		  AND w.created_at < $1
		ORDER BY w.created_at DESC
		LIMIT $2
	`, t, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectWorks(rows)
}

// GetWorksTrending returns globally recent works ordered by created_at (trending by recency; score-based ranking is done by AethyrRank on ingest).
func GetWorksTrending(db *sql.DB, limit int, before string) ([]*model.Work, error) {
	t, err := parseBefore(before)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(worksSelectSQL+`
		WHERE w.deleted_at IS NULL
		  AND NOT w.is_blocked
		  AND NOT w.subscriber_only
		  AND w.kind != 'reply'
		  AND (w.kind != 'vision' OR w.expires_at IS NULL OR w.expires_at > NOW())
		  AND (w.scheduled_at IS NULL OR w.scheduled_at <= NOW())
		  AND w.created_at < $1
		ORDER BY w.created_at DESC
		LIMIT $2
	`, t, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectWorks(rows)
}

// GetWorksLikedByUser returns works the viewer has liked, newest reaction first.
func GetWorksLikedByUser(db *sql.DB, viewerID string, limit int) ([]*model.Work, error) {
	rows, err := db.Query(worksSelectSQL+`
		JOIN work_reactions wr ON wr.work_id = w.id
		WHERE wr.reactor_id = $1::uuid
		  AND wr.reaction_type = 'like'
		  AND w.deleted_at IS NULL
		  AND NOT w.is_blocked
		ORDER BY wr.created_at DESC
		LIMIT $2
	`, viewerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectWorks(rows)
}

// GetWorksRepostedByUser returns the works a given user has reposted, newest
// repost first, each carrying that user's "<name> reposted" attribution. Powers
// the profile Reposts tab — same works-native repost source as the feeds.
func GetWorksRepostedByUser(db *sql.DB, reposterID string, limit int, before string) ([]*model.Work, error) {
	cursor, err := parseBefore(before)
	if err != nil {
		return nil, err
	}
	const idQuery = `
		SELECT wr.work_id AS id, ru.handle AS reposter_handle,
		       COALESCE(NULLIF(TRIM(rp.display_name),''), ru.handle) AS reposter_name, wr.created_at AS feed_ts
		FROM work_reactions wr
		JOIN works w ON w.id = wr.work_id
		JOIN users ru ON ru.id = wr.reactor_id
		LEFT JOIN user_profiles rp ON rp.user_id = wr.reactor_id
		WHERE wr.reaction_type = 'repost'
		  AND wr.reactor_id = $1::uuid
		  AND w.deleted_at IS NULL AND NOT w.is_blocked AND NOT w.subscriber_only AND w.kind <> 'reply'
		  AND wr.created_at < $2
		ORDER BY wr.created_at DESC
		LIMIT $3`
	return collectFeedWithReposts(db, idQuery, reposterID, cursor, limit)
}

// GetWorksSaves returns works bookmarked by the viewer (user ID, not PIAL), newest bookmark first.
func GetWorksSaves(db *sql.DB, viewerID string, limit int, before string) ([]*model.Work, error) {
	t, err := parseBefore(before)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(worksSelectSQL+`
		JOIN work_reactions wr ON wr.work_id = w.id
		WHERE wr.reactor_id = $1::uuid
		  AND wr.reaction_type = 'bookmark'
		  AND w.deleted_at IS NULL
		  AND NOT w.is_blocked
		  AND (w.kind != 'vision' OR w.expires_at IS NULL OR w.expires_at > NOW())
		  AND w.created_at < $2
		ORDER BY wr.created_at DESC
		LIMIT $3
	`, viewerID, t, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectWorks(rows)
}

// SearchWorks returns works whose body matches a full-text query, newest first.
func SearchWorks(db *sql.DB, query string, limit int, viewerID string) ([]*model.Work, error) {
	rows, err := db.Query(worksSelectSQL+`
		WHERE w.deleted_at IS NULL
		  AND NOT w.is_blocked
		  AND w.kind != 'reply'
		  AND (w.kind != 'vision' OR w.expires_at IS NULL OR w.expires_at > NOW())
		  AND (w.scheduled_at IS NULL OR w.scheduled_at <= NOW())
		  AND to_tsvector('english', w.body) @@ plainto_tsquery('english', $1)
		ORDER BY w.created_at DESC
		LIMIT $2
	`, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	works, err := collectWorks(rows)
	if err != nil {
		return nil, err
	}
	if viewerID != "" {
		EnrichWorksWithReactions(db, works, viewerID)
	}
	EnrichWorksWithQuotes(db, works)
	return works, nil
}

// collectWorks drains a rows result set into a []*model.Work slice.
func collectWorks(rows *sql.Rows) ([]*model.Work, error) {
	var works []*model.Work
	for rows.Next() {
		w, err := scanWork(rows)
		if err != nil {
			return nil, err
		}
		works = append(works, w)
	}
	return works, rows.Err()
}

// feedRow is one entry produced by a feed-id query: a work id plus optional
// repost attribution and the timestamp the item surfaced at.
type feedRow struct {
	id             string
	reposterHandle string
	reposterName   string
	feedTS         time.Time
}

// collectFeedWithReposts runs a lightweight feed-id query (originals UNION
// reposts, already deduped + ordered + paginated by the caller), then batch-loads
// the full works via worksSelectSQL and attaches repost attribution in feed order.
// Reposts are works-native: they live in work_reactions(reaction_type='repost'),
// never in the legacy posts table.
func collectFeedWithReposts(db *sql.DB, idQuery string, args ...interface{}) ([]*model.Work, error) {
	rows, err := db.Query(idQuery, args...)
	if err != nil {
		return nil, err
	}
	var order []feedRow
	for rows.Next() {
		var fr feedRow
		var rh, rn sql.NullString
		if err := rows.Scan(&fr.id, &rh, &rn, &fr.feedTS); err != nil {
			rows.Close()
			return nil, err
		}
		fr.reposterHandle = rh.String
		fr.reposterName = rn.String
		order = append(order, fr)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(order) == 0 {
		return nil, nil
	}

	ids := make([]string, len(order))
	for i, fr := range order {
		ids[i] = fr.id
	}
	wrows, err := db.Query(worksSelectSQL+`WHERE w.id = ANY($1::uuid[])`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	works, err := collectWorks(wrows)
	wrows.Close()
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*model.Work, len(works))
	for _, w := range works {
		byID[w.ID] = w
	}

	out := make([]*model.Work, 0, len(order))
	for _, fr := range order {
		w := byID[fr.id]
		if w == nil {
			continue // work was deleted/blocked between the two queries
		}
		if fr.reposterHandle != "" {
			w.RepostedByHandle = fr.reposterHandle
			w.RepostedByName = fr.reposterName
			w.RepostedAt = fr.feedTS
		}
		out = append(out, w)
	}
	return out, nil
}

// EnrichWorksWithPinnedState marks the work whose ID matches pinnedWorkID as IsPinned=true.
// Call after loading any feed that may include the viewer's own pinned post.
func EnrichWorksWithPinnedState(works []*model.Work, pinnedWorkID string) {
	if pinnedWorkID == "" {
		return
	}
	for _, w := range works {
		if w.ID == pinnedWorkID {
			w.IsPinned = true
		}
	}
}

// GetWorksSavesFiltered returns works bookmarked by the viewer, optionally filtered by kind.
// kind="" returns all; any other value (e.g. "text", "audio", "video") restricts to that kind.
func GetWorksSavesFiltered(db *sql.DB, viewerID string, limit int, before, kind string) ([]*model.Work, error) {
	t, err := parseBefore(before)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(worksSelectSQL+`
		JOIN work_reactions wr ON wr.work_id = w.id
		WHERE wr.reactor_id = $1::uuid
		  AND wr.reaction_type = 'bookmark'
		  AND w.deleted_at IS NULL
		  AND NOT w.is_blocked
		  AND w.created_at < $2
		  AND ($4 = '' OR w.kind = $4)
		ORDER BY wr.created_at DESC
		LIMIT $3
	`, viewerID, t, limit, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectWorks(rows)
}

// GetWorksByList returns works authored by members of the given list, cursor-paginated.
func GetWorksByList(db *sql.DB, listID string, limit int, before string) ([]*model.Work, error) {
	t, err := parseBefore(before)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(worksSelectSQL+`
		WHERE w.author_id IN (SELECT user_id FROM list_members WHERE list_id = $1::uuid)
		  AND w.deleted_at IS NULL
		  AND NOT w.is_blocked
		  AND (w.expires_at IS NULL OR w.expires_at > NOW())
		  AND w.created_at < $2
		ORDER BY w.created_at DESC
		LIMIT $3
	`, listID, t, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectWorks(rows)
}

// GetWorkReplies returns works that directly reply to the given work, newest first.
func GetWorkReplies(db *sql.DB, workID string, limit int) ([]*model.Work, error) {
	rows, err := db.Query(worksSelectSQL+`
		JOIN work_citations wc ON wc.work_id = w.id
		WHERE wc.target_id = $1::uuid
		  AND wc.citation_type = 'reply'
		  AND w.deleted_at IS NULL
		  AND NOT w.is_blocked
		ORDER BY w.created_at DESC
		LIMIT $2
	`, workID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectWorks(rows)
}

// GetVisionsWorksFeed returns all media-rich works from the viewer and followed accounts.
// Vision-kind works are excluded once their 24h window expires; other media works are permanent.
// Cursor-paginated by created_at.
func GetVisionsWorksFeed(db *sql.DB, viewerID string, limit int, before string) ([]*model.Work, error) {
	cursor, err := parseBefore(before)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(worksSelectSQL+`
		WHERE (
		    array_length(w.media_urls, 1) > 0
		    OR w.video_master_url IS NOT NULL
		)
		AND (
		    w.author_id = $1::uuid
		    OR w.author_id IN (
		        SELECT following_id FROM follows WHERE follower_id = $1::uuid
		    )
		)
		AND w.deleted_at IS NULL
		AND NOT w.is_blocked
		AND (w.kind != 'vision' OR w.expires_at IS NULL OR w.expires_at > NOW())
		AND w.created_at < $2
		ORDER BY w.created_at DESC
		LIMIT $3
	`, viewerID, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectWorks(rows)
}

// GetVisionsWorksForYou returns all media-rich works platform-wide, newest first.
// Vision-kind works are excluded once their 24h window expires; other media works are permanent.
func GetVisionsWorksForYou(db *sql.DB, limit int, before string) ([]*model.Work, error) {
	cursor, err := parseBefore(before)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(worksSelectSQL+`
		WHERE (
		    array_length(w.media_urls, 1) > 0
		    OR w.video_master_url IS NOT NULL
		)
		AND w.deleted_at IS NULL
		AND NOT w.is_blocked
		AND (w.kind != 'vision' OR w.expires_at IS NULL OR w.expires_at > NOW())
		AND w.created_at < $1
		ORDER BY w.created_at DESC
		LIMIT $2
	`, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectWorks(rows)
}

// SetWorkLineage sets the lineage_pial and lineage_handle on a work, marking it as
// duplicate content with a reference to the original creator's PIAL and handle.
// Called by the scan pipeline when deduplication detects a prior upload.
func SetWorkLineage(db *sql.DB, workID, lineagePIAL, lineageHandle string) error {
	_, err := db.Exec(
		`UPDATE works SET lineage_pial = $2, lineage_handle = $3
		 WHERE id = $1::uuid AND deleted_at IS NULL`,
		workID, lineagePIAL, lineageHandle,
	)
	return err
}

// GetWorkParentChain walks up the reply citation chain up to maxDepth levels.
// Returns ancestors oldest-first (index 0 = furthest ancestor).
func GetWorkParentChain(db *sql.DB, workID, viewerID string, maxDepth int) ([]*model.Work, error) {
	var chain []*model.Work
	currentID := workID
	for range maxDepth {
		var parentID string
		if err := db.QueryRow(`
			SELECT target_id::text
			FROM work_citations
			WHERE work_id = $1::uuid AND citation_type = 'reply'
		`, currentID).Scan(&parentID); err != nil {
			break
		}
		parent, err := GetWorkByID(db, parentID, viewerID)
		if err != nil {
			break
		}
		chain = append([]*model.Work{parent}, chain...)
		currentID = parentID
	}
	return chain, nil
}

// ── Microconversation ──────────────────────────────────────────────────────────

// GetMicroconversations returns the top-N active microconversations for a work.
// A microconversation is a depth-1 reply to rootWorkID that itself has sub-replies.
// Results are ordered by sub-reply count descending (most active first).
// Each microconversation collapses to seed + up to 3 direct exchanges.
func GetMicroconversations(db *sql.DB, rootWorkID string, limit int) ([]*model.Microconversation, error) {
	rows, err := db.Query(`
		SELECT w.id::text, COUNT(wc2.work_id)::int AS sub_count
		FROM works w
		JOIN work_citations wc ON wc.work_id = w.id
		                      AND wc.citation_type = 'reply'
		                      AND wc.target_id = $1::uuid
		LEFT JOIN work_citations wc2 ON wc2.target_id = w.id
		                             AND wc2.citation_type = 'reply'
		WHERE w.deleted_at IS NULL
		  AND NOT w.is_blocked
		GROUP BY w.id
		HAVING COUNT(wc2.work_id) > 0
		ORDER BY COUNT(wc2.work_id) DESC
		LIMIT $2
	`, rootWorkID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type seedRow struct {
		ID       string
		SubCount int
	}
	var seeds []seedRow
	for rows.Next() {
		var s seedRow
		if err := rows.Scan(&s.ID, &s.SubCount); err != nil {
			continue
		}
		seeds = append(seeds, s)
	}
	rows.Close()

	if len(seeds) == 0 {
		return nil, nil
	}

	const exchangeLimit = 3
	var mcs []*model.Microconversation
	for _, seed := range seeds {
		seedWork, err := GetWorkByID(db, seed.ID, "")
		if err != nil {
			continue
		}

		exchRows, err := db.Query(worksSelectSQL+`
			JOIN work_citations wc ON wc.work_id = w.id
			                      AND wc.citation_type = 'reply'
			                      AND wc.target_id = $1::uuid
			WHERE w.deleted_at IS NULL
			  AND NOT w.is_blocked
			ORDER BY w.created_at ASC
			LIMIT $2
		`, seed.ID, exchangeLimit)
		if err != nil {
			continue
		}
		exchanges, _ := collectWorks(exchRows)

		seen := map[string]bool{}
		var participants []model.MicroconversationParticipant
		addP := func(handle, avatar string) {
			if handle != "" && !seen[handle] {
				seen[handle] = true
				participants = append(participants, model.MicroconversationParticipant{Handle: handle, AvatarURL: avatar})
			}
		}
		addP(seedWork.AuthorHandle, seedWork.AvatarURL)
		for _, ex := range exchanges {
			addP(ex.AuthorHandle, ex.AvatarURL)
		}

		moreCount := seed.SubCount - len(exchanges)
		if moreCount < 0 {
			moreCount = 0
		}

		mcs = append(mcs, &model.Microconversation{
			ConversationID: seed.ID,
			SeedWork:       seedWork,
			Participants:   participants,
			ReplyCount:     seed.SubCount,
			MoreCount:      moreCount,
			Exchanges:      exchanges,
		})
	}
	return mcs, nil
}

// GetWorkLoneReplies returns direct replies to workID that have no sub-replies.
// These render as plain work cards in the reply stream rather than microconversation containers.
func GetWorkLoneReplies(db *sql.DB, workID string, limit int) ([]*model.Work, error) {
	rows, err := db.Query(worksSelectSQL+`
		JOIN work_citations wc ON wc.work_id = w.id
		                      AND wc.citation_type = 'reply'
		                      AND wc.target_id = $1::uuid
		WHERE w.deleted_at IS NULL
		  AND NOT w.is_blocked
		  AND NOT EXISTS (
		      SELECT 1 FROM work_citations wc2
		      WHERE wc2.target_id = w.id AND wc2.citation_type = 'reply'
		  )
		ORDER BY w.created_at DESC
		LIMIT $2
	`, workID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectWorks(rows)
}

