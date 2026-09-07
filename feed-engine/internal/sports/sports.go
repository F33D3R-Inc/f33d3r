// Package sports is the server-side source of truth for live game data.
//
// It owns one poller for the whole platform — never one per viewer, never one
// per open page, and never one per league. The poller writes a snapshot into an
// in-memory cache; every Facet renderer reads that snapshot. When the snapshot
// changes, the cache hands the previous and the next snapshot to a mutation
// sink, which re-renders the affected Facets and pushes the finished fragments
// over FA Live.
//
// Nothing in this package renders HTML and nothing here talks to the browser.
// It normalises an upstream feed into a league-neutral Game, computes every
// label the surface needs (so no clock, quarter or kickoff time is ever
// formatted in a browser), and reports honestly when the upstream went quiet.
//
// The lane is provider-driven. SPORTS_PROVIDER blank means the lane does not
// exist: no poller, no strip, no rail panel, no pages. See provider.go.
package sports

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	// tzdata is embedded so kickoff labels resolve a league's own zone inside a
	// scratch container that ships no zoneinfo. A kickoff printed in the wrong
	// zone is a lie, and falling back to UTC silently would be exactly that —
	// which is why the fallback below relabels itself when it happens.
	_ "time/tzdata"
)

// ── League table ─────────────────────────────────────────────────────────────
//
// A league is not a label. Adding basketball to a lane written for football
// surfaced four things that were true of the NFL and of nothing else, and every
// one of them was hiding in a package-level constant:
//
//   - REGULATION LENGTH. "Overtime" was `period > 4`. The NFL and the NBA both
//     play four periods, which is a coincidence, not a rule: ESPN drives a
//     ten-inning baseball game off the same period field and labels it
//     "Final/10". A constant here would be wrong the first time a third sport
//     arrives, and wrong silently.
//   - WHAT A PERIOD IS CALLED. "third quarter" is right for both leagues here
//     and wrong for a hockey period, a soccer half or a baseball inning.
//   - WEEKS. "Week 1" is in the NFL card header. Verified against the upstream:
//     the NBA scoreboard document carries no week object at all — the NBA has
//     no weekly bucket, and nothing replaces it, because a basketball schedule
//     is a calendar of nights. So the model does not assume a week exists; a
//     league states whether it has one and the label is absent otherwise.
//   - THE ZONE FIXTURE TIMES PRINT IN. Eastern is right for a US league and
//     wrong for every other one. It now belongs to the league, so a future
//     non-US league is not wrong by default.
//
// Everything above is stated once, here, and read by the model. Nothing else in
// this package knows a sport's name.

// League is everything about a competition its games cannot be rendered
// correctly without.
type League struct {
	// Slug is the league's entity id in facet ids and URLs ("nfl").
	Slug string
	// Label is the league as a reader sees it ("NFL").
	Label string

	// Regulation is how many periods make a complete game. Anything beyond it is
	// overtime. Zero means the league did not say, and a game in a league that
	// did not say is never reported as having gone to overtime — an unknown is
	// answered with silence, never with a guess.
	Regulation int

	// PeriodNoun is what this league calls one period, in the singular
	// ("quarter"). It is what a screen reader hears: "third quarter".
	PeriodNoun string

	// Weeks reports whether the league buckets its schedule into numbered weeks.
	// False means a week label is never printed for this league even if an
	// upstream document grows one.
	Weeks bool

	// Zone is the zone this league's fixture times are printed in, explicitly
	// labelled with the abbreviation in force on the day. Nil prints UTC and
	// says UTC.
	Zone *time.Location

	// Order is the league's place in the table. It decides which league's head
	// lane sits above the other when neither has anything live, and it breaks
	// ties in the cross-league rail so the panel is stable between renders.
	Order int
}

// zone is the league's zone, or UTC when it has none. The label always names
// the zone actually used, so the fallback cannot become a lie.
func (l League) zone() *time.Location {
	if l.Zone == nil {
		return time.UTC
	}
	return l.Zone
}

// The table. Both leagues here are US leagues that play four quarters; that is
// stated twice on purpose, because it is two facts about two leagues and not
// one fact about the package.
var (
	leagueNFL = League{
		Slug: "nfl", Label: "NFL",
		Regulation: 4, PeriodNoun: "quarter", Weeks: true,
		Zone: mustZone("America/New_York"), Order: 1,
	}
	leagueNBA = League{
		Slug: "nba", Label: "NBA",
		Regulation: 4, PeriodNoun: "quarter", Weeks: false,
		Zone: mustZone("America/New_York"), Order: 2,
	}

	// leagueTable is the whole table, in display order.
	leagueTable = []League{leagueNFL, leagueNBA}

	leagueBySlug = func() map[string]League {
		m := make(map[string]League, len(leagueTable))
		for _, l := range leagueTable {
			m[l.Slug] = l
		}
		return m
	}()
)

