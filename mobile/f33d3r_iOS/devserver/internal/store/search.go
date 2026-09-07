package store

import (
	"context"
	"strings"
)

// SearchWorks is a body and tag substring search, newest first. Blocked,
// deleted, expired and unpublished rows are excluded as everywhere else.
func (s *Store) SearchWorks(ctx context.Context, query, viewerID string, limit int) ([]*Work, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return []*Work{}, nil
	}
	now := Now()
	where, args := baseWhere(now)
	where += ` AND w.subscriber_only = 0 AND (lower(w.body) LIKE ? OR EXISTS (SELECT 1 FROM json_each(w.tags) tg WHERE lower(tg.value) = ?))`
	args = append(args, "%"+escapeLike(q)+"%", strings.TrimPrefix(q, "#"), limit)
	rows, err := s.db.QueryContext(ctx, worksSelectSQL+where+` ORDER BY w.created_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	works, err := collectWorks(rows)
	if err != nil {
		return nil, err
	}
	if works == nil {
		works = []*Work{}
	}
	return works, s.Hydrate(ctx, works, viewerID)
}

// SearchUsers matches handle or display name prefixes and substrings.
func (s *Store) SearchUsers(ctx context.Context, query string, limit int) ([]*User, error) {
	q := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(query, "@")))
	if q == "" {
		return []*User{}, nil
	}
	rows, err := s.db.QueryContext(ctx, userSelectSQL+`
		WHERE u.handle LIKE ? OR lower(p.display_name) LIKE ?
		ORDER BY CASE WHEN u.handle LIKE ? THEN 0 ELSE 1 END, p.follower_count DESC, u.handle
		LIMIT ?`, "%"+escapeLike(q)+"%", "%"+escapeLike(q)+"%", escapeLike(q)+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	if out == nil {
		out = []*User{}
	}
	return out, rows.Err()
}

func escapeLike(s string) string {
	return strings.NewReplacer(`%`, `\%`, `_`, `\_`).Replace(s)
}
