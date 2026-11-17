package handler

// work_outbox.go — the worker that pays what works are owed.
//
// Every work_outbox row was committed with the work it belongs to (InsertWork,
// ReleaseScheduledWorks). This worker claims due rows, performs each action,
// and closes the row; a failure is retried with exponential backoff and, at the
// attempt cap, quarantined with a log line and a metric until a person returns it
// (migration 0035 holds the operator queries). Any number of replicas run it:
// claims are FOR UPDATE SKIP LOCKED with a lease.
//
// Local actions — FA Live mutations, HTTP calls to Zior, the link
// preview cache, interest learning, achievements — run on every replica, with or
// without a broker. The Sitra-bound action (content_event, the moderation scan)
// is produced with ProduceSync and closed only after the broker confirms it; a
// replica without KAFKA_BROKERS leaves those rows owed rather than claiming
// what it cannot deliver.
//
// Each tick also releases scheduled works whose time has come, which is the one
// publish path a scheduled work has.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/f33d3r/feed-engine/internal/caeor"
	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/observ"
	"github.com/f33d3r/feed-engine/internal/sitra"
)

const (
	workOutboxTick          = 500 * time.Millisecond
	workOutboxBatch         = 32
	workOutboxParallel      = 8
	workOutboxLease         = 5 * time.Minute
	workOutboxActionTimeout = 2 * time.Minute
	workOutboxReleaseBatch  = 100
	workOutboxReportEvery   = time.Minute
	workOutboxPruneEvery    = time.Hour
	workOutboxRetention     = 7 * 24 * time.Hour
)

// localObligations run on every replica; sitraObligations need a broker.
var (
	localObligations = []dbpkg.WorkObligation{
		dbpkg.ObligationFollowerFanout,
		dbpkg.ObligationCitedUpdate,
		dbpkg.ObligationLinkPreview,
		dbpkg.ObligationZiorVoice,
		dbpkg.ObligationInterestLearning,
		dbpkg.ObligationAchievements,
		dbpkg.ObligationMediaBind,
	}
	sitraObligations = []dbpkg.WorkObligation{dbpkg.ObligationContentEvent}
)

var (
	workOutboxDone = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "work_outbox_done_total",
		Help: "Work obligations met, by action.",
	}, []string{"action"})
	workOutboxFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "work_outbox_failed_total",
		Help: "Failed attempts at work obligations, by action.",
	}, []string{"action"})
	workOutboxQuarantinedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "work_outbox_quarantined_total",
		Help: "Work obligations quarantined after their last attempt, by action.",
	}, []string{"action"})
	workOutboxQuarantined = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "work_outbox_quarantined",
		Help: "Work obligations currently quarantined and waiting for an operator, by action.",
	}, []string{"action"})
)

func init() {
	observ.MustRegister(workOutboxDone, workOutboxFailed, workOutboxQuarantinedTotal, workOutboxQuarantined)
}

// workOutbox is one running drain.
type workOutbox struct {
	h       *Handler
	kafka   *kgo.Client // nil without a broker
	actions []dbpkg.WorkObligation
}

// StartWorkOutbox starts the drain. brokers may be empty: local obligations are
// paid regardless, and Sitra-bound ones wait for a replica that has a broker.
func (h *Handler) StartWorkOutbox(ctx context.Context, brokers string) (func(), error) {
	o := &workOutbox{h: h, actions: append([]dbpkg.WorkObligation(nil), localObligations...)}
	if seeds := splitBrokerList(brokers); len(seeds) > 0 {
		client, err := kgo.NewClient(
			kgo.SeedBrokers(seeds...),
			kgo.RecordDeliveryTimeout(10*time.Second),
			kgo.RequiredAcks(kgo.AllISRAcks()),
		)
		if err != nil {
			return nil, fmt.Errorf("work outbox: connecting to Sitra Achra: %w", err)
		}
		o.kafka = client
		o.actions = append(o.actions, sitraObligations...)
	} else {
		log.Printf("[work-outbox] KAFKA_BROKERS is empty: content_event obligations (the moderation scan) wait for a replica with a broker")
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		o.run(runCtx)
	}()
	return func() {
		cancel()
		<-done
		if o.kafka != nil {
			o.kafka.Close()
		}
	}, nil
}

