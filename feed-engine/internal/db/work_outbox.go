package db

// work_outbox.go — the durable obligations of a work.
//
// A work owes things to the rest of the platform: a moderation scan, its
// followers' live feeds, the counts of the work it cites, a catalog entry, a link
// preview, a voice analysis, interest learning, achievements. Each is a
// work_outbox row written in the transaction that creates the work (InsertWork)
// or releases it (ReleaseScheduledWorks), so the commit that acknowledges the
// work is also the record of everything it is owed. A worker in the handler
// package drains the rows; nothing here performs an action.
//
// Obligations come in two phases, and the phase is the whole of the difference
// between an immediate and a scheduled work:
//
//	created    owed the moment the work exists, published or not:
//	           content_event (the scan must have its verdict before release),
//	           link_preview, zior_voice
//	published  owed when the work becomes visible: follower_fanout,
//	           cited_update, interest_learning, achievements,
//	           alexandria_catalog, and the notification job
//	migration  media_bind, owed once by every work created before Caeor
//	           became the media authority (queued by 0037)
//
// An immediate work is published in its create transaction; a scheduled one in
// the release transaction. Both go through enqueuePublished, so there is one
// publish path.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// WorkObligation is the closed vocabulary of work_outbox.action. Mirrored by the
// work_outbox_action_vocabulary CHECK in migration 0035; change one and the other
// must change with it.
type WorkObligation string

const (
	ObligationContentEvent      WorkObligation = "content_event"
	ObligationFollowerFanout    WorkObligation = "follower_fanout"
	ObligationCitedUpdate       WorkObligation = "cited_update"
	ObligationAlexandriaCatalog WorkObligation = "alexandria_catalog"
	ObligationLinkPreview       WorkObligation = "link_preview"
	ObligationZiorVoice         WorkObligation = "zior_voice"
	ObligationInterestLearning  WorkObligation = "interest_learning"
	ObligationAchievements      WorkObligation = "achievements"
	// ObligationMediaBind re-binds a work created before Caeor became the media
	// authority: every legacy media path is resolved to its Caeor asset, the
	// work is rewritten to the watermarked objects, and its work_assets and
	// "Media by" credit are recorded. Queued once, by migration 0037; a work
	// created since is bound in its own create transaction.
	ObligationMediaBind WorkObligation = "media_bind"
)

// WorkOutboxMaxAttempts is the attempt at which a failing row is quarantined.
// Backoff doubles from one second and is capped at an hour, so a row is retried
// for a little over an hour before a person is asked to look at it.
const WorkOutboxMaxAttempts = 12

// workOutboxDedupKey names one obligation of one work. A replayed enqueue of the
// same pair is a no-op.
func workOutboxDedupKey(action WorkObligation, workID string) string {
	return string(action) + ":" + workID
}

func enqueueObligation(tx *sql.Tx, workID string, action WorkObligation) error {
	if _, err := tx.Exec(`
		INSERT INTO work_outbox (work_id, action, dedup_key)
		VALUES ($1::uuid, $2, $3)
		ON CONFLICT (dedup_key) DO NOTHING`,
		workID, string(action), workOutboxDedupKey(action, workID)); err != nil {
		return fmt.Errorf("queueing %s for work %s: %w", action, workID, err)
	}
	return nil
}

// enqueueCreated records what a work is owed the moment it exists.
func enqueueCreated(tx *sql.Tx, workID string, hasVoice bool) error {
	actions := []WorkObligation{ObligationContentEvent, ObligationLinkPreview}
	if hasVoice {
		actions = append(actions, ObligationZiorVoice)
	}
	for _, a := range actions {
		if err := enqueueObligation(tx, workID, a); err != nil {
			return err
		}
	}
	return nil
}

// enqueuePublished records what a work is owed when it becomes visible. It reads
// the facts it decides from out of the row itself, so the create path and the
// release path cannot disagree about them.
func enqueuePublished(tx *sql.Tx, workID string) error {
	var isReply, cites, mentions bool
	err := tx.QueryRow(`
		SELECT w.is_reply,
		       EXISTS (SELECT 1 FROM work_citations c WHERE c.work_id = w.id),
		       strpos(w.body, '@') > 0
		  FROM works w
		 WHERE w.id = $1::uuid`, workID).Scan(&isReply, &cites, &mentions)
	if err != nil {
		return fmt.Errorf("reading publish facts of work %s: %w", workID, err)
	}
	actions := []WorkObligation{ObligationAchievements, ObligationAlexandriaCatalog}
	if !isReply {
		actions = append(actions, ObligationFollowerFanout)
	}
	if cites {
		actions = append(actions, ObligationCitedUpdate, ObligationInterestLearning)
	}
	for _, a := range actions {
		if err := enqueueObligation(tx, workID, a); err != nil {
			return err
		}
	}
	// The people a work replies to, quotes or names are owed a notification;
	// notification_work_jobs is that debt's own table (0028).
	if cites || mentions {
		if _, err := tx.Exec(`
			INSERT INTO notification_work_jobs (work_id) VALUES ($1::uuid)
			ON CONFLICT (work_id) DO NOTHING`, workID); err != nil {
			return fmt.Errorf("queueing notifications for work %s: %w", workID, err)
		}
	}
	return nil
}

