package handler

import (
	"bytes"
	"context"
	"html/template"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/f33d3r/feed-engine/internal/config"
	"github.com/f33d3r/feed-engine/internal/sports"
)

// A real game will not be in overtime, at halftime or in a rain delay while
// this test runs, so every state is constructed deliberately. These are the
// facets a viewer actually sees; if a state renders wrong here, it renders
// wrong on the surface.

func sportsGame(state sports.State, mutate func(*sports.Game)) sports.Game {
	g := sports.Game{
		ID:          "401872656",
		League:      "nfl",
		LeagueLabel: "NFL",
		SeasonYear:  2026,
		WeekLabel:   "Week 1",
		Start:       time.Date(2026, 9, 10, 0, 20, 0, 0, time.UTC),
		State:       state,
		Broadcast:   "NBC",
		Venue:       "Lumen Field",
		Home: sports.Side{Team: sports.Team{
			ID: "26", Name: "Seattle Seahawks", ShortName: "Seahawks", Abbr: "SEA",
			LogoURL: "https://a.espncdn.com/sea.png", Record: "1-0",
		}},
		Away: sports.Side{Team: sports.Team{
			ID: "17", Name: "New England Patriots", ShortName: "Patriots", Abbr: "NE",
			LogoURL: "https://a.espncdn.com/ne.png", Record: "0-1",
		}},
	}
	if mutate != nil {
		mutate(&g)
	}
	return g
}

// sportsFacetSet parses the sports facet templates the way the real renderer
// does, with the same id-building funcs registered — so a template that reaches
// for a func the real funcMap does not carry fails here rather than in
// production.
func sportsFacetSet(t *testing.T) *template.Template {
	t.Helper()
	names := []string{
		"_sports_game_card.html",
		"_sports_game_score.html",
		"_sports_game_clock.html",
		"_sports_game_situation.html",
		"_sports_strip.html",
		"_rail_scorecards.html",
	}
	paths := make([]string, 0, len(names))
	for _, n := range names {
		paths = append(paths, filepath.Join("..", "..", "web", "templates", "partials", n))
	}
	funcs := template.FuncMap{}
	for k, v := range sportsFacetFuncs() {
		funcs[k] = v
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFiles(paths...)
	if err != nil {
		t.Fatalf("parsing sports facets: %v", err)
	}
	return tmpl
}

func renderSports(t *testing.T, tmpl *template.Template, name string, data interface{}) string {
	t.Helper()
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		t.Fatalf("rendering %s: %v", name, err)
	}
	return buf.String()
}

// voidElements never nest, so they must not move the depth counter.
var voidElements = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true,
	"hr": true, "img": true, "input": true, "link": true, "meta": true,
	"param": true, "source": true, "track": true, "wbr": true,
}

var tagRe = regexp.MustCompile(`<(/?)([a-zA-Z][a-zA-Z0-9-]*)([^>]*)>`)

// rootElements counts the top-level elements of a fragment and returns the
// attributes of the first one.
func rootElements(frag string) (count int, firstAttrs string) {
	depth := 0
	for _, m := range tagRe.FindAllStringSubmatchIndex(frag, -1) {
		closing := frag[m[2]:m[3]] == "/"
		name := strings.ToLower(frag[m[4]:m[5]])
		attrs := frag[m[6]:m[7]]
		switch {
		case closing:
			depth--
		case voidElements[name] || strings.HasSuffix(strings.TrimSpace(attrs), "/"):
			// self-contained
		default:
			if depth == 0 {
				count++
				if count == 1 {
					firstAttrs = attrs
				}
			}
			depth++
		}
	}
	return count, firstAttrs
}

var facetIDAttrRe = regexp.MustCompile(`data-facet-id="([^"]*)"`)

// assertSingleRootFacet enforces the Facet file rule: one root element, and that
// root carries the facet id the stream will address it by. A fragment with two
// roots or a missing id is a fragment the runtime cannot apply.
func assertSingleRootFacet(t *testing.T, label, frag, facetID string) {
	t.Helper()
	if frag == "" {
		t.Fatalf("%s: empty fragment", label)
	}
	count, attrs := rootElements(frag)
	if count != 1 {
		t.Fatalf("%s: fragment has %d root elements, want 1", label, count)
	}
	m := facetIDAttrRe.FindStringSubmatch(attrs)
	if m == nil {
		t.Fatalf("%s: root element carries no data-facet-id", label)
	}
	if m[1] != facetID {
		t.Fatalf("%s: root data-facet-id %q, want %q", label, m[1], facetID)
	}
	// Facet files carry no inline JavaScript and no local styles.
	if strings.Contains(frag, "<script") || strings.Contains(frag, "<style") ||
		strings.Contains(frag, " style=") || strings.Contains(frag, "onclick=") {
		t.Fatalf("%s: fragment carries inline script or local style", label)
	}
	if strings.Contains(frag, "<html") || strings.Contains(frag, "<body") ||
		strings.Contains(frag, "<head") {
		t.Fatalf("%s: fragment carries a document wrapper", label)
	}
}

func renderCard(t *testing.T, g sports.Game, stale bool) string {
	t.Helper()
	return renderSports(t, sportsFacetSet(t), "sports_game_card", SportsCardData{
		Game:  g,
		Href:  sportsGameHref(g),
		Stale: stale,
	})
}

// ── Facet identity ───────────────────────────────────────────────────────────