func splitBrokerList(raw string) []string {
	var out []string
	for _, b := range strings.Split(raw, ",") {
		if b = strings.TrimSpace(b); b != "" {
			out = append(out, b)
		}
	}
	return out
}

func (o *workOutbox) run(ctx context.Context) {
	tick := time.NewTicker(workOutboxTick)
	defer tick.Stop()
	lastReport, lastPrune := time.Time{}, time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if n, err := dbpkg.ReleaseScheduledWorks(o.h.db, workOutboxReleaseBatch); err != nil {
			log.Printf("[work-outbox] %v", err)
		} else if n > 0 {
			log.Printf("[work-outbox] released %d scheduled work(s)", n)
		}
		o.drain(ctx)
		o.drainWithdrawals(ctx)
		o.drain2257(ctx)
		if time.Since(lastReport) >= workOutboxReportEvery {
			lastReport = time.Now()
			o.report()
			o.report2257()
		}
		if time.Since(lastPrune) >= workOutboxPruneEvery {
			lastPrune = time.Now()
			if _, err := dbpkg.PruneWorkOutbox(o.h.db, time.Now().Add(-workOutboxRetention)); err != nil {
				log.Printf("[work-outbox] %v", err)
			}
		}
	}
}

// drain claims one batch and works it with bounded parallelism.
func (o *workOutbox) drain(ctx context.Context) {
	rows, err := dbpkg.ClaimWorkOutbox(o.h.db, o.actions, workOutboxBatch, workOutboxLease)
	if err != nil {
		log.Printf("[work-outbox] %v", err)
		return
	}
	sem := make(chan struct{}, workOutboxParallel)
	var wg sync.WaitGroup
	for _, row := range rows {
		wg.Add(1)
		sem <- struct{}{}
		go func(row dbpkg.WorkOutboxRow) {
			defer wg.Done()
			defer func() { <-sem }()
			o.settle(ctx, row)
		}(row)
	}
	wg.Wait()
}

// settle performs one row and records what happened to it.
func (o *workOutbox) settle(ctx context.Context, row dbpkg.WorkOutboxRow) {
	actx, cancel := context.WithTimeout(ctx, workOutboxActionTimeout)
	defer cancel()
	err := o.perform(actx, row)
	action := string(row.Action)
	if err == nil {
		if cerr := dbpkg.CompleteWorkOutbox(o.h.db, row.ID); cerr != nil {
			// The action happened and its row is still open: the lease will give
			// it back and it runs again. Every action tolerates that.
			log.Printf("[work-outbox] %v", cerr)
			return
		}
		workOutboxDone.WithLabelValues(action).Inc()
		return
	}
	workOutboxFailed.WithLabelValues(action).Inc()
	quarantined, ferr := dbpkg.FailWorkOutbox(o.h.db, row.ID, err)
	if ferr != nil {
		log.Printf("[work-outbox] %v (after: %v)", ferr, err)
		return
	}
	if quarantined {
		workOutboxQuarantinedTotal.WithLabelValues(action).Inc()
		log.Printf("[work-outbox] INCIDENT: %s for work %s quarantined after %d attempts: %v", action, row.WorkID, row.Attempt, err)
		return
	}
	log.Printf("[work-outbox] %s for work %s, attempt %d: %v", action, row.WorkID, row.Attempt, err)
}

