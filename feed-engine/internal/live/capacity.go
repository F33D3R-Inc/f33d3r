package live

// capacity.go — the live lane's governor.
//
// Every broadcast spawns a ladder, and a ladder is the most expensive thing
// this platform does. Before this file existed there was no accounting of any
// kind: the media server accepted every publish, each publish spawned its own
// ffmpeg, and the box was oversubscribed by arithmetic nobody was doing. Past
// the ceiling the failure was not slow video, it was destroyed video — the
// media server dropped ladders as slow readers, the pulled RTSP sequence broke,
// the demuxers errored out and the encodes died, while every row still read
// "live". Broadcasts did not share the shortage. They took it out on each other.
//
// The rule this file exists to enforce is one sentence long:
//
//	A broadcast that is already running is never damaged to make room for one
//	that is not.
//
// Everything below is that rule spelled out. Capacity is reserved at the door,
// before the media server has accepted a single frame, because a source that is
// never admitted cannot spawn a ladder and cannot take a core off anybody. What
// is left over decides how good a ladder the new broadcast gets, never whether
// the ones already running keep theirs.
//
// Two budgets are spent here and they are not interchangeable:
//
//	CPU cores  — what libx264 costs. Divisible, measurable, and the thing that
//	             actually ran out.
//	NVENC      — what the GPU costs. Not divisible at all: the card grants a
//	             fixed number of concurrent encode sessions and the next one
//	             fails outright. Measured on this host at 8 (see nvencSessions).
//
// A ladder is costed against whichever it uses. Every rung is encoded: the
// moving watermark is burned into the pixels of every rung a viewer can fetch,
// and only an encode puts it there.

