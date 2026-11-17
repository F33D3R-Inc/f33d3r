package sports

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// espn_summary.go — ESPN's per-game summary document, normalised into Detail.
//
// GET <base>/<sport>/<league>/summary?event=<id> is the document espn.com's own
// game page reads. Verified on NFL game 401873065 while it was being played and
// on NBA game 401909994: the header (status, scores, linescores, records,
// possession, timeouts), drives.current / drives.previous with their plays
// (football), a flat plays list (basketball), scoringPlays, leaders, the box
// score and win probability. drives.previous includes the current drive as its
// last entry, so the two are merged by id rather than concatenated.
//
// The same rules as the scoreboard apply: the status is read through
// applyESPNStatus, every image is rewritten through ImageURL, every colour
// through hexColor, and a field the document did not carry stays empty rather
// than being guessed.

// Detail reads one game's summary.
func (p *espnProvider) Detail(ctx context.Context, base Game, prev *Detail) (*Detail, error) {
	u := fmt.Sprintf("%s/%s/%s/summary?event=%s", p.base, p.league.Sport, p.league.Path, url.QueryEscape(base.ID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("%s: %s: %s", u, resp.Status, strings.TrimSpace(string(body)))
	}
	// A late-game NFL summary is well under 1 MB; the ceiling only exists so an
	// upstream streaming something else cannot exhaust this process.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	now := time.Now()
	if prev != nil && prev.sourceHash == hash {
		// Byte-identical: nothing happened. The same reading is returned so the
		// diff is empty and nothing is pushed.
		return prev, nil
	}
	var doc espnSummary
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("decode summary %s: %w", base.ID, err)
	}
	d := p.normaliseSummary(doc, base)
	d.Observed = now
	d.Game.Observed = now
	d.sourceHash = hash
	return d, nil
}

// ── Upstream document ────────────────────────────────────────────────────────

type espnSummary struct {
	Header struct {
		Week         int `json:"week"`
		Competitions []struct {
			Competitors []espnSummaryCompetitor `json:"competitors"`
			Status      espnStatus              `json:"status"`
		} `json:"competitions"`
	} `json:"header"`
	Drives *struct {
		Current  *espnDrive  `json:"current"`
		Previous []espnDrive `json:"previous"`
	} `json:"drives"`
	Plays        []espnPlay `json:"plays"`
	ScoringPlays []espnPlay `json:"scoringPlays"`
	Leaders      []struct {
		Team struct {
			ID string `json:"id"`
		} `json:"team"`
		Leaders []struct {
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
			Leaders     []struct {
				DisplayValue string `json:"displayValue"`
				Summary      string `json:"summary"`
				MainStat     struct {
					Value string `json:"value"`
					Label string `json:"label"`
				} `json:"mainStat"`
				Athlete espnAthlete `json:"athlete"`
			} `json:"leaders"`
		} `json:"leaders"`
	} `json:"leaders"`
	Boxscore struct {
		Players []struct {
			Team struct {
				ID string `json:"id"`
			} `json:"team"`
			Statistics []struct {
				Name     string   `json:"name"`
				Labels   []string `json:"labels"`
				Totals   []string `json:"totals"`
				Athletes []struct {
					Athlete espnAthlete `json:"athlete"`
					Stats   []string    `json:"stats"`
				} `json:"athletes"`
			} `json:"statistics"`
		} `json:"players"`
	} `json:"boxscore"`
	WinProbability []struct {
		HomeWinPercentage float64 `json:"homeWinPercentage"`
		PlayID            string  `json:"playId"`
	} `json:"winprobability"`
}

