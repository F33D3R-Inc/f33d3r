package handler

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

const (
	videoJobTick = 5 * time.Second
	// A job that never reached the transcoder after this long lost its hand-off.
	videoJobHandoffLimit = 10 * time.Minute
)

// StartVideoJobWorker tells people how their video processing ended. Transcoder jobs
// live in its memory, so a job it no longer knows about ended with the transcoder.
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
		if job.Age > videoJobHandoffLimit {
			h.finishVideoJob(job, false)
		}
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.transcodingURL+"/v1/video/job/"+job.JobID, nil)
	if err != nil {
		log.Printf("[video-jobs] %s: %v", job.UploadID, err)
		return
	}
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return // the transcoder being unreachable says nothing about this job
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		h.finishVideoJob(job, false)
		return
	}
	if resp.StatusCode != http.StatusOK {
		log.Printf("[video-jobs] %s: transcoder answered %s", job.UploadID, resp.Status)
		return
	}
	var answer struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		log.Printf("[video-jobs] %s: %v", job.UploadID, err)
		return
	}
	switch answer.Status {
	case "ready":
		h.finishVideoJob(job, true)
	case "failed", "error":
		h.finishVideoJob(job, false)
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
