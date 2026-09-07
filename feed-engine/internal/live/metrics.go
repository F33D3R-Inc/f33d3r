package live

// metrics.go — what the live lane looks like from outside.
//
// The capacity failure this lane had was not hard to diagnose once someone
// looked; it was hard to notice. The only evidence a broadcast had been
// destroyed was an ffmpeg line in a container log, and the only evidence the
// box was oversubscribed was that it had already happened. Nothing said how
// many ladders were running, what they were costing, or how close the box was
// to the point where the next publish breaks the ones before it.
//
// These are the numbers to operate this lane on. The one to alert on is
// headroom: it is the only one that predicts rather than reports.
//
// They register on the application's own registry — the private one in
// internal/observ that carries only F33D3R metrics — so they are scraped by the
// Prometheus that is already running against feed-engine, with no new target,
// no new exporter and no new service.

import (
	"context"
	"log"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/f33d3r/feed-engine/internal/observ"
)

var (
	// laddersActive is the headline number: how many broadcasts this box is
	// encoding right now, split by the encoder carrying them. A GPU ladder and
	// a CPU ladder cost entirely different resources and run out at entirely
	// different points, so summing them would hide the thing worth watching.
	laddersActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "live_ladders_active",
			Help: "Concurrent ABR ladders being produced, by encoder.",
		},
		[]string{"encoder"},
	)

	// laddersDegraded counts broadcasts being served less than the full ladder
	// their source could support. A non-zero value here is the platform working
	// as designed under load, and a persistently non-zero one is the platform
	// asking for more capacity.
	laddersDegraded = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "live_ladders_degraded",
			Help: "Concurrent ladders reduced below the full ladder for their source because the platform is at capacity.",
		},
	)

	// The CPU budget and what is claimed against it. Budget is exported as well
	// as usage because a ratio computed against a constant somebody typed into
	// a dashboard goes wrong silently the day the constant changes.
	capacityCPUBudget = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "live_capacity_cpu_cores_budget",
			Help: "Cores the live lane is permitted to spend on encoding.",
		},
	)
	capacityCPUUsed = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "live_capacity_cpu_cores_used",
			Help: "Cores claimed by the ladders currently running, as costed by the capacity model.",
		},
	)

	// NVENC is a hard ceiling rather than a soft one: the card grants a fixed
	// number of sessions and the next one fails outright rather than running
	// slowly. It is exported separately for exactly that reason.
	capacityNVENCBudget = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "live_capacity_nvenc_sessions_budget",
			Help: "Concurrent NVENC encode sessions this host's GPU will open.",
		},
	)
	capacityNVENCUsed = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "live_capacity_nvenc_sessions_used",
			Help: "NVENC encode sessions claimed by the ladders currently running.",
		},
	)

	// capacityHeadroom is the fraction of the live lane still available, taken
	// as the worse of the two budgets. This is the number to alert on: it falls
	// before anything breaks, where every other metric here only moves once
	// something already has.
	capacityHeadroom = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "live_capacity_headroom_ratio",
			Help: "Fraction of live encoding capacity still free, across whichever budget is tightest. Zero means the next publish is refused.",
		},
	)

	// admissionsTotal is the door's record. "refused" rising is the platform
	// protecting broadcasts that are already running; "degraded" rising is it
	// serving a smaller ladder rather than refusing. Both are correct
	// behaviour, and both mean the box needs more capacity than it has.
	admissionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "live_admissions_total",
			Help: "Publish admission decisions, by outcome.",
		},
		[]string{"outcome"},
	)

	// pressureEventsTotal counts ladders reporting they could not hold
	// realtime. Before this existed the same condition was observable only as
	// the media server discarding frames, seconds before the broadcast died.
	pressureEventsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "live_encoder_pressure_events_total",
			Help: "Reports from a ladder that it could not sustain realtime encoding.",
		},
	)

	// ladderSpeed is the distribution of realtime ratios reported under
	// pressure. The bucket boundaries sit around 1.0 because that is the only
	// interesting value: at 1.0 an encoder is exactly keeping up and below it
	// the gap to the source widens without bound.
	ladderSpeed = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "live_ladder_realtime_ratio",
			Help:    "Encoded media seconds per wall second, sampled when a ladder reports backpressure.",
			Buckets: []float64{0.5, 0.7, 0.8, 0.9, 0.94, 0.97, 1.0, 1.05, 1.2},
		},
	)
)

// registerMetrics puts the live lane's collectors on the application registry.
// It is called once, from the governor's construction, so the series exist from
// the moment the process is scrapeable rather than appearing on first use — a
// gauge that is absent and a gauge that is zero mean different things to an
// alert, and "no ladders running" must read as zero.
func registerMetrics() {
	cs := []prometheus.Collector{
		laddersActive, laddersDegraded,
		capacityCPUBudget, capacityCPUUsed,
		capacityNVENCBudget, capacityNVENCUsed,
		capacityHeadroom,
		admissionsTotal, pressureEventsTotal, ladderSpeed,
	}
	for _, c := range cs {
		if err := observ.Registry().Register(c); err != nil {
			// Already registered is the only way this fails in practice, and it
			// means a second Manager was built in one process. Report it rather
			// than panicking a live server over an observability collector.
			if _, dup := err.(prometheus.AlreadyRegisteredError); !dup {
				log.Printf("[live-capacity] registering metric: %v", err)
			}
		}
	}
	// The counters are given their zero values so a dashboard reads "none yet"
	// rather than "no data".
	for _, outcome := range []string{"admitted", "refused", "degraded"} {
		admissionsTotal.WithLabelValues(outcome)
	}
	laddersActive.WithLabelValues(EncoderX264)
	laddersActive.WithLabelValues(EncoderNVENC)
}

// capacityExportInterval is how often the governor's state is published. The
// gauges describe a population that changes only when a broadcast starts or
// ends, so this is a sampling rate, not a polling loop with work in it.
const capacityExportInterval = 10 * time.Second

// capacityExportLoop publishes the governor's state until the context ends.
func (m *Manager) capacityExportLoop(ctx context.Context) {
	ticker := time.NewTicker(capacityExportInterval)
	defer ticker.Stop()
	m.exportCapacity()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.exportCapacity()
		}
	}
}

func (m *Manager) exportCapacity() {
	if m.capacity == nil {
		return
	}
	s := m.capacity.Snapshot()
	for encoder, n := range s.LaddersByEnc {
		laddersActive.WithLabelValues(encoder).Set(float64(n))
	}
	laddersDegraded.Set(float64(s.DegradedCount))
	capacityCPUBudget.Set(s.CPUBudget)
	capacityCPUUsed.Set(s.CPUUsed)
	capacityNVENCBudget.Set(float64(s.NVENCBudget))
	capacityNVENCUsed.Set(float64(s.NVENCUsed))
	capacityHeadroom.Set(s.Headroom())
}
