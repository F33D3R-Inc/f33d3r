package db

// work_audio.go — Zior's analysis of an audio work, stored on the work.
//
// Zior is stateless: what it hears is answered once and kept here, never read
// back from Zior. The vector replaces the text mapping, because for a track or
// a voice work the audio is the work. Schema: migration 0043.

import (
	"database/sql"
	"fmt"

	"github.com/lib/pq"

	"github.com/f33d3r/feed-engine/internal/jung"
)

// WorkAudioAnalysis is everything Zior answers about one audio work.
type WorkAudioAnalysis struct {
	Vector      []float32
	Mood        string
	Descriptor  string
	ContextTags []string
	BPM         float32
	Key         string
}

// SetWorkAudioAnalysis stores Zior's analysis and the vector it implies in one
// write. sql.ErrNoRows when the work is gone or deleted.
func SetWorkAudioAnalysis(database *sql.DB, workID string, a WorkAudioAnalysis) error {
	if len(a.Vector) != jung.Dim {
		return fmt.Errorf("works: audio vector for %s has %d dims, want %d", workID, len(a.Vector), jung.Dim)
	}
	tags := a.ContextTags
	if tags == nil {
		tags = []string{}
	}
	res, err := database.Exec(`
		UPDATE works
		   SET psych_vector       = $2,
		       audio_mood         = $3,
		       audio_descriptor   = $4,
		       audio_context_tags = $5,
		       audio_bpm          = $6,
		       audio_key          = $7,
		       audio_analyzed_at  = NOW()
		 WHERE id = $1::uuid AND deleted_at IS NULL`,
		workID, pq.Float32Array(a.Vector), a.Mood, a.Descriptor, pq.StringArray(tags), a.BPM, a.Key)
	if err != nil {
		return fmt.Errorf("storing the audio analysis of work %s: %w", workID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("storing the audio analysis of work %s: %w", workID, err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
