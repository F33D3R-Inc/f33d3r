package handler

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

const (
	workNoticeTick  = 250 * time.Millisecond
	workNoticeBatch = 50
)

// StartWorkNoticeWorker tells people about the works that name them. The job is
// written with the work, so a crash or a naming plane outage delays these
// notifications and cannot lose them.
func (h *Handler) StartWorkNoticeWorker(ctx context.Context) {
	go func() {
		tick := time.NewTicker(workNoticeTick)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			jobs, err := dbpkg.DueWorkNoticeJobs(h.db, workNoticeBatch)
			if err != nil {
				log.Printf("[work-notices] loading jobs: %v", err)
				continue
			}
			for _, job := range jobs {
				if err := h.payWorkNotices(ctx, job); err != nil {
					log.Printf("[work-notices] work %s: %v", job.WorkID, err)
					if err := dbpkg.RetryWorkNoticeJob(h.db, job.WorkID); err != nil {
						log.Printf("[work-notices] rescheduling work %s: %v", job.WorkID, err)
					}
				}
			}
		}
	}()
}

// payWorkNotices works out who the work tells, then queues those notices and closes
// the job together. A reply that also names the person replied to is one event to
// that person, so the citation is decided first.
func (h *Handler) payWorkNotices(ctx context.Context, job dbpkg.WorkNoticeJob) error {
	if job.AuthorID == "" { // the work was deleted before it was announced
		return dbpkg.FinishWorkNoticeJob(h.db, job.WorkID, nil)
	}
	preview := notificationPreview(job.Body)
	told := map[string]bool{job.AuthorID: true}
	var notices []dbpkg.Notice
	tell := func(recipient, kind, title string) {
		told[recipient] = true
		notices = append(notices, dbpkg.Notice{
			EventID:   dbpkg.WorkNoticeEventID(job.WorkID, recipient),
			Recipient: recipient, Kind: kind, Actor: job.AuthorID,
			TargetID: job.WorkID, TargetType: "work",
			Payload:   map[string]interface{}{"preview": preview},
			PushTitle: title, PushBody: preview, PushURL: "/work/" + job.WorkID,
		})
	}

	if job.Kind == "reply" || job.Kind == "quote" {
		cited, ok, err := dbpkg.WorkNoticeCitedWork(h.db, job.WorkID, job.Kind)
		if err != nil {
			return fmt.Errorf("reading cited work: %w", err)
		}
		if ok && !told[cited.AuthorID] {
			verb := "replied to you"
			if job.Kind == "quote" {
				verb = "quoted your work"
			}
			tell(cited.AuthorID, job.Kind, "@"+job.AuthorHandle+" "+verb)
		}
	}

	handles := mentionedHandles(job.Body)
	if len(handles) > 0 {
		resolveCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		// The plane being unreachable is not "nobody was mentioned". The job is
		// retried until the plane answers.
		pialByHandle, err := h.resolveHandlesToPIALs(resolveCtx, handles)
		if err != nil {
			return fmt.Errorf("resolving %d handle(s): %w", len(handles), err)
		}
		if len(pialByHandle) > 0 {
			pials := make([]string, 0, len(pialByHandle))
			for _, pial := range pialByHandle {
				pials = append(pials, pial)
			}
			targets, err := dbpkg.AccountsByPIAL(h.db, pials)
			if err != nil {
				return fmt.Errorf("loading mentioned accounts: %w", err)
			}
			for _, pial := range pials {
				if target, ok := targets[pial]; ok && !told[target.UserID] {
					tell(target.UserID, "mention", "@"+job.AuthorHandle+" mentioned you")
				}
			}
		}
	}
	return dbpkg.FinishWorkNoticeJob(h.db, job.WorkID, notices)
}

// mentionedHandles returns the distinct handles a body names, up to the limit.
func mentionedHandles(body string) []string {
	seen := make(map[string]bool)
	var handles []string
	for _, m := range mentionExtractRe.FindAllStringSubmatch(body, -1) {
		lc := strings.ToLower(m[1])
		if seen[lc] {
			continue
		}
		seen[lc] = true
		handles = append(handles, lc)
		if len(handles) == maxMentionsPerWork {
			break
		}
	}
	return handles
}
