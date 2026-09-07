-- 0006_number_namespace.sql
--
-- F33D3R Numbers: an allocated, never-reusable name that resolves to an identity.
-- Rotation mints a new number node and revokes the old name; the identity node,
-- its keys and its open conversations are never touched.

ALTER TABLE nodes DROP CONSTRAINT IF EXISTS nodes_kind_check;
ALTER TABLE nodes ADD CONSTRAINT nodes_kind_check
    CHECK (kind IN ('identity','work','media','key','address','facet','nexus','number'));

INSERT INTO node_kind_owners (kind, owner, rationale) VALUES
    ('number', 'elohim-veni',
     'A Number resolves to an identity and carries a contact policy, so the brain that owns the identity mints, rotates and retires it.')
ON CONFLICT (kind) DO NOTHING;

-- allocated: a Number is granted, never computed, so only the governing brain may bind it.
-- reusable FALSE: a retired Number must never reach a stranger who then receives contact
-- meant for its previous holder — the same rule the addr namespace carries.
INSERT INTO namespace_authorities (namespace, brain, rationale, allocated, reusable) VALUES
    ('number', 'elohim-veni',
     'Numbers are allocated to identities by the identity authority.', TRUE, FALSE)
ON CONFLICT (namespace) DO UPDATE
    SET brain     = EXCLUDED.brain,
        allocated = EXCLUDED.allocated,
        reusable  = EXCLUDED.reusable;
