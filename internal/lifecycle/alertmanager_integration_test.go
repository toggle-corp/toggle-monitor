//go:build integration

package lifecycle_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toggle-corp/toggle-monitor/internal/config"
	"github.com/toggle-corp/toggle-monitor/internal/lifecycle"
	"github.com/toggle-corp/toggle-monitor/internal/migrate"
	"github.com/toggle-corp/toggle-monitor/internal/store"
	"github.com/toggle-corp/toggle-monitor/internal/testpg"
)

// TestRunServe_alertmanagerWebhookEndToEnd is the load-bearing
// regression test for ADR-0005 Slice 8. It boots the full lifecycle
// against a real Postgres + a fake Slack and drives the end-to-end
// Alertmanager flow:
//
//  1. firing → expect 200, am_alerts row, am_alert_events row,
//     chat.postMessage call against the fake Slack.
//  2. resolved → expect ended_at stamp, chat.update on the parent,
//     chat.postMessage thread reply.
//  3. late resolve (resolve for an unseen fingerprint) → expect a
//     standalone late-resolve post (banner) and no DB row.
//
// If lifecycle ever ships without the AM handler wired into the
// listener (the ADR-0004-style "wired but never called" bug noted in
// CLAUDE.md), this test fails at step 1 because the request 404s and
// no row lands in am_alerts.
func TestRunServe_alertmanagerWebhookEndToEnd(t *testing.T) {
	// 1. Real Postgres + migrations.
	dsn := testpg.Start(t)
	if err := migrate.Up(dsn); err != nil {
		t.Fatalf("migrate.Up: %v", err)
	}
	dbCfg, err := dbConfigFromDSN(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}

	// 2. Fake Slack.
	recorder := &fakeSlackRecorder{}
	slackSrv := httptest.NewServer(recorder.handler())
	t.Cleanup(slackSrv.Close)
	t.Setenv("TOGGLE_SLACK_TOKEN", "xoxb-test")
	t.Setenv("ALERTMANAGER_WEBHOOK_TOKEN", "am-bearer-token")

	// 3. Config with an alertmanager block routing every alert to
	// ops-alerts and ignoring Watchdog. PerChannel rate-limit is set
	// high so the test never trips the flood notice.
	yaml := fmt.Sprintf(`
displayTimezone: UTC
dbBodyMaxChars: 4000
publicBaseURL: https://monitor.example.test
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
httpClient: { userAgent: "toggle-monitor/it" }
slack:
  bodyMaxChars: 200
  channels:
    - { slug: ops-alerts, channelId: C012345678, tokenEnv: TOGGLE_SLACK_TOKEN }
  userMapping:
    ops-team: SAMTEAM01
alertmanager:
  endpoint:
    path: /webhooks/alertmanager
    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN
  retentionDays: 180
  rateLimit:
    perChannel: 1000
    window: 30m
    noticeEvery: 1d
  match:
    - config:
        slack: ops-alerts
        notify: [ops-team]
    - when: { alertname: "Watchdog" }
      ignore: true
      final: true
`,
		dbCfg.Host, dbCfg.Port, dbCfg.User, dbCfg.Name, dbCfg.SSLMode,
	)

	cfg, err := config.Load([]byte(yaml))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	// 4. Start the binary.
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
	base := "http://" + addr.String()

	// 5. POST a firing webhook.
	fp := "fp-cpu-burning"
	firingBody := webhookBody(fp, "firing", "2026-06-04T11:55:00Z", "0001-01-01T00:00:00Z")
	resp := postAMWebhook(t, base+"/webhooks/alertmanager", "am-bearer-token", firingBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("firing POST: got %d, want 200 (the AM handler was probably never wired into the mux)", resp.StatusCode)
	}

	// 5a. Assert chat.postMessage fired.
	waitForPosts(t, recorder, 1, "firing post")

	// 5b. Assert the DB has the expected rows.
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	repo := store.New(pool)

	var inc *store.AMIncident
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		incs, err := repo.ListAMIncidentsByFingerprint(ctx, fp, 5)
		if err != nil {
			t.Fatalf("ListAMIncidentsByFingerprint: %v", err)
		}
		if len(incs) > 0 {
			inc = &incs[0]
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if inc == nil {
		t.Fatal("am_alerts row never landed after firing webhook")
	}
	if inc.SlackTS == "" {
		t.Errorf("am_alerts.slack_ts: got empty, want a recorded ts (handler didn't write back after Slack post)")
	}
	if inc.EndedAt.Valid {
		t.Errorf("am_alerts.ended_at on firing: got %v, want NULL", inc.EndedAt.Time)
	}

	// 6. POST the matching resolved webhook.
	resolveBody := webhookBody(fp, "resolved", "2026-06-04T11:55:00Z", "2026-06-04T12:05:00Z")
	resp = postAMWebhook(t, base+"/webhooks/alertmanager", "am-bearer-token", resolveBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resolved POST: got %d, want 200", resp.StatusCode)
	}

	// 6a. Parent edit + thread reply.
	waitForUpdates(t, recorder, 1, "resolve edit")
	waitForPosts(t, recorder, 2, "resolve thread reply")

	// 6b. DB row's ended_at is stamped.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		got, err := repo.GetAMIncident(ctx, inc.ID)
		if err != nil {
			t.Fatalf("GetAMIncident: %v", err)
		}
		if got.EndedAt.Valid {
			inc = got
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !inc.EndedAt.Valid {
		t.Fatal("am_alerts.ended_at never stamped after resolve webhook")
	}

	// 7. Late-resolve: post a resolved for a never-seen fingerprint.
	lateFP := "fp-late-only"
	postsBefore := postCount(recorder)
	lateBody := webhookBody(lateFP, "resolved", "2026-06-04T11:00:00Z", "2026-06-04T11:01:00Z")
	resp = postAMWebhook(t, base+"/webhooks/alertmanager", "am-bearer-token", lateBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("late resolve POST: got %d, want 200", resp.StatusCode)
	}
	waitForPosts(t, recorder, postsBefore+1, "late-resolve standalone post")

	// 7a. No am_alerts row for the late-only fingerprint.
	lateIncs, err := repo.ListAMIncidentsByFingerprint(ctx, lateFP, 5)
	if err != nil {
		t.Fatalf("ListAMIncidentsByFingerprint(late): %v", err)
	}
	if len(lateIncs) != 0 {
		t.Errorf("late-resolve persisted a row: got %d rows, want 0", len(lateIncs))
	}

	// 8. Shutdown.
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

// webhookBody builds a minimal AM-v4 envelope with a single alert at
// the given status. status: "firing" → endsAt is the zero-time
// placeholder; "resolved" → endsAt is the real resolve time.
func webhookBody(fingerprint, status, startsAt, endsAt string) []byte {
	body := map[string]any{
		"version":     "4",
		"groupKey":    "{}:{alertname=\"HighCPU\"}",
		"status":      status,
		"receiver":    "toggle_monitor",
		"externalURL": "https://am.example.test",
		"alerts": []map[string]any{
			{
				"status": status,
				"labels": map[string]string{
					"alertname": "HighCPU",
					"severity":  "critical",
					"instance":  "pod-1",
				},
				"annotations": map[string]string{
					"summary":     "CPU is on fire",
					"runbook_url": "https://runbooks.example.test/cpu",
				},
				"startsAt":    startsAt,
				"endsAt":      endsAt,
				"fingerprint": fingerprint,
			},
		},
	}
	b, _ := json.Marshal(body)
	return b
}

// postAMWebhook POSTs an AM payload with the Bearer header and
// returns the response (body drained, ready to read .StatusCode).
func postAMWebhook(t *testing.T, url, token string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	resp.Body.Close()
	return resp
}

func waitForPosts(t *testing.T, r *fakeSlackRecorder, want int, label string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		got := len(r.postMessages)
		r.mu.Unlock()
		if got >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	r.mu.Lock()
	got := len(r.postMessages)
	r.mu.Unlock()
	t.Fatalf("%s: got %d posts, want >= %d", label, got, want)
}

func waitForUpdates(t *testing.T, r *fakeSlackRecorder, want int, label string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		got := len(r.updateMessages)
		r.mu.Unlock()
		if got >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	r.mu.Lock()
	got := len(r.updateMessages)
	r.mu.Unlock()
	t.Fatalf("%s: got %d updates, want >= %d", label, got, want)
}

func postCount(r *fakeSlackRecorder) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.postMessages)
}