type espnSummaryCompetitor struct {
	ID         string `json:"id"`
	HomeAway   string `json:"homeAway"`
	Score      string `json:"score"`
	Possession bool   `json:"possession"`
	// The NFL document carries how many timeouts a side has used this half;
	// the NBA document carries how many it has left. Both are optional.
	TimeoutsUsed      *int `json:"timeoutsUsed"`
	TimeoutsRemaining *int `json:"timeoutsRemaining"`
	Team              struct {
		ID               string `json:"id"`
		Location         string `json:"location"`
		Name             string `json:"name"`
		Abbreviation     string `json:"abbreviation"`
		DisplayName      string `json:"displayName"`
		ShortDisplayName string `json:"shortDisplayName"`
		Color            string `json:"color"`
		AlternateColor   string `json:"alternateColor"`
		Logo             string `json:"logo"`
		Logos            []struct {
			Href string   `json:"href"`
			Rel  []string `json:"rel"`
		} `json:"logos"`
	} `json:"team"`
	Linescores []struct {
		DisplayValue string `json:"displayValue"`
	} `json:"linescores"`
	Record []struct {
		Type    string `json:"type"`
		Summary string `json:"summary"`
	} `json:"record"`
}

type espnAthlete struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	ShortName   string `json:"shortName"`
	Headshot    struct {
		Href string `json:"href"`
	} `json:"headshot"`
	Position struct {
		Abbreviation string `json:"abbreviation"`
	} `json:"position"`
}

type espnDrive struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Team        struct {
		ID string `json:"id"`
	} `json:"team"`
	Start struct {
		Period struct {
			Number int `json:"number"`
		} `json:"period"`
	} `json:"start"`
	Yards          int        `json:"yards"`
	IsScore        bool       `json:"isScore"`
	OffensivePlays int        `json:"offensivePlays"`
	DisplayResult  string     `json:"displayResult"`
	Plays          []espnPlay `json:"plays"`
}

type espnPlay struct {
	ID             string `json:"id"`
	SequenceNumber string `json:"sequenceNumber"`
	Type           struct {
		Text string `json:"text"`
	} `json:"type"`
	Text      string `json:"text"`
	AwayScore int    `json:"awayScore"`
	HomeScore int    `json:"homeScore"`
	Period    struct {
		Number int `json:"number"`
	} `json:"period"`
	Clock struct {
		DisplayValue string `json:"displayValue"`
	} `json:"clock"`
	ScoringPlay bool     `json:"scoringPlay"`
	ScoreValue  int      `json:"scoreValue"`
	StatYardage int      `json:"statYardage"`
	IsTurnover  bool     `json:"isTurnover"`
	IsPenalty   bool     `json:"isPenalty"`
	Start       espnSpot `json:"start"`
	End         espnSpot `json:"end"`
	// Team is the acting team in leagues without drives (basketball).
	Team *struct {
		ID string `json:"id"`
	} `json:"team"`
	Participants []struct {
		Athlete struct {
			ID string `json:"id"`
		} `json:"athlete"`
		Type string `json:"type"`
	} `json:"participants"`
}

type espnSpot struct {
	Down                  int    `json:"down"`
	Distance              int    `json:"distance"`
	YardLine              int    `json:"yardLine"`
	YardsToEndzone        int    `json:"yardsToEndzone"`
	DownDistanceText      string `json:"downDistanceText"`
	ShortDownDistanceText string `json:"shortDownDistanceText"`
	PossessionText        string `json:"possessionText"`
	Team                  struct {
		ID string `json:"id"`
	} `json:"team"`
}

// ── Normalisation ────────────────────────────────────────────────────────────

