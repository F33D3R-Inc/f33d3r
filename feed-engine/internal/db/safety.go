package db

// safety.go — applying Abraxas's verdicts to this brain's content.
//
// Abraxas is the integrity authority; safety.events is how its answers arrive
// (internal/sitra/safety.go). Everything here runs inside one transaction per
// event, together with the event's entry in safety_subject_seq, so an event is
// either wholly applied and recorded or not at all, and a redelivered event —
// one whose seq is not above the subject's recorded seq — changes nothing.
//
// Two kinds of answer reach a work's scan_state, and they are guarded
// differently:
//
//   - verdict: the automated judgement. It applies only while the work is
//     still 'pending'. A moderator's decision, or any earlier verdict, is never
//     overwritten by a verdict that arrives (or arrives again) later.
//   - review_decided: a person's decision, recorded in Abraxas. It applies
//     whatever the state, because a human decision is the last word — and the
//     seq ledger keeps an older decision from replacing a newer one.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/f33d3r/feed-engine/internal/model"
)

// ErrSafetyPermanent marks an event that can never be applied as sent.
var ErrSafetyPermanent = errors.New("safety event can never be applied")

// SafetyApply is one safety.events record, in the form this package applies.
type SafetyApply struct {
	EventID     string
	Seq         int64
	Type        string // verdict | ownership | review_opened | review_decided
	SubjectKind string // work | vision | asset | stream | account
	SubjectID   string
	AssetID     string
	// Verdict is approve | age_gate | human_review | block | pending.
	Verdict string
	// Ownership: the original owner to credit, resolved to the handle they
	// answer to now (empty when they have no account any more).
	OriginalPIAL   string
	OriginalHandle string
	// NotifyAdmins: the event put an item in front of a person (review_opened).
	// The notice id derives from the event id, so a redelivery tells no one
	// twice.
	NotifyAdmins bool
}

// SafetyChange is one work whose card must be redrawn because of an event.
type SafetyChange struct {
	WorkID     string
	AuthorPIAL string
	FromState  string
	ToState    string
	// Credited is set when the work's "Media by" credit changed.
	Credited bool
}

// scanStateOf maps an Abraxas verdict word to a work scan_state. "" means the
// verdict moves nothing (still pending).
func scanStateOf(verdict string) (string, error) {
	switch verdict {
	case "approve":
		return "clean", nil
	case "age_gate":
		return "age_gated", nil
	case "human_review":
		return "human_review", nil
	case "block":
		return "blocked", nil
	case "pending":
		return "", nil
	}
	return "", fmt.Errorf("%w: unknown verdict %q", ErrSafetyPermanent, verdict)
}

// claimSafetySeq locks the subject's ledger row and reports whether seq is new.
func claimSafetySeq(tx *sql.Tx, kind, id string, seq int64) (bool, error) {
	if _, err := tx.Exec(`
		INSERT INTO safety_subject_seq (subject_kind, subject_id, last_seq, last_event_id)
		VALUES ($1, $2, 0, '')
		ON CONFLICT (subject_kind, subject_id) DO NOTHING`, kind, id); err != nil {
		return false, fmt.Errorf("opening the safety ledger of %s %s: %w", kind, id, err)
	}
	var last int64
	if err := tx.QueryRow(`
		SELECT last_seq FROM safety_subject_seq
		 WHERE subject_kind = $1 AND subject_id = $2 FOR UPDATE`, kind, id).Scan(&last); err != nil {
		return false, fmt.Errorf("reading the safety ledger of %s %s: %w", kind, id, err)
	}
	return seq > last, nil
}

func recordSafetySeq(tx *sql.Tx, kind, id string, seq int64, eventID string) error {
	if _, err := tx.Exec(`
		UPDATE safety_subject_seq SET last_seq = $3, last_event_id = $4, applied_at = NOW()
		 WHERE subject_kind = $1 AND subject_id = $2 AND last_seq < $3`, kind, id, seq, eventID); err != nil {
		return fmt.Errorf("recording safety event %s: %w", eventID, err)
	}
	return nil
}

