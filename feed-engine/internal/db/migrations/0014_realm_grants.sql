-- 0014_realm_grants.sql
--
-- Realm was a pure function of XP. AwardXP recomputed it and overwrote
-- users.realm on every single award, so a realm placed by hand survived exactly
-- until the account's next like or follow. @miiyazuko sat at R5 on 125 XP and
-- @tehanibentley at R3 on 195 XP; R2 costs 500 XP, so the next award would have
-- dropped both to R1 and taken their rings with them.
--
-- Standing now has two sources and one derivation:
--
--   users.xp          → what the account EARNED   (realm.ComputeRealm)
--   users.realm_grant → the FLOOR it was PLACED on (0 = none)
--   users.role        → founder carries a permanent floor of its own
--   users.realm       → DERIVED: realm.Effective(xp, realm_grant, role)
--
-- A grant is a floor, never an override: earning past it carries the account
-- past it, and no grant can ever demote. users.realm is written by exactly two
-- statements in the source tree (realm.AwardXP and realm.SetGrant/Restate),
-- both of which route through realm.Effective. Nothing else may write it.

-- ── the grant column ─────────────────────────────────────────────────────────
-- A constant default on a new column is a catalogue-only change in PostgreSQL
-- 11+, so this does not rewrite the table.
ALTER TABLE users ADD COLUMN IF NOT EXISTS realm_grant SMALLINT NOT NULL DEFAULT 0;

-- The scale is 1–5 (realm.Min–realm.Max) plus 0 for "no grant". Pinned in Go by
-- TestTheGrantScaleMatchesTheMigration — change the scale and that test fails
-- until this constraint is replaced by a higher-numbered migration.
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_realm_grant_scale;
ALTER TABLE users ADD CONSTRAINT users_realm_grant_scale
    CHECK (realm_grant BETWEEN 0 AND 5) NOT VALID;
-- VALIDATE takes SHARE UPDATE EXCLUSIVE, not ACCESS EXCLUSIVE: readers and
-- writers keep running while it scans.
ALTER TABLE users VALIDATE CONSTRAINT users_realm_grant_scale;

-- ── honest data ──────────────────────────────────────────────────────────────
-- Every account standing above the floor today is standing there by hand: the
-- highest XP total on the platform at the time of writing is 195, and R2 costs
-- 500, so no account has earned anything above R1. Those hand-set values are
-- recorded as what they actually are — grants — instead of being left to be
-- erased by the next award or reset to R1 without anyone saying so.
--
-- For an account that HAD earned its realm this backfill is a no-op: a floor at
-- the level you already earned changes nothing while the XP that earned it
-- stands.
UPDATE users
   SET realm_grant = realm
 WHERE COALESCE(realm, 1) > 1
   AND COALESCE(realm_grant, 0) = 0;

-- The grant is an identity-affecting change, so it goes in the append-only PIAL
-- ledger with its provenance, exactly like a capability change. Backfilled rows
-- name this migration as the actor rather than pretending an admin typed them.
INSERT INTO pial_event_ledger (pial_id, account_id, event_type, payload, source)
SELECT u.pial_id, u.id, 'realm_granted',
       jsonb_build_object(
           'grant',     u.realm_grant,
           'effective', u.realm,
           'xp',        COALESCE(u.xp, 0),
           'reason',    'backfill: hand-set realm recorded as a grant so an XP award cannot erase it',
           'by',        'migration:0014_realm_grants'),
       'migration'
  FROM users u
 WHERE u.realm_grant > 0
   AND u.pial_id IS NOT NULL;

-- ── founder ──────────────────────────────────────────────────────────────────
-- Founder is a role, not a sixth realm. It carries a permanent floor at the top
-- of the existing scale (realm.FounderFloor = realm.Max = 5), derived from the
-- role itself, so it needs no grant and cannot be forgotten, erased or granted
-- to anyone else — role='founder' is already restricted to @tehanibentley at
-- the one place roles are assigned. This raises @tehanibentley from the R3 that
-- was set by hand to the R5 the role carries: the founder wears the Guardian
-- ring, always, and never has to earn it back.
UPDATE users
   SET realm = 5
 WHERE role = 'founder'
   AND COALESCE(realm, 1) < 5;

-- Bring users.realm up to the floor it is now derived from. This applies floors
-- only — the earned half stays with realm.ComputeRealm in Go, where the XP
-- thresholds live and are restated nowhere else, and the next award restates
-- the whole value anyway.
UPDATE users
   SET realm = GREATEST(COALESCE(realm, 1), realm_grant)
 WHERE realm_grant > COALESCE(realm, 1);

-- Accounts above the floor after this migration are picked up by the realm
-- index on its next reconcile pass (30s), so the rings follow without a restart.
