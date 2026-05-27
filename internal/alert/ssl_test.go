package alert_test

import (
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/alert"
)

func TestApplySSL_httpOnlyMonitorIsSkipped(t *testing.T) {
	next, ev := alert.ApplySSL(alert.SSLState{}, alert.SSLCheck{
		At:         t0,
		TLSBearing: false,
	})
	if next.Status != alert.SSLStatusSkipped {
		t.Errorf("status: got %q, want %q", next.Status, alert.SSLStatusSkipped)
	}
	if ev != nil {
		t.Errorf("expected no event, got %+v", ev)
	}
}

func TestApplySSL_healthyCertStaysOK(t *testing.T) {
	next, ev := alert.ApplySSL(alert.SSLState{Status: alert.SSLStatusOK}, alert.SSLCheck{
		At:                  t0,
		ExpiresAt:           t0.Add(90 * 24 * time.Hour),
		TLSBearing:          true,
		AlertThreshold:      30 * 24 * time.Hour,
		EscalationThreshold: 7 * 24 * time.Hour,
		ReminderInterval:    3 * 24 * time.Hour,
	})
	if next.Status != alert.SSLStatusOK {
		t.Errorf("status: got %q, want OK", next.Status)
	}
	if ev != nil {
		t.Error("expected no event for a healthy cert")
	}
}

func TestApplySSL_crossingAlertThreshold_emitsOpen(t *testing.T) {
	next, ev := alert.ApplySSL(alert.SSLState{Status: alert.SSLStatusOK}, alert.SSLCheck{
		At:                  t0,
		ExpiresAt:           t0.Add(20 * 24 * time.Hour), // 20d remaining
		TLSBearing:          true,
		AlertThreshold:      30 * 24 * time.Hour,
		EscalationThreshold: 7 * 24 * time.Hour,
		ReminderInterval:    3 * 24 * time.Hour,
	})
	if next.Status != alert.SSLStatusExpiring {
		t.Errorf("status: got %q, want ssl-expiring", next.Status)
	}
	if ev == nil || ev.Type != alert.EventSSLOpen {
		t.Errorf("expected ssl_open event, got %+v", ev)
	}
}

func TestApplySSL_reminderUsesEscalationCadenceUnderEscalationThreshold(t *testing.T) {
	prev := alert.SSLState{
		Status:         alert.SSLStatusExpiring,
		OpenedAt:       t0,
		LastReminderAt: t0,
	}
	// 5d remaining < 7d escalation → daily cadence. 24h has elapsed
	// since last reminder → reminder fires.
	next, ev := alert.ApplySSL(prev, alert.SSLCheck{
		At:                  t0.Add(25 * time.Hour),
		ExpiresAt:           t0.Add(5 * 24 * time.Hour),
		TLSBearing:          true,
		AlertThreshold:      30 * 24 * time.Hour,
		EscalationThreshold: 7 * 24 * time.Hour,
		ReminderInterval:    3 * 24 * time.Hour,
	})
	if ev == nil || ev.Type != alert.EventSSLReminder {
		t.Errorf("expected ssl_reminder, got %+v", ev)
	}
	if next.LastReminderAt.Equal(prev.LastReminderAt) {
		t.Errorf("LastReminderAt should advance, got %v", next.LastReminderAt)
	}
}

func TestApplySSL_renewalResolves(t *testing.T) {
	prev := alert.SSLState{
		Status:   alert.SSLStatusExpiring,
		OpenedAt: t0,
	}
	// TTL jumps back to 90d, well above the 30d alert threshold.
	next, ev := alert.ApplySSL(prev, alert.SSLCheck{
		At:                  t0.Add(time.Hour),
		ExpiresAt:           t0.Add(90 * 24 * time.Hour),
		TLSBearing:          true,
		AlertThreshold:      30 * 24 * time.Hour,
		EscalationThreshold: 7 * 24 * time.Hour,
		ReminderInterval:    3 * 24 * time.Hour,
	})
	if next.Status != alert.SSLStatusOK {
		t.Errorf("status: got %q, want OK after renewal", next.Status)
	}
	if ev == nil || ev.Type != alert.EventSSLResolve {
		t.Errorf("expected ssl_resolve, got %+v", ev)
	}
}
