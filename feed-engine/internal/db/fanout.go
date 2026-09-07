package db

import (
	"database/sql"
)

// Who a newly-surfaced work is pushed to.
//
// A new_post event is the following feed arriving before it is asked for, so it
// must answer to the same rules the following feed answers to. It used to
// answer to none of them: every follower was sent every work, which meant a
// subscriber-only work announced itself to people who cannot open it, a work
// held by a moderation verdict announced itself at all, an expired work
// announced itself after it had gone, and a reply — which the following feed
// has never shown — arrived as if it were a post.
//
// The predicates below are the ones GetWorksFeedFollowing composes, referenced
// rather than restated, so a change to what the feed shows changes what the
// stream pushes in the same edit. Blocks and mutes are checked here even though
// the feed query does not check them, because a push is a stronger act than a
// read: a person who has muted an account has said they do not want that
// account interrupting them, and there is no honest reading of that which ends
// in a live card.

// fanoutEligibleSQL selects the PIAL of every follower of $1 who should receive
// the work $2 live. $1 is the account whose followers are being fanned to (the
// author for an original, the reposter for a repost); $2 is the work.
//
// The subscriber-only, moderation and expiry clauses are the feed's own
// constants; the visibility of the work is decided per follower, because
// subscriber-only means something different to each of them.
const fanoutEligibleSQL = `
	SELECT DISTINCT u.pial_id::text
	FROM follows f
	JOIN users u ON u.id = f.follower_id
	JOIN works w ON w.id = $2::uuid
	WHERE f.following_id = $1::uuid
	  AND u.pial_id IS NOT NULL
	  AND w.deleted_at IS NULL
	  AND ` + workNotBlockedSQL + `
	  AND ` + workLiveSQL + `
	  AND (NOT w.subscriber_only
	       OR w.author_id = u.id
	       OR EXISTS (SELECT 1 FROM subscriptions s
	                  WHERE s.subscriber_id = u.id AND s.creator_id = w.author_id AND s.status = 'active'))
	  AND NOT EXISTS (
	      SELECT 1 FROM blocks b
	      WHERE (b.blocker_id = u.id AND b.blocked_id IN (w.author_id, $1::uuid))
	         OR (b.blocker_id IN (w.author_id, $1::uuid) AND b.blocked_id = u.id))
	  AND NOT EXISTS (
	      SELECT 1 FROM user_mutes m
	      WHERE m.muter_id = u.id AND m.muted_id IN (w.author_id, $1::uuid))`

// FanoutFollowerPIALs returns the PIAL of every follower of sourceUserID that
// workID may be pushed to live. sourceUserID is the account whose followers are
// being reached — the author for an original work, the reposter for a repost —
// and both that account and the work's author are checked against the
// follower's blocks and mutes, because a repost puts two people in front of the
// reader and either of them may be one the reader has shut out.
//
// A work that fails a visibility rule yields no rows at all rather than an
// error: there is nobody to tell, and that is not a failure.
func FanoutFollowerPIALs(db *sql.DB, sourceUserID, workID string) ([]string, error) {
	if db == nil || sourceUserID == "" || workID == "" {
		return nil, nil
	}
	rows, err := db.Query(fanoutEligibleSQL, sourceUserID, workID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var pial string
		if err := rows.Scan(&pial); err != nil {
			return nil, err
		}
		if pial != "" {
			out = append(out, pial)
		}
	}
	return out, rows.Err()
}

// WorkKindAndAuthor returns a work's kind, its author's account id and its
// author's handle. Used by the fan-out to decide whether a work is a feed item
// at all before it goes looking for an audience.
func WorkKindAndAuthor(db *sql.DB, workID string) (kind, authorID, authorHandle string, err error) {
	if db == nil || workID == "" {
		return "", "", "", sql.ErrNoRows
	}
	err = db.QueryRow(`
		SELECT w.kind, w.author_id::text, COALESCE(u.handle, '')
		FROM works w
		LEFT JOIN users u ON u.id = w.author_id
		WHERE w.id = $1::uuid AND w.deleted_at IS NULL`, workID).
		Scan(&kind, &authorID, &authorHandle)
	return kind, authorID, authorHandle, err
}
