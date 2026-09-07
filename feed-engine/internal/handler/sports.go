package handler

// sports.go — the game-card lane. Every league the platform polls, one lane.
//
// Shape of the lane, in the order the doctrine asks for it:
//
//  1. Triggering server event — the single platform poller in internal/sports
//     reads the upstream scoreboard and swaps its snapshot. No browser, no
//     viewer and no page ever triggers a read.
//  2. Server-side handler — sportsMutation below is handed the previous and the
//     next snapshot and decides which Facets actually changed.
//  3. Facet renderer — renderSportsFacet executes the matching template into a
//     buffer, server-side, exactly like every other Facet on this platform.
//  4. Produced fragment — a complete HTML snapshot whose single root carries
//     data-facet-id.
//  5. Stream mutation — the fragment goes out over FA Live as a `sports_facet`
//     event. The browser locates the matching data-facet-id and replaces it. It
//     is never told a score; it is handed a rendered scoreboard.
//
// Facet hierarchy:
//
//	shell_main_layout
//	 └─ playground
//	     └─ sports_strip              facet:f33d3r:sports:nfl:strip
//	     └─ sports_strip              facet:f33d3r:sports:nba:strip
//	         └─ sports_game_card      facet:f33d3r:sports:<game_id>:card
//	             ├─ sports_game_score facet:f33d3r:sports:<game_id>:score_away
//	             ├─ sports_game_score facet:f33d3r:sports:<game_id>:score_home
//	             ├─ sports_game_clock facet:f33d3r:sports:<game_id>:clock
//	             └─ sports_game_situation
//	                                  facet:f33d3r:sports:<game_id>:situation
//	 └─ sidebar_right
//	     └─ rail_scorecards           facet:f33d3r:sports:all:scorecards
//
// ONE STRIP PER LEAGUE, ONE RAIL FOR ALL OF THEM. The two surfaces answer two
// different questions and the split falls out of that:
//
//   - The head lane of the sports surface answers "what is my league doing".
//     It is per-league because the week label, the slate and the "All games"
//     link are all league facts, and because a league's own strip must not be
//     crowded out of existence by another league's busier night. Each strip
//     owns its own facet id, so a basketball slate changing re-renders the
//     basketball strip and nothing else.
//   - The right rail answers "what is on right now, and what is next". That
//     question is not asked per sport, so the rail is ONE ranked list across
//     every league — live first wherever it is being played, then whatever
//     starts soonest. Grouping it by league would put a game that tips off in
//     four hours above a game being played this second purely because its
//     header sorted first. League identity rides on the row instead.
//
// A ticking clock costs one small atomic fragment. A touchdown, a state change
// or a possession flip costs one card. A game joining or leaving a league's
// slate costs that league's strip. Nothing else is pushed, ever.

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/sports"
)

// ── Facet identity ───────────────────────────────────────────────────────────

const sportsFacetNS = "facet:f33d3r:sports:"

func sportsStripFacetID(league string) string { return sportsFacetNS + league + ":strip" }

// sportsRailFacetID addresses the rail panel.
//
// Its entity id is ALWAYS sportsAllLeagues, never the league that happens to
// own the top row. A facet id is an address, and this panel is one address: the
// platform's scoreboard rail. Keying it to the leading league would change the
// address the moment a basketball game outranked a football one, and the next
// fragment would arrive addressed to a facet the page no longer mounts — the
// panel would simply stop updating, silently, until the next full page load.
// What the panel SHOWS is league-dependent; where it lives is not.
func sportsRailFacetID(league string) string { return sportsFacetNS + league + ":scorecards" }

// sportsAllLeagues is the entity id of every cross-league surface: the rail and
// the /sports board.
const sportsAllLeagues = "all"

func sportsCardFacetID(gameID string) string  { return sportsFacetNS + gameID + ":card" }
func sportsClockFacetID(gameID string) string { return sportsFacetNS + gameID + ":clock" }
func sportsScoreFacetID(gameID, side string) string {
	return sportsFacetNS + gameID + ":score_" + side
}
func sportsSituationFacetID(gameID string) string { return sportsFacetNS + gameID + ":situation" }

