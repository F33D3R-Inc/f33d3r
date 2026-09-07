-- Indexes for the media authorization gate's ownership lookup.
-- Without them the gate seq-scans works on every uncached media object.
CREATE INDEX IF NOT EXISTS idx_works_media_urls_gin
    ON works USING GIN (media_urls);

CREATE INDEX IF NOT EXISTS idx_works_video_master_url
    ON works (video_master_url)
 WHERE video_master_url IS NOT NULL AND video_master_url <> '';
