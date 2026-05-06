package handler

import (
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
	"github.com/f33d3r/feed-engine/internal/realm"
)

var mentionExtractRe = regexp.MustCompile(`@([A-Za-z0-9_]{1,50})`)

// fireMentionNotifications scans body for @handles and sends mention notifications.
// Runs as a goroutine — never blocks the HTTP response.
func (h *Handler) fireMentionNotifications(body, postID, actorID string) {
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
	for _, info := range resolved {
		uid := info[0]
		_ = dbpkg.CreateNotification(h.db, uid, "mention", actorID, postID, "post")
		dbpkg.IncrementUnreadCount(h.db, uid)
	}
}

func (h *Handler) postDetailPage(w http.ResponseWriter, r *http.Request) {
	user   := h.userFromRequest(w, r)
	postID := r.PathValue("id")

	var post        *model.Post
	var replies     []*model.Post
	var threadChain []*model.Post

	if h.db != nil {
		post, _ = dbpkg.GetPostByID(h.db, postID)
		if post != nil {
			post.TimeAgo = TimeAgo(post.CreatedAt)
			post.LikedByUser, _      = dbpkg.IsLiked(h.db, user.ID, post.ID)
			post.BookmarkedByUser, _ = dbpkg.IsBookmarked(h.db, user.ID, post.ID)

			// Fetch thread continuations when this is a thread root.
			if post.ThreadCount > 0 {
				chain, err := dbpkg.GetThreadChain(h.db, postID, user.ID)
				if err == nil && len(chain) > 1 {
					// Skip index 0 (root) — it's already rendered as the detail post.
					for _, p := range chain[1:] {
						p.TimeAgo = TimeAgo(p.CreatedAt)
					}
					threadChain = chain[1:]
				}
			}

			replies, _ = dbpkg.GetReplies(h.db, postID, 50)
			for _, rp := range replies {
				rp.TimeAgo = TimeAgo(rp.CreatedAt)
				rp.LikedByUser, _      = dbpkg.IsLiked(h.db, user.ID, rp.ID)
				rp.BookmarkedByUser, _ = dbpkg.IsBookmarked(h.db, user.ID, rp.ID)
			}
		}
	}
	if post == nil {
		http.NotFound(w, r)
		return
	}

	h.render(w, "post.html", map[string]interface{}{
		"User":              user,
		"Post":              post,
		"ThreadChain":       threadChain,
		"Replies":           replies,
		"Title":             post.AuthorName + ": " + truncate(post.Body, 60),
		"SessionID":         uuid.New().String(),
		"ShowScores":        h.cfg.ShowScores,
		"Themes":            ThemesWithActive(user.ThemeID),
		"CurrentUserHandle": user.Handle,
	})
}

func (h *Handler) searchPage(w http.ResponseWriter, r *http.Request) {
	user  := h.userFromRequest(w, r)
	query := strings.TrimSpace(r.URL.Query().Get("q"))

	var posts   []*model.Post
	var people  []dbpkg.MentionResult
	if query != "" && h.db != nil {
		// Strip leading @ or # so both work naturally
		searchTerm := strings.TrimPrefix(strings.TrimPrefix(query, "@"), "#")
		posts, _ = dbpkg.SearchPosts(h.db, searchTerm, 30)
		for _, p := range posts { p.TimeAgo = TimeAgo(p.CreatedAt) }
		people, _ = dbpkg.SearchHandles(h.db, searchTerm, 10)
	}

	h.render(w, "search.html", map[string]interface{}{
		"User":          user,
		"Surface":       "search",
		"SessionID":     uuid.New().String(),
		"Title":         "Search · F33D3R",
		"ShowScores":    h.cfg.ShowScores,
		"Themes":        ThemesWithActive(user.ThemeID),
		"SearchQuery":   query,
		"SearchResults": posts,
		"PeopleResults": people,
	})
}

