-- ── The integrity gate ───────────────────────────────────────────────────────
-- Abraxas is the platform's one integrity authority. Before an uploaded asset
-- can become `ready`, caeor asks Abraxas (POST /v1/check) about it:
--
--   allow   → the asset proceeds; an ownership finding is recorded for credit
--   review  → the asset is `held`: it exists, its bytes are kept, nothing of
--             it is servable, until Abraxas answers allow or block
--   block   → the asset is withdrawn with reason banned:<category>
--
-- An unanswered check is never an allow: the asset is `held` with verdict
-- 'unreachable' and caeor asks again, with backoff, until Abraxas answers.
-- Every held asset carries the time of its next check, so the reconciler in
-- src/integrity.rs owns a durable, finite backlog and nothing is forgotten.
--
-- Fingerprints and credit survive withdrawal: the columns below are on the
-- asset row, which is never deleted (0001), and a recorded original owner can
-- be corrected but never erased (trigger at the bottom).

-- The status vocabulary gains `held`. The inline CHECK of 0001 is found by its
-- definition rather than assumed by name, so this cannot leave the old
-- vocabulary standing beside the new one.
DO $do$
DECLARE c TEXT;
BEGIN
    FOR c IN
        SELECT conname FROM pg_constraint
         WHERE conrelid = 'assets'::regclass AND contype = 'c'
           AND pg_get_constraintdef(oid) LIKE '%''processing''%''ready''%''failed''%''withdrawn''%'
    LOOP
        EXECUTE format('ALTER TABLE assets DROP CONSTRAINT %I', c);
    END LOOP;
END
$do$;

ALTER TABLE assets ADD CONSTRAINT assets_status_vocabulary
    CHECK (status IN ('processing','held','ready','failed','withdrawn'));

ALTER TABLE assets
    -- What Abraxas last said. NULL for assets that predate the gate (imports,
    -- clips drawn from the platform's own replays).
    ADD COLUMN IF NOT EXISTS integrity_verdict        TEXT
        CHECK (integrity_verdict IN ('pending','allow','review','block','unreachable')),
    ADD COLUMN IF NOT EXISTS integrity_checked_at     TIMESTAMPTZ,
    -- When a held asset is next put to Abraxas. NULL once the asset has left
    -- `held`.
    ADD COLUMN IF NOT EXISTS integrity_next_check_at  TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS integrity_attempts       INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS integrity_error          TEXT,
    -- Abraxas's fingerprint record of these bytes.
    ADD COLUMN IF NOT EXISTS fingerprint_id           TEXT,
    -- Ownership: these bytes are (or derive from) somebody else's asset. The
    -- original owner is the one credited wherever this asset is shown.
    ADD COLUMN IF NOT EXISTS original_asset           UUID,
    ADD COLUMN IF NOT EXISTS original_pial            UUID,
    ADD COLUMN IF NOT EXISTS ownership_kind           TEXT
        CHECK (ownership_kind IN ('exact','derivative','excerpt')),
    ADD COLUMN IF NOT EXISTS ownership_confidence     DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS ownership_offset_seconds DOUBLE PRECISION;

-- A held asset is waiting on an answer and always knows when it asks next.
ALTER TABLE assets ADD CONSTRAINT assets_held_consistent CHECK (
    status <> 'held'
    OR (integrity_verdict IN ('pending','review','unreachable') AND integrity_next_check_at IS NOT NULL)
);

-- An ownership finding is whole: an original owner and how the bytes relate.
ALTER TABLE assets ADD CONSTRAINT assets_ownership_consistent CHECK (
    (original_pial IS NULL) = (ownership_kind IS NULL)
    AND (original_asset IS NULL OR original_pial IS NOT NULL)
);

-- The reconciler's query: held assets, due, oldest first.
CREATE INDEX IF NOT EXISTS idx_assets_held_due
    ON assets(integrity_next_check_at, created_at) WHERE status = 'held';

-- Credit is never erased. A recorded original owner may be corrected by a
-- later verdict (an admin's credit decision), never cleared.
CREATE OR REPLACE FUNCTION caeor_credit_is_kept() RETURNS TRIGGER AS $fn$
BEGIN
    IF OLD.original_pial IS NOT NULL AND NEW.original_pial IS NULL THEN
        RAISE EXCEPTION 'an asset''s recorded original owner can be corrected, never erased'
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF OLD.fingerprint_id IS NOT NULL AND NEW.fingerprint_id IS NULL THEN
        RAISE EXCEPTION 'an asset''s fingerprint record is never erased'
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$fn$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_assets_credit_kept ON assets;
CREATE TRIGGER trg_assets_credit_kept
    BEFORE UPDATE ON assets
    FOR EACH ROW EXECUTE FUNCTION caeor_credit_is_kept();
