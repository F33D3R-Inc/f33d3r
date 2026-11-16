package db

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
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

// visionScannedSQL is the content-safety half of "readable": a Vision whose
// scan is still pending is visible to the account that posted it and to no one
// else, until its verdict arrives on safety.events — the rule works follow. $1
// is always the viewer's account id in every query that carries it.
const visionScannedSQL = `(v.scan_state <> 'pending' OR v.author_account = $1::uuid)`

// Closed value sets, mirrored by CHECK constraints in migration 0023.
var (
	visionContentTypes = map[string]bool{"text": true, "image": true, "video": true, "share": true}
	visionTypeScales   = map[string]bool{"auto": true, "s": true, "m": true, "l": true, "xl": true}
	visionAligns       = map[string]bool{"left": true, "center": true, "right": true}
	visionMediaKinds   = map[string]bool{"image": true, "video": true}
)

// VisionRef is a Vision's server-internal key: a position in one PIAL's vision
// lane, (author_pial, seq). It is the content-safety subject Abraxas judges and
// never leaves the server. Everything an observer sees — reads, rings,
// ownership, the wire address — is keyed by the posting account instead
// (migration 0040), so nothing links two of one person's accounts.
type VisionRef struct {
	AuthorPIAL string
	Seq        int64
}

// String is the canonical text form, `<author_pial>.<seq>`. It is the
// server's own form — logs, the content-safety events between brains — and is
// never written into a page or a JSON answer: it carries a PIAL. The wire form
// is VisionAddress.
func (r VisionRef) String() string {
	return r.AuthorPIAL + "." + strconv.FormatInt(r.Seq, 10)
}

// VisionAddress is a Vision's address as a client holds it:
// `<author_account>.<account_seq>` — the posting account and that account's own
// ordinal from vision_account_lanes. Each account numbers its Visions alone, so
// one persona's numbering never interleaves with another's.
type VisionAddress struct {
	AuthorAccount string
	Seq           int64
}

// String is the wire form, `<author_account>.<seq>`.
func (a VisionAddress) String() string {
	return a.AuthorAccount + "." + strconv.FormatInt(a.Seq, 10)
}

// ParseVisionAddress reads the wire form. A malformed address is an error,
// never a zero-valued address.
func ParseVisionAddress(s string) (VisionAddress, error) {
	dot := strings.LastIndexByte(s, '.')
	if dot <= 0 || dot == len(s)-1 {
		return VisionAddress{}, fmt.Errorf("vision address %q: want <author_account>.<seq>", s)
	}
	account, seqText := s[:dot], s[dot+1:]
	if _, err := uuid.Parse(account); err != nil {
		return VisionAddress{}, fmt.Errorf("vision address %q: author is not an account id", s)
	}
	seq, err := strconv.ParseInt(seqText, 10, 64)
	if err != nil || seq <= 0 {
		return VisionAddress{}, fmt.Errorf("vision address %q: seq must be a positive integer", s)
	}
	return VisionAddress{AuthorAccount: account, Seq: seq}, nil
}

// ErrVisionAddressUnknown is a wire address that names no Vision that account
// ever posted.
var ErrVisionAddressUnknown = errors.New("vision: address names no vision")

