package handler

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// RecoverShadowBalances runs at startup and finds AET trapped in shadow accounts.
//
// A shadow account forms when a tip is credited to a user's account UUID instead
// of their PIAL UUID (caused by a broken JOIN query that has since been fixed).
// This function finds every such shadow balance and transfers it to the correct
// PIAL account via Ain Soph /v1/transfer.
//
// A 1% transfer fee applies. This is a one-time cost; once balances are recovered
// subsequent tips go to the correct account directly.
//
// Safe to run repeatedly — accounts with zero shadow balance are skipped instantly.
func RecoverShadowBalances(ctx context.Context, db *sql.DB, ainSophURL string) {
	if db == nil || ainSophURL == "" {
		return
	}

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

		if err := ainSophTransfer(ctx, ainSophURL, u.accountID, u.pialID, shadowBalance); err != nil {
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
func ainSophTransfer(ctx context.Context, ainSophURL, fromID, toID string, amount int64) error {
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