func TestSportsFacetIDsFollowTheStandard(t *testing.T) {
	want := map[string]string{
		sportsStripFacetID("nfl"):               "facet:f33d3r:sports:nfl:strip",
		sportsRailFacetID(sportsAllLeagues):     "facet:f33d3r:sports:all:scorecards",
		sportsCardFacetID("401872656"):          "facet:f33d3r:sports:401872656:card",
		sportsClockFacetID("401872656"):         "facet:f33d3r:sports:401872656:clock",
		sportsScoreFacetID("401872656", "home"): "facet:f33d3r:sports:401872656:score_home",
		sportsScoreFacetID("401872656", "away"): "facet:f33d3r:sports:401872656:score_away",
		sportsSituationFacetID("401872656"):     "facet:f33d3r:sports:401872656:situation",
	}
	for got, expect := range want {
		if got != expect {
			t.Errorf("facet id %q, want %q", got, expect)
		}
		if n := strings.Count(got, ":"); n != 4 {
			t.Errorf("facet id %q has %d separators, want 4 (facet:<ns>:<type>:<entity>:<sub>)", got, n)
		}
	}
}

func TestEverySportsFragmentCarriesASingleRootWithItsFacetID(t *testing.T) {
	set := sportsFacetSet(t)
	g := sportsGame(sports.StateInProgress, func(g *sports.Game) {
		g.Period, g.Clock = 3, "8:42"
		g.Home.Score, g.Away.Score, g.HasScore = 21, 17, true
		g.DownDistance, g.PossessionText = "3rd & 7", "SEA 45"
		g.Home.HasBall = true
	})
	card := SportsCardData{Game: g, Href: sportsGameHref(g)}

	cases := []struct{ tmpl, facetID string }{
		{"sports_game_card", sportsCardFacetID(g.ID)},
		{"sports_game_clock", sportsClockFacetID(g.ID)},
		{"sports_game_situation", sportsSituationFacetID(g.ID)},
	}
	for _, c := range cases {
		frag := strings.TrimSpace(renderSports(t, set, c.tmpl, card))
		assertSingleRootFacet(t, c.tmpl, frag, c.facetID)
	}
	for _, side := range []string{"home", "away"} {
		frag := strings.TrimSpace(renderSports(t, set, "sports_game_score", sportsScoreData(g, side)))
		assertSingleRootFacet(t, "sports_game_score/"+side, frag, sportsScoreFacetID(g.ID, side))
	}

	strip := strings.TrimSpace(renderSports(t, set, "sports_strip", &SportsStripData{
		LeagueLabel: "NFL", LeagueSlug: "nfl", WeekLabel: "Week 1",
		HubHref: "/sports/nfl", Cards: []SportsCardData{card},
		FacetID: sportsStripFacetID("nfl"),
	}))
	assertSingleRootFacet(t, "sports_strip", strip, sportsStripFacetID("nfl"))

	rail := strings.TrimSpace(renderSports(t, set, "rail_scorecards", &SportsRailData{
		Title: "NFL", Subtitle: "Week 1", HubHref: "/sports/nfl",
		Cards: []SportsCardData{card}, FacetID: sportsRailFacetID(sportsAllLeagues),
	}))
	assertSingleRootFacet(t, "rail_scorecards", rail, sportsRailFacetID(sportsAllLeagues))

	// The rail's rows are markup owned by the rail facet, never facets of their
	// own: the same game already owns an addressable card in the strip, and two
	// elements on one page claiming the same facet id would make a fragment
	// replacement land on whichever the browser found first.
	if strings.Contains(rail, sportsCardFacetID(g.ID)) {
		t.Error("rail_scorecards mounts the card facet id — that id is already the strip's")
	}
}

// ── Every state renders, and none of them lie ────────────────────────────────

