-- 0015_live_capacity_truth.sql — the row tells the truth about the ladder.
--
-- live_streams.status has always recorded that a broadcast is live. It has
-- never recorded what is actually being produced for it, and those came apart
-- constantly: when the box was oversubscribed the encoders died while the
-- publisher stayed connected, so the reconcile loop could not see it, and the
-- row read "live" with nothing being written for the rest of the broadcast.
-- A viewer got a spinner, the broadcaster got no indication at all, and the
-- only place the failure existed was an ffmpeg line in a container log.
--
-- These columns close that gap. They are written by the control plane at the
-- moment it issues an encode plan and at the moment it refuses one, so what the
-- row says is what the platform is doing.

ALTER TABLE live_streams
    -- The ladder as it is actually being produced, e.g. "480p@1500k 720p@3200k
    -- 1080p@copy [libx264]". Not the ladder the source could support: the one
    -- the box agreed to pay for.
    ADD COLUMN IF NOT EXISTS ladder text NOT NULL DEFAULT '',
    -- Which encoder is carrying it. A broadcast on the GPU and a broadcast on
    -- the CPU fail differently and are diagnosed differently.
    ADD COLUMN IF NOT EXISTS encoder text NOT NULL DEFAULT '',
    -- True when this broadcast is being served less than the full ladder its
    -- source could support, because the platform is at capacity.
    ADD COLUMN IF NOT EXISTS degraded boolean NOT NULL DEFAULT false,
    -- What to tell the broadcaster, in their words rather than the encoder's.
    -- Carries the reason a ladder was reduced and the reason a publish was
    -- refused; empty when there is nothing to say.
    ADD COLUMN IF NOT EXISTS capacity_note text NOT NULL DEFAULT '';

-- Operating this lane means answering "how many broadcasts is the box carrying
-- and how many of them are degraded" constantly, from the reconcile loop and
-- from the metrics exporter. That is a scan of every live row today.
CREATE INDEX IF NOT EXISTS idx_live_streams_degraded
    ON live_streams (started_at DESC)
    WHERE status = 'live' AND degraded;
