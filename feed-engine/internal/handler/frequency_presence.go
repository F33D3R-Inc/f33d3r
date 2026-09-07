package handler

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/f33d3r/feed-engine/internal/auralis"
)

// ── who is on air, for the ring on their avatar ──────────────────────────────
//
// A person hosting a Frequency wears an accent ring and a small mic badge
// wherever their face appears — the feed, a quote card, their profile, a
// search row. The phone draws it only because this server said so, and it says
// so by putting `live_frequency_id` on the author strip.
//
// The cost of that field is the problem this file solves. Asking Auralis "is
// this person live?" once per author would be a brain call per face on every
// feed page. Instead the whole live lane is read at once — it is at most a
// hundred rooms platform-wide — and held for five seconds:
//
//   * one read fills a map of host PIAL → the id of the room they are hosting;
//   * every author strip built in the next five seconds is a map lookup;
//   * the first caller past that window serves the map it already has and
//     refreshes behind them (stale-while-revalidate), so no request ever waits
//     on the brain for a decoration;
//   * only one refresh is ever in flight (the singleflight `busy` flag);
//   * a brain that is unset, unreachable or angry yields an empty map. The
//     ring is decoration: its absence is a missing ring, never a 5xx and never
//     a slower feed.
//
// The value put on the wire is the Frequency's own uuid — the same public id
// `GET /api/v1/frequencies/{id}` takes. The host's PIAL is the map's key and
// stays on this side of the boundary, as every PIAL does.

const (
	// frequencyPresenceTTL is how long one read of the live lane is believed.
	// Five seconds is well inside the time it takes a listener to notice a
	// room opened, and it collapses a feed page's worth of faces into one call.
	frequencyPresenceTTL = 5 * time.Second
	// frequencyPresenceLimit is how much of the live lane is read. A platform
	// with more than this many rooms on air at once gets rings on the hundred
	// the brain ranks first, which is the same hundred the lane itself shows.
	frequencyPresenceLimit = 100
	// frequencyPresenceTimeout bounds one lane read. The auralis client carries
	// its own per-call budget (AURALIS_TIMEOUT_MS); this is the outer wall so a
	// refresh goroutine can never outlive the window it was refreshing for.
	frequencyPresenceTimeout = 5 * time.Second
)

// frequencyPresence is the cached answer to "who is hosting right now".
type frequencyPresence struct {
	mu     sync.Mutex
	client *auralis.Client
	ttl    time.Duration
	// hosts maps a host's PIAL to the id of the Frequency they are hosting.
	hosts map[string]string
	// filled is false until the first read completes — a first caller has
	// nothing to serve stale and so waits for the brain rather than drawing no
	// ring on a platform that has rooms open.
	filled bool
	at     time.Time
	// busy is the singleflight: one refresh at a time, whoever asked.
	busy bool
}

// frequencyRings is the process's presence cache. It is a package variable
// because the author strip is built by pure mappers (authorDTO, userAuthorDTO,
// userDTO) that are called from two dozen places and are not given a handler;
// New wires the brain into it once at start-up.
var frequencyRings = &frequencyPresence{ttl: frequencyPresenceTTL}

// useFrequencyPresence points the ring lookup at a Frequencies brain. Called
// once, from New. A nil or unconfigured client leaves every author strip
// without a ring, which is exactly right for a deployment that does not run
// Auralis.
func useFrequencyPresence(c *auralis.Client) {
	frequencyRings.mu.Lock()
	defer frequencyRings.mu.Unlock()
	frequencyRings.client = c
	frequencyRings.hosts = nil
	frequencyRings.filled = false
	frequencyRings.at = time.Time{}
}

// liveFrequencyFor is the id of the Frequency this PIAL is hosting right now,
// or nil. This is the whole of what the DTOs call.
func liveFrequencyFor(pial string) *string {
	if pial == "" {
		return nil
	}
	id, ok := frequencyRings.liveHosts()[pial]
	if !ok || id == "" {
		return nil
	}
	return &id
}

// liveHosts is the current map, refreshed if it has gone stale. The returned
// map is never written to after it is published, so callers may read it
// without the lock.
func (p *frequencyPresence) liveHosts() map[string]string {
	p.mu.Lock()
	if p.client == nil || !p.client.Configured() {
		p.mu.Unlock()
		return nil
	}
	fresh := p.filled && time.Since(p.at) < p.ttl
	hosts, filled, busy := p.hosts, p.filled, p.busy
	if fresh {
		p.mu.Unlock()
		return hosts
	}
	if busy {
		// Someone else is already asking. Serve what we have — nil on the very
		// first concurrent call, which costs one page of rings, once.
		p.mu.Unlock()
		return hosts
	}
	p.busy = true
	p.mu.Unlock()

	if filled {
		// Stale-while-revalidate: this caller is not made to wait for a ring.
		go p.refresh()
		return hosts
	}
	return p.refresh()
}

// refresh reads the live lane and replaces the map. It always clears `busy`,
// and it always leaves a map behind — an empty one when the brain could not
// answer — so a brain that is down costs one call every ttl and nothing else.
func (p *frequencyPresence) refresh() map[string]string {
	p.mu.Lock()
	c := p.client
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), frequencyPresenceTimeout)
	defer cancel()
	// No acting identity: this is the platform asking which rooms are on air,
	// not a person asking what they may see. The lane read is public.
	list, err := c.List(ctx, "live", "", frequencyPresenceLimit)

	hosts := map[string]string{}
	if err != nil {
		log.Printf("[api/v1] frequencies: reading the live lane for avatar rings: %v", err)
	} else if list != nil {
		for _, it := range list.Items {
			s := it.Frequency
			if s.ID == "" || s.HostPialID == "" {
				continue
			}
			if s.State != "live" && s.State != "starting" {
				continue
			}
			hosts[s.HostPialID] = s.ID
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.busy = false
	p.hosts = hosts
	p.filled = true
	p.at = time.Now()
	return hosts
}
