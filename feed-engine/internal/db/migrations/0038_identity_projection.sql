-- 0038_identity_projection.sql
--
-- feed-engine stops being an identity authority and becomes a projection of one.
--
-- elohim-veni is the one identity and authentication authority: PIAL roots,
-- account bindings, handles, credentials, sessions, second factors, role,
-- capabilities, age and KYC facts, standing. feed-engine keeps what it needs to
-- render and to join on, and nothing it could decide with:
--
--   users.handle, users.pial_id, users.role, users.deactivated_at
--       — the account columns every timeline query joins through. Written only
--         by the identity projection (db.ProjectAccount / db.ApplyIdentityEvent).
--
--   identity_facts
--       — one row per PIAL: role, status, granted capabilities, age and KYC
--         facts. Replaces every SQL read of pial_roots and pial_capabilities.
--
--   identity_commands
--       — the transactional outbox for identity writes that must commit with a
--         local change (a CSAM hold with its report, an org approval with its
--         badge, a purge with its erasure). Drained to elohim-veni in order.
--
--   identity_event_deadletter
--       — identity.events records that can never be applied, kept with the
--         reason, so the projection never silently skips one.
--
-- WHAT THIS MIGRATION DOES NOT DO: drop anything. The old authority tables
-- (pial_roots, pial_capabilities, pial_account_bindings, user_credentials,
-- user_sessions, backup_codes, pial_signing_keys, pial_ecdh_keys) keep every row,
-- untouched and no longer written, because elohim-veni's one-time import reads
-- them. They are dropped by the staged retirement migration in
-- internal/db/migrations_staged/, which is promoted into migrations/ only after
-- the import is confirmed — see the header of that file for the order.
--
-- Kept by decision, and re-anchored on the projection:
--   trust_scores       — Trust & Safety ranking input (trust level, NSFW tier,
--                        cooldowns) that this brain's moderation pipeline writes
--                        and its suggestion queries read. Not an identity fact.
--   pial_event_ledger  — this brain's append-only audit trail of what IT did to
--                        an identity's content and standing (reports, realm
--                        grants, compliance, moderation). elohim-veni keeps its
--                        own ledger of identity events; this one is not it.

