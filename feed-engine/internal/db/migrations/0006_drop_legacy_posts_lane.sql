-- 0006_drop_legacy_posts_lane.sql
--
-- Remove the posts lane. 0001 left it standing on purpose and said so: "posts
-- table is NOT dropped here — handlers still reference it. Cutover (DROP TABLE
-- posts) happens in a later phase once all queries migrate." This is that phase.
--
-- What was wrong: the queries migrated, the tables did not, and the two facts
-- never met. Roughly fifty call sites went on reading and writing posts,
-- post_metrics, post_likes, post_reposts, user_dislikes, poll_votes,
-- post_cashtags and post_link_previews after works became the only lane a render
-- consults. Those tables were empty, so every one of those statements succeeded
-- against nothing: a like INSERTed a row nobody counted, a bookmark toggled a
-- table no saved-items query reads, a moderation decision probed posts, missed,
-- and fell through. Nothing errored. Nothing logged. The endpoints answered 200
-- and the screen did not change, which is the one failure mode that cannot be
-- found by watching for failures. An empty table is more dangerous than a missing
-- one precisely because it answers.
--
-- What this makes true, structurally:
--   * There is one content table. A query against posts is now a syntax error at
--     the database, caught the first time it runs, instead of a successful query
--     returning zero rows forever.
--   * There is one reaction table. work_reactions holds like, repost, bookmark
--     and dislike as rows, counted at read time in worksSelectSQL. post_likes,
--     user_dislikes, post_reposts and the post_metrics counters they maintained
--     can no longer disagree with it, because they are gone.
--   * There is one ballot table. work_poll_votes is keyed to works(id); the
--     legacy poll_votes was keyed to posts(id) and could not physically hold a
--     ballot for a work, so poll voting wrote into a table with no reachable rows.
--   * content_receipts references the row it is a receipt for. Its post_id was
--     foreign-keyed to posts while the only caller passed a work id, so every
--     mint since the cutover violated that key and failed. Repointing it at
--     works(id) is what makes the receipt lane work at all.
--
-- Derived-not-stored, continued from 0004: nothing dropped here had a reader that
-- is losing a number. Every count the card shows is counted from rows — reactions
-- from work_reactions, replies and quotes from work_citations, views from
-- works.view_count. post_metrics was the last stored-counter table and it had no
-- reader left at all.

-- ── 1. Refuse to destroy content ─────────────────────────────────────────────
-- These tables are empty on the machine this was written against. They may not be
-- on another. A migration that quietly drops rows because they happened to be
-- absent for its author is the same class of failure this file exists to remove,
-- so the assertion comes first and it is fatal, not advisory. Forward-only means
-- there is no undo: if this raises, the database is telling you it still holds
-- posts-lane content, and that content must be moved into works by hand before
-- the cutover can proceed.
--
-- Tables are probed through to_regclass so a database that never had one (the
-- machine-local post_rank_signals relic, which no migration ever created) is not
-- itself an error. Only rows are an error.
DO $do$
DECLARE
    legacy_table TEXT;
    row_count    BIGINT;
    offenders    TEXT := '';
BEGIN
    FOREACH legacy_table IN ARRAY ARRAY[
        'posts',
        'post_metrics',
        'post_likes',
        'post_reposts',
        'user_dislikes',
        'poll_votes',
        'post_cashtags',
        'post_link_previews',
        'post_rank_signals'
    ] LOOP
        IF to_regclass('public.' || legacy_table) IS NULL THEN
            CONTINUE;
        END IF;
        EXECUTE format('SELECT COUNT(*) FROM public.%I', legacy_table) INTO row_count;
        IF row_count > 0 THEN
            offenders := offenders || format('%s=%s ', legacy_table, row_count);
        END IF;
    END LOOP;

    IF offenders <> '' THEN
        RAISE EXCEPTION
            'refusing to drop the legacy posts lane: still holding rows -> %',
            offenders
        USING HINT =
            'Migrate this content into works (and work_reactions / work_poll_votes / '
            'work_citations) before applying 0006. This migration destroys tables and '
            'is forward-only: there is no down path that gives the rows back.';
    END IF;
END
$do$;

-- ── 2. Detach the live tables that still point at posts ──────────────────────
-- content_receipts is not legacy — it is the PIAL provenance record for authored
-- content — but its post_id was declared REFERENCES posts(id) ON DELETE SET NULL,
-- and MintContentReceipt has passed a works id into it since the work event
-- became the create path. Every mint therefore failed the key. The column is
-- renamed to what it has always actually held and constrained to the table that
-- holds it.
--
-- Step 1 above proved posts is empty. A non-null post_id could only exist if it
-- matched a posts row, so after that assertion every value in this column is
-- NULL and the rename cannot strand a reference. The check below is not
-- redundant defence-in-depth for its own sake — it is the statement that would
-- fire if that reasoning were ever wrong, rather than a constraint violation
-- three lines later with nothing explaining it.
DO $do$
DECLARE
    stranded BIGINT;
