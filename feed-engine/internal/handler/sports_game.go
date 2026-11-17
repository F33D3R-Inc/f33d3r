package handler

// sports_game.go — one game's page: /sports/{league}/{game}.
//
// The doctrine, in order:
//
//  1. Triggering server event — the per-game poller (internal/sports gamecast)
//     takes a new reading of a game while at least one person has its page
//     open, every five seconds while it is being played. A chat line or a vote
//     is a request; it is its own trigger.
//  2. Server-side handler — sportsGameMutation diffs the previous and the next
//     reading: which atoms of the header moved, which plays are new, which
//     drive changed, which stats tables changed.
//  3. Facet renderer — the sports_gp_* templates (partials/_sports_game_page),
//     plus the shared score, clock and situation atoms.
//  4. Produced fragment — one complete fragment per Facet; a new play is a new
//     row Fragment, prepended into its list.
//  5. Stream mutation — facet.mutate to the watch sports:page:<game>, which the
//     page's top bar carries. Nobody without the page open receives any of it.
//
// Facet hierarchy:
//
//	shell_main_layout
//	 └─ playground
//	     ├─ sports_game page (center column)
//	     │   ├─ top bar (watch sports:page:<g>, never replaced)
//	     │   │   └─ sports_gp_pills        facet:f33d3r:sports:<g>:gp_pills
//	     │   ├─ sports_gp_header           facet:f33d3r:sports:<g>:gp_header
//	     │   │   ├─ sports_game_score      facet:f33d3r:sports:<g>:score_away|score_home
//	     │   │   ├─ sports_gp_side         facet:f33d3r:sports:<g>:gp_side_away|gp_side_home
//	     │   │   ├─ sports_game_clock      facet:f33d3r:sports:<g>:clock
//	     │   │   ├─ sports_game_situation  facet:f33d3r:sports:<g>:situation
//	     │   │   └─ sports_gp_latest       facet:f33d3r:sports:<g>:gp_latest
//	     │   └─ sports_gp_center           facet:f33d3r:sports:<g>:gp_center
//	     │       └─ (Highlights) sports_gp_highlights  …:gp_highlights
//	     │           └─ sports_gp_play_group           …:gp_hl_p<period>
//	     │               └─ sports_gp_play_row         …:gp_hl_<play>
//	     └─ right panel
//	         └─ sports_gp_panel            facet:f33d3r:sports:<g>:gp_panel
//	             ├─ (Stats) sports_gp_linescore …:gp_linescore
//	             │          sports_gp_winprob   …:gp_winprob
//	             │          sports_gp_leaders   …:gp_leaders
//	             │          sports_gp_box       …:gp_box
//	             │           └─ sports_gp_box_table …:gp_box_away|gp_box_home
//	             ├─ (Plays) sports_gp_plays     …:gp_plays
//	             │           └─ sports_gp_drive_group …:gp_drives_p<period>
//	             │               └─ sports_gp_drive   …:gp_drive_<drive>
//	             │                   ├─ sports_gp_drive_head …:gp_drive_<drive>_head
//	             │                   └─ rows             …:gp_drive_<drive>_rows
//	             │                       └─ sports_gp_play_row …:gp_play_<play>
//	             └─ (Chat)  sports_gp_vote      …:gp_vote
//	                         ├─ sports_gp_vote_pct …:gp_vote_away|gp_vote_home
//	                         └─ sports_gp_vote_total …:gp_vote_total
//	                        chat log            …:gp_chat_log
//	                         └─ sports_gp_chat_row …:gp_chat_<message>
//	                        sports_gp_composer  …:gp_composer
//
// Tabs are server state: a tab is an hx-get that answers the whole tab set
// with the chosen tab marked, replacing the Facet. No browser decides which tab
// is open.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/sports"
)

// ── Identity ─────────────────────────────────────────────────────────────────

// sportsSurfacePage is the game page as a watch surface, beside sports_hub.go's
// tile and frame: its watch is sports:page:<game>.
const sportsSurfacePage = "page"

func sportsPageWatchID(gameID string) string { return sportsGameWatchID(sportsSurfacePage, gameID) }

// sportsGPFacetID addresses one Facet of a game page.
func sportsGPFacetID(gameID, sub string) string { return sportsFacetNS + gameID + ":gp_" + sub }

// sportsChatRoom is the key a game's chat lines are held under in the live
// chat store, beside live streams' own.
func sportsChatRoom(gameID string) string { return "sports:" + gameID }

// sportsReactions are the quick reactions under the chat input.
var sportsReactions = []string{"🔥", "🤣", "🤡", "❤️"}

const sportsChatMaxRunes = 280

// ── Views ────────────────────────────────────────────────────────────────────

// SportsGameView is one reading of one game, as every game-page Facet renders
// it. Fresh is set when it is rendered for a mutation rather than a page load:
// it is what turns on a Fragment's entrance animation.
type SportsGameView struct {
	D     *sports.Detail
	Stale bool
	Fresh bool
}

func (v SportsGameView) Game() sports.Game         { return v.D.Game }
func (v SportsGameView) ID() string                { return v.D.Game.ID }
func (v SportsGameView) FacetID(sub string) string { return sportsGPFacetID(v.D.Game.ID, sub) }
func (v SportsGameView) Href() string              { return sportsGameHref(v.D.Game) }
func (v SportsGameView) WatchID() string           { return sportsPageWatchID(v.D.Game.ID) }
func (v SportsGameView) Score(side string) SportsScoreData {
	return sportsScoreData(v.D.Game, side)
}
func (v SportsGameView) Card() SportsCardData {
	return SportsCardData{Game: v.D.Game, Href: v.Href(), Stale: v.Stale}
}
func (v SportsGameView) SideData(side string) SportsSideData {
	return SportsSideData{Game: v.D.Game, Side: side}
}

// Endpoint is the address of one of this page's Facet endpoints.
func (v SportsGameView) Endpoint(name string) string {
	return "/facets/sports/" + v.D.Game.League + "/" + v.D.Game.ID + "/" + name
}

// Latest is the latest-play card's play.
func (v SportsGameView) Latest() (sports.Play, bool) { return v.D.Latest() }

// LatestRow is the latest play as a row, for the latest-play card.
func (v SportsGameView) LatestRow() SportsPlayRow {
	p, _ := v.D.Latest()
	return sportsPlayRow(v.D, "latest", p, "", v.Fresh)
}

