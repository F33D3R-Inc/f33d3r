package sports

import (
	"context"
	"log"
	"sync"
	"time"
)

// gamecast.go — the per-game detail poller behind the game page.
//
// The scoreboard poller reads every league's slate on a cadence of its own and
// asks nothing about any one game. The game page needs more — every play, the
// leaders, the box score — and it needs it every few seconds, which is far too
// much to ask about every game on the board when almost nobody has most of them
// open. So detail is polled on demand:
//
//   - A game's summary is read ONLY while at least one viewer has that game's
//     page open. Who has it open is not tracked here: it is read from the FA
//     Live connection registry, through the ViewerSet the render layer hands
//     in. The registry already knows which person is looking at what, and it
//     forgets them when their connection ends, so there is no second presence
//     system to drift out of step with the first.
//   - ONE upstream request per game per interval, never one per viewer. Every
//     read of a game — the poll loop's and a page render's — goes through the
//     game's own entry lock, and a reading younger than the game's interval is
//     reused, so a hundred people opening the page at once cost one request.
//   - The interval follows the game: five seconds while it is being played, a
//     minute before it starts, and NEVER again once a reading of the finished
//     game has been taken. A final does not change.
//   - A game nobody has looked at for detailForget is dropped from memory.
//
// Each successful read is handed, with the reading before it, to the
// DetailSink. That is the render layer's seam: it diffs the two and pushes only
// the Facets that changed, to only the people viewing that game.

const (
	// DetailLiveInterval is how often a game being played is read. The game
	// page's clock is refreshed at this cadence by the server; no browser ticks.
	DetailLiveInterval = 5 * time.Second
	// detailDelayedInterval applies to a delayed game: it can resume without
	// warning, but nothing is happening on the field.
	detailDelayedInterval = 30 * time.Second
	// detailPregameInterval applies before the start and to a game whose state
	// is not known.
	detailPregameInterval = 60 * time.Second
	// detailRescan is the longest the loop sleeps without re-reading the
	// registry. It equals the live interval, so a page opened on a game being
	// played is first re-read no later than one live interval after it loaded.
	detailRescan = DetailLiveInterval
	// detailForget is how long an unviewed game's detail is kept.
	detailForget = 10 * time.Minute
	// detailFetchTimeout bounds one upstream read.
	detailFetchTimeout = 8 * time.Second
	// detailParallel bounds how many games are read at once.
	detailParallel = 4
)

// DetailProvider is a provider that can read one game's full summary. It is an
// optional capability: a provider without it still drives the game page, whose
// header then moves with the scoreboard and whose play lists say the feed does
// not carry plays.
type DetailProvider interface {
	// Detail reads one game. base is the scoreboard's current reading of it;
	// prev is the last Detail built for it (nil at first), which the provider
	// may return re-stamped when the upstream document is byte-identical.
	Detail(ctx context.Context, base Game, prev *Detail) (*Detail, error)
}

// ViewerSet names every game id at least one viewer has open right now.
type ViewerSet func() []string

// DetailSink receives the previous and the next reading of one game. prev is
// nil for the first. Calls for ONE game are strictly ordered and never
// concurrent; calls for different games may run concurrently.
type DetailSink func(prev, next *Detail)

type detailEntry struct {
	// mu serialises every read of this game and the sink call that follows it,
	// which is what makes the per-game ordering guarantee hold.
	mu       sync.Mutex
	detail   *Detail
	readAt   time.Time
	viewedAt time.Time
}

type gamecast struct {
	mu      sync.Mutex
	entries map[string]*detailEntry
	viewers ViewerSet
	sink    DetailSink
}

func newGamecast() gamecast {
	return gamecast{entries: map[string]*detailEntry{}}
}

// OnDetail registers who is viewing which game and where readings go. Must be
// called before Start.
func (c *Cache) OnDetail(viewers ViewerSet, sink DetailSink) {
	if c == nil {
		return
	}
	c.cast.viewers = viewers
	c.cast.sink = sink
}

// Detail returns the current reading of one game, reading the upstream first
// when the held reading is older than the game's interval. It is what a page
// render calls, so the first viewer of a game sees its plays on first paint.
// false means the game is not on the board.
func (c *Cache) Detail(ctx context.Context, id string) (*Detail, bool) {
	if c == nil {
		return nil, false
	}
	if _, ok := c.Game(id); !ok {
		return nil, false
	}
	e := c.entry(id)
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	e.viewedAt = now
	if detailDue(e, now) {
		c.readDetail(ctx, id, e, now)
	}
	if e.detail == nil {
		// The upstream has not answered for this game yet. The header still
		// renders from the scoreboard, and the next poll fills the rest in.
		g, _ := c.Game(id)
		return &Detail{Game: g, Observed: g.Observed}, true
	}
	return e.detail, true
}