// SafetyEventApplied reports whether an event at seq has already been applied
// to its subject. Used by the actions that run outside ApplySafetyEvent's
// transaction (a banned match's report), which are idempotent themselves.
func SafetyEventApplied(ctx context.Context, database *sql.DB, kind, id string, seq int64) (bool, error) {
	var last int64
	err := database.QueryRowContext(ctx, `
		SELECT last_seq FROM safety_subject_seq WHERE subject_kind = $1 AND subject_id = $2`, kind, id).Scan(&last)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading the safety ledger of %s %s: %w", kind, id, err)
	}
	return seq <= last, nil
}

// MarkSafetyEventApplied records an event applied outside ApplySafetyEvent.
func MarkSafetyEventApplied(ctx context.Context, database *sql.DB, kind, id string, seq int64, eventID string) error {
	if _, err := database.ExecContext(ctx, `
		INSERT INTO safety_subject_seq (subject_kind, subject_id, last_seq, last_event_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (subject_kind, subject_id) DO UPDATE
		   SET last_seq = EXCLUDED.last_seq, last_event_id = EXCLUDED.last_event_id, applied_at = NOW()
		 WHERE safety_subject_seq.last_seq < EXCLUDED.last_seq`, kind, id, seq, eventID); err != nil {
		return fmt.Errorf("recording safety event %s: %w", eventID, err)
	}
	return nil
}

// ApplySafetyEvent applies one verdict, ownership finding or review decision.
// applied is false when the event had already been applied (a redelivery).
func ApplySafetyEvent(ctx context.Context, database *sql.DB, ev SafetyApply) (applied bool, changes []SafetyChange, err error) {
	if ev.Seq <= 0 {
		return false, nil, fmt.Errorf("%w: event %s has no seq", ErrSafetyPermanent, ev.EventID)
	}
	err = WithTx(database, func(tx *sql.Tx) error {
		fresh, err := claimSafetySeq(tx, ev.SubjectKind, ev.SubjectID, ev.Seq)
		if err != nil || !fresh {
			return err
		}
		applied = true
		works, err := safetySubjectWorks(tx, ev)
		if err != nil {
			return err
		}
		switch ev.Type {
		case "verdict", "review_decided":
			human := ev.Type == "review_decided"
			to, err := scanStateOf(ev.Verdict)
			if err != nil {
				return err
			}
			if to != "" {
				if ev.SubjectKind == "vision" {
					if err := applyVisionVerdict(tx, ev.SubjectID, to, human); err != nil {
						return err
					}
				}
				actor := "abraxas"
				reason := "abraxas: " + ev.Verdict
				if human {
					reason = "abraxas review decision: " + ev.Verdict
				}
				for _, w := range works {
					c, changed, err := applyWorkVerdict(tx, w, to, human, reason, actor)
					if err != nil {
						return err
					}
					if changed {
						changes = append(changes, c)
					}
				}
			}
			// A credit decision carries the owner it credits.
			if ev.OriginalPIAL != "" {
				credited, err := applyLineage(tx, works, ev.OriginalPIAL, ev.OriginalHandle)
				if err != nil {
					return err
				}
				changes = mergeCredited(changes, credited)
			}
		case "review_opened":
			// Nothing about the content moves: the item waits for a person,
			// and the admins are told below.
		case "ownership":
			if ev.OriginalPIAL == "" {
				return fmt.Errorf("%w: ownership event %s names no original owner", ErrSafetyPermanent, ev.EventID)
			}
			credited, err := applyLineage(tx, works, ev.OriginalPIAL, ev.OriginalHandle)
			if err != nil {
				return err
			}
			changes = mergeCredited(changes, credited)
		default:
			return fmt.Errorf("%w: event type %q is not applied here", ErrSafetyPermanent, ev.Type)
		}
		if ev.NotifyAdmins {
			if err := notifyReviewers(tx, ev.EventID); err != nil {
				return err
			}
		}
		return recordSafetySeq(tx, ev.SubjectKind, ev.SubjectID, ev.Seq, ev.EventID)
	})
	if err != nil {
		return false, nil, err
	}
	return applied, changes, nil
}

