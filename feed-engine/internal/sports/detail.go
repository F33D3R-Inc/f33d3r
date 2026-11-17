package sports

import (
	"fmt"
	"strings"
	"time"
)

// detail.go — one game as its own page draws it.
//
// The scoreboard Game is what a card needs: two scores, a clock and a line of
// situation. The game page needs the game itself: every play, grouped into the
// drives that produced them, the line score by period, the leaders, the box
// score and the win probability. Detail is that model. It is read from the
// upstream's per-game summary, only while somebody has the page open (see
// gamecast.go), and it is normalised here exactly as the scoreboard is: every
// label a Facet prints is computed on the server, so no browser ever formats a
// down, a clock or a yard line.
//
// A Detail is immutable once built. A poll builds a new one and hands the
// previous and the next to the render layer, which diffs them into Facet
// mutations; nothing ever edits a Detail in place, so the render layer can read
// one from any goroutine without a lock.

// Detail is everything the game page shows.
type Detail struct {
	// Game is the header: score, clock, period, possession, timeouts. It is the
	// scoreboard's Game overlaid with the summary's fresher reading, so the page
	// header moves every five seconds while the card elsewhere moves every
	// twenty.
	Game Game

	Linescore Linescore

	// Drives is every drive in the order it was played. Empty in a league whose
	// upstream carries no drives (basketball); Plays then stands alone.
	Drives []Drive
	// Plays is every play in the order it was run, across every drive.
	Plays []Play
	// LatestID is the newest play's id: the one the LATEST badge sits on and
	// the one the latest-play card shows.
	LatestID string

	Leaders []LeaderCategory
	Box     [2]TeamBox // away, home

	// HomeWinPct is the home side's win probability, 0..1, when HasWinProb.
	HomeWinPct float64
	HasWinProb bool

	// HasPlays reports whether this upstream carries a play-by-play at all. A
	// provider with no summary (Big Balls) leaves it false, and the page says
	// the feed does not carry plays rather than showing an empty list that
	// reads as "nothing has happened".
	HasPlays bool

	// Observed is when the upstream was last read for this game.
	Observed time.Time

	// sourceHash is the hash of the upstream document this was built from, so
	// a byte-identical document is not decoded and diffed again.
	sourceHash string
}

// Latest is the newest play, or false when there is none.
func (d *Detail) Latest() (Play, bool) {
	if d == nil || len(d.Plays) == 0 {
		return Play{}, false
	}
	return d.Plays[len(d.Plays)-1], true
}

// Spot is the ball's position and the down before or after a play.
type Spot struct {
	Down     int
	Distance int
	// YardLine is the ball's distance from the home end of the field as the
	// upstream numbers it (0..100); YardsToEndzone is how far the offence has
	// to go.
	YardLine       int
	YardsToEndzone int
	DownDistance   string // "2nd & 7 at BAL 26"
	ShortDown      string // "2nd & 7"
	PossessionText string // "BAL 26"
	TeamID         string
}

// Play is one snap, possession or event.
type Play struct {
	ID       string
	Sequence int64
	Period   int
	Clock    string // "3:53", the upstream's own display clock
	Type     string // "Pass Reception"
	// Headline is the play as a scoreboard line: "17-yd Pass", "3-yd Run",
	// "Incomplete pass". Computed here, per sport.
	Headline string
	Text     string
	// Side is "home" or "away": the team the play belongs to (the offence in
	// football, the shooting team in basketball). Empty when it is neither, as
	// for an end-of-period marker.
	Side    string
	TeamID  string
	DriveID string

	Start, End Spot
	Yards      int

	Scoring   bool
	Turnover  bool
	Penalty   bool
	Highlight bool
	// Scorer is who scored, when the upstream says. Empty otherwise.
	Scorer string

	AwayScore int
	HomeScore int
}

// SituationLine is the down-and-distance line above a play's headline:
// "2nd & 7 · 3:53". A play with no down (a kickoff) shows its clock alone.
func (p Play) SituationLine() string {
	parts := make([]string, 0, 2)
	if p.Start.Down > 0 && p.Start.ShortDown != "" {
		parts = append(parts, p.Start.ShortDown)
	}
	if p.Clock != "" {
		parts = append(parts, p.Clock)
	}
	return strings.Join(parts, " · ")
}

