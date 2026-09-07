package db

import (
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"log"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Ordered, versioned, forward-only migrations.
//
// Before this existed, the whole schema was one 1,687-line idempotent DDL blob
// replayed on every boot. That has no version, no ordering, and no record of
// what ran — so nothing could depend on anything else, and any table added to
// the blob raced every other table in it.
//
// The rule now:
//
//   * Every schema change is a numbered file in migrations/.
//   * Files apply in ascending version order, each inside its own transaction.
//   * A file that has been applied is frozen: its checksum is recorded, and a
//     later edit is a hard boot failure, never a silent skip.
//   * Forward-only. There is no down path. To undo, write a higher-numbered
//     migration that undoes it.
//
// Filenames: NNNN_snake_case_name.sql  (e.g. 0003_work_edges.sql)
// ─────────────────────────────────────────────────────────────────────────────

//go:embed migrations/*.sql
var migrationFS embed.FS

// advisoryLockKey serialises migration runs across every process that boots
// against the same database. Arbitrary but fixed — changing it breaks the
// mutual exclusion it exists to provide.
const advisoryLockKey int64 = 7331_0001

type migration struct {
	version  int64
	name     string
	filename string
	body     string
	checksum string
}

// loadMigrations reads and validates every embedded migration file.
// It fails hard on a malformed filename or a duplicate version — a migration
// plane you cannot fully parse is a migration plane you cannot trust.
func loadMigrations() []migration {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		log.Fatalf("[db] cannot read embedded migrations: %v", err)
	}

	seen := make(map[int64]string, len(entries))
	out := make([]migration, 0, len(entries))

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".sql")
		parts := strings.SplitN(base, "_", 2)
		if len(parts) != 2 || parts[1] == "" {
			log.Fatalf("[db] migration %q: name must be NNNN_description.sql", e.Name())
		}
		version, perr := strconv.ParseInt(parts[0], 10, 64)
		if perr != nil || version <= 0 {
			log.Fatalf("[db] migration %q: leading version must be a positive integer", e.Name())
		}
		if prev, dup := seen[version]; dup {
			log.Fatalf("[db] migration version %d used twice: %q and %q", version, prev, e.Name())
		}
		seen[version] = e.Name()

		body, rerr := migrationFS.ReadFile(path.Join("migrations", e.Name()))
		if rerr != nil {
			log.Fatalf("[db] cannot read migration %q: %v", e.Name(), rerr)
		}
		sum := sha256.Sum256(body)

		out = append(out, migration{
			version:  version,
			name:     parts[1],
			filename: e.Name(),
			body:     string(body),
			checksum: hex.EncodeToString(sum[:]),
		})
	}

	if len(out) == 0 {
		log.Fatal("[db] no migrations found — the migrations/ directory is empty")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out
}