// LeagueOf resolves a slug to its table entry. An unknown slug yields the zero
// League, whose every answer is "did not say" rather than a football default.
func LeagueOf(slug string) League { return leagueBySlug[strings.ToLower(strings.TrimSpace(slug))] }

// KnownLeague reports whether the slug names a league this build carries.
func KnownLeague(slug string) bool {
	_, ok := leagueBySlug[strings.ToLower(strings.TrimSpace(slug))]
	return ok
}

func mustZone(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		// tzdata is embedded above, so this is unreachable in a built binary.
		// UTC keeps the process alive; the kickoff label names the zone it
		// actually used, so the surface stays truthful either way.
		log.Printf("[sports] zone %s unavailable (%v), kickoff labels fall back to UTC", name, err)
		return time.UTC
	}
	return loc
}

// ── State ────────────────────────────────────────────────────────────────────

// State is the league-neutral condition of a game. Every state a sport actually
// has is named here; an upstream value that maps to none of them becomes
// StateUnknown, which renders as "Status unavailable" rather than as a guess.
type State string

const (
	StateScheduled  State = "scheduled"
	StateInProgress State = "in_progress"
	StateHalftime   State = "halftime"
	StateEndPeriod  State = "end_period"
	StateDelayed    State = "delayed"
	StatePostponed  State = "postponed"
	StateCanceled   State = "canceled"
	StateFinal      State = "final"
	StateUnknown    State = "unknown"
)

// Active reports whether the game is being played right now. Active games are
// what drive the poller to its fast cadence.
func (s State) Active() bool {
	switch s {
	case StateInProgress, StateHalftime, StateEndPeriod:
		return true
	}
	return false
}

// Settled reports whether the game will never change again. A settled game is
// worth caching hard and worth never pushing a mutation for.
func (s State) Settled() bool {
	switch s {
	case StateFinal, StatePostponed, StateCanceled:
		return true
	}
	return false
}

// ── Model ────────────────────────────────────────────────────────────────────

// Team is one club. Record is the pre-formatted win-loss summary ("2-1" in the
// NFL, "12-4" in the NBA — the upstream carries it identically for both, as a
// records entry of type "total").
type Team struct {
	ID        string
	Name      string // "Seattle Seahawks"
	ShortName string // "Seahawks"
	Abbr      string // "SEA"
	LogoURL   string
	Record    string // "0-0", "" when the upstream did not say
}

// Label is the name a card shows: the abbreviation when there is one, so the
// two rows of a card align, and the full name otherwise.
func (t Team) Label() string {
	if t.Abbr != "" {
		return t.Abbr
	}
	if t.ShortName != "" {
		return t.ShortName
	}
	return t.Name
}

// Side is one team's participation in one game.
type Side struct {
	Team     Team
	Score    int
	HasBall  bool // possession, only meaningful while the game is being played
	Timeouts int
}

// Game is one contest, normalised. Every display string on it is computed
// server-side; the browser receives these already rendered into a fragment.
type Game struct {
	ID          string // upstream event id — the Facet entity id
	League      string // "nfl" — the league slug, also the strip's entity id
	LeagueLabel string // "NFL"
	SeasonYear  int
	WeekLabel   string // "Week 1", "" in a league that has no weeks

	// Start is when the game begins — a kickoff, a tip-off or a first pitch
	// depending on the sport, which is why it is not named after any of them.
	// Always UTC; StartLabel prints it in the league's own zone.
	Start  time.Time
	State  State
	Period int    // 1..Regulation, then overtime, 0 before the game starts
	Clock  string // "8:42" — upstream's display clock, never recomputed here

	Home Side
	Away Side
	// HasScore is false before a game starts. A scheduled game showing 0–0 is a
	// lie: nobody has failed to score yet.
	HasScore bool

	// Situation — only populated while a football game is being played. The NBA
	// document carries no down, distance or red zone, so these stay empty and
	// HasSituation reports false: the situation Facet is simply not rendered for
	// a basketball game rather than rendered blank.
	RedZone        bool
	DownDistance   string // "3rd & 7"
	PossessionText string // "SEA 45"
	LastPlay       string

	Broadcast string // "NBC"
	Venue     string // "Lumen Field"
	VenueCity string // "Seattle, WA"

	// Observed is when this game was last read from the upstream. Staleness is
	// measured against it, so a card can say "may be out of date" instead of
	// presenting a frozen score as current — and it is also the only "now" this
	// model is allowed to compare a start time against. See StartLabel.
	Observed time.Time
}

// league is the table entry this game belongs to.
func (g Game) league() League { return LeagueOf(g.League) }

// Overtime reports whether the game went past regulation. A league that did not
// state its regulation length never reports overtime: the honest answer to "did
// this go to overtime" without knowing how long a game is, is nothing.
func (g Game) Overtime() bool {
	reg := g.league().Regulation
	return reg > 0 && g.Period > reg
}

