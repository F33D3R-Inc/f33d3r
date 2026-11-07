package handler

// timeline.go — the one way a list of works is built.
//
// Every timeline runs the same stages in the same order, and a surface cannot change
// them. A surface is an entry in the registry below: what it reads, whether it ranks,
// and what it says when empty. So adding a surface cannot skip a safety check, and every
// surface reports the same numbers.
//
//	prepare     work out who and what is being asked for, and whether they may ask
//	candidates  read works from the database
//	filter      gateWorks: what this viewer may see
//	rank        order them, on surfaces that rank
//	enrich      reactions, quotes, previews, ballots
//	measure     how many were read, withheld and shown
//
// What comes out is a finished list. The caller only turns it into a response: Fragments
// for a page or a scroll, DTOs for the API.

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

const timelinePageSize = 20

const (
	// A ranked window is kept this long so its later pages come from the same order.
	// Only the order is kept: every page is filtered again, so a block applies at once.
	timelinePoolTTL    = 3 * time.Minute
	timelinePoolPrefix = "pool:"
	// Works shown recently on a ranked surface sit out, so a refresh brings new ones.
	timelineServedTTL = 10 * time.Minute
	timelineServedMax = 100
)

var (
	timelinePools  = &ttlCache{max: 20000}
	timelineServed = &ttlCache{max: 50000}
)

var (
	timelineItems = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "timeline_items_total",
		Help: "Works passing through each timeline stage, by surface.",
	}, []string{"surface", "stage"})
	timelineSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "timeline_seconds",
		Help:    "Time to build one page of a timeline, by surface.",
		Buckets: prometheus.DefBuckets,
	}, []string{"surface"})
	timelineFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "timeline_failures_total",
		Help: "Timeline reads that failed, by surface.",
	}, []string{"surface"})
)

func init() {
	prometheus.MustRegister(timelineItems, timelineSeconds, timelineFailures)
}

// timelineEmpty is what a surface says when it has nothing.
type timelineEmpty struct{ Title, Sub string }

// timelineRefusal is a person not being allowed to read a surface. It is not a failure.
type timelineRefusal struct {
	Status        int
	Code, Message string
}

// timelineQuery is one request for one page.
type timelineQuery struct {
	Viewer    *model.User
	Limit     int
	Before    string
	SessionID string

	Handle    string // the person a profile surface is about
	Profile   *model.User
	ListID    string
	Filter    string // a bookmark filter
	ExcludeID string // a pinned work already shown above this feed
	Tag       string
	Terms     []string // words to find, on surfaces that search
	Def       *model.FeedSurface
}

// timelineSpec is a surface: its parts, never its order.
type timelineSpec struct {
	Name string
	// Ranked surfaces read a wider window and let AethyrRank choose the page from it.
	Ranked bool
	// KeepMuted is for a page about one person.
	KeepMuted bool
	// Paged says a later page exists to ask for.
	Paged bool
	// WebOnly surfaces aren't API lanes: the API has its own routes for them.
	WebOnly    bool
	Prepare    func(*Handler, *timelineQuery) *timelineRefusal
	Candidates func(*Handler, *timelineQuery, int) ([]*model.Work, error)
	Provenance func(*model.User) func(*model.Work) *ProvenanceDTO
	Empty      timelineEmpty
}

// timelineResult is a finished page.
type timelineResult struct {
	Works []*model.Work
	// Next is where the next page starts, or empty at the end.
	Next string
}