func TestGameCardIsCorrectInEveryState(t *testing.T) {
	cases := []struct {
		name       string
		game       sports.Game
		wantLabel  string
		wantSpoken string
		wantScore  bool
		wantReason string
	}{
		{
			name:       "scheduled",
			game:       sportsGame(sports.StateScheduled, nil),
			wantLabel:  "Wed 8:20 PM EDT",
			wantSpoken: "Starts Wed 8:20 PM EDT",
			wantReason: "the game has not started",
		},
		{
			name: "in progress",
			game: sportsGame(sports.StateInProgress, func(g *sports.Game) {
				g.Period, g.Clock = 3, "8:42"
				g.Home.Score, g.Away.Score, g.HasScore = 21, 17, true
				g.DownDistance, g.PossessionText = "3rd & 7", "SEA 45"
				g.Home.HasBall = true
			}),
			wantLabel:  "8:42 · 3rd",
			wantSpoken: "third quarter, 8:42 remaining",
			wantScore:  true,
		},
		{
			name: "in progress, red zone",
			game: sportsGame(sports.StateInProgress, func(g *sports.Game) {
				g.Period, g.Clock = 4, "1:58"
				g.Home.Score, g.Away.Score, g.HasScore = 24, 24, true
				g.DownDistance, g.RedZone = "2nd & Goal", true
				g.Away.HasBall = true
			}),
			wantLabel:  "1:58 · 4th",
			wantSpoken: "fourth quarter, 1:58 remaining, red zone",
			wantScore:  true,
		},
		{
			name: "halftime",
			game: sportsGame(sports.StateHalftime, func(g *sports.Game) {
				g.Period = 2
				g.Home.Score, g.Away.Score, g.HasScore = 14, 10, true
			}),
			wantLabel: "Halftime", wantSpoken: "Halftime", wantScore: true,
		},
		{
			name: "end of period",
			game: sportsGame(sports.StateEndPeriod, func(g *sports.Game) {
				g.Period = 1
				g.Home.Score, g.Away.Score, g.HasScore = 7, 3, true
			}),
			wantLabel: "End 1st", wantSpoken: "End of first quarter", wantScore: true,
		},
		{
			name: "delayed",
			game: sportsGame(sports.StateDelayed, func(g *sports.Game) {
				g.Home.Score, g.Away.Score, g.HasScore = 7, 7, true
			}),
			wantLabel: "Delayed", wantSpoken: "Delayed", wantScore: true,
		},
		{
			name:      "postponed",
			game:      sportsGame(sports.StatePostponed, nil),
			wantLabel: "Postponed", wantSpoken: "Postponed",
			wantReason: "the game was postponed",
		},
		{
			name:      "canceled",
			game:      sportsGame(sports.StateCanceled, nil),
			wantLabel: "Canceled", wantSpoken: "Canceled",
			wantReason: "the game was canceled",
		},
		{
			name: "final",
			game: sportsGame(sports.StateFinal, func(g *sports.Game) {
				g.Period = 4
				g.Home.Score, g.Away.Score, g.HasScore = 27, 20, true
			}),
			wantLabel: "Final", wantSpoken: "Final", wantScore: true,
		},
		{
			name: "final after overtime",
			game: sportsGame(sports.StateFinal, func(g *sports.Game) {
				g.Period = 5
				g.Home.Score, g.Away.Score, g.HasScore = 30, 27, true
			}),
			wantLabel: "Final/OT", wantSpoken: "Final after overtime", wantScore: true,
		},
		{
			name:      "status unavailable",
			game:      sportsGame(sports.StateUnknown, nil),
			wantLabel: "Status unavailable", wantSpoken: "Status unavailable",
			wantReason: "no score has been reported",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := renderCard(t, tc.game, false)

			if !strings.Contains(out, `data-state="`+string(tc.game.State)+`"`) {
				t.Errorf("card does not carry its state attribute")
			}
			if !strings.Contains(out, tc.wantLabel) {
				t.Errorf("visible status %q missing from card", tc.wantLabel)
			}
			if !strings.Contains(out, tc.wantSpoken) {
				t.Errorf("spoken status %q missing — a screen reader would hear the abbreviation", tc.wantSpoken)
			}
			// Both clubs are always named in full for a screen reader, never
			// only as a three-letter abbreviation.
			for _, name := range []string{"Seattle Seahawks", "New England Patriots"} {
				if !strings.Contains(out, name) {
					t.Errorf("club %q not named for a screen reader", name)
				}
			}

			if tc.wantScore {
				if !strings.Contains(out, ">"+itoa(tc.game.Home.Score)) &&
					!strings.Contains(out, "score </span>"+itoa(tc.game.Home.Score)) {
					t.Errorf("home score %d not rendered", tc.game.Home.Score)
				}
				if strings.Contains(out, "has not scored") {
					t.Errorf("a played game claims nobody has scored")
				}
			} else {
				// A game that has not been played has no score. Rendering 0
				// would say the teams failed to score.
				if !strings.Contains(out, "has not scored") {
					t.Errorf("unplayed game does not say it has no score")
				}
				// The reason must be true of THIS state — a canceled game was
				// not "waiting to start".
				if tc.wantReason != "" && !strings.Contains(out, tc.wantReason) {
					t.Errorf("no-score reason %q missing", tc.wantReason)
				}
				if strings.Contains(out, `class="sports-score`) &&
					strings.Contains(out, `>0<`) {
					t.Errorf("unplayed game rendered a 0 score")
				}
			}

			// The situation line exists only while a game is being played.
			hasSituation := strings.Contains(out, sportsSituationFacetID(tc.game.ID))
			if hasSituation != tc.game.HasSituation() {
				t.Errorf("situation rendered=%t, want %t", hasSituation, tc.game.HasSituation())
			}
			if tc.game.RedZone && !strings.Contains(out, "Red zone") {
				t.Errorf("red zone not surfaced")
			}
			// Nothing is ever pushed as a state delta — the fragment is markup.
			if strings.Contains(out, `{"facet_id"`) || strings.Contains(out, `"score":`) {
				t.Errorf("fragment carries a JSON delta")
			}
		})
	}
}

// ── Staleness ────────────────────────────────────────────────────────────────

func TestAStaleCardSaysSoAndStopsPretendingToBeLive(t *testing.T) {
	g := sportsGame(sports.StateInProgress, func(g *sports.Game) {
		g.Period, g.Clock = 3, "8:42"
		g.Home.Score, g.Away.Score, g.HasScore = 21, 17, true
	})

	fresh := renderCard(t, g, false)
	if strings.Contains(fresh, "sports-clock--stale") || strings.Contains(fresh, "out of date") {
		t.Error("a current card claims to be stale")
	}

	stale := renderCard(t, g, true)
	if !strings.Contains(stale, "sports-clock--stale") {
		t.Error("a stale card does not carry the stale state")
	}
	if !strings.Contains(stale, "delayed feed") {
		t.Error("a stale card shows no visible marker")
	}
	if !strings.Contains(stale, "may be out of date") {
		t.Error("a stale card says nothing to a screen reader")
	}
	// The score itself is still shown — the last known score is real, it is only
	// its currency that is in doubt.
	if !strings.Contains(stale, "21") || !strings.Contains(stale, "17") {
		t.Error("a stale card dropped the last known score")
	}
}

// ── Mutation routing ─────────────────────────────────────────────────────────

