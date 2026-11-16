package live

// capacity_test.go — the governor's invariants.
//
// One of these matters more than the rest and it is TestExistingBroadcastsWin.
// The failure this whole lane was built to end is not "the box got slow": it is
// that a fourth broadcast starting would take cores off three that were already
// running until every one of them produced corrupted video. Any change here
// that lets a new lease reduce an existing one has reintroduced that, and this
// file is where that gets caught.

import (
	"math"
	"testing"
)

// testGovernor builds a governor with an explicit budget rather than one read
// from whatever host the tests happen to run on. A test whose expectations move
// with runtime.NumCPU() proves nothing on either machine.
func testGovernor(cpuBudget float64, nvencSessions int) *Governor {
	return &Governor{
		cpuBudget:     cpuBudget,
		nvencBudget:   nvencSessions,
		nvencEnabled:  nvencSessions > 0,
		maxLadders:    64, // out of the way; these tests exercise the cost model
		segmentTarget: 1,
		minSegment:    1,
		maxSegment:    4,
		leases:        make(map[string]*Lease),
	}
}

func h264Source(height int) SourceReport {
	return SourceReport{
		Width:  height * 16 / 9,
		Height: height,
		FPS:    30,
		Codec:  "h264",
	}
}

// TestExistingBroadcastsWin is the invariant. Broadcasts are admitted until the
// box is full; the ones already running keep exactly the ladder they were
// planned; and the one that does not fit is refused rather than served at the
// expense of the others.
func TestExistingBroadcastsWin(t *testing.T) {
	g := testGovernor(4.0, 0)

	type planned struct {
		rungs   int
		encoder string
	}
	admitted := map[string]planned{}

	for i := 0; i < 20; i++ {
		id := "stream-" + string(rune('a'+i))
		if !g.Reserve(id).Admitted {
			break
		}
		plan, err := g.Plan(id, h264Source(1080))
		if err != nil {
			t.Fatalf("plan %s: %v", id, err)
		}
		if len(plan.Rungs) == 0 {
			t.Fatalf("%s was admitted and then given no ladder at all", id)
		}
		admitted[id] = planned{rungs: len(plan.Rungs), encoder: plan.Encoder}
	}

	if len(admitted) == 0 {
		t.Fatal("no broadcast was admitted to a box with a 4 core budget")
	}

	// Every plan already issued must still be exactly what it was. Nothing that
	// happened after it was issued may have taken a rung off it.
	for id, want := range admitted {
		got := g.leases[id]
		if got == nil {
			t.Fatalf("%s lost its lease while it was still running", id)
		}
		if len(got.Plan.Rungs) != want.rungs {
			t.Errorf("%s was planned %d rungs and now holds %d: a later broadcast "+
				"reduced one that was already running",
				id, want.rungs, len(got.Plan.Rungs))
		}
		if got.Encoder != want.encoder {
			t.Errorf("%s was planned on %s and now holds %s", id, want.encoder, got.Encoder)
		}
	}

	// And the box must genuinely be full: the next one is refused, not squeezed in.
	if res := g.Reserve("one-too-many"); res.Admitted {
		t.Errorf("a broadcast was admitted past the budget: %+v", res)
	} else if res.Reason == "" {
		t.Error("a refusal carried no reason; the broadcaster is told nothing")
	}
}

