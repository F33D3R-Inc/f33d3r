// Package backfill walks the works whose psych_vector is still NULL and
// gives each one its place on the Jung axes.
//
// InsertWork writes a vector with every new work and the feed writes one for
// any work it serves without, so the column converges on its own under
// ordinary reads. This task closes the remainder — works that predate
// migration 0020 and are never served again, or were inserted by a path that
// did not compute one — so that no live work stays vector-less waiting for a
// read that may never come. It is bounded on every axis: batches are small,
// it rests between them, it stops the moment the server does, and it comes
// back every hour for anything new.
//
// Audio works get the text mapping here like any other work. Zior's analysis
// of a voice work happens at the publish seam, from the file the publish
// just wrote; re-sending historical audio files to Zior is a separate task
// and not this one.
package backfill

import (
	"context"
	"database/sql"
	"time"

	"go.uber.org/zap"

	"github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/jung"
)

const (
	// BatchSize is how many works one select carries.
	BatchSize = 200
	// BatchRest is the pause between batches, so a long backlog never
	// competes with the request path for the database.
	BatchRest = 250 * time.Millisecond
	// Recheck is how long the task sleeps once the backlog is empty before
	// looking again.
	Recheck = time.Hour
	// retryRest is the pause after a failed batch before the next attempt,
	// long enough that a database outage is not hammered.
	retryRest = 30 * time.Second
)

// Run walks the backlog until ctx is done. It blocks; start it on its own
// goroutine after migrations have run. A nil database is a no-op.
func Run(ctx context.Context, database *sql.DB, logger *zap.Logger) {
	if database == nil {
		return
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	logger = logger.Named("jung.backfill")
	for {
		written, err := pass(ctx, database, logger)
		if err != nil && ctx.Err() == nil {
			logger.Warn("pass stopped on error", zap.Int("written", written), zap.Error(err))
			if !rest(ctx, retryRest) {
				return
			}
			continue
		}
		if written > 0 {
			logger.Info("pass complete", zap.Int("written", written))
		}
		if !rest(ctx, Recheck) {
			return
		}
	}
}

// pass drains the backlog one batch at a time and returns how many vectors
// it wrote. A batch that writes nothing ends the pass: the rows it saw are
// still NULL and would be selected again, so continuing would spin.
func pass(ctx context.Context, database *sql.DB, logger *zap.Logger) (int, error) {
	total := 0
	for ctx.Err() == nil {
		works, err := db.GetWorksMissingPsychVector(database, BatchSize)
		if err != nil {
			return total, err
		}
		if len(works) == 0 {
			return total, nil
		}
		vectors := make(map[string][]float32, len(works))
		for _, w := range works {
			if w == nil || w.ID == "" {
				continue
			}
			vectors[w.ID] = jung.MapWork(w).Slice()
		}
		if len(vectors) == 0 {
			return total, nil
		}
		n, err := db.SaveWorkPsychVectors(database, vectors)
		if err != nil {
			return total, err
		}
		total += n
		logger.Info("batch written",
			zap.Int("batch", len(vectors)),
			zap.Int("written", n),
			zap.Int("total", total))
		if n == 0 {
			// Every row in the batch already held a vector by the time the
			// write ran (the feed got there first, or the rows were deleted).
			// The next select would return the same rows only if that were
			// false, so this is the end of the backlog, not a stall.
			return total, nil
		}
		if len(works) < BatchSize {
			return total, nil
		}
		if !rest(ctx, BatchRest) {
			return total, ctx.Err()
		}
	}
	return total, ctx.Err()
}

// rest sleeps for d or until ctx is done, reporting whether to continue.
func rest(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
