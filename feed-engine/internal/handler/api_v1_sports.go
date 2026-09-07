package handler

import (
	"net/http"
	"strings"
	"time"

	"github.com/f33d3r/feed-engine/internal/model"
	"github.com/f33d3r/feed-engine/internal/sports"
)

// ── /api/v1/sports — the scoreboard, as JSON ─────────────────────────────────
//
// The web draws this lane as Facets: a strip per league, a card per game, and
// separate mutations for the clock, each score and the football situation, all
// pushed as rendered HTML. A native client has no DOM to swap a fragment into,
// so it gets the same games as data and draws its own cards.
//
// The labels come across already written — status, period, kickoff time — for
// the same reason the web's do. "3rd Quarter", "Final/OT" and a start time in
// the league's own zone are the server's to decide, and a client that formatted
// them itself would be a second opinion about what a game is doing.
//
// Shape is F33D3RKit/Sources/F33D3RKit/Models/SportsGame.swift.

type SportsTeamDTO struct {
	Abbr    string  `json:"abbr"`
	Name    string  `json:"name"`
	Short   string  `json:"short_name"`
	LogoURL *string `json:"logo_url,omitempty"`
	Record  *string `json:"record,omitempty"`
	Score   int     `json:"score"`
	HasBall bool    `json:"has_ball"`
}

type SportsGameDTO struct {
	ID          string `json:"id"`
	League      string `json:"league"`
	LeagueLabel string `json:"league_label"`

	// State is the vocabulary in internal/sports: scheduled, in_progress,
	// halftime, end_period, delayed, postponed, canceled, final, unknown.
	State string `json:"state"`
	// StatusLabel is what the card prints for that state, written by the
	// server: "Final", "Final/OT", "3rd Quarter", "Halftime".
	StatusLabel string `json:"status_label"`
	// StartLabel is the kickoff in the league's own zone, empty once the game
	// is under way.
	StartLabel  string    `json:"start_label,omitempty"`
	Start       time.Time `json:"start"`
	Clock       string    `json:"clock,omitempty"`
	PeriodLabel string    `json:"period_label,omitempty"`

	Home SportsTeamDTO `json:"home"`
	Away SportsTeamDTO `json:"away"`
	// HasScore is false before a game starts. A scheduled game showing 0–0 is
	// a lie: nobody has failed to score yet, and the card draws a time instead.
	HasScore bool `json:"has_score"`
	IsLive   bool `json:"is_live"`
	IsFinal  bool `json:"is_final"`

	// Football only, and absent for a basketball game rather than blank.
	DownDistance   *string `json:"down_distance,omitempty"`
	PossessionText *string `json:"possession_text,omitempty"`
	RedZone        bool    `json:"red_zone"`
	LastPlay       *string `json:"last_play,omitempty"`

	Broadcast *string `json:"broadcast,omitempty"`
	Venue     *string `json:"venue,omitempty"`
}

type SportsLeagueDTO struct {
	Slug  string          `json:"slug"`
	Label string          `json:"label"`
	Games []SportsGameDTO `json:"games"`
	// Stale says the upstream has not answered recently, so a score on screen
	// may be behind. Said rather than hidden: a frozen score presented as
	// current is worse than one admitting its age.
	Stale bool `json:"stale"`
}

type SportsBoardDTO struct {
	Leagues []SportsLeagueDTO `json:"leagues"`
	// Count is every game across every league, so a lane can badge itself
	// without walking the tree.
	Count int `json:"count"`
}

func sportsTeamDTO(side sports.Side) SportsTeamDTO {
	return SportsTeamDTO{
		Abbr:    side.Team.Abbr,
		Name:    side.Team.Name,
		Short:   side.Team.ShortName,
		LogoURL: strPtr(side.Team.LogoURL),
		Record:  strPtr(side.Team.Record),
		Score:   side.Score,
		HasBall: side.HasBall,
	}
}

func sportsGameDTO(g sports.Game) SportsGameDTO {
	d := SportsGameDTO{
		ID:          g.ID,
		League:      g.League,
		LeagueLabel: g.LeagueLabel,
		State:       string(g.State),
		StatusLabel: g.StatusLabel(),
		Start:       g.Start,
		Clock:       g.Clock,
		PeriodLabel: g.PeriodLabel(),
		Home:        sportsTeamDTO(g.Home),
		Away:        sportsTeamDTO(g.Away),
		HasScore:    g.HasScore,
		IsLive:      g.Live(),
		IsFinal:     g.Final(),
		RedZone:     g.RedZone,
		Broadcast:   strPtr(g.Broadcast),
		Venue:       strPtr(g.Venue),
	}
	// The start time is what a card shows instead of a score, so it is only
	// sent while there is no score to show.
	if !g.HasScore {
		d.StartLabel = g.StartLabel()
	}
	if g.HasSituation() {
		d.DownDistance = strPtr(g.DownDistance)
		d.PossessionText = strPtr(g.PossessionText)
		d.LastPlay = strPtr(g.LastPlay)
	}
	return d
}

// apiV1Sports — GET /api/v1/sports[?league=nba]
//
// Every league the deployment follows, each with its games in the order the
// cache holds them. `503 sports_unavailable` when no provider is configured,
// which is a different thing from a day with no games.
func (h *Handler) apiV1Sports(w http.ResponseWriter, r *http.Request, _ *model.User) {
	if h.sportsCache == nil || !h.sportsCache.Configured() {
		apiError(w, http.StatusServiceUnavailable, "sports_unavailable",
			"Scores are not available on this deployment.")
		return
	}

	wanted := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("league")))
	out := SportsBoardDTO{Leagues: []SportsLeagueDTO{}}
	for _, league := range h.sportsCache.Leagues() {
		if wanted != "" && league.Slug != wanted {
			continue
		}
		games := h.sportsCache.GetLeague(league.Slug, 0)
		row := SportsLeagueDTO{
			Slug:  league.Slug,
			Label: league.Label,
			Games: make([]SportsGameDTO, 0, len(games)),
			Stale: h.sportsCache.StaleLeague(league.Slug),
		}
		for _, g := range games {
			row.Games = append(row.Games, sportsGameDTO(g))
		}
		out.Count += len(row.Games)
		out.Leagues = append(out.Leagues, row)
	}
	apiJSON(w, http.StatusOK, out)
}
