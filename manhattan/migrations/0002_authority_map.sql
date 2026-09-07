-- 0002_authority_map.sql
--
-- Who owns what, decided once and in one place.
--
-- Until now a brain declared its own ownership on every create: the request
-- carried `owner`, and Manhattan checked it matched the caller. That works only
-- while every brain agrees, and "every brain agrees" is exactly the assumption
-- that produced pial_signing_keys existing in two databases with two comments
-- disagreeing about which one was authoritative.
--
-- So ownership stops being a claim and becomes a lookup. A node's owner is
-- derived from its KIND, here. A caller no longer says who owns what; it says
-- what the thing is, and Manhattan already knows who owns that.
--
-- The second table is the other half of the same idea. Owning a node and being
-- allowed to name it are different rights: registry-brain is the handle
-- authority, but a handle points at an identity that elohim-veni owns. Without
-- namespace authority, either registry-brain cannot bind handles or it has to
-- own identities — and neither is true.

-- ── Which brain owns which kind of node ──────────────────────────────────────
CREATE TABLE node_kind_owners (
    kind       TEXT        PRIMARY KEY,
    owner      TEXT        NOT NULL,
    rationale  TEXT        NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO node_kind_owners (kind, owner, rationale) VALUES
    ('identity', 'elohim-veni',
     'PIAL states, capability grants and the decision log live in f33d3r_security. Identity is that brain''s to assert; every other store of a PIAL is a reference.'),
    ('key',      'elohim-veni',
     'Signing and ECDH keys belong to the identity that holds them. feed-engine''s copies become read-through caches with no write privilege, which settles the contradiction where both claimed to be authoritative.'),
    ('address',  'elohim-veni',
     'A contact address resolves to an identity, so the brain that owns the identity mints and retires its addresses.'),
    ('work',     'feed-engine',
     'works, editions and citations live in f33d3r_feed.'),
    ('facet',    'feed-engine',
     'Facets are rendered by feed-engine.'),
    ('media',    'caeor',
     'Canonical media, derivatives and fingerprints are the media brain''s.');

-- ── Which brain may bind and retire names in which namespace ─────────────────
CREATE TABLE namespace_authorities (
    namespace  TEXT        PRIMARY KEY,
    brain      TEXT        NOT NULL,
    rationale  TEXT        NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO namespace_authorities (namespace, brain, rationale) VALUES
    ('pial',   'elohim-veni',
     'The identity''s own canonical name, minted with the identity.'),
    ('handle', 'registry-brain',
     'A handle is a pointer, not an identity. The registrar owns registration, transfer, auction and reclaim, so it owns the binding — on identity nodes it does not own.'),
    ('uuid',   'feed-engine',
     'Entity ids for works and the rows around them.'),
    ('cid',    'feed-engine',
     'Content addresses for works.'),
    ('addr',   'elohim-veni',
     'Contact addresses, minted alongside the identity they resolve to.');

-- A namespace with no row is unclaimed: any brain may bind in it. That is
-- deliberate — a new namespace should not require a migration before it can be
-- used, only before it becomes contested.
