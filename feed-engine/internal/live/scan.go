package live

// scan.go — the live lane's content-safety path.
//
// A live stream cannot be scanned once at upload time, because there is no
// upload: the content arrives while it is being watched. So the transcoder
// drops one keyframe every SampleInterval into a private directory that only
// this process reads, and every frame is put to Abraxas, the integrity
// authority, at POST /v1/check/frame. Abraxas runs every layer — the banned
// registries (exact bytes and perceptual) and the visual classifier — and
// answers one verdict: allow | age_gate | review | block. This file judges
// nothing itself.
//
// The frames directory is a separate volume from the delivery tree. A sampled
// frame is evidence, not content, and is never reachable from the live edge.
//
// A stream starts at scan_state 'pending_scan' and moves only on evidence.
// Nothing here writes 'clean' without Abraxas having said so.
//
// A banned match ends the broadcast here, at once. What follows for the
// account — the CSAM report and the ban or hold — arrives as a banned_match on
// safety.events and is carried out by the safety consumer, the one path every
// banned match takes whatever surface it was found on.
//
// Fail-closed contract with Abraxas:
//
//   - A frame Abraxas did not judge is recorded as 'pending_scan' with risk
//     'unscanned'. It is never recorded as clean. There is no code path in
//     this file that turns an unanswered request into a clean verdict.
//   - The frame stays on disk so it is re-judged when Abraxas returns.
//   - The backlog is capped per stream so a long outage cannot fill the volume;
//     an evicted frame keeps its evidence row, only its pixels are dropped.
//   - An outage lasting longer than classifierOutageGrace escalates every
//     affected stream to 'human_review'. The broadcast keeps running — a human
//     is put in the loop rather than the site being taken down.
//   - Stream scan_state only ever ratchets upward in severity. One clean frame
//     after a nude one does not un-gate a stream.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/f33d3r/feed-engine/internal/abraxas"
	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

// maxFrameBytes caps one sampled keyframe. A JPEG still of a 4K frame is well
// under this; anything larger is not a frame the transcoder wrote.
const maxFrameBytes = 8 << 20

// scanSweepInterval is how often the sampler walks the frame directory. The
// transcoder writes a frame every 10 s, so a 15 s sweep never falls behind.
const scanSweepInterval = 15 * time.Second

// maxRetainedFrames bounds the per-stream backlog held for a classifier that is
// not answering. At one frame per 10 s this is ~40 minutes of unjudged footage;
// beyond it the oldest pixels are dropped, never the evidence rows.
const maxRetainedFrames = 240

// classifierOutageGrace is how long the classifier may be unreachable before
// every stream with unjudged frames is escalated to human review. Two sweeps of
// slack absorb a container restart without paging a moderator.
const classifierOutageGrace = 5 * time.Minute

// outageLogInterval rate-limits the outage log so a long outage does not drown
// every other line in the log.
const outageLogInterval = time.Minute

// errClassifierDown marks a frame that could not be judged because Abraxas
// did not answer or answered something this build cannot read.
var errClassifierDown = errors.New("classifier unavailable")

// scanStateSeverity ranks the scan states so a verdict can only ratchet a
// stream upward. A scanner never downgrades a stream; only a human does.
var scanStateSeverity = map[string]int{
	"pending_scan": 0,
	"clean":        1,
	"age_gated":    2,
	"human_review": 3,
	"flagged":      4,
	"blocked":      5,
}

// frameState maps Abraxas's frame verdict onto a live scan_state and risk.
func frameState(v *abraxas.FrameVerdict) (state, risk string) {
	switch v.Verdict {
	case "block":
		return "blocked", "block"
	case "review":
		return "human_review", "review"
	case "age_gate":
		return "age_gated", "age_gate"
	}
	return "clean", "clean"
}

// Scanner walks sampled keyframes and drives live_streams.scan_state.
type Scanner struct {
	mgr     *Manager
	dir     string
	abraxas *abraxas.Client

	// Outage bookkeeping. Owned by the single run goroutine — no other
	// goroutine reads or writes these.
	outageSince   time.Time
	outageReason  string
	lastOutageLog time.Time
	escalated     map[string]bool // stream id -> escalated during this outage
	recorded      map[string]bool // "stream/frame" -> unscanned evidence row written
}

