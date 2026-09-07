package db

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/f33d3r/feed-engine/internal/model"
)

// ErrVisionNotLive is returned when a Vision exists nowhere the viewer can
// reach it: unknown, soft-deleted, or past its expiry.
var ErrVisionNotLive = errors.New("vision: not live")

// DefaultVisionTTL is the lifetime of a Vision when the caller names none.
const DefaultVisionTTL = 24 * time.Hour

// visionMediaPurgeGrace keeps the object reachable slightly past expiry so a
// view already in flight does not break mid-render.
const visionMediaPurgeGrace = time.Hour

// visionLiveSQL is the single definition of "readable Vision". Every read
// path in this file carries it, including the single-address fetch.
const visionLiveSQL = `(v.deleted_at IS NULL AND v.expires_at > NOW())`

// Closed value sets, mirrored by CHECK constraints in migration 0023.
var (
	visionContentTypes = map[string]bool{"text": true, "image": true, "video": true, "share": true}
	visionTypeScales   = map[string]bool{"auto": true, "s": true, "m": true, "l": true, "xl": true}
	visionAligns       = map[string]bool{"left": true, "center": true, "right": true}
	visionMediaKinds   = map[string]bool{"image": true, "video": true}
)

// VisionRef is a Vision's address. A Vision has no id of its own: it is a
// position in one PIAL's vision lane, addressed as (author_pial, seq) — the
// PIAL that owns the lane and the ordinal the server handed out from that
// lane's counter. The canonical text form is `<author_pial>.<seq>`, and that
// string is what the wire carries everywhere an id used to go.
type VisionRef struct {
	AuthorPIAL string
	Seq        int64
}

// String is the canonical text form, `<author_pial>.<seq>`.
func (r VisionRef) String() string {
	return r.AuthorPIAL + "." + strconv.FormatInt(r.Seq, 10)
}

// ParseVisionRef reads the canonical text form. A malformed address is an
// error, never a zero-valued ref.
func ParseVisionRef(s string) (VisionRef, error) {
	dot := strings.LastIndexByte(s, '.')
	if dot <= 0 || dot == len(s)-1 {
		return VisionRef{}, fmt.Errorf("vision address %q: want <author_pial>.<seq>", s)
	}
	pial, seqText := s[:dot], s[dot+1:]
	seq, err := strconv.ParseInt(seqText, 10, 64)
	if err != nil || seq <= 0 {
		return VisionRef{}, fmt.Errorf("vision address %q: seq must be a positive integer", s)
	}
	return VisionRef{AuthorPIAL: pial, Seq: seq}, nil
}

// VisionMediaInput is one stored object backing a Vision.
type VisionMediaInput struct {
	ObjectKey    string
	CaeorAssetID string
	AssetURL     string
	MediaKind    string // image | video
	Width        int
	Height       int
	DurationSecs float32
}

// VisionInput is everything needed to create one Vision and its media rows.
type VisionInput struct {
	AuthorPIAL string

	ContentType string
	Body        string

	ArtboardBackground string
	ArtboardTypeface   string
	ArtboardTypeScale  string
	ArtboardAlign      string

	MediaURLs    []string
	SharedWorkID string
	IsNSFW       bool

	// The room (migration 0022). PollOptions nil or empty means no poll;
	// Audience "" means everyone; AllowReplies is stored as given.
	PollOptions  []string
	Audience     string
	AllowReplies bool

	TTL      time.Duration
	Metadata []byte
	Media    []VisionMediaInput
}

var visionAudiences = map[string]bool{"everyone": true, "followers": true}

// VisionPollMaxOptions bounds a Vision poll the way the work poll is bounded.
const VisionPollMaxOptions = 4

// VisionMediaRef is one stored object due for purge.
type VisionMediaRef struct {
	ID           string
	AuthorPIAL   string
	Seq          int64
	ObjectKey    string
	CaeorAssetID string
	AssetURL     string
	PurgeAfter   time.Time
}

