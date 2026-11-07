package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/f33d3r/feed-engine/internal/herald"
	"time"

	"github.com/lib/pq"

	"github.com/f33d3r/feed-engine/internal/model"
)

// ── Queries the /api/v1 JSON surface needs and the web surfaces did not ──────
//
// Every read here is a cursor-paginated or grouped form of a query the Facet
// renderers already run; nothing selects a column the web does not. The
// notification grouping in particular follows CLAUDE.md: like, repost and
// follow group; reply, quote, mention, tip, subscribe and thread_reply do not.

// GetSessionExpiry returns when a session ends. Zero when the token names no
// live session.
func GetSessionExpiry(db *sql.DB, rawToken string) time.Time {
	var t time.Time
	_ = db.QueryRow(`SELECT expires_at FROM user_sessions WHERE token_hash = $1`, HashToken(rawToken)).Scan(&t)
	return t
}

// IsBlocked reports whether blockerID has blocked blockedID.
func IsBlocked(db *sql.DB, blockerID, blockedID string) bool {
	if blockerID == "" || blockedID == "" {
		return false
	}
	var exists bool
	_ = db.QueryRow(`SELECT EXISTS (SELECT 1 FROM blocks WHERE blocker_id = $1 AND blocked_id = $2)`, blockerID, blockedID).Scan(&exists)
	return exists
}

// GetHandleForPIAL resolves a PIAL to the handle of the account it anchors, or
// "" when no account carries it.
func GetHandleForPIAL(db *sql.DB, pialID string) string {
	if pialID == "" {
		return ""
	}
	var handle string
	_ = db.QueryRow(`SELECT handle FROM users WHERE pial_id = $1::uuid LIMIT 1`, pialID).Scan(&handle)
	return handle
}

// UsersByPIAL resolves many PIALs to the accounts that anchor them in ONE
// query, keyed by PIAL.
//
// It exists because a brain that speaks only in PIALs (Auralis, the
// Frequencies brain) hands back a room full of them — host, every speaker,
// every co-host, every raised hand — and resolving those one at a time is a
// query per face on every 2s poll of every open room. A PIAL with no local
// account is simply absent from the result, which is what lets the caller
// drop an unresolvable identity rather than leak the uuid.
func UsersByPIAL(database *sql.DB, pials []string) (map[string]*model.User, error) {
	out := map[string]*model.User{}
	if database == nil || len(pials) == 0 {
		return out, nil
	}
	seen := make(map[string]bool, len(pials))
	want := make([]string, 0, len(pials))
	for _, p := range pials {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		want = append(want, p)
	}
	if len(want) == 0 {
		return out, nil
	}
	rows, err := database.Query(userSelectSQL+"WHERE u.pial_id::text = ANY($1)", pq.Array(want))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		if u != nil && u.PIALID != "" {
			out[u.PIALID] = u
		}
	}
	return out, rows.Err()
}

