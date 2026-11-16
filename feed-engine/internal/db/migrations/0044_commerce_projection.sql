-- ── Themis is the subscription authority ─────────────────────────────────────
-- creator_plans and subscriptions stay, because the feed, fanout and gating
-- queries join on them, but this brain no longer writes them on its own
-- account. They are a READ PROJECTION of Themis's subscription_plans and
-- subscriptions, kept current by the commerce.events consumer
-- (internal/sitra/commerce.go, internal/handler/commerce_events.go), the same
-- way the identity projection follows identity.events.
--
-- The rows already here were written by this brain's old local subscribe path.
-- They are imported into Themis once, keeping their ids
-- (cmd/import-subscriptions), and Themis's events then re-project the same rows.

-- ── Amounts are AET units ────────────────────────────────────────────────────
-- The values already held Ain Soph's unit (shop pages divided by 100 to show
-- them); the columns are renamed so the name says so, and widened to match
-- Themis's BIGINT.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns
                WHERE table_name = 'creator_plans' AND column_name = 'price_aet') THEN
        ALTER TABLE creator_plans RENAME COLUMN price_aet TO price_units;
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.columns
                WHERE table_name = 'subscriptions' AND column_name = 'price_aet') THEN
        ALTER TABLE subscriptions RENAME COLUMN price_aet TO price_units;
    END IF;
END $$;
ALTER TABLE creator_plans ALTER COLUMN price_units TYPE BIGINT;
ALTER TABLE subscriptions ALTER COLUMN price_units TYPE BIGINT;

COMMENT ON TABLE creator_plans IS
    'Read projection of Themis subscription_plans, fed by commerce.events. Themis is the only writer.';
COMMENT ON TABLE subscriptions IS
    'Read projection of Themis subscriptions, fed by commerce.events. Themis is the only writer.';

-- ── commerce_subject_seq ─────────────────────────────────────────────────────
-- The last Themis sequence applied per subject. An event whose seq is not
-- above the recorded one has already been applied (a redelivery) and is
-- skipped, so a later state is never overwritten by an earlier event.
CREATE TABLE IF NOT EXISTS commerce_subject_seq (
    subject_kind  TEXT        NOT NULL,
    subject_id    TEXT        NOT NULL,
    last_seq      BIGINT      NOT NULL,
    last_event_id TEXT        NOT NULL,
    applied_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (subject_kind, subject_id)
);

-- ── commerce_event_deadletter ────────────────────────────────────────────────
-- A commerce.events record that can never be applied is kept here with its
-- coordinates and the reason, and only then committed past.
CREATE TABLE IF NOT EXISTS commerce_event_deadletter (
    id          BIGSERIAL   PRIMARY KEY,
    topic       TEXT        NOT NULL,
    partition   INT         NOT NULL,
    "offset"    BIGINT      NOT NULL,
    record      BYTEA       NOT NULL,
    reason      TEXT        NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (topic, partition, "offset")
);

-- ── What the import has verified ─────────────────────────────────────────────
-- cmd/import-subscriptions records here what it sent to Themis and what Themis
-- confirmed it holds, so the one-time import is visible after the fact.
CREATE TABLE IF NOT EXISTS commerce_import_runs (
    id                     BIGSERIAL   PRIMARY KEY,
    plans_sent             BIGINT      NOT NULL,
    plans_present          BIGINT      NOT NULL,
    subscriptions_sent     BIGINT      NOT NULL,
    subscriptions_present  BIGINT      NOT NULL,
    verified               BOOLEAN     NOT NULL,
    ran_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
