-- ── Caeor: the media-asset authority ─────────────────────────────────────────
-- Manhattan's authority map (manhattan/migrations/0002_authority_map.sql)
-- declares that canonical media, derivatives and fingerprints belong to caeor.
-- This database, f33d3r_media, is where that becomes true.
--
-- Four rules shape every table below:
--
--   1. An asset is never deleted, only withdrawn. Credit and ownership outlive
--      the work that carried the asset and the account that uploaded it. The
--      no-delete triggers at the bottom make that a property of the database,
--      not a habit of the code.
--
--   2. An asset is keyed by PIAL. uploader_pial is the identity that owns the
--      credit; uploader_account is the account it was uploaded through and may
--      later be erased. Neither is a foreign key: identities live in
--      f33d3r_security and accounts in f33d3r_feed, and a cross-service FK is a
--      second owner of somebody else's truth.
--
--   3. The clean source is never served. A source_private derivative lives in
--      the private store, whose paths are relative and never start with '/', so
--      no served path can name one. The CHECKs below enforce it.
--
--   4. Every viewer-facing copy of image and video media is watermarked. The
--      `watermarked` flag is recorded per derivative so the read path can refuse
--      anything that is not, including media imported from before this rule.

CREATE TABLE IF NOT EXISTS assets (
    id               UUID        PRIMARY KEY,
    -- 8 characters of Crockford base32 (lowercase, no i l o u). Burned into
    -- every watermark, so a leaked frame names the exact asset it came from.
    short_id         TEXT        NOT NULL UNIQUE
                                 CHECK (short_id ~ '^[0-9a-hjkmnp-tv-z]{8}$'),
    uploader_pial    UUID        NOT NULL,
    uploader_account UUID,
    -- The handle at upload time: what the watermark says. Handles change and
    -- are transferred; the PIAL above is the owner, this is only the label.
    uploader_handle  TEXT        NOT NULL DEFAULT '',
    kind             TEXT        NOT NULL
                                 CHECK (kind IN ('image','video','voice','audio','avatar',
                                                 'header','encrypted','clip')),
    status           TEXT        NOT NULL
                                 CHECK (status IN ('processing','ready','failed','withdrawn')),
    origin           TEXT        NOT NULL DEFAULT 'upload'
                                 CHECK (origin IN ('upload','clip','import')),
    -- SHA-256 of the exact bytes uploaded. NULL only where there were no
    -- uploaded bytes (a clip drawn from a replay) or an import that had none.
    raw_sha256       TEXT        CHECK (raw_sha256 ~ '^[0-9a-f]{64}$'),
    byte_size        BIGINT,
    width            INT,
    height           INT,
    duration_seconds DOUBLE PRECISION,
    failure          TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    withdrawn_at     TIMESTAMPTZ,
    withdrawn_reason TEXT,
    CONSTRAINT assets_withdrawn_consistent CHECK (
        (status = 'withdrawn') = (withdrawn_at IS NOT NULL)
        AND (withdrawn_at IS NULL OR COALESCE(withdrawn_reason, '') <> '')
    ),
    CONSTRAINT assets_failure_consistent CHECK (
        status <> 'failed' OR COALESCE(failure, '') <> ''
    )
);

