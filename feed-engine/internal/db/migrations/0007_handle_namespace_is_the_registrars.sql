-- 0007_handle_namespace_is_the_registrars.sql
--
-- feed-engine stops claiming a namespace it does not govern.
--
-- Migration 0005 taught this brain to publish two kinds of name for a person:
-- `pial:<uuid>`, which is derived from the identity and which any brain may
-- bind, and `handle:<handle>`, which is not. A handle is ALLOCATED — it is a
-- decision about who gets a scarce string, and Manhattan's authority map
-- (manhattan 0002, 0004) records that the decision belongs to registry-brain.
-- So every handle row this brain queued was refused at the wire:
--
--   POST /v1/names -> 403
--   "the 'handle' namespace is governed by 'registry-brain';
--    only that brain may bind or retire names in it"
--
-- Manhattan was right and 0005 was wrong. The refusal is not an obstacle to
-- work around; it is the authority map doing exactly the job it was written
-- for, and the correct response is to stop making the call.
--
-- What replaces it is not a smaller version of the same claim. The registrar
-- already owns the whole handle lifecycle — registration, transfer, auction,
-- reclaim — and already carries its own outbox trigger that binds
-- `handle:<handle>` to `pial:<pial_id>` on every transition. Nothing needs to
-- be built there. feed-engine's job is to route the allocation to that brain
-- and then let the registrar publish the name, which is what internal/registry
-- now does at every point a handle is claimed.
--
-- This brain keeps registering the identity NODE under its derived `pial:` name.
-- That is deliberate and stays: registration has to work whoever meets the
-- entity first, and the node must exist before the registrar's bind can point
-- at it.

-- ── identities, without the handle claim ─────────────────────────────────────
-- The handle bind and the handle revoke are both gone. A handle change is now
-- entirely the registrar's transition to publish; this brain's only remaining
-- statement about a person is that their identity exists and is named by PIAL.
CREATE OR REPLACE FUNCTION manhattan_register_identity() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.pial_id IS NULL THEN
        RETURN NEW;
    END IF;

    PERFORM manhattan_enqueue(
        'node',
        'node:pial:' || NEW.pial_id::text,
        jsonb_build_object(
            'kind',      'identity',
            'name',      'pial:' || NEW.pial_id::text,
            'namespace', 'pial'));

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ── retire the rows that were never this brain's to send ─────────────────────
-- These are not pending work. They are instructions this brain had no standing
-- to issue, and no amount of retrying turns a 403 into a binding — every one of
-- them would fail on its twelfth attempt exactly as it failed on its first,
-- holding a slot in an ordered queue the whole time.
--
-- They are deleted rather than marked delivered, because marking them delivered
-- would record that a handle was bound in Manhattan when none was. The same
-- handles reach Manhattan through the registrar, which is where the reconciler
-- in internal/registry sends the ones that already exist.
DELETE FROM manhattan_outbox
 WHERE delivered_at IS NULL
   AND (    (op = 'name'        AND payload->>'namespace' = 'handle')
         OR (op = 'revoke_name' AND payload->>'name' LIKE 'handle:%') );
