package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Work is one row of works with everything a card needs joined in. It mirrors
// feed-engine's model.Work, field for field where the iOS contract reads one.
type Work struct {
	ID         string
	CID        string
	AuthorID   string
	AuthorPIAL string
	Body       string
	Kind       string
	MediaURLs  []string
	IsNSFW     bool
	IsGore     bool
	IsBlocked  bool
	ExpiresAt  *time.Time
	CreatedAt  time.Time

	// Author, joined at query time.
	Author *User

	// Citations.
	ParentCID  string
	QuotedCID  string
	QuotedWork *Work

	// Counts. Reactions and citations are counted from their rows; the rest
	// are the denormalised columns.
	LikeCount     int
	RepostCount   int
	BookmarkCount int
	DislikeCount  int
	ReplyCount    int
	QuoteCount    int
	ViewCount     int

	// Viewer state.
	LikedByViewer       bool
	DislikedByViewer    bool
	RepostedByViewer    bool
	BookmarkedByViewer  bool
	ViewerFollowsAuthor bool
	// PurchasedByViewer is the entitlement from work_purchases: the viewer
	// has bought this priced work outright.
	PurchasedByViewer bool

	// Repost surfacing — set when a feed row exists because someone reposted.
	RepostedBy *User
	RepostedAt *time.Time

	IsRepost       bool
	RepostSourceID string
	IsSensitive    bool
	SubscriberOnly bool
	CommentGating  string
	ScheduledAt    *time.Time
	Tags           []string
	ContentType    string

	PollOptions []string
	PollEndsAt  *time.Time
	Poll        *Poll

	VoiceURL          string
	VoiceDurationSecs float64

	VideoMasterURL      string
	VideoWatermarkedURL string
	VideoPosterURL      string
	VideoDurationSecs   float64
	VideoWidth          int
	VideoHeight         int
	ReactLayout         string

	LineageHandle      string
	LineageDisplayName string

	IsEdited  bool
	EditedAt  *time.Time
	IsPinned  bool
	ScanState string
	ScoreBand string
	PriceUAET int64

	LatestReplierHandles []string
	LatestReplierAvatars []string

	TipTotalUAET int64
}

// Poll is the projected ballot: every value the client draws, computed here.
// feed-engine's model.Poll.Project is the same arithmetic.
type Poll struct {
	Options    []string
	Votes      []int
	TotalVotes int
	UserVote   int // -1 when the viewer has not voted
	EndsAt     *time.Time
	Closed     bool
	TimeLeft   string
	Results    []PollResult
}

// PollResult is one option's display state.
type PollResult struct {
	Label    string
	Votes    int
	Pct      int
	Voted    bool
	IsWinner bool
}

// Project fills the derived fields from Options, Votes, UserVote and EndsAt.
func (p *Poll) Project(now time.Time) {
	if p == nil {
		return
	}
	if len(p.Votes) != len(p.Options) {
		votes := make([]int, len(p.Options))
		copy(votes, p.Votes)
		p.Votes = votes
	}
	p.TotalVotes = 0
	best := 0
	for _, v := range p.Votes {
		p.TotalVotes += v
		if v > best {
			best = v
		}
	}
	p.Closed = p.EndsAt != nil && !p.EndsAt.After(now)
	p.TimeLeft = pollTimeLeft(p.EndsAt, now)
	p.Results = make([]PollResult, len(p.Options))
	for i, opt := range p.Options {
		pct := 0
		if p.TotalVotes > 0 {
			pct = int((float64(p.Votes[i])*100.0)/float64(p.TotalVotes) + 0.5)
		}
		p.Results[i] = PollResult{
			Label:    opt,
			Votes:    p.Votes[i],
			Pct:      pct,
			Voted:    p.UserVote == i,
			IsWinner: p.TotalVotes > 0 && p.Votes[i] == best,
		}
	}
}

func pollTimeLeft(endsAt *time.Time, now time.Time) string {
	if endsAt == nil {
		return ""
	}
	d := endsAt.Sub(now)
	if d <= 0 {
		return "Final results"
	}
	switch {
	case d <= time.Minute:
		return "Less than a minute left"
	case d <= time.Hour:
		return plural(ceilUnits(d, time.Minute), "minute") + " left"
	case d <= 24*time.Hour:
		return plural(ceilUnits(d, time.Hour), "hour") + " left"
	default:
		return plural(ceilUnits(d, 24*time.Hour), "day") + " left"
	}
}

func ceilUnits(d, unit time.Duration) int { return int((d + unit - 1) / unit) }

func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// ── Select ────────────────────────────────────────────────────────────────────

// workLiveSQL and workNotBlockedSQL are feed-engine's read predicates.
const workLiveSQL = `(w.expires_at IS NULL OR w.expires_at > ?)`
const workNotBlockedSQL = `(w.is_blocked = 0 AND w.scan_state <> 'blocked')`
const workPublishedSQL = `(w.scheduled_at IS NULL OR w.scheduled_at <= ?)`

// worksSelectSQL is the shared SELECT. Callers append FROM-clause joins,
// WHERE, ORDER BY and LIMIT. Column order is bound to scanWork.
const worksSelectSQL = `
SELECT
    w.id, w.cid, w.author_id, w.author_pial, w.body, w.kind,
    w.media_urls, w.is_nsfw, w.is_gore, w.is_blocked, w.expires_at, w.created_at,
    (SELECT COUNT(*) FROM work_reactions wr WHERE wr.work_id = w.id AND wr.reaction_type='like'),
    (SELECT COUNT(*) FROM work_reactions wr WHERE wr.work_id = w.id AND wr.reaction_type='repost'),
    (SELECT COUNT(*) FROM work_reactions wr WHERE wr.work_id = w.id AND wr.reaction_type='bookmark'),
    (SELECT COUNT(*) FROM work_reactions wr WHERE wr.work_id = w.id AND wr.reaction_type='dislike'),
    w.is_repost, COALESCE(w.repost_source_id, ''), w.content_type, w.tags, w.comment_gating,
    w.subscriber_only, w.is_sensitive, w.scan_state, w.score_band,
    w.voice_url, w.voice_duration_secs,
    w.video_master_url, w.video_watermarked_url, w.video_poster_url, w.video_duration_secs,
    w.video_width, w.video_height, COALESCE(w.react_layout, ''),
    COALESCE(w.lineage_handle, ''),
    w.poll_options, w.poll_ends_at, w.scheduled_at,
    (SELECT COUNT(*) FROM work_citations wc JOIN works r ON r.id = wc.work_id
        WHERE wc.target_id = w.id AND wc.citation_type='reply' AND r.deleted_at IS NULL),
    (SELECT COUNT(*) FROM work_citations wc JOIN works r ON r.id = wc.work_id
        WHERE wc.target_id = w.id AND wc.citation_type='quote' AND r.deleted_at IS NULL),
    w.is_edited, w.edited_at, w.view_count, w.price_uaet,
    COALESCE((SELECT SUM(amount_uaet) FROM ledger_entries le WHERE le.work_id = w.id AND le.kind = 'tip_received'), 0)
FROM works w
`

