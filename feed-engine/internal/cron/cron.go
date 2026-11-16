package cron

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/f33d3r/feed-engine/internal/caeor"
	"github.com/f33d3r/feed-engine/internal/config"
	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/manhattan"
)

// Start launches all background cron goroutines. Call once from main after DB is ready.
func Start(database *sql.DB, cfg *config.Config, client *http.Client) {
	go notificationMonitor(database)
	go healthMonitor(cfg, client)
	go pendingScanSweep(database, cfg, client)
	// Scheduled works are released by the work outbox worker
	// (handler.StartWorkOutbox), in the same transaction that records what each
	// one is owed on publication.
	// Messages written to somebody who never answered. They are held so the
	// sender does not have to type them again while the knock is outstanding;
	// they are not held for ever.
	go gnosisPendingSweep(database)
	// The Vision lane's object collector. Expiry itself is enforced in SQL on every
	// read, so this only reclaims bytes and is allowed to lag.
	go visionMediaSweep(database, cfg)
	// The Manhattan outbox drain. Registrations are queued transactionally by
	// database triggers; this is what delivers them to the naming plane.
	go manhattan.StartDrain(context.Background(), database, manhattan.New(cfg.ManhattanURL))
	log.Println("[cron] background jobs started: health-monitor, stale-scan-alert, gnosis-pending-sweep, vision-media-purge, manhattan-outbox-drain")
}

