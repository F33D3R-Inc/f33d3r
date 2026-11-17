package db

// compliance_2257.go — what 18+ works owe Verity under 18 U.S.C. § 2257.
//
// An 18+ work by a creator carries two obligations to Verity, the owner of the
// 2257 records:
//
//	record           link the work to the creator's passing creator
//	                 verification; queued in the work's create transaction
//	content_removed  report the work's removal, from which the retention of the
//	                 creator's ID copy is measured; queued in the transaction
//	                 that deletes the work (author delete, admin removal, purge)
//
// Rows are drained by the work outbox worker (handler.drain2257). A removal is
// not due until the record it reports on has been written, so Verity never
// hears of the removal of a work it was never told about.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Compliance2257Action is the closed vocabulary of compliance_2257_outbox.action.
// Mirrored by its CHECK in migration 0047; change one and the other with it.
type Compliance2257Action string

const (
	Compliance2257Record  Compliance2257Action = "record"
	Compliance2257Removed Compliance2257Action = "content_removed"
)

// Compliance2257MaxAttempts is the attempt at which a failing row is
// quarantined, as for work obligations.
const Compliance2257MaxAttempts = WorkOutboxMaxAttempts

func compliance2257DedupKey(action Compliance2257Action, workID string) string {
	return "2257:" + string(action) + ":" + workID
}

// enqueue2257Record records, in the work's create transaction, that its 2257
// record is owed.
func enqueue2257Record(tx *sql.Tx, workID, pialID string) error {
	if _, err := tx.Exec(`
		INSERT INTO compliance_2257_outbox (work_id, pial_id, action, dedup_key)
		VALUES ($1::uuid, $2::uuid, $3, $4)
		ON CONFLICT (dedup_key) DO NOTHING`,
		workID, pialID, string(Compliance2257Record), compliance2257DedupKey(Compliance2257Record, workID)); err != nil {
		return fmt.Errorf("queueing the 2257 record of work %s: %w", workID, err)
	}
	return nil
}

// enqueue2257Removals records, in a deleting transaction, the removal report of
// every work selected by worksSQL that has a 2257 record owed or written.
// worksSQL selects work ids (as uuid) and takes its arguments from args.
func enqueue2257Removals(tx DBTX, worksSQL string, args ...interface{}) error {
	n := len(args)
	if _, err := tx.Exec(fmt.Sprintf(`
		INSERT INTO compliance_2257_outbox (work_id, pial_id, action, dedup_key, removed_at)
		SELECT r.work_id, r.pial_id, $%d, $%d || r.work_id::text, NOW()
		  FROM compliance_2257_outbox r
		 WHERE r.action = $%d
		   AND r.work_id IN (%s)
		ON CONFLICT (dedup_key) DO NOTHING`, n+1, n+2, n+3, worksSQL),
		append(args, string(Compliance2257Removed), "2257:"+string(Compliance2257Removed)+":", string(Compliance2257Record))...); err != nil {
		return fmt.Errorf("queueing 2257 removal reports: %w", err)
	}
	return nil
}

// Compliance2257Row is one claimed obligation. Attempt counts this claim.
type Compliance2257Row struct {
	ID        int64
	WorkID    string
	PIALID    string
	Action    Compliance2257Action
	RemovedAt *time.Time
	Attempt   int
}

