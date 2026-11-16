// Commerce event consumer — how Themis's plans and subscriptions reach this
// brain's read projection.
//
// Themis is the commerce authority and the only writer of plans and
// subscriptions. It publishes every change to commerce.events from a
// transactional outbox, and this consumer hands each record to the handler,
// which applies it to creator_plans and subscriptions (the projection the feed,
// fanout and gating queries join on) and redraws what a delivered purchase
// changes over FA Live.
//
// The shape is the identity and safety consumers' (identity.go, safety.go):
//
//   - Commit only after a successful apply. A record whose apply fails for a
//     transient reason is retried in place with backoff and its offset is not
//     committed, so a crash redelivers it.
//   - Never skip silently. A record that can never be applied is written to
//     commerce_event_deadletter with its coordinates and the reason, and only
//     then committed past.
//   - Start at the beginning. Every apply is guarded by the subject's seq
//     ledger and carries the subject's full state, so history replays into the
//     same rows.
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

// TopicCommerce is the topic Themis produces its commerce decisions to.
const TopicCommerce = "commerce.events"

const commerceGroup = "nantar-commerce-projection"

// Commerce event types.
const (
	CommerceSubscriptionCreated   = "subscription.created"
	CommerceSubscriptionRenewed   = "subscription.renewed"
	CommerceSubscriptionResumed   = "subscription.resumed"
	CommerceSubscriptionCancelled = "subscription.cancelled"
	CommerceSubscriptionExpired   = "subscription.expired"
	CommercePlanChanged           = "plan.changed"
	CommercePurchaseDelivered     = "purchase.delivered"
)

// CommerceSubject names what an event is about.
type CommerceSubject struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// CommerceSubscription is the full state of one subscription.
type CommerceSubscription struct {
	ID                string     `json:"id"`
	SubscriberAccount string     `json:"subscriber_account"`
	CreatorAccount    string     `json:"creator_account"`
	PlanID            *string    `json:"plan_id"`
	Status            string     `json:"status"`
	PeriodEnd         time.Time  `json:"period_end"`
	CancelledAt       *time.Time `json:"cancelled_at"`
	PriceUnits        int64      `json:"price_units"`
}

// CommercePlan is the full state of one plan.
type CommercePlan struct {
	ID             string `json:"id"`
	CreatorAccount string `json:"creator_account"`
	Name           string `json:"name"`
	Description    string `json:"description"`
	PriceUnits     int64  `json:"price_units"`
	IsActive       bool   `json:"is_active"`
}

// CommercePurchase names a delivered marketplace purchase.
type CommercePurchase struct {
	ID        string `json:"id"`
	BuyerPIAL string `json:"buyer_pial"`
	ListingID string `json:"listing_id"`
}

// CommerceEvent is one decoded commerce.events record.
type CommerceEvent struct {
	EventID      string                `json:"event_id"`
	Seq          int64                 `json:"seq"`
	Type         string                `json:"type"`
	Subject      CommerceSubject       `json:"subject"`
	Subscription *CommerceSubscription `json:"subscription,omitempty"`
	Plan         *CommercePlan         `json:"plan,omitempty"`
	Purchase     *CommercePurchase     `json:"purchase,omitempty"`
	OccurredAt   time.Time             `json:"occurred_at"`
}

// CommerceHandlers are what the consumer calls.
type CommerceHandlers struct {
	// Apply carries one event out. It returns an error wrapping ErrPermanent
	// when the event can never apply, and any other error when the attempt
	// should be repeated.
	Apply func(ctx context.Context, ev CommerceEvent) error
	// DeadLetter keeps a record that can never be applied. Its failure is
	// itself retried: a record is committed past only once it is kept.
	DeadLetter func(ctx context.Context, topic string, partition int32, offset int64, record []byte, reason string) error
}

var (
	commerceMetricsOnce sync.Once
	commerceApplied     = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sitra_commerce_events_total",
		Help: "commerce.events records handled, by result.",
	}, []string{"result"})
	commerceRetries = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "sitra_commerce_apply_retries_total",
		Help: "Transient commerce event apply failures that were retried.",
	})
	commerceLag = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "sitra_commerce_consumer_lag_seconds",
		Help: "Seconds between the last applied commerce event's occurred_at and now.",
	})
)

func registerCommerceMetrics() {
	commerceMetricsOnce.Do(func() {
		observ.Registry().MustRegister(commerceApplied, commerceRetries, commerceLag)
		for _, r := range []string{"applied", "deadlettered"} {
			commerceApplied.WithLabelValues(r).Add(0)
		}
	})
}

var commerceSubscriptionStatuses = map[string]bool{
	"active": true, "cancelled": true, "expired": true,
}