// TestDegradationBeforeRefusal proves the middle of the scale exists: a
// broadcast that cannot have the full ladder is given a smaller one rather than
// being turned away, and is told so.
func TestDegradationBeforeRefusal(t *testing.T) {
	// Wide enough for one full 1080p30 software ladder and not two, which is
	// the window in which degradation rather than refusal is the right answer.
	g := testGovernor(4.0, 0)

	if !g.Reserve("first").Admitted {
		t.Fatal("the first broadcast was refused an empty box")
	}
	first, err := g.Plan("first", h264Source(1080))
	if err != nil {
		t.Fatalf("plan first: %v", err)
	}
	if first.Degraded {
		t.Fatalf("the only broadcast on the box was degraded: %s", first.Reason)
	}
	if len(first.Rungs) != 3 {
		t.Fatalf("a 1080p source on an empty box got %d rungs, want the full 3", len(first.Rungs))
	}

	if !g.Reserve("second").Admitted {
		t.Skip("this budget refuses the second outright; the degradation path needs a wider one")
	}
	second, err := g.Plan("second", h264Source(1080))
	if err != nil {
		t.Fatalf("plan second: %v", err)
	}
	if !second.Degraded {
		t.Errorf("the second broadcast fit the full ladder into a budget that "+
			"cannot carry two of them: %s", second.Summary())
	}
	if len(second.Rungs) < 1 {
		t.Error("a degraded broadcast was left with no rungs at all")
	}
	if second.Reason == "" {
		t.Error("a degraded broadcast carries no reason; the row cannot tell the truth")
	}
	// Degrading the second must not have touched the first.
	if len(g.leases["first"].Plan.Rungs) != 3 {
		t.Errorf("planning the second broadcast reduced the first to %d rungs",
			len(g.leases["first"].Plan.Rungs))
	}
}

// TestLowestRungIsNeverShed — the bottom rung is the one a viewer on a phone
// connection receives. Shedding it to save cores serves nobody.
func TestLowestRungIsNeverShed(t *testing.T) {
	g := testGovernor(0.4, 0)
	g.Reserve("only")
	plan, err := g.Plan("only", h264Source(1080))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Rungs) != 1 {
		t.Fatalf("a starved box planned %d rungs, want exactly 1", len(plan.Rungs))
	}
	if plan.Rungs[0].Height != 480 {
		t.Errorf("the surviving rung is %dp, want the 480p floor", plan.Rungs[0].Height)
	}
	if !plan.Degraded {
		t.Error("a broadcast cut to one rung does not report itself degraded")
	}
}

// TestReleaseReturnsCapacity — a lease that is not released is a broadcaster
// refused for no reason.
func TestReleaseReturnsCapacity(t *testing.T) {
	g := testGovernor(4.0, 0)
	var ids []string
	for i := 0; i < 20; i++ {
		id := "s" + string(rune('a'+i))
		if !g.Reserve(id).Admitted {
			break
		}
		if _, err := g.Plan(id, h264Source(1080)); err != nil {
			t.Fatalf("plan %s: %v", id, err)
		}
		ids = append(ids, id)
	}
	if g.Reserve("blocked").Admitted {
		t.Fatal("the box was not actually full")
	}
	g.Release(ids[0])
	if !g.Reserve("after-release").Admitted {
		t.Error("capacity freed by an ended broadcast was not offered to the next one")
	}
}

// ── the watermark ────────────────────────────────────────────────────────────

// TestEveryRungIsEncoded — the moving watermark is burned into the pixels of
// every rung, and only an encode puts it there. A rung remuxed from the
// publisher's own stream would be an unmarked copy on the edge, so no source —
// H.264 at exactly a ladder height, a portrait phone, a source above the top
// rung — is ever planned anything but encodes.
func TestEveryRungIsEncoded(t *testing.T) {
	cases := []SourceReport{
		h264Source(1080),
		h264Source(720),
		{Width: 720, Height: 1280, FPS: 30, Codec: "h264"},
		{Width: 1920, Height: 1200, FPS: 30, Codec: "h264"},
		{Width: 1920, Height: 1080, FPS: 30, Codec: "vp8"},
	}
	for _, src := range cases {
		g := testGovernor(16.0, 0)
		g.Reserve("s")
		plan, err := g.Plan("s", src)
		if err != nil {
			t.Fatalf("plan %dx%d %s: %v", src.Width, src.Height, src.Codec, err)
		}
		for _, r := range plan.Rungs {
			if r.Mode != ModeEncode {
				t.Errorf("%dx%d %s: rung %s planned as %q; an unencoded rung carries "+
					"no watermark", src.Width, src.Height, src.Codec, r.Name, r.Mode)
			}
			if r.Height > src.Height {
				t.Errorf("%dx%d %s: rung %s upscales the source", src.Width, src.Height,
					src.Codec, r.Name)
			}
		}
		if plan.SegmentSeconds != g.segmentTarget {
			t.Errorf("segment duration moved to %ds; with every rung encoded it is "+
				"the governor's target", plan.SegmentSeconds)
		}
	}
}

