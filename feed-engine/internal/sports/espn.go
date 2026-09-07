package sports

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ESPN's public scoreboard JSON. No key, no account, no terms gate — the same
// document espn.com/nfl/scoreboard reads. It is the only free feed that carries
// every state this lane has to be correct in: quarter and clock, halftime, end
// of period, delay, postponement, overtime and final, plus possession, down and
// distance and the red zone flag.
//
// It is served with Cache-Control: max-age=8 from a CDN and sends no ETag, so
// conditional requests are not available. The lane instead hashes the response
// body and skips the whole normalise-diff-render path when the bytes are
// identical, and never polls faster than LiveInterval.
const espnDefaultBase = "https://site.api.espn.com/apis/site/v2/sports"

// espnLeague binds a league from the table in sports.go to where ESPN keeps it.
// The split is the point: sports.go owns what a league IS (how long a game is,
// what a period is called, whether it has weeks, what zone it schedules in) and
// this file owns nothing but the two path segments. Adding a league is a table
// entry there and a path pair here — and nothing else in the adapter, because
// the endpoint shape, the decoder, the status mapping and the records lookup
// are all league-neutral and were verified against real NFL and NBA documents.
type espnLeague struct {
	Sport string // "football", "basketball"
	Path  string // "nfl", "nba"
	League
}

var (
	espnNFL = espnLeague{Sport: "football", Path: "nfl", League: leagueNFL}
	espnNBA = espnLeague{Sport: "basketball", Path: "nba", League: leagueNBA}

	// espnLeagues is every league the ESPN provider serves, in table order.
	espnLeagues = []espnLeague{espnNFL, espnNBA}
)

type espnProvider struct {
	base   string
	league espnLeague
	client *http.Client

	// lastHash suppresses re-normalising an unchanged document. It is only read
	// and written from the poller goroutine.
	lastHash string
	lastRaw  []Game
}