// Overtimes is how many extra periods have been played. Zero in regulation.
// NBA games reach 2OT and 3OT routinely; the upstream numbers them 6 and 7 off
// the same period field and labels them "Final/2OT", so this counts rather than
// flagging.
func (g Game) Overtimes() int {
	if !g.Overtime() {
		return 0
	}
	return g.Period - g.league().Regulation
}

// PeriodLabel is the scoreboard shorthand for the current period: "1st".."4th"
// in a four-period league, then "OT", "2OT", "3OT".
func (g Game) PeriodLabel() string {
	return periodLabel(g.Period, g.league().Regulation)
}

func periodLabel(p, regulation int) string {
	switch {
	case p <= 0:
		return ""
	case regulation > 0 && p > regulation:
		if ot := p - regulation; ot > 1 {
			return strconv.Itoa(ot) + "OT"
		}
		return "OT"
	}
	return ordinal(p)
}

// ordinal is the English ordinal for a period number. It is written out rather
// than tabled because a league with more periods than a table anticipated must
// still read correctly.
func ordinal(n int) string {
	suffix := "th"
	switch {
	case n%100 >= 11 && n%100 <= 13:
	case n%10 == 1:
		suffix = "st"
	case n%10 == 2:
		suffix = "nd"
	case n%10 == 3:
		suffix = "rd"
	}
	return strconv.Itoa(n) + suffix
}

// StartLabel is the pre-game time, in the league's own zone, explicitly
// labelled: "Thu 8:20 PM ET".
//
// A fixture far enough out carries its date as well, because "Sat 7:00 PM EDT"
// for a game four weeks away is not a time, it is a riddle — and that is the
// normal case for a league in its offseason, whose next fixture is the only
// thing it has to show. "Far enough out" is measured against Observed, the
// moment the upstream was actually read, and NEVER against time.Now(): a
// renderer that consults the wall clock produces a different fragment for the
// same snapshot on two calls, which is precisely the nondeterminism this lane
// is built not to have. A Game with no Observed has no honest "now" to compare
// against and gets the plain label.
func (g Game) StartLabel() string {
	if g.Start.IsZero() {
		return ""
	}
	local := g.Start.In(g.league().zone())
	abbr, _ := local.Zone()
	if abbr == "" {
		abbr = "UTC"
	}
	if !g.Observed.IsZero() && g.Start.Sub(g.Observed) >= startDateAfter {
		return local.Format("Jan 2, 3:04 PM") + " " + abbr
	}
	return local.Format("Mon 3:04 PM") + " " + abbr
}

// startDateAfter is how far out a fixture has to be before its weekday stops
// identifying it. Seven days exactly: inside a week "Sun 1:00 PM" names one
// Sunday, and at seven days it names the same weekday as today — after that it
// is the same three letters as a game a month later, which is the normal case
// for a league between seasons.
const startDateAfter = 7 * 24 * time.Hour

// StatusLabel is the short chip a card shows for the game's condition. It is the
// only place a state becomes words, so the strip, the rail, the card and the
// game page can never disagree about what a game is doing.
func (g Game) StatusLabel() string {
	switch g.State {
	case StateScheduled:
		if l := g.StartLabel(); l != "" {
			return l
		}
		return "Scheduled"
	case StateInProgress:
		switch {
		case g.Clock != "" && g.PeriodLabel() != "":
			return g.Clock + " · " + g.PeriodLabel()
		case g.PeriodLabel() != "":
			return g.PeriodLabel()
		default:
			return "In progress"
		}
	case StateHalftime:
		return "Halftime"
	case StateEndPeriod:
		if l := g.PeriodLabel(); l != "" {
			return "End " + l
		}
		return "End of period"
	case StateDelayed:
		return "Delayed"
	case StatePostponed:
		return "Postponed"
	case StateCanceled:
		return "Canceled"
	case StateFinal:
		// "Final/OT" and "Final/2OT" — the count comes from the period, and it
		// matches what the upstream itself prints for the same game.
		if g.Overtime() {
			return "Final/" + g.PeriodLabel()
		}
		return "Final"
	default:
		return "Status unavailable"
	}
}

// Live reports whether the card should carry the live treatment (pulse dot,
// accent status). Halftime and between-quarters count: the game is on.
func (g Game) Live() bool { return g.State.Active() }

// Final reports whether the game is over and the score is the result.
func (g Game) Final() bool { return g.State == StateFinal }

// Matchup is the pairing as a sentence fragment: "New England at Seattle".
func (g Game) Matchup() string {
	away := g.Away.Team.Name
	if away == "" {
		away = g.Away.Team.Label()
	}
	home := g.Home.Team.Name
	if home == "" {
		home = g.Home.Team.Label()
	}
	return away + " at " + home
}

