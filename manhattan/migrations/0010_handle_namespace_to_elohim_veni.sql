-- 0010_handle_namespace_to_elohim_veni.sql
--
-- The `handle` namespace moves from registry-brain to elohim-veni.
--
-- registry-brain is retired: its registry — handles, their lifecycle log,
-- auctions and the reserved namespace — is now part of elohim-veni
-- (elohim-veni/migrations/0002_handle_registry.sql), which already governs
-- the identities every handle points at. One brain now holds both halves of
-- "@bob is this person": the binding and the identity. The rule 0002 wrote
-- down does not change — a handle is a pointer, owning a name and owning the
-- node it names are different rights — only who holds the pointer's pen.
--
-- Bindings registry-brain made stay exactly as they are. Authority is looked
-- up per operation (db.rs authority_for_namespace), so from this migration on
-- elohim-veni may revoke and rebind every existing handle and registry-brain
-- may not touch one. allocated (0004), reusable and the quarantine hold
-- (0005, 0008) belong to the namespace and are untouched.
--
-- The UPDATE must move exactly one row; anything else means this database's
-- authority map is not the one 0002 wrote, and which brain should govern
-- handles on it is a decision for a person, so this stops and says so.
--
-- FORWARD-ONLY: never edit this file after it has been applied anywhere.

DO $$
DECLARE
    moved INT;
BEGIN
    UPDATE namespace_authorities
       SET brain      = 'elohim-veni',
           rationale  = 'A handle is a pointer, not an identity. The registry (registration, transfer, auction, reclaim) moved from registry-brain into elohim-veni, the brain that owns the identities handles point at; it binds handles on identity nodes and is the only brain that may.',
           updated_at = NOW()
     WHERE namespace = 'handle' AND brain = 'registry-brain';
    GET DIAGNOSTICS moved = ROW_COUNT;
    IF moved <> 1 THEN
        RAISE EXCEPTION 'expected the handle namespace to be governed by registry-brain; found % matching rows — '
                        'check namespace_authorities before restarting', moved;
    END IF;
END $$;