// TestShedRecostsTheWholeBroadcast — a lease walked down under pressure is
// recosted by the same sum Plan uses. Costing the remaining rungs alone left
// out the per-broadcast cost, the largest item in a ladder, and reserved the
// box over a core and a half short for every broadcast that had ever shed.
func TestShedRecostsTheWholeBroadcast(t *testing.T) {
	g := testGovernor(8.0, 0)
	g.Reserve("s")
	if _, err := g.Plan("s", h264Source(1080)); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if v := g.ReportPressure("s", 0.7); !v.Restart {
		t.Fatalf("pressure did not shed a rung: %s", v.Reason)
	}
	l := g.leases["s"]
	want := planCPUCost(1080, 30, l.Plan.Rungs, l.Encoder)
	if math.Abs(l.CPUCores-want) > 1e-9 {
		t.Errorf("a shed lease holds %.3f cores; its ladder costs %.3f", l.CPUCores, want)
	}
	if l.CPUCores < broadcastCPUCost(1080, 30) {
		t.Errorf("a shed lease holds %.3f cores, less than the %.3f every 1080p30 "+
			"broadcast costs before a single rung", l.CPUCores, broadcastCPUCost(1080, 30))
	}
}

// TestShedRungStaysShedAcrossRestart — the pressure verdict tells the ladder
// to restart; when it comes back and asks for a plan it must receive the
// reduced ladder, not the full table again. Replanning from the table was how
// a ladder restarted into the same overload once a minute.
func TestShedRungStaysShedAcrossRestart(t *testing.T) {
	g := testGovernor(8.0, 0)
	g.Reserve("s")
	src := SourceReport{Width: 1920, Height: 1080, FPS: 30, Codec: "vp8"}
	first, err := g.Plan("s", src)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(first.Rungs) < 2 {
		t.Skipf("test governor affords only %d rung(s); nothing to shed", len(first.Rungs))
	}
	v := g.ReportPressure("s", 0.7)
	if !v.Restart {
		t.Fatalf("pressure with %d rungs should ask for a restart: %s", len(first.Rungs), v.Reason)
	}
	again, err := g.Plan("s", src)
	if err != nil {
		t.Fatalf("replan: %v", err)
	}
	if len(again.Rungs) != len(first.Rungs)-1 {
		t.Fatalf("replan after shedding has %d rungs, want %d: %s",
			len(again.Rungs), len(first.Rungs)-1, again.Summary())
	}
	if !again.Degraded {
		t.Errorf("a replan on a shed ladder must report itself degraded")
	}
	shed := first.Rungs[len(first.Rungs)-1].Name
	for _, r := range again.Rungs {
		if r.Name == shed {
			t.Fatalf("the shed rung %s came back on restart: %s", shed, again.Summary())
		}
	}
}

// ── hardware ─────────────────────────────────────────────────────────────────

// TestNVENCSessionsAreBudgetedNotProbed — the card grants a fixed number of
// sessions and the next one fails outright rather than running slowly, so a
// plan must never call for more than remain.
func TestNVENCSessionsAreBudgetedNotProbed(t *testing.T) {
	g := testGovernor(100, 8) // cores out of the way; sessions are the constraint
	sessions := 0
	for i := 0; i < 10; i++ {
		id := "s" + string(rune('a'+i))
		if !g.Reserve(id).Admitted {
			break
		}
		plan, err := g.Plan(id, SourceReport{Width: 1920, Height: 1080, FPS: 30,
			Codec: "vp8", NVENCAvailable: true})
		if err != nil {
			t.Fatalf("plan %s: %v", id, err)
		}
		if plan.Encoder == EncoderNVENC {
			sessions += len(plan.Rungs)
		}
	}
	if sessions > 8 {
		t.Errorf("plans called for %d concurrent NVENC sessions; the card opens 8 "+
			"and fails the ninth outright", sessions)
	}
	if snap := g.Snapshot(); snap.NVENCUsed > snap.NVENCBudget {
		t.Errorf("the governor believes %d sessions are claimed against a budget of %d",
			snap.NVENCUsed, snap.NVENCBudget)
	}
}

