package live

// scan.go — the live lane's content-safety path.
//
// A live stream cannot be scanned once at upload time, because there is no
// upload: the content arrives while it is being watched. So the transcoder
// drops one keyframe every SampleInterval into a private directory that only
// this process reads, and every frame goes through the same safety path the
// Work lane uses.
//
// The frames directory is a separate volume from the delivery tree. A sampled
// frame is evidence, not content, and is never reachable from the live edge.
//
// A stream starts at scan_state 'pending_scan' and moves only on evidence.
// Nothing here writes 'clean' without a scanner having said so.
//
// The banned-content gate runs in two layers before any classification:
// the exact-bytes sha256 layer, queried locally, and the perceptual layer,
// asked of content-scan. feed-engine never computes a perceptual hash of its
// own — content-scan produced the values banned_content_hashes holds, and such
// a hash matches only when the same algorithm produced both sides. What this
// lane sends is a still frame, so the layer that answers is the frame hash;
// the audio radius on the same endpoint applies to the Work lane's uploads.
// Both report near matches the same way, and this file does not care which
// layer found one.
//
// Fail-closed contract with the classifier (content-scan, /v1/scan/image and
// /v1/hashes/check-media):
//
//   - A frame the classifier did not judge is recorded as 'pending_scan' with
//     risk 'unscanned'. It is never recorded as clean. There is no code path in
//     this file that turns an unanswered request into a clean verdict.
//   - The frame stays on disk so it is re-judged when the classifier returns.
//   - The backlog is capped per stream so a long outage cannot fill the volume;
//     an evicted frame keeps its evidence row, only its pixels are dropped.
//   - An outage lasting longer than classifierOutageGrace escalates every
//     affected stream to 'human_review'. The broadcast keeps running — a human
//     is put in the loop rather than the site being taken down.
//   - Stream scan_state only ever ratchets upward in severity. One clean frame
//     after a nude one does not un-gate a stream.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/scanclient"
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

// errClassifierDown marks a frame that could not be judged because the
// classifier did not answer or answered something this build cannot read.
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

// classifierResult is the shape content-scan returns from /v1/scan/image.
// nsfw_score/is_nsfw/labels come from the NudeNet detector; gore_score,
// risk_level, recommendation and signals come from its risk aggregation.
type classifierResult struct {
	IsNSFW         bool     `json:"is_nsfw"`
	NSFWScore      float64  `json:"nsfw_score"`
	Labels         []string `json:"labels"`
	GoreScore      float64  `json:"gore_score"`
	RiskLevel      string   `json:"risk_level"`
	Recommendation string   `json:"recommendation"`
	Signals        []string `json:"signals"`
}

// verdict maps a classifier answer onto a live scan_state. An answer this build
// cannot interpret is an error, never a clean verdict.
func (r *classifierResult) verdict() (state, risk string, signals []string, err error) {
	signals = dedupeSignals(append(append([]string{}, r.Labels...), r.Signals...))
	switch r.Recommendation {
	case "auto_block":
		return "blocked", "block", signals, nil
	case "human_review":
		return "human_review", "review", signals, nil
	case "age_gate":
		return "age_gated", "age_gate", signals, nil
	case "approve":
		return "clean", "clean", signals, nil
	case "":
		// A classifier that reports only the nudity verdict. Honour exactly
		// what it said rather than inventing the rest of the risk picture.
		if r.IsNSFW {
			return "age_gated", "age_gate", signals, nil
		}
		return "clean", "clean", signals, nil
	default:
		return "", "", nil, fmt.Errorf("unknown recommendation %q", r.Recommendation)
	}
}

