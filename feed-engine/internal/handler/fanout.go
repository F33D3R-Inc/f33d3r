package handler

import (
	"context"
	"fmt"
	"html"
	"log"
	"time"

	"github.com/f33d3r/feed-engine/internal/auralis"
	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

// fanoutIdle is how long the worker waits when no job is open.
const fanoutIdle = time.Second

// StartFanoutWorker works through follower notifications one batch at a time. It
// resumes after a crash from the position each batch recorded.
func (h *Handler) StartFanoutWorker(ctx context.Context) {
	go func() {
		for {
			job, recipients, err := dbpkg.RunFanoutBatch(ctx, h.db, fanoutTitle, fanoutURL, h.fanoutCheck)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				log.Printf("[fanout] %v", err)
			}
			if job != nil {
				h.publishFanoutLive(job, recipients)
			}
			if err != nil || job == nil {
				select {
				case <-ctx.Done():
					return
				case <-time.After(fanoutIdle):
				}
			}
		}
	}()
}

// fanoutCheck looks at a room again before each batch: nobody is told a room is
// live once it is over, and nobody in the room is told about it.
func (h *Handler) fanoutCheck(job dbpkg.FanoutJob) (bool, map[string]bool) {
	if job.Kind != frequencyLiveKind || h.auralis == nil {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), frequencyNotifyTimeout)
	defer cancel()
	ans, err := h.auralis.Get(ctx, job.TargetID, "")
	if err != nil {
		if auralis.IsNotFound(err) {
			return true, nil
		}
		// Unknown is not the same as over: the announcement still goes out.
		log.Printf("[fanout] room %s: %v", job.TargetID, err)
		return false, nil
	}
	if !ans.Frequency.Frequency.IsLive() {
		return true, nil
	}
	skip := make(map[string]bool, len(ans.Frequency.PresentPialIDs))
	for _, p := range ans.Frequency.PresentPialIDs {
		skip[p] = true
	}
	return false, skip
}

func fanoutTitle(job dbpkg.FanoutJob) string {
	return "@" + job.ActorHandle + " is live"
}

func fanoutURL(job dbpkg.FanoutJob) string {
	if job.Kind == frequencyLiveKind {
		return "/frequencies/" + job.TargetID
	}
	return "/live/" + job.TargetID
}

// publishFanoutLive sends connected followers the live signal for a batch.
func (h *Handler) publishFanoutLive(job *dbpkg.FanoutJob, recipients []dbpkg.FanoutRecipient) {
	if job.Kind != "live_start" {
		return
	}
	twin := fmt.Sprintf(`{"stream_id":%q}`, job.TargetID)
	for _, r := range recipients {
		PublishToUser(r.PIAL, SSEEvent{Type: "live_start", JSON: twin})
		publishToast(r.PIAL, "live_start_"+job.TargetID, fmt.Sprintf(
			`<a href="/live/%s">@%s is live</a>`, html.EscapeString(job.TargetID), html.EscapeString(job.ActorHandle)))
	}
}
