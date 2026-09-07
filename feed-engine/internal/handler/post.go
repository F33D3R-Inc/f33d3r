package handler

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/sitra"
)

var mentionExtractRe = regexp.MustCompile(`@([A-Za-z0-9_]{1,50})`)

// maxMentionsPerWork bounds how many distinct handles one body of text can send
// to the naming plane. A work with fifty @s is not fifty notifications, it is a
// spray, and the bound belongs on the resolver call rather than on the caller.
const maxMentionsPerWork = 20

// fireMentionNotifications scans body for @handles and sends mention notifications.
// Runs as a goroutine — never blocks the HTTP response.
//
// The handles are resolved through Manhattan, in one batch, because an @mention
// is a name and a name is what the naming plane is for. This used to join
// users.handle: that answered with whoever held the handle when the row was
// written, so a mention of a transferred handle notified the previous holder and
// nothing anywhere could tell. Resolution now yields a PIAL, and only then does
// this brain look up its own account for that identity.
func (h *Handler) fireMentionNotifications(body, postID, actorID, actorHandle string) {
	h.fireMentionNotificationsExcept(body, postID, actorID, actorHandle, nil)
}

// fireMentionNotificationsExcept is fireMentionNotifications with the set of
// account ids this work has already notified by another kind.
//
// The set exists because a reply that opens with "@them" names the same person
// twice — once as the person replied to, once as a name in the text — and two
// rows for one act is a list that lies about how much happened. The caller that
// wrote the first row owns the set, so the two decisions are made in order
// rather than raced between goroutines.
func (h *Handler) fireMentionNotificationsExcept(body, postID, actorID, actorHandle string, told map[string]bool) {
	if h.db == nil {
		return
	}
	matches := mentionExtractRe.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return
	}
	seen := make(map[string]bool)
	var handles []string
	for _, m := range matches {
		lc := strings.ToLower(m[1])
		if seen[lc] {
			continue
		}
		seen[lc] = true
		handles = append(handles, lc)
		if len(handles) == maxMentionsPerWork {
			break
		}
	}

	// Detached from the request that produced it: nobody is waiting on a mention
	// notification, so the budget is generous enough that a slow plane delays
	// delivery rather than dropping it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pialByHandle, err := h.resolveHandlesToPIALs(ctx, handles)
	if err != nil {
		// The plane being unreachable is not "nobody was mentioned". Falling back
		// to the local handle join here is exactly the drift Manhattan exists to
		// end, so this reports and delivers nothing.
		log.Printf("[mention] resolving %d handle(s) for work %s through the naming plane failed: %v",
			len(handles), postID, err)
		return
	}
	if len(pialByHandle) == 0 {
		return
	}

	pials := make([]string, 0, len(pialByHandle))
	for _, pial := range pialByHandle {
		pials = append(pials, pial)
	}
	targets, err := dbpkg.AccountsByPIAL(h.db, pials)
	if err != nil {
		log.Printf("[mention] loading accounts for %d mentioned identity(ies) on work %s: %v",
			len(pials), postID, err)
		return
	}

	preview := body
	if len(preview) > 80 {
		preview = preview[:80]
	}
	for _, pial := range pials {
		target, ok := targets[pial]
		if !ok || target.UserID == actorID || told[target.UserID] {
			continue
		}
		h.notifyUser(target.UserID, "mention", actorID, postID, "work")
		go h.HeraldNotify(pial, "mention",
			"@"+actorHandle+" mentioned you", preview,
			"", "/work/"+postID)
	}
}

// facetPostReplies: TODO_WORKS — superseded by facetWorkReplies at /facets/works/replies/{id}.
func (h *Handler) facetPostReplies(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/facets/works/replies/"+r.PathValue("id"), http.StatusMovedPermanently)
}

// facetPostEngagement: TODO_WORKS — migrate to works-based engagement facet.
func (h *Handler) facetPostEngagement(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not found", http.StatusNotFound)
}