// Drive is one possession in football.
type Drive struct {
	ID     string
	Side   string // "home" | "away"
	Team   Team
	Period int // the period it started in

	Description    string // "7 plays, 63 yards, 2:49"
	Result         string // "Touchdown", "Punt", "Field Goal"; "" while in progress
	IsScore        bool
	OffensivePlays int
	Yards          int
	InProgress     bool
	// Scorer is the player credited with the drive's score, when known.
	Scorer string

	Plays []Play // in the order run
}

// Title is the drive's heading in the Plays panel: "In progress", a score
// with who scored it ("Touchdown · Justin Herbert"), or the club.
func (d Drive) Title() string {
	switch {
	case d.InProgress:
		return "In progress"
	case d.IsScore && d.Result != "" && d.Scorer != "":
		return d.Result + " · " + d.Scorer
	case d.IsScore && d.Result != "":
		return d.Result
	}
	if d.Team.ShortName != "" {
		return d.Team.ShortName
	}
	return d.Team.Label()
}

// Summary is the drive's second line: "2 plays, 34 yards" while in progress,
// the upstream's own description after.
func (d Drive) Summary() string {
	if d.InProgress {
		plays := "plays"
		if d.OffensivePlays == 1 {
			plays = "play"
		}
		return fmt.Sprintf("%d %s, %d yards", d.OffensivePlays, plays, d.Yards)
	}
	return d.Description
}

// Chip is the non-scoring result shown beside a finished drive: "Punt",
// "Interception". Empty for a score (the title already says it) and while the
// drive is running.
func (d Drive) Chip() string {
	if d.InProgress || d.IsScore {
		return ""
	}
	return d.Result
}

// NewestPlays is the drive's plays newest first, the order the panel lists
// them.
func (d Drive) NewestPlays() []Play { return reversed(d.Plays) }

// Key changes when anything the drive's collapsed row renders changes.
func (d Drive) Key() string {
	return fmt.Sprintf("%s|%s|%s|%t|%t|%d|%d|%s|%d",
		d.ID, d.Description, d.Result, d.IsScore, d.InProgress, d.OffensivePlays, d.Yards, d.Scorer, len(d.Plays))
}

// Linescore is the score by period.
type Linescore struct {
	Periods   []string // column heads: "1".."4", "OT"
	Away      []string // one per column; "" for a period not yet played
	Home      []string
	AwayTotal string
	HomeTotal string
}

// Key changes when any cell changes.
func (l Linescore) Key() string {
	return strings.Join(l.Periods, ",") + "|" + strings.Join(l.Away, ",") + "|" +
		strings.Join(l.Home, ",") + "|" + l.AwayTotal + "|" + l.HomeTotal
}

// Athlete is one player as the page draws them.
type Athlete struct {
	ID       string
	Name     string // "Justin Herbert"
	Short    string // "J. Herbert"
	Headshot string // this platform's image route; "" when there is none
	Position string // "QB"
}

// Initials is the mark drawn when the headshot cannot be.
func (a Athlete) Initials() string {
	var b strings.Builder
	for _, w := range strings.Fields(a.Name) {
		r := []rune(w)
		if len(r) > 0 {
			b.WriteRune(r[0])
		}
		if b.Len() >= 2 {
			break
		}
	}
	return strings.ToUpper(b.String())
}

// Leader is one side's leader in one category.
type Leader struct {
	Athlete Athlete
	Value   string // "58" — the big number
	Line    string // "3/5, 1 RUSH TD" — the stat line under the name
}

// Present reports whether this side has a leader in the category at all.
func (l Leader) Present() bool { return l.Athlete.Name != "" }

// LeaderCategory is one row of the game leaders card.
type LeaderCategory struct {
	Key   string // "passingYards"
	Label string // "Passing"
	Away  Leader
	Home  Leader
}

// LeadersKey changes when any leader, number or line changes.
func (d *Detail) LeadersKey() string {
	var b strings.Builder
	for _, c := range d.Leaders {
		fmt.Fprintf(&b, "%s:%s:%s:%s:%s:%s:%s|", c.Key,
			c.Away.Athlete.ID, c.Away.Value, c.Away.Line,
			c.Home.Athlete.ID, c.Home.Value, c.Home.Line)
	}
	return b.String()
}

// TeamBox is one side's box score.
type TeamBox struct {
	Side   string
	Team   Team
	Groups []StatGroup
}

// StatGroup is one table: passing, rushing, or a basketball box.
type StatGroup struct {
	Key     string
	Label   string
	Columns []string
	Rows    []StatRow
	Totals  []string
}

// StatRow is one player's line.
type StatRow struct {
	Athlete Athlete
	Values  []string
}

