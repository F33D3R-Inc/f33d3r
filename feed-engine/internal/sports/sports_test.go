package sports

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── Fixtures ─────────────────────────────────────────────────────────────────
//
// Real games will not obligingly be in overtime, halftime or a rain delay while
// this test runs, so every state is constructed deliberately from a document
// shaped exactly like the upstream's. The scheduled and final shapes are copied
// from live captures of the 2026 Week 1 scoreboard and the 2025 December range.

type fixture struct {
	name     string
	statusID string
	status   string
	state    string
	period   int
	clock    string
	complete bool
	homeSc   string
	awaySc   string
	situated bool
	redzone  bool
}

func (f fixture) doc() string {
	situation := "null"
	if f.situated {
		situation = fmt.Sprintf(`{
          "down": 3, "distance": 7,
          "downDistanceText": "3rd & 7 at SEA 45",
          "shortDownDistanceText": "3rd & 7",
          "possessionText": "SEA 45",
          "possession": "26",
          "isRedZone": %t,
          "homeTimeouts": 3, "awayTimeouts": 2,
          "lastPlay": {"text": "J. Milroe pass incomplete to D. Metcalf"}
        }`, f.redzone)
	}
	return fmt.Sprintf(`{
      "season": {"year": 2026, "type": 2},
      "week": {"number": 1},
      "events": [{
        "id": "401872656",
        "date": "2026-09-10T00:20Z",
        "shortName": "NE @ SEA",
        "status": {"displayClock": "%s", "period": %d,
          "type": {"id": "%s", "name": "%s", "state": "%s", "completed": %t,
                   "detail": "d", "shortDetail": "sd"}},
        "competitions": [{
          "id": "401872656",
          "date": "2026-09-10T00:20Z",
          "situation": %s,
          "broadcasts": [{"market": "national", "names": ["NBC"]}],
          "venue": {"fullName": "Lumen Field", "address": {"city": "Seattle", "state": "WA"}},
          "competitors": [
            {"id": "26", "homeAway": "home", "score": "%s",
             "team": {"id": "26", "location": "Seattle", "name": "Seahawks",
                      "abbreviation": "SEA", "displayName": "Seattle Seahawks",
                      "shortDisplayName": "Seahawks",
                      "logo": "https://a.espncdn.com/i/teamlogos/nfl/500/scoreboard/sea.png"},
             "records": [{"type": "total", "summary": "1-0"}]},
            {"id": "17", "homeAway": "away", "score": "%s",
             "team": {"id": "17", "location": "New England", "name": "Patriots",
                      "abbreviation": "NE", "displayName": "New England Patriots",
                      "shortDisplayName": "Patriots",
                      "logo": "https://a.espncdn.com/i/teamlogos/nfl/500/scoreboard/ne.png"},
             "records": [{"type": "total", "summary": "0-1"}]}
          ]
        }]
      }]
    }`, f.clock, f.period, f.statusID, f.status, f.state, f.complete, situation, f.homeSc, f.awaySc)
}

// everyState is one fixture per state a sport actually has.
var everyState = []struct {
	fixture
	want State
}{
	{fixture{name: "scheduled", statusID: "1", status: "STATUS_SCHEDULED", state: "pre", clock: "0:00", homeSc: "0", awaySc: "0"}, StateScheduled},
	{fixture{name: "in progress", statusID: "2", status: "STATUS_IN_PROGRESS", state: "in", period: 3, clock: "8:42", homeSc: "21", awaySc: "17", situated: true}, StateInProgress},
	{fixture{name: "red zone", statusID: "2", status: "STATUS_IN_PROGRESS", state: "in", period: 4, clock: "1:58", homeSc: "24", awaySc: "24", situated: true, redzone: true}, StateInProgress},
	{fixture{name: "halftime", statusID: "23", status: "STATUS_HALFTIME", state: "in", period: 2, clock: "0:00", homeSc: "14", awaySc: "10"}, StateHalftime},
	{fixture{name: "end of period", statusID: "22", status: "STATUS_END_PERIOD", state: "in", period: 1, clock: "0:00", homeSc: "7", awaySc: "3"}, StateEndPeriod},
	{fixture{name: "delayed", statusID: "25", status: "STATUS_DELAYED", state: "in", period: 2, clock: "5:00", homeSc: "7", awaySc: "7"}, StateDelayed},
	{fixture{name: "postponed", statusID: "6", status: "STATUS_POSTPONED", state: "pre", clock: "0:00", homeSc: "0", awaySc: "0"}, StatePostponed},
	{fixture{name: "canceled", statusID: "5", status: "STATUS_CANCELED", state: "pre", clock: "0:00", homeSc: "0", awaySc: "0"}, StateCanceled},
	{fixture{name: "final", statusID: "3", status: "STATUS_FINAL", state: "post", period: 4, clock: "0:00", complete: true, homeSc: "27", awaySc: "20"}, StateFinal},
	{fixture{name: "final overtime", statusID: "3", status: "STATUS_FINAL", state: "post", period: 5, clock: "0:00", complete: true, homeSc: "30", awaySc: "27"}, StateFinal},
	{fixture{name: "unrecognised", statusID: "99", status: "STATUS_SOMETHING_NEW", state: "", clock: "0:00", homeSc: "0", awaySc: "0"}, StateUnknown},
}

func decodeFixture(t *testing.T, f fixture) Game {
	t.Helper()
	p := newESPN("", espnNFL)
	games, err := p.decode([]byte(f.doc()))
	if err != nil {
		t.Fatalf("%s: decode: %v", f.name, err)
	}
	if len(games) != 1 {
		t.Fatalf("%s: got %d games, want 1", f.name, len(games))
	}
	return games[0]
}

// ── Normalisation ────────────────────────────────────────────────────────────

func TestEveryStateNormalises(t *testing.T) {
	for _, tc := range everyState {
		g := decodeFixture(t, tc.fixture)
		if g.State != tc.want {
			t.Errorf("%s: state %q, want %q", tc.name, g.State, tc.want)
		}
		if g.ID != "401872656" {
			t.Errorf("%s: id %q", tc.name, g.ID)
		}
		if g.Home.Team.Abbr != "SEA" || g.Away.Team.Abbr != "NE" {
			t.Errorf("%s: sides swapped: home=%q away=%q", tc.name, g.Home.Team.Abbr, g.Away.Team.Abbr)
		}
		if g.LeagueLabel != "NFL" || g.League != "nfl" {
			t.Errorf("%s: league %q/%q", tc.name, g.League, g.LeagueLabel)
		}
		if g.WeekLabel != "Week 1" {
			t.Errorf("%s: week %q", tc.name, g.WeekLabel)
		}
		if g.Broadcast != "NBC" {
			t.Errorf("%s: broadcast %q", tc.name, g.Broadcast)
		}
		if g.Start.IsZero() {
			t.Errorf("%s: kickoff not parsed — a card would print no time", tc.name)
		}
	}
}

