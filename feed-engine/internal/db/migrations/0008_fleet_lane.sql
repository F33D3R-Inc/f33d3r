-- 0008_fleet_lane.sql
--
-- Fleet gets its own ephemeral lane. `kind='vision'` leaves the Work lane: the
-- expiry CASE in InsertWork is removed in the same change, and the rows it
-- created are soft-deleted rather than silently promoted to permanent works.

-- ── fleets ───────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS fleets (
    id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    author_id            UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    author_pial          UUID        NOT NULL,
    content_type         TEXT        NOT NULL DEFAULT 'text'
                                     CHECK (content_type IN ('text', 'image', 'video', 'share')),
    body                 TEXT        NOT NULL DEFAULT '',
    -- Server-side artboard selection. Preset keys, never client CSS: the
    -- renderer resolves them and falls back to the default on an unknown key.
    artboard_background  TEXT        NOT NULL DEFAULT 'void',
    artboard_typeface    TEXT        NOT NULL DEFAULT 'grotesk',
    artboard_type_scale  TEXT        NOT NULL DEFAULT 'auto'
                                     CHECK (artboard_type_scale IN ('auto', 's', 'm', 'l', 'xl')),
    artboard_align       TEXT        NOT NULL DEFAULT 'center'
                                     CHECK (artboard_align IN ('left', 'center', 'right')),
    media_urls           TEXT[]      NOT NULL DEFAULT '{}',
    shared_work_id       UUID        REFERENCES works(id) ON DELETE SET NULL,
    scan_state           TEXT        NOT NULL DEFAULT 'pending',
    is_nsfw              BOOLEAN     NOT NULL DEFAULT FALSE,
    is_blocked           BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at           TIMESTAMPTZ NOT NULL DEFAULT NOW() + INTERVAL '24 hours',
    deleted_at           TIMESTAMPTZ,
    metadata             JSONB       NOT NULL DEFAULT '{}',
    CONSTRAINT fleets_expiry_after_creation CHECK (expires_at > created_at)
);

-- NOW() is not immutable, so it cannot appear in an index predicate. The live
-- window is tested in each query; the index only narrows to undeleted rows.
CREATE INDEX IF NOT EXISTS idx_fleets_author_live
    ON fleets(author_id, expires_at, created_at) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_fleets_expiry
    ON fleets(expires_at) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_fleets_author_pial
    ON fleets(author_pial);
CREATE INDEX IF NOT EXISTS idx_fleets_shared_work
    ON fleets(shared_work_id) WHERE shared_work_id IS NOT NULL;

-- ── fleet_views ──────────────────────────────────────────────────────────────
-- Composite primary key: reopening a fleet is idempotent, not a second row.
CREATE TABLE IF NOT EXISTS fleet_views (
    fleet_id    UUID        NOT NULL REFERENCES fleets(id) ON DELETE CASCADE,
    viewer_pial UUID        NOT NULL,
    viewed_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (fleet_id, viewer_pial)
);
CREATE INDEX IF NOT EXISTS idx_fleet_views_viewer
    ON fleet_views(viewer_pial, viewed_at DESC);

-- ── fleet_media ──────────────────────────────────────────────────────────────
-- Ephemeral media lifecycle, separate from permanent work media.
CREATE TABLE IF NOT EXISTS fleet_media (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    fleet_id       UUID        NOT NULL REFERENCES fleets(id) ON DELETE CASCADE,
    object_key     TEXT        NOT NULL,
    caeor_asset_id TEXT        NOT NULL DEFAULT '',
    asset_url      TEXT        NOT NULL DEFAULT '',
    media_kind     TEXT        NOT NULL DEFAULT 'image'
                               CHECK (media_kind IN ('image', 'video')),
    width          INT         NOT NULL DEFAULT 0,
    height         INT         NOT NULL DEFAULT 0,
    duration_secs  REAL        NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    purge_after    TIMESTAMPTZ NOT NULL,
    purged_at      TIMESTAMPTZ,
    UNIQUE (fleet_id, object_key)
);
CREATE INDEX IF NOT EXISTS idx_fleet_media_purge
    ON fleet_media(purge_after) WHERE purged_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_fleet_media_fleet
    ON fleet_media(fleet_id);

-- No manhattan_outbox trigger on fleets, deliberately — contrast
-- trg_manhattan_register_work in 0005. A 24-hour object mints no permanent
-- naming-plane node, name or edge.

-- ── the Work lane stops being ephemeral ──────────────────────────────────────
UPDATE works
   SET deleted_at = NOW()
 WHERE kind = 'vision'
   AND deleted_at IS NULL;
