package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/lib/pq"

	"github.com/f33d3r/feed-engine/internal/model"
)

// ── The identity projection ───────────────────────────────────────────────────
//
// elohim-veni is the identity authority. This file is the only writer of what
// feed-engine keeps of identity — users.handle, users.pial_id, users.role,
// users.deactivated_at and the identity_facts table — and the reader every
// SQL join and every display surface goes through.
//
// There are exactly two ways in:
//
//   - ApplyIdentityEvent, from the identity.events consumer. Authoritative and
//     ordered: every write is guarded by the event's occurred_at, so a
//     redelivered or reordered older event never overwrites a newer one.
//
//   - SeedIdentity, from a synchronous answer the authority just gave — a
//     signup, an added or linked account. It writes only what that answer
//     established, never advances the version guard, and so can only ever be
//     corrected by the events that follow it, never block them.
//
// Nothing here decides an identity fact.

// ErrNoPIAL is returned when a capability is checked against an account that
// carries no identity. It denies; it is never a grant.
var ErrNoPIAL = errors.New("identity: account has no PIAL")

// IdentitySnapshot is the full state of one identity, and of one of its
// accounts, as the authority stated it.
type IdentitySnapshot struct {
	EventID    string
	OccurredAt time.Time
	PIALID     string
	AccountID  string // "" when the event is about the identity alone
	Handle     string
	Role       string
	Status     string
	Caps       []string
	Facts      IdentityFacts
}

// IdentityFacts are the age and verification facts of one identity.
type IdentityFacts struct {
	AgeVerified   bool
	IsAdult       bool
	IsMinor       bool
	KYCTier       string
	DateOfBirth   *time.Time
	KYCVerifiedAt *time.Time
}

// ErrInvalidIdentityEvent marks an event that can never be applied as written.
var ErrInvalidIdentityEvent = errors.New("identity event is invalid")

// IsPermanentIdentityApplyError reports whether an ApplyIdentityEvent failure
// can never succeed on retry: the event itself is invalid, or the database
// refused its data (SQLSTATE class 22, data exception, or 23, integrity
// constraint violation). Anything else — a lost connection, a timeout, a
// serialization failure — is transient.
func IsPermanentIdentityApplyError(err error) bool {
	if errors.Is(err, ErrInvalidIdentityEvent) {
		return true
	}
	var pe *pq.Error
	if errors.As(err, &pe) {
		switch pe.Code.Class() {
		case "22", "23":
			return true
		}
	}
	return false
}

// Identity statuses this file branches on.
const (
	identityStatusBanned      = "banned"
	identityStatusDeactivated = "deactivated"
)

// projectedRole is what users.role holds for an identity: its platform role,
// or 'banned' while a ban stands — the projection the suggestion and feed
// queries already read.
func projectedRole(role, status string) string {
	if status == identityStatusBanned {
		return "banned"
	}
	if role == "" {
		return model.RoleUser
	}
	return role
}

func dateParam(t *time.Time) interface{} {
	if t == nil {
		return nil
	}
	return t.Format("2006-01-02")
}

func timeParam(t *time.Time) interface{} {
	if t == nil {
		return nil
	}
	return *t
}