func newScanner(mgr *Manager) *Scanner {
	return &Scanner{
		mgr:       mgr,
		dir:       envOr("LIVE_SCAN_DIR", "/app/live-scan"),
		abraxas:   mgr.Abraxas,
		escalated: make(map[string]bool),
		recorded:  make(map[string]bool),
	}
}

// run sweeps the frame directory until ctx is cancelled.
func (s *Scanner) run(ctx context.Context) {
	if !s.abraxas.Configured() {
		// Say so once, loudly. Every frame is then held unjudged, recorded as
		// such, and escalated after the outage grace — never passed as clean.
		log.Printf("[live-scan] ABRAXAS_URL is unset — no sampled frame can be judged; " +
			"frames are held at scan_state='pending_scan' and streams escalate to human review")
	}

	ticker := time.NewTicker(scanSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.sweep(ctx); err != nil {
				log.Printf("[live-scan] sweep: %v", err)
			}
		}
	}
}

// sweep processes every frame currently waiting in the directory.
func (s *Scanner) sweep(ctx context.Context) error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // transcoder has not produced a frame yet
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		streamID := e.Name()
		if err := s.sweepStream(ctx, streamID); err != nil {
			log.Printf("[live-scan] stream %s: %v", streamID, err)
		}
	}
	s.reportOutage()
	return nil
}

func (s *Scanner) sweepStream(ctx context.Context, streamID string) error {
	dir := filepath.Join(s.dir, streamID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	// The stream's current state is read once per sweep and carried forward, so
	// the ratchet costs one query per stream rather than one per frame.
	state, ownerPIAL, err := s.currentScanState(streamID)
	if errors.Is(err, dbpkg.ErrLiveStreamNotFound) {
		// The stream row is gone, so live_scan_samples can hold no evidence for
		// it (foreign key) and nothing can act on a verdict. The frames are
		// orphans: drop them rather than retrying this directory forever.
		log.Printf("[live-scan] stream %s has no row — discarding %d orphaned frames", streamID, len(entries))
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("discarding orphaned frame directory %s: %w", dir, err)
		}
		return nil
	}
	if err != nil {
		return err
	}

	// Frame names are written with a monotonic timestamp prefix and os.ReadDir
	// sorts by name, so this walks oldest first.
	var frames []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jpg") {
			continue
		}
		frames = append(frames, e.Name())
	}

	for _, frameName := range frames {
		path := filepath.Join(dir, frameName)
		next, err := s.processFrame(ctx, streamID, ownerPIAL, frameName, path, state)
		if err != nil {
			if errors.Is(err, errClassifierDown) {
				// The classifier is not answering. Every remaining frame of
				// every remaining stream would fail the same way, so stop
				// walking rather than hammering a service that is down. The
				// frames stay on disk and are re-judged when it returns.
				break
			}
			return err
		}
		state = next
		// The frame has been judged and its verdict recorded; the evidence row
		// outlives the pixels, which are deleted so the volume stays bounded.
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("removing scanned frame %s: %w", path, err)
		}
		delete(s.recorded, streamID+"/"+frameName)

		if state == "blocked" {
			// The broadcast has been dropped and the stream is at the top of the
			// severity scale, so no later frame can change the outcome. The
			// frames that decided it keep their evidence rows; the rest are
			// discarded rather than re-posted to the classifier every sweep for
			// the lifetime of the directory.
			log.Printf("[live-scan] stream %s blocked — discarding its remaining sampled frames", streamID)
			if err := os.RemoveAll(dir); err != nil {
				return fmt.Errorf("discarding frames of blocked stream %s: %w", dir, err)
			}
			for _, name := range frames {
				delete(s.recorded, streamID+"/"+name)
			}
			return nil
		}
	}

	return s.enforceBacklogCap(streamID, dir)
}

// currentScanState reads the stream's scan_state, so a verdict can be compared
// against it before it is applied, and its broadcaster's PIAL, which Abraxas
// reports a banned match against.
func (s *Scanner) currentScanState(streamID string) (string, string, error) {
	stream, err := dbpkg.GetLiveStreamByID(s.mgr.db, streamID)
	if err != nil {
		return "", "", err
	}
	return stream.ScanState, stream.AuthorPIAL, nil
}