// The whole point of the atomic facets: a clock tick must not re-send a card.
func TestClockTickRoutesToTheClockFacetAndAScoreToTheCard(t *testing.T) {
	base := sportsGame(sports.StateInProgress, func(g *sports.Game) {
		g.Period, g.Clock = 3, "8:42"
		g.Home.Score, g.Away.Score, g.HasScore = 21, 17, true
		g.DownDistance = "3rd & 7"
		g.Home.HasBall = true
	})

	tick := base
	tick.Clock = "8:35"
	if base.ClockKey() == tick.ClockKey() {
		t.Fatal("clock tick did not move the clock key")
	}
	if base.SituationKey() != tick.SituationKey() || base.ScoreKey() != tick.ScoreKey() {
		t.Fatal("clock tick moved a key it has no business moving")
	}

	td := base
	td.Home.Score = 28
	td.Home.HasBall = false
	td.Away.HasBall = true
	td.DownDistance = ""
	if base.SituationKey() == td.SituationKey() {
		t.Fatal("a possession flip did not move the situation key")
	}

	// The clock fragment is a fraction of the card fragment, which is the
	// bandwidth argument for splitting them at all.
	set := sportsFacetSet(t)
	card := renderSports(t, set, "sports_game_card", SportsCardData{Game: base, Href: sportsGameHref(base)})
	clock := renderSports(t, set, "sports_game_clock", SportsCardData{Game: base, Href: sportsGameHref(base)})
	if len(clock) >= len(card)/2 {
		t.Errorf("clock fragment %d bytes vs card %d — the split buys nothing", len(clock), len(card))
	}
}

func TestScoreFacetIsEmptyBeforeKickoffAndExactAfter(t *testing.T) {
	set := sportsFacetSet(t)

	sched := sportsGame(sports.StateScheduled, nil)
	out := renderSports(t, set, "sports_game_score", sportsScoreData(sched, "home"))
	if !strings.Contains(out, "has not scored") {
		t.Error("scheduled score facet does not say the game has not started")
	}
	if strings.Contains(out, ">0<") {
		t.Error("scheduled score facet rendered a 0")
	}

	played := sportsGame(sports.StateFinal, func(g *sports.Game) {
		g.Period = 4
		g.Home.Score, g.Away.Score, g.HasScore = 27, 20, true
	})
	out = renderSports(t, set, "sports_game_score", sportsScoreData(played, "home"))
	if !strings.Contains(out, "27") {
		t.Error("final score facet lost the score")
	}
	if !strings.Contains(out, "Seattle Seahawks score") {
		t.Error("score facet does not name whose score it is")
	}
	if !strings.Contains(out, "sports-score--lead") {
		t.Error("the winning side is not marked")
	}
}

// ── Strip bounds ─────────────────────────────────────────────────────────────