// Claim2257Outbox takes up to limit due rows and holds each for lease. A
// removal is not due while the record it reports on is still owed.
func Claim2257Outbox(database *sql.DB, limit int, lease time.Duration) ([]Compliance2257Row, error) {
	rows, err := database.Query(`
		UPDATE compliance_2257_outbox o
		   SET attempts = o.attempts + 1,
		       next_attempt_at = NOW() + make_interval(secs => $2)
		  FROM (SELECT c.id FROM compliance_2257_outbox c
		         WHERE c.done_at IS NULL AND c.quarantined_at IS NULL
		           AND c.next_attempt_at <= NOW()
		           AND NOT (c.action = $3 AND EXISTS (
		               SELECT 1 FROM compliance_2257_outbox r
		                WHERE r.work_id = c.work_id AND r.action = $4 AND r.done_at IS NULL))
		         ORDER BY c.next_attempt_at, c.id
		         LIMIT $1
		         FOR UPDATE SKIP LOCKED) due
		 WHERE o.id = due.id
		RETURNING o.id, o.work_id::text, o.pial_id::text, o.action, o.removed_at, o.attempts`,
		limit, lease.Seconds(), string(Compliance2257Removed), string(Compliance2257Record))
	if err != nil {
		return nil, fmt.Errorf("claiming 2257 obligations: %w", err)
	}
	defer rows.Close()
	var out []Compliance2257Row
	for rows.Next() {
		var r Compliance2257Row
		var action string
		var removed sql.NullTime
		if err := rows.Scan(&r.ID, &r.WorkID, &r.PIALID, &action, &removed, &r.Attempt); err != nil {
			return nil, fmt.Errorf("claiming 2257 obligations: %w", err)
		}
		r.Action = Compliance2257Action(action)
		if removed.Valid {
			t := removed.Time
			r.RemovedAt = &t
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claiming 2257 obligations: %w", err)
	}
	return out, nil
}

// Complete2257Outbox closes an obligation Verity has accepted.
func Complete2257Outbox(database *sql.DB, id int64) error {
	if _, err := database.Exec(
		`UPDATE compliance_2257_outbox SET done_at = NOW(), last_error = NULL WHERE id = $1 AND done_at IS NULL`, id); err != nil {
		return fmt.Errorf("closing 2257 obligation %d: %w", id, err)
	}
	return nil
}

// Fail2257Outbox records why an attempt failed and schedules the next, or
// quarantines the row at the attempt cap. Reports whether it was quarantined.
func Fail2257Outbox(database *sql.DB, id int64, cause error) (bool, error) {
	reason := cause.Error()
	if len(reason) > 2000 {
		reason = reason[:2000]
	}
	var quarantined bool
	err := database.QueryRow(`
		UPDATE compliance_2257_outbox
		   SET last_error = $2,
		       next_attempt_at = NOW() + LEAST(POWER(2, LEAST(attempts, 20)) * INTERVAL '1 second', INTERVAL '1 hour'),
		       quarantined_at = CASE WHEN attempts >= $3 THEN NOW() END,
		       quarantine_reason = CASE WHEN attempts >= $3 THEN $2 END
		 WHERE id = $1 AND done_at IS NULL
		RETURNING quarantined_at IS NOT NULL`, id, reason, Compliance2257MaxAttempts).Scan(&quarantined)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // closed by another replica in the meantime
	}
	if err != nil {
		return false, fmt.Errorf("rescheduling 2257 obligation %d: %w", id, err)
	}
	return quarantined, nil
}

// Quarantine2257Outbox quarantines a row at once, for an answer no retry can
// change. The row stays open for a person.
func Quarantine2257Outbox(database *sql.DB, id int64, cause error) error {
	reason := cause.Error()
	if len(reason) > 2000 {
		reason = reason[:2000]
	}
	if _, err := database.Exec(`
		UPDATE compliance_2257_outbox
		   SET last_error = $2, quarantined_at = NOW(), quarantine_reason = $2
		 WHERE id = $1 AND done_at IS NULL`, id, reason); err != nil {
		return fmt.Errorf("quarantining 2257 obligation %d: %w", id, err)
	}
	return nil
}

// Quarantined2257Outbox counts obligations waiting for a person.
func Quarantined2257Outbox(database *sql.DB) (int, error) {
	var n int
	if err := database.QueryRow(`
		SELECT COUNT(*) FROM compliance_2257_outbox
		 WHERE quarantined_at IS NOT NULL AND done_at IS NULL`).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting quarantined 2257 obligations: %w", err)
	}
	return n, nil
}
