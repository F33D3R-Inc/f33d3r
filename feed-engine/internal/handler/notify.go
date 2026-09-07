package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// notifyUser creates a notification, increments the recipient's unread count,
// and immediately pushes a notify SSE badge to any active SSE session.
// Replaces the scattered dbpkg.CreateNotification + IncrementUnreadCount pairs.
//
// The badge is raised only when a notification row was actually written. A
// dropped notification that still raised the badge is a count of nothing: the
// recipient is sent to a list that does not contain the thing the badge is
// promising, and the count never comes back down because marking a list read
// clears rows, not a phantom.
func (h *Handler) notifyUser(recipientID, kind, actorID, targetID, targetType string) {
	h.notifyUserWithPayload(recipientID, kind, actorID, targetID, targetType, nil)
}

// notifyUserWithPayload is notifyUser with what the row carries beyond its
// kind: a preview line, a tip amount in µAET. It writes the row, bumps the
// unread count, and pushes the badge to every session the recipient holds —
// rendered for the web, as {"unread": n} for a native client.
func (h *Handler) notifyUserWithPayload(recipientID, kind, actorID, targetID, targetType string, payload map[string]interface{}) {
	if h.db == nil {
		return
	}
	var (
		created bool
		err     error
	)
	if payload == nil {
		created, err = dbpkg.CreateNotification(h.db, recipientID, kind, actorID, targetID, targetType)
	} else {
		created, err = dbpkg.CreateNotificationWithPayload(h.db, recipientID, kind, actorID, targetID, targetType, payload)
	}
	if err != nil {
		log.Printf("[notify] creating %s for %s: %v", kind, recipientID, err)
		return
	}
	if !created {
		return
	}
	dbpkg.IncrementUnreadCount(h.db, recipientID)
	h.publishUnreadBadge(recipientID)
}

// publishUnreadBadge pushes an account's current unread count to every live
// connection the person behind it holds.
//
// It is separate from notifyUser because the badge changes in both directions
// and only one of them was ever announced. A notification arriving raised the
// badge everywhere; marking notifications read lowered it on the device that
// did the marking and nowhere else, so a person with a phone and a tab kept a
// badge that stood for nothing until they reloaded. The count is re-read from
// the rows each time rather than adjusted, because the rows are the truth and
// an arithmetic badge drifts.
func (h *Handler) publishUnreadBadge(recipientID string) {
	if h.db == nil || recipientID == "" {
		return
	}
	recipientPIAL := dbpkg.GetUserPIAL(h.db, recipientID)
	if recipientPIAL == "" {
		return
	}
	var count int
	if err := h.db.QueryRow(
		`SELECT COUNT(*) FROM notifications WHERE user_id = $1 AND is_read = false`,
		recipientID,
	).Scan(&count); err != nil {
		log.Printf("[notify] unread count for %s: %v", recipientID, err)
		return
	}

	// The event carries the rendered badge, not the number behind it. A count on
	// the wire is a state delta, and the browser is not permitted to render one.
	event := SSEEvent{Type: "notify", JSON: fmt.Sprintf(`{"unread":%d}`, count)}
	if h.partial != nil {
		var buf bytes.Buffer
		if err := h.partial.ExecuteTemplate(&buf, "notif_badge", map[string]interface{}{
			"PIALID":      recipientPIAL,
			"UnreadCount": count,
		}); err != nil {
			log.Printf("[notify] rendering notif_badge: %v", err)
		} else {
			event.Data = buf.String()
		}
	}
	PublishToUser(recipientPIAL, event)
}

// publishNewPostToFollowers fans out a new_work SSE event to followers.
// The fragment is rendered once through renderWorkCardByID — the single
// work_card entry point — and broadcast, so the card that arrives over the
// stream is the same card the same work renders as on an HTTP facet fetch.
// One render is deliberate: rendering per follower would scale with the follow
// graph, so the broadcast carries the non-viewer-relative card (see
// renderWorkCardSet).
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

