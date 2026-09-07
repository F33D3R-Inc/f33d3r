package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// Notification is one stored row.
type Notification struct {
	ID         string
	UserID     string
	Type       string
	ActorID    string
	TargetID   string
	TargetType string
	Payload    map[string]any
	IsRead     bool
	CreatedAt  time.Time
}

// NotificationGroup is what the Inbox draws: one row for a single event, or
// one row standing for several of the same kind on the same target within an
// hour. Grouping follows CLAUDE.md: like, repost and follow group; reply,
// quote, mention, tip, subscribe and thread_reply do not.
type NotificationGroup struct {
	ID         string
	Kind       string
	Actors     []*User
	ActorCount int
	TargetID   string
	TargetType string
	Preview    string
	AmountUAET int64
	IsRead     bool
	CreatedAt  time.Time
	// IDs of every row folded into this group, so marking it read marks all.
	memberIDs []string
}

// MemberIDs are the row ids this group stands for.
func (g *NotificationGroup) MemberIDs() []string { return g.memberIDs }

func groups(kind string) bool {
	switch kind {
	case "like", "repost", "follow":
		return true
	}
	return false
}

// Notify writes a notification unless the actor is the recipient — a user is
// never notified by their own actions.
func (s *Store) Notify(ctx context.Context, userID, kind, actorID, targetID, targetType string, payload map[string]any) error {
	if userID == "" || userID == actorID {
		return nil
	}
	if payload == nil {
		payload = map[string]any{}
	}
	raw, _ := json.Marshal(payload)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO notifications (id, user_id, type, actor_id, target_id, target_type, payload, is_read, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?)`,
		NewID(), userID, kind, nullable(actorID), targetID, targetType, string(raw), Now())
	return err
}

// UnreadCount is the badge number.
func (s *Store) UnreadCount(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notifications WHERE user_id = ? AND is_read = 0`, userID).Scan(&n)
	return n, err
}

// ListNotifications returns grouped notifications newest first. The cursor
// is the created_at of the last group returned; rows older than it come next.
func (s *Store) ListNotifications(ctx context.Context, userID string, limit int, cursor string) ([]*NotificationGroup, string, error) {
	before, err := cursorOrNow(cursor)
	if err != nil {
		return nil, "", err
	}
	// Over-fetch raw rows: grouping shrinks the list, and the page is measured
	// in groups.
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, type, COALESCE(actor_id, ''), target_id, target_type, payload, is_read, created_at
		FROM notifications WHERE user_id = ? AND created_at < ?
		ORDER BY created_at DESC LIMIT ?`, userID, before, limit*8)
	if err != nil {
		return nil, "", err
	}
	var raw []Notification
	for rows.Next() {
		var n Notification
		var isRead int
		var payload, created string
		if err := rows.Scan(&n.ID, &n.Type, &n.ActorID, &n.TargetID, &n.TargetType, &payload, &isRead, &created); err != nil {
			rows.Close()
			return nil, "", err
		}
		n.IsRead = isRead == 1
		n.CreatedAt = ParseTime(created)
		json.Unmarshal([]byte(payload), &n.Payload)
		raw = append(raw, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	var out []*NotificationGroup
	open := map[string]*NotificationGroup{}
	actorIDs := map[string]bool{}
	for _, n := range raw {
		key := n.Type + "|" + n.TargetType + "|" + n.TargetID
		if g, ok := open[key]; ok && groups(n.Type) && g.CreatedAt.Sub(n.CreatedAt) <= time.Hour {
			g.memberIDs = append(g.memberIDs, n.ID)
			g.ActorCount++
			if !n.IsRead {
				g.IsRead = false
			}
			if n.ActorID != "" {
				actorIDs[n.ActorID] = true
				g.Actors = append(g.Actors, &User{ID: n.ActorID})
			}
			continue
		}
		if len(out) >= limit {
			break
		}
		g := &NotificationGroup{
			ID:         n.ID,
			Kind:       n.Type,
			ActorCount: 1,
			TargetID:   n.TargetID,
			TargetType: n.TargetType,
			IsRead:     n.IsRead,
			CreatedAt:  n.CreatedAt,
			memberIDs:  []string{n.ID},
		}
		if p, ok := n.Payload["preview"].(string); ok {
			g.Preview = p
		}
		if a, ok := n.Payload["amount_uaet"].(float64); ok {
			g.AmountUAET = int64(a)
		}
		if n.ActorID != "" {
			actorIDs[n.ActorID] = true
			g.Actors = append(g.Actors, &User{ID: n.ActorID})
		}
		out = append(out, g)
		if groups(n.Type) {
			open[key] = g
		}
	}

	ids := make([]string, 0, len(actorIDs))
	for id := range actorIDs {
		ids = append(ids, id)
	}
	users, err := s.GetUsersByID(ctx, ids)
	if err != nil {
		return nil, "", err
	}
	for _, g := range out {
		resolved := make([]*User, 0, len(g.Actors))
		seen := map[string]bool{}
		for _, a := range g.Actors {
			u := users[a.ID]
			if u == nil || seen[u.ID] {
				continue
			}
			seen[u.ID] = true
			resolved = append(resolved, u)
		}
		g.Actors = resolved
	}

	next := ""
	if len(out) >= limit && len(raw) > 0 {
		next = FormatTime(out[len(out)-1].CreatedAt)
	}
	if out == nil {
		out = []*NotificationGroup{}
	}
	return out, next, nil
}

// MarkNotificationRead marks one row — or, given a group's member ids, all of
// them — read for the owner.
func (s *Store) MarkNotificationsRead(ctx context.Context, userID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	q, args := inClause(ids)
	_, err := s.db.ExecContext(ctx, `UPDATE notifications SET is_read = 1 WHERE user_id = ? AND id IN (`+q+`)`,
		append([]any{userID}, args...)...)
	return err
}

// GroupMembers resolves a group id (its newest row) to every row it folds.
func (s *Store) GroupMembers(ctx context.Context, userID, headID string) ([]string, error) {
	var kind, targetID, targetType, created string
	err := s.db.QueryRowContext(ctx, `SELECT type, target_id, target_type, created_at FROM notifications WHERE id = ? AND user_id = ?`,
		headID, userID).Scan(&kind, &targetID, &targetType, &created)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if !groups(kind) {
		return []string{headID}, nil
	}
	head := ParseTime(created)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM notifications WHERE user_id = ? AND type = ? AND target_id = ? AND target_type = ?
		  AND created_at <= ? AND created_at >= ?`,
		userID, kind, targetID, targetType, created, FormatTime(head.Add(-time.Hour)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// MarkAllRead clears the badge.
func (s *Store) MarkAllRead(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE notifications SET is_read = 1 WHERE user_id = ? AND is_read = 0`, userID)
	return err
}