// SpokenStatus is the game's condition as a sentence, for a screen reader. The
// visible chip is a scoreboard abbreviation ("8:42 · 3rd"); read aloud that is
// noise, so every surface pairs the chip with this.
//
// It lives on the model rather than in markup so the strip, the rail, the hub
// and the game page cannot drift into describing the same game differently, and
// so a replacement of the clock Facet always carries its own spoken text — a
// summary sentence held one level up would go stale the moment an atomic below
// it was replaced.
func (g Game) SpokenStatus() string {
	l := g.league()
	switch g.State {
	case StateScheduled:
		if lbl := g.StartLabel(); lbl != "" {
			return "Starts " + lbl
		}
		return "Scheduled"
	case StateInProgress:
		if g.PeriodLabel() == "" {
			return "In progress"
		}
		s := spokenPeriod(g.Period, l)
		if g.Clock != "" {
			s += ", " + g.Clock + " remaining"
		}
		if g.RedZone {
			s += ", red zone"
		}
		return s
	case StateHalftime:
		return "Halftime"
	case StateEndPeriod:
		if g.Period > 0 {
			return "End of " + spokenPeriod(g.Period, l)
		}
		return "End of period"
	case StateDelayed:
		return "Delayed"
	case StatePostponed:
		return "Postponed"
	case StateCanceled:
		return "Canceled"
	case StateFinal:
		switch n := g.Overtimes(); {
		case n == 1:
			return "Final after overtime"
		case n > 1:
			return fmt.Sprintf("Final after %d overtime periods", n)
		}
		return "Final"
	default:
		return "Status unavailable"
	}
}

// NoScoreReason says why a card is showing no score. "The game has not started"
// is true of a scheduled game and false of a canceled one, and a screen reader
// must not be told the wrong one.
func (g Game) NoScoreReason() string {
	switch g.State {
	case StateScheduled:
		return "the game has not started"
	case StatePostponed:
		return "the game was postponed"
	case StateCanceled:
		return "the game was canceled"
	default:
		return "no score has been reported"
	}
}

// PossessionLabel names the team with the ball, or "" when nobody does.
func (g Game) PossessionLabel() string {
	switch {
	case g.Home.HasBall:
		return g.Home.Team.Label()
	case g.Away.HasBall:
		return g.Away.Team.Label()
	}
	return ""
}

// spokenPeriod reads a period aloud in the league's own vocabulary: "third
// quarter" in the NFL and the NBA, and whatever a league that plays halves,
// periods or innings calls one, because the noun is league data.
func spokenPeriod(p int, l League) string {
	noun := l.PeriodNoun
	if noun == "" {
		noun = "period"
	}
	switch {
	case p <= 0:
		return "the game"
	case l.Regulation > 0 && p > l.Regulation:
		if ot := p - l.Regulation; ot > 1 {
			return fmt.Sprintf("overtime period %d", ot)
		}
		return "overtime"
	}
	if w := spokenOrdinal(p); w != "" {
		return w + " " + noun
	}
	return fmt.Sprintf("%s %d", noun, p)
}

var spokenOrdinals = []string{
	"", "first", "second", "third", "fourth", "fifth",
	"sixth", "seventh", "eighth", "ninth", "tenth",
}

func spokenOrdinal(n int) string {
	if n > 0 && n < len(spokenOrdinals) {
		return spokenOrdinals[n]
	}
	return ""
}

// Leader reports which side is ahead, for the winner emphasis on a final card.
func (g Game) HomeLeads() bool { return g.HasScore && g.Home.Score > g.Away.Score }
func (g Game) AwayLeads() bool { return g.HasScore && g.Away.Score > g.Home.Score }

// HasSituation reports whether the down-and-distance line has anything to say.
// False for every basketball game, so the situation Facet is not mounted at all
// rather than mounted empty.
func (g Game) HasSituation() bool {
	return g.State == StateInProgress && (g.DownDistance != "" || g.RedZone)
}

// ── Mutation keys ────────────────────────────────────────────────────────────
//
// A poll tick that changed nothing must push nothing. These keys are what the
// cache diffs to decide which Facet actually needs re-rendering, so a ticking
// clock costs one small atomic fragment and a touchdown costs one card.

// ScoreKey changes when either score changes.
func (g Game) ScoreKey() string {
	return fmt.Sprintf("%d:%d:%t", g.Away.Score, g.Home.Score, g.HasScore)
}

// ClockKey changes when the state, period or clock changes.
func (g Game) ClockKey() string {
	return fmt.Sprintf("%s:%d:%s", g.State, g.Period, g.Clock)
}

// SituationKey changes when possession, red zone or down-and-distance changes.
func (g Game) SituationKey() string {
	return fmt.Sprintf("%t:%t:%t:%s:%s",
		g.Home.HasBall, g.Away.HasBall, g.RedZone, g.DownDistance, g.PossessionText)
}

