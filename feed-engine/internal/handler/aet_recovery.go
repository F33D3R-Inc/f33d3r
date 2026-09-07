package handler

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// RecoverShadowBalances finds AET trapped in shadow accounts and moves it to the
// owner's PIAL account.
//
// A shadow account is one keyed on a feed-engine users.id instead of a PIAL. The
// two causes are now both closed at the source: the wallet pages no longer fall
// back to user.ID (see walletIdentity), and Ain Soph's balance endpoint no longer
// opens an account merely because something looked one up. That second one is why
// this sweep used to make its own work — probing every users.id created the very
// accounts it then reported as shadows.
//
// It does NOT run automatically any more, for two reasons:
//
//   - It moves users' money. It does so through Ain Soph /v1/transfer, which
//     charges FEE_TRANSFER_BPS — 3%, not the 1% this comment used to claim. A
//     person should not be charged three percent to recover their own balance
//     because of a bug on our side, and certainly not silently, at every boot,
//     with nobody having decided to do it.
//   - With both causes closed, new shadows cannot form, so there is nothing for a
//     recurring sweep to catch. What remains is a finite, historical set.
//
// Set AET_RECOVERY=on to run it once, deliberately, having accepted the fee. It
// is safe to run repeatedly: accounts with a zero balance are skipped, and it
// only ever moves money from an account to its own owner's PIAL.
//
// internalKey authenticates the sweep to Ain Soph. This job moves value between
// two accounts with no session behind it, so it acts as a peer — Ain Soph now
// takes the payer from the authenticated caller and only a peer may name a payer
// other than itself.
func RecoverShadowBalances(ctx context.Context, db *sql.DB, ainSophURL, internalKey string) {
	if db == nil || ainSophURL == "" {
		return
	}
	if strings.ToLower(strings.TrimSpace(os.Getenv("AET_RECOVERY"))) != "on" {
		// Report rather than act. Trapped-and-visible beats silently-taxed.
		if n := countShadowAccounts(ctx, db, ainSophURL); n > 0 {
			log.Printf("aet_recovery: %d account(s) still hold a shadow balance. "+
				"Set AET_RECOVERY=on to sweep them to their owners' PIALs "+
				"(note: Ain Soph charges a 3%% transfer fee on the move).", n)
		}
		return
	}
	log.Printf("aet_recovery: AET_RECOVERY=on — sweeping shadow balances (a 3%% transfer fee applies)")

	rows, err := db.QueryContext(ctx,
		`SELECT id::text, pial_id::text FROM users WHERE pial_id IS NOT NULL`,
	)
	if err != nil {
		log.Printf("aet_recovery: failed to query users: %v", err)
		return
	}
	defer rows.Close()

	type user struct{ accountID, pialID string }
	var candidates []user
	for rows.Next() {
		var u user
		if err := rows.Scan(&u.accountID, &u.pialID); err != nil {
			continue
		}
		if u.accountID == u.pialID {
			continue // already the same — no shadow possible
		}
		candidates = append(candidates, u)
	}

	recovered := 0
	for _, u := range candidates {
		shadowBalance := ainSophBalance(ctx, ainSophURL, u.accountID)
		if shadowBalance <= 0 {
			continue
		}

		log.Printf("aet_recovery: shadow balance %d units on account %s (pial %s) — recovering",
			shadowBalance, u.accountID, u.pialID)

		if err := ainSophTransfer(ctx, ainSophURL, internalKey, u.accountID, u.pialID, shadowBalance); err != nil {
			log.Printf("aet_recovery: transfer failed for %s → %s: %v", u.accountID, u.pialID, err)
			continue
		}
		log.Printf("aet_recovery: recovered %d units → pial %s", shadowBalance, u.pialID)
		recovered++
	}

	if recovered > 0 {
		log.Printf("aet_recovery: done — %d shadow account(s) recovered", recovered)
	}
}

// ainSophBalance returns the balance (in storage units) for the given account ID.
// Returns 0 on any error or if the account doesn't exist.
func ainSophBalance(ctx context.Context, ainSophURL, accountID string) int64 {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		ainSophURL+"/v1/balance/"+accountID, nil)
	if err != nil {
		return 0
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return 0
	}
	defer resp.Body.Close()

	var body struct {
		BalanceUnits int64 `json:"balance_units"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0
	}
	return body.BalanceUnits
}

// ainSophTransfer moves amount units from shadowID → pialID via Ain Soph /v1/transfer.
func ainSophTransfer(ctx context.Context, ainSophURL, internalKey, fromID, toID string, amount int64) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	payload := map[string]interface{}{
		"from_user_id":    fromID,
		"to_user_id":      toID,
		"amount_cents":    amount,
		"idempotency_key": fmt.Sprintf("recovery-%s-%s", fromID, toID),
		"tx_meta":         "shadow_recovery",
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		ainSophURL+"/v1/transfer", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Key", internalKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errBody struct {
			Error string `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&errBody)
		return fmt.Errorf("ain-soph %d: %s", resp.StatusCode, errBody.Error)
	}
	return nil
}

// countShadowAccounts reports how many accounts still hold a balance under a
// feed-engine users.id rather than a PIAL. Read-only: it moves nothing and,
// since Ain Soph's balance endpoint no longer opens an account on lookup,
// creates nothing either.
func countShadowAccounts(ctx context.Context, db *sql.DB, ainSophURL string) int {
	rows, err := db.QueryContext(ctx,
		`SELECT id::text, pial_id::text FROM users WHERE pial_id IS NOT NULL`)
	if err != nil {
		log.Printf("aet_recovery: failed to query users: %v", err)
		return 0
	}
	defer rows.Close()

	n := 0
	for rows.Next() {
		var accountID, pialID string
		if err := rows.Scan(&accountID, &pialID); err != nil {
			continue
		}
		if accountID == pialID {
			continue
		}
		if ainSophBalance(ctx, ainSophURL, accountID) > 0 {
			n++
		}
	}
	return n
}
