-- 0037_tracks_become_works.sql
--
-- Music tracks are works: the same signed write path, the same scan, fan-out,
-- walls and media authority, a different Facet. Every row of tracks is carried
-- into works here, its likes into work_reactions and its play count into
-- work_play_totals; then every work that still names legacy media paths is
-- queued for media_bind, which re-binds it to its Caeor assets.
--
-- The tracks tables are NOT dropped here. Nothing reads or writes them after
-- this release; the staged migration
-- internal/db/migrations_staged/0040_retire_feed_engine_media_registry.sql
-- drops them once it has verified that every track arrived.

-- ── 1. tracks → works ────────────────────────────────────────────────────────
-- The work keeps the track's id, so Zior's signal (stored under the track id)
-- and every link to the track keep naming the same thing.
--
-- cid: a track was never signed, so it has no content address. 'legacy-track:'
-- marks the row as carried over rather than signed; it can never collide with
-- a signed work's 'sha256:' address and is never offered for verification.
--
-- body: the title, then a blank line and the description when there is one.
-- The track Facet shows the first line as the title.
-- tags: the genre (as one word, the tag vocabulary has no spaces), then the
-- track's own tags.
-- scan_state 'clean': the track has been served publicly as it is; carrying it
-- across is not new content.
-- A track whose author has no PIAL has no identity root to author a work; it is
-- left in tracks, and the staged 0040 refuses to drop the table while any such row remains,
-- so it is decided by a person rather than dropped by a migration.
INSERT INTO works (
    id, cid, author_id, author_pial, body, kind, media_urls, tags,
    voice_url, voice_duration_secs, psych_vector, scan_state, comment_gating,
    created_at
)
SELECT t.id,
       'legacy-track:' || t.id::text,
       t.author_id,
       u.pial_id,
       btrim(t.title || CASE WHEN btrim(t.description) <> '' THEN E'\n\n' || btrim(t.description) ELSE '' END),
       'track',
       CASE WHEN t.cover_url <> '' THEN ARRAY[t.cover_url] ELSE '{}'::text[] END,
       CASE WHEN btrim(t.genre) <> '' THEN ARRAY[regexp_replace(btrim(t.genre), '\s+', '', 'g')] ELSE '{}'::text[] END
         || COALESCE(t.tags, '{}'::text[]),
       t.audio_url,
       NULLIF(t.duration_secs, 0)::REAL,
       t.psych_vector,
       'clean',
       'everyone',
       t.created_at
  FROM tracks t
  JOIN users u ON u.id = t.author_id
 WHERE u.pial_id IS NOT NULL
ON CONFLICT (id) DO NOTHING;

-- ── 2. track likes → work reactions ──────────────────────────────────────────
INSERT INTO work_reactions (work_id, reactor_id, reaction_type, created_at)
SELECT l.track_id, l.user_id, 'like', l.created_at
  FROM track_likes l
  JOIN works w ON w.id = l.track_id AND w.kind = 'track'
ON CONFLICT (work_id, reactor_id, reaction_type) DO NOTHING;

-- ── 3. play counts ───────────────────────────────────────────────────────────
INSERT INTO work_play_totals (work_id, plays)
SELECT t.id, t.play_count
  FROM tracks t
  JOIN works w ON w.id = t.id AND w.kind = 'track'
 WHERE t.play_count > 0
ON CONFLICT (work_id) DO UPDATE SET plays = GREATEST(work_play_totals.plays, EXCLUDED.plays);

-- ── 4. queue media_bind for every work that names legacy media ───────────────
-- A work created by this release is bound in its own create transaction (it
-- has work_assets rows). Everything else — every legacy work and every track
-- carried over above — is bound by its media_bind obligation. GIFs from the picker are external https URLs and need no binding.
INSERT INTO work_outbox (work_id, action, dedup_key)
SELECT w.id, 'media_bind', 'media_bind:' || w.id::text
  FROM works w
 WHERE w.deleted_at IS NULL
   AND NOT EXISTS (SELECT 1 FROM work_assets wa WHERE wa.work_id = w.id)
   AND (   EXISTS (SELECT 1 FROM unnest(w.media_urls) m(url) WHERE m.url LIKE '/%')
        OR COALESCE(w.voice_url, '')        LIKE '/%'
        OR COALESCE(w.video_master_url, '') LIKE '/%'
        OR COALESCE(w.video_poster_url, '') LIKE '/%')
ON CONFLICT (dedup_key) DO NOTHING;
