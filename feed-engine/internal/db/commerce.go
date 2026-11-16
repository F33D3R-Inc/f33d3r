package db

// commerce.go — the read projection of Themis's plans and subscriptions.
//
// Themis is the only writer of plans and subscriptions. creator_plans and
// subscriptions here are kept current from commerce.events and nothing else:
// every write below is the apply of one event, guarded by the subject's seq in
// commerce_subject_seq inside the same transaction, so a redelivered or older
// event never overwrites a newer state. Reads (IsSubscribed, GetCreatorPlans,
// GetSubscriberCount) are local and fast, which is why the feed, fanout and
// gating queries can keep joining on these tables.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"
)

// ErrCommerceUnapplicable marks an event that can never apply here: it names
// an account this brain does not hold. The caller dead-letters it.
var ErrCommerceUnapplicable = errors.New("commerce event cannot apply")

// CommerceSubscriptionState is a subscription as Themis announced it.
type CommerceSubscriptionState struct {
	ID                string
	SubscriberAccount string
	CreatorAccount    string
	PlanID            string
	Status            string // active | cancelled | expired
	PeriodEnd         time.Time
	CancelledAt       *time.Time
	PriceUnits        int64
}

// CommercePlanState is a plan as Themis announced it.
type CommercePlanState struct {
	ID             string
	CreatorAccount string
	Name           string
	Description    string
	PriceUnits     int64
	IsActive       bool
}

// claimCommerceSeq records seq as the subject's latest inside tx. False means
// an event at or after seq was already applied, and this one is skipped.
func claimCommerceSeq(ctx context.Context, tx *sql.Tx, kind, id string, seq int64, eventID string) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, `
		INSERT INTO commerce_subject_seq (subject_kind, subject_id, last_seq, last_event_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (subject_kind, subject_id) DO UPDATE
		   SET last_seq = EXCLUDED.last_seq, last_event_id = EXCLUDED.last_event_id, applied_at = NOW()
		 WHERE commerce_subject_seq.last_seq < EXCLUDED.last_seq
		RETURNING 1`, kind, id, seq, eventID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func accountsExist(ctx context.Context, tx *sql.Tx, ids ...string) error {
	for _, id := range ids {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM users WHERE id::text = $1)`, id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: account %s is not held here", ErrCommerceUnapplicable, id)
		}
	}
	return nil
}

// ApplyCommerceSubscription brings the projection's row for one (subscriber,
// creator) pair up to the state Themis announced.
func ApplyCommerceSubscription(ctx context.Context, database *sql.DB, subjectID string, seq int64, eventID string, s CommerceSubscriptionState) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	fresh, err := claimCommerceSeq(ctx, tx, "subscription", subjectID, seq, eventID)
	if err != nil || !fresh {
		return err
	}
	if err := accountsExist(ctx, tx, s.SubscriberAccount, s.CreatorAccount); err != nil {
		return err
	}
	// One row per pair: a later subscription between the same two accounts
	// replaces the earlier one's row, id included.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM subscriptions
		 WHERE id = $1::uuid AND NOT (subscriber_id = $2::uuid AND creator_id = $3::uuid)`,
		s.ID, s.SubscriberAccount, s.CreatorAccount); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO subscriptions (id, subscriber_id, creator_id, plan_id, price_units, expires_at, status, cancelled_at)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7, $8)
		ON CONFLICT (subscriber_id, creator_id) DO UPDATE SET
			id           = EXCLUDED.id,
			plan_id      = EXCLUDED.plan_id,
			price_units  = EXCLUDED.price_units,
			expires_at   = EXCLUDED.expires_at,
			status       = EXCLUDED.status,
			cancelled_at = EXCLUDED.cancelled_at`,
		s.ID, s.SubscriberAccount, s.CreatorAccount, s.PlanID, s.PriceUnits, s.PeriodEnd, s.Status, s.CancelledAt); err != nil {
		return err
	}
	return tx.Commit()
}

// ApplyCommercePlan brings the projection's row for one plan up to the state
// Themis announced.
func ApplyCommercePlan(ctx context.Context, database *sql.DB, seq int64, eventID string, p CommercePlanState) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	fresh, err := claimCommerceSeq(ctx, tx, "plan", p.ID, seq, eventID)
	if err != nil || !fresh {
		return err
	}
	if err := accountsExist(ctx, tx, p.CreatorAccount); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO creator_plans (id, creator_id, name, description, price_units, active)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6)
		ON CONFLICT (id) DO UPDATE SET
			creator_id  = EXCLUDED.creator_id,
			name        = EXCLUDED.name,
			description = EXCLUDED.description,
			price_units = EXCLUDED.price_units,
			active      = EXCLUDED.active`,
		p.ID, p.CreatorAccount, p.Name, p.Description, p.PriceUnits, p.IsActive); err != nil {
		return err
	}
	return tx.Commit()
}

