package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// ContentReport mirrors a piece of reported content in the moderation queue.
// Reports are aggregated per content_id (one card per item, not per row) so a
// post reported by N users shows as a single card carrying ReportCount.
type ContentReport struct {
	ID             string
	ReporterHandle string // comma-joined distinct reporter handles
	ContentID      string
	ContentType    string
	Reason         string // comma-joined distinct reasons across all reporters
	Detail         string
	Status         string
	PostBody       string // populated via join when content_type=post
	ReportCount    int    // number of pending reports collapsed into this card
	CreatedAt      time.Time
}

// GetPendingReports returns pending content reports for the admin moderation
// queue, collapsed to one card per reported item. Multiple reporters on the same
// content aggregate into a single row with ReportCount, so the queue never shows
// duplicate cards for the same post.
func GetPendingReports(database *sql.DB, limit int) ([]ContentReport, error) {
	rows, err := database.Query(`
		SELECT
			MIN(cr.id::text)                                       AS id,
			COALESCE(string_agg(DISTINCT u.handle, ', '),'deleted') AS reporter_handle,
			cr.content_id,
			cr.content_type,
			string_agg(DISTINCT cr.reason, ', ')                  AS reason,
			COALESCE(MAX(NULLIF(cr.detail, '')), '')              AS detail,
			COALESCE(MAX(p.body), MAX(wk.body), '')               AS post_body,
			COUNT(*)                                              AS report_count,
			MAX(cr.created_at)                                    AS created_at
		FROM content_reports cr
		LEFT JOIN users u ON cr.reporter_id = u.id
		LEFT JOIN posts p ON cr.content_type = 'post'
		                 AND cr.content_id = p.id::text
		LEFT JOIN works wk ON cr.content_id = wk.id::text
		WHERE cr.status = 'pending'
		GROUP BY cr.content_id, cr.content_type
		ORDER BY MAX(cr.created_at) DESC
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
			&r.Reason, &r.Detail, &r.PostBody, &r.ReportCount, &r.CreatedAt,
		); err != nil {
			continue
		}
		r.Status = "pending"
		reports = append(reports, r)
	}
	return reports, nil
}

// ResolveReportsForContent marks every pending report on a piece of content as
// resolved/dismissed in one shot, and returns how many rows were closed. This is
// the single exit point that keeps the report queue in sync with moderator
// decisions: any content action (approve/age_gate/block) or explicit resolve
// closes ALL open reports pointing at that item, so nothing gets stuck pending.
func ResolveReportsForContent(database *sql.DB, contentID, status, resolvedBy string) (int64, error) {
	res, err := database.Exec(`
		UPDATE content_reports
		SET status = $1, resolved_by = $2, resolved_at = NOW()
		WHERE content_id = $3 AND status = 'pending'`, status, resolvedBy, contentID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ClearReportDrivenReview returns a post to 'clean' when it sits in human_review
// solely because a user report pushed it there — i.e. the AI scan did not
// independently flag it for hold or block. Without this, dismissing/resolving the
// report would strand the post in the review queue forever. It is a no-op unless
// the content is currently in human_review, so it never overrides an explicit
// age_gate/block decision.
func ClearReportDrivenReview(database *sql.DB, contentID, actor string) error {
	var state string
	err := database.QueryRow(`SELECT scan_state FROM posts WHERE id = $1`, contentID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		err = database.QueryRow(`SELECT COALESCE(scan_state,'clean') FROM works WHERE id = $1`, contentID).Scan(&state)
	}
	if err != nil {
		return nil // content not found / not a post or work — nothing to unstick
	}
	if state != "human_review" {
		return nil // only unstick items actually held in review
	}
	// If an AI scan independently wants this held or blocked, leave it in the queue.
	var rec string
	if e := database.QueryRow(
		`SELECT COALESCE(recommendation,'') FROM content_scan_results WHERE post_id = $1`,
		contentID).Scan(&rec); e == nil {
		if rec == "human_review" || rec == "auto_block" {
			return nil
		}
	}
	return SetPostScanState(database, contentID, "clean", "report resolved: no independent AI hold", actor)
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
