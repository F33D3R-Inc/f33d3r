package db

// kyc_attempts.go — the consent behind one identity-verification attempt.
//
// A row is written when the person agrees to the collection, claimed once when
// they submit the evidence, and deleted as soon as the attempt has been
// answered. Verity keeps the consent with the attempt's evidence; this table
// only carries it from the consent Fragment to the submission (migration 0047).

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// KYCAttemptTTL is how long a consent stays good for one attempt. Past it the
// person consents again: what they agreed to is shown to them again.
const KYCAttemptTTL = 30 * time.Minute

// KYCAttemptAbandonedAfter is when the sweep deletes an attempt nobody finished.
const KYCAttemptAbandonedAfter = 24 * time.Hour

// KYCAttempt is one consented attempt.
type KYCAttempt struct {
	AttemptID         string
	AccountID         string
	PIALID            string
	Purpose           string
	DocumentType      string
	ConsentVersion    string
	ConsentAcceptedAt time.Time
	ConsentIP         string
	ConsentUserAgent  string
}

// ErrKYCAttemptUnavailable is the answer for an attempt that does not exist for
// this account, was already submitted, or whose consent has lapsed.
var ErrKYCAttemptUnavailable = errors.New("kyc attempt: no open consented attempt")

// CreateKYCAttempt records a consented attempt.
func CreateKYCAttempt(database *sql.DB, a KYCAttempt) error {
	if _, err := database.Exec(`
		INSERT INTO kyc_attempts
		    (attempt_id, account_id, pial_id, purpose, document_type,
		     consent_version, consent_accepted_at, consent_ip, consent_user_agent)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7, $8, $9)`,
		a.AttemptID, a.AccountID, a.PIALID, a.Purpose, a.DocumentType,
		a.ConsentVersion, a.ConsentAcceptedAt, a.ConsentIP, a.ConsentUserAgent); err != nil {
		return fmt.Errorf("recording consent for attempt %s: %w", a.AttemptID, err)
	}
	return nil
}

// OpenKYCAttempt reads an attempt the account may still capture for: its own,
// not submitted, consent within KYCAttemptTTL.
func OpenKYCAttempt(database *sql.DB, attemptID, accountID string) (*KYCAttempt, error) {
	a, err := scanKYCAttempt(database.QueryRow(`
		SELECT attempt_id::text, account_id::text, pial_id::text, purpose, document_type,
		       consent_version, consent_accepted_at, consent_ip, consent_user_agent
		  FROM kyc_attempts
		 WHERE attempt_id = $1::uuid AND account_id = $2::uuid
		   AND submitted_at IS NULL
		   AND consent_accepted_at > NOW() - make_interval(secs => $3)`,
		attemptID, accountID, KYCAttemptTTL.Seconds()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrKYCAttemptUnavailable
	}
	if err != nil {
		return nil, fmt.Errorf("reading attempt %s: %w", attemptID, err)
	}
	return a, nil
}

// ClaimKYCAttempt marks an open attempt submitted and returns it. An attempt is
// submitted once: a second claim finds nothing.
func ClaimKYCAttempt(database *sql.DB, attemptID, accountID string) (*KYCAttempt, error) {
	a, err := scanKYCAttempt(database.QueryRow(`
		UPDATE kyc_attempts SET submitted_at = NOW()
		 WHERE attempt_id = $1::uuid AND account_id = $2::uuid
		   AND submitted_at IS NULL
		   AND consent_accepted_at > NOW() - make_interval(secs => $3)
		RETURNING attempt_id::text, account_id::text, pial_id::text, purpose, document_type,
		          consent_version, consent_accepted_at, consent_ip, consent_user_agent`,
		attemptID, accountID, KYCAttemptTTL.Seconds()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrKYCAttemptUnavailable
	}
	if err != nil {
		return nil, fmt.Errorf("claiming attempt %s: %w", attemptID, err)
	}
	return a, nil
}

// DeleteKYCAttempt removes an answered attempt; its consent now lives with the
// evidence at Verity.
func DeleteKYCAttempt(database *sql.DB, attemptID string) error {
	if _, err := database.Exec(`DELETE FROM kyc_attempts WHERE attempt_id = $1::uuid`, attemptID); err != nil {
		return fmt.Errorf("deleting answered attempt %s: %w", attemptID, err)
	}
	return nil
}

// SweepAbandonedKYCAttempts deletes attempts older than KYCAttemptAbandonedAfter.
func SweepAbandonedKYCAttempts(database *sql.DB) (int64, error) {
	res, err := database.Exec(`
		DELETE FROM kyc_attempts WHERE consent_accepted_at < NOW() - make_interval(secs => $1)`,
		KYCAttemptAbandonedAfter.Seconds())
	if err != nil {
		return 0, fmt.Errorf("sweeping abandoned verification attempts: %w", err)
	}
	return res.RowsAffected()
}

func scanKYCAttempt(row *sql.Row) (*KYCAttempt, error) {
	var a KYCAttempt
	if err := row.Scan(&a.AttemptID, &a.AccountID, &a.PIALID, &a.Purpose, &a.DocumentType,
		&a.ConsentVersion, &a.ConsentAcceptedAt, &a.ConsentIP, &a.ConsentUserAgent); err != nil {
		return nil, err
	}
	return &a, nil
}
