-- Founder account for a fresh local database.
-- bootstrap-local.sh applies this after feed-engine has run its migrations, because
-- these tables do not exist during Postgres's first boot.
-- Safe to re-run: an existing founder account is left untouched.

DO $$
DECLARE
  -- Same PIAL as tehanibentleyPIAL in feed-engine/internal/handler/admin.go.
  p_founder UUID := '93d563af-856c-4bf8-a69b-4f7da08cd18e';
  u_founder UUID;
  -- bootstrap-local.sh sets this from FOUNDER_PASSWORD in .env.local.
  pwd TEXT := crypt(current_setting('f33d3r.founder_password'), gen_salt('bf', 10));
BEGIN

IF EXISTS (SELECT 1 FROM users WHERE handle = 'tehanibentley') THEN
  RAISE NOTICE 'Founder account already present, nothing to seed';
  RETURN;
END IF;

INSERT INTO pial_roots (pial_id, public_key, state_hash, role, kyc_tier, kyc_verified_at,
                        age_verified, dob_locked, is_adult, is_minor)
  VALUES (p_founder, 'seed_pial_tehani_bentley', 'seed_hash_tehani', 'founder', 'full', NOW(),
          TRUE, TRUE, TRUE, FALSE);

INSERT INTO users (handle, pial_id, role, tier, realm, xp)
  VALUES ('tehanibentley', p_founder, 'founder', 'creator', 4, 8500)
  RETURNING id INTO u_founder;

INSERT INTO user_profiles (user_id, display_name, bio, is_creator, theme_id, content_setting)
  VALUES (u_founder, 'Tehani Bentley', 'Founder of F33D3R.', TRUE, 'void', 'adult_enabled');

INSERT INTO user_roles (user_id, is_id_verified, onboard_done, onboard_step)
  VALUES (u_founder, TRUE, TRUE, 99);

INSERT INTO user_credentials (user_id, password_hash)
  VALUES (u_founder, pwd);

INSERT INTO pial_account_bindings (pial_id, account_id, is_primary)
  VALUES (p_founder, u_founder, TRUE);

INSERT INTO pial_capabilities (pial_id, capability, state, granted_by)
SELECT p_founder, cap, 'granted', 'seed'
FROM unnest(ARRAY['POSTING','REALM_PROGRESSION','NEW_ACCOUNT_TRUST','MESSAGING','MUSIC_UPLOAD',
                  'MEDIA_UPLOAD','ADULT_CONTENT','TIPPING','LIVE_STREAMING','API_ACCESS']) AS cap
ON CONFLICT (pial_id, capability) DO NOTHING;

RAISE NOTICE 'Founder account seeded: tehanibentley';

END $$;