func newESPN(baseURL string, league espnLeague) *espnProvider {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = espnDefaultBase
	}
	return &espnProvider{
		base:   strings.TrimRight(baseURL, "/"),
		league: league,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (p *espnProvider) Name() string { return "espn/" + p.league.Path }

// League is the table entry this provider feeds.
func (p *espnProvider) League() League { return p.league.League }

func (p *espnProvider) Fetch(ctx context.Context) ([]Game, error) {
	url := fmt.Sprintf("%s/%s/%s/scoreboard", p.base, p.league.Sport, p.league.Path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	// No User-Agent is set here on purpose, so net/http sends its own
	// "Go-http-client/…".
	//
	// The upstream's edge answers 403 to any User-Agent outside a small
	// allowlist — a descriptive one naming this service is refused, and so is a
	// Chrome string. The two honest options left are the default Go one and
	// impersonating a browser. Impersonating a browser is a lie told to get past
	// a control that exists precisely to distinguish clients, so this lane does
	// not tell it: it identifies itself as exactly what it is, an automated Go
	// client, and stays inside a cadence that never needs to hide.

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("%s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	// 4 MB ceiling: a full Sunday slate is well under 1 MB, and an upstream that
	// starts streaming something else must not be allowed to exhaust this process.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}

	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	if hash == p.lastHash && p.lastRaw != nil {
		// Byte-identical document: nothing to normalise, nothing to diff, and no
		// fragment to push. Returning the previous slate keeps the observed
		// timestamp fresh, which is correct — the upstream did answer.
		out := make([]Game, len(p.lastRaw))
		copy(out, p.lastRaw)
		return out, nil
	}

	games, err := p.decode(raw)
	if err != nil {
		return nil, err
	}
	p.lastHash = hash
	p.lastRaw = games
	out := make([]Game, len(games))
	copy(out, games)
	return out, nil
}

// ── Upstream document ────────────────────────────────────────────────────────

type espnScoreboard struct {
	Season struct {
		Year int `json:"year"`
	} `json:"season"`
	Week struct {
		Number int `json:"number"`
	} `json:"week"`
	Events []espnEvent `json:"events"`
}

type espnEvent struct {
	ID           string            `json:"id"`
	Date         string            `json:"date"`
	Name         string            `json:"name"`
	ShortName    string            `json:"shortName"`
	Status       espnStatus        `json:"status"`
	Competitions []espnCompetition `json:"competitions"`
}

type espnCompetition struct {
	ID          string           `json:"id"`
	Date        string           `json:"date"`
	Status      *espnStatus      `json:"status"`
	Competitors []espnCompetitor `json:"competitors"`
	Situation   *espnSituation   `json:"situation"`
	Broadcasts  []struct {
		Market string   `json:"market"`
		Names  []string `json:"names"`
	} `json:"broadcasts"`
	Venue struct {
		FullName string `json:"fullName"`
		Address  struct {
			City  string `json:"city"`
			State string `json:"state"`
		} `json:"address"`
	} `json:"venue"`
}

type espnStatus struct {
	DisplayClock string  `json:"displayClock"`
	Clock        float64 `json:"clock"`
	Period       int     `json:"period"`
	Type         struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		State       string `json:"state"`
		Completed   bool   `json:"completed"`
		Description string `json:"description"`
		Detail      string `json:"detail"`
		ShortDetail string `json:"shortDetail"`
	} `json:"type"`
}

type espnCompetitor struct {
	ID       string `json:"id"`
	HomeAway string `json:"homeAway"`
	Score    string `json:"score"`
	Team     struct {
		ID               string `json:"id"`
		Location         string `json:"location"`
		Name             string `json:"name"`
		Abbreviation     string `json:"abbreviation"`
		DisplayName      string `json:"displayName"`
		ShortDisplayName string `json:"shortDisplayName"`
		Logo             string `json:"logo"`
	} `json:"team"`
	// Records is the club's win-loss summary. The shape is league-neutral: the
	// NFL sends [{"type":"total","summary":"0-1"}] and the NBA sends
	// [{"type":"total","summary":"20-17"},{"type":"home",…},{"type":"road",…}]
	// off the same field, so totalRecord below serves both. An NBA preseason
	// document sends none at all, which renders as no record rather than "0-0".
	Records []struct {
		Type    string `json:"type"`
		Summary string `json:"summary"`
	} `json:"records"`
}

type espnSituation struct {
	Down                  int    `json:"down"`
	Distance              int    `json:"distance"`
	DownDistanceText      string `json:"downDistanceText"`
	ShortDownDistanceText string `json:"shortDownDistanceText"`
	PossessionText        string `json:"possessionText"`
	Possession            string `json:"possession"`
	IsRedZone             bool   `json:"isRedZone"`
	HomeTimeouts          int    `json:"homeTimeouts"`
	AwayTimeouts          int    `json:"awayTimeouts"`
	LastPlay              *struct {
		Text string `json:"text"`
	} `json:"lastPlay"`
}

// ── Normalisation ────────────────────────────────────────────────────────────

func (p *espnProvider) decode(raw []byte) ([]Game, error) {
	var doc espnScoreboard
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("decode scoreboard: %w", err)
	}
	out := make([]Game, 0, len(doc.Events))
	for _, ev := range doc.Events {
		g, ok := p.game(doc, ev)
		if !ok {
			continue
		}
		out = append(out, g)
	}
	return out, nil
}

func (p *espnProvider) game(doc espnScoreboard, ev espnEvent) (Game, bool) {
	if len(ev.Competitions) == 0 || ev.ID == "" {
		return Game{}, false
	}
	comp := ev.Competitions[0]

	// The competition carries the authoritative status while a game is running;
	// the event-level copy is the fallback.
	st := ev.Status
	if comp.Status != nil {
		st = *comp.Status
	}

	g := Game{
		ID:          ev.ID,
		League:      p.league.Slug,
		LeagueLabel: p.league.Label,
		SeasonYear:  doc.Season.Year,
		State:       espnState(st),
		Period:      st.Period,
		Venue:       comp.Venue.FullName,
	}
	// The week label exists only where the league has weeks. Verified against
	// the upstream: the NFL scoreboard carries {"week":{"number":1}} and the NBA
	// scoreboard carries no week object at all — the NBA schedule is a calendar
	// of nights, not a run of numbered weeks, and nothing takes the label's
	// place. The league's own answer gates it either way, so an upstream that
	// one day emits a stray week for basketball still cannot print "Week 3" on
	// a card that has no such thing.
	if p.league.Weeks && doc.Week.Number > 0 {
		g.WeekLabel = fmt.Sprintf("Week %d", doc.Week.Number)
	}
	if city := strings.TrimSpace(comp.Venue.Address.City); city != "" {
		g.VenueCity = city
		if s := strings.TrimSpace(comp.Venue.Address.State); s != "" {
			g.VenueCity = city + ", " + s
		}
	}
	date := comp.Date
	if date == "" {
		date = ev.Date
	}
	// ESPN emits both "2026-09-10T00:20Z" and full RFC3339. Try both rather than
	// silently landing on the zero time, which would print a wrong kickoff.
	g.Start = parseESPNTime(date)

	// The clock is only meaningful while the game is live. Carrying "0:00" into a
	// scheduled or final card would put a stopped clock on a card that has no
	// clock, which reads as a game frozen at zero.
	if g.State.Active() {
		g.Clock = strings.TrimSpace(st.DisplayClock)
	}
	// A period number outside a live game is history, not state: a final past
	// regulation means overtime happened, which Overtime() reads off the
	// league's own regulation length, but nothing should print an ordinal.
	if !g.State.Active() && g.State != StateFinal {
		g.Period = 0
	}

	for _, c := range comp.Competitors {
		side := Side{
			Team: Team{
				ID:        c.Team.ID,
				Name:      firstNonEmpty(c.Team.DisplayName, strings.TrimSpace(c.Team.Location+" "+c.Team.Name), c.Team.Name),
				ShortName: firstNonEmpty(c.Team.ShortDisplayName, c.Team.Name),
				Abbr:      c.Team.Abbreviation,
				LogoURL:   c.Team.Logo,
				Record:    totalRecord(c.Records),
			},
		}
		if n, err := strconv.Atoi(strings.TrimSpace(c.Score)); err == nil {
			side.Score = n
		}
		switch c.HomeAway {
		case "home":
			g.Home = side
		case "away":
			g.Away = side
		}
	}
	if g.Home.Team.ID == "" || g.Away.Team.ID == "" {
		return Game{}, false
	}

	// Scores only exist once the game has started. Before that both are "0" in
	// the document and mean "no score yet", not "nil–nil". An unrecognised
	// status is in the same position: the lane does not know the game started,
	// so it must not report a score it cannot vouch for.
	switch g.State {
	case StateInProgress, StateHalftime, StateEndPeriod, StateDelayed, StateFinal:
		g.HasScore = true
	}

	if s := comp.Situation; s != nil && g.State == StateInProgress {
		g.RedZone = s.IsRedZone
		g.DownDistance = firstNonEmpty(s.ShortDownDistanceText, s.DownDistanceText)
		g.PossessionText = s.PossessionText
		g.Home.Timeouts = s.HomeTimeouts
		g.Away.Timeouts = s.AwayTimeouts
		switch s.Possession {
		case g.Home.Team.ID:
			g.Home.HasBall = true
		case g.Away.Team.ID:
			g.Away.HasBall = true
		}
		if s.LastPlay != nil {
			g.LastPlay = strings.TrimSpace(s.LastPlay.Text)
		}
	}

	for _, b := range comp.Broadcasts {
		if len(b.Names) > 0 {
			g.Broadcast = strings.Join(b.Names, "/")
			break
		}
	}

	return g, true
}

// espnState maps the upstream status onto a state this lane knows how to draw.
// The status *name* is authoritative because it distinguishes halftime and
// end-of-period from generic "in progress"; the coarse state is the fallback so
// a name ESPN adds later still lands somewhere truthful instead of on "unknown".
func espnState(st espnStatus) State {
	switch strings.ToUpper(strings.TrimSpace(st.Type.Name)) {
	case "STATUS_SCHEDULED":
		return StateScheduled
	case "STATUS_IN_PROGRESS", "STATUS_FIRST_HALF", "STATUS_SECOND_HALF":
		return StateInProgress
	case "STATUS_HALFTIME", "STATUS_END_OF_HALF":
		return StateHalftime
	case "STATUS_END_PERIOD", "STATUS_END_OF_PERIOD":
		return StateEndPeriod
	case "STATUS_DELAYED", "STATUS_RAIN_DELAY", "STATUS_SUSPENDED", "STATUS_UNCERTAIN":
		return StateDelayed
	case "STATUS_POSTPONED":
		return StatePostponed
	case "STATUS_CANCELED", "STATUS_CANCELLED", "STATUS_FORFEIT":
		return StateCanceled
	case "STATUS_FINAL", "STATUS_FINAL_OVERTIME", "STATUS_FINAL_PEN":
		return StateFinal
	}
	switch strings.ToLower(strings.TrimSpace(st.Type.State)) {
	case "pre":
		return StateScheduled
	case "in":
		return StateInProgress
	case "post":
		if st.Type.Completed {
			return StateFinal
		}
		return StateCanceled
	}
	return StateUnknown
}

func parseESPNTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04Z07:00", "2006-01-02T15:04Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func totalRecord(records []struct {
	Type    string `json:"type"`
	Summary string `json:"summary"`
}) string {
	for _, r := range records {
		if strings.EqualFold(r.Type, "total") {
			return r.Summary
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
