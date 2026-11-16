package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"
)

// Identity commands — the transactional outbox for identity writes.
//
// Some identity writes belong to a local change that must not exist without
// them: a CSAM hold is the account action of a law-enforcement report; an org
// verification approval is the identity confirmation behind a badge; a purge
// erases the account here and its identity there. A remote call cannot join a
// database transaction, so the write is recorded as a command in the same
// transaction as the local change, and the relay below delivers it to
// elohim-veni afterwards — in order, retried until it is accepted, and never
// lost. Every command is idempotent at the authority (a PUT of a state, a ban
// of a banned identity), so redelivery after a crash is harmless.
//
// A command the authority refuses outright is not retried: it is marked refused
// with the reason and logged as critical. A refused identity write is a fact
// an operator must see, not one the relay may retry into a stall or drop.

// Command kinds.
const (
	CommandFacts      = "facts"
	CommandCapability = "capability"
	CommandBan        = "ban"
	CommandUnban      = "unban"
	CommandPurge      = "purge"
)

// Command is one queued identity write.
type Command struct {
	ID       int64
	Kind     string
	PIALID   string
	Payload  json.RawMessage
	Attempts int
}

// CapabilityCommand is the payload of a capability command.
type CapabilityCommand struct {
	Capability string          `json:"capability"`
	Write      CapabilityWrite `json:"write"`
}

// StandingCommand is the payload of a ban or unban command.
type StandingCommand struct {
	Reason string `json:"reason"`
	By     string `json:"by"`
}

// PurgeCommand is the payload of a purge command.
type PurgeCommand struct {
	AccountID string `json:"account_id"`
	By        string `json:"by"`
}

// Execute delivers one command to the authority.
func (c *Client) Execute(ctx context.Context, cmd Command) error {
	switch cmd.Kind {
	case CommandFacts:
		var u FactsUpdate
		if err := json.Unmarshal(cmd.Payload, &u); err != nil {
			return fmt.Errorf("command %d: facts payload: %w", cmd.ID, errPermanent)
		}
		return c.PutFacts(ctx, cmd.PIALID, u)
	case CommandCapability:
		var p CapabilityCommand
		if err := json.Unmarshal(cmd.Payload, &p); err != nil || p.Capability == "" {
			return fmt.Errorf("command %d: capability payload: %w", cmd.ID, errPermanent)
		}
		return c.PutCapability(ctx, cmd.PIALID, p.Capability, p.Write)
	case CommandBan, CommandUnban:
		var p StandingCommand
		if err := json.Unmarshal(cmd.Payload, &p); err != nil {
			return fmt.Errorf("command %d: standing payload: %w", cmd.ID, errPermanent)
		}
		if cmd.Kind == CommandBan {
			return c.Ban(ctx, cmd.PIALID, p.Reason, p.By)
		}
		return c.Unban(ctx, cmd.PIALID, p.Reason, p.By)
	case CommandPurge:
		var p PurgeCommand
		if err := json.Unmarshal(cmd.Payload, &p); err != nil || p.AccountID == "" {
			return fmt.Errorf("command %d: purge payload: %w", cmd.ID, errPermanent)
		}
		return c.PurgeAccount(ctx, p.AccountID, p.By)
	default:
		return fmt.Errorf("command %d: unknown kind %q: %w", cmd.ID, cmd.Kind, errPermanent)
	}
}

// errPermanent marks a command that can never be delivered as written.
var errPermanent = errors.New("command can never be delivered")

// CommandStore is where commands wait. db.IdentityCommandStore implements it.
type CommandStore interface {
	// WithRelayLock runs fn while holding the cluster-wide relay lock, and
	// reports false without running fn when another replica holds it.
	WithRelayLock(ctx context.Context, fn func(context.Context) error) (bool, error)
	// Pending returns undelivered, unrefused commands due now, oldest first.
	Pending(ctx context.Context, limit int) ([]Command, error)
	Delivered(ctx context.Context, id int64) error
	Deferred(ctx context.Context, id int64, reason string, next time.Time) error
	Refused(ctx context.Context, id int64, reason string) error
}

const (
	relayInterval = 2 * time.Second
	relayBatch    = 100
	relayMaxDelay = 5 * time.Minute
)

// Relay drains the command store to the authority.
type Relay struct {
	store  CommandStore
	client *Client
	kick   chan struct{}
}

// NewRelay returns a relay. Start it with Run.
func NewRelay(store CommandStore, client *Client) *Relay {
	return &Relay{store: store, client: client, kick: make(chan struct{}, 1)}
}

// Kick asks the relay to drain now rather than at its next tick. Callers kick
// after committing a transaction that enqueued a command, so an admin action
// reaches the authority in milliseconds, not seconds.
func (r *Relay) Kick() {
	if r == nil {
		return
	}
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// Run drains until ctx ends.
func (r *Relay) Run(ctx context.Context) {
	ticker := time.NewTicker(relayInterval)
	defer ticker.Stop()
	log.Printf("[identity-relay] started")
	for {
		select {
		case <-ctx.Done():
			log.Printf("[identity-relay] stopped")
			return
		case <-ticker.C:
		case <-r.kick:
		}
		if _, err := r.store.WithRelayLock(ctx, r.drain); err != nil && ctx.Err() == nil {
			log.Printf("[identity-relay] %v", err)
		}
	}
}

// drain delivers due commands in order. The first command the authority could
// not be reached for ends the batch, so a later command about the same
// identity never overtakes an earlier one.
func (r *Relay) drain(ctx context.Context) error {
	cmds, err := r.store.Pending(ctx, relayBatch)
	if err != nil {
		return fmt.Errorf("reading pending commands: %w", err)
	}
	for _, cmd := range cmds {
		err := r.client.Execute(ctx, cmd)
		switch {
		case err == nil:
			if err := r.store.Delivered(ctx, cmd.ID); err != nil {
				return fmt.Errorf("command %d delivered but not marked: %w", cmd.ID, err)
			}
		case errors.Is(err, errPermanent) || Refusal(err) != nil:
			log.Printf("[identity-relay] CRITICAL: elohim-veni refused %s command %d for PIAL %s: %v — "+
				"held as refused in identity_commands for an operator", cmd.Kind, cmd.ID, cmd.PIALID, err)
			if err := r.store.Refused(ctx, cmd.ID, err.Error()); err != nil {
				return fmt.Errorf("command %d refused but not marked: %w", cmd.ID, err)
			}
		default:
			delay := time.Duration(1<<min(cmd.Attempts, 8)) * time.Second
			if delay > relayMaxDelay {
				delay = relayMaxDelay
			}
			if err := r.store.Deferred(ctx, cmd.ID, err.Error(), time.Now().Add(delay)); err != nil {
				return fmt.Errorf("command %d failed and could not be deferred: %w", cmd.ID, err)
			}
			return fmt.Errorf("command %d (%s, PIAL %s) deferred %s: %w", cmd.ID, cmd.Kind, cmd.PIALID, delay, err)
		}
	}
	return nil
}
