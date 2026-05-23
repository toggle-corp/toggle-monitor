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

func TestLoad_acceptsUserMappingSlugInNotify(t *testing.T) {
	// Inject a userMapping and reference one of its slugs from a
	// monitor's notify list.
	data := strings.Replace(validMinimal,
		"      tokenEnv: SLACK_BOT_TOKEN\n",
		"      tokenEnv: SLACK_BOT_TOKEN\n  userMapping:\n    alice: U01ABCDEF12\n    ops-team: S02GHIJKL34\n",
		1)
	data = strings.Replace(data, "    slack: ops-alerts\n",
		"    slack: ops-alerts\n    notify: [alice, ops-team]\n", 1)
	if _, err := config.Load([]byte(data)); err != nil {
		t.Fatalf("userMapping slugs should be valid notify entries: %v", err)
	}
}

func TestLoad_rejectsMalformedUserMappingID(t *testing.T) {
	data := strings.Replace(validMinimal,
		"      tokenEnv: SLACK_BOT_TOKEN\n",
		"      tokenEnv: SLACK_BOT_TOKEN\n  userMapping:\n    alice: \"not-an-id\"\n",
		1)
	_, err := config.Load([]byte(data))
	if err == nil {
		t.Fatal("expected malformed userMapping ID to be rejected")
	}
}

func TestLoad_acceptsGroupNotify(t *testing.T) {
	data := strings.Replace(validMinimal,
		"  - slug: gateways\n    friendlyName: Gateways\n",
		"  - slug: gateways\n    friendlyName: Gateways\n    notify: [\"<!here>\"]\n",
		1)
	if _, err := config.Load([]byte(data)); err != nil {
		t.Fatalf("group.notify should accept raw markup: %v", err)
	}
}

// TestLoad_proxies_acceptsValidBlockAndMonitorReference confirms the
// happy path: a proxies[] block + a monitor referencing one of those
// slugs validates cleanly and ends up on the loaded config.
func TestLoad_proxies_acceptsValidBlockAndMonitorReference(t *testing.T) {
	data := withReplaced(t,
		"groups:\n",
		"proxies:\n"+
			"  - slug: corp\n"+
			"    protocol: socks5\n"+
			"    server: proxy.internal.example\n"+
			"    port: 1080\n"+
			"groups:\n")
	data = withReplacedBytes(t, data, "    slack: ops-alerts", "    proxy: corp\n    slack: ops-alerts")

	cfg, err := config.Load(data)
	if err != nil {
		t.Fatalf("expected proxies block to load cleanly, got: %v", err)
	}
	if len(cfg.Proxies) != 1 || cfg.Proxies[0].Slug != "corp" {
		t.Errorf("expected 1 proxy 'corp', got %+v", cfg.Proxies)
	}
	if cfg.Monitors[0].Proxy != "corp" {
		t.Errorf("expected Monitors[0].Proxy == 'corp', got %q", cfg.Monitors[0].Proxy)
	}
}

// TestLoad_proxies_rejectsMonitorReferencingUnknownProxy: forward refs
// to undeclared proxy slugs must fail validation, with a clear error.
func TestLoad_proxies_rejectsMonitorReferencingUnknownProxy(t *testing.T) {
	data := withReplaced(t,
		"    slack: ops-alerts",
		"    proxy: ghost\n    slack: ops-alerts")
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected unknown-proxy reference to fail, got success")
	}
	if !strings.Contains(err.Error(), "unknown proxy slug") {
		t.Errorf("expected 'unknown proxy slug' in error, got: %v", err)
	}
}

// TestLoad_proxies_rejectsUnsupportedProtocol enforces the protocol
// enum (v1: socks5 only).
func TestLoad_proxies_rejectsUnsupportedProtocol(t *testing.T) {
	data := withReplaced(t,
		"groups:\n",
		"proxies:\n"+
			"  - slug: corp\n"+
			"    protocol: http\n"+
			"    server: proxy.internal.example\n"+
			"    port: 8080\n"+
			"groups:\n")
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected http protocol to be rejected, got success")
	}
	if !strings.Contains(err.Error(), "socks5") {
		t.Errorf("expected error to mention 'socks5', got: %v", err)
	}
}

// withReplacedBytes is the byte-slice cousin of withReplaced.
func withReplacedBytes(t *testing.T, data []byte, old, new string) []byte {
	t.Helper()
	if !strings.Contains(string(data), old) {
		t.Fatalf("test setup: data does not contain %q", old)
	}
	return []byte(strings.Replace(string(data), old, new, 1))
}

