package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
)

// ReferralStats is returned by GetReferralStats.
type ReferralStats struct {
	Code          string
	TotalReferred int
	VerifiedCount int
	PendingCount  int
}

// GetOrCreateReferralCode returns the user's existing referral code or generates a
// new one if none exists. Format: {handle}-{6 random hex chars}.
// Safe to call concurrently — uses INSERT … ON CONFLICT DO NOTHING.
func GetOrCreateReferralCode(database *sql.DB, userID, handle string) (string, error) {
	// Fast path — code already exists.
	var existing string
	err := database.QueryRow(
		`SELECT COALESCE(referral_code, '') FROM user_profiles WHERE user_id = $1`,
		userID,
	).Scan(&existing)
	if err == nil && existing != "" {
		return existing, nil
	}

	// Generate 6 random bytes → 12 hex chars, take first 6.
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("referral code rng: %w", err)
	}
	suffix := hex.EncodeToString(b) // 6 chars
	code := strings.ToLower(handle) + "-" + suffix

	_, err = database.Exec(`
		INSERT INTO user_profiles (user_id, referral_code)
		VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE SET referral_code = $2
		WHERE user_profiles.referral_code IS NULL
	`, userID, code)
	if err != nil {
		log.Printf("[referral] set code: %v", err)
		return "", err
	}

	// Re-read in case the ON CONFLICT WHERE skipped our write (code was set by a race).
	database.QueryRow(
		`SELECT COALESCE(referral_code, '') FROM user_profiles WHERE user_id = $1`,
		userID,
	).Scan(&code)
	return code, nil
}

// GetReferrerByCode resolves a referral_code to the referrer's user_id and handle.
// Returns ("", "", nil) when the code does not match any user.
func GetReferrerByCode(database *sql.DB, code string) (referrerID, referrerHandle string, err error) {
	if database == nil || code == "" {
		return "", "", nil
	}
	err = database.QueryRow(`
		SELECT u.id, u.handle
		FROM user_profiles p
		JOIN users u ON u.id = p.user_id
		WHERE p.referral_code = $1
	`, code).Scan(&referrerID, &referrerHandle)
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	return referrerID, referrerHandle, err
}

// RecordReferral inserts a pending creator_referrals row linking referrer → referred.
// Idempotent: UNIQUE(referred_id) means duplicate calls are silently ignored.
func RecordReferral(database *sql.DB, referrerID, referredID, code string) {
	if database == nil || referrerID == "" || referredID == "" {
		return
	}
	_, err := database.Exec(`
		INSERT INTO creator_referrals (referrer_id, referred_id, referral_code)
		VALUES ($1, $2, $3)
		ON CONFLICT (referred_id) DO NOTHING
	`, referrerID, referredID, code)
	if err != nil {
		log.Printf("[referral] record: %v", err)
	}
}

// CompleteReferral is called when a referred user's is_verified becomes TRUE.
// It marks completed_at, awards 200 XP to the referrer (once), and queues a
// notification. Safe to call multiple times — guard is reward_granted = FALSE.
func CompleteReferral(database *sql.DB, referredID, referredHandle string) {
	if database == nil || referredID == "" {
		return
	}

	var referralID, referrerID string
	err := database.QueryRow(`
		SELECT id, referrer_id
		FROM creator_referrals
		WHERE referred_id = $1
		  AND completed_at IS NULL
		  AND reward_granted = FALSE
	`, referredID).Scan(&referralID, &referrerID)
	if err == sql.ErrNoRows {
		return // no pending referral for this user
	}
	if err != nil {
		log.Printf("[referral] complete lookup: %v", err)
		return
	}

	// Mark complete + reward granted atomically.
	_, err = database.Exec(`
		UPDATE creator_referrals
		SET completed_at  = NOW(),
		    reward_granted = TRUE
		WHERE id = $1
		  AND reward_granted = FALSE
	`, referralID)
	if err != nil {
		log.Printf("[referral] complete update: %v", err)
		return
	}

	// Award 200 XP to the referrer.
	AwardXP(database, referrerID, "referral_complete", 200)

	// Insert a system notification for the referrer.
	msg := "@" + referredHandle + " joined F33D3R through your referral link!"
	database.Exec(`
		INSERT INTO notifications (user_id, type, target_id, target_type, payload)
		VALUES ($1, 'system', $2, 'user', $3::jsonb)
	`, referrerID, referredID, fmt.Sprintf(`{"message":%q}`, msg))

	log.Printf("[referral] complete: referrer=%s referred=%s (+200 XP)", referrerID, referredID)
}

// GetReferralStats returns referral statistics for the given user.
func GetReferralStats(database *sql.DB, userID, handle string) (*ReferralStats, error) {
	code, err := GetOrCreateReferralCode(database, userID, handle)
	if err != nil {
		return nil, err
	}

	stats := &ReferralStats{Code: code}
	database.QueryRow(`
		SELECT
		  COUNT(*)                                           AS total,
		  COUNT(*) FILTER (WHERE completed_at IS NOT NULL)  AS verified,
		  COUNT(*) FILTER (WHERE completed_at IS NULL)      AS pending
		FROM creator_referrals
		WHERE referrer_id = $1
	`, userID).Scan(&stats.TotalReferred, &stats.VerifiedCount, &stats.PendingCount)

	return stats, nil
}

// GrantFoundingCreator sets is_founding_creator=TRUE on user_profiles for the given userID.
func GrantFoundingCreator(database *sql.DB, userID string) error {
	_, err := database.Exec(`
		INSERT INTO user_profiles (user_id, is_founding_creator, founding_creator_at)
		VALUES ($1, TRUE, NOW())
		ON CONFLICT (user_id) DO UPDATE SET
		    is_founding_creator = TRUE,
		    founding_creator_at = COALESCE(user_profiles.founding_creator_at, NOW()),
		    updated_at          = NOW()
	`, userID)
	return err
}