// A game that has not started has no score. The upstream says "0", which means
// "nobody has scored yet"; rendering that as 0–0 tells the reader the teams have
// failed to score, which is a different and false statement.
func TestUnplayedGamesCarryNoScore(t *testing.T) {
	for _, tc := range everyState {
		g := decodeFixture(t, tc.fixture)
		// A score exists only in a state the lane can vouch for. An unrecognised
		// status is not one of them: the lane does not know the game started.
		wantScore := false
		switch tc.want {
		case StateInProgress, StateHalftime, StateEndPeriod, StateDelayed, StateFinal:
			wantScore = true
		}
		if g.HasScore != wantScore {
			t.Errorf("%s: HasScore=%t, want %t", tc.name, g.HasScore, wantScore)
		}
		if !wantScore {
			// And the reason given must be true of THIS state.
			if strings.TrimSpace(g.NoScoreReason()) == "" {
				t.Errorf("%s: no reason given for the missing score", tc.name)
			}
		}
	}
}

// A clock only exists while the game is being played. Carrying "0:00" onto a
// scheduled or final card puts a stopped clock on a card that has none.
func TestClockOnlyExistsWhilePlaying(t *testing.T) {
	for _, tc := range everyState {
		g := decodeFixture(t, tc.fixture)
		if !tc.want.Active() && g.Clock != "" {
			t.Errorf("%s: clock %q on a non-live game", tc.name, g.Clock)
		}
		if tc.want == StateInProgress && g.Clock == "" {
			t.Errorf("%s: live game has no clock", tc.name)
		}
	}
}

func TestOvertimeIsReadFromThePeriodNotAStatusString(t *testing.T) {
	reg := decodeFixture(t, everyState[8].fixture) // final, period 4
	ot := decodeFixture(t, everyState[9].fixture)  // final, period 5
	if reg.Overtime() {
		t.Error("regulation final reported as overtime")
	}
	if !ot.Overtime() {
		t.Error("period 5 final not reported as overtime")
	}
	if got := reg.StatusLabel(); got != "Final" {
		t.Errorf("regulation label %q", got)
	}
	if got := ot.StatusLabel(); got != "Final/OT" {
		t.Errorf("overtime label %q", got)
	}
	if got := ot.SpokenStatus(); got != "Final after overtime" {
		t.Errorf("overtime spoken %q", got)
	}
}

func TestPeriodLabels(t *testing.T) {
	// A four-period league: the NFL and the NBA both.
	for p, want := range map[int]string{0: "", 1: "1st", 2: "2nd", 3: "3rd", 4: "4th", 5: "OT", 6: "2OT", 7: "3OT"} {
		if got := periodLabel(p, 4); got != want {
			t.Errorf("period %d in a 4-period league: %q, want %q", p, got, want)
		}
	}
	// The regulation length is the league's, not the package's. A nine-period
	// league counts to nine before anything is overtime — the constant this
	// replaced would have called its fifth period "OT".
	for p, want := range map[int]string{4: "4th", 5: "5th", 9: "9th", 10: "OT", 11: "2OT"} {
		if got := periodLabel(p, 9); got != want {
			t.Errorf("period %d in a 9-period league: %q, want %q", p, got, want)
		}
	}
	// A league that did not state a regulation length never claims overtime.
	for p, want := range map[int]string{1: "1st", 5: "5th", 11: "11th", 12: "12th", 13: "13th", 21: "21st"} {
		if got := periodLabel(p, 0); got != want {
			t.Errorf("period %d in a league that did not say: %q, want %q", p, got, want)
		}
	}
}

func TestStatusLabelPerState(t *testing.T) {
	want := map[string]string{
		"in progress":   "8:42 · 3rd",
		"red zone":      "1:58 · 4th",
		"halftime":      "Halftime",
		"end of period": "End 1st",
		"delayed":       "Delayed",
		"postponed":     "Postponed",
		"canceled":      "Canceled",
		"final":         "Final",
		"unrecognised":  "Status unavailable",
	}
	for _, tc := range everyState {
		g := decodeFixture(t, tc.fixture)
		if w, ok := want[tc.name]; ok {
			if got := g.StatusLabel(); got != w {
				t.Errorf("%s: label %q, want %q", tc.name, got, w)
			}
		}
		// Every state says something. A blank chip is a card that has stopped
		// telling the reader anything.
		if strings.TrimSpace(g.StatusLabel()) == "" {
			t.Errorf("%s: empty status label", tc.name)
		}
		if strings.TrimSpace(g.SpokenStatus()) == "" {
			t.Errorf("%s: empty spoken status — nothing for a screen reader to say", tc.name)
		}
	}
}

// The scheduled card must show a kickoff, in the league's own zone, labelled.
func TestScheduledCardShowsAKickoffInTheLeagueZone(t *testing.T) {
	g := decodeFixture(t, everyState[0].fixture)
	label := g.StatusLabel()
	// 2026-09-10T00:20Z is Wednesday 8:20 PM Eastern Daylight Time.
	if label != "Wed 8:20 PM EDT" {
		t.Fatalf("kickoff label %q, want %q", label, "Wed 8:20 PM EDT")
	}
	if got := g.SpokenStatus(); got != "Starts Wed 8:20 PM EDT" {
		t.Fatalf("spoken kickoff %q", got)
	}
}

func TestSituationOnlyExistsWhilePlaying(t *testing.T) {
	live := decodeFixture(t, everyState[1].fixture)
	if !live.HasSituation() {
		t.Fatal("live game reports no situation")
	}
	if live.DownDistance != "3rd & 7" {
		t.Errorf("down and distance %q", live.DownDistance)
	}
	if !live.Home.HasBall || live.Away.HasBall {
		t.Errorf("possession: home=%t away=%t (team 26 is home)", live.Home.HasBall, live.Away.HasBall)
	}
	if live.PossessionLabel() != "SEA" {
		t.Errorf("possession label %q", live.PossessionLabel())
	}
	if live.LastPlay == "" {
		t.Error("last play dropped")
	}

	// A delayed game carries a situation block in the document; it must not be
	// rendered, because nobody is lined up over the ball during a delay.
	delayed := decodeFixture(t, everyState[5].fixture)
	if delayed.HasSituation() {
		t.Error("delayed game reports a live situation")
	}
}

func TestRedZoneIsAState(t *testing.T) {
	rz := decodeFixture(t, everyState[2].fixture)
	if !rz.RedZone {
		t.Fatal("red zone not carried")
	}
	if !strings.Contains(rz.SpokenStatus(), "red zone") {
		t.Errorf("red zone missing from spoken status: %q", rz.SpokenStatus())
	}
}

