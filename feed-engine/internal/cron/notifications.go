package cron

import (
	"context"
	"database/sql"
	"log"
	"time"

	"github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/f33d3r/feed-engine/internal/herald"
	"github.com/f33d3r/feed-engine/internal/observ"
)

const (
	notificationGaugeInterval     = 15 * time.Second
	notificationReconcileInterval = time.Minute
	// Long enough that Herald has had a fair chance to consume a delivered event.
	notificationReconcileAge   = 2 * time.Minute
	notificationReconcileBatch = 1000
	notificationMaxRepublish   = 3
)

var (
	outboxOldest = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "notification_outbox_oldest_seconds",
		Help: "Age of the oldest notification not yet delivered to Sitra Achra.",
	})
	outboxBlocked = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "notification_outbox_blocked",
		Help: "Notifications quarantined in the outbox.",
	})
	workNoticeOldest = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "notification_work_jobs_oldest_seconds",
		Help: "Age of the oldest overdue notification job for a new work.",
	})
	reconcileMissing = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "notification_reconcile_missing",
		Help: "Delivered notifications Herald has not stored, in the last check.",
	})
	reconcileLost = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "notification_reconcile_lost_total",
		Help: "Notifications Herald still has not stored after every republish.",
	})
)

func init() {
	observ.MustRegister(outboxOldest, outboxBlocked, workNoticeOldest, reconcileMissing, reconcileLost)
}

func notificationMonitor(database *sql.DB) {
	gauges := time.NewTicker(notificationGaugeInterval)
	reconcile := time.NewTicker(notificationReconcileInterval)
	defer gauges.Stop()
	defer reconcile.Stop()
	for {
		select {
		case <-gauges.C:
			var oldest float64
			var blocked int
			err := database.QueryRow(`
				SELECT COALESCE(EXTRACT(EPOCH FROM NOW() - MIN(created_at) FILTER (WHERE blocked_at IS NULL)), 0),
				       COUNT(*) FILTER (WHERE blocked_at IS NOT NULL)
				  FROM notification_outbox WHERE delivered_at IS NULL`).Scan(&oldest, &blocked)
			if err != nil {
				log.Printf("[notifications] outbox gauges: %v", err)
				continue
			}
			outboxOldest.Set(oldest)
			outboxBlocked.Set(float64(blocked))
			var overdue float64
			if err := database.QueryRow(`
				SELECT COALESCE(EXTRACT(EPOCH FROM NOW() - MIN(next_attempt_at)), 0)
				  FROM notification_work_jobs WHERE next_attempt_at <= NOW()`).Scan(&overdue); err != nil {
				log.Printf("[notifications] work job gauge: %v", err)
				continue
			}
			workNoticeOldest.Set(overdue)
		case <-reconcile.C:
			if err := reconcileNotifications(database); err != nil {
				log.Printf("[notifications] reconcile: %v", err)
			}
		}
	}
}

// reconcileNotifications asks Herald which delivered events it has not stored, and
// publishes those again. Herald stores by event ID, so a repeat is harmless.
func reconcileNotifications(database *sql.DB) error {
	rows, err := database.Query(`
		SELECT event_id::text, reconcile_attempts FROM notification_outbox
		 WHERE delivered_at IS NOT NULL AND reconciled_at IS NULL AND delivered_at < $1
		 ORDER BY delivered_at LIMIT $2`,
		time.Now().Add(-notificationReconcileAge), notificationReconcileBatch)
	if err != nil {
		return err
	}
	var ids []string
	attempts := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
		attempts[id] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(ids) == 0 {
		reconcileMissing.Set(0)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	missing, err := herald.Missing(ctx, ids)
	if err != nil {
		return err
	}
	reconcileMissing.Set(float64(len(missing)))

	gone := map[string]bool{}
	var republish, lost []string
	for _, id := range missing {
		gone[id] = true
		if attempts[id] < notificationMaxRepublish {
			republish = append(republish, id)
		} else {
			lost = append(lost, id)
		}
	}
	var stored []string
	for _, id := range ids {
		if !gone[id] {
			stored = append(stored, id)
		}
	}
	if len(lost) > 0 {
		reconcileLost.Add(float64(len(lost)))
		log.Printf("[notifications] Herald never stored %d notifications after republishing: %v", len(lost), lost)
	}
	if _, err := database.Exec(`
		UPDATE notification_outbox SET reconciled_at = NOW()
		 WHERE event_id = ANY($1::uuid[])`, pq.Array(append(stored, lost...))); err != nil {
		return err
	}
	_, err = database.Exec(`
		UPDATE notification_outbox SET delivered_at = NULL, reconcile_attempts = reconcile_attempts + 1
		 WHERE event_id = ANY($1::uuid[])`, pq.Array(republish))
	return err
}
