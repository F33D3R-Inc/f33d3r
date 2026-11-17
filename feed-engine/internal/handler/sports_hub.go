package handler

// sports_hub.go — every place a league's games are shown outside the game page.
//
// Four surfaces, one set of Facets:
//
//   - the league hub, /sports/{league}: a league header, the tabs Home · Games ·
//     Standings · Players · Media, the league's scoreboard and its posts;
//   - the cross-league board, /sports: one scoreboard per league;
//   - Home's topic tabs: a tab per league the viewer follows, opening that
//     league's timeline with its scoreboard pinned on top;
//   - frames in Home's ranked timelines: the scoreboard pinned at the top of the
//     first page, and a single-game card every seventh work that rotates through
//     the league's games.
//
// The frames follow the plan X's home mixer uses for its NFL timeline
// (home-mixer/frames): a pinned scoreboard at rank 0 on the first response only,
// a rotating card every seven items that resumes from the viewer's served
// history, a hard cap per response, and a frame that fails is dropped while the
// page is served. They are shown only to a viewer who follows the league's topic.
//
// The doctrine, in order:
//
//  1. Triggering server event — the platform poller in internal/sports swaps a
//     snapshot and calls the mutation sink.
//  2. Server-side handler — publishSportsSurfaces decides which of these Facets
//     changed between the two snapshots.
//  3. Facet renderer — the sports_scoreboard, sports_games, sports_tile,
//     sports_frame and sports_live_* templates, executed on the server.
//  4. Produced fragment — one complete fragment per Facet, its root carrying
//     data-facet-id.
//  5. Stream mutation — a facet.mutate sent only to the people who have that
//     Facet on screen. Each Facet carries a data-watch-id; the runtime reports
//     it while it is in view, and the mutation is published to that watch.
//
// Facet hierarchy:
//
//	sports_hub page
//	 ├─ sports_hub_tabs           facet:f33d3r:sports:<league>:hub_tabs
//	 └─ sports_hub_canvas         facet:f33d3r:sports:<league>:hub_<tab>
//	     ├─ sports_scoreboard     facet:f33d3r:sports:<league>:scoreboard   (home)
//	     ├─ sports_games          facet:f33d3r:sports:<league>:games        (games)
//	     └─ sports_hub_soon       (standings, players, media: inside the canvas)
//
//	sports_scoreboard / sports_games
//	 └─ sports_tile               facet:f33d3r:sports:<game>:tile
//	     ├─ sports_live_clock     facet:f33d3r:sports:<game>:tile_clock
//	     ├─ sports_live_score     facet:f33d3r:sports:<game>:tile_score_away
//	     ├─ sports_live_score     facet:f33d3r:sports:<game>:tile_score_home
//	     └─ sports_live_audience  facet:f33d3r:sports:<game>:tile_audience
//
//	playground (Home, ranked timelines)
//	 ├─ sports_scoreboard         pinned, first page only
//	 └─ sports_frame              facet:f33d3r:sports:<game>:frame
//	     └─ sports_live_*         facet:f33d3r:sports:<game>:frame_{clock,score_away,score_home,audience}
//
//	sidebar_right
//	 └─ rail_scorecards           facet:f33d3r:sports:all:scorecards
//
// A tile and a frame of the same game are separate Facets with separate watches,
// because they are separate surfaces: a pinned scoreboard and the rotating card
// can both be on one page, and scrolling one of them out of view must not stop
// the other from updating.

import (
	"bytes"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/sports"
)

// ── Facet identity ───────────────────────────────────────────────────────────

// The two shapes a single game is drawn in outside its own page.
const (
	sportsSurfaceTile  = "tile"  // a card in a scoreboard row or the games grid
	sportsSurfaceFrame = "frame" // the rotating single-game card in a timeline
)

func sportsScoreboardFacetID(league string) string { return sportsFacetNS + league + ":scoreboard" }
func sportsGamesFacetID(league string) string      { return sportsFacetNS + league + ":games" }
func sportsHubTabsFacetID(league string) string    { return sportsFacetNS + league + ":hub_tabs" }
func sportsHubCanvasFacetID(league, tab string) string {
	return sportsFacetNS + league + ":hub_" + tab
}

// sportsLiveFacetID addresses a tile or frame of one game, or one of its atomic
// parts: facet:f33d3r:sports:<game>:<surface>[_<part>].
func sportsLiveFacetID(gameID, surface, part string) string {
	if part == "" {
		return sportsFacetNS + gameID + ":" + surface
	}
	return sportsFacetNS + gameID + ":" + surface + "_" + part
}

// sportsRailFacetID addresses the rail panel. Its entity id is always
// sportsAllLeagues: the panel is one address whichever league leads it, so a
// fragment can never arrive addressed to a panel the page stopped mounting.
func sportsRailFacetID(league string) string { return sportsFacetNS + league + ":scorecards" }

// sportsAllLeagues is the entity id of every cross-league surface.
const sportsAllLeagues = "all"

// ── Watches ──────────────────────────────────────────────────────────────────
//
// A watch is what a Facet on screen reports through POST /api/events/watch.
// Every sports mutation is published to exactly one watch, so it reaches the
// people looking at that Facet and nobody else.

const sportsWatchPrefix = "sports:"

func sportsLeagueWatchID(league string) string { return sportsWatchPrefix + "league:" + league }
func sportsGameWatchID(surface, gameID string) string {
	return sportsWatchPrefix + surface + ":" + gameID
}

