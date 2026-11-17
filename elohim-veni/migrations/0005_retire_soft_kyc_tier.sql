-- 0005_retire_soft_kyc_tier.sql
--
-- The 'soft' verification tier is retired. It was written by the MRZ-only age
-- check: a passport's machine-readable zone read from a photo, with no face
-- match and no liveness, so it proved that a document existed, not that the
-- person holding the account was the person named on it. That lane is gone:
-- there is one verification path (document, face match, live check), run the
-- same for viewers and for creators, and it records 'full'.
--
-- No identity holds 'soft' (checked in every environment before this was
-- written). If one does, this migration stops rather than deciding what that
-- identity's verification now means: that is a decision for a person, made by
-- moving each such identity to 'basic' (and age_verified to FALSE) with a
-- ledger row saying so, and then booting again.
--
--   check before deploying:
--     SELECT pial_id, kyc_verified_at FROM pial_states WHERE kyc_tier = 'soft';
DO $$
DECLARE
    held BIGINT;
BEGIN
    SELECT COUNT(*) INTO held FROM pial_states WHERE kyc_tier = 'soft';
    IF held > 0 THEN
        RAISE EXCEPTION '% identit(y/ies) still hold kyc_tier = ''soft''; decide each one (see this migration) before retiring the tier', held;
    END IF;
END
$$;

ALTER TABLE pial_states DROP CONSTRAINT pial_states_kyc_tier_known;
ALTER TABLE pial_states
    ADD CONSTRAINT pial_states_kyc_tier_known CHECK (kyc_tier IN ('none','basic','full'));
