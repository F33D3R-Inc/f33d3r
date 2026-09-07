package handler

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/f33d3r/feed-engine/internal/aethyr"
	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/jung"
	"github.com/f33d3r/feed-engine/internal/model"
)

// ─────────────────────────────────────────────────────────────────────────────
// The ranked feed.
//
// A ranked surface is served in two steps: the chronological candidate window
// the works lane already knows how to page (a `before=` cursor over feed
// time), then AethyrRank ordering that window for this person. The ranker is
// advisory: it reorders, it never selects, and if it is slow or down the
// window is served in the order it came. The feed never blocks on it.
//
// Following stays strictly chronological, as X's Following tab does — that
// surface's promise is "everything from people I chose, in order", and a
// ranker has no business inside that promise. Every other works surface that
// pages the global pool — For You (and its aliases), Trending, Topics, and
// every pinned interest surface — is ranked.
// ─────────────────────────────────────────────────────────────────────────────

// rankWindowMultiplier is how many pages of candidates are fetched for one
// page of output on a ranked surface. Everything in the window that does not
// make the page is behind the cursor afterwards, so the multiplier is the
// trade between ranking depth and content reach: at 3, the ranker chooses 20
// from 60.
const rankWindowMultiplier = 3

// rankWindowMax bounds the candidate window regardless of page size.
const rankWindowMax = 120

// rankTimeout is the whole budget the feed gives the ranker, including the
// network round trip. Past it the chronological window is served.
const rankTimeout = 150 * time.Millisecond

// rankHistoryLimit is how many recent reactions are sent as the person's
// interaction history, which the ranker reads for fatigue.
const rankHistoryLimit = 50

// followAffinity is the creator affinity assigned to every followed account.
// The follow graph is a declared preference of uniform strength; learned
// per-creator affinities are the interest vector's job.
const followAffinity = 0.30

// rankable reports whether a surface is ordered by AethyrRank. surfaceDef is
// non-nil only when `surface` named a pinned interest surface.
func rankable(surface string, surfaceDef *model.FeedSurface) bool {
	switch surface {
	case "for_you", "feed", "field", "focus", "trending", "topics":
		return true
	}
	return surfaceDef != nil
}

// rankWindow is the candidate window size for one page of `limit` items.
func rankWindow(limit int) int {
	n := limit * rankWindowMultiplier
	if n > rankWindowMax {
		n = rankWindowMax
	}
	if n < limit {
		n = limit
	}
	return n
}

// feedTime is when a work surfaced in a feed: the repost time when it is
// here because someone reposted it, otherwise its publication time. It is
// the quantity the `before=` cursor pages over.
func feedTime(w *model.Work) time.Time {
	if !w.RepostedAt.IsZero() {
		return w.RepostedAt
	}
	return w.CreatedAt
}

// rankWorks orders one candidate window for one person and returns the page
// plus the cursor the next page must start from.
//
// The cursor is the oldest feed time in the WHOLE window, not in the page:
// the next request then starts strictly below everything this request
// considered, so no work is served twice across pages and no page overlaps
// the last. Works the ranker did not return — dropped by its pre-ranker, or
// held back by its safety barrier — follow the ranked ones in chronological
// order; they are still gated per item at render time by the content gate,
// exactly as before.
func (h *Handler) rankWorks(ctx context.Context, user *model.User, surface, sessionID string, window []*model.Work, limit int) ([]*model.Work, time.Time) {
	var cursor time.Time
	for _, w := range window {
		if t := feedTime(w); cursor.IsZero() || t.Before(cursor) {
			cursor = t
		}
	}
	// Strip what this viewer may never see before ranking, so the page the
	// ranker fills is a full page. renderWorkCardSet applies the same filter
	// again; it is idempotent.
	window = filterAdultForViewer(user, window)
	if len(window) == 0 {
		return window, cursor
	}
	if h.aethyr == nil || h.db == nil {
		return trimWorks(window, limit), cursor
	}

	h.ensureWorkVectors(window)
	req := h.buildRankRequest(user, surface, sessionID, window, limit)

	rctx, cancel := context.WithTimeout(ctx, rankTimeout)
	defer cancel()
	resp, err := h.aethyr.Rank(rctx, req)
	if err != nil {
		log.Printf("[aethyr] rank unavailable for surface=%s: %v — serving chronological window", surface, err)
		return trimWorks(window, limit), cursor
	}

	byID := make(map[string]*model.Work, len(window))
	for _, w := range window {
		byID[w.ID] = w
	}
	ordered := make([]*model.Work, 0, len(window))
	seen := make(map[string]bool, len(window))
	for _, item := range resp.RankedItems {
		if item.SafetyBlocked {
			continue
		}
		w := byID[item.ContentID]
		if w == nil || seen[w.ID] {
			continue
		}
		seen[w.ID] = true
		ordered = append(ordered, w)
	}
	for _, w := range window {
		if !seen[w.ID] {
			seen[w.ID] = true
			ordered = append(ordered, w)
		}
	}
	return trimWorks(ordered, limit), cursor
}

