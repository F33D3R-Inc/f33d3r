// Package observ — F33D3R observability primitives for Go brains.
//
// Provides:
//   - Structured JSON logging via zap, Loki-friendly
//   - Request-ID context propagation
//   - PIAL-aware log enrichment
//   - Prometheus metrics + /metrics handler
//   - HTTP client wrapper that propagates correlation headers outbound
//
// Used by Nantar today. The Rust counterpart pattern is documented in
// infra/observability/rust-pattern.md and applied to all 13 Rust brains.
//
// Design rules (Sprint 0 / S0.2):
//   - Zero allocations on the hot path (use sugared logger sparingly).
//   - Cardinality bounded — paths are normalised before becoming Prom labels.
//   - The brain name is a constant per binary, set in Init().
//   - Request IDs propagate to: logs, response headers, downstream HTTP, Kafka headers.
package observ

import (
	"context"
	"os"
	"sync/atomic"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// ─── Globals ────────────────────────────────────────────────────────────────

// brainName is the canonical service identifier surfaced in every log line and
// metric label. Set once in Init.
var brainName atomic.Value // string

// rootLogger is the JSON-encoding zap logger used as the base for all derived
// loggers. Replaced by Init.
var rootLogger atomic.Pointer[zap.Logger]

// ─── Init / Shutdown ────────────────────────────────────────────────────────

// Init configures the global logger and metric registries. Call once at the
// top of main(), before any goroutines.
//
// brain  — canonical brain name ("nantar", "verity", etc).
// level  — zap level string ("debug", "info", "warn", "error"). Falls back to "info".
// devMode — if true, uses console encoder + colour. Production uses JSON.
func Init(brain string, level string, devMode bool) {
	brainName.Store(brain)

	lvl := zapcore.InfoLevel
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = zapcore.InfoLevel
	}

	var enc zapcore.Encoder
	encCfg := zapcore.EncoderConfig{
		TimeKey:        "time",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		MessageKey:     "msg",
		StacktraceKey:  "stacktrace",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.LowercaseLevelEncoder,
		EncodeTime:     zapcore.RFC3339NanoTimeEncoder,
		EncodeDuration: zapcore.MillisDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}
	if devMode {
		encCfg.EncodeLevel = zapcore.CapitalColorLevelEncoder
		enc = zapcore.NewConsoleEncoder(encCfg)
	} else {
		enc = zapcore.NewJSONEncoder(encCfg)
	}

	core := zapcore.NewCore(enc, zapcore.AddSync(os.Stdout), lvl)
	logger := zap.New(core, zap.AddCaller(), zap.AddStacktrace(zapcore.ErrorLevel)).
		With(zap.String("brain", brain))

	rootLogger.Store(logger)

	// Initialise metrics (idempotent; safe to call once).
	initMetrics(brain)

	logger.Info("observ initialised", zap.String("level", lvl.String()), zap.Bool("dev", devMode))
}

// Shutdown flushes any buffered log entries. Defer it from main().
func Shutdown() {
	if l := rootLogger.Load(); l != nil {
		_ = l.Sync()
	}
}

// Brain returns the brain name configured at Init.
func Brain() string {
	if v := brainName.Load(); v != nil {
		return v.(string)
	}
	return "unknown"
}

// ─── Logger access ──────────────────────────────────────────────────────────

type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxPIALID
	ctxLogger
)

// L returns the request-scoped logger from ctx if one was set by middleware,
// otherwise the root logger. Always non-nil.
func L(ctx context.Context) *zap.Logger {
	if ctx != nil {
		if l, ok := ctx.Value(ctxLogger).(*zap.Logger); ok && l != nil {
			return l
		}
	}
	if rl := rootLogger.Load(); rl != nil {
		return rl
	}
	// Fallback: nop logger so callers never crash before Init().
	return zap.NewNop()
}

// WithRequestID returns a context carrying the request id, used by
// outbound HTTP calls and Kafka publishes. Set automatically by middleware.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxRequestID, id)
}

// RequestID returns the request id from ctx, or "" if not set.
func RequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(ctxRequestID).(string)
	return v
}

// WithPIAL returns a context carrying the user's PIAL id (when known).
func WithPIAL(ctx context.Context, pial string) context.Context {
	return context.WithValue(ctx, ctxPIALID, pial)
}

// PIAL returns the PIAL id from ctx, or "" if not set.
func PIAL(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(ctxPIALID).(string)
	return v
}

// withLogger attaches a derived logger to ctx (used internally by middleware).
func withLogger(ctx context.Context, l *zap.Logger) context.Context {
	return context.WithValue(ctx, ctxLogger, l)
}
