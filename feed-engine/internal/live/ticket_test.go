package live

import (
	"testing"
	"time"
)

// TestPublishTicketSurvivesOneHandshake pins the ticket's lifetime.
//
// The media server may authorise one WebRTC publish more than once while it
// sets the session up. A ticket that died on the first answer made the second
// fall through to the ingest-key check and the publisher was dropped after the
// browser had its SDP answer. So a ticket answers every call within one
// handshake of its first redemption, and none after.
func TestPublishTicketSurvivesOneHandshake(t *testing.T) {
	const stream = "33333333-3333-3333-3333-333333333333"
	value, err := MintPublishTicket(stream)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	t.Cleanup(func() {
		ticketMu.Lock()
		delete(tickets, stream)
		ticketMu.Unlock()
	})

	now := time.Now()
	if !consumePublishTicketAt(stream, value, now) {
		t.Fatal("first redemption refused")
	}
	if !consumePublishTicketAt(stream, value, now.Add(ticketHandshake/2)) {
		t.Error("second redemption inside the handshake refused")
	}
	if consumePublishTicketAt(stream, "not-the-ticket", now.Add(ticketHandshake/2)) {
		t.Error("a wrong value was accepted inside the handshake")
	}
	if consumePublishTicketAt(stream, value, now.Add(ticketHandshake+time.Second)) {
		t.Error("redemption after the handshake window was accepted")
	}
	if consumePublishTicketAt(stream, value, now.Add(2*ticketHandshake)) {
		t.Error("a spent ticket came back")
	}
}

// TestPublishTicketUnredeemedExpires pins the unredeemed lifetime: a go-live
// click that never reached the media server leaves nothing behind.
func TestPublishTicketUnredeemedExpires(t *testing.T) {
	const stream = "44444444-4444-4444-4444-444444444444"
	value, err := MintPublishTicket(stream)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	t.Cleanup(func() {
		ticketMu.Lock()
		delete(tickets, stream)
		ticketMu.Unlock()
	})
	if consumePublishTicketAt(stream, value, time.Now().Add(ticketTTL+time.Second)) {
		t.Error("an unredeemed ticket outlived its minute")
	}
	if consumePublishTicketAt("", value, time.Now()) || consumePublishTicketAt(stream, "", time.Now()) {
		t.Error("an empty stream or ticket was accepted")
	}
}