// CardKey changes when anything the whole card renders changes.
func (g Game) CardKey() string {
	return strings.Join([]string{
		g.ScoreKey(), g.ClockKey(), g.SituationKey(),
		g.Away.Team.Record, g.Home.Team.Record, g.Broadcast, g.LastPlay,
	}, "|")
}

// SlateKey identifies the membership and order of a set of games. It changes
// when a game joins, leaves or moves, which is the only time a lane made of
// those games has to be re-rendered as a whole rather than one card inside it.
func SlateKey(games []Game) string {
	ids := make([]string, 0, len(games))
	for _, g := range games {
		ids = append(ids, g.ID)
	}
	return strings.Join(ids, ",")
}

// ── Cadence ──────────────────────────────────────────────────────────────────
//
// One poller serves the whole platform, so these are absolute request rates
// against the upstream, not per-viewer rates. They are applied PER LEAGUE
// against that league's own slate: football being played on a Sunday afternoon
// is no reason to ask basketball anything, and a league in its offseason must
// cost the upstream almost nothing.
const (
	// LiveInterval applies while a game in that league is being played. The
	// upstream scoreboard is CDN-cached with max-age=8, so anything faster than
	// this buys nothing and costs the upstream.
	LiveInterval = 20 * time.Second
	// SoonInterval applies in the window around a start or a whistle.
	SoonInterval = 60 * time.Second
	// TodayInterval applies on a day that has games but none near.
	TodayInterval = 5 * time.Minute
	// IdleInterval applies when the league's next fixture is beyond today's
	// window but still inside a day.
	IdleInterval = 30 * time.Minute
	// DormantInterval applies to a league with nothing within a day — an empty
	// slate, or a next fixture weeks out because the league is between seasons.
	// A schedule that far ahead does not move minute to minute, and a league
	// that is not playing must not be paying for a poller that thinks it might
	// be. Twelve requests a day, against a public CDN document.
	DormantInterval = 2 * time.Hour

	// startWindow is how long before a game starts the poller speeds up.
	startWindow = 15 * time.Minute
	// startGrace is how long after a known start time the poller waits before
	// reading, so it arrives once the upstream's eight-second CDN cache has
	// turned over rather than a moment too early and then again a minute late.
	startGrace = 10 * time.Second
	// dayWindow is how close a fixture has to be, either side of now, for the
	// slate to count as "today's". It is a clock window rather than a calendar
	// day on purpose: a Sunday-noon slate read at eleven on Saturday night is
	// not worth five-minute polling just because a date rolled over, and a game
	// that finished forty minutes ago is worth it even if midnight has passed.
	dayWindow = 8 * time.Hour
	// dormantWindow is how far out a league's nearest fixture has to be before
	// the league is treated as not playing.
	dormantWindow = 24 * time.Hour
	// settleWindow is how long after a whistle the poller stays quick, so a
	// score correction after the final gun still lands.
	settleWindow = 20 * time.Minute

	// LiveStaleAfter is the floor on how long any slate may go without a fresh
	// upstream reading before the surface admits it may be out of date.
	LiveStaleAfter = 2 * time.Minute
)

// staleAfter is how long a league may go unheard-from before its cards must say
// so. It is derived from that league's OWN cadence rather than from a fixed
// limit, because the two are now different by design: a league polled every two
// hours because it is between seasons is not stale thirty-one minutes after its
// last successful read, and a live slate polled every twenty seconds is stale
// long before that. Three consecutive missed polls is an outage; anything less
// is the schedule working.
func staleAfter(interval time.Duration) time.Duration {
	d := 3 * interval
	if d < LiveStaleAfter {
		d = LiveStaleAfter
	}
	return d
}

// leagueInterval is one league's poll delay, derived from its own games.
func leagueInterval(games []Game, lastOK time.Time, lastErr error, now time.Time) time.Duration {
	// A failing upstream is not a reason to hammer it. Back off to the start
	// cadence and keep trying there.
	if lastErr != nil && !lastOK.IsZero() {
		return SoonInterval
	}

	next := DormantInterval
	tighten := func(d time.Duration) {
		if d < next {
			next = d
		}
	}
	for _, g := range games {
		switch {
		case g.State.Active():
			// One game being played pins that league to the fast cadence. There
			// is nothing faster to escalate to, so stop looking.
			return LiveInterval

		case g.State == StateDelayed:
			// A delay ends without warning. Stay close to it.
			tighten(SoonInterval)

		case g.State == StateScheduled:
			if g.Start.IsZero() {
				continue
			}
			d := g.Start.Sub(now)
			switch {
			case d <= 0:
				// The start time has passed and the upstream still says
				// scheduled: it is behind, and the next read is the one that
				// catches up.
				tighten(SoonInterval)
			case d <= startWindow:
				// Never sleep past a start time that is already known. Waiting
				// the full minute when the whistle is twenty seconds away means
				// arriving forty seconds into a game the surface is supposed to
				// be watching. The grace is there because the upstream document
				// is CDN-cached for eight seconds and does not flip the instant
				// the game does.
				if w := d + startGrace; w < SoonInterval {
					tighten(w)
				} else {
					tighten(SoonInterval)
				}
			case d <= dayWindow:
				tighten(TodayInterval)
			case d <= dormantWindow:
				tighten(IdleInterval)
			}

		case g.State.Settled():
			if g.Start.IsZero() {
				continue
			}
			since := now.Sub(g.Start)
			// A score can still be corrected shortly after the whistle.
			if since < 6*time.Hour && now.Sub(g.Observed) < settleWindow {
				tighten(SoonInterval)
			}
			if since >= 0 && since <= dayWindow {
				tighten(TodayInterval)
			}
			if since >= 0 && since <= dormantWindow {
				tighten(IdleInterval)
			}
		}
	}
	return next
}

