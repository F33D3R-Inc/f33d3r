package observ

import "github.com/prometheus/client_golang/prometheus"

// Outbound HTTP metrics — separate from inbound so dashboards can compare
// "I served X requests" vs "I sent Y requests downstream" per brain.
var (
	outboundRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_outbound_requests_total",
			Help: "HTTP requests this brain sent to other brains, partitioned by destination host and result.",
		},
		[]string{"brain", "dest", "status"},
	)

	outboundDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_outbound_duration_seconds",
			Help:    "Outbound HTTP latency in seconds.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		},
		[]string{"brain", "dest"},
	)
)

func init() {
	registry.MustRegister(outboundRequests, outboundDuration)
}
