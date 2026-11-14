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
		UptimeOpenFailed: true,
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

// newRecoveryNotifier wires a Notifier to a fake Slack that answers
// chat.postMessage with postResponse ("" keeps the fake's success).
func newRecoveryNotifier(t *testing.T, postResponse string) (*fakeSlack, *fakeStore, *fakeObserver, *slack.Notifier) {
	t.Helper()
	f, srv := newFakeSlack(t)
	if postResponse != "" {
		ok := f.respond
		f.respond = func(method string) (int, string) {
			if method == "chat.postMessage" {
				return 200, postResponse
			}
			return ok(method)
		}
	}
	store := &fakeStore{}
	obs := &fakeObserver{}
	n := slack.NewNotifier(slack.NotifierOptions{
		Client:   slack.NewClient(slack.WithBaseURL(srv.URL)),
		Store:    store,
		Observer: obs,
		Channels: func(string) (slack.ChannelInfo, bool) {
			return slack.ChannelInfo{ID: "C0123", Token: secret.SecretString("xoxb-test")}, true
		},
	})
	return f, store, obs, n
}

func slackCalls(f *fakeSlack) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// TestNotifier_Open_postFails_marksOpenFailed: an Open whose post
// fails is recorded, so its eventual recovery knows a page was due and
// did not land.
func TestNotifier_Open_postFails_marksOpenFailed(t *testing.T) {
	_, store, _, n := newRecoveryNotifier(t, `{"ok": false, "error": "channel_not_found"}`)
	err := n.Notify(context.Background(), "ops", nil,
		slack.MonitorView{Slug: "api", FriendlyName: "api", URL: "https://api.example.test/"},
		&alert.Event{Type: alert.EventOpen, At: time.Now().UTC(), StatusCode: 503, Error: "boom"},
	)
	if err == nil {
		t.Fatal("Notify(open): want the post error, got nil")
	}
	if len(store.openFailedMarks) != 1 || store.openFailedMarks[0] != "api" {
		t.Errorf("open-failed marks: got %v, want [api]", store.openFailedMarks)
	}
	if store.setUptimeCalls != 0 {
		t.Errorf("SetUptimeThread calls: got %d, want 0", store.setUptimeCalls)
	}
}

// TestNotifier_Open_delivered_leavesNoMark: a delivered Open stores its
// thread ref and nothing else.
func TestNotifier_Open_delivered_leavesNoMark(t *testing.T) {
	_, store, _, n := newRecoveryNotifier(t, "")
	err := n.Notify(context.Background(), "ops", nil,
		slack.MonitorView{Slug: "api", FriendlyName: "api", URL: "https://api.example.test/"},
		&alert.Event{Type: alert.EventOpen, At: time.Now().UTC(), StatusCode: 503, Error: "boom"},
	)
	if err != nil {
		t.Fatalf("Notify(open): %v", err)
	}
	if len(store.openFailedMarks) != 0 {
		t.Errorf("open-failed marks: got %v, want none", store.openFailedMarks)
	}
}

// TestNotifier_Resolve_neverAnnounced_postsNothing: no parent ts and no
// failed delivery means nobody was paged about this incident (a
// dependsOn child paused before its Open flushed), so its recovery
// stays silent.
func TestNotifier_Resolve_neverAnnounced_postsNothing(t *testing.T) {
	f, _, obs, n := newRecoveryNotifier(t, "")
	now := time.Now().UTC()
	err := n.Notify(context.Background(), "ops", nil,
		slack.MonitorView{Slug: "child", FriendlyName: "child", URL: "https://child.example.test/", OpenedAt: now.Add(-10 * time.Minute)},
		&alert.Event{Type: alert.EventResolve, At: now, Downtime: 10 * time.Minute},
	)
	if err != nil {
		t.Fatalf("Notify(resolve): %v", err)
	}
	if got := slackCalls(f); got != 0 {
		t.Errorf("Slack calls: got %d, want 0", got)
	}
	if got := obs.post.Load(); got != 0 {
		t.Errorf("slack_post count: got %d, want 0 (nothing was posted)", got)
	}
}