func TestLeaderIsOnlyMeaningfulWithAScore(t *testing.T) {
	sched := decodeFixture(t, everyState[0].fixture)
	if sched.HomeLeads() || sched.AwayLeads() {
		t.Error("a game that has not started has a leader")
	}
	fin := decodeFixture(t, everyState[8].fixture) // 27–20 home
	if !fin.HomeLeads() || fin.AwayLeads() {
		t.Error("final 27–20 did not resolve the winner")
	}
}

// ── Mutation keys ────────────────────────────────────────────────────────────

func TestMutationKeysSeparateAClockTickFromAScore(t *testing.T) {
	a := decodeFixture(t, everyState[1].fixture)
	b := a
	b.Clock = "8:35"

	if a.ClockKey() == b.ClockKey() {
		t.Error("a clock tick did not change the clock key")
	}
	if a.ScoreKey() != b.ScoreKey() {
		t.Error("a clock tick changed the score key — a tick would re-send the score facet")
	}
	if a.SituationKey() != b.SituationKey() {
		t.Error("a clock tick changed the situation key")
	}
	if a.CardKey() == b.CardKey() {
		t.Error("a clock tick left the card key unchanged")
	}

	c := a
	c.Home.Score += 7
	if a.ScoreKey() == c.ScoreKey() {
		t.Error("a touchdown did not change the score key")
	}
	if a.ClockKey() != c.ClockKey() {
		t.Error("a touchdown changed the clock key on its own")
	}
}

func TestSlateKeyTracksMembershipAndOrder(t *testing.T) {
	a := Game{ID: "1"}
	b := Game{ID: "2"}
	if SlateKey([]Game{a, b}) == SlateKey([]Game{b, a}) {
		t.Error("reordering the slate did not change the slate key")
	}
	if SlateKey([]Game{a, b}) == SlateKey([]Game{a}) {
		t.Error("dropping a game did not change the slate key")
	}
	if SlateKey([]Game{a, b}) != SlateKey([]Game{a, b}) {
		t.Error("slate key is not stable")
	}
}

// ── Cadence ──────────────────────────────────────────────────────────────────

// seedCache builds a cache holding a real per-league lane with a given slate,
// without touching the network. Every cadence and staleness assertion below
// goes through the same lane structure the poller drives.
func seedCache(t *testing.T, slates map[string][]Game) *Cache {
	t.Helper()
	byLeague := map[string]espnLeague{"nfl": espnNFL, "nba": espnNBA}
	providers := make([]Provider, 0, len(slates))
	for slug := range slates {
		l, ok := byLeague[slug]
		if !ok {
			t.Fatalf("seedCache: unknown league %q", slug)
		}
		providers = append(providers, newESPN("http://127.0.0.1:1/never-called", l))
	}
	c := NewCache(providers...)
	for _, lane := range c.lanes {
		games := slates[lane.league.Slug]
		for i := range games {
			games[i].League = lane.league.Slug
		}
		lane.games = games
		lane.lastOK = time.Now()
	}
	c.recompute()
	return c
}

func TestPollCadenceFollowsTheSlate(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name  string
		games []Game
		want  time.Duration
	}{
		{"between seasons", []Game{{ID: "1", State: StateScheduled, Start: now.Add(29 * 24 * time.Hour)}}, DormantInterval},
		{"empty slate", nil, DormantInterval},
		{"tomorrow", []Game{{ID: "1", State: StateScheduled, Start: now.Add(20 * time.Hour)}}, IdleInterval},
		{"today, hours away", []Game{{ID: "1", State: StateScheduled, Start: now.Add(4 * time.Hour)}}, TodayInterval},
		{"about to start", []Game{{ID: "1", State: StateScheduled, Start: now.Add(5 * time.Minute)}}, SoonInterval},
		{"in progress", []Game{{ID: "1", State: StateInProgress, Start: now.Add(-time.Hour)}}, LiveInterval},
		{"halftime", []Game{{ID: "1", State: StateHalftime, Start: now.Add(-time.Hour)}}, LiveInterval},
		{"delayed", []Game{{ID: "1", State: StateDelayed, Start: now.Add(-time.Hour)}}, SoonInterval},
		{"just finished", []Game{{ID: "1", State: StateFinal, Start: now.Add(-3 * time.Hour), Observed: now}}, SoonInterval},
	}
	for _, tc := range cases {
		c := seedCache(t, map[string][]Game{"nfl": tc.games})
		if got := c.LeagueInterval("nfl"); got != tc.want {
			t.Errorf("%s: interval %s, want %s", tc.name, got, tc.want)
		}
	}
}

// The whole point of a per-league cadence: a league being played does not drag
// a league that is between seasons along with it. One live football game must
// not cost a single extra basketball request.
func TestEachLeagueKeepsItsOwnCadence(t *testing.T) {
	now := time.Now()
	c := seedCache(t, map[string][]Game{
		"nfl": {{ID: "1", State: StateInProgress, Start: now.Add(-time.Hour)}},
		"nba": {{ID: "2", State: StateScheduled, Start: now.Add(29 * 24 * time.Hour)}},
	})
	if got := c.LeagueInterval("nfl"); got != LiveInterval {
		t.Errorf("nfl interval %s with a live game, want %s", got, LiveInterval)
	}
	if got := c.LeagueInterval("nba"); got != DormantInterval {
		t.Errorf("nba interval %s between seasons, want %s — a live football game pulled basketball to a faster cadence", got, DormantInterval)
	}
	// The platform still wakes at the fastest league's rate; it just does not
	// ask the slow one anything when it does.
	if got := c.Interval(); got > LiveInterval {
		t.Errorf("platform interval %s, want no slower than %s", got, LiveInterval)
	}
}

// One live game among many finals still pins that league to the fast cadence,
// and a slate of nothing but finals from a fortnight ago must not.
func TestOneLiveGameSetsTheCadenceForTheSlate(t *testing.T) {
	now := time.Now()
	c := seedCache(t, map[string][]Game{"nfl": {
		{ID: "1", State: StateFinal, Start: now.Add(-14 * 24 * time.Hour), Observed: now.Add(-time.Hour)},
		{ID: "2", State: StateInProgress, Start: now.Add(-time.Hour)},
	}})
	if got := c.LeagueInterval("nfl"); got != LiveInterval {
		t.Fatalf("interval %s with a live game, want %s", got, LiveInterval)
	}
	c.lanes[0].games = c.lanes[0].games[:1]
	if got := c.LeagueInterval("nfl"); got != DormantInterval {
		t.Fatalf("interval %s with nothing but a fortnight-old final, want %s", got, DormantInterval)
	}
}

// ── Staleness ────────────────────────────────────────────────────────────────