// sseSportsFacet is the FA Live event name for every fragment this lane pushes.
const sseSportsFacet = "sports_facet"

// sportsStripLimit bounds ONE league's Playground head lane. The strip is a
// head lane, not the surface: a bounded strip can never push the works stream
// off screen, and a slate longer than this belongs on the hub page, which is
// one tap away. It is per league rather than per slate so a fourteen-game NFL
// Sunday cannot leave the NBA strip empty on the same surface.
const sportsStripLimit = 8

// sportsRailLimit bounds the right-rail panel, across every league at once.
//
// THE NUMBER IS NOT THIS FILE'S TO CHOOSE. It is this lane's declared share of
// the rail's item budget, and it is taken from the one place that sizes the rail
// — railManifest in rail_budget.go, where the six and the argument for it are
// written down beside every other panel's. A constant here is exactly how the
// rail came to be twenty-two rows long: five files each picking a number that
// was defensible alone and unbounded together. This lane still holds a higher
// ceiling than the rail's default of three; it simply has to hold it where the
// rail can see it.
//
// What the cap means here has not changed. It is PURELY RANKED — no league is
// guaranteed a row. It is not divided per league, and no league is held a seat,
// because a reserved seat is a promise to show a basketball game that tips off
// in four hours instead of a football game being played this second. The
// ranking already gives a league every row it deserves: if only one league is
// playing, that league is what is on. "All scores" in the panel head is the
// escape hatch to the full board, and it is the reason this panel can afford a
// ceiling the others cannot.
var sportsRailLimit = railCap(railPanelScores)

// sportsSurfaceID is the feed_surfaces row this lane belongs to. The slate is
// the head lane of that surface's Playground and appears in the centre column
// nowhere else, so a viewer on Home sees the games once — in the rail — and a
// viewer who has chosen the Sports feed sees the full cards where they are the
// point of the surface.
const sportsSurfaceID = "sports"

// ── Facet data ───────────────────────────────────────────────────────────────

// SportsCardData is what one game card Facet renders from. It is self-contained
// so the identical struct serves a page render, an HTMX fetch and a stream
// mutation — there is exactly one renderer for a card.
type SportsCardData struct {
	Game  sports.Game
	Href  string
	Stale bool
}

// SportsStripData is ONE league's Playground head lane.
type SportsStripData struct {
	LeagueLabel string
	LeagueSlug  string
	WeekLabel   string
	HubHref     string
	Cards       []SportsCardData
	Stale       bool
	StaleLabel  string
	FacetID     string
}

// SportsRailData is the right-rail panel: one ranked list of what is on now and
// what is on next, across every league the platform polls.
type SportsRailData struct {
	// Title is the panel head. It is a league when every row is that league —
	// with its week, where the league has one — and the neutral "Scores" when
	// the rows are mixed, because a panel headed "NFL" with a basketball game in
	// it would be lying about half of itself.
	Title string
	// Subtitle carries a league's week when the panel is single-league. Empty
	// otherwise; a mixed panel has no week to speak of.
	Subtitle string
	// HubHref is where "Scores" goes: the league's own board when the panel is
	// single-league, the cross-league board when it is mixed.
	HubHref string
	// MixedLeagues is true when the rows span more than one league. It is what
	// turns on the per-row league tag: with every row in one league, and that
	// league named in the head, a tag repeated down the column would be six
	// copies of something the reader already knows.
	MixedLeagues bool
	Cards        []SportsCardData
	Stale        bool
	StaleLabel   string
	FacetID      string
}

// SportsGroupData is one league's section of the cross-league board.
type SportsGroupData struct {
	LeagueLabel string
	LeagueSlug  string
	WeekLabel   string
	HubHref     string
	Cards       []SportsCardData
}

func sportsGameHref(g sports.Game) string {
	return "/sports/" + g.League + "/" + g.ID
}

func sportsHubHref(league string) string { return "/sports/" + league }