// HighlightGroups is the Highlights tab.
func (v SportsGameView) HighlightGroups() []SportsPlayGroupView {
	return sportsPlayGroups(v.D, "hl", v.D.HighlightGroups(), sportsHighlightLatest(v.D), nil)
}

// PlayGroups is the Plays panel for a league with no drives.
func (v SportsGameView) PlayGroups() []SportsPlayGroupView {
	return sportsPlayGroups(v.D, "play", v.D.PlayGroups(), v.D.LatestID, nil)
}

// DriveGroups is the Plays panel for a league with drives.
func (v SportsGameView) DriveGroups() []SportsDriveGroupView {
	var out []SportsDriveGroupView
	for _, g := range v.D.DriveGroups() {
		dg := SportsDriveGroupView{GameID: v.ID(), Period: g.Period, Heading: g.Heading}
		for _, dr := range g.Drives {
			dg.Drives = append(dg.Drives, sportsDriveView(v.D, dr, false))
		}
		out = append(out, dg)
	}
	return out
}

// Box is one side's box score table.
func (v SportsGameView) Box(side string) SportsBoxTable {
	return SportsBoxTable{GameID: v.ID(), Box: v.D.BoxFor(side)}
}

// Pct is a win-probability side's width for the bar.
func (v SportsGameView) WinAway() int { a, _ := v.D.WinPct(); return a }
func (v SportsGameView) WinHome() int { _, h := v.D.WinPct(); return h }

// SportsSideData is the atomic Facet beside one side's score: possession, the
// timeouts still in hand, and the record.
type SportsSideData struct {
	Game sports.Game
	Side string
}

func (d SportsSideData) FacetID() string   { return sportsGPFacetID(d.Game.ID, "side_"+d.Side) }
func (d SportsSideData) Team() sports.Team { return d.Game.Side(d.Side).Team }
func (d SportsSideData) HasBall() bool {
	return d.Game.State == sports.StateInProgress && d.Game.Side(d.Side).HasBall
}
func (d SportsSideData) Pips() []bool { return d.Game.TimeoutPips(d.Side) }
func (d SportsSideData) TimeoutsLeft() int {
	return d.Game.Side(d.Side).Timeouts
}

// SportsPlayRow is one play in a list. Surface is "hl" (the Highlights tab),
// "play" (the Plays panel) or "latest" (the latest-play card), so the same play
// in two lists is two Facets.
type SportsPlayRow struct {
	GameID  string
	League  string
	Surface string
	Play    sports.Play
	Team    sports.Team
	Away    sports.Team
	Home    sports.Team
	Latest  bool
	Fresh   bool
}

func (r SportsPlayRow) FacetID() string {
	return sportsGPFacetID(r.GameID, r.Surface+"_"+r.Play.ID)
}

// ScoreLine is the score after a scoring play: "LAC 6 · BAL 0".
func (r SportsPlayRow) ScoreLine() string {
	if !r.Play.Scoring {
		return ""
	}
	return fmt.Sprintf("%s %d · %s %d", r.Away.Label(), r.Play.AwayScore, r.Home.Label(), r.Play.HomeScore)
}

// SportsPlayGroupView is one period's plays in one list.
type SportsPlayGroupView struct {
	GameID  string
	Surface string
	Period  int
	Heading string
	Rows    []SportsPlayRow
	Fresh   bool
}

func (g SportsPlayGroupView) FacetID() string {
	return sportsGPFacetID(g.GameID, fmt.Sprintf("%s_p%d", g.Surface, g.Period))
}

// RowsID is the list a new play in this period is prepended into.
func (g SportsPlayGroupView) RowsID() string {
	return sportsPlayGroupRowsID(g.GameID, g.Surface, g.Period)
}

func sportsPlayGroupRowsID(gameID, surface string, period int) string {
	return sportsGPFacetID(gameID, fmt.Sprintf("%s_p%d_rows", surface, period))
}

// SportsDriveView is one drive in the Plays panel.
type SportsDriveView struct {
	GameID string
	League string
	Drive  sports.Drive
	Rows   []SportsPlayRow // newest first
	Fresh  bool
}

func (d SportsDriveView) FacetID() string { return sportsGPFacetID(d.GameID, "drive_"+d.Drive.ID) }
func (d SportsDriveView) HeadID() string {
	return sportsGPFacetID(d.GameID, "drive_"+d.Drive.ID+"_head")
}
func (d SportsDriveView) RowsID() string {
	return sportsGPFacetID(d.GameID, "drive_"+d.Drive.ID+"_rows")
}

// SportsDriveGroupView is one period's drives.
type SportsDriveGroupView struct {
	GameID  string
	Period  int
	Heading string
	Drives  []SportsDriveView
	Fresh   bool
}

func (g SportsDriveGroupView) FacetID() string {
	return sportsGPFacetID(g.GameID, fmt.Sprintf("drives_p%d", g.Period))
}

// ListID is the list a new drive in this period is prepended into.
func (g SportsDriveGroupView) ListID() string { return sportsDriveListID(g.GameID, g.Period) }

func sportsDriveListID(gameID string, period int) string {
	return sportsGPFacetID(gameID, fmt.Sprintf("drives_p%d_list", period))
}

// SportsBoxTable is one side's box score.
type SportsBoxTable struct {
	GameID string
	Box    sports.TeamBox
}

func (t SportsBoxTable) FacetID() string { return sportsGPFacetID(t.GameID, "box_"+t.Box.Side) }

// SportsTab is one tab of a tab set.
type SportsTab struct {
	Key, Label, Href string
	Active           bool
}

func sportsTabs(v SportsGameView, endpoint string, active string, defs [][2]string, extra string) []SportsTab {
	out := make([]SportsTab, 0, len(defs))
	for _, d := range defs {
		out = append(out, SportsTab{
			Key: d[0], Label: d[1], Active: d[0] == active,
			Href: v.Endpoint(endpoint) + "?tab=" + d[0] + extra,
		})
	}
	return out
}

var (
	sportsCenterTabs = [][2]string{{"top", "Top"}, {"latest", "Latest"}, {"highlights", "Highlights"}}
	sportsPanelTabs  = [][2]string{{"stats", "Stats"}, {"plays", "Plays"}, {"chat", "Chat"}}
)

func pickTab(key string, defs [][2]string) string {
	for _, d := range defs {
		if d[0] == key {
			return key
		}
	}
	return defs[0][0]
}