// TestLoad_tlsInsecureSkipVerify_relaxesSSLThresholdsForHTTPS:
// for an HTTPS monitor with self-signed cert support, the SSL
// thresholds must not be required (the SSL state machine is bypassed
// entirely for that monitor).
func TestLoad_tlsInsecureSkipVerify_relaxesSSLThresholdsForHTTPS(t *testing.T) {
	// Baseline: HTTPS URL with no SSL thresholds → must fail today.
	bad := withReplaced(t,
		"url: http://bastion.local/health",
		"url: https://internal.cluster.local/health")
	if _, err := config.Load(bad); err == nil {
		t.Fatal("expected HTTPS-without-thresholds to fail validation, got success")
	}

	// With tlsInsecureSkipVerify=true, the same monitor should load.
	good := withReplaced(t,
		"url: http://bastion.local/health",
		"url: https://internal.cluster.local/health\n    tlsInsecureSkipVerify: true")
	cfg, err := config.Load(good)
	if err != nil {
		t.Fatalf("expected tlsInsecureSkipVerify to relax SSL-threshold requirement, got: %v", err)
	}
	if !cfg.Monitors[0].TLSInsecureSkipVerify {
		t.Error("expected Monitors[0].TLSInsecureSkipVerify to be true")
	}
}

// kubeWith returns validMinimal with a kube: block appended, wired
// to ops-alerts so SSL-threshold validation stays satisfied.
func kubeWith(extra string) string {
	return validMinimal + `
kube:
  annotationDomain: monitor.togglecorp.com
  resyncInterval: 30m
  presets:
    - slug: internal-api
      scheme: https
      path: /health
      httpMethod: GET
      acceptedStatusCodes: [200]
      interval: 5m
      timeout: 10s
      retries: 2
      retryBackoff: 5s
      followRedirects: false
      reminderInterval: 3d
      sslAlertThreshold: 30d
      sslEscalationThreshold: 7d
      sslReminderInterval: 3d
      slack: ops-alerts
` + extra
}

// defaultPreset is removed — the operator should be told to write a
// trailing match rule instead, and the error should call out the
// removed key explicitly so a config carried over from an older
// version fails loudly.
func TestLoad_kube_defaultPreset_isRemoved(t *testing.T) {
	data := []byte(kubeWith("  defaultPreset: internal-api\n"))
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected validation error for the removed defaultPreset field")
	}
	msg := err.Error()
	if !strings.Contains(msg, "defaultPreset") {
		t.Errorf("error should call out defaultPreset, got: %v", err)
	}
	if !strings.Contains(msg, "match") {
		t.Errorf("error should point operators at the match-rule replacement, got: %v", err)
	}
}

// A wildcard match rule (no when:) is the new replacement for
// defaultPreset; rules after it are unreachable.
func TestLoad_kube_match_acceptsWildcardFallback(t *testing.T) {
	data := []byte(kubeWith("  match:\n    - when: { namespace: prod }\n      preset: internal-api\n    - preset: internal-api\n"))
	if _, err := config.Load(data); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoad_kube_match_rejectsRulesAfterWildcard(t *testing.T) {
	data := []byte(kubeWith("  match:\n    - preset: internal-api\n    - when: { namespace: prod }\n      preset: internal-api\n"))
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected unreachable-after-wildcard error")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("error should call out the unreachable rule, got: %v", err)
	}
}

func TestLoad_kube_preset_acceptsKnownGroup(t *testing.T) {
	// gateways is declared in validMinimal so this should pass.
	data := []byte(kubeWith("      group: gateways\n"))
	if _, err := config.Load(data); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoad_kube_preset_rejectsUnknownGroup(t *testing.T) {
	data := []byte(kubeWith("      group: ghost-group\n"))
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected validation error for unknown preset group slug")
	}
	msg := err.Error()
	if !strings.Contains(msg, "ghost-group") {
		t.Errorf("error should echo the bad group slug, got: %v", err)
	}
	if !strings.Contains(msg, "unknown group") || !strings.Contains(msg, "presets") {
		t.Errorf("error should point at presets[].group, got: %v", err)
	}
}

func TestLoad_kube_match_rejectsUnknownPresetSlug(t *testing.T) {
	data := []byte(kubeWith("  match:\n    - when: { namespace: foo }\n      preset: ghost\n"))
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected validation error for unknown match preset slug")
	}
	if !strings.Contains(err.Error(), "ghost") || !strings.Contains(err.Error(), "match") {
		t.Errorf("error should mention match[].preset and the bad slug, got: %v", err)
	}
}

func TestLoad_kube_friendlyName_acceptsKnownValues(t *testing.T) {
	for _, v := range []string{"plain", "compact", "dedupe", "title"} {
		data := []byte(kubeWith("  friendlyName: " + v + "\n"))
		if _, err := config.Load(data); err != nil {
			t.Errorf("style %q rejected: %v", v, err)
		}
	}
}

func TestLoad_kube_friendlyName_rejectsUnknownValue(t *testing.T) {
	data := []byte(kubeWith("  friendlyName: cursive\n"))
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected validation error for unknown friendlyName")
	}
	msg := err.Error()
	if !strings.Contains(msg, "friendlyName") {
		t.Errorf("error should call out the offending field, got: %v", err)
	}
	if !strings.Contains(msg, `"cursive"`) {
		t.Errorf("error should echo the bad value, got: %v", err)
	}
	// Every allowed value should appear in the message so the user
	// can copy-paste the right one without re-reading the docs.
	for _, allowed := range config.KubeFriendlyNameStyles {
		if !strings.Contains(msg, allowed) {
			t.Errorf("error should list allowed value %q, got: %v", allowed, err)
		}
	}
}

