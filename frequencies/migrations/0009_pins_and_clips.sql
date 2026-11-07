-- Works pinned to a room, in the order they were pinned.
CREATE TABLE IF NOT EXISTS frequency_pins (
    frequency_id UUID        NOT NULL REFERENCES frequencies(id) ON DELETE CASCADE,
    work_id      UUID        NOT NULL,
    pinned_by    TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (frequency_id, work_id)
);

-- A host can keep their replay from being clipped.
ALTER TABLE frequencies
    ADD COLUMN IF NOT EXISTS clips_enabled BOOLEAN NOT NULL DEFAULT TRUE;
