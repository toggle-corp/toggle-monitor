//go:build integration

package lifecycle_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toggle-corp/toggle-monitor/internal/config"
	"github.com/toggle-corp/toggle-monitor/internal/lifecycle"
	"github.com/toggle-corp/toggle-monitor/internal/migrate"
	"github.com/toggle-corp/toggle-monitor/internal/testpg"
)

// TestRunServe_sigtermMidCheck_doesNotRecordFailure asserts the
// Issue-16 invariant: when the parent context is cancelled while a
// probe is in-flight, the scheduler must NOT write an alert_event row
// for that cancelled check and the final heartbeat must still go out.
func TestRunServe_sigtermMidCheck_doesNotRecordFailure(t *testing.T) {
	// Slow upstream that holds the request open until the test
	// releases it (we cancel ctx instead, simulating SIGTERM).
	hold := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hold:
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(func() { close(hold) })

	// Fake Slack + final-heartbeat recorder.
	var shutdownEvents int
	var mu sync.Mutex
	deadman := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		if strings.Contains(string(raw), `"event":"shutdown"`) {
			shutdownEvents++
		}
		mu.Unlock()
		w.WriteHeader(200)
	}))
	t.Cleanup(deadman.Close)
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok": true, "team_id": "T1", "ts": "1.0", "channel": "C0"}`))
	}))
	t.Cleanup(slackSrv.Close)

	dsn := testpg.Start(t)
	if err := migrate.Up(dsn); err != nil {
		t.Fatalf("migrate.Up: %v", err)
	}
	dbCfg, err := dbConfigFromDSN(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}

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
httpClient: { userAgent: shutdown-test }
heartbeat:
  url: %s
  interval: 30s
  failOnStalledWorker: false
slack:
  bodyMaxChars: 200
  channels:
    - { slug: ops-alerts, channelId: C0123ABCD, tokenEnv: TOGGLE_SLACK_TOKEN }
monitors:
  - slug: slow
    friendlyName: Slow
    url: %s
    tags: [prod]
    httpMethod: GET
    acceptedStatusCodes: [200]
    interval: 5m
    timeout: 8s
    retries: 0
    retryBackoff: 1s
    followRedirects: false
    reminderInterval: 3d
    slack: ops-alerts
`,
		dbCfg.Host, dbCfg.Port, dbCfg.User, dbCfg.Name, dbCfg.SSLMode,
		deadman.URL, upstream.URL,
	)

	cfg, err := config.Load([]byte(yaml))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	t.Setenv("TOGGLE_DB_PASSWORD", dbCfg.Password)
	t.Setenv("TOGGLE_SLACK_TOKEN", "xoxb-test")

	addrCh := make(chan net.Addr, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- lifecycle.RunServe(ctx, lifecycle.ServeOptions{
			Config: cfg, DBConfig: dbCfg, ListenAddr: "127.0.0.1:0",
			SlackBaseURL: slackSrv.URL,
			OnReady:      func(a net.Addr) { addrCh <- a },
		})
	}()
	select {
	case <-addrCh:
	case <-time.After(15 * time.Second):
		t.Fatal("RunServe never bound")
	}

	// Give the scheduler a moment to dispatch the slow probe before
	// we cancel.
	time.Sleep(400 * time.Millisecond)
	cancel()

	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("RunServe error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("RunServe did not exit within grace period")
	}

	// No alert_event rows should have been written for the cancelled
	// in-flight probe.
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("post-shutdown pool: %v", err)
	}
	t.Cleanup(pool.Close)
	row := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM alert_events WHERE monitor_slug = 'slow'`)
	var n int
	if err := row.Scan(&n); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if n != 0 {
		t.Errorf("expected zero alert_events for cancelled check, got %d", n)
	}

	mu.Lock()
	defer mu.Unlock()
	if shutdownEvents < 1 {
		t.Errorf("expected at least one final shutdown heartbeat POST, got %d", shutdownEvents)
	}
}
