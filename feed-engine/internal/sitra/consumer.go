// Frequency event consumer — Nantar's first Sitra Achra consumer.
//
// Auralis (the Frequencies brain) publishes every Frequency mutation to the
// topic frequency.events, keyed by frequency_id. Nantar consumes them to
// re-render Facets and push them over FA Live. Two properties decide the
// shape of this consumer:
//
//   - Every Nantar replica must see every event, because an SSE session lives
//     on exactly one replica and only that replica can push to it. So the
//     consumer group is per instance (nantar-frequency-<hostname>), not a
//     shared group that would split the topic between replicas.
//   - A fresh replica starts at the END of the topic. History is a Frequency's
//     durable state in Auralis, which Nantar reads on demand; replaying old
//     events into fresh SSE sessions would render stale mutations over current
//     state. A replica that restarts resumes from its committed offset.
//
// Offsets are committed only after onEvent has returned, so a crash mid-render
// redelivers. Redelivery is harmless because every event id is deduplicated
// in a bounded in-process set. A malformed record is logged with its
// coordinates and skipped; a panic in onEvent is recovered and counted; the
// loop never dies for one bad record.
package sitra

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/f33d3r/feed-engine/internal/observ"
)

// TopicFrequency is the topic Auralis produces Frequency events to.
const TopicFrequency = "frequency.events"

// FrequencyEvent is one decoded event from frequency.events. The envelope is
// events/taxonomy.md's plus the two fields every Frequency event carries.
type FrequencyEvent struct {
	EventID       string
	EventType     string
	FrequencyID   string
	PialID        string
	CorrelationID string
	Timestamp     time.Time
	Payload       json.RawMessage
}