func (p *espnProvider) normaliseSummary(doc espnSummary, base Game) *Detail {
	league := p.league.League
	d := &Detail{Game: base}
	g := &d.Game

	var comps []espnSummaryCompetitor
	if len(doc.Header.Competitions) > 0 {
		hc := doc.Header.Competitions[0]
		applyESPNStatus(g, hc.Status)
		g.HasScore = scoreKnown(g.State)
		comps = hc.Competitors
	}
	if league.Weeks && doc.Header.Week > 0 {
		g.WeekLabel = fmt.Sprintf("Week %d", doc.Header.Week)
	}

	// Sides. The summary's reading replaces the scoreboard's wherever it has
	// one; the scoreboard's stands wherever it does not.
	var awayLines, homeLines []string
	for _, c := range comps {
		side := &g.Away
		if c.HomeAway == "home" {
			side = &g.Home
		} else if c.HomeAway != "away" {
			continue
		}
		t := &side.Team
		t.ID = firstNonEmpty(c.Team.ID, c.ID, t.ID)
		t.Name = firstNonEmpty(c.Team.DisplayName, strings.TrimSpace(c.Team.Location+" "+c.Team.Name), t.Name)
		t.ShortName = firstNonEmpty(c.Team.ShortDisplayName, c.Team.Name, t.ShortName)
		t.Abbr = firstNonEmpty(c.Team.Abbreviation, t.Abbr)
		if logo := ImageURL(summaryLogo(c), LogoWidth); logo != "" {
			t.LogoURL = logo
		}
		if col := hexColor(c.Team.Color); col != "" {
			t.Color = col
		}
		if col := hexColor(c.Team.AlternateColor); col != "" {
			t.AltColor = col
		}
		for _, r := range c.Record {
			if strings.EqualFold(r.Type, "total") && r.Summary != "" {
				t.Record = r.Summary
			}
		}
		if n, err := strconv.Atoi(strings.TrimSpace(c.Score)); err == nil {
			side.Score = n
		}
		side.HasBall = c.Possession && g.State == StateInProgress
		switch {
		case c.TimeoutsRemaining != nil:
			side.Timeouts = *c.TimeoutsRemaining
		case c.TimeoutsUsed != nil && league.Timeouts > 0:
			side.Timeouts = league.Timeouts - *c.TimeoutsUsed
			if side.Timeouts < 0 {
				side.Timeouts = 0
			}
		}
		lines := make([]string, 0, len(c.Linescores))
		for _, l := range c.Linescores {
			lines = append(lines, strings.TrimSpace(l.DisplayValue))
		}
		if c.HomeAway == "home" {
			homeLines = lines
		} else {
			awayLines = lines
		}
	}
	d.Linescore = buildLinescore(*g, league, awayLines, homeLines)

	athletes := summaryAthletes(doc)
	scorers := summaryScorers(doc.ScoringPlays)

	// Plays: drives where the league has them, the flat list where it does not.
	if doc.Drives != nil && (doc.Drives.Current != nil || len(doc.Drives.Previous) > 0) {
		d.HasPlays = true
		var raw []espnDrive
		index := map[string]int{}
		add := func(dr espnDrive) {
			if i, ok := index[dr.ID]; ok {
				raw[i] = dr
				return
			}
			index[dr.ID] = len(raw)
			raw = append(raw, dr)
		}
		for _, dr := range doc.Drives.Previous {
			add(dr)
		}
		currentID := ""
		if doc.Drives.Current != nil {
			add(*doc.Drives.Current)
			currentID = doc.Drives.Current.ID
		}
		for _, dr := range raw {
			drive := Drive{
				ID:             dr.ID,
				Side:           g.sideOf(dr.Team.ID),
				Period:         dr.Start.Period.Number,
				Description:    strings.TrimSpace(dr.Description),
				Result:         strings.TrimSpace(dr.DisplayResult),
				IsScore:        dr.IsScore,
				OffensivePlays: dr.OffensivePlays,
				Yards:          dr.Yards,
			}
			drive.Team = g.Side(drive.Side).Team
			drive.InProgress = dr.ID == currentID && drive.Result == "" && g.State.Active()
			for _, ep := range dr.Plays {
				pl := footballPlay(ep, *g, athletes, scorers)
				pl.DriveID = dr.ID
				if pl.Side == "" {
					pl.Side = drive.Side
					pl.TeamID = dr.Team.ID
				}
				if pl.Scoring && pl.Scorer != "" {
					drive.Scorer = pl.Scorer
				}
				drive.Plays = append(drive.Plays, pl)
				d.Plays = append(d.Plays, pl)
			}
			if drive.Period == 0 && len(drive.Plays) > 0 {
				drive.Period = drive.Plays[0].Period
			}
			d.Drives = append(d.Drives, drive)
		}
	} else if len(doc.Plays) > 0 {
		d.HasPlays = true
		for _, ep := range doc.Plays {
			d.Plays = append(d.Plays, courtPlay(ep, *g, athletes, scorers))
		}
	}
	if n := len(d.Plays); n > 0 {
		d.LatestID = d.Plays[n-1].ID
		last := d.Plays[n-1]
		g.LastPlay = last.Text
		// The situation is where the last play left the ball. The summary has no
		// situation block of its own, and the scoreboard's is twenty seconds old.
		if g.State == StateInProgress && last.End.ShortDown != "" {
			g.DownDistance = last.End.ShortDown
			g.PossessionText = last.End.PossessionText
			g.RedZone = last.End.YardsToEndzone > 0 && last.End.YardsToEndzone <= 20
		}
	}
	if g.State != StateInProgress {
		g.DownDistance, g.PossessionText, g.RedZone = "", "", false
	}

	d.Leaders = summaryLeaders(doc, *g)
	d.Box = summaryBox(doc, *g, league)

	if n := len(doc.WinProbability); n > 0 {
		d.HomeWinPct = doc.WinProbability[n-1].HomeWinPercentage
		d.HasWinProb = true
	}
	return d
}

