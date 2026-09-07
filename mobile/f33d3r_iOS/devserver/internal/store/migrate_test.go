package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// A dev database written before the rename holds the lane in tables called
// fleets. Migrating must carry those rows over, not leave them behind while
// schema.sql quietly creates a fresh empty set beside them — which is what
// CREATE TABLE IF NOT EXISTS would do on its own.
func TestMigrateRenamesFleetTablesAndKeepsTheRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite3", "file:"+path+"?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	// The v3 shape is today's schema with the lane under its old name.
	old := schemaSQL
	for _, pair := range [][2]string{
		{"visions", "fleets"}, {"vision_views", "fleet_views"},
		{"vision_poll_votes", "fleet_poll_votes"}, {"vision_replies", "fleet_replies"},
		{"vision_mutes", "fleet_mutes"}, {"vision_id", "fleet_id"},
	} {
		old = strings.ReplaceAll(old, pair[0], pair[1])
	}
	if _, err := db.Exec(old); err != nil {
		t.Fatalf("building the pre-rename schema: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 3`); err != nil {
		t.Fatal(err)
	}
	// The lane's rows point at an author, so there has to be one.
	if _, err := db.Exec(`
		INSERT INTO pial_roots (pial_id, created_at) VALUES ('p1', '2026-09-05T00:00:00.000000000Z');
		INSERT INTO users (id, handle, pial_id, created_at, updated_at)
		VALUES ('u1', 'dev', 'p1', '2026-09-05T00:00:00.000000000Z', '2026-09-05T00:00:00.000000000Z')`); err != nil {
		t.Fatalf("seeding the author: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO fleets (id, author_id, author_pial, body, created_at, expires_at)
		VALUES ('v1', 'u1', 'p1', 'still here', '2026-09-05T00:00:00.000000000Z', '2026-09-06T00:00:00.000000000Z')`); err != nil {
		t.Fatalf("seeding the old table: %v", err)
	}

	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var body string
	if err := db.QueryRow(`SELECT body FROM visions WHERE id = 'v1'`).Scan(&body); err != nil {
		t.Fatalf("the row did not survive the rename: %v", err)
	}
	if body != "still here" {
		t.Errorf("body = %q", body)
	}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Errorf("user_version = %d, want %d", version, schemaVersion)
	}
	// Nothing is left under the old name for a later read to find.
	var leftovers int
	db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name LIKE 'fleet%'`).Scan(&leftovers)
	if leftovers != 0 {
		t.Errorf("%d fleet tables still present", leftovers)
	}
}

// A database created fresh today never sees the rename step.
func TestMigrateIsCleanOnANewDatabase(t *testing.T) {
	st, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM visions`).Scan(&n); err != nil {
		t.Fatalf("visions table missing: %v", err)
	}
}
