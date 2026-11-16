package db

// media_assets.go — the work side of media.
//
// Caeor is the one media-asset authority: it stores the bytes, burns the
// watermark into every viewer-facing copy, resolves exact-byte duplicates to the
// original asset and its uploader, answers which asset a served path belongs to,
// and withdraws assets. This file holds only what is decided about WORKS:
//
//   work_assets         which works reference which Caeor assets, and who
//                       uploaded each (the media gate governs an asset by the
//                       works of its uploader that reference it)
//   media_withdrawals   what deleted works owe Caeor: the author's own assets
//                       of a deleted work are withdrawn, never freed
//   pial_event_ledger   the chain of custody of every download
//
// Nothing here reads or writes canonical_media, video_raw_hashes or
// image_raw_hashes; those are retired (0036, staged 0038).

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// Media slots a work can fill. Mirrors work_assets_slot_vocabulary (0036).
const (
	SlotImage  = "image"
	SlotVoice  = "voice"
	SlotVideo  = "video"
	SlotPoster = "poster"
)

// WorkAssetRef is one media slot of a work as Caeor vouched for it.
type WorkAssetRef struct {
	Slot         string
	Position     int
	AssetID      string
	UploaderPIAL string
}

// insertWorkAssets records the assets a work references, in the transaction
// that creates (or re-binds) the work.
func insertWorkAssets(tx *sql.Tx, workID string, refs []WorkAssetRef) error {
	for _, ref := range refs {
		if ref.AssetID == "" || ref.UploaderPIAL == "" {
			return fmt.Errorf("recording media of work %s: %s slot %d names no asset or no uploader", workID, ref.Slot, ref.Position)
		}
		if _, err := tx.Exec(`
			INSERT INTO work_assets (work_id, slot, position, asset_id, uploader_pial)
			VALUES ($1::uuid, $2, $3, $4::uuid, $5::uuid)
			ON CONFLICT (work_id, slot, position) DO UPDATE
			   SET asset_id = EXCLUDED.asset_id, uploader_pial = EXCLUDED.uploader_pial`,
			workID, ref.Slot, ref.Position, ref.AssetID, ref.UploaderPIAL); err != nil {
			return fmt.Errorf("recording %s %d of work %s: %w", ref.Slot, ref.Position, workID, err)
		}
	}
	return nil
}

// enqueueWorkMediaWithdrawal records, in the deleting transaction, that the
// author's own assets of a work are to be withdrawn from Caeor. Assets somebody
// else uploaded are a credited reference and are never touched by this work's
// deletion.
//
// An author deleting one of their works withdraws an asset only when no other
// live work of theirs still carries it — the same image posted twice is one
// asset, and deleting one post does not take the other's media away. An
// administrator's removal (everywhere) withdraws the asset outright.
func enqueueWorkMediaWithdrawal(tx *sql.Tx, workID, reason string, everywhere bool) error {
	if _, err := tx.Exec(`
		INSERT INTO media_withdrawals (asset_id, work_id, reason)
		SELECT DISTINCT wa.asset_id, $1::uuid, $2
		  FROM work_assets wa
		  JOIN works w ON w.id = wa.work_id
		 WHERE wa.work_id = $1::uuid
		   AND wa.uploader_pial = w.author_pial
		   AND ($3 OR NOT EXISTS (
		         SELECT 1
		           FROM work_assets o
		           JOIN works ow ON ow.id = o.work_id
		          WHERE o.asset_id = wa.asset_id
		            AND o.work_id <> wa.work_id
		            AND ow.author_pial = w.author_pial
		            AND ow.deleted_at IS NULL))
		ON CONFLICT (asset_id) DO NOTHING`, workID, reason, everywhere); err != nil {
		return fmt.Errorf("recording the media withdrawal of work %s: %w", workID, err)
	}
	return nil
}

// MediaWithdrawal is one claimed withdrawal. Attempt counts this claim.
type MediaWithdrawal struct {
	AssetID string
	WorkID  string
	Reason  string
	Attempt int
}

