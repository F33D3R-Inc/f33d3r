package db

import (
	"database/sql"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// ─── source-level tests (no database) ────────────────────────────────────────

// deletionPathFunctions are the functions in this file whose whole job is to
// destroy data. Every statement they issue must have its error read.
var deletionPathFunctions = map[string]bool{
	"PurgeUser":       true,
	"AdminDeleteWork": true,
	"SoftDeleteWork":  true,
}

// TestNoDeletionStatementDiscardsItsError is the test that would have caught the
// original PurgeUser on the day it was written.
//
// That function was fourteen bare db.Exec(...) calls with no assignment at all —
// not `_, err :=`, not `_, _ =`, nothing. Go permits a call statement to drop
// every one of its return values, so every intermediate failure inside an
// irreversible, founder-only erasure was invisible: the account's works, profile,
// roles and sessions were destroyed and the function returned the error of the
// LAST statement as though it were the only thing that had happened.
//
// This walks the real AST of works.go and fails if any statement inside a
// deletion-path function calls Exec, Query or QueryRow in expression position —
// that is, as a bare call whose results, including the error, go nowhere. A
// statement whose error is deliberately not worth reading does not exist on a
// deletion path; if one ever seems to, the answer is to read it and decide, not
// to drop it.
func TestNoDeletionStatementDiscardsItsError(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "works.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing works.go: %v", err)
	}

	seen := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || !deletionPathFunctions[fn.Name.Name] {
			continue
		}
		seen[fn.Name.Name] = true

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			stmt, ok := n.(*ast.ExprStmt)
			if !ok {
				return true
			}
			call, ok := stmt.X.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "Exec", "ExecContext", "Query", "QueryContext", "QueryRow", "QueryRowContext":
				t.Errorf(
					"%s: %s at %s discards its result AND its error.\n"+
						"This is a deletion path. Assign the result and return the error, or the "+
						"statement can fail in the middle of destroying an account and nothing will "+
						"ever say so — which is exactly what PurgeUser did before migration 0018.",
					fn.Name.Name, render(sel), fset.Position(stmt.Pos()))
			}
			return true
		})
	}

	for name := range deletionPathFunctions {
		if !seen[name] {
			t.Errorf("%s is no longer declared in works.go — this test is guarding nothing. "+
				"Move it with the function or remove it deliberately.", name)
		}
	}
}

// TestPurgeRunsInOneTransaction pins the all-or-nothing property in the source,
// so it survives a refactor that a database-backed test would not be run against.
// A purge that fails part-way must roll back to the account still existing
// intact, and that is only true while every statement shares one transaction.
func TestPurgeRunsInOneTransaction(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "works.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing works.go: %v", err)
	}

	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		if d, ok := decl.(*ast.FuncDecl); ok && d.Name.Name == "PurgeUser" {
			fn = d
			break
		}
	}
	if fn == nil {
		t.Fatal("PurgeUser is not declared in works.go")
	}

	var begins, commits, directDBCalls int
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		switch {
		case recv.Name == "db" && sel.Sel.Name == "Begin":
			begins++
		case sel.Sel.Name == "Commit":
			commits++
		case recv.Name == "db" && strings.HasPrefix(sel.Sel.Name, "Exec"),
			recv.Name == "db" && strings.HasPrefix(sel.Sel.Name, "Query"):
			directDBCalls++
		}
		return true
	})

	if begins != 1 {
		t.Errorf("PurgeUser calls db.Begin %d times, want exactly 1 — a purge is one transaction", begins)
	}
	if commits != 1 {
		t.Errorf("PurgeUser calls Commit %d times, want exactly 1 — a purge commits once, at the end", commits)
	}
	if directDBCalls != 0 {
		t.Errorf("PurgeUser issues %d statement(s) directly on the *sql.DB instead of the transaction. "+
			"Those escape the rollback, so a failure would leave a half-purged account.", directDBCalls)
	}
}

