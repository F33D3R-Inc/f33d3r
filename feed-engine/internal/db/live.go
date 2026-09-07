package db

// live.go — the live lane's persistence. Stream lifecycle, ingest credentials,
// viewer and scan samples.
//
// Nothing here carries video. The media server owns ingest, transcoding,
// packaging and delivery; this file owns only the state the application server
// is authoritative for: who may publish, what status a stream is in, how many
// viewers the server counted, and what the content-safety path concluded.
//
// Status is server-owned and moves one way: idle -> live -> ended.

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/f33d3r/feed-engine/internal/model"
)

// ErrLiveStreamNotFound is returned when no stream matches the identifier.
var ErrLiveStreamNotFound = errors.New("live: stream not found")

// ErrLiveStreamEnded is returned when an operation is attempted against a
// stream that has already finished. Ended is terminal.
var ErrLiveStreamEnded = errors.New("live: stream already ended")

// ── identifiers ──────────────────────────────────────────────────────────────

// checkStreamID is the one gate every stream identifier passes before it is
// allowed near the database.
//
// live_streams.id is a uuid column, so an identifier that is not a UUID cannot
// name a row. Handing one to Postgres anyway is not a lookup that misses — it is
// a malformed literal, and the driver answers with a hard error that every
// caller then reports as a server fault for what is in truth a client's bad path
// segment. A value that cannot name a row is a not-found, and it is answered as
// one here, before a query is ever built.
//
// Every live route that takes {id} — watch, broadcast, heartbeat, chat, end,
// encoder reveal and the WHIP publish proxy — reaches its stream through this
// package, so this one gate covers all of them.
func checkStreamID(streamID string) error {
	return checkUUID(streamID)
}

// checkAuthorID is the same gate for the author identity a stream is looked up
// by. users.id is a uuid column too, and a session carrying anything else (the
// demo identity, a truncated cookie) must read as "holds no stream", never as a
// broken server.
func checkAuthorID(authorID string) error {
	return checkUUID(authorID)
}

// checkUUID accepts only the canonical 36-character hyphenated form — the exact
// shape this application mints (uuid.New().String()) and the exact shape
// Postgres renders back (id::text). uuid.Parse alone is too generous for a gate:
// it also accepts the URN form, which Postgres rejects outright, so a value that
// passed would still land as a 500. Fixing the length first closes that gap and
// leaves parse to reject everything else.
func checkUUID(id string) error {
	if len(id) != 36 {
		return ErrLiveStreamNotFound
	}
	if _, err := uuid.Parse(id); err != nil {
		return ErrLiveStreamNotFound
	}
	return nil
}

// ── URL shapes ───────────────────────────────────────────────────────────────
//
// One definition of the delivery URL shape, used by every caller. These paths
// are served by the live edge, not by this process.

// LiveMasterPlaylistURL is the ABR master playlist for a stream.
func LiveMasterPlaylistURL(streamID string) string {
	return "/live/" + streamID + "/master.m3u8"
}

// LiveVariantPlaylistURL is one rung of the ABR ladder.
func LiveVariantPlaylistURL(streamID string, height int) string {
	return fmt.Sprintf("/live/%s/%dp/index.m3u8", streamID, height)
}

// LivePosterURL is the periodically refreshed still frame for a stream.
func LivePosterURL(streamID string) string {
	return "/live/" + streamID + "/poster.jpg"
}

// ── ingest keys ──────────────────────────────────────────────────────────────

// ingestKeyBytes is the entropy behind one publish secret. 32 bytes of
// crypto/rand — not sequential, not derived from the stream id, not derived
// from the user id, and not recoverable from the database.
const ingestKeyBytes = 32

// ingestKeyEncoding renders the key as lowercase base32 with no padding, so it
// survives an RTMP query string, an SRT streamid and a WHIP URL unescaped.
var ingestKeyEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewIngestKey mints one publish secret. The plaintext is returned exactly once
// and only its SHA-256 is ever stored.
func NewIngestKey() (string, error) {
	buf := make([]byte, ingestKeyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("live: ingest key entropy: %w", err)
	}
	return strings.ToLower(ingestKeyEncoding.EncodeToString(buf)), nil
}

// HashIngestKey is the one-way transform applied before storage.
func HashIngestKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// ── reads ────────────────────────────────────────────────────────────────────

