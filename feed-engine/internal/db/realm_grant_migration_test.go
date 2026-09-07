package db

import (
	"strings"
	"testing"

	"github.com/f33d3r/feed-engine/internal/realm"
)

// TestTheGrantScaleMatchesTheMigration keeps the database's idea of a legal
// grant and the Go realm scale from drifting apart.
//
// 0014 constrains users.realm_grant to 0 (no grant) plus the realm scale, and
// raises the founder to realm.FounderFloor. Those bounds have to be literals in
// SQL — a migration cannot call into Go — so this test is what makes them
// derived anyway: widen or narrow the scale in package realm and this fails
// until a higher-numbered migration replaces the constraint.
func TestTheGrantScaleMatchesTheMigration(t *testing.T) {
	body, err := migrationFS.ReadFile("migrations/0014_realm_grants.sql")
	if err != nil {
		t.Fatalf("reading 0014: %v", err)
	}
	sql := string(body)

	wantCheck := "CHECK (realm_grant BETWEEN 0 AND 5)"
	if realm.Min != 1 || realm.Max != 5 {
		t.Fatalf("the realm scale moved to R%d–R%d; migration 0014 still constrains "+
			"realm_grant to 0–5 and pins the founder floor at 5. Write a new migration "+
			"that replaces users_realm_grant_scale, then update this test.",
			realm.Min, realm.Max)
	}
	if !strings.Contains(sql, wantCheck) {
		t.Errorf("0014 no longer contains %q — the column constraint and the realm scale "+
			"must say the same thing", wantCheck)
	}
	if realm.FounderFloor != realm.Max {
		t.Errorf("FounderFloor = R%d but 0014 raises the founder to R%d",
			realm.FounderFloor, realm.Max)
	}
}
