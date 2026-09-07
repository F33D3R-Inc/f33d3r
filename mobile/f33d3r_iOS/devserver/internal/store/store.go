// Package store is the dev server's database layer: SQLite behind the same
// table and column names feed-engine uses in Postgres.
//
// Every query here has a twin in feed-engine/internal/db. Where the two
// differ it is dialect — JSON text for arrays, INTEGER for booleans, string
// timestamps — and never meaning, so a handler written against this package
// says the same thing it will say against the real database.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"
)

//go:embed schema.sql
var schemaSQL string

// TimeLayout is the fixed-width RFC 3339 form every timestamp column uses.
//
// Fixed width matters: `created_at < ?` is a string comparison in SQLite, and
// Go's RFC3339Nano trims trailing zeros, which would sort "…:00Z" after
// "…:00.5Z". Nine fractional digits and a literal Z make byte order equal
// time order for every value this server writes.
const TimeLayout = "2006-01-02T15:04:05.000000000Z"

// Store is one open database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite file at path and applies the
// schema. The parent directory is created.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("store: mkdir: %w", err)
	}
	dsn := fmt.Sprintf("file:%s?_foreign_keys=on&_busy_timeout=5000&_journal_mode=WAL", path)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	// SQLite has one writer. Serialising through a single connection turns
	// "database is locked" from an error the handlers would have to retry into
	// a queue they never see.
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// schemaVersion is stamped into PRAGMA user_version. schema.sql always
// describes the newest shape; databases created before a column existed get
// the ALTERs below, in order, before the schema runs.
const schemaVersion = 6

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	var hasTables int
	db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'users'`).Scan(&hasTables)
	if hasTables > 0 && version < 2 {
		// v1 → v2: the ledger learned to attribute tips to a live stream.
		if _, err := db.Exec(`ALTER TABLE ledger_entries ADD COLUMN stream_id TEXT`); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	// v2 → v3: works can be bought outright. The new work_purchases table is
	// created by schema.sql's CREATE TABLE IF NOT EXISTS below; no ALTER is
	// needed, and the version bump records that databases at 3 have it.

	// v3 → v4: the 24-hour lane is Visions, not Fleets. The tables are renamed
	// rather than recreated, because schema.sql would otherwise leave the old
	// ones full and the new ones empty — the reader's own posts would silently
	// vanish from a dev database. RENAME carries the rows, the indexes and the
	// foreign keys pointing at them.
	// v4 → v5: Frequencies (live audio rooms). Six new tables, all created by
	// schema.sql's CREATE TABLE IF NOT EXISTS below; no ALTER is needed, and
	// the version bump records that databases at 5 have them.
	if hasTables > 0 && version < 4 {
		for old, renamed := range map[string]string{
			"fleets":           "visions",
			"fleet_views":      "vision_views",
			"fleet_poll_votes": "vision_poll_votes",
			"fleet_replies":    "vision_replies",
			"fleet_mutes":      "vision_mutes",
		} {
			var exists int
			db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, old).Scan(&exists)
			if exists == 0 {
				continue
			}
			if _, err := db.Exec(`ALTER TABLE ` + old + ` RENAME TO ` + renamed); err != nil {
				return fmt.Errorf("renaming %s to %s: %w", old, renamed, err)
			}
		}
	}

	// v5 → v6: the account fields the settings screen edits — birthday
	// visibility, country, social and tip links — and the KYC standing that
	// decides payout. ADD COLUMN has no IF NOT EXISTS in SQLite, so a column
	// already there is tolerated rather than fatal.
	if hasTables > 0 && version < 6 {
		for _, stmt := range []string{
			`ALTER TABLE pial_roots ADD COLUMN kyc_tier TEXT NOT NULL DEFAULT 'none'`,
			`ALTER TABLE pial_roots ADD COLUMN kyc_submitted_at TEXT`,
			`ALTER TABLE user_profiles ADD COLUMN birthday_md_visibility TEXT NOT NULL DEFAULT 'everyone'`,
			`ALTER TABLE user_profiles ADD COLUMN birthday_year_visibility TEXT NOT NULL DEFAULT 'only_me'`,
			`ALTER TABLE user_profiles ADD COLUMN country_code TEXT`,
			`ALTER TABLE user_profiles ADD COLUMN social_links TEXT NOT NULL DEFAULT '{}'`,
			`ALTER TABLE user_profiles ADD COLUMN external_tip_links TEXT NOT NULL DEFAULT '{}'`,
		} {
			if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
				return err
			}
		}
		// The seeded creators are identity-verified (see seedAccounts); a
		// database seeded before the column existed gets the same standing,
		// so payout reads true for them as it would on a fresh start.
		if _, err := db.Exec(`UPDATE pial_roots SET kyc_tier = 'full', kyc_submitted_at = ?
			WHERE kyc_tier = 'none' AND pial_id IN (SELECT pial_id FROM users WHERE tier = 'creator')`, Now()); err != nil {
			return err
		}
	}

	if _, err := db.Exec(schemaSQL); err != nil {
		return err
	}
	_, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion))
	return err
}

// OpenMemory opens a throwaway in-memory database, for tests.
func OpenMemory() (*Store, error) {
	db, err := sql.Open("sqlite3", "file::memory:?_foreign_keys=on")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the connection for the rare caller that needs raw access (tests).
func (s *Store) DB() *sql.DB { return s.db }

// Now is the current instant in TimeLayout.
func Now() string { return FormatTime(time.Now()) }

// FormatTime renders t in TimeLayout, in UTC.
func FormatTime(t time.Time) string { return t.UTC().Format(TimeLayout) }

// ParseTime reads a TimeLayout (or any RFC 3339) string. Zero time on failure.
func ParseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// NewID mints a UUID v4 as the string form every id column holds.
func NewID() string { return uuid.NewString() }

// IsUUID reports whether s is a well-formed UUID. Used where Postgres would
// have rejected a non-UUID with a cast error; here the check has to be explicit.
func IsUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

// RandomHex returns n random bytes as lower-case hex.
func RandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ── JSON array columns ────────────────────────────────────────────────────────

func encodeStrings(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func decodeStrings(raw sql.NullString) []string {
	if !raw.Valid || raw.String == "" {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal([]byte(raw.String), &out); err != nil || out == nil {
		return []string{}
	}
	return out
}

// nullable turns "" into NULL for optional text columns.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return FormatTime(*t)
}

func nullInt(i *int) any {
	if i == nil {
		return nil
	}
	return *i
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// tx runs fn inside a transaction.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}
