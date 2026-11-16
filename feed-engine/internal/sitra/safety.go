// Safety event consumer — how Abraxas's answers reach this brain's content.
//
// Abraxas is the integrity authority. It publishes every verdict, ownership
// finding, banned match and review decision to safety.events, and this
// consumer hands each record to the handler, which applies it to works,
// Visions and the CSAM queue and redraws the affected cards over FA Live.
//
// The shape is the identity consumer's (identity.go), for the same reasons:
//
//   - Commit only after a successful apply. A record whose apply fails for a
//     transient reason is retried in place with backoff and its offset is not
//     committed, so a crash redelivers it. No verdict is ever skipped.
//   - Never skip silently. A record that can never be applied — undecodable,
//     or refused as invalid — is written to safety_event_deadletter with its
//     coordinates and the reason, and only then committed past.
//   - Start at the beginning. A new group reads the topic from its start:
//     every apply is guarded by the subject's seq ledger and is idempotent, so
//     history replays into the same state.
//
// The group is shared, so one replica applies each partition.
package sitra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/f33d3r/feed-engine/internal/observ"
)

// TopicSafety is the topic Abraxas produces its answers to.
const TopicSafety = "safety.events"

const safetyGroup = "nantar-safety-projection"

// Safety event types.
const (
	SafetyTypeVerdict       = "verdict"
	SafetyTypeOwnership     = "ownership"
	SafetyTypeBannedMatch   = "banned_match"
	SafetyTypeReviewDecided = "review_decided"
)

// SafetySubject names what an event is about.
type SafetySubject struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// SafetyOwnership is an ownership finding: the content is (or derives from)
// the original owner's.
//
// A credit arrives two ways: a standalone ownership event (Abraxas credited it
// automatically at check time) or a review_decided carrying ownership (an ML or
// admin credit). A rejection carries ownership with state "rejected" and no
// original owner.
type SafetyOwnership struct {
	MatchID       string  `json:"match_id"`
	OriginalPIAL  string  `json:"original_pial"`
	OriginalAsset string  `json:"original_asset"`
	Kind          string  `json:"kind"`
	Confidence    float64 `json:"confidence"`
	// State is credited | ml_review | rejected.
	State string `json:"state"`
}

// Credited reports whether the finding credits an original owner.
func (o *SafetyOwnership) Credited() bool {
	return o != nil && o.OriginalPIAL != "" && (o.State == "" || o.State == "credited")
}

// SafetyBannedMatch is a match against a banned registry.
type SafetyBannedMatch struct {
	Category string `json:"category"`
	Layer    string `json:"layer"`
	SHA256   string `json:"sha256"`
	// Match is exact (the very bytes the platform received) or near (a
	// perceptual match a person must judge).
	Match    string  `json:"match"`
	BannedID string  `json:"banned_id"`
	Score    float64 `json:"score"`
}

// SafetyReview names the review item a review_decided closed.
type SafetyReview struct {
	ID        string `json:"id"`
	ItemKind  string `json:"item_kind"`
	Decision  string `json:"decision"`
	DecidedBy string `json:"decided_by"`
}

// SafetyEvent is one decoded safety.events record.
type SafetyEvent struct {
	EventID     string             `json:"event_id"`
	Seq         int64              `json:"seq"`
	Type        string             `json:"type"`
	Subject     SafetySubject      `json:"subject"`
	AssetID     string             `json:"asset_id,omitempty"`
	PIAL        string             `json:"pial,omitempty"`
	Verdict     string             `json:"verdict,omitempty"`
	Scores      map[string]float64 `json:"scores,omitempty"`
	Signals     []string           `json:"signals,omitempty"`
	Reason      string             `json:"reason,omitempty"`
	Ownership   *SafetyOwnership   `json:"ownership,omitempty"`
	BannedMatch *SafetyBannedMatch `json:"banned_match,omitempty"`
	Review      *SafetyReview      `json:"review,omitempty"`
	OccurredAt  time.Time          `json:"occurred_at"`
}