// Empty when: now means "wildcard fallback" and is valid (used to be
// rejected because there was no concept of a fallback rule).
func TestLoad_kube_match_acceptsEmptyWhenAsFallback(t *testing.T) {
	data := []byte(kubeWith("  match:\n    - when: {}\n      preset: internal-api\n"))
	if _, err := config.Load(data); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoad_kube_match_acceptsIgnoreRule(t *testing.T) {
	data := []byte(kubeWith("  match:\n    - when: { namespace: test-* }\n      ignore: true\n"))
	if _, err := config.Load(data); err != nil {
		t.Errorf("ignore rule should validate, got: %v", err)
	}
}

func TestLoad_kube_match_rejectsNeitherPresetNorIgnore(t *testing.T) {
	data := []byte(kubeWith("  match:\n    - when: { namespace: test-* }\n"))
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected validation error for match rule with neither preset nor ignore")
	}
	if !strings.Contains(err.Error(), "preset") || !strings.Contains(err.Error(), "ignore") {
		t.Errorf("error should mention both preset and ignore, got: %v", err)
	}
}

func TestLoad_kube_match_rejectsBothPresetAndIgnore(t *testing.T) {
	data := []byte(kubeWith("  match:\n    - when: { namespace: test-* }\n      preset: internal-api\n      ignore: true\n"))
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected validation error for match rule with both preset and ignore")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should call out mutual exclusion, got: %v", err)
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
		"DMs (D...)",       // channelId
		"unknown group",    // monitors[0].group
		"raw Slack markup", // monitors[0].notify[0]
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

func TestLoad_dependsOn_acceptsValidForwardReference(t *testing.T) {
	yaml := strings.Replace(validMinimal, "monitors:\n",
		"monitors:\n  - slug: api\n    friendlyName: API\n    url: http://api\n    group: gateways\n    httpMethod: GET\n    acceptedStatusCodes: [200]\n    interval: 5m\n    timeout: 10s\n    retries: 2\n    retryBackoff: 5s\n    followRedirects: false\n    reminderInterval: 3d\n    slack: ops-alerts\n    dependsOn: [bastion]\n",
		1)
	if _, err := config.Load([]byte(yaml)); err != nil {
		t.Fatalf("forward reference should be valid: %v", err)
	}
}

func TestLoad_dependsOn_rejectsUnknownSlug(t *testing.T) {
	yaml := strings.Replace(validMinimal, "    slack: ops-alerts\n",
		"    slack: ops-alerts\n    dependsOn: [does-not-exist]\n",
		1)
	_, err := config.Load([]byte(yaml))
	if err == nil {
		t.Fatal("expected unknown dependsOn slug to fail")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error should name the missing parent, got: %v", err)
	}
}

func TestLoad_dependsOn_rejectsSelfDependency(t *testing.T) {
	yaml := strings.Replace(validMinimal, "    slack: ops-alerts\n",
		"    slack: ops-alerts\n    dependsOn: [bastion]\n",
		1)
	_, err := config.Load([]byte(yaml))
	if err == nil {
		t.Fatal("expected self-dependency to fail")
	}
	if !strings.Contains(err.Error(), "cannot depend on itself") {
		t.Errorf("error should mention self-dependency, got: %v", err)
	}
}

func TestLoad_dependsOn_rejectsCycle(t *testing.T) {
	// Two monitors that depend on each other.
	yaml := strings.Replace(validMinimal,
		`  - slug: bastion
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
`,
		`  - slug: bastion
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
    dependsOn: [alpha]
  - slug: alpha
    friendlyName: Alpha
    url: http://alpha.local/health
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
    dependsOn: [bastion]
`,
		1)
	_, err := config.Load([]byte(yaml))
	if err == nil {
		t.Fatal("expected cycle to be detected")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error should mention cycle, got: %v", err)
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

func TestLoad_statusPage_acceptsExactGroupAndRegex(t *testing.T) {
	data := []byte(validMinimal + `
statusPage:
  title: Test
  sections:
    - title: Exact
      match:
        - group: gateways
    - title: Regex
      match:
        - groupRegex: "^tc-.*$"
`)
	if _, err := config.Load(data); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoad_statusPage_rejectsGroupAndRegexTogether(t *testing.T) {
	data := []byte(validMinimal + `
statusPage:
  title: Test
  sections:
    - title: Bad
      match:
        - group: gateways
          groupRegex: ".*"
`)
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected mutual-exclusion error for group + groupRegex")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should call out mutual exclusion, got: %v", err)
	}
}

func TestLoad_statusPage_rejectsInvalidRegex(t *testing.T) {
	data := []byte(validMinimal + `
statusPage:
  title: Test
  sections:
    - title: Bad
      match:
        - groupRegex: "[oops"
`)
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected invalid-regex error")
	}
	if !strings.Contains(err.Error(), "invalid regex") {
		t.Errorf("error should flag the invalid regex, got: %v", err)
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
