package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/f33d3r/feed-engine/internal/model"
)

// ── The room around Visions and Live, and the account lists the native
// clients page (migration 0022) ──────────────────────────────────────────────

// ErrVisionNoPoll: the Vision carries no poll.
var ErrVisionNoPoll = errors.New("vision: no poll on this vision")

// ErrVisionPollOption: the option index is outside the poll.
var ErrVisionPollOption = errors.New("vision: option out of range")

// ErrLiveNotOpen: the broadcast is not on air, so the room takes nothing.
var ErrLiveNotOpen = errors.New("live: this broadcast is not open")

// CastVisionPollVote records one ballot per voting account per Vision. A
// second ballot from the same account is not an error: the first stands. Each
// persona of one person votes as itself; a ballot is never shared across them.
// A Vision still pending its scan takes ballots only from its own author.
func CastVisionPollVote(database *sql.DB, ref VisionRef, voterAccount string, optionIdx int) error {
	if voterAccount == "" {
		return errors.New("vision: voter_account is required")
	}
	var nOptions int
	err := database.QueryRow(`
		SELECT COALESCE(array_length(v.poll_options, 1), 0)
		  FROM visions v WHERE v.author_pial = $2::uuid AND v.seq = $3
		   AND `+visionLiveSQL+` AND `+visionScannedSQL, voterAccount, ref.AuthorPIAL, ref.Seq).Scan(&nOptions)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrVisionNotLive
	}
	if err != nil {
		return err
	}
	if nOptions == 0 {
		return ErrVisionNoPoll
	}
	if optionIdx < 0 || optionIdx >= nOptions {
		return ErrVisionPollOption
	}
	_, err = database.Exec(`
		INSERT INTO vision_poll_votes (author_pial, seq, voter_account, option_idx)
		VALUES ($1::uuid, $2, $3::uuid, $4)
		ON CONFLICT (author_pial, seq, voter_account) DO NOTHING`, ref.AuthorPIAL, ref.Seq, voterAccount, optionIdx)
	return err
}

// VisionPollVotes returns the vote count per option, index-aligned with the
// Vision's poll_options.
func VisionPollVotes(database *sql.DB, ref VisionRef, nOptions int) ([]int, error) {
	counts := make([]int, nOptions)
	if nOptions == 0 {
		return counts, nil
	}
	rows, err := database.Query(`
		SELECT option_idx, COUNT(*)::int FROM vision_poll_votes
		 WHERE author_pial = $1::uuid AND seq = $2 GROUP BY option_idx`, ref.AuthorPIAL, ref.Seq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var idx, n int
		if err := rows.Scan(&idx, &n); err != nil {
			return nil, err
		}
		if idx >= 0 && idx < nOptions {
			counts[idx] = n
		}
	}
	return counts, rows.Err()
}

// SetVisionMuted hides (or shows again) one account's ring for one viewing
// account, account to account. Muting a persona hides that persona alone: a
// mute that reached every account on the target's PIAL would show the muter
// which accounts belong to one person.
func SetVisionMuted(database *sql.DB, muterAccount, mutedAccount string, muted bool) error {
	if muterAccount == "" || mutedAccount == "" || muterAccount == mutedAccount {
		return errors.New("vision: mute needs two different accounts")
	}
	if muted {
		_, err := database.Exec(`
			INSERT INTO vision_mutes (muter_account, muted_account) VALUES ($1::uuid, $2::uuid)
			ON CONFLICT DO NOTHING`, muterAccount, mutedAccount)
		return err
	}
	_, err := database.Exec(`DELETE FROM vision_mutes WHERE muter_account = $1::uuid AND muted_account = $2::uuid`, muterAccount, mutedAccount)
	return err
}

