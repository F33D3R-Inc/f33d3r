use anyhow::Result;
use sqlx::PgPool;

/// Verity owns evidence and legal records — never a verification level.
///
/// Before this reshape verity also decided and stored a TIER: `attestations`
/// carried `creator_tier`, `/v1/admin/tier` wrote one, `/v1/kyc/submit` promoted
/// to one, and a risk profile fed a decision engine that computed one. Identity
/// (elohim-veni) is the single owner of an account's verification level, so
/// every one of those tables leaves here.
///
/// The old shapes are removed only when they hold nothing that is evidence. A
/// legacy `kyc_submissions`, `records_2257` or `attestations` that still has
/// rows stops the boot with the count, because KYC evidence and 2257 records
/// are legal records and are never discarded as a side effect of a deploy.
/// `risk_profiles` and `csam_scans` are dropped outright: the first fed only
/// the removed tier engine, the second was the log of a stub that answered
/// "clean" to every hash and was called by nothing. `verity_audit` is reshaped
/// in place, so its existing rows survive.
const RESHAPE: &str = r#"
DO $$
DECLARE
    n BIGINT;
BEGIN
    IF to_regclass('public.attestations') IS NOT NULL THEN
        EXECUTE 'SELECT COUNT(*) FROM attestations' INTO n;
        IF n > 0 THEN
            RAISE EXCEPTION 'verity reshape refused: attestations holds % rows; tiers now live in identity (elohim-veni) — move them there before this deploy', n;
        END IF;
        DROP TABLE attestations;
    END IF;

    IF EXISTS (SELECT 1 FROM information_schema.columns
                WHERE table_schema = 'public' AND table_name = 'records_2257'
                  AND column_name = 'creator_pial_id') THEN
        EXECUTE 'SELECT COUNT(*) FROM records_2257' INTO n;
        IF n > 0 THEN
            RAISE EXCEPTION 'verity reshape refused: legacy records_2257 holds % rows; 2257 records are never dropped by a migration', n;
        END IF;
        DROP TABLE records_2257;
    END IF;

    IF EXISTS (SELECT 1 FROM information_schema.columns
                WHERE table_schema = 'public' AND table_name = 'kyc_submissions'
                  AND column_name = 'submission_type') THEN
        EXECUTE 'SELECT COUNT(*) FROM kyc_submissions' INTO n;
        IF n > 0 THEN
            RAISE EXCEPTION 'verity reshape refused: legacy kyc_submissions holds % rows; KYC evidence is never dropped by a migration', n;
        END IF;
        DROP TABLE kyc_submissions;
    END IF;

    DROP TABLE IF EXISTS risk_profiles;
    DROP TABLE IF EXISTS csam_scans;
END $$;

-- The audit trail keeps its rows; only the tier columns go.
ALTER TABLE IF EXISTS verity_audit DROP COLUMN IF EXISTS before_tier;
ALTER TABLE IF EXISTS verity_audit DROP COLUMN IF EXISTS after_tier;
ALTER TABLE IF EXISTS verity_audit DROP COLUMN IF EXISTS context;
ALTER TABLE IF EXISTS verity_audit ADD COLUMN IF NOT EXISTS attempt_id    UUID;
ALTER TABLE IF EXISTS verity_audit ADD COLUMN IF NOT EXISTS submission_id UUID;
"#;

