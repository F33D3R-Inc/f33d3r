package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Visions are 24-hour ephemeral posts: feed-engine migration 0008, with two
// columns the iOS composer needs and the web's does not yet — audience and
// allow_replies — and a poll the viewer can answer.

// Server-owned artboard presets. A text vision carries KEYS; every client
// resolves them the same way, so a vision looks the same to everyone.
var (
	VisionBackgrounds = map[string]bool{"void": true, "ember": true, "tide": true, "bloom": true, "pulp": true, "signal": true}
	VisionTypefaces   = map[string]bool{"grotesk": true, "serif": true, "mono": true, "display": true}
	VisionTypeScales  = map[string]bool{"auto": true, "s": true, "m": true, "l": true, "xl": true}
	VisionAligns      = map[string]bool{"left": true, "center": true, "right": true}
)

// DefaultVisionTTL is feed-engine's: a day.
const DefaultVisionTTL = 24 * time.Hour

// VisionBodyMax is the rune cap the web composer enforces.
const VisionBodyMax = 280

type Vision struct {
	ID          string
	AuthorID    string
	AuthorPIAL  string
	Author      *User
	ContentType string
	Body        string

	ArtboardBackground string
	ArtboardTypeface   string
	ArtboardTypeScale  string
	ArtboardAlign      string

	MediaURLs    []string
	SharedWorkID string
	IsNSFW       bool
	Audience     string
	AllowReplies bool
	PollOptions  []string
	Poll         *Poll
	CreatedAt    time.Time
	ExpiresAt    time.Time

	ViewedByViewer bool
	ViewCount      int
}

// VisionRing is one author's live vision set as the viewer sees it — the circle
// in the tray, with its visions inline so the viewer opens without a round trip.
type VisionRing struct {
	Author      *User
	State       string // none | unseen | seen
	Count       int
	UnseenCount int
	LatestAt    time.Time
	IsLive      bool
	Visions     []*Vision
}

type VisionInput struct {
	AuthorID, AuthorPIAL string
	ContentType          string
	Body                 string
	Background, Typeface string
	TypeScale, Align     string
	MediaURLs            []string
	IsNSFW               bool
	Audience             string
	AllowReplies         bool
	PollOptions          []string
	TTL                  time.Duration
	Source               string
	CreatedAt            time.Time
}

func (in *VisionInput) applyDefaults() error {
	if in.ContentType == "" {
		in.ContentType = "text"
	}
	if in.ContentType != "text" && in.ContentType != "image" {
		return fmt.Errorf("unknown content_type %q", in.ContentType)
	}
	if in.Background == "" {
		in.Background = "void"
	}
	if !VisionBackgrounds[in.Background] {
		return fmt.Errorf("unknown artboard_background %q", in.Background)
	}
	if in.Typeface == "" {
		in.Typeface = "grotesk"
	}
	if !VisionTypefaces[in.Typeface] {
		return fmt.Errorf("unknown artboard_typeface %q", in.Typeface)
	}
	if in.TypeScale == "" {
		in.TypeScale = "auto"
	}
	if !VisionTypeScales[in.TypeScale] {
		return fmt.Errorf("unknown artboard_type_scale %q", in.TypeScale)
	}
	if in.Align == "" {
		in.Align = "center"
	}
	if !VisionAligns[in.Align] {
		return fmt.Errorf("unknown artboard_align %q", in.Align)
	}
	if in.Audience == "" {
		in.Audience = "everyone"
	}
	if in.Audience != "everyone" && in.Audience != "subscribers" {
		return fmt.Errorf("unknown audience %q", in.Audience)
	}
	if in.TTL <= 0 {
		in.TTL = DefaultVisionTTL
	}
	if in.TTL > 7*24*time.Hour {
		return errors.New("a vision lives at most seven days")
	}
	if in.MediaURLs == nil {
		in.MediaURLs = []string{}
	}
	if in.ContentType == "text" && in.Body == "" && in.PollOptions == nil {
		return errors.New("a text vision needs words")
	}
	if in.ContentType == "image" && len(in.MediaURLs) == 0 {
		return errors.New("an image vision needs an image")
	}
	if in.PollOptions != nil && (len(in.PollOptions) < 2 || len(in.PollOptions) > 4) {
		return errors.New("a poll has two to four options")
	}
	if in.Source == "" {
		in.Source = "composer"
	}
	return nil
}

