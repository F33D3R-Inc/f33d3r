package db

import (
	"database/sql"
	"encoding/json"
	"log"
	"time"

	"github.com/google/uuid"
)

// DBTX is what both a pool and an open transaction can do, so a write and the
// notification it owes can commit together.
type DBTX interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
	Query(query string, args ...interface{}) (*sql.Rows, error)
	QueryRow(query string, args ...interface{}) *sql.Row
}

func nilDB(q DBTX) bool {
	if q == nil {
		return true
	}
	d, ok := q.(*sql.DB)
	return ok && d == nil
}

// WithTx runs fn in one transaction. Any error rolls everything back.
func WithTx(database *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// noticeNamespace derives stable event IDs for work that may run more than once.
var noticeNamespace = uuid.MustParse("3d1f7a64-5b0e-4c8f-a1d2-9e6b4f7c2a18")

// Notice is one notification owed to one account.
type Notice struct {
	// EventID is optional. Set it when a retry of the same work must not notify twice.
	EventID    string
	Recipient  string
	Kind       string
	Actor      string
	TargetID   string
	TargetType string
	Payload    map[string]interface{}
	PushTitle  string
	PushBody   string
	PushIcon   string
	PushURL    string
	// PushOnly sends a push with no inbox entry, such as a new-message alert.
	PushOnly bool
}

// WriteNotice queues a notice on a pool or inside the caller's transaction, and
// reports whether anything was queued. A recipient with no identity cannot be
// reached, and that is not a reason to undo the action that named them.
func WriteNotice(q DBTX, n Notice) (bool, error) {
	if n.Actor != "" && n.Actor == n.Recipient {
		return false, nil
	}
	var pial string
	if err := q.QueryRow(`SELECT COALESCE(pial_id::text,'') FROM users WHERE id = $1::uuid`, n.Recipient).Scan(&pial); err != nil {
		if err == sql.ErrNoRows {
			log.Printf("[notify] %s for %s: no such account", n.Kind, n.Recipient)
			return false, nil
		}
		return false, err
	}
	if pial == "" {
		log.Printf("[notify] %s for %s: account has no identity", n.Kind, n.Recipient)
		return false, nil
	}
	if n.EventID == "" {
		n.EventID = uuid.NewString()
	}
	if n.Payload == nil {
		n.Payload = map[string]interface{}{}
	}
	channel := "inbox"
	var push map[string]string
	if n.PushTitle != "" {
		channel = "push_inbox"
		if n.PushOnly {
			channel = "push"
		}
		push = map[string]string{"title": n.PushTitle, "body": n.PushBody, "icon_url": n.PushIcon, "action_url": n.PushURL}
	}
	body := map[string]interface{}{
		"event_id":          n.EventID,
		"recipient_account": n.Recipient,
		"recipient_pial":    pial,
		"kind":              n.Kind,
		"actor_account":     n.Actor,
		"target_id":         n.TargetID,
		"target_type":       n.TargetType,
		"payload":           n.Payload,
		"channel":           channel,
		"created_at":        time.Now().UTC().Format(time.RFC3339Nano),
	}
	if push != nil {
		body["push"] = push
	}
	event, err := json.Marshal(body)
	if err != nil {
		return false, err
	}
	res, err := q.Exec(`
		INSERT INTO notification_outbox (event_id, recipient_account, recipient_pial, event)
		VALUES ($1::uuid, $2::uuid, $3, $4::jsonb)
		ON CONFLICT (event_id) DO NOTHING`, n.EventID, n.Recipient, pial, string(event))
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// FollowUserNotifying records a follow and tells the person followed, together.
func FollowUserNotifying(database *sql.DB, followerID, followingID, followerHandle string) (bool, error) {
	changed := false
	err := WithTx(database, func(tx *sql.Tx) error {
		res, err := tx.Exec(`
			INSERT INTO follows (follower_id, following_id)
			VALUES ($1::uuid, $2::uuid) ON CONFLICT DO NOTHING`, followerID, followingID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		changed = n > 0
		if !changed {
			return nil
		}
		_, err = WriteNotice(tx, Notice{
			Recipient: followingID, Kind: "follow", Actor: followerID,
			TargetID: followerID, TargetType: "user",
			PushTitle: "@" + followerHandle + " followed you", PushURL: "/@" + followerHandle,
		})
		return err
	})
	return changed, err
}

// SetWorkReaction adds or removes a reaction and reports whether it changed. A new
// like or repost tells the author in the same transaction. A dislike and a bookmark
// tell nobody, and neither does taking a reaction back.
func SetWorkReaction(database *sql.DB, workID, reactorID, reactorHandle, reactionType string, add bool) (bool, error) {
	changed := false
	err := WithTx(database, func(tx *sql.Tx) error {
		var res sql.Result
		var err error
		if add {
			res, err = tx.Exec(`
				INSERT INTO work_reactions (work_id, reactor_id, reaction_type)
				VALUES ($1::uuid, $2::uuid, $3) ON CONFLICT DO NOTHING`, workID, reactorID, reactionType)
		} else {
			res, err = tx.Exec(`
				DELETE FROM work_reactions
				 WHERE work_id = $1::uuid AND reactor_id = $2::uuid AND reaction_type = $3`,
				workID, reactorID, reactionType)
		}
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		changed = n > 0
		if !changed || !add {
			return nil
		}
		title := ""
		switch reactionType {
		case "like":
			title = "@" + reactorHandle + " liked your work"
		case "repost":
			title = "@" + reactorHandle + " reposted your work"
		default:
			return nil
		}
		var authorID string
		if err := tx.QueryRow(`SELECT author_id::text FROM works WHERE id = $1::uuid`, workID).Scan(&authorID); err != nil {
			if err == sql.ErrNoRows {
				return nil
			}
			return err
		}
		_, err = WriteNotice(tx, Notice{
			Recipient: authorID, Kind: reactionType, Actor: reactorID,
			TargetID: workID, TargetType: "work",
			PushTitle: title, PushURL: "/work/" + workID,
		})
		return err
	})
	return changed, err
}

// CreateOrgMembershipNotifying opens a membership and tells the other side. A
// membership that already exists tells nobody.
func CreateOrgMembershipNotifying(database *sql.DB, userID, orgUserID, initiatedBy string, notice Notice) error {
	return WithTx(database, func(tx *sql.Tx) error {
		res, err := createOrgMembership(tx, userID, orgUserID, initiatedBy)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		_, err = WriteNotice(tx, notice)
		return err
	})
}

// ScanOutcome says who a change of scan state owes a word to.
type ScanOutcome struct {
	NotifyModerators bool
	NotifyAuthor     bool
}

// ApplyScanState moves a work to a new scan state and queues what that owes people,
// in one transaction.
func ApplyScanState(database *sql.DB, postID, toState, reason, actor string, outcome ScanOutcome) error {
	return WithTx(database, func(tx *sql.Tx) error {
		if err := SetPostScanState(tx, postID, toState, reason, actor); err != nil {
			return err
		}
		if outcome.NotifyModerators {
			for _, moderator := range GetAdminUserIDs(tx) {
				if _, err := WriteNotice(tx, Notice{
					Recipient: moderator, Kind: "moderation", TargetID: postID, TargetType: "post",
				}); err != nil {
					return err
				}
			}
		}
		if outcome.NotifyAuthor {
			var authorID string
			err := tx.QueryRow(`SELECT author_id::text FROM works WHERE id = $1`, postID).Scan(&authorID)
			if err == sql.ErrNoRows {
				return nil
			}
			if err != nil {
				return err
			}
			if _, err := WriteNotice(tx, Notice{
				Recipient: authorID, Kind: "violation", TargetID: postID, TargetType: "post",
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetPIALVerifiedNotifying records an age verification and tells the account.
func SetPIALVerifiedNotifying(database *sql.DB, pialID, tier, grantedBy, accountID string) error {
	return WithTx(database, func(tx *sql.Tx) error {
		if err := SetPIALVerified(tx, pialID, tier, grantedBy); err != nil {
			return err
		}
		_, err := WriteNotice(tx, Notice{
			Recipient: accountID, Kind: "verification", TargetID: pialID, TargetType: "identity",
		})
		return err
	})
}

// MessageAlerts queues a push for everyone else in a conversation, on the message's
// own transaction. The text never carries message content, because push passes
// through outside servers.
func MessageAlerts(convoID, senderID string) func(*sql.Tx) error {
	return func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT account_id::text FROM gnosis_members WHERE conversation_id = $1`, convoID)
		if err != nil {
			return err
		}
		var members []string
		for rows.Next() {
			var account string
			if err := rows.Scan(&account); err != nil {
				rows.Close()
				return err
			}
			members = append(members, account)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, account := range members {
			if account == senderID {
				continue
			}
			if _, err := WriteNotice(tx, Notice{
				Recipient: account, Kind: "dm", PushTitle: "New message", PushURL: "/messages", PushOnly: true,
			}); err != nil {
				return err
			}
		}
		return nil
	}
}

// ── Notifications owed by a new work ─────────────────────────────────────────

// WorkNoticeJob is a work that has not yet told the people it names.
type WorkNoticeJob struct {
	WorkID      string
	AuthorID    string
	AuthorHandle string
	Kind        string
	Body        string
}

// WorkNoticeCited is a work this one replies to or quotes.
type WorkNoticeCited struct {
	WorkID   string
	AuthorID string
}

// DueWorkNoticeJobs returns works whose notifications are due. A deleted work owes
// nothing and comes back with no author, so its job can be closed.
func DueWorkNoticeJobs(database *sql.DB, limit int) ([]WorkNoticeJob, error) {
	rows, err := database.Query(`
		SELECT j.work_id::text, COALESCE(w.author_id::text,''), COALESCE(u.handle,''),
		       COALESCE(w.kind,''), COALESCE(w.body,'')
		  FROM notification_work_jobs j
		  LEFT JOIN works w ON w.id = j.work_id AND w.deleted_at IS NULL
		  LEFT JOIN users u ON u.id = w.author_id
		 WHERE j.next_attempt_at <= NOW()
		 ORDER BY j.next_attempt_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []WorkNoticeJob
	for rows.Next() {
		var j WorkNoticeJob
		if err := rows.Scan(&j.WorkID, &j.AuthorID, &j.AuthorHandle, &j.Kind, &j.Body); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// WorkNoticeCitedWork returns the work a reply or quote cites, if any.
func WorkNoticeCitedWork(database *sql.DB, workID, citationType string) (WorkNoticeCited, bool, error) {
	var c WorkNoticeCited
	err := database.QueryRow(`
		SELECT t.id::text, t.author_id::text
		  FROM work_citations c JOIN works t ON t.id = c.target_id
		 WHERE c.work_id = $1::uuid AND c.citation_type = $2 AND t.deleted_at IS NULL
		 LIMIT 1`, workID, citationType).Scan(&c.WorkID, &c.AuthorID)
	if err == sql.ErrNoRows {
		return c, false, nil
	}
	return c, err == nil, err
}

// WorkNoticeEventID is stable, so a job retried after a crash cannot tell anyone twice.
func WorkNoticeEventID(workID, recipient string) string {
	return uuid.NewSHA1(noticeNamespace, []byte("work|"+workID+"|"+recipient)).String()
}

// FinishWorkNoticeJob queues the notices and closes the job together. Two replicas
// may work the same job; whichever commits second finds it closed and adds nothing.
func FinishWorkNoticeJob(database *sql.DB, workID string, notices []Notice) error {
	return WithTx(database, func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM notification_work_jobs WHERE work_id = $1::uuid`, workID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		for _, notice := range notices {
			if _, err := WriteNotice(tx, notice); err != nil {
				return err
			}
		}
		return nil
	})
}

// RetryWorkNoticeJob backs a job off after a failure, up to five minutes.
func RetryWorkNoticeJob(database *sql.DB, workID string) error {
	_, err := database.Exec(`
		UPDATE notification_work_jobs
		   SET attempts = attempts + 1,
		       next_attempt_at = NOW() + LEAST(POWER(2, attempts) * INTERVAL '1 second', INTERVAL '5 minutes')
		 WHERE work_id = $1::uuid`, workID)
	return err
}

// ── Video processing ─────────────────────────────────────────────────────────

// VideoJob is a video whose owner has not yet heard how processing ended.
type VideoJob struct {
	UploadID string
	Account  string
	PIAL     string
	Filename string
	JobID    string
	Age      time.Duration
}

// OpenVideoJob records a video before it goes to the transcoder. A handle with no
// account returns no job, because there is nobody to tell.
func OpenVideoJob(database *sql.DB, uploadID, handle, pial, filename string) (*VideoJob, error) {
	job := &VideoJob{UploadID: uploadID, PIAL: pial, Filename: filename}
	err := database.QueryRow(`
		INSERT INTO video_jobs (upload_id, account_id, pial_id, filename)
		SELECT $1, id, $3, $4 FROM users WHERE handle = $2
		ON CONFLICT (upload_id) DO UPDATE SET filename = EXCLUDED.filename
		RETURNING account_id::text`, uploadID, handle, pial, filename).Scan(&job.Account)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return job, nil
}

// SetVideoJobID records the transcoder's job for a video.
func SetVideoJobID(database *sql.DB, uploadID, jobID string) error {
	_, err := database.Exec(`UPDATE video_jobs SET job_id = $2 WHERE upload_id = $1`, uploadID, jobID)
	return err
}

// OpenVideoJobs returns every video still being processed.
func OpenVideoJobs(database *sql.DB) ([]VideoJob, error) {
	rows, err := database.Query(`
		SELECT upload_id, account_id::text, pial_id, filename, COALESCE(job_id,''),
		       EXTRACT(EPOCH FROM NOW() - created_at)::float8
		  FROM video_jobs ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []VideoJob
	for rows.Next() {
		var j VideoJob
		var ageSeconds float64
		if err := rows.Scan(&j.UploadID, &j.Account, &j.PIAL, &j.Filename, &j.JobID, &ageSeconds); err != nil {
			return nil, err
		}
		j.Age = time.Duration(ageSeconds * float64(time.Second))
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// CloseVideoJob ends a job with no word to its owner, such as a duplicate the upload
// response already explained.
func CloseVideoJob(database *sql.DB, uploadID string) error {
	_, err := database.Exec(`DELETE FROM video_jobs WHERE upload_id = $1`, uploadID)
	return err
}

// FinishVideoJob closes the job and tells its owner, together. False means another
// replica already did.
func FinishVideoJob(database *sql.DB, job VideoJob, ready bool) (bool, error) {
	finished := false
	err := WithTx(database, func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM video_jobs WHERE upload_id = $1`, job.UploadID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		finished = true
		kind, title := "video_failed", "Your video could not be processed"
		if ready {
			kind, title = "video_ready", "Your video is ready"
		}
		_, err = WriteNotice(tx, Notice{
			Recipient: job.Account, Kind: kind, TargetID: job.UploadID, TargetType: "upload",
			Payload:   map[string]interface{}{"preview": job.Filename},
			PushTitle: title, PushBody: job.Filename, PushURL: "/create",
		})
		return err
	})
	return finished, err
}