BEGIN
    IF to_regclass('public.content_receipts') IS NULL THEN
        RETURN;
    END IF;
    SELECT COUNT(*) INTO stranded
      FROM content_receipts cr
     WHERE cr.post_id IS NOT NULL
       AND NOT EXISTS (SELECT 1 FROM works w WHERE w.id = cr.post_id);
    IF stranded > 0 THEN
        RAISE EXCEPTION
            'content_receipts holds % row(s) whose post_id is not a work', stranded
        USING HINT =
            'These receipts reference posts-lane content. Resolve them before 0006.';
    END IF;
END
$do$;

ALTER TABLE content_receipts DROP CONSTRAINT IF EXISTS content_receipts_post_id_fkey;
ALTER TABLE content_receipts RENAME COLUMN post_id TO work_id;
ALTER TABLE content_receipts
  ADD CONSTRAINT fk_content_receipts_work
  FOREIGN KEY (work_id) REFERENCES works(id) ON DELETE SET NULL;
ALTER INDEX IF EXISTS idx_receipts_post RENAME TO idx_receipts_work;

-- bookmarks keeps its rows and its shape. It is not part of the legacy set being
-- dropped here, so only the dependency is removed — the constraint that named
-- posts, which cannot survive a table that does not exist.
--
-- Flagging it rather than quietly leaving it: as of this migration the bookmarks
-- table has no reader and no writer in feed-engine. Saved items are
-- work_reactions rows with reaction_type='bookmark' (GetWorksSaves), and the
-- AddBookmark/RemoveBookmark/IsBookmarked helpers that were the table's only
-- callers were deleted in this cutover because the routes reaching them had no
-- markup pointing at them. Two tables that both mean "saved" is duplicate state
-- ownership; one of them should go. Which one is a decision, and a decision that
-- destroys a table is not one a cleanup migration should make on its own. It
-- belongs in 0007, deliberately.
ALTER TABLE bookmarks DROP CONSTRAINT IF EXISTS bookmarks_post_id_fkey;

-- user_profiles.pinned_post_id is superseded outright: 0002 added pinned_work_id
-- and PinPost/UnpinPost/GetPinnedPostID have written and read only that one since.
-- The old column has no reader, holds no non-null value (its foreign key made
-- that impossible once posts emptied), and names a table that is about to stop
-- existing. It is dropped rather than detached, because unlike bookmarks there is
-- nothing here to decide.
ALTER TABLE user_profiles DROP COLUMN IF EXISTS pinned_post_id;

-- ── 3. Drop the lane ─────────────────────────────────────────────────────────
-- Children first, then the parent, so ordinary foreign keys carry the drops in
-- the order they were declared in.
--
-- Deliberately no CASCADE. CASCADE would take with it anything that still
-- depends on posts and never say what — which, in a file whose entire subject is
-- silent success, would be the wrong ending. If some object not accounted for
-- above still references this lane, this fails loudly with its name and the
-- cutover stops until that object is understood.
DROP TABLE IF EXISTS post_metrics;
DROP TABLE IF EXISTS post_likes;
DROP TABLE IF EXISTS post_reposts;
DROP TABLE IF EXISTS user_dislikes;
DROP TABLE IF EXISTS poll_votes;
DROP TABLE IF EXISTS post_cashtags;
DROP TABLE IF EXISTS post_link_previews;

-- Never created by any migration; present only on databases old enough to predate
-- the versioned plane, where it arrived with the boot-time DDL blob and was never
-- read by anything. IF EXISTS is doing real work here, not defensive padding.
DROP TABLE IF EXISTS post_rank_signals;

-- posts' remaining foreign keys are its own three self-references (parent_id,
-- quoted_post_id, repost_source_id), which go with the table.
DROP TABLE IF EXISTS posts;

-- ── 4. works.legacy_post_id stays ────────────────────────────────────────────
-- It is kept, and it is kept on purpose rather than by omission.
--
-- It was never a foreign key — just a UUID column recording which posts row a
-- work was migrated from — so nothing about it dangles now that posts is gone. It
-- points at history, not at a table, and history does not stop having happened
-- when its storage is dropped.
--
-- It is also still load-bearing. GET /post/{id} is the pre-cutover URL shape, and
-- every link to a post that was ever shared, indexed or bookmarked outside this
-- system still has that shape. legacyPostRedirect resolves those by
-- legacy_post_id and answers 301 to /work/{id}. Drop the column and the lookup
-- misses, the fallback treats the post id as a work id, and every inbound legacy
-- link lands on a work that does not exist. The column costs one partial index
-- and buys the permanence of every URL the site has already published.
--
-- Its second job — making the original backfill idempotent, so a re-run matched
-- rather than duplicated — is finished. Keeping it for the first job is enough.
