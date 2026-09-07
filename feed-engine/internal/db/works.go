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

	"github.com/f33d3r/feed-engine/internal/jung"
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

	// The work's position on the Jung axes is a pure function of the work,
	// so it is computed here, once, from exactly the values being written —
	// the same inputs the lazy read-side computation would see later. Insert
	// and vector are one statement: no work exists without its vector.
	psych := jung.MapWork(&model.Work{
		Body:           body,
		Kind:           kind,
		MediaURLs:      mediaURLs,
		Tags:           tags,
		PollOptions:    pollOptions,
		IsNSFW:         isNSFW,
		VoiceURL:       voiceURL,
		VideoMasterURL: videoMasterURL,
	})

	var workID string
	err := db.QueryRow(`
		INSERT INTO works (
			cid, author_id, author_pial, body, kind, media_urls,
			tags, poll_options, poll_ends_at, scheduled_at,
			comment_gating, subscriber_only, is_nsfw, is_repost, repost_source_id,
			voice_url, voice_duration_secs,
			video_master_url, video_watermarked_url, video_poster_url, video_duration_secs,
			video_width, video_height, react_layout,
			psych_vector,
			-- scan_state opens as 'pending': the verdict belongs to the content
			-- scan, never to the insert. cron.pendingScanSweep resolves anything
			-- the scan never answered for.
			scan_state
		) VALUES (
			$1, $2::uuid, $3::uuid, $4, $5, $6,
			$7, $8, $9, $10,
			$11, $12, $13, $14, $15::uuid,
			$16, $17,
			$18, $19, $20, $21,
			$22, $23, $24,
			$25,
			'pending'
		)
		RETURNING id
	`,
		cid, authorID, authorPIAL, body, kind, pq.Array(mediaURLs),
		pq.Array(tags), pq.Array(pollOptions), pollEndsAt, scheduledAt,
		commentGating, subscriberOnly, isNSFW, isRepost, repostSourceIDParam,
		voiceURLParam, voiceDurationSecs,
		videoMasterURLParam, videoWatermarkedURLParam, videoPosterURLParam, videoDurationSecs,
		videoWidth, videoHeight, reactLayoutParam,
		pq.Float32Array(psych.Slice()),
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
			// root_id and depth are materialized by the database (trigger
			// work_citations_materialize), so the chain this reply joins is
			// recorded on the edge itself and never has to be walked to be known.
			// ON CONFLICT makes a re-delivered create idempotent instead of an error.
			// No counter is bumped here: reply totals are counted from these rows.
			_, err = db.Exec(`
				INSERT INTO work_citations (work_id, target_id, citation_type)
				VALUES ($1::uuid, $2::uuid, 'reply')
				ON CONFLICT (work_id, target_id, citation_type) DO NOTHING
			`, workID, targetID)
			if err != nil {
				log.Printf("works: inserting reply citation for work %s: %v", workID, err)
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
				ON CONFLICT (work_id, target_id, citation_type) DO NOTHING
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

// workLiveSQL excludes works whose ephemeral lifetime has run out. Only ephemeral
// kinds get an expires_at, so the NULL check covers every permanent work.
const workLiveSQL = `(w.expires_at IS NULL OR w.expires_at > NOW())`

// workNotBlockedSQL is the moderation verdict expressed as a read predicate.
// 'blocked' is the only verdict that hides a work; an undecided scan is not a
// verdict, so a scanner outage cannot take the feeds down. is_blocked is the
// admin/manual lane and scan_state is the automated one — both are checked
// because a work can be stopped by either.
const workNotBlockedSQL = `(NOT w.is_blocked AND w.scan_state <> 'blocked')`

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
    (COALESCE(plr.kyc_tier, 'none') = 'full'), COALESCE(u.role,'user'), COALESCE(u.realm, 1),
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
    (SELECT COUNT(*) FROM work_citations wc WHERE wc.target_id = w.id AND wc.citation_type='reply')::int,
    (SELECT COUNT(*) FROM work_citations wc WHERE wc.target_id = w.id AND wc.citation_type='quote')::int,
    COALESCE(w.is_edited, FALSE),
    w.edited_at,
    COALESCE(w.view_count, 0),
    COALESCE(up.is_adult_creator, FALSE),
    w.psych_vector
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
	var videoMasterURL sql.NullString
	var videoWatermarkedURL sql.NullString
	var videoPosterURL sql.NullString
	var videoDuration sql.NullFloat64
	var videoWidth sql.NullInt64
	var videoHeight sql.NullInt64
	var editedAt sql.NullTime
	var psychVector pq.Float32Array

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
		// Jung layer — NULL until first computed
		&psychVector,
	)
	if err != nil {
		return nil, err
	}

	w.MediaURLs = []string(mediaURLs)
	if len(psychVector) == jung.Dim {
		w.PsychVector = []float32(psychVector)
	}
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
		      AND w.deleted_at IS NULL AND ` + workNotBlockedSQL + ` AND w.kind <> 'reply'
		      AND ` + workLiveSQL + `
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
		      AND w.deleted_at IS NULL AND ` + workNotBlockedSQL + ` AND w.kind <> 'reply'
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
		AND `+workNotBlockedSQL+`
		AND w.kind != 'reply'
		AND `+workLiveSQL+`
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
//
// The retirement and the hash purge are one transaction. The hashes are what
// stop the same media being uploaded again, so a hash purge that fails after the
// work is retired leaves the author unable to re-post their own media with no
// error anywhere to explain why — which is what happened while those two
// statements discarded their errors.
func SoftDeleteWork(db *sql.DB, workID, authorID string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("retiring work %s: begin: %w", workID, err)
	}
	defer func() {
		if rerr := tx.Rollback(); rerr != nil && !errors.Is(rerr, sql.ErrTxDone) {
			log.Printf("retiring work %s: rollback: %v", workID, rerr)
		}
	}()

	result, err := tx.Exec(`
		UPDATE works
		SET deleted_at = NOW()
		WHERE id = $1::uuid
		  AND author_id = $2::uuid
		  AND deleted_at IS NULL
	`, workID, authorID)
	if err != nil {
		return fmt.Errorf("retiring work %s: %w", workID, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("retiring work %s: counting retired rows: %w", workID, err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}

	// Purge hashes so the media can be freely re-uploaded by anyone. Read against
	// the same snapshot that just retired the work, so media_urls is still there.
	if _, err := tx.Exec(
		`DELETE FROM video_raw_hashes WHERE post_id = $1`, workID); err != nil {
		return fmt.Errorf("retiring work %s: purging video hashes: %w", workID, err)
	}
	if _, err := tx.Exec(`DELETE FROM image_raw_hashes WHERE media_url IN (
		SELECT unnest(media_urls) FROM works WHERE id = $1::uuid
	)`, workID); err != nil {
		return fmt.Errorf("retiring work %s: purging image hashes: %w", workID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("retiring work %s: commit: %w", workID, err)
	}
	return nil
}

// AdminDeleteWork hard-deletes a work and its associated hashes without author ownership check.
// Used by admins to remove content regardless of who posted it.
//
// One transaction, every error returned. The work's own children — reactions,
// citations, editions, scores, poll votes — are reached by ON DELETE CASCADE from
// works, and the pinned-work reference by ON DELETE SET NULL, so the only
// statements here are the two hash tables, which have no foreign key to works and
// which nothing else would ever clear.
func AdminDeleteWork(db *sql.DB, workID string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("deleting work %s: begin: %w", workID, err)
	}
	defer func() {
		if rerr := tx.Rollback(); rerr != nil && !errors.Is(rerr, sql.ErrTxDone) {
			log.Printf("deleting work %s: rollback: %v", workID, rerr)
		}
	}()

	// Image hashes are keyed by media URL, so they must be resolved through the
	// works row while it still exists.
	if _, err := tx.Exec(`DELETE FROM image_raw_hashes WHERE media_url IN (
		SELECT unnest(media_urls) FROM works WHERE id = $1::uuid
	)`, workID); err != nil {
		return fmt.Errorf("deleting work %s: purging image hashes: %w", workID, err)
	}
	if _, err := tx.Exec(
		`DELETE FROM video_raw_hashes WHERE post_id = $1`, workID); err != nil {
		return fmt.Errorf("deleting work %s: purging video hashes: %w", workID, err)
	}

	result, err := tx.Exec(`DELETE FROM works WHERE id = $1::uuid`, workID)
	if err != nil {
		return fmt.Errorf("deleting work %s: %w", workID, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("deleting work %s: counting deleted rows: %w", workID, err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("deleting work %s: commit: %w", workID, err)
	}
	return nil
}

// ErrPurgeIdentityMismatch is returned when the caller names a PIAL that is not
// the one the account actually holds. An irreversible erasure whose caller and
// whose database disagree about WHOSE identity is being destroyed does not
// proceed on either answer.
var ErrPurgeIdentityMismatch = errors.New("purge: caller's pial_id does not match the account's own")

// purgeScope names which of the two things a purge knows — the account and the
// identity root behind it — a statement is written against, and therefore what
// is bound to it. Placeholders follow the scope, in this order and no other:
//
//	scopeAccount   $1 = account id
//	scopeIdentity  $1 = PIAL id
//	scopeBoth      $1 = account id, $2 = PIAL id
//
// Stating it per statement is what keeps the list flat: the alternative is
// binding both to everything, which the driver rejects the moment a statement
// mentions only one.
type purgeScope uint8

const (
	scopeAccount purgeScope = iota
	scopeIdentity
	scopeBoth
)

// purgeOrphanStatements are the erasure statements the database cannot perform
// for itself, in the order they must run. Everything else a purge clears is
// reached by ON DELETE CASCADE from the single DELETE FROM users at the end;
// see the PurgeUser doc comment for why that split is where it is.
//
// Two shapes of statement live here:
//
//   - Tables that reference a person by account id or PIAL with NO FOREIGN KEY
//     at all, so nothing cascades and nothing ever would have. These are the rows
//     a purge silently left behind: the person's E2E signing and key-agreement
//     public keys, their media fingerprints, their Vision view history, their
//     ranking telemetry, and the sealed message keys addressed to them on
//     conversations they do not own.
//
//   - Handles and PIALs copied as plain text onto rows that SURVIVE the purge.
//     A foreign key releases an id; it cannot release a string. Left alone,
//     these keep naming the erased person on other people's rows forever.
//
// What a purge must reach is pinned against the live schema by
// TestPurgeClearsEveryTableThatNamesAPerson.
var purgeOrphanStatements = []struct {
	why   string
	scope purgeScope
	stmt  string
}{
	// ── text that names the person on rows other people keep ──────────────────
	{
		"a duplicate-upload lineage marker names the original creator on somebody else's surviving work",
		scopeBoth,
		`UPDATE works SET lineage_pial = NULL, lineage_handle = NULL
		  WHERE lineage_pial = $2::text AND author_id <> $1::uuid`,
	},
	{
		"canonical_media releases both ids by foreign key but creator_handle is text and would outlive them",
		scopeBoth,
		`UPDATE canonical_media SET creator_handle = ''
		  WHERE creator_user_id = $1::uuid OR creator_pial_id = $2::uuid`,
	},
	// ── no foreign key: nothing cascades, nothing ever did ────────────────────
	{
		"E2E signing public key — the identity's own key material",
		scopeIdentity,
		`DELETE FROM pial_signing_keys WHERE pial_id = $1::uuid`,
	},
	{
		"E2E key-agreement public key — the identity's own key material",
		scopeIdentity,
		`DELETE FROM pial_ecdh_keys WHERE pial_id = $1::uuid`,
	},
	{
		"sealed message keys addressed to this account on conversations it does not own; unopenable once the account is gone",
		scopeAccount,
		`DELETE FROM gnosis_sealed_keys WHERE recipient_account = $1::uuid`,
	},
	{
		"perceptual and audio fingerprints of everything the account uploaded",
		scopeIdentity,
		`DELETE FROM media_fingerprints WHERE uploader_pial = $1::text`,
	},
	{
		"raw video hashes, so the same media can be freely re-uploaded by anyone",
		scopeIdentity,
		`DELETE FROM video_raw_hashes WHERE uploader_pial = $1::text`,
	},
	{
		"raw image hashes, so the same media can be freely re-uploaded by anyone",
		scopeIdentity,
		`DELETE FROM image_raw_hashes WHERE uploader_pial = $1::text`,
	},
	{
		"who viewed whose Vision — the account's own viewing history",
		scopeIdentity,
		`DELETE FROM vision_views WHERE viewer_pial = $1::uuid`,
	},
	{
		"ranking telemetry: dwell, position and explore signals. The column is text and holds account ids and PIAL ids interchangeably, so both are named",
		scopeBoth,
		`DELETE FROM feedback_events WHERE user_id IN ($1::text, $2::text)`,
	},
	// ── rows that name the person to OTHER accounts ───────────────────────────
	{
		"notifications on other people's timelines that name this account, whether as the actor or as the subject a system message was written about",
		scopeAccount,
		`DELETE FROM notifications
		  WHERE actor_id = $1::uuid
		     OR (target_type = 'user' AND target_id = $1::text)`,
	},
	{
		"another account's XP ledger row for following this one. The reference is released rather than the row deleted: the XP was genuinely earned by somebody who is not being erased, and users.xp is a separate counter, so removing the event would put the ledger and the total permanently out of step",
		scopeAccount,
		`UPDATE xp_events SET content_id = ''
		  WHERE reason = 'follow' AND content_id = $1::text AND user_id <> $1::uuid`,
	},
	// ── PIAL-scoped state that no users cascade reaches ───────────────────────
	{
		"the account's own Visions and everything hung off them (views, poll votes, media) — migration 0023 addresses a Vision as (author_pial, seq) with no id and no foreign key to users, so deleting the account no longer cascades to them",
		scopeIdentity,
		`DELETE FROM visions WHERE author_pial = $1::uuid`,
	},
	{
		"Vision mutes, either direction — migration 0023 made vision_mutes PIAL-to-PIAL with no foreign key to users, so a mute this account set or was the target of would otherwise outlive it",
		scopeIdentity,
		`DELETE FROM vision_mutes WHERE muter_pial = $1::uuid OR muted_pial = $1::uuid`,
	},
	{
		"per-capability posting, messaging, tipping and streaming grants",
		scopeIdentity,
		`DELETE FROM pial_capabilities WHERE pial_id = $1::uuid`,
	},
	{
		"trust level, NSFW tier and violation counters — meaningless once the root is tombstoned, and enforcement_actions keeps the record of what was actually done",
		scopeIdentity,
		`DELETE FROM trust_scores WHERE pial_id = $1::uuid`,
	},
	{
		"hashed email, phone and device identifiers — pseudonymous personal data",
		scopeIdentity,
		`DELETE FROM identity_signals WHERE pial_id = $1::uuid`,
	},
}

// PurgeUser irreversibly erases an account. All of it or none of it.
//
// ─── WHAT A PURGE MEANS HERE ────────────────────────────────────────────────
//
// Purge is a HARD DELETE of the person, a TOMBSTONE of their identity root, and
// a short, named list of retained records. There is no anonymise-and-retain
// middle for the account itself: the users row, the profile, the works, the
// messages, the keys, the graph and the telemetry are deleted outright.
//
// SIX THINGS SURVIVE A PURGE. They are decisions, not bugs, and no future
// reader should have to guess which:
//
//  1. pial_roots — TOMBSTONED, never deleted. pial_event_ledger.pial_id is NOT
//     NULL and the ledger is append-only, so an identity's root is pinned for as
//     long as a single event references it, and every identity has events from
//     the moment it exists. The root is stripped of everything personal — public
//     key, state hash, date of birth, KYC tier, age and role flags — and left as
//     an opaque UUID with is_tombstoned, deleted_at and status='purged' set.
//     What remains names nobody and holds nothing about anyone.
//
//  2. pial_event_ledger — RETAINED with account_id released to NULL by its
//     foreign key. The append-only identity ledger is the one record on this
//     brain that is never rewritten; that is the whole of its value. The events
//     happened. What erasure is owed is that they stop pointing at a person, not
//     that the platform forgets it acted. NOTHING in this function, in
//     DeactivateUser, or in any admin path may DELETE from this table.
//
//  3. content_receipts — RETAINED against the tombstoned PIAL. These are the
//     mint receipts carrying kyc_tier_at_mint, the record that the creator was
//     verified at the moment the content was published. The schema already says
//     they were built to outlive their subject: content_receipts.work_id is
//     ON DELETE SET NULL, so a receipt already survives deletion of the work it
//     describes. 18 U.S.C. § 2257 retention is exactly why.
//
//  4. enforcement_actions — RETAINED against the tombstoned PIAL. The platform's
//     own record of what it did and why. A suspension a suspended person can
//     erase by deleting their account is not an enforcement record.
//
//  5. csam_scan_log and law_enforcement_queue — RETAINED. Mandatory retention
//     under 18 U.S.C. § 2258A; the privacy policy states plainly that these
//     cannot be deleted on request. Neither has a foreign key to anything, so
//     no cascade can reach them and this function must not either.
//
//  6. manhattan_outbox — RETAINED, and this one is easy to mistake for an
//     oversight. Its rows name PIALs, and a tombstoned PIAL is an opaque UUID
//     that identifies nobody. More to the point, the undelivered rows ARE the
//     erasure: they carry the association retractions that tell Manhattan the
//     follow edges are gone, so deleting them would leave the plane believing
//     in relationships that no longer exist — a stale authorisation in
//     elohim-veni. Its dedup keys are also what make the drain idempotent on
//     replay. Nothing here trades that for the removal of an identifier that no
//     longer identifies.
//
// ─── HOW IT WORKS ────────────────────────────────────────────────────────────
//
// One transaction. Every statement's error is returned. A failure at any point
// rolls back to the account existing intact — never to the half-purged state the
// previous implementation could only ever produce.
//
// Most of the erasure is performed by the database. Migration 0018 settled the
// referential action on every foreign key into users, so the single
// DELETE FROM users at the end cascades through works and their editions,
// citations, scores, reactions and votes; articles; live streams and their
// keys and samples; tracks; publications; lists;
// conversations, memberships and messages; sessions, credentials, devices and
// backup codes; profile, roles, achievements and XP; blocks, mutes,
// subscriptions and bookmarks; and both directions of the follow graph. A table
// added later inherits whatever ITS foreign key declares, which is why that half
// cannot rot the way a hand-maintained delete list does.
//
// purgeOrphanStatements above covers exactly what the database cannot: tables
// with no foreign key, and handles copied as text onto surviving rows.
//
// Deleting the follow edges is deliberately left to the cascade rather than done
// here. Migration 0016's trg_follows_counters keeps both parties' cached counts
// correct through a cascade, and 0017's trg_manhattan_retire_follows fires
// BEFORE DELETE ON users specifically so the association retractions reach
// Manhattan while both PIALs are still readable. Deleting the rows here first
// would produce the same end state by a path neither trigger was written for.
func PurgeUser(db *sql.DB, userID, pialID string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("purge %s: begin: %w", userID, err)
	}
	// Rollback is the default outcome. Only the explicit Commit below escapes it,
	// and rolling back an already-committed transaction is a no-op.
	defer func() {
		if rerr := tx.Rollback(); rerr != nil && !errors.Is(rerr, sql.ErrTxDone) {
			log.Printf("purge %s: rollback: %v", userID, rerr)
		}
	}()

	// The account's identity comes from the account row, not from the caller, and
	// the row is locked for the duration so nothing re-binds it mid-purge.
	var ownPIAL sql.NullString
	switch err := tx.QueryRow(
		`SELECT pial_id::text FROM users WHERE id = $1::uuid FOR UPDATE`, userID,
	).Scan(&ownPIAL); {
	case errors.Is(err, sql.ErrNoRows):
		return sql.ErrNoRows
	case err != nil:
		return fmt.Errorf("purge %s: locking account: %w", userID, err)
	}
	if pialID != "" && ownPIAL.Valid && ownPIAL.String != pialID {
		return fmt.Errorf("%w (caller said %s, account holds %s)",
			ErrPurgeIdentityMismatch, pialID, ownPIAL.String)
	}
	// An account with no PIAL predates the identity plane. Its PIAL-scoped
	// statements match nothing rather than being skipped, so the list stays one
	// list: NULL is never equal to anything, including another NULL.
	pial := ownPIAL // sql.NullString

	for _, s := range purgeOrphanStatements {
		var args []interface{}
		switch s.scope {
		case scopeAccount:
			args = []interface{}{userID}
		case scopeIdentity:
			args = []interface{}{pial}
		case scopeBoth:
			args = []interface{}{userID, pial}
		default:
			return fmt.Errorf("purge %s: %s: unknown statement scope %d", userID, s.why, s.scope)
		}
		if _, err := tx.Exec(s.stmt, args...); err != nil {
			return fmt.Errorf("purge %s: %s: %w", userID, s.why, err)
		}
	}

	// The account itself. Everything with a foreign key to users goes with it,
	// by the referential actions migration 0018 declared.
	res, err := tx.Exec(`DELETE FROM users WHERE id = $1::uuid`, userID)
	if err != nil {
		return fmt.Errorf("purge %s: deleting account: %w", userID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("purge %s: counting deleted account rows: %w", userID, err)
	}
	if n != 1 {
		return fmt.Errorf("purge %s: deleted %d account rows, wanted exactly 1", userID, n)
	}

	if pial.Valid {
		// Tombstone the identity root. It cannot be deleted while the append-only
		// ledger references it, so what is erased is its CONTENT: the key material,
		// the date of birth, the KYC tier and every age and role flag derived from
		// them. What is left is an opaque UUID marked purged.
		res, err := tx.Exec(`
			UPDATE pial_roots
			   SET public_key      = '',
			       state_hash      = '',
			       date_of_birth   = NULL,
			       dob_locked      = FALSE,
			       kyc_tier        = 'none',
			       kyc_verified_at = NULL,
			       age_verified    = FALSE,
			       is_adult        = FALSE,
			       is_minor        = FALSE,
			       role            = 'user',
			       status          = 'purged',
			       is_tombstoned   = TRUE,
			       deleted_at      = NOW()
			 WHERE pial_id = $1::uuid`, pial.String)
		if err != nil {
			return fmt.Errorf("purge %s: tombstoning identity root: %w", userID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("purge %s: counting tombstoned roots: %w", userID, err)
		}
		if n != 1 {
			return fmt.Errorf("purge %s: tombstoned %d identity roots, wanted exactly 1", userID, n)
		}

		// The purge is itself an identity-affecting event, so it is appended to the
		// ledger like every other one — with account_id already NULL, because there
		// is no longer an account to name. This is an INSERT. The ledger is only
		// ever appended to.
		if _, err := tx.Exec(`
			INSERT INTO pial_event_ledger (pial_id, account_id, event_type, payload, source)
			VALUES ($1::uuid, NULL, 'account_purged',
			        jsonb_build_object(
			            'account_id', $2::text,
			            'note',       'account hard-deleted; identity root tombstoned; ledger, content receipts, enforcement actions and legal-hold records retained'),
			        'admin')`, pial.String, userID); err != nil {
			return fmt.Errorf("purge %s: recording the purge in the ledger: %w", userID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("purge %s: commit: %w", userID, err)
	}
	return nil
}

// GetWorkByID fetches a single work by its UUID string. viewerID is used for reaction state.
// Deleted works (deleted_at IS NOT NULL) are excluded.
func GetWorkByID(db *sql.DB, workID, viewerID string) (*model.Work, error) {
	rows, err := db.Query(worksSelectSQL+`
		WHERE w.id = $1::uuid AND w.deleted_at IS NULL
		  AND `+workNotBlockedSQL+`
		  AND `+workLiveSQL+`
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
	EnrichWorksWithPolls(db, WorksWithQuoted([]*model.Work{w}), viewerID)

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

	// Follow state: does viewer follow each work's author? Resolved through the
	// one follow-state owner (FollowStateFor) — this used to be its own inline
	// query, which is how a work card and a profile header could disagree about
	// the same relationship.
	authorIDs := make([]string, 0, len(works))
	for _, w := range works {
		authorIDs = append(authorIDs, w.AuthorID)
	}
	followed, ferr := FollowStateFor(db, viewerID, authorIDs)
	if ferr != nil {
		log.Printf("works: EnrichWorksWithReactions follow state: %v", ferr)
		return
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

// QuoteRenderDepth is how many levels of a quote chain the render draws: the
// bordered quoted_work card, and the compact quoted_work_nested rail hanging off
// it. Nothing draws a third level, so nothing loads one.
const QuoteRenderDepth = 2

// GetWorkQuotesBefore lists the works that quote workID, newest first, created
// strictly before `before`.
//
// The mirror of GetWorkRepliesBefore over the other citation type: quotes are
// work_citations rows exactly as replies are, so the visibility rules are the
// same rules and are written the same way — the deleted, blocked-author and
// expired filters here, the viewer's gate in apiPrepareWorks above the call. A
// quote that a reader may not see must disappear from this list for the same
// reason it disappears from the feed, and the only way to be sure of that is
// for the two queries to differ in nothing but `citation_type`.
func GetWorkQuotesBefore(db *sql.DB, workID string, limit int, before time.Time) ([]*model.Work, error) {
	rows, err := db.Query(worksSelectSQL+`
		JOIN work_citations wc ON wc.work_id = w.id
		WHERE wc.target_id = $1::uuid
		  AND wc.citation_type = 'quote'
		  AND w.deleted_at IS NULL
		  AND w.created_at < $3
		  AND `+workNotBlockedSQL+`
		  AND `+workLiveSQL+`
		ORDER BY w.created_at DESC
		LIMIT $2
	`, workID, limit, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectWorks(rows)
}

// EnrichWorksWithQuotes attaches the quote chain each work sits on top of,
// bounded to QuoteRenderDepth.
//
// The bound is a predicate, not a walk. Every work_citations row carries the
// root of its chain and its distance from that root (materialized by the
// database, see migration 0003), so the whole visible chain for a page of works
// comes back in one query: join each seed edge to its own chain by root, keep
// the levels the render draws, stop. There is no recursion here and there is no
// depth counter in Go, because a chain that could exceed the bound cannot be
// stored in the first place — the insert trigger rejects cycles and caps depth.
//
// Two queries total: the edges, then the works those edges point at.
func EnrichWorksWithQuotes(db *sql.DB, works []*model.Work) {
	if len(works) == 0 {
		return
	}

	var seedIDs []string
	for _, w := range works {
		// react_video works also embed an original via a 'quote' citation.
		if w.Kind == "quote" || w.Kind == "react_video" {
			seedIDs = append(seedIDs, w.ID)
		}
	}
	if len(seedIDs) == 0 {
		return
	}

	// Step 1: every edge of every seed's chain, from the seed's own level down to
	// the deepest level the render draws.
	erows, err := db.Query(`
		SELECT DISTINCT e.work_id::text, e.target_id::text
		FROM work_citations seed
		JOIN work_citations e
		  ON e.root_id       = seed.root_id
		 AND e.citation_type = seed.citation_type
		 AND e.depth        <= seed.depth
		 AND e.depth         > seed.depth - $2::int
		WHERE seed.work_id       = ANY($1::uuid[])
		  AND seed.citation_type = 'quote'
	`, pq.Array(seedIDs), QuoteRenderDepth)
	if err != nil {
		log.Printf("works: EnrichWorksWithQuotes edges: %v", err)
		return
	}
	defer erows.Close()

	quotes := make(map[string]string, len(seedIDs)*QuoteRenderDepth)
	targetSet := make(map[string]struct{}, len(seedIDs)*QuoteRenderDepth)
	for erows.Next() {
		var quoter, target string
		if erows.Scan(&quoter, &target) != nil {
			continue
		}
		quotes[quoter] = target
		targetSet[target] = struct{}{}
	}
	if err := erows.Err(); err != nil {
		log.Printf("works: EnrichWorksWithQuotes edges: %v", err)
		return
	}
	if len(targetSet) == 0 {
		return
	}

	targetIDs := make([]string, 0, len(targetSet))
	for id := range targetSet {
		targetIDs = append(targetIDs, id)
	}

	// Step 2: the quoted works themselves.
	trows, err := db.Query(worksSelectSQL+`
		WHERE w.id = ANY($1::uuid[]) AND w.deleted_at IS NULL
		  AND `+workNotBlockedSQL+`
	`, pq.Array(targetIDs))
	if err != nil {
		log.Printf("works: EnrichWorksWithQuotes fetch: %v", err)
		return
	}
	defer trows.Close()

	quoted := make(map[string]*model.Work, len(targetIDs))
	for trows.Next() {
		if qw, err := scanWork(trows); err == nil {
			quoted[qw.ID] = qw
		}
	}

	// Step 3: hang each loaded work off whatever it quotes. Applied to the seeds
	// and to the works they quote, which is exactly the two levels the render
	// draws — the deepest loaded work quotes nothing in this set, so the chain
	// terminates on its own.
	attach := func(w *model.Work) {
		if target, ok := quotes[w.ID]; ok {
			w.QuotedWork = quoted[target]
		}
	}
	for _, w := range works {
		attach(w)
	}
	for _, qw := range quoted {
		attach(qw)
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

// WorksWithQuoted flattens a work set into every work the render will actually
// draw: the works themselves plus the quote chain hanging off each one, bounded
// to QuoteRenderDepth exactly as EnrichWorksWithQuotes loaded it.
//
// A quoted work is drawn by quoted_work / quoted_work_nested, so anything the
// draw needs has to be enriched onto it too — a quoted poll that is skipped here
// renders as a question with no ballot.
func WorksWithQuoted(works []*model.Work) []*model.Work {
	out := make([]*model.Work, 0, len(works))
	for _, w := range works {
		for depth, cur := 0, w; cur != nil && depth <= QuoteRenderDepth; depth, cur = depth+1, cur.QuotedWork {
			out = append(out, cur)
		}
	}
	return out
}

// EnrichWorksWithPolls attaches the rendered ballot to every work that carries
// poll options. Without it a poll work reaches the template as a body with no
// ballot, which is a poll that does not draw.
//
// Two queries for the whole page of works: the tallies, then the viewer's own
// ballots. There is no per-work read and no stored counter — work_poll_votes is
// the only place a vote exists, so a tally cannot drift from the ballots that
// produced it. Display state (percentages, winner, remaining time) is computed
// by model.Poll.Project, the single definition shared with the post-vote read.
func EnrichWorksWithPolls(db *sql.DB, works []*model.Work, viewerID string) {
	if len(works) == 0 {
		return
	}

	// Index every work carrying a ballot. A work id can appear more than once in
	// a page (an original and its repost surfacing), so index to a slice.
	index := map[string][]*model.Work{}
	var ids []string
	for _, w := range works {
		if w == nil || len(w.PollOptions) == 0 {
			continue
		}
		if _, seen := index[w.ID]; !seen {
			ids = append(ids, w.ID)
		}
		index[w.ID] = append(index[w.ID], w)
	}
	if len(ids) == 0 {
		return
	}

	polls := make(map[string]*model.Poll, len(ids))
	for id, ws := range index {
		polls[id] = &model.Poll{
			Options:  ws[0].PollOptions,
			Votes:    make([]int, len(ws[0].PollOptions)),
			UserVote: -1,
			EndsAt:   ws[0].PollEndsAt,
		}
	}

	tallies, err := db.Query(`
		SELECT work_id::text, option_idx, COUNT(*)::int
		FROM work_poll_votes
		WHERE work_id = ANY($1::uuid[])
		GROUP BY work_id, option_idx
	`, pq.Array(ids))
	if err != nil {
		log.Printf("works: EnrichWorksWithPolls tallies: %v", err)
		return
	}
	for tallies.Next() {
		var workID string
		var idx, count int
		if err := tallies.Scan(&workID, &idx, &count); err != nil {
			log.Printf("works: EnrichWorksWithPolls tally scan: %v", err)
			continue
		}
		if p, ok := polls[workID]; ok && idx >= 0 && idx < len(p.Votes) {
			p.Votes[idx] = count
		}
	}
	tallies.Close()

	if viewerID != "" {
		ballots, err := db.Query(`
			SELECT work_id::text, option_idx
			FROM work_poll_votes
			WHERE voter_id = $1::uuid AND work_id = ANY($2::uuid[])
		`, viewerID, pq.Array(ids))
		if err != nil {
			log.Printf("works: EnrichWorksWithPolls ballots: %v", err)
		} else {
			for ballots.Next() {
				var workID string
				var idx int
				if err := ballots.Scan(&workID, &idx); err != nil {
					log.Printf("works: EnrichWorksWithPolls ballot scan: %v", err)
					continue
				}
				if p, ok := polls[workID]; ok {
					p.UserVote = idx
				}
			}
			ballots.Close()
		}
	}

	now := time.Now()
	for id, p := range polls {
		p.Project(now)
		for _, w := range index[id] {
			w.Poll = p
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
		    WHERE w.deleted_at IS NULL AND ` + workNotBlockedSQL + ` AND NOT w.subscriber_only AND w.kind <> 'reply'
		      AND ` + workLiveSQL + `
		    UNION ALL
		    SELECT wr.work_id AS id, ru.handle AS reposter_handle,
		           COALESCE(NULLIF(TRIM(rp.display_name),''), ru.handle) AS reposter_name, wr.created_at AS feed_ts
		    FROM work_reactions wr
		    JOIN works w ON w.id = wr.work_id
		    JOIN users ru ON ru.id = wr.reactor_id
		    LEFT JOIN user_profiles rp ON rp.user_id = wr.reactor_id
		    WHERE wr.reaction_type = 'repost'
		      AND w.deleted_at IS NULL AND ` + workNotBlockedSQL + ` AND NOT w.subscriber_only AND w.kind <> 'reply'
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
		  AND `+workNotBlockedSQL+`
		  AND NOT w.subscriber_only
		  AND w.kind != 'reply'
		  AND `+workLiveSQL+`
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
		  AND `+workNotBlockedSQL+`
		  AND NOT w.subscriber_only
		  AND w.kind != 'reply'
		  AND `+workLiveSQL+`
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

// GetWorksBySurface returns the works that belong to a named interest surface,
// newest first.
//
// A surface is a server-side definition — a tag set and/or a content type, held
// in feed_surfaces and pinned per user in user_surface_pins. This is the query
// that makes that definition mean something: before it existed, every pinned
// surface fell through to the Following feed, so a user who pinned Sports, Art
// or Crypto got the same timeline under a different label and no error anywhere
// said so.
//
// Tag matching is case-insensitive on both sides. A surface is defined in
// lowercase and a work carries whatever case its author typed ("#NFL"), so a
// case-sensitive overlap would silently drop the works the surface exists for.
func GetWorksBySurface(db *sql.DB, tags []string, contentType string, limit int, before string) ([]*model.Work, error) {
	t, err := parseBefore(before)
	if err != nil {
		return nil, err
	}
	lowered := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag != "" {
			lowered = append(lowered, tag)
		}
	}
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	if len(lowered) == 0 && contentType == "" {
		// A surface that defines neither a tag set nor a content type selects
		// nothing. Returning the global feed instead would be a lie about what
		// the surface is.
		return nil, nil
	}
	rows, err := db.Query(worksSelectSQL+`
		WHERE w.deleted_at IS NULL
		  AND `+workNotBlockedSQL+`
		  AND NOT w.subscriber_only
		  AND w.kind != 'reply'
		  AND `+workLiveSQL+`
		  AND (w.scheduled_at IS NULL OR w.scheduled_at <= NOW())
		  AND (
		        EXISTS (SELECT 1 FROM unnest(COALESCE(w.tags,'{}'::text[])) tg
		                WHERE lower(tg) = ANY($1::text[]))
		     OR ($2 <> '' AND lower(COALESCE(w.content_type,'')) = $2)
		  )
		  AND w.created_at < $3
		ORDER BY w.created_at DESC
		LIMIT $4
	`, pq.Array(lowered), contentType, t, limit)
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
		  AND `+workNotBlockedSQL+`
		  AND `+workLiveSQL+`
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
		  AND w.deleted_at IS NULL AND ` + workNotBlockedSQL + ` AND NOT w.subscriber_only AND w.kind <> 'reply'
		  AND ` + workLiveSQL + `
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
		  AND `+workNotBlockedSQL+`
		  AND `+workLiveSQL+`
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
		  AND `+workNotBlockedSQL+`
		  AND w.kind != 'reply'
		  AND `+workLiveSQL+`
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
		  AND `+workNotBlockedSQL+`
		  AND `+workLiveSQL+`
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
		  AND `+workNotBlockedSQL+`
		  AND `+workLiveSQL+`
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
		  AND `+workNotBlockedSQL+`
		  AND `+workLiveSQL+`
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
		AND `+workNotBlockedSQL+`
		AND `+workLiveSQL+`
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
		AND `+workNotBlockedSQL+`
		AND `+workLiveSQL+`
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
		  AND `+workNotBlockedSQL+`
		  AND `+workLiveSQL+`
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
			  AND `+workNotBlockedSQL+`
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
		  AND `+workNotBlockedSQL+`
		  AND `+workLiveSQL+`
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

// ManhattanOutboxPending returns how many graph registrations are queued and not
// yet delivered to the naming plane. A number that only grows means the drain is
// not landing — surfaced on /health so it is noticed before the graph is stale.
func ManhattanOutboxPending(db *sql.DB) int {
	if db == nil {
		return 0
	}
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM manhattan_outbox WHERE delivered_at IS NULL`).Scan(&n); err != nil {
		return -1
	}
	return n
}

// ── Jung layer: work vectors ──────────────────────────────────────────────────
//
// The lean select below carries exactly the columns jung.MapWork reads plus the
// stored vector, so a caller that needs a work's psychological position — the
// reaction learner, the interest recomputation — does not pay for the full
// card select with its author join and reaction counts.

// workPsychSelectSQL is the projection jung.MapWork needs. Column order must
// match scanWorkPsych.
const workPsychSelectSQL = `
SELECT
    w.id, w.cid, w.author_id, w.body, w.kind, w.media_urls,
    COALESCE(w.tags, '{}'), COALESCE(w.content_type, 'text'),
    w.is_nsfw, w.is_gore, w.voice_url, w.video_master_url, w.poll_options,
    w.created_at, w.psych_vector
FROM works w
`

func scanWorkPsych(rows *sql.Rows) (*model.Work, error) {
	var w model.Work
	var mediaURLs, tags, pollOptions pq.StringArray
	var voiceURL, videoMasterURL sql.NullString
	var psychVector pq.Float32Array
	if err := rows.Scan(
		&w.ID, &w.CID, &w.AuthorID, &w.Body, &w.Kind, &mediaURLs,
		&tags, &w.ContentType,
		&w.IsNSFW, &w.IsGore, &voiceURL, &videoMasterURL, &pollOptions,
		&w.CreatedAt, &psychVector,
	); err != nil {
		return nil, err
	}
	w.MediaURLs = []string(mediaURLs)
	w.Tags = []string(tags)
	if len(pollOptions) > 0 {
		w.PollOptions = []string(pollOptions)
	}
	w.VoiceURL = voiceURL.String
	w.VideoMasterURL = videoMasterURL.String
	if len(psychVector) == jung.Dim {
		w.PsychVector = []float32(psychVector)
	}
	return &w, nil
}

func collectWorksPsych(rows *sql.Rows) ([]*model.Work, error) {
	defer rows.Close()
	var out []*model.Work
	for rows.Next() {
		w, err := scanWorkPsych(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// GetWorkForPsych loads the projection jung.MapWork needs for one work by id.
// A deleted work is still loadable here: a reaction to it is still evidence
// about the person who reacted.
func GetWorkForPsych(db *sql.DB, workID string) (*model.Work, error) {
	rows, err := db.Query(workPsychSelectSQL+`WHERE w.id = $1::uuid`, workID)
	if err != nil {
		return nil, err
	}
	works, err := collectWorksPsych(rows)
	if err != nil {
		return nil, err
	}
	if len(works) == 0 {
		return nil, sql.ErrNoRows
	}
	return works[0], nil
}

// GetWorkForPsychByCID is GetWorkForPsych addressed by content id, which is
// how a reply or quote names the work it cites.
func GetWorkForPsychByCID(db *sql.DB, cid string) (*model.Work, error) {
	rows, err := db.Query(workPsychSelectSQL+`WHERE w.cid = $1`, cid)
	if err != nil {
		return nil, err
	}
	works, err := collectWorksPsych(rows)
	if err != nil {
		return nil, err
	}
	if len(works) == 0 {
		return nil, sql.ErrNoRows
	}
	return works[0], nil
}

// GetEngagedWorksForPsych returns the works a person most recently reacted to
// positively (like, repost, bookmark), newest reaction first, in the psych
// projection. It is the evidence set the interest vector is recomputed from.
func GetEngagedWorksForPsych(db *sql.DB, userID string, limit int) ([]*model.Work, error) {
	rows, err := db.Query(`
		SELECT w.id, w.cid, w.author_id, w.body, w.kind, w.media_urls,
		       COALESCE(w.tags, '{}'), COALESCE(w.content_type, 'text'),
		       w.is_nsfw, w.is_gore, w.voice_url, w.video_master_url, w.poll_options,
		       w.created_at, w.psych_vector
		FROM (
		    SELECT work_id, MAX(created_at) AS reacted_at
		    FROM work_reactions
		    WHERE reactor_id = $1::uuid
		      AND reaction_type IN ('like','repost','bookmark')
		    GROUP BY work_id
		    ORDER BY reacted_at DESC
		    LIMIT $2
		) r
		JOIN works w ON w.id = r.work_id
		WHERE w.deleted_at IS NULL
		ORDER BY r.reacted_at DESC
	`, userID, limit)
	if err != nil {
		return nil, err
	}
	return collectWorksPsych(rows)
}

// SaveWorkPsychVector writes one work's vector.
func SaveWorkPsychVector(db *sql.DB, workID string, vec []float32) error {
	if len(vec) != jung.Dim {
		return fmt.Errorf("works: psych vector for %s has %d dims, want %d", workID, len(vec), jung.Dim)
	}
	_, err := db.Exec(
		`UPDATE works SET psych_vector = $1 WHERE id = $2::uuid AND psych_vector IS NULL`,
		pq.Float32Array(vec), workID,
	)
	return err
}

// SaveWorkPsychVectors writes vectors for a batch of works in one
// transaction and returns how many rows it filled. Only rows whose vector is
// still NULL are written: a vector that already exists — written by
// InsertWork, or by Zior for an audio work — is never overwritten by a lazy
// read-side computation, so the count is the number of works that were
// vector-less a moment ago and are not now.
func SaveWorkPsychVectors(db *sql.DB, vectors map[string][]float32) (int, error) {
	if len(vectors) == 0 {
		return 0, nil
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	stmt, err := tx.Prepare(`UPDATE works SET psych_vector = $1 WHERE id = $2::uuid AND psych_vector IS NULL`)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	written := 0
	for id, vec := range vectors {
		if len(vec) != jung.Dim {
			continue
		}
		res, err := stmt.Exec(pq.Float32Array(vec), id)
		if err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			written += int(n)
		}
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return written, nil
}

// SetWorkPsychVector overwrites one work's vector unconditionally. It is the
// write for a vector that is authoritative over whatever the column holds:
// Zior's analysis of a voice work replaces the text mapping InsertWork
// stored as the immediate fallback. Every other writer fills only NULLs.
func SetWorkPsychVector(db *sql.DB, workID string, vec []float32) error {
	if len(vec) != jung.Dim {
		return fmt.Errorf("works: psych vector for %s has %d dims, want %d", workID, len(vec), jung.Dim)
	}
	res, err := db.Exec(
		`UPDATE works SET psych_vector = $1 WHERE id = $2::uuid AND deleted_at IS NULL`,
		pq.Float32Array(vec), workID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// GetWorksMissingPsychVector returns up to limit live works whose vector
// column is still NULL, newest first — the backfill's work list. It walks
// the partial index migration 0020 built for exactly this predicate.
func GetWorksMissingPsychVector(db *sql.DB, limit int) ([]*model.Work, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := db.Query(
		workPsychSelectSQL+`WHERE w.psych_vector IS NULL AND w.deleted_at IS NULL ORDER BY w.created_at DESC LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	return collectWorksPsych(rows)
}

// SetTrackZiorSignal stores Zior's reading of a music track (migration 0021):
// its Jung vector, mood, descriptor and listening contexts. The track's own
// row carries them because a track is not a work — it lives in tracks, with
// its own audio metadata — and the vector must be addressable by the track
// id Zior stored the signal under.
func SetTrackZiorSignal(db *sql.DB, trackID string, vec []float32, mood, descriptor string, contextTags []string) error {
	if len(vec) != jung.Dim {
		return fmt.Errorf("tracks: psych vector for %s has %d dims, want %d", trackID, len(vec), jung.Dim)
	}
	if contextTags == nil {
		contextTags = []string{}
	}
	res, err := db.Exec(`
		UPDATE tracks
		SET psych_vector = $1, mood = $2, descriptor = $3, context_tags = $4, zior_analyzed_at = NOW()
		WHERE id = $5::uuid
	`, pq.Float32Array(vec), mood, descriptor, pq.Array(contextTags), trackID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
