package aethyr

// relay.go — publishes ranking_outbox to Sitra Achra ranking.events.
//
// Rows are queued with the action they describe (db.EnqueueRankingSignal). One
// relay runs at a time across replicas, held by advisory lock 7331_0042, and
// publishes in id order keyed by the viewer's PIAL, so one person's signals
// arrive in the order they happened. A row is marked only after the broker has
// confirmed it, and the first failure stops the batch: an outage holds the
// queue and loses nothing. A row the broker can never take (too large) is
// quarantined, kept for a person, and does not stop the rows behind it.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/f33d3r/feed-engine/internal/observ"
	"github.com/f33d3r/feed-engine/internal/sitra"
)

const (
	relayInterval  = 250 * time.Millisecond
	relayBatch     = 200
	relayLockKey   = int64(7331_0042)
	relayRetention = 7 * 24 * time.Hour
	relayReport    = time.Minute
)

var (
	relayDelivered = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ranking_outbox_delivered_total",
		Help: "Ranking signals confirmed by Sitra Achra.",
	})
	relayFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ranking_outbox_failures_total",
		Help: "Relay passes that stopped on a failure; the rows behind it wait.",
	})
	relayWaiting = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ranking_outbox_waiting",
		Help: "Ranking signals not yet delivered.",
	})
	relayOldestSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ranking_outbox_oldest_waiting_seconds",
		Help: "Age of the oldest undelivered ranking signal.",
	})
	relayQuarantined = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ranking_outbox_quarantined",
		Help: "Ranking signals the broker can never take, waiting for a person.",
	})
)

func init() {
	observ.MustRegister(relayDelivered, relayFailures, relayWaiting, relayOldestSeconds, relayQuarantined)
}

// StartSignalRelay starts publishing ranking signals. Without brokers the rows
// are kept, and said to be kept, until a replica with a broker runs.
func StartSignalRelay(ctx context.Context, database *sql.DB, brokers string) (func(), error) {
	var seeds []string
	for _, b := range strings.Split(brokers, ",") {
		if b = strings.TrimSpace(b); b != "" {
			seeds = append(seeds, b)
		}
	}
	if len(seeds) == 0 {
		log.Printf("[ranking-relay] KAFKA_BROKERS is empty: ranking signals are kept in ranking_outbox until a replica with a broker runs")
		return func() {}, nil
	}
	client, err := kgo.NewClient(
		kgo.SeedBrokers(seeds...),
		kgo.RecordDeliveryTimeout(10*time.Second),
		kgo.RequiredAcks(kgo.AllISRAcks()),
	)
	if err != nil {
		return nil, fmt.Errorf("ranking relay: connecting to Sitra Achra: %w", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(relayInterval)
		defer ticker.Stop()
		lastPrune, lastReport := time.Now(), time.Time{}
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
			}
			if err := relaySignals(runCtx, database, client); err != nil && runCtx.Err() == nil {
				relayFailures.Inc()
				log.Printf("[ranking-relay] %v", err)
				select {
				case <-runCtx.Done():
					return
				case <-time.After(2 * time.Second):
				}
			}
			if time.Since(lastReport) >= relayReport {
				lastReport = time.Now()
				reportRelay(runCtx, database)
			}
			if time.Since(lastPrune) > time.Hour {
				lastPrune = time.Now()
				if _, err := database.ExecContext(runCtx,
					`DELETE FROM ranking_outbox WHERE delivered_at < $1`,
					time.Now().Add(-relayRetention)); err != nil {
					log.Printf("[ranking-relay] prune: %v", err)
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
		client.Close()
	}, nil
}

func relaySignals(ctx context.Context, database *sql.DB, client *kgo.Client) error {
	conn, err := database.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	var locked bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, relayLockKey).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return nil
	}
	defer func() {
		if _, err := conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, relayLockKey); err != nil {
			log.Printf("[ranking-relay] releasing the relay lock: %v", err)
		}
	}()

	rows, err := conn.QueryContext(ctx, `
		SELECT id, viewer_pial, event::text FROM ranking_outbox
		 WHERE delivered_at IS NULL AND quarantined_at IS NULL
		 ORDER BY id LIMIT $1`, relayBatch)
	if err != nil {
		return err
	}
	var ids []int64
	var records []*kgo.Record
	for rows.Next() {
		var id int64
		var pial, event string
		if err := rows.Scan(&id, &pial, &event); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
		records = append(records, &kgo.Record{Topic: sitra.TopicRanking, Key: []byte(pial), Value: []byte(event)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(records) == 0 {
		return nil
	}

	var delivered []int64
	var firstErr error
	for i, res := range client.ProduceSync(ctx, records...) {
		if res.Err == nil {
			if firstErr == nil {
				delivered = append(delivered, ids[i])
			}
			continue
		}
		if errors.Is(res.Err, kerr.MessageTooLarge) {
			if _, err := conn.ExecContext(ctx,
				`UPDATE ranking_outbox SET quarantined_at = NOW(), quarantine_reason = $2 WHERE id = $1`,
				ids[i], res.Err.Error()); err != nil {
				return err
			}
			log.Printf("[ranking-relay] INCIDENT: row %d quarantined: %v", ids[i], res.Err)
			continue
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("producing ranking signal row %d: %w", ids[i], res.Err)
		}
	}
	if len(delivered) > 0 {
		if _, err := conn.ExecContext(ctx,
			`UPDATE ranking_outbox SET delivered_at = NOW() WHERE id = ANY($1)`,
			pq.Array(delivered)); err != nil {
			return err
		}
		relayDelivered.Add(float64(len(delivered)))
	}
	return firstErr
}

// reportRelay publishes the backlog, and says so while anything is stuck.
func reportRelay(ctx context.Context, database *sql.DB) {
	var waiting, quarantined int64
	var oldest sql.NullFloat64
	err := database.QueryRowContext(ctx, `
		SELECT COUNT(*) FILTER (WHERE delivered_at IS NULL AND quarantined_at IS NULL),
		       COUNT(*) FILTER (WHERE quarantined_at IS NOT NULL),
		       EXTRACT(EPOCH FROM NOW() - MIN(created_at) FILTER (WHERE delivered_at IS NULL AND quarantined_at IS NULL))
		  FROM ranking_outbox`).Scan(&waiting, &quarantined, &oldest)
	if err != nil {
		log.Printf("[ranking-relay] reading the backlog: %v", err)
		return
	}
	relayWaiting.Set(float64(waiting))
	relayQuarantined.Set(float64(quarantined))
	relayOldestSeconds.Set(oldest.Float64)
	if quarantined > 0 {
		log.Printf("[ranking-relay] INCIDENT: %d ranking signal(s) quarantined — see migration 0042 for the operator query", quarantined)
	}
	if oldest.Valid && oldest.Float64 > 300 {
		log.Printf("[ranking-relay] INCIDENT: %d ranking signal(s) waiting, the oldest for %.0fs", waiting, oldest.Float64)
	}
}
