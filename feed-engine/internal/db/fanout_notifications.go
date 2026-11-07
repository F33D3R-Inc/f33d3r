package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

const (
	fanoutBatch   = 500
	fanoutLockKey = int64(7331_0007)
)

// fanoutNamespace derives stable event IDs, so a batch retried after a crash cannot
// notify anyone twice.
var fanoutNamespace = uuid.MustParse("6f2d6b0a-7c1e-4f1a-9b52-0d3c7e5a1f44")

// FanoutJob is a notification owed to every follower of an account.
type FanoutJob struct {
	ID          int64
	ActorID     string
	ActorHandle string
	Kind        string
	TargetID    string
	TargetType  string
	Preview     string
	// Audience narrows who a job reaches: "subscribers", or anyone otherwise.
	Audience string
	// Adult drops anyone who may not see adult content.
	Adult bool
}

// FanoutRecipient is one follower reached by a batch.
type FanoutRecipient struct {
	Account string
	PIAL    string
}

// CreateFanoutJob queues a follower notification. Repeating it for the same target
// does nothing.
func CreateFanoutJob(db DBTX, job FanoutJob) error {
	_, err := db.Exec(`
		INSERT INTO notification_fanout (actor_id, actor_handle, kind, target_id, target_type, preview, audience, adult)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (kind, target_id) DO NOTHING`,
		job.ActorID, job.ActorHandle, job.Kind, job.TargetID, job.TargetType, job.Preview, job.Audience, job.Adult)
	return err
}

// FanoutEventID is the stable id of one job's notification to one account, so a
// retried batch cannot notify twice and a person told another way is not told again.
func FanoutEventID(kind, targetID, account string) string {
	return uuid.NewSHA1(fanoutNamespace, []byte(kind+"|"+targetID+"|"+account)).String()
}

// FanoutCheck is asked about a job before each batch. Stop ends the job with nobody
// else told, and Skip names PIALs this batch must leave out.
type FanoutCheck func(FanoutJob) (stop bool, skip map[string]bool)

const (
	frequencyFanoutKindPrefix = "frequency_"
	// A person hears about at most this many rooms a day, and no two close together.
	frequencyAnnounceDailyCap = 3
)

// frequencyFanoutSQL is the follower batch for a room. A push is a stronger act than
// a read, so blocks and mutes both count, and it goes only to people who could join.
const frequencyFanoutSQL = `
	SELECT f.follower_id::text, u.pial_id::text
	  FROM follows f
	  JOIN users u ON u.id = f.follower_id
	  LEFT JOIN user_profiles p ON p.user_id = u.id
	  LEFT JOIN pial_roots pr ON pr.pial_id = u.pial_id
	 WHERE f.following_id = $1::uuid AND u.pial_id IS NOT NULL
	   AND ($2::uuid IS NULL OR f.follower_id > $2::uuid)
	   AND NOT EXISTS (SELECT 1 FROM blocks b
	                    WHERE (b.blocker_id = u.id AND b.blocked_id = $1::uuid)
	                       OR (b.blocker_id = $1::uuid AND b.blocked_id = u.id))
	   AND NOT EXISTS (SELECT 1 FROM user_mutes m WHERE m.muter_id = u.id AND m.muted_id = $1::uuid)
	   AND ($4 <> 'subscribers' OR EXISTS (SELECT 1 FROM subscriptions s
	                    WHERE s.subscriber_id = u.id AND s.creator_id = $1::uuid AND s.status = 'active'))
	   AND (NOT $5 OR (COALESCE(pr.is_adult, FALSE) AND NOT COALESCE(pr.is_minor, FALSE)
	                    AND COALESCE(p.content_setting, '') = 'adult_enabled'
	                    AND COALESCE(p.official_type, '') NOT IN ('government', 'business')))
	 ORDER BY f.follower_id LIMIT $3`