// TestEncoderCapabilityIsLearnedFromTheLadder — a configuration flag about
// hardware is a claim and can be wrong in the direction that costs a broadcast.
// The ladder proves the answer by opening a session, and that answer wins.
func TestEncoderCapabilityIsLearnedFromTheLadder(t *testing.T) {
	g := testGovernor(100, 8)
	g.Reserve("s")
	plan, err := g.Plan("s", SourceReport{Width: 1920, Height: 1080, FPS: 30,
		Codec: "vp8", NVENCAvailable: false})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Encoder != EncoderX264 {
		t.Errorf("a ladder that could not open a hardware session was planned on %s",
			plan.Encoder)
	}
	if g.nvencEnabled || g.nvencBudget != 0 {
		t.Error("the governor kept a hardware budget after a ladder proved there is none")
	}
	if snap := g.Snapshot(); snap.NVENCBudget != 0 {
		t.Errorf("metrics still advertise %d hardware sessions on a box with none",
			snap.NVENCBudget)
	}
}

// ── backpressure ─────────────────────────────────────────────────────────────

// TestPressureShedsRungsThenStops — an encoder that cannot hold realtime is
// given less to do, one rung at a time, and once there is nothing left to shed
// it is left alone rather than being restarted into the same wall forever.
func TestPressureShedsRungsThenStops(t *testing.T) {
	g := testGovernor(8.0, 0)
	g.Reserve("s")
	plan, err := g.Plan("s", h264Source(1080))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	start := len(plan.Rungs)
	if start < 2 {
		t.Fatalf("need a multi-rung plan to shed from, got %d", start)
	}

	for i := 0; i < start-1; i++ {
		v := g.ReportPressure("s", 0.7)
		if !v.Restart {
			t.Fatalf("pressure report %d did not shed a rung from a %d rung ladder", i+1, start)
		}
	}
	if got := len(g.leases["s"].Plan.Rungs); got != 1 {
		t.Fatalf("after shedding down, %d rungs remain, want 1", got)
	}
	if v := g.ReportPressure("s", 0.7); v.Restart {
		t.Error("a single-rung ladder was restarted; there is nothing left to shed " +
			"and restarting only interrupts a broadcast the box cannot serve anyway")
	}
	if !g.leases["s"].Plan.Degraded {
		t.Error("a ladder walked down under pressure is not marked degraded")
	}
}

// TestPressureOnAnUnknownStreamIsHarmless — a ladder can outlive its lease
// (a restart, a reconcile that closed the row) and must not panic the control
// plane when it reports.
func TestPressureOnAnUnknownStreamIsHarmless(t *testing.T) {
	g := testGovernor(8.0, 0)
	if v := g.ReportPressure("never-seen", 0.5); v.Restart {
		t.Error("a stream holding no lease was told to restart")
	}
}

// ── the cost model ───────────────────────────────────────────────────────────

