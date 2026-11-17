package handler

// sports.go — the sports lane's shared half: the atomic score, clock and
// situation Facets every game surface is built from, the poller's mutation
// sink, and the native Shell's delivery. The game page lives in
// sports_game.go; every other surface a game is drawn on lives in
// sports_hub.go.
//
// The doctrine, in order:
//
//  1. Triggering server event — the platform poller in internal/sports swaps
//     its scoreboard snapshot, and the per-game poller (gamecast) takes a new
//     reading of a game somebody has open.
//  2. Server-side handler — sportsMutation (scoreboard) and
//     sportsGameMutation (one game, sports_game.go) diff the two readings.
//  3. Facet renderer — the matching template, executed into a buffer.
//  4. Produced fragment — one complete fragment per Facet, its root carrying
//     data-facet-id.
//  5. Stream mutation — facet.mutate, sent only to the people watching the
//     surface the Facet belongs to. The native Android Shell, which applies
//     fragments by the event name sports_facet and registers no watches,
//     receives the scoreboard Facets on that name, on native streams only.
//
// Atomic Facets shared by every surface that draws one game's numbers:
//
//	sports_game_score     facet:f33d3r:sports:<game_id>:score_away
//	sports_game_score     facet:f33d3r:sports:<game_id>:score_home
//	sports_game_clock     facet:f33d3r:sports:<game_id>:clock
//	sports_game_situation facet:f33d3r:sports:<game_id>:situation

import (
	"bytes"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/f33d3r/feed-engine/internal/sports"
)

// ── Facet identity ───────────────────────────────────────────────────────────

const sportsFacetNS = "facet:f33d3r:sports:"

func sportsClockFacetID(gameID string) string { return sportsFacetNS + gameID + ":clock" }
func sportsScoreFacetID(gameID, side string) string {
	return sportsFacetNS + gameID + ":score_" + side
}
func sportsSituationFacetID(gameID string) string { return sportsFacetNS + gameID + ":situation" }

// sseSportsFacet is the event name the native Android Shell applies sports
// fragments under (mobile/android FaLive). The web runtime never receives it:
// the web is addressed by watch with facet.mutate.
const sseSportsFacet = "sports_facet"

// ── Facet data ───────────────────────────────────────────────────────────────

// SportsCardData is one game as a row or a clock renders it: the game, where it
// lives, and whether its league's feed has gone quiet. The rail's rows and the
// clock atom render from it.
type SportsCardData struct {
	Game  sports.Game
	Href  string
	Stale bool
}

// sportsStaleLabel says how old a set of leagues' snapshots is, in words, or ""
// when they are current.
//
// It is asked about the leagues actually on screen rather than about the cache
// as a whole. A basketball feed that has gone quiet must not put a "delayed"
// notice over a rail whose every row is a football game read four seconds ago,
// and the league that IS behind has to be named or the notice cannot be acted
// on. Silently presenting a frozen score as live is the one failure mode a
// scoreboard may not have; blaming the wrong league for it is the second.
func (h *Handler) sportsStaleLabel(slugs ...string) (bool, string) {
	seen := map[string]bool{}
	var parts []string
	for _, slug := range slugs {
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true
		lastOK, _, _, stale := h.sportsCache.LeagueHealth(slug)
		if !stale {
			continue
		}
		label := sports.LeagueOf(slug).Label
		if label == "" {
			label = strings.ToUpper(slug)
		}
		if lastOK.IsZero() {
			parts = append(parts, label+": no update received yet")
			continue
		}
		parts = append(parts, label+": last update "+TimeAgo(lastOK))
	}
	if len(parts) == 0 {
		return false, ""
	}
	return true, strings.Join(parts, "; ")
}