// RunFanoutBatch advances the oldest open job by one batch and returns who it reached.
// The notifications and the job's position commit together. One worker runs at a time
// across replicas.
func RunFanoutBatch(ctx context.Context, db *sql.DB, title func(FanoutJob) string, url func(FanoutJob) string, check FanoutCheck) (*FanoutJob, []FanoutRecipient, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()
	var locked bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, fanoutLockKey).Scan(&locked); err != nil {
		return nil, nil, err
	}
	if !locked {
		return nil, nil, nil
	}
	defer conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, fanoutLockKey)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	var job FanoutJob
	var cursor sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT id, actor_id::text, actor_handle, kind, target_id, target_type, preview, audience, adult, last_follower::text
		  FROM notification_fanout WHERE done_at IS NULL ORDER BY id LIMIT 1`).Scan(
		&job.ID, &job.ActorID, &job.ActorHandle, &job.Kind, &job.TargetID, &job.TargetType, &job.Preview, &job.Audience, &job.Adult, &cursor)
	if err == sql.ErrNoRows {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}

	var skip map[string]bool
	if check != nil {
		var stop bool
		stop, skip = check(job)
		if stop {
			if _, err := tx.ExecContext(ctx, `UPDATE notification_fanout SET done_at = NOW() WHERE id = $1`, job.ID); err != nil {
				return nil, nil, err
			}
			return nil, nil, tx.Commit()
		}
	}

	capped := strings.HasPrefix(job.Kind, frequencyFanoutKindPrefix)
	var rows *sql.Rows
	if capped {
		rows, err = tx.QueryContext(ctx, frequencyFanoutSQL, job.ActorID, cursor, fanoutBatch, job.Audience, job.Adult)
	} else {
		rows, err = tx.QueryContext(ctx, `
		SELECT f.follower_id::text, u.pial_id::text
		  FROM follows f JOIN users u ON u.id = f.follower_id
		 WHERE f.following_id = $1::uuid AND u.pial_id IS NOT NULL
		   AND ($2::uuid IS NULL OR f.follower_id > $2::uuid)
		 ORDER BY f.follower_id LIMIT $3`, job.ActorID, cursor, fanoutBatch)
	}
	if err != nil {
		return nil, nil, err
	}
	var recipients []FanoutRecipient
	for rows.Next() {
		var r FanoutRecipient
		if err := rows.Scan(&r.Account, &r.PIAL); err != nil {
			rows.Close()
			return nil, nil, err
		}
		recipients = append(recipients, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	// The cursor follows everyone scanned, so a person left out is not asked again.
	var last string
	if len(recipients) > 0 {
		last = recipients[len(recipients)-1].Account
	}
	if capped {
		recipients, err = frequencyAnnounceable(ctx, tx, recipients, skip)
		if err != nil {
			return nil, nil, err
		}
	}

	for _, r := range recipients {
		eventID := FanoutEventID(job.Kind, job.TargetID, r.Account)
		event, err := json.Marshal(map[string]interface{}{
			"event_id":          eventID,
			"recipient_account": r.Account,
			"recipient_pial":    r.PIAL,
			"kind":              job.Kind,
			"actor_account":     job.ActorID,
			"target_id":         job.TargetID,
			"target_type":       job.TargetType,
			"payload":           map[string]string{"preview": job.Preview},
			"channel":           "push_inbox",
			"push":              map[string]string{"title": title(job), "body": job.Preview, "action_url": url(job)},
			"created_at":        time.Now().UTC().Format(time.RFC3339Nano),
		})
		if err != nil {
			return nil, nil, fmt.Errorf("fanout event: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO notification_outbox (event_id, recipient_account, recipient_pial, event)
			VALUES ($1::uuid, $2::uuid, $3, $4::jsonb)
			ON CONFLICT (event_id) DO NOTHING`, eventID, r.Account, r.PIAL, string(event)); err != nil {
			return nil, nil, err
		}
	}

	if last != "" {
		_, err = tx.ExecContext(ctx, `UPDATE notification_fanout SET last_follower = $2::uuid WHERE id = $1`,
			job.ID, last)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE notification_fanout SET done_at = NOW() WHERE id = $1`, job.ID)
	}
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return &job, recipients, nil
}

// frequencyAnnounceable drops anyone already in the room or already told too much,
// and records the rest, in the same transaction as their notifications.
func frequencyAnnounceable(ctx context.Context, tx *sql.Tx, batch []FanoutRecipient, skip map[string]bool) ([]FanoutRecipient, error) {
	if len(batch) == 0 {
		return batch, nil
	}
	accounts := make([]string, 0, len(batch))
	for _, r := range batch {
		accounts = append(accounts, r.Account)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT account_id::text FROM frequency_announced
		 WHERE account_id::text = ANY($1) AND at > NOW() - INTERVAL '24 hours'
		 GROUP BY account_id
		HAVING COUNT(*) >= $2 OR MAX(at) > NOW() - INTERVAL '30 minutes'`, pq.Array(accounts), frequencyAnnounceDailyCap)
	if err != nil {
		return nil, err
	}
	full := map[string]bool{}
	for rows.Next() {
		var account string
		if err := rows.Scan(&account); err != nil {
			rows.Close()
			return nil, err
		}
		full[account] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]FanoutRecipient, 0, len(batch))
	told := make([]string, 0, len(batch))
	for _, r := range batch {
		if full[r.Account] || skip[r.PIAL] {
			continue
		}
		out = append(out, r)
		told = append(told, r.Account)
	}
	if len(told) > 0 {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO frequency_announced (account_id) SELECT unnest($1::uuid[])`, pq.Array(told)); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM frequency_announced WHERE at < NOW() - INTERVAL '2 days'`); err != nil {
		return nil, err
	}
	return out, nil
}