// perform runs one obligation against a fresh read of its work. A work that was
// deleted is owed nothing, and its row closes without acting.
func (o *workOutbox) perform(ctx context.Context, row dbpkg.WorkOutboxRow) error {
	h := o.h
	w, err := dbpkg.LoadWorkSubject(h.db, row.WorkID)
	if err != nil {
		return err
	}
	if w == nil || w.Deleted {
		return nil
	}
	switch row.Action {
	case dbpkg.ObligationContentEvent:
		return o.produceContentEvent(ctx, row, w)
	case dbpkg.ObligationFollowerFanout:
		return h.publishNewPostToFollowers(w.AuthorID, w.ID)
	case dbpkg.ObligationCitedUpdate:
		return h.publishCitedWorks(w.ID)
	case dbpkg.ObligationLinkPreview:
		return h.triggerWorkLinkPreview(w.Body)
	case dbpkg.ObligationZiorVoice:
		if w.VoiceURL == "" {
			return nil
		}
		return h.ziorAnalyzeVoiceWork(ctx, w.ID, w.AuthorID, w.VoiceURL)
	case dbpkg.ObligationInterestLearning:
		return h.learnFromCitations(w.AuthorID, w.ID)
	case dbpkg.ObligationAchievements:
		return dbpkg.AwardWorkAchievements(h.db, w.ID)
	case dbpkg.ObligationMediaBind:
		return h.bindLegacyWorkMedia(ctx, w.ID)
	}
	return fmt.Errorf("no action is defined for obligation %q", row.Action)
}

// produceContentEvent hands the work to the moderation scan on content.events and
// returns once the broker has it. event_id is stable per work, so a redelivery
// after a crash is recognisable as the same event. Abraxas answers on
// safety.events, and until it does the work is visible to its author only.
func (o *workOutbox) produceContentEvent(ctx context.Context, row dbpkg.WorkOutboxRow, w *dbpkg.WorkSubject) error {
	if o.kafka == nil {
		return errors.New("content_event claimed by a replica with no broker")
	}
	assetIDs, err := dbpkg.WorkAssetIDs(o.h.db, w.ID)
	if err != nil {
		return err
	}
	facts, err := dbpkg.PolicyFactsOfAccount(o.h.db, w.AuthorID)
	if err != nil {
		return err
	}
	// Always a list: the content event's contract types media_urls as an
	// array, and a nil slice would marshal to null and be refused by Abraxas.
	media := make([]string, 0, len(w.MediaURLs)+2)
	media = append(media, w.MediaURLs...)
	if w.VideoMasterURL != "" {
		media = append(media, w.VideoMasterURL)
	}
	if w.VoiceURL != "" {
		media = append(media, w.VoiceURL)
	}
	msg := map[string]interface{}{
		"event":      "post.created",
		"event_id":   uuid.NewSHA1(uuid.NameSpaceURL, []byte("f33d3r:work_outbox:content_event:"+w.ID)).String(),
		"post_id":    w.ID,
		"work_id":    w.ID,
		"pial_id":    w.AuthorPIAL,
		"body":       w.Body,
		"has_media":  w.HasMedia(),
		"has_poll":   w.HasPoll,
		"media_urls": media,
		// The Caeor assets the work carries, each already judged at upload:
		// the work's verdict accounts for theirs.
		"asset_ids": assetIDs,
		// What the integrity policy weighs about the author.
		"is_adult_creator":   facts.IsAdultCreator,
		"account_created_at": facts.AccountCreatedAt.UTC().Format(time.RFC3339),
		// The state the row actually opens in; the verdict is the scanner's.
		"scan_state": "pending",
		"created_at": time.Now().UTC().Format(time.RFC3339),
	}
	if len(media) > 0 {
		msg["media_url"] = media[0]
	}
	value, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("encoding content event of work %s: %w", w.ID, err)
	}
	rec := &kgo.Record{Topic: sitra.TopicContent, Key: []byte(w.AuthorPIAL), Value: value}
	if err := o.kafka.ProduceSync(ctx, rec).FirstErr(); err != nil {
		return fmt.Errorf("producing content event of work %s: %w", w.ID, err)
	}
	return nil
}

// report publishes the quarantine depth and says so in the log while it is not
// zero: an obligation that did not happen must stay loud until it is handled.
func (o *workOutbox) report() {
	counts, err := dbpkg.QuarantinedWorkOutbox(o.h.db)
	if err != nil {
		log.Printf("[work-outbox] %v", err)
		return
	}
	for _, a := range append(append([]dbpkg.WorkObligation(nil), localObligations...), sitraObligations...) {
		n := counts[a]
		workOutboxQuarantined.WithLabelValues(string(a)).Set(float64(n))
		if n > 0 {
			log.Printf("[work-outbox] INCIDENT: %d %s obligation(s) quarantined — see migration 0035 for the operator queries", n, a)
		}
	}
	stuck, err := dbpkg.QuarantinedMediaWithdrawals(o.h.db)
	if err != nil {
		log.Printf("[work-outbox] %v", err)
		return
	}
	mediaWithdrawalsQuarantined.Set(float64(stuck))
	if stuck > 0 {
		log.Printf("[work-outbox] INCIDENT: %d media withdrawal(s) quarantined — deleted works whose media Caeor still serves; see migration 0036 for the operator queries", stuck)
	}
}

