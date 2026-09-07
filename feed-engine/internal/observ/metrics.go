package observ

import (
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics live on a private registry so we can export ONLY F33D3R metrics
// (no random library defaults from imported packages).
var (
	registry = prometheus.NewRegistry()
	initOnce sync.Once

	httpRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total HTTP requests received, partitioned by brain, method, normalised path, and status.",
		},
		[]string{"brain", "method", "path", "status"},
	)

	httpRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request handler latency in seconds.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		},
		[]string{"brain", "method", "path", "status"},
	)

	httpInFlight = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "http_requests_in_flight",
			Help: "Concurrent in-flight HTTP requests per brain.",
		},
		[]string{"brain"},
	)
)

// initMetrics registers all collectors. Idempotent.
func initMetrics(brain string) {
	initOnce.Do(func() {
		registry.MustRegister(
			httpRequestsTotal,
			httpRequestDuration,
			httpInFlight,
			collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
			collectors.NewGoCollector(),
		)
	})
}

// MetricsHandler returns the http.Handler that exposes the Prometheus
// scrape endpoint. Mount it at /metrics in your router.
func MetricsHandler() http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
		Registry:          registry,
	})
}

// Registry exposes the underlying registry for tests or for one-off custom
// metrics in handler packages.
func Registry() *prometheus.Registry {
	return registry
}