// TestPurgeOrphanStatementsAreDocumented keeps the reason for each statement
// attached to the statement. A deletion list nobody can explain is a deletion
// list nobody can safely change.
func TestPurgeOrphanStatementsAreDocumented(t *testing.T) {
	if len(purgeOrphanStatements) == 0 {
		t.Fatal("purgeOrphanStatements is empty — the tables with no foreign key to users " +
			"would be left behind by a purge")
	}
	for i, s := range purgeOrphanStatements {
		if strings.TrimSpace(s.why) == "" {
			t.Errorf("purgeOrphanStatements[%d] (%s) has no stated reason",
				i, strings.Join(strings.Fields(s.stmt), " "))
		}
		if !strings.Contains(s.stmt, "$1") && !strings.Contains(s.stmt, "$2") {
			t.Errorf("purgeOrphanStatements[%d] names neither the account nor the identity: %s",
				i, strings.Join(strings.Fields(s.stmt), " "))
		}
	}
}

// TestPurgeNeverDeletesFromTheLedger is the one rule in this file that is not a
// judgement call.
//
// pial_event_ledger is append-only. That is not a convention, it is the whole of
// its value: it is the record that an identity was granted a realm, had a
// capability revoked, was reported for CSAM, exported its data. A purge releases
// the account reference to NULL through the foreign key migration 0018 declared.
// It does not, and may never, remove the rows.
func TestPurgeNeverDeletesFromTheLedger(t *testing.T) {
	body, err := os.ReadFile("works.go")
	if err != nil {
		t.Fatalf("reading works.go: %v", err)
	}
	lowered := strings.ToLower(string(body))
	for _, forbidden := range []string{
		"delete from pial_event_ledger",
		"update pial_event_ledger",
		"truncate pial_event_ledger",
	} {
		if strings.Contains(lowered, forbidden) {
			t.Errorf("works.go contains %q. The PIAL event ledger is append-only — "+
				"the erasure it owes a person is a NULL account reference, not a deleted row.", forbidden)
		}
	}
	// And the append that records the purge itself must still be there.
	if !strings.Contains(lowered, "insert into pial_event_ledger") {
		t.Error("PurgeUser no longer appends an 'account_purged' event to the ledger — " +
			"an erasure the identity ledger has no record of is an erasure nobody can prove happened")
	}
}

// ─── schema-backed tests ─────────────────────────────────────────────────────
//
// These need a throwaway PostgreSQL named by PURGE_TEST_DATABASE_URL. They apply
// the real embedded migrations, so they also prove 0018 applies on top of the
// existing chain. Never point this at a database that holds real rows: these
// tests delete accounts.

var purgeTestSeq atomic.Int64