func (c *Cache) entry(id string) *detailEntry {
	c.cast.mu.Lock()
	defer c.cast.mu.Unlock()
	e := c.cast.entries[id]
	if e == nil {
		e = &detailEntry{}
		c.cast.entries[id] = e
	}
	return e
}

// detailInterval is how long a reading of g stays current. Zero means it never
// expires: the game is over and a reading of it as over has been taken.
func detailInterval(g Game) time.Duration {
	switch {
	case g.State.Active():
		return DetailLiveInterval
	case g.State == StateDelayed:
		return detailDelayedInterval
	case g.State.Settled():
		return 0
	}
	return detailPregameInterval
}

// detailDue reports whether e needs reading. Caller holds e.mu.
func detailDue(e *detailEntry, now time.Time) bool {
	if e.detail == nil {
		return e.readAt.IsZero() || now.Sub(e.readAt) >= detailPregameInterval/4
	}
	iv := detailInterval(e.detail.Game)
	if iv == 0 {
		return false
	}
	return now.Sub(e.readAt) >= iv
}

// readDetail reads one game and hands the result to the sink. Caller holds
// e.mu. A failed read keeps the last good reading and is retried at the game's
// interval rather than immediately: a failing upstream is not hammered.
func (c *Cache) readDetail(ctx context.Context, id string, e *detailEntry, now time.Time) {
	e.readAt = now
	base, ok := c.Game(id)
	if !ok {
		return
	}
	next, err := c.fetchDetail(ctx, base, e.detail)
	if err != nil {
		log.Printf("[sports] detail %s/%s: %v", base.League, id, err)
		return
	}
	prev := e.detail
	e.detail = next
	if c.cast.sink != nil && next != prev {
		c.cast.sink(prev, next)
	}
}

// fetchDetail asks the game's league provider for a summary. A provider that
// cannot read one yields the scoreboard's reading as a header-only Detail,
// which costs no request at all.
func (c *Cache) fetchDetail(ctx context.Context, base Game, prev *Detail) (*Detail, error) {
	var dp DetailProvider
	c.mu.RLock()
	for _, l := range c.lanes {
		if l.league.Slug == base.League {
			dp, _ = l.provider.(DetailProvider)
			break
		}
	}
	c.mu.RUnlock()
	if dp == nil {
		if prev != nil && prev.Game.CardKey() == base.CardKey() {
			return prev, nil
		}
		return &Detail{Game: base, Observed: base.Observed}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, detailFetchTimeout)
	defer cancel()
	return dp.Detail(ctx, base, prev)
}

func (c *Cache) startGamecast(ctx context.Context) {
	if c.cast.viewers == nil {
		return
	}
	log.Printf("[sports] game detail polled only while viewed — live every %s, never after the final", DetailLiveInterval)
	go c.detailLoop(ctx)
}

func (c *Cache) detailLoop(ctx context.Context) {
	for {
		wait := c.pollDetails(ctx, time.Now())
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// pollDetails reads every viewed game that is due, forgets games nobody has
// viewed in a while, and returns how long to sleep before the next pass.
func (c *Cache) pollDetails(ctx context.Context, now time.Time) time.Duration {
	viewed := map[string]bool{}
	for _, id := range c.cast.viewers() {
		if _, ok := c.Game(id); ok {
			viewed[id] = true
		}
	}

	c.cast.mu.Lock()
	for id, e := range c.cast.entries {
		if viewed[id] {
			continue
		}
		if e.mu.TryLock() {
			if now.Sub(e.viewedAt) > detailForget && now.Sub(e.readAt) > detailForget {
				delete(c.cast.entries, id)
			}
			e.mu.Unlock()
		}
	}
	c.cast.mu.Unlock()

	sem := make(chan struct{}, detailParallel)
	var wg sync.WaitGroup
	wait := detailRescan
	var waitMu sync.Mutex
	for id := range viewed {
		e := c.entry(id)
		sem <- struct{}{}
		wg.Add(1)
		go func(id string, e *detailEntry) {
			defer func() { <-sem; wg.Done() }()
			e.mu.Lock()
			defer e.mu.Unlock()
			e.viewedAt = now
			if detailDue(e, now) {
				c.readDetail(ctx, id, e, now)
			}
			if e.detail == nil {
				return
			}
			iv := detailInterval(e.detail.Game)
			if iv == 0 {
				return
			}
			left := iv - time.Since(e.readAt)
			if left < time.Second {
				left = time.Second
			}
			waitMu.Lock()
			if left < wait {
				wait = left
			}
			waitMu.Unlock()
		}(id, e)
	}
	wg.Wait()
	return wait
}
