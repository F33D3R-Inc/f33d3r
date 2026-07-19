package db

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/f33d3r/feed-engine/internal/model"
)

// ── PIAL Root ─────────────────────────────────────────────────────────────────

// CreatePIAL provisions a new PIAL root and returns its ID.
func CreatePIAL(database *sql.DB) (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("pial keygen: %w", err)
	}
	pubKey := hex.EncodeToString(key)
	var pialID string
	err := database.QueryRow(
		`INSERT INTO pial_roots (public_key) VALUES ($1) RETURNING pial_id`, pubKey,
	).Scan(&pialID)
	return pialID, err
}

// ── Account → PIAL resolution ─────────────────────────────────────────────────

// BindAccountToPIAL creates the account→PIAL link and stamps users.pial_id.
func BindAccountToPIAL(database *sql.DB, pialID, accountID string) error {
	_, err := database.Exec(`
		INSERT INTO pial_account_bindings (pial_id, account_id, status, is_primary)
		VALUES ($1, $2, 'active', TRUE)
		ON CONFLICT DO NOTHING`, pialID, accountID)
	if err != nil {
		return err
	}
	_, err = database.Exec(`UPDATE users SET pial_id = $1 WHERE id = $2`, pialID, accountID)
	return err
}

// ResolvePIAL returns the active PIAL ID for an account ID.
// Returns empty string if no binding exists yet.
func ResolvePIAL(database *sql.DB, accountID string) string {
	var pialID string
	database.QueryRow(
		`SELECT pial_id FROM pial_account_bindings WHERE account_id = $1 AND status = 'active' LIMIT 1`,
		accountID,
	).Scan(&pialID)
	return pialID
}

// ── Capability Engine ─────────────────────────────────────────────────────────

// GrantDefaultCapabilities inserts all default capabilities for a new PIAL.
func GrantDefaultCapabilities(database *sql.DB, pialID string) error {
	for _, cap := range model.DefaultCapabilities {
		if _, err := database.Exec(`
			INSERT INTO pial_capabilities (pial_id, capability, state, granted_by)
			VALUES ($1, $2, 'granted', 'system')
			ON CONFLICT (pial_id, capability) DO NOTHING`, pialID, cap); err != nil {
			return err
		}
	}
	return recomputeStateHash(database, pialID)
}

// GetCapabilities returns the full capability map for a PIAL root.
func GetCapabilities(database *sql.DB, pialID string) model.PIALCapabilityMap {
	rows, err := database.Query(`
		SELECT capability, state, expires_at, reason, granted_by, updated_at
		FROM pial_capabilities WHERE pial_id = $1`, pialID)
	if err != nil {
		return model.PIALCapabilityMap{}
	}
	defer rows.Close()
	m := model.PIALCapabilityMap{}
	for rows.Next() {
		c := &model.PIALCapability{PIALID: pialID}
		rows.Scan(&c.Capability, &c.State, &c.ExpiresAt, &c.Reason, &c.GrantedBy, &c.UpdatedAt)
		m[c.Capability] = c
	}
	return m
}

// HasCapability returns true if the PIAL currently has an active grant for cap.
// If the PIAL exists but has zero capability rows (pre-backfill account), we
// grant defaults on-the-fly so the user is never silently blocked.
func HasCapability(database *sql.DB, pialID, cap string) bool {
	if pialID == "" {
		return true // no PIAL yet → don't block (legacy accounts)
	}
	m := GetCapabilities(database, pialID)
	if len(m) == 0 {
		// PIAL exists but capabilities were never seeded — grant now.
		_ = GrantDefaultCapabilities(database, pialID)
		return true
	}
	return m.Can(cap)
}

