-- ── sports_game_votes ────────────────────────────────────────────────────────
-- The fan vote on a game page: which side an account backs. One vote per
-- account per game, cast once and never changed — the primary key is the rule,
-- so a second vote cannot be recorded by any path. The tally is read from here
-- and rendered by the server; no browser ever counts.
--
-- game_id is the upstream event id (the sports lane's Facet entity id); league
-- is kept beside it because an event id is only unique within its provider's
-- league namespace.
CREATE TABLE IF NOT EXISTS sports_game_votes (
    league     TEXT        NOT NULL,
    game_id    TEXT        NOT NULL,
    account_id UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    side       TEXT        NOT NULL CHECK (side IN ('home', 'away')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (league, game_id, account_id)
);

CREATE INDEX IF NOT EXISTS sports_game_votes_tally
    ON sports_game_votes (league, game_id, side);
