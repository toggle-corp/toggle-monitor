package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/config"
)

// alertmanagerMinimalBlock is the smallest valid alertmanager: block:
// a non-empty endpoint.tokenEnv plus a root rule whose config.slack
// points at the validMinimal config's ops-alerts channel. Other tests
// append more rules / replace fields to exercise specific validators.
const alertmanagerMinimalBlock = `
alertmanager:
  endpoint:
    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN
  match:
    - when: {}
      config:
        slack: ops-alerts
`

// withAlertmanager returns validMinimal with the given alertmanager
// block appended.
func withAlertmanager(extra string) []byte {
	return []byte(validMinimal + extra)
}

// TestLoad_alertmanager_validMinimal_succeeds: the smallest valid
// alertmanager block parses and validates cleanly, ending up on the
// loaded Config.
func TestLoad_alertmanager_validMinimal_succeeds(t *testing.T) {
	cfg, err := config.Load(withAlertmanager(alertmanagerMinimalBlock))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Alertmanager == nil {
		t.Fatal("expected cfg.Alertmanager to be non-nil")
	}
	if cfg.Alertmanager.Endpoint.TokenEnv != "ALERTMANAGER_WEBHOOK_TOKEN" {
		t.Errorf("tokenEnv: got %q, want ALERTMANAGER_WEBHOOK_TOKEN", cfg.Alertmanager.Endpoint.TokenEnv)
	}
	if len(cfg.Alertmanager.Match) != 1 {
		t.Fatalf("match: got %d entries, want 1", len(cfg.Alertmanager.Match))
	}
	if cfg.Alertmanager.Match[0].Config == nil || cfg.Alertmanager.Match[0].Config.Slack != "ops-alerts" {
		t.Errorf("match[0].config.slack: got %+v, want ops-alerts", cfg.Alertmanager.Match[0].Config)
	}
}

// TestLoad_alertmanager_absentBlock_isNil: the block is optional;
// omitting it leaves Config.Alertmanager == nil and the binary still
// loads.
func TestLoad_alertmanager_absentBlock_isNil(t *testing.T) {
	cfg, err := config.Load([]byte(validMinimal))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Alertmanager != nil {
		t.Errorf("expected cfg.Alertmanager to be nil when block is absent, got %+v", cfg.Alertmanager)
	}
}

// TestLoad_alertmanager_defaults_appliedWhenFieldsAbsent verifies the
// documented defaults (endpoint.path, retentionDays, rateLimit knobs)
// are populated by Load when the YAML omits them.
func TestLoad_alertmanager_defaults_appliedWhenFieldsAbsent(t *testing.T) {
	cfg, err := config.Load(withAlertmanager(alertmanagerMinimalBlock))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	am := cfg.Alertmanager
	if am == nil {
		t.Fatal("cfg.Alertmanager is nil")
	}
	if got, want := am.Endpoint.Path, "/webhooks/alertmanager"; got != want {
		t.Errorf("endpoint.path default: got %q, want %q", got, want)
	}
	if got, want := am.RetentionDays, 180; got != want {
		t.Errorf("retentionDays default: got %d, want %d", got, want)
	}
	if got, want := am.RateLimit.PerChannel, 10; got != want {
		t.Errorf("rateLimit.perChannel default: got %d, want %d", got, want)
	}
	if got, want := am.RateLimit.Window.AsDuration(), 30*time.Minute; got != want {
		t.Errorf("rateLimit.window default: got %s, want %s", got, want)
	}
	if got, want := am.RateLimit.NoticeEvery.AsDuration(), 24*time.Hour; got != want {
		t.Errorf("rateLimit.noticeEvery default: got %s, want %s", got, want)
	}
}

// TestLoad_alertmanager_explicitFields_overrideDefaults: explicitly
// set values stick (no silent override by the defaulter).
func TestLoad_alertmanager_explicitFields_overrideDefaults(t *testing.T) {
	block := `
alertmanager:
  endpoint:
    path: /webhooks/am-prod
    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN
  retentionDays: 90
  rateLimit:
    perChannel: 25
    window: 15m
    noticeEvery: 12h
  match:
    - when: {}
      config:
        slack: ops-alerts
`
	cfg, err := config.Load(withAlertmanager(block))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	am := cfg.Alertmanager
	if am.Endpoint.Path != "/webhooks/am-prod" {
		t.Errorf("endpoint.path: got %q", am.Endpoint.Path)
	}
	if am.RetentionDays != 90 {
		t.Errorf("retentionDays: got %d, want 90", am.RetentionDays)
	}
	if am.RateLimit.PerChannel != 25 {
		t.Errorf("rateLimit.perChannel: got %d, want 25", am.RateLimit.PerChannel)
	}
	if am.RateLimit.Window.AsDuration() != 15*time.Minute {
		t.Errorf("rateLimit.window: got %s, want 15m", am.RateLimit.Window)
	}
	if am.RateLimit.NoticeEvery.AsDuration() != 12*time.Hour {
		t.Errorf("rateLimit.noticeEvery: got %s, want 12h", am.RateLimit.NoticeEvery)
	}
}

