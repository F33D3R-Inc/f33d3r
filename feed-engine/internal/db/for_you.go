package db

import (
	"database/sql"

	"github.com/f33d3r/feed-engine/internal/model"
)

// GetWorksPopular returns what people are responding to now: reactions and replies from
// the last week, weighted by how much effort each takes, cooling with age. A new post
// starts at zero and can still appear: the age term ranks it above old quiet ones.
func GetWorksPopular(db *sql.DB, limit int) ([]*model.Work, error) {
	rows, err := db.Query(`
		SELECT w.id::text
		FROM works w
		WHERE w.deleted_at IS NULL AND `+workNotBlockedSQL+`
		  AND NOT w.subscriber_only AND w.kind <> 'reply'
		  AND w.created_at > NOW() - INTERVAL '7 days'
		ORDER BY (1
		    + COALESCE((SELECT SUM(CASE wr.reaction_type WHEN 'like' THEN 1 WHEN 'repost' THEN 3
		                                WHEN 'bookmark' THEN 4 WHEN 'dislike' THEN -1 ELSE 0 END)
		                FROM work_reactions wr WHERE wr.work_id = w.id), 0)
		    + 2 * (SELECT COUNT(*) FROM work_citations wc WHERE wc.target_id = w.id AND wc.citation_type = 'reply')
		  ) / POWER(EXTRACT(EPOCH FROM NOW() - w.created_at) / 3600 + 2, 1.5) DESC, w.id DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return GetWorksByIDs(db, ids)
}

// NotWanted is what a person has said they don't want to see more of.
type NotWanted struct {
	Works   map[string]bool
	Authors map[string]bool
}

// notWantedAuthorAfter is how many separate signals about one person it takes to treat
// it as being about them rather than one post.
const notWantedAuthorAfter = 2

// GetNotWanted reads the last 30 days of "not interested" and dislikes.
func GetNotWanted(db *sql.DB, viewerID string) (NotWanted, error) {
	out := NotWanted{Works: map[string]bool{}, Authors: map[string]bool{}}
	rows, err := db.Query(`
		SELECT w.id::text, w.author_id::text
		FROM feedback_events fe
		JOIN works w ON w.id::text = fe.content_id
		WHERE fe.user_id = $1 AND fe.event_type = 'negative_feedback'
		  AND fe.created_at > NOW() - INTERVAL '30 days'
		UNION
		SELECT w.id::text, w.author_id::text
		FROM work_reactions wr
		JOIN works w ON w.id = wr.work_id
		WHERE wr.reactor_id = $1::uuid AND wr.reaction_type = 'dislike'
		  AND wr.created_at > NOW() - INTERVAL '30 days'`, viewerID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	perAuthor := map[string]int{}
	for rows.Next() {
		var work, author string
		if err := rows.Scan(&work, &author); err != nil {
			return out, err
		}
		out.Works[work] = true
		perAuthor[author]++
		if perAuthor[author] >= notWantedAuthorAfter {
			out.Authors[author] = true
		}
	}
	return out, rows.Err()
}
