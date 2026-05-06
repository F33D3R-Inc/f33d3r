\c f33d3r_feed

DO $$
DECLARE
  -- PIALs
  p_tehani  UUID;
  p_admin   UUID;
  p_creator UUID;
  p_artist  UUID;
  p_dev     UUID;
  p_guest   UUID;

  -- Account IDs
  u_tehani  UUID;
  u_admin   UUID;
  u_creator UUID;
  u_artist  UUID;
  u_dev     UUID;
  u_guest   UUID;

  -- Password hash for 'f33d3rdev'
  pwd TEXT := '$2a$10$Dn2n1jFMkQ88o0ALSA.WEe4Io/Te5GsiDBVvS7cSyXVRprZHHb0kK';

  -- All capabilities granted to every account
  caps TEXT[] := ARRAY[
    'POSTING','NSFW_ACCESS','MONETIZATION','REALM_PROGRESSION',
    'NEW_ACCOUNT_TRUST','MESSAGING','MUSIC_UPLOAD'
  ];
  cap TEXT;

BEGIN

-- ── tehanibentley (Founder / Admin) ─────────────────────────────────────────
INSERT INTO pial_roots (public_key, state_hash)
  VALUES ('seed_pial_tehani_bentley', 'seed_hash_tehani')
  RETURNING pial_id INTO p_tehani;

INSERT INTO users (handle, pial_id, role, tier, realm, xp)
  VALUES ('tehanibentley', p_tehani, 'admin', 'creator', 4, 8500)
  RETURNING id INTO u_tehani;

INSERT INTO user_profiles (user_id, display_name, bio, is_creator, is_verified, theme_id, content_setting, follower_count, following_count, post_count)
  VALUES (u_tehani, 'Tehani Bentley', 'Founder of F33D3R. Building the future of the creator economy — where creators own everything.',
          true, true, 'void', 'adult_enabled', 5, 5, 12);

INSERT INTO user_roles (user_id, role_type, is_adult, is_id_verified, is_age_verified, onboard_done, onboard_step)
  VALUES (u_tehani, 'founder', true, true, true, true, 99);

INSERT INTO user_credentials (user_id, password_hash)
  VALUES (u_tehani, pwd);

INSERT INTO pial_account_bindings (pial_id, account_id, is_primary)
  VALUES (p_tehani, u_tehani, true);

FOREACH cap IN ARRAY caps LOOP
  INSERT INTO pial_capabilities (pial_id, capability, state, granted_by)
    VALUES (p_tehani, cap, 'granted', 'seed') ON CONFLICT DO NOTHING;
END LOOP;

UPDATE users SET xp = 8500, realm = 4 WHERE id = u_tehani;

-- ── admin ────────────────────────────────────────────────────────────────────
INSERT INTO pial_roots (public_key, state_hash)
  VALUES ('seed_pial_admin', 'seed_hash_admin')
  RETURNING pial_id INTO p_admin;

INSERT INTO users (handle, pial_id, role, tier, realm, xp)
  VALUES ('admin', p_admin, 'admin', 'creator', 3, 3200)
  RETURNING id INTO u_admin;

INSERT INTO user_profiles (user_id, display_name, bio, is_creator, is_verified, theme_id, content_setting, follower_count, following_count, post_count)
  VALUES (u_admin, 'F33D3R Admin', 'Platform operations and community safety.',
          false, true, 'obsidian', 'default', 5, 1, 4);

INSERT INTO user_roles (user_id, role_type, is_adult, is_id_verified, is_age_verified, onboard_done, onboard_step)
  VALUES (u_admin, 'admin', true, true, true, true, 99);

INSERT INTO user_credentials (user_id, password_hash)
  VALUES (u_admin, pwd);

INSERT INTO pial_account_bindings (pial_id, account_id, is_primary)
  VALUES (p_admin, u_admin, true);

FOREACH cap IN ARRAY caps LOOP
  INSERT INTO pial_capabilities (pial_id, capability, state, granted_by)
    VALUES (p_admin, cap, 'granted', 'seed') ON CONFLICT DO NOTHING;
END LOOP;

-- ── creator (content creator) ────────────────────────────────────────────────
INSERT INTO pial_roots (public_key, state_hash)
  VALUES ('seed_pial_creator', 'seed_hash_creator')
  RETURNING pial_id INTO p_creator;

INSERT INTO users (handle, pial_id, role, tier, realm, xp)
  VALUES ('creator', p_creator, 'user', 'creator', 3, 2500)
  RETURNING id INTO u_creator;

INSERT INTO user_profiles (user_id, display_name, bio, is_creator, is_verified, theme_id, content_setting, follower_count, following_count, post_count)
  VALUES (u_creator, 'Creator Studio', 'Making content that matters. Subscriptions open.',
          true, true, 'aurora', 'adult_enabled', 4, 3, 18);

INSERT INTO user_roles (user_id, role_type, is_adult, is_id_verified, is_age_verified, onboard_done, onboard_step)
  VALUES (u_creator, 'creator', true, true, true, true, 99);

