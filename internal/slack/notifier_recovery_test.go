package slack_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/alert"
	"github.com/toggle-corp/toggle-monitor/internal/secret"
	"github.com/toggle-corp/toggle-monitor/internal/slack"
)

// fakeObserver counts SlackPost/SlackRetry/SlackFreshParent events so
// tests can assert what the notifier emitted.
type fakeObserver struct {
	post        atomic.Int32
	postSuccess atomic.Int32
	postFail    atomic.Int32
	postReason  atomic.Value // last "reason" label seen
	fresh       atomic.Int32
	freshKind   atomic.Value // last "kind" label seen
}

func (f *fakeObserver) SlackPost(result, reason string) {
	f.post.Add(1)
	if result == "success" {
		f.postSuccess.Add(1)
	} else {
		f.postFail.Add(1)
	}
	f.postReason.Store(reason)
}

func (f *fakeObserver) SlackRetry(_, _ string) {}

func (f *fakeObserver) SlackFreshParent(kind string) {
	f.fresh.Add(1)
	f.freshKind.Store(kind)
}

// TestNotifier_Reminder_withoutParent_postsFreshParent confirms the
// Q5 recovery contract: when a reminder fires but no parent ts is on
// file (the initial Open delivery failed), the notifier posts a fresh
// Down parent with a late-notice banner, persists the new ts, and
// increments the fresh_parent metric.
func TestNotifier_Reminder_withoutParent_postsFreshParent(t *testing.T) {
	f, srv := newFakeSlack(t)
	client := slack.NewClient(slack.WithBaseURL(srv.URL))

	store := &fakeStore{}
	obs := &fakeObserver{}

	notifier := slack.NewNotifier(slack.NotifierOptions{
		Client:   client,
		Store:    store,
		Observer: obs,
		Channels: func(string) (slack.ChannelInfo, bool) {
			return slack.ChannelInfo{ID: "C0123", Token: secret.SecretString("xoxb-test")}, true
		},
	})

	openedAt := time.Now().UTC().Add(-30 * time.Minute)
	mv := slack.MonitorView{
		Slug: "api", FriendlyName: "api",
		URL:        "https://api/health",
		StatusCode: 503, StatusText: "Service Unavailable",
		OpenedAt: openedAt,
		// No UptimeThreadTS — Open delivery failed.
	}
	ev := &alert.Event{Type: alert.EventReminder, At: time.Now().UTC(), Error: "still down"}
	if err := notifier.Notify(context.Background(), "ops", nil, mv, ev); err != nil {
		t.Fatalf("Notify(reminder): %v", err)
	}

	body := findPostedMessage(t, f)
	flat := flattenMrkdwn(t, body)
	if !strings.Contains(flat, "Initial notification delivery failed") {
		t.Errorf("expected late-notice banner in fresh parent; got:\n%s", flat)
	}
	// Parent shape (not a reminder reply) — must NOT include a thread_ts.
	if _, hasThread := body["thread_ts"]; hasThread {
		t.Errorf("fresh parent must not be threaded; body had thread_ts")
	}
	// Thread ref must be persisted so subsequent reminders thread onto it.
	if store.setUptimeCalls != 1 {
		t.Errorf("SetUptimeThread calls: got %d, want 1", store.setUptimeCalls)
	}
	// Metric: fresh_parent_total{kind="uptime"} incremented once.
	if got := obs.fresh.Load(); got != 1 {
		t.Errorf("fresh-parent count: got %d, want 1", got)
	}
	if kind, _ := obs.freshKind.Load().(string); kind != "uptime" {
		t.Errorf("fresh-parent kind: got %q, want uptime", kind)
	}
}

// TestNotifier_SSLReminder_withoutParent_postsFreshParent mirrors the
// uptime test for the SSL path.
func TestNotifier_SSLReminder_withoutParent_postsFreshParent(t *testing.T) {
	f, srv := newFakeSlack(t)
	client := slack.NewClient(slack.WithBaseURL(srv.URL))

	store := &fakeStore{}
	obs := &fakeObserver{}

	notifier := slack.NewNotifier(slack.NotifierOptions{
		Client:   client,
		Store:    store,
		Observer: obs,
		Channels: func(string) (slack.ChannelInfo, bool) {
			return slack.ChannelInfo{ID: "C0123", Token: secret.SecretString("xoxb-test")}, true
		},
	})

	expiresAt := time.Now().UTC().Add(72 * time.Hour)
	mv := slack.MonitorView{
		Slug: "api", FriendlyName: "api",
		URL: "https://api/",
		// No SSLThreadTS — initial SSL Open delivery failed.
	}
	sslView := slack.SSLView{ExpiresAt: expiresAt}
	ev := &alert.SSLEvent{Type: alert.EventSSLReminder, At: time.Now().UTC(), ExpiresAt: expiresAt}
	if err := notifier.NotifySSL(context.Background(), "ops", nil, mv, sslView, ev); err != nil {
		t.Fatalf("NotifySSL(reminder): %v", err)
	}

	body := findPostedMessage(t, f)
	flat := flattenMrkdwn(t, body)
	if !strings.Contains(flat, "Initial notification delivery failed") {
		t.Errorf("expected late-notice banner in fresh SSL parent; got:\n%s", flat)
	}
	if _, hasThread := body["thread_ts"]; hasThread {
		t.Errorf("fresh SSL parent must not be threaded; body had thread_ts")
	}
	if store.setSSLCalls != 1 {
		t.Errorf("SetSSLThread calls: got %d, want 1", store.setSSLCalls)
	}
	if got := obs.fresh.Load(); got != 1 {
		t.Errorf("fresh-parent count: got %d, want 1", got)
	}
	if kind, _ := obs.freshKind.Load().(string); kind != "ssl" {
		t.Errorf("fresh-parent kind: got %q, want ssl", kind)
	}
}

// TestNotifier_emitsSlackPostMetric confirms a successful Open path
// increments slack_post_total with result=success, reason=ok.
func TestNotifier_emitsSlackPostMetric(t *testing.T) {
	_, srv := newFakeSlack(t)
	client := slack.NewClient(slack.WithBaseURL(srv.URL))

	store := &fakeStore{}
	obs := &fakeObserver{}
	notifier := slack.NewNotifier(slack.NotifierOptions{
		Client:   client,
		Store:    store,
		Observer: obs,
		Channels: func(string) (slack.ChannelInfo, bool) {
			return slack.ChannelInfo{ID: "C0123", Token: secret.SecretString("xoxb-test")}, true
		},
	})
	err := notifier.Notify(context.Background(), "ops", nil,
		slack.MonitorView{Slug: "x", FriendlyName: "x", URL: "https://x/"},
		&alert.Event{Type: alert.EventOpen, At: time.Now().UTC(), StatusCode: 503, Error: "boom"},
	)
	if err != nil {
		t.Fatalf("Notify(open): %v", err)
	}
	if got := obs.postSuccess.Load(); got != 1 {
		t.Errorf("success count: got %d, want 1", got)
	}
	if reason, _ := obs.postReason.Load().(string); reason != "ok" {
		t.Errorf("reason: got %q, want ok", reason)
	}
}