func TestStripIsBoundedSoItCannotPushTheWorksFeedOffTheSurface(t *testing.T) {
	if sportsStripLimit > 12 {
		t.Fatalf("strip limit %d is not a head lane any more", sportsStripLimit)
	}
	if sportsRailLimit > sportsStripLimit {
		t.Fatalf("rail limit %d exceeds the strip's %d", sportsRailLimit, sportsStripLimit)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// ── The rail is the slate's persistent home ──────────────────────────────────

// railTestHandler builds a Handler against the real template tree, so
// sidebar_right renders here through exactly the partial set the server serves.
// cwd-dependent, like the follow and atlas render tests.
func railTestHandler(t *testing.T) *Handler {
	t.Helper()
	cwd, _ := os.Getwd()
	if err := os.Chdir("../.."); err != nil {
		t.Skipf("cannot chdir to repo root: %v", err)
	}
	t.Cleanup(func() { os.Chdir(cwd) })
	if _, err := os.Stat("web/templates/partials"); err != nil {
		t.Skipf("no web/templates: %v", err)
	}
	h := &Handler{cfg: &config.Config{}}
	h.loadTemplates()
	return h
}

func renderRail(t *testing.T, h *Handler, page map[string]interface{}) string {
	t.Helper()
	var buf bytes.Buffer
	if err := h.partial.ExecuteTemplate(&buf, "sidebar_right", page); err != nil {
		t.Fatalf("rendering sidebar_right: %v", err)
	}
	return buf.String()
}

// TestSidebarRightHandsTheScoreboardItsOwnDataset is the regression this lane
// actually shipped with: sidebar_right rendered rail_scorecards with the PAGE's
// data instead of the panel's. On a page that had no scoreboard dataset that
// produced an empty panel; on the sports hub, which happens to define Cards and
// LeagueLabel of its own, it produced a panel carrying the hub's unbounded card
// list and — worse — an EMPTY data-facet-id, an address no stream fragment can
// ever be delivered to. The panel looked almost right and could never update.
func TestSidebarRightHandsTheScoreboardItsOwnDataset(t *testing.T) {
	h := railTestHandler(t)
	g := sportsGame(sports.StateInProgress, func(g *sports.Game) {
		g.Home.Score, g.Away.Score, g.HasScore = 7, 3, true
		g.Period, g.Clock = 1, "0:00"
	})
	card := SportsCardData{Game: g, Href: sportsGameHref(g)}

	out := renderRail(t, h, map[string]interface{}{
		"RailContext": "feed",
		// Page keys with the same names as the panel's own inputs. This is what
		// made the bug invisible: the panel read the page's values and rendered
		// something plausible.
		"Title": "PAGE LABEL, NOT THE PANEL'S",
		"Cards": []SportsCardData{card, card, card, card, card, card},
		"RailScores": &SportsRailData{
			Title:    "NFL",
			Subtitle: "Week 1",
			HubHref:  sportsHubHref("nfl"),
			Cards:    []SportsCardData{card},
			FacetID:  sportsRailFacetID(sportsAllLeagues),
		},
	})

	// The panel must carry the address the stream publishes to.
	if want := `data-facet-id="` + sportsRailFacetID(sportsAllLeagues) + `"`; !strings.Contains(out, want) {
		t.Fatalf("sidebar_right did not mount the scoreboard at %s\n%s", sportsRailFacetID(sportsAllLeagues), out)
	}
	if strings.Contains(out, `data-facet-id=""`) {
		t.Error("sidebar_right mounted a facet with an empty id — nothing can ever be delivered to it")
	}
	// It must read the panel's dataset, not the page's.
	if strings.Contains(out, "PAGE LABEL, NOT THE PANEL") {
		t.Error("rail_scorecards rendered the page's Title — it was handed the page, not RailScores")
	}
	if got := strings.Count(out, `<a class="scorecard`); got != 1 {
		t.Errorf("rail rendered %d scorecards, want 1 — the panel is reading the page's Cards", got)
	}
	// And the escape hatch to the full slate has to go somewhere.
	if !strings.Contains(out, `href="`+sportsHubHref("nfl")+`"`) {
		t.Errorf("scoreboard panel has no link to the full slate\n%s", out)
	}
}

// TestSidebarRightOmitsTheScoreboardWhenTheLaneIsOff proves the rail grows no
// empty box when the lane is off or the slate is empty — railData produces no
// RailScores then, and the rail must render as though the panel did not exist.
func TestSidebarRightOmitsTheScoreboardWhenTheLaneIsOff(t *testing.T) {
	h := railTestHandler(t)
	out := renderRail(t, h, map[string]interface{}{"RailContext": "feed"})
	if strings.Contains(out, "rail-scores-panel") {
		t.Errorf("rail rendered a scoreboard panel with no scoreboard data\n%s", out)
	}
}

// TestRailScoreboardIsBounded pins the cap. The rail is a fixed-width column
// that already carries four panels; an NBA night beside an NFL Sunday is more
// than twenty concurrent games, and rendering the board unbounded here is what
// gives the rail a scrollbar it does not have. The cap covers EVERY league at
// once — it is a height budget, not a per-league allowance.
func TestRailScoreboardIsBounded(t *testing.T) {
	if sportsRailLimit > 6 {
		t.Errorf("sportsRailLimit = %d; the rail cannot carry more than six games across every league",
			sportsRailLimit)
	}
	if sportsRailLimit >= sportsStripLimit {
		t.Errorf("rail limit %d is not tighter than one league's strip limit %d — the rail is the narrower surface",
			sportsRailLimit, sportsStripLimit)
	}
}

// TestHomeDoesNotMountTheSlateTwice guards the placement decision. The slate's
// persistent home is the rail; its full-card home is the head of the sports
// surface's Playground. The Home template must mount neither, or a viewer at
// desktop width sees the same four games twice on one screen.
func TestHomeDoesNotMountTheSlateTwice(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "web", "templates", "index.html"))
	if err != nil {
		t.Fatalf("reading index.html: %v", err)
	}
	if strings.Contains(string(body), "/facets/sports/") {
		t.Error("index.html mounts the sports strip in the centre column; the rail already carries the slate")
	}
}

// ── A score change repaints the rail in place ────────────────────────────────

// fakeSlate is a Provider that hands back whatever slate the test sets. It is
// the only way to make a touchdown happen on demand; everything downstream of it
// — the cache swap, the mutation sink, the renderer, FA Live — is the real code.
type fakeSlate struct {
	slug  string
	games []sports.Game
}

func (f *fakeSlate) Name() string { return "fake/" + f.leagueSlug() }
func (f *fakeSlate) leagueSlug() string {
	if f.slug == "" {
		return "nfl"
	}
	return f.slug
}
func (f *fakeSlate) League() sports.League { return sports.LeagueOf(f.leagueSlug()) }
func (f *fakeSlate) Fetch(context.Context) ([]sports.Game, error) {
	out := make([]sports.Game, len(f.games))
	copy(out, f.games)
	return out, nil
}

