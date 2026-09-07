package realm

import (
	"context"
	"database/sql"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ── The realm scale ───────────────────────────────────────────────────────────

// Min, Max and Default define the realm scale in ONE place. Every renderer,
// query and badge derives its bounds from these; nothing restates them.
//
// Default is what an account wears when its realm is missing, zero, or outside
// the scale. It is Min deliberately: realm is earned, and a value we cannot
// trust must never be allowed to display as earned standing. R1 draws no ring,
// so a degraded value is also visually indistinguishable from a new account
// rather than from a Guardian.
const (
	Min     = 1
	Max     = 5
	Default = Min
)

// Clamp is the ONE place a missing or out-of-range realm degrades to a defined
// value. No template, query or view struct may re-decide this: they call Clamp
// (directly, or through Index.Of) and take what it returns.
func Clamp(r int) int {
	if r < Min || r > Max {
		return Default
	}
	return r
}

// ── The index ─────────────────────────────────────────────────────────────────

// Index is the authoritative answer to "what realm does this handle wear".
//
// It exists because realm is an attribute of the author, not of the surface,
// and every surface that draws an avatar was previously required to carry it by
// hand — twenty call sites, each free to pass 0, 1, "" or nothing at all, and
// almost all of them did. Carrying an attribute through twenty view structs is
// not a plumbing problem that can be fixed once; it is a defect that returns
// with the next surface someone adds. So the ring stops being an input to the
// avatar Facet and becomes something the avatar Facet resolves for itself from
// the handle it must already have in order to render at all.
//
// The index holds only the accounts ABOVE Default. R1 is the floor everybody
// starts on and draws no ring, so "absent from the map" and "realm 1" are the
// same statement — which keeps the resident set proportional to the accounts
// that have actually earned standing rather than to the user table.
//
// Reads are a map lookup and never touch the database: a fifty-row feed costs
// fifty map lookups and zero queries. The map is loaded whole by one query and
// kept current from two directions — the XP write path updates the one row it
// changed the moment it changes it, and a reconcile pass reloads the set on an
// interval so an out-of-band write (an admin grant, a migration, another
// process) converges without anyone having to remember to invalidate anything.
type Index struct {
	db      *sql.DB
	refresh time.Duration

	mu    sync.RWMutex
	above map[string]int // lowercased handle → realm, only realms > Default
}

// live is the process's index. It is a package singleton on purpose: the avatar
// Facet resolves the ring through a template function, and a template function
// has no request, no handler and no receiver to reach a dependency through. One
// index, owned by the package that owns the realm scale, is the only shape that
// lets the Facet be self-sufficient — which is the whole point of the change.
var (
	liveMu sync.RWMutex
	live   *Index
)

// Start builds the process index, performs the first load synchronously so the
// first rendered page is already correct, and runs the reconcile loop until ctx
// is cancelled. Calling it with a nil db is legal and yields an index that
// answers Default for everything — that is the case in render tests, which have
// no database and must still render every Facet.
func Start(ctx context.Context, db *sql.DB, refresh time.Duration) *Index {
	if refresh <= 0 {
		refresh = 30 * time.Second
	}
	ix := &Index{db: db, refresh: refresh, above: map[string]int{}}

	liveMu.Lock()
	live = ix
	liveMu.Unlock()

	if db == nil {
		return ix
	}
	if err := ix.Reload(ctx); err != nil {
		// A failed first load is not fatal: every handle answers Default until
		// the reconcile pass succeeds. It is logged loudly because a ring that
		// silently flattens to R1 platform-wide must not look like a design.
		log.Printf("[realm] index: first load failed, every ring reads R%d until reconcile: %v", Default, err)
	}
	go ix.reconcile(ctx)
	return ix
}

// Of resolves the realm worn by a handle through the process index. It is the
// single read path: the avatar Facet, the badge and every server renderer ask
// here rather than trusting a value threaded through a view struct.
func Of(handle string) int {
	liveMu.RLock()
	ix := live
	liveMu.RUnlock()
	if ix == nil {
		return Default
	}
	return ix.Of(handle)
}

// Of resolves the realm worn by a handle. Unknown, empty and out-of-scale all
// answer Default, decided by Clamp and nowhere else.
func (ix *Index) Of(handle string) int {
	key := normaliseHandle(handle)
	if key == "" {
		return Default
	}
	ix.mu.RLock()
	r, ok := ix.above[key]
	ix.mu.RUnlock()
	if !ok {
		return Default
	}
	return Clamp(r)
}

// Note records the realm an account now holds. The XP write path calls it the
// moment it changes a realm, so the next render is correct without waiting for
// the reconcile pass. A realm at or below Default is a removal, not a store:
// the map holds earned standing only.
func (ix *Index) Note(handle string, r int) {
	key := normaliseHandle(handle)
	if key == "" {
		return
	}
	r = Clamp(r)
	ix.mu.Lock()
	if r > Default {
		ix.above[key] = r
	} else {
		delete(ix.above, key)
	}
	ix.mu.Unlock()
}

// NoteUser records the realm a user id now holds, resolving the handle it is
// rendered under. It runs on the XP write path, never on a render path.
func NoteUser(db *sql.DB, userID string) {
	liveMu.RLock()
	ix := live
	liveMu.RUnlock()
	if ix == nil || db == nil || userID == "" {
		return
	}
	var handle string
	var r int
	if err := db.QueryRow(`SELECT COALESCE(handle,''), COALESCE(realm, $2) FROM users WHERE id = $1`,
		userID, Default).Scan(&handle, &r); err != nil {
		if err != sql.ErrNoRows {
			log.Printf("[realm] index: note user %s: %v", userID, err)
		}
		return
	}
	ix.Note(handle, r)
}

// Reload replaces the whole set in one query. It is the reconcile half of the
// design: whatever wrote users.realm, and whichever process wrote it, the index
// converges on the table within one interval.
func (ix *Index) Reload(ctx context.Context) error {
	if ix.db == nil {
		return nil
	}
	rows, err := ix.db.QueryContext(ctx, `
		SELECT LOWER(handle), realm
		FROM users
		WHERE realm > $1 AND handle IS NOT NULL AND handle <> ''
	`, Default)
	if err != nil {
		return err
	}
	defer rows.Close()

	next := make(map[string]int, len(ix.above)+16)
	for rows.Next() {
		var h string
		var r int
		if err := rows.Scan(&h, &r); err != nil {
			return err
		}
		if r = Clamp(r); r > Default {
			next[h] = r
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	ix.mu.Lock()
	ix.above = next
	ix.mu.Unlock()
	return nil
}

// Size reports how many accounts currently hold standing above Default. It is
// the number to watch: the index is proportional to it, not to the user table.
func (ix *Index) Size() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.above)
}

func (ix *Index) reconcile(ctx context.Context) {
	t := time.NewTicker(ix.refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := ix.Reload(ctx); err != nil && ctx.Err() == nil {
				log.Printf("[realm] index: reconcile failed, serving last good set: %v", err)
			}
		}
	}
}

// normaliseHandle matches how handles are compared everywhere else: a handle is
// case-insensitive and is sometimes rendered with its leading @.
func normaliseHandle(h string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(h), "@"))
}

// RingClass is the complete realm-ring class suffix for a handle's avatar,
// including the leading space, or "" when the account is on the floor and wears
// no ring.
//
// Both halves of the ring decision live here rather than in the template: what
// realm the handle wears, and whether that realm draws anything at all. A Facet
// interpolates the result; it never compares a realm to a number, so the floor
// is defined once, in Go, next to Clamp.
func RingClass(handle string) string {
	r := Of(handle)
	if r <= Default {
		return ""
	}
	return " realm-ring realm-ring--r" + strconv.Itoa(r)
}
