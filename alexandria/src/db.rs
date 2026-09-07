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

-- Authors: one record per PIAL.
--
-- An author is an IDENTITY, and an identity is a PIAL. It is referenced by the
-- name 'pial:<pial_id>' and resolved through Manhattan; alexandria stores no
-- second copy of who that person is.
--
-- There is deliberately no `handle` column. A handle is a transferable pointer
-- owned by registry-brain, not an attribute of the identity it currently points
-- at. Storing it here made alexandria a second handle authority that could not
-- observe a transfer: the row kept the previous owner's handle, so a catalog
-- page attributed a title to the wrong person, and a UNIQUE index on a handle
-- the new owner now holds turned the next bootstrap into a constraint
-- violation. The handle is resolved at read time instead — one batch call for
-- a whole page, which is the cheap lookup whose absence caused the copying.
CREATE TABLE IF NOT EXISTS authors (
    pial_id            UUID        PRIMARY KEY,
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
    legacy_post_id   UUID,
    -- The name this title answers to on the naming plane.
    --
    -- A title that mirrors a feed-engine work is not a second work: it is the
    -- same work, catalogued. Naming it by legacy_work_id makes both brains
    -- resolve to ONE node instead of minting two nodes for one thing — which
    -- is the exact failure the naming plane exists to prevent. A title with no
    -- upstream work is alexandria-native and answers to its own id.
    --
    -- Generated rather than written, so the policy has one definition that a
    -- future insert path cannot forget to apply.
    work_name        TEXT        GENERATED ALWAYS AS ('uuid:' || COALESCE(legacy_work_id, id)::text) STORED
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

// ── Forward migrations ───────────────────────────────────────────────────────
//
// SCHEMA above describes a fresh database. This block brings an existing one to
// the same shape. Every statement is idempotent, so it runs on every boot
// through the same `raw_sql` mechanism SCHEMA uses — there is one migration
// path in this brain, not two.
const MIGRATE: &str = r#"
-- alexandria stopped being a handle authority. registry-brain owns handles;
-- this copy could not observe a transfer, so it served the previous owner's
-- handle until someone noticed, and its UNIQUE index rejected the new owner's
-- bootstrap outright. Dropped, not deprecated: a column nobody may trust is a
-- trap for the next reader.
ALTER TABLE authors DROP COLUMN IF EXISTS handle;

-- One definition of the name a title answers to on the naming plane.
ALTER TABLE titles ADD COLUMN IF NOT EXISTS work_name TEXT
    GENERATED ALWAYS AS ('uuid:' || COALESCE(legacy_work_id, id)::text) STORED;

CREATE INDEX IF NOT EXISTS idx_titles_work_name ON titles(work_name);
"#;

// ── Manhattan handoff ────────────────────────────────────────────────────────
//
// alexandria's durable handoff to the naming plane, the same pattern
// feed-engine uses (feed-engine/internal/db/migrations/0005_manhattan_outbox.sql).
//
// Manhattan is a separate brain reached over HTTP. A registration sent
// fire-and-forget is a registration that a restart, a timeout or a rolling
// deploy silently loses — and a naming plane that is silently missing rows is
// worse than no naming plane, because everything downstream trusts it.
//
// So the handoff is transactional. Rows are written by triggers on the tables
// they describe, inside the same transaction as the row that caused them.
// Either an author exists AND its registration is queued, or neither happened.
// A background drain (src/outbox.rs) delivers them in order, retrying with
// backoff, and marks them delivered.
//
// The triggers are the reason this cannot rot: a future insert path nobody has
// thought of yet still registers, because registering is a property of the
// table rather than a step a caller must remember.
//
// Payloads carry NAMES, never node ids. Manhattan assigns node ids; this side
// does not know them and must not learn them, or the two planes acquire a
// second shared identifier and we are back where we started.
//
// WHAT ALEXANDRIA MAY ENQUEUE, and why the list is short:
//
//   Manhattan's authority map (manhattan/migrations/0002_authority_map.sql)
//   gives node kinds to owning brains and namespaces to naming authorities.
//   Any brain may CREATE a node — registration has to work whoever encounters
//   the entity first — but writing further FACTS about one is the owner's
//   right alone. Identities belong to elohim-veni, works to feed-engine,
//   the 'handle' namespace to registry-brain, 'uuid' and 'cid' to feed-engine.
//
//   So alexandria enqueues node registrations and nothing else. It does NOT
//   enqueue handle bindings, cid bindings or authored_by edges: Manhattan would
//   answer 403, and because the drain is strictly ordered and never skips, one
//   such row would block every registration queued behind it forever. A write
//   this brain is not entitled to make is not queued in the first place.
const MANHATTAN: &str = r#"
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

-- ── enqueue helper ───────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION manhattan_enqueue(p_op TEXT, p_dedup TEXT, p_payload JSONB)
RETURNS VOID AS $$
BEGIN
    INSERT INTO manhattan_outbox (op, dedup_key, payload)
    VALUES (p_op, p_dedup, p_payload)
    ON CONFLICT (dedup_key) DO NOTHING;
END;
$$ LANGUAGE plpgsql;

-- ── authors ──────────────────────────────────────────────────────────────────
-- An author is named by PIAL and only by PIAL. The handle that points at that
-- identity is registry-brain's to bind; alexandria registering it here is what
-- created two authorities for one handle in the first place.
CREATE OR REPLACE FUNCTION manhattan_register_author() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.pial_id IS NULL THEN
        RETURN NEW;
    END IF;

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

DROP TRIGGER IF EXISTS trg_manhattan_register_author ON authors;
CREATE TRIGGER trg_manhattan_register_author
    AFTER INSERT OR UPDATE OF pial_id ON authors
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_author();

-- ── titles ───────────────────────────────────────────────────────────────────
-- A title is a work. work_name resolves a catalogued feed-engine work to the
-- node feed-engine already registered, and mints one only for content that
-- originates here — so one work has one node however many brains catalog it.
--
-- AFTER INSERT, because a generated column is not populated in a BEFORE trigger.
CREATE OR REPLACE FUNCTION manhattan_register_title() RETURNS TRIGGER AS $$
BEGIN
    PERFORM manhattan_enqueue(
        'node',
        'node:' || NEW.work_name,
        jsonb_build_object(
            'kind',      'work',
            'name',      NEW.work_name,
            'namespace', 'uuid'));

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_manhattan_register_title ON titles;
CREATE TRIGGER trg_manhattan_register_title
    AFTER INSERT ON titles
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_title();

-- ── backfill ─────────────────────────────────────────────────────────────────
-- Everything that already exists is enqueued once, so the plane starts complete
-- instead of only knowing about rows written after this deploy. ON CONFLICT DO
-- NOTHING in the helper makes re-running this harmless.
DO $$
DECLARE r RECORD;
BEGIN
    FOR r IN SELECT pial_id FROM authors WHERE pial_id IS NOT NULL LOOP
        PERFORM manhattan_enqueue('node', 'node:pial:' || r.pial_id::text,
            jsonb_build_object('kind','identity','name','pial:' || r.pial_id::text,'namespace','pial'));
    END LOOP;

    FOR r IN SELECT work_name FROM titles WHERE tombstoned_at IS NULL LOOP
        PERFORM manhattan_enqueue('node', 'node:' || r.work_name,
            jsonb_build_object('kind','work','name',r.work_name,'namespace','uuid'));
    END LOOP;
END $$;
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
    sqlx::raw_sql(MIGRATE).execute(pool).await?;
    sqlx::raw_sql(MANHATTAN).execute(pool).await?;
    sqlx::raw_sql(SEED).execute(pool).await?;
    tracing::info!("alexandria: schema + manhattan outbox + seed applied");
    Ok(())
}

/// How many registrations are queued and not yet delivered. Surfaced on
/// /health: a backlog that only grows means the naming plane is not receiving
/// what this brain has promised it, and that has to be visible without reading
/// logs.
pub async fn outbox_pending(pool: &PgPool) -> sqlx::Result<i64> {
    sqlx::query_scalar::<_, i64>("SELECT COUNT(*) FROM manhattan_outbox WHERE delivered_at IS NULL")
        .fetch_one(pool)
        .await
}