// LiveAuthorIDs is the set of accounts on air right now, for the ring rail's
// live mark.
func LiveAuthorIDs(database *sql.DB) (map[string]bool, error) {
	rows, err := database.Query(`
		SELECT author_id::text FROM live_streams
		 WHERE status = 'live' AND is_blocked = FALSE AND scan_state != 'blocked'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// SetStreamRoom writes the room settings a native broadcaster chose when
// opening the broadcast. Only an open (idle or live) stream takes them.
func SetStreamRoom(database *sql.DB, streamID, audience, lane string, tipGoalUAET int64, saveReplay bool) error {
	if err := checkStreamID(streamID); err != nil {
		return err
	}
	switch audience {
	case "everyone", "followers", "subscribers":
	default:
		return fmt.Errorf("live: unknown audience %q", audience)
	}
	if tipGoalUAET < 0 {
		tipGoalUAET = 0
	}
	res, err := database.Exec(`
		UPDATE live_streams
		   SET audience = $2, lane = $3, tip_goal_uaet = $4, save_replay = $5, updated_at = NOW()
		 WHERE id = $1::uuid AND status != 'ended'`,
		streamID, audience, strings.TrimSpace(lane), tipGoalUAET, saveReplay)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrLiveStreamNotFound
	}
	return nil
}

// HeartStream adds one heart to a live broadcast and returns the new total.
func HeartStream(database *sql.DB, streamID string) (int, error) {
	if err := checkStreamID(streamID); err != nil {
		return 0, err
	}
	var n int
	err := database.QueryRow(`
		UPDATE live_streams SET heart_count = heart_count + 1, updated_at = NOW()
		 WHERE id = $1::uuid AND status = 'live'
		RETURNING heart_count`, streamID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrLiveNotOpen
	}
	return n, err
}

// PinStreamLine sets (or with "" clears) the broadcaster's pinned line.
func PinStreamLine(database *sql.DB, streamID, body string) error {
	if err := checkStreamID(streamID); err != nil {
		return err
	}
	res, err := database.Exec(`
		UPDATE live_streams SET pinned_body = $2, updated_at = NOW()
		 WHERE id = $1::uuid AND status = 'live'`, streamID, strings.TrimSpace(body))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrLiveNotOpen
	}
	return nil
}

// CountStreamChatLine bumps the chat-line tally the end-of-stream summary
// reports. The lines themselves live in memory with the broadcast.
func CountStreamChatLine(database *sql.DB, streamID string) {
	_, _ = database.Exec(`UPDATE live_streams SET chat_lines = chat_lines + 1 WHERE id = $1::uuid`, streamID)
}

// RecordStreamTip stores a tip that Ain Soph has already settled and returns
// the broadcast's new running total. Recorded after the ledger says yes,
// never before: the row is a record of money that moved.
func RecordStreamTip(database *sql.DB, streamID, fromPIAL string, amountUAET int64) (int64, error) {
	if err := checkStreamID(streamID); err != nil {
		return 0, err
	}
	if amountUAET <= 0 {
		return 0, errors.New("live: tip must be positive")
	}
	tx, err := database.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		INSERT INTO live_tips (stream_id, from_pial, amount_uaet) VALUES ($1::uuid, $2::uuid, $3)`,
		streamID, fromPIAL, amountUAET); err != nil {
		return 0, err
	}
	var total int64
	if err := tx.QueryRow(`
		UPDATE live_streams SET tip_total_uaet = tip_total_uaet + $2, updated_at = NOW()
		 WHERE id = $1::uuid RETURNING tip_total_uaet`, streamID, amountUAET).Scan(&total); err != nil {
		return 0, err
	}
	return total, tx.Commit()
}

// StreamTipper is one supporter's total on a broadcast.
type StreamTipper struct {
	PIAL       string
	AmountUAET int64
}

