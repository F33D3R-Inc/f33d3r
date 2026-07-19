package middleware

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"
)

// statusWriter wraps ResponseWriter to capture the status code written by the handler.
// It also proxies http.Flusher so SSE and streaming handlers are not broken.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

// Write captures a 200 status if the handler calls Write without WriteHeader first.
func (sw *statusWriter) Write(b []byte) (int, error) {
	if sw.status == 0 {
		sw.status = http.StatusOK
	}
	return sw.ResponseWriter.Write(b)
}

// Flush proxies to the underlying ResponseWriter if it supports http.Flusher.
// Required for SSE — without this, the type assertion w.(http.Flusher) fails,
// the SSE connection drops, and the client reloads in an infinite loop.
func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// RequestLog emits one structured JSON log line per request containing the
// fields needed for post-incident forensics: timestamp, method, path, status,
// real client IP, user-agent, response time, and HTMX boost flag.
//
// Static assets (/static/, /media/) are skipped to keep logs signal-rich.
func RequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip static assets — they generate noise without security signal.
		if strings.HasPrefix(r.URL.Path, "/static/") || strings.HasPrefix(r.URL.Path, "/media/") {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		ms := time.Since(start).Milliseconds()

		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}

		// Real IP — same logic as requestIP() in security.go.
		ip := strings.TrimSpace(r.Header.Get("X-Real-IP"))
		if ip == "" {
			ip = r.RemoteAddr
		}

		entry := map[string]interface{}{
			"ts":     time.Now().UTC().Format(time.RFC3339),
			"method": r.Method,
			"path":   r.URL.Path,
			"status": status,
			"ip":     ip,
			"ua":     r.UserAgent(),
			"ms":     ms,
			"htmx":   r.Header.Get("HX-Request") == "true",
		}
		if q := r.URL.RawQuery; q != "" {
			entry["query"] = q
		}

		b, _ := json.Marshal(entry)
		log.Printf("[req] %s", b)
	})
}
