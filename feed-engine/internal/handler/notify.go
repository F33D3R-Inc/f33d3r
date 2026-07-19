package handler

import (
	"bytes"
	"fmt"
	"log"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// Ensure model import used — Work type referenced below.
var _ = model.Work{}

// notifyUser creates a notification, increments the recipient's unread count,
// and immediately pushes a notify SSE badge to any active SSE session.
// Replaces the scattered dbpkg.CreateNotification + IncrementUnreadCount pairs.
func (h *Handler) notifyUser(recipientID, kind, actorID, targetID, targetType string) {
	if h.db == nil {
		return
	}
	dbpkg.CreateNotification(h.db, recipientID, kind, actorID, targetID, targetType)
	dbpkg.IncrementUnreadCount(h.db, recipientID)

	recipientPIAL := dbpkg.GetUserPIAL(h.db, recipientID)
	if recipientPIAL == "" {
		return
	}
	var count int
	h.db.QueryRow(
		`SELECT COUNT(*) FROM notifications WHERE user_id = $1 AND is_read = false`,
		recipientID,
	).Scan(&count)
	badge := ""
	if count > 9 {
		badge = "9+"
	} else if count > 0 {
		badge = fmt.Sprintf("%d", count)
	}
	PublishToUser(recipientPIAL, SSEEvent{Type: "notify", Data: badge})
}

// publishNewPostToFollowers fans out a new_work SSE event to followers.
func (h *Handler) publishNewPostToFollowers(posterUserID, workID string) {
	if h.db == nil || h.partial == nil {
		return
	}

	work, err := dbpkg.GetWorkByID(h.db, workID, "")
	if err != nil || work == nil {
		return
	}
	dbpkg.EnrichWorksWithQuotes(h.db, []*model.Work{work})
	work.TimeAgo = TimeAgo(work.CreatedAt)

	ctx := map[string]interface{}{
		"SessionID":  uuid.New().String(),
		"Surface":    "feed",
		"ShowScores": h.cfg.ShowScores,
	}
	var buf bytes.Buffer
	if err := h.partial.ExecuteTemplate(&buf, "work_card", map[string]interface{}{
		"W":   work,
		"Ctx": ctx,
	}); err != nil {
		log.Printf("[new-work-sse] render error for work %s: %v", workID, err)
		return
	}
	html := buf.String()
	if html == "" {
		return
	}

	rows, err := h.db.Query(`
		SELECT COALESCE(u.pial_id::text, '')
		FROM follows f
		JOIN users u ON u.id = f.follower_id
		WHERE f.following_id = $1
		  AND u.pial_id IS NOT NULL`,
		posterUserID)
	if err != nil {
		return
	}
	defer rows.Close()
	event := SSEEvent{Type: "new_post", Data: html}
	for rows.Next() {
		var pial string
		rows.Scan(&pial)
		if pial != "" {
			PublishToUser(pial, event)
		}
	}
}