// TestNotifier_Resolve_openFailed_postsBannerResolve: the Open was
// attempted and failed, so the recovery posts the standalone resolve
// with the late-notice banner.
func TestNotifier_Resolve_openFailed_postsBannerResolve(t *testing.T) {
	f, _, _, n := newRecoveryNotifier(t, "")
	now := time.Now().UTC()
	err := n.Notify(context.Background(), "ops", nil,
		slack.MonitorView{
			Slug: "api", FriendlyName: "api", URL: "https://api.example.test/",
			OpenedAt: now.Add(-10 * time.Minute), UptimeOpenFailed: true,
		},
		&alert.Event{Type: alert.EventResolve, At: now, Downtime: 10 * time.Minute},
	)
	if err != nil {
		t.Fatalf("Notify(resolve): %v", err)
	}
	flat := flattenMrkdwn(t, findPostedMessage(t, f))
	if !strings.Contains(flat, "is UP") || !strings.Contains(flat, "Initial notification delivery failed") {
		t.Errorf("want a standalone is-UP with the late-notice banner; got:\n%s", flat)
	}
}

// TestNotifier_Reminder_neverAnnounced_postsDownParentWithoutBanner: a
// dependsOn child resumed still down after its parent recovered was
// never paged. Its reminder posts its first DOWN parent, without a
// banner claiming an earlier delivery failed, and stores the ts so later
// reminders thread onto it.
func TestNotifier_Reminder_neverAnnounced_postsDownParentWithoutBanner(t *testing.T) {
	f, store, obs, n := newRecoveryNotifier(t, "")
	now := time.Now().UTC()
	err := n.Notify(context.Background(), "ops", nil,
		slack.MonitorView{
			Slug: "child", FriendlyName: "child", URL: "https://child.example.test/",
			StatusCode: 503, StatusText: "Service Unavailable", OpenedAt: now.Add(-time.Hour),
		},
		&alert.Event{Type: alert.EventReminder, At: now, StatusCode: 503, Error: "still down"},
	)
	if err != nil {
		t.Fatalf("Notify(reminder): %v", err)
	}
	body := findPostedMessage(t, f)
	flat := flattenMrkdwn(t, body)
	if !strings.Contains(flat, "is DOWN") {
		t.Errorf("want a DOWN parent; got:\n%s", flat)
	}
	if strings.Contains(flat, "Initial notification delivery failed") {
		t.Errorf("first announcement carries the late-notice banner; got:\n%s", flat)
	}
	if _, hasThread := body["thread_ts"]; hasThread {
		t.Errorf("DOWN parent must not be threaded")
	}
	if store.setUptimeCalls != 1 {
		t.Errorf("SetUptimeThread calls: got %d, want 1", store.setUptimeCalls)
	}
	if got := obs.fresh.Load(); got != 0 {
		t.Errorf("fresh-parent count: got %d, want 0 (not a delivery fallback)", got)
	}
}

// TestNotifier_Reminder_freshParentFails_marksOpenFailed: a reminder
// that tries to post the monitor's parent and fails leaves the same
// mark a failed Open does.
func TestNotifier_Reminder_freshParentFails_marksOpenFailed(t *testing.T) {
	_, store, _, n := newRecoveryNotifier(t, `{"ok": false, "error": "channel_not_found"}`)
	now := time.Now().UTC()
	err := n.Notify(context.Background(), "ops", nil,
		slack.MonitorView{Slug: "child", FriendlyName: "child", URL: "https://child.example.test/", OpenedAt: now.Add(-time.Hour)},
		&alert.Event{Type: alert.EventReminder, At: now, Error: "still down"},
	)
	if err == nil {
		t.Fatal("Notify(reminder): want the post error, got nil")
	}
	if len(store.openFailedMarks) != 1 || store.openFailedMarks[0] != "child" {
		t.Errorf("open-failed marks: got %v, want [child]", store.openFailedMarks)
	}
}