// TestScoreChangeRepaintsTheRailInPlace drives a real touchdown through the real
// path: provider → cache swap → sportsMutation → rendered fragment → FA Live.
//
// What it has to prove is that the rail UPDATES rather than reflows. The panel is
// one facet, so the browser's whole job is to find one element by id and replace
// it: the fragment must carry the rail's own facet id (so the swap lands), the
// new score (so the swap is worth making), and the same row count as before (so
// the panel is the same height after the swap as before it and nothing below it
// in the rail moves).
func TestScoreChangeRepaintsTheRailInPlace(t *testing.T) {
	h := railTestHandler(t)

	kickoff := func(id, homeAbbr, awayAbbr string) sports.Game {
		g := sportsGame(sports.StateInProgress, func(g *sports.Game) {
			g.Period, g.Clock = 2, "7:11"
			g.HasScore = true
		})
		g.ID = id
		g.Home.Team.Abbr, g.Away.Team.Abbr = homeAbbr, awayAbbr
		return g
	}
	slate := []sports.Game{
		kickoff("g1", "SEA", "NE"),
		kickoff("g2", "KC", "BUF"),
		kickoff("g3", "DAL", "PHI"),
		kickoff("g4", "SF", "LAR"),
		kickoff("g5", "GB", "CHI"),
		kickoff("g6", "MIA", "NYJ"),
		kickoff("g7", "DEN", "LV"),
	}
	provider := &fakeSlate{games: slate}
	h.sportsCache = sports.NewCache(provider)
	h.sportsCache.OnMutation(h.sportsMutation)

	// A live FA Live connection, so the fragments this produces are the exact
	// bytes a browser would receive.
	const account, pial = "acct-rail-test", "pial-rail-test"
	events, _, closeSession := RegisterSSESession(account, pial)
	defer closeSession()

	drain := func() []string {
		var out []string
		for {
			select {
			case e := <-events:
				if e.Type == sseSportsFacet {
					out = append(out, e.Data)
				}
			case <-time.After(150 * time.Millisecond):
				return out
			}
		}
	}

	// First read: the slate arrives. Nothing is pushed for it — the page renders
	// it — so drain and discard whatever the arrival produced.
	h.sportsCache.Refresh(context.Background())
	drain()

	before := h.sportsRailData()
	if before == nil || len(before.Cards) != sportsRailLimit {
		t.Fatalf("rail data = %v; want %d cards", before, sportsRailLimit)
	}
	beforeFrag, ok := h.renderSportsFacet("rail_scorecards", before)
	if !ok {
		t.Fatal("rail did not render before the score changed")
	}

	// Somebody scores, inside the rail's window.
	provider.games[0].Home.Score += 7
	h.sportsCache.Refresh(context.Background())

	var railFrag string
	for _, frag := range drain() {
		if strings.Contains(frag, `data-facet-id="`+sportsRailFacetID(sportsAllLeagues)+`"`) {
			railFrag = frag
		}
	}
	if railFrag == "" {
		t.Fatal("a score changed inside the rail's window and no rail fragment was published")
	}
	if strings.Contains(railFrag, `"facet_id"`) || strings.Contains(railFrag, `"score"`) {
		t.Error("the rail event carries state, not a rendered fragment")
	}
	if !strings.Contains(railFrag, ">7<") {
		t.Errorf("published rail fragment does not carry the new score:\n%s", railFrag)
	}
	// Same shape, so the swap repaints and does not reflow.
	if got, want := strings.Count(railFrag, `<a class="scorecard`), strings.Count(beforeFrag, `<a class="scorecard`); got != want {
		t.Errorf("rail row count changed on a score update: %d → %d", want, got)
	}

	// A score OUTSIDE the rail's four-game window must not repaint the rail.
	provider.games[4].Home.Score += 3
	h.sportsCache.Refresh(context.Background())
	for _, frag := range drain() {
		if strings.Contains(frag, `data-facet-id="`+sportsRailFacetID("nfl")+`"`) {
			t.Error("a score outside the rail's window repainted the rail")
		}
	}
}

// ── The rail is cross-league, and organised ──────────────────────────────────

// nbaGame is one basketball game, shaped the way the ESPN adapter normalises
// one: no week, no down and distance, and quarters read off the league table.
func nbaGame(id, away, home string, state sports.State, mutate func(*sports.Game)) sports.Game {
	g := sports.Game{
		ID:          id,
		League:      "nba",
		LeagueLabel: "NBA",
		SeasonYear:  2027,
		Start:       time.Date(2026, 10, 21, 23, 30, 0, 0, time.UTC),
		State:       state,
		Broadcast:   "NBA TV",
		Venue:       "Paycom Center",
		Home: sports.Side{Team: sports.Team{
			ID: "h" + id, Name: home + " Home", ShortName: home, Abbr: home,
			LogoURL: "https://a.espncdn.com/" + home + ".png", Record: "12-4",
		}},
		Away: sports.Side{Team: sports.Team{
			ID: "a" + id, Name: away + " Away", ShortName: away, Abbr: away,
			LogoURL: "https://a.espncdn.com/" + away + ".png", Record: "9-7",
		}},
	}
	if mutate != nil {
		mutate(&g)
	}
	return g
}

// crossLeagueHandler wires a real cache over two fake leagues and returns the
// handler, so every assertion below runs through the real cache ordering, the
// real data builder and the real templates.
func crossLeagueHandler(t *testing.T, nfl, nba []sports.Game) *Handler {
	t.Helper()
	h := railTestHandler(t)
	h.sportsCache = sports.NewCache(
		&fakeSlate{slug: "nfl", games: nfl},
		&fakeSlate{slug: "nba", games: nba},
	)
	h.sportsCache.Refresh(context.Background())
	return h
}

