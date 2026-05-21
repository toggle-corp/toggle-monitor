package observability_test

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/observability"
)

// TestMetrics_exposesAllDocumentedSeries scrapes the /metrics handler
// after a few synthetic observations and checks that every documented
// series name is present (plus the Go runtime collectors).
func TestMetrics_exposesAllDocumentedSeries(t *testing.T) {
	m := observability.New()

	// Synthesize one observation of each kind so all series appear.
	m.ObserveCheck("api", "ok", 200*time.Millisecond)
	m.ObserveCheck("api", "paused", 0)
	m.SetWorkerLastTick(1700000000)
	m.SetActiveIncident("uptime", "api", true)
	m.ConfigLoadTotal.WithLabelValues("success").Inc()
	m.SlackPostTotal.WithLabelValues("success").Inc()
	m.IngressReconcileTotal.WithLabelValues("added").Inc()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	m.Handler().ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"toggle_monitor_checks_total",
		"toggle_monitor_check_duration_seconds",
		"toggle_monitor_active_incidents",
		"toggle_monitor_config_load_total",
		"toggle_monitor_slack_post_total",
		"toggle_monitor_ingress_reconcile_total",
		"toggle_monitor_worker_last_tick_seconds",
		"go_goroutines", // Go runtime collector
		"process_cpu_seconds_total", // process collector
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing series %q in exposition output", want)
		}
	}
}

func TestMetrics_observeCheck_recordsLabels(t *testing.T) {
	m := observability.New()
	m.ObserveCheck("monitor-a", "ok", 10*time.Millisecond)
	m.ObserveCheck("monitor-a", "fail", 20*time.Millisecond)
	m.ObserveCheck("monitor-b", "ok", 30*time.Millisecond)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	m.Handler().ServeHTTP(rr, req)
	out := rr.Body.String()

	for _, want := range []string{
		`toggle_monitor_checks_total{monitor="monitor-a",status="ok"} 1`,
		`toggle_monitor_checks_total{monitor="monitor-a",status="fail"} 1`,
		`toggle_monitor_checks_total{monitor="monitor-b",status="ok"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing series line %q in:\n%s", want, out)
		}
	}
}
