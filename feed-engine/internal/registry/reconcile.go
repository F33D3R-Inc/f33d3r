package registry

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"
)

// Reconciling the handles that predate the authority.
//
// Every handle allocated before this brain learned to ask the registrar lives
// only in feed-engine's `users` table. The registrar has no record of them, so
// it never publishes their names, so `handle:<handle>` resolves to nothing in
// Manhattan and every lookup that should have gone through the naming plane
// quietly went nowhere instead.
//
// This is the one-time transfer of those allocations into the brain that owns
// them. It is not a sync and must never become one: it moves an allocation the
// registrar has never heard of, and it never overwrites one the registrar
// already holds. Where the two disagree about who owns a handle, it changes
// nothing and says so — a conflict between two authorities is an incident for a
// person to settle, not a race for a background job to win.
//
// The `users` rows read here are this brain's own. The registrar's tables are
// reached only over the boundary, by name, exactly as everywhere else.

const (
	reconcileRetryInterval = 30 * time.Second
	reconcileBatch         = 1000
)

type localHandle struct {
	handle    string
	pialID    string
	createdAt time.Time
}

// StartReconcile transfers pre-existing handle allocations to the registrar,
// retrying until one full pass completes without a transport failure, then
// returns. Idempotent: once every handle is known to the registrar, a pass is
// all reads and changes nothing, so running it on every boot is free.
func StartReconcile(ctx context.Context, database *sql.DB, client *Client) {
	if database == nil {
		return
	}
	if !client.Configured() {
		// Loud, once. Handles cannot be allocated at all without the registrar,
		// so this is not a degraded mode that quietly works — it is a
		// misconfiguration that will refuse the next signup.
		log.Println("[registry] REGISTRY_BRAIN_URL is not set: handle allocation is unavailable and pre-existing handles cannot be reconciled")
		return
	}

	for {
		transferred, conflicts, err := reconcileOnce(ctx, database, client)
		if err == nil {
			if transferred > 0 || conflicts > 0 {
				log.Printf("[registry] reconcile complete: %d handle(s) transferred to the registrar, %d conflict(s)",
					transferred, conflicts)
			}
			return
		}
		log.Printf("[registry] reconcile could not complete, retrying in %s: %v", reconcileRetryInterval, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(reconcileRetryInterval):
		}
	}
}

func reconcileOnce(ctx context.Context, database *sql.DB, client *Client) (transferred, conflicts int, err error) {
	// Keyset pagination: a single LIMIT would leave every handle past the first
	// batch permanently unreconciled, which is the condition this repair exists
	// to clear. (created_at, handle) is ordered and unique enough to be a cursor.
	const query = `
		SELECT handle, pial_id::text, created_at
		FROM users
		WHERE pial_id IS NOT NULL
		  AND handle IS NOT NULL
		  AND handle <> ''
		  AND ($2::timestamptz IS NULL OR (created_at, handle) > ($2::timestamptz, $3::text))
		ORDER BY created_at, handle
		LIMIT $1`

	var (
		cursorTime   interface{}
		cursorHandle string
	)
	for {
		rows, qerr := database.QueryContext(ctx, query, reconcileBatch, cursorTime, cursorHandle)
		if qerr != nil {
			return transferred, conflicts, qerr
		}

		local := make([]localHandle, 0, reconcileBatch)
		for rows.Next() {
			var h localHandle
			if serr := rows.Scan(&h.handle, &h.pialID, &h.createdAt); serr != nil {
				rows.Close()
				return transferred, conflicts, serr
			}
			local = append(local, h)
		}
		rows.Close()
		if rerr := rows.Err(); rerr != nil {
			return transferred, conflicts, rerr
		}
		if len(local) == 0 {
			return transferred, conflicts, nil
		}

		for _, h := range local {
			moved, conflicted, eerr := EnsureAllocated(ctx, client, h.handle, h.pialID)
			if eerr != nil {
				return transferred, conflicts, eerr
			}
			if moved {
				transferred++
			}
			if conflicted {
				conflicts++
			}
		}

		last := local[len(local)-1]
		cursorTime, cursorHandle = last.createdAt, last.handle
		if len(local) < reconcileBatch {
			return transferred, conflicts, nil
		}
	}
}

// EnsureAllocated makes the registrar aware of a handle that already exists
// locally, and reports what it had to do about it.
//
// It transfers an allocation the registrar has never heard of, and it never
// overwrites one it already holds. `transferred` is true when this call moved
// the allocation; `conflicted` is true when the registrar and this brain
// disagree about who owns the handle, which is logged and otherwise left alone.
// A returned error means the authority could not be reached and nothing is
// known either way.
//
// This is for handles that predate the registrar knowing about them — a repair
// path minting a PIAL for an account that has had its handle for months. A NEW
// handle is never routed through here: it goes to Claim, whose refusal is the
// allocation being denied.
func EnsureAllocated(ctx context.Context, client *Client, handle, pialID string) (transferred, conflicted bool, err error) {
	existing, rerr := client.Resolve(ctx, handle)
	switch {
	case rerr == nil:
		// The registrar already holds this allocation. Agreement is the
		// expected case and is silent; disagreement is not ours to resolve.
		if existing.PIALID != pialID {
			log.Printf("[registry] INCIDENT: handle %q is held by PIAL %s at the registrar but by PIAL %s locally — left untouched, this needs a decision",
				handle, existing.PIALID, pialID)
			return false, true, nil
		}
		return false, false, nil

	case errors.Is(rerr, ErrNotFound):
		// Never allocated at the authority. Transfer it.
		if _, cerr := client.Claim(ctx, handle, pialID); cerr != nil {
			if errors.Is(cerr, ErrTaken) || errors.Is(cerr, ErrReserved) {
				log.Printf("[registry] INCIDENT: handle %q exists locally for PIAL %s but the registrar refuses it (%v) — left untouched, this needs a decision",
					handle, pialID, cerr)
				return false, true, nil
			}
			return false, false, cerr
		}
		return true, false, nil

	default:
		return false, false, rerr
	}
}