const sportsRailWatchID = sportsWatchPrefix + "rail"

// ── Placement ────────────────────────────────────────────────────────────────

// sportsScoreboardLimit bounds a scoreboard row. The row scrolls, but a
// fragment pushed to every watcher on each slate change should stay small; the
// whole slate is the hub's Games tab, linked from the row's head.
const sportsScoreboardLimit = 12

// sportsFrameInterval is how many works sit between rotating game cards, the
// interval X's NFL timeline uses.
const sportsFrameInterval = 7

// sportsRotationTTL is how long a viewer's place in the rotation is kept. The
// rotation resumes where it stopped across refreshes and sessions on the same
// day; a day later the slate itself has moved on.
const sportsRotationTTL = 24 * time.Hour

// sportsGameChatAnchor is the id of the chat on the game page. "Join chat"
// links there.
const sportsGameChatAnchor = "game-chat"

// sportsSurfaceID is the feed_surfaces row of the Sports interest surface. Its
// Home timeline carries every polled league's scoreboard, follow or not: a
// viewer who opened the Sports feed asked for exactly that.
const sportsSurfaceID = "sports"

func sportsGameHref(g sports.Game) string { return "/sports/" + g.League + "/" + g.ID }
func sportsHubHref(league string) string  { return "/sports/" + league }

// sportsIndexHref is the cross-league board.
const sportsIndexHref = "/sports"

// ── Facet data ───────────────────────────────────────────────────────────────

// SportsTileData is one game drawn as a tile or a frame. One struct serves a
// page render, a stream mutation and a resync, so there is one renderer per
// shape and they cannot disagree.
type SportsTileData struct {
	Game    sports.Game
	Surface string // sportsSurfaceTile | sportsSurfaceFrame
	Stale   bool   // this game's league feed has not answered recently
	// Audience is how many people have this game on screen, counted at the last
	// poll. Only a game being played has one.
	Audience int64
}

func (d SportsTileData) FacetID() string { return sportsLiveFacetID(d.Game.ID, d.Surface, "") }
func (d SportsTileData) WatchID() string { return sportsGameWatchID(d.Surface, d.Game.ID) }
func (d SportsTileData) Href() string    { return sportsGameHref(d.Game) }
func (d SportsTileData) ChatHref() string {
	return sportsGameHref(d.Game) + "?panel=chat#" + sportsGameChatAnchor
}

// Clock is the tile's atomic status line.
func (d SportsTileData) Clock() SportsLiveClockData {
	return SportsLiveClockData{FacetID: sportsLiveFacetID(d.Game.ID, d.Surface, "clock"), Game: d.Game, Stale: d.Stale}
}

// Score is one side's atomic score.
func (d SportsTileData) Score(side string) SportsLiveScoreData {
	g := d.Game
	s := SportsLiveScoreData{
		FacetID: sportsLiveFacetID(g.ID, d.Surface, "score_"+side),
		Show:    g.HasScore,
		Reason:  g.NoScoreReason(),
	}
	if side == "home" {
		s.Team, s.Score, s.Leading = g.Home.Team, g.Home.Score, g.HomeLeads()
	} else {
		s.Team, s.Score, s.Leading = g.Away.Team, g.Away.Score, g.AwayLeads()
	}
	return s
}

// AudienceFacet is the tile's atomic "watching" count.
func (d SportsTileData) AudienceFacet() SportsLiveAudienceData {
	return SportsLiveAudienceData{
		FacetID: sportsLiveFacetID(d.Game.ID, d.Surface, "audience"),
		Live:    d.Game.Live(),
		Count:   d.Audience,
	}
}

// SportsLiveClockData is the atomic status line of a tile or frame.
type SportsLiveClockData struct {
	FacetID string
	Game    sports.Game
	Stale   bool
}

// SportsLiveScoreData is one side's atomic score in a tile or frame.
type SportsLiveScoreData struct {
	FacetID string
	Team    sports.Team
	Score   int
	Show    bool   // false before a game starts: a scheduled game has no score
	Reason  string // why there is no score, in this state's own words
	Leading bool
	// Changed is set only on a pushed fragment whose number just moved. It puts
	// the score-change animation on the fragment; a page render never has it.
	Changed bool
}

// SportsLiveAudienceData is the atomic "watching" count.
type SportsLiveAudienceData struct {
	FacetID string
	Live    bool
	Count   int64
}

// SportsScoreboardData is one league's scoreboard row.
type SportsScoreboardData struct {
	League     sports.League
	WeekLabel  string
	Tiles      []SportsTileData
	More       bool // the slate is longer than the row
	Stale      bool
	StaleLabel string
}

func (d *SportsScoreboardData) FacetID() string   { return sportsScoreboardFacetID(d.League.Slug) }
func (d *SportsScoreboardData) WatchID() string   { return sportsLeagueWatchID(d.League.Slug) }
func (d *SportsScoreboardData) HubHref() string   { return sportsHubHref(d.League.Slug) }
func (d *SportsScoreboardData) GamesHref() string { return sportsHubHref(d.League.Slug) + "?tab=games" }

// SportsGamesSection is one group of the Games tab: live, upcoming or done.
type SportsGamesSection struct {
	Title string
	Tiles []SportsTileData
}