// The owner's ask, rendered: live games first wherever they are being played,
// then whatever starts soonest, one ranked list.
func TestRailRanksLiveGamesFirstAcrossLeagues(t *testing.T) {
	now := time.Now()
	nfl := []sports.Game{
		sportsGame(sports.StateScheduled, func(g *sports.Game) {
			g.ID, g.Start = "nfl-sunday", now.Add(50*time.Hour)
		}),
		sportsGame(sports.StateInProgress, func(g *sports.Game) {
			g.ID, g.Start = "nfl-live", now.Add(-2*time.Hour)
			g.Period, g.Clock = 4, "2:00"
			g.Home.Score, g.Away.Score, g.HasScore = 24, 20, true
		}),
	}
	nba := []sports.Game{
		nbaGame("nba-live", "OKC", "IND", sports.StateInProgress, func(g *sports.Game) {
			g.Start = now.Add(-time.Hour)
			g.Period, g.Clock = 3, "8:42"
			g.Home.Score, g.Away.Score, g.HasScore = 88, 91, true
		}),
		nbaGame("nba-soon", "LAL", "GS", sports.StateScheduled, func(g *sports.Game) {
			g.Start = now.Add(time.Hour)
		}),
	}
	h := crossLeagueHandler(t, nfl, nba)

	data := h.sportsRailData()
	if data == nil {
		t.Fatal("no rail data with four games on the board")
	}
	want := []string{"nfl-live", "nba-live", "nba-soon", "nfl-sunday"}
	got := make([]string, 0, len(data.Cards))
	for _, c := range data.Cards {
		got = append(got, c.Game.ID)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rail order %v, want %v — live first across leagues, then soonest", got, want)
	}
	if !data.MixedLeagues {
		t.Error("a rail carrying football and basketball does not know it is mixed")
	}
	if data.Title != "Scores" {
		t.Errorf("mixed rail titled %q — naming one league would be a lie about half the rows", data.Title)
	}
	if data.HubHref != sportsIndexHref {
		t.Errorf("mixed rail links to %q, want the cross-league board %q", data.HubHref, sportsIndexHref)
	}
	if data.FacetID != sportsRailFacetID(sportsAllLeagues) {
		t.Errorf("rail facet id %q — the panel's address must not move with the leading league", data.FacetID)
	}

	// Rendered: every row says which sport it is, and the reading order is
	// unchanged.
	frag, ok := h.renderSportsFacet("rail_scorecards", data)
	if !ok {
		t.Fatal("mixed rail did not render")
	}
	if n := strings.Count(frag, `class="scorecard-league"`); n != len(data.Cards) {
		t.Errorf("%d of %d rows carry a league tag — a reader must never wonder which sport a row is",
			n, len(data.Cards))
	}
	if !strings.Contains(frag, ">NBA<") || !strings.Contains(frag, ">NFL<") {
		t.Errorf("mixed rail does not name both leagues:\n%s", frag)
	}
	// The tag is decoration for the eye; the spoken text says it in full.
	if !strings.Contains(frag, "NBA. third quarter, 8:42 remaining") {
		t.Errorf("a screen reader is not told which league a row is:\n%s", frag)
	}
	// The rows are still the rail's own markup, never a second copy of an
	// addressable card.
	for _, c := range data.Cards {
		if strings.Contains(frag, sportsCardFacetID(c.Game.ID)) {
			t.Errorf("rail row mounts card facet id %s — that id belongs to the strip", sportsCardFacetID(c.Game.ID))
		}
	}
}

// One league in season, one out: the rail is that league's, named in the head,
// with no repeated tag down the column and no scaffolding for the league that
// is not playing.
func TestRailWithOneLeagueOutOfSeasonNamesTheOtherAndTagsNothing(t *testing.T) {
	now := time.Now()
	nfl := []sports.Game{
		sportsGame(sports.StateScheduled, func(g *sports.Game) {
			g.ID, g.Start = "nfl-1", now.Add(4*time.Hour)
		}),
		sportsGame(sports.StateScheduled, func(g *sports.Game) {
			g.ID, g.Start = "nfl-2", now.Add(5*time.Hour)
		}),
	}
	h := crossLeagueHandler(t, nfl, nil)

	data := h.sportsRailData()
	if data == nil {
		t.Fatal("no rail data with football on the board")
	}
	if data.MixedLeagues {
		t.Error("a single-league rail reports itself mixed")
	}
	if data.Title != "NFL" || data.Subtitle != "Week 1" {
		t.Errorf("single-league head %q / %q, want the league and its week", data.Title, data.Subtitle)
	}
	if data.HubHref != sportsHubHref("nfl") {
		t.Errorf("single-league rail links to %q, want the league's own board", data.HubHref)
	}
	frag, ok := h.renderSportsFacet("rail_scorecards", data)
	if !ok {
		t.Fatal("single-league rail did not render")
	}
	if strings.Contains(frag, `class="scorecard-league"`) {
		t.Error("a single-league rail repeats the league on every row — the head already says it")
	}
	if strings.Contains(frag, "NBA") {
		t.Errorf("a league with no games left a trace in the rail:\n%s", frag)
	}
	// The strips agree: a league with nothing on gets no head lane at all.
	strips := h.sportsStripsData()
	if len(strips) != 1 || strips[0].LeagueSlug != "nfl" {
		t.Fatalf("strips %v — an out-of-season league produced a head lane", strips)
	}
}

// Nothing anywhere: no panel. A box that exists to report its own emptiness is
// worth less than the space it takes.
func TestRailIsAbsentWhenNoLeagueHasAnythingOn(t *testing.T) {
	h := crossLeagueHandler(t, nil, nil)
	if data := h.sportsRailData(); data != nil {
		t.Fatalf("rail rendered with an empty board: %+v", data)
	}
	if strips := h.sportsStripsData(); len(strips) != 0 {
		t.Fatalf("%d head lanes with an empty board", len(strips))
	}
	out := renderRail(t, h, h.railData(nil, "feed"))
	if strings.Contains(out, "rail-scores-panel") {
		t.Errorf("the rail grew a scoreboard panel with nothing to put in it\n%s", out)
	}
}