// $1 is always the viewer PIAL, so the viewed flag is resolved in the same
// query as the row it belongs to.
const visionSelectSQL = `
SELECT
    v.author_pial::text, v.seq, v.content_type, v.body,
    v.artboard_background, v.artboard_typeface, v.artboard_type_scale, v.artboard_align,
    v.media_urls, COALESCE(v.shared_work_id::text, ''), v.scan_state,
    v.is_nsfw, v.is_blocked, v.created_at, v.expires_at, v.metadata,
    u.handle, COALESCE(up.display_name, ''), COALESCE(up.avatar_url, ''),
    (vv.viewer_pial IS NOT NULL),
    (SELECT COUNT(*) FROM vision_views c WHERE c.author_pial = v.author_pial AND c.seq = v.seq)::int,
    v.poll_options, v.audience, v.allow_replies,
    COALESCE((SELECT p.option_idx FROM vision_poll_votes p
               WHERE p.author_pial = v.author_pial AND p.seq = v.seq AND p.voter_pial = $1::uuid), -1)
FROM visions v
JOIN users u ON u.pial_id = v.author_pial
LEFT JOIN user_profiles up ON up.user_id = u.id
LEFT JOIN vision_views vv ON vv.author_pial = v.author_pial AND vv.seq = v.seq AND vv.viewer_pial = $1::uuid
`

// scanVision scans one row from visionSelectSQL. Column order must stay in sync.
func scanVision(rows *sql.Rows) (*model.Vision, error) {
	var v model.Vision
	var mediaURLs, pollOptions pq.StringArray
	var metadata []byte

	if err := rows.Scan(
		&v.AuthorPIAL, &v.Seq, &v.ContentType, &v.Body,
		&v.ArtboardBackground, &v.ArtboardTypeface, &v.ArtboardTypeScale, &v.ArtboardAlign,
		&mediaURLs, &v.SharedWorkID, &v.ScanState,
		&v.IsNSFW, &v.IsBlocked, &v.CreatedAt, &v.ExpiresAt, &metadata,
		&v.AuthorHandle, &v.AuthorName, &v.AvatarURL,
		&v.ViewedByViewer, &v.ViewCount,
		&pollOptions, &v.Audience, &v.AllowReplies, &v.ViewerVote,
	); err != nil {
		return nil, err
	}
	v.MediaURLs = []string(mediaURLs)
	v.PollOptions = []string(pollOptions)
	v.Metadata = metadata
	return &v, nil
}

// nullUUID turns an empty identifier into a SQL NULL. An empty string is not a
// UUID and must never reach the driver as one.
func nullUUID(id string) interface{} {
	if id == "" {
		return nil
	}
	return id
}

// applyVisionDefaults fills unset presets and rejects values outside the
// closed sets the schema enforces, so a bad value fails here with a readable
// error instead of as a constraint violation.
func (in *VisionInput) applyVisionDefaults() error {
	if in.AuthorPIAL == "" {
		return errors.New("vision: author_pial is required")
	}
	if in.ContentType == "" {
		in.ContentType = "text"
	}
	if !visionContentTypes[in.ContentType] {
		return fmt.Errorf("vision: unknown content_type %q", in.ContentType)
	}
	if in.ArtboardBackground == "" {
		in.ArtboardBackground = "void"
	}
	if in.ArtboardTypeface == "" {
		in.ArtboardTypeface = "grotesk"
	}
	if in.ArtboardTypeScale == "" {
		in.ArtboardTypeScale = "auto"
	}
	if !visionTypeScales[in.ArtboardTypeScale] {
		return fmt.Errorf("vision: unknown artboard_type_scale %q", in.ArtboardTypeScale)
	}
	if in.ArtboardAlign == "" {
		in.ArtboardAlign = "center"
	}
	if !visionAligns[in.ArtboardAlign] {
		return fmt.Errorf("vision: unknown artboard_align %q", in.ArtboardAlign)
	}
	if in.MediaURLs == nil {
		// pq.Array(nil) is SQL NULL, and media_urls is NOT NULL.
		in.MediaURLs = []string{}
	}
	if in.Audience == "" {
		in.Audience = "everyone"
	}
	if !visionAudiences[in.Audience] {
		return fmt.Errorf("vision: unknown audience %q", in.Audience)
	}
	if in.PollOptions == nil {
		in.PollOptions = []string{}
	}
	if n := len(in.PollOptions); n == 1 || n > VisionPollMaxOptions {
		return fmt.Errorf("vision: a poll has 2 to %d options, not %d", VisionPollMaxOptions, n)
	}
	if in.TTL <= 0 {
		in.TTL = DefaultVisionTTL
	}
	if len(in.Metadata) == 0 {
		in.Metadata = []byte(`{}`)
	}
	for i := range in.Media {
		if in.Media[i].ObjectKey == "" {
			return errors.New("vision: media object_key is required")
		}
		if in.Media[i].MediaKind == "" {
			in.Media[i].MediaKind = "image"
		}
		if !visionMediaKinds[in.Media[i].MediaKind] {
			return fmt.Errorf("vision: unknown media_kind %q", in.Media[i].MediaKind)
		}
	}
	return nil
}

