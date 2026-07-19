use sqlx::PgPool;

const SCHEMA: &str = r#"
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- Sections: broad wings of the library (seed data, never user-generated)
CREATE TABLE IF NOT EXISTS sections (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    slug          TEXT        NOT NULL UNIQUE,
    name          TEXT        NOT NULL,
    description   TEXT        NOT NULL DEFAULT '',
    display_order INTEGER     NOT NULL DEFAULT 0
);

-- Genres: taxonomy within sections; parent_genre_id enables sub-genres
CREATE TABLE IF NOT EXISTS genres (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    section_id      UUID        NOT NULL REFERENCES sections(id),
    slug            TEXT        NOT NULL UNIQUE,
    name            TEXT        NOT NULL,
    parent_genre_id UUID        REFERENCES genres(id),
    display_order   INTEGER     NOT NULL DEFAULT 0
);

-- Authors: one record per PIAL, auto-created on bootstrap.
-- pial_id is a soft reference — no FK to another brain's DB.
-- Denormalised for reads: no joins needed at render time.
CREATE TABLE IF NOT EXISTS authors (
    pial_id            UUID        PRIMARY KEY,
    handle             TEXT        NOT NULL UNIQUE,
    display_name       TEXT        NOT NULL DEFAULT '',
    avatar_url         TEXT        NOT NULL DEFAULT '',
    header_url         TEXT        NOT NULL DEFAULT '',
    bio                TEXT        NOT NULL DEFAULT '',
    primary_section_id UUID        REFERENCES sections(id),
    creator_tier       TEXT        NOT NULL DEFAULT 'standard',
    is_verified        BOOLEAN     NOT NULL DEFAULT FALSE,
    title_count        INTEGER     NOT NULL DEFAULT 0,
    follower_count     INTEGER     NOT NULL DEFAULT 0,
    following_count    INTEGER     NOT NULL DEFAULT 0,
    status             TEXT        NOT NULL DEFAULT 'active',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Titles: every piece of content ever created.
-- Immutable ID. Tombstone on delete — never hard-delete.
-- parent_id / root_id / thread_id are soft references (no FKs) so
-- wiping titles never cascades outside this table.
CREATE TABLE IF NOT EXISTS titles (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    author_pial_id   UUID        NOT NULL,
    section_id       UUID        REFERENCES sections(id),
    genre_id         UUID        REFERENCES genres(id),
    headline         TEXT,
    body             TEXT        NOT NULL DEFAULT '',
    media_area       TEXT        NOT NULL DEFAULT 'text',
    status           TEXT        NOT NULL DEFAULT 'draft',
    visibility       TEXT        NOT NULL DEFAULT 'public',
    parent_id        UUID,
    root_id          UUID,
    thread_id        UUID,
    depth            SMALLINT    NOT NULL DEFAULT 0,
    lineage          TEXT        NOT NULL DEFAULT '',
    content_hash     TEXT,
    is_nsfw          BOOLEAN     NOT NULL DEFAULT FALSE,
    is_sensitive     BOOLEAN     NOT NULL DEFAULT FALSE,
    reply_restriction TEXT       NOT NULL DEFAULT 'everyone',
    published_at     TIMESTAMPTZ,
    scheduled_at     TIMESTAMPTZ,
    tombstoned_at    TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    legacy_work_id   UUID,
    legacy_post_id   UUID
);

CREATE INDEX IF NOT EXISTS idx_titles_author      ON titles(author_pial_id, published_at DESC) WHERE tombstoned_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_titles_section     ON titles(section_id, published_at DESC)     WHERE tombstoned_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_titles_parent      ON titles(parent_id)                         WHERE parent_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_titles_root        ON titles(root_id)                           WHERE root_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_titles_thread      ON titles(thread_id)                         WHERE thread_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_titles_legacy_work ON titles(legacy_work_id)                    WHERE legacy_work_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_titles_legacy_post ON titles(legacy_post_id)                    WHERE legacy_post_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_titles_status      ON titles(status, published_at DESC);

-- Holdings: media assets — pointers to Caeor paths, never raw storage URLs.
-- ON DELETE CASCADE is safe here because titles are tombstoned, not deleted.
CREATE TABLE IF NOT EXISTS holdings (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    title_id      UUID        NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    asset_type    TEXT        NOT NULL,
    caeor_path    TEXT        NOT NULL,
    mime_type     TEXT        NOT NULL DEFAULT '',
    file_size     BIGINT,
    width         INTEGER,
    height        INTEGER,
    duration_secs INTEGER,
    is_primary    BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_holdings_title ON holdings(title_id);

-- Title signals: engagement counters — write-isolated for performance.
-- AethyrRank consumes this table for ranking signals.
CREATE TABLE IF NOT EXISTS title_signals (
    title_id       UUID        PRIMARY KEY REFERENCES titles(id) ON DELETE CASCADE,
    like_count     INTEGER     NOT NULL DEFAULT 0,
    repost_count   INTEGER     NOT NULL DEFAULT 0,
    reply_count    INTEGER     NOT NULL DEFAULT 0,
    quote_count    INTEGER     NOT NULL DEFAULT 0,
    view_count     BIGINT      NOT NULL DEFAULT 0,
    bookmark_count INTEGER     NOT NULL DEFAULT 0,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Title tags
CREATE TABLE IF NOT EXISTS title_tags (
    title_id UUID NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    tag      TEXT NOT NULL,
    PRIMARY KEY (title_id, tag)
);

CREATE INDEX IF NOT EXISTS idx_title_tags_tag ON title_tags(tag);

-- Title quotes: semantic quote links (separate from parent/reply chain)
CREATE TABLE IF NOT EXISTS title_quotes (
    source_title_id UUID        NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    target_title_id UUID        NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (source_title_id, target_title_id)
);

-- Link previews: URL unfurl cache owned by Alexandria
CREATE TABLE IF NOT EXISTS link_previews (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    title_id       UUID        NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    url            TEXT        NOT NULL,
    og_title       TEXT        NOT NULL DEFAULT '',
    og_description TEXT        NOT NULL DEFAULT '',
    og_image_url   TEXT        NOT NULL DEFAULT '',
    og_site_name   TEXT        NOT NULL DEFAULT '',
    fetched_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
"#;

const SEED: &str = r#"
INSERT INTO sections (slug, name, description, display_order) VALUES
    ('feed',     'Feed',     'Short-form posts and micro-conversations', 1),
    ('articles', 'Articles', 'Long-form writing and essays',             2),
    ('visions',  'Visions',  'Video and visual content',                 3),
    ('music',    'Music',    'Audio tracks and playlists',               4),
    ('archive',  'Archive',  'Preserved and historical content',         5)
ON CONFLICT (slug) DO NOTHING;

INSERT INTO genres (section_id, slug, name, display_order)
SELECT id, 'general',      'General',        1 FROM sections WHERE slug = 'feed'        ON CONFLICT (slug) DO NOTHING;
INSERT INTO genres (section_id, slug, name, display_order)
SELECT id, 'conversation', 'Conversation',   2 FROM sections WHERE slug = 'feed'        ON CONFLICT (slug) DO NOTHING;
INSERT INTO genres (section_id, slug, name, display_order)
SELECT id, 'news',         'News',           3 FROM sections WHERE slug = 'feed'        ON CONFLICT (slug) DO NOTHING;
INSERT INTO genres (section_id, slug, name, display_order)
SELECT id, 'humor',        'Humor',          4 FROM sections WHERE slug = 'feed'        ON CONFLICT (slug) DO NOTHING;

INSERT INTO genres (section_id, slug, name, display_order)
SELECT id, 'fiction',      'Fiction',        1 FROM sections WHERE slug = 'articles'    ON CONFLICT (slug) DO NOTHING;
INSERT INTO genres (section_id, slug, name, display_order)
SELECT id, 'non-fiction',  'Non-Fiction',    2 FROM sections WHERE slug = 'articles'    ON CONFLICT (slug) DO NOTHING;
INSERT INTO genres (section_id, slug, name, display_order)
SELECT id, 'essay',        'Essay',          3 FROM sections WHERE slug = 'articles'    ON CONFLICT (slug) DO NOTHING;
INSERT INTO genres (section_id, slug, name, display_order)
SELECT id, 'biography',    'Biography',      4 FROM sections WHERE slug = 'articles'    ON CONFLICT (slug) DO NOTHING;
INSERT INTO genres (section_id, slug, name, display_order)
SELECT id, 'science',      'Science',        5 FROM sections WHERE slug = 'articles'    ON CONFLICT (slug) DO NOTHING;
INSERT INTO genres (section_id, slug, name, display_order)
SELECT id, 'technology',   'Technology',     6 FROM sections WHERE slug = 'articles'    ON CONFLICT (slug) DO NOTHING;

INSERT INTO genres (section_id, slug, name, display_order)
SELECT id, 'short-film',   'Short Film',     1 FROM sections WHERE slug = 'visions'     ON CONFLICT (slug) DO NOTHING;
INSERT INTO genres (section_id, slug, name, display_order)
SELECT id, 'documentary',  'Documentary',    2 FROM sections WHERE slug = 'visions'     ON CONFLICT (slug) DO NOTHING;
INSERT INTO genres (section_id, slug, name, display_order)
SELECT id, 'tutorial',     'Tutorial',       3 FROM sections WHERE slug = 'visions'     ON CONFLICT (slug) DO NOTHING;
INSERT INTO genres (section_id, slug, name, display_order)
SELECT id, 'live',         'Live',           4 FROM sections WHERE slug = 'visions'     ON CONFLICT (slug) DO NOTHING;

INSERT INTO genres (section_id, slug, name, display_order)
SELECT id, 'hip-hop',      'Hip-Hop',        1 FROM sections WHERE slug = 'music'       ON CONFLICT (slug) DO NOTHING;
INSERT INTO genres (section_id, slug, name, display_order)
SELECT id, 'electronic',   'Electronic',     2 FROM sections WHERE slug = 'music'       ON CONFLICT (slug) DO NOTHING;
INSERT INTO genres (section_id, slug, name, display_order)
SELECT id, 'r-and-b',      'R&B',            3 FROM sections WHERE slug = 'music'       ON CONFLICT (slug) DO NOTHING;
INSERT INTO genres (section_id, slug, name, display_order)
SELECT id, 'pop',          'Pop',            4 FROM sections WHERE slug = 'music'       ON CONFLICT (slug) DO NOTHING;
INSERT INTO genres (section_id, slug, name, display_order)
SELECT id, 'rock',         'Rock',           5 FROM sections WHERE slug = 'music'       ON CONFLICT (slug) DO NOTHING;
INSERT INTO genres (section_id, slug, name, display_order)
SELECT id, 'jazz',         'Jazz',           6 FROM sections WHERE slug = 'music'       ON CONFLICT (slug) DO NOTHING;
INSERT INTO genres (section_id, slug, name, display_order)
SELECT id, 'classical',    'Classical',      7 FROM sections WHERE slug = 'music'       ON CONFLICT (slug) DO NOTHING;
INSERT INTO genres (section_id, slug, name, display_order)
SELECT id, 'indie',        'Indie',          8 FROM sections WHERE slug = 'music'       ON CONFLICT (slug) DO NOTHING;
"#;

pub async fn migrate(pool: &PgPool) -> anyhow::Result<()> {
    sqlx::raw_sql(SCHEMA).execute(pool).await?;
    sqlx::raw_sql(SEED).execute(pool).await?;
    tracing::info!("alexandria: schema + seed applied");
    Ok(())
}
