package handler

import (
	"context"
	"encoding/json"
	"fmt"
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

// fireMentionNotifications scans body for @handles and sends mention notifications.
// Runs as a goroutine — never blocks the HTTP response.
func (h *Handler) fireMentionNotifications(body, postID, actorID, actorHandle string) {
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
		if !seen[lc] {
			seen[lc] = true
			handles = append(handles, lc)
		}
	}
	resolved, err := dbpkg.ResolveMentionHandles(h.db, handles)
	if err != nil || len(resolved) == 0 {
		return
	}
	preview := body
	if len(preview) > 80 { preview = preview[:80] }
	for _, info := range resolved {
		uid := info[0]
		if uid == actorID { continue }
		h.notifyUser(uid, "mention", actorID, postID, "post")
		go h.HeraldNotify(dbpkg.GetUserPIAL(h.db, uid), "mention",
			"@"+actorHandle+" mentioned you", preview,
			"", "/post/"+postID)
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
	user       := h.userFromRequest(w, r)
	query      := strings.TrimSpace(r.URL.Query().Get("q"))
	searchType := r.URL.Query().Get("type") // "top" | "people" | "posts"
	if searchType == "" {
		searchType = "top"
	}

	var works  []*model.Work
	var people []dbpkg.MentionResult
	if query != "" && h.db != nil {
		searchTerm := strings.TrimPrefix(strings.TrimPrefix(query, "@"), "#")
		if searchType != "people" {
			works, _ = dbpkg.SearchWorks(h.db, searchTerm, 30, user.ID)
			for _, wk := range works { wk.TimeAgo = TimeAgo(wk.CreatedAt) }
		}
		if searchType != "posts" {
			people, _ = dbpkg.SearchHandlesFiltered(h.db, searchTerm, 20, user.IsMinor, user.IsAdult)
		}
	} else if searchType == "people" && h.db != nil {
		suggested, _ := dbpkg.GetSuggestedUsersFiltered(h.db, user.ID, 20, user.IsMinor)
		for _, s := range suggested {
			people = append(people, dbpkg.MentionResult{
				Handle:      s.Handle,
				DisplayName: s.DisplayName,
				AvatarURL:   s.AvatarURL,
			})
		}
	}

	works = filterAdultForViewer(user, works)
	rail := h.railData(user, "search")
	h.render(w, "search.html", map[string]interface{}{
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
		"TrendingTags":      rail["TrendingTags"],
		"SuggestedUsers":    rail["SuggestedUsers"],
		"RailContext":       rail["RailContext"],
		"RailNewsItems":     rail["RailNewsItems"],
		"RailNewsLabel":     rail["RailNewsLabel"],
	})
}


func (h *Handler) editPost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "405", 405); return }
	_ = r.ParseForm()
	postID  := strings.TrimSpace(r.FormValue("post_id"))
	newBody := strings.TrimSpace(r.FormValue("body"))
	if postID == "" || newBody == "" { http.Error(w, "missing fields", http.StatusBadRequest); return }
	if len([]rune(newBody)) > 25000    { http.Error(w, "too long", http.StatusBadRequest); return }

	user := h.userFromRequest(w, r)
	if h.db == nil { w.WriteHeader(http.StatusOK); return }

	// Verify ownership and 60-minute edit window
	var createdAt time.Time
	var authorID  string
	err := h.db.QueryRow(
		"SELECT author_id, created_at FROM works WHERE id = $1", postID,
	).Scan(&authorID, &createdAt)
	if err != nil { http.Error(w, "post not found", http.StatusNotFound); return }
	if authorID != user.ID { http.Error(w, "forbidden", http.StatusForbidden); return }
	if time.Since(createdAt) > 60*time.Minute {
		http.Error(w, "edit window has closed (60 minutes)", http.StatusForbidden)
		return
	}

	_, err = h.db.Exec(
		"UPDATE works SET body = $1, is_edited = true, edited_at = NOW() WHERE id = $2 AND author_id = $3",
		newBody, postID, user.ID,
	)
	if err != nil { http.Error(w, "server error", http.StatusInternalServerError); return }

	w.Header().Set("HX-Trigger", `{"postEdited":"true"}`)
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) deletePost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		http.Error(w, "405", 405)
		return
	}
	_ = r.ParseForm()
	postID := strings.TrimSpace(r.FormValue("post_id"))
	if postID == "" {
		http.Error(w, "missing post_id", http.StatusBadRequest)
		return
	}
	user := h.userFromRequest(w, r)
	if h.db != nil {
		deleted, err := dbpkg.DeletePost(h.db, postID, user.ID)
		if err != nil {
			log.Printf("[delete] error: %v", err)
			http.Error(w, "server error", 500)
			return
		}
		if !deleted {
			http.Error(w, "not found or forbidden", 403)
			return
		}
	}
	// HTMX: empty response removes the target element; also trigger feed refresh
	w.Header().Set("HX-Trigger", `{"postDeleted":"true"}`)
	w.WriteHeader(http.StatusOK)
}

// ── Poll creation ─────────────────────────────────────────────────────────────
func (h *Handler) createPoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "405", 405); return }
	user := h.userFromRequest(w, r)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		_ = r.ParseForm()
	}

	question := strings.TrimSpace(r.FormValue("question"))
	if question == "" { http.Error(w, "question required", 400); return }

	// Options: form sends option[] repeated
	options := r.Form["option"]
	var clean []string
	for _, o := range options {
		o = strings.TrimSpace(o)
		if o != "" { clean = append(clean, o) }
	}
	if len(clean) < 2 { http.Error(w, "at least 2 options required", 400); return }
	if len(clean) > 4 { clean = clean[:4] }

	// Duration: hours until poll ends (default 24h)
	var endsAt *time.Time
	if dur := r.FormValue("duration_hours"); dur != "" {
		if h, err := strconv.Atoi(dur); err == nil && h > 0 {
			t := time.Now().Add(time.Duration(h) * time.Hour)
			endsAt = &t
		}
	}

	id, err := dbpkg.InsertPoll(h.db, user.ID, question, clean, endsAt)
	if err != nil {
		log.Printf("createPoll: %v", err)
		http.Error(w, "failed", 500)
		return
	}
	w.Header().Set("HX-Trigger", `{"postCreated":"true"}`)
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(201)
	fmt.Fprint(w, id)
}

// ── Poll voting ───────────────────────────────────────────────────────────────
func (h *Handler) castPollVote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "405", 405); return }
	user := h.userFromRequest(w, r)
	_ = r.ParseForm()

	postID    := r.FormValue("post_id")
	optionStr := r.FormValue("option_idx")
	if postID == "" || optionStr == "" { http.Error(w, "missing params", 400); return }

	optionIdx, err := strconv.Atoi(optionStr)
	if err != nil || optionIdx < 0 { http.Error(w, "invalid option", 400); return }

	already, err := dbpkg.CastPollVote(h.db, postID, user.ID, optionIdx)
	if err != nil {
		http.Error(w, "vote failed", 500)
		return
	}
	if already {
		http.Error(w, "already voted", 409)
		return
	}

	// Render the updated poll card fragment — replaces #poll-{postID} via outerHTML swap.
	poll, err := dbpkg.GetPollResults(h.db, postID, user.ID)
	if err != nil || poll == nil {
		w.WriteHeader(200)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "poll_card", map[string]interface{}{
		"PostID": postID,
		"Poll":   poll,
	})
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
		"event":      "post.created",
		"post_id":    postID,
		"pial_id":    pialID,
		"body":       body,
		"has_media":  hasMedia,
		"has_poll":   hasPoll,
		"scan_state": "clean",
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
