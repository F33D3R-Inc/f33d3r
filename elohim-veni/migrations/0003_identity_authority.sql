-- 0003_identity_authority.sql
--
-- elohim-veni becomes the one identity and auth authority.
--
-- Until now a person was split across two databases: f33d3r_feed minted the
-- PIAL (pial_roots), held the account, the password, the second factor, the
-- capabilities, the trust row and the identity ledger, while this database held
-- a second PIAL table, a second capability table with a different vocabulary
-- and a second trust table with different columns. Every one of those pairs is
-- folded here into one table with one meaning:
--
--   pial_roots facts       → pial_states (dob, kyc, age, role, status)
--   users (identity part)  → accounts (id kept: content in f33d3r_feed keys on it)
--   pial_account_bindings  → accounts.pial_id / is_primary / bound_at
--   user_credentials       → credentials, account_totp
--   backup_codes           → backup_codes
--   user_sessions          → sessions (opaque token, SHA-256 at rest)
--   pial_capabilities  +
--   capability_grants      → pial_capabilities (feed-engine's SCREAMING vocabulary)
--   trust_scores ×2        → trust_scores (every column that has a reader)
--   pial_event_ledger      → pial_event_ledger (append-only, enforced)
--
-- and every change is published to Sitra Achra (identity.events) through
-- identity_outbox, written in the same transaction as the change.
--
-- FORWARD-ONLY: never edit this file after it has been applied anywhere.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ════════════════════════════════════════════════════════════════════════════
-- PIAL facts
-- ════════════════════════════════════════════════════════════════════════════
-- Ported from f33d3r_feed.pial_roots (0001_baseline.sql, the PIAL consolidation
-- block). One row per human. is_adult / is_minor are stored, derived from dob at
-- every write and re-derived by the age sweep (identity/maintenance.rs) when a
-- minor comes of age — a stored age fact that never ages is a compliance bug.
ALTER TABLE pial_states
    ADD COLUMN dob             DATE,
    ADD COLUMN dob_locked      BOOLEAN     NOT NULL DEFAULT FALSE,
    ADD COLUMN age_verified    BOOLEAN     NOT NULL DEFAULT FALSE,
    ADD COLUMN is_adult        BOOLEAN     NOT NULL DEFAULT FALSE,
    ADD COLUMN is_minor        BOOLEAN     NOT NULL DEFAULT FALSE,
    ADD COLUMN kyc_tier        TEXT        NOT NULL DEFAULT 'none',
    ADD COLUMN kyc_verified_at TIMESTAMPTZ,
    ADD COLUMN role            TEXT        NOT NULL DEFAULT 'user',
    ADD COLUMN tombstoned_at   TIMESTAMPTZ;

-- One status vocabulary. This table said ACTIVE | SUSPENDED | REVOKED (a
-- ban); pial_roots said active | suspended | tombstoned | purged. The union,
-- lower case, with the ban called what it is: banned. A status outside the
-- vocabulary fails this migration by the CHECK below: what it meant is a
-- decision for a person.
UPDATE pial_states
   SET status = CASE lower(status) WHEN 'revoked' THEN 'banned' ELSE lower(status) END
 WHERE status <> CASE lower(status) WHEN 'revoked' THEN 'banned' ELSE lower(status) END;
ALTER TABLE pial_states ALTER COLUMN status SET DEFAULT 'active';

ALTER TABLE pial_states
    ADD CONSTRAINT pial_states_status_known
        CHECK (status IN ('active','suspended','banned','tombstoned','purged')),
    ADD CONSTRAINT pial_states_kyc_tier_known
        CHECK (kyc_tier IN ('none','basic','soft','full')),
    ADD CONSTRAINT pial_states_role_known
        CHECK (role IN ('user','admin','founder')),
    ADD CONSTRAINT pial_states_age_coherent
        CHECK (NOT (is_adult AND is_minor)),
    ADD CONSTRAINT pial_states_tombstone_coherent
        CHECK ((status IN ('tombstoned','purged')) = (tombstoned_at IS NOT NULL));

CREATE INDEX idx_pial_role ON pial_states(role) WHERE role <> 'user';

-- ── The founder ──────────────────────────────────────────────────────────────
-- Ported from f33d3r_feed 0033_one_founder.sql. The founder is the first
-- account ever created; there is at most one; the role is permanent. The
-- first-account decision is made in the account-create transaction
-- (identity/accounts.rs); this index is the database's guarantee that no path
-- can grant it twice, and the trigger is its guarantee that none can grant or
-- remove it after the identity exists.
CREATE UNIQUE INDEX uq_pial_states_one_founder ON pial_states ((role)) WHERE role = 'founder';

-- The one exception is the one-shot import (`elohim-veni import`), which
-- carries the founder over from f33d3r_feed — where the first-account decision
-- was already made — onto a PIAL this brain may already know by bootstrap. It
-- sets elohim.identity_import for its own transaction only; the unique index
-- above still holds it to one founder.
CREATE OR REPLACE FUNCTION founder_role_is_permanent() RETURNS TRIGGER AS $$
BEGIN
    IF current_setting('elohim.identity_import', TRUE) = 'on' THEN
        RETURN NEW;
    END IF;
    IF OLD.role = 'founder' AND NEW.role <> 'founder' THEN
        RAISE EXCEPTION 'founder_role_permanent: the founder role cannot be removed from PIAL %', OLD.pial_id
            USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.role = 'founder' AND OLD.role <> 'founder' THEN
        RAISE EXCEPTION 'founder_role_permanent: the founder role is granted only to the first account, at creation (PIAL %)', NEW.pial_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_founder_role_is_permanent
    BEFORE UPDATE OF role ON pial_states
    FOR EACH ROW EXECUTE FUNCTION founder_role_is_permanent();

-- ════════════════════════════════════════════════════════════════════════════
-- Trust — one table
-- ════════════════════════════════════════════════════════════════════════════
-- This table kept the decision engine's signal (score, anomaly_score,
-- last_decision; read by /v1/decisions and /v1/pial/:id/trust); f33d3r_feed's
-- kept enforcement (trust_level, nsfw_tier, enforcement_state,
-- last_violation_at, cooldown_until; read by GetTrustScore, the suggestion
-- ranker and admin suspension). violation_count is in both and means the same
-- thing. Every column with a reader stays.
ALTER TABLE trust_scores
    ADD COLUMN trust_level       INT         NOT NULL DEFAULT 50,
    ADD COLUMN nsfw_tier         INT         NOT NULL DEFAULT 0,
    ADD COLUMN enforcement_state TEXT        NOT NULL DEFAULT 'none',
    ADD COLUMN last_violation_at TIMESTAMPTZ,
    ADD COLUMN cooldown_until    TIMESTAMPTZ,
    ADD CONSTRAINT trust_level_range       CHECK (trust_level BETWEEN 0 AND 100),
    ADD CONSTRAINT trust_nsfw_tier_range   CHECK (nsfw_tier BETWEEN 0 AND 4),
    ADD CONSTRAINT trust_enforcement_known
        CHECK (enforcement_state IN ('none','warning','restricted','suspended','terminated'));

