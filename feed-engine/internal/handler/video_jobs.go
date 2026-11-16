package handler

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/f33d3r/feed-engine/internal/caeor"
	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

const (
	videoJobTick = 5 * time.Second
	// A job that Caeor has not accepted after the whole hand-off budget (and a
	// margin for the spool to be hashed and gated) lost its hand-off.
	videoJobHandoffLimit = videoHandOffTimeout + 10*time.Minute
)

// StartVideoJobWorker tells people how their video processing ended. A job's
// JobID is the Caeor asset its upload became; Caeor is asked how that asset is
// doing, and the row outlives a restart of either side.
func (h *Handler) StartVideoJobWorker(ctx context.Context) {
	go func() {
		tick := time.NewTicker(videoJobTick)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			jobs, err := dbpkg.OpenVideoJobs(h.db)
			if err != nil {
				log.Printf("[video-jobs] loading: %v", err)
				continue
			}
			for _, job := range jobs {
				h.checkVideoJob(ctx, job)
			}
		}
	}()
}

func (h *Handler) checkVideoJob(ctx context.Context, job dbpkg.VideoJob) {
	if job.JobID == "" {
		// Never accepted by Caeor. The hand-off records its own failure; a row
		// this old lost its hand-off to a restart.
		if job.Age > videoJobHandoffLimit {
			h.finishVideoJob(job, false)
		}
		return
	}
	cctx, cancel := context.WithTimeout(ctx, videoStateTimeout)
	defer cancel()
	asset, err := h.caeor.Get(cctx, job.JobID)
	if errors.Is(err, caeor.ErrNotFound) {
		h.finishVideoJob(job, false)
		return
	}
	if err != nil {
		// Caeor being unreachable says nothing about this job.
		log.Printf("[video-jobs] %s: %v", job.UploadID, err)
		return
	}
	switch {
	case asset.Withdrawn, asset.Status == caeor.StatusFailed:
		h.finishVideoJob(job, false)
	case asset.Status == caeor.StatusReady:
		h.finishVideoJob(job, true)
	}
}

func (h *Handler) finishVideoJob(job dbpkg.VideoJob, ready bool) {
	finished, err := dbpkg.FinishVideoJob(h.db, job, ready)
	if err != nil {
		log.Printf("[video-jobs] finishing %s: %v", job.UploadID, err)
		return
	}
	if finished {
		PublishToUser(job.PIAL, SSEEvent{Type: "upload_done", Data: job.UploadID})
	}
}