// fanOutNewPost renders the card once and delivers it to everyone following
// sourceUserID who is allowed to see the work.
//
// One render is deliberate: rendering per follower would scale with the follow
// graph, so the broadcast carries the non-viewer-relative card (see
// renderWorkCardSet). Who receives it is decided per follower, in SQL, against
// the same predicates the following feed reads with.
func (h *Handler) fanOutNewPost(sourceUserID, workID, twin string) {
	fragment, err := h.renderWorkCardByID(workID, nil)
	if err != nil {
		log.Printf("[new-work-sse] %v", err)
		return
	}
	pials, err := dbpkg.FanoutFollowerPIALs(h.db, sourceUserID, workID)
	if err != nil {
		log.Printf("[new-work-sse] audience for work %s: %v", workID, err)
		return
	}
	event := SSEEvent{Type: "new_post", Data: string(fragment), JSON: twin}
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

// notifyReaction tells a work's author that someone liked or reposted it.
//
// Called from the reaction lane on the ADD half only: taking a like back is not
// an event anyone is told about, and the notification already written is left
// alone rather than retracted — a row that appears and vanishes is worse than
// one that stands. A dislike and a bookmark notify nobody at all: both are the
// reader's own record of the reader's own reading, and the author is not a
// party to either.
//
// A person never hears about their own hands: CreateNotification refuses a row
// whose actor is its recipient, and this refuses before it gets that far.
func (h *Handler) notifyReaction(actor *model.User, workID, reactionType string) {
	if h.db == nil || actor == nil || actor.ID == "" || workID == "" {
		return
	}
	kind := ""
	switch reactionType {
	case "like":
		kind = "like"
	case "repost":
		kind = "repost"
	default:
		return
	}
	authorID, authorPIAL, err := dbpkg.GetWorkAuthor(h.db, workID)
	if err != nil || authorID == "" || authorID == actor.ID {
		return
	}
	h.notifyUser(authorID, kind, actor.ID, workID, "work")
	verb := "liked your work"
	if kind == "repost" {
		verb = "reposted your work"
	}
	go h.HeraldNotify(authorPIAL, kind, "@"+actor.Handle+" "+verb, "", "", "/work/"+workID)
}

// notifyNewWork is everything a newly-created work owes the people it names.
//
// It runs as one goroutine rather than several because the people overlap: a
// reply that also @s the person being replied to is one event to that person,
// not two, and the only way to know that is to decide the reply first and hand
// the mention pass the name it has already told. Detached from the response —
// nobody posting is waiting to hear that their reply was announced.
//
// citedCID is the parent's CID for a reply and the quoted work's CID for a
// quote; it is empty for anything else.
func (h *Handler) notifyNewWork(actor *model.User, workID, kind, body, citedCID string) {
	if h.db == nil || actor == nil || actor.ID == "" {
		return
	}
	told := map[string]bool{actor.ID: true}

	if (kind == "reply" || kind == "quote") && citedCID != "" {
		var citedWorkID, citedAuthorID, citedAuthorPIAL string
		err := h.db.QueryRow(
			`SELECT id::text, author_id::text, COALESCE(author_pial::text,'')
			 FROM works WHERE cid = $1 AND deleted_at IS NULL LIMIT 1`, citedCID,
		).Scan(&citedWorkID, &citedAuthorID, &citedAuthorPIAL)
		if err == nil && citedAuthorID != "" && !told[citedAuthorID] {
			told[citedAuthorID] = true
			// The row points at the new work, not the cited one: tapping a
			// "replied to you" that opened the reader's own work would be a row
			// that hides the thing it is about.
			h.notifyUserWithPayload(citedAuthorID, kind, actor.ID, workID, "work",
				map[string]interface{}{"preview": notificationPreview(body)})
			verb := "replied to you"
			if kind == "quote" {
				verb = "quoted your work"
			}
			go h.HeraldNotify(citedAuthorPIAL, kind,
				"@"+actor.Handle+" "+verb, notificationPreview(body), "", "/work/"+workID)
		}
		if citedWorkID != "" {
			// The cited work's reply or quote count just moved, and anyone with
			// it on screen is owed the new numbers.
			h.publishWorkEngagement(citedWorkID)
		}
	}

	h.fireMentionNotificationsExcept(body, workID, actor.ID, actor.Handle, told)
}
