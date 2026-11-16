-- ── Manhattan outbox ─────────────────────────────────────────────────────────
-- The durable handoff to the naming plane, row for row the table every other
-- brain uses (frequencies/migrations/0004_manhattan_outbox.sql,
-- feed-engine/internal/db/migrations/0005_manhattan_outbox.sql), drained by the
-- shared libs/manhattan-outbox crate.
--
-- Rows are written by triggers inside the same transaction as the asset that
-- caused them: either an asset exists AND its registration is queued, or
-- neither happened. Payloads carry names, never Manhattan node ids.
CREATE TABLE IF NOT EXISTS manhattan_outbox (
    id              BIGSERIAL   PRIMARY KEY,
    op              TEXT        NOT NULL CHECK (op IN ('node', 'name', 'edge', 'revoke_name')),
    payload         JSONB       NOT NULL,
    dedup_key       TEXT        NOT NULL UNIQUE,
    attempts        INT         NOT NULL DEFAULT 0,
    last_error      TEXT,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    delivered_at    TIMESTAMPTZ,
    blocked_at      TIMESTAMPTZ,
    blocked_reason  TEXT
);

CREATE INDEX IF NOT EXISTS idx_manhattan_outbox_pending
    ON manhattan_outbox(next_attempt_at, id) WHERE delivered_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_manhattan_outbox_deliverable
    ON manhattan_outbox(next_attempt_at, id)
 WHERE delivered_at IS NULL AND blocked_at IS NULL;

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

-- ── media nodes ──────────────────────────────────────────────────────────────
-- Every asset is a `media` node named media:<asset_id>, owned by caeor (the
-- authority map gives the `media` kind to caeor). Its uploader is linked as
-- media:<id> —authored_by→ pial:<uploader_pial>.
--
-- The identity node is queued first. Any brain may create a node — whoever
-- meets an identity first has to be able to register it — and Manhattan gives
-- the node to elohim-veni from its kind, so caeor never writes a fact about
-- it. The queue is delivered in id order, so both nodes exist before the edge.
--
-- Withdrawal does not retract anything: credit outlives the bytes.
CREATE OR REPLACE FUNCTION caeor_manhattan_register_asset() RETURNS TRIGGER AS $fn$
DECLARE
    media_name TEXT := 'media:' || NEW.id::text;
    pial_name  TEXT := 'pial:'  || NEW.uploader_pial::text;
BEGIN
    PERFORM manhattan_enqueue(
        'node',
        'node:' || pial_name,
        jsonb_build_object('kind', 'identity', 'name', pial_name, 'namespace', 'pial'));
    PERFORM manhattan_enqueue(
        'node',
        'node:' || media_name,
        jsonb_build_object('kind', 'media', 'name', media_name, 'namespace', 'media'));
    PERFORM manhattan_enqueue(
        'edge',
        'edge:' || media_name || '|authored_by|' || pial_name,
        jsonb_build_object(
            'subject_name', media_name,
            'predicate',    'authored_by',
            'object_name',  pial_name));
    RETURN NEW;
END;
$fn$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_caeor_manhattan_register_asset ON assets;
CREATE TRIGGER trg_caeor_manhattan_register_asset
    AFTER INSERT ON assets
    FOR EACH ROW EXECUTE FUNCTION caeor_manhattan_register_asset();
