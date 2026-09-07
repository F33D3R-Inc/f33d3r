-- Phase 0 of the PIAL fold: reconcile the three drifted identity stores into PIAL,
-- the single system of record. Run ONCE per environment against f33d3r_feed, after the
-- pial_roots.role / dob_locked columns exist (Phase 1 migrate).
--
--   psql "$DATABASE_URL" -f scripts/fold-auth-to-pial.sql
--
-- Precedence: a user verified in ANY store becomes age_verified on PIAL. role comes from
-- users.role. Minors are never marked age-verified. Idempotent — safe to re-run.

BEGIN;

-- 1. age_verified: union of the three stores → PIAL. Never for a known minor.
--    kyc_tier: keep an existing documented tier; otherwise 'basic' (self-reported floor).
UPDATE pial_roots pr
SET age_verified = TRUE,
    kyc_tier     = CASE WHEN pr.kyc_tier IN ('soft','full') THEN pr.kyc_tier ELSE 'basic' END
FROM users u
LEFT JOIN user_profiles p ON p.user_id = u.id
LEFT JOIN user_roles    r ON r.user_id = u.id
WHERE u.pial_id = pr.pial_id
  AND COALESCE(r.is_minor, FALSE) = FALSE
  AND (COALESCE(p.is_verified, FALSE)
       OR COALESCE(r.is_age_verified, FALSE)
       OR pr.age_verified);

-- 2. role: users.role (the live authority today) → PIAL.
UPDATE pial_roots pr
SET role = u.role
FROM users u
WHERE u.pial_id = pr.pial_id
  AND u.role IN ('user','admin','founder')
  AND pr.role <> u.role;

-- 2a. Seed PIAL is_adult/is_minor for accounts with no DOB, from the old user_roles flags
--     (DOB-based accounts are handled idempotently by migrate.go).
UPDATE pial_roots pr SET is_adult = TRUE, is_minor = FALSE
  FROM users u JOIN user_roles r ON r.user_id = u.id
  WHERE u.pial_id = pr.pial_id AND r.is_adult AND NOT r.is_minor AND pr.date_of_birth IS NULL;
UPDATE pial_roots pr SET is_minor = TRUE, is_adult = FALSE
  FROM users u JOIN user_roles r ON r.user_id = u.id
  WHERE u.pial_id = pr.pial_id AND r.is_minor AND pr.date_of_birth IS NULL;

-- 2b. Keep the denormalized users.role (read by feed queries for author role) in sync
--     with PIAL. user_profiles.is_verified / user_roles.role_type are NOT synced here —
--     all readers now source PIAL directly and those columns are dropped by migrate.
UPDATE users u
SET role = pr.role
FROM pial_roots pr
WHERE pr.pial_id = u.pial_id AND u.role <> pr.role;

COMMIT;

-- 3. Audit — must return ZERO rows: no PIAL that disagrees with the union of the old stores.
SELECT u.handle,
       pr.age_verified AS pial,
       COALESCE(p.is_verified,FALSE)    AS profile_badge,
       COALESCE(r.is_age_verified,FALSE) AS roles_age,
       COALESCE(r.is_minor,FALSE)        AS is_minor
FROM users u
JOIN pial_roots pr ON pr.pial_id = u.pial_id
LEFT JOIN user_profiles p ON p.user_id = u.id
LEFT JOIN user_roles    r ON r.user_id = u.id
WHERE COALESCE(r.is_minor,FALSE) = FALSE
  AND (COALESCE(p.is_verified,FALSE) OR COALESCE(r.is_age_verified,FALSE))
  AND pr.age_verified = FALSE;
