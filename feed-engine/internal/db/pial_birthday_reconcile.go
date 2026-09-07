package db

import (
	"database/sql"
	"log"
	"time"
)

// Reconciling the dates of birth that never reached the identity plane.
//
// Onboarding records a date of birth twice: user_roles.date_of_birth is the
// audit trail of what the person supplied, and pial_roots is the identity plane
// every age gate on the platform reads. For as long as SetPIALBirthday's
// statement could not be parsed by Postgres — a bare parameter reaching age(),
// see setPIALBirthdaySQL — the second write failed on every signup while the
// first succeeded. The result was accounts whose supplied date of birth was on
// record but whose is_adult was FALSE, so every gate treated a verified adult as
// a non-adult and every minor as neither.
//
// This is the repair, and it is deliberately not a one-shot migration. The same
// condition is produced by any transient failure of the identity write at
// signup, so the repair has to be something that runs again — a migration fixes
// yesterday's rows and nothing else.
//
// Two rules govern it, and neither is negotiable on an adult platform:
//
//   - It only ever copies a date of birth the person actually supplied. It never
//     derives, defaults, guesses or infers one. An account with no recorded date
//     of birth anywhere is reported and left exactly as it is.
//   - It writes through SetPIALBirthday, the single writer of PIAL age facts, so
//     the kyc_tier transition and the state hash stay consistent with every other
//     path. A bulk UPDATE here would be a second writer and would skip both.
//
// A PIAL whose dob_locked is TRUE has a documented age verification and is never
// touched: that record outranks anything user_roles remembers.

const (
	// pialBirthdayReconcileInterval is how often the backstop runs after its
	// first pass. Long, because a pass over repaired data is pure reads.
	pialBirthdayReconcileInterval = 6 * time.Hour

	// pialBirthdayReconcileBatch bounds a single pass so a large backlog cannot
	// hold a connection for an unbounded time.
	pialBirthdayReconcileBatch = 500
)

// birthdayGap is one account whose identity plane is missing a date of birth
// that this brain can still account for.
type birthdayGap struct {
	handle string
	userID string
	pialID string
	dob    time.Time
}

// PIALBirthdayReconcileResult reports what one pass did and, just as importantly,
// what it refused to do.
type PIALBirthdayReconcileResult struct {
	Repaired      int // identity plane completed from the recorded date of birth
	Failed        int // a repair was attempted and the write failed
	Unrecoverable int // no date of birth on record anywhere — never invented
}

// ReconcilePIALBirthdays completes the identity plane for accounts whose date of
// birth was captured at signup but never landed on the PIAL root. It returns
// what it repaired and what it could not.
//
// Idempotent: once every account's age facts are on the PIAL root, a pass reads
// and changes nothing.
func ReconcilePIALBirthdays(database *sql.DB) (PIALBirthdayReconcileResult, error) {
	var res PIALBirthdayReconcileResult
	if database == nil {
		return res, nil
	}

	// Accounts whose PIAL has no date of birth and is not locked by a documented
	// verification. user_roles.date_of_birth is LEFT JOINed rather than required,
	// so the accounts nothing can be done for are counted here instead of being
	// filtered out of sight.
	rows, err := database.Query(`
		SELECT u.handle, u.id::text, u.pial_id::text, ur.date_of_birth
		FROM users u
		JOIN pial_roots pr ON pr.pial_id = u.pial_id
		LEFT JOIN user_roles ur ON ur.user_id = u.id
		WHERE pr.date_of_birth IS NULL
		  AND pr.dob_locked = FALSE
		ORDER BY u.created_at
		LIMIT $1`, pialBirthdayReconcileBatch)
	if err != nil {
		return res, err
	}

	gaps := make([]birthdayGap, 0, 16)
	orphans := make([]string, 0, 4)
	for rows.Next() {
		var g birthdayGap
		var dob sql.NullTime
		if serr := rows.Scan(&g.handle, &g.userID, &g.pialID, &dob); serr != nil {
			rows.Close()
			return res, serr
		}
		if !dob.Valid {
			// No date of birth on record anywhere. There is nothing honest to do
			// with this account here: an age is a fact about a person, not a value
			// a background job is entitled to choose.
			orphans = append(orphans, g.handle)
			continue
		}
		g.dob = dob.Time
		gaps = append(gaps, g)
	}
	if cerr := rows.Err(); cerr != nil {
		rows.Close()
		return res, cerr
	}
	rows.Close()

	res.Unrecoverable = len(orphans)
	if res.Unrecoverable > 0 {
		log.Printf("[pial] %d account(s) have no date of birth on record and cannot be reconciled "+
			"— they remain non-adult to every age gate until one is supplied: %v",
			res.Unrecoverable, orphans)
	}

	for _, g := range gaps {
		if werr := SetPIALBirthday(database, g.pialID, g.dob); werr != nil {
			res.Failed++
			log.Printf("[pial] reconcile: could not record date of birth for %s (account %s): %v",
				g.handle, g.userID, werr)
			continue
		}
		res.Repaired++
		LogPIALEvent(database, g.pialID, g.userID, "age_reconciled", map[string]interface{}{
			"handle": g.handle,
			"source": "user_roles.date_of_birth",
		}, "reconcile")
		log.Printf("[pial] reconcile: recorded date of birth for %s from the signup audit trail", g.handle)
	}
	return res, nil
}

// StartPIALBirthdayReconcile runs the repair at boot and then on a long interval,
// so an identity write that fails at signup is completed rather than remembered
// only in a log line. Never returns.
func StartPIALBirthdayReconcile(database *sql.DB) {
	if database == nil {
		return
	}
	for {
		res, err := ReconcilePIALBirthdays(database)
		switch {
		case err != nil:
			log.Printf("[pial] birthday reconcile pass failed: %v", err)
		case res.Repaired > 0 || res.Failed > 0:
			log.Printf("[pial] birthday reconcile: %d repaired, %d failed, %d unrecoverable",
				res.Repaired, res.Failed, res.Unrecoverable)
		}
		time.Sleep(pialBirthdayReconcileInterval)
	}
}
