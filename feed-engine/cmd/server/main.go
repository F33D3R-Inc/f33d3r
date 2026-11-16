package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/f33d3r/feed-engine/internal/herald"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/f33d3r/feed-engine/internal/abraxas"
	"github.com/f33d3r/feed-engine/internal/aethyr"
	"github.com/f33d3r/feed-engine/internal/config"
	"github.com/f33d3r/feed-engine/internal/cron"
	"github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/handler"
	"github.com/f33d3r/feed-engine/internal/jung/backfill"
	"github.com/f33d3r/feed-engine/internal/live"
	"github.com/f33d3r/feed-engine/internal/observ"
	"github.com/f33d3r/feed-engine/internal/realm"
	"github.com/f33d3r/feed-engine/internal/sitra"
)

func main() {
	cfg := config.Load()

	// ── Observability — must initialise BEFORE anything that logs ──────────
	logLevel := os.Getenv("LOG_LEVEL")
	if logLevel == "" {
		logLevel = "info"
	}
	devMode := strings.EqualFold(os.Getenv("DEV_MODE"), "true")
	observ.Init("nantar", logLevel, devMode)
	defer observ.Shutdown()

	startupLog := observ.L(context.Background())

	if cfg.InternalAPIKey == "" {
		fmt.Fprintln(os.Stderr, "FATAL: INTERNAL_API_KEY is not set. Set this env var to a strong random secret before starting the server.")
		os.Exit(1)
	}

	// Database — required. Every answer this server gives depends on it:
	// sessions, capabilities, the media gate and every publish check read it.
	// A process without it could only answer those questions without the
	// facts, so it does not serve at all. No listener is opened until the
	// database is connected and migrated.
	database, err := connectDatabase(cfg.DatabaseURL, startupLog)
	if err != nil {
		startupLog.Fatal("database unavailable; refusing to serve", zap.Error(err))
	}
	db.MigrateUp(database)
	startupLog.Info("database connected")

	// AethyrRank engine client
	engineClient := aethyr.NewClient(cfg.AethyrRankURL, cfg.AethyrRankTimeout)
	hctx, hcancel := context.WithTimeout(context.Background(), 3*time.Second)
	if engineClient.Health(hctx) {
		startupLog.Info("aethyrrank connected", zap.String("url", cfg.AethyrRankURL))
	} else {
		startupLog.Warn("aethyrrank unreachable; fallback ranker active",
			zap.String("url", cfg.AethyrRankURL))
	}
	hcancel()

	// Realm index — the ONE resolver behind every avatar ring.
	//
	// Started before the handler so the first page rendered is already correct.
	// It loads the accounts holding standing above the floor in a single query
	// and keeps them current from the XP write path plus a reconcile pass, so a
	// fifty-row feed draws fifty correct rings and issues no queries to do it.
	realmCtx, realmCancel := context.WithCancel(context.Background())
	defer realmCancel()
	realmIndex := realm.Start(realmCtx, database, 30*time.Second)
	startupLog.Info("realm index started", zap.Int("accounts_above_floor", realmIndex.Size()))

	// HTTP handler
	h := handler.New(cfg, engineClient, database)

	// Jung backfill — gives every live work still without a psych_vector its
	// place on the axes, 200 at a time with a rest between batches, and looks
	// again every hour. Stops with the server. Migrations have already run
	// above, so the column and its partial index exist before the first pass.
	backfillCtx, backfillCancel := context.WithCancel(context.Background())
	defer backfillCancel()
	go backfill.Run(backfillCtx, database, startupLog)

	// Background cron jobs — session cleanup, health monitor, subscription expiry
	herald.Configure(cfg.HeraldURL, cfg.InternalAPIKey)
	cron.Start(database, cfg, &http.Client{Timeout: 15 * time.Second})

	// Live lane — media server control plane, stream reconciliation, safety sampler.
	// Binds its own internal address; no video ever passes through this process.
	liveCtx, liveCancel := context.WithCancel(context.Background())
	defer liveCancel()
	liveManager := live.NewManager(database, cfg.InternalAPIKey)
	// The live lane decides on the server and must render on the server. Every
	// status change it takes on its own initiative — a lifecycle hook, a
	// reconcile pass, a moderation block — ends in Fragments pushed to the
	// surfaces showing that stream, including the broadcaster's own stage.
	liveManager.RenderStreamState = h.PublishStreamState
	// Every sampled frame is put to Abraxas, the integrity authority.
	liveManager.Abraxas = abraxas.New(cfg.AbraxasURL, cfg.InternalAPIKey)
	h.BlockLiveStream = liveManager.BlockStream
	// The live edge's auth_request: the watch surface's rules, applied to every
	// playlist, segment and poster before the edge serves it.
	liveManager.AuthorizeMedia = h.AuthorizeLiveMedia
	if err := liveManager.Start(liveCtx); err != nil {
		startupLog.Fatal("live lane failed to start", zap.Error(err))
	}

	// FA Live relay — carries live updates between replicas, because a person's
	// connection sits on only one of them.
	if cfg.KafkaBrokers != "" {
		relayCtx, relayCancel := context.WithCancel(context.Background())
		defer relayCancel()
		stopRelay, rerr := handler.StartLiveRelay(relayCtx, cfg.KafkaBrokers)
		if rerr != nil {
			startupLog.Fatal("live relay failed to start", zap.Error(rerr))
		}
		defer stopRelay()

		// Notifications: this brain's outbox to Herald, and Herald's counts back.
		stopNotifRelay, nerr := sitra.StartNotificationRelay(relayCtx, database, cfg.KafkaBrokers)
		if nerr != nil {
			startupLog.Fatal("notification relay failed to start", zap.Error(nerr))
		}
		defer stopNotifRelay()
		stopNotifUpdates, uerr := h.StartNotificationUpdates(relayCtx, cfg.KafkaBrokers)
		if uerr != nil {
			startupLog.Fatal("notification updates failed to start", zap.Error(uerr))
		}
		defer stopNotifUpdates()
		h.StartFanoutWorker(relayCtx)
		h.StartVideoJobWorker(relayCtx)
	} else {
		startupLog.Warn("KAFKA_BROKERS is empty: live updates reach this replica's connections only")
	}

	// Frequencies — the Sitra Achra consumer that marks rooms changed so their
	// Facets are re-rendered. Replicas share one group, so one replica owns each
	// room's events, and FA Live relays its output to the others' connections.
	if cfg.KafkaBrokers != "" {
		freqCtx, freqCancel := context.WithCancel(context.Background())
		defer freqCancel()
		stopFreq, ferr := sitra.StartFrequencyConsumer(freqCtx, cfg.KafkaBrokers, func(_ context.Context, ev sitra.FrequencyEvent) {
			h.FrequencyEvent(ev)
		})
		if ferr != nil {
			startupLog.Fatal("frequency consumer failed to start", zap.Error(ferr))
		}
		defer stopFreq()
		startupLog.Info("frequency consumer started", zap.String("brokers", cfg.KafkaBrokers))
	} else {
		startupLog.Warn("KAFKA_BROKERS is empty: frequency mutations will not reach other participants over FA Live")
	}

	// Identity. elohim-veni is the identity authority; this brain keeps a
	// projection of it and an outbox of identity writes committed with local
	// changes. The relay runs with or without a broker. The projection needs
	// one: without identity.events, nothing tells this brain about a role,
	// standing or handle change it did not make itself, so its absence is
	// fatal rather than a quietly stale projection.
	identityCtx, identityCancel := context.WithCancel(context.Background())
	defer identityCancel()
	h.StartIdentityRelay(identityCtx)
	if cfg.KafkaBrokers == "" {
		startupLog.Fatal("KAFKA_BROKERS is empty: the identity projection cannot follow identity.events")
	}
	stopIdentity, ierr := sitra.StartIdentityConsumer(identityCtx, cfg.KafkaBrokers, sitra.IdentityHandlers{
		Apply:      h.ApplyIdentityEvent,
		DeadLetter: h.IdentityDeadLetter,
	})
	if ierr != nil {
		startupLog.Fatal("identity consumer failed to start", zap.Error(ierr))
	}
	defer stopIdentity()
	startupLog.Info("identity projection consumer started", zap.String("brokers", cfg.KafkaBrokers))

	// Safety. Abraxas is the integrity authority; its verdicts, ownership
	// findings, banned matches and review decisions arrive on safety.events and
	// are the only way a verdict reaches this brain's content. Without the
	// broker a pending work would never get its verdict, so this is fatal too
	// (KAFKA_BROKERS was required just above).
	safetyCtx, safetyCancel := context.WithCancel(context.Background())
	defer safetyCancel()
	stopSafety, serr := sitra.StartSafetyConsumer(safetyCtx, cfg.KafkaBrokers, sitra.SafetyHandlers{
		Apply:      h.ApplySafetyEvent,
		DeadLetter: h.SafetyDeadLetter,
	})
	if serr != nil {
		startupLog.Fatal("safety consumer failed to start", zap.Error(serr))
	}
	defer stopSafety()
	startupLog.Info("safety consumer started", zap.String("brokers", cfg.KafkaBrokers))

	// Work obligations: what every work is owed, committed with it. Local
	// actions run with or without a broker; the moderation scan waits for one.
	workCtx, workCancel := context.WithCancel(context.Background())
	defer workCancel()
	stopWorkOutbox, werr := h.StartWorkOutbox(workCtx, cfg.KafkaBrokers)
	if werr != nil {
		startupLog.Fatal("work outbox failed to start", zap.Error(werr))
	}
	defer stopWorkOutbox()
	stopRankingRelay, rerr := aethyr.StartSignalRelay(workCtx, database, cfg.KafkaBrokers)
	if rerr != nil {
		startupLog.Fatal("ranking signal relay failed to start", zap.Error(rerr))
	}
	defer stopRankingRelay()
	h.StartWorkNoticeWorker(workCtx)

	// Commerce. Themis is the commerce authority and the only writer of plans
	// and subscriptions; this brain keeps their read projection, which the feed,
	// fanout and gating queries join on, from commerce.events. Without the
	// broker the projection would silently go stale, so this is fatal too
	// (KAFKA_BROKERS was required above).
	commerceCtx, commerceCancel := context.WithCancel(context.Background())
	defer commerceCancel()
	stopCommerce, cerr := sitra.StartCommerceConsumer(commerceCtx, cfg.KafkaBrokers, sitra.CommerceHandlers{
		Apply:      h.ApplyCommerceEvent,
		DeadLetter: h.CommerceDeadLetter,
	})
	if cerr != nil {
		startupLog.Fatal("commerce consumer failed to start", zap.Error(cerr))
	}
	defer stopCommerce()
	startupLog.Info("commerce projection consumer started", zap.String("brokers", cfg.KafkaBrokers))

	// Shadow balances (wallets keyed on an account id instead of a PIAL) are
	// returned to their owners through Themis when AET_RECOVERY=on.
	go h.RecoverShadowBalances(context.Background())

	// Middleware chain (outer → inner):
	//   request-id + logging + metrics → routes (which apply Recover, security
	//   headers, request log, compression, IP block, CSRF — see Handler.Routes)
	srv := &http.Server{
		Addr:         cfg.Host + ":" + cfg.Port,
		Handler:      observ.Middleware(h.Routes()),
		ReadTimeout:  0,
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		startupLog.Info("nantar listening",
			zap.String("addr", srv.Addr),
			zap.String("dev_mode", os.Getenv("DEV_MODE")))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			startupLog.Fatal("server crashed", zap.Error(err))
		}
	}()

	<-quit
	startupLog.Info("shutdown signal received")
	c2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel2()
	if err := srv.Shutdown(c2); err != nil {
		startupLog.Warn("graceful shutdown error", zap.Error(err))
	}
	startupLog.Info("server stopped")
}