// ── Cache ────────────────────────────────────────────────────────────────────

// MutationSink is handed the previous and next merged snapshots after every
// refresh. It is the seam between this package (which owns the data) and the
// render layer (which owns the Facets and the stream). It is called from the
// single poller goroutine, once per refresh, never concurrently — including
// when several leagues came due on the same tick, which is deliberately one
// sink call and therefore one cross-league rail re-render rather than two.
type MutationSink func(prev, next []Game)

// lane is one league's slate and its own poll schedule. Leagues are polled
// independently and merged into one snapshot; they are never polled together
// just because they share a process.
type lane struct {
	provider Provider
	league   League

	games   []Game
	lastOK  time.Time
	lastErr error
	fetches int64
	due     time.Time // zero means due now
}

// Cache holds the merged snapshot and runs the single platform poller.
type Cache struct {
	mu    sync.RWMutex
	lanes []*lane
	games []Game // merged across every lane, ordered live-first
	byID  map[string]Game

	sink MutationSink
}

// NewCache builds the cache around one provider per league. No providers is the
// disabled lane: every read returns nothing and Start does not poll.
func NewCache(providers ...Provider) *Cache {
	c := &Cache{byID: map[string]Game{}}
	for _, p := range providers {
		if p == nil {
			continue
		}
		c.lanes = append(c.lanes, &lane{provider: p, league: p.League()})
	}
	sort.SliceStable(c.lanes, func(i, j int) bool {
		return c.lanes[i].league.Order < c.lanes[j].league.Order
	})
	return c
}

// Configured reports whether the lane exists at all.
func (c *Cache) Configured() bool { return c != nil && len(c.lanes) > 0 }

// ProviderName names every configured provider, for logging and the health
// surface. Empty when the lane is disabled.
func (c *Cache) ProviderName() string {
	if !c.Configured() {
		return ""
	}
	names := make([]string, 0, len(c.lanes))
	for _, l := range c.lanes {
		names = append(names, l.provider.Name())
	}
	return strings.Join(names, ",")
}

// Leagues is the set of leagues this cache polls, in table order. It is what
// gates the sports routes: a league is known because the platform polls it, not
// because a game happens to be on the board today.
func (c *Cache) Leagues() []League {
	if !c.Configured() {
		return nil
	}
	out := make([]League, 0, len(c.lanes))
	for _, l := range c.lanes {
		out = append(out, l.league)
	}
	return out
}

// HasLeague reports whether this cache polls the named league.
func (c *Cache) HasLeague(slug string) bool {
	if !c.Configured() {
		return false
	}
	slug = strings.ToLower(strings.TrimSpace(slug))
	for _, l := range c.lanes {
		if l.league.Slug == slug {
			return true
		}
	}
	return false
}

// OnMutation registers the render-layer sink. Must be called before Start.
func (c *Cache) OnMutation(sink MutationSink) {
	if c == nil {
		return
	}
	c.sink = sink
}

// Get returns up to limit games from the merged snapshot, ordered live first,
// then by how soon they start, then finals. limit <= 0 returns all of them.
func (c *Cache) Get(limit int) []Game {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if limit > 0 && len(c.games) > limit {
		out := make([]Game, limit)
		copy(out, c.games[:limit])
		return out
	}
	out := make([]Game, len(c.games))
	copy(out, c.games)
	return out
}

// GetLeague returns up to limit games from one league's slate, in the same
// order. It is what a per-league head lane reads, so a league's strip cannot be
// crowded out by another league's busy night.
func (c *Cache) GetLeague(slug string, limit int) []Game {
	if c == nil {
		return nil
	}
	slug = strings.ToLower(strings.TrimSpace(slug))
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Game, 0, limit)
	for _, g := range c.games {
		if g.League != slug {
			continue
		}
		out = append(out, g)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}

