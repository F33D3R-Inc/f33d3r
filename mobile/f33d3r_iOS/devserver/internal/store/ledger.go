package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// This file stands in for Ain Soph, the wallet brain. Amounts are µAET.

// UAETPerAET is the unit: 1 AET = 1,000,000 µAET.
const UAETPerAET = 1_000_000

// ErrInsufficientBalance is a tip the sender cannot cover.
var ErrInsufficientBalance = errors.New("insufficient balance")

// LedgerEntry is one movement, from the owner's point of view.
type LedgerEntry struct {
	ID                 string
	PIALID             string
	Kind               string
	AmountUAET         int64
	CounterpartyPIAL   string
	CounterpartyHandle string
	WorkID             string
	Status             string
	CreatedAt          time.Time
}

// Balance is what the wallet shows: settled and pending, never summed.
type Balance struct {
	SettledUAET int64
	PendingUAET int64
}

// Balance returns the two figures for a PIAL.
func (s *Store) Balance(ctx context.Context, pial string) (Balance, error) {
	var b Balance
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(CASE WHEN status = 'settled' THEN amount_uaet ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status = 'pending' THEN amount_uaet ELSE 0 END), 0)
		FROM ledger_entries WHERE pial_id = ?`, pial).Scan(&b.SettledUAET, &b.PendingUAET)
	return b, err
}

// Entries lists a PIAL's ledger newest first, with counterparty handles
// resolved.
func (s *Store) Entries(ctx context.Context, pial string, limit int) ([]LedgerEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT le.id, le.pial_id, le.kind, le.amount_uaet, COALESCE(le.counterparty_pial, ''), COALESCE(cu.handle, ''),
		       COALESCE(le.work_id, ''), le.status, le.created_at
		FROM ledger_entries le
		LEFT JOIN users cu ON cu.pial_id = le.counterparty_pial
		WHERE le.pial_id = ?
		ORDER BY le.created_at DESC LIMIT ?`, pial, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LedgerEntry
	for rows.Next() {
		var e LedgerEntry
		var created string
		if err := rows.Scan(&e.ID, &e.PIALID, &e.Kind, &e.AmountUAET, &e.CounterpartyPIAL, &e.CounterpartyHandle, &e.WorkID, &e.Status, &created); err != nil {
			return nil, err
		}
		e.CreatedAt = ParseTime(created)
		out = append(out, e)
	}
	if out == nil {
		out = []LedgerEntry{}
	}
	return out, rows.Err()
}

// Credit adds a settled credit of any kind — the seeder's airdrop, a payout.
func (s *Store) Credit(ctx context.Context, pial, kind string, amountUAET int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO ledger_entries (id, pial_id, kind, amount_uaet, status, created_at) VALUES (?, ?, ?, ?, 'settled', ?)`,
		NewID(), pial, kind, amountUAET, FormatTime(at))
	return err
}

// Tip moves amountUAET from one PIAL to another atomically: a tip_sent debit
// on the sender, a tip_received credit on the recipient, both settled. The
// sender's settled balance must cover it.
func (s *Store) Tip(ctx context.Context, fromPIAL, toPIAL, workID string, amountUAET int64) error {
	if amountUAET <= 0 {
		return errors.New("invalid amount")
	}
	if fromPIAL == toPIAL {
		return errors.New("cannot tip yourself")
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var settled int64
		if err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(amount_uaet), 0) FROM ledger_entries WHERE pial_id = ? AND status = 'settled'`, fromPIAL).Scan(&settled); err != nil {
			return err
		}
		if settled < amountUAET {
			return ErrInsufficientBalance
		}
		now := Now()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ledger_entries (id, pial_id, kind, amount_uaet, counterparty_pial, work_id, status, created_at)
			VALUES (?, ?, 'tip_sent', ?, ?, ?, 'settled', ?)`,
			NewID(), fromPIAL, -amountUAET, toPIAL, nullable(workID), now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO ledger_entries (id, pial_id, kind, amount_uaet, counterparty_pial, work_id, status, created_at)
			VALUES (?, ?, 'tip_received', ?, ?, ?, 'settled', ?)`,
			NewID(), toPIAL, amountUAET, fromPIAL, nullable(workID), now)
		return err
	})
}

// ErrAlreadyPurchased is a buyer who already owns the work. Not a failure the
// handler reports: the row that would be written is already there, so the
// event is complete, and a retry after a lost response must not charge twice.
var ErrAlreadyPurchased = errors.New("already purchased")

// ErrNotForSale is a work with no price.
var ErrNotForSale = errors.New("not for sale")

// ErrOwnWork is an author trying to buy their own work.
var ErrOwnWork = errors.New("cannot buy your own work")

// Purchase buys a priced work outright, atomically: an `unlock` debit on the
// buyer, a `sale` credit on the author, both settled and both attributed to
// the work, and the entitlement row in work_purchases. The price is read
// inside the transaction from the work itself — the client never says what
// something costs, it only says which work.
//
// Same shape as Tip on purpose: F33D3R never holds the money, so the debit and
// the credit are one write or none.
func (s *Store) Purchase(ctx context.Context, buyerID, buyerPIAL, workID string) (int64, error) {
	if buyerID == "" || buyerPIAL == "" || workID == "" {
		return 0, errors.New("invalid purchase")
	}
	var price int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var authorPIAL string
		err := tx.QueryRowContext(ctx, `
			SELECT author_pial, price_uaet FROM works WHERE id = ? AND deleted_at IS NULL`, workID).Scan(&authorPIAL, &price)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if price <= 0 {
			return ErrNotForSale
		}
		if authorPIAL == buyerPIAL {
			return ErrOwnWork
		}
		var owned int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM work_purchases WHERE work_id = ? AND buyer_id = ?`, workID, buyerID).Scan(&owned); err != nil {
			return err
		}
		if owned > 0 {
			return ErrAlreadyPurchased
		}
		var settled int64
		if err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(amount_uaet), 0) FROM ledger_entries WHERE pial_id = ? AND status = 'settled'`, buyerPIAL).Scan(&settled); err != nil {
			return err
		}
		if settled < price {
			return ErrInsufficientBalance
		}
		now := Now()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ledger_entries (id, pial_id, kind, amount_uaet, counterparty_pial, work_id, status, created_at)
			VALUES (?, ?, 'unlock', ?, ?, ?, 'settled', ?)`,
			NewID(), buyerPIAL, -price, authorPIAL, workID, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ledger_entries (id, pial_id, kind, amount_uaet, counterparty_pial, work_id, status, created_at)
			VALUES (?, ?, 'sale', ?, ?, ?, 'settled', ?)`,
			NewID(), authorPIAL, price, buyerPIAL, workID, now); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO work_purchases (work_id, buyer_id, buyer_pial, author_pial, amount_uaet, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`, workID, buyerID, buyerPIAL, authorPIAL, price, now)
		return err
	})
	return price, err
}