// runTimeline is the pipeline. The stages are written here and nowhere else.
func (h *Handler) runTimeline(ctx context.Context, spec *timelineSpec, q *timelineQuery) (timelineResult, *timelineRefusal, error) {
	started := time.Now()
	if q.Limit <= 0 {
		q.Limit = timelinePageSize
	}

	// prepare
	if spec.Prepare != nil {
		if refusal := spec.Prepare(h, q); refusal != nil {
			return timelineResult{}, refusal, nil
		}
	}

	// candidates
	window := q.Limit
	if spec.Ranked {
		window = rankWindow(q.Limit)
	}
	viewerKey := gateViewerID(q.Viewer) + "|" + spec.Name
	var (
		read []*model.Work
		pool *timelinePoolCursor
		err  error
	)
	if spec.Ranked {
		pool = parseTimelinePool(q.Before)
	}
	if pool != nil {
		// The next stretch of an order already chosen. If it has aged out, carry on
		// below the window it came from; nothing is skipped or shown twice.
		ids, held := timelinePools.get(viewerKey + "|" + pool.ID)
		switch {
		case held:
			pool.IDs = ids.([]string)
			end := min(pool.Offset+q.Limit, len(pool.IDs))
			if pool.Offset < end {
				read, err = dbpkg.GetWorksByIDs(h.db, pool.IDs[pool.Offset:end])
			}
		case pool.Below == "":
			return timelineResult{}, nil, nil
		default:
			q.Before, pool = pool.Below, nil
		}
	}
	if pool == nil {
		read, err = spec.Candidates(h, q, window)
	}
	if err != nil {
		timelineFailures.WithLabelValues(spec.Name).Inc()
		return timelineResult{}, nil, err
	}

	// filter
	shown := h.gateWorksWith(q.Viewer, read, gateOptions{keepMuted: spec.KeepMuted})
	if spec.Ranked {
		if pool == nil {
			shown = withoutRecentlyServed(viewerKey, shown, q.Before == "")
		}
		shown = h.withoutNotWanted(q.Viewer, shown)
	}

	// rank
	var next string
	switch {
	case pool != nil:
		next = pool.Below
		if pool.Offset+q.Limit < len(pool.IDs) {
			next = pool.cursor(pool.Offset + q.Limit)
		}
	case spec.Ranked && len(shown) > 0:
		rankSurface := spec.Name
		if q.Def != nil {
			rankSurface = q.Def.ID
		}
		shown = h.rankWorks(ctx, q.Viewer, rankSurface, q.SessionID, shown, q.Limit)
		next = timelineNext(spec, read, window)
		if len(shown) > q.Limit {
			// The rest of the window stays reachable, in the order it was ranked.
			chosen := &timelinePoolCursor{ID: uuid.NewString(), Below: next}
			ids := make([]string, len(shown))
			for i, w := range shown {
				ids[i] = w.ID
			}
			timelinePools.put(viewerKey+"|"+chosen.ID, ids, time.Now().Add(timelinePoolTTL))
			next = chosen.cursor(q.Limit)
			shown = shown[:q.Limit]
		}
	default:
		next = timelineNext(spec, read, window)
	}

	// enrich
	h.enrichWorks(shown, q.Viewer)
	h.gateQuotes(q.Viewer, shown)

	// measure
	if spec.Ranked {
		rememberServed(viewerKey, shown)
	}
	timelineItems.WithLabelValues(spec.Name, "read").Add(float64(len(read)))
	timelineItems.WithLabelValues(spec.Name, "shown").Add(float64(len(shown)))
	timelineSeconds.WithLabelValues(spec.Name).Observe(time.Since(started).Seconds())

	return timelineResult{Works: shown, Next: next}, nil, nil
}

// timelinePoolCursor points into a ranked order held on the server.
type timelinePoolCursor struct {
	ID     string
	Offset int
	Below  string // where to go when the order runs out
	IDs    []string
}

func (p *timelinePoolCursor) cursor(offset int) string {
	return timelinePoolPrefix + p.ID + ":" + strconv.Itoa(offset) + ":" + p.Below
}

func parseTimelinePool(raw string) *timelinePoolCursor {
	rest, ok := strings.CutPrefix(raw, timelinePoolPrefix)
	if !ok {
		return nil
	}
	parts := strings.SplitN(rest, ":", 3)
	if len(parts) != 3 {
		return nil
	}
	offset, err := strconv.Atoi(parts[1])
	if err != nil || offset < 0 {
		return nil
	}
	return &timelinePoolCursor{ID: parts[0], Offset: offset, Below: parts[2]}
}