func (h *Handler) createPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(50 << 20); err != nil {
		_ = r.ParseForm()
	}
	body := strings.TrimSpace(r.FormValue("content"))
	if body == "" || len([]rune(body)) > 25000 {
		htmxError(w, r, "Post body is empty or too long (max 25,000 characters)", http.StatusBadRequest)
		return
	}
	user := h.userFromRequest(w, r)
	if h.db != nil && !dbpkg.HasCapability(h.db, user.PIALID, model.CapPosting) {
		htmxError(w, r, "Posting is restricted on this account", http.StatusForbidden)
		return
	}
	mediaURLs := r.Form["media_urls"]
	if mediaURLs == nil {
		mediaURLs = []string{}
	}
	// Guard: stale/buggy JS can submit "undefined" or empty strings.
	{
		clean := mediaURLs[:0]
		for _, u := range mediaURLs {
			if u != "" && u != "undefined" && u != "null" {
				clean = append(clean, u)
			}
		}
		mediaURLs = clean
	}
	// Sprint 0 / S0.6: video assets come back from /upload/post as a master URL.
	// Compose form sets these hidden fields when caeor transcoded a video.
	videoMasterURL := strings.TrimSpace(r.FormValue("video_master_url"))
	videoPosterURL := strings.TrimSpace(r.FormValue("video_poster_url"))
	videoDuration := float32(0)
	if v := r.FormValue("video_duration_secs"); v != "" {
		if f, err := strconv.ParseFloat(v, 32); err == nil {
			videoDuration = float32(f)
		}
	}
	videoWidth, _ := strconv.Atoi(r.FormValue("video_width"))
	videoHeight, _ := strconv.Atoi(r.FormValue("video_height"))
	commentGating := r.FormValue("comment_gating")
	switch commentGating {
	case "open", "followers", "verified", "none":
	default:
		commentGating = "open"
	}
	isNSFW := r.FormValue("is_nsfw") == "1" || r.FormValue("is_nsfw") == "true"

	if h.db != nil {
		contentType := "text"
		if videoMasterURL != "" {
			contentType = "video"
		} else if len(mediaURLs) > 0 {
			contentType = "image"
		}
		postID, err := dbpkg.InsertPostWithMedia(h.db, user.ID, body, contentType, []string{}, mediaURLs, commentGating)
		if err != nil {
			log.Printf("[post] insert error: %v", err)
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		// Persist HLS asset alongside the post.
		if videoMasterURL != "" {
			if err := dbpkg.SetPostVideo(h.db, postID, videoMasterURL, videoPosterURL, videoDuration, videoWidth, videoHeight); err != nil {
				log.Printf("[post] SetPostVideo error: %v", err)
			}
		}
		go realm.AwardXP(h.db, user.ID, "post", postID, realm.XPPost)
		go h.fireMentionNotifications(body, postID, user.ID)

		// Compliance: CSAM scan every media item; 2257 record for adult posts
		for _, mu := range mediaURLs {
			go func(url string) {
				result := h.csamScan(url, user.PIALID, "post_media")
				if !result.Clean {
					log.Printf("[csam-ALERT] flagged media in post %s by PIAL %s: %s", postID, user.PIALID, url)
				}
			}(mu)
		}
		if isNSFW && user.PIALID != "" {
			h.create2257Record(user.PIALID, postID)
		}
	}
	w.Header().Set("HX-Trigger", `{"postCreated":"true"}`)
	w.WriteHeader(http.StatusCreated)
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
		"SELECT author_id, created_at FROM posts WHERE id = $1", postID,
	).Scan(&authorID, &createdAt)
	if err != nil { http.Error(w, "post not found", http.StatusNotFound); return }
	if authorID != user.ID { http.Error(w, "forbidden", http.StatusForbidden); return }
	if time.Since(createdAt) > 60*time.Minute {
		http.Error(w, "edit window has closed (60 minutes)", http.StatusForbidden)
		return
	}

	_, err = h.db.Exec(
		"UPDATE posts SET body = $1, is_edited = true, updated_at = NOW() WHERE id = $2 AND author_id = $3",
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
	_ = r.ParseForm()

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

	// Return updated poll results as HTML fragment
	poll, err := dbpkg.GetPollResults(h.db, postID, user.ID)
	if err != nil || poll == nil {
		w.WriteHeader(200)
		return
	}

	// Render poll results bar HTML
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, `<div class="poll-results" data-post-id="%s">`, postID)
	for i, opt := range poll.Options {
		pct := 0.0
		if poll.TotalVotes > 0 { pct = float64(poll.Votes[i]) / float64(poll.TotalVotes) * 100 }
		voted := ""
		if poll.UserVote == i { voted = " poll-voted" }
		fmt.Fprintf(w, `
		<div class="poll-bar%s">
		  <div class="poll-bar-fill" style="width:%.0f%%"></div>
		  <span class="poll-bar-label">%s</span>
		  <span class="poll-bar-pct">%.0f%%</span>
		</div>`, voted, pct, opt, pct)
	}
	fmt.Fprintf(w, `<p class="poll-total">%d vote`, poll.TotalVotes)
	if poll.TotalVotes != 1 { fmt.Fprint(w, "s") }
	fmt.Fprint(w, `</p></div>`)
}

