package db

// atlas_review.go — persistence for the Facet Atlas admin review workflow.
// One row per facet recording the admin's visual sign-off.

import (
	"database/sql"
	"time"
)

// FacetReview is the stored review state for a single facet.
type FacetReview struct {
	Name       string
	Status     string // approved | needs_work | unreviewed
	Note       string
	ReviewedBy string
	ReviewedAt time.Time
}

// GetFacetReviews returns all stored reviews keyed by facet name. Facets with no
// row are simply absent (treated as "unreviewed" by callers).
func GetFacetReviews(database *sql.DB) map[string]FacetReview {
	out := map[string]FacetReview{}
	if database == nil {
		return out
	}
	rows, err := database.Query(
		`SELECT name, status, COALESCE(note,''), COALESCE(reviewed_by,''), reviewed_at FROM facet_reviews`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var r FacetReview
		if err := rows.Scan(&r.Name, &r.Status, &r.Note, &r.ReviewedBy, &r.ReviewedAt); err != nil {
			continue
		}
		out[r.Name] = r
	}
	return out
}

// SetFacetReview upserts a facet's review status. A status of "unreviewed" clears
// the row so the facet returns to the default state.
func SetFacetReview(database *sql.DB, name, status, note, by string) error {
	if database == nil {
		return nil
	}
	if status == "unreviewed" {
		_, err := database.Exec(`DELETE FROM facet_reviews WHERE name = $1`, name)
		return err
	}
	_, err := database.Exec(
		`INSERT INTO facet_reviews (name, status, note, reviewed_by, reviewed_at)
		 VALUES ($1, $2, $3, $4, now())
		 ON CONFLICT (name) DO UPDATE
		   SET status = EXCLUDED.status,
		       note = EXCLUDED.note,
		       reviewed_by = EXCLUDED.reviewed_by,
		       reviewed_at = now()`,
		name, status, note, by)
	return err
}