func TestStaleIsTighterWhileAGameIsBeingPlayed(t *testing.T) {
	now := time.Now()
	c := seedCache(t, map[string][]Game{"nfl": {{ID: "1", State: StateInProgress}}})
	lane := c.lanes[0]

	lane.lastOK = now.Add(-LiveStaleAfter - time.Second)
	if !c.Stale() {
		t.Error("a live slate unrefreshed past the live limit is not marked stale")
	}
	lane.lastOK = now.Add(-LiveStaleAfter + 30*time.Second)
	if c.Stale() {
		t.Error("a freshly-read live slate is marked stale")
	}

	// A slate that is only polled every five minutes cannot be stale two
	// minutes after a successful read — that limit is the live one.
	lane.games = []Game{{ID: "1", State: StateScheduled, Start: now.Add(4 * time.Hour)}}
	lane.lastOK = now.Add(-LiveStaleAfter - time.Second)
	if c.Stale() {
		t.Error("a slate on the five-minute cadence is held to the live staleness limit")
	}
	lane.lastOK = now.Add(-staleAfter(TodayInterval) - time.Second)
	if !c.Stale() {
		t.Error("a slate that missed three polls in a row is not marked stale")
	}

	// A league between seasons is polled every two hours BY DESIGN. Calling it
	// stale thirty-one minutes after a good read would put a "feed delayed"
	// badge on a schedule that is working exactly as intended.
	lane.games = []Game{{ID: "1", State: StateScheduled, Start: now.Add(29 * 24 * time.Hour)}}
	lane.lastOK = now.Add(-90 * time.Minute)
	if c.Stale() {
		t.Error("a dormant league is called stale inside its own poll interval")
	}
	lane.lastOK = now.Add(-staleAfter(DormantInterval) - time.Second)
	if !c.Stale() {
		t.Error("a dormant league that has not answered for three of its own intervals is not stale")
	}

	// Nothing has ever been read: the surface must not present an empty
	// snapshot as a current one.
	lane.lastOK = time.Time{}
	if !c.Stale() {
		t.Error("a cache that has never been refreshed is not marked stale")
	}
}

// One league's outage is not the other's. A basketball feed that stopped
// answering must not put a "delayed" badge on football rows that are current.
func TestStalenessIsPerLeague(t *testing.T) {
	now := time.Now()
	c := seedCache(t, map[string][]Game{
		"nfl": {{ID: "1", State: StateInProgress}},
		"nba": {{ID: "2", State: StateInProgress}},
	})
	for _, l := range c.lanes {
		if l.league.Slug == "nba" {
			l.lastOK = now.Add(-LiveStaleAfter - time.Second)
		} else {
			l.lastOK = now
		}
	}
	if c.StaleLeague("nfl") {
		t.Error("football marked stale because basketball went quiet")
	}
	if !c.StaleLeague("nba") {
		t.Error("basketball not marked stale after its own feed went quiet")
	}
	if !c.Stale() {
		t.Error("the platform does not report that some league is stale")
	}
}

// A disabled lane is not stale — it is absent. A "delayed feed" badge on a
// platform that was never asked to show scores would be a lie of its own.
func TestDisabledLaneIsNeverStale(t *testing.T) {
	c := NewCache()
	if c.Configured() {
		t.Fatal("nil provider reported as configured")
	}
	if c.Stale() {
		t.Error("disabled lane reports itself stale")
	}
	if got := c.Get(4); len(got) != 0 {
		t.Errorf("disabled lane returned %d games", len(got))
	}
	if _, ok := c.Game("401872656"); ok {
		t.Error("disabled lane resolved a game")
	}
	// Start must not spawn a poller or touch the network.
	c.Start(context.Background())
	if _, _, fetches := c.Health(); fetches != 0 {
		t.Errorf("disabled lane made %d requests", fetches)
	}
}

// ── Provider resolution ──────────────────────────────────────────────────────

// The two tests that used to live here pinned a blank SPORTS_PROVIDER as "the
// lane switched off" and an unknown name as "no providers". That contract is
// gone: TestSportsLaneIsAlwaysOn (below) pins the opposite — every input yields
// a running lane, and a bad name is reported without emptying it.

func TestESPNProviderNeedsNoKey(t *testing.T) {
	ps, err := NewProviders("ESPN", "", "")
	if err != nil || len(ps) == 0 {
		t.Fatalf("espn with no key: %v / %v", ps, err)
	}
	// Every league in the table gets its own provider, so every league gets its
	// own poll cadence. A single provider covering both would force one.
	want := map[string]string{"nfl": "espn/nfl", "nba": "espn/nba"}
	got := map[string]string{}
	for _, p := range ps {
		got[p.League().Slug] = p.Name()
	}
	if len(got) != len(want) {
		t.Fatalf("providers %v, want one per league %v", got, want)
	}
	for slug, name := range want {
		if got[slug] != name {
			t.Fatalf("provider for %s is %q, want %q", slug, got[slug], name)
		}
	}
}

// ── Upstream behaviour ───────────────────────────────────────────────────────

func TestRefreshKeepsTheLastGoodSnapshotWhenTheUpstreamFails(t *testing.T) {
	var fail atomic.Bool
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if fail.Load() {
			http.Error(w, "upstream down", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(everyState[1].doc()))
	}))
	defer srv.Close()

	c := NewCache(newESPN(srv.URL, espnNFL))
	c.Refresh(context.Background())
	if got := c.Get(0); len(got) != 1 || got[0].Home.Score != 21 {
		t.Fatalf("first refresh: %+v", got)
	}
	firstOK, _, _ := c.Health()

	fail.Store(true)
	c.Refresh(context.Background())

	got := c.Get(0)
	if len(got) != 1 || got[0].Home.Score != 21 {
		t.Fatalf("a failed refresh blanked the snapshot: %+v", got)
	}
	lastOK, lastErr, fetches := c.Health()
	if lastErr == nil {
		t.Error("a failed refresh did not record the error")
	}
	if !lastOK.Equal(firstOK) {
		t.Error("a failed refresh advanced the last-good timestamp — the card would claim to be current")
	}
	if fetches != 2 {
		t.Errorf("fetches=%d, want 2", fetches)
	}
	if hits.Load() != 2 {
		t.Errorf("upstream hits=%d, want 2", hits.Load())
	}
}

// A byte-identical document must not cost a normalise or a mutation. This is
// what keeps a quiet slate from re-rendering facets every poll tick.
func TestIdenticalDocumentProducesNoMutation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(everyState[1].doc()))
	}))
	defer srv.Close()

	c := NewCache(newESPN(srv.URL, espnNFL))

	var mutations int
	var lastPrev, lastNext []Game
	c.OnMutation(func(prev, next []Game) {
		mutations++
		lastPrev, lastNext = prev, next
	})

	c.Refresh(context.Background())
	c.Refresh(context.Background())
	c.Refresh(context.Background())

	if mutations != 3 {
		t.Fatalf("sink called %d times, want 3 (once per refresh)", mutations)
	}
	// The sink runs every refresh; what must be identical is the CONTENT, so the
	// sink's own diff finds nothing to push.
	if len(lastPrev) != 1 || len(lastNext) != 1 {
		t.Fatalf("snapshots: prev=%d next=%d", len(lastPrev), len(lastNext))
	}
	if lastPrev[0].CardKey() != lastNext[0].CardKey() {
		t.Error("an unchanged document produced a changed card key — every tick would push a fragment")
	}
	if SlateKey(lastPrev) != SlateKey(lastNext) {
		t.Error("an unchanged document produced a changed slate key")
	}
}

