-- 0008_number_quarantine_and_cap.sql
--
-- Two rules the address space now depends on, both belonging to the ALLOCATOR.
--
-- ── Why now ──────────────────────────────────────────────────────────────────
--
-- A F33D3R Number used to be twelve Crockford base32 symbols: 2^60 addresses,
-- so wide that neither exhausting it nor guessing into it was worth thinking
-- about. It is now twelve decimal digits — eleven payload and a Luhn check —
-- which is 10^11 ≈ 2^36.5. That is about 11.5 million times smaller, and it
-- turns two things that were previously academic into load-bearing rules:
--
--   * A retired Number is address space held out of circulation for ever.
--   * An identity holding unbounded Numbers is an identity consuming unbounded
--     address space.
--
-- Both rules live here, in the plane that allocates names, and not in the brain
-- that asks for them. A cap enforced by the asker is a cap enforced by nobody.
--
-- ── Rule one: quarantined reuse ──────────────────────────────────────────────
--
-- Migration 0006 made the `number` namespace permanently non-reusable, with the
-- rationale that "a retired Number must never reach a stranger who then receives
-- contact meant for its previous holder." That rationale is still exactly right
-- for the DAY a Number is retired, and it is no longer right for ever: cards get
-- thrown away, slides get taken down, and holding every Number ever minted out of
-- a 10^11 space permanently is the address space's largest single leak.
--
-- So reuse becomes time-gated, and the namespace table gains the ability to say
-- so. THREE STATES, expressed as two columns that each answer exactly one
-- question:
--
--   reusable = FALSE                       — never. (addr, pial, uuid, cid)
--   reusable = TRUE,  quarantine IS NULL   — immediately. (handle: a handle is
--                                            meant to move, and the registrar's
--                                            whole purpose is moving it.)
--   reusable = TRUE,  quarantine = 12mo    — after the hold. (number)
--
-- A quarantine on a namespace that never reissues is a contradiction, so the
-- CHECK below makes that state unrepresentable rather than merely unlikely.
--
-- Twelve months is chosen against the thing that actually decays: the printed
-- artefact. A conference badge, a business card, a flyer and a slide deck are
-- all out of circulation within a year, and a Number that is still being typed
-- from one after twelve months is being typed by somebody who did not get the
-- message that it was retired — which is exactly what the NEW holder's own
-- contact policy is there to refuse.
--
-- What the hold does NOT do is transfer anything. A reminted Number is a NEW
-- node with a new node_id: the previous holder's policy row, label, lease,
-- admission ledger and pending contact requests are all keyed to the old node
-- and are unreachable from the new one. Conversations are anchored to
-- identities, never to Numbers, so a conversation opened through the old Number
-- keeps working for the people in it and is invisible to the new holder.
--
-- ── Rule two: a cap belongs where allocation happens ─────────────────────────
--
-- The concurrent cap and the minting rate that go with this live in
-- manhattan/src/main.rs, at the one endpoint that allocates a Number, for the
-- same reason this quarantine lives in `may_bind_name`: they are properties of
-- the address space, and the address space is this plane's to protect.

-- ── quarantine_seconds ───────────────────────────────────────────────────────
-- Seconds rather than an INTERVAL: every other duration that crosses this stack
-- is a count of seconds, and one representation is worth more than the nicer
-- type. NULL means "no hold", which is only meaningful when `reusable` is TRUE.
ALTER TABLE namespace_authorities
    ADD COLUMN IF NOT EXISTS quarantine_seconds BIGINT;

ALTER TABLE namespace_authorities
    DROP CONSTRAINT IF EXISTS namespace_authorities_quarantine_shape;
ALTER TABLE namespace_authorities
    ADD CONSTRAINT namespace_authorities_quarantine_shape
    CHECK (quarantine_seconds IS NULL OR (reusable AND quarantine_seconds > 0));

COMMENT ON COLUMN namespace_authorities.quarantine_seconds IS
    'How long a revoked name in this namespace must sit before it may be bound again. NULL with reusable=TRUE means immediately; NULL with reusable=FALSE means never. Measured against the database''s own NOW(), the same clock that stamped revoked_at, so there is one clock and no skew.';

COMMENT ON COLUMN namespace_authorities.reusable IS
    'Whether a revoked name in this namespace may EVER be bound again. When it may, quarantine_seconds says how long it must wait first. A handle is meant to move and waits for nothing; a Number waits out its printed life; a rotated contact address never comes back at all.';

-- ── The number namespace enters quarantined reuse ────────────────────────────
-- 12 months, as 365 days of seconds.
UPDATE namespace_authorities
   SET reusable           = TRUE,
       quarantine_seconds = 365 * 24 * 60 * 60,
       rationale          = 'Numbers are allocated to identities by the identity authority. A retired Number is held for twelve months before it may be reissued, so every card, badge and slide it was printed on has left circulation first; the new holder inherits nothing, because a reissue is a new node.'
 WHERE namespace = 'number';

-- Everything else keeps exactly the rule it had. Stated rather than assumed, so
-- this migration cannot quietly widen reuse somewhere it was never intended.
UPDATE namespace_authorities
   SET quarantine_seconds = NULL
 WHERE namespace IN ('handle', 'addr', 'pial', 'uuid', 'cid');

-- The quarantine check reads the most recent revocation of a name.
CREATE INDEX IF NOT EXISTS idx_names_revoked_at
    ON names (name, revoked_at) WHERE status = 'revoked';

-- The concurrent cap counts a single identity's live Numbers, which is a walk
-- from the identity back along `resolves_to`. That direction is already served
-- by idx_edges_object_predicate from migration 0001, so nothing is added for it.
