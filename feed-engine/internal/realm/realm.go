// Package realm implements the F33D3R MMORPG-style progression system.
//
// Realm levels gate content access, posting privileges, and monetisation.
// An account's standing has two sources — what it has EARNED from XP, and the
// FLOOR it has been placed on by a grant or by the founder role — and
// Effective is the one function that combines them. See "Granted standing".
//
// XP thresholds (cumulative):
//
//	Realm 1 (Wanderer)  —      0 XP  — everyone starts here
//	Realm 2 (Initiate)  —    500 XP
//	Realm 3 (Seeker)    —  2 000 XP
//	Realm 4 (Adept)     —  7 500 XP
//	Realm 5 (Guardian)  — 20 000 XP
//
// AethyrRank trust weight: realm / 5.0  (0.20 – 1.00 range)
package realm

import (
	"database/sql"
	"fmt"
	"log"

	"github.com/f33d3r/feed-engine/internal/model"
)

// XP awards per social action.
const (
	XPOnboard    = 50
	XPPost       = 100
	XPReply      = 30
	XPLike       = 10
	XPFollow     = 20
	XPSave       = 15
	XPTrackPlay  = 5
	XPTrackLike  = 10
	XPDailyLogin = 25
)

// Realm thresholds (cumulative XP required).
var thresholds = []int64{0, 500, 2000, 7500, 20000}

// ComputeRealm returns the Realm level for a given cumulative XP total. It is
// the only place the thresholds are applied.
func ComputeRealm(xp int64) int {
	level := Min
	for i, t := range thresholds {
		if xp >= t {
			level = i + 1
		} else {
			break
		}
	}
	return Clamp(level)
}

// ── Granted standing ─────────────────────────────────────────────────────────
//
// Realm used to be a pure function of XP, and AwardXP overwrote users.realm
// with that function's answer on every single award. A realm placed by hand
// therefore survived exactly until the account's next like — @miiyazuko sat at
// R5 on 125 XP and @tehanibentley at R3 on 195 XP, and both were one follow
// away from silently falling back to R1 with their rings.
//
// A grant is a FLOOR, never an override. The reasons it is a floor:
//
//   - A floor cannot be erased by ordinary activity, which is the entire bug.
//     An override could not either, but it would also have to erase everything
//     the account went on to EARN, which is worse.
//   - Earning past a grant must carry the account past it. A partner placed at
//     R3 who then earns R4 is an R4 — a grant is a starting position, not a
//     ceiling, and nothing about being placed should cap what you can reach.
//   - A grant must never demote. Demotion is not a standing question, it is an
//     enforcement question, and enforcement already has its own lane
//     (capabilities, enforcement_state, ban). A realm control that could take
//     standing away would be a second, unaudited punishment surface.
//   - max() is commutative and idempotent, so the XP writer and the grant
//     writer can never fight: whichever lands last, the answer is the same.
//     An override would have to remember which authority wrote last, and that
//     is state that would have to be kept true forever.
//
// To lower a granted account you clear the grant (GrantNone) and it returns to
// exactly what its XP has earned. To lower an EARNED realm you remove XP. There
// is no third path, and no way to write users.realm by hand at all.

// GrantNone is the absence of a grant. It is the default the column carries and
// the value that clears one.
const GrantNone = 0

// FounderFloor is the standing the founder role carries on its own.
//
// Founder is a ROLE — it draws the Founder badge, it opens the destructive
// admin surfaces, and it is unique to one account. It is deliberately NOT a
// realm of its own, and the scale is NOT extended past Max for it: a sixth
// realm would need a sixth ring, a sixth name and a sixth threshold, and would
// leave the platform with two competing answers to "how far has this account
// come". Instead the role carries a permanent floor at the top of the existing
// scale. The founder therefore always wears the Guardian ring, automatically,
// with no grant to remember and nothing an award or a migration can erase.
const FounderFloor = Max

// Floor returns the standing an account holds before it has earned anything —
// the higher of its grant and whatever its role carries. An out-of-scale grant
// degrades through Clamp like every other untrusted realm value, which lands it
// on Default and makes it indistinguishable from no grant at all.
func Floor(grant int, role string) int {
	floor := Default
	if role == model.RoleFounder && FounderFloor > floor {
		floor = FounderFloor
	}
	if g := Clamp(grant); g > floor {
		floor = g
	}
	return floor
}

// Effective is THE definition of the realm an account wears. Earned standing
// and granted standing are combined here and nowhere else: no handler, query or
// template may take a max() of its own.
func Effective(xp int64, grant int, role string) int {
	earned := ComputeRealm(xp)
	if floor := Floor(grant, role); floor > earned {
		return floor
	}
	return earned
}