import (
	"fmt"
	"log"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ── the measured cost of an encode ───────────────────────────────────────────
//
// These are not estimates. They are fitted to measurements of the ladder's own
// ffmpeg invocation against a 1080p30 H.264 source on this host. See
// capacity_test.go, which fails if the model and those measurements stop
// agreeing.
//
// The anchor is a production measurement rather than a benchmark: a real 1080p30
// RTMP broadcast's ladder, read out of /proc, cost 3.674 cores sustained with
// three libx264 rungs. The other three points come from running the same
// invocation four ways back to back in one load window, which makes the ratios
// between them trustworthy even though the box was busy:
//
//	libx264, three rungs           4.114 cores
//	libx264, top rung passed through 2.751       -33%
//	NVENC, three rungs             2.114         -49%
//	NVENC, top rung passed through 1.891         -54%
//
// The passthrough rows are history. Passing the top rung through was this
// lane's largest single saving and it is gone on purpose: a rung remuxed from
// the publisher's own elementary stream carries no watermark, and every
// viewer-facing copy on this platform carries one. The price is the single
// 1080p libx264 encode, 1.14 cores — a third of the ladder in one rung — on
// every H.264 source that used to qualify. On the 7.8 core budget this host
// runs to, that takes the software lane from three concurrent 1080p30
// broadcasts to two, and the hardware lane from four (two sessions each) to
// two (three sessions each, against a card that opens eight).
//
// The fixed cost that no encoder choice can touch is around 1.7 cores, over
// 40% of the ladder, which is why hardware encoding raises the ceiling by much
// less than a naive reading of "the GPU does the encoding" suggests.
//
// The coefficients are fitted at the busy-box figures, not at an idle box's.
// A budget only matters when the machine is loaded, and that is exactly when an
// encode costs more than it does on a quiet one.
//
// libx264 at veryfast/zerolatency costs very close to linearly in pixel rate
// once that fixed cost is taken out separately, which is why the model is a
// single coefficient rather than a table: it stays right for the 1440p and
// 2160p rungs, which no measurement here covered but a phone will deliver.

// x264CoresPerMegapixel is the core cost of encoding one megapixel per second
// with libx264 at the ladder's preset. MEASURED on this host.
const x264CoresPerMegapixel = 0.0196

// fixedCoresPerMegapixel is the per-broadcast cost that no encoder choice
// avoids, expressed against the SOURCE pixel rate: pulling the source back over
// loopback RTSP, decoding it, normalising the frame rate, scaling it once per
// rung, the poster and safety-sampler branches, the AAC track each rung
// carries, and muxing every rendition into fMP4.
//
// It is charged against the CPU budget even when the encoders are on the GPU,
// because all of it happens on the CPU either way — and it is the largest
// single item in the ladder, which is the fact that decides how much hardware
// encoding is actually worth here.
//
// The coefficient is 0.0031 lower than the production measurement it is fitted
// to, and deliberately: that measurement was taken against a ladder whose
// poster branch scaled every frame before decimating it. Reordering those two
// filters was worth a measured 0.19 cores per 1080p30 broadcast and the ladder
// now does it the cheap way round, so the model costs the code that runs rather
// than the code it replaced. MEASURED on this host.
const fixedCoresPerMegapixel = 0.0238

// markCoresPerMegapixel is the moving watermark: one drawtext pass over the
// source frame, ahead of the split, so the mark is drawn once per broadcast and
// every rung inherits it through its own scale. drawtext's work is the glyph
// blend over the text's own pixels plus its expression evaluation, not a pass
// over the frame, so the figure is small; it is charged against the source
// pixel rate so it scales with what is being drawn on.
//
// ESTIMATED, NOT MEASURED: 0.05 cores for a 1080p30 source, chosen to sit
// above what a text blend of this size costs. Measure it the way the other
// coefficients were measured — the ladder's own invocation, /proc, a loaded
// box — and replace this figure with the measurement.
const markCoresPerMegapixel = 0.0008

// nvencCoresPerMegapixel is what a rung costs the CPU when the GPU encodes it:
// the frame still has to be handed to the card and the packet taken back.
// MEASURED on this host.
const nvencCoresPerMegapixel = 0.0021

// aspect is the width assumed for a rung of a given height when costing it
// before the real geometry is known. 16:9 is what every capture device in the
// brief produces; a wider source costs marginally more and is corrected the
// moment the plan is drawn against real geometry.
const aspect = 16.0 / 9.0

// megapixelsPerSecond is the pixel rate of one rung at a given frame rate.
func megapixelsPerSecond(height, fps int) float64 {
	width := math.Round(float64(height)*aspect/2) * 2
	return width * float64(height) * float64(fps) / 1_000_000
}

// rungCPUCost is what one rung costs in cores under a given encoder.
func rungCPUCost(height, fps int, encoder string) float64 {
	mpx := megapixelsPerSecond(height, fps)
	if encoder == EncoderNVENC {
		return mpx * nvencCoresPerMegapixel
	}
	return mpx * x264CoresPerMegapixel
}

// broadcastCPUCost is what one broadcast costs whatever its rungs: the fixed
// work every ladder does on the CPU, and the watermark drawn on the source.
func broadcastCPUCost(sourceHeight, fps int) float64 {
	return megapixelsPerSecond(sourceHeight, fps) * (fixedCoresPerMegapixel + markCoresPerMegapixel)
}

// planCPUCost is the whole cost of a ladder: the per-broadcast cost of its
// source plus each of its rungs under the encoder carrying them. It is the one
// place a ladder is costed, so a lease costed at planning and a lease recosted
// after a shed rung can never be costed by two different sums.
func planCPUCost(sourceHeight, fps int, rungs []PlannedRung, encoder string) float64 {
	total := broadcastCPUCost(sourceHeight, fps)
	for _, r := range rungs {
		total += rungCPUCost(r.Height, fps, encoder)
	}
	return total
}

// ── budgets ──────────────────────────────────────────────────────────────────

// nvencSessions is how many concurrent NVENC encode sessions this host's GPU
// will open. MEASURED: on the GeForce GTX 1060 6GB in this box, driver
// 580.178.04, session 9 fails with "OpenEncodeSessionEx failed: incompatible
// client key (21)" and sessions 1..8 all encode correctly. It is a driver
// licensing ceiling on consumer cards, not a load limit, so it does not
// degrade — it refuses. Budgeting against it is the only way the ninth rung is
// never attempted.
const nvencSessions = 8

// cpuReserveFraction is the share of the box the live lane may never take.
// This host runs forty other containers, the media server's own ingest, and
// the edge that serves the segments; a live lane sized to the whole machine
// starves the platform it is part of and then starves itself, because the
// ingest it is reading from is one of the things it starved.
const cpuReserveFraction = 0.35

// minRungCPU is the cost of the cheapest ladder that is worth calling a
// broadcast: one 480p30 rung. A box that cannot afford this cannot afford a
// broadcast at all, and says so instead of accepting one it will destroy.
var minRungCPU = rungCPUCost(480, 30, EncoderX264) + broadcastCPUCost(480, 30)

// nominalLadderCPU is what an unknown broadcast is reserved at. Geometry is not
// known when the publish is authorised — it is not known until the source has
// delivered a keyframe and the ladder has probed it — so the reservation is
// taken at the cost of the ladder a 1080p30 source would get on the software
// encoder, which is what a desktop OBS publish overwhelmingly is.
//
// Reserving high and correcting down when the truth arrives is the only safe
// direction for this error. Reserving low and correcting up means admitting a
// broadcast the box cannot carry and then discovering it with somebody else's
// video already broken, which is the failure this file exists to end.
var nominalLadderCPU = func() float64 {
	total := broadcastCPUCost(1080, 30)
	for _, h := range []int{480, 720, 1080} {
		total += rungCPUCost(h, 30, EncoderX264)
	}
	return total
}()

// ── leases ───────────────────────────────────────────────────────────────────

// Lease is one broadcast's claim on the box. It is taken at the door, before
// any video is accepted, and released when the broadcast ends.
type Lease struct {
	StreamID string
	// Provisional marks a lease taken at authorisation time, before the source
	// geometry was known, and therefore costed at nominalLadderCPU rather than
	// at what this broadcast actually needs.
	Provisional   bool
	Encoder       string
	CPUCores      float64
	NVENCSessions int
	Plan          EncodePlan
	// SourceHeight and FPS are the geometry the plan was costed against, kept
	// so a lease recosted after a shed rung is recosted against the same
	// source rather than against the rungs alone.
	SourceHeight int
	FPS          int
	AdmittedAt   time.Time
	// Pressure counts how many times this ladder has reported that it could not
	// keep up. Each report sheds a rung, so this is also how far it has been
	// walked down from the ladder it was planned.
	Pressure int
}

// Governor owns the box's live capacity. There is one, it lives in the
// application server, and it is the only thing that says yes.
type Governor struct {
	mu sync.Mutex

	cpuBudget     float64
	nvencBudget   int
	nvencEnabled  bool
	maxLadders    int
	segmentTarget int
	minSegment    int
	maxSegment    int

	leases map[string]*Lease
}

// NewGovernor reads the box's limits once at startup and reports them, so the
// number the platform is operating to is in the log rather than inferred from
// a failure.
func NewGovernor() *Governor {
	cores := float64(runtime.NumCPU())
	budget := envFloat("LIVE_CPU_BUDGET_CORES", math.Floor(cores*(1-cpuReserveFraction)*100)/100)
	if budget < minRungCPU {
		budget = minRungCPU
	}

	nvencEnabled := envBool("LIVE_NVENC_ENABLED", false)
	nvencBudget := envInt("LIVE_NVENC_SESSIONS", nvencSessions)
	if !nvencEnabled {
		nvencBudget = 0
	}

	// The hard ceiling is a backstop, not the working limit: the cost model is
	// what normally refuses a publish. It exists so that a mistake in the cost
	// model — a source geometry nobody anticipated, a coefficient that drifts
	// as the encoder is upgraded — cannot turn into an unbounded number of
	// ffmpeg processes. Default is what the CPU budget can carry at nominal
	// cost, which is the same answer by a different route.
	maxLadders := envInt("LIVE_MAX_CONCURRENT_LADDERS", int(math.Floor(budget/nominalLadderCPU))+nvencBudget)
	if maxLadders < 1 {
		maxLadders = 1
	}

	g := &Governor{
		cpuBudget:     budget,
		nvencBudget:   nvencBudget,
		nvencEnabled:  nvencEnabled,
		maxLadders:    maxLadders,
		segmentTarget: envInt("LIVE_SEGMENT_SECONDS", 1),
		minSegment:    envInt("LIVE_MIN_SEGMENT_SECONDS", 1),
		maxSegment:    envInt("LIVE_MAX_SEGMENT_SECONDS", 4),
		leases:        make(map[string]*Lease),
	}
	registerMetrics()
	log.Printf("[live-capacity] budget: %.2f of %d cores, nvenc %s (%d sessions), "+
		"hard cap %d ladders, nominal ladder %.2f cores",
		budget, runtime.NumCPU(), map[bool]string{true: "on", false: "off"}[nvencEnabled],
		nvencBudget, maxLadders, nominalLadderCPU)
	return g
}

// ── admission ────────────────────────────────────────────────────────────────

// AdmissionResult is what the door decided and why. The reason is not for a
// log line: it is the truth a broadcaster is owed when their publish is
// refused, and it is recorded on the row and rendered.
type AdmissionResult struct {
	Admitted bool
	Reason   string
}

// Reserve takes a provisional lease for a publish that has authenticated but
// has not yet delivered a frame. It is the only admission decision that can
// still be made for free, and it is therefore the one that matters: past this
// point the source is inside the media server and something has to encode it.
//
// A publish that is already holding a lease — a reconnecting encoder, a
// duplicate authorisation for the same stream — is admitted against the lease
// it already has rather than charged twice.
func (g *Governor) Reserve(streamID string) AdmissionResult {
	g.mu.Lock()
	defer g.mu.Unlock()

	if l, ok := g.leases[streamID]; ok {
		l.AdmittedAt = time.Now()
		return AdmissionResult{Admitted: true, Reason: "already admitted"}
	}

	if len(g.leases) >= g.maxLadders {
		return AdmissionResult{Admitted: false, Reason: fmt.Sprintf(
			"the platform is carrying its full %d concurrent broadcasts", g.maxLadders)}
	}

	usedCPU, usedNVENC := g.usedLocked()

	// The GPU path first: it is the cheaper of the two in cores by an order of
	// magnitude, so a broadcast that can be given sessions costs the CPU budget
	// almost nothing and leaves the software lane free for the next one.
	if g.nvencEnabled {
		// Costed at the sessions a nominal 1080p ladder needs: one per rung.
		const nominalSessions = 3
		nominalGPUCPU := broadcastCPUCost(1080, 30)
		for _, h := range []int{480, 720, 1080} {
			nominalGPUCPU += rungCPUCost(h, 30, EncoderNVENC)
		}
		if usedNVENC+nominalSessions <= g.nvencBudget && usedCPU+nominalGPUCPU <= g.cpuBudget {
			g.leases[streamID] = &Lease{
				StreamID:      streamID,
				Provisional:   true,
				Encoder:       EncoderNVENC,
				CPUCores:      nominalGPUCPU,
				NVENCSessions: nominalSessions,
				AdmittedAt:    time.Now(),
			}
			return AdmissionResult{Admitted: true, Reason: "hardware encoder"}
		}
	}

	// The software lane. A broadcast is admitted here when the box can still
	// afford the smallest ladder worth serving; how many rungs it actually gets
	// is decided when its geometry is known, against whatever is left then.
	if usedCPU+minRungCPU > g.cpuBudget {
		return AdmissionResult{Admitted: false, Reason: fmt.Sprintf(
			"the platform's encoders are full — %.1f of %.1f cores are carrying %d broadcasts",
			usedCPU, g.cpuBudget, len(g.leases))}
	}

	g.leases[streamID] = &Lease{
		StreamID:    streamID,
		Provisional: true,
		Encoder:     EncoderX264,
		// Held at the nominal cost, clamped to what is actually left, so a
		// second publish arriving in the same instant cannot be admitted
		// against capacity this one has already been promised.
		CPUCores:   math.Min(nominalLadderCPU, g.cpuBudget-usedCPU),
		AdmittedAt: time.Now(),
	}
	return AdmissionResult{Admitted: true, Reason: "software encoder"}
}

// Release gives a broadcast's capacity back. It is called on every path that
// ends a broadcast — the not-ready hook, a moderation block, reconciliation of
// a row whose publisher vanished — because capacity that is not released is
// capacity the next broadcaster is refused for no reason.
func (g *Governor) Release(streamID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.leases, streamID)
}

