-- A host's mute holds until a host lifts it, including across a rejoin.
ALTER TABLE frequency_participants
    ADD COLUMN IF NOT EXISTS moderator_muted BOOLEAN NOT NULL DEFAULT FALSE;

-- A person is in one Frequency at a time. Anyone already in several keeps the
-- room they entered last.
UPDATE frequency_participants p
   SET state = 'left', left_at = NOW()
 WHERE p.state = 'joined'
   AND EXISTS (
       SELECT 1 FROM frequency_participants newer
        WHERE newer.pial = p.pial
          AND newer.state = 'joined'
          AND (newer.joined_at, newer.id) > (p.joined_at, p.id)
   );

CREATE UNIQUE INDEX IF NOT EXISTS uq_frequency_participants_one_room
    ON frequency_participants (pial) WHERE state = 'joined';

-- An invite can admit a listener to an invite-only room.
ALTER TABLE frequency_invites DROP CONSTRAINT IF EXISTS frequency_invites_role_check;
ALTER TABLE frequency_invites
    ADD CONSTRAINT frequency_invites_role_check CHECK (role IN ('listener', 'speaker', 'cohost'));
