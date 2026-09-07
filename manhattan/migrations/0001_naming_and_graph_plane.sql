-- 0001_naming_and_graph_plane.sql
--
-- Manhattan is the ONE resolver. Three tables, one plane:
--
--   nodes  — the identity of a thing, and the single brain allowed to write facts about it.
--   names  — every public, stable string that resolves to a node.
--   edges  — every relationship, typed, with its root and depth materialised on write.
--
-- The rule this schema exists to enforce: no brain may reference another brain's rows;
-- it may only reference a NAME and resolve that name here.
--
-- FORWARD-ONLY: never edit this file after it has been applied anywhere. Its checksum is
-- recorded in schema_migrations and a drift is a hard boot failure, not a warning.

CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS citext;

-- ── nodes ─────────────────────────────────────────────────────────────────────
-- A node has no payload. It is an identity and an ownership boundary, nothing more.
-- owner is the ONE brain permitted to write facts about this node; it is enforced at
-- the wire (X-F33D3R-Brain), not by convention.
CREATE TABLE nodes (
    node_id    UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    kind       TEXT        NOT NULL
               CHECK (kind IN ('identity','work','media','key','address','facet')),
    owner      TEXT        NOT NULL,
    status     TEXT        NOT NULL DEFAULT 'active'
               CHECK (status IN ('active','revoked','tombstoned')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_nodes_kind_status ON nodes (kind, status);
CREATE INDEX idx_nodes_owner       ON nodes (owner);

-- ── names ─────────────────────────────────────────────────────────────────────
-- Globally unique across every namespace, stored as '<namespace>:<value>'.
-- CITEXT so resolution is case-insensitive without the caller normalising anything.
-- A revoked name never resolves and is never reused: the row stays forever, so a
-- rotated address can never be re-minted to someone else.
CREATE TABLE names (
    name       CITEXT      PRIMARY KEY,
    node_id    UUID        NOT NULL REFERENCES nodes(node_id) ON DELETE CASCADE,
    namespace  TEXT        NOT NULL,
    is_primary BOOLEAN     NOT NULL DEFAULT FALSE,
    status     TEXT        NOT NULL DEFAULT 'active'
               CHECK (status IN ('active','revoked')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at TIMESTAMPTZ
);
CREATE INDEX idx_names_node            ON names (node_id);
CREATE INDEX idx_names_namespace_status ON names (namespace, status);
-- At most one primary name per node — structural, not defended by application code.
CREATE UNIQUE INDEX idx_names_one_primary ON names (node_id) WHERE is_primary;

-- ── edges ─────────────────────────────────────────────────────────────────────
-- root and depth are the whole point: "this thread bounded to depth 2" is
-- WHERE root = $1 AND depth <= 2 — one round trip, no recursion, and the database
-- cannot hand back a cycle. Both are materialised inside the inserting transaction.
CREATE TABLE edges (
    subject    UUID        NOT NULL REFERENCES nodes(node_id) ON DELETE CASCADE,
    predicate  TEXT        NOT NULL
               CHECK (predicate IN ('quotes','replies_to','authored_by','derived_from','resolves_to','signs_for','contains')),
    object     UUID        NOT NULL REFERENCES nodes(node_id) ON DELETE CASCADE,
    root       UUID        NOT NULL REFERENCES nodes(node_id),
    depth      INT         NOT NULL CHECK (depth >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (subject, predicate, object),
    CHECK (subject <> object)
);
CREATE INDEX idx_edges_object_predicate ON edges (object, predicate);
CREATE INDEX idx_edges_root_depth       ON edges (root, depth);
CREATE INDEX idx_edges_subject          ON edges (subject);