// SafetyHandlers are what the consumer calls.
type SafetyHandlers struct {
	// Apply carries one event out. It returns an error wrapping ErrPermanent
	// when the event can never apply, and any other error when the attempt
	// should be repeated.
	Apply func(ctx context.Context, ev SafetyEvent) error
	// DeadLetter keeps a record that can never be applied. Its failure is
	// itself retried: a record is committed past only once it is kept.
	DeadLetter func(ctx context.Context, topic string, partition int32, offset int64, record []byte, reason string) error
}

var (
	safetyMetricsOnce sync.Once
	safetyApplied     = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sitra_safety_events_total",
		Help: "safety.events records handled, by result.",
	}, []string{"result"})
	safetyRetries = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "sitra_safety_apply_retries_total",
		Help: "Transient safety event apply failures that were retried.",
	})
	safetyLag = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "sitra_safety_consumer_lag_seconds",
		Help: "Seconds between the last applied safety event's occurred_at and now.",
	})
)

func registerSafetyMetrics() {
	safetyMetricsOnce.Do(func() {
		observ.Registry().MustRegister(safetyApplied, safetyRetries, safetyLag)
		for _, r := range []string{"applied", "deadlettered"} {
			safetyApplied.WithLabelValues(r).Add(0)
		}
	})
}

var safetyVerdicts = map[string]bool{
	"approve": true, "age_gate": true, "human_review": true, "block": true, "pending": true,
}

// decodeSafety parses and validates one record.
func decodeSafety(raw []byte) (SafetyEvent, error) {
	var ev SafetyEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return SafetyEvent{}, fmt.Errorf("decode: %w", err)
	}
	switch {
	case ev.EventID == "":
		return SafetyEvent{}, errors.New("missing event_id")
	case ev.Seq <= 0:
		return SafetyEvent{}, errors.New("missing seq")
	case ev.Subject.Kind == "" || ev.Subject.ID == "":
		return SafetyEvent{}, errors.New("missing subject")
	case ev.OccurredAt.IsZero():
		return SafetyEvent{}, errors.New("missing occurred_at")
	}
	switch ev.Type {
	case SafetyTypeVerdict, SafetyTypeReviewDecided:
		if !safetyVerdicts[ev.Verdict] {
			return SafetyEvent{}, fmt.Errorf("%s with unknown verdict %q", ev.Type, ev.Verdict)
		}
	case SafetyTypeOwnership:
		if ev.Ownership == nil || ev.Ownership.OriginalPIAL == "" {
			return SafetyEvent{}, errors.New("ownership event without an original owner")
		}
	case SafetyTypeBannedMatch:
		if ev.BannedMatch == nil || ev.BannedMatch.Category == "" || ev.BannedMatch.Layer == "" {
			return SafetyEvent{}, errors.New("banned_match event without its category and layer")
		}
		if ev.BannedMatch.SHA256 == "" {
			return SafetyEvent{}, errors.New("banned_match event without the sha256 of the matched bytes")
		}
		if ev.BannedMatch.Match != "exact" && ev.BannedMatch.Match != "near" {
			return SafetyEvent{}, fmt.Errorf("banned_match event with match %q (want exact or near)", ev.BannedMatch.Match)
		}
	default:
		return SafetyEvent{}, fmt.Errorf("unknown type %q", ev.Type)
	}
	return ev, nil
}

