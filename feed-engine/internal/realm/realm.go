// Package realm implements the F33D3R MMORPG-style progression system.
//
// Realm levels gate content access, posting privileges, and monetisation.
// XP accumulates from social actions and is the sole determinant of Realm.
//
// XP thresholds (cumulative):
//   Realm 1 (Wanderer)  —      0 XP  — everyone starts here
//   Realm 2 (Initiate)  —    500 XP
//   Realm 3 (Seeker)    —  2 000 XP
//   Realm 4 (Adept)     —  7 500 XP
//   Realm 5 (Guardian)  — 20 000 XP
//
// AethyrRank trust weight: realm / 5.0  (0.20 – 1.00 range)
package realm

import (
	"database/sql"
	"log"
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

// ComputeRealm returns the Realm level (1–5) for a given cumulative XP total.
func ComputeRealm(xp int64) int {
	level := 1
	for i, t := range thresholds {
		if xp >= t {
			level = i + 1
		} else {
			break
		}
	}
	return level
}

// RealmName returns the display name for a Realm level.
func RealmName(r int) string {
	switch r {
	case 1:
		return "Wanderer"
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

// AwardXP adds xpDelta XP to the user, recomputes their Realm, and writes
// an xp_events audit row. Safe to call in a goroutine (fire-and-forget).
func AwardXP(db *sql.DB, userID, eventType, contentID string, xpDelta int) {
	if db == nil || userID == "" || xpDelta <= 0 {
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("[realm] begin tx: %v", err)
		return
	}

	// Record the event.
	if _, err := tx.Exec(`
		INSERT INTO xp_events (user_id, event_type, xp_delta, content_id)
		VALUES ($1, $2, $3, $4)
	`, userID, eventType, xpDelta, contentID); err != nil {
		tx.Rollback()
		log.Printf("[realm] xp_events insert: %v", err)
		return
	}

	// Increment XP and recompute Realm atomically.
	var newXP int64
	if err := tx.QueryRow(`
		UPDATE users SET xp = xp + $1 WHERE id = $2 RETURNING xp
	`, xpDelta, userID).Scan(&newXP); err != nil {
		tx.Rollback()
		log.Printf("[realm] xp update: %v", err)
		return
	}

	newRealm := ComputeRealm(newXP)
	if _, err := tx.Exec(`UPDATE users SET realm = $1 WHERE id = $2`, newRealm, userID); err != nil {
		tx.Rollback()
		log.Printf("[realm] realm update: %v", err)
		return
	}

	if err := tx.Commit(); err != nil {
		log.Printf("[realm] commit: %v", err)
	}
}

// GetXPState returns the current XP, Realm level, and progress to next Realm for a user.
func GetXPState(db *sql.DB, userID string) (xp int64, realmLevel int, nextThreshold int64, pct float64) {
	if db == nil {
		return 0, 1, thresholds[1], 0
	}
	if err := db.QueryRow(`SELECT xp, realm FROM users WHERE id = $1`, userID).Scan(&xp, &realmLevel); err != nil {
		return 0, 1, thresholds[1], 0
	}
	if realmLevel >= 5 {
		return xp, 5, thresholds[4], 1.0
	}
	current := thresholds[realmLevel-1]
	next := thresholds[realmLevel]
	nextThreshold = next
	if next > current {
		pct = float64(xp-current) / float64(next-current)
	}
	return
}
