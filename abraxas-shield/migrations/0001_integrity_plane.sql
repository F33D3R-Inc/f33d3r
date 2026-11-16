-- ── Abraxas: the integrity plane ────────────────────────────────────────────
-- f33d3r_integrity is the one place the platform records what it knows about
-- the integrity of content: which bytes are banned, who first put a piece of
-- media on the platform, what every scan found, what was decided and by whom.
--
-- Four rules shape every table below:
--
--   1. Integrity facts outlive the content they describe. A fingerprint, an
--      ownership match, a banned-registry finding and a verdict survive the
--      work's deletion and the uploader's erasure. The no-delete triggers at
--      the bottom make that a property of the database, not a habit of the
--      code. Revocation is a column, never a DELETE.
--
--   2. A person is a PIAL, never a handle. A handle is a pointer that can be
--      transferred or reclaimed; credit and enforcement keyed on one land on
--      whoever holds it next. No table here stores a handle as a key.
--
--   3. Nothing sits unjudged in a log. Every case a machine cannot decide is a
--      row in review_queue, at a stage someone is responsible for: ml_review
--      (the worker's deep pass) and then admin_review (a person).
--
--   4. Every decision leaves this database through safety_outbox, written in
--      the same transaction as the decision. A verdict that was recorded but
--      never announced, or announced but never recorded, cannot happen.

-- ── Fingerprints ─────────────────────────────────────────────────────────────
-- One row per asset, written by POST /v1/check and kept for the life of the
-- platform. uploader_pial is the identity that owns the credit for the first
-- appearance of this media. origin 'import' rows come from feed-engine's
-- media_fingerprints and may lack an asset or a resolvable owner; every live
-- row has both.
CREATE TABLE fingerprints (
    id                    UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    asset_id              UUID        UNIQUE,
    uploader_pial         UUID,
    -- NULL only on imported rows: feed-engine's media_fingerprints never
    -- recorded what the media was, and guessing would be inventing it.
    kind                  TEXT        CHECK (kind IN ('image','video','voice','audio','clip',
                                                      'avatar','header','encrypted')),
    origin                TEXT        NOT NULL DEFAULT 'check'
                                      CHECK (origin IN ('check','import')),
    sha256                TEXT        CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    -- 64-bit DCT perceptual hash of a still image (imagehash.phash), stored as
    -- the two's-complement BIGINT of its 16 hex digits.
    phash                 BIGINT,
    -- chromaprint's compressed URL-safe base64 fingerprint.
    chromaprint           TEXT,
    duration_seconds      DOUBLE PRECISION CHECK (duration_seconds IS NULL OR duration_seconds >= 0),
    -- Relative to the media private store (caeor's layout); never begins with
    -- '/', so it can never name a served path.
    private_path          TEXT        CHECK (private_path IS NULL
                                             OR (private_path NOT LIKE '/%' AND private_path NOT LIKE '%..%')),
    -- Media whose re-uploads are an ownership question. Avatars, headers and
    -- ciphertext are gated against the banned registry but never become the
    -- "original" anyone is credited against.
    ownership_eligible    BOOLEAN     NOT NULL,
    segment_count         INT         NOT NULL DEFAULT 0,
    landmark_count        INT         NOT NULL DEFAULT 0,
    robust_landmark_count INT         NOT NULL DEFAULT 0,
    -- The worker's landmark parameter set (audio_landmarks.LANDMARK_VERSION).
    landmark_version      TEXT,
    -- Set once the visual segments and audio landmarks are in the index.
    perceptual_at         TIMESTAMPTZ,
    import_ref            TEXT        UNIQUE,
    legacy_asset_ref      TEXT,
    legacy_post_id        TEXT,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fingerprints_owner  CHECK (uploader_pial IS NOT NULL OR origin = 'import'),
    CONSTRAINT fingerprints_asset  CHECK (asset_id IS NOT NULL OR origin = 'import'),
    CONSTRAINT fingerprints_sha256 CHECK (sha256 IS NOT NULL OR origin = 'import'),
    CONSTRAINT fingerprints_kind   CHECK (kind IS NOT NULL OR origin = 'import'),
    CONSTRAINT fingerprints_import  CHECK ((origin = 'import') = (import_ref IS NOT NULL))
);
CREATE INDEX idx_fingerprints_sha256 ON fingerprints(sha256, created_at) WHERE sha256 IS NOT NULL;
CREATE INDEX idx_fingerprints_pial   ON fingerprints(uploader_pial, created_at DESC);
-- The frozen legacy audio set: imported rows that carry only a chromaprint.
CREATE INDEX idx_fingerprints_legacy_audio ON fingerprints(created_at)
    WHERE origin = 'import' AND chromaprint IS NOT NULL AND perceptual_at IS NULL;

-- ── Visual segment index ─────────────────────────────────────────────────────
-- One row per visual segment: a video contributes one per shot (or per fixed
-- interval, the worker's choice), an image contributes exactly one at t = 0.
-- Images and video share the index on purpose: a screenshot of a video is a
-- derivative of that video, and only a shared index can see it.
CREATE TABLE video_segments (
    id             BIGSERIAL   PRIMARY KEY,
    fingerprint_id UUID        NOT NULL REFERENCES fingerprints(id),
    asset_id       UUID,
    t              REAL        NOT NULL CHECK (t >= 0),
    phash          BIGINT      NOT NULL
);
CREATE INDEX idx_video_segments_fingerprint ON video_segments(fingerprint_id, t);

-- Multi-index hashing over the 64-bit phash: seven bands (six of 9 bits, one
-- of 10), each stored as (band_index << 16) | band_bits. Two hashes within
-- Hamming distance 6 must agree exactly on at least one of seven bands
-- (pigeonhole), so a band lookup finds every candidate inside the credit and
-- block radius with certainty. See src/hashing.rs.
CREATE TABLE segment_bands (
    band       INT    NOT NULL,
    segment_id BIGINT NOT NULL REFERENCES video_segments(id)
);
CREATE INDEX idx_segment_bands_band ON segment_bands(band) INCLUDE (segment_id);

-- ── Audio landmark index ─────────────────────────────────────────────────────
-- The constellation index.
--
--   kind 0  classic: the worker's (f1, f2, dt) pair hashes, 24 bits of payload.
--   kind 1  robust: pitch- and tempo-invariant keys. The worker's robust hashes
--           (frequency ratios and a time ratio over a peak triple) carry only
--           18 bits — far too few to index on alone: past a few hundred hours
--           of audio every value would have thousands of postings. Abraxas
--           therefore indexes PAIRS of robust hashes that share an anchor:
--           both members are invariant, so the pair is invariant, and it
--           carries 36 bits. See index::robust_keys.
--
-- t_offset is the anchor peak's time in milliseconds from the start of the
-- asset. version names the worker's landmark parameter set: landmarks of
-- different versions are different hash functions and are never compared.
-- Hash-partitioned so the index spreads over storage as it grows without a
-- migration of the lookup path.
CREATE TABLE landmark_versions (
    id   SMALLINT PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
    name TEXT     NOT NULL UNIQUE
);

CREATE TABLE audio_landmarks (
    hash     BIGINT   NOT NULL,
    asset_id UUID     NOT NULL,
    t_offset INT      NOT NULL CHECK (t_offset >= 0),
    kind     SMALLINT NOT NULL CHECK (kind IN (0, 1)),
    version  SMALLINT NOT NULL REFERENCES landmark_versions(id)
) PARTITION BY HASH (hash);
CREATE TABLE audio_landmarks_p00 PARTITION OF audio_landmarks FOR VALUES WITH (MODULUS 16, REMAINDER 0);
CREATE TABLE audio_landmarks_p01 PARTITION OF audio_landmarks FOR VALUES WITH (MODULUS 16, REMAINDER 1);
CREATE TABLE audio_landmarks_p02 PARTITION OF audio_landmarks FOR VALUES WITH (MODULUS 16, REMAINDER 2);
CREATE TABLE audio_landmarks_p03 PARTITION OF audio_landmarks FOR VALUES WITH (MODULUS 16, REMAINDER 3);
CREATE TABLE audio_landmarks_p04 PARTITION OF audio_landmarks FOR VALUES WITH (MODULUS 16, REMAINDER 4);
CREATE TABLE audio_landmarks_p05 PARTITION OF audio_landmarks FOR VALUES WITH (MODULUS 16, REMAINDER 5);
CREATE TABLE audio_landmarks_p06 PARTITION OF audio_landmarks FOR VALUES WITH (MODULUS 16, REMAINDER 6);
CREATE TABLE audio_landmarks_p07 PARTITION OF audio_landmarks FOR VALUES WITH (MODULUS 16, REMAINDER 7);
CREATE TABLE audio_landmarks_p08 PARTITION OF audio_landmarks FOR VALUES WITH (MODULUS 16, REMAINDER 8);
CREATE TABLE audio_landmarks_p09 PARTITION OF audio_landmarks FOR VALUES WITH (MODULUS 16, REMAINDER 9);
CREATE TABLE audio_landmarks_p10 PARTITION OF audio_landmarks FOR VALUES WITH (MODULUS 16, REMAINDER 10);
CREATE TABLE audio_landmarks_p11 PARTITION OF audio_landmarks FOR VALUES WITH (MODULUS 16, REMAINDER 11);
CREATE TABLE audio_landmarks_p12 PARTITION OF audio_landmarks FOR VALUES WITH (MODULUS 16, REMAINDER 12);
CREATE TABLE audio_landmarks_p13 PARTITION OF audio_landmarks FOR VALUES WITH (MODULUS 16, REMAINDER 13);
CREATE TABLE audio_landmarks_p14 PARTITION OF audio_landmarks FOR VALUES WITH (MODULUS 16, REMAINDER 14);
CREATE TABLE audio_landmarks_p15 PARTITION OF audio_landmarks FOR VALUES WITH (MODULUS 16, REMAINDER 15);
CREATE INDEX idx_audio_landmarks_hash ON audio_landmarks(hash, kind, version) INCLUDE (asset_id, t_offset);

-- ── Banned registry ──────────────────────────────────────────────────────────
-- sha256, phash and chromaprint rows hold a value. audio_landmarks and
-- video_segments rows point at a fingerprint whose index entries ARE the
-- banned material, so one index serves both ownership and the ban gate. A ban
-- is lifted by revoking it; the row stays as the record that it existed.
CREATE TABLE banned_hashes (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    layer          TEXT        NOT NULL
                               CHECK (layer IN ('sha256','phash','chromaprint',
                                                'audio_landmarks','video_segments')),
    sha256         TEXT        CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    phash          BIGINT,
    chromaprint    TEXT,
    fingerprint_id UUID        REFERENCES fingerprints(id),
    category       TEXT        NOT NULL CHECK (category ~ '^[a-z][a-z_]{1,31}$'),
    added_by       TEXT        NOT NULL,
    note           TEXT        NOT NULL DEFAULT '',
    origin         TEXT        NOT NULL DEFAULT 'admin' CHECK (origin IN ('admin','import')),
    import_ref     TEXT        UNIQUE,
    added_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at     TIMESTAMPTZ,
    revoked_by     TEXT,
    revoked_reason TEXT,
    CONSTRAINT banned_value CHECK (
            (layer = 'sha256')      = (sha256 IS NOT NULL)
        AND (layer = 'phash')       = (phash IS NOT NULL)
        AND (layer = 'chromaprint') = (chromaprint IS NOT NULL)
        AND (layer IN ('audio_landmarks','video_segments')) = (fingerprint_id IS NOT NULL)),
    CONSTRAINT banned_revocation CHECK (
        (revoked_at IS NULL) = (revoked_by IS NULL)
        AND (revoked_at IS NULL OR COALESCE(revoked_reason, '') <> ''))
);
CREATE UNIQUE INDEX uq_banned_sha256      ON banned_hashes(sha256)       WHERE layer = 'sha256'      AND revoked_at IS NULL;
CREATE UNIQUE INDEX uq_banned_phash       ON banned_hashes(phash)        WHERE layer = 'phash'       AND revoked_at IS NULL;
CREATE UNIQUE INDEX uq_banned_chromaprint ON banned_hashes(chromaprint)  WHERE layer = 'chromaprint' AND revoked_at IS NULL;
CREATE UNIQUE INDEX uq_banned_fingerprint ON banned_hashes(fingerprint_id, layer)
    WHERE fingerprint_id IS NOT NULL AND revoked_at IS NULL;

-- ── Ownership ────────────────────────────────────────────────────────────────
-- A new asset found to be a copy, a derivative or an excerpt of an earlier
-- one. The ORIGINAL owner is credited. offsets carries the alignment: where in
-- the new asset the original's material begins, the time scale between them
-- (speed changes), and the spans on both sides.
CREATE TABLE ownership_matches (
    id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    new_fingerprint      UUID        NOT NULL REFERENCES fingerprints(id),
    new_asset            UUID,
    new_pial             UUID,
    original_fingerprint UUID        NOT NULL REFERENCES fingerprints(id),
    original_asset       UUID,
    original_pial        UUID,
    kind                 TEXT        NOT NULL CHECK (kind IN ('exact','derivative','excerpt')),
    layer                TEXT        NOT NULL
                                     CHECK (layer IN ('sha256','phash','video_segments',
                                                      'audio_landmarks','chromaprint','claim')),
    confidence           DOUBLE PRECISION NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
    offset_seconds       DOUBLE PRECISION,
    offsets              JSONB       NOT NULL DEFAULT '{}',
    state                TEXT        NOT NULL
                                     CHECK (state IN ('credited','ml_review','admin_review','rejected')),
    decided_by           TEXT,
    decided_at           TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT ownership_not_self CHECK (new_fingerprint <> original_fingerprint),
    UNIQUE (new_fingerprint, original_fingerprint)
);
CREATE INDEX idx_ownership_original ON ownership_matches(original_pial, created_at DESC);
CREATE INDEX idx_ownership_new      ON ownership_matches(new_asset);

-- ── Scans and verdicts ───────────────────────────────────────────────────────
-- Subjects: a work, a vision (id <pial>.<seq>), an asset (id = asset id), a
-- live stream (id = stream id; each sampled frame is one row of its history).
-- Both tables are histories: the newest row per subject is the current answer.
CREATE TABLE scan_results (
    id           BIGSERIAL   PRIMARY KEY,
    subject_kind TEXT        NOT NULL CHECK (subject_kind IN ('work','vision','asset','stream')),
    subject_id   TEXT        NOT NULL,
    deep         BOOLEAN     NOT NULL DEFAULT FALSE,
    scan_version TEXT        NOT NULL,
    scores       JSONB       NOT NULL DEFAULT '{}',
    labels       TEXT[]      NOT NULL DEFAULT '{}',
    signals      TEXT[]      NOT NULL DEFAULT '{}',
    ocr_text     TEXT,
    transcript   TEXT,
    origin       TEXT        NOT NULL DEFAULT 'scan' CHECK (origin IN ('scan','import')),
    import_ref   TEXT        UNIQUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_scan_results_subject ON scan_results(subject_kind, subject_id, id DESC);

CREATE TABLE verdicts (
    id           BIGSERIAL   PRIMARY KEY,
    subject_kind TEXT        NOT NULL CHECK (subject_kind IN ('work','vision','asset','stream')),
    subject_id   TEXT        NOT NULL,
    verdict      TEXT        NOT NULL CHECK (verdict IN ('approve','age_gate','human_review','block','pending')),
    signals      TEXT[]      NOT NULL DEFAULT '{}',
    scores       JSONB       NOT NULL DEFAULT '{}',
    reason       TEXT        NOT NULL,
    source       TEXT        NOT NULL
                             CHECK (source IN ('check','frame','pipeline','recheck','ml_review','admin','import')),
    decided_by   TEXT        NOT NULL,
    import_ref   TEXT        UNIQUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_verdicts_subject ON verdicts(subject_kind, subject_id, id DESC);

-- ── Intake: content the async pipeline has accepted ──────────────────────────
-- Written from content.events in the same transaction as the scan job, before
-- the offset is committed. body is held only until the subject reaches a
-- final verdict; body_sha256 stays for duplicate detection.
CREATE TABLE subjects (
    kind               TEXT        NOT NULL CHECK (kind IN ('work','vision')),
    id                 TEXT        NOT NULL,
    pial               UUID        NOT NULL,
    body               TEXT,
    body_sha256        TEXT        CHECK (body_sha256 ~ '^[0-9a-f]{64}$'),
    is_adult_creator   BOOLEAN     NOT NULL DEFAULT FALSE,
    account_created_at TIMESTAMPTZ,
    -- Media the event named that does not resolve to an asset. Never ignored:
    -- unresolvable media puts the subject in front of a reviewer.
    unresolved_media   TEXT[]      NOT NULL DEFAULT '{}',
    event_id           TEXT,
    received_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (kind, id)
);
CREATE INDEX idx_subjects_pial_time ON subjects(pial, received_at DESC);
CREATE INDEX idx_subjects_pial_body ON subjects(pial, body_sha256, received_at DESC) WHERE body_sha256 IS NOT NULL;

CREATE TABLE subject_assets (
    subject_kind TEXT NOT NULL,
    subject_id   TEXT NOT NULL,
    asset_id     UUID NOT NULL,
    PRIMARY KEY (subject_kind, subject_id, asset_id),
    FOREIGN KEY (subject_kind, subject_id) REFERENCES subjects(kind, id)
);
CREATE INDEX idx_subject_assets_asset ON subject_assets(asset_id);

-- A content.events record that cannot be parsed or routed. Recorded, then the
-- offset moves: retrying a payload that will never parse would stall the
-- partition forever, and dropping it would be the silent skip.
CREATE TABLE intake_deadletter (
    id          BIGSERIAL   PRIMARY KEY,
    topic       TEXT        NOT NULL,
    partition   INT         NOT NULL,
    "offset"    BIGINT      NOT NULL,
    payload     TEXT        NOT NULL,
    reason      TEXT        NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at TIMESTAMPTZ,
    UNIQUE (topic, partition, "offset")
);

-- ── Review ───────────────────────────────────────────────────────────────────
--   content      a work or vision whose scores sit in the uncertain band
--   banned_near  an asset near (not inside) the banned registry's block radius
--   ownership    a perceptual ownership match below the credit bar
--   unjudged         media the worker could not read; only a person can judge it
--   report           a report a moderator escalated
--   ownership_claim  a person's claim that content is theirs (straight to admin)
-- dedup_key keeps one open item per case. 'withdrawn' closes an item whose
-- report was resolved directly on the report.
CREATE TABLE review_queue (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    item_kind       TEXT        NOT NULL
                                CHECK (item_kind IN ('content','banned_near','ownership','unjudged',
                                                     'report','ownership_claim')),
    subject_kind    TEXT        NOT NULL CHECK (subject_kind IN ('work','vision','asset','stream','account')),
    subject_id      TEXT        NOT NULL,
    ownership_match UUID        REFERENCES ownership_matches(id),
    banned_id       UUID        REFERENCES banned_hashes(id),
    category        TEXT,
    dedup_key       TEXT        NOT NULL,
    stage           TEXT        NOT NULL CHECK (stage IN ('ml_review','admin_review','decided')),
    reason          TEXT        NOT NULL,
    evidence        JSONB       NOT NULL DEFAULT '{}',
    ml_outcome      JSONB,
    report_id       UUID,
    decision        TEXT        CHECK (decision IN ('approve','age_gate','block','credit','reject','withdrawn')),
    decided_by      TEXT,
    note            TEXT,
    origin          TEXT        NOT NULL DEFAULT 'scan' CHECK (origin IN ('scan','import')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    admin_at        TIMESTAMPTZ,
    decided_at      TIMESTAMPTZ,
    CONSTRAINT review_decided CHECK (
        (stage = 'decided') = (decision IS NOT NULL)
        AND (decision IS NULL OR decided_by IS NOT NULL)),
    CONSTRAINT review_ownership CHECK ((item_kind = 'ownership') = (ownership_match IS NOT NULL)),
    CONSTRAINT review_report CHECK ((item_kind IN ('report','ownership_claim')) = (report_id IS NOT NULL))
);
CREATE UNIQUE INDEX uq_review_open ON review_queue(dedup_key) WHERE stage <> 'decided';
CREATE INDEX idx_review_stage ON review_queue(stage, created_at);
CREATE INDEX idx_review_subject ON review_queue(subject_kind, subject_id, created_at DESC);
CREATE INDEX idx_review_report  ON review_queue(report_id) WHERE report_id IS NOT NULL;

-- Reviewer rulings on the signals that raised an item: the feedback loop that
-- suppresses signals reviewers keep overturning.
CREATE TABLE signal_feedback (
    id          BIGSERIAL   PRIMARY KEY,
    review_id   UUID        REFERENCES review_queue(id),
    subject_ref TEXT        NOT NULL,
    signals     TEXT[]      NOT NULL,
    decision    TEXT        NOT NULL CHECK (decision IN ('approve','age_gate','block')),
    decided_by  TEXT        NOT NULL,
    import_ref  TEXT        UNIQUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_signal_feedback_signals ON signal_feedback USING GIN (signals);

-- ── Banned-registry evidence (the CSAM evidence log) ─────────────────────────
-- Every banned-registry finding on every surface, and every upload the
-- perceptual layer could not judge, append-only. csam rows are what the
-- law-enforcement report is built from; the bytes themselves are never kept.
CREATE TABLE csam_evidence_log (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    surface        TEXT        NOT NULL CHECK (surface IN ('upload','live','import')),
    subject_ref    TEXT        NOT NULL,
    asset_id       UUID,
    fingerprint_id UUID        REFERENCES fingerprints(id),
    pial           UUID,
    legacy_pial    TEXT,
    media_kind     TEXT        NOT NULL,
    result         TEXT        NOT NULL CHECK (result IN ('flagged','review','pending_scan','clean','error')),
    match_kind     TEXT        CHECK (match_kind IN ('exact','near')),
    layer          TEXT,
    category       TEXT,
    banned_id      UUID        REFERENCES banned_hashes(id),
    sha256         TEXT,
    score          REAL        NOT NULL DEFAULT 0,
    detail         JSONB       NOT NULL DEFAULT '{}',
    import_ref     TEXT        UNIQUE,
    recorded_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_csam_evidence_flagged ON csam_evidence_log(result, recorded_at DESC) WHERE result <> 'clean';
CREATE INDEX idx_csam_evidence_pial    ON csam_evidence_log(pial, recorded_at DESC);

-- ── Reports (absorbed from zodacare's content_reports) ───────────────────────
-- A report is a CASE: one open case per (subject, kind), however many people
-- file it. Each person's filing is a report_filings row. stage 'queue' is
-- waiting for a moderator; 'admin_review' has a review_queue item (escalated,
-- or an ownership claim, which opens there).
CREATE TABLE reports (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    subject_kind   TEXT        NOT NULL CHECK (subject_kind IN ('work','vision','asset','stream','account')),
    subject_id     TEXT        NOT NULL,
    kind           TEXT        NOT NULL CHECK (kind ~ '^[a-z][a-z_]{1,31}$'),
    reason         TEXT        NOT NULL,
    detail         TEXT,
    -- Whose content it is, when Abraxas knows (an asset's uploader, a work's
    -- author, an account's PIAL). The PIAL a ban would land on.
    reported_pial  UUID,
    status         TEXT        NOT NULL DEFAULT 'open' CHECK (status IN ('open','resolved','dismissed')),
    stage          TEXT        NOT NULL DEFAULT 'queue' CHECK (stage IN ('queue','admin_review')),
    -- Ownership claims: the claimants' own assets that match the subject.
    matched_assets JSONB       NOT NULL DEFAULT '[]',
    action         TEXT        CHECK (action IN ('none','ban')),
    resolved_by    TEXT,
    resolved_note  TEXT,
    resolved_at    TIMESTAMPTZ,
    escalated_at   TIMESTAMPTZ,
    origin         TEXT        NOT NULL DEFAULT 'api' CHECK (origin IN ('api','import')),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT reports_resolution CHECK ((status <> 'open') = (resolved_at IS NOT NULL))
);
CREATE UNIQUE INDEX uq_reports_open ON reports(subject_kind, subject_id, kind) WHERE status = 'open';
CREATE INDEX idx_reports_status   ON reports(status, created_at DESC);
CREATE INDEX idx_reports_reported ON reports(reported_pial);

CREATE TABLE report_filings (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    report_id     UUID        NOT NULL REFERENCES reports(id),
    reporter_pial UUID,
    reason        TEXT        NOT NULL,
    detail        TEXT,
    import_ref    TEXT        UNIQUE,
    filed_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX uq_report_filings_reporter ON report_filings(report_id, reporter_pial) WHERE reporter_pial IS NOT NULL;
CREATE INDEX idx_report_filings_report ON report_filings(report_id, filed_at);

-- ── Enforcement handoff ──────────────────────────────────────────────────────
-- Account actions belong to elohim-veni. A report decision that bans writes a
-- row here in the same transaction; the drain delivers it to
-- POST /v1/pials/{pial}/ban and never drops it.
CREATE TABLE identity_commands (
    id              BIGSERIAL   PRIMARY KEY,
    pial            UUID        NOT NULL,
    command         TEXT        NOT NULL CHECK (command IN ('ban')),
    reason          TEXT        NOT NULL,
    requested_by    TEXT        NOT NULL,
    source_ref      TEXT        NOT NULL UNIQUE,
    attempts        INT         NOT NULL DEFAULT 0,
    last_error      TEXT,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    delivered_at    TIMESTAMPTZ,
    blocked_at      TIMESTAMPTZ,
    blocked_reason  TEXT
);
CREATE INDEX idx_identity_commands_due ON identity_commands(next_attempt_at, id)
    WHERE delivered_at IS NULL AND blocked_at IS NULL;

-- ── safety.events outbox ─────────────────────────────────────────────────────
-- seq is the record's sequence number on the wire. The relay publishes in seq
-- order and marks a row only after the broker has acknowledged it.
CREATE TABLE safety_outbox (
    seq            BIGSERIAL   PRIMARY KEY,
    event_id       UUID        NOT NULL UNIQUE DEFAULT gen_random_uuid(),
    partition_key  TEXT        NOT NULL,
    payload        JSONB       NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    attempts       INT         NOT NULL DEFAULT 0,
    last_error     TEXT,
    published_at   TIMESTAMPTZ,
    blocked_at     TIMESTAMPTZ,
    blocked_reason TEXT
);
CREATE INDEX idx_safety_outbox_pending ON safety_outbox(seq)
    WHERE published_at IS NULL AND blocked_at IS NULL;

-- ── Durable jobs ─────────────────────────────────────────────────────────────
--   asset_recheck  a /v1/check the worker could not answer; payload is the request
--   subject_scan   a work or vision from content.events
--   ml_review      the worker's deep pass over one review item
-- One pending job per (kind, ref). A job that exhausts its attempts is not
-- dropped: it becomes an admin_review item (state 'escalated').
CREATE TABLE jobs (
    id              BIGSERIAL   PRIMARY KEY,
    kind            TEXT        NOT NULL CHECK (kind IN ('asset_recheck','subject_scan','ml_review')),
    ref             TEXT        NOT NULL,
    payload         JSONB       NOT NULL DEFAULT '{}',
    state           TEXT        NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','done','escalated')),
    attempts        INT         NOT NULL DEFAULT 0,
    last_error      TEXT,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at     TIMESTAMPTZ
);
CREATE UNIQUE INDEX uq_jobs_pending ON jobs(kind, ref) WHERE state = 'pending';
CREATE INDEX idx_jobs_due ON jobs(next_attempt_at, id) WHERE state = 'pending';

-- ── Import bookkeeping ───────────────────────────────────────────────────────
-- A source row the import could not carry over, with the reason. Imported plus
-- rejected must equal the source count, or the import exits non-zero.
CREATE TABLE import_rejects (
    source      TEXT        NOT NULL,
    source_id   TEXT        NOT NULL,
    reason      TEXT        NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (source, source_id)
);

-- Moderation transitions feed-engine recorded before this plane existed,
-- carried over verbatim (their state vocabulary is feed-engine's, not ours).
CREATE TABLE moderation_history (
    id         UUID        PRIMARY KEY,
    content_id TEXT        NOT NULL,
    from_state TEXT        NOT NULL,
    to_state   TEXT        NOT NULL,
    reason     TEXT        NOT NULL,
    actor      TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_moderation_history_content ON moderation_history(content_id, created_at DESC);

-- ── Permanence ───────────────────────────────────────────────────────────────
CREATE FUNCTION integrity_refuse_delete() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% is permanent: integrity records are revoked or superseded, never deleted', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER fingerprints_permanent        BEFORE DELETE OR TRUNCATE ON fingerprints        FOR EACH STATEMENT EXECUTE FUNCTION integrity_refuse_delete();
CREATE TRIGGER video_segments_permanent      BEFORE DELETE OR TRUNCATE ON video_segments      FOR EACH STATEMENT EXECUTE FUNCTION integrity_refuse_delete();
CREATE TRIGGER segment_bands_permanent       BEFORE DELETE OR TRUNCATE ON segment_bands       FOR EACH STATEMENT EXECUTE FUNCTION integrity_refuse_delete();
CREATE TRIGGER audio_landmarks_permanent     BEFORE DELETE OR TRUNCATE ON audio_landmarks     FOR EACH STATEMENT EXECUTE FUNCTION integrity_refuse_delete();
CREATE TRIGGER banned_hashes_permanent       BEFORE DELETE OR TRUNCATE ON banned_hashes       FOR EACH STATEMENT EXECUTE FUNCTION integrity_refuse_delete();
CREATE TRIGGER ownership_matches_permanent   BEFORE DELETE OR TRUNCATE ON ownership_matches   FOR EACH STATEMENT EXECUTE FUNCTION integrity_refuse_delete();
CREATE TRIGGER verdicts_permanent            BEFORE DELETE OR TRUNCATE ON verdicts            FOR EACH STATEMENT EXECUTE FUNCTION integrity_refuse_delete();
CREATE TRIGGER scan_results_permanent        BEFORE DELETE OR TRUNCATE ON scan_results        FOR EACH STATEMENT EXECUTE FUNCTION integrity_refuse_delete();
CREATE TRIGGER csam_evidence_log_permanent   BEFORE DELETE OR TRUNCATE ON csam_evidence_log   FOR EACH STATEMENT EXECUTE FUNCTION integrity_refuse_delete();
CREATE TRIGGER review_queue_permanent        BEFORE DELETE OR TRUNCATE ON review_queue        FOR EACH STATEMENT EXECUTE FUNCTION integrity_refuse_delete();
CREATE TRIGGER reports_permanent             BEFORE DELETE OR TRUNCATE ON reports             FOR EACH STATEMENT EXECUTE FUNCTION integrity_refuse_delete();
CREATE TRIGGER report_filings_permanent      BEFORE DELETE OR TRUNCATE ON report_filings      FOR EACH STATEMENT EXECUTE FUNCTION integrity_refuse_delete();
