// Identity event consumer — the identity projection's only feed.
//
// elohim-veni publishes every identity change to identity.events. Each record
// carries the full current state of one identity (role, status, granted
// capabilities, facts) and, when the change concerns an account, that
// account's handle. feed-engine applies each one to its projection —
// users.handle / pial_id / role / deactivated_at and identity_facts — so its
// SQL joins read what the authority last said.
//
// Three properties decide the shape of this consumer, and each differs from
// the Frequency consumer on purpose:
//
//   - Commit only after a successful apply. A record whose apply fails for a
//     transient reason (the database is unreachable, a lock timed out) is
//     retried in place with backoff and its offset is not committed, so a crash
//     redelivers it. The projection never moves past an event it did not apply.
//   - Never skip silently. A record that can never be applied — undecodable, or
//     refused by the database as invalid data — is written to
//     identity_event_deadletter with its coordinates and the reason, and only
//     then committed past. An operator sees every one.
//   - Start at the beginning. A new group reads the topic from its start:
//     every apply is guarded by occurred_at and idempotent, so history replays
//     into the same state, and nothing published before the group existed is
//     lost.
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

// TopicIdentity is the topic elohim-veni produces identity events to.
const TopicIdentity = "identity.events"

const identityGroup = "nantar-identity-projection"

// IdentityEvent is one decoded identity.events record.
type IdentityEvent struct {
	EventID    string
	Type       string
	PIALID     string
	AccountID  string
	Handle     string
	Role       string
	Status     string
	Caps       []string
	Facts      IdentityEventFacts
	OccurredAt time.Time
}

// IdentityEventFacts are the facts an identity event carries.
type IdentityEventFacts struct {
	AgeVerified   bool       `json:"age_verified"`
	IsAdult       bool       `json:"is_adult"`
	IsMinor       bool       `json:"is_minor"`
	KYCTier       string     `json:"kyc_tier"`
	DOB           string     `json:"dob,omitempty"`
	KYCVerifiedAt *time.Time `json:"kyc_verified_at,omitempty"`
}

type identityWire struct {
	EventID    string             `json:"event_id"`
	Type       string             `json:"type"`
	PIALID     string             `json:"pial_id"`
	AccountID  string             `json:"account_id"`
	Handle     string             `json:"handle"`
	Role       string             `json:"role"`
	Status     string             `json:"status"`
	Caps       []string           `json:"caps"`
	Facts      IdentityEventFacts `json:"facts"`
	OccurredAt time.Time          `json:"occurred_at"`
}

// ErrPermanent marks an apply failure that retrying cannot fix. The apply
// function wraps such failures with it; the consumer dead-letters the record.
var ErrPermanent = errors.New("event can never be applied")

// IdentityHandlers are what the consumer calls.
type IdentityHandlers struct {
	// Apply brings the projection up to one event. It returns an error wrapping
	// ErrPermanent when the event can never apply, and any other error when
	// the attempt should be repeated.
	Apply func(ctx context.Context, ev IdentityEvent) error
	// DeadLetter keeps a record that can never be applied. Its failure is
	// itself retried: a record is committed past only once it is kept.
	DeadLetter func(ctx context.Context, topic string, partition int32, offset int64, record []byte, reason string) error
}

var (
	identityMetricsOnce sync.Once
	identityApplied     = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sitra_identity_events_total",
		Help: "identity.events records handled, by result.",
	}, []string{"result"})
	identityRetries = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "sitra_identity_apply_retries_total",
		Help: "Transient identity projection apply failures that were retried.",
	})
	identityLag = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "sitra_identity_consumer_lag_seconds",
		Help: "Seconds between the last applied identity event's occurred_at and now.",
	})
)

func registerIdentityMetrics() {
	identityMetricsOnce.Do(func() {
		observ.Registry().MustRegister(identityApplied, identityRetries, identityLag)
		for _, r := range []string{"applied", "deadlettered"} {
			identityApplied.WithLabelValues(r).Add(0)
		}
	})
}

// decodeIdentity parses and validates one record.
func decodeIdentity(raw []byte) (IdentityEvent, error) {
	var w identityWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return IdentityEvent{}, fmt.Errorf("decode: %w", err)
	}
	switch {
	case w.EventID == "":
		return IdentityEvent{}, errors.New("missing event_id")
	case w.PIALID == "":
		return IdentityEvent{}, errors.New("missing pial_id")
	case w.OccurredAt.IsZero():
		return IdentityEvent{}, errors.New("missing occurred_at")
	case w.Status == "":
		return IdentityEvent{}, errors.New("missing status")
	case w.AccountID != "" && w.Handle == "":
		return IdentityEvent{}, errors.New("account event without a handle")
	}
	return IdentityEvent{
		EventID: w.EventID, Type: w.Type, PIALID: w.PIALID, AccountID: w.AccountID,
		Handle: w.Handle, Role: w.Role, Status: w.Status, Caps: w.Caps,
		Facts: w.Facts, OccurredAt: w.OccurredAt,
	}, nil
}