// ReleaseScheduledWorks publishes every scheduled work whose time has come, and
// in the same transaction records what each is owed on publication — exactly
// what an immediate work records in its create transaction. Replicas may run it
// concurrently: SKIP LOCKED gives each release to one of them.
func ReleaseScheduledWorks(database *sql.DB, limit int) (int, error) {
	released := 0
	err := WithTx(database, func(tx *sql.Tx) error {
		rows, err := tx.Query(`
			UPDATE works w
			   SET scheduled_at = NULL
			  FROM (SELECT id FROM works
			         WHERE scheduled_at IS NOT NULL
			           AND scheduled_at <= NOW()
			           AND deleted_at IS NULL
			         ORDER BY scheduled_at, id
			         LIMIT $1
			         FOR UPDATE SKIP LOCKED) due
			 WHERE w.id = due.id
			RETURNING w.id::text`, limit)
		if err != nil {
			return fmt.Errorf("releasing scheduled works: %w", err)
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return fmt.Errorf("releasing scheduled works: %w", err)
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("releasing scheduled works: %w", err)
		}
		for _, id := range ids {
			if err := enqueuePublished(tx, id); err != nil {
				return err
			}
		}
		released = len(ids)
		return nil
	})
	return released, err
}

// WorkOutboxRow is one claimed obligation. Attempt counts this claim.
type WorkOutboxRow struct {
	ID      int64
	WorkID  string
	Action  WorkObligation
	Attempt int
}

// awaitingVerdict are the obligations that show a work to other people, and so
// are not due while its verdict is pending.
var awaitingVerdict = []string{string(ObligationFollowerFanout), string(ObligationAlexandriaCatalog)}

// ClaimWorkOutbox takes up to limit due rows whose action is in actions, and
// holds each for lease: the claim moves next_attempt_at past the lease and counts
// the attempt, so another replica will not take the row while it is being worked
// and a replica that dies mid-action gives it back when the lease runs out.
func ClaimWorkOutbox(database *sql.DB, actions []WorkObligation, limit int, lease time.Duration) ([]WorkOutboxRow, error) {
	if len(actions) == 0 {
		return nil, nil
	}
	names := make([]string, len(actions))
	for i, a := range actions {
		names[i] = string(a)
	}
	rows, err := database.Query(`
		UPDATE work_outbox o
		   SET attempts = o.attempts + 1,
		       next_attempt_at = NOW() + make_interval(secs => $3)
		  FROM (SELECT id FROM work_outbox
		         WHERE done_at IS NULL AND quarantined_at IS NULL
		           AND next_attempt_at <= NOW()
		           AND action = ANY($1::text[])
		           -- What puts a work in front of other people waits for its
		           -- verdict: a pending work is its author's alone. The row is not
		           -- due, so it spends no attempts while it waits, and is claimed
		           -- on the first tick after the verdict lands.
		           AND NOT (action = ANY($4::text[]) AND EXISTS (
		               SELECT 1 FROM works w
		                WHERE w.id = work_outbox.work_id AND w.scan_state = 'pending'))
		         ORDER BY next_attempt_at, id
		         LIMIT $2
		         FOR UPDATE SKIP LOCKED) due
		 WHERE o.id = due.id
		RETURNING o.id, o.work_id::text, o.action, o.attempts`,
		pq.Array(names), limit, lease.Seconds(), pq.Array(awaitingVerdict))
	if err != nil {
		return nil, fmt.Errorf("claiming work obligations: %w", err)
	}
	defer rows.Close()
	var out []WorkOutboxRow
	for rows.Next() {
		var r WorkOutboxRow
		var action string
		if err := rows.Scan(&r.ID, &r.WorkID, &action, &r.Attempt); err != nil {
			return nil, fmt.Errorf("claiming work obligations: %w", err)
		}
		r.Action = WorkObligation(action)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claiming work obligations: %w", err)
	}
	return out, nil
}

// CompleteWorkOutbox closes an obligation that was met, or that no longer exists
// to be met (its work was deleted).
func CompleteWorkOutbox(database *sql.DB, id int64) error {
	if _, err := database.Exec(
		`UPDATE work_outbox SET done_at = NOW(), last_error = NULL WHERE id = $1 AND done_at IS NULL`, id); err != nil {
		return fmt.Errorf("closing work obligation %d: %w", id, err)
	}
	return nil
}

