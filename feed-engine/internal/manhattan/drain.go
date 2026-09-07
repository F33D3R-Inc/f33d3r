package manhattan

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// The outbox drain.
//
// Registrations are written into manhattan_outbox by triggers, inside the same
// transaction as the row that caused them (migration 0005). This drain is the
// other half: it delivers them to Manhattan, in order, and does not lose them.
//
// Three properties matter more than throughput here:
//
//	Order. A node must exist before an edge can point at it, and a name must
//	bind to a node that exists. So the drain works strictly in id order and
//	stops the batch on the first failure rather than skipping past it. Head-of-
//	line blocking is the correct behaviour for a plane where later rows depend
//	on earlier ones — running ahead would produce edges referring to nodes that
//	were never created.
//
//	Durability. A row is marked delivered only after Manhattan has said so —
//	applied, or already true. A failure records the error and backs off; it
//	never silently drops, and — the harder half — it never silently succeeds.
//	Whether a refusal means "already done" is Manhattan's call, answered per
//	row by /v1/apply, never inferred here from a status code.
//
//	Liveness. Head-of-line blocking stops being correct the moment a row cannot
//	land at all: then it is not ordering the queue, it is ending it. A row
//	Manhattan has permanently refused, or one that Manhattan has sent back
//	maxAttempts times, is quarantined (migration 0012) — kept, loud, and out
//	of the way, so everything behind it moves again. Only an ANSWER from
//	Manhattan counts toward that: an outage — the plane unreachable, a server
//	fault, a rejected key — says nothing about the row, holds the head, and is
//	not counted, or a seventy-minute outage would quarantine whichever write
//	was first in line when it began.
//
//	One drain at a time. Ordering is per queue, so two replicas of this brain
//	draining the same outbox would interleave an assert and a retract of the
//	same association and leave the wrong one standing. DrainOnce takes a
//	session advisory lock and a replica that does not get it does nothing
//	this tick. SKIP LOCKED would be the wrong tool: it hands out rows in
//	whatever order they are free, which is exactly not the order.

const (
	drainInterval = 10 * time.Second
	drainBatch    = 200
	maxAttempts   = 12
	// drainLockKey is the session advisory lock one draining process holds for
	// the duration of a DrainOnce. Same space as the migrator's 7331_0001 in
	// internal/db/migrate.go and distinct from it by construction.
	drainLockKey int64 = 7331_0003
)

type outboxRow struct {
	id       int64
	op       string
	payload  json.RawMessage
	attempts int
}

// failure is why a row did not deliver, and what that means for the queue.
//
// permanent means Manhattan has stated the write will not be accepted by any
// retry this queue could schedule; retrying is not a plan and blocking the
// queue behind it forever is not one either, so the row is quarantined at once
// rather than an hour later.
//
// unreachable means Manhattan did not answer about this write at all: the plane
// could not be reached, it answered with a server fault, or it refused this
// brain's key. That is a fact about the deployment, not about the row, so the
// row keeps the head of the queue and the attempt is NOT counted.
//
// Neither means Manhattan answered and the answer was not yet the end state:
// a node not created yet, a name held by the wrong node, a refusal that clears
// once something else is fixed. Back off, try again, and count the attempt.
type failure struct {
	msg         string
	permanent   bool
	unreachable bool
}

func (f *failure) Error() string { return f.msg }

func retryable(format string, a ...interface{}) *failure {
	return &failure{msg: fmt.Sprintf(format, a...)}
}

func permanent(format string, a ...interface{}) *failure {
	return &failure{msg: fmt.Sprintf(format, a...), permanent: true}
}

// StartDrain runs the outbox drain until ctx is cancelled. Safe to run in more
// than one process: DrainOnce holds an advisory lock while it runs, so only one
// replica drains at a time and the queue's order is never interleaved.
func StartDrain(ctx context.Context, database *sql.DB, client *Client) {
	if database == nil {
		return
	}
	if !client.Configured() {
		// Loud, once, and then quiet. The outbox keeps accumulating and will be
		// delivered whole when Manhattan is configured — nothing is lost by
		// running without it, but nothing is resolved either.
		log.Println("[manhattan] MANHATTAN_URL is not set: graph registrations are being queued but not delivered")
		return
	}

	log.Printf("[manhattan] outbox drain started (every %s, batches of %d)", drainInterval, drainBatch)
	tick := time.NewTicker(drainInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("[manhattan] outbox drain stopped")
			return
		case <-tick.C:
			delivered, err := DrainOnce(ctx, database, client)
			if err != nil {
				log.Printf("[manhattan] outbox drain: %v", err)
			} else if delivered > 0 {
				log.Printf("[manhattan] outbox drain: delivered %d", delivered)
			}
		}
	}
}