-- Every PIAL has a trust row from the moment it exists (pial_init_trust in
-- f33d3r_feed). A trigger, so no writer can forget — the old bootstrap handler
-- seeded it with a discarded error.
CREATE OR REPLACE FUNCTION pial_init_trust() RETURNS TRIGGER AS $$
BEGIN
    INSERT INTO trust_scores (pial_id) VALUES (NEW.pial_id) ON CONFLICT (pial_id) DO NOTHING;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_pial_init_trust
    AFTER INSERT ON pial_states
    FOR EACH ROW EXECUTE FUNCTION pial_init_trust();

INSERT INTO trust_scores (pial_id) SELECT pial_id FROM pial_states ON CONFLICT (pial_id) DO NOTHING;

-- ════════════════════════════════════════════════════════════════════════════
-- Capabilities — one vocabulary
-- ════════════════════════════════════════════════════════════════════════════
-- feed-engine's names (feed-engine/internal/model/types.go) and states. The
-- CHECKs make the vocabulary a property of the table: a capability nobody
-- defined cannot be granted, and adding one is a migration.
CREATE TABLE pial_capabilities (
    pial_id    UUID        NOT NULL REFERENCES pial_states(pial_id) ON DELETE CASCADE,
    capability TEXT        NOT NULL CHECK (capability IN (
                   'POSTING','NSFW_ACCESS','MONETIZATION','REALM_PROGRESSION',
                   'NEW_ACCOUNT_TRUST','MESSAGING','MUSIC_UPLOAD')),
    state      TEXT        NOT NULL DEFAULT 'granted'
               CHECK (state IN ('granted','restricted','revoked','cooldown')),
    expires_at TIMESTAMPTZ,
    reason     TEXT        NOT NULL DEFAULT '',
    granted_by TEXT        NOT NULL DEFAULT 'system',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- When the expiry of this row's expires_at was announced on
    -- identity.events (identity/maintenance.rs). Cleared by every write, so a
    -- new expiry is announced in its turn.
    expiry_announced_at TIMESTAMPTZ,
    PRIMARY KEY (pial_id, capability),
    -- A cooldown is a denial with an end; without the end it is a revocation
    -- that says otherwise.
    CONSTRAINT pial_capabilities_cooldown_ends CHECK (state <> 'cooldown' OR expires_at IS NOT NULL)
);

