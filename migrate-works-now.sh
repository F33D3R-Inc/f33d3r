#!/usr/bin/env bash
# migrate-works-now.sh — one-shot prod fix: migrate all posts → works table.
# Run directly on the prod server:
#   bash migrate-works-now.sh
#
# Idempotent. Safe to run multiple times (NOT EXISTS guards prevent duplicates).
# Takes ~30s on a 10K-post database; longer for larger datasets.
# No downtime required — feed-engine can stay up during the migration.

set -euo pipefail

CONTAINER="f33d3r-prod-postgres-1"
DB="f33d3r_feed"
USER="${POSTGRES_USER:-f33d3r}"

# Allow override: CONTAINER=my-postgres-name bash migrate-works-now.sh
CONTAINER="${POSTGRES_CONTAINER:-$CONTAINER}"

echo "==> F33D3R posts→works migration"
echo "    container : $CONTAINER"
echo "    database  : $DB"
echo "    user      : $USER"
echo ""

# Verify the container is running
if ! docker inspect "$CONTAINER" >/dev/null 2>&1; then
  echo "ERROR: container '$CONTAINER' not found."
  echo "List running containers with: docker ps"
  echo "Override with: POSTGRES_CONTAINER=<name> bash $0"
  exit 1
fi

psql_exec() {
  docker exec -i "$CONTAINER" \
    psql -U "$USER" -d "$DB" -v ON_ERROR_STOP=1 "$@"
}

# ── Step 0: counts before ────────────────────────────────────────────────────
echo "--- counts before migration ---"
psql_exec -tAc "SELECT 'posts', COUNT(*) FROM posts UNION ALL SELECT 'works', COUNT(*) FROM works;"
echo ""

# ── Step 1: posts → works ────────────────────────────────────────────────────
echo "==> Step 1/3: migrating posts → works ..."

psql_exec <<'SQL'
INSERT INTO works (
    id,
    cid,
    author_id,
    author_pial,
    body,
    kind,
    media_urls,
    is_nsfw,
    is_sensitive,
    is_repost,
    repost_source_id,
    content_type,
    tags,
    comment_gating,
    subscriber_only,
    scheduled_at,
    voice_url,
    voice_duration_secs,
    video_master_url,
    video_poster_url,
    video_duration_secs,
    video_width,
    video_height,
    lineage_pial,
    lineage_handle,
    scan_state,
    score_band,
    legacy_post_id,
    expires_at,
    created_at,
    deleted_at
)
SELECT
    p.id,
    'sha256:' || encode(
        digest(('legacy:' || p.id::text)::bytea, 'sha256'),
        'hex'
    ),
    p.author_id,
    COALESCE(u.pial_id, p.id)::uuid,
    COALESCE(p.body, ''),
    COALESCE(p.kind, 'post'),
    COALESCE(p.media_urls, '{}'),
    COALESCE(p.is_nsfw, FALSE),
    COALESCE(p.is_sensitive, FALSE),
    COALESCE(p.is_repost, FALSE),
    p.repost_source_id,
    COALESCE(p.content_type, 'text'),
    COALESCE(p.tags, '{}'),
    COALESCE(p.comment_gating, 'open'),
    COALESCE(p.subscriber_only, FALSE),
    p.scheduled_at,
    p.voice_url,
    p.voice_duration_secs,
    p.video_master_url,
    p.video_poster_url,
    p.video_duration_secs,
    p.video_width,
    p.video_height,
    NULLIF(p.lineage_pial, ''),
    NULLIF(p.lineage_handle, ''),
    -- Use 'clean' for posts that were already published (they passed old gating).
    -- 'human_review' and 'blocked' posts carry their existing state through.
    CASE
        WHEN COALESCE(p.scan_state, 'clean') IN ('human_review', 'blocked') THEN p.scan_state
        ELSE 'clean'
    END,
    COALESCE(p.score_band, 'steady'),
    p.id,
    p.expires_at,
    p.created_at,
    p.deleted_at
FROM posts p
LEFT JOIN users u ON u.id = p.author_id
WHERE NOT EXISTS (
    SELECT 1 FROM works w WHERE w.legacy_post_id = p.id
);
SQL

echo "    done."

# ── Step 2: post_metrics backfill for migrated reposts (already in place) ───
echo "==> Step 2/3: backfilling post_metrics for repost works ..."

psql_exec <<'SQL'
INSERT INTO post_metrics (post_id)
SELECT w.id FROM works w
WHERE w.is_repost = TRUE
  AND NOT EXISTS (SELECT 1 FROM post_metrics pm WHERE pm.post_id = w.id)
ON CONFLICT DO NOTHING;
SQL

echo "    done."

# ── Step 3: reactions → work_reactions ──────────────────────────────────────
echo "==> Step 3/3: migrating reactions (likes / bookmarks / reposts) ..."

psql_exec <<'SQL'
-- post_likes → work_reactions
INSERT INTO work_reactions (work_id, reactor_id, reaction_type, created_at)
SELECT pl.post_id, pl.user_id, 'like', pl.created_at
FROM post_likes pl
WHERE EXISTS (SELECT 1 FROM works w WHERE w.id = pl.post_id)
ON CONFLICT (work_id, reactor_id, reaction_type) DO NOTHING;

-- bookmarks → work_reactions
INSERT INTO work_reactions (work_id, reactor_id, reaction_type, created_at)
SELECT b.post_id, b.user_id, 'bookmark', b.created_at
FROM bookmarks b
WHERE EXISTS (SELECT 1 FROM works w WHERE w.id = b.post_id)
ON CONFLICT (work_id, reactor_id, reaction_type) DO NOTHING;

-- post_reposts → work_reactions
INSERT INTO work_reactions (work_id, reactor_id, reaction_type, created_at)
SELECT pr.post_id, pr.reposter_id, 'repost', pr.created_at
FROM post_reposts pr
WHERE EXISTS (SELECT 1 FROM works w WHERE w.id = pr.post_id)
ON CONFLICT (work_id, reactor_id, reaction_type) DO NOTHING;
SQL

echo "    done."

# ── Final counts ─────────────────────────────────────────────────────────────
echo ""
echo "--- counts after migration ---"
psql_exec -tAc "SELECT 'posts', COUNT(*) FROM posts UNION ALL SELECT 'works', COUNT(*) FROM works UNION ALL SELECT 'work_reactions', COUNT(*) FROM work_reactions;"
echo ""
echo "==> Migration complete. Feed will show all content immediately — no restart needed."