// liveSelectSQL is the single definition of a readable stream row.
// Column order must stay in sync with scanLiveStream.
const liveSelectSQL = `
SELECT
    s.id::text, s.author_id::text, s.author_pial::text, s.title, s.description, s.status,
    s.started_at, s.ended_at, s.viewer_count, s.peak_viewers,
    s.poster_url, s.playlist_url, s.is_nsfw, s.is_blocked, s.scan_state,
    s.source_width, s.source_height, s.ingest_proto, s.created_at,
    s.ladder, s.encoder, s.degraded, s.capacity_note,
    s.audience, s.lane, s.tip_goal_uaet, s.tip_total_uaet, s.pinned_body,
    s.heart_count, s.chat_lines, s.save_replay,
    u.handle, COALESCE(up.display_name, ''), COALESCE(up.avatar_url, '')
FROM live_streams s
JOIN users u ON u.id = s.author_id
LEFT JOIN user_profiles up ON up.user_id = s.author_id
`

type liveRowScanner interface {
	Scan(dest ...interface{}) error
}

func scanLiveStream(row liveRowScanner) (*model.LiveStream, error) {
	var s model.LiveStream
	var startedAt, endedAt sql.NullTime
	if err := row.Scan(
		&s.ID, &s.AuthorID, &s.AuthorPIAL, &s.Title, &s.Description, &s.Status,
		&startedAt, &endedAt, &s.ViewerCount, &s.PeakViewers,
		&s.PosterURL, &s.PlaylistURL, &s.IsNSFW, &s.IsBlocked, &s.ScanState,
		&s.SourceWidth, &s.SourceHeight, &s.IngestProto, &s.CreatedAt,
		&s.Ladder, &s.Encoder, &s.Degraded, &s.CapacityNote,
		&s.Audience, &s.Lane, &s.TipGoalUAET, &s.TipTotalUAET, &s.PinnedBody,
		&s.HeartCount, &s.ChatLines, &s.SaveReplay,
		&s.AuthorHandle, &s.AuthorName, &s.AvatarURL,
	); err != nil {
		return nil, err
	}
	if startedAt.Valid {
		t := startedAt.Time
		s.StartedAt = &t
	}
	if endedAt.Valid {
		t := endedAt.Time
		s.EndedAt = &t
	}
	return &s, nil
}

