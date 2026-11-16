package db

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/f33d3r/feed-engine/internal/model"
)

// ─── pure tests (no database) ────────────────────────────────────────────────

// TestVisionLiveClauseIsSingleDefinition pins the one definition of a readable
// Vision. Copies of this clause are what let expired content stay reachable in
// the Work lane; there is exactly one here.
func TestVisionLiveClauseIsSingleDefinition(t *testing.T) {
	const want = `(v.deleted_at IS NULL AND v.expires_at > NOW())`
	if visionLiveSQL != want {
		t.Fatalf("visionLiveSQL = %q, want %q", visionLiveSQL, want)
	}
}

func TestParseVisionAddress(t *testing.T) {
	addr, err := ParseVisionAddress("11111111-1111-1111-1111-111111111111.7")
	if err != nil {
		t.Fatalf("ParseVisionAddress: %v", err)
	}
	if addr.AuthorAccount != "11111111-1111-1111-1111-111111111111" || addr.Seq != 7 {
		t.Fatalf("addr = %+v, want account 11111111-1111-1111-1111-111111111111 seq 7", addr)
	}
	if addr.String() != "11111111-1111-1111-1111-111111111111.7" {
		t.Fatalf("String() = %q", addr.String())
	}
	for _, bad := range []string{"", "no-dot", "acct.", ".7", "acct.7", "11111111-1111-1111-1111-111111111111.notanumber",
		"11111111-1111-1111-1111-111111111111.0", "11111111-1111-1111-1111-111111111111.-1"} {
		if _, err := ParseVisionAddress(bad); err == nil {
			t.Errorf("ParseVisionAddress(%q) accepted, want an error", bad)
		}
	}
}

func TestApplyVisionDefaults(t *testing.T) {
	cases := []struct {
		name    string
		in      VisionInput
		wantErr bool
		check   func(*testing.T, VisionInput)
	}{
		{
			name: "empty text Vision gets the artboard defaults",
			in:   VisionInput{AuthorAccount: "a", AuthorPIAL: "p"},
			check: func(t *testing.T, in VisionInput) {
				if in.ContentType != "text" {
					t.Fatalf("content_type = %q, want text", in.ContentType)
				}
				if in.ArtboardBackground != "void" || in.ArtboardTypeface != "grotesk" {
					t.Fatalf("artboard defaults not applied: %+v", in)
				}
				if in.ArtboardTypeScale != "auto" || in.ArtboardAlign != "center" {
					t.Fatalf("artboard defaults not applied: %+v", in)
				}
				if in.TTL != DefaultVisionTTL {
					t.Fatalf("TTL = %v, want %v", in.TTL, DefaultVisionTTL)
				}
				if string(in.Metadata) != "{}" {
					t.Fatalf("metadata = %q, want {}", in.Metadata)
				}
			},
		},
		{name: "missing author_pial", in: VisionInput{}, wantErr: true},
		{name: "unknown content_type", in: VisionInput{AuthorAccount: "a", AuthorPIAL: "p", ContentType: "hologram"}, wantErr: true},
		{name: "unknown type scale", in: VisionInput{AuthorAccount: "a", AuthorPIAL: "p", ArtboardTypeScale: "huge"}, wantErr: true},
		{name: "unknown align", in: VisionInput{AuthorAccount: "a", AuthorPIAL: "p", ArtboardAlign: "justify"}, wantErr: true},
		{
			name:    "media without object key",
			in:      VisionInput{AuthorAccount: "a", AuthorPIAL: "p", Media: []VisionMediaInput{{}}},
			wantErr: true,
		},
		{
			name: "media kind defaults to image",
			in:   VisionInput{AuthorAccount: "a", AuthorPIAL: "p", Media: []VisionMediaInput{{ObjectKey: "k"}}},
			check: func(t *testing.T, in VisionInput) {
				if in.Media[0].MediaKind != "image" {
					t.Fatalf("media_kind = %q, want image", in.Media[0].MediaKind)
				}
			},
		},
		{name: "unknown media kind", in: VisionInput{AuthorAccount: "a", AuthorPIAL: "p", Media: []VisionMediaInput{{ObjectKey: "k", MediaKind: "hologram"}}}, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			err := in.applyVisionDefaults()
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.check != nil {
				tc.check(t, in)
			}
		})
	}
}

// ─── schema-backed tests ─────────────────────────────────────────────────────
//
// These need a throwaway PostgreSQL named by VISION_TEST_DATABASE_URL. They
// apply the real embedded migrations, so they also prove 0023 applies on top
// of the existing chain. Never point this at a database that holds real rows.