// broadcasterIsAdultCreator reads the broadcaster's standing, which Abraxas's
// policy weighs when it judges a frame.
func (s *Scanner) broadcasterIsAdultCreator(streamID string) (bool, error) {
	stream, err := dbpkg.GetLiveStreamByID(s.mgr.db, streamID)
	if err != nil {
		return false, err
	}
	facts, err := dbpkg.PolicyFactsOfAccount(s.mgr.db, stream.AuthorID)
	if err != nil {
		return false, err
	}
	return facts.IsAdultCreator, nil
}

// processFrame judges one sampled keyframe and returns the stream's scan_state
// after the verdict has been applied. It returns errClassifierDown when Abraxas
// could not be reached or could not be understood; in that case the frame is
// left on disk, an 'unscanned' evidence row is written, and the stream's state
// is untouched — it is never advanced to clean.
func (s *Scanner) processFrame(ctx context.Context, streamID, ownerPIAL, frameName, path, state string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return state, err
	}
	defer f.Close()

	raw, err := io.ReadAll(io.LimitReader(f, maxFrameBytes))
	if err != nil {
		return state, fmt.Errorf("reading frame %s: %w", frameName, err)
	}
	if len(raw) == 0 {
		return state, nil // transcoder mid-write; the next sweep picks it up
	}

	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])

	sample := dbpkg.LiveScanSample{
		StreamID:  streamID,
		FrameName: frameName,
		SHA256:    hash,
		Verdict:   "pending_scan",
	}

	adult, err := s.broadcasterIsAdultCreator(streamID)
	if err != nil {
		return state, err
	}
	// Abraxas reads the frame from the live-scan volume it mounts, at the same
	// <stream>/<frame> path; the hash binds its read to this one.
	verdict, err := s.abraxas.CheckFrame(ctx, abraxas.FrameCheck{
		StreamID:       streamID,
		FrameName:      frameName,
		PIAL:           ownerPIAL,
		SHA256:         hash,
		IsAdultCreator: adult,
	})
	if err != nil {
		return state, s.markUnscanned(streamID, frameName, hash, sample, err)
	}
	s.clearOutage()

	sample.Verdict, sample.RiskLevel = frameState(verdict)
	sample.Signals = append([]string(nil), verdict.Signals...)
	if verdict.RiskLevel != "" {
		sample.RiskLevel = verdict.RiskLevel
	}
	if verdict.BannedMatch != nil {
		sample.Signals = append(sample.Signals, "banned_"+verdict.BannedMatch.Layer+":"+verdict.BannedMatch.Category)
	}
	if err := dbpkg.RecordScanSample(s.mgr.db, sample); err != nil {
		return state, err
	}

	// The sample goes on the safety bus so the streaming safety fabric sees
	// every frame, whatever the verdict was.
	s.mgr.publishScanSample(streamID, hash, frameName, sample.Verdict)

	if sample.Verdict == "blocked" {
		reason := fmt.Sprintf("live-scan: block (%s)", strings.Join(sample.Signals, ","))
		return "blocked", s.mgr.BlockStream(ctx, streamID, reason)
	}

	// Ratchet: a verdict is applied only when it is more severe than the state
	// the stream already holds. One clean frame never un-gates a stream.
	if scanStateSeverity[sample.Verdict] > scanStateSeverity[state] {
		reason := fmt.Sprintf("live-scan: %s (%s)", sample.RiskLevel, strings.Join(sample.Signals, ","))
		if err := dbpkg.SetStreamScanState(s.mgr.db, streamID, sample.Verdict, reason, "live-scan"); err != nil {
			return state, err
		}
		return sample.Verdict, nil
	}
	return state, nil
}

// markUnscanned records that a frame could not be judged, opens the outage
// window if it is not already open, and returns errClassifierDown so the caller
// stops the sweep. The evidence row is written once per frame; a frame that
// fails again on a later sweep does not rewrite it.
func (s *Scanner) markUnscanned(streamID, frameName, hash string, sample dbpkg.LiveScanSample, cause error) error {
	if s.outageSince.IsZero() {
		s.outageSince = time.Now()
		s.outageReason = cause.Error()
		s.lastOutageLog = time.Time{}
		s.escalated = make(map[string]bool)
	}

	key := streamID + "/" + frameName
	if !s.recorded[key] {
		sample.Verdict = "pending_scan"
		sample.RiskLevel = "unscanned"
		sample.Signals = []string{"classifier_unavailable"}
		if err := dbpkg.RecordScanSample(s.mgr.db, sample); err != nil {
			return err
		}
		s.mgr.publishScanSample(streamID, hash, frameName, sample.Verdict)
		s.recorded[key] = true
	}
	return fmt.Errorf("%w: %v", errClassifierDown, cause)
}