// SetCapability grants, restricts, or revokes a capability on a PIAL root.
func SetCapability(database *sql.DB, pialID, capability, state, reason, grantedBy string, expiresAt *time.Time) error {
	_, err := database.Exec(`
		INSERT INTO pial_capabilities (pial_id, capability, state, expires_at, reason, granted_by, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
		ON CONFLICT (pial_id, capability) DO UPDATE
		  SET state = EXCLUDED.state,
		      expires_at = EXCLUDED.expires_at,
		      reason = EXCLUDED.reason,
		      granted_by = EXCLUDED.granted_by,
		      updated_at = NOW()`,
		pialID, capability, state, expiresAt, reason, grantedBy)
	if err != nil {
		return err
	}
	return recomputeStateHash(database, pialID)
}

// recomputeStateHash rebuilds PIAL state_hash from capabilities + identity attributes.
// Any change to capabilities, kyc_tier, or age_verified invalidates cached PIAL state.
func recomputeStateHash(database *sql.DB, pialID string) error {
	caps := GetCapabilities(database, pialID)
	keys := make([]string, 0, len(caps))
	for k := range caps { keys = append(keys, k) }
	sort.Strings(keys)
	type capEntry struct{ Cap, State string }
	entries := make([]capEntry, 0, len(keys))
	for _, k := range keys {
		entries = append(entries, capEntry{k, caps[k].State})
	}
	// Include identity attributes so cross-brain caches know when to invalidate.
	type stateSnapshot struct {
		Caps        []capEntry
		KYCTier     string
		AgeVerified bool
	}
	var kycTier string
	var ageVerified bool
	database.QueryRow(
		`SELECT kyc_tier, age_verified FROM pial_roots WHERE pial_id = $1`, pialID,
	).Scan(&kycTier, &ageVerified)
	snap := stateSnapshot{Caps: entries, KYCTier: kycTier, AgeVerified: ageVerified}
	b, _ := json.Marshal(snap)
	h := sha256.Sum256(b)
	hash := hex.EncodeToString(h[:])
	_, err := database.Exec(`UPDATE pial_roots SET state_hash = $1 WHERE pial_id = $2`, hash, pialID)
	return err
}

// ── PIAL identity attribute setters ──────────────────────────────────────────

// SetPIALBirthday writes the date of birth to the PIAL root and upgrades kyc_tier
// to 'basic' (minimum self-reported) when a birthday is provided for the first time.
// Refuses once dob_locked is TRUE — a documented-age-verified PIAL cannot rewrite its DOB.
func SetPIALBirthday(database *sql.DB, pialID string, dob time.Time) error {
	res, err := database.Exec(`
		UPDATE pial_roots
		SET date_of_birth = $1,
		    kyc_tier       = CASE WHEN kyc_tier = 'none' THEN 'basic' ELSE kyc_tier END,
		    age_verified   = TRUE,
		    is_adult       = (date_part('year', age($1)) >= 18),
		    is_minor       = (date_part('year', age($1)) <  18)
		WHERE pial_id = $2 AND dob_locked = FALSE`, dob, pialID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("pial %s: date of birth is locked after age verification", pialID)
	}
	return recomputeStateHash(database, pialID)
}

// SetPIALRole sets the platform role on the PIAL root (admin | founder | user).
// PIAL is the sole authority for roles; this is the only writer. It also syncs the
// legacy projections (users.role, user_roles.role_type) that feed queries still read —
// those projections are retired in Phase 4 once those readers source PIAL.
func SetPIALRole(database *sql.DB, pialID, role string) error {
	allowed := map[string]bool{"user": true, "admin": true, "founder": true}
	if !allowed[role] {
		role = "user"
	}
	if _, err := database.Exec(
		`UPDATE pial_roots SET role = $1 WHERE pial_id = $2`, role, pialID); err != nil {
		return err
	}
	syncRoleProjection(database, pialID, role)
	return recomputeStateHash(database, pialID)
}

// syncRoleProjection mirrors the PIAL role onto the denormalized users.role column,
// which feed queries read for author-role display. (user_roles.role_type was dropped.)
func syncRoleProjection(database *sql.DB, pialID, role string) {
	database.Exec(`UPDATE users SET role = $1 WHERE pial_id = $2`, role, pialID)
}

