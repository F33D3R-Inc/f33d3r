-- A recording is one LiveKit egress. object_key names the file in the replay volume.
ALTER TABLE frequency_recordings
    ADD COLUMN IF NOT EXISTS egress_id TEXT;

ALTER TABLE frequency_recordings DROP CONSTRAINT IF EXISTS frequency_recordings_processing_state_check;
ALTER TABLE frequency_recordings ADD CONSTRAINT frequency_recordings_processing_state_check
    CHECK (processing_state IN ('recording', 'ready', 'failed', 'deleted'));

-- A room records one thing at a time.
CREATE UNIQUE INDEX IF NOT EXISTS uq_frequency_recordings_active
    ON frequency_recordings (frequency_id) WHERE processing_state = 'recording';

ALTER TABLE frequencies DROP CONSTRAINT IF EXISTS frequencies_replay_status_check;
ALTER TABLE frequencies ADD CONSTRAINT frequencies_replay_status_check
    CHECK (replay_status IN ('none', 'processing', 'ready', 'failed', 'deleted'));
