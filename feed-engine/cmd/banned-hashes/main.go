// Command banned-hashes loads a hash list into banned_content_hashes.
//
// Input is one record per line: HASH, or HASH,CATEGORY, or TYPE,HASH,CATEGORY.
// Blank lines and lines beginning with # are ignored. Loading is idempotent.
//
//	banned-hashes -file ncmec.txt -type sha256 -category csam -source "NCMEC 2026-09"
package main

import (
	"bufio"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	_ "github.com/lib/pq"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

// hashLen is the expected hex length per type. A list loaded under the wrong
// type silently matches nothing, so length is checked before insert.
var hashLen = map[string]int{"sha256": 64, "phash": 16, "audio_fp": 0}

func main() {
	var (
		file     = flag.String("file", "", "path to the hash list (required)")
		hashType = flag.String("type", "sha256", "sha256 | phash | audio_fp")
		category = flag.String("category", "", "category when a line omits one (required)")
		source   = flag.String("source", "", "provenance recorded on every row (required)")
		dsn      = flag.String("dsn", os.Getenv("DATABASE_URL"), "postgres DSN")
		dryRun   = flag.Bool("dry-run", false, "parse and validate without writing")
	)
	flag.Parse()

	if *file == "" || *category == "" || *source == "" {
		flag.Usage()
		os.Exit(2)
	}
	if _, ok := hashLen[*hashType]; !ok {
		log.Fatalf("unknown hash type %q: want sha256, phash or audio_fp", *hashType)
	}
	if *dsn == "" {
		log.Fatal("no DSN: pass -dsn or set DATABASE_URL")
	}

	f, err := os.Open(*file)
	if err != nil {
		log.Fatalf("opening %s: %v", *file, err)
	}
	defer f.Close()

	var database *sql.DB
	if !*dryRun {
		if database, err = sql.Open("postgres", *dsn); err != nil {
			log.Fatalf("connecting: %v", err)
		}
		defer database.Close()
		if err := database.Ping(); err != nil {
			log.Fatalf("connecting: %v", err)
		}
	}

	var loaded, skipped, bad int
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for line := 1; sc.Scan(); line++ {
		typ, val, cat, err := parseLine(sc.Text(), *hashType, *category)
		if err != nil {
			log.Printf("%s:%d: %v", *file, line, err)
			bad++
			continue
		}
		if val == "" {
			skipped++
			continue
		}
		if want := hashLen[typ]; want > 0 && len(val) != want {
			log.Printf("%s:%d: %s hash is %d chars, want %d — refusing (a wrong-typed list matches nothing)",
				*file, line, typ, len(val), want)
			bad++
			continue
		}
		if *dryRun {
			loaded++
			continue
		}
		if err := dbpkg.AddBannedHash(database, typ, val, cat, "banned-hashes", *source); err != nil {
			log.Printf("%s:%d: %v", *file, line, err)
			bad++
			continue
		}
		loaded++
	}
	if err := sc.Err(); err != nil {
		log.Fatalf("reading %s: %v", *file, err)
	}

	fmt.Printf("loaded=%d skipped=%d rejected=%d dry_run=%t\n", loaded, skipped, bad, *dryRun)
	if bad > 0 {
		os.Exit(1)
	}
}

// parseLine reads HASH, HASH,CATEGORY or TYPE,HASH,CATEGORY.
func parseLine(raw, defType, defCat string) (typ, val, cat string, err error) {
	s := strings.TrimSpace(raw)
	if s == "" || strings.HasPrefix(s, "#") {
		return "", "", "", nil
	}
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	switch len(parts) {
	case 1:
		typ, val, cat = defType, parts[0], defCat
	case 2:
		typ, val, cat = defType, parts[0], parts[1]
	case 3:
		typ, val, cat = parts[0], parts[1], parts[2]
	default:
		return "", "", "", fmt.Errorf("want 1-3 comma-separated fields, got %d", len(parts))
	}
	if _, ok := hashLen[typ]; !ok {
		return "", "", "", fmt.Errorf("unknown hash type %q", typ)
	}
	if val == "" {
		return "", "", "", fmt.Errorf("empty hash")
	}
	if cat == "" {
		return "", "", "", fmt.Errorf("no category (line omits one and -category is unset)")
	}
	return typ, strings.ToLower(val), cat, nil
}
