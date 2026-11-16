package db

// tracks.go — music tracks, which are works of kind 'track'.
//
// A track is signed, scanned, fanned out, walled and stored exactly like any
// other work (0037 carried every row of the retired tracks table across). What
// is particular to a track is read here: the track list, the artist's totals,
// and the play count, which is the one tally a track has that other works do
// not. Likes are work_reactions 'like', the same reaction every work has.

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/f33d3r/feed-engine/internal/model"
)

// WorkKindTrack is the kind of a music track.
const WorkKindTrack = "track"

// TrackPageCursor is the cursor of the last track on a page: its created_at
// and id, in the "time|id" form parseCursor reads.
func TrackPageCursor(w *model.Work) string {
	return w.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + w.ID
}

// GetTrackWorks returns live tracks, newest first. genre filters by tag (the
// genre is the track's first tag); authorID, when set, keeps one artist's
// tracks. before is a TrackPageCursor; "" starts from now. The caller applies
// the viewer's wall (gateWorks) before rendering: this is the published set,
// not what any one viewer may see.
func GetTrackWorks(database *sql.DB, genre, authorID string, limit int, before string) ([]*model.Work, error) {
	c, err := parseCursor(before)
	if err != nil {
		return nil, fmt.Errorf("track page cursor %q: %w", before, err)
	}
	var authorArg interface{}
	if authorID != "" {
		authorArg = authorID
	}
	rows, err := database.Query(worksSelectSQL+`
		WHERE w.kind = 'track'
		  AND w.deleted_at IS NULL
		  AND `+workNotBlockedSQL+`
		  AND ($1 = '' OR lower($1) = ANY (SELECT lower(t) FROM unnest(w.tags) t))
		  AND ($2::uuid IS NULL OR w.author_id = $2::uuid)
		  AND (w.created_at, w.id) < ($3, $5::uuid)
		ORDER BY w.created_at DESC, w.id DESC
		LIMIT $4`, strings.TrimSpace(genre), authorArg, c.TS, limit, c.ID)
	if err != nil {
		return nil, fmt.Errorf("reading tracks: %w", err)
	}
	defer rows.Close()
	return collectWorks(rows)
}

// GetTrackWork returns one live track, or nil, nil when there is none.
func GetTrackWork(database *sql.DB, workID string) (*model.Work, error) {
	rows, err := database.Query(worksSelectSQL+`
		WHERE w.id = $1::uuid AND w.kind = 'track'
		  AND w.deleted_at IS NULL
		  AND `+workNotBlockedSQL, workID)
	if err != nil {
		return nil, fmt.Errorf("reading track %s: %w", workID, err)
	}
	defer rows.Close()
	works, err := collectWorks(rows)
	if err != nil {
		return nil, fmt.Errorf("reading track %s: %w", workID, err)
	}
	if len(works) == 0 {
		return nil, nil
	}
	return works[0], nil
}

// TrackPlayTotals returns the play count of each of the given works; a work
// never played is absent (zero).
func TrackPlayTotals(database *sql.DB, workIDs []string) (map[string]int64, error) {
	out := make(map[string]int64, len(workIDs))
	if len(workIDs) == 0 {
		return out, nil
	}
	rows, err := database.Query(`
		SELECT work_id::text, plays FROM work_play_totals
		 WHERE work_id = ANY($1::uuid[])`, pq.Array(workIDs))
	if err != nil {
		return nil, fmt.Errorf("reading play totals: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("reading play totals: %w", err)
		}
		out[id] = n
	}
	return out, rows.Err()
}

// RecordTrackPlay counts one play of a live track and returns its new total.
// sql.ErrNoRows when the work is not a live track.
func RecordTrackPlay(database *sql.DB, workID string) (int64, error) {
	var plays int64
	err := database.QueryRow(`
		INSERT INTO work_play_totals (work_id, plays)
		SELECT w.id, 1 FROM works w
		 WHERE w.id = $1::uuid AND w.kind = 'track' AND w.deleted_at IS NULL
		ON CONFLICT (work_id) DO UPDATE
		   SET plays = work_play_totals.plays + 1, updated_at = NOW()
		RETURNING plays`, workID).Scan(&plays)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, sql.ErrNoRows
	}
	if err != nil {
		return 0, fmt.Errorf("counting a play of %s: %w", workID, err)
	}
	return plays, nil
}

// WorkReactedBy reports whether one account holds one reaction on a work.
func WorkReactedBy(database *sql.DB, workID, reactorID, reactionType string) (bool, error) {
	var exists bool
	if err := database.QueryRow(`
		SELECT EXISTS (SELECT 1 FROM work_reactions
		                WHERE work_id = $1::uuid AND reactor_id = $2::uuid AND reaction_type = $3)`,
		workID, reactorID, reactionType).Scan(&exists); err != nil {
		return false, fmt.Errorf("reading %s of %s on %s: %w", reactionType, reactorID, workID, err)
	}
	return exists, nil
}

// WorkLikeCount counts the likes of one work.
func WorkLikeCount(database *sql.DB, workID string) (int, error) {
	var n int
	if err := database.QueryRow(`
		SELECT COUNT(*) FROM work_reactions WHERE work_id = $1::uuid AND reaction_type = 'like'`,
		workID).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting likes of %s: %w", workID, err)
	}
	return n, nil
}

// ArtistStats are one artist's track totals.
type ArtistStats struct {
	TotalTracks int
	TotalPlays  int64
	TotalLikes  int
}

// GetArtistStats totals one artist's live tracks.
func GetArtistStats(database *sql.DB, authorID string) (ArtistStats, error) {
	var s ArtistStats
	err := database.QueryRow(`
		SELECT COUNT(*),
		       COALESCE(SUM(COALESCE(pt.plays, 0)), 0),
		       COALESCE(SUM((SELECT COUNT(*) FROM work_reactions wr
		                      WHERE wr.work_id = w.id AND wr.reaction_type = 'like')), 0)
		  FROM works w
		  LEFT JOIN work_play_totals pt ON pt.work_id = w.id
		 WHERE w.author_id = $1::uuid AND w.kind = 'track' AND w.deleted_at IS NULL`,
		authorID).Scan(&s.TotalTracks, &s.TotalPlays, &s.TotalLikes)
	if err != nil {
		return ArtistStats{}, fmt.Errorf("totalling tracks of %s: %w", authorID, err)
	}
	return s, nil
}