func scanWork(rows *sql.Rows) (*Work, error) {
	var w Work
	var mediaURLs, tags, pollOptions sql.NullString
	var expiresAt, createdAt, pollEndsAt, scheduledAt, editedAt sql.NullString
	var isNSFW, isGore, isBlocked, isRepost, subscriberOnly, isSensitive, isEdited int
	var voiceURL, videoMaster, videoWatermarked, videoPoster sql.NullString
	var voiceDur, videoDur sql.NullFloat64
	var videoW, videoH sql.NullInt64

	err := rows.Scan(
		&w.ID, &w.CID, &w.AuthorID, &w.AuthorPIAL, &w.Body, &w.Kind,
		&mediaURLs, &isNSFW, &isGore, &isBlocked, &expiresAt, &createdAt,
		&w.LikeCount, &w.RepostCount, &w.BookmarkCount, &w.DislikeCount,
		&isRepost, &w.RepostSourceID, &w.ContentType, &tags, &w.CommentGating,
		&subscriberOnly, &isSensitive, &w.ScanState, &w.ScoreBand,
		&voiceURL, &voiceDur,
		&videoMaster, &videoWatermarked, &videoPoster, &videoDur,
		&videoW, &videoH, &w.ReactLayout,
		&w.LineageHandle,
		&pollOptions, &pollEndsAt, &scheduledAt,
		&w.ReplyCount, &w.QuoteCount,
		&isEdited, &editedAt, &w.ViewCount, &w.PriceUAET,
		&w.TipTotalUAET,
	)
	if err != nil {
		return nil, err
	}
	w.MediaURLs = decodeStrings(mediaURLs)
	w.Tags = decodeStrings(tags)
	if pollOptions.Valid {
		w.PollOptions = decodeStrings(pollOptions)
	}
	w.IsNSFW, w.IsGore, w.IsBlocked = isNSFW == 1, isGore == 1, isBlocked == 1
	w.IsRepost, w.SubscriberOnly, w.IsSensitive, w.IsEdited = isRepost == 1, subscriberOnly == 1, isSensitive == 1, isEdited == 1
	w.CreatedAt = ParseTime(createdAt.String)
	w.ExpiresAt = timePtr(expiresAt)
	w.PollEndsAt = timePtr(pollEndsAt)
	w.ScheduledAt = timePtr(scheduledAt)
	w.EditedAt = timePtr(editedAt)
	w.VoiceURL = voiceURL.String
	w.VoiceDurationSecs = voiceDur.Float64
	w.VideoMasterURL = videoMaster.String
	w.VideoWatermarkedURL = videoWatermarked.String
	w.VideoPosterURL = videoPoster.String
	w.VideoDurationSecs = videoDur.Float64
	w.VideoWidth = int(videoW.Int64)
	w.VideoHeight = int(videoH.Int64)
	return &w, nil
}

func timePtr(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	t := ParseTime(s.String)
	if t.IsZero() {
		return nil
	}
	return &t
}