// TopStreamTippers lists the broadcast's biggest supporters, most first.
func TopStreamTippers(database *sql.DB, streamID string, limit int) ([]StreamTipper, error) {
	if limit <= 0 {
		limit = 3
	}
	rows, err := database.Query(`
		SELECT from_pial::text, SUM(amount_uaet)::bigint AS total
		  FROM live_tips WHERE stream_id = $1::uuid
		 GROUP BY from_pial ORDER BY total DESC LIMIT $2`, streamID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StreamTipper
	for rows.Next() {
		var t StreamTipper
		if err := rows.Scan(&t.PIAL, &t.AmountUAET); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// StreamNewFollowers counts the follows the broadcaster gained while on air.
func StreamNewFollowers(database *sql.DB, s *model.LiveStream) int {
	if s == nil || s.StartedAt == nil {
		return 0
	}
	end := time.Now()
	if s.EndedAt != nil {
		end = *s.EndedAt
	}
	var n int
	_ = database.QueryRow(`
		SELECT COUNT(*) FROM follows
		 WHERE following_id = $1::uuid AND created_at >= $2 AND created_at <= $3`,
		s.AuthorID, *s.StartedAt, end).Scan(&n)
	return n
}

// FollowEdge is one row of a followers or following list, with the time the
// edge was made so the list pages on it.
type FollowEdge struct {
	FollowListEntry
	FollowedAt time.Time
}

func followEdges(rows *sql.Rows) ([]FollowEdge, error) {
	defer rows.Close()
	var out []FollowEdge
	for rows.Next() {
		var e FollowEdge
		if err := rows.Scan(&e.ID, &e.Handle, &e.DisplayName, &e.AvatarURL, &e.FollowedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

const followEdgeSelectSQL = `
		SELECT u.id::text, u.handle, COALESCE(p.display_name, ''), COALESCE(p.avatar_url, ''),
		       f.created_at
		  FROM follows f
		  JOIN users u ON u.id = %s
		  LEFT JOIN user_profiles p ON p.user_id = u.id
		 WHERE %s = $1::uuid AND f.created_at < $2
		 ORDER BY f.created_at DESC
		 LIMIT $3`

// GetFollowersBefore is GetFollowers with a cursor: accounts following
// userID, newest edge first, strictly before `before`.
func GetFollowersBefore(database *sql.DB, userID string, limit int, before time.Time) ([]FollowEdge, error) {
	rows, err := database.Query(fmt.Sprintf(followEdgeSelectSQL, "f.follower_id", "f.following_id"), userID, before, limit)
	if err != nil {
		return nil, err
	}
	return followEdges(rows)
}

// GetFollowingBefore is GetFollowing with a cursor.
func GetFollowingBefore(database *sql.DB, userID string, limit int, before time.Time) ([]FollowEdge, error) {
	rows, err := database.Query(fmt.Sprintf(followEdgeSelectSQL, "f.following_id", "f.follower_id"), userID, before, limit)
	if err != nil {
		return nil, err
	}
	return followEdges(rows)
}

// FollowedSet reports which of ids the viewer follows.
func FollowedSet(database *sql.DB, viewerID string, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if viewerID == "" || len(ids) == 0 {
		return out, nil
	}
	rows, err := database.Query(`
		SELECT following_id::text FROM follows
		 WHERE follower_id = $1::uuid AND following_id = ANY($2::uuid[])`, viewerID, pq.Array(ids))
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

// RecordWorkEdit is the one write of an edit: the edition row for the audit
// trail and the body on the work, in one transaction. editionCID is the
// content id of the new edition, as the caller derived it.
func RecordWorkEdit(database *sql.DB, workID, authorID, body, editionCID string, editionNumber int) error {
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		INSERT INTO editions (work_id, cid, body, edition_number)
		VALUES ($1::uuid, $2, $3, $4)`, workID, editionCID, body, editionNumber); err != nil {
		return fmt.Errorf("edition: %w", err)
	}
	res, err := tx.Exec(`
		UPDATE works SET body = $1, is_edited = TRUE, edited_at = NOW()
		 WHERE id = $2::uuid AND author_id = $3::uuid AND deleted_at IS NULL`, body, workID, authorID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}