func (h *Handler) replyAPI(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	body     := strings.TrimSpace(r.FormValue("body"))
	parentID := strings.TrimSpace(r.FormValue("parent_id"))
	if body == "" || len([]rune(body)) > 25000 || parentID == "" {
		http.Error(w, "invalid reply", http.StatusBadRequest)
		return
	}
	user := h.userFromRequest(w, r)
	if h.db != nil && user.PIALID != "" && !dbpkg.HasCapability(h.db, user.PIALID, model.CapPosting) {
		http.Error(w, "Posting is restricted on this account", http.StatusForbidden)
		return
	}
	// Enforce comment_gating on parent post
	if h.db != nil {
		parent, _ := dbpkg.GetPostByID(h.db, parentID)
		if parent != nil {
			switch parent.CommentGating {
			case "none":
				http.Error(w, "Replies are disabled for this post", http.StatusForbidden)
				return
			case "followers":
				if parent.AuthorID != user.ID {
					isFollowing, _ := dbpkg.IsFollowing(h.db, user.ID, parent.AuthorID)
					if !isFollowing {
						http.Error(w, "Only followers can reply to this post", http.StatusForbidden)
						return
					}
				}
			case "verified":
				if !user.IsVerified && parent.AuthorID != user.ID {
					http.Error(w, "Only verified accounts can reply to this post", http.StatusForbidden)
					return
				}
			}
		}
	}
	if h.db != nil {
		replyID, err := dbpkg.InsertReply(h.db, user.ID, parentID, body)
		if err != nil {
			log.Printf("[reply] insert error: %v", err)
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		go realm.AwardXP(h.db, user.ID, "reply", replyID, realm.XPReply)
		go h.fireMentionNotifications(body, replyID, user.ID)
	}
	// HTMX request → 200 OK; regular form → redirect to post page
	if r.Header.Get("HX-Request") == "true" {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/post/"+parentID, http.StatusSeeOther)
}

// ── Thread ────────────────────────────────────────────────────────────────────

// createThread accepts a JSON body: {"segments":["text1","text2",...],"comment_gating":"open"}
// It inserts the first segment as a root post, then chains the rest as self-replies.
// Returns JSON {"root_id":"<uuid>"}.
func (h *Handler) createThread(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", 405)
		return
	}
	user := h.userFromRequest(w, r)
	if h.db != nil && !dbpkg.HasCapability(h.db, user.PIALID, model.CapPosting) {
		http.Error(w, "Posting is restricted on this account", http.StatusForbidden)
		return
	}

	var req struct {
		Segments      []string `json:"segments"`
		CommentGating string   `json:"comment_gating"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	var clean []string
	for _, s := range req.Segments {
		s = strings.TrimSpace(s)
		if s != "" && len([]rune(s)) <= 25000 {
			clean = append(clean, s)
		}
	}
	if len(clean) == 0 {
		http.Error(w, "no valid segments", http.StatusBadRequest)
		return
	}
	if len(clean) > 25 {
		clean = clean[:25]
	}
	gating := req.CommentGating
	switch gating {
	case "open", "followers", "verified", "none":
	default:
		gating = "open"
	}

	if h.db == nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}

	// Insert root post
	rootID, err := dbpkg.InsertPostWithMedia(h.db, user.ID, clean[0], "text", []string{}, []string{}, gating)
	if err != nil {
		log.Printf("[thread] root insert error: %v", err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	go realm.AwardXP(h.db, user.ID, "post", rootID, realm.XPPost)
	go h.fireMentionNotifications(clean[0], rootID, user.ID)

	// Chain remaining segments as self-replies
	parentID := rootID
	for _, seg := range clean[1:] {
		segID, err := dbpkg.InsertReply(h.db, user.ID, parentID, seg)
		if err != nil {
			log.Printf("[thread] segment insert error after root %s: %v", rootID, err)
			break
		}
		go h.fireMentionNotifications(seg, segID, user.ID)
		parentID = segID
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("HX-Trigger", `{"postCreated":"true"}`)
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"root_id": rootID})
}

// threadChainAPI returns the thread chain (all self-reply segments) as an HTML partial.
// Called via HTMX when the user expands a "Thread N posts" badge in the feed.
func (h *Handler) threadChainAPI(w http.ResponseWriter, r *http.Request) {
	user   := h.userFromRequest(w, r)
	rootID := r.PathValue("id")
	if rootID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	posts, err := dbpkg.GetThreadChain(h.db, rootID, user.ID)
	if err != nil || len(posts) == 0 {
		http.Error(w, "thread not found", http.StatusNotFound)
		return
	}
	for _, p := range posts {
		p.TimeAgo = TimeAgo(p.CreatedAt)
	}
	w.Header().Set("Content-Type", "text/html")
	if err := h.partial.ExecuteTemplate(w, "thread_chain.html", map[string]interface{}{
		"Posts":             posts,
		"CurrentUserHandle": user.Handle,
		"ShowScores":        h.cfg.ShowScores,
	}); err != nil {
		log.Printf("[threadChainAPI] render error: %v", err)
	}
}