func (h *Handler) searchPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	searchType := r.URL.Query().Get("type") // "top" | "people" | "posts"
	if searchType == "" {
		searchType = "top"
	}

	var works []*model.Work
	var people []dbpkg.MentionResult
	if query != "" && h.db != nil {
		searchTerm := strings.TrimPrefix(strings.TrimPrefix(query, "@"), "#")
		if searchType != "people" {
			works, _ = dbpkg.SearchWorks(h.db, searchTerm, 30, user.ID)
			for _, wk := range works {
				wk.TimeAgo = TimeAgo(wk.CreatedAt)
			}
		}
		if searchType != "posts" {
			people, _ = dbpkg.SearchHandlesFiltered(h.db, searchTerm, 20, user.IsMinor, user.IsAdult)
		}
	} else if searchType == "people" && h.db != nil {
		// "People you might know" — the People tab with nothing typed is a
		// suggestion surface, not a search result, so it goes through the one
		// suggestion owner and its quality bar exactly like the rail does.
		suggested, sErr := dbpkg.SuggestPeople(h.db, suggestionViewer(user), dbpkg.SuggestionQuery{Limit: 20})
		if sErr != nil {
			log.Printf("[search] people you might know: %v", sErr)
		}
		for _, s := range suggested {
			people = append(people, dbpkg.MentionResult{
				Handle:      s.Handle,
				DisplayName: s.DisplayName,
				AvatarURL:   s.AvatarURL,
				IsVerified:  s.IsVerified,
			})
		}
	}

	works = filterAdultForViewer(user, works)
	h.render(w, r, "search.html", h.withRail(map[string]interface{}{
		"User":              user,
		"Surface":           "search",
		"SessionID":         uuid.New().String(),
		"ShowScores":        h.cfg.ShowScores,
		"CurrentUserHandle": user.Handle,
		"Title":             "Search · F33D3R",
		"Themes":            ThemesWithActive(user.ThemeID),
		"SearchQuery":       query,
		"SearchType":        searchType,
		"SearchResults":     works,
		"PeopleResults":     people,
	}, user, "search"))
}

func (h *Handler) editPost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", 405)
		return
	}
	_ = r.ParseForm()
	postID := strings.TrimSpace(r.FormValue("post_id"))
	newBody := strings.TrimSpace(r.FormValue("body"))
	if postID == "" || newBody == "" {
		http.Error(w, "missing fields", http.StatusBadRequest)
		return
	}
	if len([]rune(newBody)) > workBodyMaxRunes {
		http.Error(w, "too long", http.StatusBadRequest)
		return
	}

	user := h.userFromRequest(w, r)
	if h.db == nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Verify ownership and 60-minute edit window
	var createdAt time.Time
	var authorID string
	err := h.db.QueryRow(
		"SELECT author_id, created_at FROM works WHERE id = $1", postID,
	).Scan(&authorID, &createdAt)
	if err != nil {
		http.Error(w, "post not found", http.StatusNotFound)
		return
	}
	if authorID != user.ID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if time.Since(createdAt) > 60*time.Minute {
		http.Error(w, "edit window has closed (60 minutes)", http.StatusForbidden)
		return
	}

	_, err = h.db.Exec(
		"UPDATE works SET body = $1, is_edited = true, edited_at = NOW() WHERE id = $2 AND author_id = $3",
		newBody, postID, user.ID,
	)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("HX-Trigger", `{"postEdited":"true"}`)
	w.WriteHeader(http.StatusOK)
}

// NOTE: /api/post/delete (deletePost → DELETE FROM posts) and /api/poll
// (createPoll → INSERT INTO posts) were removed with the posts lane. Deletion is
// the work_delete event onto works.deleted_at; a poll is created the same way as
// any other work — the compose surface signs one carrying poll_options and
// poll_ends_at, and submitPollCompose has POSTed that shape for some time. Both
// legacy handlers were still writing to a table no feed reads.

