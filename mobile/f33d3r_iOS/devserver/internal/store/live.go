package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Live streams: feed-engine migration 0009's table with the go-live sheet's
// choices (audience, lane, tip goal, notify, replay) added as columns.
//
// Video itself is not here. Production ingests over WHIP to a media server and
// serves HLS; this server keeps the room — who is live, who is watching, what
// was said, what was tipped — and says so honestly where the video would be.

var ErrLiveNotFound = errors.New("stream not found")
var ErrAlreadyLive = errors.New("you already have an open broadcast")

type LiveStream struct {
	ID          string
	AuthorID    string
	AuthorPIAL  string
	Author      *User
	Title       string
	Description string
	Status      string
	StartedAt   *time.Time
	EndedAt     *time.Time
	ViewerCount int
	PeakViewers int
	IsNSFW      bool
	Audience    string
	Lane        string
	TipGoalUAET int64
	Notify      bool
	SaveReplay  bool
	PinnedBody  string
	HeartCount  int
	CreatedAt   time.Time

	// Derived for the room view.
	TipTotalUAET int64
	TopTippers   []TipperTotal
}

type TipperTotal struct {
	User       *User
	AmountUAET int64
}

type LiveChatMessage struct {
	ID         string
	StreamID   string
	User       *User
	Kind       string // chat | tip | system
	Body       string
	AmountUAET int64
	CreatedAt  time.Time
}

type LiveInput struct {
	AuthorID, AuthorPIAL string
	Title, Description   string
	Audience, Lane       string
	TipGoalUAET          int64
	Notify, SaveReplay   bool
	IsNSFW               bool
}

// StartLive opens a room and marks it live at once — the app has no separate
// ingest handshake to wait for.
func (s *Store) StartLive(ctx context.Context, in LiveInput) (*LiveStream, error) {
	if in.Title == "" {
		return nil, errors.New("give the broadcast a title so people know what they are joining")
	}
	if len([]rune(in.Title)) > 120 {
		in.Title = string([]rune(in.Title)[:120])
	}
	if in.Audience == "" {
		in.Audience = "everyone"
	}
	if in.Audience != "everyone" && in.Audience != "subscribers" {
		return nil, errors.New("unknown audience")
	}
	var open int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM live_streams WHERE author_id = ? AND status IN ('idle','live')`, in.AuthorID).Scan(&open)
	if open > 0 {
		return nil, ErrAlreadyLive
	}
	now := Now()
	id := NewID()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO live_streams (id, author_id, author_pial, title, description, status, started_at, is_nsfw, audience, lane,
		                          tip_goal_uaet, notify_followers, save_replay, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'live', ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, in.AuthorID, in.AuthorPIAL, in.Title, in.Description, now, boolInt(in.IsNSFW), in.Audience, in.Lane,
		in.TipGoalUAET, boolInt(in.Notify), boolInt(in.SaveReplay), now, now)
	if err != nil {
		return nil, err
	}
	return s.GetLive(ctx, id)
}

const liveSelectSQL = `
SELECT l.id, l.author_id, l.author_pial, l.title, l.description, l.status, l.started_at, l.ended_at,
       l.viewer_count, l.peak_viewers, l.is_nsfw, l.audience, l.lane, l.tip_goal_uaet, l.notify_followers, l.save_replay,
       l.pinned_body, l.heart_count, l.created_at,
       COALESCE((SELECT SUM(amount_uaet) FROM ledger_entries le WHERE le.stream_id = l.id AND le.kind = 'tip_received'), 0)
FROM live_streams l
`

func scanLive(rows interface{ Scan(...any) error }) (*LiveStream, error) {
	var l LiveStream
	var started, ended sql.NullString
	var created string
	var nsfw, notify, replay int
	if err := rows.Scan(&l.ID, &l.AuthorID, &l.AuthorPIAL, &l.Title, &l.Description, &l.Status, &started, &ended,
		&l.ViewerCount, &l.PeakViewers, &nsfw, &l.Audience, &l.Lane, &l.TipGoalUAET, &notify, &replay,
		&l.PinnedBody, &l.HeartCount, &created, &l.TipTotalUAET); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrLiveNotFound
		}
		return nil, err
	}
	l.StartedAt = timePtr(started)
	l.EndedAt = timePtr(ended)
	l.IsNSFW = nsfw == 1
	l.Notify = notify == 1
	l.SaveReplay = replay == 1
	l.CreatedAt = ParseTime(created)
	return &l, nil
}

// GetLive loads one room with its author and tip totals.
func (s *Store) GetLive(ctx context.Context, id string) (*LiveStream, error) {
	l, err := scanLive(s.db.QueryRowContext(ctx, liveSelectSQL+` WHERE l.id = ?`, id))
	if err != nil {
		return nil, err
	}
	if err := s.hydrateLive(ctx, []*LiveStream{l}); err != nil {
		return nil, err
	}
	return l, nil
}

// ActiveLive lists rooms that are live now, newest first.
func (s *Store) ActiveLive(ctx context.Context, viewer *User) ([]*LiveStream, error) {
	rows, err := s.db.QueryContext(ctx, liveSelectSQL+`
		WHERE l.status = 'live' AND l.is_blocked = 0
		  AND (l.audience = 'everyone' OR l.author_id = ?
		       OR EXISTS (SELECT 1 FROM subscriptions sb WHERE sb.subscriber_id = ? AND sb.creator_id = l.author_id AND sb.status = 'active'))
		  AND NOT EXISTS (SELECT 1 FROM blocks b WHERE (b.blocker_id = ? AND b.blocked_id = l.author_id) OR (b.blocker_id = l.author_id AND b.blocked_id = ?))
		ORDER BY l.started_at DESC`, viewer.ID, viewer.ID, viewer.ID, viewer.ID)
	if err != nil {
		return nil, err
	}
	var out []*LiveStream
	for rows.Next() {
		l, err := scanLive(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, l)
	}
	rows.Close()
	if out == nil {
		out = []*LiveStream{}
	}
	return out, s.hydrateLive(ctx, out)
}

// LiveCount is the number the Live lane's badge shows.
func (s *Store) LiveCount(ctx context.Context) int {
	var n int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM live_streams WHERE status = 'live' AND is_blocked = 0`).Scan(&n)
	return n
}

