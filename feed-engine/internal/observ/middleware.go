package observ

import (
	"bufio"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	// HeaderRequestID is the canonical request-correlation header propagated
	// across all F33D3R brains. Generated at the edge (Nantar) if missing.
	HeaderRequestID = "X-Request-ID"

	// HeaderPialIdentity carries the authenticated user's PIAL UUID when
	// Nantar proxies a request to an internal brain. Treated as opaque by
	// downstream brains.
	HeaderPialIdentity = "X-Pial-Identity"
)

// Middleware composes the standard observability stack onto a handler.
// Order is intentional:
//
//  1. Request-ID injection (so every later log/metric has the id available)
//  2. Metrics (counts even errors, low overhead)
//  3. Logging (records the final outcome including duration + status)
//
// Use this from main():
//
//	srv.Handler = observ.Middleware(routes)
func Middleware(next http.Handler) http.Handler {
	return RequestIDMiddleware(MetricsMiddleware(LoggingMiddleware(next)))
}

// RequestIDMiddleware reads X-Request-ID, generates one if missing, exposes
// it on the response header, and stows it on the request context.
//
// Also derives a request-scoped logger that includes the id + path + method
// so every log line within the request is correlated.
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if id == "" {
			id = uuid.NewString()
			r.Header.Set(HeaderRequestID, id)
		}
		w.Header().Set(HeaderRequestID, id)

		ctx := WithRequestID(r.Context(), id)

		// PIAL injection for downstream — Nantar's auth.userFromRequest sets
		// the header; we just forward it on the context for non-handler code
		// (cron jobs etc).
		if pial := r.Header.Get(HeaderPialIdentity); pial != "" {
			ctx = WithPIAL(ctx, pial)
		}

		// Derived logger with correlation fields.
		baseFields := []zap.Field{
			zap.String("request_id", id),
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
		}
		if pial := PIAL(ctx); pial != "" {
			baseFields = append(baseFields, zap.String("pial_id", pial))
		}
		ctx = withLogger(ctx, L(ctx).With(baseFields...))

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// statusRecorder captures the response status without forcing buffering.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.status = http.StatusOK
		s.wrote = true
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Flush proxies http.Flusher when supported (SSE endpoints rely on this).
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack proxies http.Hijacker when supported (WebSocket proxies rely on this).
// Without this, a WebSocket proxy can't upgrade HTTP → WebSocket through the logging
// middleware, causing every push connection to fail with a silent 502.
func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := s.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// LoggingMiddleware emits one structured log line per request, AFTER the
// handler completes. Skips noisy paths (/static/*, /metrics, /api/health).
// The `logged` decision is based on path prefix to keep the hot path fast.
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		if isNoisyPath(r.URL.Path) {
			return
		}

		dur := time.Since(start)
		L(r.Context()).Info("http_request",
			zap.Int("status", rec.status),
			zap.Int("bytes", rec.bytes),
			zap.Duration("dur", dur),
			zap.String("htmx", r.Header.Get("HX-Request")),
			zap.String("user_agent", r.UserAgent()),
			zap.String("remote", r.RemoteAddr),
		)
	})
}

// MetricsMiddleware records request count + duration to Prometheus.
// Path is normalised to a low-cardinality template (see normalize.go).
func MetricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Increment in-flight before the handler, decrement after.
		httpInFlight.WithLabelValues(Brain()).Inc()
		defer httpInFlight.WithLabelValues(Brain()).Dec()

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		path := normalisePath(r.URL.Path)
		status := strconv.Itoa(rec.status)

		httpRequestsTotal.WithLabelValues(Brain(), r.Method, path, status).Inc()
		httpRequestDuration.WithLabelValues(Brain(), r.Method, path, status).Observe(time.Since(start).Seconds())
	})
}

// isNoisyPath returns true for paths we don't want to log every request for
// (static assets, health checks, metrics scrape).
func isNoisyPath(p string) bool {
	if p == "/api/health" || p == "/metrics" {
		return true
	}
	if len(p) >= 8 && p[:8] == "/static/" {
		return true
	}
	if len(p) >= 7 && p[:7] == "/cdn/" {
		return true
	}
	return false
}
