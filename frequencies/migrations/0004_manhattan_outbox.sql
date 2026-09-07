-- ── Manhattan outbox ─────────────────────────────────────────────────────────
-- The durable handoff to the naming plane, matching
-- feed-engine/internal/db/migrations/0005_manhattan_outbox.sql and
-- lore/src/db.rs row for row.
--
-- Manhattan is a separate brain reached over HTTP. A registration sent
-- fire-and-forget is a registration a restart, a timeout or a rolling deploy
-- silently loses — and a naming plane that is silently missing rows is worse
-- than no naming plane, because everything downstream trusts it.
--
-- So the handoff is transactional. These rows are written by triggers on the
-- tables they describe, inside the same transaction as the row that caused
-- them. Either someone hosts a Frequency AND their identity registration is
-- queued, or neither happened. src/manhattan_outbox.rs delivers them in order.
--
-- Payloads carry NAMES, never node ids. Manhattan assigns node ids; this side
-- does not know them and must not learn them, or the two planes acquire a
-- second shared identifier and we are back where we started.
CREATE TABLE IF NOT EXISTS manhattan_outbox (
    id              BIGSERIAL   PRIMARY KEY,
    op              TEXT        NOT NULL CHECK (op IN ('node', 'name', 'edge', 'revoke_name')),
    payload         JSONB       NOT NULL,
    -- dedup_key makes enqueue idempotent, so a replayed insert or a backfill
    -- cannot queue the same registration twice.
    dedup_key       TEXT        NOT NULL UNIQUE,
    attempts        INT         NOT NULL DEFAULT 0,
    last_error      TEXT,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    delivered_at    TIMESTAMPTZ,
    blocked_at      TIMESTAMPTZ,
    blocked_reason  TEXT
);

-- Undelivered, quarantined included: the depth a health check reports.
CREATE INDEX IF NOT EXISTS idx_manhattan_outbox_pending
    ON manhattan_outbox(next_attempt_at, id) WHERE delivered_at IS NULL;

-- The drain's query: undelivered, not quarantined, due, oldest first.
CREATE INDEX IF NOT EXISTS idx_manhattan_outbox_deliverable
    ON manhattan_outbox(next_attempt_at, id)
 WHERE delivered_at IS NULL AND blocked_at IS NULL;

-- What is quarantined, read on every drain tick.
CREATE INDEX IF NOT EXISTS idx_manhattan_outbox_blocked
    ON manhattan_outbox(id) WHERE blocked_at IS NOT NULL AND delivered_at IS NULL;

CREATE OR REPLACE FUNCTION manhattan_enqueue(p_op TEXT, p_dedup TEXT, p_payload JSONB)
RETURNS VOID AS $fn$
BEGIN
    INSERT INTO manhattan_outbox (op, dedup_key, payload)
    VALUES (p_op, p_dedup, p_payload)
    ON CONFLICT (dedup_key) DO NOTHING;
END;
$fn$ LANGUAGE plpgsql;

-- ── identities ───────────────────────────────────────────────────────────────
-- Auralis registers the identity node of every host and participant it
-- writes, and nothing else. Any brain may CREATE a node — registration has to
-- work whoever encounters someone first — but ownership is decided by
-- Manhattan from the node's kind, so the node this queues is owned by
-- elohim-veni the moment it exists. Auralis never writes a fact about it.
--
-- Only 'pial:' names produce a registration; the CHECK constraints on every
-- identity column make anything else unwritable in the first place.
CREATE OR REPLACE FUNCTION auralis_manhattan_enqueue_identity(p_name TEXT) RETURNS VOID AS $fn$
BEGIN
    IF p_name IS NULL OR p_name NOT LIKE 'pial:%' THEN
        RETURN;
    END IF;
    PERFORM manhattan_enqueue(
        'node',
        'node:' || p_name,
        jsonb_build_object(
            'kind',      'identity',
            'name',      p_name,
            'namespace', 'pial'));
END;
$fn$ LANGUAGE plpgsql;

-- One trigger function per table. PL/pgSQL resolves every NEW.<field> in a
-- function body against the row type at plan time, so a single function
-- naming both host_pial and pial fails on whichever table lacks one.
CREATE OR REPLACE FUNCTION auralis_manhattan_register_host() RETURNS TRIGGER AS $fn$
BEGIN
    PERFORM auralis_manhattan_enqueue_identity(NEW.host_pial);
    RETURN NEW;
END;
$fn$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION auralis_manhattan_register_participant() RETURNS TRIGGER AS $fn$
BEGIN
    PERFORM auralis_manhattan_enqueue_identity(NEW.pial);
    RETURN NEW;
END;
$fn$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_auralis_manhattan_identity_host ON frequencies;
CREATE TRIGGER trg_auralis_manhattan_identity_host
    AFTER INSERT OR UPDATE OF host_pial ON frequencies
    FOR EACH ROW EXECUTE FUNCTION auralis_manhattan_register_host();

DROP TRIGGER IF EXISTS trg_auralis_manhattan_identity_participant ON frequency_participants;
CREATE TRIGGER trg_auralis_manhattan_identity_participant
    AFTER INSERT ON frequency_participants
    FOR EACH ROW EXECUTE FUNCTION auralis_manhattan_register_participant();
