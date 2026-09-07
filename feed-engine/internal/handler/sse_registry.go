package handler

import (
	"io"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// SSEEvent is a server-sent event carrying pre-rendered HTML. Never JSON.
type SSEEvent struct {
	Type string // event name (notify, post_engagement, price_update, etc.)
	Data string // pre-rendered HTML fragment — browser displays it directly
	// JSON is the same event for a native client: a JSON object, or "" when
	// the event has no structured form. The web stream never sends it; the
	// /api/v1 streams forward it and ignore events without one. One publish,
	// two projections — the server still renders, the phone still draws
	// nothing it was not told.
	JSON string
	// ID is the person-scoped sequence number this event was published under.
	// It is stamped by the publish path (never by a caller) so that every
	// connection of one PIAL sees the SAME number for the same event, and a
	// client that reconnects with Last-Event-ID names a position in a sequence
	// the server can still resolve. Zero means unnumbered — a per-connection
	// snapshot frame, which is not part of the replayable sequence.
	ID int64
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
	writeSSEFrameID(w, 0, eventType, data)
}

// writeSSEFrameID is writeSSEFrame with the frame's place in the person's
// sequence written on it. `id:` is the only field the SSE protocol carries back
// to the server on reconnect (as Last-Event-ID), so it is the whole of the gap
// story: a frame written without one cannot be resumed past. id <= 0 writes no
// `id:` line, which is correct for the snapshot frames a connection opens with —
// those describe the state at connect time and belong to no shared sequence.
func writeSSEFrameID(w io.Writer, id int64, eventType, data string) {
	var b strings.Builder
	if id > 0 {
		b.WriteString("id: ")
		b.WriteString(strconv.FormatInt(id, 10))
		b.WriteByte('\n')
	}
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
	// room marks a stream the user opened onto a PLACE (a live room, a
	// frequency) rather than onto their account. A room is entered and left
	// while the account stream stays up, so it is outside the per-account
	// bound: it neither counts toward it nor is evicted by it. See
	// maxSSEConnsPerUser.
	room bool
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
//
// The bound counts ACCOUNT streams only. A room stream (live, frequency) is a
// second connection the same device holds for as long as the user is in that
// place, and rooms are entered and left far more often than tabs are opened:
// counting them meant that walking through five live rooms silently evicted the
// user's own event stream, and the notifications, balance and messages it
// carries stopped arriving until the app reconnected. A room is bounded by the
// room, not by this.
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
	return registerSSESession(account, pial, false)
}

// RegisterRoomSSESession registers a stream held onto a PLACE — a live room, a frequency room —
// rather than onto the account. It joins both registry indexes exactly as an account stream does, so
// every push still reaches it, but it stands outside the per-account bound: it neither counts toward
// maxSSEConnsPerUser nor is ever chosen as the oldest session to evict. Being in a room must never
// cost the user the stream that carries their notifications.
func RegisterRoomSSESession(account, pial string) (<-chan SSEEvent, <-chan struct{}, func()) {
	return registerSSESession(account, pial, true)
}

func registerSSESession(account, pial string, room bool) (<-chan SSEEvent, <-chan struct{}, func()) {
	s := &sseSession{
		ch:      make(chan SSEEvent, 64),
		done:    make(chan struct{}),
		seq:     atomic.AddInt64(&sseSeq, 1),
		account: account,
		pial:    pial,
		room:    room,
	}

	acctBucket := addToRegistry(&accountRegistry, account, s)
	if pial != "" {
		_ = addToRegistry(&pialRegistry, pial, s)
	}

	// A connection opening is the moment to forget the people who have had no
	// event for long enough that no replay of theirs could still be honest.
	pruneStreamLogs()

	// Bound the set by evicting the OLDEST session(s) for this ACCOUNT — never refuse the newcomer.
	// The evicted session's handler observes <-done and returns, running its own cleanup (which
	// removes it from both indexes and closes its ch); we only close done here, so no double-close.
	// A room stream is outside the bound, so opening one neither counts nor evicts.
	if !room {
		acctBucket.mu.Lock()
		for {
			var oldest *sseSession
			held := 0
			for cand := range acctBucket.sessions {
				if cand.room {
					continue
				}
				held++
				if cand == s {
					continue
				}
				if oldest == nil || cand.seq < oldest.seq {
					oldest = cand
				}
			}
			if held <= maxSSEConnsPerUser || oldest == nil {
				break
			}
			delete(acctBucket.sessions, oldest)
			close(oldest.done)
		}
		acctBucket.mu.Unlock()
	}

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
	st := newStamper()
	deliverStamped(us, event, st)
}

// deliverStamped is deliver with one stamper shared across every bucket a single
// publish touches, so a person whose sessions sit in two indexes (their account
// bucket and their PIAL bucket) receives ONE number for the event rather than
// one per bucket. Caller must hold us.mu (read or write).
func deliverStamped(us *userSessions, event SSEEvent, st *stamper) {
	for s := range us.sessions {
		e := event
		e.ID = st.stamp(s.pial, event)
		select {
		case s.ch <- e:
		default:
			// A full 64-slot channel means this connection has not been read
			// from in a long time: the event is gone for it, and the client
			// will only notice as missing content. Silence here was the reason
			// a dropped push looked like a server that never sent one, so the
			// drop is now on the record — rate-limited, because the condition
			// that produces one produces thousands.
			noteSSEDrop(s.account, event.Type)
		}
	}
}

// ── Dropped-delivery accounting ──────────────────────────────────────────────

var (
	sseDropCount    atomic.Int64
	sseDropLoggedAt atomic.Int64 // unix nanos of the last log line
)

// sseDropLogInterval is the quiet period between two drop log lines. Every drop
// is counted; one line per interval reports the running total.
const sseDropLogInterval = 10 * time.Second

func noteSSEDrop(account, eventType string) {
	total := sseDropCount.Add(1)
	now := time.Now().UnixNano()
	last := sseDropLoggedAt.Load()
	if now-last < int64(sseDropLogInterval) {
		return
	}
	if !sseDropLoggedAt.CompareAndSwap(last, now) {
		return
	}
	log.Printf("[sse] dropped %s for account %s — connection buffer full (%d dropped since start)",
		eventType, account, total)
}

// ── Per-person sequence and replay ring ──────────────────────────────────────
//
// One counter and one ring per PIAL. The counter numbers every published event
// so the `id:` on the wire means something across all of that person's
// connections; the ring holds the last streamReplayDepth of them so a client
// that reconnects with Last-Event-ID is handed exactly what it missed instead
// of being told to reload the world.
//
// The ring deliberately does NOT live in the session bucket: that bucket is
// deleted the moment the last connection goes away, which is precisely the
// moment a gap opens. It lives beside it, and is swept on a timer instead.

// streamReplayDepth is how many events one person's ring remembers.
const streamReplayDepth = 64

// streamLogTTL is how long a person's ring outlives their last event. A client
// that reconnects later than this gets no replay and re-reads instead — a stale
// ring is worse than none, because it would hand back a prefix of a gap and let
// the client believe it had caught up.
const streamLogTTL = 5 * time.Minute

// replayFrame is one published event, remembered under its number.
type replayFrame struct {
	ID    int64
	Event SSEEvent
}

type streamLog struct {
	mu    sync.Mutex
	next  int64
	ring  []replayFrame
	touch time.Time
}

// pialStreamLogs maps PIAL → *streamLog.
var pialStreamLogs sync.Map

func logForPIAL(pial string) *streamLog {
	v, _ := pialStreamLogs.LoadOrStore(pial, &streamLog{})
	return v.(*streamLog)
}

// record numbers an event for one person and remembers it for replay.
func (sl *streamLog) record(event SSEEvent) int64 {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	sl.next++
	sl.touch = time.Now()
	sl.ring = append(sl.ring, replayFrame{ID: sl.next, Event: event})
	if len(sl.ring) > streamReplayDepth {
		sl.ring = append(sl.ring[:0], sl.ring[len(sl.ring)-streamReplayDepth:]...)
	}
	return sl.next
}

// stamper assigns one number per PIAL per publish. A publish that reaches the
// same person through two buckets must not consume two numbers, so the first
// call for a PIAL records the frame and every later call in the same publish
// returns what it recorded.
type stamper struct {
	seen map[string]int64
}

func newStamper() *stamper { return &stamper{seen: make(map[string]int64, 4)} }

func (st *stamper) stamp(pial string, event SSEEvent) int64 {
	if pial == "" {
		return 0
	}
	if id, ok := st.seen[pial]; ok {
		return id
	}
	id := logForPIAL(pial).record(event)
	st.seen[pial] = id
	return id
}

// RecordForPIAL numbers and remembers an event for one person without
// delivering it, so an event published while that person had no live connection
// still occupies its place in the sequence and can be replayed on reconnect.
func RecordForPIAL(pial string, event SSEEvent) int64 {
	if pial == "" {
		return 0
	}
	return logForPIAL(pial).record(event)
}

// ReplayAfter returns the events one person missed after lastID, oldest first.
// ok is false when the ring cannot prove it holds everything after lastID —
// the ring never saw that number, or has already discarded past it — and the
// caller must then stream live only and let the client re-read.
func ReplayAfter(pial string, lastID int64) (frames []replayFrame, ok bool) {
	if pial == "" || lastID <= 0 {
		return nil, false
	}
	v, exists := pialStreamLogs.Load(pial)
	if !exists {
		return nil, false
	}
	sl := v.(*streamLog)
	sl.mu.Lock()
	defer sl.mu.Unlock()
	if lastID > sl.next {
		// A number from an earlier process, or from another person's stream.
		return nil, false
	}
	if len(sl.ring) == 0 {
		return nil, lastID == sl.next
	}
	if sl.ring[0].ID > lastID+1 {
		// The gap starts before the oldest frame still held.
		return nil, false
	}
	for _, f := range sl.ring {
		if f.ID > lastID {
			frames = append(frames, f)
		}
	}
	return frames, true
}

// pruneStreamLogs drops the rings of people who have had no event for
// streamLogTTL. Called when a connection registers, which is both often enough
// to bound the map and cheap next to opening a stream.
func pruneStreamLogs() {
	cutoff := time.Now().Add(-streamLogTTL)
	pialStreamLogs.Range(func(k, v interface{}) bool {
		sl := v.(*streamLog)
		sl.mu.Lock()
		stale := sl.touch.Before(cutoff)
		sl.mu.Unlock()
		if stale {
			pialStreamLogs.Delete(k)
		}
		return true
	})
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
// The number is taken whether or not anyone is listening: a person whose only
// connection dropped a second ago is exactly the person about to reconnect with
// a Last-Event-ID, and an event that never took its place in the sequence is a
// gap nothing can name.
func PublishToUser(pial string, event SSEEvent) {
	if pial == "" {
		return
	}
	st := newStamper()
	st.stamp(pial, event)
	if v, ok := pialRegistry.Load(pial); ok {
		us := v.(*userSessions)
		us.mu.RLock()
		deliverStamped(us, event, st)
		us.mu.RUnlock()
	}
}

// PublishToPostWatchers pushes an event to every PIAL currently watching postID
// (delivered to all of that PIAL's connections).
func PublishToPostWatchers(postID string, event SSEEvent) {
	st := newStamper()
	pialRegistry.Range(func(_, v interface{}) bool {
		us := v.(*userSessions)
		us.mu.RLock()
		if us.ctx.postIDs[postID] {
			deliverStamped(us, event, st)
		}
		us.mu.RUnlock()
		return true
	})
}

// PublishToAllSessions broadcasts an event to every active SSE connection.
// Used for platform-wide signals (e.g. leaderboard refresh). Ranges the account index, which holds
// every live connection exactly once (the PIAL index is a subset/duplicate of the same sessions).
func PublishToAllSessions(event SSEEvent) {
	st := newStamper()
	accountRegistry.Range(func(_, v interface{}) bool {
		us := v.(*userSessions)
		us.mu.RLock()
		deliverStamped(us, event, st)
		us.mu.RUnlock()
		return true
	})
}

// PublishToTickerWatchers pushes an event to every PIAL watching ticker
// (delivered to all of that PIAL's connections).
func PublishToTickerWatchers(ticker string, event SSEEvent) {
	st := newStamper()
	pialRegistry.Range(func(_, v interface{}) bool {
		us := v.(*userSessions)
		us.mu.RLock()
		if us.ctx.tickers[ticker] {
			deliverStamped(us, event, st)
		}
		us.mu.RUnlock()
		return true
	})
}
