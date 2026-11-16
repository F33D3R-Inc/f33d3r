-- retire_identity_tables.sql  (number assigned when promoted: the next free one in migrations/)   (STAGED — not embedded, not applied)
--
-- Drops the identity authority tables feed-engine kept until elohim-veni's
-- one-time import had copied them.
--
-- This file lives in internal/db/migrations_staged/, which the migrator does not
-- embed, because the migrator is forward-only and applies every file it embeds
-- on the next boot. Shipping this in migrations/ before the import ran would
-- destroy the only copy of every password hash, session, second factor and
-- identity root on the platform. Staging it keeps the drop reviewed and in the
-- tree without letting a deploy run it early.
--
-- ORDER OF OPERATIONS
--
--   1. Deploy feed-engine with 0038_identity_projection applied. Nothing reads
--      or writes these tables any more; they hold their last rows unchanged.
--   2. Run elohim-veni's identity import against this database. It copies
--      pial_roots, pial_capabilities, pial_account_bindings, user_credentials
--      (password hashes and TOTP secrets), backup_codes, user_sessions and the
--      key tables, then publishes one identity.events snapshot for every
--      identity it imported.
--   3. Wait for feed-engine's identity projection to consume those snapshots.
--      Every account's identity_facts row is then confirmed = TRUE. Check:
--
--        SELECT COUNT(*) FROM users u
--          LEFT JOIN identity_facts f ON f.pial_id = u.pial_id
--         WHERE u.pial_id IS NULL OR f.pial_id IS NULL OR NOT f.confirmed;
--
--      must be 0.
--   4. git mv this file into internal/db/migrations/. If 0039 has been taken by
--      then, renumber it to the next free version in the same move: it has never
--      been applied, so no checksum is recorded against its name.
--   5. Deploy. The precondition below runs first and refuses — failing the
--      boot, applying nothing — if step 3 is not true, so promoting it early
--      cannot lose data; it can only stop the deploy.

DO $$
DECLARE
    pending BIGINT;
BEGIN
    SELECT COUNT(*) INTO pending
      FROM users u
      LEFT JOIN identity_facts f ON f.pial_id = u.pial_id
     WHERE u.pial_id IS NULL OR f.pial_id IS NULL OR NOT f.confirmed;
    IF pending > 0 THEN
        RAISE EXCEPTION 'identity retirement refused: % account(s) have an identity elohim-veni has not yet confirmed '
                        'through identity.events — run the import and let the projection catch up first', pending;
    END IF;
END $$;

-- Sessions, credentials and second factors: elohim-veni's now.
DROP TABLE IF EXISTS user_sessions;
DROP TABLE IF EXISTS user_credentials;
DROP TABLE IF EXISTS backup_codes;
DROP TABLE IF EXISTS password_reset_tokens;

-- Bindings and capabilities: elohim-veni's now; the projection carries what
-- this brain reads (users.pial_id, identity_facts.caps).
DROP TABLE IF EXISTS pial_account_bindings;
DROP TABLE IF EXISTS pial_capabilities;

-- Key material: registered with and served by elohim-veni.
DROP TABLE IF EXISTS pial_signing_keys;
DROP TABLE IF EXISTS pial_ecdh_keys;

-- The signup's copy of the supplied date of birth. elohim-veni records the date
-- a person supplies; this brain stopped writing a second copy with 0038.
ALTER TABLE user_roles DROP COLUMN IF EXISTS date_of_birth;

-- The roots themselves. 0038 moved every surviving foreign key onto
-- identity_facts, so nothing outside the tables dropped above references this
-- one; the founder index from 0033 goes with it.
DROP TABLE IF EXISTS pial_roots;