var visionTestSeq atomic.Int64

func visionTestDB(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("VISION_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("VISION_TEST_DATABASE_URL not set — schema-backed Vision tests skipped")
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
	if _, err := database.Exec(`TRUNCATE vision_media, vision_views, visions, vision_lanes CASCADE`); err != nil {
		t.Fatalf("truncating vision tables: %v", err)
	}
	return database
}

func applyMigrationsForTest(t *testing.T, database *sql.DB) {
	t.Helper()
	if _, err := database.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version     BIGINT      PRIMARY KEY,
			name        TEXT        NOT NULL,
			checksum    TEXT        NOT NULL,
			applied_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			duration_ms BIGINT      NOT NULL DEFAULT 0
		)`); err != nil {
		t.Fatalf("creating schema_migrations: %v", err)
	}

	applied := map[int64]bool{}
	rows, err := database.Query(`SELECT version FROM schema_migrations`)
	if err != nil {
		t.Fatalf("reading schema_migrations: %v", err)
	}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			t.Fatalf("scanning schema_migrations: %v", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("reading schema_migrations: %v", err)
	}
	rows.Close()

	for _, m := range loadMigrations() {
		if applied[m.version] {
			continue
		}
		if _, err := database.Exec(m.body); err != nil {
			t.Fatalf("migration %s failed: %v", m.filename, err)
		}
		if _, err := database.Exec(
			`INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
			m.version, m.name, m.checksum); err != nil {
			t.Fatalf("recording migration %s: %v", m.filename, err)
		}
	}
}

