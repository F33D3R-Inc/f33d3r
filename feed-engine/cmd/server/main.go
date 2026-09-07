package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"

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

	// Database — optional in dev mode
	var database *sql.DB
	if cfg.DatabaseURL != "" {
		d, err := db.Open(cfg.DatabaseURL)
		if err != nil {
			startupLog.Warn("DB unavailable; running without persistence", zap.Error(err))
		} else {
			database = d
			db.MigrateUp(d)
			startupLog.Info("database connected")
		}
	} else {
		startupLog.Info("DATABASE_URL not set; running without persistence")
	}

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
	if err := liveManager.Start(liveCtx); err != nil {
		startupLog.Fatal("live lane failed to start", zap.Error(err))
	}

	// Frequencies — the Sitra Achra consumer that turns Auralis's events into
	// Facet mutations for everyone present. Each Nantar instance consumes on
	// its own group so every replica sees every event; the SSE sessions it
	// pushes to are its own.
	if cfg.KafkaBrokers != "" {
		freqCtx, freqCancel := context.WithCancel(context.Background())
		defer freqCancel()
		stopFreq, ferr := sitra.StartFrequencyConsumer(freqCtx, cfg.KafkaBrokers, func(ctx context.Context, ev sitra.FrequencyEvent) {
			h.PublishFrequencyState(ctx, ev.FrequencyID)
		})
		if ferr != nil {
			startupLog.Fatal("frequency consumer failed to start", zap.Error(ferr))
		}
		defer stopFreq()
		startupLog.Info("frequency consumer started", zap.String("brokers", cfg.KafkaBrokers))
	} else {
		startupLog.Warn("KAFKA_BROKERS is empty: frequency mutations will not reach other participants over FA Live")
	}

	// Recover any AET trapped in shadow accounts (account UUID vs PIAL UUID mismatch).
	// Runs once in the background — no-op if all balances are already correct.
	go handler.RecoverShadowBalances(context.Background(), database, cfg.AinSophURL, cfg.InternalAPIKey)

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