// GetLiveStreamByID returns one stream regardless of status, so an ended stream
// still renders its own page instead of vanishing.
func GetLiveStreamByID(database *sql.DB, streamID string) (*model.LiveStream, error) {
	if database == nil {
		return nil, errors.New("live: no database")
	}
	if err := checkStreamID(streamID); err != nil {
		return nil, err
	}
	row := database.QueryRow(liveSelectSQL+` WHERE s.id = $1::uuid`, streamID)
	s, err := scanLiveStream(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrLiveStreamNotFound
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

// GetActiveStreams returns streams currently broadcasting, newest first.
// A blocked stream is never returned: the same rule the Work lane applies.
// limit <= 0 means the default page of 50.
func GetActiveStreams(database *sql.DB, limit int) ([]*model.LiveStream, error) {
	if database == nil {
		return nil, errors.New("live: no database")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := database.Query(liveSelectSQL+`
		WHERE s.status = 'live'
		  AND s.is_blocked = FALSE
		  AND s.scan_state != 'blocked'
		ORDER BY s.started_at DESC NULLS LAST
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*model.LiveStream, 0, limit)
	for rows.Next() {
		s, err := scanLiveStream(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetStreamForAuthor returns the author's one open stream (idle or live), or
// ErrLiveStreamNotFound when they hold none. Ended streams are history and are
// not returned here.
func GetStreamForAuthor(database *sql.DB, authorID string) (*model.LiveStream, error) {
	if database == nil {
		return nil, errors.New("live: no database")
	}
	if err := checkAuthorID(authorID); err != nil {
		return nil, err
	}
	row := database.QueryRow(liveSelectSQL+`
		WHERE s.author_id = $1::uuid AND s.status IN ('idle', 'live')`, authorID)
	s, err := scanLiveStream(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrLiveStreamNotFound
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

// ── writes ───────────────────────────────────────────────────────────────────

// CreateStream opens a stream for an author and mints its ingest key.
//
// The returned plaintext key is the only copy that will ever exist; the row
// stores its SHA-256. When the author already holds an open stream, that stream
// is returned instead and the key string is empty — the existing key is still
// valid and cannot be read back. Call RotateIngestKey to mint a fresh one.
func CreateStream(database *sql.DB, authorID, authorPIAL, title string, isNSFW bool) (*model.LiveStream, string, error) {
	if database == nil {
		return nil, "", errors.New("live: no database")
	}
	if authorID == "" || authorPIAL == "" {
		return nil, "", errors.New("live: author identity required")
	}
	// Both identities are uuid columns on the row about to be written. A session
	// carrying anything else is a broken caller, not a malformed request, so it
	// is named plainly here rather than reaching Postgres as a syntax error.
	if err := checkAuthorID(authorID); err != nil {
		return nil, "", fmt.Errorf("live: author id %q is not an account id", authorID)
	}
	if err := checkUUID(authorPIAL); err != nil {
		return nil, "", fmt.Errorf("live: author pial %q is not a PIAL id", authorPIAL)
	}

	if existing, err := GetStreamForAuthor(database, authorID); err == nil {
		return existing, "", nil
	} else if !errors.Is(err, ErrLiveStreamNotFound) {
		return nil, "", err
	}

	key, err := NewIngestKey()
	if err != nil {
		return nil, "", err
	}

	tx, err := database.Begin()
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()

	var streamID string
	// playlist_url and poster_url are derived from the id the same statement
	// generates, so a stream is never persisted without its delivery URLs.
	err = tx.QueryRow(`
		INSERT INTO live_streams (author_id, author_pial, title, is_nsfw)
		VALUES ($1::uuid, $2::uuid, $3, $4)
		RETURNING id::text`,
		authorID, authorPIAL, strings.TrimSpace(title), isNSFW,
	).Scan(&streamID)
	if err != nil {
		return nil, "", fmt.Errorf("live: insert stream: %w", err)
	}

	if _, err := tx.Exec(`
		UPDATE live_streams SET playlist_url = $2, poster_url = $3, updated_at = NOW()
		WHERE id = $1::uuid`,
		streamID, LiveMasterPlaylistURL(streamID), LivePosterURL(streamID),
	); err != nil {
		return nil, "", fmt.Errorf("live: set delivery urls: %w", err)
	}

	if _, err := tx.Exec(`
		INSERT INTO live_ingest_keys (stream_id, key_hash) VALUES ($1::uuid, $2)`,
		streamID, HashIngestKey(key),
	); err != nil {
		return nil, "", fmt.Errorf("live: insert ingest key: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, "", err
	}

	s, err := GetLiveStreamByID(database, streamID)
	if err != nil {
		return nil, "", err
	}
	return s, key, nil
}

// RecordStreamSource writes what the ladder measured about a publisher —
// geometry and ingest protocol — without moving the stream's status.
//
// It exists because "the source has been probed" and "viewers can watch" are
// two different moments, twelve to twenty-five seconds apart, and the row used
// to say live at the first of them. The ladder reports its measurements here
// when it asks for its plan; the status moves in StartStream, and only once
// the first segment is on disk.
//
// A stream that has ended is refused: a ladder that probes a publisher who
// reconnected to a finished broadcast must not quietly resurrect its geometry.
func RecordStreamSource(database *sql.DB, streamID string, sourceWidth, sourceHeight int, proto string) error {
	if database == nil {
		return errors.New("live: no database")
	}
	if err := checkStreamID(streamID); err != nil {
		return err
	}
	res, err := database.Exec(`
		UPDATE live_streams
		   SET source_width  = $2,
		       source_height = $3,
		       ingest_proto  = $4,
		       updated_at    = NOW()
		 WHERE id = $1::uuid AND status IN ('idle', 'live')`,
		streamID, sourceWidth, sourceHeight, proto)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var status string
		if err := database.QueryRow(`SELECT status FROM live_streams WHERE id = $1::uuid`, streamID).Scan(&status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrLiveStreamNotFound
			}
			return err
		}
		return ErrLiveStreamEnded
	}
	return nil
}

// StartStream moves idle -> live. It is called by the media server lifecycle
// hook when the first segment of the ladder is on disk — the moment a viewer
// handed the playlist URL will actually receive video — never by a client
// asking to be live, and never merely because a publisher connected.
// Calling it on an already-live stream refreshes the observed geometry and is
// not an error, so a ladder restart is idempotent.
func StartStream(database *sql.DB, streamID string, sourceWidth, sourceHeight int, proto string) error {
	if database == nil {
		return errors.New("live: no database")
	}
	if err := checkStreamID(streamID); err != nil {
		return err
	}
	res, err := database.Exec(`
		UPDATE live_streams
		   SET status        = 'live',
		       started_at    = COALESCE(started_at, NOW()),
		       ended_at      = NULL,
		       source_width  = $2,
		       source_height = $3,
		       ingest_proto  = $4,
		       updated_at    = NOW()
		 WHERE id = $1::uuid AND status IN ('idle', 'live')`,
		streamID, sourceWidth, sourceHeight, proto)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// Either unknown or already ended — distinguish, never guess.
		var status string
		if err := database.QueryRow(`SELECT status FROM live_streams WHERE id = $1::uuid`, streamID).Scan(&status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrLiveStreamNotFound
			}
			return err
		}
		return ErrLiveStreamEnded
	}
	return nil
}

// EndStream moves a stream to ended and zeroes the live viewer count.
// Ended is terminal: peak_viewers survives, viewer_count does not.
func EndStream(database *sql.DB, streamID string) error {
	if database == nil {
		return errors.New("live: no database")
	}
	if err := checkStreamID(streamID); err != nil {
		return err
	}
	res, err := database.Exec(`
		UPDATE live_streams
		   SET status       = 'ended',
		       ended_at     = COALESCE(ended_at, NOW()),
		       viewer_count = 0,
		       updated_at   = NOW()
		 WHERE id = $1::uuid AND status != 'ended'`, streamID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var exists bool
		if err := database.QueryRow(`SELECT TRUE FROM live_streams WHERE id = $1::uuid`, streamID).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrLiveStreamNotFound
			}
			return err
		}
		// Already ended — the hook fired twice. Not an error.
	}
	return nil
}

// UpdateViewerCount records the server's count for a live stream and raises
// peak_viewers when the count exceeds it. A sample row is appended so the peak
// is auditable rather than asserted.
func UpdateViewerCount(database *sql.DB, streamID string, count int) error {
	if database == nil {
		return errors.New("live: no database")
	}
	if err := checkStreamID(streamID); err != nil {
		return err
	}
	if count < 0 {
		return fmt.Errorf("live: negative viewer count %d", count)
	}
	res, err := database.Exec(`
		UPDATE live_streams
		   SET viewer_count = $2,
		       peak_viewers = GREATEST(peak_viewers, $2),
		       updated_at   = NOW()
		 WHERE id = $1::uuid AND status = 'live'`, streamID, count)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLiveStreamNotFound
	}
	_, err = database.Exec(`
		INSERT INTO live_viewer_samples (stream_id, viewer_count) VALUES ($1::uuid, $2)`,
		streamID, count)
	return err
}

// RotateIngestKey mints a fresh publish secret and invalidates the previous one
// immediately. Returns the plaintext key, which is never stored and never shown
// again. An ended stream cannot be republished, so it cannot be rotated.
func RotateIngestKey(database *sql.DB, streamID string) (string, error) {
	if database == nil {
		return "", errors.New("live: no database")
	}
	if err := checkStreamID(streamID); err != nil {
		return "", err
	}
	var status string
	if err := database.QueryRow(`SELECT status FROM live_streams WHERE id = $1::uuid`, streamID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrLiveStreamNotFound
		}
		return "", err
	}
	if status == model.LiveStatusEnded {
		return "", ErrLiveStreamEnded
	}

	key, err := NewIngestKey()
	if err != nil {
		return "", err
	}
	if _, err := database.Exec(`
		INSERT INTO live_ingest_keys (stream_id, key_hash, rotated_at)
		VALUES ($1::uuid, $2, NOW())
		ON CONFLICT (stream_id) DO UPDATE
		   SET key_hash = EXCLUDED.key_hash, rotated_at = NOW(), last_used = NULL`,
		streamID, HashIngestKey(key),
	); err != nil {
		return "", err
	}
	return key, nil
}

// SetStreamMeta updates the broadcaster-authored text on an open stream. Title
// and description are metadata, so they are editable while the stream runs and
// frozen once it ends.
func SetStreamMeta(database *sql.DB, streamID, title, description string) error {
	if database == nil {
		return errors.New("live: no database")
	}
	if err := checkStreamID(streamID); err != nil {
		return err
	}
	res, err := database.Exec(`
		UPDATE live_streams SET title = $2, description = $3, updated_at = NOW()
		 WHERE id = $1::uuid AND status != 'ended'`,
		streamID, strings.TrimSpace(title), strings.TrimSpace(description))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLiveStreamNotFound
	}
	return nil
}

// ── ingest authorisation ─────────────────────────────────────────────────────

// AuthorizeIngest decides whether a publish attempt against streamID carrying
// key may proceed. It is the only place that answers that question.
//
// A stream may be published to when it exists, is not ended, is not blocked,
// and the presented key matches the stored hash under a constant-time compare.
// On success last_used is stamped so an unused key is visible as such.
func AuthorizeIngest(database *sql.DB, streamID, key string) (*model.LiveStream, error) {
	if database == nil {
		return nil, errors.New("live: no database")
	}
	if key == "" {
		return nil, ErrLiveStreamNotFound
	}
	if err := checkStreamID(streamID); err != nil {
		return nil, err
	}

	var storedHash, status string
	var isBlocked bool
	err := database.QueryRow(`
		SELECT k.key_hash, s.status, s.is_blocked
		  FROM live_ingest_keys k
		  JOIN live_streams s ON s.id = k.stream_id
		 WHERE k.stream_id = $1::uuid`, streamID).Scan(&storedHash, &status, &isBlocked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrLiveStreamNotFound
	}
	if err != nil {
		return nil, err
	}

	// Constant-time — a publish endpoint is a timing oracle otherwise.
	if subtle.ConstantTimeCompare([]byte(storedHash), []byte(HashIngestKey(key))) != 1 {
		return nil, ErrLiveStreamNotFound
	}
	if status == model.LiveStatusEnded {
		return nil, ErrLiveStreamEnded
	}
	if isBlocked {
		return nil, errors.New("live: stream is blocked")
	}

	if _, err := database.Exec(
		`UPDATE live_ingest_keys SET last_used = NOW() WHERE stream_id = $1::uuid`, streamID); err != nil {
		return nil, err
	}
	return GetLiveStreamByID(database, streamID)
}

// ── content safety ───────────────────────────────────────────────────────────

// SetStreamScanState transitions a live stream's scan_state and mirrors the
// terminal states onto the flags the read paths filter on. It is the live-lane
// twin of SetPostScanState and writes the same moderation log.
func SetStreamScanState(database *sql.DB, streamID, toState, reason, actor string) error {
	if database == nil {
		return errors.New("live: no database")
	}
	if err := checkStreamID(streamID); err != nil {
		return err
	}
	var fromState string
	if err := database.QueryRow(
		`SELECT scan_state FROM live_streams WHERE id = $1::uuid`, streamID).Scan(&fromState); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLiveStreamNotFound
		}
		return err
	}
	if fromState == toState {
		return nil
	}
	if _, err := database.Exec(
		`UPDATE live_streams SET scan_state = $2, updated_at = NOW() WHERE id = $1::uuid`,
		streamID, toState); err != nil {
		return err
	}
	if toState == "blocked" {
		if _, err := database.Exec(
			`UPDATE live_streams SET is_blocked = TRUE WHERE id = $1::uuid`, streamID); err != nil {
			return err
		}
	}
	if toState == "age_gated" {
		if _, err := database.Exec(
			`UPDATE live_streams SET is_nsfw = TRUE WHERE id = $1::uuid`, streamID); err != nil {
			return err
		}
	}
	if _, err := database.Exec(
		`INSERT INTO content_moderation_log (post_id, from_state, to_state, reason, actor)
		 VALUES ($1, $2, $3, $4, $5)`,
		streamID, fromState, toState, reason, actor); err != nil {
		return fmt.Errorf("live: moderation log: %w", err)
	}
	return nil
}

// LiveScanSample is one keyframe pulled out of a running ladder and put through
// the content-safety path.
type LiveScanSample struct {
	StreamID  string
	FrameName string
	SHA256    string
	Verdict   string
	RiskLevel string
	Signals   []string
}

// RecordScanSample stores the outcome for one sampled frame. Re-recording the
// same frame overwrites the verdict rather than duplicating the evidence.
func RecordScanSample(database *sql.DB, s LiveScanSample) error {
	if database == nil {
		return errors.New("live: no database")
	}
	if err := checkStreamID(s.StreamID); err != nil {
		return err
	}
	signals := s.Signals
	if signals == nil {
		signals = []string{}
	}
	_, err := database.Exec(`
		INSERT INTO live_scan_samples (stream_id, frame_name, sha256, verdict, risk_level, signals)
		VALUES ($1::uuid, $2, $3, $4, $5, $6)
		ON CONFLICT (stream_id, frame_name) DO UPDATE
		   SET sha256 = EXCLUDED.sha256, verdict = EXCLUDED.verdict,
		       risk_level = EXCLUDED.risk_level, signals = EXCLUDED.signals`,
		s.StreamID, s.FrameName, s.SHA256, s.Verdict, s.RiskLevel, pq.Array(signals))
	return err
}

// ── reconciliation ───────────────────────────────────────────────────────────

// ListLiveStreamIDs returns the ids of every stream the database believes is
// broadcasting, so the media server can be asked whether that is still true.
func ListLiveStreamIDs(database *sql.DB) ([]string, error) {
	if database == nil {
		return nil, errors.New("live: no database")
	}
	rows, err := database.Query(`SELECT id::text FROM live_streams WHERE status = 'live'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// StaleIdleStreams returns open streams that were created before cutoff and
// never went live, so an abandoned "go live" click does not hold an author's
// one open slot forever.
func StaleIdleStreams(database *sql.DB, olderThan time.Duration) ([]string, error) {
	if database == nil {
		return nil, errors.New("live: no database")
	}
	rows, err := database.Query(`
		SELECT id::text FROM live_streams
		 WHERE status = 'idle' AND created_at < NOW() - $1::interval`,
		fmt.Sprintf("%d seconds", int(olderThan.Seconds())))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ── capacity truth ───────────────────────────────────────────────────────────
//
// A row that says "live" while nothing is being encoded is not a row with stale
// data in it; it is the platform lying to a broadcaster about whether they are
// on air. These two writes are what keep it honest, and they are called from
// the only place that knows the answer: the control plane, at the instant it
// issues an encode plan or refuses one.

// RecordStreamLadder writes what is actually being produced for a stream.
//
// It is deliberately separate from StartStream. StartStream records the source
// — geometry, protocol — which is a fact about the publisher. This records the
// ladder, which is a decision the platform took about how much of the box it
// was willing to spend, and that decision is retaken every time the ladder
// restarts under a reduced plan. Folding the two together would mean the second
// could not be updated without rewriting the first.
func RecordStreamLadder(database *sql.DB, streamID, ladder, encoder string, degraded bool, note string) error {
	if database == nil {
		return errors.New("live: no database")
	}
	if err := checkStreamID(streamID); err != nil {
		return err
	}
	_, err := database.Exec(`
		UPDATE live_streams
		   SET ladder        = $2,
		       encoder       = $3,
		       degraded      = $4,
		       capacity_note = $5,
		       updated_at    = NOW()
		 WHERE id = $1::uuid AND status <> 'ended'`,
		streamID, ladder, encoder, degraded, note)
	return err
}

// RecordStreamCapacityRefusal records that a publish was turned away because
// the platform had no room for it.
//
// The broadcaster's encoder learns this as a rejected connection and nothing
// more — RTMP has no channel for a reason and OBS shows "failed to connect",
// which is indistinguishable from a wrong key, a dead server or a broken
// network. The reason has to reach them some other way, so it is written here,
// against the stream they were trying to publish into, for the broadcast
// surface to render. A refusal the platform cannot explain is a refusal the
// broadcaster will read as a bug and retry into forever.
//
// The status is not touched. The stream stays idle: nothing about it has ended,
// and the moment the box has room the same ingest key works.
func RecordStreamCapacityRefusal(database *sql.DB, streamID, note string) error {
	if database == nil {
		return errors.New("live: no database")
	}
	if err := checkStreamID(streamID); err != nil {
		return err
	}
	_, err := database.Exec(`
		UPDATE live_streams
		   SET capacity_note = $2,
		       degraded      = false,
		       ladder        = '',
		       encoder       = '',
		       updated_at    = NOW()
		 WHERE id = $1::uuid AND status = 'idle'`,
		streamID, note)
	return err
}
