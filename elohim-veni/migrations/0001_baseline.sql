-- 0001_baseline.sql
--
-- Baseline schema, extracted verbatim from the four DDL blobs this brain used to
-- replay on every boot (db.rs SCHEMA, SCHEMA_MANHATTAN, SCHEMA_CONTACT and
-- number_lease::SCHEMA), in the order db::migrate applied them. Every statement
-- is idempotent, so this file applies cleanly to a fresh database and to any
-- database that already ran the old boot replay.
--
-- FORWARD-ONLY: never edit this file after it has been applied anywhere. Its
-- checksum is recorded in schema_migrations and a drift is a hard boot failure.

-- ════════════════════════════════════════════════════════════════════════════
-- SCHEMA — PIAL states, capability grants, trust, decision log, PIAL keys
-- ════════════════════════════════════════════════════════════════════════════

-- ── PIAL States ───────────────────────────────────────────────────────────────
-- One row per account. The PIAL is the authoritative identity anchor.
CREATE TABLE IF NOT EXISTS pial_states (
    pial_id     UUID        PRIMARY KEY,  -- the identity itself; the only key this brain files a person under
    status      TEXT        NOT NULL DEFAULT 'ACTIVE',   -- ACTIVE | SUSPENDED | REVOKED
    tier        TEXT        NOT NULL DEFAULT 'BASIC',    -- BASIC | VERIFIED | TRUSTED | PRIVILEGED
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_pial_status   ON pial_states(status);
CREATE INDEX IF NOT EXISTS idx_pial_tier     ON pial_states(tier);

-- ── Capability Grants ─────────────────────────────────────────────────────────
-- Explicit allow/deny for each named capability per PIAL.
-- A missing row means the capability inherits from tier defaults.
CREATE TABLE IF NOT EXISTS capability_grants (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    pial_id     UUID        NOT NULL REFERENCES pial_states(pial_id) ON DELETE CASCADE,
    capability  TEXT        NOT NULL,   -- post | message | monetize | adult_content | node_relay
    granted     BOOLEAN     NOT NULL DEFAULT FALSE,
    granted_by  UUID,                   -- admin PIAL that issued the grant
    reason      TEXT,
    expires_at  TIMESTAMPTZ,            -- NULL = never expires
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (pial_id, capability)
);
CREATE INDEX IF NOT EXISTS idx_cap_pial       ON capability_grants(pial_id);
CREATE INDEX IF NOT EXISTS idx_cap_capability ON capability_grants(capability);
CREATE INDEX IF NOT EXISTS idx_cap_expires    ON capability_grants(expires_at) WHERE expires_at IS NOT NULL;

-- ── Trust Scores ──────────────────────────────────────────────────────────────
-- Running trust signal per PIAL — updated after each decision.
--
-- AUTHORITY. Like the key tables below, a table of this name also exists in
-- f33d3r_feed. This one is the authority: a trust score is a fact about an
-- identity, decisions are made and logged here, and identity is this brain's to
-- assert. The other copy is a read-through cache with no write privilege. The
-- pial_id column is not a foreign key into anyone else's rows — it is the
-- identity name this brain registers into Manhattan.
CREATE TABLE IF NOT EXISTS trust_scores (
    pial_id         UUID        PRIMARY KEY REFERENCES pial_states(pial_id) ON DELETE CASCADE,
    score           DOUBLE PRECISION NOT NULL DEFAULT 1.0,   -- 0.0 (no trust) – 1.0 (full trust)
    anomaly_score   DOUBLE PRECISION NOT NULL DEFAULT 0.0,   -- 0.0 (normal) – 1.0 (highly anomalous)
    violation_count INT         NOT NULL DEFAULT 0,
    last_decision   TEXT,                                    -- last decision outcome
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ── Decision Log ──────────────────────────────────────────────────────────────
-- Immutable audit trail. Never deleted, only appended.
CREATE TABLE IF NOT EXISTS decision_log (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    pial_id             UUID        NOT NULL REFERENCES pial_states(pial_id) ON DELETE CASCADE,
    action              TEXT        NOT NULL,
    decision            TEXT        NOT NULL,   -- ALLOW | ALLOW_RESTRICTED | QUARANTINE | DENY
    confidence          DOUBLE PRECISION NOT NULL,
    reason              TEXT        NOT NULL,
    capability_required TEXT,
    content_risk        DOUBLE PRECISION NOT NULL DEFAULT 0.0,
    anomaly_score       DOUBLE PRECISION NOT NULL DEFAULT 0.0,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_dl_pial      ON decision_log(pial_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_dl_decision  ON decision_log(decision, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_dl_action    ON decision_log(action, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_dl_today     ON decision_log(created_at DESC);

-- ── PIAL Signing Keys ─────────────────────────────────────────────────────────
-- ECDSA-P256 public keys used by Themis to verify marketplace event signatures.
--
-- AUTHORITY. These rows are the authoritative store of a PIAL's signing key.
-- Manhattan's authority map (manhattan/migrations/0002_authority_map.sql) assigns
-- the 'key' node kind to elohim-veni, because a key belongs to the identity that
-- holds it and identity is this brain's to assert. f33d3r_feed holds a table of
-- the same name; it is a read-through cache with no write privilege. The
-- question is no longer settled by two comments disagreeing in two databases:
-- each key here is registered into Manhattan as a `key` node with a `signs_for`
-- edge to its identity, so "whose key is this" has exactly one recorded answer.
--
-- key_id is that node's name. It is the identity of the key MATERIAL, not of the
-- row: rotating a PIAL's key mints a new key_id, so the graph records a new key
-- signing for the same identity rather than quietly redefining the old one.
CREATE TABLE IF NOT EXISTS pial_signing_keys (
    pial_id       UUID        PRIMARY KEY,
    key_id        UUID        NOT NULL DEFAULT gen_random_uuid(),
    public_key_b64 TEXT       NOT NULL,
    algorithm     TEXT        NOT NULL DEFAULT 'ECDSA-P256',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ── PIAL ECDH Keys ────────────────────────────────────────────────────────────
-- ECDH-P256 public keys used by Themis to wrap content encryption keys (CEK).
-- Authoritative here for the same reason, and registered the same way.
CREATE TABLE IF NOT EXISTS pial_ecdh_keys (
    pial_id        UUID        PRIMARY KEY,
    key_id         UUID        NOT NULL DEFAULT gen_random_uuid(),
    public_key_b64 TEXT        NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ════════════════════════════════════════════════════════════════════════════
-- SCHEMA_MANHATTAN — the naming-plane outbox and its triggers
-- ════════════════════════════════════════════════════════════════════════════

-- ── Identity is keyed by PIAL and nothing else ───────────────────────────────
-- account_id was feed-engine's users.id: another brain's row id, carried here as
-- a second identity key, with a UNIQUE constraint that let a re-bootstrap move a
-- PIAL from under an account. No brain may reference another brain's rows, and
-- a PIAL is the root that never moves — so the column and the alternate key go.
-- The mapping is not lost: it lives in f33d3r_feed.users, where the account is.
ALTER TABLE pial_states DROP COLUMN IF EXISTS account_id;

-- ── Every key gets its own identity, so the graph can name it ────────────────
-- Volatile default: PostgreSQL rewrites the table and gives each existing row
-- its own key_id, which is exactly right — these are distinct keys.
ALTER TABLE pial_signing_keys ADD COLUMN IF NOT EXISTS key_id UUID NOT NULL DEFAULT gen_random_uuid();
ALTER TABLE pial_ecdh_keys    ADD COLUMN IF NOT EXISTS key_id UUID NOT NULL DEFAULT gen_random_uuid();
CREATE UNIQUE INDEX IF NOT EXISTS idx_signing_key_id ON pial_signing_keys(key_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_ecdh_key_id    ON pial_ecdh_keys(key_id);

-- ── The outbox ───────────────────────────────────────────────────────────────
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

-- ── Quarantine ───────────────────────────────────────────────────────────────
-- Head-of-line blocking is correct while a row can still land: later rows depend
-- on earlier ones, and running ahead would write edges pointing at nodes that do
-- not exist. It stops being correct the moment a row cannot land at all — then
-- it is not ordering the queue, it is ending it. One unfixable row used to stop
-- this brain's entire naming-plane output indefinitely, with a log line as the
-- only signal and a hand-written migration as the only cure.
--
-- A quarantined row is neither dropped nor delivered. It keeps its payload, its
-- error and its id; the drain simply stops claiming it, so everything queued
-- behind it moves again. It represents a naming-plane write that did NOT happen,
-- so it is reported as an incident on every tick until a person clears
-- blocked_at — loud by construction rather than by anyone remembering to look.
--
--   what is stuck:  SELECT id, op, blocked_at, blocked_reason, payload
--                     FROM manhattan_outbox
--                    WHERE blocked_at IS NOT NULL AND delivered_at IS NULL
--                    ORDER BY id;
--   put one back:   UPDATE manhattan_outbox
--                      SET blocked_at = NULL, blocked_reason = NULL,
--                          attempts = 0, next_attempt_at = NOW()
--                    WHERE id = $1;
--
-- Returning a row to the queue re-enters it at its original id, so the ordering
-- the drain depends on survives the round trip.
ALTER TABLE manhattan_outbox ADD COLUMN IF NOT EXISTS blocked_at     TIMESTAMPTZ;
ALTER TABLE manhattan_outbox ADD COLUMN IF NOT EXISTS blocked_reason TEXT;

-- The drain's query is now "undelivered, not quarantined, due, oldest first", so
-- it gets its own partial index. The one above stays: the backlog counters still
-- ask for everything undelivered, quarantined rows included, because a write
-- that did not happen must not disappear from the depth a health check reports.
CREATE INDEX IF NOT EXISTS idx_manhattan_outbox_deliverable
    ON manhattan_outbox(next_attempt_at, id)
 WHERE delivered_at IS NULL AND blocked_at IS NULL;

-- What is quarantined, read on every drain tick.
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

-- ── identities ───────────────────────────────────────────────────────────────
-- A person's identity node is named by PIAL and by nothing else. A handle is a
-- pointer that can be transferred, sold or reclaimed and belongs to the
-- registrar's namespace; PIAL is the root, and minting it is this brain's.
CREATE OR REPLACE FUNCTION manhattan_register_identity() RETURNS TRIGGER AS $$
BEGIN
    PERFORM manhattan_enqueue(
        'node',
        'node:pial:' || NEW.pial_id::text,
        jsonb_build_object(
            'kind',      'identity',
            'name',      'pial:' || NEW.pial_id::text,
            'namespace', 'pial'));
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_manhattan_register_identity ON pial_states;
CREATE TRIGGER trg_manhattan_register_identity
    AFTER INSERT OR UPDATE OF pial_id ON pial_states
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_identity();

-- ── keys ─────────────────────────────────────────────────────────────────────
-- The key node and the edge that says whose it is. `signs_for` is the recorded
-- answer to the question two databases used to answer differently.
--
-- The identity node is enqueued first, and from here rather than only from
-- pial_states, because a key may be registered for a PIAL this brain has not
-- been asked to bootstrap yet. The edge cannot land before both its ends exist,
-- and the drain delivers in id order, so the order of these three enqueues is
-- the order they are applied in.
--
-- One function serves both key tables: they differ in what the key is for, not
-- in whose it is.
CREATE OR REPLACE FUNCTION manhattan_register_key() RETURNS TRIGGER AS $$
BEGIN
    PERFORM manhattan_enqueue(
        'node',
        'node:pial:' || NEW.pial_id::text,
        jsonb_build_object(
            'kind',      'identity',
            'name',      'pial:' || NEW.pial_id::text,
            'namespace', 'pial'));

    PERFORM manhattan_enqueue(
        'node',
        'node:uuid:' || NEW.key_id::text,
        jsonb_build_object(
            'kind',      'key',
            'name',      'uuid:' || NEW.key_id::text,
            'namespace', 'uuid'));

    PERFORM manhattan_enqueue(
        'edge',
        'edge:uuid:' || NEW.key_id::text || '|signs_for|pial:' || NEW.pial_id::text,
        jsonb_build_object(
            'subject_name', 'uuid:' || NEW.key_id::text,
            'predicate',    'signs_for',
            'object_name',  'pial:' || NEW.pial_id::text));

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_manhattan_register_signing_key ON pial_signing_keys;
CREATE TRIGGER trg_manhattan_register_signing_key
    AFTER INSERT OR UPDATE OF key_id ON pial_signing_keys
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_key();

DROP TRIGGER IF EXISTS trg_manhattan_register_ecdh_key ON pial_ecdh_keys;
CREATE TRIGGER trg_manhattan_register_ecdh_key
    AFTER INSERT OR UPDATE OF key_id ON pial_ecdh_keys
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_key();

-- ── backfill ─────────────────────────────────────────────────────────────────
-- Everything that already exists is enqueued once, so the graph plane starts
-- complete instead of only knowing about identities asserted after this deploy.
-- Guarded on an empty outbox: rows here are never deleted, only marked
-- delivered, so a non-empty outbox means the backfill has already run. Each
-- statement is one set-based insert, and they run identities → keys → edges so
-- the BIGSERIAL order the drain follows is the order the graph needs.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM manhattan_outbox) THEN
        RETURN;
    END IF;

    INSERT INTO manhattan_outbox (op, dedup_key, payload)
    SELECT 'node', 'node:pial:' || pial_id::text,
           jsonb_build_object('kind','identity','name','pial:' || pial_id::text,'namespace','pial')
      FROM pial_states
    ON CONFLICT (dedup_key) DO NOTHING;

    INSERT INTO manhattan_outbox (op, dedup_key, payload)
    SELECT 'node', 'node:pial:' || pial_id::text,
           jsonb_build_object('kind','identity','name','pial:' || pial_id::text,'namespace','pial')
      FROM (SELECT pial_id FROM pial_signing_keys
            UNION SELECT pial_id FROM pial_ecdh_keys) k
    ON CONFLICT (dedup_key) DO NOTHING;

    INSERT INTO manhattan_outbox (op, dedup_key, payload)
    SELECT 'node', 'node:uuid:' || key_id::text,
           jsonb_build_object('kind','key','name','uuid:' || key_id::text,'namespace','uuid')
      FROM (SELECT key_id FROM pial_signing_keys
            UNION ALL SELECT key_id FROM pial_ecdh_keys) k
    ON CONFLICT (dedup_key) DO NOTHING;

    INSERT INTO manhattan_outbox (op, dedup_key, payload)
    SELECT 'edge',
           'edge:uuid:' || key_id::text || '|signs_for|pial:' || pial_id::text,
           jsonb_build_object('subject_name','uuid:' || key_id::text,
                              'predicate','signs_for',
                              'object_name','pial:' || pial_id::text)
      FROM (SELECT key_id, pial_id FROM pial_signing_keys
            UNION ALL SELECT key_id, pial_id FROM pial_ecdh_keys) k
    ON CONFLICT (dedup_key) DO NOTHING;
END $$;

-- ════════════════════════════════════════════════════════════════════════════
-- SCHEMA_CONTACT — the contact plane
-- ════════════════════════════════════════════════════════════════════════════

-- ── Per-identity contact defaults ────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS contact_policies (
    pial_id               UUID        PRIMARY KEY REFERENCES pial_states(pial_id) ON DELETE CASCADE,
    -- Reaching someone by @handle. 'open' preserves the platform's current behaviour.
    handle_policy         TEXT        NOT NULL DEFAULT 'open'
        CHECK (handle_policy IN ('open','followers','mutuals','number_only','capability_only','closed')),
    -- Applied to a Number with no policy row of its own.
    default_number_policy TEXT        NOT NULL DEFAULT 'number_only'
        CHECK (default_number_policy IN ('open','followers','mutuals','number_only','capability_only','closed')),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ── Per-Number policy ────────────────────────────────────────────────────────
-- A conference-badge Number and a business-card Number are different promises.
CREATE TABLE IF NOT EXISTS number_policies (
    number_node_id UUID        PRIMARY KEY,
    pial_id        UUID        NOT NULL REFERENCES pial_states(pial_id) ON DELETE CASCADE,
    policy         TEXT        NOT NULL DEFAULT 'number_only'
        CHECK (policy IN ('open','followers','mutuals','number_only','capability_only','closed')),
    label          TEXT        NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_number_policies_pial ON number_policies(pial_id);

-- ── Contact capabilities ─────────────────────────────────────────────────────
-- The bearer half of contact: 160 bits, never spoken, delivered as a link or QR.
-- Only the SHA-256 of the secret is stored, so a database read does not yield a
-- working capability.
CREATE TABLE IF NOT EXISTS contact_capabilities (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    pial_id        UUID        NOT NULL REFERENCES pial_states(pial_id) ON DELETE CASCADE,
    number_node_id UUID,
    secret_sha256  BYTEA       NOT NULL UNIQUE,
    label          TEXT        NOT NULL DEFAULT '',
    max_uses       INT,
    uses           INT         NOT NULL DEFAULT 0,
    expires_at     TIMESTAMPTZ,
    revoked_at     TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_contact_caps_pial ON contact_capabilities(pial_id);

-- ── Contact grants ───────────────────────────────────────────────────────────
-- A standing allow between two identities: a redeemed capability, an accepted
-- request, or an explicit choice. Survives Number rotation by design — it is
-- keyed by identity, not by the Number that introduced them.
CREATE TABLE IF NOT EXISTS contact_grants (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_pial     UUID        NOT NULL REFERENCES pial_states(pial_id) ON DELETE CASCADE,
    peer_pial      UUID        NOT NULL,
    source         TEXT        NOT NULL CHECK (source IN ('capability','request','manual')),
    number_node_id UUID,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at     TIMESTAMPTZ,
    UNIQUE (owner_pial, peer_pial)
);
CREATE INDEX IF NOT EXISTS idx_contact_grants_owner ON contact_grants(owner_pial) WHERE revoked_at IS NULL;

-- ── Contact requests ─────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS contact_requests (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_pial     UUID        NOT NULL REFERENCES pial_states(pial_id) ON DELETE CASCADE,
    requester_pial UUID        NOT NULL,
    number_node_id UUID,
    note           TEXT        NOT NULL DEFAULT '',
    status         TEXT        NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending','accepted','declined')),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    decided_at     TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_contact_requests_pending
    ON contact_requests(owner_pial, requester_pial) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_contact_requests_owner
    ON contact_requests(owner_pial, created_at DESC);

-- ── Messaging public keys ────────────────────────────────────────────────────
-- The X25519 public key Gnosis actually seals to. Authoritative here for the
-- same reason the signing and ECDH keys are: Manhattan assigns the 'key' node
-- kind to this brain. device_id is the seam per-device keys drop into; today
-- every identity registers exactly one row under the empty device.
CREATE TABLE IF NOT EXISTS pial_messaging_keys (
    pial_id        UUID        NOT NULL REFERENCES pial_states(pial_id) ON DELETE CASCADE,
    device_id      TEXT        NOT NULL DEFAULT '',
    key_id         UUID        NOT NULL DEFAULT gen_random_uuid(),
    public_key_b64 TEXT        NOT NULL,
    algorithm      TEXT        NOT NULL DEFAULT 'X25519',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (pial_id, device_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_messaging_key_id ON pial_messaging_keys(key_id);

-- ════════════════════════════════════════════════════════════════════════════
-- number_lease::SCHEMA — the lease plane
-- ════════════════════════════════════════════════════════════════════════════

-- ── The lease on a Number ────────────────────────────────────────────────────
-- Both columns are NULL by default, and NULL means "no limit". A Number that
-- existed before this migration keeps behaving exactly as it did: a lease is an
-- offer, never something applied retroactively to a promise already made.
ALTER TABLE number_policies ADD COLUMN IF NOT EXISTS expires_at     TIMESTAMPTZ;
ALTER TABLE number_policies ADD COLUMN IF NOT EXISTS max_admissions INT;

ALTER TABLE number_policies DROP CONSTRAINT IF EXISTS number_policies_budget_positive;
ALTER TABLE number_policies ADD CONSTRAINT number_policies_budget_positive
    CHECK (max_admissions IS NULL OR max_admissions > 0);

COMMENT ON COLUMN number_policies.expires_at IS
    'When this Number stops admitting new contact. NULL is never, and never is the default. Compared against the database''s own NOW() at decision time, so there is one clock and no skew.';
COMMENT ON COLUMN number_policies.max_admissions IS
    'How many DISTINCT identities this Number may admit. NULL is unlimited. Counted in admissions, never in resolve attempts: a counter a stranger can burn down is a denial of service on the owner.';

-- The sweeper's only query.
CREATE INDEX IF NOT EXISTS idx_number_policies_expiring
    ON number_policies (expires_at) WHERE expires_at IS NOT NULL;

-- ── The admission ledger ─────────────────────────────────────────────────────
-- One row per (Number, identity admitted). The primary key is the whole budget
-- rule: an identity admitted twice through the same Number is the same
-- admission, so a person who re-resolves a Number they already used spends
-- nothing. The row is a fact about an introduction that happened, so it is
-- never removed to give a slot back — a later mutual follow does not un-admit
-- anybody.
--
-- ON DELETE CASCADE from number_policies: retiring a Number drops its policy
-- row, and the `number` namespace is not reusable, so this Number can never
-- come back and its ledger has nothing left to say.
CREATE TABLE IF NOT EXISTS number_admissions (
    number_node_id UUID        NOT NULL
                   REFERENCES number_policies(number_node_id) ON DELETE CASCADE,
    initiator_pial UUID        NOT NULL,
    admitted_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (number_node_id, initiator_pial)
);
CREATE INDEX IF NOT EXISTS idx_number_admissions_initiator
    ON number_admissions (initiator_pial);