// DrainOnce drains the queue until it is empty or a row holds the head, and
// returns how many rows it landed. Exported so a boot-time or operational run
// can force delivery without waiting for a tick.
//
// It runs under the drain lock. A replica that finds the lock held delivers
// nothing this tick and returns (0, nil): the other replica is draining, and
// ordering is its responsibility.
//
// It keeps going while batches come back full. A tick that delivered one batch
// of 200 and then waited ten seconds for the next capped the drain at twenty
// rows a second, which turned a backfill of a million works into fourteen
// hours; a queue with more in it than one batch is drained batch after batch
// until a batch is short.
func DrainOnce(ctx context.Context, database *sql.DB, client *Client) (int, error) {
	if database == nil || !client.Configured() {
		return 0, ErrNotConfigured
	}

	// The lock is session-scoped, so it lives on one dedicated connection for
	// exactly as long as this call.
	conn, err := database.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("acquiring a connection for the drain lock: %w", err)
	}
	defer conn.Close()

	var locked bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, drainLockKey).Scan(&locked); err != nil {
		return 0, fmt.Errorf("taking the drain lock: %w", err)
	}
	if !locked {
		return 0, nil
	}
	defer func() {
		// Released on every path, on a context that outlives a cancelled one.
		// An advisory lock is session-scoped, so the only way this can fail is
		// a dead backend — and a dead backend has already released it.
		if _, err := conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, drainLockKey); err != nil {
			log.Printf("[manhattan] releasing the drain lock: %v", err)
		}
	}()

	total := 0
	for {
		delivered, full, err := drainOneBatch(ctx, database, client)
		total += delivered
		if err != nil {
			reportQuarantined(ctx, database)
			return total, err
		}
		if !full || ctx.Err() != nil {
			break
		}
	}
	reportQuarantined(ctx, database)
	return total, nil
}