// InsertVision stores a vision and returns its id.
func (s *Store) InsertVision(ctx context.Context, in VisionInput) (string, error) {
	if err := in.applyDefaults(); err != nil {
		return "", err
	}
	created := in.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	id := NewID()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO visions (id, author_id, author_pial, content_type, body, artboard_background, artboard_typeface,
		                    artboard_type_scale, artboard_align, media_urls, is_nsfw, audience, allow_replies, poll_options,
		                    created_at, expires_at, metadata)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, in.AuthorID, in.AuthorPIAL, in.ContentType, in.Body, in.Background, in.Typeface,
		in.TypeScale, in.Align, encodeStrings(in.MediaURLs), boolInt(in.IsNSFW), in.Audience, boolInt(in.AllowReplies),
		pollOptionsColumn(in.PollOptions), FormatTime(created), FormatTime(created.Add(in.TTL)),
		fmt.Sprintf(`{"source":%q}`, in.Source))
	if err != nil {
		return "", err
	}
	return id, nil
}

const visionSelectSQL = `
SELECT f.id, f.author_id, f.author_pial, f.content_type, f.body,
       f.artboard_background, f.artboard_typeface, f.artboard_type_scale, f.artboard_align,
       f.media_urls, COALESCE(f.shared_work_id, ''), f.is_nsfw, f.audience, f.allow_replies, f.poll_options,
       f.created_at, f.expires_at,
       (SELECT COUNT(*) FROM vision_views v WHERE v.vision_id = f.id)
FROM visions f
`

func scanVision(rows *sql.Rows) (*Vision, error) {
	var f Vision
	var media, poll sql.NullString
	var created, expires string
	var nsfw, allow int
	if err := rows.Scan(&f.ID, &f.AuthorID, &f.AuthorPIAL, &f.ContentType, &f.Body,
		&f.ArtboardBackground, &f.ArtboardTypeface, &f.ArtboardTypeScale, &f.ArtboardAlign,
		&media, &f.SharedWorkID, &nsfw, &f.Audience, &allow, &poll,
		&created, &expires, &f.ViewCount); err != nil {
		return nil, err
	}
	f.MediaURLs = decodeStrings(media)
	if poll.Valid {
		f.PollOptions = decodeStrings(poll)
	}
	f.IsNSFW = nsfw == 1
	f.AllowReplies = allow == 1
	f.CreatedAt = ParseTime(created)
	f.ExpiresAt = ParseTime(expires)
	return &f, nil
}

// visionLiveSQL: not deleted, not expired, not blocked.
const visionLiveSQL = `f.deleted_at IS NULL AND f.is_blocked = 0 AND f.expires_at > ?`

