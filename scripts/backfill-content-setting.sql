-- One-off backfill: promote named verified accounts to content_setting='adult_enabled'
-- so they see adult content directly instead of the "standard" 18+ click-to-reveal gate.
--
-- Context: content_setting (the porn axis) was write-once at onboarding with no
-- settings/admin control, so adult accounts that onboarded as 'default' were stuck
-- behind the gate. Going forward this is handled automatically:
--   * becoming an adult creator (admin nsfw-toggle OR Verity/KYC approval) now sets
--     content_setting='adult_enabled' at the source,
--   * users can self-change it in Settings → Privacy & Safety → Content sensitivity,
--   * admins can set it per-user in the admin user lookup panel.
-- This script only repairs accounts that pre-date those fixes.
--
-- Run against the feed-engine database (f33d3r_feed). Edit the handle list as needed.
-- The is_minor guard makes it impossible to flip a minor regardless of the list.
--
--   psql "$DATABASE_URL" -f scripts/backfill-content-setting.sql

UPDATE user_profiles p
SET content_setting = 'adult_enabled', updated_at = NOW()
FROM users u
WHERE p.user_id = u.id
  AND lower(u.handle) IN ('miiyazuko','tehanibentley')
  AND NOT EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id AND r.is_minor)
RETURNING u.handle, p.content_setting;