// Key changes when any cell of the side's box score changes.
func (b TeamBox) Key() string {
	var s strings.Builder
	for _, g := range b.Groups {
		s.WriteString(g.Key)
		for _, r := range g.Rows {
			s.WriteString(";" + r.Athlete.ID + "=" + strings.Join(r.Values, ","))
		}
		s.WriteString("|" + strings.Join(g.Totals, ",") + "|")
	}
	return s.String()
}

// BoxFor returns one side's box score.
func (d *Detail) BoxFor(side string) TeamBox {
	if side == "home" {
		return d.Box[1]
	}
	return d.Box[0]
}

// WinPct is each side's chance as a whole percentage, away then home, for the
// win-probability bar. They always sum to 100.
func (d *Detail) WinPct() (away, home int) {
	h := int(d.HomeWinPct*100 + 0.5)
	if h < 0 {
		h = 0
	}
	if h > 100 {
		h = 100
	}
	return 100 - h, h
}

// WinProbKey changes when the rounded percentage changes.
func (d *Detail) WinProbKey() string {
	if !d.HasWinProb {
		return "none"
	}
	a, h := d.WinPct()
	return fmt.Sprintf("%d:%d", a, h)
}

// ── Grouping ─────────────────────────────────────────────────────────────────
//
// The page lists newest first, grouped by period: the reader arrives wanting to
// know what just happened. Grouping is computed here so the Highlights list,
// the Plays panel and the mutations that extend them agree on what a group is.

// PlayGroup is one period's plays, newest first.
type PlayGroup struct {
	Period  int
	Heading string
	Plays   []Play
}

// DriveGroup is one period's drives, newest first.
type DriveGroup struct {
	Period  int
	Heading string
	Drives  []Drive
}

// HighlightGroups is the highlight plays grouped by period, newest period and
// newest play first.
func (d *Detail) HighlightGroups() []PlayGroup {
	return d.playGroups(func(p Play) bool { return p.Highlight })
}

// PlayGroups is every play grouped by period, for a league with no drives.
func (d *Detail) PlayGroups() []PlayGroup {
	return d.playGroups(func(Play) bool { return true })
}

func (d *Detail) playGroups(keep func(Play) bool) []PlayGroup {
	var out []PlayGroup
	for i := len(d.Plays) - 1; i >= 0; i-- {
		p := d.Plays[i]
		if !keep(p) {
			continue
		}
		if n := len(out); n == 0 || out[n-1].Period != p.Period {
			out = append(out, PlayGroup{Period: p.Period, Heading: PeriodHeading(d.Game.League, p.Period)})
		}
		out[len(out)-1].Plays = append(out[len(out)-1].Plays, p)
	}
	return out
}

// DriveGroups is every drive grouped by the period it started in, newest
// first.
func (d *Detail) DriveGroups() []DriveGroup {
	var out []DriveGroup
	for i := len(d.Drives) - 1; i >= 0; i-- {
		dr := d.Drives[i]
		if n := len(out); n == 0 || out[n-1].Period != dr.Period {
			out = append(out, DriveGroup{Period: dr.Period, Heading: PeriodHeading(d.Game.League, dr.Period)})
		}
		out[len(out)-1].Drives = append(out[len(out)-1].Drives, dr)
	}
	return out
}

// PlayByID finds one play.
func (d *Detail) PlayByID(id string) (Play, bool) {
	if d == nil {
		return Play{}, false
	}
	for _, p := range d.Plays {
		if p.ID == id {
			return p, true
		}
	}
	return Play{}, false
}

// DriveByID finds one drive.
func (d *Detail) DriveByID(id string) (Drive, bool) {
	if d == nil {
		return Drive{}, false
	}
	for _, dr := range d.Drives {
		if dr.ID == id {
			return dr, true
		}
	}
	return Drive{}, false
}

// NewPlays is every play in next that prev did not have, in the order run.
// This is the diff a poll is reduced to: the plays that happened since the
// page last heard.
func NewPlays(prev, next *Detail) []Play {
	if next == nil {
		return nil
	}
	seen := map[string]bool{}
	if prev != nil {
		for _, p := range prev.Plays {
			seen[p.ID] = true
		}
	}
	var out []Play
	for _, p := range next.Plays {
		if !seen[p.ID] {
			out = append(out, p)
		}
	}
	return out
}

func reversed(in []Play) []Play {
	out := make([]Play, len(in))
	for i, p := range in {
		out[len(in)-1-i] = p
	}
	return out
}