// ── Media withdrawals ────────────────────────────────────────────────────────

var (
	mediaWithdrawalsDone = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "media_withdrawals_done_total",
		Help: "Assets of deleted works withdrawn from Caeor.",
	})
	mediaWithdrawalsFailed = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "media_withdrawals_failed_total",
		Help: "Failed attempts to withdraw an asset of a deleted work.",
	})
	mediaWithdrawalsQuarantined = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "media_withdrawals_quarantined",
		Help: "Withdrawals waiting for an operator: deleted works whose media Caeor still serves.",
	})
)

func init() {
	observ.MustRegister(mediaWithdrawalsDone, mediaWithdrawalsFailed, mediaWithdrawalsQuarantined)
}

// drainWithdrawals pays what deleted works owe Caeor: every claimed asset is
// withdrawn, and its row closed. A failure backs off and, at the cap, is
// quarantined and reported until a person returns it.
func (o *workOutbox) drainWithdrawals(ctx context.Context) {
	rows, err := dbpkg.ClaimMediaWithdrawals(o.h.db, workOutboxBatch, workOutboxLease)
	if err != nil {
		log.Printf("[work-outbox] %v", err)
		return
	}
	for _, m := range rows {
		actx, cancel := context.WithTimeout(ctx, workOutboxActionTimeout)
		err := o.h.caeor.Withdraw(actx, m.AssetID, m.Reason)
		cancel()
		if errors.Is(err, caeor.ErrNotFound) {
			// Caeor has no such asset: there is nothing left to serve, so nothing
			// is owed. Said, because a work referenced it.
			log.Printf("[work-outbox] withdrawal of asset %s (work %s): Caeor has no such asset", m.AssetID, m.WorkID)
			err = nil
		}
		if err == nil {
			if cerr := dbpkg.CompleteMediaWithdrawal(o.h.db, m.AssetID); cerr != nil {
				log.Printf("[work-outbox] %v", cerr)
				continue
			}
			mediaWithdrawalsDone.Inc()
			continue
		}
		mediaWithdrawalsFailed.Inc()
		quarantined, ferr := dbpkg.FailMediaWithdrawal(o.h.db, m.AssetID, err)
		if ferr != nil {
			log.Printf("[work-outbox] %v (after: %v)", ferr, err)
			continue
		}
		if quarantined {
			log.Printf("[work-outbox] INCIDENT: withdrawal of asset %s (work %s) quarantined after %d attempts: %v", m.AssetID, m.WorkID, m.Attempt, err)
			continue
		}
		log.Printf("[work-outbox] withdrawal of asset %s (work %s), attempt %d: %v", m.AssetID, m.WorkID, m.Attempt, err)
	}
}

// ── media_bind ───────────────────────────────────────────────────────────────