// --- Validation-rule tests: one per rule. Each starts from a known-
// valid block and flips a single field into a violating state. ---

func TestLoad_alertmanager_rejectsInvalidEndpointPath(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"missing leading /webhooks/ prefix", "/am"},
		{"two segments", "/webhooks/alertmanager/extra"},
		{"uppercase letters", "/webhooks/AlertManager"},
		{"empty after prefix", "/webhooks/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			block := strings.Replace(alertmanagerMinimalBlock,
				"    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN",
				"    path: "+tc.path+"\n    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN", 1)
			_, err := config.Load(withAlertmanager(block))
			if err == nil {
				t.Fatalf("expected invalid endpoint path %q to be rejected", tc.path)
			}
			if !strings.Contains(err.Error(), "alertmanager.endpoint.path") {
				t.Errorf("error should point at alertmanager.endpoint.path, got: %v", err)
			}
		})
	}
}

func TestLoad_alertmanager_rejectsEmptyTokenEnv(t *testing.T) {
	block := strings.Replace(alertmanagerMinimalBlock,
		"    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN",
		`    tokenEnv: ""`, 1)
	_, err := config.Load(withAlertmanager(block))
	if err == nil {
		t.Fatal("expected empty endpoint.tokenEnv to be rejected")
	}
	if !strings.Contains(err.Error(), "alertmanager.endpoint.tokenEnv") {
		t.Errorf("error should point at alertmanager.endpoint.tokenEnv, got: %v", err)
	}
}

func TestLoad_alertmanager_rejectsNegativeRetentionDays(t *testing.T) {
	block := strings.Replace(alertmanagerMinimalBlock,
		"  match:",
		"  retentionDays: -1\n  match:", 1)
	_, err := config.Load(withAlertmanager(block))
	if err == nil {
		t.Fatal("expected negative retentionDays to be rejected")
	}
	if !strings.Contains(err.Error(), "retentionDays") {
		t.Errorf("error should mention retentionDays, got: %v", err)
	}
}

func TestLoad_alertmanager_rejectsNegativeRateLimitPerChannel(t *testing.T) {
	block := strings.Replace(alertmanagerMinimalBlock,
		"  match:",
		"  rateLimit:\n    perChannel: -5\n  match:", 1)
	_, err := config.Load(withAlertmanager(block))
	if err == nil {
		t.Fatal("expected negative rateLimit.perChannel to be rejected")
	}
	if !strings.Contains(err.Error(), "rateLimit.perChannel") {
		t.Errorf("error should mention rateLimit.perChannel, got: %v", err)
	}
}

func TestLoad_alertmanager_rejectsZeroRateLimitWindowWhenEnabled(t *testing.T) {
	// perChannel > 0 implies window > 0 and noticeEvery > 0.
	block := strings.Replace(alertmanagerMinimalBlock,
		"  match:",
		"  rateLimit:\n    perChannel: 5\n    window: 0s\n    noticeEvery: 1h\n  match:", 1)
	_, err := config.Load(withAlertmanager(block))
	if err == nil {
		t.Fatal("expected rateLimit.window=0 with perChannel>0 to be rejected")
	}
	if !strings.Contains(err.Error(), "rateLimit.window") {
		t.Errorf("error should mention rateLimit.window, got: %v", err)
	}
}

func TestLoad_alertmanager_rejectsZeroRateLimitNoticeEveryWhenEnabled(t *testing.T) {
	block := strings.Replace(alertmanagerMinimalBlock,
		"  match:",
		"  rateLimit:\n    perChannel: 5\n    window: 30m\n    noticeEvery: 0s\n  match:", 1)
	_, err := config.Load(withAlertmanager(block))
	if err == nil {
		t.Fatal("expected rateLimit.noticeEvery=0 with perChannel>0 to be rejected")
	}
	if !strings.Contains(err.Error(), "rateLimit.noticeEvery") {
		t.Errorf("error should mention rateLimit.noticeEvery, got: %v", err)
	}
}

