-- 0023_vision_lane.sql
--
-- The Fleet lane is a Vision lane now, and PIAL is the only standalone
-- identity it carries: a Vision has no id of its own. It is a position in its
-- author's own PIAL-owned lane, addressed as (author_pial, seq) — the PIAL
-- that owns the lane and the ordinal vision_lanes handed out for it. The
-- canonical text form is `<author_pial>.<seq>`, and that string is what the
-- wire carries everywhere an id used to go — the API, the facet ids, the
-- content-safety events.
--
-- There is no data to carry forward (pre-production), so this drops the old
-- fleets/fleet_views/fleet_media/fleet_poll_votes/fleet_mutes tables outright
-- rather than migrating rows. devserver's SQLite mirror (internal/store) made
-- the identical move — see its schema.sql and store/visions.go, which this
-- migration exists to catch feed-engine's Postgres schema up to.
--
-- Dropping author_id also drops the ON DELETE CASCADE a purge relied on to
-- remove an erased account's own Visions; db.purgeOrphanStatements in
-- works.go now deletes them explicitly by author_pial instead.

DROP TABLE IF EXISTS fleet_poll_votes;
DROP TABLE IF EXISTS fleet_mutes;
DROP TABLE IF EXISTS fleet_media;
DROP TABLE IF EXISTS fleet_views;
DROP TABLE IF EXISTS fleets;

-- ── vision_lanes ─────────────────────────────────────────────────────────────
-- One row per PIAL that has ever posted a Vision. last_seq is the ordinal
-- most recently handed out from that lane; InsertVision increments it inside
-- the same transaction as the row, so two posts racing on one lane never
-- collide and never reuse an address.
CREATE TABLE IF NOT EXISTS vision_lanes (
    pial_id  UUID   PRIMARY KEY,
    last_seq BIGINT NOT NULL DEFAULT 0
);

-- ── visions ──────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS visions (
    author_pial          UUID        NOT NULL,
    seq                  BIGINT      NOT NULL,
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
    poll_options         TEXT[]      NOT NULL DEFAULT '{}',
    audience             TEXT        NOT NULL DEFAULT 'everyone'
                                     CHECK (audience IN ('everyone', 'followers')),
    allow_replies        BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at           TIMESTAMPTZ NOT NULL DEFAULT NOW() + INTERVAL '24 hours',
    deleted_at           TIMESTAMPTZ,
    metadata             JSONB       NOT NULL DEFAULT '{}',
    PRIMARY KEY (author_pial, seq),
    CONSTRAINT visions_expiry_after_creation CHECK (expires_at > created_at)
);

-- NOW() is not immutable, so it cannot appear in an index predicate. The live
-- window is tested in each query; the index only narrows to undeleted rows.
CREATE INDEX IF NOT EXISTS idx_visions_author_live
    ON visions(author_pial, expires_at, created_at) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_visions_expiry
    ON visions(expires_at) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_visions_shared_work
    ON visions(shared_work_id) WHERE shared_work_id IS NOT NULL;

-- ── vision_views ─────────────────────────────────────────────────────────────
-- Composite primary key: reopening a Vision is idempotent, not a second row.
CREATE TABLE IF NOT EXISTS vision_views (
    author_pial UUID        NOT NULL,
    seq         BIGINT      NOT NULL,
    viewer_pial UUID        NOT NULL,
    viewed_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (author_pial, seq, viewer_pial),
    FOREIGN KEY (author_pial, seq) REFERENCES visions(author_pial, seq) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_vision_views_viewer
    ON vision_views(viewer_pial, viewed_at DESC);

-- ── vision_poll_votes ────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS vision_poll_votes (
    author_pial UUID        NOT NULL,
    seq         BIGINT      NOT NULL,
    voter_pial  UUID        NOT NULL,
    option_idx  INT         NOT NULL CHECK (option_idx >= 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (author_pial, seq, voter_pial),
    FOREIGN KEY (author_pial, seq) REFERENCES visions(author_pial, seq) ON DELETE CASCADE
);

-- ── vision_media ─────────────────────────────────────────────────────────────
-- Ephemeral media lifecycle, separate from permanent work media.
CREATE TABLE IF NOT EXISTS vision_media (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    author_pial    UUID        NOT NULL,
    seq            BIGINT      NOT NULL,
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
    UNIQUE (author_pial, seq, object_key),
    FOREIGN KEY (author_pial, seq) REFERENCES visions(author_pial, seq) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_vision_media_purge
    ON vision_media(purge_after) WHERE purged_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_vision_media_vision
    ON vision_media(author_pial, seq);

-- ── vision_mutes ─────────────────────────────────────────────────────────────
-- PIAL to PIAL, not id to id — the old fleet_mutes kept muter_id/muted_id;
-- everything about a Vision, including who has muted whom, is PIAL-addressed
-- now.
CREATE TABLE IF NOT EXISTS vision_mutes (
    muter_pial UUID        NOT NULL,
    muted_pial UUID        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (muter_pial, muted_pial)
);

-- No manhattan_outbox trigger on visions, deliberately — contrast
-- trg_manhattan_register_work in 0005. A 24-hour object mints no permanent
-- naming-plane node, name or edge.
