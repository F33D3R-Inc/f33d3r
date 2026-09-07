-- 0017_publish_follow_graph.sql
--
-- Publishes the follow graph to Manhattan's association plane.
--
-- WHY. elohim-veni decides who may contact whom. Two of its contact policies —
-- `followers` and `mutuals` — are decisions about the follow graph, and the
-- follow graph is this brain's. Until now elohim-veni took this brain's word for
-- it: the contact-evaluate call carried `"follows": true` and `"mutual": true`
-- as plain fields on the wire, and the deciding brain believed them. A security
-- decision resting on an unverifiable assertion from another brain is precisely
-- what the naming plane exists to end.
--
-- And it did not even work. On the Number path elohim-veni will not disclose who
-- a Number belongs to before it has decided — that is the whole privacy promise
-- of a Number — so this brain could not compute the follow facts, so it sent
-- none, so `followers` and `mutuals` silently degraded to "open a contact
-- request" for people who genuinely were mutual follows. With the graph readable
-- by the brain that takes the decision, both properties hold at once: elohim-veni
-- looks the relationship up itself, and this brain still never learns who the
-- Number belongs to.
--
-- WHAT THIS IS NOT. This is not a migration of the follow graph. `follows` stays
-- the system of record here: it is traversed by ranking, by suggestion, by the
-- fleet lane and by AethyrRank, and a network hop per traversal is how a 20ms
-- feed becomes a 400ms feed. What crosses to Manhattan is the published
-- projection, one writer, delivered by the outbox that already exists.
--
-- WHY TRIGGERS, NOT GO. The same reason migration 0016 moved the follower
-- counters onto a trigger. `follows` is not written only by FollowUser and
-- UnfollowUser: the foreign keys are ON DELETE CASCADE, so deleting an account
-- removes follow rows inside the database with no Go code running at all. A
-- publication step that lives in application code is a publication step that a
-- cascade, a backfill, or a hand-written statement in psql walks straight past —
-- and a stale association in the plane is a stale authorisation in elohim-veni.
-- A trigger fires for every writer, or it is not a rule.

-- ── the two new outbox ops ────────────────────────────────────────────────────
-- `assoc` records an association, `unassoc` retracts one. Both are name-addressed
-- and idempotent at the far end, so a replay is the end state the row wanted and
-- never a conflict the drain has to interpret.
ALTER TABLE manhattan_outbox DROP CONSTRAINT IF EXISTS manhattan_outbox_op_check;
ALTER TABLE manhattan_outbox ADD CONSTRAINT manhattan_outbox_op_check
    CHECK (op IN ('node', 'name', 'edge', 'revoke_name', 'assoc', 'unassoc'));

-- ── enqueue helper for one follow edge ────────────────────────────────────────
-- Payloads carry NAMES, never node ids — the rule migration 0005 set and the
-- reason this brain can publish a relationship between two identity nodes that
-- belong to elohim-veni without ever holding elohim-veni's primary keys.
--
-- An identity with no PIAL has no node in the plane, so there is nothing to
-- associate yet. That is not a silent skip: trg_manhattan_publish_follows_on_pial
-- below publishes every one of that account's follow edges the moment its PIAL
-- is assigned, so the row is deferred rather than dropped.
--
-- The dedup key carries the transaction id and a microsecond clock, because
-- follow → unfollow → follow is an ordinary sequence and a stable key would make
-- the outbox swallow the second follow as a duplicate of the first.
CREATE OR REPLACE FUNCTION manhattan_publish_follow(
    p_op TEXT, p_follower UUID, p_following UUID, p_dedup_suffix TEXT
) RETURNS VOID AS $$
DECLARE
    a UUID;
    b UUID;
BEGIN
    SELECT pial_id INTO a FROM users WHERE id = p_follower;
    SELECT pial_id INTO b FROM users WHERE id = p_following;
    IF a IS NULL OR b IS NULL THEN
        RETURN;
    END IF;

    PERFORM manhattan_enqueue(
        p_op,
        p_op || ':follows:' || a::text || '|' || b::text || ':' || p_dedup_suffix,
        jsonb_build_object(
            'assoc',        'follows',
            'subject_name', 'pial:' || a::text,
            'object_name',  'pial:' || b::text));
END;
$$ LANGUAGE plpgsql;

