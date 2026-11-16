package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/f33d3r/feed-engine/internal/identity"
)

// identityRelayLockKey serialises the identity command relay across replicas.
// Arbitrary but fixed, and distinct from every other advisory key in use.
const identityRelayLockKey int64 = 7331_0007

// EnqueueIdentityCommand records an identity write in the caller's transaction.
// It is delivered to elohim-veni after commit by identity.Relay.
func EnqueueIdentityCommand(tx DBTX, kind, pialID string, payload interface{}) error {
	if pialID == "" {
		return fmt.Errorf("identity command %s: no PIAL", kind)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("identity command %s: encoding: %w", kind, err)
	}
	if _, err := tx.Exec(
		`INSERT INTO identity_commands (kind, pial_id, payload) VALUES ($1, $2::uuid, $3::jsonb)`,
		kind, pialID, string(body)); err != nil {
		return fmt.Errorf("identity command %s for %s: %w", kind, pialID, err)
	}
	return nil
}

// IdentityCommandStore is the relay's view of identity_commands.
type IdentityCommandStore struct {
	DB *sql.DB
}

var _ identity.CommandStore = IdentityCommandStore{}

// WithRelayLock holds a session-level advisory lock on one dedicated
// connection for the duration of fn.
func (s IdentityCommandStore) WithRelayLock(ctx context.Context, fn func(context.Context) error) (bool, error) {
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return false, fmt.Errorf("relay lock connection: %w", err)
	}
	defer conn.Close()
	var held bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, identityRelayLockKey).Scan(&held); err != nil {
		return false, fmt.Errorf("relay lock: %w", err)
	}
	if !held {
		return false, nil
	}
	defer func() {
		// Released on this same connection, with a context the caller's
		// cancellation cannot pre-empt; a lock left held would stop the relay
		// on every replica until the connection died.
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(unlockCtx, `SELECT pg_advisory_unlock($1)`, identityRelayLockKey); err != nil {
			log.Printf("[identity-relay] releasing relay lock: %v", err)
		}
	}()
	return true, fn(ctx)
}

// Pending returns the oldest undelivered commands, in order — and none at all
// while the oldest is waiting out a retry delay, so nothing overtakes it.
func (s IdentityCommandStore) Pending(ctx context.Context, limit int) ([]identity.Command, error) {
	rows, err := s.DB.QueryContext(ctx, `
		WITH queue AS (
		    SELECT id, kind, pial_id::text AS pial_id, payload, attempts, next_attempt
		      FROM identity_commands
		     WHERE delivered_at IS NULL AND refused_at IS NULL
		     ORDER BY id
		     LIMIT $1)
		SELECT id, kind, pial_id, payload, attempts FROM queue
		 WHERE (SELECT next_attempt FROM queue ORDER BY id LIMIT 1) <= NOW()
		 ORDER BY id`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []identity.Command
	for rows.Next() {
		var c identity.Command
		var payload []byte
		if err := rows.Scan(&c.ID, &c.Kind, &c.PIALID, &payload, &c.Attempts); err != nil {
			return nil, err
		}
		c.Payload = payload
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s IdentityCommandStore) Delivered(ctx context.Context, id int64) error {
	return s.mark(ctx, `UPDATE identity_commands SET delivered_at = NOW(), last_error = '' WHERE id = $1`, id)
}

func (s IdentityCommandStore) Deferred(ctx context.Context, id int64, reason string, next time.Time) error {
	return s.mark(ctx, `UPDATE identity_commands
		SET attempts = attempts + 1, last_error = $2, next_attempt = $3 WHERE id = $1`, id, reason, next)
}

func (s IdentityCommandStore) Refused(ctx context.Context, id int64, reason string) error {
	return s.mark(ctx, `UPDATE identity_commands
		SET attempts = attempts + 1, last_error = $2, refused_at = NOW() WHERE id = $1`, id, reason)
}

func (s IdentityCommandStore) mark(ctx context.Context, stmt string, args ...interface{}) error {
	res, err := s.DB.ExecContext(ctx, stmt, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("identity command %v: marked %d rows, wanted 1", args[0], n)
	}
	return nil
}

// RecordIdentityDeadLetter keeps an identity.events record that can never be
// applied, with the reason, so the consumer can move past it without the
// record being lost. A record already kept is not an error.
func RecordIdentityDeadLetter(ctx context.Context, database *sql.DB, topic string, partition int32, offset int64, record []byte, reason string) error {
	_, err := database.ExecContext(ctx, `
		INSERT INTO identity_event_deadletter (topic, partition, "offset", record, reason)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (topic, partition, "offset") DO NOTHING`,
		topic, partition, offset, record, reason)
	return err
}