// clearOutage closes the outage window after Abraxas answered.
func (s *Scanner) clearOutage() {
	if s.outageSince.IsZero() {
		return
	}
	log.Printf("[live-scan] Abraxas recovered after %s (was: %s) — working through the backlog",
		time.Since(s.outageSince).Round(time.Second), s.outageReason)
	s.outageSince = time.Time{}
	s.outageReason = ""
	s.escalated = make(map[string]bool)
}

// reportOutage logs an open outage at a bounded rate and, once the outage has
// run past the grace window, escalates every stream still holding unjudged
// frames to human review. The broadcast is not stopped: a moderator is put in
// the loop instead, which fails closed without taking the live lane down.
func (s *Scanner) reportOutage() {
	if s.outageSince.IsZero() {
		return
	}
	down := time.Since(s.outageSince)

	if time.Since(s.lastOutageLog) >= outageLogInterval {
		s.lastOutageLog = time.Now()
		log.Printf("[live-scan] Abraxas unreachable for %s (%s) — sampled frames are held "+
			"unjudged at scan_state='pending_scan'; nothing is being marked clean",
			down.Round(time.Second), s.outageReason)
	}

	if down < classifierOutageGrace {
		return
	}
	for _, streamID := range s.streamsWithPendingFrames() {
		if s.escalated[streamID] {
			continue
		}
		state, _, err := s.currentScanState(streamID)
		if err != nil {
			log.Printf("[live-scan] escalation: stream %s: %v", streamID, err)
			continue
		}
		s.escalated[streamID] = true
		if scanStateSeverity["human_review"] <= scanStateSeverity[state] {
			continue
		}
		reason := fmt.Sprintf("live-scan: classifier unreachable for %s — unjudged frames escalated",
			down.Round(time.Second))
		if err := dbpkg.SetStreamScanState(s.mgr.db, streamID, "human_review", reason, "live-scan"); err != nil {
			log.Printf("[live-scan] escalation: stream %s: %v", streamID, err)
			continue
		}
		log.Printf("[live-scan] stream %s escalated to human_review: %s", streamID, reason)
	}
}

// streamsWithPendingFrames lists the streams that still hold unjudged frames.
func (s *Scanner) streamsWithPendingFrames() []string {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		frames, err := os.ReadDir(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		for _, f := range frames {
			if !f.IsDir() && strings.HasSuffix(f.Name(), ".jpg") {
				out = append(out, e.Name())
				break
			}
		}
	}
	return out
}

// enforceBacklogCap keeps the held-frame backlog bounded during an outage. The
// oldest pixels beyond the cap are dropped; their evidence rows are written
// first, so what was seen is still on record even though the frame is gone.
func (s *Scanner) enforceBacklogCap(streamID, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var frames []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jpg") {
			frames = append(frames, e.Name())
		}
	}
	if len(frames) <= maxRetainedFrames {
		return nil
	}

	drop := frames[:len(frames)-maxRetainedFrames]
	for _, frameName := range drop {
		path := filepath.Join(dir, frameName)
		key := streamID + "/" + frameName
		if !s.recorded[key] {
			raw, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("reading evicted frame %s: %w", path, err)
			}
			sum := sha256.Sum256(raw)
			if err := dbpkg.RecordScanSample(s.mgr.db, dbpkg.LiveScanSample{
				StreamID:  streamID,
				FrameName: frameName,
				SHA256:    hex.EncodeToString(sum[:]),
				Verdict:   "pending_scan",
				RiskLevel: "unscanned",
				Signals:   []string{"classifier_unavailable", "evicted_unscanned"},
			}); err != nil {
				return err
			}
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("evicting unscanned frame %s: %w", path, err)
		}
		delete(s.recorded, key)
	}
	log.Printf("[live-scan] stream %s: backlog over %d frames — dropped %d oldest unjudged frames "+
		"(evidence rows kept, verdict stays 'pending_scan')", streamID, maxRetainedFrames, len(drop))
	return nil
}
