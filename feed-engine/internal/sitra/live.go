// Live relay: carries FA Live events between Nantar replicas.
//
// An SSE connection sits on exactly one replica, while the action that changes a
// Facet can land on any of them. Every replica publishes what it delivers and
// consumes what the others publish, so an update reaches the person whichever
// replica holds their connection.
//
// This is not a durable record. Live events describe state the owning brain still
// holds, so the topic keeps minutes of history and a replica joins at the end of
// it. A person who misses events is told to re-read (see handler/fa_live.go).
// There is deliberately no consumer group: every replica must see every event,
// and a group per replica would leave one abandoned group behind each deploy.
package sitra

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

// TopicLive is the topic replicas relay FA Live events over.
const TopicLive = "fa.live"

// LiveEnvelope is one event on TopicLive.
type LiveEnvelope struct {
	Origin string `json:"origin"`
	Kind   string `json:"kind"`
	Key    string `json:"key"`
	ID     int64  `json:"id,omitempty"`
	Type   string `json:"type,omitempty"`
	Data   string `json:"data,omitempty"`
	JSON   string `json:"json,omitempty"`
	Bulk   bool   `json:"bulk,omitempty"`
	Action string `json:"action,omitempty"`
}

// LiveBus publishes to and consumes from TopicLive.
type LiveBus struct {
	client *kgo.Client
	origin string
	logged atomic.Int64
}

// liveFailureLogInterval rate-limits relay failure logs. A broker outage fails
// every publish, and one line per interval is enough to see it.
const liveFailureLogInterval = 10 * time.Second

func (b *LiveBus) logFailure(what string, err error) {
	now := time.Now().UnixNano()
	last := b.logged.Load()
	if now-last < int64(liveFailureLogInterval) || !b.logged.CompareAndSwap(last, now) {
		return
	}
	log.Printf("[sitra] live relay %s: %v", what, err)
}

// Publish relays one envelope without blocking the caller. A full buffer or a
// dead broker costs other replicas this event, never the local delivery that has
// already happened. Records are handed over in call order, which keeps them in
// order per key.
func (b *LiveBus) Publish(env LiveEnvelope) {
	env.Origin = b.origin
	value, err := json.Marshal(env)
	if err != nil {
		b.logFailure("encode", err)
		return
	}
	rec := &kgo.Record{Topic: TopicLive, Key: []byte(env.Key), Value: value}
	b.client.TryProduce(context.Background(), rec, func(_ *kgo.Record, err error) {
		if err != nil {
			b.logFailure("produce", err)
		}
	})
}

// StartLiveBus connects to the broker and consumes TopicLive from its end.
// onEvent receives every event from other replicas. onGap is called when
// consuming recovers after an error, because events may have been lost during it.
func StartLiveBus(ctx context.Context, brokers, origin string, onEvent func(LiveEnvelope), onGap func()) (*LiveBus, func(), error) {
	seeds := splitBrokers(brokers)
	if len(seeds) == 0 {
		return nil, nil, errors.New("sitra: no brokers given for the live relay")
	}
	if origin == "" || onEvent == nil || onGap == nil {
		return nil, nil, errors.New("sitra: live relay needs an origin, an event handler and a gap handler")
	}
	client, err := kgo.NewClient(
		kgo.SeedBrokers(seeds...),
		kgo.ConsumeTopics(TopicLive),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtEnd()),
		kgo.RecordDeliveryTimeout(5*time.Second),
		kgo.MaxBufferedRecords(10_000),
		kgo.FetchMaxBytes(4<<20),
	)
	if err != nil {
		return nil, nil, err
	}
	bus := &LiveBus{client: client, origin: origin}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		troubled := false
		for {
			fetches := client.PollFetches(runCtx)
			if runCtx.Err() != nil {
				return
			}
			if errs := fetches.Errors(); len(errs) > 0 {
				troubled = true
				bus.logFailure("consume", errs[0].Err)
				select {
				case <-runCtx.Done():
					return
				case <-time.After(time.Second):
				}
				continue
			}
			if troubled {
				troubled = false
				onGap()
			}
			fetches.EachRecord(func(rec *kgo.Record) {
				var env LiveEnvelope
				if err := json.Unmarshal(rec.Value, &env); err != nil {
					bus.logFailure("decode", err)
					return
				}
				if env.Origin != origin {
					onEvent(env)
				}
			})
		}
	}()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				log.Printf("[sitra] live relay did not stop within 5s; closing client anyway")
			}
			client.Close()
		})
	}
	return bus, stop, nil
}