// ApplyIdentityEvent brings the projection up to one identity.events record.
// It returns the account ids whose resolved view changed, so the caller can
// drop cached sessions for them. Idempotent: applying the same event twice, or
// an older one after a newer, changes nothing.
func ApplyIdentityEvent(ctx context.Context, database *sql.DB, ev IdentitySnapshot) (affected []string, err error) {
	if ev.PIALID == "" || ev.EventID == "" || ev.OccurredAt.IsZero() {
		return nil, fmt.Errorf("%w: identity event %q: pial_id, event_id and occurred_at are required", ErrInvalidIdentityEvent, ev.EventID)
	}
	err = WithTx(database, func(tx *sql.Tx) error {
		// ── the identity ────────────────────────────────────────────────────
		res, err := tx.ExecContext(ctx, `
			INSERT INTO identity_facts
			    (pial_id, role, status, caps, age_verified, is_adult, is_minor, kyc_tier,
			     kyc_verified_at, date_of_birth, source_event_id, source_occurred_at, confirmed, projected_at)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10::date, $11, $12, TRUE, NOW())
			ON CONFLICT (pial_id) DO UPDATE SET
			    role               = EXCLUDED.role,
			    status             = EXCLUDED.status,
			    caps               = EXCLUDED.caps,
			    age_verified       = EXCLUDED.age_verified,
			    is_adult           = EXCLUDED.is_adult,
			    is_minor           = EXCLUDED.is_minor,
			    kyc_tier           = EXCLUDED.kyc_tier,
			    kyc_verified_at    = EXCLUDED.kyc_verified_at,
			    date_of_birth      = EXCLUDED.date_of_birth,
			    source_event_id    = EXCLUDED.source_event_id,
			    source_occurred_at = EXCLUDED.source_occurred_at,
			    confirmed          = TRUE,
			    projected_at       = NOW()
			  WHERE identity_facts.source_occurred_at <= EXCLUDED.source_occurred_at`,
			ev.PIALID, ev.Role, ev.Status, pq.Array(nonNilStrings(ev.Caps)),
			ev.Facts.AgeVerified, ev.Facts.IsAdult, ev.Facts.IsMinor, kycTierOrNone(ev.Facts.KYCTier),
			timeParam(ev.Facts.KYCVerifiedAt), dateParam(ev.Facts.DateOfBirth), ev.EventID, ev.OccurredAt)
		if err != nil {
			return fmt.Errorf("identity %s: facts: %w", ev.PIALID, err)
		}
		identityApplied, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("identity %s: facts row count: %w", ev.PIALID, err)
		}

		// Role and ban standing belong to the identity, so every account on it
		// carries them — but only when this event is the newest about it.
		if identityApplied == 1 {
			rows, err := tx.QueryContext(ctx,
				`UPDATE users SET role = $2, updated_at = NOW()
				  WHERE pial_id = $1::uuid AND role IS DISTINCT FROM $2
				 RETURNING id::text`,
				ev.PIALID, projectedRole(ev.Role, ev.Status))
			if err != nil {
				return fmt.Errorf("identity %s: role projection: %w", ev.PIALID, err)
			}
			ids, err := scanIDs(rows)
			if err != nil {
				return fmt.Errorf("identity %s: role projection: %w", ev.PIALID, err)
			}
			affected = append(affected, ids...)
		}

		if ev.AccountID == "" {
			return nil
		}
		// ── the account ─────────────────────────────────────────────────────
		changed, err := projectAccountTx(ctx, tx, ev)
		if err != nil {
			return err
		}
		if changed {
			affected = append(affected, ev.AccountID)
		}
		return nil
	})
	return affected, err
}

