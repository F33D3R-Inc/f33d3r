package db

import (
	"database/sql"
	"encoding/json"
)

// GetTrustScore returns trust state for a PIAL root. Returns safe defaults if not found.
func GetTrustScore(database *sql.DB, pialID string) (trustLevel int, nsfwTier int, enforcementState string) {
	trustLevel, nsfwTier, enforcementState = 50, 0, "none"
	database.QueryRow(`
		SELECT trust_level, nsfw_tier, enforcement_state
		FROM trust_scores WHERE pial_id = $1`, pialID,
	).Scan(&trustLevel, &nsfwTier, &enforcementState)
	return
}

// SetTrustScore upserts a PIAL's trust entry.
func SetTrustScore(database DBTX, pialID string, trustLevel, nsfwTier int, state string) error {
	_, err := database.Exec(`
		INSERT INTO trust_scores (pial_id, trust_level, nsfw_tier, enforcement_state, updated_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (pial_id) DO UPDATE
		  SET trust_level        = EXCLUDED.trust_level,
		      nsfw_tier          = EXCLUDED.nsfw_tier,
		      enforcement_state  = EXCLUDED.enforcement_state,
		      updated_at         = NOW()`,
		pialID, trustLevel, nsfwTier, state)
	return err
}

// LogEnforcementAction appends an immutable entry to the enforcement log.
func LogEnforcementAction(database DBTX, pialID, actionType, reason, actor string, evidence map[string]interface{}) {
	if pialID == "" {
		return
	}
	b := []byte("{}")
	if evidence != nil {
		if enc, err := json.Marshal(evidence); err == nil {
			b = enc
		}
	}
	database.Exec(`
		INSERT INTO enforcement_actions (pial_id, action_type, reason, actor, evidence)
		VALUES ($1, $2, $3, $4, $5)`,
		pialID, actionType, reason, actor, string(b))
}