// withoutRecentlyServed drops what this person just saw. On a first page it won't leave
// nothing: on a small network a repeat is better than an empty feed.
func withoutRecentlyServed(viewerKey string, works []*model.Work, keepIfEmpty bool) []*model.Work {
	served, ok := timelineServed.get(viewerKey)
	if !ok {
		return works
	}
	seen := map[string]bool{}
	for _, id := range served.([]string) {
		seen[id] = true
	}
	fresh := make([]*model.Work, 0, len(works))
	for _, w := range works {
		if !seen[w.ID] {
			fresh = append(fresh, w)
		}
	}
	if len(fresh) == 0 && keepIfEmpty {
		return works
	}
	return fresh
}

// withoutNotWanted takes out what this person asked to see less of. It can only shape a
// recommendation feed: people they follow still appear in Following.
func (h *Handler) withoutNotWanted(viewer *model.User, works []*model.Work) []*model.Work {
	viewerID := gateViewerID(viewer)
	if viewerID == "" || h.db == nil || len(works) == 0 {
		return works
	}
	notWanted, err := dbpkg.GetNotWanted(h.db, viewerID)
	if err != nil {
		log.Printf("[timeline] not wanted: %v", err)
		return works
	}
	kept := make([]*model.Work, 0, len(works))
	for _, w := range works {
		if !notWanted.Works[w.ID] && !notWanted.Authors[w.AuthorID] {
			kept = append(kept, w)
		}
	}
	return kept
}

// forYouCandidates reads from several places. The stream of recent works is what pages:
// it goes last so the cursor continues from it. The first read also brings people the
// viewer follows, topics they follow and what is popular. A source that fails is left
// out rather than losing the feed, but the stream itself must answer.
func forYouCandidates(h *Handler, q *timelineQuery, n int) ([]*model.Work, error) {
	stream, err := dbpkg.GetWorksForYou(h.db, n, q.Before)
	if err != nil {
		return nil, err
	}
	viewerID := gateViewerID(q.Viewer)
	if q.Before != "" || viewerID == "" {
		return stream, nil
	}
	inStream := make(map[string]bool, len(stream))
	for _, w := range stream {
		inStream[w.ID] = true
	}
	var merged []*model.Work
	add := func(source string, works []*model.Work, err error) {
		if err != nil {
			log.Printf("[timeline] for_you %s: %v", source, err)
			return
		}
		for _, w := range works {
			if !inStream[w.ID] {
				inStream[w.ID] = true
				merged = append(merged, w)
			}
		}
	}
	following, err := dbpkg.GetWorksFeedFollowing(h.db, viewerID, n/2, "")
	add("following", following, err)
	if topics := dbpkg.GetSubscribedTopics(h.db, viewerID); len(topics) > 0 {
		fromTopics, err := dbpkg.GetWorksBySurface(h.db, topics, "", n/4, "")
		add("topics", fromTopics, err)
	}
	popular, err := dbpkg.GetWorksPopular(h.db, n/3)
	add("popular", popular, err)
	return append(merged, stream...), nil
}

func rememberServed(viewerKey string, works []*model.Work) {
	if len(works) == 0 {
		return
	}
	var ids []string
	if served, ok := timelineServed.get(viewerKey); ok {
		ids = append(ids, served.([]string)...)
	}
	for _, w := range works {
		ids = append(ids, w.ID)
	}
	if len(ids) > timelineServedMax {
		ids = ids[len(ids)-timelineServedMax:]
	}
	timelineServed.put(viewerKey, ids, time.Now().Add(timelineServedTTL))
}

// timelineNext says where the next page starts: the exact place of the last work read,
// whether or not this viewer could see it, so withheld works can never end or repeat a
// feed. A short read means the feed is finished.
func timelineNext(spec *timelineSpec, read []*model.Work, window int) string {
	if !spec.Paged || len(read) < window {
		return ""
	}
	last := read[len(read)-1]
	return feedTime(last).Format(time.RFC3339Nano) + "|" + last.ID
}

// ── prepare stages ───────────────────────────────────────────────────────────