// IsGranted reports whether an account's standing is currently being held up by
// its floor rather than by its XP — i.e. whether clearing the grant would drop
// it. It is what the admin surface reads to say "granted" instead of "earned".
func IsGranted(xp int64, grant int, role string) bool {
	return Floor(grant, role) > ComputeRealm(xp)
}

// RealmName returns the display name for a Realm level. An out-of-scale level
// degrades through Clamp like every other realm read.
func RealmName(r int) string {
	switch Clamp(r) {
	case 2:
		return "Initiate"
	case 3:
		return "Seeker"
	case 4:
		return "Adept"
	case 5:
		return "Guardian"
	default:
		return "Wanderer"
	}
}

// NextThreshold returns the cumulative XP that carries an account out of realm
// r. At Max there is nothing further to earn, so the top threshold is returned.
func NextThreshold(r int) int64 {
	r = Clamp(r)
	if r >= Max {
		return thresholds[Max-1]
	}
	return thresholds[r]
}

// Progress returns how far, as a whole percentage, an account with xp has
// travelled through realm r towards the next one. Max is always 100.
func Progress(xp int64, r int) int {
	r = Clamp(r)
	if r >= Max {
		return 100
	}
	cur, next := thresholds[r-1], thresholds[r]
	if next <= cur {
		return 100
	}
	pct := int(float64(xp-cur) / float64(next-cur) * 100)
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}

// AwardXP adds xpDelta XP to the user, restates their Realm through Effective,
// writes an xp_events audit row, and tells the realm Index what the account now
// wears. It is the ONE XP write path — db.AwardXP delegates here — so the XP
// thresholds are applied by ComputeRealm and by nothing else, and so no caller
// can move a realm without the rings that draw it finding out.
//
// The grant and the role are read back in the same statement that moves the XP,
// so an award can never overwrite a floor: it recomputes the earned half and
// re-applies the granted half in one place.
//
// Safe to call in a goroutine (fire-and-forget). A negative delta is allowed and
// clamps XP at zero; a zero delta is a no-op.
func AwardXP(db *sql.DB, userID, eventType, contentID string, xpDelta int) {
	if db == nil || userID == "" || xpDelta == 0 {
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("[realm] begin tx: %v", err)
		return
	}

	// Record the event. xp_events.content_id is NOT NULL DEFAULT '' — an award
	// with no content behind it (a daily login, an onboard, a follow) writes the
	// empty string, which is what "no content" means in that column.
	//
	// This used to bind an explicit NULL for the no-content case. An explicit
	// NULL does not fall back to a column default, so the insert violated the
	// NOT NULL constraint, the whole transaction rolled back, and the award
	// vanished: no xp_events row, no XP, no realm. Every call through
	// db.AwardXP — which always passes an empty content id — silently did
	// nothing, which is why the platform had 8 xp_events rows in total.
	if _, err := tx.Exec(`
		INSERT INTO xp_events (user_id, reason, xp_delta, content_id)
		VALUES ($1, $2, $3, $4)
	`, userID, eventType, xpDelta, contentID); err != nil {
		tx.Rollback()
		log.Printf("[realm] xp_events insert: %v", err)
		return
	}

	// Increment XP and restate Realm atomically. XP never goes below zero.
	var newXP int64
	var grant int
	var role string
	if err := tx.QueryRow(`
		UPDATE users SET xp = GREATEST(0, COALESCE(xp, 0) + $1) WHERE id = $2
		RETURNING xp, COALESCE(realm_grant, 0), COALESCE(role, '')
	`, xpDelta, userID).Scan(&newXP, &grant, &role); err != nil {
		tx.Rollback()
		if err != sql.ErrNoRows {
			log.Printf("[realm] xp update: %v", err)
		}
		return
	}

	newRealm := Effective(newXP, grant, role)
	if _, err := tx.Exec(`UPDATE users SET realm = $1 WHERE id = $2`, newRealm, userID); err != nil {
		tx.Rollback()
		log.Printf("[realm] realm update: %v", err)
		return
	}

	if err := tx.Commit(); err != nil {
		log.Printf("[realm] commit: %v", err)
		return
	}

	// The rings drawn from this account's handle are now stale by one write.
	// Correcting them here, on the write path, is what keeps every render path
	// free of a query.
	NoteUser(db, userID)
}