// InsertVision creates a Vision and its media rows in one transaction, so a
// crash can never leave stored objects with no row that owns their purge. The
// address comes from vision_lanes inside the same transaction as the row, so
// two posts racing on one lane get two ordinals and neither is ever reused.
func InsertVision(database *sql.DB, in VisionInput) (VisionRef, error) {
	if err := in.applyVisionDefaults(); err != nil {
		return VisionRef{}, err
	}

	tx, err := database.Begin()
	if err != nil {
		return VisionRef{}, err
	}
	committed := false
	defer func() {
		if !committed {
			if rerr := tx.Rollback(); rerr != nil && !errors.Is(rerr, sql.ErrTxDone) {
				log.Printf("vision: rollback after failed insert: %v", rerr)
			}
		}
	}()

	expiresAt := time.Now().UTC().Add(in.TTL)

	ref := VisionRef{AuthorPIAL: in.AuthorPIAL}
	if err := tx.QueryRow(`
		INSERT INTO vision_lanes (pial_id, last_seq) VALUES ($1::uuid, 1)
		ON CONFLICT (pial_id) DO UPDATE SET last_seq = vision_lanes.last_seq + 1
		RETURNING last_seq
	`, in.AuthorPIAL).Scan(&ref.Seq); err != nil {
		return VisionRef{}, err
	}

	_, err = tx.Exec(`
		INSERT INTO visions (
			author_pial, seq, content_type, body,
			artboard_background, artboard_typeface, artboard_type_scale, artboard_align,
			media_urls, shared_work_id, is_nsfw, expires_at, metadata,
			poll_options, audience, allow_replies
		) VALUES (
			$1::uuid, $2, $3, $4,
			$5, $6, $7, $8,
			$9, $10::uuid, $11, $12, $13::jsonb,
			$14, $15, $16
		)
	`,
		ref.AuthorPIAL, ref.Seq, in.ContentType, in.Body,
		in.ArtboardBackground, in.ArtboardTypeface, in.ArtboardTypeScale, in.ArtboardAlign,
		pq.Array(in.MediaURLs), nullUUID(in.SharedWorkID), in.IsNSFW, expiresAt, string(in.Metadata),
		pq.Array(in.PollOptions), in.Audience, in.AllowReplies,
	)
	if err != nil {
		return VisionRef{}, err
	}

	purgeAfter := expiresAt.Add(visionMediaPurgeGrace)
	for _, m := range in.Media {
		if _, err := tx.Exec(`
			INSERT INTO vision_media (
				author_pial, seq, object_key, caeor_asset_id, asset_url,
				media_kind, width, height, duration_secs, purge_after
			) VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (author_pial, seq, object_key) DO NOTHING
		`, ref.AuthorPIAL, ref.Seq, m.ObjectKey, m.CaeorAssetID, m.AssetURL,
			m.MediaKind, m.Width, m.Height, m.DurationSecs, purgeAfter); err != nil {
			return VisionRef{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return VisionRef{}, err
	}
	committed = true
	return ref, nil
}

// GetVisionByRef returns one live Vision. An expired or soft-deleted Vision
// is not readable by direct address, which is the only thing that makes
// expiry real.
func GetVisionByRef(database *sql.DB, ref VisionRef, viewerPIAL string) (*model.Vision, error) {
	rows, err := database.Query(visionSelectSQL+`
		WHERE v.author_pial = $2::uuid AND v.seq = $3
		  AND `+visionLiveSQL+`
	`, nullUUID(viewerPIAL), ref.AuthorPIAL, ref.Seq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, ErrVisionNotLive
	}
	v, err := scanVision(rows)
	if err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return v, nil
}

// GetActiveVisionsForAuthor returns one author's live Visions, oldest first —
// the order the sequential viewer plays them in.
func GetActiveVisionsForAuthor(database *sql.DB, authorPIAL, viewerPIAL string, limit int) ([]*model.Vision, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := database.Query(visionSelectSQL+`
		WHERE v.author_pial = $2::uuid
		  AND NOT v.is_blocked
		  AND `+visionLiveSQL+`
		ORDER BY v.seq ASC
		LIMIT $3
	`, nullUUID(viewerPIAL), authorPIAL, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*model.Vision, 0, limit)
	for rows.Next() {
		v, err := scanVision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// GetVisionRingsForViewer returns the avatar rings for the viewer and
// everyone they follow. Ring state, the unseen count and the resume point are
// all resolved by this one query; the browser is told the answer, never the
// inputs.
//
// The follow graph and vision_mutes are keyed by PIAL now, not users.id, so
// this reaches the follow graph through users.pial_id the same way
// devserver's VisionTray does.
func GetVisionRingsForViewer(database *sql.DB, viewerID, viewerPIAL string, limit int) ([]*model.VisionRing, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := database.Query(`
SELECT
    v.author_pial::text,
    u.handle,
    COALESCE(up.display_name, ''),
    COALESCE(up.avatar_url, ''),
    COUNT(*)::int,
    COUNT(*) FILTER (WHERE vv.viewer_pial IS NULL)::int,
    CASE WHEN COUNT(*) FILTER (WHERE vv.viewer_pial IS NULL) > 0
         THEN 'unseen' ELSE 'seen' END,
    COALESCE(
        (ARRAY_AGG(v.seq ORDER BY v.seq) FILTER (WHERE vv.viewer_pial IS NULL))[1],
        (ARRAY_AGG(v.seq ORDER BY v.seq))[1]
    ),
    MAX(v.created_at)
FROM visions v
JOIN users u ON u.pial_id = v.author_pial
LEFT JOIN user_profiles up ON up.user_id = u.id
LEFT JOIN vision_views vv ON vv.author_pial = v.author_pial AND vv.seq = v.seq AND vv.viewer_pial = $1::uuid
WHERE `+visionLiveSQL+`
  AND NOT v.is_blocked
  AND (
        v.author_pial = $1::uuid
     OR v.author_pial IN (SELECT u2.pial_id FROM follows f JOIN users u2 ON u2.id = f.following_id WHERE f.follower_id = $2::uuid)
  )
  AND v.author_pial NOT IN (SELECT muted_pial FROM vision_mutes WHERE muter_pial = $1::uuid)
GROUP BY v.author_pial, u.handle, up.display_name, up.avatar_url
ORDER BY (COUNT(*) FILTER (WHERE vv.viewer_pial IS NULL) > 0) DESC, MAX(v.created_at) DESC
LIMIT $3
	`, nullUUID(viewerPIAL), viewerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*model.VisionRing, 0, limit)
	for rows.Next() {
		var r model.VisionRing
		var nextSeq int64
		if err := rows.Scan(
			&r.AuthorPIAL, &r.AuthorHandle, &r.AuthorName, &r.AvatarURL,
			&r.Count, &r.UnseenCount, &r.State, &nextSeq, &r.LatestAt,
		); err != nil {
			return nil, err
		}
		r.NextVisionRef = VisionRef{AuthorPIAL: r.AuthorPIAL, Seq: nextSeq}.String()
		out = append(out, &r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// MarkVisionViewed records that a viewer opened a Vision. Reopening is
// idempotent. A zero-row write is disambiguated rather than assumed:
// already-viewed is success, anything else is ErrVisionNotLive.
func MarkVisionViewed(database *sql.DB, ref VisionRef, viewerPIAL string) error {
	if viewerPIAL == "" {
		return errors.New("vision: viewer_pial is required")
	}
	res, err := database.Exec(`
		INSERT INTO vision_views (author_pial, seq, viewer_pial)
		SELECT v.author_pial, v.seq, $3::uuid
		  FROM visions v
		 WHERE v.author_pial = $1::uuid AND v.seq = $2
		   AND `+visionLiveSQL+`
		ON CONFLICT (author_pial, seq, viewer_pial) DO NOTHING
	`, ref.AuthorPIAL, ref.Seq, viewerPIAL)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}

	var live, alreadyViewed bool
	if err := database.QueryRow(`
		SELECT EXISTS (SELECT 1 FROM visions v WHERE v.author_pial = $1::uuid AND v.seq = $2 AND `+visionLiveSQL+`),
		       EXISTS (SELECT 1 FROM vision_views
		                WHERE author_pial = $1::uuid AND seq = $2 AND viewer_pial = $3::uuid)
	`, ref.AuthorPIAL, ref.Seq, viewerPIAL).Scan(&live, &alreadyViewed); err != nil {
		return err
	}
	if !live {
		return ErrVisionNotLive
	}
	if alreadyViewed {
		return nil
	}
	return fmt.Errorf("vision %s: view not recorded and no reason found", ref)
}

// GetVisionViewers returns who has opened a live Vision, most recent first.
func GetVisionViewers(database *sql.DB, ref VisionRef, limit int) ([]model.VisionViewer, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := database.Query(`
		SELECT vv.viewer_pial::text,
		       COALESCE(u.handle, ''),
		       COALESCE(up.display_name, ''),
		       COALESCE(up.avatar_url, ''),
		       vv.viewed_at
		  FROM vision_views vv
		  JOIN visions v ON v.author_pial = vv.author_pial AND v.seq = vv.seq
		  LEFT JOIN users u ON u.pial_id = vv.viewer_pial
		  LEFT JOIN user_profiles up ON up.user_id = u.id
		 WHERE vv.author_pial = $1::uuid AND vv.seq = $2
		   AND `+visionLiveSQL+`
		 ORDER BY vv.viewed_at DESC
		 LIMIT $3
	`, ref.AuthorPIAL, ref.Seq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]model.VisionViewer, 0, limit)
	for rows.Next() {
		var v model.VisionViewer
		if err := rows.Scan(&v.ViewerPIAL, &v.Handle, &v.Name, &v.AvatarURL, &v.ViewedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// SoftDeleteVision removes a live Vision from every read path. Ownership is
// the address itself: the caller's PIAL must be the lane the address names.
// A delete that matched nothing returns ErrVisionNotLive rather than
// reporting success.
func SoftDeleteVision(database *sql.DB, ref VisionRef, callerPIAL string) error {
	if ref.AuthorPIAL != callerPIAL {
		return ErrVisionNotLive
	}
	res, err := database.Exec(`
		UPDATE visions v
		   SET deleted_at = NOW()
		 WHERE v.author_pial = $1::uuid AND v.seq = $2
		   AND `+visionLiveSQL+`
	`, ref.AuthorPIAL, ref.Seq)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrVisionNotLive
	}
	return nil
}

// ExpiredVisionMedia returns stored objects whose Vision lifetime has run out
// and which have not been purged yet.
func ExpiredVisionMedia(database *sql.DB, limit int) ([]VisionMediaRef, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := database.Query(`
		SELECT m.id::text, m.author_pial::text, m.seq, m.object_key, m.caeor_asset_id, m.asset_url, m.purge_after
		  FROM vision_media m
		 WHERE m.purged_at IS NULL
		   AND m.purge_after <= NOW()
		 ORDER BY m.purge_after ASC
		 LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]VisionMediaRef, 0, limit)
	for rows.Next() {
		var m VisionMediaRef
		if err := rows.Scan(&m.ID, &m.AuthorPIAL, &m.Seq, &m.ObjectKey, &m.CaeorAssetID, &m.AssetURL, &m.PurgeAfter); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// MarkVisionMediaPurged records that the stored objects are gone, so the
// purge sweep does not hand back the same rows forever.
func MarkVisionMediaPurged(database *sql.DB, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := database.Exec(`
		UPDATE vision_media
		   SET purged_at = NOW()
		 WHERE id = ANY($1::uuid[])
		   AND purged_at IS NULL
	`, pq.Array(ids))
	return err
}

// visionSequenceOrderSQL is the order the sequential viewer plays a scope in:
// authors newest-first, and inside one author oldest-first. Grouping by
// author happens here, in SQL, so the surface is handed a sequence it only
// has to walk.
const visionSequenceOrderSQL = `
	ORDER BY MAX(v.created_at) OVER (PARTITION BY v.author_pial) DESC,
	         v.author_pial,
	         v.seq ASC
`

// visionSequenceLimit caps a sequence read. A viewer walks one Vision at a
// time, so the whole scope is fetched once and neighbours are resolved from it.
const visionSequenceLimit = 300

// GetActiveVisionsForViewer returns every live Vision from the viewer and the
// accounts they follow, in play order. includeNSFW is the viewer's
// clearance, decided by the caller: a viewer who must not see 18+ content
// never has one selected, so it can never reach a Fragment.
func GetActiveVisionsForViewer(database *sql.DB, viewerID, viewerPIAL string, includeNSFW bool, limit int) ([]*model.Vision, error) {
	if limit <= 0 {
		limit = visionSequenceLimit
	}
	rows, err := database.Query(visionSelectSQL+`
		WHERE `+visionLiveSQL+`
		  AND NOT v.is_blocked
		  AND (NOT v.is_nsfw OR $3)
		  AND (
		        v.author_pial = $1::uuid
		     OR v.author_pial IN (SELECT u2.pial_id FROM follows f JOIN users u2 ON u2.id = f.following_id WHERE f.follower_id = $2::uuid)
		  )
	`+visionSequenceOrderSQL+`
		LIMIT $4
	`, nullUUID(viewerPIAL), viewerID, includeNSFW, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectVisions(rows, limit)
}

// GetActiveVisions returns every live Vision on the platform in play order —
// the discovery scope behind the reels Playground.
func GetActiveVisions(database *sql.DB, viewerPIAL string, includeNSFW bool, limit int) ([]*model.Vision, error) {
	if limit <= 0 {
		limit = visionSequenceLimit
	}
	rows, err := database.Query(visionSelectSQL+`
		WHERE `+visionLiveSQL+`
		  AND NOT v.is_blocked
		  AND (NOT v.is_nsfw OR $2)
	`+visionSequenceOrderSQL+`
		LIMIT $3
	`, nullUUID(viewerPIAL), includeNSFW, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectVisions(rows, limit)
}

// collectVisions drains a visionSelectSQL result set. A scan failure is
// returned, never skipped: a short sequence would silently change what
// "next" means.
func collectVisions(rows *sql.Rows, limit int) ([]*model.Vision, error) {
	out := make([]*model.Vision, 0, limit)
	for rows.Next() {
		v, err := scanVision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