// safetySubjectWorks lists the works an event is about: the work itself, or
// every work that carries the asset. Other subjects carry no work.
func safetySubjectWorks(tx *sql.Tx, ev SafetyApply) ([]string, error) {
	switch ev.SubjectKind {
	case "work":
		if _, err := uuid.Parse(ev.SubjectID); err != nil {
			return nil, fmt.Errorf("%w: work subject %q is not a work id", ErrSafetyPermanent, ev.SubjectID)
		}
		return []string{ev.SubjectID}, nil
	case "asset":
		assetID := ev.SubjectID
		if assetID == "" {
			assetID = ev.AssetID
		}
		if _, err := uuid.Parse(assetID); err != nil {
			return nil, fmt.Errorf("%w: asset subject %q is not an asset id", ErrSafetyPermanent, assetID)
		}
		rows, err := tx.Query(`SELECT DISTINCT work_id::text FROM work_assets WHERE asset_id = $1::uuid`, assetID)
		if err != nil {
			return nil, fmt.Errorf("works carrying asset %s: %w", assetID, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, fmt.Errorf("works carrying asset %s: %w", assetID, err)
			}
			out = append(out, id)
		}
		return out, rows.Err()
	case "vision", "stream", "account":
		return nil, nil
	}
	return nil, fmt.Errorf("%w: unknown subject kind %q", ErrSafetyPermanent, ev.SubjectKind)
}

// applyWorkVerdict moves one work's scan state. An automated verdict applies
// only to a pending work; a human decision applies to any. A work this brain
// no longer holds is not an error: there is nothing left to judge.
func applyWorkVerdict(tx *sql.Tx, workID, to string, human bool, reason, actor string) (SafetyChange, bool, error) {
	c := SafetyChange{WorkID: workID, ToState: to}
	var deleted bool
	err := tx.QueryRow(`
		SELECT scan_state, author_pial::text, deleted_at IS NOT NULL
		  FROM works WHERE id = $1::uuid FOR UPDATE`, workID).Scan(&c.FromState, &c.AuthorPIAL, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return c, false, nil
	}
	if err != nil {
		return c, false, fmt.Errorf("reading work %s for a verdict: %w", workID, err)
	}
	if deleted || c.FromState == to || (!human && c.FromState != "pending") {
		return c, false, nil
	}
	// Moderators are told once per event (notifyReviewers), not once per work.
	if err := applyScanStateTx(tx, workID, to, reason, actor, ScanOutcome{
		NotifyAuthor: to == "blocked" || to == "age_gated",
	}); err != nil {
		return c, false, err
	}
	// is_blocked mirrors a blocked verdict (SetPostScanState sets it, and
	// nothing else does). A person lifting a block lifts it too, or their
	// decision would leave the work hidden.
	if human && c.FromState == "blocked" && to != "blocked" {
		if _, err := tx.Exec(`UPDATE works SET is_blocked = FALSE WHERE id = $1::uuid`, workID); err != nil {
			return c, false, fmt.Errorf("work %s: lifting the block: %w", workID, err)
		}
	}
	return c, true, nil
}

