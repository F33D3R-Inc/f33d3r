package store

import (
	"context"
	"database/sql"
	"errors"
)

// This file stands in for Elohim Veni's signing-key authority. One key per
// PIAL: a registration replaces whatever was there, which is the real
// authority's model too and is why the client re-registers on every launch.

// RegisterSigningKey stores (or replaces) a PIAL's ECDSA-P256 SPKI public key.
func (s *Store) RegisterSigningKey(ctx context.Context, pial, publicKeyB64, algorithm string) error {
	if algorithm == "" {
		algorithm = "ECDSA-P256"
	}
	now := Now()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO pial_signing_keys (pial_id, public_key_b64, algorithm, registered_at, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (pial_id) DO UPDATE SET public_key_b64 = excluded.public_key_b64, algorithm = excluded.algorithm, updated_at = excluded.updated_at`,
		pial, publicKeyB64, algorithm, now, now)
	return err
}

// SigningKey returns the registered SPKI base64, or "" when there is none —
// the same contract as feed-engine's fetchSigningPubkey.
func (s *Store) SigningKey(ctx context.Context, pial string) string {
	var key string
	err := s.db.QueryRowContext(ctx, `SELECT public_key_b64 FROM pial_signing_keys WHERE pial_id = ?`, pial).Scan(&key)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	return key
}