// SportsGamesData is the hub's Games tab: the whole slate, grouped.
type SportsGamesData struct {
	League     sports.League
	WeekLabel  string
	Sections   []SportsGamesSection
	Stale      bool
	StaleLabel string
}

func (d *SportsGamesData) FacetID() string { return sportsGamesFacetID(d.League.Slug) }
func (d *SportsGamesData) WatchID() string { return sportsLeagueWatchID(d.League.Slug) }

// SportsRailData is the right-rail panel: one ranked list of what is on now
// and what is on next, across every league the platform polls.
type SportsRailData struct {
	// Title is a league when every row is that league, "Scores" when mixed.
	Title string
	// Subtitle carries a single league's week.
	Subtitle string
	// HubHref is the league's hub when single-league, the board when mixed.
	HubHref string
	// MixedLeagues turns on the per-row league tag.
	MixedLeagues bool
	Cards        []SportsCardData
	Stale        bool
	StaleLabel   string
	FacetID      string
	WatchID      string
}

// sportsRailLimit is this panel's share of the rail's item budget, declared in
// railManifest beside every other panel's.
var sportsRailLimit = railCap(railPanelScores)

// ── Hub tabs ─────────────────────────────────────────────────────────────────

// sportsHubTab is one tab of a league hub.
type sportsHubTab struct {
	Key   string
	Label string
	// Soon is what the tab will hold, said plainly, for a tab whose data the
	// platform does not have yet. Empty for a tab that ships.
	Soon string
}

// sportsHubTabs is the tab row, in order. Standings, Players and Media say
// what they will hold rather than showing anything the platform cannot vouch
// for: the poller reads scoreboards only, and a standings table built from
// anything else would be invented.
var sportsHubTabs = []sportsHubTab{
	{Key: "home", Label: "Home"},
	{Key: "games", Label: "Games"},
	{Key: "standings", Label: "Standings", Soon: "Division and conference standings are on the way."},
	{Key: "players", Label: "Players", Soon: "Player pages and leaders are on the way."},
	{Key: "media", Label: "Media", Soon: "Highlights and clips from the league are on the way."},
}

// resolveSportsHubTab maps a requested tab to a known one; anything else is Home.
func resolveSportsHubTab(key string) sportsHubTab {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, t := range sportsHubTabs {
		if t.Key == key {
			return t
		}
	}
	return sportsHubTabs[0]
}

// SportsHubTabsData is the hub's tab row.
type SportsHubTabsData struct {
	League sports.League
	Active string
	Tabs   []sportsHubTab
	OOB    bool // appended out of band to a canvas answer
}

func (d SportsHubTabsData) FacetID() string { return sportsHubTabsFacetID(d.League.Slug) }

// HubHref is a tab's address: the page the tab shows, for history and links.
func (d SportsHubTabsData) HubHref(tab string) string {
	if tab == sportsHubTabs[0].Key {
		return sportsHubHref(d.League.Slug)
	}
	return sportsHubHref(d.League.Slug) + "?tab=" + tab
}

// CanvasHref is the Facet a tab asks for.
func (d SportsHubTabsData) CanvasHref(tab string) string {
	return "/facets/sports/" + d.League.Slug + "/hub?tab=" + tab
}

// SportsHubCanvasData is what one hub tab shows.
type SportsHubCanvasData struct {
	League     sports.League
	Tab        sportsHubTab
	Scoreboard *SportsScoreboardData // home
	Games      *SportsGamesData      // games
	FeedURL    string                // home: the league's posts
}

func (d SportsHubCanvasData) FacetID() string {
	return sportsHubCanvasFacetID(d.League.Slug, d.Tab.Key)
}

// ── Builders ─────────────────────────────────────────────────────────────────

// sportsLeague resolves a slug to a league the platform polls.
func (h *Handler) sportsLeague(slug string) (sports.League, bool) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	for _, l := range h.sportsCache.Leagues() {
		if l.Slug == slug {
			return l, true
		}
	}
	return sports.League{}, false
}

// sportsTiles draws a slate in one shape, each tile marked stale by its own
// league's health and carrying the audience counted at the last poll.
func (h *Handler) sportsTiles(games []sports.Game, surface string) []SportsTileData {
	staleByLeague := map[string]bool{}
	out := make([]SportsTileData, 0, len(games))
	for _, g := range games {
		stale, known := staleByLeague[g.League]
		if !known {
			stale = h.sportsCache.StaleLeague(g.League)
			staleByLeague[g.League] = stale
		}
		d := SportsTileData{Game: g, Surface: surface, Stale: stale}
		if g.Live() {
			d.Audience = sportsAudiences.count(g.ID)
		}
		out = append(out, d)
	}
	return out
}

// sportsTile draws one game in one shape.
func (h *Handler) sportsTile(g sports.Game, surface string) SportsTileData {
	return h.sportsTiles([]sports.Game{g}, surface)[0]
}

// sportsScoreboard builds a league's scoreboard row, or nil when the league has
// no games. Nil renders nothing: a league between seasons has no row.
func (h *Handler) sportsScoreboard(league sports.League) *SportsScoreboardData {
	games := h.sportsCache.GetLeague(league.Slug, 0)
	if len(games) == 0 {
		return nil
	}
	more := len(games) > sportsScoreboardLimit
	if more {
		games = games[:sportsScoreboardLimit]
	}
	stale, label := h.sportsStaleLabel(league.Slug)
	return &SportsScoreboardData{
		League:     league,
		WeekLabel:  games[0].WeekLabel,
		Tiles:      h.sportsTiles(games, sportsSurfaceTile),
		More:       more,
		Stale:      stale,
		StaleLabel: label,
	}
}