// sideOf names the side a team id plays for, or "".
func (g Game) sideOf(teamID string) string {
	switch {
	case teamID == "":
		return ""
	case teamID == g.Home.Team.ID:
		return "home"
	case teamID == g.Away.Team.ID:
		return "away"
	}
	return ""
}

// summaryLogo picks the club's default logo from the summary's logo set.
func summaryLogo(c espnSummaryCompetitor) string {
	if c.Team.Logo != "" {
		return c.Team.Logo
	}
	for _, l := range c.Team.Logos {
		for _, rel := range l.Rel {
			if rel == "default" {
				return l.Href
			}
		}
	}
	if len(c.Team.Logos) > 0 {
		return c.Team.Logos[0].Href
	}
	return ""
}

// buildLinescore lays the per-period scores out under one set of column heads:
// every regulation period whether played or not, then each overtime played.
func buildLinescore(g Game, l League, away, home []string) Linescore {
	cols := l.Regulation
	if len(away) > cols {
		cols = len(away)
	}
	if len(home) > cols {
		cols = len(home)
	}
	ls := Linescore{}
	cell := func(lines []string, i int) string {
		if i < len(lines) {
			return lines[i]
		}
		return ""
	}
	for i := 0; i < cols; i++ {
		head := strconv.Itoa(i + 1)
		if l.Regulation > 0 && i >= l.Regulation {
			head = periodLabel(i+1, l.Regulation)
		}
		ls.Periods = append(ls.Periods, head)
		ls.Away = append(ls.Away, cell(away, i))
		ls.Home = append(ls.Home, cell(home, i))
	}
	if g.HasScore {
		ls.AwayTotal = strconv.Itoa(g.Away.Score)
		ls.HomeTotal = strconv.Itoa(g.Home.Score)
	}
	return ls
}

func athleteFrom(a espnAthlete) Athlete {
	return Athlete{
		ID:       a.ID,
		Name:     strings.TrimSpace(a.DisplayName),
		Short:    strings.TrimSpace(a.ShortName),
		Headshot: ImageURL(a.Headshot.Href, HeadshotWidth),
		Position: a.Position.Abbreviation,
	}
}

// summaryAthletes indexes every athlete the document names, so a play that
// carries only participant ids can still say who.
func summaryAthletes(doc espnSummary) map[string]Athlete {
	out := map[string]Athlete{}
	for _, team := range doc.Boxscore.Players {
		for _, st := range team.Statistics {
			for _, a := range st.Athletes {
				if a.Athlete.ID != "" {
					out[a.Athlete.ID] = athleteFrom(a.Athlete)
				}
			}
		}
	}
	for _, team := range doc.Leaders {
		for _, cat := range team.Leaders {
			for _, l := range cat.Leaders {
				if l.Athlete.ID != "" {
					if _, ok := out[l.Athlete.ID]; !ok {
						out[l.Athlete.ID] = athleteFrom(l.Athlete)
					}
				}
			}
		}
	}
	return out
}

