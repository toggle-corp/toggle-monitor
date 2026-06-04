//go:build integration

package alertmanager_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toggle-corp/toggle-monitor/internal/alertmanager"
	"github.com/toggle-corp/toggle-monitor/internal/config"
	"github.com/toggle-corp/toggle-monitor/internal/migrate"
	"github.com/toggle-corp/toggle-monitor/internal/secret"
	"github.com/toggle-corp/toggle-monitor/internal/slack"
	"github.com/toggle-corp/toggle-monitor/internal/store"
	"github.com/toggle-corp/toggle-monitor/internal/testpg"
)

// -- test harness -----------------------------------------------------

// fakeSlack is a recording stand-in for the Slack Web API. It mirrors
// the shape used by internal/slack tests but is kept local so the AM
// handler tests are self-contained.
type fakeSlack struct {
	mu       sync.Mutex
	requests []recordedSlackReq
	respond  func(method string) (statusCode int, body string)
}

type recordedSlackReq struct {
	Method string
	Body   map[string]any
}

func newFakeSlack(t *testing.T) (*fakeSlack, *httptest.Server) {
	t.Helper()
	f := &fakeSlack{
		respond: func(method string) (int, string) {
			switch method {
			case "chat.postMessage":
				return 200, `{"ok": true, "ts": "1700000000.000100", "channel": "C0123ABCD"}`
			case "chat.update":
				return 200, `{"ok": true}`
			}
			return 404, `{"ok": false, "error": "unknown method"}`
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		method := parts[len(parts)-1]

		body := map[string]any{}
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &body)
		}
		f.mu.Lock()
		f.requests = append(f.requests, recordedSlackReq{Method: method, Body: body})
		f.mu.Unlock()

		code, resp := f.respond(method)
		w.WriteHeader(code)
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeSlack) reqsOf(method string) []recordedSlackReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedSlackReq
	for _, r := range f.requests {
		if r.Method == method {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeSlack) reqCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// recordingObserver captures every observer call so tests can assert on
// emitted counters / histograms.
type recordingObserver struct {
	mu       sync.Mutex
	requests [][2]string // result, reason
	alerts   [][2]string
	posts    [][2]string
	drops    []string
	late     int
	latency  []float64
	batches  []int
}

func (o *recordingObserver) AMWebhookRequest(result, reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.requests = append(o.requests, [2]string{result, reason})
}
func (o *recordingObserver) AMAlertProcessed(result, reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.alerts = append(o.alerts, [2]string{result, reason})
}
func (o *recordingObserver) AMSlackPost(result, reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.posts = append(o.posts, [2]string{result, reason})
}
func (o *recordingObserver) AMRateLimitDrop(channel string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.drops = append(o.drops, channel)
}
func (o *recordingObserver) AMLateResolve() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.late++
}
func (o *recordingObserver) AMWebhookLatency(s float64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.latency = append(o.latency, s)
}
func (o *recordingObserver) AMBatchSize(n int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.batches = append(o.batches, n)
}

// fakeMentionResolver returns the input verbatim, allowing tests to
// pass pre-resolved strings.
type fakeMentionResolver struct{}

func (fakeMentionResolver) Resolve(notify []string) []string { return notify }