-- A suffix unique to this exact statement, so consecutive opposite writes on the
-- same pair each get their own row and the ordered drain replays them in the
-- order they happened.
CREATE OR REPLACE FUNCTION manhattan_follow_dedup_suffix() RETURNS TEXT AS $$
    SELECT txid_current()::text || '.' ||
           (extract(epoch FROM clock_timestamp()) * 1000000)::bigint::text;
$$ LANGUAGE sql VOLATILE;

-- ── follows → the association plane ───────────────────────────────────────────
CREATE OR REPLACE FUNCTION manhattan_follow_projection() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM manhattan_publish_follow('assoc', NEW.follower_id, NEW.following_id,
                                         manhattan_follow_dedup_suffix());
    ELSIF TG_OP = 'DELETE' THEN
        -- When this fires as part of a users cascade, the deleted account's row
        -- is already gone and its PIAL is unreadable here, so the helper above
        -- returns without enqueueing. That case is covered before it happens by
        -- trg_manhattan_retire_follows, which runs BEFORE the users row is
        -- deleted and while both PIALs can still be read.
        PERFORM manhattan_publish_follow('unassoc', OLD.follower_id, OLD.following_id,
                                         manhattan_follow_dedup_suffix());
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_manhattan_follow_projection ON follows;
CREATE TRIGGER trg_manhattan_follow_projection
    AFTER INSERT OR DELETE ON follows
    FOR EACH ROW EXECUTE FUNCTION manhattan_follow_projection();

-- ── account deletion, before the cascade erases the evidence ──────────────────
-- Proven behaviour, not an assumption: in an AFTER DELETE trigger on `follows`
-- reached through an ON DELETE CASCADE from `users`, the parent row is already
-- deleted and SELECT pial_id FROM users WHERE id = OLD.follower_id returns NULL.
-- So the retraction has to be enqueued from the parent side, BEFORE the cascade
-- runs, which is the only point where both PIALs are still readable.
CREATE OR REPLACE FUNCTION manhattan_retire_follows() RETURNS TRIGGER AS $$
DECLARE r RECORD;
BEGIN
    FOR r IN SELECT follower_id, following_id FROM follows
              WHERE follower_id = OLD.id OR following_id = OLD.id LOOP
        PERFORM manhattan_publish_follow('unassoc', r.follower_id, r.following_id,
                                         manhattan_follow_dedup_suffix());
    END LOOP;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_manhattan_retire_follows ON users;
CREATE TRIGGER trg_manhattan_retire_follows
    BEFORE DELETE ON users
    FOR EACH ROW EXECUTE FUNCTION manhattan_retire_follows();

-- ── an account that gains its PIAL after it gained followers ──────────────────
-- pial_id is assigned by AssignPIAL after the account row exists, so an account
-- can accumulate follow edges before it has an identity node to associate. Those
-- edges are deferred by manhattan_publish_follow, and this is what un-defers
-- them.
CREATE OR REPLACE FUNCTION manhattan_publish_follows_on_pial() RETURNS TRIGGER AS $$
DECLARE r RECORD;
BEGIN
    IF NEW.pial_id IS NULL OR OLD.pial_id IS NOT DISTINCT FROM NEW.pial_id THEN
        RETURN NEW;
    END IF;
    FOR r IN SELECT follower_id, following_id FROM follows
              WHERE follower_id = NEW.id OR following_id = NEW.id LOOP
        PERFORM manhattan_publish_follow('assoc', r.follower_id, r.following_id,
                                         'pial-assigned:' || NEW.pial_id::text);
    END LOOP;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_manhattan_publish_follows_on_pial ON users;
CREATE TRIGGER trg_manhattan_publish_follows_on_pial
    AFTER UPDATE OF pial_id ON users
    FOR EACH ROW EXECUTE FUNCTION manhattan_publish_follows_on_pial();

-- ── backfill ──────────────────────────────────────────────────────────────────
-- Every follow that already exists is published once, with a stable dedup key so
-- re-running this migration's body cannot queue it twice. The identity nodes
-- these associations point at were registered by migration 0005 and have lower
-- outbox ids, and the drain works strictly in id order, so the nodes exist by the
-- time the associations reach Manhattan.
DO $$
DECLARE r RECORD;
BEGIN
    FOR r IN SELECT follower_id, following_id FROM follows LOOP
        PERFORM manhattan_publish_follow('assoc', r.follower_id, r.following_id, 'backfill');
    END LOOP;
END $$;