// SetPIALVerified records a documented age verification on the PIAL root — the manual
// admin/KYC override path. Stamps the tier, verification time, locks the DOB, and records
// provenance (grantedBy = "admin:<handle>" | "ekyc" | "ekyc:mrz"). tier: basic|soft|full.
func SetPIALVerified(database *sql.DB, pialID, tier, grantedBy string) error {
	allowed := map[string]bool{"basic": true, "soft": true, "full": true}
	if !allowed[tier] {
		tier = "soft"
	}
	// Documented age verification implies adult (a confirmed minor is never sent here).
	_, err := database.Exec(`
		UPDATE pial_roots
		SET kyc_tier        = $1,
		    kyc_verified_at = NOW(),
		    age_verified    = TRUE,
		    dob_locked      = TRUE,
		    is_adult        = TRUE,
		    is_minor        = FALSE
		WHERE pial_id = $2`, tier, pialID)
	if err != nil {
		return err
	}
	LogPIALEvent(database, pialID, "", "age_verified", map[string]interface{}{
		"tier": tier, "granted_by": grantedBy,
	}, grantedBy)
	return recomputeStateHash(database, pialID)
}

// SetPIALUnverified clears a documented age verification on the PIAL root — the admin
// de-verify path. PIAL is the only writer: this records the reversal in the ledger and
// recomputes the state hash (the raw UPDATE it replaces skipped both). grantedBy is the
// provenance, e.g. "admin:<handle>".
func SetPIALUnverified(database *sql.DB, pialID, grantedBy string) error {
	_, err := database.Exec(
		`UPDATE pial_roots SET age_verified = FALSE WHERE pial_id = $1::uuid`, pialID)
	if err != nil {
		return err
	}
	LogPIALEvent(database, pialID, "", "age_unverified", map[string]interface{}{
		"granted_by": grantedBy,
	}, grantedBy)
	return recomputeStateHash(database, pialID)
}

// SetPIALKYCTier upgrades the KYC tier and stamps the verification time.
// tier must be one of: none | basic | soft | full.
func SetPIALKYCTier(database *sql.DB, pialID, tier string) error {
	now := time.Now()
	_, err := database.Exec(`
		UPDATE pial_roots
		SET kyc_tier = $1, kyc_verified_at = $2, age_verified = ($1 != 'none')
		WHERE pial_id = $3`, tier, now, pialID)
	if err != nil {
		return err
	}
	return recomputeStateHash(database, pialID)
}

// SetPIALStatus updates the operational status of a PIAL root.
// status must be one of: active | suspended | tombstoned.
func SetPIALStatus(database *sql.DB, pialID, status string) error {
	_, err := database.Exec(`
		UPDATE pial_roots SET status = $1 WHERE pial_id = $2`, status, pialID)
	return err
}

// PIALState is the resolved identity+authz view of a PIAL root — the single source
// the session reads. Verification, role, and age facts all come from here, replacing
// the old user_roles / user_profiles.is_verified / users.role triplication.
type PIALState struct {
	AgeVerified bool
	KYCTier     string
	Role        string // user | admin | founder
	DOBLocked   bool
	IsAdult     bool // derived from PIAL date_of_birth
	IsMinor     bool // derived from PIAL date_of_birth
}

// LoadPIALState resolves the canonical identity state for a PIAL root. Returns nil if
// the PIAL doesn't exist. is_adult/is_minor are derived from date_of_birth at read time
// (single source = the DOB on PIAL); an unknown DOB yields neither adult nor minor.
func LoadPIALState(database *sql.DB, pialID string) *PIALState {
	if database == nil || pialID == "" {
		return nil
	}
	s := &PIALState{}
	err := database.QueryRow(`
		SELECT COALESCE(age_verified,FALSE), COALESCE(kyc_tier,'none'),
		       COALESCE(role,'user'), COALESCE(dob_locked,FALSE),
		       COALESCE(is_adult,FALSE), COALESCE(is_minor,FALSE)
		FROM pial_roots WHERE pial_id = $1`, pialID,
	).Scan(&s.AgeVerified, &s.KYCTier, &s.Role, &s.DOBLocked, &s.IsAdult, &s.IsMinor)
	if err != nil {
		return nil
	}
	return s
}

