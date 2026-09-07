-- 0005_rebindable_names.sql
--
-- Fixes a bug that made handle transfer impossible.
--
-- `names.name` was the PRIMARY KEY, and revoking a name kept the row forever.
-- Those two facts together mean a revoked name can never be bound again: the
-- insert hits the primary key and returns 409. So the transfer flow the
-- registrar must perform — revoke handle:bob from A, bind handle:bob to B —
-- could not complete. The handle stayed revoked and B never got it.
--
-- The original "a revoked name is never reused" rule was right for one kind of
-- name and wrong for another, and the table could not tell them apart:
--
--   A HANDLE IS MEANT TO MOVE. Registration, transfer, auction and reclaim are
--   the registrar's whole purpose. Refusing to rebind a released handle does not
--   protect anyone; it just breaks the product.
--
--   A CONTACT ADDRESS MUST NEVER COME BACK. Rotating an address is a security
--   act: someone published it, retired it, and must be certain it can never be
--   re-minted to a stranger who then receives mail meant for them. Here the
--   original rule is exactly right and is preserved.
--
-- So reusability becomes a property of the namespace, and the constraint becomes
-- "at most one ACTIVE binding per name" rather than "one row per name ever".
-- History is kept either way: revoked rows are never deleted, so who held a name
-- and when is always answerable.

-- ── names: surrogate key, uniqueness on the ACTIVE binding ───────────────────
ALTER TABLE names ADD COLUMN IF NOT EXISTS name_id UUID NOT NULL DEFAULT gen_random_uuid();

ALTER TABLE names DROP CONSTRAINT IF EXISTS names_pkey;
ALTER TABLE names ADD CONSTRAINT names_pkey PRIMARY KEY (name_id);

-- The real rule: one live binding per name. A name may have many revoked rows
-- behind it (its history) and at most one active row (its current holder).
CREATE UNIQUE INDEX IF NOT EXISTS idx_names_active_unique
    ON names (name) WHERE status = 'active';

-- Resolution and history lookups both walk by name.
CREATE INDEX IF NOT EXISTS idx_names_name_status ON names (name, status);

-- ── which namespaces may be rebound after revocation ─────────────────────────
ALTER TABLE namespace_authorities
    ADD COLUMN IF NOT EXISTS reusable BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN namespace_authorities.reusable IS
    'TRUE when a revoked name may be bound again to a different node (a handle is meant to move). FALSE when revocation is permanent (a rotated contact address must never be re-minted to a stranger).';

UPDATE namespace_authorities SET reusable = TRUE  WHERE namespace = 'handle';
UPDATE namespace_authorities SET reusable = FALSE WHERE namespace IN ('addr', 'pial', 'uuid', 'cid');
