package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/config"
)

// selfHealthValidBlock is a minimal valid selfHealth: block whose
// channel points at the validMinimal config's ops-alerts channel.
const selfHealthValidBlock = `
selfHealth:
  window: 90s
  minMonitors: 3
  channel: ops-alerts
  mention: ops-team
`

// TestLoad_selfHealth_valid_succeeds: a minimal valid block parses onto
// the loaded Config.
func TestLoad_selfHealth_valid_succeeds(t *testing.T) {
	cfg, err := config.Load([]byte(validMinimal + selfHealthValidBlock))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SelfHealth == nil {
		t.Fatal("expected cfg.SelfHealth to be non-nil")
	}
	if cfg.SelfHealth.Window.AsDuration() != 90*time.Second {
		t.Errorf("window: got %s, want 90s", cfg.SelfHealth.Window)
	}
	if cfg.SelfHealth.MinMonitors != 3 {
		t.Errorf("minMonitors: got %d, want 3", cfg.SelfHealth.MinMonitors)
	}
	if cfg.SelfHealth.Channel != "ops-alerts" {
		t.Errorf("channel: got %q, want ops-alerts", cfg.SelfHealth.Channel)
	}
}

// TestLoad_selfHealth_absentBlock_isNil: omitting the block disables
// the feature (Config.SelfHealth == nil), and the binary still loads.
func TestLoad_selfHealth_absentBlock_isNil(t *testing.T) {
	cfg, err := config.Load([]byte(validMinimal))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SelfHealth != nil {
		t.Errorf("expected nil SelfHealth when block absent, got %+v", cfg.SelfHealth)
	}
}

// TestLoad_selfHealth_rejectsMinMonitorsBelowTwo: minMonitors < 2 is
// pathological (a 1–2 monitor deployment inferring global blindness off
// a single flaky lookup) and rejected.
func TestLoad_selfHealth_rejectsMinMonitorsBelowTwo(t *testing.T) {
	block := `
selfHealth:
  window: 90s
  minMonitors: 1
  channel: ops-alerts
`
	_, err := config.Load([]byte(validMinimal + block))
	if err == nil {
		t.Fatal("expected error for minMonitors < 2")
	}
	if !strings.Contains(err.Error(), "minMonitors") {
		t.Errorf("error should name minMonitors: %v", err)
	}
}

// TestLoad_selfHealth_rejectsUnknownChannel: the channel slug must
// resolve to a configured Slack channel.
func TestLoad_selfHealth_rejectsUnknownChannel(t *testing.T) {
	block := `
selfHealth:
  window: 90s
  minMonitors: 3
  channel: nope-not-real
`
	_, err := config.Load([]byte(validMinimal + block))
	if err == nil {
		t.Fatal("expected error for unknown channel slug")
	}
	if !strings.Contains(err.Error(), "channel") {
		t.Errorf("error should name channel: %v", err)
	}
}