INSERT INTO user_credentials (user_id, password_hash)
  VALUES (u_creator, pwd);

INSERT INTO pial_account_bindings (pial_id, account_id, is_primary)
  VALUES (p_creator, u_creator, true);

FOREACH cap IN ARRAY caps LOOP
  INSERT INTO pial_capabilities (pial_id, capability, state, granted_by)
    VALUES (p_creator, cap, 'granted', 'seed') ON CONFLICT DO NOTHING;
END LOOP;

-- ── artist (music creator) ───────────────────────────────────────────────────
INSERT INTO pial_roots (public_key, state_hash)
  VALUES ('seed_pial_artist', 'seed_hash_artist')
  RETURNING pial_id INTO p_artist;

INSERT INTO users (handle, pial_id, role, tier, realm, xp)
  VALUES ('artist', p_artist, 'user', 'creator', 2, 1200)
  RETURNING id INTO u_artist;

INSERT INTO user_profiles (user_id, display_name, bio, is_creator, is_verified, theme_id, content_setting, follower_count, following_count, post_count)
  VALUES (u_artist, 'The Artist', 'Music producer. Zior verified. 8 axis audio — find your sound.',
          true, false, 'dusk', 'default', 3, 4, 9);

INSERT INTO user_roles (user_id, role_type, is_adult, is_id_verified, is_age_verified, onboard_done, onboard_step)
  VALUES (u_artist, 'creator', true, false, false, true, 99);

INSERT INTO user_credentials (user_id, password_hash)
  VALUES (u_artist, pwd);

INSERT INTO pial_account_bindings (pial_id, account_id, is_primary)
  VALUES (p_artist, u_artist, true);

FOREACH cap IN ARRAY caps LOOP
  INSERT INTO pial_capabilities (pial_id, capability, state, granted_by)
    VALUES (p_artist, cap, 'granted', 'seed') ON CONFLICT DO NOTHING;
END LOOP;

-- ── dev (engineer) ───────────────────────────────────────────────────────────
INSERT INTO pial_roots (public_key, state_hash)
  VALUES ('seed_pial_dev', 'seed_hash_dev')
  RETURNING pial_id INTO p_dev;

INSERT INTO users (handle, pial_id, role, tier, realm, xp)
  VALUES ('dev', p_dev, 'user', 'free', 2, 600)
  RETURNING id INTO u_dev;

INSERT INTO user_profiles (user_id, display_name, bio, is_creator, is_verified, theme_id, content_setting, follower_count, following_count, post_count)
  VALUES (u_dev, 'Dev', 'Building at the edge. F33D3R engineer.',
          false, false, 'moss', 'default', 3, 5, 7);

INSERT INTO user_roles (user_id, role_type, is_adult, is_id_verified, is_age_verified, onboard_done, onboard_step)
  VALUES (u_dev, 'user', false, false, false, true, 99);

INSERT INTO user_credentials (user_id, password_hash)
  VALUES (u_dev, pwd);

INSERT INTO pial_account_bindings (pial_id, account_id, is_primary)
  VALUES (p_dev, u_dev, true);

FOREACH cap IN ARRAY caps LOOP
  INSERT INTO pial_capabilities (pial_id, capability, state, granted_by)
    VALUES (p_dev, cap, 'granted', 'seed') ON CONFLICT DO NOTHING;
END LOOP;

-- ── guest (regular user) ─────────────────────────────────────────────────────
INSERT INTO pial_roots (public_key, state_hash)
  VALUES ('seed_pial_guest', 'seed_hash_guest')
  RETURNING pial_id INTO p_guest;

INSERT INTO users (handle, pial_id, role, tier, realm, xp)
  VALUES ('guest', p_guest, 'user', 'free', 1, 80)
  RETURNING id INTO u_guest;

INSERT INTO user_profiles (user_id, display_name, bio, is_creator, is_verified, theme_id, content_setting, follower_count, following_count, post_count)
  VALUES (u_guest, 'Guest User', 'Just browsing. New to F33D3R.',
          false, false, 'void', 'default', 2, 2, 2);

INSERT INTO user_roles (user_id, role_type, is_adult, is_id_verified, is_age_verified, onboard_done, onboard_step)
  VALUES (u_guest, 'user', false, false, false, true, 99);

INSERT INTO user_credentials (user_id, password_hash)
  VALUES (u_guest, pwd);

INSERT INTO pial_account_bindings (pial_id, account_id, is_primary)
  VALUES (p_guest, u_guest, true);

FOREACH cap IN ARRAY caps LOOP
  INSERT INTO pial_capabilities (pial_id, capability, state, granted_by)
    VALUES (p_guest, cap, 'granted', 'seed') ON CONFLICT DO NOTHING;
END LOOP;