func TestSlateIsOrderedLiveFirstThenUpcomingThenFinal(t *testing.T) {
	now := time.Now()
	games := []Game{
		{ID: "final", State: StateFinal, Start: now.Add(-4 * time.Hour)},
		{ID: "later", State: StateScheduled, Start: now.Add(3 * time.Hour)},
		{ID: "live", State: StateInProgress, Start: now.Add(-time.Hour)},
		{ID: "soon", State: StateScheduled, Start: now.Add(time.Hour)},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(everyState[0].doc()))
	}))
	defer srv.Close()

	c := NewCache(newESPN(srv.URL, espnNFL))
	_ = c
	// Exercise the same comparator the refresh uses.
	sorted := append([]Game(nil), games...)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if less(sorted[j], sorted[i]) {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	want := []string{"live", "soon", "later", "final"}
	for i, id := range want {
		if sorted[i].ID != id {
			t.Fatalf("slate order %v, want %v", ids(sorted), want)
		}
	}
}

func ids(games []Game) []string {
	out := make([]string, len(games))
	for i, g := range games {
		out[i] = g.ID
	}
	return out
}

// The fixture documents must stay valid JSON of the shape the upstream sends;
// a typo here would silently weaken every test above.
func TestFixturesAreWellFormed(t *testing.T) {
	for _, tc := range everyState {
		var v map[string]interface{}
		if err := json.Unmarshal([]byte(tc.doc()), &v); err != nil {
			t.Fatalf("%s fixture is not valid JSON: %v", tc.name, err)
		}
	}
}

func TestNoScoreReasonIsTrueOfEachState(t *testing.T) {
	want := map[State]string{
		StateScheduled: "the game has not started",
		StatePostponed: "the game was postponed",
		StateCanceled:  "the game was canceled",
		StateUnknown:   "no score has been reported",
	}
	for state, expect := range want {
		if got := (Game{State: state}).NoScoreReason(); got != expect {
			t.Errorf("%s: reason %q, want %q", state, got, expect)
		}
	}
}

// ── NBA ──────────────────────────────────────────────────────────────────────
//
// Every document below is the real shape of a real ESPN basketball response,
// reduced to the fields this lane reads. They were captured from
// site.api.espn.com/apis/site/v2/sports/basketball/nba/scoreboard:
//
//   - the 2OT final is OKC @ IND, 2025-10-23, which the upstream reports as
//     period 6 with detail "Final/2OT" — the two-overtime case this lane has to
//     get right, because it is routine in basketball and does not exist in a
//     regular-season football week;
//   - the scheduled game is MIA @ TOR, 2026-10-03, read on 2026-09-04 while the
//     league is between seasons: no records at all, and a start time a month
//     out;
//   - NO DOCUMENT HAS A "week" KEY. That is the finding, not an omission: the
//     NBA scoreboard has no week object, and the label has no replacement.

type nbaFixture struct {
	name    string
	status  string
	state   string
	period  int
	clock   string
	done    bool
	homeSc  string
	awaySc  string
	records bool
	date    string
}

func (f nbaFixture) doc() string {
	records := `[]`
	if f.records {
		records = `[{"type":"total","summary":"20-17"},{"type":"home","summary":"13-9"},{"type":"road","summary":"7-8"}]`
	}
	date := f.date
	if date == "" {
		date = "2025-10-24T23:30Z"
	}
	return fmt.Sprintf(`{
      "season": {"year": 2026, "type": 2},
      "day": {"date": "2025-10-23"},
      "events": [{
        "id": "401810500",
        "date": "%s",
        "shortName": "OKC @ IND",
        "status": {"displayClock": "%s", "period": %d,
          "type": {"id": "3", "name": "%s", "state": "%s", "completed": %t,
                   "detail": "Final/2OT", "shortDetail": "Final/2OT"}},
        "competitions": [{
          "id": "401810500",
          "date": "%s",
          "situation": null,
          "broadcasts": [{"market": "national", "names": ["NBA TV"]}],
          "venue": {"fullName": "Gainbridge Fieldhouse", "address": {"city": "Indianapolis", "state": "IN"}},
          "competitors": [
            {"id": "11", "homeAway": "home", "score": "%s",
             "team": {"id": "11", "location": "Indiana", "name": "Pacers",
                      "abbreviation": "IND", "displayName": "Indiana Pacers",
                      "shortDisplayName": "Pacers",
                      "logo": "https://a.espncdn.com/i/teamlogos/nba/500/scoreboard/ind.png"},
             "records": %s},
            {"id": "25", "homeAway": "away", "score": "%s",
             "team": {"id": "25", "location": "Oklahoma City", "name": "Thunder",
                      "abbreviation": "OKC", "displayName": "Oklahoma City Thunder",
                      "shortDisplayName": "Thunder",
                      "logo": "https://a.espncdn.com/i/teamlogos/nba/500/scoreboard/okc.png"},
             "records": %s}
          ]
        }]
      }]
    }`, date, f.clock, f.period, f.status, f.state, f.done, date, f.homeSc, records, f.awaySc, records)
}

func decodeNBA(t *testing.T, f nbaFixture) Game {
	t.Helper()
	p := newESPN("", espnNBA)
	games, err := p.decode([]byte(f.doc()))
	if err != nil {
		t.Fatalf("%s: decode: %v", f.name, err)
	}
	if len(games) != 1 {
		t.Fatalf("%s: got %d games, want 1", f.name, len(games))
	}
	return games[0]
}

// The NBA has no weeks. The card header must not invent one, and it must not
// borrow the NFL's.
func TestNBAHasNoWeekAndDoesNotBorrowOne(t *testing.T) {
	g := decodeNBA(t, nbaFixture{name: "final", status: "STATUS_FINAL", state: "post",
		period: 4, clock: "0.0", done: true, homeSc: "119", awaySc: "135", records: true})
	if g.WeekLabel != "" {
		t.Errorf("NBA game carries week label %q — the NBA scoreboard has no week object", g.WeekLabel)
	}
	if g.League != "nba" || g.LeagueLabel != "NBA" {
		t.Errorf("league %q/%q", g.League, g.LeagueLabel)
	}

	// The gate is the league's own answer, not the absence of the field: a
	// document that grows a week for basketball still must not print one.
	p := newESPN("", espnNBA)
	withWeek := strings.Replace(g0NBAWeekDoc, "\n", "", -1)
	games, err := p.decode([]byte(withWeek))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if games[0].WeekLabel != "" {
		t.Errorf("a stray upstream week printed on an NBA card: %q", games[0].WeekLabel)
	}

	// And the NFL still has one, from the identical decoder.
	nfl := decodeFixture(t, everyState[0].fixture)
	if nfl.WeekLabel != "Week 1" {
		t.Errorf("NFL week label %q, want %q", nfl.WeekLabel, "Week 1")
	}
}