// liveAuthorIDs: which accounts are broadcasting right now (for the red ring).
func (s *Store) liveAuthorIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT author_id FROM live_streams WHERE status = 'live' AND is_blocked = 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		rows.Scan(&id)
		out[id] = true
	}
	return out, nil
}

// OpenLiveFor returns the caller's open room, if any.
func (s *Store) OpenLiveFor(ctx context.Context, authorID string) (*LiveStream, error) {
	l, err := scanLive(s.db.QueryRowContext(ctx, liveSelectSQL+` WHERE l.author_id = ? AND l.status = 'live'`, authorID))
	if err != nil {
		return nil, err
	}
	return l, s.hydrateLive(ctx, []*LiveStream{l})
}

func (s *Store) hydrateLive(ctx context.Context, streams []*LiveStream) error {
	if len(streams) == 0 {
		return nil
	}
	ids := []string{}
	for _, l := range streams {
		ids = append(ids, l.AuthorID)
	}
	users, err := s.GetUsersByID(ctx, ids)
	if err != nil {
		return err
	}
	for _, l := range streams {
		l.Author = users[l.AuthorID]
	}
	return nil
}

// TopTippers is the creator-only leaderboard for a room.
func (s *Store) TopTippers(ctx context.Context, streamID string, limit int) ([]TipperTotal, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT le.counterparty_pial, SUM(le.amount_uaet) AS total
		FROM ledger_entries le WHERE le.stream_id = ? AND le.kind = 'tip_received'
		GROUP BY le.counterparty_pial ORDER BY total DESC LIMIT ?`, streamID, limit)
	if err != nil {
		return nil, err
	}
	type row struct {
		pial  string
		total int64
	}
	var raw []row
	for rows.Next() {
		var r row
		rows.Scan(&r.pial, &r.total)
		raw = append(raw, r)
	}
	rows.Close()
	out := make([]TipperTotal, 0, len(raw))
	for _, r := range raw {
		u, err := s.GetUserByPIAL(ctx, r.pial)
		if err != nil {
			continue
		}
		out = append(out, TipperTotal{User: u, AmountUAET: r.total})
	}
	return out, nil
}

// TipStream moves a tip and attributes it to the room.
func (s *Store) TipStream(ctx context.Context, fromPIAL, toPIAL, streamID string, amountUAET int64) error {
	if amountUAET <= 0 {
		return errors.New("invalid amount")
	}
	if fromPIAL == toPIAL {
		return errors.New("cannot tip yourself")
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var settled int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(amount_uaet), 0) FROM ledger_entries WHERE pial_id = ? AND status = 'settled'`, fromPIAL).Scan(&settled); err != nil {
			return err
		}
		if settled < amountUAET {
			return ErrInsufficientBalance
		}
		now := Now()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ledger_entries (id, pial_id, kind, amount_uaet, counterparty_pial, stream_id, status, created_at)
			VALUES (?, ?, 'tip_sent', ?, ?, ?, 'settled', ?)`, NewID(), fromPIAL, -amountUAET, toPIAL, streamID, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO ledger_entries (id, pial_id, kind, amount_uaet, counterparty_pial, stream_id, status, created_at)
			VALUES (?, ?, 'tip_received', ?, ?, ?, 'settled', ?)`, NewID(), toPIAL, amountUAET, fromPIAL, streamID, now)
		return err
	})
}

