-- 0018_erasure_referential_actions.sql
--
-- Erasure is a referential decision before it is a Go function.
--
-- db.PurgeUser was fourteen bare db.Exec calls: no assignment, so no error was
-- ever read; no transaction, so no failure could roll back; and a final
-- DELETE FROM users that could not succeed, because six foreign keys to users
-- were ON DELETE NO ACTION. Measured on this database before this migration,
-- with a probe account created through the real /onboard flow:
--
--   DELETE FROM pial_roots -> ERROR: violates pial_account_bindings_pial_id_fkey
--   DELETE FROM users      -> ERROR: violates pial_event_ledger_account_id_fkey
--
-- The endpoint answered "DB error" having already destroyed the account's
-- profile, roles, sessions, works, editions, scores and follow edges, and left
-- the users row, the password in user_credentials, the capabilities and the XP
-- standing. A live account with no profile. That is the worst state a deletion
-- path can produce, and it was the ONLY state it could produce.
--
-- The Go rewrite puts the whole purge in one transaction. This migration is the
-- other half: it decides, per foreign key, what a deleted person leaves behind.
-- Once these actions are declared, the database performs most of the erasure
-- itself on a single DELETE FROM users, and a table added later inherits
-- whatever its own foreign key declares rather than waiting to be remembered in
-- a hand-maintained list.
--
-- THE RULE THIS MIGRATION APPLIES, stated once so the next person does not have
-- to infer it from twelve ALTER statements:
--
--   * Rows where the purged person is the SUBJECT are theirs, and CASCADE.
--     Their works, articles, fleets, streams, sessions, keys, XP, preferences.
--
--   * Rows where the purged person acted with AUTHORITY OVER SOMEONE ELSE, or
--     where the row is the platform's own record that something happened,
--     survive with the person released — SET NULL. An admin's audit trail, a
--     reviewer's decision, an identity-ledger event, a referral another account
--     earned. Accountability that a person can delete by deleting themselves is
--     not accountability.
--
--   * The append-only PIAL ledger is never deleted from, in any branch, ever.
--
-- Every constraint below is replaced with the NOT VALID + VALIDATE pair that
-- migration 0014 established: ADD ... NOT VALID skips the full-table scan under
-- ACCESS EXCLUSIVE, and VALIDATE CONSTRAINT then scans under SHARE UPDATE
-- EXCLUSIVE, so readers and writers keep running.

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. pial_event_ledger.account_id -> SET NULL
--
-- The append-only identity ledger. Its entire value is that it cannot be
-- rewritten: a realm grant, a capability revocation, a CSAM enforcement, a data
-- export all land here and nothing in the source tree issues UPDATE or DELETE
-- against this table (LogPIALEvent says so in as many words, and this migration
-- does not make it a liar).
--
-- The event genuinely happened. What the purged person is entitled to is that
-- the record stops pointing at them, not that the platform forgets it acted.
-- account_id is already nullable, so SET NULL costs nothing and the row keeps
-- its event_type, payload, source and timestamp against the tombstoned PIAL.
--
-- DELETING these rows is forbidden. Nothing in PurgeUser, DeactivateUser or any
-- admin path may issue DELETE FROM pial_event_ledger.
ALTER TABLE pial_event_ledger DROP CONSTRAINT IF EXISTS pial_event_ledger_account_id_fkey;
ALTER TABLE pial_event_ledger ADD CONSTRAINT pial_event_ledger_account_id_fkey
    FOREIGN KEY (account_id) REFERENCES users(id) ON DELETE SET NULL NOT VALID;
ALTER TABLE pial_event_ledger VALIDATE CONSTRAINT pial_event_ledger_account_id_fkey;

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. org_verification_applications.reviewed_by -> SET NULL
--
-- The admin who reviewed somebody else's verification application. The review
-- happened, the badge it granted is still on the applicant's account, and the
-- applicant is not the person being erased here. A reviewer leaving must not
-- erase the decision, or a badge exists with no record of who approved it.
--
-- The sibling key on the same table, user_id (the applicant), already CASCADEs
-- and stays that way: that half IS the purged person's own application.
ALTER TABLE org_verification_applications DROP CONSTRAINT IF EXISTS org_verification_applications_reviewed_by_fkey;
ALTER TABLE org_verification_applications ADD CONSTRAINT org_verification_applications_reviewed_by_fkey
    FOREIGN KEY (reviewed_by) REFERENCES users(id) ON DELETE SET NULL NOT VALID;
