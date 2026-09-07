-- 0007_association_plane.sql
--
-- The association plane: relationships that have no lineage.
--
-- Manhattan already has `edges`, and `edges` is not this. An edge is a rooted
-- lineage DAG — `root` and `depth` are NOT NULL and materialised on write —
-- because the questions it answers are "what thread is this in" and "how deep".
-- A reply has a root. A signature has a root. A quote has a root.
--
-- A follow has no root and no depth. There is no chain it belongs to, nothing it
-- descends from, and no ceiling it could exceed. Forcing one into `edges` means
-- inventing a root and a depth that mean nothing — a lie materialised in two
-- NOT NULL columns, which every future reader of `idx_edges_root_depth` would
-- then have to know to ignore. That is drift, so this is a second table with the
-- shape the fact actually has:
--
--     unrooted    — no placement, so no advisory lock and no ordering hazard on
--                   write. Two people following each other concurrently is two
--                   independent inserts, not a race over a shared parent.
--     typed       — `assoc` says what the relationship IS, and which brain may
--                   assert it.
--     time-ordered— (subject, assoc, created_at DESC) is the index a "most
--                   recent N" read walks. TAO calls that assoc_range.
--     countable   — the same index answers COUNT(*) for one subject without
--                   touching a row of another. TAO calls that assoc_count, and
--                   the reason it is a graph operation there is that a count
--                   maintained by application code drifts. feed-engine learned
--                   the same lesson in migration 0016 and moved its follower
--                   counters onto a trigger.
--
-- WHY THIS EXISTS AT ALL — the first customer.
--
-- elohim-veni decides who may contact whom. Two of its contact policies,
-- `followers` and `mutuals`, are decisions about the follow graph, and the
-- follow graph lives in feed-engine. Until now elohim-veni took feed-engine's
-- word for it: the evaluate call carried `"follows": true` and `"mutual": true`
-- as plain fields on the wire, and elohim-veni believed them. A security
-- decision resting on an unverifiable assertion from another brain is exactly
-- what Manhattan exists to end.
--
-- Worse, it did not even work. On the Number path elohim-veni will not disclose
-- who a Number belongs to before it has decided, so feed-engine cannot compute
-- the follow facts, so it sent nothing, so `followers` and `mutuals` silently
-- degraded to "open a contact request" for people who genuinely were mutual
-- follows. The privacy property and the feature were in direct conflict, and the
-- feature lost. With the graph readable by the brain that takes the decision,
-- both hold: elohim-veni knows the target (it always did) and now looks the
-- relationship up itself, while feed-engine still never learns who the Number
-- belongs to.
--
-- WHAT THIS IS NOT. This is not a migration of the follow graph. feed-engine
-- remains the system of record: `follows` is traversed by feed ranking, by
-- suggestion, by the fleet lane and by AethyrRank, and a network hop per
-- traversal is how a 20ms feed becomes a 400ms feed. TAO could take that
-- traffic only because it was fronted by an enormous read-through cache, and
-- there is no such cache here. What lands in this table is the PUBLISHED
-- projection of that graph — one writer, delivered transactionally through
-- feed-engine's existing outbox — so that a second brain can verify a
-- relationship instead of being told about one. If the graph tier is ever worth
-- building, this table is already the right shape and the migration becomes a
-- change of writer, not a change of schema.
--
-- FORWARD-ONLY: never edit this file after it has been applied anywhere. Its
-- checksum is recorded in schema_migrations and a drift is a hard boot failure.

-- ── who may assert which association ──────────────────────────────────────────
-- The same idea as node_kind_owners and namespace_authorities (migration 0002),
-- applied to relationships. It has to be separate from node ownership, because
-- an association's two endpoints are owned by a brain that is not the one making
-- the assertion: identity nodes belong to elohim-veni, but who follows whom is
-- feed-engine's fact about a pair of them. Node ownership governs facts ABOUT a
-- node; an association is a fact about a RELATIONSHIP, and its type carries its
-- own authority.
--
-- An association type with no row here does not exist. Unlike namespaces, which
-- default to unclaimed so a new one needs no migration, an association is a
-- claim one brain makes about two identities it does not own — "whoever gets
-- there first" is the wrong rule for that, always.
CREATE TABLE assoc_authorities (
    assoc      TEXT        PRIMARY KEY,
    brain      TEXT        NOT NULL,
    rationale  TEXT        NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO assoc_authorities (assoc, brain, rationale) VALUES
    ('follows', 'feed-engine',
     'The follows table lives in f33d3r_feed and is traversed by ranking, suggestion and the fleet lane. feed-engine stays its system of record and publishes it here; this plane is what lets elohim-veni verify a follow instead of being told about one.');

-- ── assocs ────────────────────────────────────────────────────────────────────
-- No root. No depth. No placement rule. An association is true or it is absent.
CREATE TABLE assocs (
    subject    UUID        NOT NULL REFERENCES nodes(node_id) ON DELETE CASCADE,
    assoc      TEXT        NOT NULL REFERENCES assoc_authorities(assoc),
    object     UUID        NOT NULL REFERENCES nodes(node_id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (subject, assoc, object),
    CHECK (subject <> object)
);

-- The forward read: "the N most recent things this subject associates with",
-- and COUNT(*) over the same prefix. One index serves assoc_range and
-- assoc_count both.
CREATE INDEX idx_assocs_subject_time ON assocs (subject, assoc, created_at DESC);
-- The inverse read: followers rather than following. Symmetric by construction,
-- so neither direction is the slow one — which is the property a graph plane has
-- to have and a foreign key on somebody else's table does not.
CREATE INDEX idx_assocs_object_time  ON assocs (object,  assoc, created_at DESC);

COMMENT ON TABLE assocs IS
    'Unrooted typed associations between nodes. Deliberately NOT the edges table: an edge carries a materialised root and depth because it belongs to a lineage; an association has neither and inventing them would put a lie in two NOT NULL columns.';
