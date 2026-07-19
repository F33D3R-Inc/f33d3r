package middleware

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// tokenBucket is a per-key token bucket for rate limiting.
type tokenBucket struct {
	tokens   float64
	lastFill time.Time
	mu       sync.Mutex
}

// RateLimiter implements a per-IP token bucket limiter.
// Not suitable for multi-instance deployments (use Redis there).
// Burst = capacity / 4 so short spikes are allowed without rewarding sustained floods.
type RateLimiter struct {
	buckets  sync.Map
	rate     float64 // tokens added per second
	capacity float64 // max tokens
}

// NewRateLimiter creates a limiter allowing reqPerMin requests per minute per key,
// with a burst allowance of reqPerMin/4.
func NewRateLimiter(reqPerMin int) *RateLimiter {
	r := float64(reqPerMin) / 60.0
	return &RateLimiter{rate: r, capacity: float64(reqPerMin) / 4}
}

func (rl *RateLimiter) allow(key string) bool {
	now := time.Now()
	v, _ := rl.buckets.LoadOrStore(key, &tokenBucket{
		tokens:   rl.capacity,
		lastFill: now,
	})
	b := v.(*tokenBucket)
	b.mu.Lock()
	defer b.mu.Unlock()

	elapsed := now.Sub(b.lastFill).Seconds()
	b.tokens = min64(rl.capacity, b.tokens+elapsed*rl.rate)
	b.lastFill = now
	if b.tokens >= 1.0 {
		b.tokens--
		return true
	}
	return false
}

func (rl *RateLimiter) clientIP(r *http.Request) string {
	// X-Real-IP is set by Caddy to the true client IP and cannot be spoofed.
	// X-Forwarded-For is client-controlled (clients prepend arbitrary IPs) so
	// we never use it for security decisions.
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Limit wraps a handler with per-IP rate limiting.
func (rl *RateLimiter) Limit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := rl.clientIP(r)
		if !rl.allow(ip) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "Too many requests — slow down", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

// LimitHandler wraps an http.Handler with per-IP rate limiting.
func (rl *RateLimiter) LimitHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := rl.clientIP(r)
		if !rl.allow(ip) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "Too many requests — slow down", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func min64(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
