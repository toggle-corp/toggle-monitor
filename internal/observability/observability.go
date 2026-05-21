// Package observability registers Prometheus metrics and configures
// the slog JSON logger. The Metrics struct holds every counter,
// histogram, and gauge the binary emits; callers grab the relevant
// vec from there and Inc/Observe directly.
package observability

import (
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics is the registered series set for toggle-monitor. Constructed
// once in lifecycle.RunServe and passed down to the modules that
// produce data points. The labels and series names match
// docs/issues-v1.md issue 14.
//
// The lastTickUnix field is a private mirror of the
// WorkerLastTickSeconds gauge, kept in sync via atomic ops so the
// heartbeat package can read it without pulling from the registry.
type Metrics struct {
	registry *prometheus.Registry

	ChecksTotal           *prometheus.CounterVec
	CheckDurationSeconds  *prometheus.HistogramVec
	ActiveIncidents       *prometheus.GaugeVec
	ConfigLoadTotal       *prometheus.CounterVec
	SlackPostTotal        *prometheus.CounterVec
	IngressReconcileTotal *prometheus.CounterVec
	WorkerLastTickSeconds prometheus.Gauge

	lastTickUnix atomic.Int64
}

// New builds a Metrics with all series registered into a fresh
// Registry plus the standard Go runtime + process collectors.
func New() *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		registry: reg,
		ChecksTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toggle_monitor_checks_total",
			Help: "Total HTTP probes performed, partitioned by monitor and outcome.",
		}, []string{"monitor", "status"}),
		CheckDurationSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "toggle_monitor_check_duration_seconds",
			Help:    "Wall-clock duration of an HTTP probe per monitor.",
			Buckets: prometheus.DefBuckets,
		}, []string{"monitor"}),
		ActiveIncidents: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "toggle_monitor_active_incidents",
			Help: "1 if the monitor has an open incident of the given type, 0 otherwise.",
		}, []string{"type", "monitor"}),
		ConfigLoadTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toggle_monitor_config_load_total",
			Help: "Config load attempts, partitioned by outcome.",
		}, []string{"result"}),
		SlackPostTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toggle_monitor_slack_post_total",
			Help: "Slack API call attempts (post/update), partitioned by outcome.",
		}, []string{"result"}),
		IngressReconcileTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toggle_monitor_ingress_reconcile_total",
			Help: "k8s ingress reconcile events, partitioned by outcome.",
		}, []string{"result"}),
		WorkerLastTickSeconds: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "toggle_monitor_worker_last_tick_seconds",
			Help: "Unix time of the most recent check completion (success or failure).",
		}),
	}

	reg.MustRegister(
		m.ChecksTotal,
		m.CheckDurationSeconds,
		m.ActiveIncidents,
		m.ConfigLoadTotal,
		m.SlackPostTotal,
		m.IngressReconcileTotal,
		m.WorkerLastTickSeconds,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Handler returns the Prometheus exposition HTTP handler. Mounted at
// /metrics by the web server.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// ObserveCheck implements scheduler.Metrics: increments the per-status
// counter and observes the per-monitor duration histogram. Paused
// ticks pass duration = 0 (no probe ran).
func (m *Metrics) ObserveCheck(monitor, status string, duration time.Duration) {
	m.ChecksTotal.WithLabelValues(monitor, status).Inc()
	if duration > 0 {
		m.CheckDurationSeconds.WithLabelValues(monitor).Observe(duration.Seconds())
	}
}

// SetWorkerLastTick implements scheduler.Metrics: stamps the worker
// liveness gauge and mirrors the value for non-Prometheus consumers.
func (m *Metrics) SetWorkerLastTick(unixSeconds float64) {
	m.WorkerLastTickSeconds.Set(unixSeconds)
	m.lastTickUnix.Store(int64(unixSeconds))
}

// LastTick returns the most recent worker tick as a Go time.Time, or
// the zero time if no tick has been recorded yet.
func (m *Metrics) LastTick() time.Time {
	v := m.lastTickUnix.Load()
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0)
}

// SetActiveIncident implements scheduler.Metrics: flips the
// per-incident-type gauge.
func (m *Metrics) SetActiveIncident(typeLabel, monitor string, active bool) {
	val := 0.0
	if active {
		val = 1
	}
	m.ActiveIncidents.WithLabelValues(typeLabel, monitor).Set(val)
}