func prepareAdultFeed(_ *Handler, q *timelineQuery) *timelineRefusal {
	if hideAdultCreators(q.Viewer) {
		return &timelineRefusal{http.StatusForbidden, "adult_content_disabled", "Turn on adult content in Settings to open this feed."}
	}
	return nil
}

// prepareProfile applies a person's walls, whichever road asked for their works.
func prepareProfile(h *Handler, q *timelineQuery) *timelineRefusal {
	if q.Profile == nil {
		profile, err := dbpkg.GetUserByHandle(h.db, strings.TrimPrefix(q.Handle, "@"))
		if err != nil || profile == nil {
			return &timelineRefusal{http.StatusNotFound, "not_found", "user not found"}
		}
		q.Profile = profile
	}
	if code, message := h.apiProfileWall(q.Profile, q.Viewer); code != "" {
		return &timelineRefusal{http.StatusForbidden, code, message}
	}
	return nil
}

// prepareOwnOnly is for what a person keeps to themselves.
func prepareOwnOnly(code, message string) func(*Handler, *timelineQuery) *timelineRefusal {
	return func(h *Handler, q *timelineQuery) *timelineRefusal {
		if q.Handle == "" && q.Profile == nil {
			return nil
		}
		if refusal := prepareProfile(h, q); refusal != nil {
			return refusal
		}
		if q.Profile.ID != gateViewerID(q.Viewer) {
			return &timelineRefusal{http.StatusForbidden, code, message}
		}
		return nil
	}
}

// ── the registry ─────────────────────────────────────────────────────────────

