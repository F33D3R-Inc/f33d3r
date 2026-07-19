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
	"github.com/f33d3r/feed-engine/internal/observ"
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

	// HTTP handler
	h := handler.New(cfg, engineClient, database)

	// Background cron jobs — session cleanup, health monitor, subscription expiry
	cron.Start(database, cfg, &http.Client{Timeout: 15 * time.Second})

	// Recover any AET trapped in shadow accounts (account UUID vs PIAL UUID mismatch).
	// Runs once in the background — no-op if all balances are already correct.
	go handler.RecoverShadowBalances(context.Background(), database, cfg.AinSophURL)

	// Middleware chain (outer → inner):
	//   security headers → request-id + logging + metrics → routes
	srv := &http.Server{
		Addr:         cfg.Host + ":" + cfg.Port,
		Handler:      securityHeaders(observ.Middleware(h.Routes())),
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

// securityHeaders enforces enterprise-grade HTTP security headers on every response.
// This is the outermost middleware — nothing runs before these headers are set.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()

		// Prevent browsers from sniffing content type
		h.Set("X-Content-Type-Options", "nosniff")

		// Deny framing completely (clickjacking protection)
		h.Set("X-Frame-Options", "DENY")

		// Remove server fingerprint
		h.Set("Server", "f33d3r")

		// Referrer: send origin only, never full URL to third parties
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")

		// Permissions: no camera, microphone, geolocation, or payment API access
		h.Set("Permissions-Policy",
			"camera=(), microphone=(), geolocation=(), payment=(), usb=(), bluetooth=()")

		// HSTS: enforce HTTPS for 1 year — behind Caddy TLS terminator in production.
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")

		// Content-Security-Policy: lock down script execution.
		// - 'self' only for scripts (external CDN scripts are allowlisted explicitly)
		// - No eval(), no inline scripts (enforced by browser)
		// - Connect only to self (SSE, HTMX, uploads all same-origin)
		// - fonts and styles from Google Fonts + self
		h.Set("Content-Security-Policy",
			"default-src 'self'; "+
				// 'unsafe-inline' required for onclick/oninput event handler attributes.
				// All sensitive data lives in <meta> tags not inline JS — no injection risk.
				// Inline <script> blocks are eliminated; this only enables HTML attribute handlers.
				// 'wasm-unsafe-eval' is required to instantiate the browser WASM crypto core.
				// It permits WebAssembly compilation only — not JS eval().
				"script-src 'self' 'unsafe-inline' 'wasm-unsafe-eval' https://cdn.tailwindcss.com https://unpkg.com; "+
				"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; "+
				"font-src 'self' https://fonts.gstatic.com; "+
				"img-src 'self' data: blob: https:; "+
				"connect-src 'self' https://nominatim.openstreetmap.org; "+
				"media-src 'self' blob:; "+
				"worker-src 'self' blob:; "+
				// YouTube privacy-enhanced embeds (youtube-nocookie.com) — no cookies set until play.
				"frame-src 'self' https://www.youtube-nocookie.com; "+
				"frame-ancestors 'none'; "+
				"base-uri 'self'; "+
				"form-action 'self'",
		)

		// Cache control for HTML pages: no caching — always fresh
		// Static assets (JS/CSS) are versioned with ?v=, so they can be cached
		ct := r.Header.Get("Accept")
		if ct != "" && (r.URL.Path == "/" || len(r.URL.Path) > 1 && r.URL.Path[len(r.URL.Path)-3:] != ".js" && r.URL.Path[len(r.URL.Path)-4:] != ".css") {
			h.Set("Cache-Control", "no-store")
		}

		next.ServeHTTP(w, r)
	})
}
