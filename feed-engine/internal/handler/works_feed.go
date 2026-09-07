package handler

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
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
	h.render(w, r, "works.html", map[string]interface{}{
		"User": user,
	})
}

// facetWorksFeed — GET /facets/works/feed
// Surfaces: following, trending, nsfw, for_you/feed/field/focus, topics,
//
//	profile_posts, profile_replies, profile_media, profile_saves, profile_articles
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
	// The feed_sentinel Facet publishes its cursor as data-next-after so a
	// native Shell can page without parsing the URL; that cursor comes back as
	// after= and names the same page the web's before= does. Only the article
	// wall pages by after= natively (an id, not an instant), and reads it below.
	if before == "" && surface != "profile_articles" {
		before = after
	}
	const limit = 20

	type emptyState struct{ Title, Sub string }
	emptyStates := map[string]emptyState{
		"following":        {"Nothing yet", "Follow people to fill your feed"},
		"trending":         {"No trending works", "Check back soon"},
		"nsfw":             {"No NSFW works yet", "Adult content posted via the new pipeline will appear here"},
		"for_you":          {"Nothing here yet", "Post something to get started"},
		"profile_posts":    {"No posts yet", "This creator hasn't posted yet"},
		"profile_replies":  {"No replies yet", ""},
		"profile_media":    {"No media yet", ""},
		"profile_saves":    {"No saves yet", "Like or bookmark works to save them"},
		"profile_reposts":  {"No reposts yet", ""},
		"profile_articles": {"No articles yet", ""},
		"topics":           {"No results", "Try a different topic"},
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
			// This section asserts pinned state for the profile owner's pinned
			// work, which is not necessarily the viewer's own pinned work, so
			// the mark is set here rather than derived from the viewer.
			work.IsPinned = true
			cards := h.renderWorkCards([]*model.Work{work}, user, h.workCardCtx(user, surface))
			if len(cards) == 0 {
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			for _, frag := range cards {
				_, _ = w.Write([]byte(frag))
			}
		}
		return
	}

	var works []*model.Work
	var err error
	// surfaceDef is set when `surface` named a user-pinnable interest surface, so
	// the empty state and the head lane below can speak about that surface by
	// name instead of about the Following feed.
	var surfaceDef *model.FeedSurface
	// A ranked surface fetches a wider chronological window than one page so
	// AethyrRank has something to choose from; rankWorks trims it back to the
	// page and moves the cursor below the whole window (see aethyr_rank.go).
	// Following and the profile/NSFW lanes fetch exactly one page as before.
	window := rankWindow(limit)

	switch surface {
	case "following":
		works, err = dbpkg.GetWorksFeedFollowing(h.db, user.ID, limit, before)

	case "trending":
		works, err = dbpkg.GetWorksTrending(h.db, window, before)

	case "nsfw":
		works, err = dbpkg.GetWorksNSFW(h.db, limit, before)

	case "for_you", "feed", "field", "focus":
		works, err = dbpkg.GetWorksForYou(h.db, window, before)

	case "topics":
		topicsParam := r.URL.Query().Get("topics")
		if topicsParam == "" {
			works, err = dbpkg.GetWorksForYou(h.db, window, before)
		} else {
			seen := map[string]bool{}
			for _, tag := range strings.SplitN(topicsParam, ",", 8) {
				tag = strings.TrimSpace(strings.ToLower(tag))
				if tag == "" {
					continue
				}
				res, qerr := dbpkg.SearchWorks(h.db, tag, window/4, user.ID)
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
		// A pinned interest surface. Its meaning is a server-side definition —
		// the tag set and content type held in feed_surfaces — so the surface id
		// is resolved to that definition and the works are selected from it.
		// Falling through to the Following feed here (as this branch used to)
		// made every pinned tab a relabelled copy of one timeline.
		if s := h.feedSurface(surface); s != nil {
			surfaceDef = s
			works, err = dbpkg.GetWorksBySurface(h.db, s.Tags, s.ContentType, window, before)
		} else {
			works, err = dbpkg.GetWorksFeedFollowing(h.db, user.ID, limit, before)
		}
	}

	if err != nil {
		log.Printf("[works-feed] %v", err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}

	// AethyrRank orders the window for this person on the Jung axes. The
	// cursor it returns is the oldest feed time in the whole window, so the
	// next page starts strictly below everything considered here. On any
	// ranker failure the window is served chronologically, trimmed to a page.
	var rankCursor time.Time
	if rankable(surface, surfaceDef) && len(works) > 0 {
		works, rankCursor = h.rankWorks(r.Context(), user, surface, r.URL.Query().Get("session_id"), works, limit)
	}
	// Adult stripping, enrichment (reactions, quotes, link previews, repliers,
	// the viewer's pinned mark) and the vision branch all live in
	// renderWorkCardSet — the single work_card entry point — so this feed and
	// the stream fan-out cannot produce different cards for the same work.
	cards := h.renderWorkCardSet(works, user, h.workCardCtx(user, surface))

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// A tab strip that asked for this surface receives its own re-render out of
	// band, so the active tab is the server's statement and not a class the
	// page toggled on itself.
	h.writeFeedTabsOOB(w, r, user, surface)

	// Facet(sports_strip) — the slate is the head lane of the sports surface and
	// of nothing else. It is written here, ahead of the works, because this
	// response IS the Playground content for the requested surface: switching to
	// another tab replaces this container and the strip goes with it, which is
	// what keeps the Home feed from carrying the same games the right rail is
	// already carrying persistently. Head lanes are not paginated, so it is
	// written on the first page only and can never reappear on page 2.
	// One strip per league, in the order sportsStripsData decides: a league with
	// something being played leads, and a league with nothing on writes nothing
	// at all — no header, no empty row.
	if surfaceDef != nil && surfaceDef.ID == sportsSurfaceID && before == "" {
		for _, strip := range h.sportsStripsData() {
			h.renderPartial(w, "sports_strip", strip)
		}
	}

	if len(cards) == 0 {
		es := emptyStates[surface]
		if es.Title == "" && surfaceDef != nil {
			es = emptyState{
				"Nothing in " + surfaceDef.Label + " yet",
				"Works tagged for this feed will appear here",
			}
		}
		if es.Title == "" {
			es = emptyState{"No works yet", "Works from people you follow will appear here"}
		}
		h.renderPartial(w, "stateEmpty", map[string]interface{}{
			"Title": es.Title,
			"Sub":   es.Sub,
		})
		return
	}
	for _, c := range cards {
		_, _ = w.Write([]byte(c.Fragment))
	}
	// The next page starts below the last card on a chronological surface,
	// and below the whole candidate window on a ranked one.
	cursorTS := cards[len(cards)-1].Work.CreatedAt
	if !rankCursor.IsZero() {
		cursorTS = rankCursor
	}
	cursor := cursorTS.Format(time.RFC3339)
	sentinelURL := fmt.Sprintf("/facets/works/feed?surface=%s&before=%s", url.QueryEscape(surface), url.QueryEscape(cursor))
	if handle := r.URL.Query().Get("handle"); handle != "" {
		sentinelURL += "&handle=" + url.QueryEscape(handle)
	}
	if topics := r.URL.Query().Get("topics"); topics != "" {
		sentinelURL += "&topics=" + url.QueryEscape(topics)
	}
	h.renderPartial(w, "feed_sentinel", map[string]interface{}{"URL": sentinelURL, "Cursor": cursor})
}

// homeTabSurface folds the surface ids the "For You" tab answers to onto the
// tab's own id, so the strip can mark it active for any of them.
func homeTabSurface(surface string) string {
	switch surface {
	case "for_you", "field", "focus":
		return "feed"
	}
	return surface
}

// writeFeedTabsOOB appends the tab strip that owns the requested container,
// re-rendered with the requested surface active, as an out-of-band fragment.
// Only a first page is a tab change; a sentinel page leaves the strip alone.
func (h *Handler) writeFeedTabsOOB(w http.ResponseWriter, r *http.Request, viewer *model.User, surface string) {
	q := r.URL.Query()
	if q.Get("before") != "" || q.Get("after") != "" || q.Get("pinned_id") != "" {
		return
	}
	switch r.Header.Get("HX-Target") {
	case "feed-container":
		var pinned []model.FeedSurface
		if h.db != nil && viewer != nil && viewer.ID != "" {
			pinned, _ = dbpkg.GetUserSurfaces(h.db, viewer.ID)
		}
		h.renderPartial(w, "home_feed_tabs", map[string]interface{}{
			"User":           viewer,
			"PinnedSurfaces": pinned,
			"SessionID":      q.Get("session_id"),
			"Active":         homeTabSurface(surface),
			"OOB":            true,
		})
	case "profile-feed":
		handle := q.Get("handle")
		if handle == "" {
			return
		}
		h.renderPartial(w, "profile_tabs", map[string]interface{}{
			"Handle":  handle,
			"IsOwner": viewer != nil && strings.EqualFold(handle, viewer.Handle),
			"Active":  surface,
			"OOB":     true,
		})
	}
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
	ctx := h.workCardCtx(user, "work_replies")

	// The lone replies in the stream are rendered as one batch through the
	// single work_card entry point (one enrichment pass for all of them), then
	// written back in stream order so the interleaving with microconversation
	// containers is preserved exactly.
	var loneReplies []*model.Work
	for _, item := range stream {
		if !item.IsMicroconversation && item.Work != nil {
			loneReplies = append(loneReplies, item.Work)
		}
	}
	fragments := make(map[string]template.HTML, len(loneReplies))
	for _, c := range h.renderWorkCardSet(loneReplies, user, ctx) {
		fragments[c.Work.ID] = c.Fragment
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
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
			continue
		}
		if item.Work == nil {
			continue
		}
		if frag, ok := fragments[item.Work.ID]; ok {
			_, _ = w.Write([]byte(frag))
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

	// Lone replies are NOT enriched here: they render as work_card fragments,
	// and renderWorkCardSet owns that enrichment. The work detail page, which
	// renders them through its page template instead, applies the same sequence
	// via enrichWorks. Microconversation seeds/exchanges render through their
	// own Facet and are enriched here.
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
		replyStream := h.buildReplyStream(id, viewerID)
		// The page template renders the lone replies itself; run them through
		// the same enrichment sequence renderWorkCardSet applies so a reply
		// looks identical on the page and on the reply-stream facet fetch.
		var loneReplies []*model.Work
		for _, item := range replyStream {
			if !item.IsMicroconversation && item.Work != nil {
				loneReplies = append(loneReplies, item.Work)
			}
		}
		h.enrichWorks(loneReplies, user)
		data["ReplyStream"] = replyStream
	}

	h.render(w, r, "works_detail.html", h.withRail(data, user, "default"))
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

	// The work is named by work_id; post_id is the same identifier under the
	// name the older web callers still send. eventField reads either, from a
	// form body or a JSON one.
	workID := eventField(r, rawBody, "work_id", "post_id")
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

	// The reaction is evidence about the reactor. Move their interest vector
	// on the Jung axes toward this work — or away from it, for a dislike — at
	// the rate that reaction type warrants; taking a reaction back reverses
	// the move at half the rate. Detached: the 204 does not wait on it, and
	// the viewer's cached session is evicted once the vector has moved.
	go h.learnFromReaction(user, workID, op.reactionType, op.add, sessionCacheKeys(r, user))

	if op.add {
		// The author is told that a like or a repost happened — and only on the
		// add half. Taking a reaction back is not an event anyone is told
		// about, and a notification that appears and then vanishes is worse
		// than one that stands. notifyReaction ignores the kinds nobody is owed
		// a word about (a dislike and a bookmark are the reader's own record of
		// their own reading) and refuses to tell a person about their own hands.
		go h.notifyReaction(user, workID, op.reactionType)

		// A repost is a feed item — GetWorksFeedFollowing unions reposts by
		// followed accounts into the feed at the time of the repost — so a feed
		// being kept live has to be told about one, or the live feed and the
		// fetched feed disagree for as long as the reader stays on the page.
		if op.reactionType == "repost" {
			go h.publishRepostToFollowers(user.ID, workID)
		}
	}

	// Troll achievement checks on engagement-count milestones (like/dislike/repost)
	if op.reactionType == "like" || op.reactionType == "dislike" || op.reactionType == "repost" {
		wID := workID
		go func() {
			var authorID, authorPIAL string
			h.db.QueryRow(`SELECT author_id, author_pial FROM works WHERE id = $1::uuid`, wID).Scan(&authorID, &authorPIAL)
			if authorID != "" {
				dbpkg.TryAwardTrollOnPostStats(h.db, authorID, authorPIAL, wID)
			}
		}()
	}

	// A JSON caller is answered with the work itself, read back after the write:
	// the counts and the viewer's own reaction state come from the database, so
	// the phone never has to compute what a tap did, and never has to follow the
	// mutation with a read. This is the same work GET /api/v1/works/{id} puts
	// under `.work` — same load, same walls, same viewer pipeline, same DTO.
	if reactionWantsJSON(r) {
		work, ok := h.apiLoadWork(w, workID, user)
		if !ok {
			return
		}
		prepared := h.apiPrepareWorks([]*model.Work{work}, user)
		if len(prepared) == 0 {
			apiError(w, http.StatusForbidden, "restricted", "This work is not available to you.")
			return
		}
		// The freshly-read work already carries the new counts — publish from it
		// rather than reading the row a second time.
		h.publishWorkEngagementFor(prepared[0])
		apiJSON(w, http.StatusOK, workDTO(prepared[0], nil))
		return
	}

	h.publishWorkEngagement(workID)
	w.WriteHeader(http.StatusNoContent)
}

// reactionWantsJSON reports whether the caller sent a JSON body and therefore
// expects the work back rather than an empty 204. The browser posts
// form-encoded (HTMX) and keeps the 204 it has always had; the app posts
// application/json. Parameters after the media type (charset) are ignored so a
// client that sends one is not silently downgraded.
func reactionWantsJSON(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.EqualFold(strings.TrimSpace(ct), "application/json")
}

// ── FA Live: engagement fan-out ──────────────────────────────────────────────

// workEngagementTwin is the counts-only projection of a work's engagement, the
// JSON half of the work_engagement event.
//
// It carries nothing viewer-relative. The event is fanned out to every watcher
// of the work at once, so a field that is true for the person who tapped and
// false for everyone else would be delivered to everyone else as a lie. Whether
// THIS viewer liked the work is answered by the reaction response and by
// GET /api/v1/works/{id}; the stream only ever moves the tallies.
//
// The keys are the WorkDTO count keys, verbatim, so the client applies them to
// the model it already holds without a second vocabulary. tip_total_uaet is not
// here because WorkDTO never populates it — an always-zero key would tell the
// client a work has no tips, which is not something this server knows.
type workEngagementTwin struct {
	WorkID        string `json:"work_id"`
	LikeCount     int    `json:"like_count"`
	DislikeCount  int    `json:"dislike_count"`
	ReplyCount    int    `json:"reply_count"`
	RepostCount   int    `json:"repost_count"`
	QuoteCount    int    `json:"quote_count"`
	BookmarkCount int    `json:"bookmark_count"`
	ViewCount     int    `json:"view_count"`
}

func newWorkEngagementTwin(work *model.Work) workEngagementTwin {
	return workEngagementTwin{
		WorkID:        work.ID,
		LikeCount:     work.LikeCount,
		DislikeCount:  work.DislikeCount,
		ReplyCount:    work.ReplyCount,
		RepostCount:   work.RepostCount,
		QuoteCount:    work.QuoteCount,
		BookmarkCount: work.BookmarkCount,
		ViewCount:     work.ViewCount,
	}
}

// publishWorkEngagement re-reads a work's tallies and pushes them to everyone
// watching it. Call it after any write that moves a count: a reaction, a reply,
// a quote. The viewer is deliberately absent from the read (viewerID "") — the
// row is loaded for its counts, which belong to no one.
//
// This is the entry point other handlers use (work_event.go's reply and quote
// paths); it is synchronous so the mutation that caused it cannot be answered
// before the watchers have been told.
func (h *Handler) publishWorkEngagement(workID string) {
	if h == nil || h.db == nil || workID == "" {
		return
	}
	work, err := dbpkg.GetWorkByID(h.db, workID, "")
	if err != nil {
		log.Printf("[engagement] reload %s: %v", workID, err)
		return
	}
	h.publishWorkEngagementFor(work)
}

// publishWorkEngagementFor publishes the engagement of an already-loaded work.
//
// Two events leave here for one mutation, because two clients are listening
// under two names for two shapes of the same fact:
//
//	work_engagement — the JSON twin, the only one the app's user stream forwards.
//	post_engagement — the rendered counts Facet, the name f33d3r.js has bound
//	                  since the FA Live layer landed. The browser finds the
//	                  element carrying the fragment's id and swaps it.
//
// The web event carries no twin and the app event carries no fragment, so
// neither client is handed a payload it would have to ignore.
func (h *Handler) publishWorkEngagementFor(work *model.Work) {
	if work == nil || work.ID == "" {
		return
	}
	twin, err := json.Marshal(newWorkEngagementTwin(work))
	if err != nil {
		log.Printf("[engagement] twin %s: %v", work.ID, err)
		return
	}
	PublishToPostWatchers(work.ID, SSEEvent{Type: "work_engagement", JSON: string(twin)})

	if fragment := h.renderWorkCountsRow(work); fragment != "" {
		PublishToPostWatchers(work.ID, SSEEvent{Type: "post_engagement", Data: fragment})
	}
}

// renderWorkCountsRow renders the work_counts_row Facet. A render failure costs
// the browser its live tally update and nothing else — the app's twin has
// already gone out — so it is logged and swallowed rather than propagated.
func (h *Handler) renderWorkCountsRow(work *model.Work) string {
	if h.partial == nil {
		return ""
	}
	var buf bytes.Buffer
	if err := h.partial.ExecuteTemplate(&buf, "work_counts_row", work); err != nil {
		log.Printf("[engagement] render work_counts_row %s: %v", work.ID, err)
		return ""
	}
	return buf.String()
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

	// What this work was citing, read before it goes. A deleted reply or quote
	// takes a count off the work it cited, and the citation edge is the only
	// record of which work that was — reply and quote totals are counted from
	// work_citations, not stored on the row.
	var citedWorkID string
	_ = h.db.QueryRow(
		`SELECT target_id::text FROM work_citations WHERE work_id = $1::uuid LIMIT 1`,
		workID).Scan(&citedWorkID)

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

	// The work it was replying to or quoting is now one shorter, and anyone
	// with that work on screen is still being shown the old number.
	if citedWorkID != "" {
		go h.publishWorkEngagement(citedWorkID)
	}

	// Broadcast post_deleted SSE to all active sessions so every viewer's DOM removes the card.
	// Data carries the work UUID so the client can locate the article by data-facet-id.
	go PublishToAllSessions(SSEEvent{
		Type: "post_deleted",
		Data: workID,
		JSON: fmt.Sprintf(`{"work_id":%q}`, workID),
	})

	// A work deleted from its own page leaves the viewer on a page that no
	// longer exists: the Shell is sent to the author's profile instead.
	if facetRequest(r) && strings.HasPrefix(hxCurrentPath(r), "/work/"+workID) {
		hxLocationHeader(w, "/"+user.Handle, true)
	}

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

	newBody := strings.TrimSpace(r.FormValue("body"))
	// One set of rules with the native clients' work_edit event; see
	// applyWorkEdit. A closed edit window answers 409 on both surfaces.
	if status, msg := h.applyWorkEdit(user, r.FormValue("work_id"), newBody); status != http.StatusOK {
		http.Error(w, msg, status)
		return
	}
	fmt.Fprintf(w, `<div class="work-body-text">%s</div>`, template.HTMLEscapeString(newBody))
}

// workEditionCID is the content id of one edition of a work.
func workEditionCID(workID string, edition int, body string) string {
	raw := workID + "|" + fmt.Sprintf("%d", edition) + "|" + body
	sum := sha256.Sum256([]byte(raw))
	return "sha256:" + hex.EncodeToString(sum[:])
}

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