// usedLocked totals what the outstanding leases have claimed.
func (g *Governor) usedLocked() (cpu float64, nvenc int) {
	for _, l := range g.leases {
		cpu += l.CPUCores
		nvenc += l.NVENCSessions
	}
	return cpu, nvenc
}

// ── planning ─────────────────────────────────────────────────────────────────

// SourceReport is what the ladder measured about the source before encoding it.
// It is measurement only: nothing in it is a request, and nothing in it decides
// anything. The decisions are all taken here.
type SourceReport struct {
	Width  int
	Height int
	FPS    int
	// Codec is the source's video codec as the demuxer names it. It is
	// recorded, not acted on: every source is decoded and encoded.
	Codec string
	// NVENCAvailable is the ladder's answer to "can you actually open a
	// hardware encode session", established by opening one, not by reading a
	// list of encoders its ffmpeg was compiled with.
	//
	// This is reported rather than configured because a configuration flag
	// about hardware is a claim, and a claim can be wrong in the direction that
	// costs a broadcast: a governor that plans NVENC for a container with no
	// GPU produces a plan whose every rung fails to open, which is a broadcast
	// that never packages a frame. The container that would have to run the
	// encode is the only thing that can answer this, so it is the thing asked.
	NVENCAvailable bool
}

// Plan converts a provisional lease into the real one and returns the ladder
// this broadcast will actually get.
//
// This is where degradation happens, and it only ever degrades the broadcast
// being planned. The leases already held are read, never reduced: a broadcast
// that is running keeps the ladder it was planned, and a new one takes whatever
// is left over — including, at the limit, a single rung. Three rungs for two
// broadcasts and one rung for a third is a worse ladder for one viewer. Four
// full ladders on a box that can carry three is broken video for everybody.
func (g *Governor) Plan(streamID string, src SourceReport) (EncodePlan, error) {
	rungs := LadderFor(src.Height)
	if len(rungs) == 0 {
		return EncodePlan{}, fmt.Errorf("live: source height %d is below the %dp floor",
			src.Height, minLadderHeight)
	}
	fps := src.FPS
	if fps <= 0 {
		fps = 30
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	g.observeEncoderCapabilityLocked(src.NVENCAvailable, streamID)

	lease, held := g.leases[streamID]
	if !held {
		// The ladder is reporting for a stream that holds no lease. That is a
		// restart after the lease was released, or a source the door never saw.
		// It is admitted here against what is free rather than refused — the
		// video is already arriving, and refusing at this point would leave the
		// media server carrying a source with nothing encoding it — but it is
		// costed like everything else, so it can only ever get what is left.
		lease = &Lease{StreamID: streamID, Provisional: true, AdmittedAt: time.Now()}
		g.leases[streamID] = lease
	}

	// What the rest of the box has claimed, this broadcast's own provisional
	// reservation excluded: it is about to be replaced by the real one.
	usedCPU, usedNVENC := g.usedLocked()
	freeCPU := g.cpuBudget - (usedCPU - lease.CPUCores)
	freeNVENC := g.nvencBudget - (usedNVENC - lease.NVENCSessions)

	planned := plannedFrom(rungs)
	segment := g.segmentTarget
	degraded := false
	reason := ""

	// ── a reduction already taken stays taken ────────────────────────────────
	// ReportPressure records a shed rung on the lease and asks the ladder to
	// restart. When it comes back through here it must get the reduced ladder,
	// not the table again: replanning from the full table against a budget the
	// shed rung no longer counts in is exactly how a ladder restarted into the
	// same overload once a minute, leaving a hole in the broadcast each time.
	if held && lease.Plan.Degraded && len(lease.Plan.Rungs) > 0 {
		keep := make(map[int]bool, len(lease.Plan.Rungs))
		for _, r := range lease.Plan.Rungs {
			keep[r.Height] = true
		}
		reduced := planned[:0]
		for _, r := range planned {
			if keep[r.Height] {
				reduced = append(reduced, r)
			}
		}
		if len(reduced) > 0 {
			planned = reduced
			degraded = true
			reason = lease.Plan.Reason
			if reason == "" {
				reason = "reduced after backpressure"
			}
		}
	}

	// ── encoder selection ────────────────────────────────────────────────────
	encoder := EncoderX264
	needSessions := len(planned)
	// Three things all have to hold. The platform must believe it has a card;
	// there must be sessions left on it; and THIS ladder must have proved it
	// can open one. The last is not redundant with the first: a ladder whose
	// own probe failed gets the software encoder even while the card is happily
	// carrying other broadcasts, because a plan whose every rung fails to open
	// is a broadcast that never packages a frame.
	if g.nvencEnabled && src.NVENCAvailable && needSessions > 0 && needSessions <= freeNVENC {
		encoder = EncoderNVENC
	}

	// ── fit the plan to what is left ─────────────────────────────────────────
	// Rungs are shed from the top, because the top rung is both the most
	// expensive and the one fewest viewers can actually receive. The bottom
	// rung is never shed: it is the one a viewer on a phone connection has.
	cost := func(rs []PlannedRung) float64 {
		return planCPUCost(src.Height, fps, rs, encoder)
	}
	sessions := func(rs []PlannedRung) int {
		if encoder != EncoderNVENC {
			return 0
		}
		return len(rs)
	}

	// The top rung is shed first, and never the lowest: it is the one a viewer
	// on a phone connection actually receives, and a ladder that has shed its
	// way down to 1080p alone has not degraded gracefully, it has stopped
	// serving the people degradation exists for.
	for len(planned) > 1 && (cost(planned) > freeCPU || sessions(planned) > freeNVENC) {
		dropped := planned[len(planned)-1]
		planned = planned[:len(planned)-1]
		degraded = true
		reason = fmt.Sprintf("the platform is at capacity; %s was not encoded", dropped.Name)
	}
	// A single rung that still does not fit is served anyway. The lease was
	// granted at the door on the promise that one rung was affordable, and
	// breaking that promise here would mean a source inside the media server
	// with nothing encoding it — the exact silent failure this replaces.
	if cost(planned) > freeCPU {
		degraded = true
		reason = "the platform is beyond capacity; this broadcast is being served at its lowest rung only"
	}

	// ── isolation ────────────────────────────────────────────────────────────
	// A thread ceiling per rung is the one isolation primitive that is both
	// enforceable here and correct. The encodes all live in one ffmpeg process
	// inside one container, so there is no cgroup boundary between them to
	// draw; what there is, is the number of threads each encoder is permitted
	// to occupy. Left to itself libx264 sizes its thread pool from the whole
	// machine, so a single 1080p rung will happily open a dozen threads and
	// take cores that another broadcast was admitted on the promise of. Capped
	// at what the cost model says the rung needs, plus one, it cannot.
	//
	// The cap is on parallelism, not on work: an encode held to three threads
	// does the same encode, it just cannot sprawl into somebody else's share.
	// Sliced threading is what makes the cap safe at these sizes — frame-level
	// threading buys throughput by holding frames back, which is latency this
	// lane cannot spend, and measured worse here besides.
	for i := range planned {
		need := rungCPUCost(planned[i].Height, fps, encoder)
		t := int(math.Ceil(need)) + 1
		if t < 1 {
			t = 1
		}
		if t > 4 {
			t = 4
		}
		planned[i].Threads = t
	}

	plan := EncodePlan{
		Rungs:          planned,
		Encoder:        encoder,
		SegmentSeconds: segment,
		Degraded:       degraded,
		Reason:         reason,
	}

	lease.Provisional = false
	lease.Encoder = encoder
	lease.CPUCores = cost(planned)
	lease.NVENCSessions = sessions(planned)
	lease.Plan = plan
	lease.SourceHeight = src.Height
	lease.FPS = fps
	return plan, nil
}

// observeEncoderCapabilityLocked reconciles what the box was configured to
// believe about hardware encoding with what a ladder has just proved.
//
// Ground truth wins in both directions, and it only has to be learned once: a
// box either has a usable NVENC or it does not, and that does not change
// between broadcasts. Until the first ladder reports, the environment's hint is
// used for admission, and being wrong about it costs at most one broadcast's
// worth of over- or under-reservation — Plan re-costs every lease against the
// encoder it actually assigns, so the error does not persist.
func (g *Governor) observeEncoderCapabilityLocked(available bool, reportingStream string) {
	if available == g.nvencEnabled {
		return
	}
	if available {
		g.nvencEnabled = true
		g.nvencBudget = envInt("LIVE_NVENC_SESSIONS", nvencSessions)
		log.Printf("[live-capacity] a ladder opened a hardware encode session: "+
			"NVENC enabled, %d sessions", g.nvencBudget)
		return
	}
	// One ladder failing to open a session is not proof the box has no card,
	// and it must not be allowed to become that proof while other ladders are
	// encoding on it right now. A probe can fail for reasons that have nothing
	// to do with the hardware — several ladders starting in the same second and
	// contending, a driver still coming up with the container — and disabling
	// the GPU platform-wide on one such report is a transient fault latched
	// into permanent degradation, which is the failure mode this whole lane
	// exists to end.
	//
	// So the report is believed only when nothing is currently contradicting
	// it. If any lease is holding NVENC sessions, that card is demonstrably
	// working and this ladder's failure is its own.
	for id, l := range g.leases {
		// A provisional lease is a reservation, not a running encode: it was
		// costed at the GPU path on the assumption this box has one, and that
		// assumption is precisely what is in question. Only a ladder that was
		// planned onto NVENC and is actually running is evidence. Nor does the
		// reporting stream count as evidence about itself.
		if id != reportingStream && !l.Provisional && l.NVENCSessions > 0 {
			log.Printf("[live-capacity] a ladder could not open a hardware encode " +
				"session, but others are holding them — treating it as transient")
			return
		}
	}
	g.nvencEnabled = false
	g.nvencBudget = 0
	// Sessions already claimed by leases are released with those leases; the
	// budget going to zero only stops new ones being handed out. A later ladder
	// that does open a session turns this back on.
	log.Printf("[live-capacity] no ladder could open a hardware encode session: " +
		"NVENC disabled, every ladder is on the software encoder")
}

// ── backpressure ─────────────────────────────────────────────────────────────

// PressureVerdict is what the server tells a ladder that reported it cannot
// keep up.
type PressureVerdict struct {
	// Restart asks the ladder to exit so the media server starts it again. It
	// comes back, asks for a plan, and gets the reduced one recorded here. That
	// costs the broadcast a few seconds; the alternative it replaces is the
	// encoder grinding until the media server drops it as a slow reader and the
	// broadcast ends while the row still says live.
	Restart bool   `json:"restart"`
	Reason  string `json:"reason"`
}

// ReportPressure records that a ladder is falling behind realtime and decides
// what to do about it.
//
// The media server's "reader is too slow, discarding N frames" is the first
// symptom of this and today it is the only one anybody sees, in a log, seconds
// before the stream dies. An encoder that cannot hold realtime is not a
// warning: it is a broadcast that is already failing, and the only recoveries
// are to give it less to do or to admit it cannot be served.
func (g *Governor) ReportPressure(streamID string, speed float64) PressureVerdict {
	g.mu.Lock()
	defer g.mu.Unlock()

	ladderSpeed.Observe(speed)

	lease, ok := g.leases[streamID]
	if !ok {
		return PressureVerdict{Restart: false, Reason: "no lease held"}
	}
	lease.Pressure++

	if len(lease.Plan.Rungs) <= 1 {
		// Nothing left to shed. Restarting would only interrupt a broadcast
		// that is already as small as this platform can make it, so it is left
		// running and reported as degraded. This is the honest end of the
		// scale: the box cannot serve this source and says so.
		return PressureVerdict{Restart: false, Reason: fmt.Sprintf(
			"encoding at %.2fx realtime on a single rung — nothing further can be shed", speed)}
	}

	// Shed the top rung by writing the reduction into the lease; the ladder
	// restarts, asks for a plan, and Plan() draws it against a budget that no
	// longer includes what the shed rung was costing. The lease is recosted by
	// the same sum Plan used, per-broadcast cost included: the rungs alone
	// leave out the largest item in the ladder and under-reserve the box by
	// over a core and a half for every broadcast that has ever shed.
	dropped := lease.Plan.Rungs[len(lease.Plan.Rungs)-1]
	lease.Plan.Rungs = lease.Plan.Rungs[:len(lease.Plan.Rungs)-1]
	lease.Plan.Degraded = true
	lease.CPUCores = planCPUCost(lease.SourceHeight, lease.FPS, lease.Plan.Rungs, lease.Encoder)
	if lease.Encoder == EncoderNVENC {
		lease.NVENCSessions = len(lease.Plan.Rungs)
	}
	return PressureVerdict{Restart: true, Reason: fmt.Sprintf(
		"encoding at %.2fx realtime — dropping the %s rung", speed, dropped.Name)}
}

// ── observation ──────────────────────────────────────────────────────────────

// Snapshot is the governor's state as one readable value, for metrics and for
// anything that needs to say how close the box is to its ceiling.
type Snapshot struct {
	CPUBudget     float64
	CPUUsed       float64
	NVENCBudget   int
	NVENCUsed     int
	MaxLadders    int
	Ladders       int
	LaddersByEnc  map[string]int
	DegradedCount int
}

// Headroom is the fraction of the live lane still free, taken as the worse of
// the two budgets, because a broadcast needs room in whichever one its encoder
// spends and having room in the other buys it nothing. Zero means the next
// publish is refused.
func (s Snapshot) Headroom() float64 {
	cpu := 1.0
	if s.CPUBudget > 0 {
		cpu = (s.CPUBudget - s.CPUUsed) / s.CPUBudget
	}
	worst := cpu
	if s.NVENCBudget > 0 {
		nvenc := float64(s.NVENCBudget-s.NVENCUsed) / float64(s.NVENCBudget)
		if nvenc > worst {
			// The GPU having room is what saves a box whose cores are spoken
			// for, so the better of the two is the headroom that matters when
			// both encoders are available.
			worst = nvenc
		}
	}
	if s.MaxLadders > 0 {
		byCount := float64(s.MaxLadders-s.Ladders) / float64(s.MaxLadders)
		if byCount < worst {
			worst = byCount
		}
	}
	if worst < 0 {
		return 0
	}
	if worst > 1 {
		return 1
	}
	return worst
}

// PlanSummary is the ladder currently planned for a stream, as one line. It is
// read by the paths that record the truth on the row after the plan changes
// under pressure.
func (g *Governor) PlanSummary(streamID string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if l, ok := g.leases[streamID]; ok {
		return l.Plan.Summary()
	}
	return ""
}

// EncoderFor is the encoder currently carrying a stream.
func (g *Governor) EncoderFor(streamID string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if l, ok := g.leases[streamID]; ok {
		return l.Encoder
	}
	return ""
}

// Snapshot reads the governor without changing it.
func (g *Governor) Snapshot() Snapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	cpu, nvenc := g.usedLocked()
	s := Snapshot{
		CPUBudget:    g.cpuBudget,
		CPUUsed:      cpu,
		NVENCBudget:  g.nvencBudget,
		NVENCUsed:    nvenc,
		MaxLadders:   g.maxLadders,
		Ladders:      len(g.leases),
		LaddersByEnc: map[string]int{EncoderX264: 0, EncoderNVENC: 0},
	}
	for _, l := range g.leases {
		if l.Encoder != "" {
			s.LaddersByEnc[l.Encoder]++
		}
		if l.Plan.Degraded {
			s.DegradedCount++
		}
	}
	return s
}

// ── environment ──────────────────────────────────────────────────────────────

func envFloat(key string, fallback float64) float64 {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return f
		}
		log.Printf("[live-capacity] %s=%q is not a positive number; using %.2f", key, v, fallback)
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
		log.Printf("[live-capacity] %s=%q is not a whole number; using %d", key, v, fallback)
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "":
		return fallback
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		log.Printf("[live-capacity] %s is not a boolean; using %v", key, fallback)
		return fallback
	}
}
