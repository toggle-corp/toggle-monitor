package config_test

import (
	"strings"
	"testing"

	"github.com/toggle-corp/toggle-monitor/internal/config"
)

// validMinimal is the smallest YAML payload that should pass the
// Issue-3 validator: every required top-level field is set, the
// `kube-discovered` group is declared, a slack channel is declared,
// and one static monitor references a real group + slack channel.
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
slack:
  bodyMaxChars: 200
  channels:
    - slug: ops-alerts
      channelId: C0123ABCD
      tokenEnv: SLACK_BOT_TOKEN
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
    slack: ops-alerts
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

func TestLoad_rejectsDMChannelID(t *testing.T) {
	data := withReplaced(t, "channelId: C0123ABCD", "channelId: D0123ABCD")
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected DM channel to be rejected")
	}
	if !strings.Contains(err.Error(), "DM") {
		t.Errorf("error should mention DM, got: %v", err)
	}
}

func TestLoad_rejectsMalformedChannelID(t *testing.T) {
	data := withReplaced(t, "channelId: C0123ABCD", "channelId: not-a-channel")
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected malformed channel id to be rejected")
	}
}

func TestLoad_rejectsMonitorReferencingUnknownSlackChannel(t *testing.T) {
	data := withReplaced(t, "slack: ops-alerts", "slack: nope")
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected unknown slack channel to be rejected")
	}
	if !strings.Contains(err.Error(), "unknown channel slug") {
		t.Errorf("error should mention unknown channel slug, got: %v", err)
	}
}

func TestLoad_rejectsNotifyEntriesThatAreNotRawMarkup(t *testing.T) {
	data := []byte(validMinimal + "    notify: [alice]\n")
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected non-markup notify entry to be rejected (userMapping lands in Issue 13)")
	}
}

func TestLoad_acceptsNotifyEntriesThatAreRawMarkup(t *testing.T) {
	data := []byte(validMinimal + "    notify: [\"<!here>\", \"<@U0123ABC>\", \"<!subteam^S0456DEF>\"]\n")
	if _, err := config.Load(data); err != nil {
		t.Fatalf("expected raw markup to pass: %v", err)
	}
}

func TestLoad_rejectsDBBodyMaxCharsSmallerThanSlackBodyMaxChars(t *testing.T) {
	data := withReplaced(t, "dbBodyMaxChars: 4000", "dbBodyMaxChars: 100")
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected validation error for dbBodyMaxChars < slack.bodyMaxChars")
	}
}

func TestLoad_acceptsYAMLAnchors(t *testing.T) {
	yaml := `
x-monitor-defaults: &staticDefaults
  httpMethod: GET
  acceptedStatusCodes: [200]
  interval: 5m
  timeout: 10s
  retries: 2
  retryBackoff: 5s
  followRedirects: false
  reminderInterval: 3d
  slack: ops-alerts
displayTimezone: UTC
dbBodyMaxChars: 4000
database:
  host: pg
  port: 5432
  user: tm
  name: tm
  sslMode: require
  passwordEnv: DB_PASSWORD
ui:
  pageSize: { homepageAlerts: 20, monitorListing: 50, monitorHistory: 50, discoveryListing: 50 }
  maxPerPage: 200
theme: { defaultGroupColor: "#64748b" }
httpClient: { userAgent: "x" }
slack:
  bodyMaxChars: 200
  channels:
    - { slug: ops-alerts, channelId: C0123ABCD, tokenEnv: SLACK_BOT_TOKEN }
groups:
  - { slug: kube-discovered, friendlyName: Kube Discovered }
  - { slug: gw, friendlyName: GW }
monitors:
  - <<: *staticDefaults
    slug: a
    friendlyName: A
    url: http://a/health
    group: gw
  - <<: *staticDefaults
    slug: b
    friendlyName: B
    url: http://b/health
    group: gw
    interval: 1m
`
	cfg, err := config.Load([]byte(yaml))
	if err != nil {
		t.Fatalf("anchors should work: %v", err)
	}
	if len(cfg.Monitors) != 2 {
		t.Fatalf("monitors: got %d, want 2", len(cfg.Monitors))
	}
	// Inherited from anchor.
	if cfg.Monitors[0].HTTPMethod != "GET" {
		t.Errorf("monitors[0].httpMethod should inherit from anchor, got %q", cfg.Monitors[0].HTTPMethod)
	}
	// Overridden on monitor[1].
	if cfg.Monitors[1].Interval.AsDuration().String() != "1m0s" {
		t.Errorf("monitors[1].interval should be overridden to 1m, got %s", cfg.Monitors[1].Interval)
	}
}

func TestLoad_ignoresXPrefixedTopLevelKeys(t *testing.T) {
	data := []byte(validMinimal + "\nx-arbitrary: { foo: bar, nested: [1, 2] }\n")
	if _, err := config.Load(data); err != nil {
		t.Fatalf("x-* top-level keys should be ignored: %v", err)
	}
}