func pickSide(s string) string {
	if s == "home" {
		return "home"
	}
	return "away"
}

// SportsCenterData is the center column's tab set and its open tab.
type SportsCenterData struct {
	View              SportsGameView
	Tab               string
	Tabs              []SportsTab
	Feed              surfacePage
	User              *model.User
	CurrentUserHandle string
}

func (c SportsCenterData) FacetID() string { return c.View.FacetID("center") }

// CardArgs, SentinelArgs, ErrorArgs and EmptyArgs hand the platform's shared
// feed Facets the arguments they take.
func (c SportsCenterData) CardArgs(w *model.Work) map[string]interface{} {
	return map[string]interface{}{
		"W":   w,
		"Ctx": map[string]interface{}{"CurrentUserHandle": c.CurrentUserHandle, "User": c.User},
	}
}
func (c SportsCenterData) SentinelArgs() map[string]interface{} {
	return map[string]interface{}{"URL": c.Feed.NextURL, "Cursor": c.Feed.NextCursor}
}
func (c SportsCenterData) ErrorArgs() map[string]interface{} {
	return map[string]interface{}{"URL": c.Self()}
}
func (c SportsCenterData) EmptyArgs() map[string]interface{} {
	return map[string]interface{}{
		"Title": "Nothing posted about this game yet",
		"Sub":   "Be the first to say something about it",
	}
}
func (c SportsCenterData) Self() string { return c.View.Endpoint("center") + "?tab=" + c.Tab }

// SportsPanelData is the right panel's tab set and its open tab.
type SportsPanelData struct {
	View    SportsGameView
	Tab     string
	Tabs    []SportsTab
	BoxSide string
	Vote    SportsVoteData
	Chat    SportsChatData
}

func (p SportsPanelData) FacetID() string { return p.View.FacetID("panel") }
func (p SportsPanelData) Self() string {
	return p.View.Endpoint("panel") + "?tab=" + p.Tab + "&team=" + p.BoxSide
}
func (p SportsPanelData) BoxData() SportsBoxData {
	return SportsBoxData{View: p.View, Side: p.BoxSide}
}

// SportsBoxData is the box score card: the team toggle and one side's tables.
type SportsBoxData struct {
	View SportsGameView
	Side string
}

func (b SportsBoxData) FacetID() string { return b.View.FacetID("box") }
func (b SportsBoxData) Table() SportsBoxTable {
	return b.View.Box(b.Side)
}
func (b SportsBoxData) Toggle() []SportsTab {
	g := b.View.Game()
	return []SportsTab{
		{Key: "away", Label: g.Away.Team.Label(), Active: b.Side == "away", Href: b.View.Endpoint("box") + "?team=away"},
		{Key: "home", Label: g.Home.Team.Label(), Active: b.Side == "home", Href: b.View.Endpoint("box") + "?team=home"},
	}
}
func (b SportsBoxData) ToggleTeam(side string) sports.Team { return b.View.Game().Side(side).Team }

// SportsVoteData is the fan vote. Mine and Signed are the viewer's; the
// percentages are everyone's and are their own atoms, so a vote cast elsewhere
// moves them without touching the viewer's own choice.
type SportsVoteData struct {
	GameID  string
	League  string
	Away    sports.Team
	Home    sports.Team
	Tally   dbpkg.SportsVoteTally
	Mine    string
	Signed  bool
	Settled bool
}

func (v SportsVoteData) FacetID() string      { return sportsGPFacetID(v.GameID, "vote") }
func (v SportsVoteData) TotalFacetID() string { return sportsGPFacetID(v.GameID, "vote_total") }
func (v SportsVoteData) Action() string       { return "/sports/" + v.League + "/" + v.GameID + "/vote" }
func (v SportsVoteData) CanVote() bool        { return v.Signed && v.Mine == "" && !v.Settled }
func (v SportsVoteData) Pct(side string) SportsVotePct {
	a, h := v.Tally.Pct()
	p := SportsVotePct{GameID: v.GameID, Side: side, Team: v.Away, Pct: a, Voted: v.Tally.Total() > 0}
	if side == "home" {
		p.Team, p.Pct = v.Home, h
	}
	return p
}

// Buttons is the two sides, away first, as the vote form draws them.
func (v SportsVoteData) Buttons() []SportsVoteButton {
	return []SportsVoteButton{
		{Side: "away", Team: v.Away, Pct: v.Pct("away"), Mine: v.Mine == "away"},
		{Side: "home", Team: v.Home, Pct: v.Pct("home"), Mine: v.Mine == "home"},
	}
}

func (v SportsVoteData) TotalData() SportsVoteTotal {
	return SportsVoteTotal{GameID: v.GameID, Total: v.Tally.Total()}
}

// SportsVoteButton is one side's vote button.
type SportsVoteButton struct {
	Side string
	Team sports.Team
	Pct  SportsVotePct
	Mine bool
}

// SportsVotePct is one side's share, the label inside its button.
type SportsVotePct struct {
	GameID string
	Side   string
	Team   sports.Team
	Pct    int
	Voted  bool
}

func (p SportsVotePct) FacetID() string { return sportsGPFacetID(p.GameID, "vote_"+p.Side) }

// SportsVoteTotal is the vote count under the buttons.
type SportsVoteTotal struct {
	GameID string
	Total  int
}

func (t SportsVoteTotal) FacetID() string { return sportsGPFacetID(t.GameID, "vote_total") }

// SportsChatData is the chat tab: the log, newest first, and the composer.
type SportsChatData struct {
	GameID   string
	Rows     []SportsChatRow
	Composer SportsComposerData
}

func (c SportsChatData) LogID() string { return sportsGPFacetID(c.GameID, "chat_log") }

// SportsChatRow is one chat line.
type SportsChatRow struct {
	GameID string
	M      liveChatMessage
	Fresh  bool
}

func (r SportsChatRow) FacetID() string { return sportsGPFacetID(r.GameID, "chat_"+r.M.ID) }

// AvatarArgs hands the platform's avatar Facet its arguments.
func (r SportsChatRow) AvatarArgs() map[string]interface{} {
	return map[string]interface{}{"URL": r.M.AvatarURL, "Handle": r.M.Handle, "Size": "sm"}
}

// SportsComposerData is the chat input and the quick reactions.
type SportsComposerData struct {
	GameID    string
	League    string
	Signed    bool
	Reactions []string
	Focus     bool
}

