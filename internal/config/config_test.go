package config_test

import (
	"strings"
	"testing"
	"time"

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
httpClient:
  userAgent: "toggle-monitor/test"
slack:
  bodyMaxChars: 200
  channels:
    - slug: ops-alerts
      channelId: C0123ABCD
      tokenEnv: SLACK_BOT_TOKEN
monitors:
  - slug: bastion
    friendlyName: Bastion
    url: http://bastion.local/health
    tags: [gateways]
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
    tags: [gateways]
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

// TestLoad_proxies_acceptsValidBlockAndMonitorReference confirms the
// happy path: a proxies[] block + a monitor referencing one of those
// slugs validates cleanly and ends up on the loaded config.
func TestLoad_proxies_acceptsValidBlockAndMonitorReference(t *testing.T) {
	data := withReplaced(t,
		"monitors:\n",
		"proxies:\n"+
			"  - slug: corp\n"+
			"    protocol: socks5\n"+
			"    server: proxy.internal.example\n"+
			"    port: 1080\n"+
			"monitors:\n")
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
		"monitors:\n",
		"proxies:\n"+
			"  - slug: corp\n"+
			"    protocol: http\n"+
			"    server: proxy.internal.example\n"+
			"    port: 8080\n"+
			"monitors:\n")
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

// kubeRootBaseline is the YAML fragment for a valid root rule
// (the mandatory empty-when: rule that every materialized monitor
// inherits). Tests that want to exercise a particular kube.match
// shape inject this as the first entry so they don't drown in
// required-at-root-field errors.
const kubeRootBaseline = `    - when: {}
      config:
        path: /
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
`

// canonicalKubeTree is a representative kube.match tree exercising
// the rule shape: a root rule with empty when:, a kill-switch with
// final:true + ignore:true, a namespace match with nested children,
// !override on notify, and acceptedStatusCodes replace-by-default.
const canonicalKubeTree = `
kube:
  resyncInterval: 30m
  friendlyName: compact
  match:
    - when: {}
      config:
        scheme: https
        path: /
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
        notify: ["<!subteam^S0123ABC>"]
    - when:
        labels:
          monitor.togglecorp.com/disabled: "true"
      ignore: true
      final: true
      nested:
        - when: { namespace: "test-critical-*" }
          ignore: false
    - when: { namespace: "acme-*" }
      config:
        tags: [acme]
        notify: ["<@U0123ABC>"]
      nested:
        - when: { namespaceRegex: "acme-service-a-eoapi-\\d+" }
          config:
            tags: [service-a, eoapi]
          nested:
            - when:
                labels:
                  app.kubernetes.io/name: minio
              config:
                path: /minio/health/live
                acceptedStatusCodes: [200, 204]
              final: true
`

func TestLoad_kube_parsesCascadingTree(t *testing.T) {
	data := []byte(validMinimal + canonicalKubeTree)
	cfg, err := config.Load(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Kube == nil {
		t.Fatal("expected cfg.Kube to be populated")
	}
	if got, want := len(cfg.Kube.Match), 3; got != want {
		t.Fatalf("len(Kube.Match): got %d, want %d", got, want)
	}

	root := cfg.Kube.Match[0]
	if root.When.Namespace != "" || root.When.Host != "" || len(root.When.Labels) != 0 {
		t.Errorf("root rule's When should be zero-value, got %+v", root.When)
	}
	if root.Final {
		t.Errorf("root rule should have final=false, got final=%v", root.Final)
	}
	if root.Ignore != nil {
		t.Errorf("root rule should have ignore unset (nil), got %v", *root.Ignore)
	}
	if root.Config.Path != "/" {
		t.Errorf("root path: got %q, want %q", root.Config.Path, "/")
	}
	if !root.Config.IsSet("path") {
		t.Error("root.Config.IsSet(\"path\") should be true")
	}
	if !root.Config.IsSet("acceptedStatusCodes") {
		t.Error("root.Config.IsSet(\"acceptedStatusCodes\") should be true")
	}
	if root.Config.IsSet("proxy") {
		t.Error("root.Config.IsSet(\"proxy\") should be false (not in YAML)")
	}
	if got := []int(root.Config.AcceptedStatusCodes); len(got) != 1 || got[0] != 200 {
		t.Errorf("acceptedStatusCodes: got %v, want [200]", got)
	}
	if root.Config.Notify.Override {
		t.Error("root.Config.Notify.Override should be false (no !override tag)")
	}
	if got := root.Config.Notify.Values; len(got) != 1 || got[0] != "<!subteam^S0123ABC>" {
		t.Errorf("root notify values: got %v", got)
	}

	killSwitch := cfg.Kube.Match[1]
	if killSwitch.Ignore == nil || !*killSwitch.Ignore || !killSwitch.Final {
		t.Errorf("kill-switch rule should have ignore=final=true, got ignore=%v final=%v",
			killSwitch.Ignore, killSwitch.Final)
	}
	if killSwitch.Config.IsSet("ignore") {
		t.Error("ignore is a rule-level directive — should not appear in Config.setFields")
	}
	if killSwitch.Config.IsSet("final") {
		t.Error("final is a rule-level directive — should not appear in Config.setFields")
	}
	if got, want := killSwitch.When.Labels["monitor.togglecorp.com/disabled"], "true"; got != want {
		t.Errorf("kill-switch label value: got %q, want %q", got, want)
	}
	if len(killSwitch.Nested) != 1 {
		t.Fatalf("expected one nested un-ignore rule under kill-switch, got %d", len(killSwitch.Nested))
	}
	unignore := killSwitch.Nested[0]
	if unignore.Ignore == nil {
		t.Error("un-ignore rule should have ignore set (non-nil), got nil")
	} else if *unignore.Ignore {
		t.Errorf("un-ignore rule should have *ignore=false, got true")
	}
	if unignore.When.Namespace != "test-critical-*" {
		t.Errorf("un-ignore namespace: got %q, want %q", unignore.When.Namespace, "test-critical-*")
	}

	acme := cfg.Kube.Match[2]
	if acme.When.Namespace != "acme-*" {
		t.Errorf("acme namespace: got %q", acme.When.Namespace)
	}
	if len(acme.Nested) != 1 {
		t.Fatalf("expected one nested rule under acme, got %d", len(acme.Nested))
	}
	eoapi := acme.Nested[0]
	if eoapi.When.NamespaceRegex != `acme-service-a-eoapi-\d+` {
		t.Errorf("eoapi namespaceRegex: got %q", eoapi.When.NamespaceRegex)
	}
	if len(eoapi.Nested) != 1 {
		t.Fatalf("expected one nested rule under eoapi, got %d", len(eoapi.Nested))
	}
	minio := eoapi.Nested[0]
	if !minio.Final {
		t.Error("minio rule should have final: true")
	}
	if minio.Config.Path != "/minio/health/live" {
		t.Errorf("minio path: got %q", minio.Config.Path)
	}
	if got := []int(minio.Config.AcceptedStatusCodes); len(got) != 2 || got[0] != 200 || got[1] != 204 {
		t.Errorf("minio acceptedStatusCodes: got %v, want [200 204]", got)
	}
}

// TestLoad_kube_anchorMergeInConfig confirms that a YAML merge key
// (`<<: *anchor`) inside a kube.match[].config: block both
// populates the struct fields AND marks them as Set on the
// setFields map. Without merge-key expansion in UnmarshalYAML,
// IsSet("path") would return false even though the decoded struct
// has Path populated, and the validator would reject the root rule
// as missing required-at-root fields.
func TestLoad_kube_anchorMergeInConfig(t *testing.T) {
	yaml := validMinimal + `
x-kube-root-config: &x-kube-root-config
  scheme: https
  path: /
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

kube:
  resyncInterval: 30m
  match:
    - when: {}
      config:
        <<: *x-kube-root-config
`
	cfg, err := config.Load([]byte(yaml))
	if err != nil {
		t.Fatalf("anchor merge inside config: should validate, got: %v", err)
	}
	root := cfg.Kube.Match[0].Config
	// Struct fields populated by Decode.
	if root.Path != "/" {
		t.Errorf("path: got %q, want %q", root.Path, "/")
	}
	if root.HTTPMethod != "GET" {
		t.Errorf("httpMethod: got %q, want GET", root.HTTPMethod)
	}
	// setFields reflects the anchor-side keys.
	for _, key := range []string{"path", "httpMethod", "acceptedStatusCodes", "interval", "timeout", "retries", "retryBackoff", "followRedirects", "reminderInterval", "sslAlertThreshold", "sslEscalationThreshold", "sslReminderInterval", "slack"} {
		if !root.IsSet(key) {
			t.Errorf("IsSet(%q) should be true after merge-key expansion", key)
		}
	}
}

func TestLoad_kube_notifyOverrideTagSetsOverrideFlag(t *testing.T) {
	// Override tag lives on a *descendant* rule; the root carries the
	// required-at-root baseline so validation has nothing to flag.
	yaml := validMinimal + `
kube:
  resyncInterval: 30m
  match:
` + kubeRootBaseline + `    - when: { namespace: "acme-*" }
      config:
        notify: !override ["<!here>", "<@U0123ABC>"]
`
	cfg, err := config.Load([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	notify := cfg.Kube.Match[1].Config.Notify
	if !notify.Override {
		t.Error("expected Override=true after !override tag")
	}
	if got, want := notify.Values, []string{"<!here>", "<@U0123ABC>"}; !equalStringSlices(got, want) {
		t.Errorf("notify values: got %v, want %v", got, want)
	}
}

func TestLoad_kube_notifyWithoutOverrideTagLeavesOverrideFalse(t *testing.T) {
	yaml := validMinimal + `
kube:
  resyncInterval: 30m
  match:
` + kubeRootBaseline + `    - when: { namespace: "acme-*" }
      config:
        notify: ["<!here>"]
`
	cfg, err := config.Load([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Kube.Match[1].Config.Notify.Override {
		t.Error("expected Override=false without !override tag")
	}
}

func TestLoad_kube_tagsAndDependsOnSupportOverride(t *testing.T) {
	yaml := validMinimal + `
kube:
  resyncInterval: 30m
  match:
` + kubeRootBaseline + `    - when: { namespace: "acme-*" }
      config:
        tags: !override [a, b]
        dependsOn: !override [bastion]
`
	cfg, err := config.Load([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cfgBlock := cfg.Kube.Match[1].Config
	if !cfgBlock.Tags.Override {
		t.Error("tags.Override should be true")
	}
	if !cfgBlock.DependsOn.Override {
		t.Error("dependsOn.Override should be true")
	}
	if got, want := cfgBlock.Tags.Values, []string{"a", "b"}; !equalStringSlices(got, want) {
		t.Errorf("tags values: got %v, want %v", got, want)
	}
	if got, want := cfgBlock.DependsOn.Values, []string{"bastion"}; !equalStringSlices(got, want) {
		t.Errorf("dependsOn values: got %v, want %v", got, want)
	}
}

func TestLoad_kube_emptyWhenParsesAsZeroValue(t *testing.T) {
	yaml := validMinimal + `
kube:
  resyncInterval: 30m
  match:
` + kubeRootBaseline
	cfg, err := config.Load([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	when := cfg.Kube.Match[0].When
	if when.Namespace != "" || when.NamespaceRegex != "" || when.Host != "" || when.HostRegex != "" || len(when.Labels) != 0 {
		t.Errorf("empty when: should parse to zero-value, got %+v", when)
	}
}

func TestLoad_kube_acceptedStatusCodesParsesWithoutOverrideTag(t *testing.T) {
	// acceptedStatusCodes is replace-by-default; the test confirms that
	// the root list parses without the !override tag.
	yaml := validMinimal + `
kube:
  resyncInterval: 30m
  match:
    - when: {}
      config:
        path: /
        httpMethod: GET
        acceptedStatusCodes: [200, 301, 302]
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
`
	cfg, err := config.Load([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := []int(cfg.Kube.Match[0].Config.AcceptedStatusCodes)
	want := []int{200, 301, 302}
	if !equalIntSlices(got, want) {
		t.Errorf("acceptedStatusCodes: got %v, want %v", got, want)
	}
}

func TestLoad_kube_friendlyNameAcceptsDefinedStyles(t *testing.T) {
	for _, style := range config.KubeFriendlyNameStyles {
		yaml := validMinimal + `
kube:
  resyncInterval: 30m
  friendlyName: ` + style + `
  match:
` + kubeRootBaseline
		cfg, err := config.Load([]byte(yaml))
		if err != nil {
			t.Errorf("style %q rejected: %v", style, err)
			continue
		}
		if cfg.Kube.FriendlyName != style {
			t.Errorf("friendlyName: got %q, want %q", cfg.Kube.FriendlyName, style)
		}
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalIntSlices(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
httpClient: { userAgent: "x" }
slack:
  bodyMaxChars: 200
  channels:
    - { slug: ops-alerts, channelId: C0123ABCD, tokenEnv: SLACK_BOT_TOKEN }
monitors:
  - <<: *staticDefaults
    slug: a
    friendlyName: A
    url: http://a/health
    tags: [gw]
  - <<: *staticDefaults
    slug: b
    friendlyName: B
    url: http://b/health
    tags: [gw]
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
	// Two distinct violations: invalid channelId (DM) and a non-markup
	// notify entry on monitors[0]. Validates multi-error accumulation
	// + line numbers.
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
httpClient: { userAgent: x }
slack:
  bodyMaxChars: 200
  channels:
    - { slug: ops-alerts, channelId: D0DM_BAD0, tokenEnv: SLACK_BOT_TOKEN }
monitors:
  - slug: api
    friendlyName: API
    url: http://api/health
    tags: [public]
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
		"raw Slack markup", // monitors[0].notify[0]
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to mention %q, full message:\n%s", want, msg)
		}
	}
	// Two errors → at least two "line " markers.
	if got := strings.Count(msg, "line "); got < 2 {
		t.Errorf("expected at least 2 line-number markers, got %d in:\n%s", got, msg)
	}
}

func TestLoad_dependsOn_acceptsValidForwardReference(t *testing.T) {
	yaml := strings.Replace(validMinimal, "monitors:\n",
		"monitors:\n  - slug: api\n    friendlyName: API\n    url: http://api\n    tags: [gateways]\n    httpMethod: GET\n    acceptedStatusCodes: [200]\n    interval: 5m\n    timeout: 10s\n    retries: 2\n    retryBackoff: 5s\n    followRedirects: false\n    reminderInterval: 3d\n    slack: ops-alerts\n    dependsOn: [bastion]\n",
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
    tags: [gateways]
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
    tags: [gateways]
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
    tags: [gateways]
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

func TestLoad_statusPages_acceptsLeafAndBranch(t *testing.T) {
	data := []byte(validMinimal + `
statusPages:
  - slug: public
    friendlyName: Public
    sections:
      - title: Leaf
        match:
          tags: [gateways]
      - title: Branch
        match:
          any:
            - tags: [gateways]
            - hostRegex: '.*\.example\.com'
`)
	if _, err := config.Load(data); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoad_statusPages_acceptsMultiplePagesWithUniqueSlugs(t *testing.T) {
	data := []byte(validMinimal + `
statusPages:
  - slug: public
    friendlyName: Public
    sections:
      - title: Gateways
        match: { tags: [gateways] }
  - slug: internal
    friendlyName: Internal
    sections:
      - title: All
        match: { hostRegex: ".+" }
`)
	cfg, err := config.Load(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.StatusPages) != 2 {
		t.Fatalf("expected 2 status pages, got %d", len(cfg.StatusPages))
	}
	if cfg.StatusPages[0].Slug != "public" || cfg.StatusPages[1].Slug != "internal" {
		t.Errorf("slug order should be preserved, got %q, %q", cfg.StatusPages[0].Slug, cfg.StatusPages[1].Slug)
	}
}

func TestLoad_statusPages_rejectsDuplicateSlug(t *testing.T) {
	data := []byte(validMinimal + `
statusPages:
  - slug: public
    friendlyName: A
    sections:
      - title: One
        match: { tags: [gateways] }
  - slug: public
    friendlyName: B
    sections:
      - title: Two
        match: { tags: [gateways] }
`)
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected duplicate-slug error")
	}
	if !strings.Contains(err.Error(), "duplicate slug") {
		t.Errorf("error should flag duplicate slug, got: %v", err)
	}
}

func TestLoad_statusPages_rejectsMissingSlug(t *testing.T) {
	data := []byte(validMinimal + `
statusPages:
  - friendlyName: No slug here
    sections:
      - title: One
        match: { tags: [gateways] }
`)
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected missing-slug error")
	}
}

func TestLoad_statusPages_rejectsAnyAndAllTogether(t *testing.T) {
	data := []byte(validMinimal + `
statusPages:
  - slug: public
    friendlyName: Test
    sections:
      - title: Bad
        match:
          any:
            - { tags: [a] }
          all:
            - { tags: [b] }
`)
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected mutual-exclusion error for any + all")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should call out mutual exclusion, got: %v", err)
	}
}

func TestLoad_statusPages_rejectsInvalidRegex(t *testing.T) {
	data := []byte(validMinimal + `
statusPages:
  - slug: public
    friendlyName: Test
    sections:
      - title: Bad
        match:
          hostRegex: "[oops"
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

// ----------------------------------------------------------------------
// Kube cascading-tree validator tests (Task 3 / ADR-0002 §Validation).
// ----------------------------------------------------------------------

// withKubeBlock prepends `kube:` + tree to validMinimal so a per-test
// kube payload can be exercised against an otherwise valid config.
func withKubeBlock(tree string) []byte {
	return []byte(validMinimal + "\nkube:\n  resyncInterval: 30m\n  match:\n" + tree)
}

// TestLoad_kube_canonicalTreeValidates exercises the happy path —
// the canonicalKubeTree fixture covers root + kill-switch + nested
// chain and must pass cleanly.
func TestLoad_kube_canonicalTreeValidates(t *testing.T) {
	data := []byte(validMinimal + canonicalKubeTree)
	if _, err := config.Load(data); err != nil {
		t.Fatalf("canonical tree should validate cleanly, got: %v", err)
	}
}

// (1) Match list missing entirely → must error.
func TestLoad_kube_rejectsEmptyMatch(t *testing.T) {
	data := []byte(validMinimal + "\nkube:\n  resyncInterval: 30m\n  match: []\n")
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected empty match list to be rejected")
	}
	if !strings.Contains(err.Error(), "at least one rule is required") {
		t.Errorf("error should explain the missing root rule, got: %v", err)
	}
}

// (1) First rule is not the root baseline (when: is non-empty) → must error.
func TestLoad_kube_rejectsFirstRuleNotEmptyWhen(t *testing.T) {
	tree := `    - when: { namespace: "acme-*" }
      config:
        path: /
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
`
	_, err := config.Load(withKubeBlock(tree))
	if err == nil {
		t.Fatal("expected first-rule-not-root to be rejected")
	}
	if !strings.Contains(err.Error(), "kube.match[0].when") {
		t.Errorf("error should point at kube.match[0].when, got: %v", err)
	}
	if !strings.Contains(err.Error(), "empty when:") {
		t.Errorf("error should explain root baseline, got: %v", err)
	}
}

// (2) Missing required-at-root field → must error with the field name.
func TestLoad_kube_rejectsMissingRequiredAtRoot(t *testing.T) {
	// Same as kubeRootBaseline but with `slack: ops-alerts` removed.
	tree := `    - when: {}
      config:
        path: /
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
`
	_, err := config.Load(withKubeBlock(tree))
	if err == nil {
		t.Fatal("expected missing-slack-at-root to be rejected")
	}
	if !strings.Contains(err.Error(), "kube.match[0].config.slack") {
		t.Errorf("error should point at the missing field, got: %v", err)
	}
	if !strings.Contains(err.Error(), "required at the root rule") {
		t.Errorf("error should explain why the field is required, got: %v", err)
	}
}

// (2) followRedirects: false counts as "set" because of IsSet —
// removing the key entirely is what should fail.
func TestLoad_kube_rejectsFollowRedirectsUnset(t *testing.T) {
	tree := `    - when: {}
      config:
        path: /
        httpMethod: GET
        acceptedStatusCodes: [200]
        interval: 5m
        timeout: 10s
        retries: 2
        retryBackoff: 5s
        reminderInterval: 3d
        sslAlertThreshold: 30d
        sslEscalationThreshold: 7d
        sslReminderInterval: 3d
        slack: ops-alerts
`
	_, err := config.Load(withKubeBlock(tree))
	if err == nil {
		t.Fatal("expected missing followRedirects to be rejected")
	}
	if !strings.Contains(err.Error(), "followRedirects") {
		t.Errorf("error should mention followRedirects, got: %v", err)
	}
}

// (3) Mixing glob + regex on the same dimension is illegal.
func TestLoad_kube_rejectsNamespaceAndNamespaceRegexInSameWhen(t *testing.T) {
	tree := kubeRootBaseline + `    - when: { namespace: "acme-*", namespaceRegex: "acme-.*" }
`
	_, err := config.Load(withKubeBlock(tree))
	if err == nil {
		t.Fatal("expected namespace + namespaceRegex to be rejected")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should flag mutual exclusion, got: %v", err)
	}
	if !strings.Contains(err.Error(), "kube.match[1].when") {
		t.Errorf("error should point at the offending when:, got: %v", err)
	}
}

func TestLoad_kube_rejectsHostAndHostRegexInSameWhen(t *testing.T) {
	tree := kubeRootBaseline + `    - when: { host: "*.example.com", hostRegex: ".*\\.example\\.com" }
`
	_, err := config.Load(withKubeBlock(tree))
	if err == nil {
		t.Fatal("expected host + hostRegex to be rejected")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should flag mutual exclusion, got: %v", err)
	}
}

// (4) Bad regex must fail with a regex-parse error.
func TestLoad_kube_rejectsInvalidNamespaceRegex(t *testing.T) {
	tree := kubeRootBaseline + `    - when: { namespaceRegex: "[oops" }
`
	_, err := config.Load(withKubeBlock(tree))
	if err == nil {
		t.Fatal("expected invalid regex to be rejected")
	}
	if !strings.Contains(err.Error(), "invalid regex") {
		t.Errorf("error should flag invalid regex, got: %v", err)
	}
	if !strings.Contains(err.Error(), "kube.match[1].when.namespaceRegex") {
		t.Errorf("error path should include the field, got: %v", err)
	}
}

// (5) Bad glob must fail with a glob-parse error.
func TestLoad_kube_rejectsInvalidHostGlob(t *testing.T) {
	// path.Match accepts most patterns; an unclosed character class
	// triggers ErrBadPattern.
	tree := kubeRootBaseline + `    - when: { host: "api-[abc" }
`
	_, err := config.Load(withKubeBlock(tree))
	if err == nil {
		t.Fatal("expected invalid glob to be rejected")
	}
	if !strings.Contains(err.Error(), "invalid glob") {
		t.Errorf("error should flag invalid glob, got: %v", err)
	}
}

// (6) Invalid label key syntax must fail.
func TestLoad_kube_rejectsInvalidLabelKey(t *testing.T) {
	tree := kubeRootBaseline + `    - when:
        labels:
          "BAD!KEY": "value"
`
	_, err := config.Load(withKubeBlock(tree))
	if err == nil {
		t.Fatal("expected invalid label key to be rejected")
	}
	if !strings.Contains(err.Error(), "invalid k8s label key") {
		t.Errorf("error should call out k8s label-key syntax, got: %v", err)
	}
}

// (7) final: true with empty when: is illegal.
func TestLoad_kube_rejectsFinalWithEmptyWhen(t *testing.T) {
	tree := kubeRootBaseline + `    - when: {}
      final: true
`
	_, err := config.Load(withKubeBlock(tree))
	if err == nil {
		t.Fatal("expected final+empty-when to be rejected")
	}
	if !strings.Contains(err.Error(), "final: true requires at least one selector") {
		t.Errorf("error should explain the final/when invariant, got: %v", err)
	}
}

// (8a) Unknown slack channel in a config block must fail.
func TestLoad_kube_rejectsUnknownSlackInConfig(t *testing.T) {
	tree := kubeRootBaseline + `    - when: { namespace: "acme-*" }
      config:
        slack: nope-not-a-channel
`
	_, err := config.Load(withKubeBlock(tree))
	if err == nil {
		t.Fatal("expected unknown slack channel to be rejected")
	}
	if !strings.Contains(err.Error(), "unknown channel slug") {
		t.Errorf("error should mention unknown channel slug, got: %v", err)
	}
	if !strings.Contains(err.Error(), "kube.match[1].config.slack") {
		t.Errorf("error path should include the field, got: %v", err)
	}
}

// (8c) Unknown proxy in a config block must fail.
func TestLoad_kube_rejectsUnknownProxyInConfig(t *testing.T) {
	tree := kubeRootBaseline + `    - when: { namespace: "acme-*" }
      config:
        proxy: ghost
`
	_, err := config.Load(withKubeBlock(tree))
	if err == nil {
		t.Fatal("expected unknown proxy to be rejected")
	}
	if !strings.Contains(err.Error(), "unknown proxy slug") {
		t.Errorf("error should mention unknown proxy slug, got: %v", err)
	}
}

// (8d) Notify entries must resolve to a userMapping slug or be raw
// <…> Slack markup.
func TestLoad_kube_rejectsUnknownNotifyEntry(t *testing.T) {
	tree := kubeRootBaseline + `    - when: { namespace: "acme-*" }
      config:
        notify: [not-a-real-slug]
`
	_, err := config.Load(withKubeBlock(tree))
	if err == nil {
		t.Fatal("expected unknown notify entry to be rejected")
	}
	if !strings.Contains(err.Error(), "raw Slack markup") {
		t.Errorf("error should explain notify syntax, got: %v", err)
	}
}

// (8d) Valid notify entries — userMapping slug + raw markup — must pass.
func TestLoad_kube_acceptsUserMappingNotifyEntry(t *testing.T) {
	data := strings.Replace(validMinimal,
		"      tokenEnv: SLACK_BOT_TOKEN\n",
		"      tokenEnv: SLACK_BOT_TOKEN\n  userMapping:\n    alice: U01ABCDEF12\n",
		1)
	data += "\nkube:\n  resyncInterval: 30m\n  match:\n" + kubeRootBaseline +
		`    - when: { namespace: "acme-*" }
      config:
        notify: [alice, "<!here>"]
`
	if _, err := config.Load([]byte(data)); err != nil {
		t.Fatalf("userMapping slug + raw markup should pass, got: %v", err)
	}
}

// (8e) dependsOn must resolve to a static monitor; unknown slugs fail.
func TestLoad_kube_rejectsUnknownDependsOnRef(t *testing.T) {
	tree := kubeRootBaseline + `    - when: { namespace: "acme-*" }
      config:
        dependsOn: [does-not-exist]
`
	_, err := config.Load(withKubeBlock(tree))
	if err == nil {
		t.Fatal("expected unknown dependsOn to be rejected")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error should name the missing parent, got: %v", err)
	}
}

// (8e) dependsOn pointing at a declared static monitor passes.
func TestLoad_kube_acceptsValidDependsOnRef(t *testing.T) {
	tree := kubeRootBaseline + `    - when: { namespace: "acme-*" }
      config:
        dependsOn: [bastion]
`
	if _, err := config.Load(withKubeBlock(tree)); err != nil {
		t.Fatalf("dependsOn on a real static monitor should pass, got: %v", err)
	}
}

// (9) httpMethod enum on the root must be one of the supported verbs.
func TestLoad_kube_rejectsInvalidHTTPMethod(t *testing.T) {
	tree := `    - when: {}
      config:
        path: /
        httpMethod: PATCH
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
`
	_, err := config.Load(withKubeBlock(tree))
	if err == nil {
		t.Fatal("expected PATCH to be rejected (v1 enum: GET/HEAD/POST/PUT/DELETE)")
	}
	if !strings.Contains(err.Error(), "GET, HEAD, POST, PUT, DELETE") {
		t.Errorf("error should list the enum, got: %v", err)
	}
}

// (10) scheme enum.
func TestLoad_kube_rejectsInvalidScheme(t *testing.T) {
	tree := kubeRootBaseline + `    - when: { namespace: "acme-*" }
      config:
        scheme: ftp
`
	_, err := config.Load(withKubeBlock(tree))
	if err == nil {
		t.Fatal("expected invalid scheme to be rejected")
	}
	if !strings.Contains(err.Error(), `must be "http" or "https"`) {
		t.Errorf("error should explain the scheme enum, got: %v", err)
	}
}

// (11) friendlyName enum.
func TestLoad_kube_rejectsUnknownFriendlyName(t *testing.T) {
	data := []byte(validMinimal + "\nkube:\n  resyncInterval: 30m\n  friendlyName: shouty\n  match:\n" + kubeRootBaseline)
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected unknown friendlyName to be rejected")
	}
	if !strings.Contains(err.Error(), "friendlyName") {
		t.Errorf("error should mention friendlyName, got: %v", err)
	}
}

// (12) resyncInterval lower bound (>= 1m).
func TestLoad_kube_rejectsResyncIntervalTooSmall(t *testing.T) {
	data := []byte(validMinimal + "\nkube:\n  resyncInterval: 30s\n  match:\n" + kubeRootBaseline)
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected sub-1m resyncInterval to be rejected")
	}
	if !strings.Contains(err.Error(), "resyncInterval") || !strings.Contains(err.Error(), ">= 1m") {
		t.Errorf("error should explain the minimum, got: %v", err)
	}
}

// (2 sanity) acceptedStatusCodes at root must contain valid HTTP codes.
func TestLoad_kube_rejectsRootBadStatusCode(t *testing.T) {
	tree := `    - when: {}
      config:
        path: /
        httpMethod: GET
        acceptedStatusCodes: [99]
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
`
	_, err := config.Load(withKubeBlock(tree))
	if err == nil {
		t.Fatal("expected invalid status code to be rejected")
	}
	if !strings.Contains(err.Error(), "not a valid HTTP status code") {
		t.Errorf("error should mention status code validity, got: %v", err)
	}
}

// Recursive walk: nested rule's error path must include the full chain.
func TestLoad_kube_recursiveWalkPathIncludesNested(t *testing.T) {
	tree := kubeRootBaseline + `    - when: { namespace: "acme-*" }
      nested:
        - when: { namespaceRegex: "[oops" }
`
	_, err := config.Load(withKubeBlock(tree))
	if err == nil {
		t.Fatal("expected nested invalid regex to be rejected")
	}
	if !strings.Contains(err.Error(), "kube.match[1].nested[0].when.namespaceRegex") {
		t.Errorf("error path should descend into nested, got: %v", err)
	}
}

// TODO(warnings): the ADR defines two structural warnings (empty
// when: deeper than root; ignore:true at a leaf with non-empty
// config). Skipped here pending a warning channel — see
// validateKube's TODO comment.

// ----------------------------------------------------------------------
// Unknown-key validation — strict schema enforcement at every level.
// ----------------------------------------------------------------------

// (1) Typo at the rule level inside kube.match — the original
// bug-report case: `nestedd:` instead of `nested:`.
func TestLoad_unknownKey_kubeMatchRule(t *testing.T) {
	tree := kubeRootBaseline + `    - when: { namespace: "toggle-*" }
      nestedd:
        - when: { namespace: "toggle-capnnepal" }
`
	_, err := config.Load(withKubeBlock(tree))
	if err == nil {
		t.Fatal("expected unknown key 'nestedd' to be rejected")
	}
	msg := err.Error()
	if !strings.Contains(msg, "kube.match[1]") {
		t.Errorf("error path should point at the rule, got: %s", msg)
	}
	if !strings.Contains(msg, `unknown key "nestedd"`) {
		t.Errorf("error should name the unknown key, got: %s", msg)
	}
	if !strings.Contains(msg, "allowed keys here: [config, final, ignore, nested, when]") {
		t.Errorf("error should list the allowed keys for KubeMatchRule, got: %s", msg)
	}
}

// (2) Misplaced rule-level key inside config: the original bug-report
// case — `final: true` belongs as a sibling of `config:`, not inside
// it.
func TestLoad_unknownKey_kubeConfigBlock(t *testing.T) {
	tree := kubeRootBaseline + `    - when: { namespace: "toggle-*" }
      config:
        tags: [prod]
        final: true
`
	_, err := config.Load(withKubeBlock(tree))
	if err == nil {
		t.Fatal("expected misplaced 'final' inside config: to be rejected")
	}
	msg := err.Error()
	if !strings.Contains(msg, "kube.match[1].config") {
		t.Errorf("error path should point at the config block, got: %s", msg)
	}
	if !strings.Contains(msg, `unknown key "final"`) {
		t.Errorf("error should name the unknown key, got: %s", msg)
	}
}

// (3) Typo at the Monitor level.
func TestLoad_unknownKey_monitor(t *testing.T) {
	data := withReplaced(t, "    slack: ops-alerts",
		"    slack: ops-alerts\n    timeoutt: 1s")
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected unknown 'timeoutt' on a monitor to be rejected")
	}
	msg := err.Error()
	if !strings.Contains(msg, "monitors[0]") {
		t.Errorf("error path should point at monitors[0], got: %s", msg)
	}
	if !strings.Contains(msg, `unknown key "timeoutt"`) {
		t.Errorf("error should name the unknown key, got: %s", msg)
	}
}

// (4) Typo at the Slack block level.
func TestLoad_unknownKey_slackBlock(t *testing.T) {
	data := withReplaced(t,
		"slack:\n  bodyMaxChars: 200",
		"slack:\n  bodyMaxCharz: 200\n  bodyMaxChars: 200")
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected unknown key in slack block to be rejected")
	}
	msg := err.Error()
	if !strings.Contains(msg, `unknown key "bodyMaxCharz"`) {
		t.Errorf("error should name the unknown key, got: %s", msg)
	}
	if !strings.Contains(msg, "slack:") {
		t.Errorf("error path should mention slack, got: %s", msg)
	}
}

// (4b) Typo inside a SlackChannel.
func TestLoad_unknownKey_slackChannel(t *testing.T) {
	data := withReplaced(t,
		"      tokenEnv: SLACK_BOT_TOKEN",
		"      tokenEnv: SLACK_BOT_TOKEN\n      channelIdd: C0123ABCD")
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected unknown key in slack channel to be rejected")
	}
	msg := err.Error()
	if !strings.Contains(msg, `unknown key "channelIdd"`) {
		t.Errorf("error should name the unknown key, got: %s", msg)
	}
	if !strings.Contains(msg, "slack.channels[0]") {
		t.Errorf("error path should descend into channels[0], got: %s", msg)
	}
}

// (5) Typo inside a StatusPage tree.
func TestLoad_unknownKey_statusPageSection(t *testing.T) {
	data := []byte(validMinimal + `
statusPages:
  - slug: public
    friendlyName: Public
    sections:
      - title: Bad
        matchh:
          tags: [gateways]
`)
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected unknown 'matchh' in section to be rejected")
	}
	msg := err.Error()
	if !strings.Contains(msg, `unknown key "matchh"`) {
		t.Errorf("error should name the unknown key, got: %s", msg)
	}
	if !strings.Contains(msg, "statusPages[0].sections[0]") {
		t.Errorf("error path should descend into sections[0], got: %s", msg)
	}
}

// (6) Merge keys are validated against the destination struct's
// allowlist — a typo behind an anchor still fails.
func TestLoad_unknownKey_mergeKey(t *testing.T) {
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
  bogusKey: 1
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
httpClient: { userAgent: "x" }
slack:
  bodyMaxChars: 200
  channels:
    - { slug: ops-alerts, channelId: C0123ABCD, tokenEnv: SLACK_BOT_TOKEN }
monitors:
  - <<: *staticDefaults
    slug: a
    friendlyName: A
    url: http://a/health
    tags: [gw]
`
	_, err := config.Load([]byte(yaml))
	if err == nil {
		t.Fatal("expected merged-in unknown key to be rejected")
	}
	msg := err.Error()
	if !strings.Contains(msg, `unknown key "bogusKey"`) {
		t.Errorf("error should name the unknown key carried via merge, got: %s", msg)
	}
}

// (7) x-* escape hatch is honoured only at the top level — a nested
// x-foo: is still a typo.
func TestLoad_unknownKey_xPrefixNestedFails(t *testing.T) {
	data := withReplaced(t, "    slack: ops-alerts",
		"    slack: ops-alerts\n    x-note: \"nested anchors not allowed\"")
	_, err := config.Load(data)
	if err == nil {
		t.Fatal("expected nested x-* key to be rejected (top-level only)")
	}
	msg := err.Error()
	if !strings.Contains(msg, `unknown key "x-note"`) {
		t.Errorf("error should name the unknown key, got: %s", msg)
	}
}

// (8a) User-keyed maps (Labels) accept arbitrary keys — no false
// positive from the walker.
func TestLoad_unknownKey_labelsAcceptArbitraryKeys(t *testing.T) {
	tree := kubeRootBaseline + `    - when:
        labels:
          app.kubernetes.io/name: anything
          weird.example.com/key: also-fine
`
	if _, err := config.Load(withKubeBlock(tree)); err != nil {
		t.Fatalf("k8s label keys must not be flagged as unknown, got: %v", err)
	}
}

// (8b) UserMapping is also user-keyed — arbitrary slug keys are valid.
func TestLoad_unknownKey_userMappingAcceptsArbitraryKeys(t *testing.T) {
	data := strings.Replace(validMinimal,
		"      tokenEnv: SLACK_BOT_TOKEN\n",
		"      tokenEnv: SLACK_BOT_TOKEN\n  userMapping:\n    arbitrary-slug: U01ABCDEF12\n    another-one: S02GHIJKL34\n",
		1)
	if _, err := config.Load([]byte(data)); err != nil {
		t.Fatalf("userMapping keys must not be flagged as unknown, got: %v", err)
	}
}

// (9) errf now includes column numbers — assert the prefix shape on a
// known-failing fixture. Other tests deliberately rely only on path +
// message contents so they don't churn when prefix formatting changes.
func TestLoad_errorsCarryLineAndColumn(t *testing.T) {
	tree := kubeRootBaseline + `    - when: { namespace: "toggle-*" }
      nestedd: []
`
	_, err := config.Load(withKubeBlock(tree))
	if err == nil {
		t.Fatal("expected unknown-key error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "col ") {
		t.Errorf("expected error prefix to include column, got: %s", msg)
	}
	if !strings.Contains(msg, "line ") {
		t.Errorf("expected error prefix to include line, got: %s", msg)
	}
}

// ── slack.coalesce: burst-dispatch knobs (ADR-0004) ──────────────────────────
//
// The 2026-05-27 design (always-coalesce digest) is superseded: per-channel
// dispatch is now three-state (individual / pending / group), with the pool
// promoting to a group only when ≥ burstThreshold failures land within the
// pendingWait window. groupWait is accepted as a deprecated alias for one
// release.

// withCoalesce splices a `coalesce:` sub-block into validMinimal under
// the `slack:` key. `body` is the field listing without a trailing
// newline; multi-line bodies join with "\n    " to preserve indentation.
func withCoalesce(body string) []byte {
	insert := "  coalesce:\n    " + body + "\n"
	return []byte(strings.Replace(validMinimal,
		"  channels:",
		insert+"  channels:", 1))
}

func TestLoad_coalesce_pendingWaitParses(t *testing.T) {
	cfg, err := config.Load(withCoalesce("pendingWait: 45s"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Slack.Coalesce.PendingWait.AsDuration(); got != 45*time.Second {
		t.Errorf("PendingWait: got %s, want 45s", got)
	}
}

func TestLoad_coalesce_groupWaitAliasStillAccepted(t *testing.T) {
	cfg, err := config.Load(withCoalesce("groupWait: 45s"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// EffectivePendingWait collapses the alias.
	if got := cfg.Slack.Coalesce.EffectivePendingWait(); got != 45*time.Second {
		t.Errorf("EffectivePendingWait via groupWait alias: got %s, want 45s", got)
	}
}

func TestLoad_coalesce_rejectsBothPendingWaitAndGroupWait(t *testing.T) {
	_, err := config.Load(withCoalesce("pendingWait: 45s\n    groupWait: 30s"))
	if err == nil {
		t.Fatal("expected error when both pendingWait and groupWait set")
	}
	if !strings.Contains(err.Error(), "pendingWait") || !strings.Contains(err.Error(), "groupWait") {
		t.Errorf("error should name both fields, got: %v", err)
	}
}

func TestLoad_coalesce_burstThresholdParses(t *testing.T) {
	cfg, err := config.Load(withCoalesce("burstThreshold: 7"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Slack.Coalesce.BurstThreshold == nil {
		t.Fatal("BurstThreshold: nil, want pointer to 7")
	}
	if *cfg.Slack.Coalesce.BurstThreshold != 7 {
		t.Errorf("BurstThreshold: got %d, want 7", *cfg.Slack.Coalesce.BurstThreshold)
	}
}

func TestLoad_coalesce_burstThresholdZeroDisables(t *testing.T) {
	cfg, err := config.Load(withCoalesce("burstThreshold: 0"))
	if err != nil {
		t.Fatalf("burstThreshold: 0 must parse cleanly (disables group-mode): %v", err)
	}
	if cfg.Slack.Coalesce.BurstThreshold == nil || *cfg.Slack.Coalesce.BurstThreshold != 0 {
		t.Errorf("BurstThreshold: want *0, got %v", cfg.Slack.Coalesce.BurstThreshold)
	}
}

func TestLoad_coalesce_rejectsBurstThresholdOne(t *testing.T) {
	_, err := config.Load(withCoalesce("burstThreshold: 1"))
	if err == nil {
		t.Fatal("expected error for burstThreshold: 1 (pathological — trips on any single failure)")
	}
	if !strings.Contains(err.Error(), "burstThreshold") {
		t.Errorf("error should name burstThreshold, got: %v", err)
	}
}

func TestLoad_coalesce_groupMentionAcceptsKnownValues(t *testing.T) {
	for _, v := range []string{"channel", "here", "none"} {
		cfg, err := config.Load(withCoalesce("groupMention: " + v))
		if err != nil {
			t.Errorf("groupMention: %s: unexpected error: %v", v, err)
			continue
		}
		if cfg.Slack.Coalesce.GroupMention != v {
			t.Errorf("groupMention %s: got %q", v, cfg.Slack.Coalesce.GroupMention)
		}
	}
}

func TestLoad_coalesce_rejectsUnknownGroupMention(t *testing.T) {
	_, err := config.Load(withCoalesce("groupMention: everyone"))
	if err == nil {
		t.Fatal("expected error for unknown groupMention")
	}
	if !strings.Contains(err.Error(), "groupMention") {
		t.Errorf("error should name groupMention, got: %v", err)
	}
}

func TestLoad_coalesce_onDemandProbeTimeoutParses(t *testing.T) {
	cfg, err := config.Load(withCoalesce("onDemandProbeTimeout: 3s"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Slack.Coalesce.OnDemandProbeTimeout.AsDuration(); got != 3*time.Second {
		t.Errorf("OnDemandProbeTimeout: got %s, want 3s", got)
	}
}

func TestLoad_coalesce_defaultsWhenOmitted(t *testing.T) {
	cfg, err := config.Load([]byte(validMinimal))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Slack.Coalesce.EffectivePendingWait(); got != 30*time.Second {
		t.Errorf("EffectivePendingWait default: got %s, want 30s", got)
	}
	if got := cfg.Slack.Coalesce.EffectiveBurstThreshold(); got != 5 {
		t.Errorf("EffectiveBurstThreshold default: got %d, want 5", got)
	}
	if got := cfg.Slack.Coalesce.EffectiveGroupMention(); got != "channel" {
		t.Errorf("EffectiveGroupMention default: got %q, want channel", got)
	}
	if got := cfg.Slack.Coalesce.EffectiveOnDemandProbeTimeout(); got != 5*time.Second {
		t.Errorf("EffectiveOnDemandProbeTimeout default: got %s, want 5s", got)
	}
}