const SCHEMA: &str = r#"
-- ── KYC submissions ──────────────────────────────────────────────────────────
-- The evidence for ONE verification attempt, as decided by ekyc and forwarded
-- by feed-engine. One row per attempt; attempt_id is the idempotency key.
--
-- What is held, and for how long, is enforced here rather than trusted to the
-- writer:
--   * legal_name and dob exist only on a pass;
--   * a pass always carries the document facts that justify it;
--   * a fail's PII (document facts, consent ip / user agent) is purged by the
--     retention job after 30 days — reasons, decision and scores stay.
-- The raw document number never reaches verity; doc_hmac is ekyc's keyed hash.
CREATE TABLE IF NOT EXISTS kyc_submissions (
    id                   UUID        PRIMARY KEY,
    attempt_id           UUID        NOT NULL UNIQUE,
    pial_id              UUID        NOT NULL,
    purpose              TEXT        NOT NULL CHECK (purpose  IN ('age','creator')),
    decision             TEXT        NOT NULL CHECK (decision IN ('pass','fail')),
    reasons              TEXT[]      NOT NULL DEFAULT '{}',
    evidence_types       TEXT[]      NOT NULL DEFAULT '{}',
    doc_hmac             TEXT        CHECK (doc_hmac ~ '^[0-9a-f]{64}$'),
    doc_type             TEXT        CHECK (doc_type IN ('passport','id_card','drivers_license')),
    issuing_country      TEXT,
    issuing_region       TEXT,
    doc_expiry           DATE,
    legal_name           TEXT,
    dob                  DATE,
    face_cosine          REAL,
    pad_score            REAL,
    liveness_score       REAL,
    consent_version      TEXT        NOT NULL CHECK (consent_version <> ''),
    consent_accepted_at  TIMESTAMPTZ NOT NULL,
    consent_ip           TEXT,
    consent_user_agent   TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    pii_purged_at        TIMESTAMPTZ,

    CONSTRAINT kyc_identity_only_on_pass
        CHECK (decision = 'pass' OR (legal_name IS NULL AND dob IS NULL)),
    CONSTRAINT kyc_pass_is_documented
        CHECK (decision = 'fail' OR (doc_hmac IS NOT NULL AND doc_type IS NOT NULL
                                     AND issuing_country IS NOT NULL AND doc_expiry IS NOT NULL
                                     AND legal_name IS NOT NULL AND dob IS NOT NULL)),
    CONSTRAINT kyc_only_fails_are_purged
        CHECK (pii_purged_at IS NULL OR decision = 'fail'),
    CONSTRAINT kyc_consent_ip_until_purged
        CHECK (pii_purged_at IS NOT NULL OR consent_ip IS NOT NULL),
    CONSTRAINT kyc_purge_is_complete
        CHECK (pii_purged_at IS NULL OR (doc_hmac IS NULL AND doc_type IS NULL
                                         AND issuing_country IS NULL AND issuing_region IS NULL
                                         AND doc_expiry IS NULL AND consent_ip IS NULL
                                         AND consent_user_agent IS NULL)),
    -- The key the dependent tables reference, so the database itself refuses
    -- an ID copy, a binding or a 2257 record hung off the wrong kind of attempt
    -- or the wrong person.
    CONSTRAINT kyc_submission_identity UNIQUE (id, pial_id, purpose, decision)
);
CREATE INDEX IF NOT EXISTS idx_kyc_pial_latest ON kyc_submissions(pial_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_kyc_fail_purge
    ON kyc_submissions(created_at) WHERE decision = 'fail' AND pii_purged_at IS NULL;

-- ── Document binding ─────────────────────────────────────────────────────────
-- One passing document → one PIAL. The primary key on doc_hmac IS the rule: a
-- second PIAL cannot bind a document another PIAL has passed with. The first
-- passing submission is recorded as the reason the binding exists.
CREATE TABLE IF NOT EXISTS kyc_document_bindings (
    doc_hmac       TEXT        PRIMARY KEY CHECK (doc_hmac ~ '^[0-9a-f]{64}$'),
    pial_id        UUID        NOT NULL,
    submission_id  UUID        NOT NULL,
    purpose        TEXT        NOT NULL,
    decision       TEXT        NOT NULL CHECK (decision = 'pass'),
    bound_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (submission_id, pial_id, purpose, decision)
        REFERENCES kyc_submissions(id, pial_id, purpose, decision)
);
CREATE INDEX IF NOT EXISTS idx_kyc_binding_pial ON kyc_document_bindings(pial_id);

-- ── ID copies (2257) ─────────────────────────────────────────────────────────
-- The copy of a creator's identification document that 18 U.S.C. 2257
-- requires. Only a passing purpose=creator submission may own one (the
-- composite key below), and every image is AES-256-GCM ciphertext under
-- VERITY_RECORDS_KEY: nonce + ciphertext-with-tag per image, plus the id of
-- the key that sealed it. No plaintext image is ever stored. The back is
-- absent for documents that have none (passports).
CREATE TABLE IF NOT EXISTS kyc_id_copies (
    submission_id    UUID        PRIMARY KEY,
    pial_id          UUID        NOT NULL,
    purpose          TEXT        NOT NULL CHECK (purpose  = 'creator'),
    decision         TEXT        NOT NULL CHECK (decision = 'pass'),
    key_id           TEXT        NOT NULL,
    front_nonce      BYTEA       NOT NULL CHECK (length(front_nonce) = 12),
    front_ct         BYTEA       NOT NULL,
    back_nonce       BYTEA       CHECK (back_nonce IS NULL OR length(back_nonce) = 12),
    back_ct          BYTEA,
    portrait_nonce   BYTEA       NOT NULL CHECK (length(portrait_nonce) = 12),
    portrait_ct      BYTEA       NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT id_copy_back_whole CHECK ((back_nonce IS NULL) = (back_ct IS NULL)),
    FOREIGN KEY (submission_id, pial_id, purpose, decision)
        REFERENCES kyc_submissions(id, pial_id, purpose, decision)
);
CREATE INDEX IF NOT EXISTS idx_id_copy_pial ON kyc_id_copies(pial_id);

-- ── 18 U.S.C. 2257 records ───────────────────────────────────────────────────
-- One row per (performer, piece of content): links the content to the passing
-- creator submission whose ID copy is the record for it. removed_at is the
-- last time feed-engine reported the content removed; the retention clock for
-- the ID copy runs from the latest removal across all of its records.
CREATE TABLE IF NOT EXISTS records_2257 (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    pial_id        UUID        NOT NULL,
    content_id     TEXT        NOT NULL CHECK (content_id <> ''),
    record_type    TEXT        NOT NULL CHECK (record_type ~ '^[a-z][a-z0-9_]{0,63}$'),
    submission_id  UUID        NOT NULL,
    purpose        TEXT        NOT NULL CHECK (purpose  = 'creator'),
    decision       TEXT        NOT NULL CHECK (decision = 'pass'),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    removed_at     TIMESTAMPTZ,
    CONSTRAINT records_2257_one_per_content UNIQUE (pial_id, content_id),
    FOREIGN KEY (submission_id, pial_id, purpose, decision)
        REFERENCES kyc_submissions(id, pial_id, purpose, decision)
);
CREATE INDEX IF NOT EXISTS idx_2257_pial       ON records_2257(pial_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_2257_content    ON records_2257(content_id);
CREATE INDEX IF NOT EXISTS idx_2257_submission ON records_2257(submission_id);

-- ── Audit log ────────────────────────────────────────────────────────────────
-- Append-only. Every step is a row, written in the same transaction as the
-- step: received, decided, document_in_use, id_copy_stored, 2257_record,
-- 2257_content_removed, purge. Metadata never carries PII or images.
CREATE TABLE IF NOT EXISTS verity_audit (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    pial_id        UUID        NOT NULL,
    action         TEXT        NOT NULL,
    attempt_id     UUID,
    submission_id  UUID,
    metadata       JSONB       NOT NULL DEFAULT '{}',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_audit_pial    ON verity_audit(pial_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_attempt ON verity_audit(attempt_id) WHERE attempt_id IS NOT NULL;
"#;

const NEXUS_SCHEMA: &str = r#"
CREATE TABLE IF NOT EXISTS nexus_identities (
    nexus_id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    biometric_hash    VARCHAR(128) NOT NULL,
    document_hash     VARCHAR(128) NOT NULL,
    jurisdiction      VARCHAR(10),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_verified_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    is_suspended      BOOL        NOT NULL DEFAULT false,
    suspension_reason TEXT,
    linking_refused   BOOL        NOT NULL DEFAULT false,
    CONSTRAINT uq_biometric UNIQUE (biometric_hash),
    CONSTRAINT uq_document  UNIQUE (document_hash)
);

-- A NEXUS groups a person's PIALs; it does not grade them. The verification
-- level is identity's (elohim-veni) alone, so the tier column that used to
-- live here is gone, along with its CHECK.
ALTER TABLE nexus_identities DROP COLUMN IF EXISTS verity_tier;

CREATE TABLE IF NOT EXISTS nexus_persona_links (
    nexus_id          UUID        NOT NULL REFERENCES nexus_identities(nexus_id),
    pial_shard_id     TEXT        NOT NULL,
    persona_type      VARCHAR(20) NOT NULL CHECK (persona_type IN ('personal','creator','business')),
    display_label     VARCHAR(50),
    linked_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    linked_by         VARCHAR(20) NOT NULL CHECK (linked_by IN ('user_initiated','biometric_match','admin_review')),
    is_primary        BOOL        NOT NULL DEFAULT false,
    PRIMARY KEY (nexus_id, pial_shard_id),
    CONSTRAINT uq_pial_one_nexus UNIQUE (pial_shard_id)
);

CREATE TABLE IF NOT EXISTS nexus_session_context (
    session_id            VARCHAR(128) PRIMARY KEY,
    nexus_id              UUID        NOT NULL REFERENCES nexus_identities(nexus_id),
    active_pial_shard_id  TEXT        NOT NULL,
    unified_view_mode     BOOL        NOT NULL DEFAULT false,
    unified_mode_type     VARCHAR(20) CHECK (unified_mode_type IN ('notifications','messages','earnings')),
    last_switched_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at            TIMESTAMPTZ NOT NULL DEFAULT NOW() + INTERVAL '7 days'
);

CREATE TABLE IF NOT EXISTS nexus_link_requests (
    request_id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    nexus_id                UUID        NOT NULL REFERENCES nexus_identities(nexus_id),
    target_pial_shard_id    TEXT        NOT NULL,
    status                  VARCHAR(20) NOT NULL DEFAULT 'pending'
                            CHECK (status IN ('pending','confirmed','declined','expired')),
    initiated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at              TIMESTAMPTZ NOT NULL DEFAULT NOW() + INTERVAL '30 minutes',
    confirmed_at            TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS nexus_audit_log (
    log_id        UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    nexus_id      UUID        NOT NULL,
    action        VARCHAR(50) NOT NULL,
    pial_shard_id TEXT,
    performed_by  VARCHAR(20) NOT NULL CHECK (performed_by IN ('user','system','admin','law_enforcement')),
    timestamp     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    metadata_hash VARCHAR(128)
);

CREATE TABLE IF NOT EXISTS nexus_aml_aggregates (
    nexus_id            UUID        NOT NULL REFERENCES nexus_identities(nexus_id),
    period_start        TIMESTAMPTZ NOT NULL,
    period_end          TIMESTAMPTZ NOT NULL,
    total_withdrawn_uaet BIGINT     NOT NULL DEFAULT 0,
    flag_for_edd        BOOL        NOT NULL DEFAULT false,
    flagged_at          TIMESTAMPTZ,
    PRIMARY KEY (nexus_id, period_start)
);

CREATE INDEX IF NOT EXISTS idx_nexus_biometric     ON nexus_identities(biometric_hash);
CREATE INDEX IF NOT EXISTS idx_nexus_persona_links ON nexus_persona_links(pial_shard_id);
CREATE INDEX IF NOT EXISTS idx_nexus_session_nid   ON nexus_session_context(nexus_id);
CREATE INDEX IF NOT EXISTS idx_nexus_session_exp   ON nexus_session_context(expires_at);
CREATE INDEX IF NOT EXISTS idx_nexus_link_status   ON nexus_link_requests(nexus_id, status, expires_at);
CREATE INDEX IF NOT EXISTS idx_nexus_aml_period    ON nexus_aml_aggregates(nexus_id, period_start DESC);

-- ── NEXUS v2 migrations: user-initiated linking without biometrics ─────────────
-- Allow NULL biometric/document hashes for user-initiated NEXUS (no KYC required
-- to link your own accounts).
ALTER TABLE nexus_identities ALTER COLUMN biometric_hash DROP NOT NULL;
ALTER TABLE nexus_identities ALTER COLUMN document_hash  DROP NOT NULL;

-- Track how this NEXUS was created.
ALTER TABLE nexus_identities ADD COLUMN IF NOT EXISTS source VARCHAR(20)
    NOT NULL DEFAULT 'biometric'
    CHECK (source IN ('biometric','user_initiated'));

-- Replace global unique constraints with partial indexes (enforce only when set).
ALTER TABLE nexus_identities DROP CONSTRAINT IF EXISTS uq_biometric;
ALTER TABLE nexus_identities DROP CONSTRAINT IF EXISTS uq_document;
CREATE UNIQUE INDEX IF NOT EXISTS uq_nexus_biometric_nn
    ON nexus_identities(biometric_hash) WHERE biometric_hash IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_nexus_document_nn
    ON nexus_identities(document_hash)  WHERE document_hash  IS NOT NULL;

-- Track which shard sent the link request (so the target knows who is asking).
ALTER TABLE nexus_link_requests ADD COLUMN IF NOT EXISTS source_pial_shard_id TEXT;

-- Fast lookup: all pending requests targeting a given shard.
CREATE INDEX IF NOT EXISTS idx_nexus_link_target
    ON nexus_link_requests(target_pial_shard_id, status, expires_at);
"#;

/// Verity's durable handoff to the Manhattan naming plane.
///
/// Manhattan is a separate brain reached over HTTP. A graph write sent
/// fire-and-forget is a graph write that a restart, a timeout or a rolling
/// deploy silently loses — and a naming plane that is silently missing rows is
/// worse than no naming plane, because everything downstream trusts it.
///
/// So the handoff is transactional. These rows are written by triggers on the
/// tables they describe, inside the SAME transaction as the row that caused
/// them. Either an attestation exists AND the registration of the identity it
/// references is queued, or neither happened. A background drain
/// (manhattan_outbox.rs) then delivers them in order, retrying with backoff,
/// and marks them delivered.
///
/// The triggers are the reason this cannot rot. A future writer that inserts an
/// attestation through some path nobody has thought of yet still registers the
/// identity it names, because registering is a property of the table and not a
/// step a caller must remember.
///
/// Payloads carry NAMES, never node ids. Manhattan assigns node ids; this side
/// does not know them and must not learn them, or the two planes acquire a
/// second shared identifier and we are back where we started.
const SCHEMA_MANHATTAN: &str = r#"
-- ── The second vocabulary, and what it turned out to be ──────────────────────
--
-- Verity names one person two ways inside one database:
--
--   pial_id        UUID  — kyc_submissions, records_2257, verity_audit
--   pial_shard_id  TEXT  — every NEXUS table
--
-- They are not two concepts. A shard is
--
--     hex(sha256(<pial uuid> || ':nexus-pial-shard-v1'))
--
-- a pure, one-way function of exactly one PIAL UUID: no persona index, no
-- counter, no salt. The identical formula is implemented in feed-engine
-- (nexus.PersonaShardFromPIAL) and in elohim-veni (handlers::pial_shard_id).
-- A shard IS the identity, blinded — it is not a persona belonging to one.
--
-- The persona in NEXUS is the whole PIAL. nexus_identities sits ABOVE PIALs:
-- one verified natural person holding several identities, with persona_type
-- ('personal','creator','business') describing the ROLE an identity plays for
-- that person — a property of the link, not a thing of its own.
--
-- What made this unfixable before was the direction of the hash. Given a
-- shard, nothing can recover the PIAL. But given a PIAL, the shard is one
-- line — and Verity holds a PIAL for every person it has ever recorded
-- evidence about. So the map
-- is built forwards, from the side that can build it, and kept.
CREATE TABLE IF NOT EXISTS pial_shard_index (
    pial_shard_id TEXT        PRIMARY KEY,
    pial_id       UUID        NOT NULL UNIQUE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- sha256() is core in PostgreSQL 11+; this needs no extension.
CREATE OR REPLACE FUNCTION manhattan_pial_shard(p_pial TEXT)
RETURNS TEXT AS $$
    SELECT encode(sha256(convert_to(p_pial || ':nexus-pial-shard-v1', 'UTF8')), 'hex');
$$ LANGUAGE sql IMMUTABLE STRICT;

-- ── The outbox ───────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS manhattan_outbox (
    id              BIGSERIAL   PRIMARY KEY,
    op              TEXT        NOT NULL,
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

-- The op vocabulary as a named constraint rather than an inline one, because
-- CREATE TABLE IF NOT EXISTS will not revisit an inline CHECK on a database
-- that already has the table. Dropping and re-adding it converges both a fresh
-- database and one deployed before 'delete_edge' existed.
ALTER TABLE manhattan_outbox DROP CONSTRAINT IF EXISTS manhattan_outbox_op_check;
ALTER TABLE manhattan_outbox ADD  CONSTRAINT manhattan_outbox_op_check
    CHECK (op IN ('node', 'name', 'edge', 'delete_edge', 'revoke_name'));

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

-- ── what a dedup key is allowed to mean ──────────────────────────────────────
-- ON CONFLICT DO NOTHING is only safe when the key identifies the WRITE, not
-- merely the thing written about. Keyed on the target alone, a node
-- registration can never be corrected: the key already exists, the new payload
-- is discarded on insert, and the node keeps whatever kind it was first given.
-- For a nexus that is not cosmetic — kind decides the owning brain, and
-- Manhattan derives edge-write permission from the SUBJECT node's owner, so a
-- nexus registered under the wrong kind makes every persona edge beneath it
-- unwritable forever.
--
-- Fingerprinting the payload gives the key the right meaning: a repeat of the
-- same registration still collides and is correctly dropped, while a genuinely
-- different registration is a different key and a real row. jsonb renders
-- canonically (keys sorted, whitespace normalised), so the fingerprint is
-- stable across writers.
CREATE OR REPLACE FUNCTION manhattan_node_dedup(p_target TEXT, p_payload JSONB)
RETURNS TEXT AS $$
    SELECT p_target || '#'
        || left(encode(sha256(convert_to(p_payload::text, 'UTF8')), 'hex'), 16);
$$ LANGUAGE sql IMMUTABLE STRICT;

-- A persona link and its retraction are EVENTS, not facts, and every one of
-- them must be delivered. Any key derived from a clock collides: now() is
-- transaction-start truncated to whole seconds, so unlink → relink → unlink
-- inside one second — or two writes in one transaction — produce one key and
-- the second write is silently dropped. A sequence is non-transactional and
-- strictly increasing, so every trigger fire gets a key that belongs to it
-- alone.
CREATE SEQUENCE IF NOT EXISTS manhattan_link_event_seq;

-- ── identity references ──────────────────────────────────────────────────────
-- Verity does not own identity; elohim-veni does (Manhattan migration 0002).
-- Every PIAL in these tables is a REFERENCE, so it registers as a NAME and is
-- resolved through Manhattan instead of being assumed to exist somewhere. Any
-- brain may register a node it encounters first — ownership of the node is
-- decided by Manhattan from its kind, not claimed here.
--
-- The same trigger records the shard for that PIAL. One row per person in
-- pial_shard_index is what lets every NEXUS relationship below be expressed
-- with the canonical `pial:` name instead of a blinded alias nothing resolves.
--
-- The column carrying the PIAL is passed as a trigger argument and read off
-- the row — one function, not one per table that must be kept in step.
CREATE OR REPLACE FUNCTION manhattan_register_identity_ref() RETURNS TRIGGER AS $$
DECLARE
    v_pial    TEXT;
    v_payload JSONB;
BEGIN
    v_pial := to_jsonb(NEW) ->> TG_ARGV[0];
    IF v_pial IS NULL OR v_pial = '' THEN
        RETURN NEW;
    END IF;

    INSERT INTO pial_shard_index (pial_shard_id, pial_id)
    VALUES (manhattan_pial_shard(v_pial), v_pial::uuid)
    ON CONFLICT DO NOTHING;

    v_payload := jsonb_build_object(
        'kind',      'identity',
        'name',      'pial:' || v_pial,
        'namespace', 'pial');

    PERFORM manhattan_enqueue(
        'node',
        manhattan_node_dedup('node:pial:' || v_pial, v_payload),
        v_payload);

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_manhattan_kyc_submissions ON kyc_submissions;
CREATE TRIGGER trg_manhattan_kyc_submissions
    AFTER INSERT ON kyc_submissions
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_identity_ref('pial_id');

DROP TRIGGER IF EXISTS trg_manhattan_records_2257 ON records_2257;
CREATE TRIGGER trg_manhattan_records_2257
    AFTER INSERT ON records_2257
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_identity_ref('pial_id');

DROP TRIGGER IF EXISTS trg_manhattan_verity_audit ON verity_audit;
CREATE TRIGGER trg_manhattan_verity_audit
    AFTER INSERT ON verity_audit
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_identity_ref('pial_id');

-- ── NEXUS: the person above the identities ───────────────────────────────────
-- A nexus_identity is not a PIAL and must never be conflated with one. It is
-- the verified natural person who holds several PIALs, so it is its own node,
-- with its own name and its own KIND.
--
-- kind 'nexus' is owned by verity (Manhattan migration 0003), and that is what
-- makes the persona edge writable at all: Manhattan derives who may assert an
-- edge from who owns its SUBJECT. Registering the person as kind 'identity'
-- would hand the node to elohim-veni and leave verity unable to record the one
-- relationship it exists to record.
CREATE OR REPLACE FUNCTION manhattan_register_nexus() RETURNS TRIGGER AS $$
DECLARE
    v_payload JSONB;
BEGIN
    v_payload := jsonb_build_object(
        'kind',      'nexus',
        'name',      'nexus:' || NEW.nexus_id::text,
        'namespace', 'nexus');

    PERFORM manhattan_enqueue(
        'node',
        manhattan_node_dedup('node:nexus:' || NEW.nexus_id::text, v_payload),
        v_payload);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_manhattan_nexus_identities ON nexus_identities;
CREATE TRIGGER trg_manhattan_nexus_identities
    AFTER INSERT ON nexus_identities
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_nexus();

-- nexus_persona_links is a relationship table, which is exactly what the edge
-- plane is for. It is recorded as `nexus contains identity`: the natural
-- person is the subject, because the nexus is the row this brain owns and
-- Manhattan derives who may write an edge from who owns its subject.
--
-- Manhattan materialises root and depth on write and refuses to close a cycle,
-- so a persona graph in which a person is transitively their own persona stops
-- being something application code has to defend against and becomes something
-- the database will not store. `edges_out(nexus, 'contains')` answers
-- list_personas in one indexed lookup; `edges_in(pial, 'contains')` answers
-- "whose identity is this" in the other direction.
--
-- The object is named `pial:<uuid>`, and only ever that.
--
-- It used to fall back to `shard:<64 hex>` when this database did not know the
-- PIAL behind the shard. That name resolves to nothing, and nothing in the
-- platform can ever make it resolve: `shard` governs no namespace authority, so
-- Manhattan falls back to node ownership; identity nodes belong to elohim-veni,
-- which has no shard code at all; lore refuses to bind one for that reason and
-- herald rejects shards outright. The drain resolves an edge's object before it
-- writes the edge and stops the batch on the first failure — so ONE persona link
-- for an unmapped shard did not merely fail to deliver, it ended verity's entire
-- naming-plane output, permanently, for every row queued behind it.
--
-- Binding `shard:` somewhere would not be the fix. A shard is the PIAL blinded —
-- sha256(pial || ':nexus-pial-shard-v1') — and publishing it as a name beside
-- `pial:` on the same node hands anyone who can read that node's names the
-- mapping the blinding exists to withhold. The graph would gain an edge and the
-- person would lose the separation between their personas, which is the trade
-- this whole subsystem exists to refuse.
--
-- So the edge is DEFERRED, not dropped and not faked. The link itself is already
-- durable in nexus_persona_links, which is the source of truth; what is missing
-- is only the naming-plane expression of it, and the one thing that can supply
-- it is the shard→PIAL mapping. That mapping can only ever be built forwards,
-- from a PIAL, and pial_shard_index is where it lands the moment verity meets
-- one. A trigger on that table completes every deferred link at that instant —
-- see manhattan_complete_deferred_links below. Until then the count of deferred
-- links is reported on /health, so a link waiting for its identity is a visible
-- number rather than a silence.

-- Assert one persona link in the graph: the identity node it points at, the
-- nexus node it hangs off, then the edge. That order, and every step idempotent,
-- so this is safe to call from the link trigger and from the deferred-completion
-- trigger alike.
CREATE OR REPLACE FUNCTION manhattan_enqueue_persona_edge(p_nexus UUID, p_pial UUID)
RETURNS VOID AS $$
DECLARE
    v_payload JSONB;
BEGIN
    v_payload := jsonb_build_object(
        'kind',      'identity',
        'name',      'pial:' || p_pial::text,
        'namespace', 'pial');
    PERFORM manhattan_enqueue(
        'node',
        manhattan_node_dedup('node:pial:' || p_pial::text, v_payload),
        v_payload);

    -- The nexus node too. When this is called from the shard-index trigger the
    -- nexus registration may sit further down the queue, and the drain resolves
    -- an edge's subject before writing it — an edge ahead of its own subject is
    -- a head-of-line block by construction. Enqueuing it here costs one dedup
    -- probe and removes the ordering hazard entirely.
    v_payload := jsonb_build_object(
        'kind',      'nexus',
        'name',      'nexus:' || p_nexus::text,
        'namespace', 'nexus');
    PERFORM manhattan_enqueue(
        'node',
        manhattan_node_dedup('node:nexus:' || p_nexus::text, v_payload),
        v_payload);

    -- One sequence value per call. A persona link can be revoked and made again,
    -- so this relationship is an EVENT and not an eternal fact, and a key naming
    -- only the pair — or the pair plus a whole-second linked_at — lets a relink
    -- inside the same second collide with the link before it. The assertion is
    -- then swallowed and the graph is permanently missing an edge the database
    -- says exists.
    PERFORM manhattan_enqueue(
        'edge',
        'edge:nexus:' || p_nexus::text || '|contains|pial:' || p_pial::text
                      || ':' || nextval('manhattan_link_event_seq')::text,
        jsonb_build_object(
            'subject_name', 'nexus:' || p_nexus::text,
            'predicate',    'contains',
            'object_name',  'pial:' || p_pial::text));
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION manhattan_register_persona_link() RETURNS TRIGGER AS $$
DECLARE
    v_pial    UUID;
    v_payload JSONB;
BEGIN
    v_payload := jsonb_build_object(
        'kind',      'nexus',
        'name',      'nexus:' || NEW.nexus_id::text,
        'namespace', 'nexus');

    PERFORM manhattan_enqueue(
        'node',
        manhattan_node_dedup('node:nexus:' || NEW.nexus_id::text, v_payload),
        v_payload);

    SELECT pial_id INTO v_pial
      FROM pial_shard_index WHERE pial_shard_id = NEW.pial_shard_id;

    -- No PIAL, no edge. Enqueuing one now would enqueue a name that cannot be
    -- resolved by anything, ever, at the head of an ordered queue.
    IF v_pial IS NULL THEN
        RETURN NEW;
    END IF;

    PERFORM manhattan_enqueue_persona_edge(NEW.nexus_id, v_pial);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- The other half of the deferral. A shard→PIAL mapping is written the instant
-- verity decides anything about a person (manhattan_register_identity_ref), and
-- at that instant every persona link still holding that shard becomes
-- expressible. This asserts them, in the same transaction as the mapping that
-- made them possible, so the deferral resolves itself rather than waiting for
-- someone to notice.
--
-- It reads nexus_persona_links as it stands right now, which is what makes the
-- retraction side correct too: a link unlinked while its shard was still unknown
-- asserted no edge and is simply not here to be resurrected.
CREATE OR REPLACE FUNCTION manhattan_complete_deferred_links() RETURNS TRIGGER AS $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN
        SELECT nexus_id FROM nexus_persona_links WHERE pial_shard_id = NEW.pial_shard_id
    LOOP
        PERFORM manhattan_enqueue_persona_edge(r.nexus_id, NEW.pial_id);
    END LOOP;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_manhattan_pial_shard_index ON pial_shard_index;
CREATE TRIGGER trg_manhattan_pial_shard_index
    AFTER INSERT ON pial_shard_index
    FOR EACH ROW EXECUTE FUNCTION manhattan_complete_deferred_links();

DROP TRIGGER IF EXISTS trg_manhattan_nexus_persona_links ON nexus_persona_links;
CREATE TRIGGER trg_manhattan_nexus_persona_links
    AFTER INSERT ON nexus_persona_links
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_persona_link();

-- Unlinking a persona must retract the edge. An edge left behind after the row
-- that justified it is gone is not a stale cache — it is the graph asserting
-- that two accounts belong to the same human after the user asked us to stop
-- saying so, which on a multi-persona platform is the failure that matters
-- most. So the retraction goes through the same transactional path as the
-- assertion, and is delivered by the same ordered drain.
CREATE OR REPLACE FUNCTION manhattan_retract_persona_link() RETURNS TRIGGER AS $$
DECLARE
    v_pial   UUID;
    v_object TEXT;
BEGIN
    SELECT pial_id INTO v_pial
      FROM pial_shard_index WHERE pial_shard_id = OLD.pial_shard_id;

    -- Symmetric with the assertion. No PIAL means no edge was ever asserted for
    -- this link — not by the insert trigger, and not by the shard-index trigger,
    -- which only ever sees links that still exist — so there is nothing to
    -- retract and a delete naming an unresolvable object would wedge the queue
    -- for a retraction of nothing.
    IF v_pial IS NULL THEN
        RETURN OLD;
    END IF;

    v_object := 'pial:' || v_pial::text;

    -- One sequence value per trigger fire, so every retraction is its own row.
    -- now() is transaction-start truncated to whole seconds: two unlinks in one
    -- transaction, or an unlink → relink → unlink inside one second, collide on
    -- the key and the second retraction is dropped by ON CONFLICT DO NOTHING —
    -- leaving the graph asserting that two personas belong to the same human
    -- after the user retracted it, which is the exact failure this trigger
    -- exists to prevent.
    PERFORM manhattan_enqueue(
        'delete_edge',
        'unedge:nexus:' || OLD.nexus_id::text || '|contains|' || v_object
                        || ':' || nextval('manhattan_link_event_seq')::text,
        jsonb_build_object(
            'subject_name', 'nexus:' || OLD.nexus_id::text,
            'predicate',    'contains',
            'object_name',  v_object));

    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_manhattan_nexus_persona_unlink ON nexus_persona_links;
CREATE TRIGGER trg_manhattan_nexus_persona_unlink
    AFTER DELETE ON nexus_persona_links
    FOR EACH ROW EXECUTE FUNCTION manhattan_retract_persona_link();

-- nexus_link_requests deliberately registers no edge. A pending request is a
-- proposal; an edge in Manhattan is a fact. A confirmed request inserts into
-- nexus_persona_links, and the trigger above records the relationship at the
-- moment it becomes true — which is also why source_pial_shard_id needs no
-- edge of its own: two personas under one nexus already share a root.

-- ── backfill ─────────────────────────────────────────────────────────────────
-- Everything that already exists is enqueued once, so the plane starts complete
-- instead of only knowing about rows written after this deploy. Guarded on an
-- empty outbox: rows here are never deleted, only marked delivered, so a
-- non-empty outbox means the backfill has already run. The statements run
-- shard index → identities → nexuses, and the shard-index insert fires
-- trg_manhattan_pial_shard_index, which enqueues each persona edge behind the
-- nodes it needs — so the BIGSERIAL order the drain follows is the order the
-- graph needs.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM manhattan_outbox) THEN
        RETURN;
    END IF;

    INSERT INTO pial_shard_index (pial_shard_id, pial_id)
    SELECT manhattan_pial_shard(pial::text), pial
      FROM (SELECT pial_id AS pial FROM kyc_submissions
            UNION SELECT pial_id FROM records_2257
            UNION SELECT pial_id FROM verity_audit) p
     WHERE pial IS NOT NULL
    ON CONFLICT DO NOTHING;

    INSERT INTO manhattan_outbox (op, dedup_key, payload)
    SELECT 'node', manhattan_node_dedup('node:pial:' || pial_id::text, payload), payload
      FROM (SELECT pial_id,
                   jsonb_build_object('kind','identity',
                                      'name','pial:' || pial_id::text,
                                      'namespace','pial') AS payload
              FROM pial_shard_index) p
    ON CONFLICT (dedup_key) DO NOTHING;

    INSERT INTO manhattan_outbox (op, dedup_key, payload)
    SELECT 'node', manhattan_node_dedup('node:nexus:' || nexus_id::text, payload), payload
      FROM (SELECT nexus_id,
                   jsonb_build_object('kind','nexus',
                                      'name','nexus:' || nexus_id::text,
                                      'namespace','nexus') AS payload
              FROM nexus_identities) n
    ON CONFLICT (dedup_key) DO NOTHING;

    -- No persona-edge statement here any more, and its absence is deliberate.
    -- The pial_shard_index insert above fires trg_manhattan_pial_shard_index for
    -- every mapping it creates, and that trigger enqueues the identity node, the
    -- nexus node and the `contains` edge for every persona link holding that
    -- shard — the whole of what this statement used to do, in an order the drain
    -- can actually deliver. Repeating it here would enqueue each edge a second
    -- time: the edge dedup key carries a sequence value precisely so that two
    -- real events never collide, which also means two enqueues of one event
    -- never dedup.
    --
    -- This is complete rather than merely equivalent. The backfill runs only into
    -- an empty outbox, and pial_shard_index is only ever written by this
    -- statement or by manhattan_register_identity_ref — which enqueues as it
    -- writes. An empty outbox therefore implies an empty index, so every mapping
    -- that exists at this point was created here, and every one of them fired
    -- the trigger.
    --
    -- What it deliberately does NOT do is enqueue an edge for a link whose shard
    -- has no PIAL. That row used to be written as `shard:<hex>`, a name nothing
    -- in the platform can resolve, at the head of a strictly ordered queue on the
    -- very first boot — which stopped every row behind it from ever being
    -- delivered. Such a link is completed by the same trigger the moment verity
    -- learns the PIAL, and counted by deferred_persona_links() until then.
END $$;
"#;

pub async fn migrate(pool: &PgPool) -> Result<()> {
    // First: the old tier-owning shapes leave before the evidence tables are
    // created under the same names.
    sqlx::raw_sql(RESHAPE).execute(pool).await?;
    sqlx::raw_sql(SCHEMA).execute(pool).await?;
    sqlx::raw_sql(NEXUS_SCHEMA).execute(pool).await?;
    // Last: the outbox triggers hang off every table above, so they are created
    // once those tables exist.
    sqlx::raw_sql(SCHEMA_MANHATTAN).execute(pool).await?;
    Ok(())
}

// ── Audit ─────────────────────────────────────────────────────────────────────

/// The audit vocabulary. One constant per step, so a step cannot be spelled two
/// ways in two places.
pub mod action {
    pub const RECEIVED: &str = "received";
    pub const DECIDED: &str = "decided";
    pub const DOCUMENT_IN_USE: &str = "document_in_use";
    pub const ID_COPY_STORED: &str = "id_copy_stored";
    pub const RECORD_2257: &str = "2257_record";
    pub const CONTENT_REMOVED_2257: &str = "2257_content_removed";
    pub const PURGE: &str = "purge";
}

/// Appends one audit row on the caller's executor — always the transaction
/// that performs the step, so a step and its audit row commit or fail together.
/// A failed audit write fails the step; it is never discarded.
pub async fn audit<'e, E>(
    exec: E,
    pial_id: uuid::Uuid,
    action: &str,
    attempt_id: Option<uuid::Uuid>,
    submission_id: Option<uuid::Uuid>,
    metadata: serde_json::Value,
) -> Result<(), sqlx::Error>
where
    E: sqlx::PgExecutor<'e>,
{
    sqlx::query(
        "INSERT INTO verity_audit (pial_id, action, attempt_id, submission_id, metadata)
         VALUES ($1, $2, $3, $4, $5)",
    )
    .bind(pial_id)
    .bind(action)
    .bind(attempt_id)
    .bind(submission_id)
    .bind(metadata)
    .execute(exec)
    .await?;
    Ok(())
}

// ── Manhattan outbox ──────────────────────────────────────────────────────────

/// What the outbox has not yet delivered. Depth alone says a queue is behind;
/// the age of the head and the error it last failed with say whether it is
/// moving. All three are reported by /health so a stalled plane is visible on
/// the tick it stalls rather than the day someone notices a missing name.
pub struct OutboxBacklog {
    pub pending: i64,
    pub oldest_age_seconds: Option<f64>,
    pub head_last_error: Option<String>,
    /// Rows the drain has taken out of the queue because they cannot be
    /// delivered. Counted separately and still counted in `pending`: a
    /// quarantined row is a naming-plane write that did NOT happen, so it must
    /// not vanish from the depth this endpoint reports.
    pub quarantined: i64,
}

pub async fn outbox_backlog(pool: &PgPool) -> Result<OutboxBacklog> {
    let row: (i64, Option<f64>, Option<String>, i64) = sqlx::query_as(
        "SELECT COUNT(*)::BIGINT,
                EXTRACT(EPOCH FROM (NOW() - MIN(created_at)))::DOUBLE PRECISION,
                (SELECT last_error FROM manhattan_outbox
                  WHERE delivered_at IS NULL AND blocked_at IS NULL ORDER BY id LIMIT 1),
                COUNT(*) FILTER (WHERE blocked_at IS NOT NULL)::BIGINT
           FROM manhattan_outbox
          WHERE delivered_at IS NULL",
    )
    .fetch_one(pool)
    .await?;
    Ok(OutboxBacklog {
        pending: row.0,
        oldest_age_seconds: row.1,
        head_last_error: row.2,
        quarantined: row.3,
    })
}

/// Persona links whose shard has no known PIAL, and which therefore have no edge
/// in the naming plane yet.
///
/// The link is real and durable in `nexus_persona_links`; what is missing is the
/// name that can express it, and that arrives the moment verity decides anything
/// about the person behind the shard. Until then this number is the deferral, and
/// it is reported on /health so a link waiting for its identity is visible rather
/// than silent. Enqueuing `shard:<hex>` instead — the old behaviour — would have
/// made it visible too, by stopping the queue forever.
pub async fn deferred_persona_links(pool: &PgPool) -> Result<i64> {
    let row: (i64,) = sqlx::query_as(
        "SELECT COUNT(*)::BIGINT
           FROM nexus_persona_links l
          WHERE NOT EXISTS (SELECT 1 FROM pial_shard_index x
                             WHERE x.pial_shard_id = l.pial_shard_id)",
    )
    .fetch_one(pool)
    .await?;
    Ok(row.0)
}