// Game returns one game by id.
func (c *Cache) Game(id string) (Game, bool) {
	if c == nil {
		return Game{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	g, ok := c.byID[id]
	return g, ok
}

// Health reports the poller's honesty record across every league: the OLDEST
// successful read (so "last update" is a floor, never a flattering maximum),
// the first error still outstanding, and how many requests this process has
// made in total.
func (c *Cache) Health() (lastOK time.Time, lastErr error, fetches int64) {
	if c == nil {
		return time.Time{}, nil, 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	neverAnswered := false
	for _, l := range c.lanes {
		fetches += l.fetches
		if lastErr == nil {
			lastErr = l.lastErr
		}
		if l.lastOK.IsZero() {
			neverAnswered = true
			continue
		}
		if lastOK.IsZero() || l.lastOK.Before(lastOK) {
			lastOK = l.lastOK
		}
	}
	if neverAnswered {
		// One league that has never answered makes the floor "never", whatever
		// the others have managed. A platform figure that reported the healthy
		// league's timestamp would be a summary that hides the outage it exists
		// to surface.
		lastOK = time.Time{}
	}
	return lastOK, lastErr, fetches
}

// LeagueHealth reports one league's record: when its upstream last answered,
// its outstanding error, its request count, and whether its snapshot is old
// enough that the surface must say so.
func (c *Cache) LeagueHealth(slug string) (lastOK time.Time, lastErr error, fetches int64, stale bool) {
	if c == nil {
		return time.Time{}, nil, 0, false
	}
	slug = strings.ToLower(strings.TrimSpace(slug))
	now := time.Now()
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, l := range c.lanes {
		if l.league.Slug != slug {
			continue
		}
		return l.lastOK, l.lastErr, l.fetches, laneStale(l, now)
	}
	return time.Time{}, nil, 0, false
}

// laneStale is the staleness verdict for one lane. Caller holds the lock.
func laneStale(l *lane, now time.Time) bool {
	if l.lastOK.IsZero() {
		return true
	}
	return now.Sub(l.lastOK) > staleAfter(leagueInterval(l.games, l.lastOK, l.lastErr, now))
}

// StaleLeague reports whether one league's snapshot is stale.
func (c *Cache) StaleLeague(slug string) bool {
	_, _, _, stale := c.LeagueHealth(slug)
	return stale
}

// Stale reports whether ANY polled league's snapshot is old enough that the
// surface must say so.
func (c *Cache) Stale() bool {
	if c == nil || !c.Configured() {
		return false
	}
	now := time.Now()
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, l := range c.lanes {
		if laneStale(l, now) {
			return true
		}
	}
	return false
}

// Start runs the single platform poller until ctx is done. A disabled lane logs
// once and returns; it never starts a goroutine and never makes a request.
func (c *Cache) Start(ctx context.Context) {
	if !c.Configured() {
		log.Printf("[sports] SPORTS_PROVIDER not set — sports lane disabled")
		return
	}
	log.Printf("[sports] providers %s — ONE platform poller, per-league cadence: live %s, soon %s, today %s, idle %s, dormant %s",
		c.ProviderName(), LiveInterval, SoonInterval, TodayInterval, IdleInterval, DormantInterval)
	go c.loop(ctx)
}

func (c *Cache) loop(ctx context.Context) {
	for {
		c.refreshDue(ctx, time.Now())
		wait := c.Interval()
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Interval is the delay before the next poll: the soonest any league is due.
// Exported so a test and the operator health line can read the cadence without
// waiting for it.
func (c *Cache) Interval() time.Duration {
	if !c.Configured() {
		return DormantInterval
	}
	now := time.Now()
	c.mu.RLock()
	defer c.mu.RUnlock()
	next := time.Duration(-1)
	for _, l := range c.lanes {
		d := l.due.Sub(now)
		if l.due.IsZero() {
			d = 0
		}
		if d < 0 {
			d = 0
		}
		if next < 0 || d < next {
			next = d
		}
	}
	if next < 0 {
		return DormantInterval
	}
	return next
}

// LeagueInterval is one league's own poll cadence, from its own slate.
func (c *Cache) LeagueInterval(slug string) time.Duration {
	if !c.Configured() {
		return DormantInterval
	}
	slug = strings.ToLower(strings.TrimSpace(slug))
	now := time.Now()
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, l := range c.lanes {
		if l.league.Slug == slug {
			return leagueInterval(l.games, l.lastOK, l.lastErr, now)
		}
	}
	return DormantInterval
}

// Refresh reads EVERY league once and swaps the merged snapshot in. It is the
// operator- and test-facing entry point; the poller uses refreshDue, which asks
// only the leagues whose own schedule says it is time. Both take the identical
// commit path — there is no second way to get data into this cache.
func (c *Cache) Refresh(ctx context.Context) { c.refresh(ctx, time.Now(), false) }

func (c *Cache) refreshDue(ctx context.Context, now time.Time) { c.refresh(ctx, now, true) }

func (c *Cache) refresh(ctx context.Context, now time.Time, onlyDue bool) {
	if !c.Configured() {
		return
	}

	// Fetches happen OUTSIDE the lock and one league at a time. Sequential is
	// deliberate: two leagues coming due on the same tick are two requests to
	// one upstream host a few milliseconds apart, not a burst, and it keeps the
	// sink call below strictly serialised without a second mutex.
	type result struct {
		l     *lane
		games []Game
		err   error
	}
	var results []result

	c.mu.RLock()
	lanes := append([]*lane(nil), c.lanes...)
	dues := make([]time.Time, len(lanes))
	for i, l := range lanes {
		dues[i] = l.due
	}
	c.mu.RUnlock()

	for i, l := range lanes {
		if onlyDue && !dues[i].IsZero() && dues[i].After(now) {
			continue
		}
		games, err := l.provider.Fetch(ctx)
		results = append(results, result{l: l, games: games, err: err})
	}
	if len(results) == 0 {
		return
	}

	observed := time.Now()
	c.mu.Lock()
	prev := c.games
	for _, r := range results {
		r.l.fetches++
		if r.err != nil {
			r.l.lastErr = r.err
			// Keep this league's last good slate rather than blanking it, and
			// let staleness decide when its cards must stop presenting it as
			// current. A failure in one league never touches another's.
			log.Printf("[sports] %s refresh failed (last good %s ago): %v",
				r.l.league.Slug, sinceLabel(r.l.lastOK), r.err)
			continue
		}
		for i := range r.games {
			r.games[i].Observed = observed
		}
		r.l.games = r.games
		r.l.lastOK = observed
		r.l.lastErr = nil
	}
	// Every lane's next due time is recomputed, including the ones that were not
	// asked: a league whose neighbour just went live must not inherit its speed,
	// and a league that just finished its last game must slow down at once.
	for _, l := range c.lanes {
		if l.due.IsZero() || !l.due.After(observed) {
			l.due = observed.Add(leagueInterval(l.games, l.lastOK, l.lastErr, observed))
		}
	}

	merged := c.mergeLocked()
	c.mu.Unlock()

	if c.sink != nil {
		c.sink(prev, merged)
	}
}

// mergeLocked rebuilds the cross-league snapshot from every lane's slate and
// installs it. The merge lives in one place so the rail, the strips, the hub
// and the game lookup can never be reading three different orderings of the
// same games. Caller holds the write lock.
func (c *Cache) mergeLocked() []Game {
	merged := make([]Game, 0, 16)
	for _, l := range c.lanes {
		merged = append(merged, l.games...)
	}
	sort.SliceStable(merged, func(i, j int) bool { return less(merged[i], merged[j]) })
	byID := make(map[string]Game, len(merged))
	for _, g := range merged {
		byID[g.ID] = g
	}
	c.games = merged
	c.byID = byID
	return merged
}

// recompute reinstalls the merged snapshot from the lanes as they stand.
func (c *Cache) recompute() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mergeLocked()
}

func sinceLabel(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return time.Since(t).Truncate(time.Second).String()
}

// less orders the merged slate, and with it the right rail.
//
// The ordering is a single ranked list across every league rather than a list
// grouped by league. The rail answers one question — what is on right now, and
// what is on next — and that question is not asked per sport. Grouping would
// put a basketball game that tips off in four hours above a football game being
// played this second, purely because basketball's header sorted first; ranking
// cannot do that. League identity is carried on the row instead, which costs a
// tag rather than a header and survives any ordering.
//
// Within a rank, games sort by when they start: among live games that is the
// one furthest through, and among upcoming ones it is the one starting soonest.
// Ties break on the league's table position and then the upstream id, so a
// panel rendered twice from the same snapshot is byte-identical and games that
// start at the same minute cluster by sport instead of interleaving.
func less(a, b Game) bool {
	ra, rb := slateRank(a), slateRank(b)
	if ra != rb {
		return ra < rb
	}
	if ra == rankFinal {
		// Most recently finished first.
		if !a.Start.Equal(b.Start) {
			return a.Start.After(b.Start)
		}
	} else if !a.Start.Equal(b.Start) {
		return a.Start.Before(b.Start)
	}
	if oa, ob := LeagueOf(a.League).Order, LeagueOf(b.League).Order; oa != ob {
		return oa < ob
	}
	if a.League != b.League {
		return a.League < b.League
	}
	return a.ID < b.ID
}

const (
	rankLive = iota
	rankDelayed
	rankUpcoming
	rankFinal
	rankOff
)

func slateRank(g Game) int {
	switch {
	case g.State.Active():
		return rankLive
	case g.State == StateDelayed:
		return rankDelayed
	case g.State == StateScheduled:
		return rankUpcoming
	case g.State == StateFinal:
		return rankFinal
	default:
		return rankOff
	}
}