// summaryScorers maps a scoring play's id to the player its summary line
// credits. The scoring summary is written "Justin Herbert 22 Yd Rush (Cameron
// Dicker Kick)": the scorer is the name before the first number. A line that
// does not have that shape credits nobody rather than a guess.
func summaryScorers(plays []espnPlay) map[string]string {
	out := map[string]string{}
	for _, p := range plays {
		if name := scorerFrom(p.Text); name != "" {
			out[p.ID] = name
		}
	}
	return out
}

func scorerFrom(text string) string {
	words := strings.Fields(text)
	for i, w := range words {
		if r := []rune(w); len(r) > 0 && unicode.IsDigit(r[0]) {
			if i == 0 {
				return ""
			}
			return strings.Join(words[:i], " ")
		}
	}
	return ""
}

// playScorer resolves who scored on a play: the scoring summary first, then
// the play's first participant.
func playScorer(ep espnPlay, athletes map[string]Athlete, scorers map[string]string) string {
	if name := scorers[ep.ID]; name != "" {
		return name
	}
	for _, part := range ep.Participants {
		if a, ok := athletes[part.Athlete.ID]; ok && a.Name != "" {
			return a.Name
		}
	}
	return ""
}

func spotFrom(s espnSpot) Spot {
	return Spot{
		Down: s.Down, Distance: s.Distance,
		YardLine: s.YardLine, YardsToEndzone: s.YardsToEndzone,
		DownDistance:   strings.TrimSpace(s.DownDistanceText),
		ShortDown:      strings.TrimSpace(s.ShortDownDistanceText),
		PossessionText: strings.TrimSpace(s.PossessionText),
		TeamID:         s.Team.ID,
	}
}

func basePlay(ep espnPlay) Play {
	seq, _ := strconv.ParseInt(strings.TrimSpace(ep.SequenceNumber), 10, 64)
	return Play{
		ID:        ep.ID,
		Sequence:  seq,
		Period:    ep.Period.Number,
		Clock:     strings.TrimSpace(ep.Clock.DisplayValue),
		Type:      strings.TrimSpace(ep.Type.Text),
		Text:      strings.TrimSpace(ep.Text),
		Start:     spotFrom(ep.Start),
		End:       spotFrom(ep.End),
		Yards:     ep.StatYardage,
		Scoring:   ep.ScoringPlay,
		Turnover:  ep.IsTurnover,
		Penalty:   ep.IsPenalty,
		AwayScore: ep.AwayScore,
		HomeScore: ep.HomeScore,
	}
}

// footballPlay normalises one snap. The acting side is the offence, which is
// the team holding the ball at the snap.
func footballPlay(ep espnPlay, g Game, athletes map[string]Athlete, scorers map[string]string) Play {
	p := basePlay(ep)
	p.TeamID = ep.Start.Team.ID
	p.Side = g.sideOf(p.TeamID)
	if p.Scoring {
		p.Scorer = playScorer(ep, athletes, scorers)
	}
	t := strings.ToLower(p.Type)
	sack := strings.Contains(t, "sack")
	switch {
	case strings.Contains(t, "touchdown"), strings.Contains(t, "field goal"),
		strings.Contains(t, "interception"), strings.Contains(t, "fumble"),
		strings.Contains(t, "safety"), strings.Contains(t, "penalty"):
		p.Headline = p.Type
	case sack:
		p.Headline = yardsHeadline(abs(p.Yards), "Sack", "Sack")
	case strings.Contains(t, "pass incompletion"):
		p.Headline = "Incomplete pass"
	case strings.Contains(t, "pass reception"), strings.Contains(t, "pass completion"):
		p.Headline = yardsHeadline(p.Yards, "Pass", "No gain")
	case strings.Contains(t, "rush"):
		if p.Yards < 0 {
			p.Headline = yardsHeadline(-p.Yards, "Loss", "No gain")
		} else {
			p.Headline = yardsHeadline(p.Yards, "Run", "No gain")
		}
	case strings.Contains(t, "punt") && p.Yards > 0:
		p.Headline = yardsHeadline(p.Yards, "Punt", "Punt")
	default:
		p.Headline = p.Type
	}
	big := (strings.Contains(t, "rush") || strings.Contains(t, "pass")) && p.Yards >= 15
	p.Highlight = p.Scoring || p.Turnover || sack || big ||
		strings.Contains(t, "interception") || strings.Contains(t, "fumble") || strings.Contains(t, "blocked")
	return p
}