// bindLegacyWorkMedia is the media_bind obligation: a work created before Caeor
// became the media authority names its media by legacy paths. Every path is
// resolved through Caeor (its import registered each one), the work is
// rewritten to the watermarked object of each asset, its work_assets are
// recorded, and media its author did not upload gets its "Media by" credit.
// Any path Caeor cannot answer for fails the attempt; the row backs off and is
// quarantined for a person if it never resolves.
func (h *Handler) bindLegacyWorkMedia(ctx context.Context, workID string) error {
	m, err := dbpkg.LoadWorkMediaToBind(h.db, workID)
	if err != nil {
		return err
	}
	if m == nil || m.Bound {
		return nil
	}
	b := dbpkg.WorkMediaBinding{MediaURLs: make([]string, 0, len(m.MediaURLs))}
	credit := func(a *caeor.Asset) error {
		if a.UploaderPIAL == m.AuthorPIAL || b.LineagePIAL != "" {
			return nil
		}
		handle, _, err := dbpkg.UploaderCredit(h.db, a.UploaderPIAL)
		if err != nil {
			return err
		}
		b.LineagePIAL, b.LineageHandle = a.UploaderPIAL, handle
		return nil
	}

	for i, raw := range m.MediaURLs {
		if !strings.HasPrefix(raw, "/") {
			// A GIF from the picker: external, nobody's upload.
			b.MediaURLs = append(b.MediaURLs, raw)
			continue
		}
		a, served, err := h.legacyAsset(ctx, raw, func(a *caeor.Asset) string { return a.ViewerURL() })
		if err != nil {
			return err
		}
		if err := credit(a); err != nil {
			return err
		}
		b.MediaURLs = append(b.MediaURLs, served)
		b.Assets = append(b.Assets, dbpkg.WorkAssetRef{Slot: dbpkg.SlotImage, Position: i, AssetID: a.AssetID, UploaderPIAL: a.UploaderPIAL})
	}
	if m.VoiceURL != "" {
		a, served, err := h.legacyAsset(ctx, m.VoiceURL, func(a *caeor.Asset) string { return a.ViewerURL() })
		if err != nil {
			return err
		}
		if err := credit(a); err != nil {
			return err
		}
		b.VoiceURL = served
		b.Assets = append(b.Assets, dbpkg.WorkAssetRef{Slot: dbpkg.SlotVoice, AssetID: a.AssetID, UploaderPIAL: a.UploaderPIAL})
	}
	var video *caeor.Asset
	if m.VideoMasterURL != "" {
		a, served, err := h.legacyAsset(ctx, m.VideoMasterURL, func(a *caeor.Asset) string { return a.URLs.HLSMaster })
		if err != nil {
			return err
		}
		if err := credit(a); err != nil {
			return err
		}
		video = a
		b.VideoMasterURL = served
		b.Assets = append(b.Assets, dbpkg.WorkAssetRef{Slot: dbpkg.SlotVideo, AssetID: a.AssetID, UploaderPIAL: a.UploaderPIAL})
	}
	if m.VideoPosterURL != "" {
		// The poster is the video's own when it resolves to the same asset;
		// otherwise it is an image asset of its own.
		a, served, err := h.legacyAsset(ctx, m.VideoPosterURL, func(a *caeor.Asset) string {
			if video != nil && a.AssetID == video.AssetID {
				return a.URLs.Poster
			}
			return a.ViewerURL()
		})
		if err != nil {
			return err
		}
		b.VideoPosterURL = served
		b.Assets = append(b.Assets, dbpkg.WorkAssetRef{Slot: dbpkg.SlotPoster, AssetID: a.AssetID, UploaderPIAL: a.UploaderPIAL})
	}
	return dbpkg.BindWorkMedia(h.db, workID, b)
}

// legacyAsset resolves one legacy path to its Caeor asset and the watermarked
// object the work should name instead (pick).
func (h *Handler) legacyAsset(ctx context.Context, path string, pick func(*caeor.Asset) string) (*caeor.Asset, string, error) {
	obj, err := h.caeor.Resolve(ctx, canonicalMediaPath(path))
	if err != nil {
		return nil, "", fmt.Errorf("media_bind: resolving %s: %w", path, err)
	}
	a, err := h.caeor.Get(ctx, obj.AssetID)
	if err != nil {
		return nil, "", fmt.Errorf("media_bind: describing asset %s of %s: %w", obj.AssetID, path, err)
	}
	if a.UploaderPIAL == "" {
		a.UploaderPIAL = obj.UploaderPIAL
	}
	if a.Kind == "" {
		a.Kind = obj.Kind
	}
	if a.UploaderPIAL == "" {
		return nil, "", fmt.Errorf("media_bind: asset %s of %s has no uploader", a.AssetID, path)
	}
	served := pick(a)
	if served == "" {
		return nil, "", fmt.Errorf("media_bind: asset %s of %s has no watermarked object for its slot yet (status %s)", a.AssetID, path, a.Status)
	}
	return a, served, nil
}
