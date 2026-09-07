package middleware

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// tokenBucket is a per-key token bucket for rate limiting.
type tokenBucket struct {
	tokens   float64
	lastFill time.Time
	mu       sync.Mutex
}

// maxBuckets caps the live key set. IPv6 hands any single client an effectively
// unlimited supply of source addresses, so an uncapped key map is a memory
// exhaustion vector reachable by anyone.
const maxBuckets = 100000

// RateLimiter implements a per-IP token bucket limiter.
//
// Scope: this limiter counts within ONE process. It is correct for the current
// single-instance deployment and wrong the moment feed-engine runs more than one
// replica, where the effective limit multiplies by replica count. Horizontal
// scaling requires moving the buckets to the Redis the stack already runs
// (docker-compose: REDIS_URL reaches feed-engine) — a change that also adds the
// first Redis client dependency to this module, so it is deliberately not made
// here alongside unrelated authorization work.
//
// Burst = capacity / 4 so short spikes are allowed without rewarding sustained floods.
type RateLimiter struct {
	buckets  sync.Map
	rate     float64 // tokens added per second
	capacity float64 // max tokens
	live     atomic.Int64
	// idle is how long an untouched bucket takes to refill completely. Past that
	// point a stored bucket and a fresh one are indistinguishable, so evicting it
	// costs no enforcement.
	idle time.Duration
}

// NewRateLimiter creates a limiter allowing reqPerMin requests per minute per key,
// with a burst allowance of reqPerMin/4.
func NewRateLimiter(reqPerMin int) *RateLimiter {
	r := float64(reqPerMin) / 60.0
	capacity := float64(reqPerMin) / 4
	idle := time.Minute
	if r > 0 {
		idle = time.Duration(capacity/r*float64(time.Second)) + time.Second
	}
	return &RateLimiter{rate: r, capacity: capacity, idle: idle}
}

// evictRefilled drops buckets that have sat idle long enough to be full again.
// A concurrent holder of an evicted bucket loses its decrement, which can only
// happen to a bucket that was already full.
func (rl *RateLimiter) evictRefilled(now time.Time) {
	var live int64
	rl.buckets.Range(func(k, v any) bool {
		b := v.(*tokenBucket)
		b.mu.Lock()
		stale := now.Sub(b.lastFill) > rl.idle
		b.mu.Unlock()
		if stale {
			rl.buckets.Delete(k)
			return true
		}
		live++
		return true
	})
	rl.live.Store(live)
}

func (rl *RateLimiter) allow(key string) bool {
	now := time.Now()
	v, loaded := rl.buckets.LoadOrStore(key, &tokenBucket{
		tokens:   rl.capacity,
		lastFill: now,
	})
	if !loaded && rl.live.Add(1) > maxBuckets {
		rl.evictRefilled(now)
	}
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

// clientIP is the limiter's key for a request: the address ClientIP attributes
// it to, which honours the edge's forwarding headers only when the peer is the
// edge. See clientip.go.
func (rl *RateLimiter) clientIP(r *http.Request) string {
	return ClientIP(r)
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

// Allow charges this limiter one token for the request's client, reporting
// whether the request may proceed. It is the same bucket Limit uses, keyed the
// same way.
//
// It exists because a route is not always one act. An endpoint that takes two
// shapes — an ordinary write, and an act with its own far stricter budget —
// cannot express that with a single wrapper, and wrapping the whole route with
// the stricter limiter would throttle the ordinary shape to the rare shape's
// rate. The handler charges the right bucket for the act it is about to perform.
//
// The caller owns the refusal: it has already parsed the request and knows what
// the client should be told.
func (rl *RateLimiter) Allow(r *http.Request) bool {
	return rl.allow(rl.clientIP(r))
}

// LimitKeyed wraps a handler with rate limiting keyed by the account the
// request is acting on rather than the address it came from. A per-IP budget
// is no defence for a login form: a credential-stuffing run spreads its
// guesses across as many addresses as it likes, and each address stays under
// the limit while the one account under attack absorbs all of them. Keying on
// the target — the handle in the form, the session behind a 2FA code — puts the
// ceiling on the account, where the exposure is.
//
// keyFn returns the account key for the request, or "" when the request names
// no account, in which case the client address is charged instead so an
// unkeyed request is never free.
func (rl *RateLimiter) LimitKeyed(keyFn func(*http.Request) string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := keyFn(r)
		if key == "" {
			key = "ip:" + ClientIP(r)
		} else {
			key = "acct:" + key
		}
		if !rl.allow(key) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "Too many attempts for this account — slow down", http.StatusTooManyRequests)
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
