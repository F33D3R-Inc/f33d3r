-- 0003_work_edges.sql
--
-- Promote work_citations from a loose relationship log to a real edge table.
--
-- What was wrong: target_id had no foreign key (nothing guaranteed it pointed at
-- a work that exists), nothing stopped the same citation being written twice, and
-- a chain had no root and no depth — so nesting could only be computed by walking,
-- and the walk had no bound. The two-level quote render was therefore a bound
-- defended by application code, which is a rule that lives in the wrong place.
--
-- What this makes true, structurally:
--   * target_id is a real reference. A deleted work takes its citations with it.
--   * (work_id, target_id, citation_type) is unique. A double-write is an error,
--     not a duplicate row that renders twice.
--   * every edge carries root_id and depth, materialized by the database itself,
--     so "this thread, bounded to depth 2" is a predicate — WHERE root_id = $1
--     AND depth <= 2 — one round trip, no recursion.
--   * a cycle cannot be inserted. The database can no longer hand back a chain
--     that does not terminate.
--
-- Depth semantics: root_id is the BOTTOM of the chain — the work that cites
-- nothing. depth 1 is an edge that cites the root directly; depth n cites the
-- work at depth n-1. Deeper = further from the root = the more recent citer.

-- ── 1. Repair the data before constraining it ────────────────────────────────
-- Orphans: citations whose target no longer exists. These could never render;
-- they only existed because no foreign key was there to prevent them.
DELETE FROM work_citations wc
 WHERE NOT EXISTS (SELECT 1 FROM works w WHERE w.id = wc.target_id);

-- Self-citations: a work citing itself is a zero-length cycle.
DELETE FROM work_citations WHERE work_id = target_id;

-- Duplicates: keep the earliest row per (work_id, target_id, citation_type).
DELETE FROM work_citations a
 USING work_citations b
 WHERE a.ctid > b.ctid
   AND a.work_id       = b.work_id
   AND a.target_id     = b.target_id
   AND a.citation_type = b.citation_type;

-- Unknown citation types: only 'reply' and 'quote' are rendered by anything.
DELETE FROM work_citations WHERE citation_type NOT IN ('reply', 'quote');

-- ── 2. Constrain it ──────────────────────────────────────────────────────────
ALTER TABLE work_citations
  ADD CONSTRAINT fk_work_citations_target
  FOREIGN KEY (target_id) REFERENCES works(id) ON DELETE CASCADE;

ALTER TABLE work_citations
  ADD CONSTRAINT uq_work_citations_edge
  UNIQUE (work_id, target_id, citation_type);

ALTER TABLE work_citations
  ADD CONSTRAINT ck_work_citations_type
  CHECK (citation_type IN ('reply', 'quote'));

ALTER TABLE work_citations
  ADD CONSTRAINT ck_work_citations_no_self
  CHECK (work_id <> target_id);

-- ── 3. Materialize the chain ─────────────────────────────────────────────────
ALTER TABLE work_citations ADD COLUMN IF NOT EXISTS root_id UUID;
ALTER TABLE work_citations ADD COLUMN IF NOT EXISTS depth   INT;

-- Backfill. Base case: an edge whose target cites nothing of the same type — the
-- target IS the root. Recursive case: an edge whose target is itself a citer
-- inherits that chain's root and sits one level above it.
WITH RECURSIVE chain AS (
    SELECT wc.id,
           wc.work_id,
           wc.citation_type,
           wc.target_id AS root_id,
           1            AS depth
      FROM work_citations wc
     WHERE NOT EXISTS (
           SELECT 1 FROM work_citations p
            WHERE p.work_id       = wc.target_id
              AND p.citation_type = wc.citation_type)
    UNION ALL
    SELECT c.id,
           c.work_id,
           c.citation_type,
           ch.root_id,
           ch.depth + 1
      FROM work_citations c
      JOIN chain ch
        ON ch.work_id       = c.target_id
       AND ch.citation_type = c.citation_type
     WHERE ch.depth < 64
)
UPDATE work_citations wc
   SET root_id = ch.root_id,
       depth   = ch.depth
  FROM chain ch
 WHERE ch.id = wc.id;

-- Anything still unrooted was part of a cycle (no base case can reach it) or sat
-- beyond the depth bound. Both are corrupt structure that predates the constraints
-- added below. Degrade them to flat, top-level citations rather than delete the
-- content: the edge survives, the chain terminates, and the invariant holds.
UPDATE work_citations
   SET root_id = target_id,
       depth   = 1
 WHERE root_id IS NULL OR depth IS NULL;

ALTER TABLE work_citations ALTER COLUMN root_id SET NOT NULL;
ALTER TABLE work_citations ALTER COLUMN depth   SET NOT NULL;

ALTER TABLE work_citations
  ADD CONSTRAINT ck_work_citations_depth
  CHECK (depth >= 1 AND depth <= 64);

-- ── 4. Keep it true on every write ───────────────────────────────────────────
-- root_id and depth are derived, never supplied. The database computes them so
-- that no writer — this service, a backfill script, or a brain that does not
-- exist yet — can produce an edge that disagrees with the chain it belongs to.
CREATE OR REPLACE FUNCTION work_citations_materialize() RETURNS TRIGGER AS $$
DECLARE
    parent_root  UUID;
    parent_depth INT;
BEGIN
    -- A citation into this edge's own ancestry would close a loop. Reject it here,
    -- so no reader ever has to defend against one. The walk is bounded, so this
    -- check cannot itself hang on the cycle it is looking for.
    IF EXISTS (
        WITH RECURSIVE ancestry AS (
            SELECT NEW.target_id AS node, 0 AS d
            UNION ALL
            SELECT p.target_id, a.d + 1
              FROM work_citations p
              JOIN ancestry a ON p.work_id = a.node
             WHERE p.citation_type = NEW.citation_type
               AND a.d < 64
        )
        SELECT 1 FROM ancestry WHERE node = NEW.work_id
    ) THEN
        RAISE EXCEPTION 'work_citations: % -> % would close a % cycle',
            NEW.work_id, NEW.target_id, NEW.citation_type;
    END IF;

    SELECT p.root_id, p.depth
      INTO parent_root, parent_depth
      FROM work_citations p
     WHERE p.work_id       = NEW.target_id
       AND p.citation_type = NEW.citation_type
     ORDER BY p.depth ASC
     LIMIT 1;

    IF parent_root IS NULL THEN
        NEW.root_id := NEW.target_id;
        NEW.depth   := 1;
    ELSE
        NEW.root_id := parent_root;
        NEW.depth   := parent_depth + 1;
    END IF;

    IF NEW.depth > 64 THEN
        RAISE EXCEPTION 'work_citations: % chain from root % exceeds max depth 64',
            NEW.citation_type, NEW.root_id;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_work_citations_materialize ON work_citations;
CREATE TRIGGER trg_work_citations_materialize
    BEFORE INSERT OR UPDATE OF work_id, target_id, citation_type
    ON work_citations
    FOR EACH ROW
    EXECUTE FUNCTION work_citations_materialize();

-- ── 5. Index for the queries this table now serves ───────────────────────────
-- (root_id, depth) is the bounded-thread read. The other two are the per-work
-- fan-in and fan-out that the render and the derived counters walk.
CREATE INDEX IF NOT EXISTS idx_work_citations_root        ON work_citations(root_id, depth);
CREATE INDEX IF NOT EXISTS idx_work_citations_target_type ON work_citations(target_id, citation_type);
CREATE INDEX IF NOT EXISTS idx_work_citations_work_type   ON work_citations(work_id, citation_type);
