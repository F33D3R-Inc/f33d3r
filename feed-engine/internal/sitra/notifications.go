package sitra

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/lib/pq"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
)

const (
	// TopicNotificationEvents carries notifications from brains to Herald.
	TopicNotificationEvents = "notification.events"
	// TopicNotificationUpdates carries unread counts from Herald to every replica.
	TopicNotificationUpdates = "notification.updates"

	notificationRelayInterval = 250 * time.Millisecond
	notificationRelayBatch    = 200
	notificationRelayLockKey  = int64(7331_0005)
	notificationRetention     = 7 * 24 * time.Hour
)

// NotificationUpdate is one account's new unread count.
type NotificationUpdate struct {
	Account string `json:"account"`
	Pial    string `json:"pial"`
	Unread  int    `json:"unread"`
	// AtMillis is when Herald announced this, for measuring time to the badge.
	AtMillis int64 `json:"at_ms"`
}

// StartNotificationRelay publishes notification_outbox rows in order. A row is
// marked only after the broker confirms it, and the first failure stops the batch.
// One relay runs at a time across replicas, held by an advisory lock.
func StartNotificationRelay(ctx context.Context, database *sql.DB, brokers string) (func(), error) {
	seeds := splitBrokers(brokers)
	if len(seeds) == 0 {
		return nil, errors.New("sitra: no brokers given for the notification relay")
	}
	client, err := kgo.NewClient(kgo.SeedBrokers(seeds...), kgo.RecordDeliveryTimeout(10*time.Second))
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(notificationRelayInterval)
		defer ticker.Stop()
		lastPrune := time.Now()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
			}
			if err := relayNotifications(runCtx, database, client); err != nil && runCtx.Err() == nil {
				log.Printf("[notification-relay] %v", err)
				select {
				case <-runCtx.Done():
					return
				case <-time.After(2 * time.Second):
				}
			}
			if time.Since(lastPrune) > time.Hour {
				lastPrune = time.Now()
				if _, err := database.ExecContext(runCtx,
					`DELETE FROM notification_outbox WHERE delivered_at < $1`,
					time.Now().Add(-notificationRetention)); err != nil {
					log.Printf("[notification-relay] prune: %v", err)
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

func relayNotifications(ctx context.Context, database *sql.DB, client *kgo.Client) error {
	conn, err := database.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	var locked bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, notificationRelayLockKey).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return nil
	}
	defer conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, notificationRelayLockKey)

	rows, err := conn.QueryContext(ctx, `
		SELECT id, recipient_pial, event::text FROM notification_outbox
		 WHERE delivered_at IS NULL AND blocked_at IS NULL
		 ORDER BY id LIMIT $1`, notificationRelayBatch)
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
		records = append(records, &kgo.Record{
			Topic: TopicNotificationEvents, Key: []byte(pial), Value: []byte(event),
		})
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
		// This row can never land, so it is kept and set aside rather than left to
		// stop everything behind it. Any other failure holds the queue.
		if errors.Is(res.Err, kerr.MessageTooLarge) {
			if _, err := conn.ExecContext(ctx,
				`UPDATE notification_outbox SET blocked_at = NOW(), blocked_reason = $2 WHERE id = $1`,
				ids[i], res.Err.Error()); err != nil {
				return err
			}
			log.Printf("[notification-relay] row %d quarantined: %v", ids[i], res.Err)
			continue
		}
		if firstErr == nil {
			firstErr = res.Err
		}
	}
	if len(delivered) > 0 {
		if _, err := conn.ExecContext(ctx,
			`UPDATE notification_outbox SET delivered_at = NOW() WHERE id = ANY($1)`,
			pq.Array(delivered)); err != nil {
			return err
		}
	}
	return firstErr
}

// StartNotificationUpdates hands every replica each account's new unread count.
// There is no group, because every replica must see every update.
func StartNotificationUpdates(ctx context.Context, brokers string, onUpdate func(NotificationUpdate), onGap func()) (func(), error) {
	seeds := splitBrokers(brokers)
	if len(seeds) == 0 {
		return nil, errors.New("sitra: no brokers given for notification updates")
	}
	client, err := kgo.NewClient(
		kgo.SeedBrokers(seeds...),
		kgo.ConsumeTopics(TopicNotificationUpdates),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtEnd()),
	)
	if err != nil {
		return nil, err
	}
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
				log.Printf("[notification-updates] %v", errs[0].Err)
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
				var u NotificationUpdate
				if err := json.Unmarshal(rec.Value, &u); err != nil || u.Account == "" {
					log.Printf("[notification-updates] unreadable update: %v", err)
					return
				}
				onUpdate(u)
			})
		}
	}()
	return func() {
		cancel()
		<-done
		client.Close()
	}, nil
}