// healthMonitor pings critical services every 5 minutes and logs if any are down.
func healthMonitor(cfg *config.Config, client *http.Client) {
	endpoints := []struct{ name, url string }{
		{"aethyrrank", cfg.AethyrRankURL + "/health"},
		{"ain-soph", cfg.AinSophURL + "/health"},
		{"abraxas", cfg.AbraxasURL + "/health"},
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

// staleScanAge is how long a work may sit without a verdict before this sweep
// calls it out. Abraxas Shield answers asynchronously over Sitra Achra, so the
// window has to be generous enough that a healthy scan is never reported as a
// stuck one.
const staleScanAge = 15 * time.Minute

// staleScanSampleMax bounds how many identifiers one alert names. The count is
// the signal; the sample is there to start an investigation.
const staleScanSampleMax = 10

// pendingScanSweep reports works that have been sitting without a scan verdict
// for longer than staleScanAge. Runs every 5 minutes, and once on startup.
//
// This sweep deliberately writes NOTHING. It used to stamp 'clean' on anything
// still pending after three minutes, which asserted a verdict no scanner ever
// produced — and, now that Abraxas Shield actually runs, would race Shield and
// overwrite real verdicts, including 'blocked', with a fabricated pass. A scan
// that timed out is not a clean scan. It also used to auto-approve human_review
// works after fifteen minutes, which is the same fabrication one hop later: a
// timer is not a human reviewer. Releasing a human_review work stays an operator
// action (admin's rescue sweep), where a person is accountable for it.
//
// Moving the row to another state is not an option either: Shield selects work
// to scan by scan_state, so re-stating a row here could deny it the real verdict
// that is still coming. The honest action is to leave the row exactly as the
// scanner left it and raise a loud operational alert.
//
// A work waiting on a verdict is visible to its author only and is neither
// fanned out nor ranked (db.workNotBlockedSQL, ClaimWorkOutbox), so a stale
// verdict is a work its author can see and nobody else can: this alert is how
// an operator learns Abraxas is not answering.
func pendingScanSweep(database *sql.DB, cfg *config.Config, client *http.Client) {
	sweepPendingScanOnce(database, cfg, client)
	tick := time.NewTicker(5 * time.Minute)
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

	rows, err := database.Query(`
		SELECT p.id::text, p.created_at
		FROM works p
		WHERE p.scan_state = 'pending'
		  AND p.deleted_at IS NULL
		  AND p.created_at < NOW() - make_interval(secs => $1)
		ORDER BY p.created_at ASC
		LIMIT 200
	`, staleScanAge.Seconds())
	if err != nil {
		log.Printf("[cron][scan-sweep] stale scan query error: %v", err)
		return
	}
	defer rows.Close()

	var sample []string
	var oldest time.Time
	total := 0
	for rows.Next() {
		var id string
		var createdAt time.Time
		if err := rows.Scan(&id, &createdAt); err != nil {
			// A row that will not scan is a broken read, not an empty result.
			log.Printf("[cron][scan-sweep] stale scan row scan error: %v", err)
			return
		}
		total++
		if oldest.IsZero() || createdAt.Before(oldest) {
			oldest = createdAt
		}
		if len(sample) < staleScanSampleMax {
			sample = append(sample, id)
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("[cron][scan-sweep] stale scan read error: %v", err)
		return
	}
	if total == 0 {
		return
	}

	log.Printf("[cron][scan-sweep] ALERT: %d work(s) have no scan verdict after %s — "+
		"oldest waiting %s. Abraxas Shield is not answering for these. "+
		"scan_state left untouched; no verdict is being fabricated. sample=%v",
		total, staleScanAge, time.Since(oldest).Truncate(time.Second), sample)
}

// visionMediaSweep deletes the stored objects behind expired Visions and stamps
// the vision_media row that owns each one. Runs every 10 minutes.
//
// This worker is allowed to lag. What makes a Vision unreadable is the read path
// — every read in db/vision.go carries visionLiveSQL, so an expired Vision is gone
// from every surface the moment it expires, whether or not its bytes have been
// collected yet. A row is stamped purged only when its object was actually
// deleted, so a failed delete is retried on the next pass instead of leaking the
// object forever.
func visionMediaSweep(database *sql.DB, cfg *config.Config) {
	tick := time.NewTicker(10 * time.Minute)
	defer tick.Stop()
	for range tick.C {
		sweepVisionMediaOnce(database, cfg)
	}
}

func sweepVisionMediaOnce(database *sql.DB, cfg *config.Config) {
	if database == nil {
		return
	}
	refs, err := dbpkg.ExpiredVisionMedia(database, 200)
	if err != nil {
		log.Printf("[cron][vision-purge] query error: %v", err)
		return
	}
	if len(refs) == 0 {
		return
	}

	cc := caeor.New(cfg.CaeorURL, cfg.InternalAPIKey)
	purged := make([]string, 0, len(refs))
	failed := 0
	for _, m := range refs {
		if err := deleteVisionObject(database, cc, m); err != nil {
			failed++
			log.Printf("[cron][vision-purge] vision %s object %s: %v", dbpkg.VisionRef{AuthorPIAL: m.AuthorPIAL, Seq: m.Seq}, m.ObjectKey, err)
			continue
		}
		purged = append(purged, m.ID)
	}

	if len(purged) > 0 {
		if err := dbpkg.MarkVisionMediaPurged(database, purged); err != nil {
			// The objects are gone but the rows still say otherwise. Reported, and
			// the next pass will try to delete objects that no longer exist — which
			// is treated as already-purged, so the rows do settle.
			log.Printf("[cron][vision-purge] stamping %d purged rows: %v", len(purged), err)
			return
		}
	}
	log.Printf("[cron][vision-purge] purged %d expired vision objects, %d failed", len(purged), failed)
}

// visionLocalMediaPrefix is the path a Vision object stored on this platform's
// own disk carried, before Caeor was the one media authority. Nothing writes
// there any more; expired legacy objects are still reclaimed.
const visionLocalMediaPrefix = "/static/uploads/visions/"

// visionLocalMediaDir is where those legacy objects live.
var visionLocalMediaDir = filepath.Join("web", "static", "uploads", "visions")

// visionPurgeTimeout bounds the Caeor calls for one expired Vision object.
const visionPurgeTimeout = 15 * time.Second

// deleteVisionObject reclaims one expired Vision object. An object that is
// already gone is a success — the point of the sweep is that it is not there.
//
// A Caeor asset is withdrawn only when it is the Vision author's own upload and
// nothing else of theirs still carries it: a Vision that showed a photo the
// author also posted as a work leaves the work's media alone.
func deleteVisionObject(database *sql.DB, cc *caeor.Client, m dbpkg.VisionMediaRef) error {
	key := strings.TrimSpace(m.ObjectKey)
	if key == "" {
		key = strings.TrimSpace(m.AssetURL)
	}
	if key == "" && m.CaeorAssetID == "" {
		return errors.New("vision media row names no object")
	}

	if strings.HasPrefix(key, visionLocalMediaPrefix) && m.CaeorAssetID == "" {
		name := strings.TrimPrefix(key, visionLocalMediaPrefix)
		// The name is one path segment written by this process. Anything else is
		// refused rather than handed to os.Remove.
		if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
			return fmt.Errorf("refusing to purge malformed object key %q", key)
		}
		if err := os.Remove(filepath.Join(visionLocalMediaDir, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	if !cc.Configured() {
		return errors.New("caeor is not configured, cannot withdraw the object")
	}
	ctx, cancel := context.WithTimeout(context.Background(), visionPurgeTimeout)
	defer cancel()

	assetID := strings.TrimSpace(m.CaeorAssetID)
	if assetID == "" {
		obj, err := cc.Resolve(ctx, key)
		if errors.Is(err, caeor.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		assetID = obj.AssetID
	}
	asset, err := cc.Get(ctx, assetID)
	if errors.Is(err, caeor.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if asset.Withdrawn || asset.UploaderPIAL != m.AuthorPIAL {
		// Already off the platform, or somebody else's upload the Vision only
		// referenced: nothing of this Vision's to withdraw.
		return nil
	}
	carried, err := dbpkg.AssetStillCarried(database, assetID, m.AuthorPIAL, m.ID)
	if err != nil {
		return err
	}
	if carried {
		return nil
	}
	if err := cc.Withdraw(ctx, assetID, "vision expired"); err != nil && !errors.Is(err, caeor.ErrNotFound) {
		return err
	}
	return nil
}

// gnosisPendingMaxAge is how long a message written to somebody who has not
// agreed to receive it is held.
//
// It exists because the alternative is a table that only grows: a message to a
// person who never answers a knock has nothing to become. A week is long enough
// that somebody who accepts over a weekend still gets what was written, and
// short enough that an unanswered attempt does not sit in the database for ever.
//
// Nothing is lost that the sender was not already told was undelivered: the gate
// under their own message says so in as many words, every time they open it.
const gnosisPendingMaxAge = 7 * 24 * time.Hour

// gnosisPendingSweep releases held messages that have aged out.
//
// It deletes only by age. A declined knock is caught by the same rule rather
// than by a second one: the decision belongs to the handler that owns it, and a
// sweep that tried to read decisions would be a second place deciding what a
// contact request means.
//
// Age is measured from the last time the sender touched the message, not from
// the first attempt. Rewriting a held sentence is the sender saying it again,
// and updated_at is what the upsert moves when they do; ageing by created_at
// would delete a message written minutes ago because an earlier draft of it was
// a week old.
func gnosisPendingSweep(database *sql.DB) {
	tick := time.NewTicker(1 * time.Hour)
	defer tick.Stop()
	for range tick.C {
		if database == nil {
			continue
		}
		result, err := database.Exec(`
			DELETE FROM gnosis_pending_messages
			WHERE GREATEST(created_at, updated_at) < NOW() - $1::interval`,
			fmt.Sprintf("%d seconds", int(gnosisPendingMaxAge.Seconds())))
		if err != nil {
			log.Printf("[cron] gnosis pending sweep error: %v", err)
			continue
		}
		if n, _ := result.RowsAffected(); n > 0 {
			log.Printf("[cron] gnosis pending sweep: released %d undelivered message(s) older than %s", n, gnosisPendingMaxAge)
		}
	}
}