// newRepo spins up a Postgres container, applies migrations, and
// returns a store.Repo against the resulting pool. Mirrors
// internal/store/store_integration_test.go's helper.
func newRepo(t *testing.T) *store.Repo {
	t.Helper()
	dsn := testpg.Start(t)
	if err := migrate.Up(dsn); err != nil {
		t.Fatalf("migrate.Up: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return store.New(pool)
}

// harness packages a configured handler plus the dependent fakes for
// per-test use.
type harness struct {
	handler  *alertmanager.Handler
	repo     *store.Repo
	slack    *fakeSlack
	observer *recordingObserver
}

const testBearer = "supersecret-token"
const testTokenEnv = "ALERTMANAGER_WEBHOOK_TOKEN_TEST"

// newHandler builds a handler against the given AM config. Token env
// var is set automatically; tests just pass `Authorization: Bearer
// testBearer` headers.
func newHandler(t *testing.T, am *config.AlertmanagerConfig) *harness {
	t.Helper()
	t.Setenv(testTokenEnv, testBearer)
	am.Endpoint.TokenEnv = testTokenEnv

	repo := newRepo(t)
	fs, srv := newFakeSlack(t)
	client := slack.NewClient(slack.WithBaseURL(srv.URL))
	obs := &recordingObserver{}

	h, err := alertmanager.NewHandler(alertmanager.HandlerOptions{
		Config:      am,
		Repo:        repo,
		SlackClient: client,
		Channels: func(slug string) (slack.ChannelInfo, bool) {
			switch slug {
			case "ops-alerts":
				return slack.ChannelInfo{ID: "C0AAAA", Token: secret.SecretString("xoxb-ops")}, true
			case "ops-critical":
				return slack.ChannelInfo{ID: "C0CRIT", Token: secret.SecretString("xoxb-crit")}, true
			}
			return slack.ChannelInfo{}, false
		},
		Mentions:   fakeMentionResolver{},
		Now:        func() time.Time { return time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC) },
		Observer:   obs,
		PublicBase: "https://monitor.example.test",
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return &harness{handler: h, repo: repo, slack: fs, observer: obs}
}

// defaultAMConfig returns a minimal valid AM config with a root rule
// routing to ops-alerts, and a Watchdog ignore rule.
func defaultAMConfig() *config.AlertmanagerConfig {
	rootCfg := &config.AlertmanagerMatchConfig{Slack: "ops-alerts", Notify: config.NotifyList{Values: []string{"ops-team"}}}
	watchdog := true
	critCfg := &config.AlertmanagerMatchConfig{Slack: "ops-critical"}
	return &config.AlertmanagerConfig{
		Endpoint:      config.AlertmanagerEndpoint{Path: "/webhooks/alertmanager"},
		RetentionDays: 180,
		RateLimit: config.AlertmanagerRateLimit{
			PerChannel:  100,
			Window:      config.Duration(30 * time.Minute),
			NoticeEvery: config.Duration(24 * time.Hour),
		},
		Match: []config.AlertmanagerMatchRule{
			{Config: rootCfg},
			{When: &config.AlertmanagerMatchWhen{Alertname: "Watchdog"}, Ignore: &watchdog, Final: true},
			{When: &config.AlertmanagerMatchWhen{Labels: map[string]string{"severity": "critical"}}, Config: critCfg},
		},
	}
}

// firingWebhook returns a webhook payload with the given fingerprints
// all in "firing" status.
func firingWebhook(fingerprints ...string) []byte {
	alerts := []map[string]any{}
	for _, fp := range fingerprints {
		alerts = append(alerts, map[string]any{
			"status":      "firing",
			"labels":      map[string]string{"alertname": "HighCPU", "severity": "critical", "instance": "pod-1"},
			"annotations": map[string]string{"summary": "CPU is on fire", "runbook_url": "https://runbooks.example.test/cpu"},
			"startsAt":    "2026-06-04T11:55:00Z",
			"endsAt":      "0001-01-01T00:00:00Z",
			"fingerprint": fp,
		})
	}
	body := map[string]any{
		"version":     "4",
		"groupKey":    "{}:{}",
		"status":      "firing",
		"receiver":    "toggle_monitor",
		"externalURL": "https://am.prod.example.test",
		"alerts":      alerts,
	}
	raw, _ := json.Marshal(body)
	return raw
}

// resolvedWebhook builds a webhook envelope with each fp marked resolved.
func resolvedWebhook(fingerprints ...string) []byte {
	alerts := []map[string]any{}
	for _, fp := range fingerprints {
		alerts = append(alerts, map[string]any{
			"status":      "resolved",
			"labels":      map[string]string{"alertname": "HighCPU", "severity": "critical", "instance": "pod-1"},
			"annotations": map[string]string{"summary": "CPU is on fire"},
			"startsAt":    "2026-06-04T11:55:00Z",
			"endsAt":      "2026-06-04T12:00:00Z",
			"fingerprint": fp,
		})
	}
	body := map[string]any{
		"version":     "4",
		"groupKey":    "{}:{}",
		"status":      "resolved",
		"receiver":    "toggle_monitor",
		"externalURL": "https://am.prod.example.test",
		"alerts":      alerts,
	}
	raw, _ := json.Marshal(body)
	return raw
}

// watchdogWebhook builds a webhook with the Watchdog alertname so the
// ignore rule fires.
func watchdogWebhook(fp string) []byte {
	body := map[string]any{
		"version":     "4",
		"groupKey":    "{}:{}",
		"status":      "firing",
		"receiver":    "toggle_monitor",
		"externalURL": "https://am.prod.example.test",
		"alerts": []map[string]any{{
			"status":      "firing",
			"labels":      map[string]string{"alertname": "Watchdog", "severity": "info"},
			"annotations": map[string]string{"summary": "Watchdog"},
			"startsAt":    "2026-06-04T11:55:00Z",
			"endsAt":      "0001-01-01T00:00:00Z",
			"fingerprint": fp,
		}},
	}
	raw, _ := json.Marshal(body)
	return raw
}

// do is a small helper that issues a POST with the given body and
// returns the response.
func (h *harness) do(t *testing.T, method, path string, body []byte, withAuth bool) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if withAuth {
		req.Header.Set("Authorization", "Bearer "+testBearer)
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, req)
	return w.Result()
}

// -- tests -----------------------------------------------------------

func TestHandler_happyFiring_insertsRowAndPostsToSlack(t *testing.T) {
	h := newHandler(t, defaultAMConfig())
	resp := h.do(t, http.MethodPost, "/webhooks/alertmanager", firingWebhook("fp-1"), true)
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 200; body=%s", resp.StatusCode, raw)
	}

	posts := h.slack.reqsOf("chat.postMessage")
	if len(posts) != 1 {
		t.Fatalf("expected 1 Slack post, got %d", len(posts))
	}
	if posts[0].Body["channel"] != "C0CRIT" {
		t.Errorf("posted to wrong channel: got %v, want C0CRIT", posts[0].Body["channel"])
	}

	incidents, err := h.repo.ListAMIncidentsByFingerprint(context.Background(), "fp-1", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(incidents) != 1 {
		t.Fatalf("expected 1 incident, got %d", len(incidents))
	}
	if incidents[0].SlackTS == "" {
		t.Error("slack_ts should be persisted after successful post")
	}
}

func TestHandler_authMissingBearer_returns401(t *testing.T) {
	h := newHandler(t, defaultAMConfig())
	resp := h.do(t, http.MethodPost, "/webhooks/alertmanager", firingWebhook("fp-1"), false /*no auth*/)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", resp.StatusCode)
	}
	if h.slack.reqCount() != 0 {
		t.Error("no Slack call should have been made on auth failure")
	}
}

func TestHandler_authWrongBearer_returns401(t *testing.T) {
	h := newHandler(t, defaultAMConfig())
	req := httptest.NewRequest(http.MethodPost, "/webhooks/alertmanager", bytes.NewReader(firingWebhook("fp-1")))
	req.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", w.Code)
	}
}