func (c SportsComposerData) FacetID() string { return sportsGPFacetID(c.GameID, "composer") }
func (c SportsComposerData) Action() string  { return "/sports/" + c.League + "/" + c.GameID + "/chat" }
func (c SportsComposerData) LogDOMID() string {
	return "sg-chat-log-" + c.GameID
}

// SportsPillsData is the top bar's row of the league's other games.
type SportsPillsData struct {
	GameID string
	Games  []sports.Game
}

func (p SportsPillsData) FacetID() string { return sportsGPFacetID(p.GameID, "pills") }

// ── Builders ─────────────────────────────────────────────────────────────────

func sportsPlayRow(d *sports.Detail, surface string, p sports.Play, latestID string, fresh bool) SportsPlayRow {
	g := d.Game
	return SportsPlayRow{
		GameID: g.ID, League: g.League, Surface: surface, Play: p,
		Team: g.Side(p.Side).Team, Away: g.Away.Team, Home: g.Home.Team,
		Latest: latestID != "" && p.ID == latestID, Fresh: fresh,
	}
}

// sportsHighlightLatest is the newest highlight's id: where the Highlights tab
// puts its LATEST badge.
func sportsHighlightLatest(d *sports.Detail) string {
	for i := len(d.Plays) - 1; i >= 0; i-- {
		if d.Plays[i].Highlight {
			return d.Plays[i].ID
		}
	}
	return ""
}

func sportsPlayGroups(d *sports.Detail, surface string, groups []sports.PlayGroup, latestID string, fresh map[string]bool) []SportsPlayGroupView {
	out := make([]SportsPlayGroupView, 0, len(groups))
	for _, g := range groups {
		gv := SportsPlayGroupView{GameID: d.Game.ID, Surface: surface, Period: g.Period, Heading: g.Heading}
		for _, p := range g.Plays {
			gv.Rows = append(gv.Rows, sportsPlayRow(d, surface, p, latestID, fresh[p.ID]))
		}
		out = append(out, gv)
	}
	return out
}

func sportsDriveView(d *sports.Detail, dr sports.Drive, fresh bool) SportsDriveView {
	v := SportsDriveView{GameID: d.Game.ID, League: d.Game.League, Drive: dr, Fresh: fresh}
	for _, p := range dr.NewestPlays() {
		v.Rows = append(v.Rows, sportsPlayRow(d, "play", p, d.LatestID, false))
	}
	return v
}

// sportsPillKey changes when anything a pill draws changes.
func sportsPillKey(g sports.Game) string {
	return g.CompactStatus() + "|" + g.ScoreKey() + "|" + g.Away.Team.LogoURL + "|" + g.Home.Team.LogoURL
}

func (h *Handler) sportsPills(g sports.Game) SportsPillsData {
	out := SportsPillsData{GameID: g.ID}
	for _, other := range h.sportsCache.GetLeague(g.League, 0) {
		if other.ID != g.ID {
			out.Games = append(out.Games, other)
		}
	}
	return out
}

func (h *Handler) sportsGameView(ctx context.Context, g sports.Game) SportsGameView {
	d, ok := h.sportsCache.Detail(ctx, g.ID)
	if !ok {
		d = &sports.Detail{Game: g, Observed: g.Observed}
	}
	return SportsGameView{D: d, Stale: h.sportsCache.StaleLeague(g.League)}
}

func (h *Handler) sportsCenter(r *http.Request, v SportsGameView, user *model.User, tab string) SportsCenterData {
	tab = pickTab(tab, sportsCenterTabs)
	c := SportsCenterData{
		View: v, Tab: tab, Tabs: sportsTabs(v, "center", tab, sportsCenterTabs, ""),
		User: user, CurrentUserHandle: user.Handle,
	}
	if tab == "highlights" || h.db == nil {
		return c
	}
	// Top and Latest are the works about the game: the ones that name either
	// club, ranked (Top) or newest first (Latest). The terms are the clubs'
	// nicknames, which is what people write.
	q := &timelineQuery{Viewer: user, Limit: 20, Terms: sportsSearchTerms(v.Game())}
	spec := specTopics
	if tab == "latest" {
		spec = specSearch
	}
	c.Feed = h.readSurfacePage(r.Context(), spec, q)
	return c
}

