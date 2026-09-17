package lifecycle

import (
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/config"
	"github.com/toggle-corp/toggle-monitor/internal/pingcheck"
	"github.com/toggle-corp/toggle-monitor/internal/proxypool"
)

// buildPlans is the only place a pingMonitors entry becomes something
// the scheduler can drive, so the ping-specific parts of the plan —
// the prober, and the SSL machinery staying switched off — are worth
// pinning.
func TestBuildPlans_pingMonitorGetsAnICMPProber(t *testing.T) {
	proxies, err := proxypool.Build(nil)
	if err != nil {
		t.Fatalf("build proxy pool: %v", err)
	}

	cfg := config.Config{
		PingMonitors: []config.PingMonitor{{
			Slug:             "edge-router",
			FriendlyName:     "Edge Router",
			Host:             "router.example.test",
			Interval:         config.Duration(time.Minute),
			Timeout:          config.Duration(5 * time.Second),
			Retries:          2,
			RetryBackoff:     config.Duration(2 * time.Second),
			ReminderInterval: config.Duration(72 * time.Hour),
			Slack:            "ops-alerts",
		}},
	}

	plans := buildPlans(cfg, proxies)
	if len(plans) != 1 {
		t.Fatalf("plans: got %d, want 1", len(plans))
	}
	p := plans[0]
	if p.Kind != "ping" {
		t.Errorf("Kind: got %q, want ping", p.Kind)
	}
	if p.URL != "ping://router.example.test" {
		t.Errorf("URL: got %q", p.URL)
	}
	prober, ok := p.Prober.(pingcheck.Config)
	if !ok {
		t.Fatalf("Prober: got %T, want pingcheck.Config", p.Prober)
	}
	if prober.Host != "router.example.test" || prober.Timeout != 5*time.Second {
		t.Errorf("prober config: %+v", prober)
	}
	// An ICMP echo presents no certificate, so the SSL state machine
	// must stay switched off — a non-zero threshold here would start
	// evaluating expiry against a cert that never arrives.
	if p.TLSBearing {
		t.Error("TLSBearing should be false for a ping monitor")
	}
	if p.SSLAlertThreshold != 0 || p.SSLEscalationThreshold != 0 || p.SSLReminderInterval != 0 {
		t.Error("ping plan carries SSL thresholds")
	}
	if p.ProxyDialer != nil {
		t.Error("ping plan carries a proxy dialer")
	}
	if p.Retries != 2 || p.RetryBackoff != 2*time.Second {
		t.Errorf("retry knobs not carried: retries=%d backoff=%v", p.Retries, p.RetryBackoff)
	}
}
