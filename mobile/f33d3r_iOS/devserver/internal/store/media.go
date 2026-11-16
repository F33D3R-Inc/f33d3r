package store

import (
	"context"
	"database/sql"
	"errors"
)

// MediaAsset is one file the devserver has been handed, as Caeor would hold it.
type MediaAsset struct {
	AssetID        string
	UploaderID     string
	UploaderHandle string
	Kind           string
	Status         string
}

// Asset statuses, the integrity authority's answers.
const (
	AssetReady  = "ready"
	AssetHeld   = "held"
	AssetBanned = "banned"
)

// AdmitMediaAsset records assetID as uploaded by uploaderID unless it is
// already known, and returns the asset as it now stands. An asset somebody
// uploaded first stays theirs: the second uploader gets the original back.
func (s *Store) AdmitMediaAsset(ctx context.Context, assetID, uploaderID, kind string) (*MediaAsset, error) {
	if _, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO media_assets (asset_id, uploader_id, kind, status, created_at)
		VALUES (?, ?, ?, 'ready', ?)`, assetID, uploaderID, kind, Now()); err != nil {
		return nil, err
	}
	return s.MediaAsset(ctx, assetID)
}

// MediaAsset returns one asset, or ErrNotFound.
func (s *Store) MediaAsset(ctx context.Context, assetID string) (*MediaAsset, error) {
	a := &MediaAsset{}
	err := s.db.QueryRowContext(ctx, `
		SELECT m.asset_id, m.uploader_id, COALESCE(u.handle, ''), m.kind, m.status
		FROM media_assets m LEFT JOIN users u ON u.id = m.uploader_id
		WHERE m.asset_id = ?`, assetID).Scan(&a.AssetID, &a.UploaderID, &a.UploaderHandle, &a.Kind, &a.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}
