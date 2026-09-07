package sports

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// bigballs.go — the Big Balls Sports adapter.
//
// It answers the same question espn.go answers, from a keyed commercial feed
// instead of a public one, and it normalises into the same league-neutral Game.
// Nothing downstream — the cache, the ranker, the Facet renderers — knows which
// of the two supplied a slate.
//
// WHAT THIS UPSTREAM CARRIES, AND WHAT IT DOES NOT. This is the whole reason
// the adapter is not a copy of the ESPN one. The document is a match list:
// two clubs, a kickoff, a status, a final-or-running score, and a per-period
// linescore. It carries NO game clock, NO down and distance, NO possession, NO
// red-zone flag, NO venue, NO club record and NO week number. Those fields stay
// zero here and the model already treats a zero as "the upstream did not say":
// the situation Facet is not rendered for a game with no situation, and
// StatusLabel prints the period without a clock rather than inventing one. That
// is the honest degradation, and it is why this file never reaches for a second
// endpoint to fill a gap the card can simply not print.
//
// THE PERIOD IS DERIVED, NOT INVENTED. The feed states no period field, but the
// linescore is one entry per period played, so its length IS the period — four
// entries means the fourth quarter, five means overtime. That is a reading of
// the document, not a guess about the game: a game with no linescore reports
// period zero and prints no period label at all.
const bigballsDefaultBase = "https://api.bigballsdata.com"

// bigballsLeague binds a league from the table in sports.go to the two query
// parameters this upstream files it under. Same split as espnLeague: sports.go
// owns what a league IS, this owns nothing but the vendor's two strings.
type bigballsLeague struct {
	Sport  string // "american_football", "basketball"
	League string // "nfl", "nba"
	L      League
}

var (
	bigballsNFL = bigballsLeague{Sport: "american_football", League: "nfl", L: leagueNFL}
	bigballsNBA = bigballsLeague{Sport: "basketball", League: "nba", L: leagueNBA}

	// bigballsLeagues is every league this provider serves, in table order.
	bigballsLeagues = []bigballsLeague{bigballsNFL, bigballsNBA}
)

type bigballsProvider struct {
	base   string
	apiKey string
	league bigballsLeague
	client *http.Client

	// lastHash suppresses re-normalising an unchanged document, exactly as the
	// ESPN adapter does. Read and written only from the poller goroutine.
	lastHash string
	lastRaw  []Game
}