// StartSafetyConsumer subscribes to safety.events. It returns a stop func.
func StartSafetyConsumer(ctx context.Context, brokers string, h SafetyHandlers) (stop func(), err error) {
	seeds := splitBrokers(brokers)
	if len(seeds) == 0 {
		return nil, errors.New("sitra: no brokers given for the safety consumer")
	}
	if h.Apply == nil || h.DeadLetter == nil {
		return nil, errors.New("sitra: safety consumer needs Apply and DeadLetter")
	}
	registerSafetyMetrics()

	client, err := kgo.NewClient(
		kgo.SeedBrokers(seeds...),
		kgo.ConsumerGroup(safetyGroup),
		kgo.ConsumeTopics(TopicSafety),
		kgo.DisableAutoCommit(),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.SessionTimeout(30*time.Second),
		kgo.FetchMaxBytes(1<<20),
	)
	if err != nil {
		return nil, fmt.Errorf("sitra: safety consumer client: %w", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		log.Printf("[sitra] safety consumer started group=%s topic=%s brokers=%v", safetyGroup, TopicSafety, seeds)
		backoff := time.Second
		for {
			fetches := client.PollFetches(runCtx)
			if runCtx.Err() != nil {
				return
			}
			if errs := fetches.Errors(); len(errs) > 0 {
				for _, fe := range errs {
					log.Printf("[sitra] safety consumer poll error topic=%s partition=%d: %v", fe.Topic, fe.Partition, fe.Err)
				}
				if !sleepCtx(runCtx, backoff) {
					return
				}
				backoff = minDuration(backoff*2, 30*time.Second)
				continue
			}
			backoff = time.Second

			var handled []*kgo.Record
			stopped := false
			fetches.EachRecord(func(rec *kgo.Record) {
				if stopped {
					return
				}
				if !handleSafetyRecord(runCtx, rec, h) {
					stopped = true // context ended mid-retry; this record stays uncommitted
					return
				}
				handled = append(handled, rec)
			})
			if len(handled) > 0 {
				commitCtx, commitCancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := client.CommitRecords(commitCtx, handled...); err != nil {
					// Uncommitted records are redelivered and re-applied; every
					// apply is guarded by the seq ledger, so this costs work,
					// not correctness.
					log.Printf("[sitra] safety consumer commit error (%d records): %v", len(handled), err)
				}
				commitCancel()
			}
			if stopped {
				return
			}
		}
	}()

	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				log.Printf("[sitra] safety consumer did not stop within 5s; closing client anyway")
			}
			leaveCtx, leaveCancel := context.WithTimeout(context.Background(), 3*time.Second)
			if err := client.LeaveGroupContext(leaveCtx); err != nil {
				log.Printf("[sitra] safety consumer leave group: %v", err)
			}
			leaveCancel()
			client.Close()
			log.Printf("[sitra] safety consumer stopped group=%s", safetyGroup)
		})
	}
	return stop, nil
}

// handleSafetyRecord applies or dead-letters one record, retrying transient
// failures until it succeeds. It returns false only when ctx ended first.
func handleSafetyRecord(ctx context.Context, rec *kgo.Record, h SafetyHandlers) bool {
	backoff := 500 * time.Millisecond
	for {
		reason := ""
		ev, err := decodeSafety(rec.Value)
		if err != nil {
			reason = "malformed: " + err.Error()
		} else {
			applyErr := applySafetyRecovered(ctx, h.Apply, ev)
			switch {
			case applyErr == nil:
				safetyApplied.WithLabelValues("applied").Inc()
				safetyLag.Set(time.Since(ev.OccurredAt).Seconds())
				return true
			case errors.Is(applyErr, ErrPermanent):
				reason = applyErr.Error()
			default:
				safetyRetries.Inc()
				log.Printf("[sitra] safety event %s (%s %s %s) not applied, retrying in %s: %v",
					ev.EventID, ev.Type, ev.Subject.Kind, ev.Subject.ID, backoff, applyErr)
				if !sleepCtx(ctx, backoff) {
					return false
				}
				backoff = minDuration(backoff*2, 30*time.Second)
				continue
			}
		}

		log.Printf("[sitra] CRITICAL: safety record topic=%s partition=%d offset=%d can never be applied: %s — dead-lettered",
			rec.Topic, rec.Partition, rec.Offset, reason)
		if err := h.DeadLetter(ctx, rec.Topic, rec.Partition, rec.Offset, rec.Value, reason); err != nil {
			log.Printf("[sitra] safety record offset=%d could not be dead-lettered, retrying in %s: %v", rec.Offset, backoff, err)
			if !sleepCtx(ctx, backoff) {
				return false
			}
			backoff = minDuration(backoff*2, 30*time.Second)
			continue
		}
		safetyApplied.WithLabelValues("deadlettered").Inc()
		return true
	}
}

// applySafetyRecovered turns a panic in Apply into a permanent failure, so one
// poisoned record is dead-lettered instead of killing the consumer.
func applySafetyRecovered(ctx context.Context, apply func(context.Context, SafetyEvent) error, ev SafetyEvent) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: apply panicked: %v", ErrPermanent, r)
		}
	}()
	return apply(observ.WithRequestID(ctx, ev.EventID), ev)
}