func TestLoad_alertmanager_acceptsRateLimitDisabledWithZeroSubFields(t *testing.T) {
	// perChannel == 0 (disabled) means window / noticeEvery aren't
	// required and are not validated for positivity.
	block := strings.Replace(alertmanagerMinimalBlock,
		"  match:",
		"  rateLimit:\n    perChannel: 0\n    window: 0s\n    noticeEvery: 0s\n  match:", 1)
	if _, err := config.Load(withAlertmanager(block)); err != nil {
		t.Fatalf("expected rateLimit-disabled config to validate, got: %v", err)
	}
}

func TestLoad_alertmanager_rejectsMissingRootRule(t *testing.T) {
	// First rule has a non-empty `when:` → no root.
	block := `
alertmanager:
  endpoint:
    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN
  match:
    - when: { alertname: "Foo" }
      config:
        slack: ops-alerts
`
	_, err := config.Load(withAlertmanager(block))
	if err == nil {
		t.Fatal("expected missing root rule to be rejected")
	}
	if !strings.Contains(err.Error(), "alertmanager.match") {
		t.Errorf("error should mention alertmanager.match, got: %v", err)
	}
}

func TestLoad_alertmanager_rejectsRootMissingSlack(t *testing.T) {
	block := `
alertmanager:
  endpoint:
    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN
  match:
    - when: {}
      config:
        notify: ["<!here>"]
`
	_, err := config.Load(withAlertmanager(block))
	if err == nil {
		t.Fatal("expected root-without-slack to be rejected")
	}
	if !strings.Contains(err.Error(), "slack") {
		t.Errorf("error should mention slack, got: %v", err)
	}
}

func TestLoad_alertmanager_rejectsUnknownSlackChannelInConfig(t *testing.T) {
	block := strings.Replace(alertmanagerMinimalBlock,
		"        slack: ops-alerts",
		"        slack: ghost-channel", 1)
	_, err := config.Load(withAlertmanager(block))
	if err == nil {
		t.Fatal("expected unknown slack channel reference to be rejected")
	}
	if !strings.Contains(err.Error(), "unknown channel slug") {
		t.Errorf("error should mention 'unknown channel slug', got: %v", err)
	}
}

func TestLoad_alertmanager_rejectsUnknownNotifyMappingEntry(t *testing.T) {
	// A bare slug that doesn't resolve to slack.userMapping must fail.
	block := strings.Replace(alertmanagerMinimalBlock,
		"        slack: ops-alerts",
		"        slack: ops-alerts\n        notify: [alice]", 1)
	_, err := config.Load(withAlertmanager(block))
	if err == nil {
		t.Fatal("expected unknown notify mapping slug to be rejected")
	}
	if !strings.Contains(err.Error(), "notify") {
		t.Errorf("error should mention notify, got: %v", err)
	}
}

func TestLoad_alertmanager_acceptsRawSlackMarkupInNotify(t *testing.T) {
	block := strings.Replace(alertmanagerMinimalBlock,
		"        slack: ops-alerts",
		"        slack: ops-alerts\n        notify: [\"<!here>\", \"<!channel>\"]", 1)
	if _, err := config.Load(withAlertmanager(block)); err != nil {
		t.Fatalf("expected raw <…> markup to be accepted in notify: %v", err)
	}
}

func TestLoad_alertmanager_rejectsAlertnameAndAlertnameRegexBothSet(t *testing.T) {
	block := `
alertmanager:
  endpoint:
    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN
  match:
    - when: {}
      config:
        slack: ops-alerts
    - when:
        alertname: "Foo"
        alertnameRegex: "Foo.*"
      config:
        slack: ops-alerts
`
	_, err := config.Load(withAlertmanager(block))
	if err == nil {
		t.Fatal("expected alertname+alertnameRegex collision to be rejected")
	}
	if !strings.Contains(err.Error(), "alertname") {
		t.Errorf("error should mention alertname, got: %v", err)
	}
}

func TestLoad_alertmanager_rejectsLabelTwinKeysOnSameWhen(t *testing.T) {
	// Both `instance` and `instanceRegex` set on the same when.labels
	// is a validation error (per-key twin convention).
	block := `
alertmanager:
  endpoint:
    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN
  match:
    - when: {}
      config:
        slack: ops-alerts
    - when:
        labels:
          instance: "pod-1"
          instanceRegex: "pod-.*"
      config:
        slack: ops-alerts
`
	_, err := config.Load(withAlertmanager(block))
	if err == nil {
		t.Fatal("expected K + KRegex on the same when.labels to be rejected")
	}
	if !strings.Contains(err.Error(), "instance") {
		t.Errorf("error should mention the offending label key, got: %v", err)
	}
}