// AppendChat stores a line and returns it hydrated.
func (s *Store) AppendChat(ctx context.Context, streamID string, user *User, kind, body string, amountUAET int64) (*LiveChatMessage, error) {
	now := time.Now()
	m := &LiveChatMessage{ID: NewID(), StreamID: streamID, User: user, Kind: kind, Body: body, AmountUAET: amountUAET, CreatedAt: now}
	var userID any
	if user != nil {
		userID = user.ID
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO live_chat (id, stream_id, user_id, kind, body, amount_uaet, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		m.ID, streamID, userID, kind, body, amountUAET, FormatTime(now))
	return m, err
}

// ChatBacklog is the last `limit` lines, oldest first.
func (s *Store) ChatBacklog(ctx context.Context, streamID string, limit int) ([]*LiveChatMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, COALESCE(user_id, ''), kind, body, amount_uaet, created_at FROM live_chat
		WHERE stream_id = ? ORDER BY created_at DESC LIMIT ?`, streamID, limit)
	if err != nil {
		return nil, err
	}
	var out []*LiveChatMessage
	userIDs := map[string]bool{}
	for rows.Next() {
		var m LiveChatMessage
		var uid, created string
		if err := rows.Scan(&m.ID, &uid, &m.Kind, &m.Body, &m.AmountUAET, &created); err != nil {
			rows.Close()
			return nil, err
		}
		m.StreamID = streamID
		m.CreatedAt = ParseTime(created)
		if uid != "" {
			m.User = &User{ID: uid}
			userIDs[uid] = true
		}
		out = append(out, &m)
	}
	rows.Close()
	ids := make([]string, 0, len(userIDs))
	for id := range userIDs {
		ids = append(ids, id)
	}
	users, err := s.GetUsersByID(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, m := range out {
		if m.User != nil {
			m.User = users[m.User.ID]
		}
	}
	// Reverse to oldest-first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if out == nil {
		out = []*LiveChatMessage{}
	}
	return out, nil
}

// SetViewerCount records presence and the peak.
func (s *Store) SetViewerCount(ctx context.Context, streamID string, n int) {
	s.db.ExecContext(ctx, `UPDATE live_streams SET viewer_count = ?, peak_viewers = MAX(peak_viewers, ?), updated_at = ? WHERE id = ?`,
		n, n, Now(), streamID)
}

// PinLive sets (or with "" clears) the pinned line.
func (s *Store) PinLive(ctx context.Context, streamID, body string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE live_streams SET pinned_body = ?, updated_at = ? WHERE id = ?`, body, Now(), streamID)
	return err
}

// Heart bumps the heart counter and returns the new total.
func (s *Store) Heart(ctx context.Context, streamID string) (int, error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE live_streams SET heart_count = heart_count + 1 WHERE id = ? AND status = 'live'`, streamID); err != nil {
		return 0, err
	}
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT heart_count FROM live_streams WHERE id = ?`, streamID).Scan(&n)
	return n, err
}

// LiveSummary is what the broadcaster sees when they end.
type LiveSummary struct {
	DurationSecs int64
	PeakViewers  int
	TipsUAET     int64
	NewFollowers int
	ChatLines    int
	Hearts       int
	ReplaySaved  bool
}

// EndLive closes the room and computes the summary.
func (s *Store) EndLive(ctx context.Context, streamID, authorID string) (*LiveSummary, error) {
	l, err := s.GetLive(ctx, streamID)
	if err != nil {
		return nil, err
	}
	if l.AuthorID != authorID {
		return nil, errors.New("forbidden")
	}
	if l.Status == "ended" {
		return nil, errors.New("this stream has ended")
	}
	now := time.Now()
	if _, err := s.db.ExecContext(ctx, `UPDATE live_streams SET status = 'ended', ended_at = ?, viewer_count = 0, updated_at = ? WHERE id = ?`,
		FormatTime(now), FormatTime(now), streamID); err != nil {
		return nil, err
	}
	sum := &LiveSummary{PeakViewers: l.PeakViewers, TipsUAET: l.TipTotalUAET, Hearts: l.HeartCount, ReplaySaved: l.SaveReplay}
	if l.StartedAt != nil {
		sum.DurationSecs = int64(now.Sub(*l.StartedAt).Seconds())
		s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM follows WHERE following_id = ? AND created_at >= ?`, authorID, FormatTime(*l.StartedAt)).Scan(&sum.NewFollowers)
	}
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM live_chat WHERE stream_id = ? AND kind = 'chat'`, streamID).Scan(&sum.ChatLines)
	return sum, nil
}
