package cron

import (
	"context"
	"database/sql"
	"encoding/json"
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
	go subscriptionExpiry(cfg, client)
	log.Println("[cron] background jobs started: session-cleanup, health-monitor, subscription-expiry")
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
		{"aethyr-msg", cfg.VovinURL + "/health"},
		{"zodacare", cfg.ZodacareURL + "/health"},
		{"elohim-veni", cfg.ElohimVeniURL + "/health"},
		{"verity", cfg.VerityURL + "/health"},
		{"thessalon", cfg.ThessalonURL + "/health"},
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

// subscriptionExpiry tells Thessalon to expire overdue subscriptions every hour.
func subscriptionExpiry(cfg *config.Config, client *http.Client) {
	if cfg.ThessalonURL == "" {
		return
	}
	tick := time.NewTicker(1 * time.Hour)
	defer tick.Stop()
	for range tick.C {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			cfg.ThessalonURL+"/v1/subscriptions/expire", nil)
		if err != nil {
			cancel()
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		cancel()
		if err != nil {
			log.Printf("[cron] subscription expiry call failed: %v", err)
			continue
		}
		var result struct {
			Expired int `json:"expired"`
		}
		if json.NewDecoder(resp.Body).Decode(&result) == nil && result.Expired > 0 {
			log.Printf("[cron] subscription expiry: %d subscriptions expired", result.Expired)
		}
		resp.Body.Close()
	}
}
