//go:build integration

package lifecycle_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/config"
	"github.com/toggle-corp/toggle-monitor/internal/db"
	"github.com/toggle-corp/toggle-monitor/internal/lifecycle"
	"github.com/toggle-corp/toggle-monitor/internal/migrate"
	"github.com/toggle-corp/toggle-monitor/internal/testpg"
)

// TestRunServe_endToEndTracerBullet exercises the full Issue 2 flow:
// load YAML config → connect to Postgres → check schema version →
// reconcile monitors → start scheduler + HTTP server → observe the
// scheduler hit the upstream service and the UI reflect it.
//
// This is the integration test required by Issue 2's acceptance
// criteria: "Integration test covers the full path: YAML → check →
// DB → UI."
func TestRunServe_endToEndTracerBullet(t *testing.T) {
	// 1. Upstream service the monitor will probe. We flip its behavior
	// from 500 to 200 after a few hits so the integration covers both
	// the down and the resolve transitions.
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	// 2. Real Postgres + migrations.
	dsn := testpg.Start(t)
	if err := migrate.Up(dsn); err != nil {
		t.Fatalf("migrate.Up: %v", err)
	}
	dbCfg, err := dbConfigFromDSN(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}

	// 3. YAML config — minimal Issue-2 schema with a very small
	// interval so the test completes quickly. The retries gate forces
	// `retries × (timeout + retryBackoff) < interval`, so we set
	// retries=0 to keep the math trivial.
	yaml := fmt.Sprintf(`
displayTimezone: UTC
dbBodyMaxChars: 4000
database:
  host: %s
  port: %d
  user: %s
  name: %s
  sslMode: %s
  passwordEnv: TOGGLE_DB_PASSWORD
ui:
  pageSize: { homepageAlerts: 20, monitorListing: 50, monitorHistory: 50, discoveryListing: 50 }
  maxPerPage: 200
theme: { defaultGroupColor: "#64748b" }
httpClient: { userAgent: "toggle-monitor/it" }
slack:
  bodyMaxChars: 200
  channels:
    - { slug: ops-alerts, channelId: C0123ABCD, tokenEnv: TOGGLE_SLACK_TOKEN }
groups:
  - { slug: kube-discovered, friendlyName: Kube Discovered }
  - { slug: prod, friendlyName: Prod }
monitors:
  - slug: api
    friendlyName: API
    url: %s
    group: prod
    httpMethod: GET
    acceptedStatusCodes: [200]
    interval: 200ms
    timeout: 100ms
    retries: 0
    retryBackoff: 1s
    followRedirects: false
    reminderInterval: 3d
    slack: ops-alerts
`,
		dbCfg.Host, dbCfg.Port, dbCfg.User, dbCfg.Name, dbCfg.SSLMode,
		upstream.URL,
	)

	cfg, err := config.Load([]byte(yaml))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	// Issue 3 wires Slack into serve; this test isn't about Slack
	// behavior but the YAML still requires a slack: block, so point
	// it at an always-OK fake server and set a stub token.
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok": true, "team_id": "T1", "ts": "1.0", "channel": "C0"}`))
	}))
	t.Cleanup(slackSrv.Close)
	t.Setenv("TOGGLE_SLACK_TOKEN", "xoxb-test")

	// Listen on :0 so the test grabs a free port; capture the bound
	// address via the OnReady callback.
	addrCh := make(chan net.Addr, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- lifecycle.RunServe(ctx, lifecycle.ServeOptions{
			Config:       cfg,
			DBConfig:     dbCfg,
			ListenAddr:   "127.0.0.1:0",
			SlackBaseURL: slackSrv.URL,
			OnReady:      func(a net.Addr) { addrCh <- a },
		})
	}()

	var addr net.Addr
	select {
	case addr = <-addrCh:
	case <-time.After(20 * time.Second):
		t.Fatal("RunServe never bound a listener")
	}

	// 4. Wait until the scheduler has observed both a down and a
	// resolve transition (visible via the /monitor/api detail page).
	base := "http://" + addr.String()
	deadline := time.Now().Add(15 * time.Second)
	var lastBody string
	for {
		body := mustGet(t, base+"/monitor/api")
		lastBody = body
		if strings.Contains(body, "UP") && strings.Contains(body, "open") && strings.Contains(body, "resolve") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for full uptime lifecycle on /monitor/api; last body excerpt:\n%s", firstN(lastBody, 600))
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 5. Probe /healthz and /readyz.
	if got := mustStatus(t, base+"/healthz"); got != 200 {
		t.Errorf("/healthz: got %d, want 200", got)
	}
	if got := mustStatus(t, base+"/readyz"); got != 200 {
		t.Errorf("/readyz after MarkReady: got %d, want 200", got)
	}

	// 6. Shut down. RunServe should return nil within the grace period.
	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("RunServe returned error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("RunServe did not exit within grace period")
	}
}

// dbConfigFromDSN extracts a db.Config from the testcontainers DSN
// (postgres://USER:PASS@HOST:PORT/DB?sslmode=...).
func dbConfigFromDSN(dsn string) (db.Config, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return db.Config{}, fmt.Errorf("parse dsn: %w", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return db.Config{}, fmt.Errorf("parse port: %w", err)
	}
	pw, _ := u.User.Password()
	sslMode := u.Query().Get("sslmode")
	if sslMode == "" {
		sslMode = "disable"
	}
	return db.Config{
		Host:     u.Hostname(),
		Port:     port,
		User:     u.User.Username(),
		Password: pw,
		Name:     strings.TrimPrefix(u.Path, "/"),
		SSLMode:  sslMode,
	}, nil
}

func mustGet(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func mustStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