func trimWorks(works []*model.Work, limit int) []*model.Work {
	if limit > 0 && len(works) > limit {
		return works[:limit]
	}
	return works
}

// ensureWorkVectors guarantees every work in the set carries its psych
// vector, computing it with jung.MapWork where the column was NULL, and
// returns the vectors in the same order. Lazily computed vectors are written
// back in one detached batch — a work computed once is stored once — so the
// column converges to full coverage under ordinary reads with no backfill.
func (h *Handler) ensureWorkVectors(works []*model.Work) []jung.Vector {
	out := make([]jung.Vector, len(works))
	var computed map[string][]float32
	for i, w := range works {
		if w == nil {
			out[i] = jung.Neutral()
			continue
		}
		if v, ok := jung.FromSlice(w.PsychVector); ok {
			out[i] = v
			continue
		}
		v := jung.MapWork(w)
		w.PsychVector = v.Slice()
		out[i] = v
		if computed == nil {
			computed = make(map[string][]float32)
		}
		computed[w.ID] = w.PsychVector
	}
	if len(computed) > 0 && h.db != nil {
		go func(vectors map[string][]float32) {
			if _, err := dbpkg.SaveWorkPsychVectors(h.db, vectors); err != nil {
				log.Printf("[jung] persisting %d work vectors: %v", len(vectors), err)
			}
		}(computed)
	}
	return out
}

