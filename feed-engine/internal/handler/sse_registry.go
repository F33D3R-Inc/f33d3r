package handler

import (
	"io"
	"strings"
	"sync"
	"sync/atomic"
)

// SSEEvent is a server-sent event carrying pre-rendered HTML. Never JSON.
type SSEEvent struct {
	Type string // event name (notify, post_engagement, price_update, etc.)
	Data string // pre-rendered HTML fragment — browser displays it directly
}

// writeSSEFrame writes one SSE event to w using spec-compliant framing. The payload is almost always
// a MULTI-LINE rendered facet fragment, so every line is emitted as its own `data:` field; the
// browser rejoins them with "\n" and reproduces the exact fragment.
//
// This is not optional polish — it is the wire contract. A single `data: <fragment>` write is
// silently broken: SSE payload is ONLY the text on `data:` lines, so a fragment that begins with a
// newline (every html/template fragment does) lands an EMPTY first data line and every following
// line is parsed as an unknown field and discarded. The client then receives an event whose
// `e.data` is "" and drops it (`if (!e.data) return;`) — the message/post/engagement never renders
// and the user must refresh. Worse, a fragment containing a blank line dispatches the event EARLY,
// desyncing the shared stream and corrupting the next event (notify/balance) too. One malformed
// frame takes the whole FA Live layer down.
//
// Caller flushes after this returns.
func writeSSEFrame(w io.Writer, eventType, data string) {
	var b strings.Builder
	b.WriteString("event: ")
	b.WriteString(eventType)
	b.WriteByte('\n')
	// strings.Split never returns empty, so an empty payload still emits one `data:` line (data == "").
	for _, line := range strings.Split(data, "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n') // blank line dispatches the event
	_, _ = io.WriteString(w, b.String())
}

// watchContext tracks what a session's owner is currently observing (a property of the user's
// attention, shared across all of that user's live connections).
type watchContext struct {
	postIDs map[string]bool
	tickers map[string]bool
}

// sseSession holds one active SSE connection (one browser tab / device). A connection belongs to a
// specific persona (account) opened by a specific person (PIAL), so it is indexed under both keys.
type sseSession struct {
	ch      chan SSEEvent
	done    chan struct{} // closed when this session is evicted by a newer one for the same account
	seq     int64         // creation order, for oldest-first eviction
	account string        // the persona this connection is scoped to (users.id) — messaging key
	pial    string        // the person behind it (PIAL) — person-level pipe key (notify/wallet/likes)
}

// userSessions holds every live SSE session under one registry key — one entry per connected device
// or tab — plus that key's shared watch context. A key maps to N connections, so a mutation fans out
// to all of them (each device shows the same live update). This replaces the old one-session-per-key
// model, where a second connection silently overwrote the first (only the last-connected device
// received pushes).
type userSessions struct {
	mu       sync.RWMutex
	sessions map[*sseSession]struct{}
	ctx      watchContext
	dead     bool // set under mu when the bucket is removed from the registry
}

// accountRegistry is the FA Live session registry keyed by ACCOUNT (users.id) — the persona pipe.
// Messaging delivery (PublishToAccount) routes here so a message reaches only the addressed persona's
// live connections, never the person's other personas.
// Key: account UUID string → *userSessions
var accountRegistry sync.Map

// pialRegistry is the FA Live session registry keyed by PIAL — the person pipe. Person-level signals
// (notifications, wallet balance, likes/engagement, price/post watchers) route here so every persona
// the person has open receives them. Key: PIAL UUID string → *userSessions
var pialRegistry sync.Map

// maxSSEConnsPerUser bounds the live-connection set per ACCOUNT. When a new connection would exceed
// it, the OLDEST session is evicted (see RegisterSSESession) rather than the new one refused — a
// fresh tab/refresh must NEVER end up with a dead feed (the old 429-refusal did exactly that, which
// is why new messages only appeared after a manual refresh). Eviction also reaps the stale sessions
// left by reconnect/refresh churn, so a message fans only to genuinely-live connections.
const maxSSEConnsPerUser = 5

// sseSeq gives each session a monotonic creation order, for oldest-first eviction.
var sseSeq int64

// addToRegistry inserts a session into one registry index (account or PIAL) under key, creating the
// bucket if absent. Returns the bucket it landed in. Eviction (account-bounded) is handled by the
// caller against the account bucket only.
func addToRegistry(reg *sync.Map, key string, s *sseSession) *userSessions {
	for {
		v, _ := reg.LoadOrStore(key, &userSessions{
			sessions: make(map[*sseSession]struct{}),
			ctx: watchContext{
				postIDs: make(map[string]bool),
				tickers: make(map[string]bool),
			},
		})
		us := v.(*userSessions)
		us.mu.Lock()
		if us.dead {
			// This bucket was torn down concurrently (its last session cleaned up and it was
			// removed from the map). Retry to create/get a fresh one.
			us.mu.Unlock()
			continue
		}
		us.sessions[s] = struct{}{}
		us.mu.Unlock()
		return us
	}
}

// removeFromRegistry removes a session from one registry index, reaping the bucket when it empties.
func removeFromRegistry(reg *sync.Map, key string, s *sseSession) {
	if key == "" {
		return
	}
	if v, ok := reg.Load(key); ok {
		us := v.(*userSessions)
		us.mu.Lock()
		delete(us.sessions, s)
		if len(us.sessions) == 0 {
			us.dead = true
			reg.Delete(key)
		}
		us.mu.Unlock()
	}
}

// RegisterSSESession creates a session for an ACCOUNT (persona) and the PIAL (person) behind it,
// adds it to BOTH registry indexes, and returns its event channel plus a cleanup func that must be
// deferred by the SSE handler. Multiple concurrent connections for the same account all stay
// registered. The account index drives messaging delivery; the PIAL index drives person-level
// signals (notifications/wallet/likes). pial may be "" (anonymous/no identity) — then only the
// account index is used. The connection set is bounded per ACCOUNT (evict-oldest).
func RegisterSSESession(account, pial string) (<-chan SSEEvent, <-chan struct{}, func()) {
	s := &sseSession{
		ch:      make(chan SSEEvent, 64),
		done:    make(chan struct{}),
		seq:     atomic.AddInt64(&sseSeq, 1),
		account: account,
		pial:    pial,
	}

	acctBucket := addToRegistry(&accountRegistry, account, s)
	if pial != "" {
		_ = addToRegistry(&pialRegistry, pial, s)
	}

	// Bound the set by evicting the OLDEST session(s) for this ACCOUNT — never refuse the newcomer.
	// The evicted session's handler observes <-done and returns, running its own cleanup (which
	// removes it from both indexes and closes its ch); we only close done here, so no double-close.
	acctBucket.mu.Lock()
	for len(acctBucket.sessions) > maxSSEConnsPerUser {
		var oldest *sseSession
		for cand := range acctBucket.sessions {
			if cand == s {
				continue
			}
			if oldest == nil || cand.seq < oldest.seq {
				oldest = cand
			}
		}
		if oldest == nil {
			break
		}
		delete(acctBucket.sessions, oldest)
		close(oldest.done)
	}
	acctBucket.mu.Unlock()

	return s.ch, s.done, func() {
		removeFromRegistry(&accountRegistry, account, s)
		removeFromRegistry(&pialRegistry, pial, s)
		close(s.ch)
	}
}

// withPial runs fn against the PIAL's bucket under its lock, if one exists (person-level watch ctx).
func withPial(pial string, fn func(us *userSessions)) {
	if v, ok := pialRegistry.Load(pial); ok {
		us := v.(*userSessions)
		us.mu.Lock()
		fn(us)
		us.mu.Unlock()
	}
}

// AddPostWatcher registers that PIAL is viewing a specific post.
func AddPostWatcher(pial, postID string) {
	withPial(pial, func(us *userSessions) { us.ctx.postIDs[postID] = true })
}

// RemovePostWatcher deregisters PIAL from watching a post.
func RemovePostWatcher(pial, postID string) {
	withPial(pial, func(us *userSessions) { delete(us.ctx.postIDs, postID) })
}

// AddTickerWatcher registers that PIAL has a cashtag card for ticker visible.
func AddTickerWatcher(pial, ticker string) {
	withPial(pial, func(us *userSessions) { us.ctx.tickers[ticker] = true })
}

// RemoveTickerWatcher deregisters PIAL from watching a ticker.
func RemoveTickerWatcher(pial, ticker string) {
	withPial(pial, func(us *userSessions) { delete(us.ctx.tickers, ticker) })
}

// deliver pushes an event to every session in a bucket. Non-blocking per session: a full channel
// drops that session's event. Caller must hold us.mu (read or write).
func deliver(us *userSessions, event SSEEvent) {
	for s := range us.sessions {
		select {
		case s.ch <- event:
		default:
		}
	}
}

// PublishToAccount pushes an event to every live connection of one specific ACCOUNT (persona).
// This is the messaging pipe — a DM reaches only the addressed persona's devices, never the
// person's other personas. Non-blocking: a full connection channel drops the event for it.
func PublishToAccount(account string, event SSEEvent) {
	if v, ok := accountRegistry.Load(account); ok {
		us := v.(*userSessions)
		us.mu.RLock()
		deliver(us, event)
		us.mu.RUnlock()
	}
}

// PublishToUser pushes an event to every live connection of one specific PIAL (person) — across all
// of that person's open personas. This is the person-level pipe (notifications, wallet balance,
// likes/engagement). Non-blocking: a full connection channel drops the event for it.
func PublishToUser(pial string, event SSEEvent) {
	if v, ok := pialRegistry.Load(pial); ok {
		us := v.(*userSessions)
		us.mu.RLock()
		deliver(us, event)
		us.mu.RUnlock()
	}
}

// PublishToPostWatchers pushes an event to every PIAL currently watching postID
// (delivered to all of that PIAL's connections).
func PublishToPostWatchers(postID string, event SSEEvent) {
	pialRegistry.Range(func(_, v interface{}) bool {
		us := v.(*userSessions)
		us.mu.RLock()
		if us.ctx.postIDs[postID] {
			deliver(us, event)
		}
		us.mu.RUnlock()
		return true
	})
}

// PublishToAllSessions broadcasts an event to every active SSE connection.
// Used for platform-wide signals (e.g. leaderboard refresh). Ranges the account index, which holds
// every live connection exactly once (the PIAL index is a subset/duplicate of the same sessions).
func PublishToAllSessions(event SSEEvent) {
	accountRegistry.Range(func(_, v interface{}) bool {
		us := v.(*userSessions)
		us.mu.RLock()
		deliver(us, event)
		us.mu.RUnlock()
		return true
	})
}

// PublishToTickerWatchers pushes an event to every PIAL watching ticker
// (delivered to all of that PIAL's connections).
func PublishToTickerWatchers(ticker string, event SSEEvent) {
	pialRegistry.Range(func(_, v interface{}) bool {
		us := v.(*userSessions)
		us.mu.RLock()
		if us.ctx.tickers[ticker] {
			deliver(us, event)
		}
		us.mu.RUnlock()
		return true
	})
}