// decodeCommerce parses and validates one record.
func decodeCommerce(raw []byte) (CommerceEvent, error) {
	var ev CommerceEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return CommerceEvent{}, fmt.Errorf("decode: %w", err)
	}
	switch {
	case ev.EventID == "":
		return CommerceEvent{}, errors.New("missing event_id")
	case ev.Seq <= 0:
		return CommerceEvent{}, errors.New("missing seq")
	case ev.Subject.Kind == "" || ev.Subject.ID == "":
		return CommerceEvent{}, errors.New("missing subject")
	case ev.OccurredAt.IsZero():
		return CommerceEvent{}, errors.New("missing occurred_at")
	}
	switch ev.Type {
	case CommerceSubscriptionCreated, CommerceSubscriptionRenewed, CommerceSubscriptionResumed,
		CommerceSubscriptionCancelled, CommerceSubscriptionExpired:
		s := ev.Subscription
		if s == nil || s.ID == "" || s.SubscriberAccount == "" || s.CreatorAccount == "" {
			return CommerceEvent{}, fmt.Errorf("%s without the subscription's id and accounts", ev.Type)
		}
		if !commerceSubscriptionStatuses[s.Status] {
			return CommerceEvent{}, fmt.Errorf("%s with status %q", ev.Type, s.Status)
		}
		if s.PeriodEnd.IsZero() {
			return CommerceEvent{}, fmt.Errorf("%s without period_end", ev.Type)
		}
	case CommercePlanChanged:
		if ev.Plan == nil || ev.Plan.ID == "" || ev.Plan.CreatorAccount == "" {
			return CommerceEvent{}, errors.New("plan.changed without the plan's id and creator")
		}
	case CommercePurchaseDelivered:
		if ev.Purchase == nil || ev.Purchase.ID == "" || ev.Purchase.BuyerPIAL == "" {
			return CommerceEvent{}, errors.New("purchase.delivered without the purchase and its buyer")
		}
	default:
		return CommerceEvent{}, fmt.Errorf("unknown type %q", ev.Type)
	}
	return ev, nil
}

// StartCommerceConsumer subscribes to commerce.events. It returns a stop func.
func StartCommerceConsumer(ctx context.Context, brokers string, h CommerceHandlers) (stop func(), err error) {
	seeds := splitBrokers(brokers)
	if len(seeds) == 0 {
		return nil, errors.New("sitra: no brokers given for the commerce consumer")
	}
	if h.Apply == nil || h.DeadLetter == nil {
		return nil, errors.New("sitra: commerce consumer needs Apply and DeadLetter")
	}
	registerCommerceMetrics()

	client, err := kgo.NewClient(
		kgo.SeedBrokers(seeds...),
		kgo.ConsumerGroup(commerceGroup),
		kgo.ConsumeTopics(TopicCommerce),
		kgo.DisableAutoCommit(),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.SessionTimeout(30*time.Second),
		kgo.FetchMaxBytes(1<<20),
	)
	if err != nil {
		return nil, fmt.Errorf("sitra: commerce consumer client: %w", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		log.Printf("[sitra] commerce consumer started group=%s topic=%s brokers=%v", commerceGroup, TopicCommerce, seeds)
		backoff := time.Second
		for {
			fetches := client.PollFetches(runCtx)
			if runCtx.Err() != nil {
				return
			}
			if errs := fetches.Errors(); len(errs) > 0 {
				for _, fe := range errs {
					log.Printf("[sitra] commerce consumer poll error topic=%s partition=%d: %v", fe.Topic, fe.Partition, fe.Err)
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
				if !handleCommerceRecord(runCtx, rec, h) {
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
					log.Printf("[sitra] commerce consumer commit error (%d records): %v", len(handled), err)
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
				log.Printf("[sitra] commerce consumer did not stop within 5s; closing client anyway")
			}
			leaveCtx, leaveCancel := context.WithTimeout(context.Background(), 3*time.Second)
			if err := client.LeaveGroupContext(leaveCtx); err != nil {
				log.Printf("[sitra] commerce consumer leave group: %v", err)
			}
			leaveCancel()
			client.Close()
			log.Printf("[sitra] commerce consumer stopped group=%s", commerceGroup)
		})
	}
	return stop, nil
}

// handleCommerceRecord applies or dead-letters one record, retrying transient
// failures until it succeeds. It returns false only when ctx ended first.
func handleCommerceRecord(ctx context.Context, rec *kgo.Record, h CommerceHandlers) bool {
	backoff := 500 * time.Millisecond
	for {
		reason := ""
		ev, err := decodeCommerce(rec.Value)
		if err != nil {
			reason = "malformed: " + err.Error()
		} else {
			applyErr := applyCommerceRecovered(ctx, h.Apply, ev)
			switch {
			case applyErr == nil:
				commerceApplied.WithLabelValues("applied").Inc()
				commerceLag.Set(time.Since(ev.OccurredAt).Seconds())
				return true
			case errors.Is(applyErr, ErrPermanent):
				reason = applyErr.Error()
			default:
				commerceRetries.Inc()
				log.Printf("[sitra] commerce event %s (%s %s %s) not applied, retrying in %s: %v",
					ev.EventID, ev.Type, ev.Subject.Kind, ev.Subject.ID, backoff, applyErr)
				if !sleepCtx(ctx, backoff) {
					return false
				}
				backoff = minDuration(backoff*2, 30*time.Second)
				continue
			}
		}

		log.Printf("[sitra] CRITICAL: commerce record topic=%s partition=%d offset=%d can never be applied: %s — dead-lettered",
			rec.Topic, rec.Partition, rec.Offset, reason)
		if err := h.DeadLetter(ctx, rec.Topic, rec.Partition, rec.Offset, rec.Value, reason); err != nil {
			log.Printf("[sitra] commerce record offset=%d could not be dead-lettered, retrying in %s: %v", rec.Offset, backoff, err)
			if !sleepCtx(ctx, backoff) {
				return false
			}
			backoff = minDuration(backoff*2, 30*time.Second)
			continue
		}
		commerceApplied.WithLabelValues("deadlettered").Inc()
		return true
	}
}

// applyCommerceRecovered turns a panic in Apply into a permanent failure, so
// one poisoned record is dead-lettered instead of killing the consumer.
func applyCommerceRecovered(ctx context.Context, apply func(context.Context, CommerceEvent) error, ev CommerceEvent) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: apply panicked: %v", ErrPermanent, r)
		}
	}()
	return apply(observ.WithRequestID(ctx, ev.EventID), ev)
}
