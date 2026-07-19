package handler

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)


// worksPage — GET /works
func (h *Handler) worksPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	h.render(w, "works.html", map[string]interface{}{
		"User": user,
	})
}

// facetWorksFeed — GET /facets/works/feed
// Surfaces: following, trending, nsfw, for_you/feed/field/focus, topics,
//           profile_posts, profile_replies, profile_media, profile_saves, profile_articles
func (h *Handler) facetWorksFeed(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	surface := r.URL.Query().Get("surface")
	if surface == "" {
		surface = "following"
	}
	before := r.URL.Query().Get("before")
	after := r.URL.Query().Get("after")
	const limit = 20

	type emptyState struct{ Title, Sub string }
	emptyStates := map[string]emptyState{
		"following":       {"Nothing yet", "Follow people to fill your feed"},
		"trending":        {"No trending works", "Check back soon"},
		"nsfw":            {"No NSFW works yet", "Adult content posted via the new pipeline will appear here"},
		"for_you":         {"Nothing here yet", "Post something to get started"},
		"profile_posts":   {"No posts yet", "This creator hasn't posted yet"},
		"profile_replies": {"No replies yet", ""},
		"profile_media":   {"No media yet", ""},
		"profile_saves":   {"No saves yet", "Like or bookmark works to save them"},
		"profile_reposts": {"No reposts yet", ""},
		"profile_articles": {"No articles yet", ""},
		"topics":          {"No results", "Try a different topic"},
	}

	// profile_articles: articles table — render via article_feed Facet, not work_card.
	if surface == "profile_articles" {
		handle := strings.TrimSpace(r.URL.Query().Get("handle"))
		targetID := ""
		if handle != "" {
			if tu, err := dbpkg.GetUserByHandle(h.db, handle); err == nil && tu != nil {
				targetID = tu.ID
			}
		}
		if targetID == "" {
			targetID = user.ID
		}
		sessionID := r.URL.Query().Get("session_id")
		const articleLimit = 20
		articles, aErr := dbpkg.GetUserArticles(h.db, targetID, articleLimit+1, after)
		if aErr != nil {
			log.Printf("[works] user articles for %s: %v", targetID, aErr)
		}
		var nextAfter string
		if len(articles) > articleLimit {
			articles = articles[:articleLimit]
			nextAfter = articles[len(articles)-1].ID
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if len(articles) == 0 {
			h.renderPartial(w, "stateEmpty", map[string]interface{}{
				"Title": "No articles yet",
				"Sub":   "",
			})
			return
		}
		h.renderPartial(w, "article_feed", map[string]interface{}{
			"Articles":  articles,
			"After":     nextAfter,
			"Surface":   surface,
			"SessionID": sessionID,
		})
		return
	}

	// nsfw: hard-gated — only adult-enabled users.
	if surface == "nsfw" && hideAdultCreators(user) {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}

	// Guard against non-UUID user IDs (dev guest mode falls back to non-UUID strings).
	if _, err := uuid.Parse(user.ID); err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		h.renderPartial(w, "stateEmpty", map[string]interface{}{
			"Title": "Sign in to view your feed",
			"Sub":   "",
		})
		return
	}

	// pinned_id: render a single pinned post (used by profile page pinned-post-section).
	pinnedID := r.URL.Query().Get("pinned_id")
	if pinnedID != "" && surface == "profile_posts" {
		work, werr := dbpkg.GetWorkByID(h.db, pinnedID, user.ID)
		if werr == nil && work != nil {
			if len(filterAdultForViewer(user, []*model.Work{work})) == 0 {
				return
			}
			work.IsPinned = true
			dbpkg.EnrichWorksWithReactions(h.db, []*model.Work{work}, user.ID)
			dbpkg.EnrichWorksWithQuotes(h.db, []*model.Work{work})
			dbpkg.EnrichWorksWithLinkPreviews(h.db, []*model.Work{work})
			dbpkg.EnrichWorksWithRepliers(h.db, []*model.Work{work})
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			ctx := map[string]interface{}{"CurrentUserHandle": user.Handle, "User": user}
			h.renderPartial(w, "work_card", map[string]interface{}{"W": work, "Ctx": ctx})
		}
		return
	}

	var works []*model.Work
	var err error

	switch surface {
	case "following":
		works, err = dbpkg.GetWorksFeedFollowing(h.db, user.ID, limit, before)

	case "trending":
		works, err = dbpkg.GetWorksTrending(h.db, limit, before)

	case "nsfw":
		works, err = dbpkg.GetWorksNSFW(h.db, limit, before)

	case "for_you", "feed", "field", "focus":
		works, err = dbpkg.GetWorksForYou(h.db, limit, before)

	case "topics":
		topicsParam := r.URL.Query().Get("topics")
		if topicsParam == "" {
			works, err = dbpkg.GetWorksForYou(h.db, limit, before)
		} else {
			seen := map[string]bool{}
			for _, tag := range strings.SplitN(topicsParam, ",", 8) {
				tag = strings.TrimSpace(strings.ToLower(tag))
				if tag == "" {
					continue
				}
				res, qerr := dbpkg.SearchWorks(h.db, tag, limit/4, user.ID)
				if qerr == nil {
					for _, wk := range res {
						if !seen[wk.ID] {
							seen[wk.ID] = true
							works = append(works, wk)
						}
					}
				}
			}
		}

	case "profile_posts", "profile_replies", "profile_media":
		handle := r.URL.Query().Get("handle")
		if handle == "" {
			http.Error(w, "handle required", http.StatusBadRequest)
			return
		}
		profileUser, dbErr := dbpkg.GetUserByHandle(h.db, handle)
		if dbErr != nil || profileUser == nil {
			http.Error(w, "user not found", http.StatusNotFound)
			return
		}
		works, err = dbpkg.GetWorksForProfile(h.db, profileUser.PIALID, limit, before)
		// Exclude the pinned post from the main feed to avoid duplication.
		if surface == "profile_posts" {
			if excludeID := r.URL.Query().Get("exclude_id"); excludeID != "" {
				filtered := works[:0]
				for _, wk := range works {
					if wk.ID != excludeID {
						filtered = append(filtered, wk)
					}
				}
				works = filtered
			}
		}

	case "profile_reposts":
		handle := r.URL.Query().Get("handle")
		if handle == "" {
			http.Error(w, "handle required", http.StatusBadRequest)
			return
		}
		profileUser, dbErr := dbpkg.GetUserByHandle(h.db, handle)
		if dbErr != nil || profileUser == nil {
			http.Error(w, "user not found", http.StatusNotFound)
			return
		}
		works, err = dbpkg.GetWorksRepostedByUser(h.db, profileUser.ID, limit, before)

	case "profile_saves":
		handle := r.URL.Query().Get("handle")
		viewerID := user.ID
		if handle != "" {
			if profileUser, dbErr := dbpkg.GetUserByHandle(h.db, handle); dbErr == nil && profileUser != nil {
				viewerID = profileUser.ID
			}
		}
		works, err = dbpkg.GetWorksSaves(h.db, viewerID, limit, before)

	default:
		works, err = dbpkg.GetWorksFeedFollowing(h.db, user.ID, limit, before)
	}

	if err != nil {
		log.Printf("[works-feed] %v", err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	// Strip adult content for minor/safe/gov/biz viewers so it never appears in any feed
	// (prevents adult creators leaking into Trending → 403 on click).
	works = filterAdultForViewer(user, works)
	dbpkg.EnrichWorksWithReactions(h.db, works, user.ID)
	dbpkg.EnrichWorksWithQuotes(h.db, works)
	dbpkg.EnrichWorksWithLinkPreviews(h.db, works)
	dbpkg.EnrichWorksWithRepliers(h.db, works)

	// Mark the viewer's pinned work so the menu shows "Unpin from profile" correctly.
	viewerPinnedID := dbpkg.GetPinnedPostID(h.db, user.ID)
	dbpkg.EnrichWorksWithPinnedState(works, viewerPinnedID)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if len(works) == 0 {
		es := emptyStates[surface]
		if es.Title == "" {
			es = emptyState{"No works yet", "Works from people you follow will appear here"}
		}
		h.renderPartial(w, "stateEmpty", map[string]interface{}{
			"Title": es.Title,
			"Sub":   es.Sub,
		})
		return
	}
	ctx := map[string]interface{}{"CurrentUserHandle": user.Handle, "User": user}

	for _, wk := range works {
		if wk.Kind == "vision" {
			h.renderPartial(w, "vision_card", wk)
		} else {
			h.renderPartial(w, "work_card", map[string]interface{}{"W": wk, "Ctx": ctx})
		}
	}
	last := works[len(works)-1]
	sentinelURL := fmt.Sprintf("/facets/works/feed?surface=%s&before=%s", surface, last.CreatedAt.Format(time.RFC3339))
	if handle := r.URL.Query().Get("handle"); handle != "" {
		sentinelURL += "&handle=" + handle
	}
	if topics := r.URL.Query().Get("topics"); topics != "" {
		sentinelURL += "&topics=" + topics
	}
	h.renderPartial(w, "feed_sentinel", map[string]interface{}{"URL": sentinelURL})
}

// facetWorkReplies — GET /facets/works/replies/{id}
// Returns the reply stream fragment for #work-replies.
// Used by HTMX to refresh after a new reply is posted.
func (h *Handler) facetWorkReplies(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	stream := h.buildReplyStream(id, user.ID)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	ctx := map[string]interface{}{"CurrentUserHandle": user.Handle, "User": user}
	if len(stream) == 0 {
		h.renderPartial(w, "stateEmpty", map[string]interface{}{
			"Title": "No replies yet",
			"Sub":   "Be the first to reply",
		})
		return
	}
	for _, item := range stream {
		if item.IsMicroconversation {
			h.renderPartial(w, "microconversation", map[string]interface{}{"MC": item.Microconversation, "Ctx": ctx})
		} else {
			h.renderPartial(w, "work_card", map[string]interface{}{"W": item.Work, "Ctx": ctx})
		}
	}
}

// mcPalette is the ordered accent palette. Colors are assigned sequentially per page render
// so every microconversation on screen gets a distinct color.
var mcPalette = []string{
	"mc-cobalt", "mc-violet", "mc-rose", "mc-amber",
	"mc-emerald", "mc-sky", "mc-fuchsia", "mc-teal",
}

func validAccentClass(s string) bool {
	for _, c := range mcPalette {
		if c == s {
			return true
		}
	}
	return false
}

// facetWorkConversation — GET /facets/works/conversation/{id}?accent=mc-*
// Expands a single microconversation — swaps the collapsed container for the full exchange.
// The accent class is passed as a query param so the expanded container keeps its original color.
func (h *Handler) facetWorkConversation(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	seedID := r.PathValue("id")
	if seedID == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	accentClass := r.URL.Query().Get("accent")
	if !validAccentClass(accentClass) {
		accentClass = mcPalette[0]
	}
	seedWork, err := dbpkg.GetWorkByID(h.db, seedID, user.ID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	exchanges, exErr := dbpkg.GetWorkReplies(h.db, seedID, 50)
	if exErr != nil {
		log.Printf("[works] replies for %s: %v", seedID, exErr)
	}
	all := append([]*model.Work{seedWork}, exchanges...)
	dbpkg.EnrichWorksWithReactions(h.db, all, user.ID)
	dbpkg.EnrichWorksWithQuotes(h.db, all)

	seen := map[string]bool{}
	var participants []model.MicroconversationParticipant
	addP := func(handle, avatar string) {
		if handle != "" && !seen[handle] {
			seen[handle] = true
			participants = append(participants, model.MicroconversationParticipant{Handle: handle, AvatarURL: avatar})
		}
	}
	addP(seedWork.AuthorHandle, seedWork.AvatarURL)
	for _, ex := range exchanges {
		addP(ex.AuthorHandle, ex.AvatarURL)
	}

	mc := &model.Microconversation{
		ConversationID: seedID,
		AccentClass:    accentClass,
		SeedWork:       seedWork,
		Participants:   participants,
		ReplyCount:     len(exchanges),
		MoreCount:      0,
		Exchanges:      exchanges,
		IsExpanded:     true,
	}
	ctx := map[string]interface{}{"CurrentUserHandle": user.Handle, "User": user}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "microconversation", map[string]interface{}{"MC": mc, "Ctx": ctx})
}

// buildReplyStream returns a merged, time-sorted stream of microconversation containers
// and lone reply cards for a work detail page.
// Colors are assigned sequentially so every container on screen is a distinct color.
func (h *Handler) buildReplyStream(workID, userID string) []model.ReplyStreamItem {
	mcs, mcErr := dbpkg.GetMicroconversations(h.db, workID, 10)
	if mcErr != nil {
		log.Printf("[works] microconversations for %s: %v", workID, mcErr)
	}
	for i, mc := range mcs {
		mc.AccentClass = mcPalette[i%len(mcPalette)]
	}
	loneReplies, lrErr := dbpkg.GetWorkLoneReplies(h.db, workID, 20)
	if lrErr != nil {
		log.Printf("[works] lone replies for %s: %v", workID, lrErr)
	}

	if len(loneReplies) > 0 {
		dbpkg.EnrichWorksWithReactions(h.db, loneReplies, userID)
		dbpkg.EnrichWorksWithQuotes(h.db, loneReplies)
		dbpkg.EnrichWorksWithLinkPreviews(h.db, loneReplies)
	}
	for _, mc := range mcs {
		all := append([]*model.Work{mc.SeedWork}, mc.Exchanges...)
		dbpkg.EnrichWorksWithReactions(h.db, all, userID)
		dbpkg.EnrichWorksWithQuotes(h.db, all)
	}

	var stream []model.ReplyStreamItem
	for _, mc := range mcs {
		stream = append(stream, model.ReplyStreamItem{
			IsMicroconversation: true,
			Microconversation:   mc,
			SortTime:            mc.SeedWork.CreatedAt,
		})
	}
	for _, wk := range loneReplies {
		stream = append(stream, model.ReplyStreamItem{
			IsMicroconversation: false,
			Work:                wk,
			SortTime:            wk.CreatedAt,
		})
	}
	sort.Slice(stream, func(i, j int) bool {
		return stream[i].SortTime.After(stream[j].SortTime)
	})
	return stream
}

// workDetailPage — GET /work/{id}
// Renders work detail with parent chain and microconversation reply stream.
// workWall is the single source of truth for whether a work's body/media must be
// hidden from a viewer. Returns (privateWall, workGate); walled == privateWall ||
// workGate != "". Account-private works are hidden from non-owner/non-follower viewers;
// adult works are gated for anonymous ("auth") and minor/safe_mode/gov/business
// ("restricted") viewers. Authed 18+ viewers fall through (per-item content_gate handles
// them). author/viewer may be nil; viewer may be the anonymous DemoUser sentinel.
func (h *Handler) workWall(work *model.Work, author, viewer *model.User) (privateWall bool, workGate string) {
	if work == nil {
		return false, ""
	}
	anon := viewer == nil || viewer.ID == "demo_user"
	isOwner := !anon && author != nil && author.ID == viewer.ID
	isFollowing := false
	if h.db != nil && !anon && !isOwner && author != nil {
		isFollowing, _ = dbpkg.IsFollowing(h.db, viewer.ID, author.ID)
	}
	privateWall = author != nil && author.IsPrivate && !isOwner && !isFollowing
	if !privateWall && (work.IsNSFW || work.IsGore || work.AuthorIsAdultCreator) && !isOwner {
		if anon {
			workGate = "auth"
		} else if excludeNSFW(viewer) {
			workGate = "restricted"
		}
	}
	return privateWall, workGate
}

func (h *Handler) workDetailPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	// Anonymous (logged-out) visitor — the page is public (like X.com). The work header
	// renders; the body/media are walled per privacy / adult rules below. No 401/login bounce.
	anon := user == nil || user.ID == "demo_user"

	id := r.PathValue("id")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	if h.db == nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}
	// Per-viewer reaction flags need a real account UUID; never pass the demo_user sentinel.
	viewerID := ""
	if !anon {
		viewerID = user.ID
	}
	work, err := dbpkg.GetWorkByID(h.db, id, viewerID)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Canonical URL is /{handle}/work/{id}. Redirect the bare /work/{id} form (and any
	// stale handle) to the canonical path so shared links and crawlers see one URL.
	urlHandle := r.PathValue("handle")
	if urlHandle != work.AuthorHandle {
		http.Redirect(w, r, "/"+work.AuthorHandle+"/work/"+id, http.StatusMovedPermanently)
		return
	}

	// Resolve the author for account-level privacy / adult-creator status.
	var author *model.User
	if h.db != nil {
		author, _ = dbpkg.GetUserByHandle(h.db, work.AuthorHandle)
	}
	isOwner := !anon && author != nil && author.ID == user.ID

	// ── Walls (body/media replaced; the work header still renders) ────────────────
	// Single source of truth shared with facetShareSheet so the two paths can't drift.
	privateWall, workGate := h.workWall(work, author, user)
	walled := privateWall || workGate != ""

	// Open Graph image — first media thumbnail ONLY for a fully-public SFW work; adult,
	// private or walled works fall back to the author avatar / platform default so no
	// NSFW media ever leaks into a link preview.
	ogImage := "/static/brand/og-image.png"
	if author != nil && author.AvatarURL != "" {
		ogImage = author.AvatarURL
	}
	if !walled && !work.IsNSFW && !work.IsGore && !work.AuthorIsAdultCreator {
		if work.VideoPosterURL != "" {
			ogImage = work.VideoPosterURL
		} else if len(work.MediaURLs) > 0 {
			ogImage = work.MediaURLs[0]
		}
	}
	if strings.HasPrefix(ogImage, "/") {
		ogImage = "https://f33d3r.com" + ogImage
	}
	ogDesc := truncate(work.Body, 180)
	if walled || ogDesc == "" {
		ogDesc = "@" + work.AuthorHandle + " on f33d3r"
	}
	// The <title> must not leak a walled work's body (visible in the tab, history, and
	// to crawlers). Use the same wall check as ogDesc.
	pageTitle := "@" + work.AuthorHandle + " on f33d3r"
	if !walled && strings.TrimSpace(work.Body) != "" {
		pageTitle = work.AuthorHandle + ": " + truncate(work.Body, 60)
	}

	data := map[string]interface{}{
		"User":              user,
		"Work":              work,
		"Title":             pageTitle,
		"CurrentUserHandle": user.Handle,
		"IsAnon":            anon,
		"IsOwner":           isOwner,
		"PrivateWall":       privateWall,
		"WorkGate":          workGate,
		"OGTitle":           "@" + work.AuthorHandle + " on f33d3r",
		"OGDesc":            ogDesc,
		"OGImage":           ogImage,
		"OGUrl":             "https://f33d3r.com/" + work.AuthorHandle + "/work/" + id,
	}

	// Only enrich and load the conversation when the work is actually shown — a walled
	// work must not stream its media, quotes, parents or replies to the public.
	if !walled {
		dbpkg.EnrichWorksWithQuotes(h.db, []*model.Work{work})
		dbpkg.EnrichWorksWithLinkPreviews(h.db, []*model.Work{work})
		parentChain, pcErr := dbpkg.GetWorkParentChain(h.db, id, viewerID, 3)
		if pcErr != nil {
			log.Printf("[works] parent chain for %s: %v", id, pcErr)
		}
		data["ParentChain"] = parentChain
		data["ReplyStream"] = h.buildReplyStream(id, viewerID)
	}

	rail := h.railData(user, "default")
	for k, v := range rail {
		data[k] = v
	}
	h.render(w, "works_detail.html", data)
}

// workReactionEvent handles work_like, work_unlike, work_repost, work_unrepost, work_bookmark, work_unbookmark.
func (h *Handler) workReactionEvent(w http.ResponseWriter, r *http.Request, eventType string, rawBody map[string]json.RawMessage) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if h.db == nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}

	// Extract work_id from rawBody or form value.
	workID := r.FormValue("work_id")
	if workID == "" && rawBody != nil {
		json.Unmarshal(rawBody["work_id"], &workID)
	}
	if workID == "" {
		http.Error(w, "work_id required", http.StatusBadRequest)
		return
	}

	// Map event_type to reaction_type and action (add/remove).
	type reactionOp struct {
		reactionType string
		add          bool
	}
	ops := map[string]reactionOp{
		"work_like":       {"like", true},
		"work_unlike":     {"like", false},
		"work_repost":     {"repost", true},
		"work_unrepost":   {"repost", false},
		"work_bookmark":   {"bookmark", true},
		"work_unbookmark": {"bookmark", false},
		"work_dislike":    {"dislike", true},
		"work_undislike":  {"dislike", false},
	}
	op, ok := ops[eventType]
	if !ok {
		http.Error(w, "unknown reaction event", http.StatusBadRequest)
		return
	}

	if op.add {
		h.db.Exec(
			`INSERT INTO work_reactions (work_id, reactor_id, reaction_type)
             VALUES ($1::uuid, $2::uuid, $3)
             ON CONFLICT DO NOTHING`,
			workID, user.ID, op.reactionType,
		)
	} else {
		h.db.Exec(
			`DELETE FROM work_reactions
             WHERE work_id = $1::uuid AND reactor_id = $2::uuid AND reaction_type = $3`,
			workID, user.ID, op.reactionType,
		)
	}

	// Troll achievement checks on engagement-count milestones (like/dislike/repost)
	if op.reactionType == "like" || op.reactionType == "dislike" || op.reactionType == "repost" {
		wID := workID
		go func() {
			var authorID, authorPIAL string
			h.db.QueryRow(`SELECT author_id, author_pial_id FROM works WHERE id = $1::uuid`, wID).Scan(&authorID, &authorPIAL)
			if authorID != "" {
				dbpkg.TryAwardTrollOnPostStats(h.db, authorID, authorPIAL, wID)
			}
		}()
	}

	w.WriteHeader(http.StatusNoContent)
}

