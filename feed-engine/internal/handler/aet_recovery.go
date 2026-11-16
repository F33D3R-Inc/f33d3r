package handler

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

// RecoverShadowBalances returns AET trapped in shadow accounts to its owners.
//
// A shadow account is a wallet keyed on a feed-engine users.id instead of a
// PIAL. Both causes are closed at the source (wallet pages key on the PIAL only,
// and Ain Soph no longer opens an account on a read), so the set is finite and
// historical.
//
// The move goes through Themis, the only door through which value moves, to
// Ain Soph's service-only reattribution: fee-free, because the shadow was this
// platform's mistake; refused if the "shadow" key is a registered identity, so
// it can only ever return a mistake to its owner; and idempotent per shadow
// account. feed-engine supplies only what it alone knows — which account
// belongs to which identity — and moves nothing itself.
//
// It runs when AET_RECOVERY=on, once at boot, in the founder's name.
func (h *Handler) RecoverShadowBalances(ctx context.Context) {
	if h.db == nil {
		return
	}
	if strings.ToLower(strings.TrimSpace(os.Getenv("AET_RECOVERY"))) != "on" {
		return
	}
	founder, err := dbpkg.GetFounder(h.db)
	if err != nil || founder == nil || founder.PIALID == "" {
		log.Printf("aet_recovery: no founder to act in the name of (found: %v): %v — nothing recovered", founder != nil, err)
		return
	}
	rows, err := h.db.QueryContext(ctx,
		`SELECT id::text, pial_id::text FROM users WHERE pial_id IS NOT NULL AND id <> pial_id`)
	if err != nil {
		log.Printf("aet_recovery: reading accounts: %v", err)
		return
	}
	type pair struct{ account, pial string }
	var pairs []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.account, &p.pial); err != nil {
			log.Printf("aet_recovery: reading an account: %v", err)
			continue
		}
		pairs = append(pairs, p)
	}
	if err := rows.Err(); err != nil {
		log.Printf("aet_recovery: reading accounts: %v", err)
	}
	rows.Close()

	log.Printf("aet_recovery: AET_RECOVERY=on — returning shadow balances of %d account(s) to their owners", len(pairs))
	recovered := 0
	for _, p := range pairs {
		status, body, err := h.themisPeerCall(ctx, "/v1/admin/accounts/reattribute", founder, map[string]string{
			"shadow_account": p.account,
			"owner_pial":     p.pial,
		})
		switch {
		case err != nil:
			log.Printf("aet_recovery: account %s: themis unavailable: %v", p.account, err)
		case status == http.StatusOK:
			recovered++
			log.Printf("aet_recovery: account %s's shadow balance returned to its owner", p.account)
		case status == http.StatusUnprocessableEntity:
			// The ledger declined: no shadow balance under this account.
		default:
			log.Printf("aet_recovery: account %s: themis %d: %s", p.account, status, themisRefusal(body, "refused"))
		}
	}
	log.Printf("aet_recovery: done — %d shadow balance(s) returned", recovered)
}