// TestCostModelMatchesMeasurement pins the model to the numbers measured on
// this host. If a coefficient is edited without a fresh measurement behind it,
// this is what says so.
//
// The measured figure is the ladder's own ffmpeg invocation against a 1080p30
// H.264 source, the full three rung software ladder. The watermark's share is
// an estimate (markCoresPerMegapixel) and sits well inside the tolerance.
func TestCostModelMatchesMeasurement(t *testing.T) {
	const (
		// A real 1080p30 RTMP broadcast's ladder, read out of /proc at 3.674
		// cores sustained, less the 0.19 the poster-branch reordering saves.
		measuredFullLadderCores = 3.48
		// The box is shared and these were taken on a loaded one. The model is
		// a budget, not a stopwatch; this is wide enough to survive a noisy
		// measurement and narrow enough to catch a coefficient edited blind.
		tolerance = 0.35
	)

	full := broadcastCPUCost(1080, 30)
	for _, h := range []int{480, 720, 1080} {
		full += rungCPUCost(h, 30, EncoderX264)
	}
	if math.Abs(full-measuredFullLadderCores) > tolerance {
		t.Errorf("the model costs the full software ladder at %.2f cores; it was "+
			"measured at %.2f. Re-measure before changing a coefficient.",
			full, measuredFullLadderCores)
	}
	t.Logf("modelled: full software ladder %.2f cores", full)
}

// TestThreadCeilingsAreBounded — the isolation mechanism. An encode left to
// size its own thread pool takes it from the whole machine, which is how one
// broadcast reaches into another's share.
func TestThreadCeilingsAreBounded(t *testing.T) {
	g := testGovernor(8.0, 0)
	g.Reserve("s")
	plan, err := g.Plan("s", h264Source(1080))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	for _, r := range plan.Rungs {
		if r.Threads < 1 {
			t.Errorf("%s was given no thread ceiling; libx264 will size its pool "+
				"from the whole machine", r.Name)
		}
		if r.Threads > 4 {
			t.Errorf("%s may occupy %d threads, which is a third of this box for "+
				"one rung of one broadcast", r.Name, r.Threads)
		}
	}
}

// TestHeadroomReachesZeroBeforeRefusing — headroom is the number this lane is
// operated and alerted on, so it has to reach zero at the point admissions
// actually stop and not before or after.
func TestHeadroomReachesZeroBeforeRefusing(t *testing.T) {
	g := testGovernor(4.0, 0)
	for i := 0; i < 20; i++ {
		id := "s" + string(rune('a'+i))
		if !g.Reserve(id).Admitted {
			break
		}
		if _, err := g.Plan(id, h264Source(1080)); err != nil {
			t.Fatalf("plan %s: %v", id, err)
		}
	}
	h := g.Snapshot().Headroom()
	if h < 0 || h > 1 {
		t.Fatalf("headroom is %.3f, outside 0..1", h)
	}
	if h > 0.25 {
		t.Errorf("the box refuses new broadcasts while reporting %.0f%% headroom; "+
			"an operator watching this metric has no warning", h*100)
	}
}