-- ── Follows (everyone follows tehanibentley; tehani follows everyone) ─────────
INSERT INTO follows (follower_id, following_id) VALUES
  (u_admin,   u_tehani),
  (u_creator, u_tehani),
  (u_artist,  u_tehani),
  (u_dev,     u_tehani),
  (u_guest,   u_tehani),
  (u_tehani,  u_admin),
  (u_tehani,  u_creator),
  (u_tehani,  u_artist),
  (u_tehani,  u_dev),
  (u_tehani,  u_guest),
  (u_creator, u_artist),
  (u_artist,  u_creator),
  (u_dev,     u_creator)
ON CONFLICT DO NOTHING;

-- Update follower/following counts
UPDATE user_profiles SET follower_count = 5 WHERE user_id = u_tehani;
UPDATE user_profiles SET following_count = 5 WHERE user_id = u_tehani;
UPDATE user_profiles SET follower_count = 1 WHERE user_id = u_admin;
UPDATE user_profiles SET follower_count = 2 WHERE user_id = u_creator;
UPDATE user_profiles SET follower_count = 2 WHERE user_id = u_artist;
UPDATE user_profiles SET follower_count = 0 WHERE user_id = u_dev;
UPDATE user_profiles SET follower_count = 1 WHERE user_id = u_guest;

-- ── Sample posts ─────────────────────────────────────────────────────────────
INSERT INTO posts (author_id, body, content_type, tags) VALUES
  (u_tehani, 'F33D3R is live. This is the beginning of a creator economy that actually works for creators — 97.5% of every transaction goes directly to you. AET economy, Signal-grade messaging, Jungian-behavioral ranking. Built different.', 'text', ARRAY['f33d3r','launch','creators']),
  (u_tehani, 'The creator economy is broken. Platforms take 20–80%. We take 2.5%. That''s not a feature — it''s the whole point.', 'text', ARRAY['economy','aet','creators']),
  (u_creator, 'First post on F33D3R. This platform hits different. Subscribe if you want the real content.', 'text', ARRAY['firstpost','subscribe']),
  (u_creator, 'Thessalon is wild. Subscriptions, PPV, tips — all in one place, all on-chain via AET. No middleman.', 'text', ARRAY['thessalon','monetization','aet']),
  (u_artist,  'Zior just clustered my music. 8-axis behavioral fingerprint of my sound. Finding my people algorithmically now.', 'text', ARRAY['zior','music','discovery']),
  (u_artist,  'Uploaded my first track. The audio analysis found my niche audience in under an hour. This is the future of music distribution.', 'text', ARRAY['music','upload','zior']),
  (u_dev,    'AethyrRank latency: 18ms p99. Rust ranking engine doing exactly what it should — invisible and fast.', 'text', ARRAY['aethyrrank','performance','rust']),
  (u_dev,    'All 14 brains running healthy. Vovin E2E encrypted. Ain Soph wallet live. Aethyr Ledger ticking every 2 seconds.', 'text', ARRAY['devlog','infrastructure']),
  (u_tehani, 'PIAL is the most important thing we built. Your identity is yours permanently — survives bans, handle changes, processor pressure. No platform can take it.', 'text', ARRAY['pial','identity','web3']),
  (u_tehani, 'Vovin uses Signal-grade encryption. Keys are generated in your browser. We literally cannot read your messages. Not a feature — a guarantee.', 'text', ARRAY['vovin','encryption','privacy']),
  (u_guest,  'Just signed up. The UI is clean.', 'text', ARRAY['newuser']),
  (u_guest,  'Sent my first AET tip. Instant. Zero friction.', 'text', ARRAY['aet','tip'])
ON CONFLICT DO NOTHING;

-- Update post counts
UPDATE user_profiles SET post_count = 4 WHERE user_id = u_tehani;
UPDATE user_profiles SET post_count = 2 WHERE user_id = u_creator;
UPDATE user_profiles SET post_count = 2 WHERE user_id = u_artist;
UPDATE user_profiles SET post_count = 2 WHERE user_id = u_dev;
UPDATE user_profiles SET post_count = 2 WHERE user_id = u_guest;

-- ── Post metrics (likes/impressions) ────────────────────────────────────────
INSERT INTO post_metrics (post_id, likes, impressions, reposts, comments, saves)
SELECT p.id,
  CASE WHEN p.author_id = u_tehani THEN floor(random()*120+30)::int
       WHEN p.author_id = u_creator THEN floor(random()*60+10)::int
       ELSE floor(random()*20+2)::int END,
  CASE WHEN p.author_id = u_tehani THEN floor(random()*800+200)::int
       ELSE floor(random()*200+20)::int END,
  floor(random()*10)::int,
  floor(random()*8)::int,
  floor(random()*15)::int
FROM posts p
ON CONFLICT DO NOTHING;

RAISE NOTICE 'Seed complete. Accounts: tehanibentley, admin, creator, artist, dev, guest — all password: f33d3rdev';
RAISE NOTICE 'PIAL tehanibentley: %', p_tehani;
RAISE NOTICE 'PIAL creator: %', p_creator;
RAISE NOTICE 'PIAL artist: %', p_artist;

END $$;
