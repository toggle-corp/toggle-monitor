//go:build integration

package lifecycle_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/config"
	"github.com/toggle-corp/toggle-monitor/internal/lifecycle"
	"github.com/toggle-corp/toggle-monitor/internal/migrate"
	"github.com/toggle-corp/toggle-monitor/internal/testpg"
)

// fakeSlackRecorder is a minimal httptest harness for the Slack Web
// API: auth.test, chat.postMessage, chat.update.
type fakeSlackRecorder struct {
	mu sync.Mutex

	authCalls       int
	postMessages    []map[string]any
	updateMessages  []map[string]any

	nextTS atomic.Int64
}

func (f *fakeSlackRecorder) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		body := map[string]any{}
		if r.ContentLength != 0 {
			raw, _ := io.ReadAll(r.Body)
			if len(raw) > 0 {
				_ = json.Unmarshal(raw, &body)
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch method {
		case "auth.test":
			f.authCalls++
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok": true, "team_id": "T123", "team": "TestCorp"}`))
		case "chat.postMessage":
			f.postMessages = append(f.postMessages, body)
			ts := fmt.Sprintf("17000000%02d.%06d", len(f.postMessages), f.nextTS.Add(1))
			channel, _ := body["channel"].(string)
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok": true, "ts": "` + ts + `", "channel": "` + channel + `"}`))
		case "chat.update":
			f.updateMessages = append(f.updateMessages, body)
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok": true}`))
		default:
			http.NotFound(w, r)
		}
	})
}

// TestRunServe_slackLifecycle_postsParentReminderUpdateAndReply drives
// a monitor through down → reminder → up while a fake Slack records
// every call. Asserts the parent ↔ thread ref handoff, the absence of
// mentions on reminders/resolves, the parent edit on resolve, and the
// thread reply on resolve.
func TestRunServe_slackLifecycle_postsParentReminderUpdateAndReply(t *testing.T) {
	// Upstream service: stays down through this whole test (we'll
	// observe open + reminder + resolve by flipping the gate below).
	var resolveGate atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if resolveGate.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)

	// Fake Slack.
	recorder := &fakeSlackRecorder{}
	slackSrv := httptest.NewServer(recorder.handler())
	t.Cleanup(slackSrv.Close)

	// Real Postgres + migrations.
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
publicBaseURL: https://monitor.example.com
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
httpClient: { userAgent: "toggle-monitor/slack-it" }
slack:
  bodyMaxChars: 200
  channels:
    - { slug: ops-alerts, channelId: C0123ABCD, tokenEnv: TOGGLE_SLACK_TOKEN }
  coalesce:
    # Short pending window so the lone monitor's failure flushes the
    # individual-notification path quickly (ADR-0004: the dispatcher
    # waits pendingWait before deciding individual-vs-group). The
    # default 30s would never flush inside this test's 15s waits.
    pendingWait: 1s
monitors:
  - slug: api
    friendlyName: API
    url: %s
    tags: [prod]
    httpMethod: GET
    acceptedStatusCodes: [200]
    interval: 200ms
    timeout: 100ms
    retries: 0
    retryBackoff: 1s
    followRedirects: false
    reminderInterval: 400ms
    slack: ops-alerts
    notify: ["<!here>"]
`,
		dbCfg.Host, dbCfg.Port, dbCfg.User, dbCfg.Name, dbCfg.SSLMode,
		upstream.URL,
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
			Config:       cfg,
			DBConfig:     dbCfg,
			ListenAddr:   "127.0.0.1:0",
			SlackBaseURL: slackSrv.URL,
			OnReady:      func(a net.Addr) { addrCh <- a },
		})
	}()
	select {
	case <-addrCh:
	case <-time.After(15 * time.Second):
		t.Fatal("RunServe never bound")
	}

	// Wait for a parent + at least one reminder. The individual flush is
	// gated by pendingWait (1s) plus the central evaluator's cadence
	// (5s), so the first post can land ~5–6s in — give it generous room.
	deadline := time.Now().Add(15 * time.Second)
	for {
		recorder.mu.Lock()
		posts := len(recorder.postMessages)
		recorder.mu.Unlock()
		if posts >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for parent + reminder; got %d posts", posts)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Flip the upstream gate so the next tick resolves.
	resolveGate.Store(true)

	// Wait for the update + the resolve thread reply.
	deadline = time.Now().Add(15 * time.Second)
	for {
		recorder.mu.Lock()
		updates := len(recorder.updateMessages)
		posts := len(recorder.postMessages)
		recorder.mu.Unlock()
		if updates >= 1 && posts >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for resolve; updates=%d posts=%d", updates, posts)
		}
		time.Sleep(50 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("RunServe error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunServe did not exit cleanly")
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()

	if recorder.authCalls < 1 {
		t.Error("expected at least one auth.test call at startup")
	}

	// --- Parent: header sits on the top-level blocks (outside the
	// color stripe); mentions + View-details link live in the body /
	// footer context blocks inside attachments[0].blocks.
	parent := recorder.postMessages[0]
	if !blocksContain(t, parent["blocks"], ":red_circle: API is DOWN") {
		t.Error("parent message missing DOWN header")
	}
	if !blocksContain(t, parent["attachments"], "<!here>") {
		t.Error("parent message missing <!here> mention in body attachment")
	}
	if !blocksContain(t, parent["attachments"], "View details") {
		t.Error("parent message missing View-details link in footer")
	}
	if _, hasThread := parent["thread_ts"]; hasThread {
		t.Error("first post must be a parent (no thread_ts), but thread_ts is set")
	}

	// --- Reminder(s): no mentions, must be a thread reply
	for i, p := range recorder.postMessages[1:] {
		if _, hasThread := p["thread_ts"]; !hasThread {
			t.Errorf("post[%d] (reminder/resolve reply) must be threaded; missing thread_ts", i+1)
		}
		if blocksContain(t, p["blocks"], "<!here>") || blocksContain(t, p["attachments"], "<!here>") {
			t.Errorf("post[%d] (reminder/resolve reply) leaked the parent's mention", i+1)
		}
	}

	// --- Update: parent edit must rewrite header to the green
	// "is UP" form.
	if len(recorder.updateMessages) == 0 {
		t.Fatal("expected at least one chat.update on resolve")
	}
	update := recorder.updateMessages[0]
	if !blocksContain(t, update["blocks"], ":large_green_circle: API is UP") {
		t.Error("update did not rewrite header to resolved")
	}
}

// blocksContain reports whether any string field nested anywhere in
// the blocks slice contains the given substring. Tests use it without
// caring about the exact block layout.
func blocksContain(t *testing.T, blocks any, want string) bool {
	t.Helper()
	return containsStr(blocks, want)
}

func containsStr(v any, want string) bool {
	switch x := v.(type) {
	case string:
		return strings.Contains(x, want)
	case []any:
		for _, e := range x {
			if containsStr(e, want) {
				return true
			}
		}
	case map[string]any:
		for _, e := range x {
			if containsStr(e, want) {
				return true
			}
		}
	}
	return false
}
