-- Who asked to be told when a scheduled Frequency starts.
CREATE TABLE IF NOT EXISTS frequency_reminders (
    frequency_id UUID        NOT NULL REFERENCES frequencies(id) ON DELETE CASCADE,
    pial         TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (frequency_id, pial)
);

-- Each of these fires once per scheduled time. Changing the time clears them.
ALTER TABLE frequencies
    ADD COLUMN IF NOT EXISTS soon_notified_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS start_due_notified_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_frequencies_scheduled_due
    ON frequencies (scheduled_at) WHERE state = 'scheduled';
