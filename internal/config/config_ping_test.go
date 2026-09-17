package config_test

import (
	"strings"
	"testing"

	"github.com/toggle-corp/toggle-monitor/internal/config"
)

// pingBase is a minimal valid config with no HTTP monitors; tests
// append a pingMonitors block.
const pingBase = `
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
monitors: []
`

func TestPing_valid(t *testing.T) {
	yaml := pingBase + `
pingMonitors:
  - slug: edge-router
    friendlyName: Edge Router
    host: router.example.test
    interval: 1m
    timeout: 5s
    retries: 2
    retryBackoff: 2s
    reminderInterval: 3d
    slack: ops-alerts
    tags: [network]
`
	cfg, err := config.Load([]byte(yaml))
	if err != nil {
		t.Fatalf("expected valid config, got: %v", err)
	}
	if len(cfg.PingMonitors) != 1 {
		t.Fatalf("pingMonitors: got %d, want 1", len(cfg.PingMonitors))
	}
	if got := cfg.PingMonitors[0].URL(); got != "ping://router.example.test" {
		t.Errorf("URL(): got %q", got)
	}
}

// A bare IP is a legitimate ping target — devices without DNS names are
// most of the reason this monitor kind exists.
func TestPing_literalIPHost(t *testing.T) {
	yaml := pingBase + `
pingMonitors:
  - slug: gateway
    friendlyName: Gateway
    host: 192.0.2.1
    interval: 1m
    timeout: 5s
    retries: 0
    retryBackoff: 2s
    reminderInterval: 3d
    slack: ops-alerts
`
	if _, err := config.Load([]byte(yaml)); err != nil {
		t.Fatalf("literal IP host should be valid, got: %v", err)
	}
}

func TestPing_errors(t *testing.T) {
	cases := []struct {
		name    string
		block   string
		wantSub string
	}{
		{
			name: "missing host",
			block: `
  - slug: rtr
    friendlyName: RTR
    interval: 1m
    timeout: 5s
    retries: 0
    retryBackoff: 2s
    reminderInterval: 3d
    slack: ops-alerts`,
			wantSub: "host",
		},
		{
			name: "host carries a URL",
			block: `
  - slug: rtr
    friendlyName: RTR
    host: https://router.example.test/
    interval: 1m
    timeout: 5s
    retries: 0
    retryBackoff: 2s
    reminderInterval: 3d
    slack: ops-alerts`,
			wantSub: "bare hostname or IP",
		},
		{
			name: "host carries a port",
			block: `
  - slug: rtr
    friendlyName: RTR
    host: router.example.test:22
    interval: 1m
    timeout: 5s
    retries: 0
    retryBackoff: 2s
    reminderInterval: 3d
    slack: ops-alerts`,
			wantSub: "bare hostname or IP",
		},
		{
			// ICMP can't be routed through SOCKS5, so the field must not
			// silently exist and be ignored.
			name: "proxy is not a ping field",
			block: `
  - slug: rtr
    friendlyName: RTR
    host: router.example.test
    interval: 1m
    timeout: 5s
    retries: 0
    retryBackoff: 2s
    reminderInterval: 3d
    slack: ops-alerts
    proxy: egress`,
			wantSub: "proxy",
		},
		{
			name: "timeout not less than interval",
			block: `
  - slug: rtr
    friendlyName: RTR
    host: router.example.test
    interval: 5s
    timeout: 5s
    retries: 0
    retryBackoff: 2s
    reminderInterval: 3d
    slack: ops-alerts`,
			wantSub: "must be less than interval",
		},
		{
			name: "unknown slack channel",
			block: `
  - slug: rtr
    friendlyName: RTR
    host: router.example.test
    interval: 1m
    timeout: 5s
    retries: 0
    retryBackoff: 2s
    reminderInterval: 3d
    slack: nope`,
			wantSub: "unknown channel",
		},
		{
			name: "unknown dependsOn",
			block: `
  - slug: rtr
    friendlyName: RTR
    host: router.example.test
    interval: 1m
    timeout: 5s
    retries: 0
    retryBackoff: 2s
    reminderInterval: 3d
    slack: ops-alerts
    dependsOn: [ghost]`,
			wantSub: "unknown monitor slug",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Load([]byte(pingBase + "\npingMonitors:" + tc.block + "\n"))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// Ping monitors share one slug namespace with HTTP and SMTP monitors.
func TestPing_sharedSlugNamespace(t *testing.T) {
	yaml := pingBase + `
smtpMonitors:
  - slug: dup
    friendlyName: Mail
    host: smtp.example.test
    port: 25
    tls: none
    interval: 5m
    timeout: 10s
    retries: 0
    retryBackoff: 5s
    reminderInterval: 3d
    slack: ops-alerts
pingMonitors:
  - slug: dup
    friendlyName: Router
    host: router.example.test
    interval: 1m
    timeout: 5s
    retries: 0
    retryBackoff: 2s
    reminderInterval: 3d
    slack: ops-alerts
`
	_, err := config.Load([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "duplicate slug") {
		t.Fatalf("expected duplicate slug error, got: %v", err)
	}
}

// A ping monitor is a plausible dependsOn parent for an HTTP monitor:
// "the host is unreachable" explains the web check failing.
func TestPing_isAValidDependsOnParent(t *testing.T) {
	yaml := strings.Replace(pingBase, "monitors: []", `monitors:
  - slug: web
    friendlyName: Web
    url: https://web.example.test/health
    httpMethod: GET
    acceptedStatusCodes: [200]
    interval: 5m
    timeout: 10s
    retries: 0
    retryBackoff: 5s
    reminderInterval: 3d
    slack: ops-alerts
    dependsOn: [host-up]
    sslAlertThreshold: 14d
    sslEscalationThreshold: 3d
    sslReminderInterval: 24h`, 1) + `
pingMonitors:
  - slug: host-up
    friendlyName: Host
    host: web.example.test
    interval: 1m
    timeout: 5s
    retries: 0
    retryBackoff: 2s
    reminderInterval: 3d
    slack: ops-alerts
`
	if _, err := config.Load([]byte(yaml)); err != nil {
		t.Fatalf("http dependsOn ping parent should be valid, got: %v", err)
	}
}
