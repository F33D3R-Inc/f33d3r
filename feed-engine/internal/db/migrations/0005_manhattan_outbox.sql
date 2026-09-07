-- 0005_manhattan_outbox.sql
--
-- feed-engine's durable handoff to the Manhattan naming plane.
--
-- Manhattan is a separate brain reached over HTTP. A graph write sent
-- fire-and-forget is a graph write that a restart, a timeout or a rolling deploy
-- silently loses — and a naming plane that is silently missing rows is worse
-- than no naming plane, because everything downstream trusts it.
--
-- So the handoff is transactional. These rows are written by triggers on the
-- tables they describe, inside the same transaction as the row that caused
-- them. Either a work exists AND its registration is queued, or neither
-- happened. A background drain then delivers them in order, retrying with
-- backoff, and marks them delivered.
--
-- The triggers are the reason this cannot rot. A future writer that inserts a
-- work through some path nobody has thought of yet still registers it, because
-- registering is a property of the table and not a step a caller must remember.
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
    delivered_at    TIMESTAMPTZ
);

-- The drain's only query: undelivered, due, oldest first.
CREATE INDEX IF NOT EXISTS idx_manhattan_outbox_pending
    ON manhattan_outbox(next_attempt_at, id) WHERE delivered_at IS NULL;

-- ── enqueue helper ───────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION manhattan_enqueue(p_op TEXT, p_dedup TEXT, p_payload JSONB)
RETURNS VOID AS $$
BEGIN
    INSERT INTO manhattan_outbox (op, dedup_key, payload)
    VALUES (p_op, p_dedup, p_payload)
    ON CONFLICT (dedup_key) DO NOTHING;
END;
$$ LANGUAGE plpgsql;

-- ── identities ───────────────────────────────────────────────────────────────
-- A person's identity node is named by PIAL, never by handle. A handle is a
-- pointer that can be transferred, sold or reclaimed; PIAL is the root that
-- never moves. Binding the handle as a secondary name is what makes a handle
-- change a rename rather than a migration.
CREATE OR REPLACE FUNCTION manhattan_register_identity() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.pial_id IS NULL THEN
        RETURN NEW;
    END IF;

    -- A handle that moved must stop resolving to the identity that gave it up,
    -- or the next person to take that handle inherits the previous owner's
    -- identity on every lookup. Revoke before binding, and let the ordered
    -- drain deliver them in that order.
    IF TG_OP = 'UPDATE' AND OLD.handle IS DISTINCT FROM NEW.handle
       AND OLD.handle IS NOT NULL AND OLD.handle <> '' THEN
        PERFORM manhattan_enqueue(
            'revoke_name',
            'revoke:handle:' || OLD.handle || ':' || extract(epoch from now())::bigint::text,
            jsonb_build_object('name', 'handle:' || OLD.handle));
    END IF;

    PERFORM manhattan_enqueue(
        'node',
        'node:pial:' || NEW.pial_id::text,
        jsonb_build_object(
            'kind',      'identity',
            'name',      'pial:' || NEW.pial_id::text,
            'namespace', 'pial'));

    IF NEW.handle IS NOT NULL AND NEW.handle <> '' THEN
        PERFORM manhattan_enqueue(
            'name',
            'name:handle:' || NEW.handle || ':' || NEW.pial_id::text,
            jsonb_build_object(
                'node_name', 'pial:' || NEW.pial_id::text,
                'name',      'handle:' || NEW.handle,
                'namespace', 'handle'));
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_manhattan_register_identity ON users;
CREATE TRIGGER trg_manhattan_register_identity
    AFTER INSERT OR UPDATE OF handle, pial_id ON users
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_identity();

-- ── works ────────────────────────────────────────────────────────────────────
-- A work answers to two names: its UUID and its content address. Both bind to
-- one node, which is precisely the thing that was missing — works.id and
-- works.cid were two names for one work with nothing recording that fact.
CREATE OR REPLACE FUNCTION manhattan_register_work() RETURNS TRIGGER AS $$
BEGIN
    PERFORM manhattan_enqueue(
        'node',
        'node:uuid:' || NEW.id::text,
        jsonb_build_object(
            'kind',      'work',
            'name',      'uuid:' || NEW.id::text,
            'namespace', 'uuid'));

    IF NEW.cid IS NOT NULL AND NEW.cid <> '' THEN
        PERFORM manhattan_enqueue(
            'name',
            'name:cid:' || NEW.cid,
            jsonb_build_object(
                'node_name', 'uuid:' || NEW.id::text,
                'name',      'cid:' || NEW.cid,
                'namespace', 'cid'));
    END IF;

    IF NEW.author_pial IS NOT NULL THEN
        PERFORM manhattan_enqueue(
            'edge',
            'edge:uuid:' || NEW.id::text || '|authored_by|pial:' || NEW.author_pial::text,
            jsonb_build_object(
                'subject_name', 'uuid:' || NEW.id::text,
                'predicate',    'authored_by',
                'object_name',  'pial:' || NEW.author_pial::text));
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_manhattan_register_work ON works;
CREATE TRIGGER trg_manhattan_register_work
    AFTER INSERT ON works
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_work();