// drainOneBatch sends one batch to Manhattan in id order and acts on the
// verdicts. full is true when the batch was a whole one and every row in it was
// either delivered or quarantined — the signal that there may be more behind it
// and the caller should go again.
//
// The rows go as the triggers wrote them, one request for the batch, and
// Manhattan answers one outcome per row. Whether a replayed bind is already
// true, whether a refused edge is nonetheless present, whether a retraction of
// a pair that cannot exist is settled — every such question used to be decided
// here by reading a status and resolving again, and is now decided beside the
// row by the process that wrote it. This function applies outcomes; it does
// not interpret conflicts.
func drainOneBatch(ctx context.Context, database *sql.DB, client *Client) (delivered int, full bool, err error) {
	rows, err := database.QueryContext(ctx, `
		SELECT id, op, payload, attempts
		FROM manhattan_outbox
		WHERE delivered_at IS NULL
		  AND blocked_at IS NULL
		  AND next_attempt_at <= NOW()
		ORDER BY id
		LIMIT $1`, drainBatch)
	if err != nil {
		return 0, false, fmt.Errorf("reading outbox: %w", err)
	}

	pending := make([]outboxRow, 0, drainBatch)
	for rows.Next() {
		var r outboxRow
		if err := rows.Scan(&r.id, &r.op, &r.payload, &r.attempts); err != nil {
			rows.Close()
			return 0, false, fmt.Errorf("scanning outbox: %w", err)
		}
		pending = append(pending, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, false, fmt.Errorf("reading outbox: %w", err)
	}
	if len(pending) == 0 {
		return 0, false, nil
	}

	ops := make([]ApplyOp, len(pending))
	for i, r := range pending {
		ops[i] = ApplyOp{Op: r.op, Payload: r.payload}
	}
	results, err := client.Apply(ctx, ops)
	if err != nil {
		// No verdicts, so nothing is known about any row: the plane could not
		// be reached, predates the endpoint, or rejected the request as a
		// whole. None of that is a fact about the head row, so it is held and
		// the attempt is not counted. If the request did land and only the
		// answer was lost, replaying is safe — every op answers "already" the
		// second time, which is the whole reason the outcomes exist.
		cause := &failure{unreachable: true}
		if errors.Is(err, ErrNotFound) {
			cause.msg = fmt.Sprintf("applying a batch of %d: this Manhattan predates /v1/apply; the queue waits for one that has it", len(pending))
		} else {
			cause.msg = fmt.Sprintf("applying a batch of %d: %v", len(pending), err)
		}
		markFailed(ctx, database, pending[0], cause)
		return 0, false, nil
	}

	// A retry verdict halts the plane's walk of the batch, so every row after it
	// comes back skipped. When that row is then quarantined here, the skipped
	// rows are free to go and the caller should send them at once rather than
	// wait a tick.
	quarantinedAHold := false
	for i, r := range pending {
		res := results[i]
		var cause *failure
		switch res.Outcome {
		case OutcomeApplied, OutcomeAlready:
			if err := markDelivered(ctx, database, r.id); err != nil {
				return delivered, false, fmt.Errorf("marking outbox row %d delivered: %w", r.id, err)
			}
			delivered++
			continue
		case OutcomeRefused:
			cause = permanent("%s was refused (%s): %s", r.op, res.Code, res.Error)
		case OutcomeRetry:
			cause = retryable("%s was not delivered yet (%s): %s", r.op, res.Code, res.Error)
		case OutcomeError:
			cause = &failure{msg: fmt.Sprintf("%s could not be applied on Manhattan's side (%s): %s", r.op, res.Code, res.Error), unreachable: true}
		case OutcomeSkipped:
			// An earlier row held, and this one was left exactly as it was.
			return delivered, quarantinedAHold, nil
		default:
			// A verdict this build does not know, from a newer plane. It says
			// nothing this build can act on, so it is held without counting.
			cause = &failure{msg: fmt.Sprintf("%s came back with an outcome this build does not know: %q (%s): %s", r.op, res.Outcome, res.Code, res.Error), unreachable: true}
		}
		// Stop here unless the row has been taken out of the queue. Later rows
		// may depend on this one; a quarantined row is no longer in the queue,
		// so the batch carries on past it — that is what quarantining is for.
		if !markFailed(ctx, database, r, cause) {
			return delivered, false, nil
		}
		if res.Outcome == OutcomeRetry {
			quarantinedAHold = true
		}
	}
	return delivered, len(pending) == drainBatch, nil
}

func markDelivered(ctx context.Context, database *sql.DB, id int64) error {
	_, err := database.ExecContext(ctx,
		`UPDATE manhattan_outbox SET delivered_at = NOW(), last_error = NULL WHERE id = $1`, id)
	return err
}

// markFailed records the error and schedules a retry with exponential backoff,
// capped so a persistently failing row retries hourly rather than never.
//
// It returns true when the row has been quarantined, which is the caller's
// signal that the row is out of the queue and the batch may continue past it.
func markFailed(ctx context.Context, database *sql.DB, r outboxRow, cause *failure) bool {
	// The plane did not answer about this row. Record what happened and try
	// again next tick, but leave attempts alone: the count measures how many
	// times Manhattan has looked at this write and sent it back, and an outage
	// is not that. Counted, a seventy-minute outage would quarantine whichever
	// write was at the head of the queue when it began and hand an operator a
	// "failed 12 times" that was never refused once.
	if cause.unreachable {
		log.Printf("[manhattan] outbox row %d (%s) held, the naming plane did not answer (attempts so far %d), retrying in %s: %v",
			r.id, r.op, r.attempts, drainInterval, cause)
		if _, err := database.ExecContext(ctx, `
			UPDATE manhattan_outbox
			   SET last_error      = $2,
			       next_attempt_at = NOW() + $3::interval
			 WHERE id = $1`,
			r.id, cause.Error(), fmt.Sprintf("%d seconds", int(drainInterval.Seconds())),
		); err != nil {
			log.Printf("[manhattan] recording outbox hold for row %d: %v", r.id, err)
		}
		return false
	}

	attempt := r.attempts + 1
	backoff := time.Duration(1<<minInt(r.attempts, 11)) * time.Second
	if backoff > time.Hour {
		backoff = time.Hour
	}
	// Every failure says so. A queue that stops draining must be visible on the
	// first tick it happens, not on the twelfth.
	log.Printf("[manhattan] outbox row %d (%s) failed, attempt %d, retrying in %s: %v",
		r.id, r.op, attempt, backoff, cause)
	if _, err := database.ExecContext(ctx, `
		UPDATE manhattan_outbox
		   SET attempts        = attempts + 1,
		       last_error      = $2,
		       next_attempt_at = NOW() + $3::interval
		 WHERE id = $1`,
		r.id, cause.Error(), fmt.Sprintf("%d seconds", int(backoff.Seconds())),
	); err != nil {
		log.Printf("[manhattan] recording outbox failure for row %d: %v", r.id, err)
		// The attempt count did not advance, so quarantining now would be a
		// decision taken on a number that was never written. Hold the head.
		return false
	}

	// A refusal Manhattan will never withdraw is quarantined at once: retrying it
	// for an hour first changes nothing except how long everything behind it
	// waits. Every other answered failure earns its quarantine by being sent
	// back maxAttempts times, which leaves room for a rolling deploy to clear.
	// An outage never reaches here — it is held above, uncounted.
	if !cause.permanent && attempt < maxAttempts {
		return false
	}

	reason := fmt.Sprintf("Manhattan sent this back %d times: %v", attempt, cause)
	if cause.permanent {
		reason = fmt.Sprintf("Manhattan refused this for good: %v", cause)
	}
	if _, err := database.ExecContext(ctx, `
		UPDATE manhattan_outbox
		   SET blocked_at     = NOW(),
		       blocked_reason = $2
		 WHERE id = $1 AND blocked_at IS NULL`, r.id, reason); err != nil {
		log.Printf("[manhattan] quarantining outbox row %d: %v", r.id, err)
		// Not quarantined, so it keeps the head. That is the safe answer: the
		// queue stalls loudly rather than running past a row still in it.
		return false
	}

	// Never dropped, never silent. The row keeps its payload and its error; it is
	// out of the queue's way and says so on every tick until someone resolves it.
	log.Printf("[manhattan] INCIDENT: outbox row %d (%s) quarantined after %d attempts — this naming-plane write did not happen; the queue now drains past it: %s",
		r.id, r.op, attempt, reason)
	return true
}

// Quarantined reports how many rows the drain has given up on, and a sample of
// them. A quarantined row is a naming-plane write that did not happen, so
// nothing about it may be quiet: it is read on every tick and reported, and this
// is exported so /health or an operator can ask the same question.
func Quarantined(ctx context.Context, database *sql.DB) (int64, []string, error) {
	if database == nil {
		return 0, nil, nil
	}
	var count int64
	if err := database.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM manhattan_outbox
		 WHERE blocked_at IS NOT NULL AND delivered_at IS NULL`).Scan(&count); err != nil {
		return 0, nil, fmt.Errorf("counting quarantined outbox rows: %w", err)
	}
	if count == 0 {
		return 0, nil, nil
	}
	rows, err := database.QueryContext(ctx, `
		SELECT id, op, COALESCE(blocked_reason, 'no reason recorded')
		FROM manhattan_outbox
		WHERE blocked_at IS NOT NULL AND delivered_at IS NULL
		ORDER BY id
		LIMIT 5`)
	if err != nil {
		return count, nil, fmt.Errorf("reading quarantined outbox rows: %w", err)
	}
	defer rows.Close()

	sample := make([]string, 0, 5)
	for rows.Next() {
		var (
			id     int64
			op     string
			reason string
		)
		if err := rows.Scan(&id, &op, &reason); err != nil {
			return count, sample, fmt.Errorf("scanning quarantined outbox rows: %w", err)
		}
		sample = append(sample, fmt.Sprintf("%d (%s): %s", id, op, reason))
	}
	if err := rows.Err(); err != nil {
		return count, sample, fmt.Errorf("reading quarantined outbox rows: %w", err)
	}
	return count, sample, nil
}

// reportQuarantined says what is quarantined, on every tick where anything is.
func reportQuarantined(ctx context.Context, database *sql.DB) {
	count, sample, err := Quarantined(ctx, database)
	if err != nil {
		log.Printf("[manhattan] %v", err)
		return
	}
	if count == 0 {
		return
	}
	log.Printf("[manhattan] INCIDENT: %d outbox row(s) quarantined — these naming-plane writes did not happen and will not until an operator clears blocked_at: %s",
		count, strings.Join(sample, " | "))
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