// sportsGames builds the Games tab: every game on the league's slate, grouped
// into what is being played, what is coming and what is done. A group with no
// games has no section.
func (h *Handler) sportsGames(league sports.League) *SportsGamesData {
	games := h.sportsCache.GetLeague(league.Slug, 0)
	var live, upcoming, done []sports.Game
	for _, g := range games {
		switch {
		case g.Live():
			live = append(live, g)
		case g.State.Settled():
			done = append(done, g)
		default:
			upcoming = append(upcoming, g)
		}
	}
	stale, label := h.sportsStaleLabel(league.Slug)
	d := &SportsGamesData{League: league, Stale: stale, StaleLabel: label}
	if len(games) > 0 {
		d.WeekLabel = games[0].WeekLabel
	}
	for _, s := range []struct {
		title string
		games []sports.Game
	}{{"Live now", live}, {"Upcoming", upcoming}, {"Completed", done}} {
		if len(s.games) > 0 {
			d.Sections = append(d.Sections, SportsGamesSection{Title: s.title, Tiles: h.sportsTiles(s.games, sportsSurfaceTile)})
		}
	}
	return d
}

// sportsHubCanvas builds what one tab of a league hub shows.
func (h *Handler) sportsHubCanvas(league sports.League, tab sportsHubTab) SportsHubCanvasData {
	d := SportsHubCanvasData{League: league, Tab: tab}
	switch tab.Key {
	case "home":
		d.Scoreboard = h.sportsScoreboard(league)
		d.FeedURL = "/facets/works/feed?" + url.Values{"surface": {timelineLeagueSurface(league.Slug)}}.Encode()
	case "games":
		d.Games = h.sportsGames(league)
	}
	return d
}

// sportsRailData builds the right-rail panel, or nil when no league has
// anything on: a rail panel that exists to report its own emptiness is worth
// less than the space it takes.
func (h *Handler) sportsRailData() *SportsRailData {
	if !h.sportsCache.Configured() {
		return nil
	}
	// The cache orders the merged snapshot live-first, then by start; its head
	// IS the ranking.
	cards := h.sportsCardsFrom(h.sportsCache.Get(sportsRailLimit))
	if len(cards) == 0 {
		return nil
	}
	var slugs []string
	seen := map[string]bool{}
	for _, c := range cards {
		if !seen[c.Game.League] {
			seen[c.Game.League] = true
			slugs = append(slugs, c.Game.League)
		}
	}
	stale, label := h.sportsStaleLabel(slugs...)
	data := &SportsRailData{
		Cards:      cards,
		Stale:      stale,
		StaleLabel: label,
		FacetID:    sportsRailFacetID(sportsAllLeagues),
		WatchID:    sportsRailWatchID,
	}
	if len(slugs) == 1 {
		data.Title = cards[0].Game.LeagueLabel
		data.Subtitle = cards[0].Game.WeekLabel
		data.HubHref = sportsHubHref(slugs[0])
		return data
	}
	data.Title = "Scores"
	data.MixedLeagues = true
	data.HubHref = sportsIndexHref
	return data
}

// ── Topics ───────────────────────────────────────────────────────────────────

// sportsFollowedLeagues is the leagues this viewer follows as topics, in the
// league table's order. A league is followed by following its topic tag, which
// is its slug (#nfl). A viewer with no account follows nothing.
func (h *Handler) sportsFollowedLeagues(viewer *model.User) ([]sports.League, error) {
	viewerID := gateViewerID(viewer)
	if viewerID == "" || h.db == nil || !h.sportsCache.Configured() {
		return nil, nil
	}
	tags, err := dbpkg.GetSubscribedTopics(h.db, viewerID)
	if err != nil {
		return nil, fmt.Errorf("topics of %s: %w", viewerID, err)
	}
	followed := make(map[string]bool, len(tags))
	for _, t := range tags {
		followed[strings.ToLower(t)] = true
	}
	var out []sports.League
	for _, l := range h.sportsCache.Leagues() {
		if followed[l.Slug] {
			out = append(out, l)
		}
	}
	return out, nil
}

// SportsTopicTab is one league tab on Home.
type SportsTopicTab struct {
	Surface string
	Label   string
}

// sportsTopicTabs is the Home tab for every league the viewer follows. A read
// that fails costs the viewer those tabs on this render, never Home itself.
func (h *Handler) sportsTopicTabs(viewer *model.User) []SportsTopicTab {
	leagues, err := h.sportsFollowedLeagues(viewer)
	if err != nil {
		log.Printf("[sports] topic tabs: %v", err)
		return nil
	}
	out := make([]SportsTopicTab, 0, len(leagues))
	for _, l := range leagues {
		out = append(out, SportsTopicTab{Surface: timelineLeagueSurface(l.Slug), Label: l.Label})
	}
	return out
}

// ── Pages and Facet endpoints ────────────────────────────────────────────────