CREATE INDEX IF NOT EXISTS idx_assets_uploader_pial ON assets(uploader_pial, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_assets_raw_sha256   ON assets(raw_sha256) WHERE raw_sha256 IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_assets_processing   ON assets(created_at) WHERE status = 'processing';

-- ── Derivatives ──────────────────────────────────────────────────────────────
-- One row per file (or HLS tree) made from an asset.
--
--   store = 'served'  → path is the URL path the read path serves, beginning
--                       with '/'. Files under /static/media/ live on the
--                       f33d3r_media volume.
--   store = 'private' → path is relative to caeor's private store, a volume
--                       mounted only into caeor and transcoding. Never served;
--                       reached only through caeor's own endpoints.
--
-- serves_prefix is set for HLS roles: every file under that directory (the
-- playlist's segments) belongs to this derivative. resolve() looks a segment up
-- by its ancestor directories, so the lookup stays an index probe.
--
-- removed_at records that a derivative's bytes were removed by a withdrawal.
-- The row stays: what existed, and what was taken down, is part of the record.
CREATE TABLE IF NOT EXISTS derivatives (
    asset_id      UUID        NOT NULL REFERENCES assets(id),
    role          TEXT        NOT NULL
                              CHECK (role IN ('thumb','feed','full','hls_master','hls_variant',
                                              'poster','download','source_private')),
    -- '' except for hls_variant, where it names the rendition ('720p').
    variant       TEXT        NOT NULL DEFAULT '',
    store         TEXT        NOT NULL CHECK (store IN ('served','private')),
    path          TEXT        NOT NULL,
    serves_prefix TEXT,
    watermarked   BOOLEAN     NOT NULL,
    content_type  TEXT        NOT NULL,
    width         INT,
    height        INT,
    bandwidth     INT,
    byte_size     BIGINT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    removed_at    TIMESTAMPTZ,
    PRIMARY KEY (asset_id, role, variant),
    CONSTRAINT derivatives_store_path CHECK (
        (store = 'served'  AND path LIKE '/%' AND path NOT LIKE '%/../%' AND path NOT LIKE '%/..')
     OR (store = 'private' AND path NOT LIKE '/%' AND path NOT LIKE '%..%')
    ),
    CONSTRAINT derivatives_source_is_private CHECK (role <> 'source_private' OR store = 'private'),
    CONSTRAINT derivatives_prefix_only_hls CHECK (
        (serves_prefix IS NULL) = (role NOT IN ('hls_master','hls_variant'))
        AND (serves_prefix IS NULL OR (store = 'served' AND serves_prefix LIKE '/%/'))
    ),
    CONSTRAINT derivatives_variant_only_hls CHECK ((variant = '') = (role <> 'hls_variant'))
);

-- A served path names exactly one derivative.
CREATE UNIQUE INDEX IF NOT EXISTS idx_derivatives_served_path
    ON derivatives(path) WHERE store = 'served';
CREATE UNIQUE INDEX IF NOT EXISTS idx_derivatives_serves_prefix
    ON derivatives(serves_prefix) WHERE serves_prefix IS NOT NULL;

-- ── Raw hashes: exact-byte dedup ─────────────────────────────────────────────
-- sha256 → the FIRST asset made from those exact bytes. Credit goes to that
-- asset's uploader for as long as the platform exists: the row survives the
-- asset's withdrawal and its uploader's erasure.
--
-- This is exact-byte identity only. Perceptual fingerprints (re-encodes,
-- crops, screen recordings) are Abraxas's job in a later phase; they will live
-- in their own table keyed by asset_id, written by Abraxas's verdicts, and
-- consulted beside this one. Nothing here needs to change for that.
CREATE TABLE IF NOT EXISTS raw_hashes (
    sha256        TEXT        PRIMARY KEY CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    asset_id      UUID        NOT NULL REFERENCES assets(id),
    uploader_pial UUID        NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_raw_hashes_asset ON raw_hashes(asset_id);

-- ── Upload jobs: the durable job table ───────────────────────────────────────
-- Replaces transcoding's in-memory job map as the record of truth. Transcoding
-- is a stateless worker: caeor dispatches a job, polls it, and re-dispatches
-- it if transcoding restarted and lost it. The source lives in the private
-- store, so a re-dispatch has everything it needs and a restart of either side
-- loses nothing.
--
--   queued      → waiting to be handed to transcoding
--   dispatched  → transcoding has it; caeor polls
--   cancelling  → the asset was withdrawn while transcoding had it; caeor waits
--                 for transcoding to stop, then removes whatever it wrote
--   ready / failed / cancelled → terminal
CREATE TABLE IF NOT EXISTS upload_jobs (
    id              UUID        PRIMARY KEY,
    asset_id        UUID        NOT NULL REFERENCES assets(id),
    purpose         TEXT        NOT NULL CHECK (purpose IN ('transcode','clip','remark')),
    state           TEXT        NOT NULL
                                CHECK (state IN ('queued','dispatched','cancelling',
                                                 'ready','failed','cancelled')),
    request         JSONB       NOT NULL,
    result          JSONB,
    attempts        INT         NOT NULL DEFAULT 0,
    last_error      TEXT,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    dispatched_at   TIMESTAMPTZ,
    finished_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT upload_jobs_finished_consistent CHECK (
        (finished_at IS NOT NULL) = (state IN ('ready','failed','cancelled'))
    )
);

-- The reconciler's query: open jobs, due, oldest first.
CREATE INDEX IF NOT EXISTS idx_upload_jobs_open
    ON upload_jobs(next_attempt_at, created_at)
 WHERE state IN ('queued','dispatched','cancelling');

-- One open job per asset: two encodes racing to write one asset's tree is a
-- corrupted tree.
CREATE UNIQUE INDEX IF NOT EXISTS idx_upload_jobs_one_open_per_asset
    ON upload_jobs(asset_id)
 WHERE state IN ('queued','dispatched','cancelling');

-- ── Never deleted ────────────────────────────────────────────────────────────
-- Withdrawal is an UPDATE. A DELETE of an asset or of the hash that credits it
-- is refused by the database itself, whoever asks.
CREATE OR REPLACE FUNCTION caeor_refuse_delete() RETURNS TRIGGER AS $fn$
BEGIN
    RAISE EXCEPTION '% rows are never deleted; withdraw the asset instead', TG_TABLE_NAME
        USING ERRCODE = 'restrict_violation';
END;
$fn$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_assets_never_deleted ON assets;
CREATE TRIGGER trg_assets_never_deleted
    BEFORE DELETE ON assets
    FOR EACH ROW EXECUTE FUNCTION caeor_refuse_delete();

DROP TRIGGER IF EXISTS trg_raw_hashes_never_deleted ON raw_hashes;
CREATE TRIGGER trg_raw_hashes_never_deleted
    BEFORE DELETE ON raw_hashes
    FOR EACH ROW EXECUTE FUNCTION caeor_refuse_delete();

-- A TRUNCATE skips row triggers, so it is refused separately.
CREATE OR REPLACE FUNCTION caeor_refuse_truncate() RETURNS TRIGGER AS $fn$
BEGIN
    RAISE EXCEPTION '% is never truncated', TG_TABLE_NAME
        USING ERRCODE = 'restrict_violation';
END;
$fn$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_assets_never_truncated ON assets;
CREATE TRIGGER trg_assets_never_truncated
    BEFORE TRUNCATE ON assets
    FOR EACH STATEMENT EXECUTE FUNCTION caeor_refuse_truncate();

DROP TRIGGER IF EXISTS trg_raw_hashes_never_truncated ON raw_hashes;
CREATE TRIGGER trg_raw_hashes_never_truncated
    BEFORE TRUNCATE ON raw_hashes
    FOR EACH STATEMENT EXECUTE FUNCTION caeor_refuse_truncate();

-- An asset's owner is fixed at creation: credit cannot be reassigned by an
-- UPDATE any more than it can be erased by a DELETE.
CREATE OR REPLACE FUNCTION caeor_owner_is_fixed() RETURNS TRIGGER AS $fn$
BEGIN
    IF NEW.uploader_pial IS DISTINCT FROM OLD.uploader_pial
       OR NEW.short_id IS DISTINCT FROM OLD.short_id
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'an asset''s uploader, short_id and creation time are fixed'
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF OLD.status = 'withdrawn' AND NEW.status <> 'withdrawn' THEN
        RAISE EXCEPTION 'a withdrawn asset stays withdrawn'
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$fn$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_assets_owner_fixed ON assets;
CREATE TRIGGER trg_assets_owner_fixed
    BEFORE UPDATE ON assets
    FOR EACH ROW EXECUTE FUNCTION caeor_owner_is_fixed();