func TestHandler_bodyTooLarge_returns413(t *testing.T) {
	h := newHandler(t, defaultAMConfig())
	// Build a JSON body larger than 10 MiB.
	big := bytes.Repeat([]byte("x"), 11*1024*1024)
	resp := h.do(t, http.MethodPost, "/webhooks/alertmanager", big, true)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status: got %d, want 413", resp.StatusCode)
	}
	if h.slack.reqCount() != 0 {
		t.Error("no Slack call should have happened")
	}
}

func TestHandler_malformedJSON_returns400(t *testing.T) {
	h := newHandler(t, defaultAMConfig())
	resp := h.do(t, http.MethodPost, "/webhooks/alertmanager", []byte("{not json"), true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
}

func TestHandler_validateFails_returns400(t *testing.T) {
	h := newHandler(t, defaultAMConfig())
	body := map[string]any{
		"version": "4",
		"status":  "firing",
		"alerts":  []map[string]any{{"status": "firing", "labels": map[string]string{"a": "b"}}}, // missing fingerprint
	}
	raw, _ := json.Marshal(body)
	resp := h.do(t, http.MethodPost, "/webhooks/alertmanager", raw, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
}

func TestHandler_methodGet_returns405(t *testing.T) {
	h := newHandler(t, defaultAMConfig())
	resp := h.do(t, http.MethodGet, "/webhooks/alertmanager", nil, true)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status: got %d, want 405", resp.StatusCode)
	}
	if resp.Header.Get("Allow") != "POST" {
		t.Errorf("Allow header: got %q, want POST", resp.Header.Get("Allow"))
	}
}

func TestHandler_ignoredAlert_noRowNoSlackPost(t *testing.T) {
	h := newHandler(t, defaultAMConfig())
	resp := h.do(t, http.MethodPost, "/webhooks/alertmanager", watchdogWebhook("fp-wd"), true)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d, want 200", resp.StatusCode)
	}
	if h.slack.reqCount() != 0 {
		t.Errorf("Watchdog should not post; got %d calls", h.slack.reqCount())
	}
	incidents, err := h.repo.ListAMIncidentsByFingerprint(context.Background(), "fp-wd", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(incidents) != 0 {
		t.Errorf("expected no row for ignored alert, got %d", len(incidents))
	}
	// Observer should have logged a drop=ignored.
	h.observer.mu.Lock()
	found := false
	for _, a := range h.observer.alerts {
		if a[0] == "drop" && a[1] == "ignored" {
			found = true
		}
	}
	h.observer.mu.Unlock()
	if !found {
		t.Error("observer did not record drop/ignored")
	}
}

func TestHandler_resolveAfterFiring_editsAndRepliesInThread(t *testing.T) {
	h := newHandler(t, defaultAMConfig())
	// 1) firing
	resp := h.do(t, http.MethodPost, "/webhooks/alertmanager", firingWebhook("fp-r"), true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("firing: %d", resp.StatusCode)
	}
	// 2) resolved
	resp = h.do(t, http.MethodPost, "/webhooks/alertmanager", resolvedWebhook("fp-r"), true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resolve: %d", resp.StatusCode)
	}

	// chat.update happened plus a chat.postMessage thread reply.
	updates := h.slack.reqsOf("chat.update")
	if len(updates) != 1 {
		t.Errorf("expected 1 chat.update, got %d", len(updates))
	}
	// posts: 1 initial parent + 1 thread reply = 2
	posts := h.slack.reqsOf("chat.postMessage")
	if len(posts) != 2 {
		t.Fatalf("expected 2 chat.postMessage (parent + reply), got %d", len(posts))
	}
	// The reply should carry a thread_ts.
	if posts[1].Body["thread_ts"] == nil || posts[1].Body["thread_ts"] == "" {
		t.Errorf("resolve reply missing thread_ts: %+v", posts[1].Body)
	}

	// DB: ended_at populated.
	incs, _ := h.repo.ListAMIncidentsByFingerprint(context.Background(), "fp-r", 10)
	if len(incs) != 1 {
		t.Fatalf("expected 1 incident, got %d", len(incs))
	}
	if !incs[0].EndedAt.Valid {
		t.Error("ended_at should be set after resolve")
	}
}

func TestHandler_lateResolve_postsStandaloneAndCountsObserver(t *testing.T) {
	h := newHandler(t, defaultAMConfig())
	resp := h.do(t, http.MethodPost, "/webhooks/alertmanager", resolvedWebhook("fp-late"), true)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d, want 200", resp.StatusCode)
	}
	posts := h.slack.reqsOf("chat.postMessage")
	if len(posts) != 1 {
		t.Errorf("expected 1 standalone resolve post, got %d", len(posts))
	}
	h.observer.mu.Lock()
	late := h.observer.late
	h.observer.mu.Unlock()
	if late != 1 {
		t.Errorf("observer late-resolve: got %d, want 1", late)
	}
	// No DB row should exist (we don't insert for late-resolve).
	incs, _ := h.repo.ListAMIncidentsByFingerprint(context.Background(), "fp-late", 10)
	if len(incs) != 0 {
		t.Errorf("expected no DB row for late-resolve, got %d", len(incs))
	}
}