// sportsHubPage — GET /sports and GET /sports/{league}
//
// /sports/{league} is the league hub; ?tab= opens it on a tab. /sports is the
// cross-league board: one scoreboard per league with anything on.
func (h *Handler) sportsHubPage(w http.ResponseWriter, r *http.Request) {
	slug := strings.ToLower(strings.TrimSpace(r.PathValue("league")))
	user := h.userFromRequest(w, r)
	// Every key the page reads is set in both modes, so the page renders the
	// same under strict templates.
	page := map[string]interface{}{
		"User":              user,
		"SessionID":         uuid.New().String(),
		"CurrentUserHandle": user.Handle,
		"Themes":            ThemesWithActive(user.ThemeID),
		"IsBoard":           slug == "",
		"Boards":            []*SportsScoreboardData(nil),
		"League":            sports.League{},
		"CanFollow":         false,
		"IsFollowingTopic":  false,
		"HubTabs":           SportsHubTabsData{},
		"Canvas":            SportsHubCanvasData{},
	}

	if slug == "" {
		var boards []*SportsScoreboardData
		for _, l := range h.sportsCache.Leagues() {
			if b := h.sportsScoreboard(l); b != nil {
				boards = append(boards, b)
			}
		}
		page["Boards"] = boards
		page["Title"] = "Sports · F33D3R"
		h.render(w, r, "sports_hub.html", h.withRail(page, user, "sports"))
		return
	}

	league, ok := h.sportsLeague(slug)
	if !ok {
		http.NotFound(w, r)
		return
	}
	tab := resolveSportsHubTab(r.URL.Query().Get("tab"))
	following := false
	if followed, err := h.sportsFollowedLeagues(user); err != nil {
		log.Printf("[sports] hub %s: %v", slug, err)
	} else {
		for _, l := range followed {
			following = following || l.Slug == slug
		}
	}
	page["League"] = league
	page["CanFollow"] = gateViewerID(user) != ""
	page["IsFollowingTopic"] = following
	page["HubTabs"] = SportsHubTabsData{League: league, Active: tab.Key, Tabs: sportsHubTabs}
	page["Canvas"] = h.sportsHubCanvas(league, tab)
	page["Title"] = league.Label + " · F33D3R"
	h.render(w, r, "sports_hub.html", h.withRail(page, user, "sports"))
}