// seedVisionUser creates a user with a PIAL root, as every Vision author has.
func seedVisionUser(t *testing.T, database *sql.DB) (userID, pialID string) {
	t.Helper()
	handle := fmt.Sprintf("visiontest_%d_%d", time.Now().UnixNano(), visionTestSeq.Add(1))

	if err := database.QueryRow(
		`INSERT INTO identity_facts (pial_id) VALUES (gen_random_uuid()) RETURNING pial_id::text`).Scan(&pialID); err != nil {
		t.Fatalf("seeding identity_facts: %v", err)
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
	return userID, pialID
}

func seedFollow(t *testing.T, database *sql.DB, followerID, followingID string) {
	t.Helper()
	if _, err := database.Exec(
		`INSERT INTO follows (follower_id, following_id) VALUES ($1::uuid, $2::uuid)`,
		followerID, followingID); err != nil {
		t.Fatalf("seeding follow: %v", err)
	}
}

// expireVision ages a Vision past its window. created_at moves with it,
// because the schema refuses a Vision that expires before it was made.
func expireVision(t *testing.T, database *sql.DB, ref VisionRef) {
	t.Helper()
	res, err := database.Exec(`
		UPDATE visions
		   SET created_at = NOW() - INTERVAL '48 hours',
		       expires_at = NOW() - INTERVAL '24 hours'
		 WHERE author_pial = $1::uuid AND seq = $2`, ref.AuthorPIAL, ref.Seq)
	if err != nil {
		t.Fatalf("expiring vision: %v", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("expiring vision: %v", err)
	}
	if n != 1 {
		t.Fatalf("expiring vision: updated %d rows, want 1", n)
	}
}

// mustInsertVision posts a Vision and lands a clean verdict on it, the way
// safety.events would: a Vision still pending its scan is readable only by its
// author, and these tests are about everything after that.
func mustInsertVision(t *testing.T, database *sql.DB, in VisionInput) VisionRef {
	t.Helper()
	ref, _, err := InsertVision(database, in)
	if err != nil {
		t.Fatalf("InsertVision: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE visions SET scan_state = 'clean' WHERE author_pial = $1::uuid AND seq = $2`,
		ref.AuthorPIAL, ref.Seq); err != nil {
		t.Fatalf("landing a clean verdict: %v", err)
	}
	return ref
}

// wireAddress is the address a client holds for ref.
func wireAddress(t *testing.T, database *sql.DB, ref VisionRef) string {
	t.Helper()
	var a VisionAddress
	if err := database.QueryRow(
		`SELECT author_account::text, account_seq FROM visions WHERE author_pial = $1::uuid AND seq = $2`,
		ref.AuthorPIAL, ref.Seq).Scan(&a.AuthorAccount, &a.Seq); err != nil {
		t.Fatalf("reading wire address: %v", err)
	}
	return a.String()
}

// TestVisionExpiryEnforcedOnDirectFetch is the test the Work lane did not
// have: an expired object must be unreachable by its own address, not merely
// absent from feeds.
func TestVisionExpiryEnforcedOnDirectFetch(t *testing.T) {
	database := visionTestDB(t)
	authorID, authorPIAL := seedVisionUser(t, database)

	ref := mustInsertVision(t, database, VisionInput{
		AuthorAccount: authorID,
		AuthorPIAL:    authorPIAL,
		Body:       "text artboard",
	})

	v, err := GetVisionByRef(database, ref, authorID)
	if err != nil {
		t.Fatalf("GetVisionByRef while live: %v", err)
	}
	if v.Body != "text artboard" || v.ContentType != "text" {
		t.Fatalf("unexpected Vision: %+v", v)
	}

	expireVision(t, database, ref)

	if _, err := GetVisionByRef(database, ref, authorID); !errors.Is(err, ErrVisionNotLive) {
		t.Fatalf("GetVisionByRef after expiry = %v, want ErrVisionNotLive", err)
	}
	visions, err := GetActiveVisionsForAuthor(database, authorID, authorID, 10)
	if err != nil {
		t.Fatalf("GetActiveVisionsForAuthor: %v", err)
	}
	if len(visions) != 0 {
		t.Fatalf("expired Vision still listed: %d rows", len(visions))
	}
	if err := MarkVisionViewed(database, ref, authorID); !errors.Is(err, ErrVisionNotLive) {
		t.Fatalf("MarkVisionViewed on expired Vision = %v, want ErrVisionNotLive", err)
	}
}

func TestMarkVisionViewedIsIdempotent(t *testing.T) {
	database := visionTestDB(t)
	authorID, authorPIAL := seedVisionUser(t, database)
	viewerID, _ := seedVisionUser(t, database)

	ref := mustInsertVision(t, database, VisionInput{AuthorAccount: authorID, AuthorPIAL: authorPIAL, Body: "once"})

	for i := 0; i < 3; i++ {
		if err := MarkVisionViewed(database, ref, viewerID); err != nil {
			t.Fatalf("MarkVisionViewed call %d: %v", i+1, err)
		}
	}

	var views int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM vision_views WHERE author_pial = $1::uuid AND seq = $2`, ref.AuthorPIAL, ref.Seq).Scan(&views); err != nil {
		t.Fatalf("counting views: %v", err)
	}
	if views != 1 {
		t.Fatalf("vision_views rows = %d, want 1", views)
	}

	viewers, err := GetVisionViewers(database, ref, 10)
	if err != nil {
		t.Fatalf("GetVisionViewers: %v", err)
	}
	if len(viewers) != 1 || viewers[0].ViewerAccountID != viewerID {
		t.Fatalf("viewers = %+v, want the one viewer %s", viewers, viewerID)
	}

	if err := MarkVisionViewed(database, ref, ""); err == nil {
		t.Fatal("MarkVisionViewed with no viewer account should fail, got nil")
	}
}

// TestVisionRingStates covers every state the avatar ring can be in. All of
// it is resolved by one query on the server.
func TestVisionRingStates(t *testing.T) {
	database := visionTestDB(t)
	viewerID, _ := seedVisionUser(t, database)

	ringFor := func(t *testing.T, authorID string) *model.VisionRing {
		t.Helper()
		rings, err := GetVisionRingsForViewer(database, viewerID, 50)
		if err != nil {
			t.Fatalf("GetVisionRingsForViewer: %v", err)
		}
		for _, r := range rings {
			if r.AuthorAccountID == authorID {
				return r
			}
		}
		return nil
	}

	t.Run("followed author with no Visions has no ring", func(t *testing.T) {
		authorID, _ := seedVisionUser(t, database)
		seedFollow(t, database, viewerID, authorID)
		if r := ringFor(t, authorID); r != nil {
			t.Fatalf("expected no ring, got %+v", r)
		}
	})

	t.Run("unseen", func(t *testing.T) {
		authorID, authorPIAL := seedVisionUser(t, database)
		seedFollow(t, database, viewerID, authorID)
		ref := mustInsertVision(t, database, VisionInput{AuthorAccount: authorID, AuthorPIAL: authorPIAL, Body: "unseen"})
		r := ringFor(t, authorID)
		if r == nil {
			t.Fatal("expected a ring, got none")
		}
		if r.State != "unseen" || r.Count != 1 || r.UnseenCount != 1 {
			t.Fatalf("ring = %+v, want state unseen, count 1, unseen 1", r)
		}
		if r.NextVisionRef != wireAddress(t, database, ref) {
			t.Fatalf("NextVisionRef = %q, want %q", r.NextVisionRef, wireAddress(t, database, ref))
		}
	})

	t.Run("seen", func(t *testing.T) {
		authorID, authorPIAL := seedVisionUser(t, database)
		seedFollow(t, database, viewerID, authorID)
		ref := mustInsertVision(t, database, VisionInput{AuthorAccount: authorID, AuthorPIAL: authorPIAL, Body: "seen"})
		if err := MarkVisionViewed(database, ref, viewerID); err != nil {
			t.Fatalf("MarkVisionViewed: %v", err)
		}
		r := ringFor(t, authorID)
		if r == nil {
			t.Fatal("expected a ring, got none")
		}
		if r.State != "seen" || r.UnseenCount != 0 {
			t.Fatalf("ring = %+v, want state seen with 0 unseen", r)
		}
		if r.NextVisionRef != wireAddress(t, database, ref) {
			t.Fatalf("NextVisionRef = %q, want the replay start %q", r.NextVisionRef, wireAddress(t, database, ref))
		}
	})

	t.Run("expired Visions drop the ring", func(t *testing.T) {
		authorID, authorPIAL := seedVisionUser(t, database)
		seedFollow(t, database, viewerID, authorID)
		ref := mustInsertVision(t, database, VisionInput{AuthorAccount: authorID, AuthorPIAL: authorPIAL, Body: "gone"})
		expireVision(t, database, ref)
		if r := ringFor(t, authorID); r != nil {
			t.Fatalf("expected no ring after expiry, got %+v", r)
		}
	})

	t.Run("multiple Visions resume at the oldest unseen", func(t *testing.T) {
		authorID, authorPIAL := seedVisionUser(t, database)
		seedFollow(t, database, viewerID, authorID)

		first := mustInsertVision(t, database, VisionInput{AuthorAccount: authorID, AuthorPIAL: authorPIAL, Body: "first"})
		second := mustInsertVision(t, database, VisionInput{AuthorAccount: authorID, AuthorPIAL: authorPIAL, Body: "second"})
		third := mustInsertVision(t, database, VisionInput{AuthorAccount: authorID, AuthorPIAL: authorPIAL, Body: "third"})
		// created_at defaults to NOW() for all three; order them explicitly so
		// "oldest unseen" is a fact and not a tie-break.
		orderVisions(t, database, first, second, third)

		r := ringFor(t, authorID)
		if r == nil {
			t.Fatal("expected a ring, got none")
		}
		if r.Count != 3 || r.UnseenCount != 3 || r.NextVisionRef != wireAddress(t, database, first) {
			t.Fatalf("ring = %+v, want count 3, unseen 3, next %s", r, first)
		}

		if err := MarkVisionViewed(database, first, viewerID); err != nil {
			t.Fatalf("MarkVisionViewed: %v", err)
		}
		r = ringFor(t, authorID)
		if r.State != "unseen" || r.UnseenCount != 2 || r.NextVisionRef != wireAddress(t, database, second) {
			t.Fatalf("ring = %+v, want state unseen, unseen 2, next %s", r, second)
		}

		for _, ref := range []VisionRef{second, third} {
			if err := MarkVisionViewed(database, ref, viewerID); err != nil {
				t.Fatalf("MarkVisionViewed: %v", err)
			}
		}
		r = ringFor(t, authorID)
		if r.State != "seen" || r.UnseenCount != 0 || r.NextVisionRef != wireAddress(t, database, first) {
			t.Fatalf("ring = %+v, want state seen, unseen 0, replay from %s", r, first)
		}
	})
}

// orderVisions spaces created_at so ordering assertions do not depend on how
// fast three inserts ran.
func orderVisions(t *testing.T, database *sql.DB, refs ...VisionRef) {
	t.Helper()
	for i, ref := range refs {
		offset := time.Duration(len(refs)-i) * time.Minute
		if _, err := database.Exec(
			`UPDATE visions SET created_at = NOW() - $3::interval WHERE author_pial = $1::uuid AND seq = $2`,
			ref.AuthorPIAL, ref.Seq, fmt.Sprintf("%d seconds", int(offset.Seconds()))); err != nil {
			t.Fatalf("ordering visions: %v", err)
		}
	}
}

func TestSoftDeleteVisionHidesFromEveryRead(t *testing.T) {
	database := visionTestDB(t)
	authorID, authorPIAL := seedVisionUser(t, database)
	viewerID, _ := seedVisionUser(t, database)
	seedFollow(t, database, viewerID, authorID)

	ref := mustInsertVision(t, database, VisionInput{AuthorAccount: authorID, AuthorPIAL: authorPIAL, Body: "deleted soon"})
	if err := MarkVisionViewed(database, ref, viewerID); err != nil {
		t.Fatalf("MarkVisionViewed: %v", err)
	}

	if err := SoftDeleteVision(database, ref, authorID); err != nil {
		t.Fatalf("SoftDeleteVision: %v", err)
	}

	if _, err := GetVisionByRef(database, ref, viewerID); !errors.Is(err, ErrVisionNotLive) {
		t.Fatalf("GetVisionByRef after delete = %v, want ErrVisionNotLive", err)
	}
	visions, err := GetActiveVisionsForAuthor(database, authorID, viewerID, 10)
	if err != nil {
		t.Fatalf("GetActiveVisionsForAuthor: %v", err)
	}
	if len(visions) != 0 {
		t.Fatalf("deleted Vision still listed: %d rows", len(visions))
	}
	rings, err := GetVisionRingsForViewer(database, viewerID, 50)
	if err != nil {
		t.Fatalf("GetVisionRingsForViewer: %v", err)
	}
	for _, r := range rings {
		if r.AuthorAccountID == authorID {
			t.Fatalf("deleted Vision still rings: %+v", r)
		}
	}
	viewers, err := GetVisionViewers(database, ref, 10)
	if err != nil {
		t.Fatalf("GetVisionViewers: %v", err)
	}
	if len(viewers) != 0 {
		t.Fatalf("viewer list readable after delete: %d rows", len(viewers))
	}
	if err := MarkVisionViewed(database, ref, viewerID); !errors.Is(err, ErrVisionNotLive) {
		t.Fatalf("MarkVisionViewed after delete = %v, want ErrVisionNotLive", err)
	}

	// A delete that matched nothing must say so rather than report success.
	if err := SoftDeleteVision(database, ref, authorID); !errors.Is(err, ErrVisionNotLive) {
		t.Fatalf("second SoftDeleteVision = %v, want ErrVisionNotLive", err)
	}
	otherID, _ := seedVisionUser(t, database)
	live := mustInsertVision(t, database, VisionInput{AuthorAccount: authorID, AuthorPIAL: authorPIAL, Body: "not yours"})
	if err := SoftDeleteVision(database, live, otherID); !errors.Is(err, ErrVisionNotLive) {
		t.Fatalf("deleting another author's Vision = %v, want ErrVisionNotLive", err)
	}
}

func TestVisionMediaPurgeSweep(t *testing.T) {
	database := visionTestDB(t)
	authorID, authorPIAL := seedVisionUser(t, database)

	ref := mustInsertVision(t, database, VisionInput{
		AuthorAccount: authorID,
		AuthorPIAL:    authorPIAL,
		ContentType: "image",
		MediaURLs:   []string{"https://cdn.example/v.webp"},
		Media: []VisionMediaInput{{
			ObjectKey:    "vision/" + authorPIAL + "/v.webp",
			CaeorAssetID: "asset-1",
			AssetURL:     "https://cdn.example/v.webp",
			MediaKind:    "image",
			Width:        1080,
			Height:       1920,
		}},
	})

	due, err := ExpiredVisionMedia(database, 10)
	if err != nil {
		t.Fatalf("ExpiredVisionMedia: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("media due for purge while Vision is live: %d rows", len(due))
	}

	if _, err := database.Exec(
		`UPDATE vision_media SET purge_after = NOW() - INTERVAL '1 hour' WHERE author_pial = $1::uuid AND seq = $2`,
		ref.AuthorPIAL, ref.Seq); err != nil {
		t.Fatalf("ageing vision_media: %v", err)
	}

	due, err = ExpiredVisionMedia(database, 10)
	if err != nil {
		t.Fatalf("ExpiredVisionMedia: %v", err)
	}
	if len(due) != 1 || due[0].CaeorAssetID != "asset-1" {
		t.Fatalf("due = %+v, want the one aged object", due)
	}

	if err := MarkVisionMediaPurged(database, []string{due[0].ID}); err != nil {
		t.Fatalf("MarkVisionMediaPurged: %v", err)
	}
	due, err = ExpiredVisionMedia(database, 10)
	if err != nil {
		t.Fatalf("ExpiredVisionMedia after purge: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("purged media returned again: %d rows", len(due))
	}
}
