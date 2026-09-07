use anyhow::Result;
use sqlx::PgPool;

const SCHEMA: &str = r#"
-- ── Attestations ─────────────────────────────────────────────────────────────
-- One row per verification event per PIAL.
-- Immutable once written. New decisions append as new rows.
CREATE TABLE IF NOT EXISTS attestations (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    pial_id         UUID        NOT NULL,
    context         TEXT        NOT NULL,           -- onboarding | age_gate | creator_signup | payout
    status          TEXT        NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('approved','denied','pending_review','pending')),
    age_band        TEXT        NOT NULL DEFAULT 'unknown'
                    CHECK (age_band IN ('18+','21+','underage','unknown')),
    creator_tier    INTEGER     NOT NULL DEFAULT 0 CHECK (creator_tier BETWEEN 0 AND 3),
    payout_enabled  BOOLEAN     NOT NULL DEFAULT FALSE,
    nsfw_access     BOOLEAN     NOT NULL DEFAULT FALSE,
    risk_score      REAL        NOT NULL DEFAULT 0.0 CHECK (risk_score BETWEEN 0.0 AND 1.0),
    required_actions TEXT[]     NOT NULL DEFAULT '{}',
    -- Cryptographic anchor: SHA-256(pial_id + status + age_band + tier + timestamp)
    decision_hash   TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at      TIMESTAMPTZ,
    metadata        JSONB       NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_att_pial    ON attestations(pial_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_att_status  ON attestations(status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_att_context ON attestations(context, pial_id);

-- ── KYC submissions ───────────────────────────────────────────────────────────
-- Tracks verification attempts. Raw documents are NEVER stored here.
-- Only hashes, status, and non-sensitive extracted values.
CREATE TABLE IF NOT EXISTS kyc_submissions (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    pial_id         UUID        NOT NULL,
    submission_type TEXT        NOT NULL   -- gov_id | liveness | phone | email
                    CHECK (submission_type IN ('gov_id','liveness','phone','email','manual')),
    status          TEXT        NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending','processing','passed','failed','manual_review')),
    -- Hash of the submitted document (SHA-256) — document itself is never stored
    document_hash   TEXT,
    age_band        TEXT        DEFAULT 'unknown',
    confidence      REAL        DEFAULT 0.0,
    failure_reason  TEXT,
    provider        TEXT        NOT NULL DEFAULT 'internal', -- 'internal' | 'jumio' | 'persona' | etc
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    processed_at    TIMESTAMPTZ,
    metadata        JSONB       NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_kyc_pial   ON kyc_submissions(pial_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_kyc_status ON kyc_submissions(status);

-- ── Risk profiles ─────────────────────────────────────────────────────────────
-- Per-PIAL aggregated fraud signals. Mutable — updated on each decision.
CREATE TABLE IF NOT EXISTS risk_profiles (
    pial_id             UUID    PRIMARY KEY,
    risk_score          REAL    NOT NULL DEFAULT 0.0,
    device_count        INTEGER NOT NULL DEFAULT 0,
    failed_kyc_attempts INTEGER NOT NULL DEFAULT 0,
    ip_reputation_score REAL    NOT NULL DEFAULT 1.0,  -- 1.0 = clean, 0.0 = datacenter/VPN
    velocity_score      REAL    NOT NULL DEFAULT 0.0,  -- 0.0 = normal, 1.0 = burst anomaly
    duplicate_detected  BOOLEAN NOT NULL DEFAULT FALSE,
    last_evaluated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ── Audit log ─────────────────────────────────────────────────────────────────
-- Append-only. Every decision recorded.
CREATE TABLE IF NOT EXISTS verity_audit (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    pial_id     UUID        NOT NULL,
    action      TEXT        NOT NULL,  -- decision | kyc_submit | risk_update | tier_change
    context     TEXT,
    before_tier INTEGER,
    after_tier  INTEGER,
    metadata    JSONB       NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_audit_pial ON verity_audit(pial_id, created_at DESC);
"#;

const COMPLIANCE_SCHEMA: &str = r#"
-- ── 18 U.S.C. 2257 Records ──────────────────────────────────────────────────
-- One record per verified adult creator. Immutable once created.
-- We store a decision hash and document hash only — no ID images or biometrics.
-- Record-keeper statement is derived from this row at query time.
CREATE TABLE IF NOT EXISTS records_2257 (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    creator_pial_id     UUID        NOT NULL UNIQUE,
    -- Linked to the KYC submission that established age verification
    kyc_submission_id   UUID        REFERENCES kyc_submissions(id),
    age_band            TEXT        NOT NULL CHECK (age_band IN ('18+','21+')),
    document_type       TEXT        NOT NULL DEFAULT 'gov_id',
    -- SHA-256 hash of the verified document — document itself is never stored
    document_hash       TEXT        NOT NULL,
    verification_date   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    record_keeper       TEXT        NOT NULL DEFAULT 'F33D3R Platform',
    -- Custodian URL per 18 USC 2257(f)(5)
    record_location     TEXT        NOT NULL DEFAULT 'https://f33d3r.com/legal/2257',
    -- SHA-256 of (creator_pial_id + age_band + verification_date) — audit anchor
    statement_hash      TEXT        NOT NULL,
    is_active           BOOLEAN     NOT NULL DEFAULT TRUE,
    revoked_at          TIMESTAMPTZ,
    revocation_reason   TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_2257_creator ON records_2257(creator_pial_id);
CREATE INDEX IF NOT EXISTS idx_2257_active  ON records_2257(is_active);

-- ── CSAM scan log ─────────────────────────────────────────────────────────────
-- Every media upload is checked. We store the result for audit and repeat-check avoidance.
-- In production this integrates with NCMEC / PhotoDNA. Local dev: stub returns "clean".
CREATE TABLE IF NOT EXISTS csam_scans (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Perceptual hash of the uploaded content (SHA-256 of raw bytes)
    content_hash    TEXT        NOT NULL,
    uploader_pial   UUID        NOT NULL,
    -- Provider: ncmec_stub | photodna | aws_rekognition
    scan_provider   TEXT        NOT NULL DEFAULT 'ncmec_stub',
    -- Result: clean | flagged | error | skipped
    result          TEXT        NOT NULL DEFAULT 'clean'
                    CHECK (result IN ('clean','flagged','error','skipped')),
    match_count     INTEGER     NOT NULL DEFAULT 0,
    -- If flagged: was a CyberTip report sent to NCMEC?
    report_sent     BOOLEAN     NOT NULL DEFAULT FALSE,
    report_sent_at  TIMESTAMPTZ,
    media_url       TEXT,
    media_type      TEXT,
    scanned_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_csam_hash     ON csam_scans(content_hash);
CREATE        INDEX IF NOT EXISTS idx_csam_flagged  ON csam_scans(result) WHERE result = 'flagged';
CREATE        INDEX IF NOT EXISTS idx_csam_uploader ON csam_scans(uploader_pial, scanned_at DESC);
"#;

const NEXUS_SCHEMA: &str = r#"
-- nexus_id references will be added after nexus_identities table exists
ALTER TABLE attestations ADD COLUMN IF NOT EXISTS nexus_id UUID;

CREATE TABLE IF NOT EXISTS nexus_identities (
    nexus_id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    biometric_hash    VARCHAR(128) NOT NULL,
    document_hash     VARCHAR(128) NOT NULL,
    verity_tier       SMALLINT    NOT NULL DEFAULT 1,
    jurisdiction      VARCHAR(10),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_verified_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    is_suspended      BOOL        NOT NULL DEFAULT false,
    suspension_reason TEXT,
    linking_refused   BOOL        NOT NULL DEFAULT false,
    CONSTRAINT uq_biometric UNIQUE (biometric_hash),
    CONSTRAINT uq_document  UNIQUE (document_hash),
    CONSTRAINT chk_tier CHECK (verity_tier BETWEEN 0 AND 3)
);

-- Add FK after table exists
DO $$ BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.table_constraints
        WHERE constraint_name = 'attestations_nexus_id_fkey'
    ) THEN
        ALTER TABLE attestations ADD CONSTRAINT attestations_nexus_id_fkey
            FOREIGN KEY (nexus_id) REFERENCES nexus_identities(nexus_id);
    END IF;
END $$;

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
CREATE INDEX IF NOT EXISTS idx_att_nexus_id        ON attestations(nexus_id) WHERE nexus_id IS NOT NULL;

-- ── NEXUS v2 migrations: user-initiated linking without biometrics ─────────────
-- Allow NULL biometric/document hashes for user-initiated NEXUS (no KYC required
-- to link your own accounts; KYC upgrades tier later).
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
--   pial_id        UUID  — attestations, kyc_submissions, risk_profiles,
--                          records_2257 (creator_pial_id), csam_scans
--                          (uploader_pial), verity_audit
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
-- shard, nothing can recover the PIAL; nexus.rs says so in as many words
-- ("Since Verity doesn't store the mapping…") and get_tier's fallback returns
-- tier 0 for that exact reason. But given a PIAL, the shard is one line — and
-- Verity holds a PIAL for every person it has ever decided about. So the map
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
-- The column carrying the PIAL differs per table (pial_id, creator_pial_id,
-- uploader_pial), so it is passed as a trigger argument and read off the row —
-- one function, not six that must be kept in step.
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

DROP TRIGGER IF EXISTS trg_manhattan_attestations ON attestations;
CREATE TRIGGER trg_manhattan_attestations
    AFTER INSERT ON attestations
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_identity_ref('pial_id');

DROP TRIGGER IF EXISTS trg_manhattan_kyc_submissions ON kyc_submissions;
CREATE TRIGGER trg_manhattan_kyc_submissions
    AFTER INSERT ON kyc_submissions
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_identity_ref('pial_id');

DROP TRIGGER IF EXISTS trg_manhattan_risk_profiles ON risk_profiles;
CREATE TRIGGER trg_manhattan_risk_profiles
    AFTER INSERT ON risk_profiles
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_identity_ref('pial_id');

DROP TRIGGER IF EXISTS trg_manhattan_records_2257 ON records_2257;
CREATE TRIGGER trg_manhattan_records_2257
    AFTER INSERT ON records_2257
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_identity_ref('creator_pial_id');

DROP TRIGGER IF EXISTS trg_manhattan_csam_scans ON csam_scans;
CREATE TRIGGER trg_manhattan_csam_scans
    AFTER INSERT ON csam_scans
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_identity_ref('uploader_pial');

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
      FROM (SELECT pial_id         AS pial FROM attestations
            UNION SELECT pial_id         FROM kyc_submissions
            UNION SELECT pial_id         FROM risk_profiles
            UNION SELECT creator_pial_id FROM records_2257
            UNION SELECT uploader_pial   FROM csam_scans
            UNION SELECT pial_id         FROM verity_audit) p
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
    sqlx::raw_sql(SCHEMA).execute(pool).await?;
    sqlx::raw_sql(COMPLIANCE_SCHEMA).execute(pool).await?;
    sqlx::raw_sql(NEXUS_SCHEMA).execute(pool).await?;
    // Last: the outbox triggers hang off every table above, so they are created
    // once those tables exist.
    sqlx::raw_sql(SCHEMA_MANHATTAN).execute(pool).await?;
    Ok(())
}

/// Returns the latest attestation for a PIAL + context combination.
pub async fn get_latest_attestation(
    pool: &PgPool,
    pial_id: uuid::Uuid,
    context: &str,
) -> Option<crate::models::AttestationRow> {
    sqlx::query_as(
        "SELECT * FROM attestations WHERE pial_id = $1 AND context = $2
         ORDER BY created_at DESC LIMIT 1",
    )
    .bind(pial_id)
    .bind(context)
    .fetch_optional(pool)
    .await
    .unwrap_or(None)
}

/// Returns the current risk profile, or a default if none exists.
pub async fn get_risk_profile(pool: &PgPool, pial_id: uuid::Uuid) -> crate::models::RiskProfile {
    sqlx::query_as("SELECT * FROM risk_profiles WHERE pial_id = $1")
        .bind(pial_id)
        .fetch_optional(pool)
        .await
        .unwrap_or(None)
        .unwrap_or_else(|| crate::models::RiskProfile {
            pial_id,
            risk_score: 0.0,
            device_count: 0,
            failed_kyc_attempts: 0,
            ip_reputation_score: 1.0,
            velocity_score: 0.0,
            duplicate_detected: false,
            last_evaluated_at: chrono::Utc::now(),
            updated_at: chrono::Utc::now(),
        })
}

/// Upserts a risk profile.
pub async fn upsert_risk_profile(pool: &PgPool, p: &crate::models::RiskProfile) {
    let _ = sqlx::query(
        "INSERT INTO risk_profiles
            (pial_id, risk_score, device_count, failed_kyc_attempts,
             ip_reputation_score, velocity_score, duplicate_detected, last_evaluated_at, updated_at)
         VALUES ($1,$2,$3,$4,$5,$6,$7,NOW(),NOW())
         ON CONFLICT (pial_id) DO UPDATE SET
            risk_score          = EXCLUDED.risk_score,
            device_count        = EXCLUDED.device_count,
            failed_kyc_attempts = EXCLUDED.failed_kyc_attempts,
            ip_reputation_score = EXCLUDED.ip_reputation_score,
            velocity_score      = EXCLUDED.velocity_score,
            duplicate_detected  = EXCLUDED.duplicate_detected,
            last_evaluated_at   = NOW(),
            updated_at          = NOW()",
    )
    .bind(p.pial_id)
    .bind(p.risk_score)
    .bind(p.device_count)
    .bind(p.failed_kyc_attempts)
    .bind(p.ip_reputation_score)
    .bind(p.velocity_score)
    .bind(p.duplicate_detected)
    .execute(pool)
    .await;
}

/// Returns the 2257 record for a creator, if it exists and is active.
pub async fn get_2257_record(
    pool: &PgPool,
    creator_pial: uuid::Uuid,
) -> Option<crate::models::Record2257> {
    sqlx::query_as("SELECT * FROM records_2257 WHERE creator_pial_id = $1 AND is_active = true")
        .bind(creator_pial)
        .fetch_optional(pool)
        .await
        .unwrap_or(None)
}

/// Inserts a new 2257 record.
pub async fn create_2257_record(
    pool: &PgPool,
    r: &crate::models::Record2257,
) -> anyhow::Result<uuid::Uuid> {
    let id: uuid::Uuid = sqlx::query_scalar(
        "INSERT INTO records_2257
           (creator_pial_id, kyc_submission_id, age_band, document_type, document_hash,
            record_keeper, record_location, statement_hash)
         VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id",
    )
    .bind(r.creator_pial_id)
    .bind(r.kyc_submission_id)
    .bind(&r.age_band)
    .bind(&r.document_type)
    .bind(&r.document_hash)
    .bind(&r.record_keeper)
    .bind(&r.record_location)
    .bind(&r.statement_hash)
    .fetch_one(pool)
    .await?;
    Ok(id)
}

/// Gets a CSAM scan result by content hash. Returns None if never scanned.
pub async fn get_csam_scan(pool: &PgPool, content_hash: &str) -> Option<crate::models::CsamScan> {
    sqlx::query_as("SELECT * FROM csam_scans WHERE content_hash = $1")
        .bind(content_hash)
        .fetch_optional(pool)
        .await
        .unwrap_or(None)
}

/// Records a CSAM scan result.
pub async fn record_csam_scan(
    pool: &PgPool,
    content_hash: &str,
    uploader_pial: uuid::Uuid,
    result: &str,
    match_count: i32,
    media_url: Option<&str>,
    media_type: Option<&str>,
) -> anyhow::Result<uuid::Uuid> {
    let id: uuid::Uuid = sqlx::query_scalar(
        // RETURNING closes the statement; it cannot sit between VALUES and
        // ON CONFLICT. Written that way this never parsed, so every scan
        // errored and no scan was ever recorded — including, until now, the
        // uploader's identity registration that hangs off this insert.
        "INSERT INTO csam_scans (content_hash, uploader_pial, result, match_count, media_url, media_type)
         VALUES ($1,$2,$3,$4,$5,$6)
         ON CONFLICT (content_hash) DO UPDATE SET
           result      = EXCLUDED.result,
           match_count = EXCLUDED.match_count,
           scanned_at  = NOW()
         RETURNING id",
    )
    .bind(content_hash)
    .bind(uploader_pial)
    .bind(result)
    .bind(match_count)
    .bind(media_url)
    .bind(media_type)
    .fetch_one(pool)
    .await?;
    Ok(id)
}

/// Appends an audit event.
pub async fn audit(
    pool: &PgPool,
    pial_id: uuid::Uuid,
    action: &str,
    context: Option<&str>,
    before_tier: Option<i32>,
    after_tier: Option<i32>,
    metadata: serde_json::Value,
) {
    let _ = sqlx::query(
        "INSERT INTO verity_audit (pial_id, action, context, before_tier, after_tier, metadata)
         VALUES ($1,$2,$3,$4,$5,$6)",
    )
    .bind(pial_id)
    .bind(action)
    .bind(context)
    .bind(before_tier)
    .bind(after_tier)
    .bind(metadata)
    .execute(pool)
    .await;
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
