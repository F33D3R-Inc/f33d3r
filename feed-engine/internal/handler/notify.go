package handler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

// publishNewPostToFollowers is the follower_fanout obligation: it tells the
// author's followers there is something new. The stream carries a count for the
// web and the work's id for the app; the card is read per person. Who is told is
// decided in SQL (FanoutFollowerPIALs) against the following feed's own rules,
// replies included, so a reply is never announced as a post.
func (h *Handler) publishNewPostToFollowers(posterUserID, workID string) error {
	_, _, authorHandle, err := dbpkg.WorkFanoutFacts(h.db, workID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // removed before it was announced
	}
	if err != nil {
		return fmt.Errorf("reading work %s for fan-out: %w", workID, err)
	}
	twin, err := json.Marshal(map[string]string{"work_id": workID, "author_handle": authorHandle})
	if err != nil {
		return fmt.Errorf("encoding fan-out twin of %s: %w", workID, err)
	}
	return h.fanOutNewPost(posterUserID, workID, string(twin))
}

// publishRepostToFollowers announces a repost to the reposter's followers.
//
// A repost is a first-class feed item — GetWorksFeedFollowing unions reposts by
// followed accounts into the feed at the time of the repost — so a feed that is
// being kept live must be told about one.
//
// The twin names both people. The card is the work; the reason it is on this
// person's screen is the reposter, and a client that was only told the author
// cannot draw the line that says so.
func (h *Handler) publishRepostToFollowers(reposterID, workID string) {
	if reposterID == "" || workID == "" {
		return
	}
	_, _, authorHandle, err := dbpkg.WorkFanoutFacts(h.db, workID)
	if err != nil {
		log.Printf("[repost-sse] work %s: %v", workID, err)
		return
	}
	var reposterHandle string
	if err := h.db.QueryRow(`SELECT handle FROM users WHERE id = $1::uuid`, reposterID).Scan(&reposterHandle); err != nil {
		log.Printf("[repost-sse] reposter %s: %v", reposterID, err)
		return
	}
	twin, err := json.Marshal(map[string]string{
		"work_id":       workID,
		"author_handle": authorHandle,
		"reposted_by":   reposterHandle,
	})
	if err != nil {
		log.Printf("[repost-sse] twin %s: %v", workID, err)
		return
	}
	if err := h.fanOutNewPost(reposterID, workID, string(twin)); err != nil {
		log.Printf("[repost-sse] %v", err)
	}
}

// fanOutNewPost tells everyone following sourceUserID who may see the work. Who that is
// is decided per follower, in SQL, against the same rules the following feed reads with.
func (h *Handler) fanOutNewPost(sourceUserID, workID, twin string) error {
	pials, err := dbpkg.FanoutFollowerPIALs(h.db, sourceUserID, workID)
	if err != nil {
		return fmt.Errorf("audience for work %s: %w", workID, err)
	}
	h.tellNewPost(pials)
	event := SSEEvent{Type: "new_post", Data: workID, JSON: twin}
	for _, pial := range pials {
		PublishToUser(pial, event)
	}
	return nil
}

// ── Notifications for engagement ─────────────────────────────────────────────

// notificationPreviewRunes is how much of what was said a notification row
// carries. Enough to recognise the thing; never the whole of it, because a row
// is a pointer and not a copy.
const notificationPreviewRunes = 80

func notificationPreview(body string) string {
	body = strings.TrimSpace(body)
	r := []rune(body)
	if len(r) <= notificationPreviewRunes {
		return body
	}
	return strings.TrimSpace(string(r[:notificationPreviewRunes])) + "…"
}

// publishCitedWorks is the cited_update obligation: everyone with a cited work
// on screen gets its new counts, and the thread of a replied-to work gets the new
// row. A cited work that is gone, or held by moderation, has nobody to update.
func (h *Handler) publishCitedWorks(workID string) error {
	targets, err := dbpkg.WorkCitationTargets(h.db, workID)
	if err != nil {
		return err
	}
	for _, t := range targets {
		cited, err := dbpkg.GetWorkByID(h.db, t.WorkID, "")
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("reloading cited work %s: %w", t.WorkID, err)
		}
		h.publishWorkEngagementFor(cited)
		if t.Type == "reply" {
			h.publishNewReply(t.WorkID, workID)
		}
	}
	return nil
}
