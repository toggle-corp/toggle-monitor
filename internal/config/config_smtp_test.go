package config_test

import (
	"strings"
	"testing"

	"github.com/toggle-corp/toggle-monitor/internal/config"
)

// smtpBase is a minimal valid config with no HTTP monitors; tests
// append an smtpMonitors block (and optionally an http monitor) via
// fmt-free string concatenation.
const smtpBase = `
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

func TestSMTP_validStartTLS(t *testing.T) {
	yaml := smtpBase + `
smtpMonitors:
  - slug: mail-relay
    friendlyName: Mail Relay
    host: smtp.example.test
    port: 2525
    tls: starttls
    interval: 5m
    timeout: 10s
    retries: 1
    retryBackoff: 5s
    reminderInterval: 3d
    slack: ops-alerts
    sslAlertThreshold: 14d
    sslEscalationThreshold: 3d
    sslReminderInterval: 24h
`
	cfg, err := config.Load([]byte(yaml))
	if err != nil {
		t.Fatalf("expected valid config, got: %v", err)
	}
	if len(cfg.SMTPMonitors) != 1 {
		t.Fatalf("smtpMonitors: got %d, want 1", len(cfg.SMTPMonitors))
	}
	m := cfg.SMTPMonitors[0]
	if got := m.URL(); got != "smtp://smtp.example.test:2525" {
		t.Errorf("URL(): got %q", got)
	}
	if !m.TracksSSL() {
		t.Error("starttls monitor should track SSL")
	}
}

func TestSMTP_tlsDefaultsToStartTLS(t *testing.T) {
	yaml := smtpBase + `
smtpMonitors:
  - slug: mail-relay
    friendlyName: Mail Relay
    host: smtp.example.test
    port: 587
    interval: 5m
    timeout: 10s
    retries: 0
    retryBackoff: 5s
    reminderInterval: 3d
    slack: ops-alerts
    sslAlertThreshold: 14d
    sslEscalationThreshold: 3d
    sslReminderInterval: 24h
`
	cfg, err := config.Load([]byte(yaml))
	if err != nil {
		t.Fatalf("expected valid config, got: %v", err)
	}
	if got := cfg.SMTPMonitors[0].TLSMode(); got != config.SMTPTLSStartTLS {
		t.Errorf("default TLS mode: got %q, want starttls", got)
	}
}

func TestSMTP_noneSkipsSSLThresholds(t *testing.T) {
	yaml := smtpBase + `
smtpMonitors:
  - slug: mail-relay
    friendlyName: Mail Relay
    host: smtp.example.test
    port: 25
    tls: none
    interval: 5m
    timeout: 10s
    retries: 0
    retryBackoff: 5s
    reminderInterval: 3d
    slack: ops-alerts
`
	if _, err := config.Load([]byte(yaml)); err != nil {
		t.Fatalf("tls:none should not require SSL thresholds, got: %v", err)
	}
}

func TestSMTP_errors(t *testing.T) {
	cases := []struct {
		name    string
		block   string
		wantSub string
	}{
		{
			name: "missing host",
			block: `
  - slug: mr
    friendlyName: MR
    port: 25
    tls: none
    interval: 5m
    timeout: 10s
    retries: 0
    retryBackoff: 5s
    reminderInterval: 3d
    slack: ops-alerts`,
			wantSub: "host",
		},
		{
			name: "bad tls value",
			block: `
  - slug: mr
    friendlyName: MR
    host: smtp.example.test
    port: 25
    tls: bogus
    interval: 5m
    timeout: 10s
    retries: 0
    retryBackoff: 5s
    reminderInterval: 3d
    slack: ops-alerts`,
			wantSub: "tls",
		},
		{
			name: "starttls missing ssl thresholds",
			block: `
  - slug: mr
    friendlyName: MR
    host: smtp.example.test
    port: 587
    tls: starttls
    interval: 5m
    timeout: 10s
    retries: 0
    retryBackoff: 5s
    reminderInterval: 3d
    slack: ops-alerts`,
			wantSub: "sslAlertThreshold",
		},
		{
			name: "unknown slack channel",
			block: `
  - slug: mr
    friendlyName: MR
    host: smtp.example.test
    port: 25
    tls: none
    interval: 5m
    timeout: 10s
    retries: 0
    retryBackoff: 5s
    reminderInterval: 3d
    slack: nope`,
			wantSub: "unknown channel",
		},
		{
			name: "unknown dependsOn",
			block: `
  - slug: mr
    friendlyName: MR
    host: smtp.example.test
    port: 25
    tls: none
    interval: 5m
    timeout: 10s
    retries: 0
    retryBackoff: 5s
    reminderInterval: 3d
    slack: ops-alerts
    dependsOn: [ghost]`,
			wantSub: "unknown monitor slug",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Load([]byte(smtpBase + "\nsmtpMonitors:" + tc.block + "\n"))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestSMTP_sharedSlugNamespace: an SMTP monitor and an HTTP monitor
// cannot share a slug.
func TestSMTP_sharedSlugNamespace(t *testing.T) {
	yaml := strings.Replace(smtpBase, "monitors: []", `monitors:
  - slug: dup
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
    sslAlertThreshold: 14d
    sslEscalationThreshold: 3d
    sslReminderInterval: 24h`, 1) + `
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
`
	_, err := config.Load([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "duplicate slug") {
		t.Fatalf("expected duplicate slug error, got: %v", err)
	}
}

// TestSMTP_dependsOnHTTPParent: an SMTP monitor may depend on a declared
// HTTP monitor (shared dependsOn graph).
func TestSMTP_dependsOnHTTPParent(t *testing.T) {
	yaml := strings.Replace(smtpBase, "monitors: []", `monitors:
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
    sslAlertThreshold: 14d
    sslEscalationThreshold: 3d
    sslReminderInterval: 24h`, 1) + `
smtpMonitors:
  - slug: mail
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
    dependsOn: [web]
`
	if _, err := config.Load([]byte(yaml)); err != nil {
		t.Fatalf("smtp dependsOn http parent should be valid, got: %v", err)
	}
}