func purgeTestDB(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("PURGE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PURGE_TEST_DATABASE_URL not set — schema-backed purge tests skipped")
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

// seedPurgeUser builds an account the way onboarding does — identity root first,
// then the account bound to it, then the profile — and gives it a work, a
// session, an XP event and a ledger entry, so a purge has something to destroy
// and the ledger has something to survive with.
func seedPurgeUser(t *testing.T, database *sql.DB) (userID, pialID, handle string) {
	t.Helper()
	handle = fmt.Sprintf("purgetest_%d_%d", time.Now().UnixNano(), purgeTestSeq.Add(1))

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
	if _, err := database.Exec(
		`INSERT INTO pial_account_bindings (pial_id, account_id) VALUES ($1::uuid, $2::uuid)`,
		pialID, userID); err != nil {
		t.Fatalf("seeding pial_account_bindings: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO user_sessions (user_id, token_hash, pial_id, active_account_id)
		 VALUES ($1::uuid, $2, $3::uuid, $1::uuid)`,
		userID, handle+"-token", pialID); err != nil {
		t.Fatalf("seeding user_sessions: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO works (cid, author_id, author_pial, body, kind)
		 VALUES ($1, $2::uuid, $3::uuid, 'seeded for the purge test', 'post')`,
		"sha256:"+handle, userID, pialID); err != nil {
		t.Fatalf("seeding works: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO pial_signing_keys (pial_id, public_key_b64) VALUES ($1::uuid, 'seeded')`,
		pialID); err != nil {
		t.Fatalf("seeding pial_signing_keys: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO feedback_events (user_id, session_id, surface, content_id, event_type)
		 VALUES ($1, $2, 'home', 'x', 'view')`, userID, handle+"-session"); err != nil {
		t.Fatalf("seeding feedback_events: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO pial_event_ledger (pial_id, account_id, event_type, source)
		 VALUES ($1::uuid, $2::uuid, 'seeded_for_test', 'test')`,
		pialID, userID); err != nil {
		t.Fatalf("seeding pial_event_ledger: %v", err)
	}
	return userID, pialID, handle
}

func countRows(t *testing.T, database *sql.DB, query string, args ...interface{}) int {
	t.Helper()
	var n int
	if err := database.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("counting (%s): %v", strings.Join(strings.Fields(query), " "), err)
	}
	return n
}

// TestPurgeClearsEveryTableThatNamesAPerson is the pin on what a purge must
// clear. It is written against the SCHEMA, not against a hand-copied list, so a
// table added tomorrow is covered tomorrow.
//
// Every table with a foreign key to users must resolve to one of three answers,
// and a fourth — NO ACTION with nothing else to say — is what broke the purge in
// the first place and fails here:
//
//	CASCADE   the row is the purged person's own, and goes with them
//	SET NULL  the row survives with the person released (see 0018 for each)
//	named     the purge clears it explicitly in purgeOrphanStatements
func TestPurgeClearsEveryTableThatNamesAPerson(t *testing.T) {
	database := purgeTestDB(t)

	rows, err := database.Query(`
		SELECT src.relname,
		       (SELECT a.attname FROM pg_attribute a
		         WHERE a.attrelid = con.conrelid AND a.attnum = con.conkey[1]),
		       con.confdeltype::text
		  FROM pg_constraint con
		  JOIN pg_class src ON src.oid = con.conrelid
		  JOIN pg_class tgt ON tgt.oid = con.confrelid
		 WHERE con.contype = 'f' AND tgt.relname = 'users'
		 ORDER BY src.relname`)
	if err != nil {
		t.Fatalf("reading foreign keys to users: %v", err)
	}
	defer rows.Close()

	var offenders []string
	for rows.Next() {
		var table, column, action string
		if err := rows.Scan(&table, &column, &action); err != nil {
			t.Fatalf("scanning foreign key: %v", err)
		}
		switch action {
		case "c", "n": // CASCADE, SET NULL — both are decisions 0018 states.
			continue
		}
		named := false
		for _, s := range purgeOrphanStatements {
			// Token match, not substring: the statements wrap across lines, so a
			// table name can be followed by a newline rather than a space.
			for _, tok := range strings.Fields(s.stmt) {
				if tok == table {
					named = true
					break
				}
			}
			if named {
				break
			}
		}
		if !named {
			offenders = append(offenders, fmt.Sprintf("%s.%s (ON DELETE %s)", table, column, action))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading foreign keys to users: %v", err)
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("these foreign keys to users neither cascade, nor release the person, nor are "+
			"cleared by PurgeUser, so DELETE FROM users cannot succeed while any of them holds a row:\n  %s\n"+
			"Decide each one in a new migration the way 0018 did — what should outlive a deleted person "+
			"and why — rather than letting the purge fail half-way through destroying an account.",
			strings.Join(offenders, "\n  "))
	}
}

// TestPurgeErasesTheAccountAndKeepsTheLedger is the end-to-end shape of the
// decision: the person goes, the append-only record stays and stops naming them,
// and the identity root is tombstoned rather than deleted.
func TestPurgeErasesTheAccountAndKeepsTheLedger(t *testing.T) {
	database := purgeTestDB(t)
	userID, pialID, _ := seedPurgeUser(t, database)

	if got := countRows(t, database,
		`SELECT COUNT(*) FROM pial_event_ledger WHERE pial_id = $1::uuid AND account_id IS NOT NULL`,
		pialID); got != 1 {
		t.Fatalf("seeded ledger rows naming the account = %d, want 1", got)
	}

	if err := PurgeUser(database, userID, pialID); err != nil {
		t.Fatalf("PurgeUser: %v", err)
	}

	for _, c := range []struct {
		what  string
		query string
		arg   string
	}{
		{"users", `SELECT COUNT(*) FROM users WHERE id = $1::uuid`, userID},
		{"user_profiles", `SELECT COUNT(*) FROM user_profiles WHERE user_id = $1::uuid`, userID},
		{"user_sessions", `SELECT COUNT(*) FROM user_sessions WHERE user_id = $1::uuid`, userID},
		{"works", `SELECT COUNT(*) FROM works WHERE author_id = $1::uuid`, userID},
		{"feedback_events", `SELECT COUNT(*) FROM feedback_events WHERE user_id = $1`, userID},
		{"pial_account_bindings", `SELECT COUNT(*) FROM pial_account_bindings WHERE account_id = $1::uuid`, userID},
		{"pial_signing_keys", `SELECT COUNT(*) FROM pial_signing_keys WHERE pial_id = $1::uuid`, pialID},
		{"pial_capabilities", `SELECT COUNT(*) FROM pial_capabilities WHERE pial_id = $1::uuid`, pialID},
		{"trust_scores", `SELECT COUNT(*) FROM trust_scores WHERE pial_id = $1::uuid`, pialID},
	} {
		if n := countRows(t, database, c.query, c.arg); n != 0 {
			t.Errorf("%s still holds %d row(s) for the purged account", c.what, n)
		}
	}

	// The ledger survives, and no longer names anyone.
	if n := countRows(t, database,
		`SELECT COUNT(*) FROM pial_event_ledger WHERE pial_id = $1::uuid AND event_type = 'seeded_for_test'`,
		pialID); n != 1 {
		t.Errorf("the seeded ledger event is gone (%d rows). The PIAL event ledger is append-only.", n)
	}
	if n := countRows(t, database,
		`SELECT COUNT(*) FROM pial_event_ledger WHERE pial_id = $1::uuid AND account_id IS NOT NULL`,
		pialID); n != 0 {
		t.Errorf("%d ledger row(s) still name the purged account; the foreign key should have released them", n)
	}
	if n := countRows(t, database,
		`SELECT COUNT(*) FROM pial_event_ledger WHERE pial_id = $1::uuid AND event_type = 'account_purged'`,
		pialID); n != 1 {
		t.Errorf("the purge appended %d 'account_purged' events, want 1", n)
	}

	// The identity root is tombstoned, not deleted, and holds nothing personal.
	var tombstoned bool
	var publicKey, kycTier, status string
	var dob sql.NullString
	if err := database.QueryRow(`
		SELECT is_tombstoned, public_key, kyc_tier, status, date_of_birth::text
		  FROM pial_roots WHERE pial_id = $1::uuid`, pialID,
	).Scan(&tombstoned, &publicKey, &kycTier, &status, &dob); err != nil {
		t.Fatalf("reading the tombstoned identity root: %v — it must survive, "+
			"or the append-only ledger loses the row it references", err)
	}
	if !tombstoned || status != "purged" {
		t.Errorf("identity root: is_tombstoned=%v status=%q, want true/\"purged\"", tombstoned, status)
	}
	if publicKey != "" || kycTier != "none" || dob.Valid {
		t.Errorf("identity root still holds personal content: public_key=%q kyc_tier=%q dob=%v",
			publicKey, kycTier, dob)
	}
}

// TestPurgeRollsBackCompletely proves the all-or-nothing property against a real
// database rather than only in the source: a purge that fails at its last
// statement must leave the account exactly as it was, not half destroyed.
//
// The failure is constructed deliberately and honestly — a trigger that raises on
// DELETE FROM users, installed for the length of this test — because the point is
// to observe a real mid-purge abort, and the previous implementation's abort was
// at exactly that statement.
func TestPurgeRollsBackCompletely(t *testing.T) {
	database := purgeTestDB(t)
	userID, pialID, handle := seedPurgeUser(t, database)

	if _, err := database.Exec(`
		CREATE OR REPLACE FUNCTION purge_test_refuse() RETURNS TRIGGER AS $$
		BEGIN
			RAISE EXCEPTION 'purge_test_refuse: deliberate failure at the final statement';
		END;
		$$ LANGUAGE plpgsql;
		DROP TRIGGER IF EXISTS trg_purge_test_refuse ON users;
		CREATE TRIGGER trg_purge_test_refuse BEFORE DELETE ON users
			FOR EACH ROW WHEN (OLD.handle = '` + handle + `')
			EXECUTE FUNCTION purge_test_refuse();`); err != nil {
		t.Fatalf("installing the deliberate failure: %v", err)
	}
	t.Cleanup(func() {
		if _, err := database.Exec(`DROP TRIGGER IF EXISTS trg_purge_test_refuse ON users`); err != nil {
			t.Errorf("removing the deliberate failure: %v", err)
		}
	})

	err := PurgeUser(database, userID, pialID)
	if err == nil {
		t.Fatal("PurgeUser returned nil while the account could not be deleted")
	}
	if !strings.Contains(err.Error(), "purge_test_refuse") {
		t.Fatalf("PurgeUser returned %v, want the failure raised at DELETE FROM users", err)
	}

	// Everything the purge had already done before the failure must be undone.
	for _, c := range []struct {
		what  string
		query string
		arg   string
		want  int
	}{
		{"users", `SELECT COUNT(*) FROM users WHERE id = $1::uuid`, userID, 1},
		{"user_profiles", `SELECT COUNT(*) FROM user_profiles WHERE user_id = $1::uuid`, userID, 1},
		{"user_sessions", `SELECT COUNT(*) FROM user_sessions WHERE user_id = $1::uuid`, userID, 1},
		{"works", `SELECT COUNT(*) FROM works WHERE author_id = $1::uuid`, userID, 1},
		{"pial_signing_keys", `SELECT COUNT(*) FROM pial_signing_keys WHERE pial_id = $1::uuid`, pialID, 1},
		{"feedback_events", `SELECT COUNT(*) FROM feedback_events WHERE user_id = $1`, userID, 1},
		{"pial_account_bindings", `SELECT COUNT(*) FROM pial_account_bindings WHERE account_id = $1::uuid`, userID, 1},
	} {
		if n := countRows(t, database, c.query, c.arg); n != c.want {
			t.Errorf("after a failed purge, %s has %d row(s), want %d — the transaction did not roll back "+
				"and the account is half destroyed", c.what, n, c.want)
		}
	}

	var tombstoned bool
	if err := database.QueryRow(
		`SELECT is_tombstoned FROM pial_roots WHERE pial_id = $1::uuid`, pialID).Scan(&tombstoned); err != nil {
		t.Fatalf("reading the identity root after a failed purge: %v", err)
	}
	if tombstoned {
		t.Error("the identity root was tombstoned by a purge that failed — the account still exists, " +
			"so its identity must still be live")
	}

	// And with the deliberate failure removed, the same purge succeeds.
	if _, err := database.Exec(`DROP TRIGGER IF EXISTS trg_purge_test_refuse ON users`); err != nil {
		t.Fatalf("removing the deliberate failure: %v", err)
	}
	if err := PurgeUser(database, userID, pialID); err != nil {
		t.Fatalf("PurgeUser after removing the deliberate failure: %v", err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM users WHERE id = $1::uuid`, userID); n != 0 {
		t.Errorf("users still holds %d row(s) after a successful purge", n)
	}
}

// TestPurgeRefusesAnIdentityMismatch — an irreversible erasure whose caller and
// whose database disagree about whose identity is being destroyed stops.
func TestPurgeRefusesAnIdentityMismatch(t *testing.T) {
	database := purgeTestDB(t)
	userID, pialID, _ := seedPurgeUser(t, database)
	otherID, otherPIAL, _ := seedPurgeUser(t, database)

	if err := PurgeUser(database, userID, otherPIAL); err == nil {
		t.Fatal("PurgeUser accepted another account's PIAL and destroyed the account anyway")
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM users WHERE id = $1::uuid`, userID); n != 1 {
		t.Errorf("the account was purged despite the identity mismatch (%d rows remain, want 1)", n)
	}

	// Clean up: purge both properly so the test leaves nothing behind.
	if err := PurgeUser(database, userID, pialID); err != nil {
		t.Errorf("purging the first seeded account: %v", err)
	}
	if err := PurgeUser(database, otherID, otherPIAL); err != nil {
		t.Errorf("purging the second seeded account: %v", err)
	}
}

// TestPurgeKeepsFollowCountersAndAssociationsConsistent covers the two triggers
// that now sit between a users delete and the rest of the platform: migration
// 0016's cached follower counters and migration 0017's Manhattan association
// projection. Both fire from inside the cascade, so a purge must leave the
// surviving party's counters correct and one retraction queued per follow edge.
func TestPurgeKeepsFollowCountersAndAssociationsConsistent(t *testing.T) {
	database := purgeTestDB(t)
	goneID, gonePIAL, _ := seedPurgeUser(t, database)
	stayID, stayPIAL, _ := seedPurgeUser(t, database)

	for _, edge := range [][2]string{{goneID, stayID}, {stayID, goneID}} {
		if _, err := database.Exec(
			`INSERT INTO follows (follower_id, following_id) VALUES ($1::uuid, $2::uuid)`,
			edge[0], edge[1]); err != nil {
			t.Fatalf("seeding follow %s -> %s: %v", edge[0], edge[1], err)
		}
	}

	before := countRows(t, database, `SELECT COUNT(*) FROM manhattan_outbox`)
	if got := countRows(t, database,
		`SELECT follower_count + following_count FROM user_profiles WHERE user_id = $1::uuid`,
		stayID); got != 2 {
		t.Fatalf("surviving account's counters before the purge = %d, want 2", got)
	}

	if err := PurgeUser(database, goneID, gonePIAL); err != nil {
		t.Fatalf("PurgeUser: %v", err)
	}

	if n := countRows(t, database,
		`SELECT COUNT(*) FROM follows WHERE follower_id = $1::uuid OR following_id = $1::uuid`,
		goneID); n != 0 {
		t.Errorf("%d follow edge(s) survive the purged account", n)
	}
	var followers, following int
	if err := database.QueryRow(
		`SELECT follower_count, following_count FROM user_profiles WHERE user_id = $1::uuid`,
		stayID).Scan(&followers, &following); err != nil {
		t.Fatalf("reading the surviving account's counters: %v", err)
	}
	if followers != 0 || following != 0 {
		t.Errorf("surviving account's cached counters are %d/%d after the purge, want 0/0 — "+
			"migration 0016's trigger must fire through the cascade too", followers, following)
	}

	// 0017 retires the associations from the parent side, BEFORE the cascade
	// erases the evidence, so both PIALs are still readable: two edges, two
	// retractions.
	after := countRows(t, database, `SELECT COUNT(*) FROM manhattan_outbox`)
	if after-before != 2 {
		t.Errorf("the purge queued %d Manhattan outbox rows for 2 follow edges, want 2 — "+
			"a follow the association plane still believes in is a stale authorisation in elohim-veni",
			after-before)
	}
	unassoc := countRows(t, database,
		`SELECT COUNT(*) FROM manhattan_outbox WHERE op = 'unassoc' AND payload->>'assoc' = 'follows'
		   AND (payload->>'subject_name' = $1 OR payload->>'object_name' = $1)`,
		"pial:"+gonePIAL)
	if unassoc != 2 {
		t.Errorf("%d follow retractions name the purged identity, want 2", unassoc)
	}

	if err := PurgeUser(database, stayID, stayPIAL); err != nil {
		t.Errorf("purging the surviving seeded account: %v", err)
	}
}

// render prints a selector expression like "tx.Exec" for an error message.
func render(sel *ast.SelectorExpr) string {
	if id, ok := sel.X.(*ast.Ident); ok {
		return id.Name + "." + sel.Sel.Name
	}
	return sel.Sel.Name
}