// VisionTray is what the tray draws for a viewer: their own ring first (nil
// when they have no live vision), then the accounts they follow with a live
// vision, unseen first, newest first. Nobody suggested, nobody muted.
func (s *Store) VisionTray(ctx context.Context, viewer *User) (own *VisionRing, rings []*VisionRing, err error) {
	now := Now()
	rows, err := s.db.QueryContext(ctx, visionSelectSQL+`
		WHERE `+visionLiveSQL+`
		  AND (f.author_id = ?
		       OR f.author_id IN (SELECT following_id FROM follows WHERE follower_id = ?))
		  AND f.author_id NOT IN (SELECT muted_id FROM vision_mutes WHERE muter_id = ?)
		  AND f.author_id NOT IN (SELECT muted_id FROM user_mutes WHERE muter_id = ?)
		  AND (f.audience = 'everyone' OR f.author_id = ?
		       OR EXISTS (SELECT 1 FROM subscriptions sb WHERE sb.subscriber_id = ? AND sb.creator_id = f.author_id AND sb.status = 'active'))
		ORDER BY f.author_id, f.created_at ASC`,
		now, viewer.ID, viewer.ID, viewer.ID, viewer.ID, viewer.ID, viewer.ID)
	if err != nil {
		return nil, nil, err
	}
	var visions []*Vision
	for rows.Next() {
		f, err := scanVision(rows)
		if err != nil {
			rows.Close()
			return nil, nil, err
		}
		visions = append(visions, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if err := s.hydrateVisions(ctx, visions, viewer); err != nil {
		return nil, nil, err
	}

	byAuthor := map[string]*VisionRing{}
	order := []string{}
	for _, f := range visions {
		r, ok := byAuthor[f.AuthorID]
		if !ok {
			r = &VisionRing{Author: f.Author}
			byAuthor[f.AuthorID] = r
			order = append(order, f.AuthorID)
		}
		r.Visions = append(r.Visions, f)
		r.Count++
		if !f.ViewedByViewer {
			r.UnseenCount++
		}
		if f.CreatedAt.After(r.LatestAt) {
			r.LatestAt = f.CreatedAt
		}
	}
	live, _ := s.liveAuthorIDs(ctx)
	for _, id := range order {
		r := byAuthor[id]
		r.State = "seen"
		if r.UnseenCount > 0 {
			r.State = "unseen"
		}
		r.IsLive = live[id]
		if id == viewer.ID {
			own = r
			continue
		}
		rings = append(rings, r)
	}
	// Unseen before seen; within each, newest first.
	sortRings(rings)
	if rings == nil {
		rings = []*VisionRing{}
	}
	return own, rings, nil
}

func sortRings(rings []*VisionRing) {
	for i := 1; i < len(rings); i++ {
		for j := i; j > 0 && ringBefore(rings[j], rings[j-1]); j-- {
			rings[j], rings[j-1] = rings[j-1], rings[j]
		}
	}
}

func ringBefore(a, b *VisionRing) bool {
	if a.IsLive != b.IsLive {
		return a.IsLive
	}
	if (a.UnseenCount > 0) != (b.UnseenCount > 0) {
		return a.UnseenCount > 0
	}
	return a.LatestAt.After(b.LatestAt)
}

func (s *Store) hydrateVisions(ctx context.Context, visions []*Vision, viewer *User) error {
	if len(visions) == 0 {
		return nil
	}
	ids := []string{}
	authorIDs := map[string]bool{}
	index := map[string]*Vision{}
	for _, f := range visions {
		ids = append(ids, f.ID)
		authorIDs[f.AuthorID] = true
		index[f.ID] = f
	}
	authorList := make([]string, 0, len(authorIDs))
	for id := range authorIDs {
		authorList = append(authorList, id)
	}
	users, err := s.GetUsersByID(ctx, authorList)
	if err != nil {
		return err
	}
	for _, f := range visions {
		f.Author = users[f.AuthorID]
	}
	q, args := inClause(ids)
	rows, err := s.db.QueryContext(ctx, `SELECT vision_id FROM vision_views WHERE viewer_pial = ? AND vision_id IN (`+q+`)`,
		append([]any{viewer.PIALID}, args...)...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		rows.Scan(&id)
		if f := index[id]; f != nil {
			f.ViewedByViewer = true
		}
	}
	rows.Close()

	// Polls.
	polls := map[string]*Poll{}
	pollIDs := []string{}
	for _, f := range visions {
		if len(f.PollOptions) > 0 {
			polls[f.ID] = &Poll{Options: f.PollOptions, Votes: make([]int, len(f.PollOptions)), UserVote: -1, EndsAt: &f.ExpiresAt}
			pollIDs = append(pollIDs, f.ID)
		}
	}
	if len(pollIDs) > 0 {
		pq, pargs := inClause(pollIDs)
		rows, err := s.db.QueryContext(ctx, `SELECT vision_id, option_idx, COUNT(*) FROM vision_poll_votes WHERE vision_id IN (`+pq+`) GROUP BY vision_id, option_idx`, pargs...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			var idx, n int
			rows.Scan(&id, &idx, &n)
			if p := polls[id]; p != nil && idx >= 0 && idx < len(p.Votes) {
				p.Votes[idx] = n
			}
		}
		rows.Close()
		rows, err = s.db.QueryContext(ctx, `SELECT vision_id, option_idx FROM vision_poll_votes WHERE voter_id = ? AND vision_id IN (`+pq+`)`,
			append([]any{viewer.ID}, pargs...)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			var idx int
			rows.Scan(&id, &idx)
			if p := polls[id]; p != nil {
				p.UserVote = idx
			}
		}
		rows.Close()
		now := time.Now()
		for id, p := range polls {
			p.Project(now)
			index[id].Poll = p
		}
	}
	return nil
}

// GetVision loads one live vision for a viewer.
func (s *Store) GetVision(ctx context.Context, id string, viewer *User) (*Vision, error) {
	rows, err := s.db.QueryContext(ctx, visionSelectSQL+` WHERE f.id = ? AND `+visionLiveSQL, id, Now())
	if err != nil {
		return nil, err
	}
	var visions []*Vision
	for rows.Next() {
		f, err := scanVision(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		visions = append(visions, f)
	}
	rows.Close()
	if len(visions) == 0 {
		return nil, ErrNotFound
	}
	if err := s.hydrateVisions(ctx, visions, viewer); err != nil {
		return nil, err
	}
	return visions[0], nil
}

// MarkVisionViewed records a view. Idempotent; the author's own views are not
// counted.
func (s *Store) MarkVisionViewed(ctx context.Context, visionID string, viewer *User) error {
	var authorID string
	if err := s.db.QueryRowContext(ctx, `SELECT author_id FROM visions WHERE id = ? AND deleted_at IS NULL`, visionID).Scan(&authorID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if authorID == viewer.ID {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO vision_views (vision_id, viewer_pial, viewed_at) VALUES (?, ?, ?)`,
		visionID, viewer.PIALID, Now())
	return err
}

// VisionViewers lists who has seen a vision, newest first. Owner-only at the API.
func (s *Store) VisionViewers(ctx context.Context, visionID string, limit int) ([]*User, []time.Time, error) {
	rows, err := s.db.QueryContext(ctx, userSelectSQL+`
		JOIN vision_views fv ON fv.viewer_pial = u.pial_id
		WHERE fv.vision_id = ? ORDER BY fv.viewed_at DESC LIMIT ?`, visionID, limit)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var users []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, nil, err
		}
		users = append(users, u)
	}
	return users, nil, rows.Err()
}

// SoftDeleteVision removes the caller's own vision.
func (s *Store) SoftDeleteVision(ctx context.Context, visionID, authorID string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE visions SET deleted_at = ? WHERE id = ? AND author_id = ? AND deleted_at IS NULL`, Now(), visionID, authorID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CastVisionPollVote records one ballot per voter while the vision is live.
func (s *Store) CastVisionPollVote(ctx context.Context, visionID, voterID string, idx int) error {
	var optionsRaw sql.NullString
	var expires string
	err := s.db.QueryRowContext(ctx, `SELECT poll_options, expires_at FROM visions WHERE id = ? AND deleted_at IS NULL`, visionID).Scan(&optionsRaw, &expires)
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
	if idx < 0 || idx >= len(options) {
		return errors.New("option out of range")
	}
	if !ParseTime(expires).After(time.Now()) {
		return errors.New("this vision has expired")
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO vision_poll_votes (vision_id, voter_id, option_idx, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (vision_id, voter_id) DO UPDATE SET option_idx = excluded.option_idx, created_at = excluded.created_at`,
		visionID, voterID, idx, Now())
	return err
}

// ReplyToVision stores a private reply and returns the author to notify.
func (s *Store) ReplyToVision(ctx context.Context, visionID string, from *User, body string) (*Vision, error) {
	f, err := s.GetVision(ctx, visionID, from)
	if err != nil {
		return nil, err
	}
	if !f.AllowReplies {
		return nil, errors.New("replies are off on this vision")
	}
	if f.AuthorID == from.ID {
		return nil, errors.New("cannot reply to your own vision")
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO vision_replies (id, vision_id, from_id, to_id, body, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		NewID(), visionID, from.ID, f.AuthorID, body, Now())
	if err != nil {
		return nil, err
	}
	return f, nil
}

// SetVisionMuted hides one creator's visions from the tray.
func (s *Store) SetVisionMuted(ctx context.Context, muterID, mutedID string, muted bool) error {
	if muted {
		_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO vision_mutes (muter_id, muted_id, created_at) VALUES (?, ?, ?)`, muterID, mutedID, Now())
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM vision_mutes WHERE muter_id = ? AND muted_id = ?`, muterID, mutedID)
	return err
}
