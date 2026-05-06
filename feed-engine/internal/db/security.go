package db

import (
	"database/sql"
	"encoding/json"
	"time"
)

// ContentReport mirrors the content_reports table row.
type ContentReport struct {
	ID             string
	ReporterHandle string
	ContentID      string
	ContentType    string
	Reason         string
	Detail         string
	Status         string
	PostBody       string // populated via join when content_type=post
	CreatedAt      time.Time
}

// GetPendingReports returns the most recent pending content reports for the
// admin moderation queue.
func GetPendingReports(database *sql.DB, limit int) ([]ContentReport, error) {
	rows, err := database.Query(`
		SELECT
			cr.id,
			COALESCE(u.handle, 'deleted')   AS reporter_handle,
			cr.content_id,
			cr.content_type,
			cr.reason,
			cr.detail,
			cr.status,
			COALESCE(p.body, '')             AS post_body,
			cr.created_at
		FROM content_reports cr
		LEFT JOIN users u ON cr.reporter_id = u.id
		LEFT JOIN posts p ON cr.content_type = 'post'
		                 AND cr.content_id = p.id::text
		WHERE cr.status = 'pending'
		ORDER BY cr.created_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var reports []ContentReport
	for rows.Next() {
		var r ContentReport
		if err := rows.Scan(
			&r.ID, &r.ReporterHandle, &r.ContentID, &r.ContentType,
			&r.Reason, &r.Detail, &r.Status, &r.PostBody, &r.CreatedAt,
		); err != nil {
			continue
		}
		reports = append(reports, r)
	}
	return reports, nil
}

// ResolveReport marks a report as resolved or dismissed.
func ResolveReport(database *sql.DB, reportID, status, resolvedBy string) error {
	_, err := database.Exec(`
		UPDATE content_reports
		SET status = $1, resolved_by = $2, resolved_at = NOW()
		WHERE id = $3`, status, resolvedBy, reportID)
	return err
}

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
func SetTrustScore(database *sql.DB, pialID string, trustLevel, nsfwTier int, state string) error {
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
func LogEnforcementAction(database *sql.DB, pialID, actionType, reason, actor string, evidence map[string]interface{}) {
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