func TestLoad_reportsMultipleErrorsWithLineNumbers(t *testing.T) {
	// Three distinct violations: invalid channelId (DM), unknown group
	// on monitors[0], non-markup notify entry.
	data := []byte(`
displayTimezone: UTC
dbBodyMaxChars: 4000
database:
  host: pg
  port: 5432
  user: tm
  name: tm
  sslMode: require
  passwordEnv: DB_PASSWORD
ui:
  pageSize: { homepageAlerts: 20, monitorListing: 50, monitorHistory: 50, discoveryListing: 50 }
  maxPerPage: 200
theme: { defaultGroupColor: "#64748b" }
httpClient: { userAgent: x }
slack:
  bodyMaxChars: 200
  channels:
    - { slug: ops-alerts, channelId: D0DM_BAD0, tokenEnv: SLACK_BOT_TOKEN }
groups:
  - { slug: kube-discovered, friendlyName: Kube Discovered }
monitors:
  - slug: api
    friendlyName: API
    url: http://api/health
    group: nope-this-group
    httpMethod: GET
    acceptedStatusCodes: [200]
    interval: 5m
    timeout: 10s
    retries: 2
    retryBackoff: 5s
    followRedirects: false
    reminderInterval: 3d
    slack: ops-alerts
    notify: ["plain-not-markup"]
`)
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected validation errors")
	}
	msg := err.Error()
	for _, want := range []string{
		"DMs (D...)",         // channelId
		"unknown group",      // monitors[0].group
		"raw Slack markup",   // monitors[0].notify[0]
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to mention %q, full message:\n%s", want, msg)
		}
	}
	// Three errors → at least three "line " markers.
	if got := strings.Count(msg, "line "); got < 3 {
		t.Errorf("expected at least 3 line-number markers, got %d in:\n%s", got, msg)
	}
}

func TestInterp_strictResolvesSetVar(t *testing.T) {
	t.Setenv("TM_TEST_VAR", "hello")
	data := withReplaced(t, `userAgent: "toggle-monitor/test"`, `userAgent: "toggle-monitor/${TM_TEST_VAR}"`)
	cfg, err := config.Load(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.HTTPClient.UserAgent != "toggle-monitor/hello" {
		t.Errorf("user agent: got %q", cfg.HTTPClient.UserAgent)
	}
}

func TestInterp_strictUnsetErrorsWithLineNumber(t *testing.T) {
	data := withReplaced(t, `userAgent: "toggle-monitor/test"`, `userAgent: "toggle-monitor/${TM_DEFINITELY_UNSET_XYZZY}"`)
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected unset-var error")
	}
	if !strings.Contains(err.Error(), "TM_DEFINITELY_UNSET_XYZZY") {
		t.Errorf("error should name the var, got: %v", err)
	}
	if !strings.Contains(err.Error(), "line ") {
		t.Errorf("error should include a line number, got: %v", err)
	}
}

func TestInterp_fallbackForUnsetVar(t *testing.T) {
	data := withReplaced(t, `userAgent: "toggle-monitor/test"`, `userAgent: "${TM_UNSET_XYZZY:-defaulted}"`)
	cfg, err := config.Load(data)
	if err != nil {
		t.Fatalf("expected fallback to resolve cleanly, got: %v", err)
	}
	if cfg.HTTPClient.UserAgent != "defaulted" {
		t.Errorf("user agent: got %q, want 'defaulted'", cfg.HTTPClient.UserAgent)
	}
}

func TestInterp_fallbackForEmptyVar(t *testing.T) {
	t.Setenv("TM_EMPTY_VAR", "")
	data := withReplaced(t, `userAgent: "toggle-monitor/test"`, `userAgent: "${TM_EMPTY_VAR:-from-fallback}"`)
	cfg, err := config.Load(data)
	if err != nil {
		t.Fatalf("expected fallback to apply for empty value, got: %v", err)
	}
	if cfg.HTTPClient.UserAgent != "from-fallback" {
		t.Errorf("user agent: got %q", cfg.HTTPClient.UserAgent)
	}
}

func TestInterp_dollarEscape(t *testing.T) {
	data := withReplaced(t, `userAgent: "toggle-monitor/test"`, `userAgent: "literal $$dollar"`)
	cfg, err := config.Load(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.HTTPClient.UserAgent != "literal $dollar" {
		t.Errorf("user agent: got %q", cfg.HTTPClient.UserAgent)
	}
}

func TestInterp_rejectsInterpolationIntoPasswordEnv(t *testing.T) {
	t.Setenv("TM_PASSWORD_VAR", "actual_password_value")
	data := withReplaced(t, "passwordEnv: DB_PASSWORD", "passwordEnv: ${TM_PASSWORD_VAR}")
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected interpolation into passwordEnv to be rejected")
	}
	if !strings.Contains(err.Error(), "passwordEnv") {
		t.Errorf("error should mention passwordEnv, got: %v", err)
	}
}

func TestInterp_rejectsInterpolationIntoTokenEnv(t *testing.T) {
	t.Setenv("TM_TOKEN_VAR", "xoxb-secret-value")
	data := withReplaced(t, "tokenEnv: SLACK_BOT_TOKEN", "tokenEnv: ${TM_TOKEN_VAR}")
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected interpolation into tokenEnv to be rejected")
	}
	if !strings.Contains(err.Error(), "tokenEnv") {
		t.Errorf("error should mention tokenEnv, got: %v", err)
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
