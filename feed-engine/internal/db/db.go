// Package db provides the PostgreSQL connection pool for the feed engine.
// All queries are parameterized — no string interpolation near SQL ever.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	_ "github.com/lib/pq"
)

// pingTimeout bounds the connectivity check in Open.
const pingTimeout = 5 * time.Second

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

	// Bounded, so a caller retrying a database that is not up yet gets an
	// answer per attempt instead of hanging on one connect. A failed pool is
	// closed here: the caller only ever receives a pool that works.
	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		if cerr := db.Close(); cerr != nil {
			return nil, fmt.Errorf("db.Ping: %w (closing the pool also failed: %v)", err, cerr)
		}
		return nil, fmt.Errorf("db.Ping: %w", err)
	}

	log.Printf("[db] connected: %s", sanitizeURL(url))
	return db, nil
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
