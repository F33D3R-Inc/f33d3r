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
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

const timelinePageSize = 20

var errBadTimelineCursor = errors.New("timeline: bad cursor")

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

	Handle  string // the person a profile surface is about
	Profile *model.User
	ListID  string
	Filter  string // a bookmark filter
	Tag     string
	Terms   []string // words to find, on surfaces that search
	Def     *model.FeedSurface
}

// timelineSpec is a surface: its parts, never its order.
type timelineSpec struct {
	Name string
	// Ranked surfaces read a wider window and let AethyrRank choose the page from it.
	Ranked bool
	// KeepMuted is for a page about one person.
	KeepMuted bool
	// Paged says a later page exists to ask for.
	Paged      bool
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
	read, err := spec.Candidates(h, q, window)
	if err != nil {
		timelineFailures.WithLabelValues(spec.Name).Inc()
		return timelineResult{}, nil, err
	}

	// filter
	shown := h.gateWorksWith(q.Viewer, read, gateOptions{keepMuted: spec.KeepMuted})

	// rank
	if spec.Ranked && len(shown) > 0 {
		rankSurface := spec.Name
		if q.Def != nil {
			rankSurface = q.Def.ID
		}
		shown = h.rankWorks(ctx, q.Viewer, rankSurface, q.SessionID, shown, q.Limit)
	}

	// enrich
	h.enrichWorks(shown, q.Viewer)
	h.gateQuotes(q.Viewer, shown)

	// measure
	timelineItems.WithLabelValues(spec.Name, "read").Add(float64(len(read)))
	timelineItems.WithLabelValues(spec.Name, "shown").Add(float64(len(shown)))
	timelineSeconds.WithLabelValues(spec.Name).Observe(time.Since(started).Seconds())

	return timelineResult{
		Works: shown,
		Next:  timelineNext(spec, read, window),
	}, nil, nil
}

// timelineNext says where the next page starts. It comes from what was read, not what
// was shown, so works withheld from this viewer can never end or repeat their feed. A
// ranked page starts below its whole window, because the ranker chose from all of it.
func timelineNext(spec *timelineSpec, read []*model.Work, window int) string {
	if !spec.Paged || len(read) < window {
		return ""
	}
	cursor := feedTime(read[len(read)-1])
	if spec.Ranked {
		for _, w := range read {
			if t := feedTime(w); t.Before(cursor) {
				cursor = t
			}
		}
	}
	return cursor.Format(time.RFC3339Nano)
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

func timelineCursorTime(before string) (time.Time, error) {
	t, ok := apiCursor(before)
	if !ok {
		return time.Time{}, errBadTimelineCursor
	}
	return t, nil
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
		Empty: timelineEmpty{"Nothing here yet", "Post something to get started"},
		Candidates: func(h *Handler, q *timelineQuery, n int) ([]*model.Work, error) {
			return dbpkg.GetWorksForYou(h.db, n, q.Before)
		},
	}
	specTrending = &timelineSpec{
		Name: "trending", Ranked: true, Paged: true,
		Provenance: func(*model.User) func(*model.Work) *ProvenanceDTO { return apiTrendingProvenance() },
		Empty:      timelineEmpty{"No trending works", "Check back soon"},
		Candidates: func(h *Handler, q *timelineQuery, n int) ([]*model.Work, error) {
			return dbpkg.GetWorksTrending(h.db, n, q.Before)
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
	// specTopics reads several tags at once and has no later page: it is a set, not a stream.
	specTopics = &timelineSpec{
		Name: "topics", Ranked: true,
		Empty: timelineEmpty{"No results", "Try a different topic"},
		Candidates: func(h *Handler, q *timelineQuery, n int) ([]*model.Work, error) {
			if len(q.Terms) == 0 {
				return dbpkg.GetWorksForYou(h.db, n, q.Before)
			}
			return searchEachTerm(h, q, n/4)
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
			before, err := timelineCursorTime(q.Before)
			if err != nil {
				return nil, err
			}
			return dbpkg.GetWorkRepliesForProfile(h.db, q.Profile.PIALID, n, before)
		},
	}
	specProfileMedia = &timelineSpec{
		Name: "profile_media", KeepMuted: true, Paged: true, Prepare: prepareProfile,
		Empty: timelineEmpty{"No media yet", ""},
		Candidates: func(h *Handler, q *timelineQuery, n int) ([]*model.Work, error) {
			before, err := timelineCursorTime(q.Before)
			if err != nil {
				return nil, err
			}
			return dbpkg.GetWorksForProfileMedia(h.db, q.Profile.PIALID, n, before)
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
			before, err := timelineCursorTime(q.Before)
			if err != nil {
				return nil, err
			}
			return dbpkg.GetWorksLikedByUserBefore(h.db, gateViewerID(q.Viewer), n, before)
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
		Name:  "bookmarks",
		Empty: timelineEmpty{"No bookmarks yet", "Works you save will appear here"},
		Candidates: func(h *Handler, q *timelineQuery, n int) ([]*model.Work, error) {
			return dbpkg.GetWorksSavesFiltered(h.db, gateViewerID(q.Viewer), n, "", q.Filter)
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
		Name: "search",
		Candidates: func(h *Handler, q *timelineQuery, n int) ([]*model.Work, error) {
			return searchEachTerm(h, q, n)
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

// searchEachTerm finds each term, keeping each work once.
func searchEachTerm(h *Handler, q *timelineQuery, perTerm int) ([]*model.Work, error) {
	seen := map[string]bool{}
	var works []*model.Work
	var lastErr error
	failed := 0
	for _, term := range q.Terms {
		found, err := dbpkg.SearchWorks(h.db, term, perTerm, gateViewerID(q.Viewer))
		if err != nil {
			log.Printf("[timeline] search %q: %v", term, err)
			lastErr, failed = err, failed+1
			continue
		}
		for _, w := range found {
			if !seen[w.ID] {
				seen[w.ID] = true
				works = append(works, w)
			}
		}
	}
	if failed > 0 && failed == len(q.Terms) {
		return nil, lastErr
	}
	return works, nil
}

// readSurface runs a page-rendered surface and logs a failure. The page still draws;
// showing the failure honestly belongs to the error Facet.
func (h *Handler) readSurface(ctx context.Context, spec *timelineSpec, q *timelineQuery) timelineResult {
	result, _, err := h.runTimeline(ctx, spec, q)
	if err != nil {
		log.Printf("[timeline] %s: %v", spec.Name, err)
	}
	return result
}

// renderTimelineCards turns a finished page into Fragments.
func (h *Handler) renderTimelineCards(result timelineResult, viewer *model.User, surface string) []workCardFragment {
	return h.renderFinishedCards(result.Works, h.workCardCtx(viewer, surface))
}
