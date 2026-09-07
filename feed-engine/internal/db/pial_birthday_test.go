package db

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// ─── pinned without a database ───────────────────────────────────────────────

// TestSetPIALBirthdayCastsEveryParameter pins the reason this statement works.
//
// lib/pq lets the server infer parameter types, so a bare $1 handed to age()
// arrives as `unknown` and Postgres refuses the whole statement:
// "function age(unknown) is not unique". It fails at parse time, so nothing is
// written and no row count reveals it — the only signal is the error, and for as
// long as onboarding discarded that error every account was created with a NULL
// date of birth and is_adult FALSE.
//
// This test costs nothing and runs everywhere. The schema-backed test below
// proves the statement does the right thing; this one proves the property that
// makes it runnable at all is still present after someone tidies the SQL.
func TestSetPIALBirthdayCastsEveryParameter(t *testing.T) {
	if strings.Contains(setPIALBirthdaySQL, "age($1)") {
		t.Fatal("setPIALBirthdaySQL passes an uncast $1 to age(). Postgres cannot resolve " +
			"age(unknown) against age(timestamp)/age(timestamptz) and rejects the statement at " +
			"parse time, so no date of birth is ever recorded. Write age($1::date).")
	}
	for _, want := range []string{"$1::date", "$2::uuid"} {
		if !strings.Contains(setPIALBirthdaySQL, want) {
			t.Errorf("setPIALBirthdaySQL no longer contains %s — every parameter in this "+
				"statement carries an explicit cast on purpose", want)
		}
	}
}

// ─── schema-backed ───────────────────────────────────────────────────────────
//
// These need a throwaway PostgreSQL named by PIAL_TEST_DATABASE_URL. They apply
// the real embedded migrations. Never point this at a database that holds real
// rows: they write and delete identity records.

var pialTestSeq atomic.Int64

func pialTestDB(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("PIAL_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PIAL_TEST_DATABASE_URL not set — schema-backed PIAL birthday tests skipped")
	}
	database, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := database.Ping(); err != nil {
		database.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("closing test database: %v", err)
		}
	})
	applyMigrationsForTest(t, database)
	return database
}

// seedPIALAccount creates a users + user_profiles + pial_roots triple, as every
// onboarded account has, and removes it when the test ends.
func seedPIALAccount(t *testing.T, database *sql.DB) (userID, pialID, handle string) {
	t.Helper()
	handle = fmt.Sprintf("pialtest_%d_%d", time.Now().UnixNano(), pialTestSeq.Add(1))

	if err := database.QueryRow(
		`INSERT INTO pial_roots DEFAULT VALUES RETURNING pial_id::text`).Scan(&pialID); err != nil {
		t.Fatalf("seeding pial_roots: %v", err)
	}
	if err := database.QueryRow(
		`INSERT INTO users (handle, pial_id) VALUES ($1, $2::uuid) RETURNING id::text`,
		handle, pialID).Scan(&userID); err != nil {
		t.Fatalf("seeding users: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO user_profiles (user_id, display_name) VALUES ($1::uuid, $2)`,
		userID, handle); err != nil {
		t.Fatalf("seeding user_profiles: %v", err)
	}
	t.Cleanup(func() {
		database.Exec(`DELETE FROM user_roles WHERE user_id = $1::uuid`, userID)
		database.Exec(`DELETE FROM user_profiles WHERE user_id = $1::uuid`, userID)
		database.Exec(`DELETE FROM pial_event_ledger WHERE pial_id = $1::uuid`, pialID)
		database.Exec(`DELETE FROM trust_scores WHERE pial_id = $1::uuid`, pialID)
		database.Exec(`DELETE FROM pial_capabilities WHERE pial_id = $1::uuid`, pialID)
		database.Exec(`DELETE FROM pial_account_bindings WHERE pial_id = $1::uuid`, pialID)
		database.Exec(`DELETE FROM users WHERE id = $1::uuid`, userID)
		database.Exec(`DELETE FROM pial_roots WHERE pial_id = $1::uuid`, pialID)
	})
	return userID, pialID, handle
}

type ageFacts struct {
	dob         sql.NullString
	isAdult     bool
	isMinor     bool
	ageVerified bool
	kycTier     string
}

func readAgeFacts(t *testing.T, database *sql.DB, pialID string) ageFacts {
	t.Helper()
	var f ageFacts
	if err := database.QueryRow(`
		SELECT to_char(date_of_birth,'YYYY-MM-DD'), is_adult, is_minor, age_verified, kyc_tier
		FROM pial_roots WHERE pial_id = $1::uuid`, pialID,
	).Scan(&f.dob, &f.isAdult, &f.isMinor, &f.ageVerified, &f.kycTier); err != nil {
		t.Fatalf("reading age facts: %v", err)
	}
	return f
}

// TestSetPIALBirthdayRecordsTheAgeFacts is the regression test for the bug that
// left every onboarded account non-adult: the statement must actually run, and
// the day supplied must be the day stored.
func TestSetPIALBirthdayRecordsTheAgeFacts(t *testing.T) {
	database := pialTestDB(t)

	cases := []struct {
		name      string
		dob       string
		wantAdult bool
		wantMinor bool
	}{
		{"an adult", "1990-04-11", true, false},
		{"a minor", time.Now().UTC().AddDate(-17, 0, 0).Format("2006-01-02"), false, true},
		{"the day before turning 18", time.Now().UTC().AddDate(-18, 0, 1).Format("2006-01-02"), false, true},
		{"the day of turning 18", time.Now().UTC().AddDate(-18, 0, 0).Format("2006-01-02"), true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, pialID, _ := seedPIALAccount(t, database)
			dob, err := time.Parse("2006-01-02", tc.dob)
			if err != nil {
				t.Fatalf("bad case date: %v", err)
			}
			if err := SetPIALBirthday(database, pialID, dob); err != nil {
				t.Fatalf("SetPIALBirthday(%s): %v", tc.dob, err)
			}
			got := readAgeFacts(t, database, pialID)
			if !got.dob.Valid || got.dob.String != tc.dob {
				t.Errorf("date_of_birth = %q, want %q", got.dob.String, tc.dob)
			}
			if got.isAdult != tc.wantAdult {
				t.Errorf("is_adult = %v, want %v", got.isAdult, tc.wantAdult)
			}
			if got.isMinor != tc.wantMinor {
				t.Errorf("is_minor = %v, want %v", got.isMinor, tc.wantMinor)
			}
			if !got.ageVerified {
				t.Error("age_verified is false after a date of birth was recorded")
			}
			if got.kycTier != "basic" {
				t.Errorf("kyc_tier = %q, want %q (self-reported)", got.kycTier, "basic")
			}
		})
	}
}

