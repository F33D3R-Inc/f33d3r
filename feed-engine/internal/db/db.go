// Package db provides the PostgreSQL connection pool for the feed engine.
// All queries are parameterized — no string interpolation near SQL ever.
package db

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	_ "github.com/lib/pq"
)

// Open opens a PostgreSQL connection pool and verifies connectivity.
// url format: postgres://user:pass@host:port/dbname?sslmode=disable
func Open(url string) (*sql.DB, error) {
	if url == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}

	db, err := sql.Open("postgres", url)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}

	// Connection pool settings — Twitter/TikTok production standard
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetConnMaxIdleTime(2 * time.Minute)

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("db.Ping: %w", err)
	}

	log.Printf("[db] connected: %s", sanitizeURL(url))
	return db, nil
}

// MustOpen opens a DB connection or fatals. Use in main() only.
func MustOpen(url string) *sql.DB {
	db, err := Open(url)
	if err != nil {
		log.Fatalf("[db] fatal: %v", err)
	}
	return db
}

// sanitizeURL strips password from connection string for logging.
func sanitizeURL(url string) string {
	// Simple sanitiser — show up to the @ sign
	for i, c := range url {
		if c == '@' {
			return "postgres://***@" + url[i+1:]
		}
	}
	return "postgres://***"
}