// A league's head lane is its own. Basketball's slate changing must re-render
// the basketball strip and nothing else — a football strip re-sent for a
// basketball tip-off is a fragment nobody asked for on every open page.
func TestALeaguesSlateChangeOnlyTouchesThatLeaguesStrip(t *testing.T) {
	now := time.Now()
	nflSlate := []sports.Game{sportsGame(sports.StateInProgress, func(g *sports.Game) {
		g.ID, g.Start = "nfl-1", now.Add(-time.Hour)
		g.Period, g.Clock = 3, "8:42"
		g.Home.Score, g.Away.Score, g.HasScore = 21, 17, true
	})}
	nbaSlate := []sports.Game{nbaGame("nba-1", "OKC", "IND", sports.StateScheduled, func(g *sports.Game) {
		g.Start = now.Add(3 * time.Hour)
	})}

	h := railTestHandler(t)
	nflProvider := &fakeSlate{slug: "nfl", games: nflSlate}
	nbaProvider := &fakeSlate{slug: "nba", games: nbaSlate}
	h.sportsCache = sports.NewCache(nflProvider, nbaProvider)
	h.sportsCache.OnMutation(h.sportsMutation)

	const account, pial = "acct-strip-test", "pial-strip-test"
	events, _, closeSession := RegisterSSESession(account, pial)
	defer closeSession()
	drain := func() []string {
		var out []string
		for {
			select {
			case e := <-events:
				if e.Type == sseSportsFacet {
					out = append(out, e.Data)
				}
			case <-time.After(150 * time.Millisecond):
				return out
			}
		}
	}

	h.sportsCache.Refresh(context.Background())
	drain()

	// A second basketball game joins the board.
	nbaProvider.games = append(nbaProvider.games,
		nbaGame("nba-2", "LAL", "GS", sports.StateScheduled, func(g *sports.Game) {
			g.Start = now.Add(4 * time.Hour)
		}))
	h.sportsCache.Refresh(context.Background())

	var nflStrips, nbaStrips int
	for _, frag := range drain() {
		if strings.Contains(frag, `data-facet-id="`+sportsStripFacetID("nba")+`"`) {
			nbaStrips++
		}
		if strings.Contains(frag, `data-facet-id="`+sportsStripFacetID("nfl")+`"`) {
			nflStrips++
		}
	}
	if nbaStrips != 1 {
		t.Errorf("basketball strip published %d times for a basketball slate change, want 1", nbaStrips)
	}
	if nflStrips != 0 {
		t.Errorf("football strip published %d times for a basketball slate change, want 0", nflStrips)
	}
}

// The cross-league board is one section per league, and a league with nothing
// on has no section rather than an empty header.
func TestTheBoardSectionsByLeagueAndSkipsEmptyOnes(t *testing.T) {
	now := time.Now()
	nfl := []sports.Game{sportsGame(sports.StateScheduled, func(g *sports.Game) {
		g.ID, g.Start = "nfl-1", now.Add(4*time.Hour)
	})}
	h := crossLeagueHandler(t, nfl, nil)

	groups, slugs, total := h.sportsBoard("")
	if len(groups) != 1 || groups[0].LeagueSlug != "nfl" || total != 1 {
		t.Fatalf("board groups %+v (total %d) — a league with no games drew a section", groups, total)
	}
	// The empty league is still covered: it is polled, so its health is
	// reported even while it has nothing on.
	if strings.Join(slugs, ",") != "nfl,nba" {
		t.Errorf("board covers %v, want both polled leagues", slugs)
	}
	body := renderBoard(t, h, "", groups)
	if !strings.Contains(body, `id="sports-board-nfl"`) {
		t.Errorf("/sports has no football section:\n%s", body)
	}
	if strings.Contains(body, `id="sports-board-nba"`) {
		t.Error("/sports drew a section for a league with no games")
	}

	// With basketball back, both sections appear.
	h = crossLeagueHandler(t, nfl, []sports.Game{
		nbaGame("nba-1", "OKC", "IND", sports.StateScheduled, func(g *sports.Game) {
			g.Start = now.Add(6 * time.Hour)
		}),
	})
	groups, _, _ = h.sportsBoard("")
	body = renderBoard(t, h, "", groups)
	if !strings.Contains(body, `id="sports-board-nfl"`) || !strings.Contains(body, `id="sports-board-nba"`) {
		t.Errorf("/sports does not carry both leagues:\n%s", body)
	}

	// Filtered to one league, the section head is gone: the page head already
	// names the league and a heading dividing nothing from nothing is chrome.
	groups, _, _ = h.sportsBoard("nfl")
	if len(groups) != 1 {
		t.Fatalf("/sports/nfl produced %d sections", len(groups))
	}
	if body := renderBoard(t, h, "nfl", groups); strings.Contains(body, "sports-hub__league-head") {
		t.Errorf("/sports/nfl repeats the league above its only section:\n%s", body)
	}

	// A league that the platform polls is a real address all summer, even with
	// an empty slate — /sports/nba must not 404 in the offseason.
	h = crossLeagueHandler(t, nfl, nil)
	if !h.sportsLeagueKnown("nba") {
		t.Error("/sports/nba would 404 while the NBA is between seasons")
	}
	if h.sportsLeagueKnown("mlb") {
		t.Error("a league the platform does not poll is reported as known")
	}
}

// renderBoard executes the board page's body block, which is the markup the
// /sports response carries. The surrounding page chrome needs a session and a
// database this test has neither of, and neither is what the board is being
// asked about.
func renderBoard(t *testing.T, h *Handler, league string, groups []SportsGroupData) string {
	t.Helper()
	tmpl, ok := h.pages["sports_hub.html"]
	if !ok {
		t.Fatal("sports_hub.html is not in the page set")
	}
	var buf bytes.Buffer
	err := tmpl.ExecuteTemplate(&buf, "body", map[string]interface{}{
		"LeagueSlug": league,
		"HeadTitle":  "Scores",
		"WeekLabel":  "",
		"Groups":     groups,
		"GameCount":  len(groups),
		"Stale":      false,
		"StaleLabel": "",
	})
	if err != nil {
		t.Fatalf("rendering the board: %v", err)
	}
	return buf.String()
}