ALTER TABLE org_verification_applications VALIDATE CONSTRAINT org_verification_applications_reviewed_by_fkey;

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. article_versions.author_id -> CASCADE
--
-- Revisions of the person's own long-form writing. Their content, so it goes
-- with them, exactly as works and articles already do.
--
-- NO ACTION here was not a decision, it was an inconsistency: article_versions
-- already CASCADEs from articles, and articles already CASCADEs from users, so
-- every version of an article whose author is purged was ALREADY going to be
-- deleted by that chain. The only rows this key ever blocked on were versions
-- authored by one account on another account's article — a co-edit in a
-- publication — and blocking there stopped the whole purge dead rather than
-- protecting anything. author_id is NOT NULL, so SET NULL is not available
-- without weakening a column that is genuinely never empty.
ALTER TABLE article_versions DROP CONSTRAINT IF EXISTS article_versions_author_id_fkey;
ALTER TABLE article_versions ADD CONSTRAINT article_versions_author_id_fkey
    FOREIGN KEY (author_id) REFERENCES users(id) ON DELETE CASCADE NOT VALID;
ALTER TABLE article_versions VALIDATE CONSTRAINT article_versions_author_id_fkey;

-- ─────────────────────────────────────────────────────────────────────────────
-- 4/5. creator_referrals — two parties, two different answers.
--
-- A referral is a fact about two accounts, and which one is being erased
-- changes what should be left.
--
-- referred_id -> SET NULL. This row is how the REFERRER's standing is counted:
-- GetReferralStats reads COUNT(*) ... WHERE referrer_id = $1 and reports it as
-- their total, verified and pending referrals. Cascading here would silently
-- reduce a surviving creator's earned count every time somebody they brought to
-- the platform left — the same class of drift migration 0016 removed from the
-- follower counters, and the same tell, a number nobody can trust. The column
-- is NOT NULL today, which is what forced NO ACTION; dropping that is not
-- weakening a protection, it is admitting that the counterpart can legitimately
-- cease to exist. The UNIQUE constraint on referred_id stays, and NULLs are
-- distinct in PostgreSQL, so many released rows coexist.
--
-- referrer_id -> CASCADE. The row exists to attribute a signup TO the referrer
-- and to pay them for it. With the referrer erased there is no beneficiary and
-- no meaning left, and CompleteReferral would otherwise scan a NULL referrer_id
-- into a string on its way to awarding XP to nobody. The referred account keeps
-- everything of its own; only the attribution goes.
ALTER TABLE creator_referrals ALTER COLUMN referred_id DROP NOT NULL;

ALTER TABLE creator_referrals DROP CONSTRAINT IF EXISTS creator_referrals_referred_id_fkey;
ALTER TABLE creator_referrals ADD CONSTRAINT creator_referrals_referred_id_fkey
    FOREIGN KEY (referred_id) REFERENCES users(id) ON DELETE SET NULL NOT VALID;
ALTER TABLE creator_referrals VALIDATE CONSTRAINT creator_referrals_referred_id_fkey;

ALTER TABLE creator_referrals DROP CONSTRAINT IF EXISTS creator_referrals_referrer_id_fkey;
ALTER TABLE creator_referrals ADD CONSTRAINT creator_referrals_referrer_id_fkey
    FOREIGN KEY (referrer_id) REFERENCES users(id) ON DELETE CASCADE NOT VALID;
ALTER TABLE creator_referrals VALIDATE CONSTRAINT creator_referrals_referrer_id_fkey;

-- ─────────────────────────────────────────────────────────────────────────────
-- 6. user_sessions.active_account_id -> SET NULL
--
-- A DIFFERENT account's live session, pointed at this one through multi-account
-- switching. The session belongs to somebody who is not being erased, so
-- deleting it would sign a bystander out of the platform because a linked
-- account was purged.
--
-- SET NULL is not a dangling pointer here, it is the documented resting state.
-- GetSessionIdentity reads COALESCE(active_account_id, user_id): NULL already
-- means "this session is acting as its own owner". So the moment the viewed
-- account is erased, the session falls back to the person who actually holds
-- it, which is precisely the correct outcome and needs no code change.
--
-- The sibling key user_id already CASCADEs and stays that way: the purged
-- person's own sessions must die with them.
ALTER TABLE user_sessions DROP CONSTRAINT IF EXISTS user_sessions_active_account_id_fkey;
ALTER TABLE user_sessions ADD CONSTRAINT user_sessions_active_account_id_fkey
    FOREIGN KEY (active_account_id) REFERENCES users(id) ON DELETE SET NULL NOT VALID;
ALTER TABLE user_sessions VALIDATE CONSTRAINT user_sessions_active_account_id_fkey;