// FailWorkOutbox records why an attempt failed and schedules the next one, or
// quarantines the row once it has used WorkOutboxMaxAttempts. Reports whether the
// row was quarantined.
func FailWorkOutbox(database *sql.DB, id int64, cause error) (bool, error) {
	reason := cause.Error()
	if len(reason) > 2000 {
		reason = reason[:2000]
	}
	var quarantined bool
	err := database.QueryRow(`
		UPDATE work_outbox
		   SET last_error = $2,
		       next_attempt_at = NOW() + LEAST(POWER(2, LEAST(attempts, 20)) * INTERVAL '1 second', INTERVAL '1 hour'),
		       quarantined_at = CASE WHEN attempts >= $3 THEN NOW() END,
		       quarantine_reason = CASE WHEN attempts >= $3 THEN $2 END
		 WHERE id = $1 AND done_at IS NULL
		RETURNING quarantined_at IS NOT NULL`, id, reason, WorkOutboxMaxAttempts).Scan(&quarantined)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // closed by another replica in the meantime
	}
	if err != nil {
		return false, fmt.Errorf("rescheduling work obligation %d: %w", id, err)
	}
	return quarantined, nil
}

// QuarantinedWorkOutbox counts obligations that did not happen and are waiting
// for a person, by action.
func QuarantinedWorkOutbox(database *sql.DB) (map[WorkObligation]int, error) {
	rows, err := database.Query(`
		SELECT action, COUNT(*) FROM work_outbox
		 WHERE quarantined_at IS NOT NULL AND done_at IS NULL
		 GROUP BY action`)
	if err != nil {
		return nil, fmt.Errorf("counting quarantined work obligations: %w", err)
	}
	defer rows.Close()
	out := map[WorkObligation]int{}
	for rows.Next() {
		var action string
		var n int
		if err := rows.Scan(&action, &n); err != nil {
			return nil, fmt.Errorf("counting quarantined work obligations: %w", err)
		}
		out[WorkObligation(action)] = n
	}
	return out, rows.Err()
}

// PruneWorkOutbox removes rows finished before the cutoff.
func PruneWorkOutbox(database *sql.DB, before time.Time) (int64, error) {
	res, err := database.Exec(`DELETE FROM work_outbox WHERE done_at IS NOT NULL AND done_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("pruning work obligations: %w", err)
	}
	return res.RowsAffected()
}

// WorkSubject is what an obligation's action reads about its work, read fresh
// for every attempt so an action never works from a stale copy.
type WorkSubject struct {
	ID             string
	AuthorID       string
	AuthorPIAL     string
	AuthorHandle   string
	Kind           string
	Body           string
	IsReply        bool
	MediaURLs      []string
	VoiceURL       string
	VideoMasterURL string
	HasPoll        bool
	IsNSFW         bool
	SubscriberOnly bool
	Tags           []string
	Deleted        bool
	// Unpublished is true while a scheduled work waits for its release.
	Unpublished bool
}

// HasMedia reports whether the work carries anything to scan beyond its text.
func (s *WorkSubject) HasMedia() bool {
	return len(s.MediaURLs) > 0 || s.VoiceURL != "" || s.VideoMasterURL != ""
}

// LoadWorkSubject reads a work for an obligation. nil, nil when the row is gone.
func LoadWorkSubject(database *sql.DB, workID string) (*WorkSubject, error) {
	var s WorkSubject
	var media, tags pq.StringArray
	err := database.QueryRow(`
		SELECT w.id::text, w.author_id::text, w.author_pial::text, COALESCE(u.handle, ''),
		       w.kind, w.body, w.is_reply, w.media_urls,
		       COALESCE(w.voice_url, ''), COALESCE(w.video_master_url, ''),
		       COALESCE(cardinality(w.poll_options), 0) > 0,
		       w.is_nsfw, w.subscriber_only, w.tags,
		       w.deleted_at IS NOT NULL,
		       (w.scheduled_at IS NOT NULL AND w.scheduled_at > NOW())
		  FROM works w
		  LEFT JOIN users u ON u.id = w.author_id
		 WHERE w.id = $1::uuid`, workID).Scan(
		&s.ID, &s.AuthorID, &s.AuthorPIAL, &s.AuthorHandle,
		&s.Kind, &s.Body, &s.IsReply, &media,
		&s.VoiceURL, &s.VideoMasterURL, &s.HasPoll,
		&s.IsNSFW, &s.SubscriberOnly, &tags,
		&s.Deleted, &s.Unpublished)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("loading work %s for its obligations: %w", workID, err)
	}
	s.MediaURLs = []string(media)
	s.Tags = []string(tags)
	return &s, nil
}

// CitationTarget is a work another work cites, as the citing work's obligations
// need it.
type CitationTarget struct {
	WorkID   string
	AuthorID string
	Type     string // reply | quote
}

// WorkCitationTargets returns the live works workID cites.
func WorkCitationTargets(database *sql.DB, workID string) ([]CitationTarget, error) {
	rows, err := database.Query(`
		SELECT t.id::text, t.author_id::text, c.citation_type
		  FROM work_citations c
		  JOIN works t ON t.id = c.target_id
		 WHERE c.work_id = $1::uuid AND t.deleted_at IS NULL
		 ORDER BY c.citation_type`, workID)
	if err != nil {
		return nil, fmt.Errorf("reading citations of work %s: %w", workID, err)
	}
	defer rows.Close()
	var out []CitationTarget
	for rows.Next() {
		var t CitationTarget
		if err := rows.Scan(&t.WorkID, &t.AuthorID, &t.Type); err != nil {
			return nil, fmt.Errorf("reading citations of work %s: %w", workID, err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