// GetXPState returns the current XP, Realm level, and progress to next Realm
// for a user. The stored realm goes through Clamp before it is used to index the
// threshold table, so a NULL, zero or out-of-scale column degrades to Default
// instead of panicking on a negative index.
func GetXPState(db *sql.DB, userID string) (xp int64, realmLevel int, nextThreshold int64, pct float64) {
	if db == nil {
		return 0, Default, NextThreshold(Default), 0
	}
	var stored sql.NullInt64
	if err := db.QueryRow(`SELECT COALESCE(xp, 0), realm FROM users WHERE id = $1`, userID).Scan(&xp, &stored); err != nil {
		return 0, Default, NextThreshold(Default), 0
	}
	realmLevel = Clamp(int(stored.Int64))
	return xp, realmLevel, NextThreshold(realmLevel), float64(Progress(xp, realmLevel)) / 100
}

// SetGrant places an account on a granted realm floor, or clears one with
// GrantNone, and restates the standing it now wears. It returns the effective
// realm the account holds after the write.
//
// It is the ONE way users.realm_grant moves, and — with AwardXP — one of only
// two statements in the codebase that write users.realm at all. Both derive
// that column from Effective, so the column is never anything but derived.
//
// The write is one transaction: the grant and the standing it implies land
// together or not at all, so no crash can leave a floor recorded that the ring
// does not draw. NoteUser then republishes the account to the index, which is
// what makes the ring change on the very next render rather than at the next
// reconcile.
//
// It does NOT write the audit trail. Provenance belongs to the caller that has
// the acting admin in hand — see handler.adminSetRealmGrant, which appends the
// grant to the PIAL event ledger exactly as the capability surface does.
func SetGrant(database *sql.DB, userID string, level int) (int, error) {
	if database == nil {
		return Default, fmt.Errorf("realm: no database")
	}
	if userID == "" {
		return Default, fmt.Errorf("realm: no account")
	}
	// A grant is either absent or a real realm. Anything else is refused at the
	// door rather than degraded quietly by Clamp, because a grant is typed in by
	// a human and a silently ignored one is a privilege change that appears to
	// have happened and did not.
	if level != GrantNone && Clamp(level) != level {
		return Default, fmt.Errorf("realm: grant %d is outside the scale R%d–R%d", level, Min, Max)
	}

	tx, err := database.Begin()
	if err != nil {
		return Default, err
	}

	var xp int64
	var role string
	if err := tx.QueryRow(`
		UPDATE users SET realm_grant = $1 WHERE id = $2
		RETURNING COALESCE(xp, 0), COALESCE(role, '')
	`, level, userID).Scan(&xp, &role); err != nil {
		tx.Rollback()
		if err == sql.ErrNoRows {
			return Default, fmt.Errorf("realm: no such account")
		}
		return Default, err
	}

	effective := Effective(xp, level, role)
	if _, err := tx.Exec(`UPDATE users SET realm = $1 WHERE id = $2`, effective, userID); err != nil {
		tx.Rollback()
		return Default, err
	}
	if err := tx.Commit(); err != nil {
		return Default, err
	}

	NoteUser(database, userID)
	return effective, nil
}

// Restate re-derives an account's standing from its XP, its grant and its role
// and writes the answer to users.realm. It exists for the paths that change one
// of those three inputs without touching XP — today that is the role change,
// because promoting an account to founder must raise it to FounderFloor and
// demoting it must let it fall back to what it has actually earned.
//
// It is a read-then-write inside one transaction, with the row locked, so it
// cannot interleave with a concurrent award and lose one of the two changes.
func Restate(database *sql.DB, userID string) (int, error) {
	if database == nil || userID == "" {
		return Default, fmt.Errorf("realm: no account")
	}

	tx, err := database.Begin()
	if err != nil {
		return Default, err
	}

	var xp int64
	var grant int
	var role string
	if err := tx.QueryRow(`
		SELECT COALESCE(xp, 0), COALESCE(realm_grant, 0), COALESCE(role, '')
		FROM users WHERE id = $1 FOR UPDATE
	`, userID).Scan(&xp, &grant, &role); err != nil {
		tx.Rollback()
		if err == sql.ErrNoRows {
			return Default, fmt.Errorf("realm: no such account")
		}
		return Default, err
	}

	effective := Effective(xp, grant, role)
	if _, err := tx.Exec(`UPDATE users SET realm = $1 WHERE id = $2`, effective, userID); err != nil {
		tx.Rollback()
		return Default, err
	}
	if err := tx.Commit(); err != nil {
		return Default, err
	}

	NoteUser(database, userID)
	return effective, nil
}