// projectAccountTx writes the account columns of one event, guarded by the
// account's own version. It reports whether the row changed.
func projectAccountTx(ctx context.Context, tx *sql.Tx, ev IdentitySnapshot) (bool, error) {
	if ev.Handle == "" {
		return false, fmt.Errorf("%w: identity event %s: account %s without a handle", ErrInvalidIdentityEvent, ev.EventID, ev.AccountID)
	}
	// A handle is held by one account at a time at the authority. If the
	// projection still shows it on another account, that account's own event
	// releasing it has not arrived yet; this event is the authority saying who
	// holds it now. The stale holder is parked on its own id — unique, and
	// visibly not a handle — until its event lands.
	if _, err := tx.ExecContext(ctx, `
		UPDATE users SET handle = id::text, updated_at = NOW()
		 WHERE handle = $1 AND id <> $2::uuid`, ev.Handle, ev.AccountID); err != nil {
		return false, fmt.Errorf("account %s: releasing handle %q from its previous holder: %w", ev.AccountID, ev.Handle, err)
	}

	deactivated := ev.Status == identityStatusDeactivated
	res, err := tx.ExecContext(ctx, `
		INSERT INTO users (id, handle, pial_id, role, deactivated_at, identity_event_at)
		VALUES ($1::uuid, $2, $3::uuid, $4, CASE WHEN $5 THEN $6::timestamptz END, $6)
		ON CONFLICT (id) DO UPDATE SET
		    handle            = EXCLUDED.handle,
		    pial_id           = EXCLUDED.pial_id,
		    role              = EXCLUDED.role,
		    deactivated_at    = CASE WHEN $5 THEN COALESCE(users.deactivated_at, EXCLUDED.deactivated_at) END,
		    deactivated_reason = CASE WHEN $5 THEN users.deactivated_reason ELSE '' END,
		    identity_event_at = EXCLUDED.identity_event_at,
		    updated_at        = NOW()
		  WHERE users.identity_event_at <= EXCLUDED.identity_event_at`,
		ev.AccountID, ev.Handle, ev.PIALID, projectedRole(ev.Role, ev.Status), deactivated, ev.OccurredAt)
	if err != nil {
		return false, fmt.Errorf("account %s: projection: %w", ev.AccountID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("account %s: projection row count: %w", ev.AccountID, err)
	}
	return n == 1, nil
}

// SeedIdentity records what a synchronous answer from the authority just
// established: an identity, and an account on it. The identity row is created
// only if the projection has never heard of it; the account row is created,
// or — for a linked account — moved onto the identity. Neither advances a
// version guard, so every later event still applies over it.
func SeedIdentity(tx DBTX, s IdentitySnapshot) error {
	if s.PIALID == "" || s.AccountID == "" || s.Handle == "" {
		return errors.New("seed identity: pial_id, account_id and handle are required")
	}
	if _, err := tx.Exec(`
		INSERT INTO identity_facts
		    (pial_id, role, status, caps, age_verified, is_adult, is_minor, kyc_tier, date_of_birth)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9::date)
		ON CONFLICT (pial_id) DO NOTHING`,
		s.PIALID, roleOrUser(s.Role), statusOrActive(s.Status), pq.Array(nonNilStrings(s.Caps)),
		s.Facts.AgeVerified, s.Facts.IsAdult, s.Facts.IsMinor, kycTierOrNone(s.Facts.KYCTier),
		dateParam(s.Facts.DateOfBirth)); err != nil {
		return fmt.Errorf("seed identity %s: %w", s.PIALID, err)
	}
	if _, err := tx.Exec(`
		INSERT INTO users (id, handle, pial_id, role)
		VALUES ($1::uuid, $2, $3::uuid, $4)
		ON CONFLICT (id) DO UPDATE SET pial_id = EXCLUDED.pial_id, updated_at = NOW()`,
		s.AccountID, s.Handle, s.PIALID, projectedRole(s.Role, s.Status)); err != nil {
		return fmt.Errorf("seed account %s (@%s): %w", s.AccountID, s.Handle, err)
	}
	return nil
}

// SetProjectedDeactivated records, on the account projection, a deactivation
// the authority has just accepted. The identity.events record for it follows
// and is authoritative; this only makes the account disappear from this
// brain's surfaces without waiting for it.
func SetProjectedDeactivated(database *sql.DB, accountID, reason string) error {
	_, err := database.Exec(`
		UPDATE users SET deactivated_at = COALESCE(deactivated_at, NOW()), deactivated_reason = $2, updated_at = NOW()
		 WHERE id = $1::uuid`, accountID, reason)
	return err
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func kycTierOrNone(t string) string {
	if t == "" {
		return model.KYCTierNone
	}
	return t
}

func roleOrUser(r string) string {
	if r == "" {
		return model.RoleUser
	}
	return r
}

func statusOrActive(s string) string {
	if s == "" {
		return "active"
	}
	return s
}

func scanIDs(rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ── Reads ─────────────────────────────────────────────────────────────────────

// PIALState is the projected identity state of one PIAL: what the age and
// verification gates and the role checks read for accounts other than the
// signed-in viewer, whose state comes from its resolved session.
type PIALState struct {
	AgeVerified   bool
	KYCTier       string
	Role          string // user | admin | founder
	Status        string
	IsAdult       bool
	IsMinor       bool
	Caps          []string
	DateOfBirth   *time.Time
	KYCVerifiedAt *time.Time
}

// LoadPIALState reads the projected state of an identity, or nil when this
// brain has no projection of it.
func LoadPIALState(database *sql.DB, pialID string) *PIALState {
	if database == nil || pialID == "" {
		return nil
	}
	s, err := loadPIALState(database, pialID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			// Every caller renders without the facts rather than failing the
			// page; the read failure is still said, once, here.
			log.Printf("[identity] reading projected state of %s: %v", pialID, err)
		}
		return nil
	}
	return s
}

func loadPIALState(database DBTX, pialID string) (*PIALState, error) {
	s := &PIALState{}
	var caps pq.StringArray
	var dob, kycAt sql.NullTime
	err := database.QueryRow(`
		SELECT age_verified, kyc_tier, role, status, is_adult, is_minor, caps, date_of_birth, kyc_verified_at
		  FROM identity_facts WHERE pial_id = $1::uuid`, pialID,
	).Scan(&s.AgeVerified, &s.KYCTier, &s.Role, &s.Status, &s.IsAdult, &s.IsMinor, &caps, &dob, &kycAt)
	if err != nil {
		return nil, err
	}
	s.Caps = []string(caps)
	if dob.Valid {
		t := dob.Time
		s.DateOfBirth = &t
	}
	if kycAt.Valid {
		t := kycAt.Time
		s.KYCVerifiedAt = &t
	}
	return s, nil
}

// CheckCapability answers from the projection whether an identity holds a
// capability now. It denies on every failure and says why. A request with a
// resolved session asks the session (model.User.Can) instead, which is fresher.
func CheckCapability(database DBTX, pialID, capability string) (bool, error) {
	if pialID == "" {
		return false, ErrNoPIAL
	}
	if nilDB(database) {
		return false, errors.New("identity: no database")
	}
	var holds bool
	err := database.QueryRow(
		`SELECT $2 = ANY(caps) FROM identity_facts WHERE pial_id = $1::uuid`, pialID, capability,
	).Scan(&holds)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("identity %s: not projected", pialID)
	}
	return holds, err
}

// HasCapability is CheckCapability with every failure reported and denied.
func HasCapability(database *sql.DB, pialID, capability string) bool {
	ok, err := CheckCapability(database, pialID, capability)
	if err != nil {
		log.Printf("[identity] capability %s denied for %q: %v", capability, pialID, err)
		return false
	}
	return ok
}

// GetCapabilities returns the capabilities an identity holds now, for display.
// An authorization decision uses CheckCapability or the resolved session.
func GetCapabilities(database DBTX, pialID string) model.PIALCapabilityMap {
	m := model.PIALCapabilityMap{}
	if nilDB(database) || pialID == "" {
		return m
	}
	s, err := loadPIALState(database, pialID)
	if err != nil {
		return m
	}
	for _, c := range s.Caps {
		m[c] = &model.PIALCapability{PIALID: pialID, Capability: c, State: model.CapStateGranted}
	}
	return m
}

// GetPIALKYCVerifiedAt returns when the identity tier was recorded, or nil.
func GetPIALKYCVerifiedAt(database *sql.DB, pialID string) *time.Time {
	if database == nil || pialID == "" {
		return nil
	}
	s, err := loadPIALState(database, pialID)
	if err != nil {
		return nil
	}
	return s.KYCVerifiedAt
}

// ResolvePIAL returns the identity an account belongs to, or "".
func ResolvePIAL(database *sql.DB, accountID string) string {
	if database == nil || accountID == "" {
		return ""
	}
	var pialID sql.NullString
	if err := database.QueryRow(`SELECT pial_id::text FROM users WHERE id = $1::uuid`, accountID).Scan(&pialID); err != nil {
		return ""
	}
	return pialID.String
}

// AccountForPIAL returns the identity's first account, or "".
func AccountForPIAL(database *sql.DB, pialID string) string {
	account, err := PrimaryAccountForPIAL(database, pialID)
	if err != nil {
		return ""
	}
	return account
}

// PrimaryAccountForPIAL returns the account a person's notifications belong to:
// the identity's first account. sql.ErrNoRows when it has none.
func PrimaryAccountForPIAL(database *sql.DB, pialID string) (string, error) {
	if database == nil || pialID == "" {
		return "", sql.ErrNoRows
	}
	var account string
	err := database.QueryRow(`
		SELECT id::text FROM users WHERE pial_id = $1::uuid
		 ORDER BY created_at, id LIMIT 1`, pialID).Scan(&account)
	return account, err
}

// GetAccountsForPIAL lists every account on an identity, first-created first,
// marking the session's active one.
func GetAccountsForPIAL(database *sql.DB, pialID, activeAccountID string) ([]model.LinkedAccount, error) {
	rows, err := database.Query(`
		SELECT u.id::text, u.handle, COALESCE(p.display_name,''), COALESCE(p.avatar_url,'')
		  FROM users u
		  LEFT JOIN user_profiles p ON p.user_id = u.id
		 WHERE u.pial_id = $1::uuid AND u.deactivated_at IS NULL
		 ORDER BY u.created_at, u.id`, pialID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var accounts []model.LinkedAccount
	for rows.Next() {
		var a model.LinkedAccount
		if err := rows.Scan(&a.ID, &a.Handle, &a.DisplayName, &a.AvatarURL); err != nil {
			return nil, err
		}
		a.IsPrimary = len(accounts) == 0
		a.IsActive = a.ID == activeAccountID
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

// GetFounder returns the founder's account: the first account of the identity
// elohim-veni holds the founder role for. (nil, nil) when there is none.
func GetFounder(database *sql.DB) (*model.User, error) {
	return scanUser(database.QueryRow(userSelectSQL+`
		WHERE u.id = (
			SELECT fu.id FROM users fu
			  JOIN identity_facts f ON f.pial_id = fu.pial_id
			 WHERE f.role = $1
			 ORDER BY fu.created_at, fu.id
			 LIMIT 1)`, model.RoleFounder))
}