// sportsCardsFrom turns a slate into card data, marking each card stale
// according to ITS OWN league's health.
func (h *Handler) sportsCardsFrom(games []sports.Game) []SportsCardData {
	staleByLeague := map[string]bool{}
	out := make([]SportsCardData, 0, len(games))
	for _, g := range games {
		stale, known := staleByLeague[g.League]
		if !known {
			stale = h.sportsCache.StaleLeague(g.League)
			staleByLeague[g.League] = stale
		}
		out = append(out, SportsCardData{Game: g, Href: sportsGameHref(g), Stale: stale})
	}
	return out
}

// safeGameIDRe bounds a game id to what an upstream event id can be, so a path
// segment can never reach the cache lookup as anything but an identifier.
var safeGameIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

func (h *Handler) sportsGame(id string) (sports.Game, bool) {
	if !h.sportsCache.Configured() || !safeGameIDRe.MatchString(id) {
		return sports.Game{}, false
	}
	return h.sportsCache.Game(id)
}

// sportsLeagueKnown gates every route on the leagues the platform actually
// polls. An unknown league is 204/404, never an empty scoreboard that implies
// the league exists and has no games today.
//
// It asks the cache's league table rather than its current snapshot, because
// those are different questions: "does this platform cover the NBA" is true all
// summer, and "does the NBA have a game on the board" is not. Answering the
// first with the second would make /sports/nba a 404 every offseason. It also
// replaces a string match on the provider's name, which only ever worked while
// there was exactly one provider.
func (h *Handler) sportsLeagueKnown(league string) bool {
	return h.sportsCache.HasLeague(league)
}

// ── Stream mutation ──────────────────────────────────────────────────────────

// renderSportsFacet executes one Facet template into a complete fragment. A
// render failure produces no event: a broken fragment on the wire corrupts the
// stream for every other lane sharing it.
func (h *Handler) renderSportsFacet(name string, data interface{}) (string, bool) {
	var buf bytes.Buffer
	if err := h.partial.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("[sports] render %s: %v", name, err)
		return "", false
	}
	frag := strings.TrimSpace(buf.String())
	if frag == "" {
		return "", false
	}
	return frag, true
}

// sportsMutation is the cache's mutation sink. It runs once per successful
// refresh, on the poller goroutine.
//
// The scoreboard surfaces — the league scoreboards, the Games tab, the
// timeline frames and the rail — are published by publishSportsSurfaces
// (sports_hub.go), each to its own watch. This half sends the game page's pill
// row to the pages showing it, and the native Shell its game-page numbers.
func (h *Handler) sportsMutation(prev, next []sports.Game) {
	h.publishSportsSurfaces(prev, next)
	sportsForgetGames(prev, next)

	prevByID := make(map[string]sports.Game, len(prev))
	for _, g := range prev {
		prevByID[g.ID] = g
	}
	pillsMoved := map[string]bool{}
	for _, slug := range leaguesWhoseSlateChanged(prev, next) {
		pillsMoved[slug] = true
	}
	for _, g := range next {
		old, existed := prevByID[g.ID]
		if existed && sportsPillKey(old) == sportsPillKey(g) {
			continue
		}
		pillsMoved[g.League] = true
		if !existed {
			continue
		}
		// The native Shell's game page draws the same score and clock atoms as
		// the web page's header. It cannot report that it has the page open, so
		// it is sent the atoms at scoreboard cadence.
		for _, side := range []string{"away", "home"} {
			if old.Side(side).Score != g.Side(side).Score || old.HasScore != g.HasScore {
				if frag, ok := h.renderSportsFacet("sports_game_score", sportsScoreData(g, side)); ok {
					deliverNative(SSEEvent{Type: sseSportsFacet, Data: frag})
				}
			}
		}
		if old.ClockKey() != g.ClockKey() {
			card := SportsCardData{Game: g, Href: sportsGameHref(g), Stale: h.sportsCache.StaleLeague(g.League)}
			if frag, ok := h.renderSportsFacet("sports_game_clock", card); ok {
				deliverNative(SSEEvent{Type: sseSportsFacet, Data: frag})
			}
		}
	}
	if len(pillsMoved) == 0 {
		return
	}
	// A game page's pill row is its league's slate less the game itself, so it
	// is re-sent to every open game page in a league whose pills moved.
	for _, id := range watchedKeysWithPrefix(sportsGameWatchID(sportsSurfacePage, "")) {
		g, ok := h.sportsGame(id)
		if !ok || !pillsMoved[g.League] {
			continue
		}
		pills := h.sportsPills(g)
		h.publishSportsGame(id, pills.FacetID(), "sports_gp_pills", pills)
	}
}