// MigrateUp brings the database to the latest schema version.
//
// Safe to run on every boot: already-applied versions are skipped, pending ones
// are applied in order. Any failure is fatal — a half-migrated database is not
// a database this process is allowed to serve from.
func MigrateUp(database *sql.DB) {
	migrations := loadMigrations()

	// One migrator at a time, across every process pointed at this database.
	if _, err := database.Exec(`SELECT pg_advisory_lock($1)`, advisoryLockKey); err != nil {
		log.Fatalf("[db] cannot acquire migration lock: %v", err)
	}
	defer func() {
		if _, err := database.Exec(`SELECT pg_advisory_unlock($1)`, advisoryLockKey); err != nil {
			log.Printf("[db] warning: releasing migration lock: %v", err)
		}
	}()

	if _, err := database.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version     BIGINT      PRIMARY KEY,
			name        TEXT        NOT NULL,
			checksum    TEXT        NOT NULL,
			applied_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			duration_ms BIGINT      NOT NULL DEFAULT 0
		)`); err != nil {
		log.Fatalf("[db] cannot create schema_migrations: %v", err)
	}

	applied, maxApplied := loadAppliedVersions(database)

	pending := make([]migration, 0, len(migrations))
	for _, m := range migrations {
		recorded, ok := applied[m.version]
		if !ok {
			// Forward-only: a new file may never sort below an applied one, or the
			// order it was written in is not the order it will run in.
			if m.version < maxApplied {
				log.Fatalf(
					"[db] migration %s is unapplied but sorts below applied version %d — "+
						"migrations are forward-only; renumber it above %d",
					m.filename, maxApplied, maxApplied)
			}
			pending = append(pending, m)
			continue
		}
		if recorded != m.checksum {
			log.Fatalf(
				"[db] migration %s was already applied with a different checksum "+
					"(recorded %s, file %s) — applied migrations are frozen; "+
					"write a new higher-numbered migration instead of editing this one",
				m.filename, recorded[:12], m.checksum[:12])
		}
	}

	// Every recorded version must still have a file, or the schema history has a
	// hole and no one can reason about what this database actually contains.
	known := make(map[int64]bool, len(migrations))
	for _, m := range migrations {
		known[m.version] = true
	}
	for v := range applied {
		if !known[v] {
			log.Fatalf("[db] database records applied migration %d but no such file exists "+
				"— the migration history and the source tree disagree", v)
		}
	}

	if len(pending) == 0 {
		log.Printf("[db] schema up to date at version %d (%d applied)", maxApplied, len(applied))
		return
	}

	for _, m := range pending {
		applyMigration(database, m)
	}
	log.Printf("[db] schema migrated to version %d (%d applied this boot)",
		pending[len(pending)-1].version, len(pending))
}

func loadAppliedVersions(database *sql.DB) (map[int64]string, int64) {
	rows, err := database.Query(`SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		log.Fatalf("[db] cannot read schema_migrations: %v", err)
	}
	defer rows.Close()

	applied := make(map[int64]string)
	var maxApplied int64
	for rows.Next() {
		var v int64
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			log.Fatalf("[db] cannot scan schema_migrations: %v", err)
		}
		applied[v] = sum
		if v > maxApplied {
			maxApplied = v
		}
	}
	if err := rows.Err(); err != nil {
		log.Fatalf("[db] cannot read schema_migrations: %v", err)
	}
	return applied, maxApplied
}

// applyMigration runs one migration and records it, atomically. The migration
// body and its schema_migrations row commit together or not at all, so the
// recorded version can never claim work the database did not do.
func applyMigration(database *sql.DB, m migration) {
	log.Printf("[db] applying migration %d %s", m.version, m.name)
	start := time.Now()

	tx, err := database.Begin()
	if err != nil {
		log.Fatalf("[db] migration %s: cannot begin transaction: %v", m.filename, err)
	}

	if _, err := tx.Exec(m.body); err != nil {
		_ = tx.Rollback()
		log.Fatalf("[db] migration %s failed: %v", m.filename, err)
	}
	if _, err := tx.Exec(
		`INSERT INTO schema_migrations (version, name, checksum, duration_ms)
		 VALUES ($1, $2, $3, $4)`,
		m.version, m.name, m.checksum, time.Since(start).Milliseconds(),
	); err != nil {
		_ = tx.Rollback()
		log.Fatalf("[db] migration %s: cannot record version: %v", m.filename, err)
	}
	if err := tx.Commit(); err != nil {
		log.Fatalf("[db] migration %s: commit failed: %v", m.filename, err)
	}

	log.Printf("[db] applied migration %d %s in %s", m.version, m.name, time.Since(start).Round(time.Millisecond))
}

// SchemaVersion returns the highest applied migration version, or 0 if the
// migration plane has not been initialised. Exposed for /health reporting.
func SchemaVersion(database *sql.DB) int64 {
	var v sql.NullInt64
	if err := database.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0
	}
	if !v.Valid {
		return 0
	}
	return v.Int64
}
