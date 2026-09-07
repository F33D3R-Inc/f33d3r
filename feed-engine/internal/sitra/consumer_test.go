package sitra

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

func record(t *testing.T, v any) *kgo.Record {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return &kgo.Record{Topic: TopicFrequency, Partition: 0, Offset: 1, Value: b}
}

func goodEvent(id string) map[string]any {
	return map[string]any{
		"event_id":       id,
		"event_type":     "frequency.started",
		"schema_version": "1.0",
		"pial_id":        "c0ffee00-0000-4000-8000-000000000001",
		"timestamp":      time.Now().UTC().Format(time.RFC3339Nano),
		"frequency_id":   "11111111-2222-4333-8444-555555555555",
		"correlation_id": "req-1",
		"payload":        map[string]any{"title": "t"},
	}
}

func TestDecodeRecordCarriesTheEnvelope(t *testing.T) {
	ev, err := decodeRecord(record(t, goodEvent("e1")))
	if err != nil {
		t.Fatal(err)
	}
	if ev.EventID != "e1" || ev.EventType != "frequency.started" || ev.FrequencyID != "11111111-2222-4333-8444-555555555555" {
		t.Fatalf("envelope: %+v", ev)
	}
	if ev.CorrelationID != "req-1" || ev.PialID == "" || ev.Timestamp.IsZero() {
		t.Fatalf("envelope: %+v", ev)
	}
	var p map[string]any
	if err := json.Unmarshal(ev.Payload, &p); err != nil || p["title"] != "t" {
		t.Fatalf("payload: %s %v", ev.Payload, err)
	}
}

func TestDecodeRecordRefusesMissingFields(t *testing.T) {
	for _, drop := range []string{"event_id", "event_type", "frequency_id", "timestamp"} {
		e := goodEvent("e2")
		delete(e, drop)
		if _, err := decodeRecord(record(t, e)); err == nil {
			t.Errorf("missing %s accepted", drop)
		}
	}
	if _, err := decodeRecord(&kgo.Record{Value: []byte("not json")}); err == nil {
		t.Error("garbage accepted")
	}
}

func TestHandleRecordDispatchesOnceAndDeduplicates(t *testing.T) {
	registerConsumerMetrics()
	seen := newDedup(10)
	var calls int
	on := func(_ context.Context, ev FrequencyEvent) { calls++ }

	if r := handleRecord(context.Background(), record(t, goodEvent("dup")), seen, on); r != resultOK {
		t.Fatalf("first: %s", r)
	}
	if r := handleRecord(context.Background(), record(t, goodEvent("dup")), seen, on); r != resultDuplicate {
		t.Fatalf("second: %s", r)
	}
	if calls != 1 {
		t.Fatalf("handler called %d times", calls)
	}
}

func TestHandleRecordMalformedIsSkippedNotFatal(t *testing.T) {
	registerConsumerMetrics()
	seen := newDedup(10)
	called := false
	r := handleRecord(context.Background(), &kgo.Record{Value: []byte("{")}, seen, func(context.Context, FrequencyEvent) { called = true })
	if r != resultMalformed || called {
		t.Fatalf("result=%s called=%v", r, called)
	}
}

func TestHandleRecordRecoversAPanic(t *testing.T) {
	registerConsumerMetrics()
	seen := newDedup(10)
	r := handleRecord(context.Background(), record(t, goodEvent("boom")), seen, func(context.Context, FrequencyEvent) { panic("render failed") })
	if r != resultPanic {
		t.Fatalf("result=%s", r)
	}
	// The id was recorded before the panic, so a redelivery does not re-run
	// a handler that already blew up on it.
	r = handleRecord(context.Background(), record(t, goodEvent("boom")), seen, func(context.Context, FrequencyEvent) { t.Fatal("re-run") })
	if r != resultDuplicate {
		t.Fatalf("redelivery result=%s", r)
	}
}

func TestDedupIsBoundedAndForgetsOldest(t *testing.T) {
	d := newDedup(3)
	for _, id := range []string{"a", "b", "c"} {
		if d.markSeen(id) {
			t.Fatalf("%s seen early", id)
		}
	}
	if !d.markSeen("a") {
		t.Fatal("a forgotten too early")
	}
	if d.markSeen("d") {
		t.Fatal("d seen early")
	}
	// Capacity 3 holding a,b,c; adding d evicted a.
	if d.markSeen("a") {
		t.Fatal("a should have been evicted")
	}
	if len(d.seen) > 3 {
		t.Fatalf("set grew to %d", len(d.seen))
	}
}

func TestStartRefusesNoBrokersOrHandler(t *testing.T) {
	if _, err := StartFrequencyConsumer(context.Background(), "", func(context.Context, FrequencyEvent) {}); err == nil {
		t.Fatal("empty brokers accepted")
	}
	if _, err := StartFrequencyConsumer(context.Background(), "localhost:1", nil); err == nil {
		t.Fatal("nil handler accepted")
	}
}

// Against a real broker only. SITRA_TEST_BROKERS=localhost:9092 go test ./internal/sitra/
func TestConsumerAgainstBroker(t *testing.T) {
	brokers := os.Getenv("SITRA_TEST_BROKERS")
	if brokers == "" {
		t.Skip("SITRA_TEST_BROKERS not set — broker-backed test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var mu sync.Mutex
	got := map[string]FrequencyEvent{}
	stop, err := StartFrequencyConsumer(ctx, brokers, func(_ context.Context, ev FrequencyEvent) {
		mu.Lock()
		got[ev.EventID] = ev
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// The group joins at the end; give it a moment to be assigned before producing.
	time.Sleep(3 * time.Second)

	prod, err := kgo.NewClient(kgo.SeedBrokers(splitBrokers(brokers)...))
	if err != nil {
		t.Fatal(err)
	}
	defer prod.Close()
	id := fmt.Sprintf("test-%d", time.Now().UnixNano())
	b, _ := json.Marshal(goodEvent(id))
	if err := prod.ProduceSync(ctx, &kgo.Record{Topic: TopicFrequency, Key: []byte("k"), Value: b}).FirstErr(); err != nil {
		t.Fatal(err)
	}
	// Duplicate on the wire: must be handled once.
	if err := prod.ProduceSync(ctx, &kgo.Record{Topic: TopicFrequency, Key: []byte("k"), Value: b}).FirstErr(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		_, ok := got[id]
		mu.Unlock()
		if ok {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if _, ok := got[id]; !ok {
		t.Fatalf("event %s never consumed", id)
	}
	// stop twice must be safe.
	stop()
	stop()
}
