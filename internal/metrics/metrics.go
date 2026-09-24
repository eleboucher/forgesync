// Package metrics holds forgesync's Prometheus metrics, served on /metrics next
// to /healthz.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"git.erwanleboucher.dev/eleboucher/forgesync/internal/version"
)

var (
	registry = prometheus.NewRegistry()
	factory  = promauto.With(registry)
)

// Label names shared by several metrics.
const (
	labelResult    = "result"
	labelRepo      = "repo"
	labelMirror    = "mirror"
	labelDirection = "direction"
)

// Values of the "result" label.
const (
	resultOK    = "ok"
	resultError = "error"
)

var (
	Ticks = factory.NewCounterVec(prometheus.CounterOpts{
		Name: "forgesync_ticks_total",
		Help: "Sync ticks, by result. A tick fails only when the canonical Forgejo can't be listed; per-flow failures are in forgesync_flow_runs_total.",
	}, []string{labelResult})

	TickDuration = factory.NewHistogram(prometheus.HistogramOpts{
		Name:    "forgesync_tick_duration_seconds",
		Help:    "How long a sync tick took.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 12), // 1s to ~34m
	})

	LastSuccess = factory.NewGauge(prometheus.GaugeOpts{
		Name: "forgesync_last_success_timestamp_seconds",
		Help: "Unix time the last tick finished without error; 0 until the first one does.",
	})

	FlowRuns = factory.NewCounterVec(prometheus.CounterOpts{
		Name: "forgesync_flow_runs_total",
		Help: "Flow runs, by canonical repo, mirror, direction (inbound, outbound, upstream) and result.",
	}, []string{labelRepo, labelMirror, labelDirection, labelResult})

	FlowLastSuccess = factory.NewGaugeVec(prometheus.GaugeOpts{
		Name: "forgesync_flow_last_success_timestamp_seconds",
		Help: "Unix time a flow last ran without error; the point it resumes from after failures.",
	}, []string{labelRepo, labelMirror, labelDirection})

	Items = factory.NewCounterVec(prometheus.CounterOpts{
		Name: "forgesync_items_total",
		Help: "Issues and comments processed (written, or already up to date), by kind, source forge, destination forge and result.",
	}, []string{"kind", "from", "to", labelResult})

	GitHubRateLimitRemaining = factory.NewGaugeVec(prometheus.GaugeOpts{
		Name: "forgesync_github_rate_limit_remaining",
		Help: "Requests left in GitHub's current rate-limit window, by resource (core, search, graphql).",
	}, []string{"resource"})

	buildInfo = factory.NewGaugeVec(prometheus.GaugeOpts{
		Name: "forgesync_build_info",
		Help: "Always 1; the version label is the running build.",
	}, []string{"version"})
)

func init() {
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	buildInfo.WithLabelValues(version.String()).Set(1)

	// Create the known series at zero, so the first failure shows up in rate()
	// and increase() instead of appearing out of nowhere.
	for _, r := range []string{resultOK, resultError} {
		Ticks.WithLabelValues(r)
		for _, kind := range []string{"issue", "comment"} {
			Items.WithLabelValues(kind, "github", "forgejo", r)
			Items.WithLabelValues(kind, "forgejo", "github", r)
		}
	}
}

// InitFlow creates a flow's run counters at zero, for the same reason.
func InitFlow(repo, mirror, direction string) {
	FlowRuns.WithLabelValues(repo, mirror, direction, resultOK)
	FlowRuns.WithLabelValues(repo, mirror, direction, resultError)
}

// Handler serves the metrics in the Prometheus text format, and counts its
// own scrapes.
func Handler() http.Handler {
	return promhttp.InstrumentMetricHandler(registry,
		promhttp.HandlerFor(registry, promhttp.HandlerOpts{Registry: registry}))
}

// Result is the "result" label value for an outcome: "ok" or "error".
func Result(err error) string {
	if err != nil {
		return resultError
	}
	return resultOK
}