// applyVisionVerdict moves one Vision's scan state, under the same guard.
func applyVisionVerdict(tx *sql.Tx, ref, to string, human bool) error {
	dot := strings.LastIndexByte(ref, '.')
	if dot <= 0 || dot == len(ref)-1 {
		return fmt.Errorf("%w: vision subject %q is not <author_pial>.<seq>", ErrSafetyPermanent, ref)
	}
	pial, seqText := ref[:dot], ref[dot+1:]
	if _, err := uuid.Parse(pial); err != nil {
		return fmt.Errorf("%w: vision subject %q: author is not a PIAL", ErrSafetyPermanent, ref)
	}
	seq, err := strconv.ParseInt(seqText, 10, 64)
	if err != nil || seq <= 0 {
		return fmt.Errorf("%w: vision subject %q: seq is not a positive integer", ErrSafetyPermanent, ref)
	}
	if _, err := tx.Exec(`
		UPDATE visions
		   SET scan_state = $3,
		       is_blocked = CASE WHEN $3 = 'blocked' THEN TRUE
		                         WHEN $4 AND scan_state = 'blocked' THEN FALSE
		                         ELSE is_blocked END,
		       is_nsfw    = is_nsfw OR $3 = 'age_gated'
		 WHERE author_pial = $1::uuid AND seq = $2
		   AND scan_state <> $3
		   AND ($4 OR scan_state = 'pending')`, pial, seq, to, human); err != nil {
		return fmt.Errorf("applying %s to vision %s: %w", to, ref, err)
	}
	return nil
}

// applyLineage records the original owner as the "Media by" credit of every
// work that carries the content, except the original owner's own works.
func applyLineage(tx *sql.Tx, works []string, originalPIAL, originalHandle string) ([]SafetyChange, error) {
	if len(works) == 0 {
		return nil, nil
	}
	if _, err := uuid.Parse(originalPIAL); err != nil {
		return nil, fmt.Errorf("%w: original owner %q is not a PIAL", ErrSafetyPermanent, originalPIAL)
	}
	rows, err := tx.Query(`
		UPDATE works
		   SET lineage_pial = $2, lineage_handle = NULLIF($3, '')
		 WHERE id = ANY($1::uuid[])
		   AND author_pial::text <> $2
		   AND (lineage_pial IS DISTINCT FROM $2 OR lineage_handle IS DISTINCT FROM NULLIF($3, ''))
		RETURNING id::text, author_pial::text, scan_state`, pq.Array(works), originalPIAL, originalHandle)
	if err != nil {
		return nil, fmt.Errorf("recording the original owner's credit: %w", err)
	}
	defer rows.Close()
	var out []SafetyChange
	for rows.Next() {
		c := SafetyChange{Credited: true}
		if err := rows.Scan(&c.WorkID, &c.AuthorPIAL, &c.ToState); err != nil {
			return nil, fmt.Errorf("recording the original owner's credit: %w", err)
		}
		c.FromState = c.ToState
		out = append(out, c)
	}
	return out, rows.Err()
}

// mergeCredited folds credit changes into state changes, one entry per work.
func mergeCredited(changes, credited []SafetyChange) []SafetyChange {
	for _, cr := range credited {
		merged := false
		for i := range changes {
			if changes[i].WorkID == cr.WorkID {
				changes[i].Credited = true
				merged = true
				break
			}
		}
		if !merged {
			changes = append(changes, cr)
		}
	}
	return changes
}

// RecordSafetyDeadLetter keeps a safety.events record that can never be applied.
func RecordSafetyDeadLetter(ctx context.Context, database *sql.DB, topic string, partition int32, offset int64, record []byte, reason string) error {
	_, err := database.ExecContext(ctx, `
		INSERT INTO safety_event_deadletter (topic, partition, "offset", record, reason)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (topic, partition, "offset") DO NOTHING`,
		topic, partition, offset, record, reason)
	return err
}

// ── What the moderation Facets show of a subject ─────────────────────────────

// ModerationWork is what a moderation card shows of a work: its text, its
// author's handle and a preview image. It is read whatever the work's scan
// state, because a moderator judges works nobody else may see yet.
type ModerationWork struct {
	AuthorHandle string
	Body         string
	ThumbURL     string
	ScanState    string
}

