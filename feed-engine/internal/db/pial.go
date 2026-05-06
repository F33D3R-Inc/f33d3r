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
func HasCapability(database *sql.DB, pialID, cap string) bool {
	if pialID == "" {
		return true // no PIAL yet → don't block (legacy accounts)
	}
	m := GetCapabilities(database, pialID)
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

// recomputeStateHash rebuilds PIAL state_hash from current capability tree.
func recomputeStateHash(database *sql.DB, pialID string) error {
	caps := GetCapabilities(database, pialID)
	keys := make([]string, 0, len(caps))
	for k := range caps { keys = append(keys, k) }
	sort.Strings(keys)
	type entry struct{ Cap, State string }
	entries := make([]entry, 0, len(keys))
	for _, k := range keys {
		entries = append(entries, entry{k, caps[k].State})
	}
	b, _ := json.Marshal(entries)
	h := sha256.Sum256(b)
	hash := hex.EncodeToString(h[:])
	_, err := database.Exec(`UPDATE pial_roots SET state_hash = $1 WHERE pial_id = $2`, hash, pialID)
	return err
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