// sportsSearchTerms is what the game page searches works for.
func sportsSearchTerms(g sports.Game) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range []string{g.Home.Team.ShortName, g.Away.Team.ShortName} {
		t = strings.TrimSpace(t)
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

func (h *Handler) sportsVote(g sports.Game, user *model.User) SportsVoteData {
	v := SportsVoteData{
		GameID: g.ID, League: g.League, Away: g.Away.Team, Home: g.Home.Team,
		Signed: liveActor(user) != nil, Settled: g.State.Settled(),
	}
	if h.db == nil {
		return v
	}
	tally, err := dbpkg.SportsVoteTallyOf(h.db, g.League, g.ID)
	if err != nil {
		log.Printf("[sports] %v", err)
	}
	v.Tally = tally
	if v.Signed {
		mine, err := dbpkg.SportsVoteOf(h.db, g.League, g.ID, user.ID)
		if err != nil {
			log.Printf("[sports] %v", err)
		}
		v.Mine = mine
	}
	return v
}

func (h *Handler) sportsChat(g sports.Game, user *model.User) SportsChatData {
	c := SportsChatData{
		GameID: g.ID,
		Composer: SportsComposerData{
			GameID: g.ID, League: g.League, Signed: liveActor(user) != nil, Reactions: sportsReactions,
		},
	}
	backlog := liveRuntime.chatBacklog(sportsChatRoom(g.ID))
	// Newest first: the log is laid out bottom-up, so a new line is prepended
	// and lands at the bottom without the browser scrolling anything.
	for i := len(backlog) - 1; i >= 0; i-- {
		c.Rows = append(c.Rows, SportsChatRow{GameID: g.ID, M: backlog[i]})
	}
	return c
}

func (h *Handler) sportsPanel(v SportsGameView, user *model.User, tab, side string) SportsPanelData {
	tab = pickTab(tab, sportsPanelTabs)
	side = pickSide(side)
	p := SportsPanelData{
		View: v, Tab: tab, BoxSide: side,
		Tabs: sportsTabs(v, "panel", tab, sportsPanelTabs, "&team="+side),
	}
	if tab == "chat" {
		p.Vote = h.sportsVote(v.Game(), user)
		p.Chat = h.sportsChat(v.Game(), user)
	}
	return p
}

// sportsGameFromPath resolves /…/{league}/{game} to a game on the board.
func (h *Handler) sportsGameFromPath(r *http.Request) (sports.Game, bool) {
	league := strings.ToLower(strings.TrimSpace(r.PathValue("league")))
	if !h.sportsLeagueKnown(league) {
		return sports.Game{}, false
	}
	g, ok := h.sportsGame(r.PathValue("game"))
	if !ok || g.League != league {
		return sports.Game{}, false
	}
	return g, true
}

// ── Page and Facet endpoints ─────────────────────────────────────────────────

// sportsGamePage — GET /sports/{league}/{game}
func (h *Handler) sportsGamePage(w http.ResponseWriter, r *http.Request) {
	g, ok := h.sportsGameFromPath(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	user := h.userFromRequest(w, r)
	v := h.sportsGameView(r.Context(), g)
	g = v.Game()

	h.render(w, r, "sports_game.html", map[string]interface{}{
		"User":              user,
		"View":              v,
		"Pills":             h.sportsPills(g),
		"Center":            h.sportsCenter(r, v, user, r.URL.Query().Get("tab")),
		"Panel":             h.sportsPanel(v, user, r.URL.Query().Get("panel"), r.URL.Query().Get("team")),
		"TeamsCSS":          h.sportsTeamsCSSHref(),
		"Title":             g.Matchup() + " · " + g.LeagueLabel + " · F33D3R",
		"SessionID":         uuid.New().String(),
		"CurrentUserHandle": user.Handle,
		"Themes":            ThemesWithActive(user.ThemeID),
	})
}

// facetSportsGameHeader — GET /facets/sports/{league}/{game}/header
// The scoreboard header with its latest-play card. A page re-reads it after a
// stream reconnect.
func (h *Handler) facetSportsGameHeader(w http.ResponseWriter, r *http.Request) {
	g, ok := h.sportsGameFromPath(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	h.renderPartial(w, "sports_gp_header", h.sportsGameView(r.Context(), g))
}

// facetSportsGameCenter — GET /facets/sports/{league}/{game}/center?tab=
func (h *Handler) facetSportsGameCenter(w http.ResponseWriter, r *http.Request) {
	g, ok := h.sportsGameFromPath(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	user := h.userFromRequest(w, r)
	v := h.sportsGameView(r.Context(), g)
	w.Header().Set("Cache-Control", "no-store")
	h.renderPartial(w, "sports_gp_center", h.sportsCenter(r, v, user, r.URL.Query().Get("tab")))
}

// facetSportsGamePanel — GET /facets/sports/{league}/{game}/panel?tab=&team=
func (h *Handler) facetSportsGamePanel(w http.ResponseWriter, r *http.Request) {
	g, ok := h.sportsGameFromPath(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	user := h.userFromRequest(w, r)
	v := h.sportsGameView(r.Context(), g)
	w.Header().Set("Cache-Control", "no-store")
	h.renderPartial(w, "sports_gp_panel", h.sportsPanel(v, user, r.URL.Query().Get("tab"), r.URL.Query().Get("team")))
}

// facetSportsGameBox — GET /facets/sports/{league}/{game}/box?team=
func (h *Handler) facetSportsGameBox(w http.ResponseWriter, r *http.Request) {
	g, ok := h.sportsGameFromPath(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	v := h.sportsGameView(r.Context(), g)
	w.Header().Set("Cache-Control", "no-store")
	h.renderPartial(w, "sports_gp_box", SportsBoxData{View: v, Side: pickSide(r.URL.Query().Get("team"))})
}

// facetSportsGameShare — GET /facets/sports/{league}/{game}/share
// The platform's share sheet, pointed at this game.
func (h *Handler) facetSportsGameShare(w http.ResponseWriter, r *http.Request) {
	g, ok := h.sportsGameFromPath(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	base := strings.TrimRight(h.cfg.BaseURL, "/")
	if base == "" {
		base = "https://f33d3r.com"
	}
	h.renderPartial(w, "share_sheet", map[string]interface{}{
		"ShareURL":  base + sportsGameHref(g),
		"ShareText": g.Matchup() + " · " + g.LeagueLabel,
	})
}

// sportsImage — GET /img/sports/{width}/{path...}
func (h *Handler) sportsImage(w http.ResponseWriter, r *http.Request) {
	if h.sportsImages == nil {
		http.NotFound(w, r)
		return
	}
	h.sportsImages.ServeHTTP(w, r)
}

// SportsMark is a club's logo as a page draws it: the logo over the club's
// initials on its colour, so a logo that fails to load leaves the initials.
type SportsMark struct {
	Team   sports.Team
	League string
	Size   int
}

// Class is the club's colour class (see sportsTeamsCSS).
func (m SportsMark) Class() string { return sportsTeamClass(m.League, m.Team.ID) }

func sportsMark(league string, t sports.Team, size int) SportsMark {
	return SportsMark{Team: t, League: league, Size: size}
}

// sportsTeamClass is the class that carries one club's colours.
func sportsTeamClass(league, teamID string) string {
	if !safeGameIDRe.MatchString(league) || !safeGameIDRe.MatchString(teamID) {
		return "sg-team"
	}
	return "sg-team sg-team-" + league + "-" + teamID
}

// sportsTeamsCSS — GET /css/sports/teams.css
//
// Every club on the board as a class carrying its colours as custom
// properties. Club colours are data from the upstream, and a Fragment carries
// no local styles, so they reach the page as a stylesheet the server writes:
// a Fragment names the class, never the colour. The page links it with the
// slate's hash in the URL, so a new slate is a new address and the old one can
// be cached for good.
func (h *Handler) sportsTeamsCSS(w http.ResponseWriter, r *http.Request) {
	css := sportsTeamsStylesheet(h.sportsCache.Get(0))
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(css))
}

func sportsTeamsStylesheet(games []sports.Game) string {
	seen := map[string]bool{}
	var b strings.Builder
	for _, g := range games {
		for _, t := range []sports.Team{g.Away.Team, g.Home.Team} {
			cls := sportsTeamClass(g.League, t.ID)
			if cls == "sg-team" || seen[cls] {
				continue
			}
			seen[cls] = true
			// ColorHex and AltColorHex are validated six-digit hex or a fixed
			// fallback; nothing else can reach this text.
			fmt.Fprintf(&b, ".sg-team-%s-%s{--team:%s;--team-alt:%s}\n", g.League, t.ID, t.ColorHex(), t.AltColorHex())
		}
	}
	return b.String()
}

// sportsTeamsCSSHref is the stylesheet's address for the current board.
func (h *Handler) sportsTeamsCSSHref() string {
	sum := sha256.Sum256([]byte(sportsTeamsStylesheet(h.sportsCache.Get(0))))
	return "/css/sports/teams.css?v=" + hex.EncodeToString(sum[:6])
}

// sportsImageDir is where club logos and headshots are cached on disk.
func sportsImageDir() string { return filepath.Join(os.TempDir(), "f33d3r-sports-images") }

// ── Chat and vote ────────────────────────────────────────────────────────────

// sportsGameChat — POST /sports/{league}/{game}/chat
//
// The live chat's pattern, in a game's room: the line is rendered once, kept in
// the same bounded in-memory store as a live stream's chat, and the identical
// Fragment goes to the sender in the response and to everyone with the page
// open over the stream. The sender's answer is a fresh composer with the line
// placed out of band, so the input clears without any script.
func (h *Handler) sportsGameChat(w http.ResponseWriter, r *http.Request) {
	g, ok := h.sportsGameFromPath(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	viewer := liveActor(h.userFromRequest(w, r))
	if viewer == nil {
		http.Error(w, "log in to chat", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	body := strings.TrimSpace(r.FormValue("body"))
	if reaction := r.FormValue("reaction"); reaction != "" {
		// A quick reaction is sent by its own button. Only the offered ones are
		// accepted, so the button cannot be repurposed to post anything else.
		body = ""
		for _, offered := range sportsReactions {
			if reaction == offered {
				body = offered
			}
		}
	}
	if body == "" {
		http.Error(w, "message required", http.StatusBadRequest)
		return
	}
	if runes := []rune(body); len(runes) > sportsChatMaxRunes {
		body = string(runes[:sportsChatMaxRunes])
	}

	now := time.Now()
	msg := liveChatMessage{
		ID:        uuid.NewString(),
		StreamID:  sportsChatRoom(g.ID),
		Handle:    viewer.Handle,
		Name:      viewer.DisplayName,
		AvatarURL: viewer.AvatarURL,
		Body:      body,
		CreatedAt: now,
		TimeLabel: now.Format("15:04"),
		Kind:      "chat",
	}
	if msg.Name == "" {
		msg.Name = msg.Handle
	}
	if h.db != nil {
		side, err := dbpkg.SportsVoteOf(h.db, g.League, g.ID, viewer.ID)
		if err != nil {
			log.Printf("[sports] %v", err)
		}
		if side != "" {
			t := g.Side(side).Team
			msg.Badge, msg.BadgeClass = t.Label(), sportsTeamClass(g.League, t.ID)
		}
	}

	row := SportsChatRow{GameID: g.ID, M: msg, Fresh: true}
	frag, okRow := h.renderSportsFacet("sports_gp_chat_row", row)
	composer, okComposer := h.renderSportsFacet("sports_gp_composer", SportsComposerData{
		GameID: g.ID, League: g.League, Signed: true, Reactions: sportsReactions, Focus: true,
	})
	if !okRow || !okComposer {
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	liveRuntime.appendChat(msg)
	PublishFacetToPostWatchers(sportsPageWatchID(g.ID), FacetMutation{
		FacetID: row.FacetID(), Fragment: frag, Priority: FacetPriorityNormal,
		Mode: FacetPrepend, ParentFacetID: sportsGPFacetID(g.ID, "chat_log"),
	})

	var out bytes.Buffer
	out.WriteString(composer)
	// htmx inserts the out-of-band element's children at the start of the
	// log; the stream's copy of the same row then replaces it in place.
	fmt.Fprintf(&out, `<div hx-swap-oob="afterbegin:#%s">%s</div>`,
		template.HTMLEscapeString(SportsComposerData{GameID: g.ID}.LogDOMID()), frag)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(out.Bytes())
}

// sportsGameVote — POST /sports/{league}/{game}/vote
//
// One vote per account per game, held in sports_game_votes. The voter is
// answered with their own vote Facet (their side marked); everyone else with
// the page open receives only the two percentages and the total, which are the
// same for everybody.
func (h *Handler) sportsGameVote(w http.ResponseWriter, r *http.Request) {
	g, ok := h.sportsGameFromPath(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	viewer := liveActor(h.userFromRequest(w, r))
	if viewer == nil {
		http.Error(w, "log in to vote", http.StatusUnauthorized)
		return
	}
	if h.db == nil {
		http.Error(w, "voting unavailable", http.StatusServiceUnavailable)
		return
	}
	if g.State.Settled() {
		// The vote closed at the final whistle. The voter is shown it closed,
		// which is the answer to the button they pressed.
		w.Header().Set("Cache-Control", "no-store")
		h.renderPartial(w, "sports_gp_vote", h.sportsVote(g, viewer))
		return
	}
	side := r.FormValue("side")
	if side != "home" && side != "away" {
		http.Error(w, "side must be home or away", http.StatusBadRequest)
		return
	}
	if _, err := dbpkg.CastSportsVote(h.db, g.League, g.ID, viewer.ID, side); err != nil {
		log.Printf("[sports] %v", err)
		http.Error(w, "vote not recorded", http.StatusInternalServerError)
		return
	}
	v := h.sportsVote(g, viewer)
	watch := sportsPageWatchID(g.ID)
	for _, s := range []string{"away", "home"} {
		p := v.Pct(s)
		if frag, ok := h.renderSportsFacet("sports_gp_vote_pct", p); ok {
			PublishFacetToPostWatchers(watch, FacetMutation{FacetID: p.FacetID(), Fragment: frag, Priority: FacetPriorityLow})
		}
	}
	if frag, ok := h.renderSportsFacet("sports_gp_vote_total", v.TotalData()); ok {
		PublishFacetToPostWatchers(watch, FacetMutation{FacetID: v.TotalFacetID(), Fragment: frag, Priority: FacetPriorityLow})
	}
	w.Header().Set("Cache-Control", "no-store")
	h.renderPartial(w, "sports_gp_vote", v)
}

// ── Live delivery ────────────────────────────────────────────────────────────

// sportsPageViewers is the gamecast's ViewerSet: every game whose page someone
// on this replica has open, read from the FA Live watch registry.
func sportsPageViewers() []string {
	return watchedKeysWithPrefix(sportsPageWatchID(""))
}

// publishSportsGame renders one game-page Facet and delivers it to the people
// on this replica with the page open. The per-game poller runs on every
// replica for that replica's own viewers, so this is never relayed.
func (h *Handler) publishSportsGame(gameID, facetID, partial string, data interface{}) {
	h.publishSportsGameMutation(gameID, FacetMutation{FacetID: facetID}, partial, data)
}

func (h *Handler) publishSportsGameMutation(gameID string, m FacetMutation, partial string, data interface{}) {
	frag, ok := h.renderSportsFacet(partial, data)
	if !ok {
		return
	}
	m.Fragment = frag
	if m.Priority == "" {
		m.Priority = FacetPriorityNormal
	}
	event, err := newFacetEvent(m)
	if err != nil {
		log.Printf("[sports] %v", err)
		return
	}
	deliverWatchedLocal(sportsPageWatchID(gameID), event)
}

// sportsGameMutation is the gamecast's DetailSink: the previous and the next
// reading of one game, reduced to the Facets that changed.
func (h *Handler) sportsGameMutation(prev, next *sports.Detail) {
	if prev == nil || next == nil || prev == next {
		// The first reading is what the page that asked for it renders; there
		// is nothing on any screen yet to mutate.
		return
	}
	id := next.Game.ID
	pg, ng := prev.Game, next.Game
	nv := SportsGameView{D: next, Stale: h.sportsCache.StaleLeague(ng.League), Fresh: true}
	pub := func(facetID, partial string, data interface{}) { h.publishSportsGame(id, facetID, partial, data) }

	// Header. A change of state reshapes it (records settle at the final, the
	// timeout pips appear and go), so it is re-sent whole; otherwise only the
	// atoms that moved are.
	if pg.State != ng.State {
		pub(nv.FacetID("header"), "sports_gp_header", nv)
	} else {
		h.publishSportsHeaderAtoms(nv, pg)
	}

	// Plays.
	if next.HasPlays != prev.HasPlays {
		// The feed started (or stopped) carrying plays: the lists change shape.
		pub(nv.FacetID("highlights"), "sports_gp_highlights", nv)
		pub(nv.FacetID("plays"), "sports_gp_plays", nv)
	} else if next.HasPlays {
		h.publishPlayList(nv, prev, "hl", nv.FacetID("highlights"), "sports_gp_highlights",
			func(p sports.Play) bool { return p.Highlight }, sportsHighlightLatest(prev), sportsHighlightLatest(next))
		if len(next.Drives) > 0 {
			h.publishDrives(nv, prev)
		} else {
			h.publishPlayList(nv, prev, "play", nv.FacetID("plays"), "sports_gp_plays",
				func(sports.Play) bool { return true }, prev.LatestID, next.LatestID)
		}
	}

	// Stats.
	if prev.Linescore.Key() != next.Linescore.Key() {
		pub(nv.FacetID("linescore"), "sports_gp_linescore", nv)
	}
	if prev.WinProbKey() != next.WinProbKey() {
		pub(nv.FacetID("winprob"), "sports_gp_winprob", nv)
	}
	if prev.LeadersKey() != next.LeadersKey() {
		pub(nv.FacetID("leaders"), "sports_gp_leaders", nv)
	}
	for _, side := range []string{"away", "home"} {
		if prev.BoxFor(side).Key() != next.BoxFor(side).Key() {
			t := nv.Box(side)
			pub(t.FacetID(), "sports_gp_box_table", t)
		}
	}
}

// publishSportsHeaderAtoms sends the header's atoms that moved: a score, the
// possession and timeouts beside it, the clock, the down and distance, and
// the latest-play card.
func (h *Handler) publishSportsHeaderAtoms(nv SportsGameView, pg sports.Game) {
	id, ng := nv.ID(), nv.Game()
	pub := func(facetID, partial string, data interface{}) { h.publishSportsGame(id, facetID, partial, data) }
	for _, side := range []string{"away", "home"} {
		if pg.Side(side).Score != ng.Side(side).Score || pg.HasScore != ng.HasScore {
			d := sportsScoreData(ng, side)
			d.Changed = pg.Side(side).Score != ng.Side(side).Score
			pub(sportsScoreFacetID(id, side), "sports_game_score", d)
		}
		if pg.SideKey(side) != ng.SideKey(side) {
			sd := nv.SideData(side)
			pub(sd.FacetID(), "sports_gp_side", sd)
		}
	}
	if pg.ClockKey() != ng.ClockKey() {
		pub(sportsClockFacetID(id), "sports_game_clock", nv.Card())
	}
	if pg.SituationKey() != ng.SituationKey() {
		pub(sportsSituationFacetID(id), "sports_game_situation", nv.Card())
	}
	if pg.LastPlay != ng.LastPlay {
		pub(nv.FacetID("latest"), "sports_gp_latest", nv)
	}
}

// sportsNewInOrder reports whether every play in next that prev lacks comes
// after every play prev had — the normal case, where new rows can simply be
// prepended. A correction that inserts an older play returns false and the
// list is re-rendered whole instead, so it can never show plays out of order.
func sportsNewInOrder(prev, next *sports.Detail, keep func(sports.Play) bool) bool {
	old := map[string]bool{}
	for _, p := range prev.Plays {
		old[p.ID] = true
	}
	seenNew := false
	for _, p := range next.Plays {
		if !keep(p) {
			continue
		}
		if old[p.ID] {
			if seenNew {
				return false
			}
			continue
		}
		seenNew = true
	}
	return true
}

// publishPlayList extends one period-grouped play list (Highlights, or the
// Plays panel of a league without drives) with the plays that are new: a row
// prepended into its period's group, or a whole new group prepended into the
// list when the period is new. The row that carried the LATEST badge is
// re-sent without it.
func (h *Handler) publishPlayList(nv SportsGameView, prev *sports.Detail, surface, listID, listPartial string,
	keep func(sports.Play) bool, prevLatest, nextLatest string) {
	next := nv.D
	id := next.Game.ID
	var fresh []sports.Play
	for _, p := range sports.NewPlays(prev, next) {
		if keep(p) {
			fresh = append(fresh, p)
		}
	}
	if len(fresh) == 0 {
		return
	}
	if !sportsNewInOrder(prev, next, keep) {
		h.publishSportsGame(id, listID, listPartial, nv)
		return
	}
	prevPeriods := map[int]bool{}
	newestPrevPeriod := 0
	for _, p := range prev.Plays {
		if keep(p) {
			prevPeriods[p.Period] = true
			if p.Period > newestPrevPeriod {
				newestPrevPeriod = p.Period
			}
		}
	}
	var groups []sports.PlayGroup
	if surface == "hl" {
		groups = next.HighlightGroups()
	} else {
		groups = next.PlayGroups()
	}
	freshIDs := map[string]bool{}
	for _, p := range fresh {
		freshIDs[p.ID] = true
	}
	sentGroup := map[int]bool{}
	for _, p := range fresh {
		switch {
		case prevPeriods[p.Period] && p.Period == newestPrevPeriod:
			row := sportsPlayRow(next, surface, p, nextLatest, true)
			h.publishSportsGameMutation(id, FacetMutation{
				FacetID: row.FacetID(), Mode: FacetPrepend,
				ParentFacetID: sportsPlayGroupRowsID(id, surface, p.Period),
			}, "sports_gp_play_row", row)
		case prevPeriods[p.Period]:
			// A new play in a period that is no longer the newest group: the
			// order cannot be kept by prepending, so the list is re-rendered.
			h.publishSportsGame(id, listID, listPartial, nv)
			return
		case !sentGroup[p.Period]:
			sentGroup[p.Period] = true
			for _, g := range sportsPlayGroups(next, surface, groups, nextLatest, freshIDs) {
				if g.Period != p.Period {
					continue
				}
				g.Fresh = true
				h.publishSportsGameMutation(id, FacetMutation{
					FacetID: g.FacetID(), Mode: FacetPrepend, ParentFacetID: listID,
				}, "sports_gp_play_group", g)
			}
		}
	}
	if prevLatest != "" && prevLatest != nextLatest {
		if p, ok := next.PlayByID(prevLatest); ok && keep(p) {
			row := sportsPlayRow(next, surface, p, nextLatest, false)
			h.publishSportsGame(id, row.FacetID(), "sports_gp_play_row", row)
		}
	}
}

// publishDrives updates the Plays panel of a league with drives. The drive in
// progress grows by prepending its new rows and re-sending its heading; a
// drive that changed shape (it ended, it was scored, it was corrected) is
// re-sent whole, collapsed; a new drive is prepended into its period, or with a
// new period's group into the list.
func (h *Handler) publishDrives(nv SportsGameView, prev *sports.Detail) {
	next := nv.D
	id := next.Game.ID
	prevDrives := map[string]sports.Drive{}
	prevPeriods := map[int]bool{}
	newestPrevPeriod := 0
	for _, dr := range prev.Drives {
		prevDrives[dr.ID] = dr
		prevPeriods[dr.Period] = true
		if dr.Period > newestPrevPeriod {
			newestPrevPeriod = dr.Period
		}
	}
	// New drives must all follow the old ones, or the list is re-rendered.
	seenNew := false
	for _, dr := range next.Drives {
		if _, ok := prevDrives[dr.ID]; ok {
			if seenNew {
				h.publishSportsGame(id, nv.FacetID("plays"), "sports_gp_plays", nv)
				return
			}
			continue
		}
		seenNew = true
	}

	replaced := map[string]bool{}
	sentGroup := map[int]bool{}
	for _, dr := range next.Drives {
		pd, existed := prevDrives[dr.ID]
		switch {
		case !existed && prevPeriods[dr.Period] && dr.Period == newestPrevPeriod:
			v := sportsDriveView(next, dr, true)
			h.publishSportsGameMutation(id, FacetMutation{
				FacetID: v.FacetID(), Mode: FacetPrepend,
				ParentFacetID: sportsDriveListID(id, dr.Period),
			}, "sports_gp_drive", v)
			replaced[dr.ID] = true
		case !existed && prevPeriods[dr.Period]:
			h.publishSportsGame(id, nv.FacetID("plays"), "sports_gp_plays", nv)
			return
		case !existed:
			if sentGroup[dr.Period] {
				continue
			}
			sentGroup[dr.Period] = true
			for _, g := range nv.DriveGroups() {
				if g.Period == dr.Period {
					g.Fresh = true
					h.publishSportsGameMutation(id, FacetMutation{
						FacetID: g.FacetID(), Mode: FacetPrepend, ParentFacetID: nv.FacetID("plays"),
					}, "sports_gp_drive_group", g)
					for _, d := range g.Drives {
						replaced[d.Drive.ID] = true
					}
				}
			}
		case pd.Key() == dr.Key():
		case pd.InProgress && dr.InProgress && sportsDriveGrew(pd, dr):
			v := sportsDriveView(next, dr, false)
			old := map[string]bool{}
			for _, p := range pd.Plays {
				old[p.ID] = true
			}
			for _, p := range dr.Plays {
				if old[p.ID] {
					continue
				}
				row := sportsPlayRow(next, "play", p, next.LatestID, true)
				h.publishSportsGameMutation(id, FacetMutation{
					FacetID: row.FacetID(), Mode: FacetPrepend, ParentFacetID: v.RowsID(),
				}, "sports_gp_play_row", row)
			}
			h.publishSportsGame(id, v.HeadID(), "sports_gp_drive_head", v)
		default:
			v := sportsDriveView(next, dr, false)
			h.publishSportsGame(id, v.FacetID(), "sports_gp_drive", v)
			replaced[dr.ID] = true
		}
	}
	if prev.LatestID != "" && prev.LatestID != next.LatestID {
		if p, ok := next.PlayByID(prev.LatestID); ok && !replaced[p.DriveID] {
			row := sportsPlayRow(next, "play", p, next.LatestID, false)
			h.publishSportsGame(id, row.FacetID(), "sports_gp_play_row", row)
		}
	}
}

// sportsDriveGrew reports whether next is prev with plays added after its own,
// and nothing else of its plays changed.
func sportsDriveGrew(prev, next sports.Drive) bool {
	if len(next.Plays) < len(prev.Plays) {
		return false
	}
	for i, p := range prev.Plays {
		if next.Plays[i].ID != p.ID || next.Plays[i].Text != p.Text {
			return false
		}
	}
	return true
}

// sportsForgetGames releases the chat held for games that left the board.
func sportsForgetGames(prev, next []sports.Game) {
	still := make(map[string]bool, len(next))
	for _, g := range next {
		still[g.ID] = true
	}
	for _, g := range prev {
		if !still[g.ID] {
			liveRuntime.forget(sportsChatRoom(g.ID))
		}
	}
}