func collectWorks(rows *sql.Rows) ([]*Work, error) {
	defer rows.Close()
	var out []*Work
	for rows.Next() {
		w, err := scanWork(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// cursorOrNow turns an opaque page cursor into the `before` bound. Empty means
// "start from now". A cursor that does not parse is treated as malformed.
func cursorOrNow(cursor string) (string, error) {
	if cursor == "" {
		return FormatTime(time.Now().Add(time.Second)), nil
	}
	if ParseTime(cursor).IsZero() {
		return "", fmt.Errorf("bad cursor %q", cursor)
	}
	return cursor, nil
}

// ── Insert ────────────────────────────────────────────────────────────────────

// InsertWorkParams is the admitted, verified work. It matches the argument
// list of feed-engine's InsertWork.
type InsertWorkParams struct {
	AuthorID, AuthorPIAL, CID, Body, Kind string
	MediaURLs                             []string
	ParentCID, QuotedCID                  string
	Tags                                  []string
	PollOptions                           []string
	PollEndsAt                            *time.Time
	ScheduledAt                           *time.Time
	CommentGating                         string
	SubscriberOnly                        bool
	IsNSFW                                bool
	IsRepost                              bool
	RepostSourceID                        string
	VoiceURL                              string
	VoiceDurationSecs                     *int
	VideoMasterURL                        string
	VideoWatermarkedURL                   string
	VideoPosterURL                        string
	VideoDurationSecs                     *int
	VideoWidth, VideoHeight               int
	ReactLayout                           string
	// PriceUAET sells the work outright at this price in µAET. Zero is not
	// for sale. Not part of the signed payload today — the seeder sets it;
	// a client price would have to enter the canonical payload first.
	PriceUAET int64
	// CreatedAt overrides the clock; the seeder uses it. Zero means now.
	CreatedAt time.Time
}

// ErrDuplicateCID is returned when a work with the same CID already exists.
// feed-engine surfaces this as a bare 500; naming it lets the handler answer
// the retry case honestly.
var ErrDuplicateCID = errors.New("duplicate cid")

// InsertWork stores a work, its citations, its first edition, and bumps the
// author's post count and XP. Returns the new id.
func (s *Store) InsertWork(ctx context.Context, p InsertWorkParams) (string, error) {
	if p.CommentGating == "" {
		p.CommentGating = "open"
	}
	if p.Kind == "" {
		p.Kind = "post"
	}
	created := p.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	contentType := contentTypeFor(p)
	id := NewID()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO works (
				id, cid, author_id, author_pial, body, kind, media_urls,
				tags, poll_options, poll_ends_at, scheduled_at,
				comment_gating, subscriber_only, is_nsfw, is_repost, repost_source_id,
				voice_url, voice_duration_secs,
				video_master_url, video_watermarked_url, video_poster_url, video_duration_secs,
				video_width, video_height, react_layout, content_type, created_at, scan_state, price_uaet
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'clean', ?)`,
			id, p.CID, p.AuthorID, p.AuthorPIAL, p.Body, p.Kind, encodeStrings(p.MediaURLs),
			encodeStrings(p.Tags), pollOptionsColumn(p.PollOptions), nullTime(p.PollEndsAt), nullTime(p.ScheduledAt),
			p.CommentGating, boolInt(p.SubscriberOnly), boolInt(p.IsNSFW), boolInt(p.IsRepost), nullable(p.RepostSourceID),
			nullable(p.VoiceURL), nullInt(p.VoiceDurationSecs),
			nullable(p.VideoMasterURL), nullable(p.VideoWatermarkedURL), nullable(p.VideoPosterURL), nullInt(p.VideoDurationSecs),
			nullIfZero(p.VideoWidth), nullIfZero(p.VideoHeight), nullable(p.ReactLayout), contentType, FormatTime(created), p.PriceUAET)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed: works.cid") {
				return ErrDuplicateCID
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO editions (id, work_id, cid, body, edition_number, created_at) VALUES (?, ?, ?, ?, 1, ?)`,
			NewID(), id, p.CID, p.Body, FormatTime(created)); err != nil {
			return err
		}
		for _, cit := range []struct{ cid, kind string }{{p.ParentCID, "reply"}, {p.QuotedCID, "quote"}} {
			if cit.cid == "" {
				continue
			}
			var targetID string
			err := tx.QueryRowContext(ctx, `SELECT id FROM works WHERE cid = ?`, cit.cid).Scan(&targetID)
			if err != nil {
				// Parent not found: the citation is skipped, as feed-engine does.
				continue
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT OR IGNORE INTO work_citations (id, work_id, target_id, citation_type, created_at) VALUES (?, ?, ?, ?, ?)`,
				NewID(), id, targetID, cit.kind, FormatTime(created)); err != nil {
				return err
			}
		}
		if p.Kind != "reply" {
			if _, err := tx.ExecContext(ctx, `UPDATE user_profiles SET post_count = post_count + 1 WHERE user_id = ?`, p.AuthorID); err != nil {
				return err
			}
		}
		xp := XPPost
		if p.Kind == "reply" {
			xp = XPReply
		}
		_, err = tx.ExecContext(ctx, `UPDATE pial_roots SET xp = xp + ? WHERE pial_id = ?`, xp, p.AuthorPIAL)
		return err
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

func nullIfZero(i int) any {
	if i == 0 {
		return nil
	}
	return i
}

func pollOptionsColumn(opts []string) any {
	if opts == nil {
		return nil
	}
	return encodeStrings(opts)
}

// contentTypeFor derives works.content_type from what the work carries, so
// the Music surface (content_type = audio) and the media tabs can select on it.
func contentTypeFor(p InsertWorkParams) string {
	switch {
	case p.VideoMasterURL != "" || p.Kind == "video" || p.Kind == "react_video":
		return "video"
	case p.VoiceURL != "" || p.Kind == "voice":
		return "audio"
	case p.PollOptions != nil || p.Kind == "poll":
		return "poll"
	case len(p.MediaURLs) > 0:
		return "image"
	default:
		return "text"
	}
}

// ── Reads ─────────────────────────────────────────────────────────────────────

// GetWorkByID loads one live work with citations, quotes, poll and viewer
// state hydrated. ErrNotFound when it does not exist, is deleted, blocked or
// expired.
func (s *Store) GetWorkByID(ctx context.Context, id, viewerID string) (*Work, error) {
	now := Now()
	rows, err := s.db.QueryContext(ctx, worksSelectSQL+`
		WHERE w.id = ? AND w.deleted_at IS NULL AND `+workNotBlockedSQL+` AND `+workLiveSQL, id, now)
	if err != nil {
		return nil, err
	}
	works, err := collectWorks(rows)
	if err != nil {
		return nil, err
	}
	if len(works) == 0 {
		return nil, ErrNotFound
	}
	if err := s.Hydrate(ctx, works, viewerID); err != nil {
		return nil, err
	}
	return works[0], nil
}

// GetWorkByCID resolves a CID to its live work.
func (s *Store) GetWorkByCID(ctx context.Context, cid, viewerID string) (*Work, error) {
	var id string
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM works WHERE cid = ? AND deleted_at IS NULL`, cid).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return s.GetWorkByID(ctx, id, viewerID)
}

// WorkOwner returns the author id of a live work, for ownership checks.
func (s *Store) WorkOwner(ctx context.Context, id string) (authorID, authorPIAL string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT author_id, author_pial FROM works WHERE id = ? AND deleted_at IS NULL`, id).Scan(&authorID, &authorPIAL)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return
}

// feedRow is one surfacing of a work in a feed: the work, and if it is there
// because of a repost, who reposted and when.
type feedRow struct {
	id         string
	reposterID string
	feedTS     string
}

// feedIDQuery is the UNION shape both Following and For-you use: originals
// plus repost surfacings, each work once at its newest surfacing, ordered by
// the time it hit the feed. The `%s` holes are the two author scopes. Named
// parameters throughout, because the same value (the viewer, the clock) is
// read in several places and positional binding would need it repeated.
const feedIDQuery = `
SELECT id, reposter_id, feed_ts FROM (
  SELECT id, reposter_id, feed_ts, ROW_NUMBER() OVER (PARTITION BY id ORDER BY feed_ts DESC) AS rn FROM (
    SELECT w.id AS id, NULL AS reposter_id, w.created_at AS feed_ts
    FROM works w
    WHERE %s
      AND w.deleted_at IS NULL AND (w.is_blocked = 0 AND w.scan_state <> 'blocked') AND w.kind <> 'reply'
      AND (w.expires_at IS NULL OR w.expires_at > :now)
      AND (w.scheduled_at IS NULL OR w.scheduled_at <= :now)
      AND (w.subscriber_only = 0 OR w.author_id = :viewer
           OR EXISTS (SELECT 1 FROM subscriptions sb WHERE sb.subscriber_id = :viewer AND sb.creator_id = w.author_id AND sb.status = 'active'))
      AND NOT EXISTS (SELECT 1 FROM user_mutes m WHERE m.muter_id = :viewer AND m.muted_id = w.author_id)
      AND NOT EXISTS (SELECT 1 FROM blocks b WHERE (b.blocker_id = :viewer AND b.blocked_id = w.author_id) OR (b.blocker_id = w.author_id AND b.blocked_id = :viewer))
    UNION ALL
    SELECT wr.work_id AS id, wr.reactor_id AS reposter_id, wr.created_at AS feed_ts
    FROM work_reactions wr
    JOIN works w ON w.id = wr.work_id
    WHERE wr.reaction_type = 'repost'
      AND %s
      AND w.deleted_at IS NULL AND (w.is_blocked = 0 AND w.scan_state <> 'blocked') AND w.kind <> 'reply'
      AND (w.expires_at IS NULL OR w.expires_at > :now)
      AND (w.subscriber_only = 0 OR w.author_id = :viewer
           OR EXISTS (SELECT 1 FROM subscriptions sb WHERE sb.subscriber_id = :viewer AND sb.creator_id = w.author_id AND sb.status = 'active'))
      AND NOT EXISTS (SELECT 1 FROM user_mutes m WHERE m.muter_id = :viewer AND m.muted_id = w.author_id)
      AND NOT EXISTS (SELECT 1 FROM blocks b WHERE (b.blocker_id = :viewer AND b.blocked_id = w.author_id) OR (b.blocker_id = w.author_id AND b.blocked_id = :viewer))
  ) feed
  WHERE feed_ts < :before
) d
WHERE rn = 1
ORDER BY feed_ts DESC
LIMIT :limit`

// FeedFollowing is the viewer's own works plus the works and reposts of the
// accounts they follow, newest surfacing first.
func (s *Store) FeedFollowing(ctx context.Context, viewerID string, limit int, cursor string) ([]*Work, string, error) {
	scopeW := `(w.author_id = :viewer OR w.author_id IN (SELECT following_id FROM follows WHERE follower_id = :viewer))`
	scopeR := `(wr.reactor_id = :viewer OR wr.reactor_id IN (SELECT following_id FROM follows WHERE follower_id = :viewer))`
	return s.feedWithReposts(ctx, fmt.Sprintf(feedIDQuery, scopeW, scopeR), viewerID, limit, cursor)
}

// FeedForYou is every public work and repost, newest surfacing first — the
// global discovery feed.
func (s *Store) FeedForYou(ctx context.Context, viewerID string, limit int, cursor string) ([]*Work, string, error) {
	scopeW := `w.subscriber_only = 0`
	scopeR := `w.subscriber_only = 0`
	return s.feedWithReposts(ctx, fmt.Sprintf(feedIDQuery, scopeW, scopeR), viewerID, limit, cursor)
}

func (s *Store) feedWithReposts(ctx context.Context, idQuery, viewerID string, limit int, cursor string) ([]*Work, string, error) {
	before, err := cursorOrNow(cursor)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.db.QueryContext(ctx, idQuery,
		sql.Named("viewer", viewerID),
		sql.Named("before", before),
		sql.Named("limit", limit+1),
		sql.Named("now", Now()),
	)
	if err != nil {
		return nil, "", err
	}
	var feed []feedRow
	for rows.Next() {
		var r feedRow
		var reposter sql.NullString
		if err := rows.Scan(&r.id, &reposter, &r.feedTS); err != nil {
			rows.Close()
			return nil, "", err
		}
		r.reposterID = reposter.String
		feed = append(feed, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(feed) > limit {
		feed = feed[:limit]
		next = feed[len(feed)-1].feedTS
	}
	if len(feed) == 0 {
		return []*Work{}, "", nil
	}
	ids := make([]string, len(feed))
	for i, r := range feed {
		ids[i] = r.id
	}
	byID, err := s.worksByID(ctx, ids)
	if err != nil {
		return nil, "", err
	}
	reposterIDs := []string{}
	for _, r := range feed {
		if r.reposterID != "" {
			reposterIDs = append(reposterIDs, r.reposterID)
		}
	}
	reposters, err := s.GetUsersByID(ctx, reposterIDs)
	if err != nil {
		return nil, "", err
	}
	out := make([]*Work, 0, len(feed))
	for _, r := range feed {
		w, ok := byID[r.id]
		if !ok {
			continue
		}
		if r.reposterID != "" {
			// Copy: the same work could in principle surface twice on a page
			// boundary, and the attribution belongs to this surfacing.
			c := *w
			c.RepostedBy = reposters[r.reposterID]
			ts := ParseTime(r.feedTS)
			c.RepostedAt = &ts
			w = &c
		}
		out = append(out, w)
	}
	if err := s.Hydrate(ctx, out, viewerID); err != nil {
		return nil, "", err
	}
	return out, next, nil
}

func (s *Store) worksByID(ctx context.Context, ids []string) (map[string]*Work, error) {
	q, args := inClause(ids)
	rows, err := s.db.QueryContext(ctx, worksSelectSQL+` WHERE w.id IN (`+q+`)`, args...)
	if err != nil {
		return nil, err
	}
	works, err := collectWorks(rows)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*Work, len(works))
	for _, w := range works {
		out[w.ID] = w
	}
	return out, nil
}

// simpleFeed runs worksSelectSQL with a WHERE that already excludes deleted,
// blocked, unpublished and expired rows, pages by created_at, and hydrates.
func (s *Store) simpleFeed(ctx context.Context, viewerID, where string, args []any, limit int, cursor string) ([]*Work, string, error) {
	before, err := cursorOrNow(cursor)
	if err != nil {
		return nil, "", err
	}
	args = append(args, before, limit+1)
	rows, err := s.db.QueryContext(ctx, worksSelectSQL+where+` AND w.created_at < ? ORDER BY w.created_at DESC LIMIT ?`, args...)
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
		next = FormatTime(works[len(works)-1].CreatedAt)
	}
	if works == nil {
		works = []*Work{}
	}
	if err := s.Hydrate(ctx, works, viewerID); err != nil {
		return nil, "", err
	}
	return works, next, nil
}

// baseWhere is the predicate every public list shares.
func baseWhere(now string) (string, []any) {
	return ` WHERE w.deleted_at IS NULL AND ` + workNotBlockedSQL + ` AND ` + workLiveSQL + ` AND ` + workPublishedSQL,
		[]any{now, now}
}

// FeedTrending: public, non-reply works ordered by recent engagement.
//
// feed-engine's GetWorksTrending is newest-first today; here the rows are
// ranked by reactions and replies in the last 48 hours with recency as the
// tiebreak, so the lane is visibly different from For you on a small dataset.
// Both are the server's call and neither leaks anything about how.
func (s *Store) FeedTrending(ctx context.Context, viewerID string, limit int, cursor string) ([]*Work, string, error) {
	now := Now()
	since := FormatTime(time.Now().Add(-48 * time.Hour))
	before, err := cursorOrNow(cursor)
	if err != nil {
		return nil, "", err
	}
	// Trending pages by rank, so the cursor is an offset encoded as a number.
	offset := 0
	if cursor != "" {
		fmt.Sscanf(cursor, "offset:%d", &offset)
		before = FormatTime(time.Now().Add(time.Second))
	}
	rows, err := s.db.QueryContext(ctx, worksSelectSQL+`
		WHERE w.deleted_at IS NULL AND `+workNotBlockedSQL+` AND `+workLiveSQL+` AND `+workPublishedSQL+`
		  AND w.subscriber_only = 0 AND w.kind <> 'reply' AND w.created_at < ?
		ORDER BY
		  ((SELECT COUNT(*) FROM work_reactions wr WHERE wr.work_id = w.id AND wr.created_at > ?)
		   + 2 * (SELECT COUNT(*) FROM work_citations wc WHERE wc.target_id = w.id AND wc.created_at > ?)) DESC,
		  w.created_at DESC
		LIMIT ? OFFSET ?`, now, now, before, since, since, limit+1, offset)
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
		next = fmt.Sprintf("offset:%d", offset+limit)
	}
	if works == nil {
		works = []*Work{}
	}
	if err := s.Hydrate(ctx, works, viewerID); err != nil {
		return nil, "", err
	}
	return works, next, nil
}

// FeedBySurface selects works whose tags overlap a tag set or whose content
// type matches — feed-engine's GetWorksBySurface, which is what the Music
// lane is defined as.
func (s *Store) FeedBySurface(ctx context.Context, viewerID string, tags []string, contentType string, limit int, cursor string) ([]*Work, string, error) {
	now := Now()
	where, args := baseWhere(now)
	where += ` AND w.subscriber_only = 0 AND w.kind <> 'reply' AND (`
	clauses := []string{}
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		clauses = append(clauses, `EXISTS (SELECT 1 FROM json_each(w.tags) tg WHERE lower(tg.value) = ?)`)
		args = append(args, t)
	}
	if contentType != "" {
		clauses = append(clauses, `lower(w.content_type) = ?`)
		args = append(args, strings.ToLower(contentType))
	}
	if len(clauses) == 0 {
		return []*Work{}, "", nil
	}
	where += strings.Join(clauses, " OR ") + `)`
	return s.simpleFeed(ctx, viewerID, where, args, limit, cursor)
}

// FeedVisions is media-rich works from the viewer and the accounts they
// follow, newest first — feed-engine's GetVisionsWorksFeed.
func (s *Store) FeedVisions(ctx context.Context, viewerID string, limit int, cursor string) ([]*Work, string, error) {
	now := Now()
	where, args := baseWhere(now)
	where += ` AND (json_array_length(w.media_urls) > 0 OR w.video_master_url IS NOT NULL)
	           AND (w.author_id = ? OR w.author_id IN (SELECT following_id FROM follows WHERE follower_id = ?))`
	args = append(args, viewerID, viewerID)
	return s.simpleFeed(ctx, viewerID, where, args, limit, cursor)
}

// ProfileTab is one tab of a profile's works.
type ProfileTab string

const (
	TabWorks   ProfileTab = "works"
	TabReplies ProfileTab = "replies"
	TabMedia   ProfileTab = "media"
	TabLikes   ProfileTab = "likes"
)

// ProfileWorks lists one tab of an author's works.
func (s *Store) ProfileWorks(ctx context.Context, authorID, viewerID string, tab ProfileTab, limit int, cursor string) ([]*Work, string, error) {
	now := Now()
	where, args := baseWhere(now)
	switch tab {
	case TabReplies:
		where += ` AND w.author_id = ? AND w.kind = 'reply'`
		args = append(args, authorID)
	case TabMedia:
		where += ` AND w.author_id = ? AND (json_array_length(w.media_urls) > 0 OR w.video_master_url IS NOT NULL)`
		args = append(args, authorID)
	case TabLikes:
		// Ordered by when the like happened, which is what "newest like first"
		// means; the cursor is therefore the reaction time.
		return s.likedWorks(ctx, authorID, viewerID, limit, cursor)
	default:
		where += ` AND w.author_id = ? AND w.kind <> 'reply'`
		args = append(args, authorID)
	}
	return s.simpleFeed(ctx, viewerID, where, args, limit, cursor)
}

func (s *Store) likedWorks(ctx context.Context, userID, viewerID string, limit int, cursor string) ([]*Work, string, error) {
	before, err := cursorOrNow(cursor)
	if err != nil {
		return nil, "", err
	}
	now := Now()
	rows, err := s.db.QueryContext(ctx, worksSelectSQL+`
		JOIN work_reactions lk ON lk.work_id = w.id AND lk.reactor_id = ? AND lk.reaction_type = 'like'
		WHERE w.deleted_at IS NULL AND `+workNotBlockedSQL+` AND `+workLiveSQL+`
		  AND lk.created_at < ?
		ORDER BY lk.created_at DESC LIMIT ?`, userID, now, before, limit+1)
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
		// The reaction time is not on the row; look it up for the cursor.
		s.db.QueryRowContext(ctx, `SELECT created_at FROM work_reactions WHERE work_id = ? AND reactor_id = ? AND reaction_type = 'like'`,
			works[len(works)-1].ID, userID).Scan(&next)
	}
	if works == nil {
		works = []*Work{}
	}
	if err := s.Hydrate(ctx, works, viewerID); err != nil {
		return nil, "", err
	}
	return works, next, nil
}

// Replies lists the direct replies to a work, oldest first — a conversation
// reads top to bottom.
func (s *Store) Replies(ctx context.Context, workID, viewerID string, limit int, cursor string) ([]*Work, string, error) {
	after := ""
	if cursor != "" {
		if ParseTime(cursor).IsZero() {
			return nil, "", fmt.Errorf("bad cursor %q", cursor)
		}
		after = cursor
	}
	now := Now()
	rows, err := s.db.QueryContext(ctx, worksSelectSQL+`
		JOIN work_citations wc ON wc.work_id = w.id AND wc.citation_type = 'reply' AND wc.target_id = ?
		WHERE w.deleted_at IS NULL AND `+workNotBlockedSQL+` AND `+workLiveSQL+`
		  AND w.created_at > ?
		  AND NOT EXISTS (SELECT 1 FROM blocks b WHERE (b.blocker_id = ? AND b.blocked_id = w.author_id) OR (b.blocker_id = w.author_id AND b.blocked_id = ?))
		ORDER BY w.created_at ASC LIMIT ?`, workID, now, after, viewerID, viewerID, limit+1)
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
		next = FormatTime(works[len(works)-1].CreatedAt)
	}
	if works == nil {
		works = []*Work{}
	}
	if err := s.Hydrate(ctx, works, viewerID); err != nil {
		return nil, "", err
	}
	return works, next, nil
}

// Quotes lists the works that quote one work, newest first.
//
// The mirror of Replies over the other citation type, and deliberately the
// same query but for `citation_type` and the sort: a quote a reader may not
// see has to fall out of this list for the same reasons it falls out of a
// feed, and the only way to be sure of that is for the two to differ in
// nothing else. Newest first rather than oldest, because this is a list of
// reactions to a work and not a conversation to read top to bottom.
func (s *Store) Quotes(ctx context.Context, workID, viewerID string, limit int, cursor string) ([]*Work, string, error) {
	before := FormatTime(time.Now().Add(365 * 24 * time.Hour))
	if cursor != "" {
		if ParseTime(cursor).IsZero() {
			return nil, "", fmt.Errorf("bad cursor %q", cursor)
		}
		before = cursor
	}
	now := Now()
	rows, err := s.db.QueryContext(ctx, worksSelectSQL+`
		JOIN work_citations wc ON wc.work_id = w.id AND wc.citation_type = 'quote' AND wc.target_id = ?
		WHERE w.deleted_at IS NULL AND `+workNotBlockedSQL+` AND `+workLiveSQL+`
		  AND w.created_at < ?
		  AND NOT EXISTS (SELECT 1 FROM blocks b WHERE (b.blocker_id = ? AND b.blocked_id = w.author_id) OR (b.blocker_id = w.author_id AND b.blocked_id = ?))
		ORDER BY w.created_at DESC LIMIT ?`, workID, now, before, viewerID, viewerID, limit+1)
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
		next = FormatTime(works[len(works)-1].CreatedAt)
	}
	if works == nil {
		works = []*Work{}
	}
	if err := s.Hydrate(ctx, works, viewerID); err != nil {
		return nil, "", err
	}
	return works, next, nil
}

// ParentChain walks up the reply citations from a work, root first, at most
// maxDepth levels.
func (s *Store) ParentChain(ctx context.Context, workID, viewerID string, maxDepth int) ([]*Work, error) {
	var chain []*Work
	current := workID
	for range maxDepth {
		var parentID string
		err := s.db.QueryRowContext(ctx, `SELECT target_id FROM work_citations WHERE work_id = ? AND citation_type = 'reply'`, current).Scan(&parentID)
		if err != nil {
			break
		}
		parent, err := s.GetWorkByID(ctx, parentID, viewerID)
		if err != nil {
			break
		}
		chain = append([]*Work{parent}, chain...)
		current = parentID
	}
	return chain, nil
}

// ── Hydration ─────────────────────────────────────────────────────────────────

// Hydrate fills everything a page of works needs beyond its own row: authors,
// citations, viewer reaction and follow state, quoted works, polls, repliers,
// pinned state. It is the whole Enrich* chain from feed-engine, called once
// per page so no handler can forget a step.
func (s *Store) Hydrate(ctx context.Context, works []*Work, viewerID string) error {
	if len(works) == 0 {
		return nil
	}
	if err := s.hydrateAuthors(ctx, works); err != nil {
		return err
	}
	if err := s.hydrateCitations(ctx, works); err != nil {
		return err
	}
	if err := s.hydrateQuotes(ctx, works, viewerID); err != nil {
		return err
	}
	if err := s.hydrateReactions(ctx, works, viewerID); err != nil {
		return err
	}
	if err := s.hydratePolls(ctx, works, viewerID); err != nil {
		return err
	}
	if err := s.hydrateRepliers(ctx, works); err != nil {
		return err
	}
	if err := s.hydratePurchases(ctx, works, viewerID); err != nil {
		return err
	}
	return s.hydratePinned(ctx, works)
}

// hydratePurchases marks the priced works the viewer has bought. Only priced
// rows are asked about — an unpriced work has nothing to own.
func (s *Store) hydratePurchases(ctx context.Context, works []*Work, viewerID string) error {
	if viewerID == "" {
		return nil
	}
	priced := []*Work{}
	for _, w := range works {
		if w.PriceUAET > 0 {
			priced = append(priced, w)
		}
	}
	if len(priced) == 0 {
		return nil
	}
	ids, index := indexWorks(priced)
	q, args := inClause(ids)
	rows, err := s.db.QueryContext(ctx, `
		SELECT work_id FROM work_purchases WHERE buyer_id = ? AND work_id IN (`+q+`)`,
		append([]any{viewerID}, args...)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var workID string
		if err := rows.Scan(&workID); err != nil {
			return err
		}
		for _, w := range index[workID] {
			w.PurchasedByViewer = true
		}
	}
	return rows.Err()
}

func (s *Store) hydrateAuthors(ctx context.Context, works []*Work) error {
	ids := []string{}
	seen := map[string]bool{}
	for _, w := range works {
		if !seen[w.AuthorID] {
			seen[w.AuthorID] = true
			ids = append(ids, w.AuthorID)
		}
	}
	users, err := s.GetUsersByID(ctx, ids)
	if err != nil {
		return err
	}
	for _, w := range works {
		w.Author = users[w.AuthorID]
	}
	return nil
}

func (s *Store) hydrateCitations(ctx context.Context, works []*Work) error {
	ids, index := indexWorks(works)
	q, args := inClause(ids)
	rows, err := s.db.QueryContext(ctx, `
		SELECT wc.work_id, wc.citation_type, t.cid
		FROM work_citations wc JOIN works t ON t.id = wc.target_id
		WHERE wc.work_id IN (`+q+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var workID, kind, cid string
		if err := rows.Scan(&workID, &kind, &cid); err != nil {
			return err
		}
		for _, w := range index[workID] {
			switch kind {
			case "reply":
				w.ParentCID = cid
			case "quote":
				w.QuotedCID = cid
			}
		}
	}
	return rows.Err()
}

// QuoteRenderDepth is how many levels of a quote chain the render draws: the
// bordered quoted card, and the compact nested rail hanging off it. Nothing
// draws a third level, so nothing loads one. feed-engine's constant of the
// same name is the definition; this mirrors it so the dev server hands the app
// the same chain production does.
const QuoteRenderDepth = 2

// hydrateQuotes attaches the quote chain each work sits on top of, bounded to
// QuoteRenderDepth.
//
// A round per level, and each round is the previous round's targets: level one
// is what these works quote, level two is what *those* quote. It stops at the
// bound rather than at the end of the chain, because the bound is what the
// render draws — a level nobody draws is a query nobody should pay for. A
// quote whose target is deleted simply has no quoted work, which is the same
// answer the card wants: no card.
func (s *Store) hydrateQuotes(ctx context.Context, works []*Work, viewerID string) error {
	level := works
	for range QuoteRenderDepth {
		cids := []string{}
		seen := map[string]bool{}
		for _, w := range level {
			if w.QuotedCID != "" && !seen[w.QuotedCID] {
				seen[w.QuotedCID] = true
				cids = append(cids, w.QuotedCID)
			}
		}
		if len(cids) == 0 {
			return nil
		}
		q, args := inClause(cids)
		rows, err := s.db.QueryContext(ctx, worksSelectSQL+` WHERE w.cid IN (`+q+`) AND w.deleted_at IS NULL`, args...)
		if err != nil {
			return err
		}
		quoted, err := collectWorks(rows)
		if err != nil {
			return err
		}
		if len(quoted) == 0 {
			return nil
		}
		if err := s.hydrateAuthors(ctx, quoted); err != nil {
			return err
		}
		// The next round needs to know what these quote, and the card needs
		// their media and poll the same way the level above needs its own.
		if err := s.hydrateCitations(ctx, quoted); err != nil {
			return err
		}
		byCID := map[string]*Work{}
		for _, qw := range quoted {
			byCID[qw.CID] = qw
		}
		for _, w := range level {
			if w.QuotedCID != "" {
				w.QuotedWork = byCID[w.QuotedCID]
			}
		}
		level = quoted
	}
	return nil
}

func (s *Store) hydrateReactions(ctx context.Context, works []*Work, viewerID string) error {
	if viewerID == "" {
		return nil
	}
	ids, index := indexWorks(works)
	q, args := inClause(ids)
	rows, err := s.db.QueryContext(ctx, `
		SELECT work_id, reaction_type FROM work_reactions WHERE reactor_id = ? AND work_id IN (`+q+`)`,
		append([]any{viewerID}, args...)...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var workID, kind string
		if err := rows.Scan(&workID, &kind); err != nil {
			rows.Close()
			return err
		}
		for _, w := range index[workID] {
			switch kind {
			case "like":
				w.LikedByViewer = true
			case "repost":
				w.RepostedByViewer = true
			case "bookmark":
				w.BookmarkedByViewer = true
			case "dislike":
				w.DislikedByViewer = true
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	authorIDs := []string{}
	for _, w := range works {
		authorIDs = append(authorIDs, w.AuthorID)
	}
	followed, err := s.FollowStateFor(ctx, viewerID, authorIDs)
	if err != nil {
		return err
	}
	for _, w := range works {
		w.ViewerFollowsAuthor = followed[w.AuthorID]
	}
	return nil
}

func (s *Store) hydratePolls(ctx context.Context, works []*Work, viewerID string) error {
	polls := map[string]*Poll{}
	ids := []string{}
	for _, w := range works {
		if len(w.PollOptions) == 0 {
			continue
		}
		if _, ok := polls[w.ID]; !ok {
			ids = append(ids, w.ID)
			polls[w.ID] = &Poll{Options: w.PollOptions, Votes: make([]int, len(w.PollOptions)), UserVote: -1, EndsAt: w.PollEndsAt}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	q, args := inClause(ids)
	rows, err := s.db.QueryContext(ctx, `
		SELECT work_id, option_idx, COUNT(*) FROM work_poll_votes WHERE work_id IN (`+q+`) GROUP BY work_id, option_idx`, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var workID string
		var idx, n int
		if err := rows.Scan(&workID, &idx, &n); err != nil {
			rows.Close()
			return err
		}
		if p := polls[workID]; p != nil && idx >= 0 && idx < len(p.Votes) {
			p.Votes[idx] = n
		}
	}
	rows.Close()
	if viewerID != "" {
		rows, err := s.db.QueryContext(ctx, `
			SELECT work_id, option_idx FROM work_poll_votes WHERE voter_id = ? AND work_id IN (`+q+`)`,
			append([]any{viewerID}, args...)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var workID string
			var idx int
			if err := rows.Scan(&workID, &idx); err != nil {
				rows.Close()
				return err
			}
			if p := polls[workID]; p != nil {
				p.UserVote = idx
			}
		}
		rows.Close()
	}
	now := time.Now()
	for _, w := range works {
		if p := polls[w.ID]; p != nil {
			p.Project(now)
			w.Poll = p
		}
	}
	return nil
}

func (s *Store) hydrateRepliers(ctx context.Context, works []*Work) error {
	ids, index := indexWorks(works)
	q, args := inClause(ids)
	rows, err := s.db.QueryContext(ctx, `
		SELECT wc.target_id, u.handle, p.avatar_url
		FROM work_citations wc
		JOIN works r ON r.id = wc.work_id
		JOIN users u ON u.id = r.author_id
		JOIN user_profiles p ON p.user_id = u.id
		WHERE wc.target_id IN (`+q+`) AND wc.citation_type = 'reply' AND r.deleted_at IS NULL
		ORDER BY r.created_at DESC`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var targetID, handle, avatar string
		if err := rows.Scan(&targetID, &handle, &avatar); err != nil {
			return err
		}
		for _, w := range index[targetID] {
			if len(w.LatestReplierHandles) >= 3 {
				continue
			}
			for _, h := range w.LatestReplierHandles {
				if h == handle {
					handle = ""
				}
			}
			if handle == "" {
				break
			}
			w.LatestReplierHandles = append(w.LatestReplierHandles, handle)
			w.LatestReplierAvatars = append(w.LatestReplierAvatars, avatar)
		}
	}
	return rows.Err()
}

func (s *Store) hydratePinned(ctx context.Context, works []*Work) error {
	for _, w := range works {
		if w.Author != nil && w.Author.PinnedWorkID == w.ID {
			w.IsPinned = true
		}
	}
	return nil
}

func indexWorks(works []*Work) ([]string, map[string][]*Work) {
	index := map[string][]*Work{}
	ids := []string{}
	for _, w := range works {
		if _, seen := index[w.ID]; !seen {
			ids = append(ids, w.ID)
		}
		index[w.ID] = append(index[w.ID], w)
	}
	return ids, index
}

// ── Mutations ─────────────────────────────────────────────────────────────────

// React adds or removes a reaction. Idempotent both ways; returns whether the
// row changed so callers notify only on a real change.
func (s *Store) React(ctx context.Context, workID, reactorID, reactionType string, add bool) (bool, error) {
	if add {
		res, err := s.db.ExecContext(ctx, `
			INSERT OR IGNORE INTO work_reactions (work_id, reactor_id, reaction_type, created_at) VALUES (?, ?, ?, ?)`,
			workID, reactorID, reactionType, Now())
		if err != nil {
			return false, err
		}
		n, _ := res.RowsAffected()
		return n > 0, nil
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM work_reactions WHERE work_id = ? AND reactor_id = ? AND reaction_type = ?`,
		workID, reactorID, reactionType)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SoftDeleteWork marks a work deleted if the caller owns it. ErrNotFound when
// there is no such live work owned by authorID.
func (s *Store) SoftDeleteWork(ctx context.Context, workID, authorID string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE works SET deleted_at = ? WHERE id = ? AND author_id = ? AND deleted_at IS NULL`, Now(), workID, authorID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE user_profiles SET post_count = (SELECT COUNT(*) FROM works WHERE author_id = ? AND deleted_at IS NULL AND kind <> 'reply')
			WHERE user_id = ?`, authorID, authorID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE user_profiles SET pinned_work_id = NULL WHERE user_id = ? AND pinned_work_id = ?`, authorID, workID)
		return err
	})
}

// SetCommentGating changes who may reply. Stored values follow feed-engine:
// the payload vocabulary is everyone|followers|circle|none and the column
// keeps `open` for everyone.
func (s *Store) SetCommentGating(ctx context.Context, workID, gating string) error {
	if gating == "everyone" {
		gating = "open"
	}
	_, err := s.db.ExecContext(ctx, `UPDATE works SET comment_gating = ? WHERE id = ?`, gating, workID)
	return err
}

// CastPollVote records one ballot per voter. A second vote replaces the first
// while the poll is open; a closed poll refuses.
func (s *Store) CastPollVote(ctx context.Context, workID, voterID string, optionIdx int) error {
	var optionsRaw sql.NullString
	var endsAt sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT poll_options, poll_ends_at FROM works WHERE id = ? AND deleted_at IS NULL`, workID).Scan(&optionsRaw, &endsAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	options := decodeStrings(optionsRaw)
	if len(options) == 0 {
		return errors.New("not a poll")
	}
	if optionIdx < 0 || optionIdx >= len(options) {
		return errors.New("option out of range")
	}
	if end := timePtr(endsAt); end != nil && !end.After(time.Now()) {
		return errors.New("poll closed")
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO work_poll_votes (work_id, voter_id, option_idx, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (work_id, voter_id) DO UPDATE SET option_idx = excluded.option_idx, created_at = excluded.created_at`,
		workID, voterID, optionIdx, Now())
	return err
}

// RecordView bumps the denormalised view counter.
func (s *Store) RecordView(ctx context.Context, workID string) {
	s.db.ExecContext(ctx, `UPDATE works SET view_count = view_count + 1 WHERE id = ?`, workID)
}

// InsertReport files a report.
func (s *Store) InsertReport(ctx context.Context, reporterID, workID, targetUser, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO reports (id, reporter_id, work_id, target_user, reason, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		NewID(), reporterID, nullable(workID), nullable(targetUser), reason, Now())
	return err
}

// FindWorkByVideoMaster returns the oldest live work whose video is the
// rendition at masterURL — the original, when the same bytes are uploaded
// again. ErrNotFound when no work carries it.
func (s *Store) FindWorkByVideoMaster(ctx context.Context, masterURL string) (*Work, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM works WHERE video_master_url = ? AND deleted_at IS NULL ORDER BY created_at ASC LIMIT 1`,
		masterURL).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return s.GetWorkByID(ctx, id, "")
}