const g0NBAWeekDoc = `{"season":{"year":2026},"week":{"number":3},"events":[{"id":"401810500","date":"2025-10-24T23:30Z","status":{"displayClock":"0.0","period":4,"type":{"id":"3","name":"STATUS_FINAL","state":"post","completed":true}},"competitions":[{"id":"401810500","date":"2025-10-24T23:30Z","competitors":[{"id":"11","homeAway":"home","score":"119","team":{"id":"11","abbreviation":"IND","displayName":"Indiana Pacers","shortDisplayName":"Pacers"}},{"id":"25","homeAway":"away","score":"135","team":{"id":"25","abbreviation":"OKC","displayName":"Oklahoma City Thunder","shortDisplayName":"Thunder"}}]}]}]}`

// Multiple overtimes are normal in basketball. The upstream reports the second
// one as period 6 and prints "Final/2OT"; this lane derives the same string
// from the period and the league's regulation length, and never from the
// upstream's text.
func TestNBAMultipleOvertimes(t *testing.T) {
	cases := []struct {
		period      int
		wantLabel   string
		wantSpoken  string
		wantOvertim int
	}{
		{4, "Final", "Final", 0},
		{5, "Final/OT", "Final after overtime", 1},
		{6, "Final/2OT", "Final after 2 overtime periods", 2},
		{7, "Final/3OT", "Final after 3 overtime periods", 3},
	}
	for _, tc := range cases {
		g := decodeNBA(t, nbaFixture{name: "final", status: "STATUS_FINAL", state: "post",
			period: tc.period, clock: "0.0", done: true, homeSc: "135", awaySc: "141", records: true})
		if got := g.Overtimes(); got != tc.wantOvertim {
			t.Errorf("period %d: overtimes %d, want %d", tc.period, got, tc.wantOvertim)
		}
		if got := g.StatusLabel(); got != tc.wantLabel {
			t.Errorf("period %d: label %q, want %q", tc.period, got, tc.wantLabel)
		}
		if got := g.SpokenStatus(); got != tc.wantSpoken {
			t.Errorf("period %d: spoken %q, want %q", tc.period, got, tc.wantSpoken)
		}
	}
}

// Basketball is played in quarters and read aloud as quarters, and its live
// chip is the same shorthand the football card uses.
func TestNBALiveStatesReadCorrectly(t *testing.T) {
	cases := []struct {
		name       string
		status     string
		state      string
		period     int
		clock      string
		wantState  State
		wantLabel  string
		wantSpoken string
	}{
		{"third quarter", "STATUS_IN_PROGRESS", "in", 3, "8:42", StateInProgress, "8:42 · 3rd", "third quarter, 8:42 remaining"},
		{"first overtime", "STATUS_IN_PROGRESS", "in", 5, "2:11", StateInProgress, "2:11 · OT", "overtime, 2:11 remaining"},
		{"second overtime", "STATUS_IN_PROGRESS", "in", 6, "0:48", StateInProgress, "0:48 · 2OT", "overtime period 2, 0:48 remaining"},
		{"halftime", "STATUS_HALFTIME", "in", 2, "0.0", StateHalftime, "Halftime", "Halftime"},
		{"end of first", "STATUS_END_PERIOD", "in", 1, "0.0", StateEndPeriod, "End 1st", "End of first quarter"},
		// A basketball-only status this build has never seen must still land on
		// something truthful, off the coarse state, rather than on "unknown".
		{"a status name added later", "STATUS_SOMETHING_NEW", "in", 3, "5:00", StateInProgress, "5:00 · 3rd", "third quarter, 5:00 remaining"},
	}
	for _, tc := range cases {
		g := decodeNBA(t, nbaFixture{name: tc.name, status: tc.status, state: tc.state,
			period: tc.period, clock: tc.clock, homeSc: "88", awaySc: "91", records: true})
		if g.State != tc.wantState {
			t.Errorf("%s: state %q, want %q", tc.name, g.State, tc.wantState)
		}
		if got := g.StatusLabel(); got != tc.wantLabel {
			t.Errorf("%s: label %q, want %q", tc.name, got, tc.wantLabel)
		}
		if got := g.SpokenStatus(); got != tc.wantSpoken {
			t.Errorf("%s: spoken %q, want %q", tc.name, got, tc.wantSpoken)
		}
		if !g.HasScore {
			t.Errorf("%s: a game being played reports no score", tc.name)
		}
		// Basketball has no down and distance. The situation Facet must not be
		// mounted for it at all.
		if g.HasSituation() {
			t.Errorf("%s: a basketball game reports a down-and-distance situation", tc.name)
		}
	}
}

// Records are populated from the identical field for both leagues, and their
// absence is absence — never "0-0".
func TestNBARecordsComeFromTheSameFieldAsTheNFLs(t *testing.T) {
	with := decodeNBA(t, nbaFixture{name: "regular season", status: "STATUS_FINAL", state: "post",
		period: 4, clock: "0.0", done: true, homeSc: "119", awaySc: "135", records: true})
	if with.Home.Team.Record != "20-17" || with.Away.Team.Record != "20-17" {
		t.Errorf("records %q / %q, want the type=total summary", with.Home.Team.Record, with.Away.Team.Record)
	}
	// A preseason document carries no records at all.
	without := decodeNBA(t, nbaFixture{name: "preseason", status: "STATUS_SCHEDULED", state: "pre",
		clock: "0.0", homeSc: "0", awaySc: "0"})
	if without.Home.Team.Record != "" || without.Away.Team.Record != "" {
		t.Errorf("invented records %q / %q on a document that carried none",
			without.Home.Team.Record, without.Away.Team.Record)
	}
	if without.HasScore {
		t.Error("a scheduled basketball game reports a score")
	}
}

// A league's start times print in that league's zone, and the zone lives in the
// league table so a future non-US league cannot inherit Eastern by default.
func TestStartTimesPrintInTheLeaguesOwnZone(t *testing.T) {
	g := decodeNBA(t, nbaFixture{name: "scheduled", status: "STATUS_SCHEDULED", state: "pre",
		clock: "0.0", homeSc: "0", awaySc: "0", date: "2026-10-03T23:00Z"})
	// 2026-10-03T23:00Z is Saturday 7:00 PM Eastern Daylight Time.
	if got := g.StartLabel(); got != "Sat 7:00 PM EDT" {
		t.Fatalf("NBA start label %q, want %q", got, "Sat 7:00 PM EDT")
	}
	if got := g.SpokenStatus(); got != "Starts Sat 7:00 PM EDT" {
		t.Errorf("spoken start %q — a basketball game does not have a kickoff", got)
	}

	// A league that stated no zone says UTC and labels it UTC. It never borrows
	// the zone of the league next to it in the table.
	unknown := g
	unknown.League = "kbl"
	if got := unknown.StartLabel(); got != "Sat 11:00 PM UTC" {
		t.Fatalf("unknown league start label %q, want %q", got, "Sat 11:00 PM UTC")
	}
}

