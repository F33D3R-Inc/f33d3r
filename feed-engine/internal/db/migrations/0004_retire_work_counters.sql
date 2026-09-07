-- 0004_retire_work_counters.sql
--
-- works.reply_count and works.quote_count were denormalized counters with no
-- single owner. reply_count was incremented by a handler after a successful
-- citation insert — a second write that could fail independently of the first,
-- so the number drifted from the rows it claimed to count. quote_count was never
-- incremented by anything at all: every work reported zero quotes forever, and
-- the "N Quotes" line on the work detail Facet could never appear.
--
-- work_citations (0003) is row truth. These numbers are derived from it or they
-- do not exist. There is no third option where a cached copy is allowed to be
-- wrong, because a cached copy that is allowed to be wrong is what produced this.
--
-- The reaction tallies on a work card have always been derived this way — like,
-- repost, bookmark and dislike are counted from work_reactions at read time in
-- the shared work SELECT. These two columns were the only ones that were not.
-- After this migration the whole card counts the same way.
--
-- Reads move to a correlated count over idx_work_citations_target_type, which
-- 0003 created for exactly this.

ALTER TABLE works DROP COLUMN IF EXISTS reply_count;
ALTER TABLE works DROP COLUMN IF EXISTS quote_count;