// StartIdentityConsumer subscribes to identity.events. It returns a stop func.
func StartIdentityConsumer(ctx context.Context, brokers string, h IdentityHandlers) (stop func(), err error) {
	seeds := splitBrokers(brokers)
	if len(seeds) == 0 {
		return nil, errors.New("sitra: no brokers given for the identity consumer")
	}
	if h.Apply == nil || h.DeadLetter == nil {
		return nil, errors.New("sitra: identity consumer needs Apply and DeadLetter")
	}
	registerIdentityMetrics()

	client, err := kgo.NewClient(
		kgo.SeedBrokers(seeds...),
		kgo.ConsumerGroup(identityGroup),
		kgo.ConsumeTopics(TopicIdentity),
		kgo.DisableAutoCommit(),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.SessionTimeout(30*time.Second),
		kgo.FetchMaxBytes(1<<20),
	)
	if err != nil {
		return nil, fmt.Errorf("sitra: identity consumer client: %w", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		log.Printf("[sitra] identity consumer started group=%s topic=%s brokers=%v", identityGroup, TopicIdentity, seeds)
		backoff := time.Second
		for {
			fetches := client.PollFetches(runCtx)
			if runCtx.Err() != nil {
				return
			}
			if errs := fetches.Errors(); len(errs) > 0 {
				for _, fe := range errs {
					log.Printf("[sitra] identity consumer poll error topic=%s partition=%d: %v", fe.Topic, fe.Partition, fe.Err)
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
				if !handleIdentityRecord(runCtx, rec, h) {
					stopped = true // context ended mid-retry; this record stays uncommitted
					return
				}
				handled = append(handled, rec)
			})
			if len(handled) > 0 {
				commitCtx, commitCancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := client.CommitRecords(commitCtx, handled...); err != nil {
					// Uncommitted records are redelivered and re-applied; every
					// apply is idempotent, so this costs work, not correctness.
					log.Printf("[sitra] identity consumer commit error (%d records): %v", len(handled), err)
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
				log.Printf("[sitra] identity consumer did not stop within 5s; closing client anyway")
			}
			leaveCtx, leaveCancel := context.WithTimeout(context.Background(), 3*time.Second)
			if err := client.LeaveGroupContext(leaveCtx); err != nil {
				log.Printf("[sitra] identity consumer leave group: %v", err)
			}
			leaveCancel()
			client.Close()
			log.Printf("[sitra] identity consumer stopped group=%s", identityGroup)
		})
	}
	return stop, nil
}

// handleIdentityRecord applies or dead-letters one record, retrying transient
// failures until it succeeds. It returns false only when ctx ended first.
func handleIdentityRecord(ctx context.Context, rec *kgo.Record, h IdentityHandlers) bool {
	backoff := 500 * time.Millisecond
	for {
		reason := ""
		ev, err := decodeIdentity(rec.Value)
		if err != nil {
			reason = "malformed: " + err.Error()
		} else {
			applyErr := applyRecovered(ctx, h.Apply, ev)
			switch {
			case applyErr == nil:
				identityApplied.WithLabelValues("applied").Inc()
				identityLag.Set(time.Since(ev.OccurredAt).Seconds())
				return true
			case errors.Is(applyErr, ErrPermanent):
				reason = applyErr.Error()
			default:
				identityRetries.Inc()
				log.Printf("[sitra] identity event %s (pial %s) not applied, retrying in %s: %v",
					ev.EventID, ev.PIALID, backoff, applyErr)
				if !sleepCtx(ctx, backoff) {
					return false
				}
				backoff = minDuration(backoff*2, 30*time.Second)
				continue
			}
		}

		log.Printf("[sitra] CRITICAL: identity record topic=%s partition=%d offset=%d can never be applied: %s — dead-lettered",
			rec.Topic, rec.Partition, rec.Offset, reason)
		if err := h.DeadLetter(ctx, rec.Topic, rec.Partition, rec.Offset, rec.Value, reason); err != nil {
			log.Printf("[sitra] identity record offset=%d could not be dead-lettered, retrying in %s: %v", rec.Offset, backoff, err)
			if !sleepCtx(ctx, backoff) {
				return false
			}
			backoff = minDuration(backoff*2, 30*time.Second)
			continue
		}
		identityApplied.WithLabelValues("deadlettered").Inc()
		return true
	}
}

// applyRecovered turns a panic in Apply into a permanent failure, so one
// poisoned record is dead-lettered instead of killing the consumer.
func applyRecovered(ctx context.Context, apply func(context.Context, IdentityEvent) error, ev IdentityEvent) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: apply panicked: %v", ErrPermanent, r)
		}
	}()
	return apply(observ.WithRequestID(ctx, ev.EventID), ev)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