// A fixture weeks out is the normal case for a league between seasons, and a
// bare weekday cannot identify it. The date appears; and the decision is made
// against the moment the upstream was read, never against the wall clock, so
// one snapshot always renders to one fragment.
func TestAFarOffFixtureCarriesItsDate(t *testing.T) {
	g := decodeNBA(t, nbaFixture{name: "scheduled", status: "STATUS_SCHEDULED", state: "pre",
		clock: "0.0", homeSc: "0", awaySc: "0", date: "2026-10-03T23:00Z"})

	g.Observed = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	if got := g.StartLabel(); got != "Oct 3, 7:00 PM EDT" {
		t.Errorf("a fixture a month out reads %q — three letters of weekday name it no better than nothing", got)
	}
	g.Observed = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if got := g.StartLabel(); got != "Sat 7:00 PM EDT" {
		t.Errorf("a fixture two days out reads %q, want the weekday form", got)
	}
	// The boundary is a week to the minute: at six days and change a weekday
	// names one day, and at seven it names today as well.
	g.Observed = g.Start.Add(-startDateAfter + time.Minute)
	if got := g.StartLabel(); got != "Sat 7:00 PM EDT" {
		t.Errorf("a fixture a minute inside a week reads %q, want the weekday form", got)
	}
	g.Observed = g.Start.Add(-startDateAfter)
	if got := g.StartLabel(); got != "Oct 3, 7:00 PM EDT" {
		t.Errorf("a fixture exactly a week out reads %q — that weekday is today's too", got)
	}

	// Same snapshot, same fragment, every time it is rendered.
	first := g.StartLabel()
	for i := 0; i < 3; i++ {
		if got := g.StartLabel(); got != first {
			t.Fatalf("the same game rendered two different labels: %q then %q", first, got)
		}
	}
}

// The rail is one ranked list across every league. Live games come first
// wherever they are being played, then whatever starts soonest — and a league
// that is between seasons simply ranks last and contributes nothing to the top
// of the list, without any per-league scaffolding to be empty.
func TestCrossLeagueOrderIsLiveFirstThenSoonest(t *testing.T) {
	now := time.Now()
	games := []Game{
		{ID: "nfl-sunday", League: "nfl", State: StateScheduled, Start: now.Add(50 * time.Hour)},
		{ID: "nba-late", League: "nba", State: StateScheduled, Start: now.Add(3 * time.Hour)},
		{ID: "nfl-final", League: "nfl", State: StateFinal, Start: now.Add(-4 * time.Hour)},
		{ID: "nba-live", League: "nba", State: StateInProgress, Start: now.Add(-time.Hour)},
		{ID: "nfl-live", League: "nfl", State: StateInProgress, Start: now.Add(-2 * time.Hour)},
		{ID: "nfl-soon", League: "nfl", State: StateScheduled, Start: now.Add(time.Hour)},
	}
	sort.SliceStable(games, func(i, j int) bool { return less(games[i], games[j]) })
	want := []string{"nfl-live", "nba-live", "nfl-soon", "nba-late", "nfl-sunday", "nfl-final"}
	if got := ids(games); !equalStrings(got, want) {
		t.Fatalf("cross-league order %v, want %v", got, want)
	}
}

// Two games starting on the same minute must not shuffle between renders. The
// tie breaks on the league's table position and then the upstream id, both of
// which are fixed.
func TestCrossLeagueOrderIsStable(t *testing.T) {
	at := time.Now().Add(time.Hour)
	base := []Game{
		{ID: "9", League: "nba", State: StateScheduled, Start: at},
		{ID: "2", League: "nfl", State: StateScheduled, Start: at},
		{ID: "1", League: "nba", State: StateScheduled, Start: at},
		{ID: "7", League: "nfl", State: StateScheduled, Start: at},
	}
	var first []string
	for round := 0; round < 5; round++ {
		games := append([]Game(nil), base...)
		// A different input order each round — the upstream does not promise one.
		games[0], games[round%len(games)] = games[round%len(games)], games[0]
		sort.SliceStable(games, func(i, j int) bool { return less(games[i], games[j]) })
		got := ids(games)
		if first == nil {
			first = got
			continue
		}
		if !equalStrings(got, first) {
			t.Fatalf("the panel reshuffled between renders: %v then %v", first, got)
		}
	}
	// Football sorts above basketball at an equal start time because it sits
	// above it in the table, and ids order within a league.
	want := []string{"2", "7", "1", "9"}
	if !equalStrings(first, want) {
		t.Fatalf("tie-break order %v, want %v", first, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A whole league being out of season costs nothing: no rows, no header, and a
// poll cadence measured in hours.
func TestAnOutOfSeasonLeagueContributesNothingAndCostsAlmostNothing(t *testing.T) {
	now := time.Now()
	c := seedCache(t, map[string][]Game{
		"nfl": {{ID: "1", State: StateScheduled, Start: now.Add(2 * time.Hour)}},
		"nba": nil,
	})
	got := c.Get(0)
	if len(got) != 1 || got[0].League != "nfl" {
		t.Fatalf("merged slate %v — an empty league contributed something", ids(got))
	}
	if got := c.LeagueInterval("nba"); got != DormantInterval {
		t.Errorf("an empty slate is polled every %s, want %s", got, DormantInterval)
	}
	// Both leagues out of season: nothing at all, which is the rail's empty
	// state — no panel rather than an empty one.
	c = seedCache(t, map[string][]Game{"nfl": nil, "nba": nil})
	if got := c.Get(0); len(got) != 0 {
		t.Fatalf("two empty leagues produced %d games", len(got))
	}
}

// The lane must not double the upstream load just because it grew a league.
// Each league is asked once per refresh, and never on another league's clock.
func TestOneRequestPerLeaguePerRefresh(t *testing.T) {
	var hits sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := hits.LoadOrStore(r.URL.Path, new(int64))
		atomic.AddInt64(n.(*int64), 1)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "basketball") {
			_, _ = w.Write([]byte(nbaFixture{name: "final", status: "STATUS_FINAL", state: "post",
				period: 6, clock: "0.0", done: true, homeSc: "135", awaySc: "141", records: true}.doc()))
			return
		}
		_, _ = w.Write([]byte(everyState[1].doc()))
	}))
	defer srv.Close()

	ps, err := NewProviders("espn", "", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCache(ps...)
	c.Refresh(context.Background())
	c.Refresh(context.Background())

	for _, path := range []string{"/football/nfl/scoreboard", "/basketball/nba/scoreboard"} {
		n, ok := hits.Load(path)
		if !ok {
			t.Fatalf("%s was never requested", path)
		}
		if got := atomic.LoadInt64(n.(*int64)); got != 2 {
			t.Errorf("%s hit %d times across 2 refreshes, want 2", path, got)
		}
	}
	if _, _, fetches := c.Health(); fetches != 4 {
		t.Errorf("fetches=%d, want 4 (two leagues, two refreshes)", fetches)
	}

	// Both leagues land in one merged snapshot, addressable by id.
	if got := c.Get(0); len(got) != 2 {
		t.Fatalf("merged slate has %d games, want 2", len(got))
	}
	if g, ok := c.Game("401810500"); !ok || g.League != "nba" {
		t.Error("the basketball game is not addressable in the merged snapshot")
	}
	if g, ok := c.Game("401872656"); !ok || g.League != "nfl" {
		t.Error("the football game is not addressable in the merged snapshot")
	}
	if got := c.GetLeague("nba", 0); len(got) != 1 || got[0].League != "nba" {
		t.Errorf("per-league read returned %v", ids(got))
	}
	// One live football game must not have dragged basketball onto the fast
	// cadence — that is the whole reason the lanes are separate.
	if got := c.LeagueInterval("nfl"); got != LiveInterval {
		t.Errorf("nfl cadence %s, want %s", got, LiveInterval)
	}
	if got := c.LeagueInterval("nba"); got == LiveInterval {
		t.Error("basketball inherited football's live cadence")
	}
}

