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
	SlackRetryTotal       *prometheus.CounterVec
	SlackFreshParentTotal *prometheus.CounterVec
	IngressReconcileTotal *prometheus.CounterVec
	WorkerLastTickSeconds prometheus.Gauge

	// AM-scoped counters (ADR-0005). Kept distinct from SlackPostTotal
	// so dashboards can separate AM-driven volume from monitor-driven
	// volume even though both ultimately hit the same Slack channel.
	AMWebhookRequestTotal *prometheus.CounterVec
	AMAlertProcessedTotal *prometheus.CounterVec
	AMSlackPostTotal      *prometheus.CounterVec
	AMRateLimitDropTotal  *prometheus.CounterVec
	AMLateResolveTotal       prometheus.Counter
	AMWebhookLatencySeconds  prometheus.Histogram
	AMBatchSizeHist          prometheus.Histogram

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
			Help: "Slack notify operations, partitioned by result (success/fail) and reason " +
				"(ok/transient/persistent/permanent_bug/cancelled).",
		}, []string{"result", "reason"}),
		SlackRetryTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toggle_monitor_slack_retry_total",
			Help: "Slack client retry outcomes (recovered = succeeded after at least one retry; " +
				"exhausted = budget ran out). The code label is the slack error or transport " +
				"label that triggered the retry.",
		}, []string{"outcome", "code"}),
		SlackFreshParentTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toggle_monitor_slack_fresh_parent_total",
			Help: "Fresh-parent fallbacks: a reminder fired but no parent thread ts was on " +
				"file (initial Open delivery had failed), so the notifier posted a new " +
				"parent. Partitioned by uptime/ssl.",
		}, []string{"kind"}),
		IngressReconcileTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toggle_monitor_ingress_reconcile_total",
			Help: "k8s ingress reconcile events, partitioned by outcome.",
		}, []string{"result"}),
		WorkerLastTickSeconds: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "toggle_monitor_worker_last_tick_seconds",
			Help: "Unix time of the most recent check completion (success or failure).",
		}),
		AMWebhookRequestTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toggle_monitor_am_webhook_request_total",
			Help: "Alertmanager webhook deliveries, partitioned by result " +
				"(success/fail) and reason (ok/auth/method/too_large/malformed/" +
				"partial_failure).",
		}, []string{"result", "reason"}),
		AMAlertProcessedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toggle_monitor_am_alert_processed_total",
			Help: "Per-alert outcomes inside an AM batch, partitioned by result " +
				"(success/drop/fail) and reason (ok/ignored/duplicate/rate_limited/" +
				"channel_unknown/slack_post/db_insert/db_resolve).",
		}, []string{"result", "reason"}),
		AMSlackPostTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toggle_monitor_am_slack_post_total",
			Help: "Slack postMessage / chat.update calls fired by the AM " +
				"handler. Separate counter from the monitor-side slack_post_total " +
				"so dashboards can disaggregate volumes.",
		}, []string{"result", "reason"}),
		AMRateLimitDropTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toggle_monitor_am_rate_limit_drop_total",
			Help: "AM alerts dropped by the per-channel sliding-window flood " +
				"detector. The channel label is the configured slack slug.",
		}, []string{"channel"}),
		AMLateResolveTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "toggle_monitor_am_late_resolve_total",
			Help: "AM resolves received without any prior firing record on file.",
		}),
		AMWebhookLatencySeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "toggle_monitor_am_webhook_latency_seconds",
			Help:    "End-to-end wall-clock time for one AM webhook delivery.",
			Buckets: prometheus.DefBuckets,
		}),
		AMBatchSizeHist: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "toggle_monitor_am_batch_size",
			Help:    "Size of the alerts[] array in each AM webhook delivery.",
			Buckets: []float64{1, 2, 5, 10, 25, 50, 100, 250},
		}),
	}

	reg.MustRegister(
		m.ChecksTotal,
		m.CheckDurationSeconds,
		m.ActiveIncidents,
		m.ConfigLoadTotal,
		m.SlackPostTotal,
		m.SlackRetryTotal,
		m.SlackFreshParentTotal,
		m.IngressReconcileTotal,
		m.WorkerLastTickSeconds,
		m.AMWebhookRequestTotal,
		m.AMAlertProcessedTotal,
		m.AMSlackPostTotal,
		m.AMRateLimitDropTotal,
		m.AMLateResolveTotal,
		m.AMWebhookLatencySeconds,
		m.AMBatchSizeHist,
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

// SlackPost increments the per-Notify outcome counter. Implements the
// slack.NotifierObserver interface (defined in internal/slack so the
// notifier can stay observability-agnostic).
func (m *Metrics) SlackPost(result, reason string) {
	m.SlackPostTotal.WithLabelValues(result, reason).Inc()
}

// SlackRetry increments the per-retry outcome counter. Implements the
// slack.Observer interface used by the client retry loop.
func (m *Metrics) SlackRetry(outcome, code string) {
	m.SlackRetryTotal.WithLabelValues(outcome, code).Inc()
}

// SlackFreshParent increments the fresh-parent fallback counter.
// Implements the slack.NotifierObserver interface.
func (m *Metrics) SlackFreshParent(kind string) {
	m.SlackFreshParentTotal.WithLabelValues(kind).Inc()
}

// AMWebhookRequest, AMAlertProcessed, AMSlackPost, AMRateLimitDrop,
// AMLateResolve, AMWebhookLatency, AMBatchSize implement the
// alertmanager.Observer interface (defined in internal/alertmanager so
// the AM handler stays observability-agnostic). Method names are
// suggestive of the underlying series; semantics mirror the existing
// monitor-side counters.

// AMWebhookRequest increments toggle_monitor_am_webhook_request_total.
func (m *Metrics) AMWebhookRequest(result, reason string) {
	m.AMWebhookRequestTotal.WithLabelValues(result, reason).Inc()
}

// AMAlertProcessed increments toggle_monitor_am_alert_processed_total.
func (m *Metrics) AMAlertProcessed(result, reason string) {
	m.AMAlertProcessedTotal.WithLabelValues(result, reason).Inc()
}

// AMSlackPost increments toggle_monitor_am_slack_post_total.
func (m *Metrics) AMSlackPost(result, reason string) {
	m.AMSlackPostTotal.WithLabelValues(result, reason).Inc()
}

// AMRateLimitDrop increments toggle_monitor_am_rate_limit_drop_total.
func (m *Metrics) AMRateLimitDrop(channel string) {
	m.AMRateLimitDropTotal.WithLabelValues(channel).Inc()
}

// AMLateResolve increments toggle_monitor_am_late_resolve_total.
func (m *Metrics) AMLateResolve() {
	m.AMLateResolveTotal.Inc()
}

// AMWebhookLatency observes one wall-clock latency sample.
func (m *Metrics) AMWebhookLatency(seconds float64) {
	m.AMWebhookLatencySeconds.Observe(seconds)
}

// AMBatchSize observes the size of one AM batch (len(webhook.Alerts)).
func (m *Metrics) AMBatchSize(n int) {
	m.AMBatchSizeHist.Observe(float64(n))
}