-- ─────────────────────────────────────────────────────────────────────────────
-- 7. admin_audit_log.admin_id -> SET NULL  (was CASCADE)
--
-- Not one of the six that blocked the delete. Worse: it did not block, it
-- deleted. Every administrative action an account ever took — every purge,
-- every ban, every badge, every IP block — was erased by deleting that account.
-- An audit log its own subject can destroy is not an audit log, and a founder
-- purging an admin was quietly purging the record of what that admin did.
--
-- The read path was already written for this outcome and never got to use it:
-- GetAdminAuditLog does LEFT JOIN users ... COALESCE(u.handle,''), which under
-- CASCADE can never match a missing row because the row is gone too. SET NULL
-- is what makes that fallback reachable. admin_id has to become nullable for
-- it; the accompanying Go change makes the query COALESCE the id as well.
ALTER TABLE admin_audit_log ALTER COLUMN admin_id DROP NOT NULL;
ALTER TABLE admin_audit_log DROP CONSTRAINT IF EXISTS admin_audit_log_admin_id_fkey;
ALTER TABLE admin_audit_log ADD CONSTRAINT admin_audit_log_admin_id_fkey
    FOREIGN KEY (admin_id) REFERENCES users(id) ON DELETE SET NULL NOT VALID;
ALTER TABLE admin_audit_log VALIDATE CONSTRAINT admin_audit_log_admin_id_fkey;

-- ─────────────────────────────────────────────────────────────────────────────
-- 8. data_deletion_requests -> the request must outlive the deletion.
--
-- This table is the GDPR/CCPA queue: requestDataDeletion writes a row here when
-- somebody asks to be erased, and status/processed_at record that the platform
-- honoured it. It CASCADEd from users, so completing the request destroyed the
-- proof the request was ever made. A right-to-erasure system that erases its
-- own evidence of compliance cannot answer the one question a regulator asks.
--
-- user_id was the primary key, which is why it could not simply be released. It
-- gets a surrogate key instead, keeps a UNIQUE index so the ON CONFLICT
-- (user_id) upsert in requestDataDeletion keeps working unchanged, and becomes
-- nullable so the row can be released while pial_id — already a plain text
-- column, now pointing at a tombstoned identity that names nobody — carries the
-- record forward.
--
-- gen_random_uuid() is VOLATILE, so unlike migration 0014's constant default
-- this ADD COLUMN does rewrite the table. That is stated rather than glossed:
-- this is a small operational queue, not a content table, and the rewrite is
-- bounded by the number of outstanding erasure requests.
ALTER TABLE data_deletion_requests ADD COLUMN IF NOT EXISTS id UUID NOT NULL DEFAULT gen_random_uuid();
ALTER TABLE data_deletion_requests DROP CONSTRAINT IF EXISTS data_deletion_requests_pkey;
ALTER TABLE data_deletion_requests ADD CONSTRAINT data_deletion_requests_pkey PRIMARY KEY (id);
ALTER TABLE data_deletion_requests ALTER COLUMN user_id DROP NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_data_deletion_requests_user
    ON data_deletion_requests (user_id);
ALTER TABLE data_deletion_requests DROP CONSTRAINT IF EXISTS data_deletion_requests_user_id_fkey;
ALTER TABLE data_deletion_requests ADD CONSTRAINT data_deletion_requests_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE SET NULL NOT VALID;
ALTER TABLE data_deletion_requests VALIDATE CONSTRAINT data_deletion_requests_user_id_fkey;

-- ─────────────────────────────────────────────────────────────────────────────
-- WHAT THIS MIGRATION DELIBERATELY DOES NOT TOUCH
--
-- The nine foreign keys into pial_roots stay ON DELETE NO ACTION, every one of
-- them, because a PIAL root is never deleted. It cannot be: pial_event_ledger
-- .pial_id is NOT NULL and the ledger is append-only, so the moment an identity
-- has a single event — and every identity does, from the moment it is created —
-- its root is pinned for as long as that event exists. PurgeUser's old
-- DELETE FROM pial_roots was not merely failing, it was asking for something the
-- ledger's integrity guarantee forbids.
--
-- So a purge TOMBSTONES the root instead, and pial_roots was built for exactly
-- that and was simply never used: is_tombstoned, deleted_at and status have been
-- sitting on the table since the baseline. The Go side clears the personal
-- content of the root — public key, state hash, date of birth, KYC tier and age
-- flags — and leaves an opaque UUID marked purged, holding the ledger,
-- content_receipts and enforcement_actions referentially whole while naming
-- nobody. Those NO ACTION keys are the thing that makes that guarantee real, so
-- weakening them would be removing the protection, not fixing it.