// courtPlay normalises one basketball event. The acting side is the team the
// upstream credits it to.
func courtPlay(ep espnPlay, g Game, athletes map[string]Athlete, scorers map[string]string) Play {
	p := basePlay(ep)
	if ep.Team != nil {
		p.TeamID = ep.Team.ID
		p.Side = g.sideOf(p.TeamID)
	}
	p.Headline = p.Type
	if p.Scoring {
		p.Scorer = playScorer(ep, athletes, scorers)
	}
	t := strings.ToLower(p.Type)
	p.Highlight = (p.Scoring && ep.ScoreValue >= 3) || (p.Scoring && strings.Contains(t, "dunk")) ||
		strings.Contains(t, "block")
	return p
}

func yardsHeadline(yards int, noun, zero string) string {
	if yards == 0 {
		return zero
	}
	return strconv.Itoa(yards) + "-yd " + noun
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// summaryLeaders builds the game leaders card: one row per category, in the
// order the upstream lists them, each side's leader beside the other.
func summaryLeaders(doc espnSummary, g Game) []LeaderCategory {
	var out []LeaderCategory
	index := map[string]int{}
	for _, team := range doc.Leaders {
		side := g.sideOf(team.Team.ID)
		if side == "" {
			continue
		}
		for _, cat := range team.Leaders {
			i, ok := index[cat.Name]
			if !ok {
				i = len(out)
				index[cat.Name] = i
				out = append(out, LeaderCategory{Key: cat.Name, Label: leaderLabel(cat.DisplayName)})
			}
			if len(cat.Leaders) == 0 {
				continue
			}
			l := cat.Leaders[0]
			leader := Leader{
				Athlete: athleteFrom(l.Athlete),
				Value:   firstNonEmpty(l.MainStat.Value, l.DisplayValue),
				Line:    strings.TrimSpace(l.Summary),
			}
			if side == "home" {
				out[i].Home = leader
			} else {
				out[i].Away = leader
			}
		}
	}
	return out
}

// leaderLabel turns "Passing Yards" into "Passing": the yards are the number
// beside it, and the label is the category.
func leaderLabel(display string) string {
	display = strings.TrimSpace(display)
	if s, ok := strings.CutSuffix(display, " Yards"); ok && s != "" {
		return s
	}
	return display
}

// summaryBox builds each side's box score, keeping the groups the league
// shows, in the league's order.
func summaryBox(doc espnSummary, g Game, l League) [2]TeamBox {
	box := [2]TeamBox{
		{Side: "away", Team: g.Away.Team},
		{Side: "home", Team: g.Home.Team},
	}
	for _, team := range doc.Boxscore.Players {
		side := g.sideOf(team.Team.ID)
		if side == "" {
			continue
		}
		idx := 0
		if side == "home" {
			idx = 1
		}
		byName := map[string]StatGroup{}
		var order []string
		for _, st := range team.Statistics {
			grp := StatGroup{
				Key:     st.Name,
				Label:   groupLabel(st.Name),
				Columns: st.Labels,
				Totals:  st.Totals,
			}
			for _, a := range st.Athletes {
				grp.Rows = append(grp.Rows, StatRow{Athlete: athleteFrom(a.Athlete), Values: a.Stats})
			}
			if len(grp.Rows) == 0 {
				continue
			}
			byName[st.Name] = grp
			order = append(order, st.Name)
		}
		if len(l.BoxGroups) > 0 {
			order = l.BoxGroups
		}
		for _, name := range order {
			if grp, ok := byName[name]; ok {
				box[idx].Groups = append(box[idx].Groups, grp)
			}
		}
	}
	return box
}

func groupLabel(name string) string {
	if name == "" {
		return "Players"
	}
	r := []rune(name)
	return string(unicode.ToUpper(r[0])) + string(r[1:])
}