func newBigBalls(baseURL, apiKey string, league bigballsLeague) *bigballsProvider {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = bigballsDefaultBase
	}
	return &bigballsProvider{
		base:   strings.TrimRight(baseURL, "/"),
		apiKey: strings.TrimSpace(apiKey),
		league: league,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (p *bigballsProvider) Name() string { return "bigballs/" + p.league.League }

// League is the table entry this provider feeds.
func (p *bigballsProvider) League() League { return p.league.L }

// Fetch reads the current slate.
//
// ONE REQUEST ON A GAME DAY, TWO ON A QUIET ONE, AND THE REASON IS THE QUOTA.
// This upstream is metered — the free plan is a thousand calls a day across
// every league — so the cadence in sports.go is spending real money here in a
// way it never did against a public CDN. The shape below is the cheapest one
// that is still correct:
//
//  1. ?date=<today, in the league's own zone> returns everything the league is
//     doing today whatever its status — being played, finished this afternoon,
//     or tipping off tonight. That is precisely a scoreboard, and on any day
//     with games it is the only call made.
//  2. Only when today is empty does it ask the unfiltered endpoint, which
//     answers with the next fixtures. That is what lets the strip say what is
//     on next in an off-season instead of rendering nothing — and an off-season
//     league is polled at DormantInterval, so the second call costs a couple of
//     dozen requests a day, not thousands.
//
// A league between seasons therefore costs about 24 requests a day and a league
// in season costs one per poll. See the note on LiveInterval in sports.go: a
// full day of live football at a twenty-second cadence will still exceed a
// thousand-call plan on its own, which is a billing decision, not a code one.
func (p *bigballsProvider) Fetch(ctx context.Context) ([]Game, error) {
	today := time.Now().In(p.league.L.zone()).Format("2006-01-02")

	raw, err := p.get(ctx, url.Values{
		"sport":  {p.league.Sport},
		"league": {p.league.League},
		"date":   {today},
		"limit":  {"50"},
	})
	if err != nil {
		return nil, err
	}
	doc, err := decodeBigBalls(raw)
	if err != nil {
		return nil, err
	}
	if len(doc.Matches) == 0 {
		// Nothing today. Ask what is next, so the strip has a fixture to name.
		raw, err = p.get(ctx, url.Values{
			"sport":  {p.league.Sport},
			"league": {p.league.League},
			"limit":  {"10"},
		})
		if err != nil {
			return nil, err
		}
		if doc, err = decodeBigBalls(raw); err != nil {
			return nil, err
		}
	}

	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	if hash == p.lastHash && p.lastRaw != nil {
		out := make([]Game, len(p.lastRaw))
		copy(out, p.lastRaw)
		return out, nil
	}

	games := make([]Game, 0, len(doc.Matches))
	observed := time.Now().UTC()
	for _, m := range doc.Matches {
		g, ok := p.normalise(m, observed)
		if !ok {
			continue
		}
		games = append(games, g)
	}

	p.lastHash = hash
	p.lastRaw = games
	out := make([]Game, len(games))
	copy(out, games)
	return out, nil
}

// get performs one authenticated read and returns the raw body.
func (p *bigballsProvider) get(ctx context.Context, q url.Values) ([]byte, error) {
	endpoint := p.base + "/v1/matches?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		// Named explicitly because this is the one failure an operator can fix,
		// and "401" alone has sent people looking in the wrong file before.
		return nil, fmt.Errorf("%s: %s: the sports key was refused — check BBS_API "+
			"in the environment; a rotated key stops the previous one immediately",
			endpoint, resp.Status)
	case http.StatusTooManyRequests:
		return nil, fmt.Errorf("%s: %s: the sports plan's request quota is spent; "+
			"the lane keeps its last good snapshot and marks itself stale", endpoint, resp.Status)
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("%s: %s: %s", endpoint, resp.Status, strings.TrimSpace(string(body)))
	}

	// 4 MB ceiling, same reasoning as the ESPN adapter: a full slate is orders
	// of magnitude under this, and an upstream that starts streaming something
	// else must not be allowed to exhaust the process.
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// ── Upstream document ────────────────────────────────────────────────────────

type bigballsDoc struct {
	// Data is a match list on a day with fixtures. On a day with none the
	// upstream answers an object ({"scores": null}, meta.coverage=false)
	// instead of an empty list, so it is decoded in two steps: the raw value
	// here, and Matches below once its shape is known.
	Data    json.RawMessage `json:"data"`
	Matches []bigballsMatch `json:"-"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type bigballsMatch struct {
	ID         string         `json:"id"`
	Sport      string         `json:"sport"`
	League     string         `json:"league"`
	Home       bigballsTeam   `json:"home"`
	Away       bigballsTeam   `json:"away"`
	KickoffUTC string         `json:"kickoff_utc"`
	Status     string         `json:"status"`
	Score      *bigballsScore `json:"score"`
	Linescore  *struct {
		Home []int `json:"home"`
		Away []int `json:"away"`
	} `json:"linescore"`
	Broadcast *string `json:"broadcast"`
	Round     *string `json:"round"`
}

type bigballsTeam struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ShortName string `json:"short_name"`
	LogoURL   string `json:"logo_url"`
}

type bigballsScore struct {
	Home int `json:"home"`
	Away int `json:"away"`
}

func decodeBigBalls(raw []byte) (*bigballsDoc, error) {
	var doc bigballsDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("bigballs: decode: %w", err)
	}
	if doc.Error != nil && doc.Error.Message != "" {
		return nil, fmt.Errorf("bigballs: upstream error: %s", doc.Error.Message)
	}
	trimmed := bytes.TrimSpace(doc.Data)
	switch {
	case len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")):
		// No matches.
	case trimmed[0] == '[':
		if err := json.Unmarshal(trimmed, &doc.Matches); err != nil {
			return nil, fmt.Errorf("bigballs: decode matches: %w", err)
		}
	case trimmed[0] == '{':
		// The "nothing on this day" shape. An object here is not a match list
		// and is not an error either; the caller asks for the next fixtures.
	default:
		return nil, fmt.Errorf("bigballs: decode: data is neither a list nor an object")
	}
	return &doc, nil
}

// normalise turns one upstream match into a Game. It returns false for a match
// the model cannot represent honestly — no id, or a kickoff that does not
// parse — rather than emitting a card with a hole in it.
func (p *bigballsProvider) normalise(m bigballsMatch, observed time.Time) (Game, bool) {
	if strings.TrimSpace(m.ID) == "" {
		return Game{}, false
	}
	start, err := time.Parse(time.RFC3339, m.KickoffUTC)
	if err != nil {
		return Game{}, false
	}

	g := Game{
		ID:          m.ID,
		League:      p.league.L.Slug,
		LeagueLabel: p.league.L.Label,
		Start:       start.UTC(),
		State:       bigballsState(m.Status),
		Home:        Side{Team: bigballsTeamOf(m.Home)},
		Away:        Side{Team: bigballsTeamOf(m.Away)},
		Observed:    observed,
	}

	// A scheduled game has no score. Printing 0–0 before kickoff would say that
	// both sides have failed to score, which is not what "not started" means.
	if m.Score != nil && g.State != StateScheduled {
		g.HasScore = true
		g.Home.Score = m.Score.Home
		g.Away.Score = m.Score.Away
	}

	// The period is the length of the linescore — see the file header. A game
	// with no linescore stays at period zero and prints no period label.
	if m.Linescore != nil {
		g.Period = len(m.Linescore.Home)
		if a := len(m.Linescore.Away); a > g.Period {
			g.Period = a
		}
	}

	if m.Broadcast != nil {
		g.Broadcast = strings.TrimSpace(*m.Broadcast)
	}

	return g, true
}

func bigballsTeamOf(t bigballsTeam) Team {
	// short_name is this upstream's abbreviation ("SEA"), which is what a card
	// row prints. It carries no separate nickname form, so ShortName stays empty
	// and Team.Label falls through to the abbreviation as intended.
	return Team{
		ID:      t.ID,
		Name:    strings.TrimSpace(t.Name),
		Abbr:    strings.TrimSpace(t.ShortName),
		LogoURL: strings.TrimSpace(t.LogoURL),
	}
}

// bigballsState maps the upstream's status vocabulary onto the model's.
// An unrecognised status becomes StateUnknown rather than being forced into the
// nearest neighbour: a card that says nothing is better than one that says the
// wrong thing.
func bigballsState(s string) State {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "scheduled", "upcoming", "not_started":
		return StateScheduled
	case "live", "in_progress", "inprogress":
		return StateInProgress
	case "halftime":
		return StateHalftime
	case "finished", "final", "completed", "ft":
		return StateFinal
	case "postponed":
		return StatePostponed
	case "canceled", "cancelled":
		return StateCanceled
	case "delayed", "suspended":
		return StateDelayed
	default:
		return StateUnknown
	}
}