// GetPIALRoot returns the full PIAL root for a given PIAL ID.
func GetPIALRoot(database *sql.DB, pialID string) (*model.PIALRoot, error) {
	r := &model.PIALRoot{}
	err := database.QueryRow(`
		SELECT pial_id, public_key, state_hash, created_at,
		       is_tombstoned, status, date_of_birth, kyc_tier,
		       kyc_verified_at, age_verified
		FROM pial_roots WHERE pial_id = $1`, pialID).Scan(
		&r.PIALID, &r.PublicKey, &r.StateHash, &r.CreatedAt,
		&r.IsTombstoned, &r.Status, &r.DateOfBirth, &r.KYCTier,
		&r.KYCVerifiedAt, &r.AgeVerified,
	)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// ── Event Ledger ──────────────────────────────────────────────────────────────

// LogPIALEvent appends an immutable event to the ledger. Never call UPDATE/DELETE on this table.
func LogPIALEvent(database *sql.DB, pialID, accountID, eventType string, payload map[string]interface{}, source string) {
	if pialID == "" {
		return
	}
	b, _ := json.Marshal(payload)
	var acctParam interface{}
	if accountID != "" {
		acctParam = accountID
	}
	database.Exec(`
		INSERT INTO pial_event_ledger (pial_id, account_id, event_type, payload, source)
		VALUES ($1, $2, $3, $4, $5)`,
		pialID, acctParam, eventType, string(b), source)
}

// GetPIALEvents returns the most recent ledger entries for a PIAL.
func GetPIALEvents(database *sql.DB, pialID string, limit int) ([]PIALEvent, error) {
	rows, err := database.Query(`
		SELECT id, event_type, payload, source, created_at
		FROM pial_event_ledger WHERE pial_id = $1
		ORDER BY created_at DESC LIMIT $2`, pialID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []PIALEvent
	for rows.Next() {
		var e PIALEvent
		var payloadStr string
		rows.Scan(&e.ID, &e.EventType, &payloadStr, &e.Source, &e.CreatedAt)
		json.Unmarshal([]byte(payloadStr), &e.Payload)
		events = append(events, e)
	}
	return events, nil
}

type PIALEvent struct {
	ID        string
	EventType string
	Payload   map[string]interface{}
	Source    string
	CreatedAt time.Time
}

// ── Onboarding helper ─────────────────────────────────────────────────────────

// GetAccountsForPIAL returns all active accounts bound to a PIAL root,
// marking which one is the current session's active account.
func GetAccountsForPIAL(database *sql.DB, pialID, activeAccountID string) ([]model.LinkedAccount, error) {
	rows, err := database.Query(`
		SELECT u.id, u.handle, COALESCE(p.display_name,''), COALESCE(p.avatar_url,''), pab.is_primary
		FROM pial_account_bindings pab
		JOIN users u ON u.id = pab.account_id
		LEFT JOIN user_profiles p ON p.user_id = u.id
		WHERE pab.pial_id = $1 AND pab.status = 'active'
		ORDER BY pab.is_primary DESC, pab.bound_at ASC
	`, pialID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var accounts []model.LinkedAccount
	for rows.Next() {
		var a model.LinkedAccount
		if err := rows.Scan(&a.ID, &a.Handle, &a.DisplayName, &a.AvatarURL, &a.IsPrimary); err != nil {
			continue
		}
		a.IsActive = (a.ID == activeAccountID)
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

// SwitchActiveAccount updates the session's active_account_id after verifying
// the target account is bound to the same PIAL. rawToken is the cookie value.
func SwitchActiveAccount(database *sql.DB, rawToken, targetAccountID, pialID string) error {
	var count int
	if err := database.QueryRow(`
		SELECT COUNT(*) FROM pial_account_bindings
		WHERE pial_id = $1 AND account_id = $2 AND status = 'active'
	`, pialID, targetAccountID).Scan(&count); err != nil || count == 0 {
		return fmt.Errorf("account not bound to this PIAL")
	}
	hash := HashToken(rawToken)
	_, err := database.Exec(`
		UPDATE user_sessions SET active_account_id = $1 WHERE token_hash = $2
	`, targetAccountID, hash)
	return err
}

// LinkExistingAccount moves an independently-created account into currentPIAL.
// The target must be a solo account (no other accounts already on its PIAL).
// Runs in a transaction: removes the old binding, inserts the new one, updates users.pial_id.
func LinkExistingAccount(database *sql.DB, currentPIAL, targetUserID string) error {
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var targetPIAL string
	if err := tx.QueryRow(`SELECT pial_id FROM users WHERE id = $1`, targetUserID).Scan(&targetPIAL); err != nil {
		return fmt.Errorf("target account not found")
	}
	if targetPIAL == currentPIAL {
		return fmt.Errorf("already linked")
	}

	var otherCount int
	if err := tx.QueryRow(`
		SELECT COUNT(*) FROM pial_account_bindings
		WHERE pial_id = $1 AND account_id != $2 AND status = 'active'
	`, targetPIAL, targetUserID).Scan(&otherCount); err != nil {
		return err
	}
	if otherCount > 0 {
		return fmt.Errorf("target account already has linked accounts")
	}

	if _, err := tx.Exec(`DELETE FROM pial_account_bindings WHERE account_id = $1`, targetUserID); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO pial_account_bindings (pial_id, account_id, status, is_primary)
		VALUES ($1, $2, 'active', FALSE)
	`, currentPIAL, targetUserID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE users SET pial_id = $1 WHERE id = $2`, currentPIAL, targetUserID); err != nil {
		return err
	}
	return tx.Commit()
}

// BindAdditionalAccount links a secondary account to an existing PIAL (is_primary = FALSE).
// Use BindAccountToPIAL for the first (primary) account created at onboarding.
func BindAdditionalAccount(database *sql.DB, pialID, accountID string) error {
	_, err := database.Exec(`
		INSERT INTO pial_account_bindings (pial_id, account_id, status, is_primary)
		VALUES ($1, $2, 'active', FALSE)
		ON CONFLICT DO NOTHING`, pialID, accountID)
	if err != nil {
		return err
	}
	_, err = database.Exec(`UPDATE users SET pial_id = $1 WHERE id = $2`, pialID, accountID)
	return err
}



// ── Malkuth/Themis signing keys ───────────────────────────────────────────────

// RegisterPIALSigningKey upserts a PIAL's ECDSA-P256 (SPKI) signing public key.
// The PIAL record (feed-engine) is the authoritative store for signature verification.
func RegisterPIALSigningKey(database *sql.DB, pialID, pubKeyB64, algo string) error {
	if algo == "" {
		algo = "ECDSA-P256"
	}
	_, err := database.Exec(`
		INSERT INTO pial_signing_keys (pial_id, public_key_b64, algorithm)
		VALUES ($1::uuid, $2, $3)
		ON CONFLICT (pial_id) DO UPDATE
		  SET public_key_b64 = EXCLUDED.public_key_b64,
		      algorithm      = EXCLUDED.algorithm,
		      updated_at     = NOW()`,
		pialID, pubKeyB64, algo)
	return err
}


// BootstrapPIAL creates a PIAL root, binds it to an account, grants default
// capabilities, and logs the account_created event. Call once per new account.
func BootstrapPIAL(database *sql.DB, accountID, handle string) string {
	pialID, err := CreatePIAL(database)
	if err != nil {
		return ""
	}
	if err := BindAccountToPIAL(database, pialID, accountID); err != nil {
		return ""
	}
	if err := GrantDefaultCapabilities(database, pialID); err != nil {
		return ""
	}
	LogPIALEvent(database, pialID, accountID, "account_created", map[string]interface{}{
		"handle": handle,
	}, "nantar")
	return pialID
}