-- capability_grants (this brain's alternative: lower-case names, a boolean) is
-- reconciled into the one vocabulary and then removed. A name with no meaning
-- in the vocabulary stops the migration and is named: which capability it was
-- meant to be is a decision for a person.
DO $$
DECLARE
    unknown TEXT;
BEGIN
    SELECT string_agg(DISTINCT capability, ', ')
      INTO unknown
      FROM capability_grants
     WHERE upper(capability) NOT IN ('POSTING','NSFW_ACCESS','MONETIZATION','REALM_PROGRESSION',
                                     'NEW_ACCOUNT_TRUST','MESSAGING','MUSIC_UPLOAD',
                                     'POST','MESSAGE','MONETIZE','ADULT_CONTENT');
    IF unknown IS NOT NULL THEN
        RAISE EXCEPTION 'capability_grants holds capabilities outside the vocabulary (%); '
                        'map or delete them, then restart', unknown;
    END IF;

    INSERT INTO pial_capabilities (pial_id, capability, state, expires_at, reason, granted_by, created_at, updated_at)
    SELECT pial_id,
           CASE upper(capability)
               WHEN 'POST'          THEN 'POSTING'
               WHEN 'MESSAGE'       THEN 'MESSAGING'
               WHEN 'MONETIZE'      THEN 'MONETIZATION'
               WHEN 'ADULT_CONTENT' THEN 'NSFW_ACCESS'
               ELSE upper(capability)
           END,
           CASE WHEN granted THEN 'granted' ELSE 'revoked' END,
           expires_at,
           COALESCE(reason, ''),
           COALESCE(granted_by::text, 'system'),
           created_at,
           updated_at
      FROM capability_grants
    ON CONFLICT (pial_id, capability) DO NOTHING;
END $$;

DROP TABLE capability_grants;

-- ════════════════════════════════════════════════════════════════════════════
-- Accounts
-- ════════════════════════════════════════════════════════════════════════════
-- An account is a handle-bearing persona bound to a PIAL. id is the SAME UUID
-- f33d3r_feed.users.id carries, so every work, follow and message keyed on it
-- keeps working. The binding (pial_account_bindings) is folded into the row:
-- a separate table carrying pial_id next to accounts.pial_id would be two
-- answers to one question.
CREATE TABLE accounts (
    id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    pial_id            UUID        NOT NULL REFERENCES pial_states(pial_id),
    handle             CITEXT      NOT NULL UNIQUE REFERENCES handles(handle) ON UPDATE CASCADE,
    is_primary         BOOLEAN     NOT NULL DEFAULT TRUE,
    bound_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deactivated_at     TIMESTAMPTZ,
    deactivated_reason TEXT        NOT NULL DEFAULT '',
    -- A purged account is gone for good: its credentials are deleted, it can
    -- never sign in or be reactivated. The row stays because content, the
    -- ledger and the handle registry name it.
    purged_at          TIMESTAMPTZ,
    CONSTRAINT accounts_purge_deactivates CHECK (purged_at IS NULL OR deactivated_at IS NOT NULL)
);
CREATE INDEX idx_accounts_pial ON accounts(pial_id);
CREATE INDEX idx_accounts_created ON accounts(created_at);
CREATE UNIQUE INDEX uq_accounts_one_primary ON accounts(pial_id) WHERE is_primary;

-- An account's handle is bound, in the registry, to the account's own PIAL.
-- Checked at COMMIT (deferred), so one transaction may move an account and its
-- handle together — linking an account to another identity does exactly that —
-- while no transaction can commit them apart. The registry routes refuse to
-- move a handle an account holds before they ever reach this; this is the
-- guarantee behind the refusal.
CREATE OR REPLACE FUNCTION account_handle_bound_to_own_pial() RETURNS TRIGGER AS $$
DECLARE
    broken TEXT;
BEGIN
    SELECT string_agg(a.handle::text || ' (account ' || a.id::text || ')', ', ')
      INTO broken
      FROM accounts a
      JOIN handles h ON h.handle = a.handle
     WHERE h.pial_id <> a.pial_id
       AND a.handle = NEW.handle;
    IF broken IS NOT NULL THEN
        RAISE EXCEPTION 'handle_in_use: % is held by an account on another identity', broken
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER trg_accounts_handle_bound
    AFTER INSERT OR UPDATE OF handle, pial_id ON accounts
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION account_handle_bound_to_own_pial();

CREATE CONSTRAINT TRIGGER trg_handles_account_bound
    AFTER UPDATE OF handle, pial_id ON handles
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION account_handle_bound_to_own_pial();

-- ════════════════════════════════════════════════════════════════════════════
-- Credentials
-- ════════════════════════════════════════════════════════════════════════════
-- password_hash is bcrypt ($2a$, from Go's bcrypt or pgcrypto's gen_salt('bf'),
-- which interoperate), verified with pgcrypto: password_hash = crypt($1, password_hash).
-- An account with no password has no row; it never has an empty hash.
CREATE TABLE credentials (
    account_id    UUID        PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    password_hash TEXT        NOT NULL CHECK (password_hash LIKE '$2%'),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- RFC 6238 second factor. A row exists from enrolment; enabled flips on the
-- first verified code. Disabling deletes the row, secret included.
-- last_used_step is the 30-second step of the last code accepted: a code is
-- accepted only for a later step, so an observed code cannot be replayed
-- inside its validity window (RFC 6238 §5.2).
CREATE TABLE account_totp (
    account_id     UUID        PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    secret         TEXT        NOT NULL CHECK (secret <> ''),
    enabled        BOOLEAN     NOT NULL DEFAULT FALSE,
    last_used_step BIGINT      NOT NULL DEFAULT 0,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Single-use recovery codes, bcrypt-hashed over the normalised code.
CREATE TABLE backup_codes (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    code_hash  TEXT        NOT NULL CHECK (code_hash LIKE '$2%'),
    used       BOOLEAN     NOT NULL DEFAULT FALSE,
    used_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_backup_codes_unused ON backup_codes(account_id) WHERE NOT used;

-- ════════════════════════════════════════════════════════════════════════════
-- Sessions
-- ════════════════════════════════════════════════════════════════════════════
-- The client holds an opaque token (32 random bytes, base64url). Only its
-- SHA-256 is stored, so a read of this table yields no working session.
-- account_id is the persona the session is acting as (it moves on an account
-- switch); login_account_id is the account whose credentials opened it, which
-- is what "end every session of this account" means after a password change.
CREATE TABLE sessions (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    token_sha256     BYTEA       NOT NULL UNIQUE CHECK (octet_length(token_sha256) = 32),
    account_id       UUID        NOT NULL REFERENCES accounts(id),
    login_account_id UUID        NOT NULL REFERENCES accounts(id),
    pial_id          UUID        NOT NULL REFERENCES pial_states(pial_id),
    device_name      TEXT        NOT NULL DEFAULT '',
    ip               TEXT        NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at       TIMESTAMPTZ NOT NULL DEFAULT NOW() + INTERVAL '30 days',
    revoked_at       TIMESTAMPTZ,
    revoked_reason   TEXT
);
CREATE INDEX idx_sessions_account_live ON sessions(account_id)       WHERE revoked_at IS NULL;
CREATE INDEX idx_sessions_login_live   ON sessions(login_account_id) WHERE revoked_at IS NULL;
CREATE INDEX idx_sessions_pial_live    ON sessions(pial_id)          WHERE revoked_at IS NULL;
CREATE INDEX idx_sessions_reapable     ON sessions(COALESCE(revoked_at, expires_at));

-- ── Recovery ─────────────────────────────────────────────────────────────────
-- A spent backup code buys one short-lived recovery token, which sets a new
-- password exactly once. Only the token's SHA-256 is stored.
CREATE TABLE recovery_tokens (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    token_sha256 BYTEA       NOT NULL UNIQUE CHECK (octet_length(token_sha256) = 32),
    account_id   UUID        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at   TIMESTAMPTZ NOT NULL DEFAULT NOW() + INTERVAL '15 minutes',
    used_at      TIMESTAMPTZ
);
CREATE INDEX idx_recovery_tokens_account ON recovery_tokens(account_id);

-- ════════════════════════════════════════════════════════════════════════════
-- Identity ledger
-- ════════════════════════════════════════════════════════════════════════════
-- Ported from f33d3r_feed.pial_event_ledger. Append-only, and now enforced:
-- the database refuses UPDATE, DELETE and TRUNCATE.
CREATE TABLE pial_event_ledger (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    pial_id    UUID        NOT NULL REFERENCES pial_states(pial_id),
    account_id UUID,
    event_type TEXT        NOT NULL,
    payload    JSONB       NOT NULL DEFAULT '{}',
    source     TEXT        NOT NULL DEFAULT 'system',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_pial_ledger      ON pial_event_ledger(pial_id, created_at DESC);
CREATE INDEX idx_pial_ledger_type ON pial_event_ledger(pial_id, event_type, created_at DESC);

CREATE TRIGGER trg_pial_ledger_append_only
    BEFORE UPDATE OR DELETE ON pial_event_ledger
    FOR EACH ROW EXECUTE FUNCTION refuse_history_rewrite();
CREATE TRIGGER trg_pial_ledger_no_truncate
    BEFORE TRUNCATE ON pial_event_ledger
    FOR EACH STATEMENT EXECUTE FUNCTION refuse_history_rewrite();

-- ════════════════════════════════════════════════════════════════════════════
-- identity.events — the transactional outbox
-- ════════════════════════════════════════════════════════════════════════════
-- One row per published event, written in the transaction that made the
-- change. The relay (identity/relay.rs) publishes rows in id order to the
-- identity.events topic through the Sitra Achra HTTP proxy and marks a row only
-- after the broker answers with an offset. partition_key keeps one identity's
-- events in one partition, in order; record_key is the Kafka record key, the
-- PIAL, so every record about one identity shares a key.
CREATE TABLE identity_outbox (
    id             BIGSERIAL   PRIMARY KEY,
    event_id       UUID        NOT NULL UNIQUE,
    partition_key  TEXT        NOT NULL,
    record_key     TEXT        NOT NULL,
    event          JSONB       NOT NULL,
    attempts       INT         NOT NULL DEFAULT 0,
    last_error     TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    delivered_at   TIMESTAMPTZ,
    blocked_at     TIMESTAMPTZ,
    blocked_reason TEXT
);
CREATE INDEX idx_capability_expiry_unannounced
    ON pial_capabilities(expires_at) WHERE expires_at IS NOT NULL AND expiry_announced_at IS NULL;
CREATE INDEX idx_identity_outbox_deliverable
    ON identity_outbox(id) WHERE delivered_at IS NULL AND blocked_at IS NULL;
CREATE INDEX idx_identity_outbox_blocked
    ON identity_outbox(id) WHERE blocked_at IS NOT NULL AND delivered_at IS NULL;

-- ════════════════════════════════════════════════════════════════════════════
-- Genesis
-- ════════════════════════════════════════════════════════════════════════════
-- The founder is "the first account ever created". On a platform whose
-- accounts still live in f33d3r_feed, the first account created HERE is not
-- that — so account creation stays closed until the identity plane has its
-- history: the one-shot import opens it when it commits (an empty source opens
-- it too, which is the brand-new-platform case). Until then POST /v1/accounts
-- answers 503 identity_plane_sealed, and no signup can take the founder role
-- from the person who holds it.
CREATE TABLE identity_genesis (
    singleton BOOLEAN     PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    opened_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    opened_by TEXT        NOT NULL,
    detail    JSONB       NOT NULL DEFAULT '{}'
);
