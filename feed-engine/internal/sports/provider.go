package sports

import (
	"context"
	"fmt"
	"strings"
)

// Provider is one upstream game feed for ONE league, normalised into Games.
//
// One provider per league is the seam that makes a per-league poll cadence
// possible at all: football being played on a Sunday afternoon is no reason to
// ask basketball anything, and a league between seasons must be able to cost
// almost nothing while its neighbour is polled every twenty seconds. A provider
// that fetched several leagues in one call would force one cadence on all of
// them.
type Provider interface {
	// Name identifies the provider in logs and on the health surface.
	Name() string
	// League is the table entry this provider feeds. The cache reads its slug,
	// its display order and its cadence from here; nothing downstream has to
	// know which vendor supplied it.
	League() League
	// Fetch reads the current slate. It returns an error rather than a partial
	// or invented slate — the cache keeps its last good snapshot and the surface
	// marks itself stale.
	Fetch(ctx context.Context) ([]Game, error)
}

// NewProviders resolves the sports lane into one Provider per league.
//
// The lane is ALWAYS on. There is no blank-means-off switch here and no
// configuration that can leave the platform without game cards: every time
// this was a switch in an env file, the file got edited and the cards went
// dark, and a scoreboard that is sometimes there is worse than none.
//
// Resolution, in order:
//   - name names a provider → that provider.
//   - name is blank → Big Balls when BBS_API is set (the paid, fuller slate);
//     ESPN's public scoreboard otherwise (keyless, always available).
//   - a named provider that cannot be built (unknown name, or Big Balls with
//     no key) → ESPN, and the returned error says what was asked for and why
//     it was not honoured. The caller logs it; the lane still runs.
//
// The returned slice is never empty. The error is advisory, never fatal.
func NewProviders(name, apiKey, baseURL string) ([]Provider, error) {
	key := strings.TrimSpace(apiKey)
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "":
		if key != "" {
			return bigBallsProviders(baseURL, key), nil
		}
		return espnProviders(baseURL), nil
	case "bigballs", "bbs", "bigballsdata":
		// Keyed. Without the key every poll would 401 and the rail would look
		// like an off-season, so the keyless provider serves instead — loudly.
		if key == "" {
			return espnProviders(baseURL), fmt.Errorf(
				"sports provider %q needs an API key and BBS_API is empty — serving espn instead", name)
		}
		return bigBallsProviders(baseURL, key), nil
	case "espn":
		return espnProviders(baseURL), nil
	default:
		return espnProviders(baseURL), fmt.Errorf(
			"unknown SPORTS_PROVIDER %q (known: bigballs, espn) — serving espn instead", name)
	}
}

func bigBallsProviders(baseURL, apiKey string) []Provider {
	out := make([]Provider, 0, len(bigballsLeagues))
	for _, l := range bigballsLeagues {
		out = append(out, newBigBalls(baseURL, apiKey, l))
	}
	return out
}

// ESPN's public scoreboard needs no key; a BBS_API that is set is simply
// unused by it.
func espnProviders(baseURL string) []Provider {
	out := make([]Provider, 0, len(espnLeagues))
	for _, l := range espnLeagues {
		out = append(out, newESPN(baseURL, l))
	}
	return out
}
