-- 0040_vision_personas.sql
--
-- A PIAL is a person's root identity, and one person can hold several accounts
-- (personas). Migration 0023 keyed every Vision table by PIAL and joined the
-- result to EVERY account on that PIAL, so a Vision posted as persona B showed
-- on persona A's ring, A's wire address could open it, A could delete it, and
-- an author's viewer list showed every persona of whoever watched. Each of
-- those lets an observer link two of one person's accounts.
--
-- This migration makes the account the owner of everything an observer sees:
--
--   visions        gains author_account (the persona that posted) and
--                  account_seq (that persona's own ordinal). The public address
--                  is (author_account, account_seq). (author_pial, seq) stays
--                  as the server-internal key: it is the content-safety subject
--                  `<author_pial>.<seq>` Abraxas judges, and it never leaves the
--                  server. A persona's ordinals come from its own lane, so the
--                  numbering of one persona never interleaves with another's.
--   vision_account_lanes
--                  one counter per account, handing out account_seq.
--   vision_views   keyed by the viewing account, not the viewer's PIAL.
--   vision_poll_votes
--                  keyed by the voting account.
--   vision_mutes   account to account.
--
-- Every account column references users(id) ON DELETE CASCADE, so erasing one
-- account removes that account's Visions, views, votes and mutes and nothing of
-- the person's other accounts (see 0018 for the cascade doctrine). The PIAL
-- columns that existed only so a purge could find these rows are dropped.
--
-- ── Backfill ────────────────────────────────────────────────────────────────
-- No record of the posting account exists anywhere: the vision.created content
-- event carries only pial_id, visions.metadata carries only {source}, and
-- vision_lanes counts per PIAL. Each existing row is therefore attributed to the
-- PIAL's primary account — its earliest users row by (created_at, id), the same
-- rule the media gate uses for a Vision's governing account. A row whose PIAL
-- has no account left is unreachable by any address and is deleted.

-- ── vision_account_lanes ────────────────────────────────────────────────────
CREATE TABLE vision_account_lanes (
    account_id UUID   PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    last_seq   BIGINT NOT NULL DEFAULT 0
);

-- ── visions ─────────────────────────────────────────────────────────────────
ALTER TABLE visions ADD COLUMN author_account UUID;
ALTER TABLE visions ADD COLUMN account_seq    BIGINT;

UPDATE visions v
   SET author_account = p.id
  FROM (SELECT DISTINCT ON (u.pial_id) u.pial_id, u.id
          FROM users u
         WHERE u.pial_id IS NOT NULL
         ORDER BY u.pial_id, u.created_at, u.id) p
 WHERE p.pial_id = v.author_pial;

DELETE FROM visions WHERE author_account IS NULL;

UPDATE visions v
   SET account_seq = n.rn
  FROM (SELECT author_pial, seq,
               ROW_NUMBER() OVER (PARTITION BY author_account ORDER BY seq) AS rn
          FROM visions) n
 WHERE n.author_pial = v.author_pial AND n.seq = v.seq;

ALTER TABLE visions ALTER COLUMN author_account SET NOT NULL;
ALTER TABLE visions ALTER COLUMN account_seq    SET NOT NULL;
ALTER TABLE visions
    ADD CONSTRAINT visions_author_account_fkey
    FOREIGN KEY (author_account) REFERENCES users(id) ON DELETE CASCADE;
ALTER TABLE visions
    ADD CONSTRAINT visions_account_address_key UNIQUE (author_account, account_seq);
ALTER TABLE visions
    ADD CONSTRAINT visions_account_seq_positive CHECK (account_seq > 0);

INSERT INTO vision_account_lanes (account_id, last_seq)
SELECT author_account, MAX(account_seq) FROM visions GROUP BY author_account;

DROP INDEX IF EXISTS idx_visions_author_live;
CREATE INDEX idx_visions_account_live
    ON visions(author_account, expires_at, created_at) WHERE deleted_at IS NULL;

-- ── vision_views ────────────────────────────────────────────────────────────
ALTER TABLE vision_views ADD COLUMN viewer_account UUID;

UPDATE vision_views vv
   SET viewer_account = p.id
  FROM (SELECT DISTINCT ON (u.pial_id) u.pial_id, u.id
          FROM users u
         WHERE u.pial_id IS NOT NULL
         ORDER BY u.pial_id, u.created_at, u.id) p
 WHERE p.pial_id = vv.viewer_pial;

DELETE FROM vision_views WHERE viewer_account IS NULL;

ALTER TABLE vision_views DROP CONSTRAINT vision_views_pkey;
DROP INDEX IF EXISTS idx_vision_views_viewer;
ALTER TABLE vision_views DROP COLUMN viewer_pial;
ALTER TABLE vision_views ALTER COLUMN viewer_account SET NOT NULL;
ALTER TABLE vision_views
    ADD CONSTRAINT vision_views_viewer_account_fkey
    FOREIGN KEY (viewer_account) REFERENCES users(id) ON DELETE CASCADE;
ALTER TABLE vision_views ADD PRIMARY KEY (author_pial, seq, viewer_account);
CREATE INDEX idx_vision_views_viewer_account
    ON vision_views(viewer_account, viewed_at DESC);

-- ── vision_poll_votes ───────────────────────────────────────────────────────
ALTER TABLE vision_poll_votes ADD COLUMN voter_account UUID;

UPDATE vision_poll_votes pv
   SET voter_account = p.id
  FROM (SELECT DISTINCT ON (u.pial_id) u.pial_id, u.id
          FROM users u
         WHERE u.pial_id IS NOT NULL
         ORDER BY u.pial_id, u.created_at, u.id) p
 WHERE p.pial_id = pv.voter_pial;

DELETE FROM vision_poll_votes WHERE voter_account IS NULL;

ALTER TABLE vision_poll_votes DROP CONSTRAINT vision_poll_votes_pkey;
ALTER TABLE vision_poll_votes DROP COLUMN voter_pial;
ALTER TABLE vision_poll_votes ALTER COLUMN voter_account SET NOT NULL;
ALTER TABLE vision_poll_votes
    ADD CONSTRAINT vision_poll_votes_voter_account_fkey
    FOREIGN KEY (voter_account) REFERENCES users(id) ON DELETE CASCADE;
ALTER TABLE vision_poll_votes ADD PRIMARY KEY (author_pial, seq, voter_account);

-- ── vision_mutes ────────────────────────────────────────────────────────────
ALTER TABLE vision_mutes ADD COLUMN muter_account UUID;
ALTER TABLE vision_mutes ADD COLUMN muted_account UUID;

UPDATE vision_mutes m
   SET muter_account = a.id
  FROM (SELECT DISTINCT ON (u.pial_id) u.pial_id, u.id
          FROM users u
         WHERE u.pial_id IS NOT NULL
         ORDER BY u.pial_id, u.created_at, u.id) a
 WHERE a.pial_id = m.muter_pial;

UPDATE vision_mutes m
   SET muted_account = a.id
  FROM (SELECT DISTINCT ON (u.pial_id) u.pial_id, u.id
          FROM users u
         WHERE u.pial_id IS NOT NULL
         ORDER BY u.pial_id, u.created_at, u.id) a
 WHERE a.pial_id = m.muted_pial;

DELETE FROM vision_mutes
 WHERE muter_account IS NULL OR muted_account IS NULL OR muter_account = muted_account;

ALTER TABLE vision_mutes DROP CONSTRAINT vision_mutes_pkey;
ALTER TABLE vision_mutes DROP COLUMN muter_pial;
ALTER TABLE vision_mutes DROP COLUMN muted_pial;
ALTER TABLE vision_mutes ALTER COLUMN muter_account SET NOT NULL;
ALTER TABLE vision_mutes ALTER COLUMN muted_account SET NOT NULL;
ALTER TABLE vision_mutes
    ADD CONSTRAINT vision_mutes_muter_account_fkey
    FOREIGN KEY (muter_account) REFERENCES users(id) ON DELETE CASCADE;
ALTER TABLE vision_mutes
    ADD CONSTRAINT vision_mutes_muted_account_fkey
    FOREIGN KEY (muted_account) REFERENCES users(id) ON DELETE CASCADE;
ALTER TABLE vision_mutes
    ADD CONSTRAINT vision_mutes_distinct CHECK (muter_account <> muted_account);
ALTER TABLE vision_mutes ADD PRIMARY KEY (muter_account, muted_account);