// GetWorkRepliesBefore is GetWorkReplies with a cursor: replies to workID
// created strictly before `before`, newest first.
func GetWorkRepliesBefore(db *sql.DB, workID string, limit int, before time.Time) ([]*model.Work, error) {
	rows, err := db.Query(worksSelectSQL+`
		JOIN work_citations wc ON wc.work_id = w.id
		WHERE wc.target_id = $1::uuid
		  AND wc.citation_type = 'reply'
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

// GetWorksLikedByUserBefore is GetWorksLikedByUser with a cursor over the
// time of the like, so the owner's Likes tab pages the way the others do.
func GetWorksLikedByUserBefore(db *sql.DB, viewerID string, limit int, before time.Time) ([]*model.Work, error) {
	rows, err := db.Query(worksSelectSQL+`
		JOIN work_reactions lr ON lr.work_id = w.id
		WHERE lr.reactor_id = $1::uuid
		  AND lr.reaction_type = 'like'
		  AND lr.created_at < $3
		  AND w.deleted_at IS NULL
		  AND `+workNotBlockedSQL+`
		  AND `+workLiveSQL+`
		ORDER BY lr.created_at DESC
		LIMIT $2
	`, viewerID, limit, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectWorks(rows)
}

// GetWorkAuthor returns the account and PIAL behind a live work.
func GetWorkAuthor(db *sql.DB, workID string) (authorID, authorPIAL string, err error) {
	err = db.QueryRow(`SELECT author_id::text, COALESCE(author_pial::text, '') FROM works WHERE id = $1::uuid AND deleted_at IS NULL`, workID).Scan(&authorID, &authorPIAL)
	return
}

// GetWorkBodyPreview returns the first 120 bytes of a work's body, trimmed,
// for a notification preview. "" when the work is gone.
func GetWorkBodyPreview(db *sql.DB, workID string) string {
	var body string
	_ = db.QueryRow(`SELECT LEFT(COALESCE(body, ''), 120) FROM works WHERE id = $1::uuid`, workID).Scan(&body)
	return body
}

// NotificationGroup is what the Inbox draws: one row for a single event, or
// one row standing for several of the same kind on the same target within an
// hour.
type NotificationGroup struct {
	ID         string
	Kind       string
	ActorIDs   []string
	ActorCount int
	TargetID   string
	TargetType string
	Preview    string
	AmountUAET int64
	IsRead     bool
	CreatedAt  time.Time
	// MemberIDs are the row ids this group stands for, so marking it read
	// marks all of them.
	MemberIDs []string
}

func notificationKindGroups(kind string) bool {
	switch kind {
	case "like", "repost", "follow":
		return true
	}
	return false
}

// ListNotificationGroups returns grouped notifications newest first. `before`
// is the created_at of the last group of the previous page; the next cursor
// is returned alongside and is "" when the page is the last.
func ListNotificationGroups(db *sql.DB, userID string, limit int, before time.Time, kinds ...string) ([]*NotificationGroup, string, error) {
	// Over-fetch raw rows: grouping shrinks the list and the page is measured
	// in groups.
	ctx, cancel := context.WithTimeout(context.Background(), heraldTimeout)
	defer cancel()
	rows, err := herald.List(ctx, userID, limit*8, before, kinds...)
	if err != nil {
		return nil, "", err
	}

	var out []*NotificationGroup
	open := map[string]*NotificationGroup{}
	exhausted := true
	scanned := 0
	for _, r := range rows {
		scanned++
		id, kind, actorID, targetID, targetType, payload := r.ID, r.Kind, r.ActorAccount, r.TargetID, r.TargetType, r.Payload
		isRead, created := r.IsRead, r.CreatedAt
		key := kind + "|" + targetType + "|" + targetID
		if g, ok := open[key]; ok && notificationKindGroups(kind) && g.CreatedAt.Sub(created) <= time.Hour {
			g.MemberIDs = append(g.MemberIDs, id)
			g.ActorCount++
			if !isRead {
				g.IsRead = false
			}
			if actorID != "" {
				g.ActorIDs = append(g.ActorIDs, actorID)
			}
			continue
		}
		if len(out) >= limit {
			exhausted = false
			break
		}
		g := &NotificationGroup{
			ID:         id,
			Kind:       kind,
			ActorCount: 1,
			TargetID:   targetID,
			TargetType: targetType,
			IsRead:     isRead,
			CreatedAt:  created,
			MemberIDs:  []string{id},
		}
		if actorID != "" {
			g.ActorIDs = []string{actorID}
		}
		var p struct {
			Preview    string `json:"preview"`
			AmountUAET int64  `json:"amount_uaet"`
		}
		if payload != "" {
			_ = json.Unmarshal([]byte(payload), &p)
		}
		g.Preview = p.Preview
		g.AmountUAET = p.AmountUAET
		open[key] = g
		out = append(out, g)
	}
	// A full raw window may have more below it even when the groups fit.
	if scanned >= limit*8 {
		exhausted = false
	}
	next := ""
	if !exhausted && len(out) > 0 {
		next = out[len(out)-1].CreatedAt.Format(time.RFC3339Nano)
	}
	return out, next, nil
}

// NotificationGroupMemberIDs returns the ids of every row the group headed by
// headID stands for — the head and the rows of the same kind on the same
// target within the hour before it — scoped to userID so one account cannot
// mark another's rows.
func NotificationGroupMemberIDs(db *sql.DB, userID, headID string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), heraldTimeout)
	defer cancel()
	kind, ids, err := herald.Group(ctx, userID, headID)
	if err != nil {
		return nil, err
	}
	if !notificationKindGroups(kind) {
		return []string{headID}, nil
	}
	return ids, nil
}

// MarkNotificationsRead marks the named rows read for userID.
func MarkNotificationsRead(db *sql.DB, userID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), heraldTimeout)
	defer cancel()
	return herald.MarkRead(ctx, userID, ids, false)
}

// GetUsersByIDs loads full user rows for a set of ids, keyed by id. Ids that
// name nothing are absent from the map.
func GetUsersByIDs(db *sql.DB, ids []string) (map[string]*model.User, error) {
	out := map[string]*model.User{}
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		u, err := GetUserByID(db, id)
		if err != nil {
			return nil, err
		}
		if u != nil {
			out[id] = u
		}
	}
	return out, nil
}

// GetWorkRepliesForProfile is the Replies tab: replies authored by the PIAL,
// newest first, strictly before the cursor.
func GetWorkRepliesForProfile(db *sql.DB, authorPIAL string, limit int, before time.Time) ([]*model.Work, error) {
	rows, err := db.Query(worksSelectSQL+`
		WHERE w.author_pial = $1::uuid
		  AND w.deleted_at IS NULL
		  AND w.kind = 'reply'
		  AND `+workNotBlockedSQL+`
		  AND `+workLiveSQL+`
		  AND w.created_at < $2
		ORDER BY w.created_at DESC
		LIMIT $3
	`, authorPIAL, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectWorks(rows)
}

// GetWorksForProfileMedia is the Media tab: the PIAL's works that carry an
// image, a video or a voice note, newest first, strictly before the cursor.
func GetWorksForProfileMedia(db *sql.DB, authorPIAL string, limit int, before time.Time) ([]*model.Work, error) {
	rows, err := db.Query(worksSelectSQL+`
		WHERE w.author_pial = $1::uuid
		  AND w.deleted_at IS NULL
		  AND (COALESCE(array_length(w.media_urls, 1), 0) > 0
		       OR COALESCE(w.video_master_url, '') <> ''
		       OR COALESCE(w.voice_url, '') <> '')
		  AND `+workNotBlockedSQL+`
		  AND `+workLiveSQL+`
		  AND w.created_at < $2
		ORDER BY w.created_at DESC
		LIMIT $3
	`, authorPIAL, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectWorks(rows)
}