// TestSetPIALBirthdayRefusesALockedDOB pins the one case where refusing is
// correct: a documented age verification outranks anything a form supplies.
func TestSetPIALBirthdayRefusesALockedDOB(t *testing.T) {
	database := pialTestDB(t)
	_, pialID, _ := seedPIALAccount(t, database)

	if err := SetPIALBirthday(database, pialID, time.Date(1990, 4, 11, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE pial_roots SET dob_locked = TRUE WHERE pial_id = $1::uuid`, pialID); err != nil {
		t.Fatalf("locking: %v", err)
	}
	err := SetPIALBirthday(database, pialID, time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("SetPIALBirthday rewrote a locked date of birth")
	}
	if !strings.Contains(err.Error(), "locked") {
		t.Errorf("error = %q, want it to say the date of birth is locked", err)
	}
	if got := readAgeFacts(t, database, pialID); got.dob.String != "1990-04-11" {
		t.Errorf("date_of_birth = %q after a refused write, want it unchanged", got.dob.String)
	}
}

// TestSetPIALBirthdayReportsAMissingRoot proves the caller can tell a locked DOB
// apart from a PIAL that is not there — both produce zero rows, and onboarding
// has to record which one happened.
func TestSetPIALBirthdayReportsAMissingRoot(t *testing.T) {
	database := pialTestDB(t)
	err := SetPIALBirthday(database, "00000000-0000-0000-0000-000000000000",
		time.Date(1990, 4, 11, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("SetPIALBirthday reported success for a PIAL that does not exist")
	}
	if !strings.Contains(err.Error(), "no such PIAL root") {
		t.Errorf("error = %q, want it to name a missing root", err)
	}
}

// TestSetPIALBirthdayRejectsAnEmptyPIAL pins the guard on the call site that
// actually broke: onboarding used to skip the write entirely when the PIAL id
// was empty, which looked like success and recorded nothing.
func TestSetPIALBirthdayRejectsAnEmptyPIAL(t *testing.T) {
	if err := SetPIALBirthday(nil, "", time.Now()); err == nil {
		t.Fatal("SetPIALBirthday accepted an empty PIAL id")
	}
}

// TestReconcilePIALBirthdaysCompletesFromTheAuditTrail proves the backstop
// repairs an account whose date of birth was captured at signup but never
// reached the identity plane — and, just as importantly, that it invents nothing
// for an account that has no date of birth on record at all.
func TestReconcilePIALBirthdaysCompletesFromTheAuditTrail(t *testing.T) {
	database := pialTestDB(t)

	// Broken exactly the way the bug left them: user_roles has the date, PIAL does not.
	repairable, repairablePIAL, _ := seedPIALAccount(t, database)
	if _, err := database.Exec(
		`INSERT INTO user_roles (user_id, date_of_birth) VALUES ($1::uuid, $2::date)`,
		repairable, "1990-04-11"); err != nil {
		t.Fatalf("seeding user_roles: %v", err)
	}

	// No date of birth anywhere. Nothing may be written for this one, ever.
	_, orphanPIAL, _ := seedPIALAccount(t, database)

	res, err := ReconcilePIALBirthdays(database)
	if err != nil {
		t.Fatalf("ReconcilePIALBirthdays: %v", err)
	}
	if res.Repaired < 1 {
		t.Fatalf("Repaired = %d, want at least the one seeded account", res.Repaired)
	}
	if res.Unrecoverable < 1 {
		t.Fatalf("Unrecoverable = %d, want at least the one account with no date on record", res.Unrecoverable)
	}

	got := readAgeFacts(t, database, repairablePIAL)
	if got.dob.String != "1990-04-11" {
		t.Errorf("repaired date_of_birth = %q, want %q", got.dob.String, "1990-04-11")
	}
	if !got.isAdult {
		t.Error("repaired account is still not is_adult")
	}

	orphan := readAgeFacts(t, database, orphanPIAL)
	if orphan.dob.Valid {
		t.Fatalf("the reconciler invented a date of birth (%q) for an account that had none — "+
			"it must never do this", orphan.dob.String)
	}
	if orphan.isAdult || orphan.ageVerified {
		t.Error("an account with no date of birth on record was marked adult or age-verified")
	}
}