// FollowedAmongPIALs says which of these PIALs an account follows.
func FollowedAmongPIALs(db *sql.DB, followerID string, pials []string) (map[string]bool, error) {
	rows, err := db.Query(`
		SELECT u.pial_id::text FROM follows f
		  JOIN users u ON u.id = f.following_id
		 WHERE f.follower_id = $1::uuid AND u.pial_id::text = ANY($2)`, followerID, pq.Array(pials))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var pial string
		if err := rows.Scan(&pial); err != nil {
			return nil, err
		}
		out[pial] = true
	}
	return out, rows.Err()
}

// RecordFrequencyClip records a clip being made, unless the account has made its limit
// in the last hour. False means the limit was reached and nothing was recorded.
func RecordFrequencyClip(db *sql.DB, uploadID, frequencyID, accountID string, perHour int) (bool, error) {
	recorded := false
	err := WithTx(db, func(tx *sql.Tx) error {
		// One account at a time, so simultaneous requests can't all pass the count.
		if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext($1))`, "frequency_clip:"+accountID); err != nil {
			return err
		}
		res, err := tx.Exec(`
			INSERT INTO frequency_clips (upload_id, frequency_id, account_id)
			SELECT $1, $2::uuid, $3::uuid
			 WHERE (SELECT COUNT(*) FROM frequency_clips
			         WHERE account_id = $3::uuid AND created_at > NOW() - INTERVAL '1 hour') < $4`,
			uploadID, frequencyID, accountID, perHour)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		recorded = n == 1
		return err
	})
	return recorded, err
}

// PrivateAmong says which of these accounts are private.
func PrivateAmong(db *sql.DB, accountIDs []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(accountIDs) == 0 {
		return out, nil
	}
	rows, err := db.Query(`SELECT user_id::text FROM user_profiles WHERE is_private AND user_id::text = ANY($1)`, pq.Array(accountIDs))
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

// FollowedAmong says which of these accounts the viewer follows.
func FollowedAmong(db *sql.DB, followerID string, accountIDs []string) (map[string]bool, error) {
	out := map[string]bool{}
	rows, err := db.Query(`SELECT following_id::text FROM follows WHERE follower_id = $1::uuid AND following_id::text = ANY($2)`, followerID, pq.Array(accountIDs))
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

// UserIDsByHandles maps handles to account ids.
func UserIDsByHandles(db *sql.DB, handles []string) (map[string]string, error) {
	out := map[string]string{}
	rows, err := db.Query(`SELECT handle, id::text FROM users WHERE handle = ANY($1)`, pq.Array(handles))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var handle, id string
		if err := rows.Scan(&handle, &id); err != nil {
			return nil, err
		}
		out[handle] = id
	}
	return out, rows.Err()
}