var (
	specFollowing = &timelineSpec{
		Name: "following", Paged: true, Provenance: apiFollowingProvenance,
		Empty: timelineEmpty{"Nothing yet", "Follow people to fill your feed"},
		Candidates: func(h *Handler, q *timelineQuery, n int) ([]*model.Work, error) {
			return dbpkg.GetWorksFeedFollowing(h.db, gateViewerID(q.Viewer), n, q.Before)
		},
	}
	specForYou = &timelineSpec{
		Name: "for_you", Ranked: true, Paged: true, Provenance: apiDiscoveryProvenance,
		Empty:      timelineEmpty{"Nothing here yet", "Post something to get started"},
		Candidates: forYouCandidates,
	}
	specTrending = &timelineSpec{
		Name: "trending", Ranked: true,
		Provenance: func(*model.User) func(*model.Work) *ProvenanceDTO { return apiTrendingProvenance() },
		Empty:      timelineEmpty{"No trending works", "Check back soon"},
		Candidates: func(h *Handler, q *timelineQuery, n int) ([]*model.Work, error) {
			return dbpkg.GetWorksPopular(h.db, n)
		},
	}
	specNSFW = &timelineSpec{
		Name: "nsfw", Paged: true, Prepare: prepareAdultFeed,
		Empty: timelineEmpty{"No NSFW works yet", "Adult content posted via the new pipeline will appear here"},
		Candidates: func(h *Handler, q *timelineQuery, n int) ([]*model.Work, error) {
			return dbpkg.GetWorksNSFW(h.db, n, q.Before)
		},
	}
	specVisions = &timelineSpec{
		Name: surfaceVisions, Paged: true,
		Empty: timelineEmpty{"No visions yet", ""},
		Candidates: func(h *Handler, q *timelineQuery, n int) ([]*model.Work, error) {
			return dbpkg.GetVisionsWorksForYou(h.db, n, q.Before)
		},
	}
	specMusic = &timelineSpec{
		Name: "music", Ranked: true, Paged: true,
		Empty: timelineEmpty{"Nothing in Music yet", "Works tagged for this feed will appear here"},
		Candidates: func(h *Handler, q *timelineQuery, n int) ([]*model.Work, error) {
			return dbpkg.GetWorksBySurface(h.db, []string{"music"}, "audio", n, q.Before)
		},
	}
	// specInterest serves every lane defined in feed_surfaces.
	specInterest = &timelineSpec{
		Name: "interest", Ranked: true, Paged: true,
		Candidates: func(h *Handler, q *timelineQuery, n int) ([]*model.Work, error) {
			return dbpkg.GetWorksBySurface(h.db, q.Def.Tags, q.Def.ContentType, n, q.Before)
		},
	}
	specTopics = &timelineSpec{
		Name: "topics", Ranked: true, Paged: true,
		Empty: timelineEmpty{"No results", "Try a different topic"},
		Candidates: func(h *Handler, q *timelineQuery, n int) ([]*model.Work, error) {
			if len(q.Terms) == 0 {
				return dbpkg.GetWorksForYou(h.db, n, q.Before)
			}
			return dbpkg.SearchWorksPage(h.db, q.Terms, n, q.Before, gateViewerID(q.Viewer))
		},
	}
	specTagLane = &timelineSpec{
		Name: "tag_lane", Paged: true,
		Empty: timelineEmpty{"No works yet", ""},
		Candidates: func(h *Handler, q *timelineQuery, n int) ([]*model.Work, error) {
			return dbpkg.GetWorksBySurface(h.db, []string{q.Tag}, "", n, q.Before)
		},
	}
	specProfilePosts = &timelineSpec{
		Name: "profile_posts", KeepMuted: true, Paged: true, Prepare: prepareProfile,
		Empty: timelineEmpty{"No posts yet", "This creator hasn't posted yet"},
		Candidates: func(h *Handler, q *timelineQuery, n int) ([]*model.Work, error) {
			return dbpkg.GetWorksForProfile(h.db, q.Profile.PIALID, n, q.Before)
		},
	}
	specProfileReplies = &timelineSpec{
		Name: "profile_replies", KeepMuted: true, Paged: true, Prepare: prepareProfile,
		Empty: timelineEmpty{"No replies yet", ""},
		Candidates: func(h *Handler, q *timelineQuery, n int) ([]*model.Work, error) {
			return dbpkg.GetWorkRepliesForProfile(h.db, q.Profile.PIALID, n, q.Before)
		},
	}
	specProfileMedia = &timelineSpec{
		Name: "profile_media", KeepMuted: true, Paged: true, Prepare: prepareProfile,
		Empty: timelineEmpty{"No media yet", ""},
		Candidates: func(h *Handler, q *timelineQuery, n int) ([]*model.Work, error) {
			return dbpkg.GetWorksForProfileMedia(h.db, q.Profile.PIALID, n, q.Before)
		},
	}
	specProfileReposts = &timelineSpec{
		Name: "profile_reposts", KeepMuted: true, Paged: true, Prepare: prepareProfile,
		Empty: timelineEmpty{"No reposts yet", ""},
		Candidates: func(h *Handler, q *timelineQuery, n int) ([]*model.Work, error) {
			return dbpkg.GetWorksRepostedByUser(h.db, q.Profile.ID, n, q.Before)
		},
	}
	specProfileLikes = &timelineSpec{
		Name: "profile_likes", KeepMuted: true, Paged: true,
		Prepare: prepareOwnOnly("likes_private", "Likes are private."),
		Empty:   timelineEmpty{"No likes yet", ""},
		Candidates: func(h *Handler, q *timelineQuery, n int) ([]*model.Work, error) {
			return dbpkg.GetWorksLikedByUserBefore(h.db, gateViewerID(q.Viewer), n, q.Before)
		},
	}
	specProfileSaves = &timelineSpec{
		Name: "profile_saves", KeepMuted: true, Paged: true,
		Prepare: prepareOwnOnly("saves_private", "Saves are private."),
		Empty:   timelineEmpty{"No saves yet", "Like or bookmark works to save them"},
		Candidates: func(h *Handler, q *timelineQuery, n int) ([]*model.Work, error) {
			return dbpkg.GetWorksSavesFiltered(h.db, gateViewerID(q.Viewer), n, q.Before, q.Filter)
		},
	}
	specBookmarks = &timelineSpec{
		Name: "bookmarks", Paged: true, WebOnly: true,
		Empty: timelineEmpty{"No bookmarks yet", "Works you save will appear here"},
		Candidates: func(h *Handler, q *timelineQuery, n int) ([]*model.Work, error) {
			return dbpkg.GetWorksSavesFiltered(h.db, gateViewerID(q.Viewer), n, q.Before, q.Filter)
		},
	}
	specList = &timelineSpec{
		Name: "list", Paged: true,
		Empty: timelineEmpty{"No posts yet", "Works from list members will appear here."},
		Candidates: func(h *Handler, q *timelineQuery, n int) ([]*model.Work, error) {
			return dbpkg.GetWorksByList(h.db, q.ListID, n, q.Before)
		},
	}
	// specSearch serves search and everything built on finding words: tags, tickers, games.
	specSearch = &timelineSpec{
		Name: "search", Paged: true, WebOnly: true,
		Candidates: func(h *Handler, q *timelineQuery, n int) ([]*model.Work, error) {
			return dbpkg.SearchWorksPage(h.db, q.Terms, n, q.Before, gateViewerID(q.Viewer))
		},
	}
)

