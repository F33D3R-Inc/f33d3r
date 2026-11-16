-- 0002_handle_registry.sql
--
-- The handle registry, moved here from registry-brain.
--
-- A handle is a pointer; a PIAL is identity. That rule does not change by moving
-- the registry: transferring a handle never transfers followers, reputation,
-- wallet or messages. What changes is that the registry and the identities it
-- points at now live in one database, so "this handle belongs to this account"
-- can be a constraint instead of an agreement between two services
-- (see 0003, accounts.handle).
--
-- Ported from registry-brain/src/db.rs. Two things are deliberately NOT ported:
--
--   * manhattan_outbox and manhattan_enqueue. This brain already has both
--     (0001), with the same shape and an op CHECK that includes every op the
--     handle trigger writes ('node', 'name', 'revoke_name').
--   * the boot backfill. This table starts empty in f33d3r_security; every row
--     arrives through INSERT — by a claim or by the one-shot import from
--     f33d3r_handles — and the trigger below queues its Manhattan binding as it
--     lands. Manhattan answers "already" for a binding it holds, so imported
--     handles converge rather than duplicate.
--
-- Manhattan's authority map moves the `handle` namespace to elohim-veni in
-- manhattan/migrations/0010_handle_namespace_to_elohim_veni.sql.
--
-- FORWARD-ONLY: never edit this file after it has been applied anywhere.

CREATE EXTENSION IF NOT EXISTS citext;

