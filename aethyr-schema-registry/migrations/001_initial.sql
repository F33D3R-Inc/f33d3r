-- ── F33D3R Schema Registry — Initial Migration ──────────────────────────────
-- Tables for tracking brain registrations and their signal schemas.
-- Run via: psql $DATABASE_URL < migrations/001_initial.sql

CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- ── schemas ───────────────────────────────────────────────────────────────────
-- Stores versioned JSON Schemas for each brain's signal types.
-- Every brain registers its schemas here on startup.
-- The compatibility checker enforces backward-compatible evolution.

CREATE TABLE IF NOT EXISTS schemas (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    brain_id        VARCHAR(50) NOT NULL,
    signal_type     VARCHAR(100) NOT NULL,
    version         INTEGER     NOT NULL CHECK (version >= 1),
    schema_json     JSONB       NOT NULL,
    description     TEXT        NOT NULL DEFAULT '',
    registered_by   VARCHAR(200) NOT NULL DEFAULT '',
    registered_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    UNIQUE(brain_id, signal_type, version)
);

CREATE INDEX IF NOT EXISTS idx_schemas_brain_signal
    ON schemas(brain_id, signal_type, version DESC);

-- ── brain_registrations ───────────────────────────────────────────────────────
-- One row per brain. Upserted on every startup call to POST /brain/register.
-- Tracks liveness (last_seen) and current schema version.

CREATE TABLE IF NOT EXISTS brain_registrations (
    brain_id        VARCHAR(50)  PRIMARY KEY,
    current_version INTEGER      NOT NULL DEFAULT 1,
    port            INTEGER      NOT NULL,
    host            VARCHAR(200) NOT NULL DEFAULT 'localhost',
    signal_types    TEXT[]       NOT NULL DEFAULT '{}',
    description     TEXT         NOT NULL DEFAULT '',
    status          VARCHAR(20)  NOT NULL DEFAULT 'online'
                    CHECK (status IN ('online','degraded','offline','planned')),
    last_seen       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    registered_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_brain_reg_status
    ON brain_registrations(status);

CREATE INDEX IF NOT EXISTS idx_brain_reg_last_seen
    ON brain_registrations(last_seen DESC);

-- ── compatibility_log ─────────────────────────────────────────────────────────
-- Audit trail of every schema registration and its compatibility result.
-- Elohim Veni uses this to detect schema drift across brain regions.

CREATE TABLE IF NOT EXISTS compatibility_log (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    brain_id        VARCHAR(50) NOT NULL,
    signal_type     VARCHAR(100) NOT NULL,
    from_version    INTEGER,
    to_version      INTEGER     NOT NULL,
    is_compatible   BOOLEAN     NOT NULL,
    violations      JSONB       NOT NULL DEFAULT '[]',
    checked_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_compat_log_brain
    ON compatibility_log(brain_id, checked_at DESC);

-- Pre-populate planned brains so they appear in GET /brains immediately
INSERT INTO brain_registrations (brain_id, port, description, status) VALUES
    ('aethyr_rank',    8080, 'Core ranking engine',                        'planned'),
    ('zior',           8082, 'Music cortex',                               'planned'),
    ('nantar',         8083, 'Feed brain — social posts and timeline',     'planned'),
    ('thessalon',      8084, 'Shop brain — commerce and creator stores',   'planned'),
    ('vovin',          8085, 'Messaging brain — private encrypted comms',  'planned'),
    ('caeor',          8086, 'Streaming brain — live video',               'planned'),
    ('loxion',         8087, 'Voice Rooms brain — audio spaces',           'planned'),
    ('astraon',        8088, 'Video brain — short and long form video',    'planned'),
    ('ain_soph',       8089, 'Wallet brain — Uphold crypto and fiat',      'planned'),
    ('zodacare',       8090, 'Git/Code brain — repository hosting',        'planned'),
    ('elohim_veni',    8091, 'Security brain — SOC and cyber ops',         'planned')
ON CONFLICT (brain_id) DO NOTHING;