// buildRankRequest assembles the engine's request for one window. Every
// per-candidate signal comes from one batched query over the window.
func (h *Handler) buildRankRequest(user *model.User, surface, sessionID string, window []*model.Work, limit int) *aethyr.RankRequest {
	ids := make([]string, 0, len(window))
	authorSet := make(map[string]bool, len(window))
	authorIDs := make([]string, 0, len(window))
	for _, w := range window {
		ids = append(ids, w.ID)
		if !authorSet[w.AuthorID] {
			authorSet[w.AuthorID] = true
			authorIDs = append(authorIDs, w.AuthorID)
		}
	}

	viewerID := viewerAccountID(user)
	var followed map[string]bool
	var history []string
	if viewerID != "" {
		followed = dbpkg.GetFollowedUserIDSet(h.db, viewerID)
		history = dbpkg.GetRecentReactionWorkIDs(h.db, viewerID, rankHistoryLimit)
	}
	posts24 := dbpkg.BatchGetCreatorPostCounts24h(h.db, authorIDs)
	exposure := dbpkg.BatchGetCreatorExposure(h.db, authorIDs)
	recent := dbpkg.BatchGetRecentReactionCounts(h.db, ids, aethyr.VelocityWindow)
	selfReply := dbpkg.BatchGetSelfReplyFirst30m(h.db, ids)
	dwell := dbpkg.BatchGetDwellSecondsPerContent(h.db, ids)

	pool := make([]model.AethyrContent, 0, len(window))
	for _, w := range window {
		pool = append(pool, aethyr.WorkToContent(w, aethyr.WorkSignals{
			ViewTimeSeconds: dwell[w.ID],
			RecentReactions: recent[w.ID],
			PostsLast24h:    posts24[w.AuthorID],
			SelfReply:       selfReply[w.ID],
			CreatorExposure: exposure[w.AuthorID],
			InNetwork:       followed[w.AuthorID],
		}))
	}

	affinities := make(map[string]float32, len(followed))
	for id := range followed {
		affinities[id] = followAffinity
	}
	if history == nil {
		history = []string{}
	}

	interest, ok := jung.FromSlice(user.InterestVector)
	if !ok {
		interest, _ = jung.FromSlice(h.computeInterestVector(user))
	}
	tier := user.Tier
	if tier == "" {
		tier = "free"
	}
	realm := user.Realm
	if realm < 1 {
		realm = 1
	}
	if sessionID == "" {
		sessionID = uuid.New().String()
	}

	return &aethyr.RankRequest{
		UserState: model.AethyrUserState{
			UserID:             user.ID,
			InterestVector:     interest.Slice(),
			InteractionHistory: history,
			IsColdStart:        user.IsColdStart || len(history) == 0,
			CreatorAffinities:  affinities,
			SafetyEpsilon:      user.SafetyEpsilon,
			UserTier:           tier,
			RealmLevel:         realm,
		},
		ContentPool: pool,
		SessionContext: model.AethyrSession{
			Surface:   surface,
			RequestID: uuid.New().String(),
			SessionID: sessionID,
			MaxItems:  limit,
			Timestamp: time.Now().UTC(),
		},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Learning: the interest vector as an online average.
//
// Every reaction moves the person's interest vector toward (or, for a
// dislike, away from) the vector of the work reacted to, by a rate that says
// how strong a statement the reaction is. Undoing a reaction reverses the
// move at half the rate: taking a like back is weaker evidence than giving
// it was. A reply or quote is engagement with the work it cites and moves the
// vector toward that parent.
// ─────────────────────────────────────────────────────────────────────────────

const (
	rateLike     float32 = 0.06
	rateRepost   float32 = 0.10
	rateBookmark float32 = 0.08
	rateDislike  float32 = 0.10
	rateCitation float32 = 0.08
)

// reactionRate maps a reaction type to its learning rate and direction.
// A zero rate means the reaction teaches nothing.
func reactionRate(reactionType string) (rate float32, away bool) {
	switch reactionType {
	case "like":
		return rateLike, false
	case "repost":
		return rateRepost, false
	case "bookmark":
		return rateBookmark, false
	case "dislike":
		return rateDislike, true
	}
	return 0, false
}

// sessionCacheKeys are the session-cache entries that hold this viewer, so a
// changed interest vector is picked up on the very next request rather than
// after the cache TTL.
func sessionCacheKeys(r *http.Request, user *model.User) []string {
	keys := make([]string, 0, 2)
	if tok := GetSessionToken(r); tok != "" && !strings.HasPrefix(tok, "__handle__") {
		keys = append(keys, tok)
	}
	if user != nil && user.Handle != "" {
		keys = append(keys, "__handle__"+user.Handle)
	}
	return keys
}

// learnFromReaction moves the reactor's interest vector for one reaction.
// Runs detached from the request; the reaction itself was already written.
func (h *Handler) learnFromReaction(user *model.User, workID, reactionType string, add bool, cacheKeys []string) {
	if h.db == nil || viewerAccountID(user) == "" {
		return
	}
	rate, away := reactionRate(reactionType)
	if rate == 0 {
		return
	}
	if !add {
		rate /= 2
		away = !away
	}
	w, err := dbpkg.GetWorkForPsych(h.db, workID)
	if err != nil {
		log.Printf("[jung] work %s for reaction learning: %v", workID, err)
		return
	}
	target := h.ensureWorkVectors([]*model.Work{w})[0]
	h.nudgeInterestVector(user, target, rate, away, cacheKeys)
}

// learnFromCitation moves the author's interest vector toward the work their
// new reply or quote cites.
func (h *Handler) learnFromCitation(user *model.User, parentCID string, cacheKeys []string) {
	if h.db == nil || viewerAccountID(user) == "" || parentCID == "" {
		return
	}
	parent, err := dbpkg.GetWorkForPsychByCID(h.db, parentCID)
	if err != nil {
		log.Printf("[jung] parent %s for citation learning: %v", parentCID, err)
		return
	}
	target := h.ensureWorkVectors([]*model.Work{parent})[0]
	h.nudgeInterestVector(user, target, rateCitation, false, cacheKeys)
}

// nudgeInterestVector applies one EMA step to the stored interest vector,
// persists it, and evicts the viewer's cached session so the next request
// carries the moved vector. The stored vector is re-read rather than taken
// from the request's user, so two reactions in quick succession compound
// instead of the second overwriting the first.
func (h *Handler) nudgeInterestVector(user *model.User, target jung.Vector, rate float32, away bool, cacheKeys []string) {
	current, ok := jung.FromSlice(dbpkg.LoadInterestVector(h.db, user.ID))
	if !ok {
		if current, ok = jung.FromSlice(user.InterestVector); !ok {
			current = jung.ArchetypePrior(user.JungArchetype)
		}
	}
	var next jung.Vector
	if away {
		next = jung.Repel(current, target, rate)
	} else {
		next = jung.Blend(current, target, rate)
	}
	if err := dbpkg.SaveInterestVector(h.db, user.ID, next.Slice()); err != nil {
		log.Printf("[jung] saving interest vector for %s: %v", user.ID, err)
		return
	}
	for _, k := range cacheKeys {
		h.sessionCache.Delete(k)
	}
}