// timelineSurfaces names every fixed surface, with the older names still in links.
var timelineSurfaces = map[string]*timelineSpec{
	"following":       specFollowing,
	"for_you":         specForYou,
	"foryou":          specForYou,
	"feed":            specForYou,
	"field":           specForYou,
	"focus":           specForYou,
	"trending":        specTrending,
	"nsfw":            specNSFW,
	surfaceVisions:    specVisions,
	"topics":          specTopics,
	"profile_posts":   specProfilePosts,
	"profile_replies": specProfileReplies,
	"profile_media":   specProfileMedia,
	"profile_reposts": specProfileReposts,
	"profile_likes":   specProfileLikes,
	"profile_saves":   specProfileSaves,
	"search":          specSearch,
	"bookmarks":       specBookmarks,
}

// timelineSurface finds a surface by name. Anything not fixed is looked up in
// feed_surfaces, where interest lanes are defined. Nil means there is no such surface.
func (h *Handler) timelineSurface(name string, q *timelineQuery) *timelineSpec {
	if spec, ok := timelineSurfaces[name]; ok {
		return spec
	}
	if def := h.feedSurface(name); def != nil {
		q.Def = def
		return specInterest
	}
	return nil
}

// surfacePage runs a surface a page draws itself, for its first screen.
type surfacePage struct {
	Works      []*model.Work
	NextURL    string
	NextCursor string
	Failed     bool
}

func (h *Handler) readSurfacePage(ctx context.Context, spec *timelineSpec, q *timelineQuery) surfacePage {
	result, _, err := h.runTimeline(ctx, spec, q)
	if err != nil {
		log.Printf("[timeline] %s: %v", spec.Name, err)
		return surfacePage{Failed: true}
	}
	page := surfacePage{Works: result.Works, NextCursor: result.Next}
	if result.Next != "" {
		page.NextURL = timelineNextURL(spec.Name, q, result.Next)
	}
	return page
}

// timelineNextURL is the address of the next page, carrying what chose this one.
func timelineNextURL(surface string, q *timelineQuery, cursor string) string {
	v := url.Values{"surface": {surface}, "before": {cursor}}
	if q.Handle != "" {
		v.Set("handle", q.Handle)
	}
	if q.Filter != "" {
		v.Set("filter", q.Filter)
	}
	if q.ExcludeID != "" {
		v.Set("exclude_id", q.ExcludeID)
	}
	if q.SessionID != "" {
		v.Set("session_id", q.SessionID)
	}
	if surface == "topics" {
		v.Set("topics", strings.Join(q.Terms, ","))
	} else {
		for _, term := range q.Terms {
			v.Add("term", term)
		}
	}
	return "/facets/works/feed?" + v.Encode()
}

// renderTimelineCards turns a finished page into Fragments.
func (h *Handler) renderTimelineCards(result timelineResult, viewer *model.User, surface string) []workCardFragment {
	return h.renderFinishedCards(result.Works, h.workCardCtx(viewer, surface))
}