// ClaimCommerceEvent records an event that changes no projected row (a
// delivered purchase, which only redraws). False means it was already handled.
func ClaimCommerceEvent(ctx context.Context, database *sql.DB, kind, id string, seq int64, eventID string) (bool, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	fresh, err := claimCommerceSeq(ctx, tx, kind, id, seq, eventID)
	if err != nil {
		return false, err
	}
	return fresh, tx.Commit()
}

// RecordCommerceDeadLetter keeps a commerce.events record that can never be
// applied.
func RecordCommerceDeadLetter(ctx context.Context, database *sql.DB, topic string, partition int32, offset int64, record []byte, reason string) error {
	_, err := database.ExecContext(ctx, `
		INSERT INTO commerce_event_deadletter (topic, partition, "offset", record, reason)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (topic, partition, "offset") DO NOTHING`,
		topic, partition, offset, record, reason)
	return err
}

// ── Reads ─────────────────────────────────────────────────────────────────────

// IsSubscribed reports whether subscriberID holds a paid, unexpired
// subscription to creatorID. A database fault is an error, never "not
// subscribed": the caller fails closed with its own unavailable answer.
func IsSubscribed(database *sql.DB, subscriberID, creatorID string) (bool, error) {
	if database == nil {
		return false, errors.New("subscriptions: no database")
	}
	var exists bool
	err := database.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM subscriptions
			WHERE subscriber_id = $1 AND creator_id = $2
			  AND status = 'active'
			  AND expires_at > NOW()
		)`, subscriberID, creatorID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("subscriptions: %w", err)
	}
	return exists, nil
}

// CreatorPlan is a plan as the projection holds it.
type CreatorPlan struct {
	ID          string
	Name        string
	Description string
	PriceUnits  int64
}

// GetCreatorPlans lists a creator's active plans, cheapest first.
func GetCreatorPlans(database *sql.DB, creatorID string) ([]CreatorPlan, error) {
	rows, err := database.Query(`
		SELECT id::text, name, description, price_units
		FROM creator_plans
		WHERE creator_id = $1 AND active = TRUE
		ORDER BY price_units ASC
	`, creatorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var plans []CreatorPlan
	for rows.Next() {
		var p CreatorPlan
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.PriceUnits); err != nil {
			return nil, err
		}
		plans = append(plans, p)
	}
	return plans, rows.Err()
}

// GetSubscriberCount is how many paid, unexpired subscribers a creator has.
func GetSubscriberCount(database *sql.DB, creatorID string) int {
	var n int
	if err := database.QueryRow(`
		SELECT COUNT(*) FROM subscriptions
		WHERE creator_id = $1 AND status = 'active' AND expires_at > NOW()
	`, creatorID).Scan(&n); err != nil {
		// A count shown on a dashboard: an unreadable one is reported, and
		// shown as zero rather than taking the page down.
		log.Printf("[subscriptions] count for creator %s: %v", creatorID, err)
	}
	return n
}

// ── The one-time import into Themis ───────────────────────────────────────────

// LegacyPlan is a creator_plans row as the import sends it to Themis.
type LegacyPlan struct {
	ID             string
	CreatorAccount string
	CreatorPIAL    string
	Name           string
	Description    string
	PriceUnits     int64
	IsActive       bool
	CreatedAt      time.Time
}

// LegacySubscription is a subscriptions row as the import sends it to Themis.
type LegacySubscription struct {
	ID                string
	SubscriberAccount string
	SubscriberPIAL    string
	CreatorAccount    string
	CreatorPIAL       string
	PlanID            string
	Status            string
	PeriodEnd         time.Time
	CancelledAt       *time.Time
	CreatedAt         time.Time
}

// LegacyCommerceRows reads every plan and subscription this brain holds whose
// people have identities, for the one-time import into Themis, plus how many
// rows were left out because an account has no identity.
func LegacyCommerceRows(ctx context.Context, database *sql.DB) (plans []LegacyPlan, subs []LegacySubscription, skipped int, err error) {
	rows, err := database.QueryContext(ctx, `
		SELECT p.id::text, p.creator_id::text, COALESCE(u.pial_id::text, ''), p.name, p.description,
		       p.price_units, p.active, p.created_at
		  FROM creator_plans p JOIN users u ON u.id = p.creator_id
		 ORDER BY p.created_at, p.id`)
	if err != nil {
		return nil, nil, 0, err
	}
	for rows.Next() {
		var p LegacyPlan
		if err := rows.Scan(&p.ID, &p.CreatorAccount, &p.CreatorPIAL, &p.Name, &p.Description,
			&p.PriceUnits, &p.IsActive, &p.CreatedAt); err != nil {
			rows.Close()
			return nil, nil, 0, err
		}
		if p.CreatorPIAL == "" {
			skipped++
			continue
		}
		plans = append(plans, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, 0, err
	}
	rows.Close()

	rows, err = database.QueryContext(ctx, `
		SELECT s.id::text, s.subscriber_id::text, COALESCE(su.pial_id::text, ''),
		       s.creator_id::text, COALESCE(cu.pial_id::text, ''), s.plan_id,
		       s.status, s.expires_at, s.cancelled_at, s.created_at
		  FROM subscriptions s
		  JOIN users su ON su.id = s.subscriber_id
		  JOIN users cu ON cu.id = s.creator_id
		 ORDER BY s.created_at, s.id`)
	if err != nil {
		return nil, nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var s LegacySubscription
		if err := rows.Scan(&s.ID, &s.SubscriberAccount, &s.SubscriberPIAL, &s.CreatorAccount, &s.CreatorPIAL,
			&s.PlanID, &s.Status, &s.PeriodEnd, &s.CancelledAt, &s.CreatedAt); err != nil {
			return nil, nil, 0, err
		}
		if s.SubscriberPIAL == "" || s.CreatorPIAL == "" {
			skipped++
			continue
		}
		subs = append(subs, s)
	}
	return plans, subs, skipped, rows.Err()
}

// RecordCommerceImport keeps what one import run sent and Themis confirmed.
func RecordCommerceImport(ctx context.Context, database *sql.DB, plansSent, plansPresent, subsSent, subsPresent int64, verified bool) error {
	_, err := database.ExecContext(ctx, `
		INSERT INTO commerce_import_runs (plans_sent, plans_present, subscriptions_sent, subscriptions_present, verified)
		VALUES ($1, $2, $3, $4, $5)`, plansSent, plansPresent, subsSent, subsPresent, verified)
	return err
}

// ProjectedSubscription is the viewer's paid, unexpired subscription to a
// creator, as the projection holds it.
type ProjectedSubscription struct {
	ID          string
	ExpiresAt   time.Time
	CancelledAt *time.Time
}

// SubscriptionFor is subscriberID's paid, unexpired subscription to
// creatorID, or nil. A database fault is an error, never "not subscribed".
func SubscriptionFor(database *sql.DB, subscriberID, creatorID string) (*ProjectedSubscription, error) {
	var s ProjectedSubscription
	err := database.QueryRow(`
		SELECT id::text, expires_at, cancelled_at FROM subscriptions
		 WHERE subscriber_id = $1 AND creator_id = $2
		   AND status = 'active' AND expires_at > NOW()`, subscriberID, creatorID).
		Scan(&s.ID, &s.ExpiresAt, &s.CancelledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("subscriptions: %w", err)
	}
	return &s, nil
}

// SubscriptionProjected reports whether the projection already shows the
// subscription in the state Themis answered with.
func SubscriptionProjected(ctx context.Context, database *sql.DB, subscriptionID string, active, cancelled bool) (bool, error) {
	var ok bool
	err := database.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM subscriptions
		                WHERE id::text = $1
		                  AND (status = 'active') = $2
		                  AND (cancelled_at IS NOT NULL) = $3)`,
		subscriptionID, active, cancelled).Scan(&ok)
	return ok, err
}

// PlanProjectedSince reports whether a plan event was applied to the projection
// at or after since — that is, whether a change Themis just answered for has
// reached this brain.
func PlanProjectedSince(ctx context.Context, database *sql.DB, planID string, since time.Time) (bool, error) {
	var ok bool
	err := database.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM commerce_subject_seq
		                WHERE subject_kind = 'plan' AND subject_id = $1 AND applied_at >= $2)`,
		planID, since).Scan(&ok)
	return ok, err
}