func TestHandler_repeatFiring_noSecondSlackPost(t *testing.T) {
	h := newHandler(t, defaultAMConfig())
	for i := 0; i < 2; i++ {
		resp := h.do(t, http.MethodPost, "/webhooks/alertmanager", firingWebhook("fp-dup"), true)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("iter %d: status %d", i, resp.StatusCode)
		}
	}
	posts := h.slack.reqsOf("chat.postMessage")
	if len(posts) != 1 {
		t.Errorf("expected 1 Slack post (repeat firing is no-op), got %d", len(posts))
	}
}

func TestHandler_crashRecovery_postsSlackForOrphanedRow(t *testing.T) {
	h := newHandler(t, defaultAMConfig())
	// Seed a row with slack_ts="" — simulating a process crash between
	// DB-INSERT and Slack-post.
	_, _, err := h.repo.InsertOpenAMIncident(context.Background(), store.AMIncidentInsert{
		Fingerprint: "fp-orphan",
		Alertname:   "HighCPU",
		Labels:      map[string]string{"alertname": "HighCPU", "severity": "critical"},
		Annotations: map[string]string{"summary": "x"},
		StartedAt:   time.Now(),
		ChannelSlug: "ops-critical",
		RuleChain:   "match[0] → match[2]",
		ExternalURL: "https://am.prod.example.test",
		Receiver:    "toggle_monitor",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// AM redelivers — handler should detect missing slack_ts and post.
	resp := h.do(t, http.MethodPost, "/webhooks/alertmanager", firingWebhook("fp-orphan"), true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	posts := h.slack.reqsOf("chat.postMessage")
	if len(posts) != 1 {
		t.Errorf("expected 1 Slack post after crash recovery, got %d", len(posts))
	}
	incs, _ := h.repo.ListAMIncidentsByFingerprint(context.Background(), "fp-orphan", 10)
	if len(incs) != 1 || incs[0].SlackTS == "" {
		t.Errorf("crash-recovery row should now carry slack_ts, got %+v", incs)
	}
}

func TestHandler_rateLimitDrop_postsThrottleWarning(t *testing.T) {
	cfg := defaultAMConfig()
	cfg.RateLimit = config.AlertmanagerRateLimit{
		PerChannel:  2,
		Window:      config.Duration(1 * time.Minute),
		NoticeEvery: config.Duration(24 * time.Hour),
	}
	h := newHandler(t, cfg)

	for i := 0; i < 5; i++ {
		fp := fmt.Sprintf("fp-rl-%d", i)
		resp := h.do(t, http.MethodPost, "/webhooks/alertmanager", firingWebhook(fp), true)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("iter %d: %d", i, resp.StatusCode)
		}
	}
	// First 2 = parent posts; 3rd triggers engagement → 1 throttle notice.
	// 4th and 5th = silent drops (until noticeEvery elapses).
	posts := h.slack.reqsOf("chat.postMessage")
	if len(posts) < 3 {
		t.Fatalf("expected at least 3 Slack posts (2 parents + throttle notice), got %d", len(posts))
	}
	if len(posts) > 3 {
		t.Errorf("expected at most 3 Slack posts (silence after throttle notice), got %d", len(posts))
	}
	h.observer.mu.Lock()
	drops := len(h.observer.drops)
	h.observer.mu.Unlock()
	if drops != 3 {
		t.Errorf("observer rate-limit drops: got %d, want 3", drops)
	}
}

func TestHandler_batchedMixedAlerts_processesAll(t *testing.T) {
	h := newHandler(t, defaultAMConfig())

	body := map[string]any{
		"version":     "4",
		"groupKey":    "{}:{}",
		"status":      "firing",
		"receiver":    "toggle_monitor",
		"externalURL": "https://am.prod.example.test",
		"alerts": []map[string]any{
			{
				"status":      "firing",
				"labels":      map[string]string{"alertname": "HighCPU", "severity": "critical"},
				"annotations": map[string]string{"summary": "x"},
				"startsAt":    "2026-06-04T11:55:00Z",
				"endsAt":      "0001-01-01T00:00:00Z",
				"fingerprint": "fp-batch-fire",
			},
			{
				"status":      "resolved",
				"labels":      map[string]string{"alertname": "HighCPU", "severity": "critical"},
				"annotations": map[string]string{"summary": "x"},
				"startsAt":    "2026-06-04T11:55:00Z",
				"endsAt":      "2026-06-04T12:00:00Z",
				"fingerprint": "fp-batch-late",
			},
			{
				"status":      "firing",
				"labels":      map[string]string{"alertname": "Watchdog", "severity": "info"},
				"annotations": map[string]string{"summary": "wd"},
				"startsAt":    "2026-06-04T11:55:00Z",
				"endsAt":      "0001-01-01T00:00:00Z",
				"fingerprint": "fp-batch-wd",
			},
		},
	}
	raw, _ := json.Marshal(body)
	resp := h.do(t, http.MethodPost, "/webhooks/alertmanager", raw, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}

	// Expect 2 Slack posts: 1 parent for fp-batch-fire, 1 standalone for late-resolve.
	posts := h.slack.reqsOf("chat.postMessage")
	if len(posts) != 2 {
		t.Errorf("expected 2 Slack posts, got %d", len(posts))
	}

	// Batch size observation.
	h.observer.mu.Lock()
	bs := append([]int(nil), h.observer.batches...)
	h.observer.mu.Unlock()
	if len(bs) != 1 || bs[0] != 3 {
		t.Errorf("batch size observation: got %v, want [3]", bs)
	}
}

func TestHandler_partialFailure_returns503(t *testing.T) {
	h := newHandler(t, defaultAMConfig())
	// Make chat.postMessage permanently fail.
	h.slack.respond = func(method string) (int, string) {
		if method == "chat.postMessage" {
			return 200, `{"ok": false, "error": "channel_not_found"}`
		}
		return 200, `{"ok": true}`
	}
	resp := h.do(t, http.MethodPost, "/webhooks/alertmanager", firingWebhook("fp-503"), true)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want 503", resp.StatusCode)
	}
}

func TestHandler_concurrentBatchesIsolated(t *testing.T) {
	h := newHandler(t, defaultAMConfig())
	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			fp := fmt.Sprintf("fp-conc-%d", i)
			resp := h.do(t, http.MethodPost, "/webhooks/alertmanager", firingWebhook(fp), true)
			if resp.StatusCode != http.StatusOK {
				failures.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Errorf("got %d failed concurrent batches", failures.Load())
	}
	// Each fingerprint should have produced a row + Slack post.
	for i := 0; i < 4; i++ {
		incs, _ := h.repo.ListAMIncidentsByFingerprint(context.Background(), fmt.Sprintf("fp-conc-%d", i), 10)
		if len(incs) != 1 {
			t.Errorf("fp-conc-%d: expected 1 incident, got %d", i, len(incs))
		}
	}
}