-- ── identity_facts ───────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS identity_facts (
    pial_id            UUID        PRIMARY KEY,
    role               TEXT        NOT NULL DEFAULT 'user',
    status             TEXT        NOT NULL DEFAULT 'active',
    caps               TEXT[]      NOT NULL DEFAULT '{}',
    age_verified       BOOLEAN     NOT NULL DEFAULT FALSE,
    is_adult           BOOLEAN     NOT NULL DEFAULT FALSE,
    is_minor           BOOLEAN     NOT NULL DEFAULT FALSE,
    kyc_tier           TEXT        NOT NULL DEFAULT 'none',
    kyc_verified_at    TIMESTAMPTZ,
    date_of_birth      DATE,
    -- The identity.events record this row was last brought to. A row seeded
    -- from a synchronous signup answer, or backfilled below, carries
    -- '-infinity' so the first event about it always wins.
    source_event_id    TEXT        NOT NULL DEFAULT '',
    source_occurred_at TIMESTAMPTZ NOT NULL DEFAULT '-infinity',
    -- TRUE once elohim-veni has asserted this identity by an event. The
    -- retirement migration refuses to run while any account's identity is
    -- still unconfirmed: that is the local proof the import is complete.
    confirmed          BOOLEAN     NOT NULL DEFAULT FALSE,
    projected_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_identity_facts_unconfirmed
    ON identity_facts (pial_id) WHERE NOT confirmed;

-- Backfill from the old authority tables, so every join reads the same answer
-- the moment this migration commits. caps holds the capabilities in force now:
-- granted and unexpired, or a cooldown whose end has passed.
INSERT INTO identity_facts
    (pial_id, role, status, caps, age_verified, is_adult, is_minor,
     kyc_tier, kyc_verified_at, date_of_birth)
SELECT pr.pial_id,
       COALESCE(pr.role, 'user'),
       CASE WHEN pr.is_tombstoned THEN 'purged' ELSE COALESCE(pr.status, 'active') END,
       COALESCE((
           SELECT array_agg(pc.capability ORDER BY pc.capability)
             FROM pial_capabilities pc
            WHERE pc.pial_id = pr.pial_id
              AND (   (pc.state = 'granted'  AND (pc.expires_at IS NULL OR pc.expires_at > NOW()))
                   OR (pc.state = 'cooldown' AND pc.expires_at IS NOT NULL AND pc.expires_at <= NOW()))
       ), '{}'),
       COALESCE(pr.age_verified, FALSE),
       COALESCE(pr.is_adult, FALSE),
       COALESCE(pr.is_minor, FALSE),
       COALESCE(pr.kyc_tier, 'none'),
       pr.kyc_verified_at,
       pr.date_of_birth
  FROM pial_roots pr
ON CONFLICT (pial_id) DO NOTHING;

-- ── users: the account projection's version guard ───────────────────────────
-- Per-account ordering of identity.events: an older event about an account
-- never overwrites a newer one.
ALTER TABLE users ADD COLUMN IF NOT EXISTS identity_event_at TIMESTAMPTZ NOT NULL DEFAULT '-infinity';

-- users.role carried 'banned' as a projection of a ban; it stays that way.
-- Nothing else about the column changes.

-- ── Re-anchor every foreign key that names an identity ───────────────────────
-- Tables that hold a pial_id keep their referential guarantee, now against the
-- projection — the set of identities this brain knows — instead of against an
-- authority table that is about to stop receiving new identities. The retiring
-- tables themselves keep their keys into pial_roots: they receive no new rows
-- and are dropped together with it.
--
-- NOT VALID then VALIDATE, as 0014 and 0018 established: the add skips the scan
-- under ACCESS EXCLUSIVE, the validate scans under SHARE UPDATE EXCLUSIVE. Every
-- referenced id is already present: the backfill above copied every root.
DO $$
DECLARE
    fk     RECORD;
    newdef TEXT;
BEGIN
    FOR fk IN
        SELECT c.conname,
               c.conrelid::regclass AS tbl,
               pg_get_constraintdef(c.oid) AS def
          FROM pg_constraint c
         WHERE c.contype = 'f'
           AND c.confrelid = 'pial_roots'::regclass
           AND c.conrelid NOT IN ('pial_capabilities'::regclass,
                                  'pial_account_bindings'::regclass,
                                  'user_sessions'::regclass)
    LOOP
        newdef := regexp_replace(fk.def, 'REFERENCES\s+(\S+\.)?pial_roots\(', 'REFERENCES identity_facts(');
        IF newdef = fk.def THEN
            RAISE EXCEPTION 'cannot re-anchor foreign key % on %: unexpected definition %',
                            fk.conname, fk.tbl, fk.def;
        END IF;
        EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I', fk.tbl, fk.conname);
        EXECUTE format('ALTER TABLE %s ADD CONSTRAINT %I %s NOT VALID', fk.tbl, fk.conname, newdef);
        EXECUTE format('ALTER TABLE %s VALIDATE CONSTRAINT %I', fk.tbl, fk.conname);
    END LOOP;
END $$;

-- ── trust_scores follows identities into the projection ──────────────────────
-- A trust row was created by a trigger on pial_roots, which no longer receives
-- identities. The same initialisation now fires when this brain first learns of
-- an identity, and the old trigger goes so nothing depends on the retiring table.
DROP TRIGGER IF EXISTS trg_pial_init_trust ON pial_roots;
DROP TRIGGER IF EXISTS trg_identity_init_trust ON identity_facts;
CREATE TRIGGER trg_identity_init_trust
    AFTER INSERT ON identity_facts
    FOR EACH ROW EXECUTE FUNCTION pial_init_trust();

-- ── identity_commands: the identity write outbox ─────────────────────────────
CREATE TABLE IF NOT EXISTS identity_commands (
    id            BIGSERIAL   PRIMARY KEY,
    kind          TEXT        NOT NULL CHECK (kind IN ('facts', 'capability', 'ban', 'unban', 'purge')),
    pial_id       UUID        NOT NULL,
    payload       JSONB       NOT NULL DEFAULT '{}',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    attempts      INT         NOT NULL DEFAULT 0,
    next_attempt  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_error    TEXT        NOT NULL DEFAULT '',
    delivered_at  TIMESTAMPTZ,
    -- Set when elohim-veni refused the command outright. A refused identity
    -- write is never retried and never forgotten: it stays here, named, for an
    -- operator, and the relay logs it as critical.
    refused_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_identity_commands_pending
    ON identity_commands (id) WHERE delivered_at IS NULL AND refused_at IS NULL;

-- ── identity_event_deadletter ────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS identity_event_deadletter (
    id          BIGSERIAL   PRIMARY KEY,
    topic       TEXT        NOT NULL,
    partition   INT         NOT NULL,
    "offset"    BIGINT      NOT NULL,
    record      BYTEA       NOT NULL,
    reason      TEXT        NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (topic, partition, "offset")
);
