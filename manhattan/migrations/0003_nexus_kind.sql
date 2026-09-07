-- 0003_nexus_kind.sql
--
-- A verified natural person is a distinct kind of thing, and verity owns it.
--
-- NEXUS is the layer above identity: one verified human may hold several PIALs
-- (a personal account, a creator account, a business account). Until now the
-- only kinds were identity/work/media/key/address/facet, so verity had nothing
-- it owned — and since an edge may only be written by the brain that owns its
-- subject, verity could not record the one relationship it exists to record:
-- which identities belong to the same person.
--
-- The edge is oriented nexus -> identity ('contains') rather than the reverse
-- precisely because of that rule. The nexus row is verity's; the identity is
-- elohim-veni's. A brain writes facts about what it owns, and "this person
-- holds this identity" is verity's fact to assert.
--
-- Note what this deliberately does NOT do: it does not make a nexus another
-- name for an identity. Conflating the person with the account is how a
-- multi-persona platform leaks one persona into another, which is the single
-- worst failure this product can have.

ALTER TABLE nodes DROP CONSTRAINT IF EXISTS nodes_kind_check;
ALTER TABLE nodes ADD CONSTRAINT nodes_kind_check
    CHECK (kind IN ('identity','work','media','key','address','facet','nexus'));

INSERT INTO node_kind_owners (kind, owner, rationale) VALUES
    ('nexus', 'verity',
     'A verified natural person holding one or more PIALs. Verity performs the verification, so verity asserts which identities belong to the same person. Distinct from an identity: conflating the person with the account defeats the whole point of separable personas.')
ON CONFLICT (kind) DO NOTHING;
