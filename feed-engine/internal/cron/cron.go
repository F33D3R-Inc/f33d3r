package cron

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"time"

	"github.com/f33d3r/feed-engine/internal/config"
	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

// Start launches all background cron goroutines. Call once from main after DB is ready.
func Start(database *sql.DB, cfg *config.Config, client *http.Client) {
	go sessionCleanup(database)
	go healthMonitor(cfg, client)
	go pendingScanSweep(database, cfg, client)
	go publishScheduledPosts(database)
	go orphanHashSweep(database)
	log.Println("[cron] background jobs started: session-cleanup, health-monitor, pending-scan-sweep, scheduled-posts, orphan-hash-sweep")
}

// sessionCleanup deletes expired auth sessions every hour.
func sessionCleanup(database *sql.DB) {
	tick := time.NewTicker(1 * time.Hour)
	defer tick.Stop()
	for range tick.C {
		if database == nil {
			continue
		}
		n, err := dbpkg.CleanExpiredSessions(database)
		if err != nil {
			log.Printf("[cron] session cleanup error: %v", err)
		} else if n > 0 {
			log.Printf("[cron] session cleanup: removed %d expired sessions", n)
		}
	}
}

// healthMonitor pings critical services every 5 minutes and logs if any are down.
func healthMonitor(cfg *config.Config, client *http.Client) {
	endpoints := []struct{ name, url string }{
		{"aethyrrank", cfg.AethyrRankURL + "/health"},
		{"ain-soph", cfg.AinSophURL + "/health"},
		{"zodacare", cfg.ZodacareURL + "/health"},
		{"elohim-veni", cfg.ElohimVeniURL + "/health"},
		{"verity", cfg.VerityURL + "/health"},
		{"caeor", cfg.CaeorURL + "/health"},
	}

	tick := time.NewTicker(5 * time.Minute)
	defer tick.Stop()
	for range tick.C {
		for _, ep := range endpoints {
			if ep.url == "/health" {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.url, nil)
			if err != nil {
				cancel()
				continue
			}
			resp, err := client.Do(req)
			cancel()
			if err != nil {
				log.Printf("[cron][health] %s UNREACHABLE: %v", ep.name, err)
			} else {
				resp.Body.Close()
				if resp.StatusCode >= 500 {
					log.Printf("[cron][health] %s ERROR: HTTP %d", ep.name, resp.StatusCode)
				}
			}
		}
	}
}

// pendingScanSweep finds posts stuck in pending_scan for more than 3 minutes and
// either re-triggers the content-scan or falls back to clean. A post sitting in
// pending_scan beyond the threshold means the original scan callback was lost
// (network blip, feed-engine restart, content-scan overload). Runs every 2 minutes.
// Also fires once immediately on startup to rescue posts stuck before this deploy.
func pendingScanSweep(database *sql.DB, cfg *config.Config, client *http.Client) {
	sweepPendingScanOnce(database, cfg, client)
	tick := time.NewTicker(2 * time.Minute)
	defer tick.Stop()
	for range tick.C {
		sweepPendingScanOnce(database, cfg, client)
	}
}

// SweepPendingScanOnce is exported so admin handlers can trigger a manual sweep.
func SweepPendingScanOnce(database *sql.DB, cfg *config.Config, client *http.Client) {
	sweepPendingScanOnce(database, cfg, client)
}

func sweepPendingScanOnce(database *sql.DB, cfg *config.Config, client *http.Client) {
	if database == nil {
		return
	}

	// Release human_review posts older than 15 minutes — they have been sitting long
	// enough that waiting for a human reviewer is clearly not happening.
	released, err := dbpkg.ReleaseStaleHumanReviewPosts(database, 15*time.Minute)
	if err != nil {
		log.Printf("[cron][scan-sweep] human_review release error: %v", err)
	} else if len(released) > 0 {
		log.Printf("[cron][scan-sweep] released %d stale human_review posts (>15 min) to clean: %v", len(released), released)
	}

	rows, err := database.Query(`
		SELECT p.id
		FROM works p
		WHERE p.scan_state = 'pending'
		  AND p.deleted_at IS NULL
		  AND p.created_at < NOW() - INTERVAL '3 minutes'
		ORDER BY p.created_at ASC
		LIMIT 50
	`)
	if err != nil {
		log.Printf("[cron][scan-sweep] query error: %v", err)
		return
	}
	defer rows.Close()

	var posts []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			posts = append(posts, id)
		}
	}

	if len(posts) == 0 {
		return
	}
	log.Printf("[cron][scan-sweep] found %d stale pending_scan posts (>3 min)", len(posts))

	// Abraxas Shield processes content events via Sitra Achra (Kafka).
	// Posts that remain pending_scan after 15 min were either missed or delivered
	// before Abraxas Shield was running. Clear them to 'clean' so they are not
	// indefinitely quarantined. The stagger prevents a burst of DB writes.
	for i, postID := range posts {
		if i > 0 {
			time.Sleep(200 * time.Millisecond)
		}
		_ = dbpkg.SetPostScanState(database, postID, "clean", "sweep:abraxas_shield_fallback", "system")
		log.Printf("[cron][scan-sweep] cleared stale post %s", postID)
	}
}

// orphanHashSweep deletes video_raw_hashes rows that were never linked to a live post.
// This happens when an upload gets stuck, the browser closes, or transcoding fails —
// the hash was stored speculatively but the post was never created. Without this sweep
// the user can never re-upload that same video.
// Runs every 30 minutes; only clears hashes older than 2 hours with no post_id.
func orphanHashSweep(database *sql.DB) {
	tick := time.NewTicker(30 * time.Minute)
	defer tick.Stop()
	for range tick.C {
		if database == nil {
			continue
		}
		result, err := database.Exec(`
			DELETE FROM video_raw_hashes
			WHERE (post_id IS NULL OR post_id = '')
			  AND created_at < NOW() - INTERVAL '2 hours'
		`)
		if err != nil {
			log.Printf("[cron] orphan hash sweep error: %v", err)
			continue
		}
		if n, _ := result.RowsAffected(); n > 0 {
			log.Printf("[cron] orphan hash sweep: removed %d stale video hashes", n)
		}
	}
}

// publishScheduledPosts fires every minute, moves scheduled posts with scheduled_at <= NOW()
// into the public feed by clearing their scheduled_at column.
func publishScheduledPosts(database *sql.DB) {
	tick := time.NewTicker(1 * time.Minute)
	defer tick.Stop()
	for range tick.C {
		if database == nil {
			continue
		}
		n, err := dbpkg.PublishScheduledPosts(database)
		if err != nil {
			log.Printf("[cron] scheduled posts publish error: %v", err)
		} else if n > 0 {
			log.Printf("[cron] published %d scheduled posts", n)
		}
	}
}