// ResolveVisionAddress turns a wire address into the server's ref. The lookup
// is by the posting account and its own ordinal, so an address can only ever
// reach a Vision that account posted — never one another persona of the same
// person posted.
func ResolveVisionAddress(database *sql.DB, a VisionAddress) (VisionRef, error) {
	var ref VisionRef
	err := database.QueryRow(`
		SELECT author_pial::text, seq FROM visions
		 WHERE author_account = $1::uuid AND account_seq = $2`, a.AuthorAccount, a.Seq).Scan(&ref.AuthorPIAL, &ref.Seq)
	if errors.Is(err, sql.ErrNoRows) {
		return VisionRef{}, ErrVisionAddressUnknown
	}
	if err != nil {
		return VisionRef{}, fmt.Errorf("vision address %s: resolving: %w", a, err)
	}
	return ref, nil
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
	// AuthorAccount is the persona that posts; AuthorPIAL is its identity root,
	// checked against the account inside the insert transaction.
	AuthorAccount string
	AuthorPIAL    string

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

// $1 is always the viewer's account id, so the viewed flag and the viewer's
// ballot are resolved in the same query as the row they belong to. The author
// join is the posting account alone — never every account on its PIAL.
const visionSelectSQL = `
SELECT
    v.author_pial::text, v.seq, v.account_seq, v.content_type, v.body,
    v.artboard_background, v.artboard_typeface, v.artboard_type_scale, v.artboard_align,
    v.media_urls, COALESCE(v.shared_work_id::text, ''), v.scan_state,
    v.is_nsfw, v.is_blocked, v.created_at, v.expires_at, v.metadata,
    u.handle, u.id::text, COALESCE(up.display_name, ''), COALESCE(up.avatar_url, ''),
    (vv.viewer_account IS NOT NULL),
    (SELECT COUNT(*) FROM vision_views c WHERE c.author_pial = v.author_pial AND c.seq = v.seq)::int,
    v.poll_options, v.audience, v.allow_replies,
    COALESCE((SELECT p.option_idx FROM vision_poll_votes p
               WHERE p.author_pial = v.author_pial AND p.seq = v.seq AND p.voter_account = $1::uuid), -1)
FROM visions v
JOIN users u ON u.id = v.author_account
LEFT JOIN user_profiles up ON up.user_id = u.id
LEFT JOIN vision_views vv ON vv.author_pial = v.author_pial AND vv.seq = v.seq AND vv.viewer_account = $1::uuid
`

// scanVision scans one row from visionSelectSQL. Column order must stay in sync.
func scanVision(rows *sql.Rows) (*model.Vision, error) {
	var v model.Vision
	var mediaURLs, pollOptions pq.StringArray
	var metadata []byte

	if err := rows.Scan(
		&v.AuthorPIAL, &v.Seq, &v.AccountSeq, &v.ContentType, &v.Body,
		&v.ArtboardBackground, &v.ArtboardTypeface, &v.ArtboardTypeScale, &v.ArtboardAlign,
		&mediaURLs, &v.SharedWorkID, &v.ScanState,
		&v.IsNSFW, &v.IsBlocked, &v.CreatedAt, &v.ExpiresAt, &metadata,
		&v.AuthorHandle, &v.AuthorAccountID, &v.AuthorName, &v.AvatarURL,
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
	if in.AuthorAccount == "" {
		return errors.New("vision: author_account is required")
	}
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
// crash can never leave stored objects with no row that owns their purge. Both
// ordinals — the PIAL lane's internal seq and the account's own account_seq —
// come from their counters inside the same transaction as the row, so two
// posts racing on one lane get two ordinals and neither is ever reused.
//
// It returns the internal ref (the content-safety subject) and the wire
// address (what a client is handed).
func InsertVision(database *sql.DB, in VisionInput) (VisionRef, VisionAddress, error) {
	if err := in.applyVisionDefaults(); err != nil {
		return VisionRef{}, VisionAddress{}, err
	}

	tx, err := database.Begin()
	if err != nil {
		return VisionRef{}, VisionAddress{}, err
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

	// The posting account must be bound to the identity the Vision is judged
	// under; the row is held so nothing re-binds it mid-insert.
	var boundPIAL sql.NullString
	err = tx.QueryRow(`SELECT pial_id::text FROM users WHERE id = $1::uuid FOR SHARE`, in.AuthorAccount).Scan(&boundPIAL)
	if errors.Is(err, sql.ErrNoRows) {
		return VisionRef{}, VisionAddress{}, fmt.Errorf("vision: author account %s does not exist", in.AuthorAccount)
	}
	if err != nil {
		return VisionRef{}, VisionAddress{}, fmt.Errorf("vision: reading author account %s: %w", in.AuthorAccount, err)
	}
	if !boundPIAL.Valid || boundPIAL.String != in.AuthorPIAL {
		return VisionRef{}, VisionAddress{}, fmt.Errorf("vision: account %s is not bound to the posting identity", in.AuthorAccount)
	}

	ref := VisionRef{AuthorPIAL: in.AuthorPIAL}
	if err := tx.QueryRow(`
		INSERT INTO vision_lanes (pial_id, last_seq) VALUES ($1::uuid, 1)
		ON CONFLICT (pial_id) DO UPDATE SET last_seq = vision_lanes.last_seq + 1
		RETURNING last_seq
	`, in.AuthorPIAL).Scan(&ref.Seq); err != nil {
		return VisionRef{}, VisionAddress{}, err
	}
	addr := VisionAddress{AuthorAccount: in.AuthorAccount}
	if err := tx.QueryRow(`
		INSERT INTO vision_account_lanes (account_id, last_seq) VALUES ($1::uuid, 1)
		ON CONFLICT (account_id) DO UPDATE SET last_seq = vision_account_lanes.last_seq + 1
		RETURNING last_seq
	`, in.AuthorAccount).Scan(&addr.Seq); err != nil {
		return VisionRef{}, VisionAddress{}, err
	}

	_, err = tx.Exec(`
		INSERT INTO visions (
			author_pial, seq, author_account, account_seq, content_type, body,
			artboard_background, artboard_typeface, artboard_type_scale, artboard_align,
			media_urls, shared_work_id, is_nsfw, expires_at, metadata,
			poll_options, audience, allow_replies
		) VALUES (
			$1::uuid, $2, $3::uuid, $4, $5, $6,
			$7, $8, $9, $10,
			$11, $12::uuid, $13, $14, $15::jsonb,
			$16, $17, $18
		)
	`,
		ref.AuthorPIAL, ref.Seq, addr.AuthorAccount, addr.Seq, in.ContentType, in.Body,
		in.ArtboardBackground, in.ArtboardTypeface, in.ArtboardTypeScale, in.ArtboardAlign,
		pq.Array(in.MediaURLs), nullUUID(in.SharedWorkID), in.IsNSFW, expiresAt, string(in.Metadata),
		pq.Array(in.PollOptions), in.Audience, in.AllowReplies,
	)
	if err != nil {
		return VisionRef{}, VisionAddress{}, err
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
			return VisionRef{}, VisionAddress{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return VisionRef{}, VisionAddress{}, err
	}
	committed = true
	return ref, addr, nil
}

// GetVisionByRef returns one live Vision. An expired or soft-deleted Vision
// is not readable by direct address, which is the only thing that makes
// expiry real; a Vision still pending its scan is readable only by the
// account that posted it.
func GetVisionByRef(database *sql.DB, ref VisionRef, viewerAccount string) (*model.Vision, error) {
	rows, err := database.Query(visionSelectSQL+`
		WHERE v.author_pial = $2::uuid AND v.seq = $3
		  AND `+visionLiveSQL+`
		  AND `+visionScannedSQL+`
	`, nullUUID(viewerAccount), ref.AuthorPIAL, ref.Seq)
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

// GetActiveVisionsForAuthor returns one account's live Visions, oldest first —
// the order the sequential viewer plays them in. The run is the account's
// alone; another persona of the same person is a different author.
func GetActiveVisionsForAuthor(database *sql.DB, authorAccount, viewerAccount string, limit int) ([]*model.Vision, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := database.Query(visionSelectSQL+`
		WHERE v.author_account = $2::uuid
		  AND NOT v.is_blocked
		  AND `+visionLiveSQL+`
		  AND `+visionScannedSQL+`
		ORDER BY v.account_seq ASC
		LIMIT $3
	`, nullUUID(viewerAccount), authorAccount, limit)
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

// GetVisionRingsForViewer returns the avatar rings for the viewer's account
// and every account it follows. Ring state, the unseen count and the resume
// point are all resolved by this one query; the browser is told the answer,
// never the inputs.
//
// Everything here is account to account: the viewer's own ring is the Visions
// this account posted, the follow graph is accounts, views and mutes are the
// viewing account's. A ring is one account's run, so another persona of the
// same person is never folded into it.
func GetVisionRingsForViewer(database *sql.DB, viewerID string, limit int) ([]*model.VisionRing, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := database.Query(`
SELECT
    u.id::text,
    u.handle,
    COALESCE(up.display_name, ''),
    COALESCE(up.avatar_url, ''),
    COUNT(*)::int,
    COUNT(*) FILTER (WHERE vv.viewer_account IS NULL)::int,
    CASE WHEN COUNT(*) FILTER (WHERE vv.viewer_account IS NULL) > 0
         THEN 'unseen' ELSE 'seen' END,
    COALESCE(
        (ARRAY_AGG(v.account_seq ORDER BY v.account_seq) FILTER (WHERE vv.viewer_account IS NULL))[1],
        (ARRAY_AGG(v.account_seq ORDER BY v.account_seq))[1]
    ),
    MAX(v.created_at)
FROM visions v
JOIN users u ON u.id = v.author_account
LEFT JOIN user_profiles up ON up.user_id = u.id
LEFT JOIN vision_views vv ON vv.author_pial = v.author_pial AND vv.seq = v.seq AND vv.viewer_account = $1::uuid
WHERE `+visionLiveSQL+`
  AND `+visionScannedSQL+`
  AND NOT v.is_blocked
  AND (
        v.author_account = $1::uuid
     OR v.author_account IN (SELECT f.following_id FROM follows f WHERE f.follower_id = $1::uuid)
  )
  AND v.author_account NOT IN (SELECT muted_account FROM vision_mutes WHERE muter_account = $1::uuid)
GROUP BY u.id, u.handle, up.display_name, up.avatar_url
ORDER BY (COUNT(*) FILTER (WHERE vv.viewer_account IS NULL) > 0) DESC, MAX(v.created_at) DESC
LIMIT $2
	`, viewerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*model.VisionRing, 0, limit)
	for rows.Next() {
		var r model.VisionRing
		var nextSeq int64
		if err := rows.Scan(
			&r.AuthorAccountID, &r.AuthorHandle, &r.AuthorName, &r.AvatarURL,
			&r.Count, &r.UnseenCount, &r.State, &nextSeq, &r.LatestAt,
		); err != nil {
			return nil, err
		}
		r.NextVisionRef = VisionAddress{AuthorAccount: r.AuthorAccountID, Seq: nextSeq}.String()
		out = append(out, &r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// MarkVisionViewed records that a viewing account opened a Vision. Reopening
// is idempotent. A zero-row write is disambiguated rather than assumed:
// already-viewed is success, anything else is ErrVisionNotLive. The posting
// account is never one of its own Vision's viewers, and a Vision still
// pending its scan has no viewers to record.
func MarkVisionViewed(database *sql.DB, ref VisionRef, viewerAccount string) error {
	if viewerAccount == "" {
		return errors.New("vision: viewer_account is required")
	}
	res, err := database.Exec(`
		INSERT INTO vision_views (author_pial, seq, viewer_account)
		SELECT v.author_pial, v.seq, $3::uuid
		  FROM visions v
		 WHERE v.author_pial = $1::uuid AND v.seq = $2
		   AND v.author_account <> $3::uuid
		   AND v.scan_state <> 'pending'
		   AND `+visionLiveSQL+`
		ON CONFLICT (author_pial, seq, viewer_account) DO NOTHING
	`, ref.AuthorPIAL, ref.Seq, viewerAccount)
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
		SELECT EXISTS (SELECT 1 FROM visions v
		                WHERE v.author_pial = $1::uuid AND v.seq = $2
		                  AND v.author_account <> $3::uuid AND v.scan_state <> 'pending'
		                  AND `+visionLiveSQL+`),
		       EXISTS (SELECT 1 FROM vision_views
		                WHERE author_pial = $1::uuid AND seq = $2 AND viewer_account = $3::uuid)
	`, ref.AuthorPIAL, ref.Seq, viewerAccount).Scan(&live, &alreadyViewed); err != nil {
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

// GetVisionViewers returns which accounts have opened a live Vision, most
// recent first — one entry per viewing account, never every persona of the
// person behind it.
func GetVisionViewers(database *sql.DB, ref VisionRef, limit int) ([]model.VisionViewer, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := database.Query(`
		SELECT vv.viewer_account::text,
		       u.handle,
		       COALESCE(up.display_name, ''),
		       COALESCE(up.avatar_url, ''),
		       vv.viewed_at
		  FROM vision_views vv
		  JOIN visions v ON v.author_pial = vv.author_pial AND v.seq = vv.seq
		  JOIN users u ON u.id = vv.viewer_account
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
		if err := rows.Scan(&v.ViewerAccountID, &v.Handle, &v.Name, &v.AvatarURL, &v.ViewedAt); err != nil {
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
// the posting account: another persona of the same person does not own it.
// A delete that matched nothing returns ErrVisionNotLive rather than
// reporting success.
func SoftDeleteVision(database *sql.DB, ref VisionRef, callerAccount string) error {
	if callerAccount == "" {
		return ErrVisionNotLive
	}
	res, err := database.Exec(`
		UPDATE visions v
		   SET deleted_at = NOW()
		 WHERE v.author_pial = $1::uuid AND v.seq = $2
		   AND v.author_account = $3::uuid
		   AND `+visionLiveSQL+`
	`, ref.AuthorPIAL, ref.Seq, callerAccount)
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
	ORDER BY MAX(v.created_at) OVER (PARTITION BY v.author_account) DESC,
	         v.author_account,
	         v.account_seq ASC
`

// visionSequenceLimit caps a sequence read. A viewer walks one Vision at a
// time, so the whole scope is fetched once and neighbours are resolved from it.
const visionSequenceLimit = 300

// GetActiveVisionsForViewer returns every live Vision from the viewer and the
// accounts they follow, in play order. includeNSFW is the viewer's
// clearance, decided by the caller: a viewer who must not see 18+ content
// never has one selected, so it can never reach a Fragment.
func GetActiveVisionsForViewer(database *sql.DB, viewerID string, includeNSFW bool, limit int) ([]*model.Vision, error) {
	if limit <= 0 {
		limit = visionSequenceLimit
	}
	rows, err := database.Query(visionSelectSQL+`
		WHERE `+visionLiveSQL+`
		  AND `+visionScannedSQL+`
		  AND NOT v.is_blocked
		  AND (NOT v.is_nsfw OR $2)
		  AND (
		        v.author_account = $1::uuid
		     OR v.author_account IN (SELECT f.following_id FROM follows f WHERE f.follower_id = $1::uuid)
		  )
		  AND v.author_account NOT IN (SELECT muted_account FROM vision_mutes WHERE muter_account = $1::uuid)
	`+visionSequenceOrderSQL+`
		LIMIT $3
	`, viewerID, includeNSFW, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectVisions(rows, limit)
}

// GetActiveVisions returns every live Vision on the platform in play order —
// the discovery scope behind the reels Playground. viewerAccount may be empty
// (no viewer): then only scanned Visions are in it.
func GetActiveVisions(database *sql.DB, viewerAccount string, includeNSFW bool, limit int) ([]*model.Vision, error) {
	if limit <= 0 {
		limit = visionSequenceLimit
	}
	rows, err := database.Query(visionSelectSQL+`
		WHERE `+visionLiveSQL+`
		  AND `+visionScannedSQL+`
		  AND NOT v.is_blocked
		  AND (NOT v.is_nsfw OR $2)
	`+visionSequenceOrderSQL+`
		LIMIT $3
	`, nullUUID(viewerAccount), includeNSFW, limit)
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
