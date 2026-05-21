package config_test

import (
	"strings"
	"testing"

	"github.com/toggle-corp/toggle-monitor/internal/config"
)

// validMinimal is the smallest YAML payload that should pass the
// Issue-2 validator: every required top-level field is set, the
// `kube-discovered` group is declared, and one static monitor
// references a real group.
const validMinimal = `
displayTimezone: Asia/Kathmandu
dbBodyMaxChars: 4000
database:
  host: pg
  port: 5432
  user: tm
  name: tm
  sslMode: require
  passwordEnv: DB_PASSWORD
ui:
  pageSize:
    homepageAlerts: 20
    monitorListing: 50
    monitorHistory: 50
    discoveryListing: 50
  maxPerPage: 200
theme:
  defaultGroupColor: "#64748b"
httpClient:
  userAgent: "toggle-monitor/test"
groups:
  - slug: kube-discovered
    friendlyName: Kube Discovered
  - slug: gateways
    friendlyName: Gateways
monitors:
  - slug: bastion
    friendlyName: Bastion
    url: http://bastion.local/health
    group: gateways
    httpMethod: GET
    acceptedStatusCodes: [200]
    interval: 5m
    timeout: 10s
    retries: 2
    retryBackoff: 5s
    followRedirects: false
    reminderInterval: 3d
`

func TestLoad_validMinimal_succeeds(t *testing.T) {
	cfg, err := config.Load([]byte(validMinimal))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DisplayTimezone != "Asia/Kathmandu" {
		t.Errorf("displayTimezone: got %q, want Asia/Kathmandu", cfg.DisplayTimezone)
	}
	if len(cfg.Monitors) != 1 {
		t.Fatalf("monitors: got %d, want 1", len(cfg.Monitors))
	}
	if cfg.Monitors[0].Slug != "bastion" {
		t.Errorf("monitors[0].slug: got %q, want bastion", cfg.Monitors[0].Slug)
	}
	if cfg.Monitors[0].URL != "http://bastion.local/health" {
		t.Errorf("monitors[0].url: got %q", cfg.Monitors[0].URL)
	}
}

// withReplaced returns validMinimal with `old` replaced by `new`. Used
// by tests that flip a single rule into a violating state.
func withReplaced(t *testing.T, old, new string) []byte {
	t.Helper()
	if !strings.Contains(validMinimal, old) {
		t.Fatalf("test setup: validMinimal does not contain %q", old)
	}
	return []byte(strings.Replace(validMinimal, old, new, 1))
}

func TestLoad_rejectsWhenKubeDiscoveredGroupMissing(t *testing.T) {
	data := withReplaced(t,
		"  - slug: kube-discovered\n    friendlyName: Kube Discovered\n  - slug: gateways",
		"  - slug: gateways",
	)
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "kube-discovered") {
		t.Errorf("error should mention the missing kube-discovered group, got: %v", err)
	}
}

func TestLoad_rejectsMonitorReferencingUnknownGroup(t *testing.T) {
	data := withReplaced(t, "group: gateways", "group: nope")
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown group") {
		t.Errorf("error should mention the unknown group, got: %v", err)
	}
}

func TestLoad_rejectsInvalidSlugRegex(t *testing.T) {
	data := withReplaced(t, "slug: bastion", "slug: Bastion")
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "monitors[0].slug") {
		t.Errorf("error should point at the offending field, got: %v", err)
	}
}

func TestLoad_rejectsDuplicateMonitorSlugs(t *testing.T) {
	dup := `
  - slug: bastion
    friendlyName: Bastion Twin
    url: http://other/health
    group: gateways
    httpMethod: GET
    acceptedStatusCodes: [200]
    interval: 5m
    timeout: 10s
    retries: 2
    retryBackoff: 5s
    followRedirects: false
    reminderInterval: 3d`
	data := []byte(validMinimal + dup)
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error should mention duplicate, got: %v", err)
	}
}

func TestLoad_rejectsUnknownTopLevelKey(t *testing.T) {
	data := []byte(validMinimal + "\nbogus: 1\n")
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected error for unknown top-level key, got nil")
	}
}

func TestLoad_rejectsWhenRetryWindowExceedsInterval(t *testing.T) {
	// retries=2 × (timeout 10s + retryBackoff 5s) = 30s; bump interval down to 20s.
	data := withReplaced(t, "interval: 5m", "interval: 20s")
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "retries × (timeout + retryBackoff)") {
		t.Errorf("error message should explain the timing rule, got: %v", err)
	}
}