func TestLoad_alertmanager_rejectsFinalWithEmptyWhen(t *testing.T) {
	block := `
alertmanager:
  endpoint:
    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN
  match:
    - when: {}
      config:
        slack: ops-alerts
    - when: {}
      final: true
      config:
        slack: ops-alerts
`
	_, err := config.Load(withAlertmanager(block))
	if err == nil {
		t.Fatal("expected final:true with empty when: to be rejected")
	}
	if !strings.Contains(err.Error(), "final") {
		t.Errorf("error should mention final, got: %v", err)
	}
}

func TestLoad_alertmanager_rejectsInvalidAlertnameRegex(t *testing.T) {
	block := `
alertmanager:
  endpoint:
    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN
  match:
    - when: {}
      config:
        slack: ops-alerts
    - when:
        alertnameRegex: "[invalid"
      config:
        slack: ops-alerts
`
	_, err := config.Load(withAlertmanager(block))
	if err == nil {
		t.Fatal("expected invalid regex to be rejected")
	}
	if !strings.Contains(err.Error(), "alertnameRegex") {
		t.Errorf("error should mention alertnameRegex, got: %v", err)
	}
}

func TestLoad_alertmanager_rejectsInvalidAlertnameGlob(t *testing.T) {
	block := `
alertmanager:
  endpoint:
    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN
  match:
    - when: {}
      config:
        slack: ops-alerts
    - when:
        alertname: "[invalid"
      config:
        slack: ops-alerts
`
	_, err := config.Load(withAlertmanager(block))
	if err == nil {
		t.Fatal("expected invalid glob to be rejected")
	}
	if !strings.Contains(err.Error(), "alertname") {
		t.Errorf("error should mention alertname, got: %v", err)
	}
}

func TestLoad_alertmanager_rejectsInvalidLabelKey(t *testing.T) {
	block := `
alertmanager:
  endpoint:
    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN
  match:
    - when: {}
      config:
        slack: ops-alerts
    - when:
        labels:
          "BAD KEY!": "x"
      config:
        slack: ops-alerts
`
	_, err := config.Load(withAlertmanager(block))
	if err == nil {
		t.Fatal("expected invalid label key to be rejected")
	}
	if !strings.Contains(err.Error(), "label") {
		t.Errorf("error should mention label, got: %v", err)
	}
}

func TestLoad_alertmanager_rejectsInvalidLabelValueRegex(t *testing.T) {
	block := `
alertmanager:
  endpoint:
    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN
  match:
    - when: {}
      config:
        slack: ops-alerts
    - when:
        labels:
          instanceRegex: "[bad"
      config:
        slack: ops-alerts
`
	_, err := config.Load(withAlertmanager(block))
	if err == nil {
		t.Fatal("expected invalid label-value regex to be rejected")
	}
	if !strings.Contains(err.Error(), "instanceRegex") {
		t.Errorf("error should mention instanceRegex, got: %v", err)
	}
}

func TestLoad_alertmanager_acceptsNestedRulesAndCascadeShape(t *testing.T) {
	// A small but realistic tree: root + Watchdog ignore-final +
	// severity=critical branch. Confirms nested rules, ignore/final,
	// and the receiver/externalURL selectors all parse and validate.
	block := `
alertmanager:
  endpoint:
    path: /webhooks/am-prod
    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN
  retentionDays: 90
  match:
    - when: {}
      config:
        slack: ops-alerts
    - when: { alertname: "Watchdog" }
      ignore: true
      final: true
    - when:
        receiver: "toggle_monitor"
        externalURL: "https://am.prod.example.test"
        labels:
          severity: "critical"
      config:
        slack: ops-alerts
`
	cfg, err := config.Load(withAlertmanager(block))
	if err != nil {
		t.Fatalf("expected nested tree to validate, got: %v", err)
	}
	if len(cfg.Alertmanager.Match) != 3 {
		t.Fatalf("expected 3 top-level rules, got %d", len(cfg.Alertmanager.Match))
	}
	wd := cfg.Alertmanager.Match[1]
	if wd.Ignore == nil || !*wd.Ignore {
		t.Errorf("Watchdog rule: expected ignore=true, got %+v", wd.Ignore)
	}
	if !wd.Final {
		t.Errorf("Watchdog rule: expected final=true")
	}
}
