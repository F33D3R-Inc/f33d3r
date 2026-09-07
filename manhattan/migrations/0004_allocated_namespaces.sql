-- 0004_allocated_namespaces.sql
--
-- Closes a hole: POST /v1/names enforced namespace authority, but POST /v1/nodes
-- bound a primary name without consulting it. So any brain could mint a node
-- whose primary name was `handle:bob` and bypass the registrar entirely — the
-- exact authority the handle namespace exists to hold.
--
-- The naive fix (enforce authority at create too) breaks something we need:
-- registration must work whoever encounters an entity first, or a work cannot be
-- registered until the identity brain happens to have seen its author.
--
-- The distinction that resolves it is what KIND of name it is:
--
--   DERIVED names are computed from the entity itself. `pial:<uuid>`,
--   `uuid:<id>`, `cid:sha256:<hex>` — anyone can compute them, two brains
--   computing the same one always agree, and there is no decision to make.
--   Any brain may bind these, which is what makes bootstrap work.
--
--   ALLOCATED names are decisions about who gets a scarce string. `handle:bob`
--   is granted to someone, and could have been granted to someone else. A
--   contact address is minted. These are exactly the names where "whoever gets
--   there first" is the wrong rule, so only the governing brain may bind them.
--
-- Put plainly: a brain may always say "this thing exists and here is its
-- computed name". It may never say "this handle is now mine to hand out".

ALTER TABLE namespace_authorities
    ADD COLUMN IF NOT EXISTS allocated BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN namespace_authorities.allocated IS
    'TRUE when a name in this namespace is granted rather than computed. Allocated names may be bound only by the governing brain; derived names may be bound by whichever brain encounters the entity first.';

UPDATE namespace_authorities SET allocated = TRUE  WHERE namespace IN ('handle', 'addr');
UPDATE namespace_authorities SET allocated = FALSE WHERE namespace IN ('pial', 'uuid', 'cid');
