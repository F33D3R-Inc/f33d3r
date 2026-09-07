package live

// manager.go — the live lane's server-side owner.
//
// The application server is authoritative for stream status, viewer count and
// moderation. The media system is authoritative for pixels. This file is the
// seam: it listens for the media server's lifecycle hooks, reconciles what the
// database believes against what the media server is actually carrying, and
// runs the content-safety sampler.
//
// It never touches a video frame in transit.

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/sitra"
)

// reconcileInterval is how often the database's idea of "live" is checked
// against the media server's.
const reconcileInterval = 30 * time.Second

// missesBeforeEnd is how many consecutive reconcile passes a stream must be
// absent from the media server before it is ended. Two passes covers a media
// server restart or a momentary API timeout without dropping a live broadcast.
const missesBeforeEnd = 2

// idleStreamTTL is how long an opened-but-never-published stream holds an
// author's one open slot before the server closes it.
const idleStreamTTL = 30 * time.Minute

// Manager owns the live lane's background work and its control plane.
type Manager struct {
	db          *sql.DB
	media       *MediaServer
	events      *sitra.Producer
	internalKey string

	// capacity is the governor: what the box can carry, what is on it now, and
	// the only thing in this process that says yes to a publish. It exists
	// because nothing did — every broadcast used to size its own ladder from
	// its own point of view, and past three of them they destroyed each other.
	capacity *Governor

	// RenderStreamState is how a decision taken here reaches the surfaces it
	// changes. Every path in this package that moves a stream's status does so
	// on the server's own initiative — a lifecycle hook from the media server,
	// a reconcile pass, a moderation block — with no browser request to answer,
	// so a rendered Fragment is the only way any open surface can learn of it.
	// A row quietly changing under a page that is still showing the old state is
	// not a state change the application made; it is one it hid.
	//
	// It is set once at wiring time by whatever owns rendering. When it is nil
	// this package still keeps the database honest, and says so at startup
	// rather than pretending the surfaces were told.
	RenderStreamState func(streamID string)

	mu     sync.Mutex
	misses map[string]int // stream id -> consecutive reconcile misses
}

// renderState pushes a stream's surfaces if a renderer was wired in.
func (m *Manager) renderState(streamID string) {
	if m.RenderStreamState == nil || streamID == "" {
		return
	}
	m.RenderStreamState(streamID)
}

// NewManager builds the live lane owner. internalKey is the shared secret the
// media server's lifecycle hooks present.
func NewManager(database *sql.DB, internalKey string) *Manager {
	return &Manager{
		db:          database,
		media:       NewMediaServer(),
		events:      sitra.New(),
		internalKey: internalKey,
		capacity:    NewGovernor(),
		misses:      make(map[string]int),
	}
}

// Start brings up the control plane listener, the reconcile loop and the
// content-safety sampler. It returns once the listener is bound; the loops run
// until ctx is cancelled.
//
// The control plane binds its own address, separate from the public router, so
// the media server's callbacks never traverse the session, rate-limit or
// rendering middleware that the viewer-facing surface needs.
func (m *Manager) Start(ctx context.Context) error {
	if m.db == nil {
		log.Printf("[live] no database — live lane disabled")
		return nil
	}
	if m.internalKey == "" {
		log.Printf("[live] INTERNAL_API_KEY is empty — live lane disabled rather than " +
			"exposing unauthenticated lifecycle hooks")
		return nil
	}
	if m.RenderStreamState == nil {
		log.Printf("[live] no renderer wired — status changes taken here will not " +
			"reach any open surface until it is reloaded")
	}

	addr := envOr("LIVE_CONTROL_ADDR", ":8110")
	srv := &http.Server{
		Addr:              addr,
		Handler:           m.controlRoutes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("[live] control plane listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[live] control plane stopped: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("[live] control plane shutdown: %v", err)
		}
	}()

	go m.reconcileLoop(ctx)
	go m.capacityExportLoop(ctx)
	go newScanner(m).run(ctx)
	return nil
}