// ── Poll voting ───────────────────────────────────────────────────────────────
func (h *Handler) castPollVote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", 405)
		return
	}
	user := h.userFromRequest(w, r)
	_ = r.ParseForm()

	postID := r.FormValue("post_id")
	optionStr := r.FormValue("option_idx")
	if postID == "" || optionStr == "" {
		http.Error(w, "missing params", 400)
		return
	}

	// Which wireframe drew the ballot. A work is drawn by more than one wireframe
	// on the same page, so the fragment must come back addressed to the ballot
	// that was clicked — see poll_card's Surface input.
	surface := pollSurface(r.FormValue("surface"))

	optionIdx, err := strconv.Atoi(optionStr)
	if err != nil || optionIdx < 0 {
		http.Error(w, "invalid option", 400)
		return
	}

	// A ballot already cast is not an error — the vote stands and the ballot is
	// what the voter asked to see. Casting is idempotent (one row per work+voter),
	// so both paths end at the same render.
	if _, err := dbpkg.CastPollVote(h.db, postID, user.ID, optionIdx); err != nil {
		log.Printf("[poll] cast vote work=%s voter=%s: %v", postID, user.ID, err)
		http.Error(w, "vote failed", 500)
		return
	}

	// Render the ballot back over #poll-{surface}-{postID} via outerHTML swap.
	poll, err := dbpkg.GetPollResults(h.db, postID, user.ID)
	if err != nil || poll == nil {
		log.Printf("[poll] results work=%s: %v", postID, err)
		http.Error(w, "vote recorded, ballot unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "poll_card", map[string]interface{}{
		"PostID":   postID,
		"Poll":     poll,
		"Surface":  surface,
		"ReadOnly": false,
	})
}

// pollSurface constrains the requested render surface to the wireframes that
// actually draw a votable ballot. An unknown value falls back to the desktop
// card rather than being echoed into a DOM id.
func pollSurface(s string) string {
	switch s {
	case "pcd", "pcf":
		return s
	default:
		return "pcd"
	}
}

// ── Thread ────────────────────────────────────────────────────────────────────

// createThread accepts a JSON body: {"segments":["text1","text2",...],"comment_gating":"open"}
// It inserts the first segment as a root post, then chains the rest as self-replies.
// Returns JSON {"root_id":"<uuid>"}.

// threadChainAPI: TODO_WORKS — superseded by works-based conversation endpoint.
func (h *Handler) threadChainAPI(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not found", http.StatusNotFound)
}

// ── Sitra Achra helpers ───────────────────────────────────────────────────────

// publishContentEvent fires a content.events message to Redpanda after a post
// is successfully inserted. Always runs in a goroutine — never blocks the response.
// workID is the works-table UUID when the content was created via the Malkuth
// signed-work path; pass empty string for legacy posts-table content.
func (h *Handler) publishContentEvent(postID, workID, pialID, body string, hasMedia, hasPoll bool) {
	if h.sitra == nil {
		return
	}
	msg := map[string]interface{}{
		"event":     "post.created",
		"post_id":   postID,
		"pial_id":   pialID,
		"body":      body,
		"has_media": hasMedia,
		"has_poll":  hasPoll,
		// The state the row actually opens in. Announcing 'clean' here told
		// every scanner the verdict was already in and to skip the content.
		"scan_state": "pending",
		"created_at": time.Now().UTC().Format(time.RFC3339),
	}
	if workID != "" {
		msg["work_id"] = workID
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		log.Printf("[sitra] marshal content event: %v", err)
		return
	}
	h.sitra.Publish(context.Background(), sitra.TopicContent, []byte(pialID), payload)
}

// publishRankingEvent fires a ranking.events message to Redpanda after a
// user interaction (like, unlike, repost, view). Always runs in a goroutine.
func (h *Handler) publishRankingEvent(eventType, postID, actorPIAL string) {
	if h.sitra == nil {
		return
	}
	payload, err := json.Marshal(map[string]interface{}{
		"event":      "post.interaction",
		"event_type": eventType,
		"post_id":    postID,
		"actor_pial": actorPIAL,
		"created_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		log.Printf("[sitra] marshal ranking event: %v", err)
		return
	}
	h.sitra.Publish(context.Background(), sitra.TopicRanking, []byte(actorPIAL), payload)
}