// TestSummaryNamesRungsAndEncoder — the row records the summary, and the
// quality selector reads the rungs back out of it.
func TestSummaryNamesRungsAndEncoder(t *testing.T) {
	g := testGovernor(8.0, 0)
	g.Reserve("s")
	plan, _ := g.Plan("s", h264Source(1080))
	got := plan.Summary()
	if got == "" {
		t.Fatal("a plan rendered an empty summary; the row records nothing")
	}
	if want := "1080p@6500k"; !contains(got, want) {
		t.Errorf("summary %q does not name the 1080p rung and its bitrate", got)
	}
	if !contains(got, EncoderX264) {
		t.Errorf("summary %q does not name the encoder carrying the ladder", got)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) &&
		(haystack == needle || len(needle) == 0 ||
			indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// TestCapacityCeiling expresses the lane's concurrency against this host's
// real budget, so the number cannot drift away from the code that produces it.
//
// Every rung is encoded, because every rung carries the burned-in watermark.
// The software lane is bounded by cores; the hardware lane by the card's
// sessions, three per 1080p broadcast; and the real door admits the hardware
// broadcasts and then fills what cores are left with software ones, so it
// carries more than the software lane alone.
func TestCapacityCeiling(t *testing.T) {
	const budget = 7.8 // LIVE_CPU_BUDGET_CORES on this host: 12 cores less reserve

	full := func(encoder string) float64 {
		total := broadcastCPUCost(1080, 30)
		for _, h := range []int{480, 720, 1080} {
			total += rungCPUCost(h, 30, encoder)
		}
		return total
	}
	software := full(EncoderX264)
	hardware := full(EncoderNVENC)
	softwareCeiling := int(budget / software)
	hardwareCeiling := nvencSessions / 3
	if byCores := int(budget / hardware); byCores < hardwareCeiling {
		hardwareCeiling = byCores
	}
	t.Logf("per 1080p30 broadcast: software %.2f cores, hardware %.2f cores; "+
		"at a %.1f core budget: software %d, hardware %d",
		software, hardware, budget, softwareCeiling, hardwareCeiling)

	if softwareCeiling < 1 || hardwareCeiling < 1 {
		t.Fatalf("this host cannot carry one 1080p30 broadcast: software %d, hardware %d",
			softwareCeiling, hardwareCeiling)
	}

	g := testGovernor(budget, nvencSessions)
	admitted := 0
	for i := 0; i < 20; i++ {
		id := "s" + string(rune('a'+i))
		if !g.Reserve(id).Admitted {
			break
		}
		plan, err := g.Plan(id, SourceReport{Width: 1920, Height: 1080, FPS: 30,
			Codec: "h264", NVENCAvailable: true})
		if err != nil {
			t.Fatalf("plan %s: %v", id, err)
		}
		admitted++
		t.Logf("  broadcast %d: %s%s", admitted, plan.Summary(),
			map[bool]string{true: " DEGRADED", false: ""}[plan.Degraded])
	}
	if admitted < hardwareCeiling {
		t.Errorf("the governor admitted %d broadcasts where the card affords %d",
			admitted, hardwareCeiling)
	}
	if admitted <= softwareCeiling {
		t.Errorf("the governor admitted %d broadcasts with a card present; the "+
			"software lane alone carries %d", admitted, softwareCeiling)
	}
}

// TestOneFailedProbeDoesNotDisableAWorkingCard guards the rule that a single
// ladder failing to open a hardware session must not disable the GPU for the
// whole platform while other ladders are demonstrably encoding on it.
//
// This is here because it happened. The first version of the ladder cached a
// failed probe permanently, four ladders started in the same second, one probe
// lost the race, and the control plane put every broadcast on the software
// encoder for the life of the container with nothing reporting why. A transient
// fault latched into permanent degradation is the exact failure this lane was
// rebuilt to end, and it does not get to come back in through the capability
// check.
func TestOneFailedProbeDoesNotDisableAWorkingCard(t *testing.T) {
	g := testGovernor(100, 8)

	// Two broadcasts running on the card.
	for _, id := range []string{"running-a", "running-b"} {
		g.Reserve(id)
		plan, err := g.Plan(id, SourceReport{Width: 1920, Height: 1080, FPS: 30,
			Codec: "vp8", NVENCAvailable: true})
		if err != nil {
			t.Fatalf("plan %s: %v", id, err)
		}
		if plan.Encoder != EncoderNVENC {
			t.Fatalf("%s was not planned onto the card: %s", id, plan.Encoder)
		}
	}

	// A third ladder's probe loses a race and reports no hardware.
	g.Reserve("unlucky")
	plan, err := g.Plan("unlucky", SourceReport{Width: 1280, Height: 720, FPS: 30,
		Codec: "vp8", NVENCAvailable: false})
	if err != nil {
		t.Fatalf("plan unlucky: %v", err)
	}
	if plan.Encoder != EncoderX264 {
		t.Errorf("the ladder that could not open a session was planned onto %s anyway",
			plan.Encoder)
	}
	if !g.nvencEnabled {
		t.Error("one failed probe disabled a card that two other ladders are encoding on")
	}

	// With nothing left on the card, the same report is believed.
	g.Release("running-a")
	g.Release("running-b")
	g.Reserve("last")
	if _, err := g.Plan("last", SourceReport{Width: 1280, Height: 720, FPS: 30,
		Codec: "vp8", NVENCAvailable: false}); err != nil {
		t.Fatalf("plan last: %v", err)
	}
	if g.nvencEnabled {
		t.Error("a report of no hardware was ignored even with nothing contradicting it")
	}
}
