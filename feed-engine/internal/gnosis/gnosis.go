// Package gnosis is the messaging brain. It runs one of two render paths per
// conversation, decided entirely by participant 18+ verification status (PIAL is
// the source of truth):
//
//   - sealed : end-to-end encrypted. Used IFF every participant is 18+ verified.
//   - plain  : server-readable plaintext, rendered server-side like every other FA
//     surface. Used when ANY participant is not 18+ verified.
//
// A conversation is sealed iff every participant is 18+ verified — encryption needs
// both sides to hold key material, and an unverified account has none, so one
// unverified party forces plaintext. The mode is a property of the conversation,
// fixed at creation by the lowest-capability participant, and never silently
// changed: there is NO mode setter anywhere.
package gnosis

import (
	"database/sql"
	"errors"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

// Conversation modes. mode is persisted on gnosis_conversations with a CHECK
// constraint and is immutable after insert.
const (
	ModePlain  = "plain"
	ModeSealed = "sealed"
)

// ErrCannotDowngradeSealed is returned when an unverified PIAL would be added to a
// sealed conversation. Sealed conversations accept 18+ verified members only; there
// is no silent downgrade to plaintext (that would be a classic downgrade attack).
var ErrCannotDowngradeSealed = errors.New("gnosis: cannot add an unverified member to a sealed conversation")

// verifiedAdult reports whether a PIAL is 18+ verified — the single gate that
// decides whether key material can ever exist for this identity. Reads PIAL, the
// source of truth, via the canonical loader.
func verifiedAdult(database *sql.DB, pialID string) bool {
	ps := dbpkg.LoadPIALState(database, pialID)
	return ps != nil && ps.AgeVerified && ps.IsAdult
}

// modeFor is the pure mode-decision: sealed iff EVERY participant is a verified
// adult, else plain. Kept free of the DB so the invariant is unit-testable with a
// stubbed predicate.
func modeFor(pialIDs []string, isVerifiedAdult func(string) bool) string {
	if len(pialIDs) == 0 {
		return ModePlain
	}
	for _, pid := range pialIDs {
		if !isVerifiedAdult(pid) {
			return ModePlain
		}
	}
	return ModeSealed
}

// guardAddMember enforces no-silent-downgrade: a member may be added to a sealed
// conversation only if they are a verified adult. Pure; unit-testable.
func guardAddMember(mode string, canAdd bool) error {
	if mode == ModeSealed && !canAdd {
		return ErrCannotDowngradeSealed
	}
	return nil
}

// ModeForParticipants returns ModeSealed iff every participant PIAL is 18+ verified
// (age_verified && is_adult); otherwise ModePlain. The lowest-capability participant
// fixes the floor.
func ModeForParticipants(database *sql.DB, pialIDs []string) string {
	return modeFor(pialIDs, func(pid string) bool { return verifiedAdult(database, pid) })
}

// CanAddToSealed reports whether a candidate PIAL may join a sealed conversation.
func CanAddToSealed(database *sql.DB, pialID string) bool {
	return verifiedAdult(database, pialID)
}