// sportsIndexHref is the cross-league board — every league on one page.
const sportsIndexHref = "/sports"

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

// sportsLeagueSlugs is the distinct set of leagues a card set draws from, in the
// order they first appear.
func sportsLeagueSlugs(cards []SportsCardData) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range cards {
		if seen[c.Game.League] {
			continue
		}
		seen[c.Game.League] = true
		out = append(out, c.Game.League)
	}
	return out
}

// sportsStripsData builds one head lane per league that has games, ordered so
// that a league with something being played sits above a league that does not.
// An empty slice means the surface renders no markup at all — no scaffolding,
// no empty panel, no layout shift, and nothing at all for a league that is
// between seasons.
func (h *Handler) sportsStripsData() []*SportsStripData {
	if !h.sportsCache.Configured() {
		return nil
	}
	var out []*SportsStripData
	for _, l := range h.sportsCache.Leagues() {
		games := h.sportsCache.GetLeague(l.Slug, sportsStripLimit)
		if len(games) == 0 {
			// A league with nothing on contributes nothing: no strip, no header,
			// no empty state. It is not playing, and the surface says so by not
			// mentioning it.
			continue
		}
		cards := h.sportsCardsFrom(games)
		stale, label := h.sportsStaleLabel(l.Slug)
		out = append(out, &SportsStripData{
			LeagueLabel: l.Label,
			LeagueSlug:  l.Slug,
			WeekLabel:   games[0].WeekLabel,
			HubHref:     sportsHubHref(l.Slug),
			Cards:       cards,
			Stale:       stale,
			StaleLabel:  label,
			FacetID:     sportsStripFacetID(l.Slug),
		})
	}
	// A league with a game being played leads. Both leagues idle keeps the
	// table's own order, so the surface does not reshuffle between renders.
	sort.SliceStable(out, func(i, j int) bool {
		return sportsStripRank(out[i]) < sportsStripRank(out[j])
	})
	return out
}

// sportsStripRank is 0 for a league with something being played and 1 otherwise.
func sportsStripRank(s *SportsStripData) int {
	for _, c := range s.Cards {
		if c.Game.Live() {
			return 0
		}
	}
	return 1
}

// sportsRailData builds the right-rail panel, or nil when there is nothing to
// show anywhere.
//
// Nil is the empty state, and it is deliberate: when no league has a game being
// played and none has one coming, the rail renders NO PANEL — not a panel
// saying nothing is on. A rail is a column of things worth glancing at, and a
// box that exists to report its own emptiness is worth less than the space it
// takes. The same rule retires an out-of-season league from the panel without
// any per-league scaffolding to leave behind.
//
// Called from railData on every page that carries the rail.
func (h *Handler) sportsRailData() *SportsRailData {
	if !h.sportsCache.Configured() {
		return nil
	}
	// The cache already orders the merged snapshot live-first, then by whatever
	// starts soonest, with deterministic ties. Taking the head of it IS the
	// ranking; there is no second ordering here to drift from it.
	cards := h.sportsCardsFrom(h.sportsCache.Get(sportsRailLimit))
	if len(cards) == 0 {
		return nil
	}
	slugs := sportsLeagueSlugs(cards)
	stale, label := h.sportsStaleLabel(slugs...)

	data := &SportsRailData{
		Cards:      cards,
		Stale:      stale,
		StaleLabel: label,
	}
	data.FacetID = sportsRailFacetID(sportsAllLeagues)
	if len(slugs) == 1 {
		// Every row is one league: name it in the head, with its week where it
		// has one, and let the rows stay clean.
		data.Title = cards[0].Game.LeagueLabel
		data.Subtitle = cards[0].Game.WeekLabel
		data.HubHref = sportsHubHref(slugs[0])
		return data
	}
	// Mixed: a neutral head, per-row league tags, and a link to the board that
	// carries every league rather than to whichever one sorted first.
	data.Title = "Scores"
	data.MixedLeagues = true
	data.HubHref = sportsIndexHref
	return data
}