func dedupeSignals(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// Scanner walks sampled keyframes and drives live_streams.scan_state.
type Scanner struct {
	mgr        *Manager
	dir        string
	classifier string // base URL of the image classifier, empty when none
	client     *http.Client

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
		mgr:        mgr,
		dir:        envOr("LIVE_SCAN_DIR", "/app/live-scan"),
		classifier: strings.TrimRight(strings.TrimSpace(os.Getenv("CONTENT_SCAN_URL")), "/"),
		client:     &http.Client{Timeout: 20 * time.Second},
		escalated:  make(map[string]bool),
		recorded:   make(map[string]bool),
	}
}

// run sweeps the frame directory until ctx is cancelled.
func (s *Scanner) run(ctx context.Context) {
	if s.classifier == "" {
		// Say so once, loudly. A safety path with no classifier still catches
		// known-banned hashes, and pretending otherwise would be the lie.
		log.Printf("[live-scan] CONTENT_SCAN_URL is unset — sampled frames are checked against " +
			"the banned-hash list and published to Sitra Achra, but no visual classifier runs. " +
			"Streams stay at scan_state='pending_scan' until one is configured.")
	} else {
		log.Printf("[live-scan] classifier: %s", s.classifier)
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
	state, err := s.currentScanState(streamID)
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
		next, err := s.processFrame(ctx, streamID, frameName, path, state)
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

// currentScanState reads the stream's scan_state so a verdict can be compared
// against it before it is applied.
func (s *Scanner) currentScanState(streamID string) (string, error) {
	stream, err := dbpkg.GetLiveStreamByID(s.mgr.db, streamID)
	if err != nil {
		return "", err
	}
	return stream.ScanState, nil
}

// processFrame judges one sampled keyframe and returns the stream's scan_state
// after the verdict has been applied. It returns errClassifierDown when the
// classifier could not be reached or could not be understood; in that case the
// frame is left on disk, an 'unscanned' evidence row is written, and the
// stream's state is untouched — it is never advanced to clean.
func (s *Scanner) processFrame(ctx context.Context, streamID, frameName, path, state string) (string, error) {
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

	// 1. Banned-hash check. This is the same list the upload path enforces and
	//    it is the one verdict that ends a broadcast outright.
	banned, category, err := dbpkg.IsBannedHash(s.mgr.db, "sha256", hash)
	if err != nil {
		return state, fmt.Errorf("banned-hash lookup: %w", err)
	}
	if banned {
		sample.Verdict = "blocked"
		sample.RiskLevel = "block"
		sample.Signals = []string{"banned_hash:" + category}
		if err := dbpkg.RecordScanSample(s.mgr.db, sample); err != nil {
			return state, err
		}
		s.mgr.publishScanSample(streamID, hash, frameName, sample.Verdict)
		return "blocked", s.mgr.BlockStream(ctx, streamID, "live-scan: banned hash ("+category+")")
	}

	// 1b. Perceptual banned-hash check. content-scan produced every perceptual
	//     hash the registry holds, and such a hash matches only when the same
	//     algorithm produced both sides — so the frame is sent there to be
	//     hashed rather than hashed here. A re-encoded broadcast of banned
	//     content survives the exact-bytes layer above and is caught by this one.
	var perceptualNears []scanclient.NearMatch
	var perceptualDegraded []string
	if sc := scanclient.Shared(); sc.Configured() {
		verdict, verr := sc.CheckMedia(ctx, scanclient.KindImage, frameName, raw)
		if verr != nil {
			// content-scan is also the classifier, so a frame it cannot answer
			// for is held unjudged on the existing outage path rather than
			// passed along with one layer of the gate quietly missing.
			return state, s.markUnscanned(streamID, frameName, hash, sample, verr)
		}
		if verdict.Banned {
			sample.Verdict = "blocked"
			sample.RiskLevel = "block"
			sample.Signals = []string{"banned_" + verdict.HashType + ":" + verdict.Category}
			if err := dbpkg.RecordScanSample(s.mgr.db, sample); err != nil {
				return state, err
			}
			s.mgr.publishScanSample(streamID, hash, frameName, sample.Verdict)
			return "blocked", s.mgr.BlockStream(ctx, streamID,
				"live-scan: banned "+verdict.HashType+" ("+verdict.Category+")")
		}
		perceptualNears = verdict.Nears()
		perceptualDegraded = verdict.Degraded
	}

	// 2. Visual classification, when a classifier is configured.
	if s.classifier == "" {
		// No classifier. The frame is recorded as unjudged and published, and
		// the stream stays wherever it is. Nothing here calls it clean.
		sample.RiskLevel = "unscanned"
		sample.Signals = []string{"classifier_unconfigured"}
		if err := dbpkg.RecordScanSample(s.mgr.db, sample); err != nil {
			return state, err
		}
		s.mgr.publishScanSample(streamID, hash, frameName, sample.Verdict)
		return state, nil
	}

	res, err := s.classify(ctx, raw, frameName)
	if err == nil {
		var verdict, risk string
		var signals []string
		verdict, risk, signals, err = res.verdict()
		if err == nil {
			sample.Verdict = verdict
			sample.RiskLevel = risk
			sample.Signals = signals
		}
	}
	if err != nil {
		return state, s.markUnscanned(streamID, frameName, hash, sample, err)
	}

	s.clearOutage()

	// The perceptual layer's findings ride on the same evidence row as the
	// classifier's, so one sample carries everything that was known about the
	// frame — including the part of the gate that could not be computed.
	if len(perceptualDegraded) > 0 {
		sample.Signals = append(sample.Signals,
			"perceptual_degraded:"+strings.Join(perceptualDegraded, "|"))
	}
	for _, near := range perceptualNears {
		// One signal per layer that found something. Detail carries that
		// layer's own measurement — a Hamming distance for a frame hash, a
		// similarity score for audio — under one signal name, so a moderator
		// reads the same vocabulary whichever registry produced the finding.
		sample.Signals = append(sample.Signals, fmt.Sprintf("banned_%s_near:%s:%s",
			near.HashType, near.Category, near.Detail()))
	}
	if len(perceptualNears) > 0 {
		// Close to banned content but not equal to it. Perceptual hashes of
		// low-detail frames collide and audio below the block cut is derived
		// rather than identical, so a human decides rather than a radius
		// measurement ending someone's broadcast.
		if scanStateSeverity["human_review"] > scanStateSeverity[sample.Verdict] {
			sample.Verdict = "human_review"
			sample.RiskLevel = "review"
		}
	}

	if err := dbpkg.RecordScanSample(s.mgr.db, sample); err != nil {
		return state, err
	}

	// 3. Put the sample on the safety bus so the streaming safety fabric sees
	//    every frame, whatever the verdict was.
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

// clearOutage closes the outage window after a successful classification.
func (s *Scanner) clearOutage() {
	if s.outageSince.IsZero() {
		return
	}
	log.Printf("[live-scan] classifier recovered after %s (was: %s) — working through the backlog",
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
		log.Printf("[live-scan] classifier %s unreachable for %s (%s) — sampled frames are held "+
			"unjudged at scan_state='pending_scan'; nothing is being marked clean",
			s.classifier, down.Round(time.Second), s.outageReason)
	}

	if down < classifierOutageGrace {
		return
	}
	for _, streamID := range s.streamsWithPendingFrames() {
		if s.escalated[streamID] {
			continue
		}
		state, err := s.currentScanState(streamID)
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

// classify posts one frame to the image classifier and returns its verdict.
func (s *Scanner) classify(ctx context.Context, raw []byte, frameName string) (*classifierResult, error) {
	body, err := json.Marshal(map[string]string{
		"image_b64": base64.StdEncoding.EncodeToString(raw),
		"filename":  frameName,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.classifier+"/v1/scan/image", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// content-scan answers 503 when its detector is not loaded. Carrying the
		// body into the error is what makes that diagnosable from the log.
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	var out classifierResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}