// ModerationWorkSubject loads a work for a moderation card; nil when there is
// no such work.
func ModerationWorkSubject(database *sql.DB, workID string) (*ModerationWork, error) {
	if _, err := uuid.Parse(workID); err != nil {
		return nil, nil
	}
	var m ModerationWork
	err := database.QueryRow(`
		SELECT COALESCE(u.handle, ''), COALESCE(w.body, ''),
		       COALESCE(NULLIF(w.video_poster_url, ''), w.media_urls[1], ''),
		       w.scan_state
		  FROM works w
		  LEFT JOIN users u ON u.id = w.author_id
		 WHERE w.id = $1::uuid`, workID).Scan(&m.AuthorHandle, &m.Body, &m.ThumbURL, &m.ScanState)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("loading work %s for moderation: %w", workID, err)
	}
	return &m, nil
}

// AuthorOfWork is the account that wrote a work; nil when there is no such work.
func AuthorOfWork(database *sql.DB, workID string) (*model.User, error) {
	var authorID string
	err := database.QueryRow(`SELECT author_id::text FROM works WHERE id = $1::uuid`, workID).Scan(&authorID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the author of work %s: %w", workID, err)
	}
	u, err := GetUserByID(database, authorID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return u, err
}

// WorkAssetIDs lists the Caeor assets a work carries, so its content event can
// name them to Abraxas: a work's verdict must account for its assets' own.
func WorkAssetIDs(database *sql.DB, workID string) ([]string, error) {
	rows, err := database.Query(`
		SELECT DISTINCT asset_id::text FROM work_assets WHERE work_id = $1::uuid ORDER BY 1`, workID)
	if err != nil {
		return nil, fmt.Errorf("assets of work %s: %w", workID, err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("assets of work %s: %w", workID, err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// notifyReviewers tells every admin an item is waiting in the review queue. The
// notice names no subject (a subject id can be a PIAL); it opens the queue. Its
// event id is derived from the safety event, so a replay tells nobody twice.
func notifyReviewers(tx *sql.Tx, eventID string) error {
	for _, admin := range GetAdminUserIDs(tx) {
		if _, err := WriteNotice(tx, Notice{
			EventID:    "safety-review:" + eventID + ":" + admin,
			Recipient:  admin,
			Kind:       "moderation",
			TargetType: "abraxas_review",
		}); err != nil {
			return fmt.Errorf("telling admin %s about review event %s: %w", admin, eventID, err)
		}
	}
	return nil
}

// NotifyReviewers tells every admin, in one transaction, that an item is
// waiting in the review queue. key makes the notices idempotent.
func NotifyReviewers(database *sql.DB, key string) error {
	return WithTx(database, func(tx *sql.Tx) error { return notifyReviewers(tx, key) })
}

// AuthorPIALOfWork is the identity root of a work's author; "" when there is
// no such work.
func AuthorPIALOfWork(database *sql.DB, workID string) (string, error) {
	if _, err := uuid.Parse(workID); err != nil {
		return "", nil
	}
	var pial sql.NullString
	err := database.QueryRow(`SELECT author_pial::text FROM works WHERE id = $1::uuid`, workID).Scan(&pial)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading the author of work %s: %w", workID, err)
	}
	return pial.String, nil
}

// AuthorPolicyFacts are the facts about an author the integrity policy reads:
// whether they are a verified adult creator, and when their account was made.
type AuthorPolicyFacts struct {
	IsAdultCreator   bool
	AccountCreatedAt time.Time
}

// PolicyFactsOfAccount reads an account's AuthorPolicyFacts. A missing account
// is an error: content always has an author.
func PolicyFactsOfAccount(database *sql.DB, accountID string) (AuthorPolicyFacts, error) {
	var f AuthorPolicyFacts
	err := database.QueryRow(`
		SELECT COALESCE(up.is_adult_creator, FALSE), u.created_at
		  FROM users u
		  LEFT JOIN user_profiles up ON up.user_id = u.id
		 WHERE u.id = $1::uuid`, accountID).Scan(&f.IsAdultCreator, &f.AccountCreatedAt)
	if err != nil {
		return f, fmt.Errorf("policy facts of account %s: %w", accountID, err)
	}
	return f, nil
}