// facetSportsHubCanvas — GET /facets/sports/{league}/hub?tab=<tab>
// A tab click is a request for the canvas drawn for that tab; the tab row comes
// back with it out of band, so the active tab is the server's statement.
func (h *Handler) facetSportsHubCanvas(w http.ResponseWriter, r *http.Request) {
	league, ok := h.sportsLeague(r.PathValue("league"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	tab := resolveSportsHubTab(r.URL.Query().Get("tab"))
	canvas, err := h.sportsFragment("sports_hub_canvas", h.sportsHubCanvas(league, tab))
	if err != nil {
		log.Printf("[sports] hub canvas %s/%s: %v", league.Slug, tab.Key, err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	tabs, err := h.sportsFragment("sports_hub_tabs", SportsHubTabsData{League: league, Active: tab.Key, Tabs: sportsHubTabs, OOB: true})
	if err != nil {
		log.Printf("[sports] hub tabs %s/%s: %v", league.Slug, tab.Key, err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(canvas))
	_, _ = w.Write([]byte(tabs))
}

// facetSportsStrip — GET /facets/sports/{league}/strip
// One league's scoreboard row, on its own. The Android Shell mounts it by this
// address, so the address stays; what it serves is the same sports_scoreboard
// Facet the hub and Home draw. An unknown league is 204; a league with no games
// writes nothing, which is how a league between seasons says it is not playing.
func (h *Handler) facetSportsStrip(w http.ResponseWriter, r *http.Request) {
	league, ok := h.sportsLeague(r.PathValue("league"))
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	board := h.sportsScoreboard(league)
	if board == nil {
		return
	}
	frag, err := h.sportsFragment("sports_scoreboard", board)
	if err != nil {
		log.Printf("[sports] strip %s: %v", league.Slug, err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write([]byte(frag))
}

// ── Timeline frames ──────────────────────────────────────────────────────────

// sportsFrameWant is a frame the plan asks for before anything is rendered.
type sportsFrameWant struct {
	slot   int
	rank   int
	league sports.League
	pinned bool
}

// sportsTimelineFrames is the sports frame source for one page of a Home
// timeline. Which frames a surface carries:
//
//	For You           the scoreboard of every followed league, pinned, first page
//	                  only; and a rotating game card every seventh work, on every
//	                  page, from each followed league
//	a league tab      that league's scoreboard, pinned, first page only
//	the Sports feed   every polled league's scoreboard, pinned, first page only
//
// The plan is made before anything is rendered and is cut to the page's frame
// budget first, so the rotation only advances past cards the viewer is served.
// A frame that fails to render is dropped and the next one of its league closes
// up into its slot; the page is served either way.
func (h *Handler) sportsTimelineFrames(spec *timelineSpec, q *timelineQuery, firstPage bool, works int) []timelineFrame {
	if q.Placement != timelinePlacementHome || !h.sportsCache.Configured() {
		return nil
	}
	var leagues []sports.League
	rotate := false
	switch {
	case spec == specForYou:
		followed, err := h.sportsFollowedLeagues(q.Viewer)
		if err != nil {
			log.Printf("[sports] frames: %v", err)
			return nil
		}
		leagues, rotate = followed, true
	case spec == specLeague:
		if l, ok := h.sportsLeague(q.Tag); ok {
			leagues = []sports.League{l}
		}
	case spec == specInterest && q.Def != nil && q.Def.ID == sportsSurfaceID:
		leagues = h.sportsCache.Leagues()
	}
	if len(leagues) == 0 {
		return nil
	}

	// Pinned scoreboards: a league with something being played leads.
	var boards []*SportsScoreboardData
	if firstPage {
		for _, l := range leagues {
			if b := h.sportsScoreboard(l); b != nil {
				boards = append(boards, b)
			}
		}
		sort.SliceStable(boards, func(i, j int) bool {
			return sportsBoardLive(boards[i]) && !sportsBoardLive(boards[j])
		})
	}

	var wants []sportsFrameWant
	for i, b := range boards {
		wants = append(wants, sportsFrameWant{slot: 0, rank: i, league: b.League, pinned: true})
	}
	if rotate {
		for slot := sportsFrameInterval; slot <= works; slot += sportsFrameInterval {
			for i, l := range leagues {
				wants = append(wants, sportsFrameWant{slot: slot, rank: i, league: l})
			}
		}
	}
	sort.SliceStable(wants, func(i, j int) bool {
		if wants[i].slot != wants[j].slot {
			return wants[i].slot < wants[j].slot
		}
		return wants[i].rank < wants[j].rank
	})
	if len(wants) > timelineFramesPerPage {
		wants = wants[:timelineFramesPerPage]
	}

	var frames []timelineFrame
	boardByLeague := make(map[string]*SportsScoreboardData, len(boards))
	for _, b := range boards {
		boardByLeague[b.League.Slug] = b
	}
	slotsByLeague := map[string][]sportsFrameWant{}
	var rotationOrder []string
	for _, want := range wants {
		if want.pinned {
			frag, err := h.sportsFragment("sports_scoreboard", boardByLeague[want.league.Slug])
			if err != nil {
				log.Printf("[sports] pinned scoreboard %s: %v", want.league.Slug, err)
				continue
			}
			frames = append(frames, timelineFrame{Slot: want.slot, Rank: want.rank, Fragment: template.HTML(frag)})
			continue
		}
		if _, seen := slotsByLeague[want.league.Slug]; !seen {
			rotationOrder = append(rotationOrder, want.league.Slug)
		}
		slotsByLeague[want.league.Slug] = append(slotsByLeague[want.league.Slug], want)
	}

	viewerKey := gateViewerID(q.Viewer)
	for _, slug := range rotationOrder {
		slots := slotsByLeague[slug]
		key := viewerKey + "|" + slug
		picked := sportsRotationNext(key, h.sportsCache.GetLeague(slug, 0), len(slots), firstPage)
		var served []sports.Game
		for _, g := range picked {
			frag, err := h.sportsFragment("sports_frame", h.sportsTile(g, sportsSurfaceFrame))
			if err != nil {
				log.Printf("[sports] frame %s: %v", g.ID, err)
				continue
			}
			want := slots[len(served)]
			served = append(served, g)
			frames = append(frames, timelineFrame{Slot: want.slot, Rank: want.rank, Fragment: template.HTML(frag)})
		}
		sportsRotationRecord(key, served, firstPage)
	}
	return placeTimelineFrames(frames, works)
}

// sportsBoardLive reports whether a scoreboard has a game being played.
func sportsBoardLive(b *SportsScoreboardData) bool {
	for _, t := range b.Tiles {
		if t.Game.Live() {
			return true
		}
	}
	return false
}

// sportsRotation is one viewer's place in one league's rotation.
type sportsRotation struct {
	// Last is the game served most recently; the next card is the one after it.
	Last string
	// Shown is every game served since the viewer's current scroll began, so a
	// scroll never carries the same card twice.
	Shown map[string]bool
}

var sportsRotations = &ttlCache{max: 50000}

// sportsRotationOrder is the order a league's games rotate in: by start, then
// id. It does not move when a game goes live, so "the one after the last" means
// the same game on every request.
func sportsRotationOrder(games []sports.Game) []sports.Game {
	order := append([]sports.Game(nil), games...)
	sort.SliceStable(order, func(i, j int) bool {
		if !order[i].Start.Equal(order[j].Start) {
			return order[i].Start.Before(order[j].Start)
		}
		return order[i].ID < order[j].ID
	})
	return order
}

// sportsRotationNext picks up to n games, resuming after the last one served.
// A first page starts a new scroll; a later page skips what this scroll
// already carried, and when every game has been carried it carries no more.
func sportsRotationNext(key string, games []sports.Game, n int, firstPage bool) []sports.Game {
	if n <= 0 || len(games) == 0 {
		return nil
	}
	var rot sportsRotation
	if v, ok := sportsRotations.get(key); ok {
		rot = v.(sportsRotation)
	}
	order := sportsRotationOrder(games)
	start := 0
	for i, g := range order {
		if g.ID == rot.Last {
			start = i + 1
			break
		}
	}
	var out []sports.Game
	for k := 0; k < len(order) && len(out) < n; k++ {
		g := order[(start+k)%len(order)]
		if !firstPage && rot.Shown[g.ID] {
			continue
		}
		out = append(out, g)
	}
	return out
}

// sportsRotationRecord writes what was served into the viewer's history. The
// stored value is replaced, never edited, so a concurrent reader never sees a
// half-written map.
func sportsRotationRecord(key string, served []sports.Game, firstPage bool) {
	var prev sportsRotation
	if v, ok := sportsRotations.get(key); ok {
		prev = v.(sportsRotation)
	}
	next := sportsRotation{Last: prev.Last, Shown: map[string]bool{}}
	if !firstPage {
		for id := range prev.Shown {
			next.Shown[id] = true
		}
	}
	for _, g := range served {
		next.Shown[g.ID] = true
		next.Last = g.ID
	}
	sportsRotations.put(key, next, time.Now().Add(sportsRotationTTL))
}

// ── Audience ─────────────────────────────────────────────────────────────────

// sportsAudienceBook holds, per game being played, how many people had it on
// screen at the last poll. Renders read it, so drawing a tile never walks the
// session registry; the poller refreshes it once per tick.
type sportsAudienceBook struct {
	mu   sync.Mutex
	last map[string]int64
}

var sportsAudiences = &sportsAudienceBook{last: map[string]int64{}}

func (b *sportsAudienceBook) count(gameID string) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.last[gameID]
}

// refresh counts every live game's audience and returns the games whose count
// changed. Games no longer live are forgotten.
func (b *sportsAudienceBook) refresh(live []sports.Game) map[string]int64 {
	counted := make(map[string]int64, len(live))
	for _, g := range live {
		counted[g.ID] = sportsWatchingCount(g.ID)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	changed := map[string]int64{}
	for id, n := range counted {
		if prev, had := b.last[id]; !had || prev != n {
			changed[id] = n
		}
	}
	b.last = counted
	return changed
}

// sportsWatchingCount is how many people have a game on screen in any shape:
// a tile, a frame or its own page. It counts people, not connections, so two
// tabs on one game are one person. It reads this replica's watch registry,
// which in a single-replica deployment is the platform.
func sportsWatchingCount(gameID string) int64 {
	ids := []string{
		sportsGameWatchID(sportsSurfaceTile, gameID),
		sportsGameWatchID(sportsSurfaceFrame, gameID),
		sportsPageWatchID(gameID),
	}
	var n int64
	pialRegistry.Range(func(_, v interface{}) bool {
		us := v.(*userSessions)
		us.mu.RLock()
		for _, id := range ids {
			if us.ctx.postIDs[id] {
				n++
				break
			}
		}
		us.mu.RUnlock()
		return true
	})
	return n
}

// ── Stream mutation ──────────────────────────────────────────────────────────

// sportsFragment executes one Facet template into a complete fragment.
func (h *Handler) sportsFragment(name string, data interface{}) (string, error) {
	if h.partial == nil {
		return "", fmt.Errorf("%s: no template set", name)
	}
	var buf bytes.Buffer
	if err := h.partial.ExecuteTemplate(&buf, name, data); err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	frag := strings.TrimSpace(buf.String())
	if frag == "" {
		return "", fmt.Errorf("%s rendered an empty fragment", name)
	}
	return frag, nil
}

// sportsNative says whether a Facet is also drawn by the Android Shell, which
// mounts the league strip and the /sports pages but cannot report what it has
// on screen. Those Facets reach native streams by the event name the Shell
// applies (sports_facet); the frame and the rail are web-only.
type sportsNative bool

const (
	sportsWebOnly   sportsNative = false
	sportsWebNative sportsNative = true
)

// sportsPublish renders one Facet and delivers it to one watch on this replica.
//
// Delivery is local because the poller is: every replica polls and runs this
// sink itself, so relaying would hand each watcher the same fragment once per
// replica. A render that fails sends nothing: a broken fragment on the wire is
// worse than a missed update, which the next poll or a reconnect's resync puts
// right.
func (h *Handler) sportsPublish(watchID, facetID, name string, data interface{}, priority FacetPriority, native sportsNative) {
	frag, err := h.sportsFragment(name, data)
	if err != nil {
		log.Printf("[sports] publish %s: %v", facetID, err)
		return
	}
	event, err := newFacetEvent(FacetMutation{FacetID: facetID, Fragment: frag, Priority: priority})
	if err != nil {
		log.Printf("[sports] publish %s: %v", facetID, err)
		return
	}
	deliverWatchedLocal(watchID, event)
	if native {
		deliverNative(SSEEvent{Type: sseSportsFacet, Data: frag})
	}
}

// sportsStructuralChange reports whether a game changed in a way that reshapes
// its card — a state, a possession, the red zone, a record, a broadcast —
// rather than only moving its numbers.
func sportsStructuralChange(old, g sports.Game) bool {
	return old.State != g.State ||
		old.SituationKey() != g.SituationKey() ||
		old.Home.Team.Record != g.Home.Team.Record ||
		old.Away.Team.Record != g.Away.Team.Record ||
		old.Broadcast != g.Broadcast ||
		old.HasScore != g.HasScore
}

// publishSportsSurfaces is this file's half of the poller's mutation sink. It
// runs on the poller goroutine once per refresh and sends each changed Facet
// to the people watching it:
//
//   - a league whose slate changed membership or order: its scoreboard and its
//     Games tab, to the league's watchers;
//   - a game whose card changed: its tile and its frame — whole when the change
//     is structural, otherwise only the score or clock that moved;
//   - a live game whose audience changed: its "watching" counts;
//   - the rail, when its window changed.
func (h *Handler) publishSportsSurfaces(prev, next []sports.Game) {
	prevByID := make(map[string]sports.Game, len(prev))
	for _, g := range prev {
		prevByID[g.ID] = g
	}

	for _, slug := range leaguesWhoseSlateChanged(prev, next) {
		h.publishSportsLeague(slug)
	}

	railWindow := min(sportsRailLimit, len(next))
	railTouched := sports.SlateKey(prev) != sports.SlateKey(next)

	var live []sports.Game
	for _, g := range next {
		if g.Live() {
			live = append(live, g)
		}
	}
	audienceChanged := sportsAudiences.refresh(live)

	for i, g := range next {
		old, existed := prevByID[g.ID]
		if !existed {
			// New to the slate: the league republish above carries it.
			continue
		}
		cardChanged := old.CardKey() != g.CardKey()
		if cardChanged && i < railWindow {
			railTouched = true
		}
		for _, surface := range []string{sportsSurfaceTile, sportsSurfaceFrame} {
			d := h.sportsTile(g, surface)
			watch := d.WatchID()
			// Tiles are inside the scoreboards the Android Shell draws; frames
			// are only ever in a web timeline.
			native := sportsNative(surface == sportsSurfaceTile)
			if cardChanged && sportsStructuralChange(old, g) {
				h.sportsPublish(watch, d.FacetID(), "sports_"+surface, d, FacetPriorityNormal, native)
				continue
			}
			if cardChanged {
				for _, side := range []string{"away", "home"} {
					oldScore, newScore := old.Away.Score, g.Away.Score
					if side == "home" {
						oldScore, newScore = old.Home.Score, g.Home.Score
					}
					if oldScore == newScore {
						continue
					}
					s := d.Score(side)
					s.Changed = true
					h.sportsPublish(watch, s.FacetID, "sports_live_score", s, FacetPriorityHigh, native)
				}
				if old.ClockKey() != g.ClockKey() {
					c := d.Clock()
					h.sportsPublish(watch, c.FacetID, "sports_live_clock", c, FacetPriorityNormal, native)
				}
			}
			if _, moved := audienceChanged[g.ID]; moved {
				a := d.AudienceFacet()
				h.sportsPublish(watch, a.FacetID, "sports_live_audience", a, FacetPriorityLow, native)
			}
		}
	}

	if railTouched {
		h.publishSportsRail()
	}
}

// publishSportsLeague sends a league's slate-level Facets to its watchers. A
// league with no games left sends nothing; its row leaves on the next read.
func (h *Handler) publishSportsLeague(slug string) {
	league, ok := h.sportsLeague(slug)
	if !ok {
		return
	}
	watch := sportsLeagueWatchID(slug)
	if b := h.sportsScoreboard(league); b != nil {
		h.sportsPublish(watch, b.FacetID(), "sports_scoreboard", b, FacetPriorityNormal, sportsWebNative)
	}
	g := h.sportsGames(league)
	h.sportsPublish(watch, g.FacetID(), "sports_games", g, FacetPriorityNormal, sportsWebNative)
}

func (h *Handler) publishSportsRail() {
	if data := h.sportsRailData(); data != nil {
		h.sportsPublish(sportsRailWatchID, data.FacetID, "rail_scorecards", data, FacetPriorityNormal, sportsWebOnly)
	}
}

// sportsWatchResync sends one person the current fragments behind a sports
// watch. The runtime re-reports its watches with resync after a reconnect, and
// whatever moved while it was away arrives here instead of by polling.
func (h *Handler) sportsWatchResync(pial, watchID string) {
	rest := strings.TrimPrefix(watchID, sportsWatchPrefix)
	send := func(facetID, name string, data interface{}) {
		frag, err := h.sportsFragment(name, data)
		if err != nil {
			log.Printf("[sports] resync %s: %v", facetID, err)
			return
		}
		PublishFacetToUser(pial, FacetMutation{FacetID: facetID, Fragment: frag})
	}
	if rest == "rail" {
		if data := h.sportsRailData(); data != nil {
			send(data.FacetID, "rail_scorecards", data)
		}
		return
	}
	kind, id, ok := strings.Cut(rest, ":")
	if !ok {
		return
	}
	switch kind {
	case "league":
		league, known := h.sportsLeague(id)
		if !known {
			return
		}
		if b := h.sportsScoreboard(league); b != nil {
			send(b.FacetID(), "sports_scoreboard", b)
		}
		g := h.sportsGames(league)
		send(g.FacetID(), "sports_games", g)
	case sportsSurfaceTile, sportsSurfaceFrame:
		game, found := h.sportsGame(id)
		if !found {
			return
		}
		d := h.sportsTile(game, kind)
		send(d.FacetID(), "sports_"+kind, d)
	}
}

// leaguesWhoseSlateChanged names the leagues whose membership or order moved
// between two snapshots. Comparing the merged board would report every league
// whenever any one of them changed.
func leaguesWhoseSlateChanged(prev, next []sports.Game) []string {
	byLeague := func(games []sports.Game) map[string][]sports.Game {
		out := map[string][]sports.Game{}
		for _, g := range games {
			out[g.League] = append(out[g.League], g)
		}
		return out
	}
	before, after := byLeague(prev), byLeague(next)
	changed := map[string]bool{}
	for slug := range after {
		if sports.SlateKey(before[slug]) != sports.SlateKey(after[slug]) {
			changed[slug] = true
		}
	}
	for slug := range before {
		if sports.SlateKey(before[slug]) != sports.SlateKey(after[slug]) {
			changed[slug] = true
		}
	}
	out := make([]string, 0, len(changed))
	for slug := range changed {
		out = append(out, slug)
	}
	sort.Strings(out)
	return out
}