// reconcileLoop keeps the database honest about what is actually broadcasting.
// A crashed encoder, a killed container or a lost network leaves a row saying
// "live" that nothing will ever close; this closes it.
func (m *Manager) reconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.reconcile(ctx); err != nil {
				log.Printf("[live] reconcile: %v", err)
			}
		}
	}
}

func (m *Manager) reconcile(ctx context.Context) error {
	liveIDs, err := dbpkg.ListLiveStreamIDs(m.db)
	if err != nil {
		return err
	}
	if len(liveIDs) > 0 {
		publishing, err := m.media.PublishingIDs(ctx)
		if err != nil {
			// The media server is unreachable. Every stream would look dead,
			// so end none of them and say why.
			return err
		}
		var orphaned []string
		m.mu.Lock()
		for _, id := range liveIDs {
			if publishing[id] {
				delete(m.misses, id)
				continue
			}
			m.misses[id]++
			if m.misses[id] >= missesBeforeEnd {
				delete(m.misses, id)
				orphaned = append(orphaned, id)
			}
		}
		m.mu.Unlock()

		for _, id := range orphaned {
			if err := dbpkg.EndStream(m.db, id); err != nil {
				log.Printf("[live] ending orphaned stream %s: %v", id, err)
				continue
			}
			m.capacity.Release(id)
			log.Printf("[live] stream %s ended: no publisher at the media server", id)
			m.publishLifecycle("live.ended", id, "reconcile")
			m.renderState(id)
		}
	}

	stale, err := dbpkg.StaleIdleStreams(m.db, idleStreamTTL)
	if err != nil {
		return err
	}
	for _, id := range stale {
		if err := dbpkg.EndStream(m.db, id); err != nil {
			log.Printf("[live] closing stale idle stream %s: %v", id, err)
			continue
		}
		// An idle stream that timed out may still be holding a lease taken at
		// the door by a publish that authorised and then never delivered a
		// frame. Capacity that is never released is capacity the next
		// broadcaster is refused for no reason at all.
		m.capacity.Release(id)
		m.renderState(id)
	}
	return nil
}

// BlockStream is the enforcement end of a moderation decision: the state moves
// to blocked and the publisher is dropped at the media server. Hiding a Facet
// while the ingest keeps running is not a block.
func (m *Manager) BlockStream(ctx context.Context, streamID, reason string) error {
	if err := dbpkg.SetStreamScanState(m.db, streamID, "blocked", reason, "live-scan"); err != nil {
		return err
	}
	if err := m.media.KickPublisher(ctx, streamID); err != nil {
		return err
	}
	if err := dbpkg.EndStream(m.db, streamID); err != nil {
		return err
	}
	m.capacity.Release(streamID)
	log.Printf("[live] stream %s blocked and dropped: %s", streamID, reason)
	m.publishLifecycle("live.blocked", streamID, reason)
	m.renderState(streamID)
	return nil
}

// ── Sitra Achra ──────────────────────────────────────────────────────────────

// publishLifecycle puts a stream state change on the content bus.
func (m *Manager) publishLifecycle(event, streamID, detail string) {
	if m.events == nil {
		return
	}
	payload, err := json.Marshal(map[string]interface{}{
		"event":      event,
		"stream_id":  streamID,
		"detail":     detail,
		"created_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		log.Printf("[live] marshal lifecycle event: %v", err)
		return
	}
	m.events.Publish(context.Background(), sitra.TopicContent, []byte(streamID), payload)
}

// publishScanSample puts one sampled keyframe's verdict on the content bus so
// the safety fabric sees live frames on the same topic as uploaded work.
func (m *Manager) publishScanSample(streamID, hash, frameName, verdict string) {
	if m.events == nil {
		return
	}
	payload, err := json.Marshal(map[string]interface{}{
		"event":      "live.frame.sampled",
		"stream_id":  streamID,
		"frame":      frameName,
		"sha256":     hash,
		"scan_state": verdict,
		"created_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		log.Printf("[live] marshal scan sample event: %v", err)
		return
	}
	m.events.Publish(context.Background(), sitra.TopicContent, []byte(streamID), payload)
}