-- ── Handle registry ───────────────────────────────────────────────────────────
-- One row per handle. handle is globally unique and case-insensitive.
-- pial_id is the only link to identity.
CREATE TABLE handles (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    handle          CITEXT      NOT NULL,
    pial_id         UUID        NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'active'
                    CHECK (status IN ('active','inactive','frozen','quarantined','auction','reserved')),
    tier            TEXT        NOT NULL DEFAULT 'standard'
                    CHECK (tier IN ('standard','premium','elite')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_bound_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT handles_handle_key UNIQUE (handle)
);
CREATE INDEX idx_handles_pial   ON handles(pial_id);
CREATE INDEX idx_handles_status ON handles(status);
CREATE INDEX idx_handles_active ON handles(handle) WHERE status = 'active';

-- ── Lifecycle event log ───────────────────────────────────────────────────────
-- Append-only. event_type: register | bind | unbind | transfer | freeze |
-- unfreeze | reclaim | quarantine | auction_start | auction_settle | import
CREATE TABLE handle_events (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    handle_id       UUID        NOT NULL REFERENCES handles(id) ON DELETE RESTRICT,
    event_type      TEXT        NOT NULL,
    from_pial_id    UUID,
    to_pial_id      UUID,
    metadata        JSONB       NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_he_handle ON handle_events(handle_id, created_at DESC);
CREATE INDEX idx_he_pial   ON handle_events(to_pial_id, created_at DESC);
CREATE INDEX idx_he_type   ON handle_events(event_type, created_at DESC);

-- registry-brain documented this log as append-only and enforced it nowhere.
-- Here the database refuses to rewrite history, whoever asks.
CREATE OR REPLACE FUNCTION refuse_history_rewrite() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % refused', TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_handle_events_append_only
    BEFORE UPDATE OR DELETE ON handle_events
    FOR EACH ROW EXECUTE FUNCTION refuse_history_rewrite();
CREATE TRIGGER trg_handle_events_no_truncate
    BEFORE TRUNCATE ON handle_events
    FOR EACH STATEMENT EXECUTE FUNCTION refuse_history_rewrite();

-- ── Auctions ──────────────────────────────────────────────────────────────────
CREATE TABLE handle_auctions (
    id                    UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    handle_id             UUID        NOT NULL REFERENCES handles(id) ON DELETE RESTRICT,
    start_time            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    end_time              TIMESTAMPTZ NOT NULL,
    starting_price_aet    INTEGER     NOT NULL DEFAULT 100,
    current_price_aet     INTEGER     NOT NULL DEFAULT 100,
    highest_bidder_pial   UUID,
    status                TEXT        NOT NULL DEFAULT 'active'
                          CHECK (status IN ('active','ended','settled','cancelled')),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_auctions_status ON handle_auctions(status, end_time);
CREATE INDEX idx_auctions_handle ON handle_auctions(handle_id);
-- One open auction per handle: two would sell the same pointer twice.
CREATE UNIQUE INDEX uq_auctions_one_active ON handle_auctions(handle_id) WHERE status = 'active';

CREATE TABLE auction_bids (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    auction_id      UUID        NOT NULL REFERENCES handle_auctions(id) ON DELETE CASCADE,
    bidder_pial     UUID        NOT NULL,
    amount_aet      INTEGER     NOT NULL CHECK (amount_aet > 0),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_bids_auction ON auction_bids(auction_id, created_at DESC);

-- ── Reserved namespace ────────────────────────────────────────────────────────
-- Handles that can never be claimed. One list, where there used to be two that
-- disagreed: registry-brain's system/trademark words and feed-engine's
-- top-level route segments (feed-engine/internal/handler/helpers.go), which
-- exist because a profile lives at f33d3r.com/{handle} and a route segment
-- would shadow it. reason: system | trademark | route | admin | offensive
CREATE TABLE reserved_handles (
    handle      CITEXT      PRIMARY KEY,
    reason      TEXT        NOT NULL DEFAULT 'system',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO reserved_handles (handle, reason) VALUES
    ('admin','system'), ('root','system'), ('system','system'), ('null','system'),
    ('undefined','system'), ('api','system'), ('support','system'), ('help','system'),
    ('security','system'), ('moderator','system'), ('official','system'), ('verified','system'),
    ('f33d3r','trademark'), ('aethyr','trademark'),
    ('pial','system'), ('fabric','system'), ('elohim','system'), ('vovin','system'),
    ('zior','system'), ('nantar','system'), ('zodacare','system'), ('thessalon','system'),
    ('registrar','system'), ('registry','system'),
    -- the registry's own route segments under /v1/handles
    ('reserved','route'), ('auctions','route'), ('claim','route'),
    -- feed-engine top-level routes
    ('onboard','route'), ('login','route'), ('logout','route'), ('signin','route'),
    ('signup','route'), ('register','route'), ('auth','route'), ('oauth','route'),
    ('deactivated','route'), ('forgot-password','route'), ('backup-codes','route'),
    ('kyc','route'), ('verity','route'), ('explore','route'), ('music','route'),
    ('create','route'), ('nsfw','route'), ('video','route'), ('visions','route'),
    ('works','route'), ('work','route'), ('communities','route'), ('lists','route'),
    ('spheres','route'), ('spaces','route'), ('marketplace','route'),
    ('buyer-library','route'), ('notifications','route'), ('messages','route'),
    ('bookmarks','route'), ('profile','route'), ('settings','route'), ('search','route'),
    ('wallet','route'), ('post','route'), ('feed','route'), ('shop','route'),
    ('stocks','route'), ('events','route'), ('library','route'), ('ledger','route'),
    ('upload','route'), ('react-video','route'), ('tag','route'), ('org','route'),
    ('achievements','route'), ('analytics','route'), ('leaderboard','route'),
    ('ainsoph','route'), ('static','route'), ('media','route'), ('facets','route'),
    ('partials','route'), ('_atlas','route'), ('u','route'), ('article','route'),
    ('articles','route'), ('sw','route'), ('robots','route'), ('sitemap','route'),
    ('favicon','route'), ('assets','route'), ('app','route'), ('www','route'),
    ('legal','route'), ('privacy','route'), ('terms','route'), ('dmca','route'),
    ('about','route'), ('home','route')
ON CONFLICT (handle) DO NOTHING;

-- ── Manhattan: the handle trigger ────────────────────────────────────────────
-- Ported unchanged from registry-brain (registry-brain/src/db.rs). Rows are
-- written by a trigger on `handles`, in the same transaction as the write that
-- changed the truth, so either a handle moved AND its name operations are
-- queued, or neither happened.
--
-- A transition is not content-addressable: bob may go A → B → A → B, and every
-- one of those moves is a distinct revoke and rebind. Transition rows are keyed
-- by a monotonic counter and never deduplicated.
CREATE SEQUENCE manhattan_transition_seq;

-- Which handle statuses carry a live name in Manhattan.
--   active  — bound and resolving.
--   auction — still owned by the seller until settlement.
-- Everything else must resolve to nobody.
CREATE OR REPLACE FUNCTION manhattan_handle_resolves(p_status TEXT) RETURNS BOOLEAN AS $$
    SELECT p_status IN ('active','auction');
$$ LANGUAGE sql IMMUTABLE;

-- Every lifecycle endpoint is a transition of (handle, pial_id, status):
--   register        INSERT, active                       → node + bind
--   bind            inactive|quarantined → active        → node + bind
--   unbind          active → inactive                    → revoke
--   freeze          active → frozen                      → revoke
--   quarantine      active → quarantined                 → revoke
--   reclaim         active → reserved                    → revoke
--   unfreeze        frozen → active                      → node + bind
--   transfer        pial_id changes while active         → revoke, then bind
--   auction open    active → auction                     → (no change)
--   auction settle  auction → active, pial_id changes    → revoke, then bind
--   auction settle  auction → active, no bids            → (no change)
-- Revoke is enqueued before bind and the outbox drains in id order, so a handle
-- never resolves to two identities at once.
CREATE OR REPLACE FUNCTION manhattan_sync_handle() RETURNS TRIGGER AS $$
DECLARE
    was_bound BOOLEAN := FALSE;
    is_bound  BOOLEAN;
    moved     BOOLEAN;
    seq       BIGINT;
BEGIN
    is_bound := manhattan_handle_resolves(NEW.status);

    IF TG_OP = 'UPDATE' THEN
        was_bound := manhattan_handle_resolves(OLD.status);
        moved     := (NEW.pial_id IS DISTINCT FROM OLD.pial_id)
                  OR (NEW.handle  IS DISTINCT FROM OLD.handle);
    ELSE
        moved := TRUE;
    END IF;

    IF was_bound AND (NOT is_bound OR moved) THEN
        seq := nextval('manhattan_transition_seq');
        PERFORM manhattan_enqueue(
            'revoke_name',
            'revoke:handle:' || OLD.handle::text || ':' || seq::text,
            jsonb_build_object('name', 'handle:' || OLD.handle::text));
    END IF;

    IF is_bound AND (NOT was_bound OR moved) THEN
        PERFORM manhattan_enqueue(
            'node',
            'node:pial:' || NEW.pial_id::text,
            jsonb_build_object(
                'kind',      'identity',
                'name',      'pial:' || NEW.pial_id::text,
                'namespace', 'pial'));

        seq := nextval('manhattan_transition_seq');
        PERFORM manhattan_enqueue(
            'name',
            'bind:handle:' || NEW.handle::text || ':' || NEW.pial_id::text || ':' || seq::text,
            jsonb_build_object(
                'node_name', 'pial:' || NEW.pial_id::text,
                'name',      'handle:' || NEW.handle::text,
                'namespace', 'handle'));
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_manhattan_sync_handle
    AFTER INSERT OR UPDATE OF handle, pial_id, status ON handles
    FOR EACH ROW EXECUTE FUNCTION manhattan_sync_handle();
