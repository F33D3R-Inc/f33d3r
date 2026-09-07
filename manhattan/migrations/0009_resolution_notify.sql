-- 0009_resolution_notify.sql
--
-- The database announces every change that can alter what a name resolves to.
--
-- Resolution is the hot path of the whole estate: fourteen brains resolve
-- names for every page they render, and at hundreds of millions of monthly
-- accounts that is far more reads than one Postgres will answer. So Manhattan
-- keeps an in-process cache of resolutions. The rule that cache must keep is
-- the plane's oldest one — a revoked name NEVER resolves — and a cache that
-- keeps it by expiring entries on a timer keeps it late, by the length of the
-- timer, on every replica. A rotated contact address that still resolves for
-- thirty seconds is thirty seconds of mail delivered to the wrong person.
--
-- So invalidation is not a timer. It is this: a trigger on every write that
-- can change a resolution sends the affected name on one NOTIFY channel, and
-- every Manhattan replica listens and drops that name from its cache. Postgres
-- delivers notifications when the transaction that raised them COMMITS, which
-- is exactly the moment the change becomes visible, so a replica hears about a
-- revocation at the same instant a fresh read would see it. The timer stays,
-- as a safety net against a listener that missed something, not as the
-- mechanism.
--
-- What changes a resolution:
--   * names — a binding created, retired, re-pointed, or removed. Every row
--     change on this table names exactly one name.
--   * nodes — a node's status, kind or owner changing alters what every name
--     bound to it resolves TO, so each of its names is announced.
--
-- The payload is the name folded to lower case, because names are CITEXT and
-- the cache keys on the folded form. A payload is limited to 8000 bytes and a
-- name to 512, so it always fits.
--
-- FORWARD-ONLY: never edit this file after it has been applied anywhere.

CREATE OR REPLACE FUNCTION manhattan_announce_name() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        PERFORM pg_notify('manhattan_resolution', lower(OLD.name::text));
    ELSE
        PERFORM pg_notify('manhattan_resolution', lower(NEW.name::text));
        -- A re-pointed row announces its old spelling too, in case the name
        -- itself was edited; both keys must fall out of every cache.
        IF TG_OP = 'UPDATE' AND OLD.name IS DISTINCT FROM NEW.name THEN
            PERFORM pg_notify('manhattan_resolution', lower(OLD.name::text));
        END IF;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_manhattan_announce_name ON names;
CREATE TRIGGER trg_manhattan_announce_name
    AFTER INSERT OR UPDATE OR DELETE ON names
    FOR EACH ROW EXECUTE FUNCTION manhattan_announce_name();

CREATE OR REPLACE FUNCTION manhattan_announce_node() RETURNS TRIGGER AS $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN SELECT name FROM names WHERE node_id = NEW.node_id LOOP
        PERFORM pg_notify('manhattan_resolution', lower(r.name::text));
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_manhattan_announce_node ON nodes;
CREATE TRIGGER trg_manhattan_announce_node
    AFTER UPDATE OF status, kind, owner ON nodes
    FOR EACH ROW EXECUTE FUNCTION manhattan_announce_node();

COMMENT ON FUNCTION manhattan_announce_name() IS
    'Announces a changed name on the manhattan_resolution channel so every Manhattan replica drops it from its resolution cache the instant the change commits.';