// How long startup waits for the database before giving up. The window covers
// the gaps compose's depends_on does not: Postgres reports ready over its unix
// socket during first-boot initialisation, before it accepts TCP or has created
// f33d3r_feed, and a database restarting under a running stack is briefly gone.
// It is shorter than the feed-engine healthcheck's start_period plus its
// retries (15s + 3 × 10s), so a database that never arrives ends in a clean
// non-zero exit for the restart policy to act on, not a hung unhealthy process.
const (
	dbConnectBudget    = 30 * time.Second
	dbConnectFirstWait = 500 * time.Millisecond
	dbConnectMaxWait   = 5 * time.Second
)

// connectDatabase opens the database, retrying with capped exponential backoff
// for dbConnectBudget. It returns an error only when the database stayed
// unreachable for the whole budget, or when there is no DATABASE_URL to try.
func connectDatabase(url string, log *zap.Logger) (*sql.DB, error) {
	if url == "" {
		return nil, errors.New("DATABASE_URL is not set")
	}
	deadline := time.Now().Add(dbConnectBudget)
	wait := dbConnectFirstWait
	for attempt := 1; ; attempt++ {
		d, err := db.Open(url)
		if err == nil {
			return d, nil
		}
		if time.Now().Add(wait).After(deadline) {
			return nil, fmt.Errorf("no connection after %d attempts in %s: %w", attempt, dbConnectBudget, err)
		}
		log.Warn("database not reachable yet; retrying",
			zap.Int("attempt", attempt), zap.Duration("retry_in", wait), zap.Error(err))
		time.Sleep(wait)
		wait = min(wait*2, dbConnectMaxWait)
	}
}
