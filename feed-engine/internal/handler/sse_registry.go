package handler

import (
	"io"
	"log"
	"sort"
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
	// ID is this event's place in the live sequence. It is stamped by the publish
	// path (never by a caller) and travels with the event between replicas, so a
	// client that reconnects with Last-Event-ID names a position any replica can
	// resolve. Zero means unnumbered: a per-connection frame that is not part of
	// the replayable sequence.
	ID int64
	// Bulk marks a high-volume event. Bulk events have their own buffer, so a burst
	// of them cannot crowd out a person's other updates.
	Bulk bool
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
	high    chan SSEEvent // updates addressed to this person
	low     chan SSEEvent // high-volume events, delivered after high
	out     chan SSEEvent // what the stream handler reads
	stop    chan struct{} // closed by cleanup
	lost    chan struct{} // signalled when a delivery was dropped
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
		high:    make(chan SSEEvent, sseBufferSize),
		low:     make(chan SSEEvent, sseBufferSize),
		out:     make(chan SSEEvent),
		stop:    make(chan struct{}),
		lost:    make(chan struct{}, 1),
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

	go s.pump()

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

	return s.out, s.done, func() {
		removeFromRegistry(&accountRegistry, account, s)
		removeFromRegistry(&pialRegistry, pial, s)
		close(s.stop)
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
	applyWatch(pial, liveWatchPost, postID, true)
	relayWatch(pial, liveWatchPost, postID, true)
}

// RemovePostWatcher deregisters PIAL from watching a post.
func RemovePostWatcher(pial, postID string) {
	applyWatch(pial, liveWatchPost, postID, false)
	relayWatch(pial, liveWatchPost, postID, false)
}

// AddTickerWatcher registers that PIAL has a cashtag card for ticker visible.
func AddTickerWatcher(pial, ticker string) {
	applyWatch(pial, liveWatchTicker, ticker, true)
	relayWatch(pial, liveWatchTicker, ticker, true)
}

// RemoveTickerWatcher deregisters PIAL from watching a ticker.
func RemoveTickerWatcher(pial, ticker string) {
	applyWatch(pial, liveWatchTicker, ticker, false)
	relayWatch(pial, liveWatchTicker, ticker, false)
}

const maxWatchesPerPerson = 2000

// applyWatch updates this replica's watch context for one person.
func applyWatch(pial, watchType, id string, add bool) {
	withPial(pial, func(us *userSessions) {
		set := us.ctx.postIDs
		if watchType == liveWatchTicker {
			set = us.ctx.tickers
		}
		if add {
			// A page holds tens of watches. Past this a client is misbehaving, so an
			// old one gives way rather than memory growing without limit.
			if len(set) >= maxWatchesPerPerson && !set[id] {
				for old := range set {
					delete(set, old)
					break
				}
			}
			set[id] = true
		} else {
			delete(set, id)
		}
	})
}

// sseBufferSize is how many undelivered events one session holds per buffer.
const sseBufferSize = 64

// sseResync tells a client it may have missed updates and must re-read its Facets.
const sseResync = "resync"

// sseCursor gives a new connection a resume point before any event has reached it,
// so a drop in a quiet moment still has a position to resume from.
const sseCursor = "fa.cursor"

// bulkEventTypes are the high-volume event names that use the low buffer.
var bulkEventTypes = map[string]bool{
	"new_post": true, "post_engagement": true, "work_engagement": true, "price_update": true,
	"sports_facet": true, "leaderboard_refresh": true, "live_chat": true, "article_view": true,
}

// offer queues an event without blocking. A full buffer costs this connection the
// event, so it is counted and the client is told to re-read.
func (s *sseSession) offer(event SSEEvent) {
	ch := s.high
	if event.Bulk || bulkEventTypes[event.Type] {
		ch = s.low
	}
	select {
	case ch <- event:
	default:
		noteSSEDrop(s.account, event.Type)
		s.markLost()
	}
}

// markLost asks the pump to send one resync. Repeats collapse into one.
func (s *sseSession) markLost() {
	select {
	case s.lost <- struct{}{}:
	default:
	}
}

// pump moves events to the stream handler, high buffer first.
func (s *sseSession) pump() {
	defer close(s.out)
	for {
		var ev SSEEvent
		select {
		case <-s.lost:
			ev = SSEEvent{Type: sseResync}
		case ev = <-s.high:
		default:
			select {
			case <-s.stop:
				return
			case <-s.lost:
				ev = SSEEvent{Type: sseResync}
			case ev = <-s.high:
			case ev = <-s.low:
			}
		}
		select {
		case s.out <- ev:
		case <-s.stop:
			return
		}
	}
}

// deliver pushes an event to every session in a bucket. Caller must hold us.mu.
func deliver(us *userSessions, event SSEEvent) {
	for s := range us.sessions {
		s.offer(event)
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

// ── Live sequence and replay log ─────────────────────────────────────────────
//
// IDs come from the clock, assigned once by the replica that published the event
// and carried with it. That keeps a number meaningful across restarts and across
// replicas, which per-process counters could not do: a counter restarted at 1
// could replay the wrong events, and two replicas numbered the same event
// differently. Replica clocks that disagree by a moment can order two events
// differently, which is safe because a Facet fragment is a complete snapshot.
//
// The log holds every event this replica saw, whoever it was for, so messages and
// broadcasts to someone offline can still be replayed when they return. Its floor
// is the point it can vouch for: a resume point below the floor gets a re-read
// instead of a partial replay it would mistake for the whole gap.

const (
	liveKindUser    = "user"
	liveKindAccount = "account"
	liveKindPost    = "post"
	liveKindTicker  = "ticker"
	liveKindAll     = "all"
	liveKindWatch   = "watch"

	liveWatchPost   = "post"
	liveWatchTicker = "ticker"
	liveWatchAdd    = "add"
	liveWatchRemove = "remove"

	liveLogMaxFrames = 50_000
	liveLogMaxBytes  = 64 << 20
	liveLogTTL       = 5 * time.Minute
)

// liveLastID is the last ID this replica issued.
var liveLastID atomic.Int64

// nextLiveID returns a unique, increasing ID based on the clock.
func nextLiveID() int64 {
	for {
		id := time.Now().UnixMicro()
		last := liveLastID.Load()
		if id <= last {
			id = last + 1
		}
		if liveLastID.CompareAndSwap(last, id) {
			return id
		}
	}
}

// replayFrame is one event handed back to a reconnecting client.
type replayFrame struct {
	ID    int64
	Event SSEEvent
}

type liveFrame struct {
	Kind  string
	Key   string
	Event SSEEvent
	At    time.Time
}

type liveLog struct {
	mu     sync.Mutex
	frames []liveFrame
	bytes  int
	floor  int64
}

// liveEvents starts with its floor at boot: nothing earlier can be vouched for.
var liveEvents = &liveLog{floor: nextLiveID()}

func (l *liveLog) add(kind, key string, event SSEEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.frames = append(l.frames, liveFrame{Kind: kind, Key: key, Event: event, At: now})
	l.bytes += len(event.Data) + len(event.JSON)

	drop := 0
	for drop < len(l.frames) {
		f := l.frames[drop]
		if len(l.frames)-drop <= liveLogMaxFrames && l.bytes <= liveLogMaxBytes && now.Sub(f.At) <= liveLogTTL {
			break
		}
		l.bytes -= len(f.Event.Data) + len(f.Event.JSON)
		if f.Event.ID > l.floor {
			l.floor = f.Event.ID
		}
		drop++
	}
	if drop > 0 {
		l.frames = append(l.frames[:0], l.frames[drop:]...)
	}
}

// raiseFloor records that this replica cannot vouch for events before id.
func (l *liveLog) raiseFloor(id int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if id > l.floor {
		l.floor = id
	}
}

// ReplayAfter returns the events one connection missed after lastID, oldest first.
// ok is false when this replica cannot vouch for the whole gap, and the client
// must then re-read.
func ReplayAfter(account, pial string, lastID int64) (frames []replayFrame, ok bool) {
	if lastID <= 0 || lastID > time.Now().Add(time.Minute).UnixMicro() {
		return nil, false
	}
	posts := map[string]bool{}
	tickers := map[string]bool{}
	withPial(pial, func(us *userSessions) {
		for id := range us.ctx.postIDs {
			posts[id] = true
		}
		for id := range us.ctx.tickers {
			tickers[id] = true
		}
	})

	liveEvents.mu.Lock()
	defer liveEvents.mu.Unlock()
	if lastID < liveEvents.floor {
		return nil, false
	}
	for _, f := range liveEvents.frames {
		if f.Event.ID <= lastID {
			continue
		}
		match := false
		switch f.Kind {
		case liveKindUser:
			match = pial != "" && f.Key == pial
		case liveKindAccount:
			match = f.Key == account
		case liveKindPost:
			match = posts[f.Key]
		case liveKindTicker:
			match = tickers[f.Key]
		case liveKindAll:
			match = true
		}
		if match {
			frames = append(frames, replayFrame{ID: f.Event.ID, Event: f.Event})
		}
	}
	sort.SliceStable(frames, func(i, j int) bool { return frames[i].ID < frames[j].ID })
	return frames, true
}

// deliverLive records an event and hands it to this replica's connections.
func deliverLive(kind, key string, event SSEEvent) {
	liveEvents.add(kind, key, event)
	switch kind {
	case liveKindUser:
		if v, ok := pialRegistry.Load(key); ok {
			us := v.(*userSessions)
			us.mu.RLock()
			deliver(us, event)
			us.mu.RUnlock()
		}
	case liveKindAccount:
		if v, ok := accountRegistry.Load(key); ok {
			us := v.(*userSessions)
			us.mu.RLock()
			deliver(us, event)
			us.mu.RUnlock()
		}
	case liveKindPost, liveKindTicker:
		pialRegistry.Range(func(_, v interface{}) bool {
			us := v.(*userSessions)
			us.mu.RLock()
			watching := us.ctx.postIDs[key]
			if kind == liveKindTicker {
				watching = us.ctx.tickers[key]
			}
			if watching {
				deliver(us, event)
			}
			us.mu.RUnlock()
			return true
		})
	case liveKindAll:
		accountRegistry.Range(func(_, v interface{}) bool {
			us := v.(*userSessions)
			us.mu.RLock()
			deliver(us, event)
			us.mu.RUnlock()
			return true
		})
	}
}

// publishLive numbers an event, delivers it here, and relays it to other replicas.
func publishLive(kind, key string, event SSEEvent) {
	event.ID = nextLiveID()
	deliverLive(kind, key, event)
	relayLive(kind, key, event)
}

// PublishToAccount pushes an event to every live connection of one ACCOUNT (persona).
// This is the messaging pipe: a message reaches only the addressed persona's devices.
func PublishToAccount(account string, event SSEEvent) {
	if account == "" {
		return
	}
	publishLive(liveKindAccount, account, event)
}

// PublishToUser pushes an event to every live connection of one PIAL (person), across
// all of that person's open personas. This is the person-level pipe.
func PublishToUser(pial string, event SSEEvent) {
	if pial == "" {
		return
	}
	publishLive(liveKindUser, pial, event)
}

// PublishToPostWatchers pushes an event to every person currently watching postID.
func PublishToPostWatchers(postID string, event SSEEvent) {
	publishLive(liveKindPost, postID, event)
}

// PublishToAllSessions broadcasts an event to every active connection.
func PublishToAllSessions(event SSEEvent) {
	publishLive(liveKindAll, "", event)
}

// PublishToTickerWatchers pushes an event to every person watching ticker.
func PublishToTickerWatchers(ticker string, event SSEEvent) {
	publishLive(liveKindTicker, ticker, event)
}

// resyncLocalSessions tells every connection on this replica to re-read.
func resyncLocalSessions() {
	accountRegistry.Range(func(_, v interface{}) bool {
		us := v.(*userSessions)
		us.mu.RLock()
		for s := range us.sessions {
			s.markLost()
		}
		us.mu.RUnlock()
		return true
	})
}
