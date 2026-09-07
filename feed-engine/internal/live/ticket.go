package live

// ticket.go — one-time publish tickets for browser broadcast.
//
// A phone broadcasting from the browser cannot be handed the stream's ingest
// key: the key is a long-lived secret, and anything rendered into a page has
// left the server. So the browser is handed nothing. It posts its WebRTC offer
// to a same-origin endpoint, the server mints a single-use ticket, and the
// server presents that ticket to the media server on the broadcaster's behalf.
//
// A ticket is valid for one stream, for one minute, and for one handshake. The
// stream's real ingest key — the one an OBS user configured — is never
// touched, so going live from a phone does not invalidate a desktop encoder's
// configuration.
//
// "One handshake" and not "one call": the media server may ask the control
// plane about the same WebRTC publish more than once while it sets the session
// up — once when the HTTP handler resolves the path, again when the session
// registers as the publisher. A ticket that vanished on the first answer made
// the second fall through to the ingest-key check, which the ticket cannot
// pass, and the publisher was dropped after the browser had already been given
// its SDP answer: a broadcast that looked connected and carried nothing. So a
// redeemed ticket stays redeemable for the length of one handshake, bound to
// the stream it was minted for. The ticket never leaves the container network
// — it travels from this process to the media server and back — so the window
// widens nothing an outsider can reach.

import (
	"crypto/subtle"
	"sync"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

// ticketTTL is how long a minted ticket may be redeemed for the first time.
// The proxy redeems it on the very next request, so this only has to survive
// one round trip.
const ticketTTL = 60 * time.Second

// ticketHandshake is how long a ticket stays redeemable after its first
// redemption: the media server's WebRTC handshake timeout, within which every
// authorisation call for one publish is made.
const ticketHandshake = 10 * time.Second

type ticket struct {
	value    string
	expires  time.Time
	redeemed time.Time // zero until the first successful redemption
}

var (
	ticketMu sync.Mutex
	tickets  = map[string]ticket{} // stream id -> outstanding ticket
)

// MintPublishTicket issues a single-use publish credential for one stream,
// replacing any ticket already outstanding for it. The caller must already have
// established that the requester owns the stream.
func MintPublishTicket(streamID string) (string, error) {
	value, err := newTicketValue()
	if err != nil {
		return "", err
	}
	ticketMu.Lock()
	defer ticketMu.Unlock()
	tickets[streamID] = ticket{value: value, expires: time.Now().Add(ticketTTL)}
	return value, nil
}

// ConsumePublishTicket redeems a ticket. It returns true for every call within
// one handshake of the first redemption, and never after: a ticket is gone
// once the handshake window closes, or once its unredeemed minute runs out.
func ConsumePublishTicket(streamID, presented string) bool {
	return consumePublishTicketAt(streamID, presented, time.Now())
}

// consumePublishTicketAt is ConsumePublishTicket with the clock passed in, so
// the handshake window can be tested without waiting for it.
func consumePublishTicketAt(streamID, presented string, now time.Time) bool {
	if streamID == "" || presented == "" {
		return false
	}
	ticketMu.Lock()
	defer ticketMu.Unlock()

	// Sweep spent entries on every redemption so an abandoned go-live click
	// does not leave a credential sitting in memory.
	for id, t := range tickets {
		if now.After(t.expires) {
			delete(tickets, id)
		}
	}

	t, ok := tickets[streamID]
	if !ok {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(t.value), []byte(presented)) != 1 {
		return false
	}
	if t.redeemed.IsZero() {
		// First redemption: the ticket now lives exactly one handshake longer,
		// whatever was left of its minute.
		t.redeemed = now
		t.expires = now.Add(ticketHandshake)
		tickets[streamID] = t
	}
	return true
}

// newTicketValue draws a ticket from the same entropy source as an ingest key:
// 32 bytes of crypto/rand, never derived from the stream id or the user id.
func newTicketValue() (string, error) {
	return dbpkg.NewIngestKey()
}