-- ── citations ────────────────────────────────────────────────────────────────
-- The edge that already exists locally (0003) is mirrored into the graph plane,
-- where it is one predicate among many rather than a table of its own.
CREATE OR REPLACE FUNCTION manhattan_register_citation() RETURNS TRIGGER AS $$
DECLARE
    pred TEXT;
BEGIN
    pred := CASE NEW.citation_type
                WHEN 'reply' THEN 'replies_to'
                WHEN 'quote' THEN 'quotes'
            END;
    IF pred IS NULL THEN
        RETURN NEW;
    END IF;

    PERFORM manhattan_enqueue(
        'edge',
        'edge:uuid:' || NEW.work_id::text || '|' || pred || '|uuid:' || NEW.target_id::text,
        jsonb_build_object(
            'subject_name', 'uuid:' || NEW.work_id::text,
            'predicate',    pred,
            'object_name',  'uuid:' || NEW.target_id::text));

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_manhattan_register_citation ON work_citations;
CREATE TRIGGER trg_manhattan_register_citation
    AFTER INSERT ON work_citations
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_citation();

-- ── backfill ─────────────────────────────────────────────────────────────────
-- Everything that already exists is enqueued once, so the graph plane starts
-- complete instead of only knowing about works created after this deploy.
-- ON CONFLICT DO NOTHING in the helper makes re-running this harmless.
DO $$
DECLARE r RECORD;
BEGIN
    FOR r IN SELECT pial_id, handle FROM users WHERE pial_id IS NOT NULL LOOP
        PERFORM manhattan_enqueue('node', 'node:pial:' || r.pial_id::text,
            jsonb_build_object('kind','identity','name','pial:' || r.pial_id::text,'namespace','pial'));
        IF r.handle IS NOT NULL AND r.handle <> '' THEN
            PERFORM manhattan_enqueue('name', 'name:handle:' || r.handle || ':' || r.pial_id::text,
                jsonb_build_object('node_name','pial:' || r.pial_id::text,
                                   'name','handle:' || r.handle,'namespace','handle'));
        END IF;
    END LOOP;

    FOR r IN SELECT id, cid, author_pial FROM works WHERE deleted_at IS NULL LOOP
        PERFORM manhattan_enqueue('node', 'node:uuid:' || r.id::text,
            jsonb_build_object('kind','work','name','uuid:' || r.id::text,'namespace','uuid'));
        IF r.cid IS NOT NULL AND r.cid <> '' THEN
            PERFORM manhattan_enqueue('name', 'name:cid:' || r.cid,
                jsonb_build_object('node_name','uuid:' || r.id::text,
                                   'name','cid:' || r.cid,'namespace','cid'));
        END IF;
        IF r.author_pial IS NOT NULL THEN
            PERFORM manhattan_enqueue('edge',
                'edge:uuid:' || r.id::text || '|authored_by|pial:' || r.author_pial::text,
                jsonb_build_object('subject_name','uuid:' || r.id::text,
                                   'predicate','authored_by',
                                   'object_name','pial:' || r.author_pial::text));
        END IF;
    END LOOP;

    FOR r IN SELECT work_id, target_id, citation_type FROM work_citations LOOP
        PERFORM manhattan_enqueue('edge',
            'edge:uuid:' || r.work_id::text || '|' ||
                CASE r.citation_type WHEN 'reply' THEN 'replies_to' ELSE 'quotes' END ||
                '|uuid:' || r.target_id::text,
            jsonb_build_object('subject_name','uuid:' || r.work_id::text,
                               'predicate', CASE r.citation_type WHEN 'reply' THEN 'replies_to' ELSE 'quotes' END,
                               'object_name','uuid:' || r.target_id::text));
    END LOOP;
END $$;
