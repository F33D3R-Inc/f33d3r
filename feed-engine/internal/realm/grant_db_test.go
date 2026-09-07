package realm_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/realm"
)

// ─── schema-backed grant tests ───────────────────────────────────────────────
//
// These need a throwaway PostgreSQL named by REALM_TEST_DATABASE_URL. They
// apply the real embedded migrations, so they also prove 0014 applies on top of
// the existing chain. Never point this at a database that holds real rows.
//
// The pure tests in grant_test.go prove Effective is a floor. These prove the
// two statements that write users.realm actually go through it — which is the
// half a pure test cannot reach, and the half the bug lived in.

func realmTestDB(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("REALM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("REALM_TEST_DATABASE_URL not set — schema-backed realm tests skipped")
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
	dbpkg.MigrateUp(database)
	return database
}

// probeAccount creates one account and returns its id. The handle is unique per
// test so the cases do not have to run in any particular order.
func probeAccount(t *testing.T, database *sql.DB, handle string, xp int64, role string) string {
	t.Helper()
	if _, err := database.Exec(`DELETE FROM users WHERE handle = $1`, handle); err != nil {
		t.Fatalf("clearing %s: %v", handle, err)
	}
	var id string
	if err := database.QueryRow(`
		INSERT INTO users (handle, xp, realm, role) VALUES ($1, $2, 1, $3) RETURNING id
	`, handle, xp, role).Scan(&id); err != nil {
		t.Fatalf("creating %s: %v", handle, err)
	}
	t.Cleanup(func() { database.Exec(`DELETE FROM users WHERE handle = $1`, handle) })
	return id
}

func standing(t *testing.T, database *sql.DB, id string) (xp int64, grant, realmLevel int) {
	t.Helper()
	if err := database.QueryRow(`
		SELECT COALESCE(xp,0), COALESCE(realm_grant,0), COALESCE(realm,1) FROM users WHERE id = $1
	`, id).Scan(&xp, &grant, &realmLevel); err != nil {
		t.Fatalf("reading standing: %v", err)
	}
	return
}

// TestAnAwardCannotEraseAGrantInTheDatabase is the bug, reproduced and closed.
// Before grants existed, AwardXP wrote ComputeRealm(xp) straight over
// users.realm, so an account placed at R5 on 125 XP fell to R1 on its next like.
func TestAnAwardCannotEraseAGrantInTheDatabase(t *testing.T) {
	database := realmTestDB(t)
	ix := realm.Start(context.Background(), database, time.Minute)

	const handle = "realmprobe_granted"
	id := probeAccount(t, database, handle, 125, model.RoleUser)

	effective, err := realm.SetGrant(database, id, 5)
	if err != nil {
		t.Fatalf("SetGrant: %v", err)
	}
	if effective != 5 {
		t.Fatalf("SetGrant returned R%d, want R5", effective)
	}
	if xp, grant, level := standing(t, database, id); grant != 5 || level != 5 || xp != 125 {
		t.Fatalf("after grant: xp=%d grant=%d realm=%d, want 125/5/5", xp, grant, level)
	}
	// The ring is correct without waiting for the reconcile pass.
	if got := realm.Of(handle); got != 5 {
		t.Errorf("index says R%d immediately after the grant, want R5", got)
	}
	if got := realm.RingClass(handle); got != " realm-ring realm-ring--r5" {
		t.Errorf("RingClass = %q, want the R5 ring", got)
	}

	// A like. Under the old code this wrote realm=1 and the ring vanished.
	realm.AwardXP(database, id, "like", "", realm.XPLike)

	xp, grant, level := standing(t, database, id)
	if xp != 135 {
		t.Errorf("xp = %d, want 135 — the award must still land", xp)
	}
	if grant != 5 {
		t.Errorf("grant = %d after an award, want it untouched at 5", grant)
	}
	if level != 5 {
		t.Fatalf("realm = %d after an award, want 5 — THE AWARD ERASED THE GRANT", level)
	}
	if got := realm.Of(handle); got != 5 {
		t.Errorf("index says R%d after the award, want R5", got)
	}
	if ix.Size() < 1 {
		t.Error("the granted account is missing from the resident set")
	}
}

// TestEarnedStandingCarriesPastTheGrant proves the floor is a floor: XP that
// passes the grant raises the account, and the grant does not cap it.
func TestEarnedStandingCarriesPastTheGrant(t *testing.T) {
	database := realmTestDB(t)
	realm.Start(context.Background(), database, time.Minute)

	const handle = "realmprobe_earner"
	id := probeAccount(t, database, handle, 19990, model.RoleUser)

	if _, err := realm.SetGrant(database, id, 2); err != nil {
		t.Fatalf("SetGrant: %v", err)
	}
	// 19990 XP is R4 earned; the R2 grant is already inert.
	if _, _, level := standing(t, database, id); level != 4 {
		t.Fatalf("19990 XP with an R2 grant sits at R%d, want the earned R4", level)
	}

	realm.AwardXP(database, id, "like", "", realm.XPLike) // → 20000 XP, R5
	xp, grant, level := standing(t, database, id)
	if xp != 20000 || level != 5 || grant != 2 {
		t.Fatalf("xp=%d grant=%d realm=%d, want 20000/2/5", xp, grant, level)
	}

	// Clearing the grant leaves earned standing exactly where it was.
	if _, err := realm.SetGrant(database, id, realm.GrantNone); err != nil {
		t.Fatalf("clearing grant: %v", err)
	}
	if _, grant, level := standing(t, database, id); grant != 0 || level != 5 {
		t.Fatalf("after clearing: grant=%d realm=%d, want 0/5 — earned standing is not a grant", grant, level)
	}
}

// TestClearingAGrantReturnsTheAccountToWhatItEarned is the only way standing
// comes down. It must return the account to its earned realm, not to R1 and not
// to somewhere in between.
func TestClearingAGrantReturnsTheAccountToWhatItEarned(t *testing.T) {
	database := realmTestDB(t)
	realm.Start(context.Background(), database, time.Minute)

	const handle = "realmprobe_cleared"
	id := probeAccount(t, database, handle, 2000, model.RoleUser) // earned R3

	if _, err := realm.SetGrant(database, id, 5); err != nil {
		t.Fatalf("SetGrant: %v", err)
	}
	if _, _, level := standing(t, database, id); level != 5 {
		t.Fatalf("granted account sits at R%d, want R5", level)
	}
	if _, err := realm.SetGrant(database, id, realm.GrantNone); err != nil {
		t.Fatalf("clearing grant: %v", err)
	}
	if _, _, level := standing(t, database, id); level != 3 {
		t.Errorf("cleared account sits at R%d, want the earned R3", level)
	}
	if got := realm.Of(handle); got != 3 {
		t.Errorf("index says R%d after the grant was cleared, want R3", got)
	}
}

// TestTheFounderRoleCarriesItsFloorThroughTheDatabase proves the founder floor
// is derived from the role on the write paths, not stamped once by a migration.
func TestTheFounderRoleCarriesItsFloorThroughTheDatabase(t *testing.T) {
	database := realmTestDB(t)
	realm.Start(context.Background(), database, time.Minute)

	const handle = "realmprobe_founder"
	id := probeAccount(t, database, handle, 0, model.RoleUser)

	if _, err := database.Exec(`UPDATE users SET role = $1 WHERE id = $2`, model.RoleFounder, id); err != nil {
		t.Fatalf("promoting: %v", err)
	}
	// The role-change path restates standing; this is what the admin role
	// control calls.
	if effective, err := realm.Restate(database, id); err != nil || effective != realm.Max {
		t.Fatalf("Restate after promotion = R%d, %v; want R%d", effective, err, realm.Max)
	}
	if _, grant, level := standing(t, database, id); level != realm.Max || grant != 0 {
		t.Fatalf("founder sits at R%d with grant %d, want R%d with no grant at all", level, grant, realm.Max)
	}

	// And an award does not knock it off the top of the scale.
	realm.AwardXP(database, id, "like", "", realm.XPLike)
	if _, _, level := standing(t, database, id); level != realm.Max {
		t.Errorf("founder fell to R%d after an award", level)
	}

	// Demotion returns the account to what it actually earned — the floor
	// belongs to the role and leaves with it.
	if _, err := database.Exec(`UPDATE users SET role = $1 WHERE id = $2`, model.RoleUser, id); err != nil {
		t.Fatalf("demoting: %v", err)
	}
	if effective, err := realm.Restate(database, id); err != nil || effective != realm.Min {
		t.Fatalf("Restate after demotion = R%d, %v; want R%d", effective, err, realm.Min)
	}
}

// TestAnOutOfScaleGrantIsRefusedNotDegraded proves a grant typed in by a human
// is either applied or rejected out loud. Silently clamping it to R1 would look
// like a privilege change that happened and did not.
func TestAnOutOfScaleGrantIsRefusedNotDegraded(t *testing.T) {
	database := realmTestDB(t)
	realm.Start(context.Background(), database, time.Minute)

	const handle = "realmprobe_outofscale"
	id := probeAccount(t, database, handle, 0, model.RoleUser)

	for _, bad := range []int{-1, realm.Max + 1, 99} {
		if _, err := realm.SetGrant(database, id, bad); err == nil {
			t.Errorf("SetGrant(%d) was accepted, want a refusal", bad)
		}
	}
	if _, grant, level := standing(t, database, id); grant != 0 || level != 1 {
		t.Errorf("a refused grant still moved the row: grant=%d realm=%d", grant, level)
	}
}
