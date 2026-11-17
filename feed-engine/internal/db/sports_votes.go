package db

import (
	"database/sql"
	"errors"
	"fmt"
)

// SportsVoteTally is the fan vote on one game.
type SportsVoteTally struct {
	Away, Home int
}

// Total is every vote cast.
func (t SportsVoteTally) Total() int { return t.Away + t.Home }

// Pct is each side's share as whole percentages that always sum to 100, away
// then home. With no votes both are zero.
func (t SportsVoteTally) Pct() (away, home int) {
	total := t.Total()
	if total == 0 {
		return 0, 0
	}
	away = (t.Away*100 + total/2) / total
	return away, 100 - away
}

// CastSportsVote records an account's side on a game. The first vote stands:
// a second is not an error, it is simply not recorded. It returns the side the
// account is on after the call.
func CastSportsVote(database *sql.DB, league, gameID, accountID, side string) (string, error) {
	if side != "home" && side != "away" {
		return "", fmt.Errorf("sports vote: side %q", side)
	}
	if _, err := database.Exec(`
		INSERT INTO sports_game_votes (league, game_id, account_id, side)
		VALUES ($1, $2, $3::uuid, $4)
		ON CONFLICT (league, game_id, account_id) DO NOTHING`,
		league, gameID, accountID, side); err != nil {
		return "", fmt.Errorf("sports vote: %w", err)
	}
	return SportsVoteOf(database, league, gameID, accountID)
}

// SportsVoteOf is the side an account voted for, or "" when it has not voted.
func SportsVoteOf(database *sql.DB, league, gameID, accountID string) (string, error) {
	var side string
	err := database.QueryRow(`
		SELECT side FROM sports_game_votes
		WHERE league = $1 AND game_id = $2 AND account_id = $3::uuid`,
		league, gameID, accountID).Scan(&side)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("sports vote of %s: %w", accountID, err)
	}
	return side, nil
}

// SportsVoteTallyOf counts one game's votes.
func SportsVoteTallyOf(database *sql.DB, league, gameID string) (SportsVoteTally, error) {
	var t SportsVoteTally
	rows, err := database.Query(`
		SELECT side, count(*) FROM sports_game_votes
		WHERE league = $1 AND game_id = $2
		GROUP BY side`, league, gameID)
	if err != nil {
		return t, fmt.Errorf("sports vote tally: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var side string
		var n int
		if err := rows.Scan(&side, &n); err != nil {
			return t, fmt.Errorf("sports vote tally: %w", err)
		}
		if side == "home" {
			t.Home = n
		} else {
			t.Away = n
		}
	}
	return t, rows.Err()
}