// SportsScoreData is the atomic score Facet's data: one side of one game.
type SportsScoreData struct {
	GameID string
	Side   string // "home" | "away"
	Team   sports.Team
	Score  int
	Show   bool // false before a game starts — a scheduled game has no score
	// Reason says why there is no score, so the spoken text is true of THIS
	// state rather than assuming every scoreless game is one waiting to start.
	Reason  string
	Leading bool
	// Changed is set on a fragment rendered because the score just moved: it
	// carries the class whose keyframes mark the change. A page render never
	// sets it, so a score is not announced as new on arrival.
	Changed bool
}

func sportsScoreData(g sports.Game, side string) SportsScoreData {
	d := SportsScoreData{GameID: g.ID, Side: side, Show: g.HasScore, Reason: g.NoScoreReason()}
	if side == "home" {
		d.Team, d.Score, d.Leading = g.Home.Team, g.Home.Score, g.HomeLeads()
	} else {
		d.Team, d.Score, d.Leading = g.Away.Team, g.Away.Score, g.AwayLeads()
	}
	return d
}

// ── Template helpers ─────────────────────────────────────────────────────────
//
// Registered on the funcMap so the Facet templates never compute an id by
// string-concatenating in markup, where a typo would silently produce a facet
// nothing can ever address.

func sportsFacetFuncs() map[string]interface{} {
	return map[string]interface{}{
		"sportsClockFacetID":     sportsClockFacetID,
		"sportsScoreFacetID":     sportsScoreFacetID,
		"sportsSituationFacetID": sportsSituationFacetID,
		"sportsScoreData":        sportsScoreData,
		"sportsMark":             sportsMark,
		"sportsTeamClass":        sportsTeamClass,
	}
}

// sportsHealthLine is the operator view of the lane: the platform total, then
// one segment per league carrying its slate size, its request count, its own
// cadence and how long ago its upstream answered.
//
// It is per league because the cadences are per league: a single aggregate
// number would hide the fact that a league between seasons is being asked
// twelve times a day while the one beside it is asked every twenty seconds,
// which is the whole property the poller exists to have.
func (h *Handler) sportsHealthLine() string {
	if !h.sportsCache.Configured() {
		return "sports: disabled (SPORTS_PROVIDER unset)"
	}
	_, _, fetches := h.sportsCache.Health()
	parts := []string{fmt.Sprintf("sports: provider=%s games=%d fetches=%d next_poll=%s",
		h.sportsCache.ProviderName(), len(h.sportsCache.Get(0)), fetches, h.sportsCache.Interval())}
	for _, l := range h.sportsCache.Leagues() {
		lastOK, lastErr, n, stale := h.sportsCache.LeagueHealth(l.Slug)
		errLabel := "none"
		if lastErr != nil {
			errLabel = lastErr.Error()
		}
		last := "never"
		if !lastOK.IsZero() {
			last = time.Since(lastOK).Truncate(time.Second).String() + " ago"
		}
		parts = append(parts, fmt.Sprintf("%s: games=%d fetches=%d cadence=%s last_ok=%s stale=%t last_error=%s",
			l.Slug, len(h.sportsCache.GetLeague(l.Slug, 0)), n,
			h.sportsCache.LeagueInterval(l.Slug), last, stale, errLabel))
	}
	return strings.Join(parts, " | ")
}
