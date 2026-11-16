// Command retire-alexandria closes out the Alexandria catalog.
//
// Alexandria (f33d3r_library) was a second catalog of works. Its titles were
// written by feed-engine's alexandria_catalog obligation, each one a copy of a
// feed-engine work carrying that work's id as legacy_work_id, and nothing on the
// platform ever read a title back. Works are feed-engine's, so no row is
// imported: what the catalog held is either already in f33d3r_feed or was read
// by nobody. Migration 0041 retires the obligation; this command is the one
// reader of f33d3r_library and does the two things a migration cannot do,
// because they cross databases and services:
//
//  1. Verify. It counts the catalog against f33d3r_feed and prints what each
//     table held, so the database can be dropped knowing exactly what it held.
//  2. Retire the naming-plane entries Alexandria minted. A mirrored title named
//     itself uuid:<legacy_work_id>, which is the work's own name, registered by
//     feed-engine and left alone. A title with no legacy_work_id named itself
//     uuid:<title id>, a `work` node no feed-engine work stands behind. With
//     -apply those names are revoked through Manhattan as feed-engine, which is
//     the authority for the uuid namespace, and the command then re-resolves
//     every one of them and fails unless none resolves.
//
// It is idempotent: a name already revoked or never registered counts as
// retired, and a second run reports the same counts and revokes nothing.
//
//	retire-alexandria -library "$LIBRARY_DATABASE_URL" -feed "$DATABASE_URL"          # verify only
//	retire-alexandria -library "$LIBRARY_DATABASE_URL" -feed "$DATABASE_URL" -apply   # verify and revoke
//
// MANHATTAN_URL and INTERNAL_API_KEY are read from the environment for -apply.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/lib/pq"

	"github.com/f33d3r/feed-engine/internal/manhattan"
)

const batch = 1000

func main() {
	var (
		libraryURL   = flag.String("library", os.Getenv("LIBRARY_DATABASE_URL"), "f33d3r_library connection string")
		feedURL      = flag.String("feed", os.Getenv("DATABASE_URL"), "f33d3r_feed connection string")
		manhattanURL = flag.String("manhattan", os.Getenv("MANHATTAN_URL"), "Manhattan base URL (for -apply)")
		apply        = flag.Bool("apply", false, "revoke the names of Alexandria-only titles")
	)
	flag.Parse()
	if *libraryURL == "" || *feedURL == "" {
		flag.Usage()
		os.Exit(2)
	}

	library := open("f33d3r_library", *libraryURL)
	defer library.Close()
	feed := open("f33d3r_feed", *feedURL)
	defer feed.Close()

	ctx := context.Background()
	native, err := verify(ctx, library, feed)
	if err != nil {
		log.Fatal(err)
	}
	if !*apply {
		fmt.Printf("\nverify only: %d Alexandria-only title name(s) would be revoked; rerun with -apply\n", len(native))
		return
	}

	plane := manhattan.New(*manhattanURL)
	if !plane.Configured() {
		log.Fatal("-apply needs Manhattan: pass -manhattan or set MANHATTAN_URL (and INTERNAL_API_KEY)")
	}
	if err := retireNames(ctx, plane, native); err != nil {
		log.Fatal(err)
	}
}

