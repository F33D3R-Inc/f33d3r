package handler

// compliance_2257.go — 18 U.S.C. § 2257 on the publish path.
//
// Two decisions, each with one owner:
//
//	may this 18+ work exist?   Asked synchronously, before the work is written
//	                           (adultPublishGate). Only Verity can answer: it holds
//	                           the creator's passing purpose=creator verification
//	                           and the copy of their ID. An identity tier — even
//	                           'full', even an admin override — never answers it.
//	                           No answer is a refusal (fail closed).
//
//	is the record written?     Durably, by the compliance_2257_outbox queued in
//	                           the work's create transaction and drained here,
//	                           retried with backoff and quarantined for a person
//	                           at the cap. Removals are queued in the deleting
//	                           transaction and reported the same way.
//
// Why both, and not one: a refusal alone cannot write the record (the work has
// no id until it is written), and a record queued without the refusal would let
// a work exist first and be found unrecordable afterwards. The gate makes an
// unrecordable 18+ work never exist; the outbox makes a recordable one never go
// unrecorded. If Verity nonetheless refuses the record (the verification was
// withdrawn between the gate and the drain), the work is blocked from view and
// the row is quarantined for a person: an 18+ work without a 2257 record is not
// shown to anyone.

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/observ"
	"github.com/f33d3r/feed-engine/internal/verity"
)

// record2257Type is the record_type Verity files a work under.
const record2257Type = "content_post"

var (
	compliance2257Done = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "compliance_2257_done_total",
		Help: "2257 obligations Verity accepted, by action.",
	}, []string{"action"})
	compliance2257Failed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "compliance_2257_failed_total",
		Help: "Failed attempts at 2257 obligations, by action.",
	}, []string{"action"})
	compliance2257Quarantined = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "compliance_2257_quarantined",
		Help: "2257 obligations quarantined and waiting for a person.",
	})
)

func init() {
	observ.MustRegister(compliance2257Done, compliance2257Failed, compliance2257Quarantined)
}

// adultPublishGate refuses an 18+ work unless Verity holds a passing creator
// verification for its author. nil means the work may be written.
func (h *Handler) adultPublishGate(ctx context.Context, user *model.User) *workCreateAnswer {
	if user.IsMinor {
		return refuse(WorkStateAdultRecordRequired)
	}
	if !h.verity.Configured() {
		log.Printf("[2257] 18+ work by @%s refused: Verity is not configured, so no creator verification can be confirmed", user.Handle)
		return refuse(WorkStateComplianceUnavailable)
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	eligible, err := h.verity.Eligible2257(cctx, user.PIALID)
	if err != nil {
		log.Printf("[2257] 18+ work by @%s refused: Verity could not be asked: %v", user.Handle, err)
		return refuse(WorkStateComplianceUnavailable)
	}
	if !eligible {
		return refuse(WorkStateAdultRecordRequired)
	}
	return nil
}

// drain2257 pays what 18+ works owe Verity. Called on every work-outbox tick.
func (o *workOutbox) drain2257(ctx context.Context) {
	h := o.h
	rows, err := dbpkg.Claim2257Outbox(h.db, workOutboxBatch, workOutboxLease)
	if err != nil {
		log.Printf("[2257] %v", err)
		return
	}
	for _, row := range rows {
		actx, cancel := context.WithTimeout(ctx, workOutboxActionTimeout)
		err := h.perform2257(actx, row)
		cancel()
		action := string(row.Action)
		if err == nil {
			if cerr := dbpkg.Complete2257Outbox(h.db, row.ID); cerr != nil {
				// Verity accepted; the row stays open and its lease gives it
				// back. Both Verity calls are idempotent on (pial, content).
				log.Printf("[2257] %v", cerr)
				continue
			}
			compliance2257Done.WithLabelValues(action).Inc()
			continue
		}
		compliance2257Failed.WithLabelValues(action).Inc()
		if row.Action == dbpkg.Compliance2257Record && errors.Is(err, verity.ErrNoCreatorPass) {
			h.withhold2257Unrecordable(row, err)
			continue
		}
		quarantined, ferr := dbpkg.Fail2257Outbox(h.db, row.ID, err)
		if ferr != nil {
			log.Printf("[2257] %v (after: %v)", ferr, err)
			continue
		}
		if quarantined {
			log.Printf("[2257] INCIDENT: %s for work %s quarantined after %d attempts: %v", action, row.WorkID, row.Attempt, err)
			continue
		}
		log.Printf("[2257] %s for work %s, attempt %d: %v", action, row.WorkID, row.Attempt, err)
	}
}

// perform2257 makes one call to Verity.
func (h *Handler) perform2257(ctx context.Context, row dbpkg.Compliance2257Row) error {
	switch row.Action {
	case dbpkg.Compliance2257Record:
		return h.verity.Record2257(ctx, row.PIALID, row.WorkID, record2257Type)
	case dbpkg.Compliance2257Removed:
		removedAt := time.Now()
		if row.RemovedAt != nil {
			removedAt = *row.RemovedAt
		}
		return h.verity.ContentRemoved2257(ctx, row.WorkID, removedAt)
	}
	return errors.New("unknown 2257 action " + string(row.Action))
}

// withhold2257Unrecordable handles Verity refusing a work's 2257 record: the
// creator's verification is no longer there. The work is blocked from view —
// an 18+ work without a record is shown to nobody — and the row is quarantined
// so a person decides what happens next.
func (h *Handler) withhold2257Unrecordable(row dbpkg.Compliance2257Row, cause error) {
	log.Printf("[2257] INCIDENT: Verity refused the 2257 record of work %s: %v — blocking the work", row.WorkID, cause)
	if err := dbpkg.SetPostScanState(h.db, row.WorkID, "blocked",
		"no 18 U.S.C. 2257 record: Verity holds no passing creator verification for the author", "verity_2257", false); err != nil {
		log.Printf("[2257] INCIDENT: work %s has no 2257 record and could NOT be blocked: %v", row.WorkID, err)
	}
	if err := dbpkg.Quarantine2257Outbox(h.db, row.ID, cause); err != nil {
		log.Printf("[2257] %v", err)
	}
}

// report2257 publishes the quarantine count and says it out loud.
func (o *workOutbox) report2257() {
	n, err := dbpkg.Quarantined2257Outbox(o.h.db)
	if err != nil {
		log.Printf("[2257] %v", err)
		return
	}
	compliance2257Quarantined.Set(float64(n))
	if n > 0 {
		log.Printf("[2257] INCIDENT: %d 2257 obligation(s) quarantined — see migration 0047 for the operator queries", n)
	}
}