// legacyPostRedirect handles GET /post/{id}.
// Looks up the corresponding work by legacy_post_id first; falls back to treating
// the post ID directly as a work ID for rows migrated with the same UUID.
func (h *Handler) legacyPostRedirect(w http.ResponseWriter, r *http.Request) {
	postID := r.PathValue("id")
	if h.db != nil {
		var workID string
		err := h.db.QueryRow(
			`SELECT id::text FROM works WHERE legacy_post_id = $1::uuid AND deleted_at IS NULL LIMIT 1`,
			postID,
		).Scan(&workID)
		if err == nil && workID != "" {
			http.Redirect(w, r, "/work/"+workID, http.StatusMovedPermanently)
			return
		}
	}
	http.Redirect(w, r, "/work/"+postID, http.StatusMovedPermanently)
}

// workDeleteEvent handles POST /events {event_type:"work_delete","work_id":"<uuid>"}.
// It soft-deletes the work (sets deleted_at = NOW()) after verifying ownership, then:
//   - Returns an empty body with HTTP 200 so HTMX outerHTML swap removes the article element.
//   - Broadcasts a post_deleted SSE event to all sessions so every viewer's DOM is updated.
func (h *Handler) workDeleteEvent(w http.ResponseWriter, r *http.Request, rawBody map[string]json.RawMessage) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if h.db == nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}

	// Extract work_id from JSON body or form value.
	workID := r.FormValue("work_id")
	if workID == "" && rawBody != nil {
		json.Unmarshal(rawBody["work_id"], &workID)
	}
	if workID == "" {
		http.Error(w, "work_id required", http.StatusBadRequest)
		return
	}

	if err := dbpkg.SoftDeleteWork(h.db, workID, user.ID); err != nil {
		if err == sql.ErrNoRows {
			// Work not found, already deleted, or caller is not the owner.
			htmxError(w, r, "Post not found or you do not own it", http.StatusForbidden)
			return
		}
		log.Printf("[work_delete] SoftDeleteWork work=%s user=%s: %v", workID, user.ID, err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}

	// Broadcast post_deleted SSE to all active sessions so every viewer's DOM removes the card.
	// Data carries the work UUID so the client can locate the article by data-facet-id.
	go PublishToAllSessions(SSEEvent{
		Type: "post_deleted",
		Data: workID,
	})

	// Return empty body — HTMX hx-swap="outerHTML" on the delete button replaces the
	// closest article with nothing, removing it from the DOM immediately for the author.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

// editWork handles POST /api/work/edit.
// Allows the author to update a work's body within 60 minutes of posting.
// Each save appends an immutable edition snapshot for chain-of-custody.
func (h *Handler) editWork(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", http.StatusMethodNotAllowed)
		return
	}
	user := h.userFromRequest(w, r)
	_ = r.ParseForm()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	workID := r.FormValue("work_id")
	newBody := strings.TrimSpace(r.FormValue("body"))
	if workID == "" || newBody == "" {
		http.Error(w, "work_id and body required", http.StatusBadRequest)
		return
	}
	if h.db == nil {
		http.Error(w, "db unavailable", http.StatusInternalServerError)
		return
	}

	// Verify ownership and check the 60-minute edit window.
	var authorID string
	var createdAt time.Time
	err := h.db.QueryRow(
		`SELECT author_id, created_at FROM works WHERE id = $1 AND deleted_at IS NULL`, workID,
	).Scan(&authorID, &createdAt)
	if err != nil {
		http.Error(w, "work not found", http.StatusNotFound)
		return
	}
	if authorID != user.ID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if time.Since(createdAt) > 60*time.Minute {
		// Belt-and-suspenders: the UI hides the edit button after 60 min.
		http.Error(w, "edit window closed", http.StatusForbidden)
		return
	}

	// Derive next edition number.
	var editionCount int
	_ = h.db.QueryRow(`SELECT COUNT(*) FROM editions WHERE work_id = $1::uuid`, workID).Scan(&editionCount)
	nextEdition := editionCount + 1

	// Content-addressed CID for this edition: SHA-256(workID + "|" + edition + "|" + body).
	raw := workID + "|" + fmt.Sprintf("%d", nextEdition) + "|" + newBody
	sum := sha256.Sum256([]byte(raw))
	editionCID := "sha256:" + hex.EncodeToString(sum[:])

	// Append the edition snapshot (immutable chain-of-custody record).
	if err := dbpkg.InsertEdition(h.db, workID, editionCID, newBody, nextEdition); err != nil {
		log.Printf("[editWork] InsertEdition work=%s: %v", workID, err)
	}

	// Update the live work body.
	_, err = h.db.Exec(
		`UPDATE works SET body = $1, is_edited = TRUE, edited_at = NOW() WHERE id = $2 AND author_id = $3`,
		newBody, workID, user.ID,
	)
	if err != nil {
		log.Printf("[editWork] update work=%s: %v", workID, err)
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}

	// Return the updated body text for inline HTMX swap.
	_ = uuid.New() // keep uuid import used
	fmt.Fprintf(w, `<div class="work-body-text">%s</div>`, template.HTMLEscapeString(newBody))
}

// facetWorkEditions — GET /facets/work/{id}/editions
// Returns the chain-of-custody edition history panel for a work.
// Rendered when the user clicks the "Edited · time" label on a work card.
func (h *Handler) facetWorkEditions(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	editions, err := dbpkg.GetWorkEditions(h.db, id)
	if err != nil || len(editions) == 0 {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<div class="work-editions-panel"><div class="editions-header">No edition history found</div></div>`)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "work_editions_panel", map[string]interface{}{
		"Editions": editions,
		"WorkID":   id,
	})
}
