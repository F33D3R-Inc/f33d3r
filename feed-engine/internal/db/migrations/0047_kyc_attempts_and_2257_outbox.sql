-- 0047_kyc_attempts_and_2257_outbox.sql
--
-- Two pieces of the identity-verification rebuild.
--
-- ── 1. kyc_attempts: consent, held only until the attempt is submitted ────────
-- No camera opens and no upload is taken before the person has read what is
-- collected and agreed to it. The agreement is recorded here when they submit
-- the consent Fragment, and it travels with the attempt's evidence to Verity,
-- which keeps it. This row is orchestration state only: it names the attempt
-- (attempt_id is the id Verity records the submission under), the purpose and
-- document type the person chose, and the consent. The row is deleted as soon
-- as the attempt has been answered, and the cron sweep deletes any attempt
-- abandoned for longer than a day, so the consent's IP address and user agent
-- are never kept here past the attempt.
CREATE TABLE IF NOT EXISTS kyc_attempts (
    attempt_id          UUID        PRIMARY KEY,
    account_id          UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    pial_id             UUID        NOT NULL,
    purpose             TEXT        NOT NULL CHECK (purpose IN ('age', 'creator')),
    document_type       TEXT        NOT NULL CHECK (document_type IN ('passport', 'id_card', 'drivers_license')),
    consent_version     TEXT        NOT NULL,
    consent_accepted_at TIMESTAMPTZ NOT NULL,
    consent_ip          TEXT        NOT NULL,
    consent_user_agent  TEXT        NOT NULL,
    -- Set when the evidence is submitted: an attempt is submitted once.
    submitted_at        TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_kyc_attempts_age ON kyc_attempts (consent_accepted_at);

-- ── 2. compliance_2257_outbox: what 18+ works owe Verity ─────────────────────
-- An 18+ work by a creator is published only when Verity holds a passing
-- creator verification for its author (the publish gate refuses otherwise).
-- The 2257 record that links the work to that verification is written to
-- Verity by this outbox, queued in the work's own create transaction; the
-- removal report is queued in the transaction that deletes the work. A failure
-- is retried with backoff and quarantined for a person at the attempt cap,
-- never dropped.
--
-- There is deliberately no foreign key to works: an administrator's removal and
-- an account purge delete the works row, and the removal report must outlive
-- it. Rows name the author's PIAL because Verity's records are keyed on it;
-- they are 2257 obligations and are never erased by a purge.
--
--   what is stuck:  SELECT id, action, work_id, attempts, last_error, quarantined_at
--                     FROM compliance_2257_outbox
--                    WHERE quarantined_at IS NOT NULL AND done_at IS NULL;
--
--   put one back:   UPDATE compliance_2257_outbox
--                      SET quarantined_at = NULL, quarantine_reason = NULL,
--                          attempts = 0, next_attempt_at = NOW()
--                    WHERE id = $1;
CREATE TABLE IF NOT EXISTS compliance_2257_outbox (
    id                BIGSERIAL   PRIMARY KEY,
    work_id           UUID        NOT NULL,
    pial_id           UUID        NOT NULL,
    -- Mirrors db.Compliance2257Action (compliance_2257.go). Change one and the
    -- other must change with it.
    action            TEXT        NOT NULL CHECK (action IN ('record', 'content_removed')),
    dedup_key         TEXT        NOT NULL UNIQUE,
    removed_at        TIMESTAMPTZ,
    attempts          INT         NOT NULL DEFAULT 0,
    last_error        TEXT,
    next_attempt_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    done_at           TIMESTAMPTZ,
    quarantined_at    TIMESTAMPTZ,
    quarantine_reason TEXT,
    CONSTRAINT compliance_2257_outbox_removed_at CHECK ((action = 'content_removed') = (removed_at IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS idx_compliance_2257_outbox_due
    ON compliance_2257_outbox (next_attempt_at, id)
 WHERE done_at IS NULL AND quarantined_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_compliance_2257_outbox_work
    ON compliance_2257_outbox (work_id, action);