// MediaWithdrawalMaxAttempts is the attempt at which a failing withdrawal is
// quarantined for a person to look at (0036 holds the operator queries).
const MediaWithdrawalMaxAttempts = WorkOutboxMaxAttempts

// ClaimMediaWithdrawals takes up to limit due withdrawals and holds each for
// lease, exactly as ClaimWorkOutbox holds an obligation.
func ClaimMediaWithdrawals(database *sql.DB, limit int, lease time.Duration) ([]MediaWithdrawal, error) {
	rows, err := database.Query(`
		UPDATE media_withdrawals m
		   SET attempts = m.attempts + 1,
		       next_attempt_at = NOW() + make_interval(secs => $2)
		  FROM (SELECT asset_id FROM media_withdrawals
		         WHERE done_at IS NULL AND quarantined_at IS NULL
		           AND next_attempt_at <= NOW()
		         ORDER BY next_attempt_at, asset_id
		         LIMIT $1
		         FOR UPDATE SKIP LOCKED) due
		 WHERE m.asset_id = due.asset_id
		RETURNING m.asset_id::text, m.work_id::text, m.reason, m.attempts`, limit, lease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("claiming media withdrawals: %w", err)
	}
	defer rows.Close()
	var out []MediaWithdrawal
	for rows.Next() {
		var m MediaWithdrawal
		if err := rows.Scan(&m.AssetID, &m.WorkID, &m.Reason, &m.Attempt); err != nil {
			return nil, fmt.Errorf("claiming media withdrawals: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claiming media withdrawals: %w", err)
	}
	return out, nil
}

// CompleteMediaWithdrawal closes a withdrawal Caeor has carried out.
func CompleteMediaWithdrawal(database *sql.DB, assetID string) error {
	if _, err := database.Exec(`
		UPDATE media_withdrawals SET done_at = NOW(), last_error = NULL
		 WHERE asset_id = $1::uuid AND done_at IS NULL`, assetID); err != nil {
		return fmt.Errorf("closing the withdrawal of asset %s: %w", assetID, err)
	}
	return nil
}

// FailMediaWithdrawal records why an attempt failed and schedules the next, or
// quarantines the row at the attempt cap. Reports whether it was quarantined.
func FailMediaWithdrawal(database *sql.DB, assetID string, cause error) (bool, error) {
	reason := cause.Error()
	if len(reason) > 2000 {
		reason = reason[:2000]
	}
	var quarantined bool
	err := database.QueryRow(`
		UPDATE media_withdrawals
		   SET last_error = $2,
		       next_attempt_at = NOW() + LEAST(POWER(2, LEAST(attempts, 20)) * INTERVAL '1 second', INTERVAL '1 hour'),
		       quarantined_at = CASE WHEN attempts >= $3 THEN NOW() END
		 WHERE asset_id = $1::uuid AND done_at IS NULL
		RETURNING quarantined_at IS NOT NULL`, assetID, reason, MediaWithdrawalMaxAttempts).Scan(&quarantined)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // closed by another replica in the meantime
	}
	if err != nil {
		return false, fmt.Errorf("rescheduling the withdrawal of asset %s: %w", assetID, err)
	}
	return quarantined, nil
}

// QuarantinedMediaWithdrawals counts withdrawals waiting for a person.
func QuarantinedMediaWithdrawals(database *sql.DB) (int, error) {
	var n int
	if err := database.QueryRow(`
		SELECT COUNT(*) FROM media_withdrawals
		 WHERE quarantined_at IS NOT NULL AND done_at IS NULL`).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting quarantined media withdrawals: %w", err)
	}
	return n, nil
}

// DownloadableAsset is the asset a work's download hands out.
type DownloadableAsset struct {
	AssetID string
	Slot    string
}