func open(label, url string) *sql.DB {
	d, err := sql.Open("postgres", url)
	if err != nil {
		log.Fatalf("opening %s: %v", label, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.PingContext(ctx); err != nil {
		log.Fatalf("connecting to %s: %v", label, err)
	}
	return d
}

func count(ctx context.Context, d *sql.DB, label, q string) (int64, error) {
	var n int64
	if err := d.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting %s: %w", label, err)
	}
	fmt.Printf("  %-58s %d\n", label, n)
	return n, nil
}

// verify prints what the catalog held against f33d3r_feed and returns the ids
// of the titles that were never a feed-engine work.
func verify(ctx context.Context, library, feed *sql.DB) ([]string, error) {
	fmt.Println("f33d3r_library")
	for _, c := range []struct{ label, q string }{
		{"titles", `SELECT COUNT(*) FROM titles`},
		{"titles, live", `SELECT COUNT(*) FROM titles WHERE tombstoned_at IS NULL`},
		{"titles mirroring a feed-engine work (legacy_work_id set)", `SELECT COUNT(*) FROM titles WHERE legacy_work_id IS NOT NULL`},
		{"titles with no feed-engine work (Alexandria-only)", `SELECT COUNT(*) FROM titles WHERE legacy_work_id IS NULL`},
		{"title tags", `SELECT COUNT(*) FROM title_tags`},
		{"holdings", `SELECT COUNT(*) FROM holdings`},
		{"title_signals rows with any non-zero counter", `SELECT COUNT(*) FROM title_signals
			WHERE like_count + repost_count + reply_count + quote_count + bookmark_count <> 0 OR view_count <> 0`},
		{"title_quotes", `SELECT COUNT(*) FROM title_quotes`},
		{"link_previews", `SELECT COUNT(*) FROM link_previews`},
		{"authors", `SELECT COUNT(*) FROM authors`},
		{"authors with a profile field set", `SELECT COUNT(*) FROM authors
			WHERE display_name <> '' OR avatar_url <> '' OR header_url <> '' OR bio <> ''`},
	} {
		if _, err := count(ctx, library, c.label, c.q); err != nil {
			return nil, err
		}
	}

	// Mirrored titles against the works they copy.
	works, err := stringColumn(ctx, library, `SELECT DISTINCT legacy_work_id::text FROM titles WHERE legacy_work_id IS NOT NULL ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("reading mirrored work ids: %w", err)
	}
	var present int64
	for start := 0; start < len(works); start += batch {
		end := min(start+batch, len(works))
		var n int64
		if err := feed.QueryRowContext(ctx, `SELECT COUNT(*) FROM works WHERE id = ANY($1::uuid[])`,
			pq.Array(works[start:end])).Scan(&n); err != nil {
			return nil, fmt.Errorf("matching mirrored titles to works: %w", err)
		}
		present += n
	}
	fmt.Println("against f33d3r_feed")
	fmt.Printf("  %-58s %d\n", "distinct works the catalog mirrored", len(works))
	fmt.Printf("  %-58s %d\n", "  of which feed-engine still holds the work", present)
	fmt.Printf("  %-58s %d\n", "  of which the work is gone from f33d3r_feed", int64(len(works))-present)

	native, err := stringColumn(ctx, library, `SELECT id::text FROM titles WHERE legacy_work_id IS NULL ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("reading Alexandria-only titles: %w", err)
	}
	// A title id that is also a work id would name a real work. Random uuids do
	// not collide, but the names are revoked on the strength of this check, so
	// it is made rather than assumed.
	var collisions []string
	for start := 0; start < len(native); start += batch {
		end := min(start+batch, len(native))
		ids, err := stringColumn(ctx, feed, `SELECT id::text FROM works WHERE id = ANY($1::uuid[])`, pq.Array(native[start:end]))
		if err != nil {
			return nil, fmt.Errorf("checking Alexandria-only titles against works: %w", err)
		}
		collisions = append(collisions, ids...)
	}
	if len(collisions) > 0 {
		return nil, fmt.Errorf("%d Alexandria-only title id(s) are also feed-engine work ids and their names must not be revoked: %v", len(collisions), collisions)
	}
	return native, nil
}

func stringColumn(ctx context.Context, d *sql.DB, q string, args ...interface{}) ([]string, error) {
	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// retireNames revokes uuid:<title id> for every Alexandria-only title that
// resolves to a work node, then proves none resolves.
func retireNames(ctx context.Context, plane *manhattan.Client, titleIDs []string) error {
	var revoked, absent int
	names := make([]string, 0, len(titleIDs))
	for _, id := range titleIDs {
		name := manhattan.WorkName(id)
		names = append(names, name)
		node, err := plane.Resolve(ctx, name)
		if errors.Is(err, manhattan.ErrNotFound) {
			absent++
			continue
		}
		if err != nil {
			return fmt.Errorf("resolving %s: %w", name, err)
		}
		if node.Kind != manhattan.KindWork {
			return fmt.Errorf("%s resolves to a %s node (%s), not a work; refusing to revoke it", name, node.Kind, node.NodeID)
		}
		err = plane.RevokeName(ctx, name)
		var conflict *manhattan.ConflictError
		switch {
		case err == nil:
			revoked++
		case errors.As(err, &conflict) && conflict.Code == manhattan.ConflictNameAlreadyRevoked:
			absent++
		default:
			return fmt.Errorf("revoking %s: %w", name, err)
		}
	}

	var still int
	for start := 0; start < len(names); start += batch {
		end := min(start+batch, len(names))
		resolved, err := plane.ResolveBatch(ctx, names[start:end])
		if err != nil {
			return fmt.Errorf("re-resolving retired names: %w", err)
		}
		still += len(resolved)
	}
	fmt.Println("Manhattan")
	fmt.Printf("  %-58s %d\n", "Alexandria-only title names", len(names))
	fmt.Printf("  %-58s %d\n", "  revoked by this run", revoked)
	fmt.Printf("  %-58s %d\n", "  already revoked or never registered", absent)
	fmt.Printf("  %-58s %d\n", "  still resolving after this run", still)
	if still != 0 {
		return fmt.Errorf("%d Alexandria-only title name(s) still resolve", still)
	}
	return nil
}
