// Package sitra is the Sitra Achra event backbone — a fire-and-forget Kafka
// producer that publishes domain events from Nantar (feed-engine) to Redpanda.
//
// Design principles:
//   - Lazy init: if Redpanda is unreachable at startup, Nantar starts anyway and
//     serves requests normally. The producer is initialised on first successful
//     Publish call.
//   - Fire-and-forget: Publish never blocks the HTTP response. Errors are logged
//     but never returned to the caller.
//   - PIAL partitioning: callers supply the PIAL UUID as the message key so all
//     events from the same identity land on the same partition (ordered per user).
//   - Topic auto-creation: Redpanda auto_create_topics_enabled=true means we
//     never need to pre-create topics.
package sitra

import (
	"context"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

// Topics published by Nantar.
const (
	TopicContent = "content.events"
	TopicRanking = "ranking.events"
)

// Producer wraps a franz-go Kafka client for fire-and-forget event publishing.
// The zero value is not usable; construct via New.
type Producer struct {
	brokers []string

	mu     sync.Mutex
	client *kgo.Client // nil until first successful init
	failed bool        // set permanently if init fails after all retries
}

// New creates a Producer reading broker addresses from the KAFKA_BROKERS
// environment variable (comma-separated). If the env var is empty, the
// producer silently no-ops on every Publish call.
func New() *Producer {
	raw := os.Getenv("KAFKA_BROKERS")
	if raw == "" {
		log.Println("[sitra] KAFKA_BROKERS not set — event publishing disabled")
		return &Producer{}
	}
	brokers := splitBrokers(raw)
	log.Printf("[sitra] producer initialised, brokers: %v", brokers)
	return &Producer{brokers: brokers}
}

// Publish sends a message to the given topic asynchronously.
// key should be the PIAL UUID when available (ensures ordered delivery per identity).
// value must be valid JSON. Errors are logged; the caller is never blocked.
func (p *Producer) Publish(ctx context.Context, topic string, key, value []byte) {
	if len(p.brokers) == 0 {
		return // disabled — KAFKA_BROKERS not set
	}
	go func() {
		client, ok := p.client_(ctx)
		if !ok {
			return
		}
		rec := &kgo.Record{
			Topic: topic,
			Key:   key,
			Value: value,
		}
		client.Produce(ctx, rec, func(r *kgo.Record, err error) {
			if err != nil {
				log.Printf("[sitra] produce error topic=%s key=%s: %v", topic, key, err)
			}
		})
	}()
}

// Close shuts down the underlying Kafka client gracefully.
// Should be called once on application shutdown.
func (p *Producer) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		p.client.Close()
		p.client = nil
	}
}

// client_ returns the initialised kgo.Client, creating it lazily if needed.
// Returns (client, true) on success, (nil, false) if init has permanently failed.
func (p *Producer) client_(ctx context.Context) (*kgo.Client, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.client != nil {
		return p.client, true
	}
	if p.failed {
		return nil, false
	}

	client, err := kgo.NewClient(
		kgo.SeedBrokers(p.brokers...),
		kgo.ProduceRequestTimeout(5*time.Second),
		// RequiredAcks=1: leader ack only — best performance for fire-and-forget.
		kgo.RequiredAcks(kgo.LeaderAck()),
		// Allow up to 100ms of batching before flushing.
		kgo.ProducerBatchMaxBytes(1<<20), // 1 MiB
	)
	if err != nil {
		log.Printf("[sitra] client init error: %v — event publishing disabled", err)
		p.failed = true
		return nil, false
	}

	// Verify connectivity with a quick metadata fetch (non-blocking, 3s timeout).
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx); err != nil {
		log.Printf("[sitra] broker unreachable: %v — will retry on next publish", err)
		client.Close()
		// Don't mark failed=true here — allow retry on next Publish call.
		return nil, false
	}

	p.client = client
	log.Printf("[sitra] connected to Redpanda, brokers: %v", p.brokers)
	return p.client, true
}

func splitBrokers(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, b := range parts {
		b = strings.TrimSpace(b)
		if b != "" {
			out = append(out, b)
		}
	}
	return out
}