// wireEvent is the JSON shape on the topic.
type wireEvent struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	SchemaVersion string          `json:"schema_version"`
	PialID        string          `json:"pial_id"`
	Timestamp     time.Time       `json:"timestamp"`
	FrequencyID   string          `json:"frequency_id"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// ── metrics ──────────────────────────────────────────────────────────────────

var (
	consumerMetricsOnce sync.Once

	consumedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sitra_frequency_events_consumed_total",
			Help: "frequency.events records handled, by result.",
		},
		[]string{"result"},
	)
	consumerLag = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "sitra_frequency_consumer_lag_seconds",
		Help: "Seconds between the last consumed record's timestamp and now.",
	})
	consumerErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "sitra_frequency_consumer_errors_total",
		Help: "Poll/commit errors on the frequency.events consumer.",
	})
)

func registerConsumerMetrics() {
	consumerMetricsOnce.Do(func() {
		observ.Registry().MustRegister(consumedTotal, consumerLag, consumerErrors)
		for _, r := range []string{"ok", "duplicate", "malformed", "panic"} {
			consumedTotal.WithLabelValues(r).Add(0)
		}
	})
}

// ── dedup ────────────────────────────────────────────────────────────────────

// dedup remembers the last N event ids. Bounded: when full, the oldest id is
// forgotten. Redelivery windows are seconds, not days, so 10k is generous.
type dedup struct {
	mu    sync.Mutex
	cap   int
	seen  map[string]struct{}
	order []string
	head  int
}

func newDedup(capacity int) *dedup {
	if capacity < 1 {
		capacity = 1
	}
	return &dedup{cap: capacity, seen: make(map[string]struct{}, capacity), order: make([]string, capacity)}
}

// markSeen returns true if id was already present.
func (d *dedup) markSeen(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.seen[id]; ok {
		return true
	}
	if old := d.order[d.head]; old != "" {
		delete(d.seen, old)
	}
	d.order[d.head] = id
	d.head = (d.head + 1) % d.cap
	d.seen[id] = struct{}{}
	return false
}

// ── per-record handling ──────────────────────────────────────────────────────

type handleResult string

const (
	resultOK        handleResult = "ok"
	resultDuplicate handleResult = "duplicate"
	resultMalformed handleResult = "malformed"
	resultPanic     handleResult = "panic"
)

// decodeRecord parses one record. It refuses anything missing the envelope
// fields a consumer needs to act on.
func decodeRecord(rec *kgo.Record) (FrequencyEvent, error) {
	var w wireEvent
	if err := json.Unmarshal(rec.Value, &w); err != nil {
		return FrequencyEvent{}, fmt.Errorf("decode: %w", err)
	}
	switch {
	case w.EventID == "":
		return FrequencyEvent{}, errors.New("missing event_id")
	case w.EventType == "":
		return FrequencyEvent{}, errors.New("missing event_type")
	case w.FrequencyID == "":
		return FrequencyEvent{}, errors.New("missing frequency_id")
	case w.Timestamp.IsZero():
		return FrequencyEvent{}, errors.New("missing timestamp")
	}
	return FrequencyEvent{
		EventID:       w.EventID,
		EventType:     w.EventType,
		FrequencyID:   w.FrequencyID,
		PialID:        w.PialID,
		CorrelationID: w.CorrelationID,
		Timestamp:     w.Timestamp,
		Payload:       w.Payload,
	}, nil
}

// handleRecord decodes, deduplicates and dispatches one record. It never
// panics and never returns an error: every outcome is a result label, because
// the loop's only decision afterwards is to commit, which it always does.
func handleRecord(ctx context.Context, rec *kgo.Record, seen *dedup, onEvent func(context.Context, FrequencyEvent)) (res handleResult) {
	ev, err := decodeRecord(rec)
	if err != nil {
		log.Printf("[sitra] frequency consumer: malformed record topic=%s partition=%d offset=%d: %v",
			rec.Topic, rec.Partition, rec.Offset, err)
		consumedTotal.WithLabelValues(string(resultMalformed)).Inc()
		return resultMalformed
	}
	if seen.markSeen(ev.EventID) {
		consumedTotal.WithLabelValues(string(resultDuplicate)).Inc()
		return resultDuplicate
	}
	consumerLag.Set(time.Since(ev.Timestamp).Seconds())

	defer func() {
		if r := recover(); r != nil {
			log.Printf("[sitra] frequency consumer: handler panic event_id=%s event_type=%s frequency_id=%s correlation_id=%s: %v",
				ev.EventID, ev.EventType, ev.FrequencyID, ev.CorrelationID, r)
			consumedTotal.WithLabelValues(string(resultPanic)).Inc()
			res = resultPanic
		}
	}()
	ctx = observ.WithRequestID(ctx, ev.CorrelationID)
	onEvent(ctx, ev)
	consumedTotal.WithLabelValues(string(resultOK)).Inc()
	return resultOK
}

// ── consumer ─────────────────────────────────────────────────────────────────

func groupName() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return "nantar-frequency-" + h
	}
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "nantar-frequency-" + fmt.Sprint(time.Now().UnixNano())
	}
	return "nantar-frequency-" + hex.EncodeToString(b[:])
}

// StartFrequencyConsumer subscribes to topic frequency.events and calls
// onEvent for each new event, in per-Frequency order. Returns a stop func that
// leaves the group and closes the client.
//
// The returned error is only for a client that cannot be constructed (bad
// broker list). Brokers that are down at start are not an error: franz-go
// reconnects, and until then poll errors are logged and counted.
func StartFrequencyConsumer(ctx context.Context, brokers string, onEvent func(ctx context.Context, ev FrequencyEvent)) (stop func(), err error) {
	seeds := splitBrokers(brokers)
	if len(seeds) == 0 {
		return nil, errors.New("sitra: no brokers given for the frequency consumer")
	}
	if onEvent == nil {
		return nil, errors.New("sitra: frequency consumer needs an onEvent handler")
	}
	registerConsumerMetrics()

	group := groupName()
	client, err := kgo.NewClient(
		kgo.SeedBrokers(seeds...),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(TopicFrequency),
		kgo.DisableAutoCommit(),
		// A fresh replica joins at the end: current state comes from Auralis,
		// not from replaying mutations that already happened.
		kgo.ConsumeResetOffset(kgo.NewOffset().AtEnd()),
		kgo.SessionTimeout(30*time.Second),
		// Bounded polls: one Frequency event is a few hundred bytes; a poll of
		// a megabyte is thousands of them, more than one loop turn should hold.
		kgo.FetchMaxBytes(1<<20),
		kgo.FetchMaxPartitionBytes(512<<10),
	)
	if err != nil {
		return nil, fmt.Errorf("sitra: frequency consumer client: %w", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	seen := newDedup(10_000)

	go func() {
		defer close(done)
		backoff := time.Second
		log.Printf("[sitra] frequency consumer started group=%s topic=%s brokers=%v", group, TopicFrequency, seeds)
		for {
			fetches := client.PollFetches(runCtx)
			if runCtx.Err() != nil {
				return
			}
			if errs := fetches.Errors(); len(errs) > 0 {
				for _, fe := range errs {
					consumerErrors.Inc()
					log.Printf("[sitra] frequency consumer poll error topic=%s partition=%d: %v", fe.Topic, fe.Partition, fe.Err)
				}
				// Do not spin on a dead broker; franz-go reconnects underneath.
				select {
				case <-runCtx.Done():
					return
				case <-time.After(backoff):
				}
				backoff *= 2
				if backoff > 30*time.Second {
					backoff = 30 * time.Second
				}
				continue
			}
			backoff = time.Second

			var handled []*kgo.Record
			fetches.EachRecord(func(rec *kgo.Record) {
				handleRecord(runCtx, rec, seen, onEvent)
				handled = append(handled, rec)
			})
			if len(handled) == 0 {
				continue
			}
			// Commit only what onEvent has already seen. A crash before this
			// line redelivers; the dedup set makes that a no-op.
			commitCtx, commitCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := client.CommitRecords(commitCtx, handled...); err != nil {
				consumerErrors.Inc()
				log.Printf("[sitra] frequency consumer commit error (%d records): %v", len(handled), err)
			}
			commitCancel()
		}
	}()

	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				log.Printf("[sitra] frequency consumer did not stop within 5s; closing client anyway")
			}
			// Leaving the group promptly hands its partition to no one — it is
			// a per-instance group — but it does let the broker forget us.
			leaveCtx, leaveCancel := context.WithTimeout(context.Background(), 3*time.Second)
			if err := client.LeaveGroupContext(leaveCtx); err != nil {
				log.Printf("[sitra] frequency consumer leave group: %v", err)
			}
			leaveCancel()
			client.Close()
			log.Printf("[sitra] frequency consumer stopped group=%s", group)
		})
	}
	return stop, nil
}
