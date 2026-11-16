-- ── Manhattan outbox ─────────────────────────────────────────────────────────
-- The durable handoff to the naming plane, row for row the table every other
-- brain uses (caeor/migrations/0002_manhattan_outbox.sql), drained by the
-- shared libs/manhattan-outbox crate.
--
-- Abraxas took over zodacare's reports, and with them zodacare's obligation:
-- every identity a report names is registered with Manhattan, by a trigger, in
-- the same transaction as the report. A report whose parties cannot be named
-- later is a report nobody can audit.
--
-- Abraxas registers identity NODES (pial:<uuid>) and nothing else. The pial
-- namespace is elohim-veni's and identity nodes are owned by elohim-veni; any
-- brain may create a node, none but the owner may bind names or edges on it.
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
RETURNS VOID AS $$
BEGIN
    INSERT INTO manhattan_outbox (op, dedup_key, payload)
    VALUES (p_op, p_dedup, p_payload)
    ON CONFLICT (dedup_key) DO NOTHING;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION manhattan_register_pial(p_pial UUID) RETURNS VOID AS $$
BEGIN
    IF p_pial IS NULL THEN
        RETURN;
    END IF;
    PERFORM manhattan_enqueue(
        'node',
        'node:pial:' || p_pial::text,
        jsonb_build_object(
            'kind',      'identity',
            'name',      'pial:' || p_pial::text,
            'namespace', 'pial'));
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION manhattan_register_report() RETURNS TRIGGER AS $$
BEGIN
    PERFORM manhattan_register_pial(NEW.reported_pial);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_manhattan_register_report
    AFTER INSERT OR UPDATE OF reported_pial ON reports
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_report();

CREATE OR REPLACE FUNCTION manhattan_register_filing() RETURNS TRIGGER AS $$
BEGIN
    PERFORM manhattan_register_pial(NEW.reporter_pial);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_manhattan_register_filing
    AFTER INSERT ON report_filings
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_filing();