// WorkDownloadableAsset returns the asset the download of a work hands out:
// the work's own video, else its own audio, else its first own image — "own"
// meaning uploaded by the work's author. A work that carries only somebody
// else's media (a credited reference) has nothing of its own to hand out.
// nil, nil when there is none.
func WorkDownloadableAsset(database DBTX, workID string) (*DownloadableAsset, error) {
	if nilDB(database) {
		return nil, errors.New("resolving a work's download: no database")
	}
	var a DownloadableAsset
	err := database.QueryRow(`
		SELECT wa.asset_id::text, wa.slot
		  FROM work_assets wa
		  JOIN works w ON w.id = wa.work_id
		 WHERE wa.work_id = $1::uuid
		   AND wa.uploader_pial = w.author_pial
		   AND wa.slot IN ('video', 'voice', 'image')
		 ORDER BY CASE wa.slot WHEN 'video' THEN 0 WHEN 'voice' THEN 1 ELSE 2 END, wa.position
		 LIMIT 1`, workID).Scan(&a.AssetID, &a.Slot)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resolving the download of work %s: %w", workID, err)
	}
	return &a, nil
}

// SetWorkDownloadsAllowed is the author switching downloads of one of their
// works on or off. sql.ErrNoRows when the work is not theirs or is gone.
func SetWorkDownloadsAllowed(database *sql.DB, workID, authorID string, allowed bool) error {
	res, err := database.Exec(`
		UPDATE works SET downloads_allowed = $3
		 WHERE id = $1::uuid AND author_id = $2::uuid AND deleted_at IS NULL`,
		workID, authorID, allowed)
	if err != nil {
		return fmt.Errorf("setting downloads of work %s: %w", workID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("setting downloads of work %s: %w", workID, err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ── media_bind: legacy works onto Caeor assets ───────────────────────────────

// WorkMediaToBind is a legacy work's media as stored before Caeor.
type WorkMediaToBind struct {
	WorkID         string
	AuthorPIAL     string
	MediaURLs      []string
	VoiceURL       string
	VideoMasterURL string
	VideoPosterURL string
	// Bound is true when the work already has media references, so there is
	// nothing to bind.
	Bound bool
}

// LoadWorkMediaToBind reads one work for its media_bind obligation. nil, nil
// when the work is gone.
func LoadWorkMediaToBind(database *sql.DB, workID string) (*WorkMediaToBind, error) {
	var m WorkMediaToBind
	var media pq.StringArray
	err := database.QueryRow(`
		SELECT w.id::text, w.author_pial::text, w.media_urls,
		       COALESCE(w.voice_url, ''), COALESCE(w.video_master_url, ''), COALESCE(w.video_poster_url, ''),
		       EXISTS (SELECT 1 FROM work_assets wa WHERE wa.work_id = w.id)
		  FROM works w
		 WHERE w.id = $1::uuid`, workID).Scan(
		&m.WorkID, &m.AuthorPIAL, &media, &m.VoiceURL, &m.VideoMasterURL, &m.VideoPosterURL, &m.Bound)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("loading the media of work %s to bind: %w", workID, err)
	}
	m.MediaURLs = []string(media)
	return &m, nil
}

// WorkMediaBinding is a legacy work's media re-bound to Caeor: the
// watermarked objects the work now names, the assets behind them, and the
// credit of whoever uploaded media that is not the author's.
type WorkMediaBinding struct {
	MediaURLs      []string
	VoiceURL       string
	VideoMasterURL string
	VideoPosterURL string
	Assets         []WorkAssetRef
	LineagePIAL    string
	LineageHandle  string
}

// BindWorkMedia rewrites a legacy work onto its Caeor assets in one
// transaction: the work names the watermarked objects, its work_assets are
// recorded, and the "Media by" credit is set when the media is somebody
// else's. A work that gained media references in the meantime is left as it
// is; the bind is then already done.
func BindWorkMedia(database *sql.DB, workID string, b WorkMediaBinding) error {
	return WithTx(database, func(tx *sql.Tx) error {
		var bound bool
		if err := tx.QueryRow(`
			SELECT EXISTS (SELECT 1 FROM work_assets WHERE work_id = $1::uuid)
			  FROM works WHERE id = $1::uuid FOR UPDATE`, workID).Scan(&bound); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil // the work is gone; nothing is owed
			}
			return fmt.Errorf("binding the media of work %s: %w", workID, err)
		}
		if bound {
			return nil
		}
		if b.MediaURLs == nil {
			b.MediaURLs = []string{}
		}
		if _, err := tx.Exec(`
			UPDATE works
			   SET media_urls       = $2,
			       voice_url        = NULLIF($3, ''),
			       video_master_url = NULLIF($4, ''),
			       video_poster_url = NULLIF($5, ''),
			       lineage_pial     = COALESCE(NULLIF($6, ''), lineage_pial),
			       lineage_handle   = COALESCE(NULLIF($7, ''), lineage_handle)
			 WHERE id = $1::uuid`,
			workID, pq.Array(b.MediaURLs), b.VoiceURL, b.VideoMasterURL, b.VideoPosterURL,
			b.LineagePIAL, b.LineageHandle); err != nil {
			return fmt.Errorf("binding the media of work %s: %w", workID, err)
		}
		return insertWorkAssets(tx, workID, b.Assets)
	})
}

// ── chain of custody ─────────────────────────────────────────────────────────

// RecordContentDownload appends the chain-of-custody entry for one download to
// the PIAL event ledger. Unlike LogPIALEvent it reports failure: a copy is only
// handed out when the record of who received it exists.
func RecordContentDownload(database DBTX, pialID, accountID string, payload map[string]interface{}) error {
	if nilDB(database) {
		return errors.New("recording content download: no database")
	}
	if pialID == "" || accountID == "" {
		return errors.New("recording content download: downloader has no PIAL or account")
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("recording content download: %w", err)
	}
	if _, err := database.Exec(`
		INSERT INTO pial_event_ledger (pial_id, account_id, event_type, payload, source)
		VALUES ($1, $2, 'content_downloaded', $3, 'download_gate')`,
		pialID, accountID, string(b)); err != nil {
		return fmt.Errorf("recording content download: %w", err)
	}
	return nil
}

// UploaderCredit names the uploader of an asset for its "Media by" credit: the
// handle and display name of the PIAL's first local account. ("", "", nil)
// when the PIAL has no local account any more — the media then carries no
// credit chip, and is still governed by the uploader's works.
func UploaderCredit(database DBTX, uploaderPIAL string) (handle, displayName string, err error) {
	if nilDB(database) {
		return "", "", errors.New("reading an uploader's credit: no database")
	}
	err = database.QueryRow(`
		SELECT u.handle, COALESCE(NULLIF(TRIM(p.display_name), ''), u.handle)
		  FROM users u
		  LEFT JOIN user_profiles p ON p.user_id = u.id
		 WHERE u.pial_id = $1::uuid
		 ORDER BY u.created_at, u.id
		 LIMIT 1`, uploaderPIAL).Scan(&handle, &displayName)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("reading the credit of uploader %s: %w", uploaderPIAL, err)
	}
	return handle, displayName, nil
}

// AssetStillCarried reports whether anything of the uploader's still shows an
// asset: a live work that references it, or another Vision media row not yet
// purged (excludingVisionMediaID is the row being purged). An expiring Vision
// withdraws its asset only when nothing else of its author's carries it.
func AssetStillCarried(database *sql.DB, assetID, uploaderPIAL, excludingVisionMediaID string) (bool, error) {
	var carried bool
	err := database.QueryRow(`
		SELECT EXISTS (
		         SELECT 1 FROM work_assets wa
		           JOIN works w ON w.id = wa.work_id
		          WHERE wa.asset_id = $1::uuid AND w.author_pial = $2::uuid AND w.deleted_at IS NULL)
		    OR EXISTS (
		         SELECT 1 FROM vision_media vm
		          WHERE vm.caeor_asset_id = $1::text AND vm.author_pial = $2::uuid
		            AND vm.purged_at IS NULL AND vm.id <> $3::uuid)`,
		assetID, uploaderPIAL, excludingVisionMediaID).Scan(&carried)
	if err != nil {
		return false, fmt.Errorf("checking what still carries asset %s: %w", assetID, err)
	}
	return carried, nil
}