// ── Facet endpoints ──────────────────────────────────────────────────────────

// facetSportsStrip — GET /facets/sports/{league}/strip
// One league's Playground head lane. Writes nothing when the lane is off or
// that league has no games, so a surface with no basketball on it carries no
// basketball markup.
func (h *Handler) facetSportsStrip(w http.ResponseWriter, r *http.Request) {
	league := strings.ToLower(strings.TrimSpace(r.PathValue("league")))
	if !h.sportsLeagueKnown(league) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	for _, data := range h.sportsStripsData() {
		if data.LeagueSlug != league {
			continue
		}
		w.Header().Set("Cache-Control", "no-store")
		h.renderPartial(w, "sports_strip", data)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
}

// facetSportsCard — GET /facets/sports/{league}/card?game=<id>
// One game card. Serves the game page's initial paint and gives an operator a
// way to read the exact fragment the stream would push for that game.
func (h *Handler) facetSportsCard(w http.ResponseWriter, r *http.Request) {
	if !h.sportsLeagueKnown(r.PathValue("league")) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("game"))
	g, ok := h.sportsGame(id)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	h.renderPartial(w, "sports_game_card", SportsCardData{
		Game:  g,
		Href:  sportsGameHref(g),
		Stale: h.sportsCache.Stale(),
	})
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

// ── Pages ────────────────────────────────────────────────────────────────────

// sportsHubPage — GET /sports and GET /sports/{league}
//
// The durable destination for the board. A score in a post scrolls away; this
// page is the address the games always live at, and it is where "Scores" in the
// rail goes.
//
// One handler serves both because they are one page with one section per
// league: /sports/nba is that page filtered to a league. The cross-league form
// is what a mixed rail must link to — sending "Scores" to whichever league
// happened to own the top row would answer a question the reader did not ask.
//
// A league with nothing on renders no section at all, so the board of a summer
// evening is an empty state and not two empty headers.
func (h *Handler) sportsHubPage(w http.ResponseWriter, r *http.Request) {
	league := strings.ToLower(strings.TrimSpace(r.PathValue("league")))
	if league != "" && !h.sportsLeagueKnown(league) {
		http.NotFound(w, r)
		return
	}
	user := h.userFromRequest(w, r)

	groups, slugs, _ := h.sportsBoard(league)
	stale, staleLabel := h.sportsStaleLabel(slugs...)

	title, week := "Scores", ""
	if league != "" {
		title = sports.LeagueOf(league).Label
		if title == "" {
			title = strings.ToUpper(league)
		}
		title += " scores"
		if len(groups) > 0 {
			week = groups[0].WeekLabel
		}
	}
	h.render(w, r, "sports_hub.html", h.withRail(map[string]interface{}{
		"User":              user,
		"LeagueSlug":        league,
		"HeadTitle":         title,
		"WeekLabel":         week,
		"Groups":            groups,
		"Stale":             stale,
		"StaleLabel":        staleLabel,
		"Title":             title + " · F33D3R",
		"SessionID":         uuid.New().String(),
		"CurrentUserHandle": user.Handle,
		"Themes":            ThemesWithActive(user.ThemeID),
	}, user, "sports"))
}

// sportsBoard builds the board's league sections, the leagues it covers and the
// total game count. A league the platform polls always contributes its slug —
// so its staleness is reported even when it has nothing on — but a league with
// no games contributes no section, because a header over nothing is
// scaffolding, not information.
func (h *Handler) sportsBoard(league string) (groups []SportsGroupData, slugs []string, total int) {
	for _, l := range h.sportsCache.Leagues() {
		if league != "" && l.Slug != league {
			continue
		}
		slugs = append(slugs, l.Slug)
		games := h.sportsCache.GetLeague(l.Slug, 0)
		if len(games) == 0 {
			continue
		}
		total += len(games)
		groups = append(groups, SportsGroupData{
			LeagueLabel: l.Label,
			LeagueSlug:  l.Slug,
			WeekLabel:   games[0].WeekLabel,
			HubHref:     sportsHubHref(l.Slug),
			Cards:       h.sportsCardsFrom(games),
		})
	}
	return groups, slugs, total
}

// sportsGamePage — GET /sports/{league}/{game}
// One game as a durable destination: the card, and the conversation about it.
// This is the same shape as /stocks/{ticker} — an external-data card at the head
// and the platform's own works underneath — because it is the same problem.
func (h *Handler) sportsGamePage(w http.ResponseWriter, r *http.Request) {
	league := strings.ToLower(strings.TrimSpace(r.PathValue("league")))
	if !h.sportsLeagueKnown(league) {
		http.NotFound(w, r)
		return
	}
	g, ok := h.sportsGame(r.PathValue("game"))
	if !ok || g.League != league {
		http.NotFound(w, r)
		return
	}
	user := h.userFromRequest(w, r)

	var works interface{}
	if h.db != nil {
		// The conversation about a game is the works that name either club. The
		// query is the club nickname ("Seahawks"), which is what people write.
		seen := map[string]bool{}
		var merged []*model.Work
		for _, term := range sportsSearchTerms(g) {
			if term == "" || seen[term] {
				continue
			}
			seen[term] = true
			wks, sErr := dbpkg.SearchWorks(h.db, term, 20, user.ID)
			if sErr != nil {
				log.Printf("[sports] search works %q: %v", term, sErr)
				continue
			}
			merged = append(merged, wks...)
		}
		works = filterAdultForViewer(user, dedupeWorkRows(merged))
	}

	h.render(w, r, "sports_game.html", h.withRail(map[string]interface{}{
		"User":       user,
		"LeagueSlug": league,
		"Game":       g,
		"Card": SportsCardData{
			Game:  g,
			Href:  sportsGameHref(g),
			Stale: h.sportsCache.Stale(),
		},
		"Works":             works,
		"Title":             g.Matchup() + " · " + g.LeagueLabel + " · F33D3R",
		"SessionID":         uuid.New().String(),
		"CurrentUserHandle": user.Handle,
		"Themes":            ThemesWithActive(user.ThemeID),
	}, user, "sports"))
}

// sportsSearchTerms is what the game page searches works for.
func sportsSearchTerms(g sports.Game) []string {
	return []string{
		strings.TrimSpace(g.Home.Team.ShortName),
		strings.TrimSpace(g.Away.Team.ShortName),
	}
}

// dedupeWorkRows keeps the first occurrence of each work across several
// searches, preserving relevance order within each term.
func dedupeWorkRows(rows []*model.Work) []*model.Work {
	seen := make(map[string]bool, len(rows))
	out := make([]*model.Work, 0, len(rows))
	for _, w := range rows {
		if w == nil || seen[w.ID] {
			continue
		}
		seen[w.ID] = true
		out = append(out, w)
	}
	return out
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

// publishSportsFacet puts one rendered fragment on FA Live.
//
// Delivery is a platform broadcast, not a per-viewer subscription, because a
// score is public platform data identical for every viewer — the same reason
// leaderboard_refresh broadcasts. A session whose page does not mount the facet
// finds no matching data-facet-id and does nothing; no browser holds any state
// about which games it is following, and no viewer causes an upstream request.
func publishSportsFacet(fragment string) {
	PublishToAllSessions(SSEEvent{Type: sseSportsFacet, Data: fragment})
}

// sportsMutation is the cache's mutation sink. It runs once per successful
// refresh, on the poller goroutine, and emits one event per changed Facet.
func (h *Handler) sportsMutation(prev, next []sports.Game) {
	prevByID := make(map[string]sports.Game, len(prev))
	for _, g := range prev {
		prevByID[g.ID] = g
	}

	// Membership and order are tracked PER LEAGUE, because each league owns its
	// own head lane. A basketball game joining the board re-renders the
	// basketball strip; the football strip beside it is not touched, and its
	// cards are not re-sent.
	railTouched := sports.SlateKey(prev) != sports.SlateKey(next)
	changedLeagues := map[string]bool{}
	for _, slug := range leaguesWhoseSlateChanged(prev, next) {
		changedLeagues[slug] = true
	}
	if len(changedLeagues) > 0 {
		for _, data := range h.sportsStripsData() {
			if !changedLeagues[data.LeagueSlug] {
				continue
			}
			if frag, ok := h.renderSportsFacet("sports_strip", data); ok {
				publishSportsFacet(frag)
			}
		}
	}

	railWindow := sportsRailLimit
	if railWindow > len(next) {
		railWindow = len(next)
	}

	for i, g := range next {
		old, existed := prevByID[g.ID]
		if !existed {
			// New to the board; the strip re-render above already carries it.
			continue
		}
		if old.CardKey() == g.CardKey() {
			continue
		}
		if i < railWindow {
			railTouched = true
		}

		// Staleness is this game's league's, not the platform's: a card must not
		// wear a "delayed feed" mark because a different sport went quiet.
		stale := h.sportsCache.StaleLeague(g.League)
		card := SportsCardData{Game: g, Href: sportsGameHref(g), Stale: stale}

		// Anything structural — a state transition, a possession flip, the red
		// zone, a corrected record, a new broadcast — re-renders the card, once.
		structural := old.State != g.State ||
			old.SituationKey() != g.SituationKey() ||
			old.Home.Team.Record != g.Home.Team.Record ||
			old.Away.Team.Record != g.Away.Team.Record ||
			old.Broadcast != g.Broadcast ||
			old.HasScore != g.HasScore
		if structural {
			if frag, ok := h.renderSportsFacet("sports_game_card", card); ok {
				publishSportsFacet(frag)
			}
			continue
		}

		// Otherwise only the numbers moved: emit the smallest Facets that hold
		// them. This is the common case during a live game and it is why a
		// ticking clock does not re-send a whole card every twenty seconds.
		if old.Away.Score != g.Away.Score {
			if frag, ok := h.renderSportsFacet("sports_game_score", sportsScoreData(g, "away")); ok {
				publishSportsFacet(frag)
			}
		}
		if old.Home.Score != g.Home.Score {
			if frag, ok := h.renderSportsFacet("sports_game_score", sportsScoreData(g, "home")); ok {
				publishSportsFacet(frag)
			}
		}
		if old.ClockKey() != g.ClockKey() {
			if frag, ok := h.renderSportsFacet("sports_game_clock", card); ok {
				publishSportsFacet(frag)
			}
		}
	}

	if railTouched {
		if data := h.sportsRailData(); data != nil {
			if frag, ok := h.renderSportsFacet("rail_scorecards", data); ok {
				publishSportsFacet(frag)
			}
		}
	}
}

// leaguesWhoseSlateChanged names the leagues whose membership or order moved
// between two snapshots. Comparing the merged board would report every league
// as changed whenever any one of them did, and re-send a strip that is
// byte-identical to the one already on the page.
func leaguesWhoseSlateChanged(prev, next []sports.Game) []string {
	byLeague := func(games []sports.Game) map[string][]sports.Game {
		out := map[string][]sports.Game{}
		for _, g := range games {
			out[g.League] = append(out[g.League], g)
		}
		return out
	}
	before, after := byLeague(prev), byLeague(next)
	seen := map[string]bool{}
	var out []string
	for slug := range after {
		if sports.SlateKey(before[slug]) != sports.SlateKey(after[slug]) {
			seen[slug] = true
			out = append(out, slug)
		}
	}
	for slug := range before {
		if !seen[slug] && sports.SlateKey(before[slug]) != sports.SlateKey(after[slug]) {
			out = append(out, slug)
		}
	}
	sort.Strings(out)
	return out
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
		"sportsCardFacetID":      sportsCardFacetID,
		"sportsClockFacetID":     sportsClockFacetID,
		"sportsScoreFacetID":     sportsScoreFacetID,
		"sportsSituationFacetID": sportsSituationFacetID,
		"sportsScoreData":        sportsScoreData,
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
