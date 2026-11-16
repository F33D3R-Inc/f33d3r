-- 0033_one_founder.sql
--
-- The founder is the first account ever created on a database, and there is
-- only ever one. db.CreateAccount grants the role in the same transaction that
-- creates the first account; this index is the database's guarantee that no
-- path, present or future, can grant it a second time.
--
-- pial_roots.role is the authority for platform roles (users.role is a
-- projection of it), so the constraint lives there. Indexing the constant
-- expression over the founder rows alone means the index can hold one row.
--
-- A database that already has its founder keeps it unchanged: one founder row
-- satisfies the index as it is built. A database holding more than one is in a
-- state no rule produced, and which identity is the real founder is a decision
-- for a person, not for a migration — so this stops and names them.
DO $$
DECLARE
    founders TEXT;
BEGIN
    SELECT string_agg(pial_id::text, ', ' ORDER BY created_at)
      INTO founders
      FROM pial_roots
     WHERE role = 'founder'
    HAVING COUNT(*) > 1;

    IF founders IS NOT NULL THEN
        RAISE EXCEPTION 'more than one PIAL root holds role founder (%); '
                        'set every one except the first account''s back to user, then restart',
                        founders;
    END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS uq_pial_roots_one_founder
    ON pial_roots ((role))
 WHERE role = 'founder';