// The poller asks only the leagues whose own schedule says it is time.
func TestThePollerSkipsALeagueThatIsNotDue(t *testing.T) {
	var nfl, nba int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "basketball") {
			atomic.AddInt64(&nba, 1)
			// Between seasons: nothing at all.
			_, _ = w.Write([]byte(`{"season":{"year":2027},"events":[]}`))
			return
		}
		atomic.AddInt64(&nfl, 1)
		_, _ = w.Write([]byte(everyState[1].doc()))
	}))
	defer srv.Close()

	ps, _ := NewProviders("espn", "", srv.URL)
	c := NewCache(ps...)
	now := time.Now()
	c.refreshDue(context.Background(), now)
	if atomic.LoadInt64(&nfl) != 1 || atomic.LoadInt64(&nba) != 1 {
		t.Fatalf("first poll asked nfl=%d nba=%d, want 1 each", nfl, nba)
	}
	// A minute later, only the live league is due.
	c.refreshDue(context.Background(), now.Add(time.Minute))
	if got := atomic.LoadInt64(&nfl); got != 2 {
		t.Errorf("football asked %d times, want 2 — it has a game in progress", got)
	}
	if got := atomic.LoadInt64(&nba); got != 1 {
		t.Errorf("basketball asked %d times a minute into a %s interval, want 1", got, DormantInterval)
	}
}

// The poller must not sleep past a start time it already knows about. A whistle
// twenty seconds away and a one-minute interval means arriving forty seconds
// into a game the surface exists to watch.
func TestThePollerDoesNotSleepPastAKnownStart(t *testing.T) {
	now := time.Now()
	cases := []struct {
		until time.Duration
		want  time.Duration
	}{
		{20 * time.Second, 20*time.Second + startGrace},
		{45 * time.Second, 45*time.Second + startGrace},
		{2 * time.Minute, SoonInterval},
		{14 * time.Minute, SoonInterval},
	}
	for _, tc := range cases {
		c := seedCache(t, map[string][]Game{"nfl": {
			{ID: "1", State: StateScheduled, Start: now.Add(tc.until)},
		}})
		got := c.LeagueInterval("nfl")
		// LeagueInterval reads its own clock, so allow the test's own drift.
		if got > tc.want || got < tc.want-2*time.Second {
			t.Errorf("start in %s: interval %s, want about %s", tc.until, got, tc.want)
		}
	}
}

// The platform health figure is a floor, not a flattering maximum: one league
// that has never answered makes the whole lane's "last update" read never,
// whatever the league beside it has managed.
func TestPlatformHealthReportsTheOldestReading(t *testing.T) {
	now := time.Now()
	c := seedCache(t, map[string][]Game{
		"nfl": {{ID: "1", State: StateScheduled, Start: now.Add(time.Hour)}},
		"nba": {{ID: "2", State: StateScheduled, Start: now.Add(2 * time.Hour)}},
	})
	byslug := map[string]*lane{}
	for _, l := range c.lanes {
		byslug[l.league.Slug] = l
	}
	byslug["nfl"].lastOK = now.Add(-9 * time.Minute)
	byslug["nba"].lastOK = now.Add(-1 * time.Minute)
	byslug["nfl"].fetches, byslug["nba"].fetches = 40, 3

	lastOK, _, fetches := c.Health()
	if !lastOK.Equal(byslug["nfl"].lastOK) {
		t.Errorf("platform last_ok %s, want the oldest league's %s", lastOK, byslug["nfl"].lastOK)
	}
	if fetches != 43 {
		t.Errorf("fetches %d, want the sum across leagues", fetches)
	}

	// A league that has never answered is the floor, whichever lane it is.
	for _, order := range []string{"nfl", "nba"} {
		byslug["nfl"].lastOK, byslug["nba"].lastOK = now, now
		byslug[order].lastOK = time.Time{}
		if lastOK, _, _ := c.Health(); !lastOK.IsZero() {
			t.Errorf("%s has never answered and the platform reports last_ok %s", order, lastOK)
		}
	}
}

// TestSportsLaneIsAlwaysOn pins the rule that no configuration can leave the
// platform without game cards. Every shape of input yields providers; the
// keyed provider is chosen when the key exists, the keyless one otherwise, and
// a misconfiguration is reported without emptying the lane.
func TestSportsLaneIsAlwaysOn(t *testing.T) {
	cases := []struct {
		name, provider, key string
		wantName            string
		wantErr             bool
	}{
		{"blank with key is big balls", "", "bbs_live_test", "bigballs", false},
		{"blank without key is espn", "", "", "espn", false},
		{"espn named", "espn", "bbs_live_test", "espn", false},
		{"bigballs named with key", "bigballs", "bbs_live_test", "bigballs", false},
		{"bigballs named without key still serves", "bigballs", "", "espn", true},
		{"unknown name still serves", "nonsense", "", "espn", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			providers, err := NewProviders(tc.provider, tc.key, "")
			if len(providers) == 0 {
				t.Fatalf("sports lane came back empty — the lane must never be off")
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got := providers[0].Name(); !strings.Contains(strings.ToLower(got), tc.wantName) {
				t.Fatalf("provider = %q, want %q", got, tc.wantName)
			}
		})
	}
}
