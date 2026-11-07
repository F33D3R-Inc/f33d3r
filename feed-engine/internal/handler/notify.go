package handler

import (
	"database/sql"
	"encoding/json"
	"log"
	"strings"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

// publishNewPostToFollowers tells followers there is something new. The stream carries
// a count for the web and the work's id for the app; the card is read per person.
func (h *Handler) publishNewPostToFollowers(posterUserID, workID string) {
	if h.db == nil || h.partial == nil {
		return
	}
	kind, _, authorHandle, err := dbpkg.WorkKindAndAuthor(h.db, workID)
	if err != nil {
		log.Printf("[new-work-sse] work %s: %v", workID, err)
		return
	}
	if !newPostFanoutKind(kind) {
		return
	}
	twin, _ := json.Marshal(map[string]string{"work_id": workID, "author_handle": authorHandle})
	h.fanOutNewPost(posterUserID, workID, string(twin))
}

// newPostFanoutKind reports whether a work of this kind is a feed item, and so
// whether its arrival is a new_post for the people following its author.
//
// A reply is not. The following feed excludes kind='reply' — it always has —
// but the fan-out did not, so every reply anyone wrote was pushed to all of
// their followers as a card that the feed those followers were looking at
// would never have shown them, and that no amount of scrolling would reproduce.
// The exclusion belongs in one sentence that both paths can be read against.
func newPostFanoutKind(kind string) bool {
	return kind != "reply"
}

// publishRepostToFollowers announces a repost to the reposter's followers.
//
// A repost is a first-class feed item — GetWorksFeedFollowing unions reposts by
// followed accounts into the feed at the time of the repost — so a feed that is
// being kept live must be told about one. It never was: the stream only fired
// on an original work, which meant the live feed and the fetched feed disagreed
// about what had happened for as long as the reader stayed on the page.
//
// The twin names both people. The card is the work; the reason it is on this
// person's screen is the reposter, and a client that was only told the author
// cannot draw the line that says so.
func (h *Handler) publishRepostToFollowers(reposterID, workID string) {
	if h.db == nil || h.partial == nil || reposterID == "" || workID == "" {
		return
	}
	kind, _, authorHandle, err := dbpkg.WorkKindAndAuthor(h.db, workID)
	if err != nil {
		log.Printf("[repost-sse] work %s: %v", workID, err)
		return
	}
	if !newPostFanoutKind(kind) {
		return
	}
	var reposterHandle string
	_ = h.db.QueryRow(`SELECT handle FROM users WHERE id = $1::uuid`, reposterID).Scan(&reposterHandle)
	twin, _ := json.Marshal(map[string]string{
		"work_id":       workID,
		"author_handle": authorHandle,
		"reposted_by":   reposterHandle,
	})
	h.fanOutNewPost(reposterID, workID, string(twin))
}

// fanOutNewPost tells everyone following sourceUserID who may see the work. Who that is
// is decided per follower, in SQL, against the same rules the following feed reads with.
func (h *Handler) fanOutNewPost(sourceUserID, workID, twin string) {
	pials, err := dbpkg.FanoutFollowerPIALs(h.db, sourceUserID, workID)
	if err != nil {
		log.Printf("[new-work-sse] audience for work %s: %v", workID, err)
		return
	}
	h.tellNewPost(pials)
	event := SSEEvent{Type: "new_post", Data: workID, JSON: twin}
	for _, pial := range pials {
		PublishToUser(pial, event)
	}
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

// publishCitedWork updates anyone with the cited work on screen: its counts, and the
// new row in its replies. What the work owes people is queued with the work itself
// and paid by StartWorkNoticeWorker.
func (h *Handler) publishCitedWork(workID, kind, citedCID string) {
	if h.db == nil || citedCID == "" || (kind != "reply" && kind != "quote") {
		return
	}
	var citedWorkID string
	err := h.db.QueryRow(`SELECT id::text FROM works WHERE cid = $1 AND deleted_at IS NULL LIMIT 1`, citedCID).Scan(&citedWorkID)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Printf("[work] reading cited work %s: %v", citedCID, err)
		}
		return
	}
	h.publishWorkEngagement(citedWorkID)
	if kind == "reply" {
		h.publishNewReply(citedWorkID, workID)
	}
}
