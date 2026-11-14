package slack_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/alert"
	"github.com/toggle-corp/toggle-monitor/internal/slack"
)

func openEvent(at time.Time) *alert.Event {
	return &alert.Event{Type: alert.EventOpen, At: at, StatusCode: 503, Error: "boom"}
}

var apiView = slack.MonitorView{Slug: "api", FriendlyName: "api", URL: "https://api.example.test/"}

// TestNotifier_Open_threadRefWriteFailsOnce_isRetried: a transient
// store error right after a delivered Open must not lose the only
// handle on the DOWN message.
func TestNotifier_Open_threadRefWriteFailsOnce_isRetried(t *testing.T) {
	_, store, _, n := newRecoveryNotifier(t, "")
	store.setUptimeFails = 1
	if err := n.Notify(context.Background(), "ops", nil, apiView, openEvent(time.Now().UTC())); err != nil {
		t.Fatalf("Notify(open): %v", err)
	}
	if store.setUptimeCalls != 2 {
		t.Errorf("SetUptimeThread calls: got %d, want 2 (one retry)", store.setUptimeCalls)
	}
	if len(store.openFailedMarks) != 0 {
		t.Errorf("open-failed marks: got %v, want none", store.openFailedMarks)
	}
}

// TestNotifier_Open_threadRefNeverStored_marksOpenFailed: the DOWN went
// out but its ref cannot be stored. The row then says what a failed
// Open says, so the recovery still reports the incident instead of
// staying silent, and this process, which remembers the ref, still
// edits the DOWN.
func TestNotifier_Open_threadRefNeverStored_marksOpenFailed(t *testing.T) {
	f, store, _, n := newRecoveryNotifier(t, "")
	store.setUptimeFails = -1
	ctx := context.Background()
	opened := time.Now().UTC().Add(-time.Minute)
	if err := n.Notify(ctx, "ops", nil, apiView, openEvent(opened)); err != nil {
		t.Fatalf("Notify(open): %v", err)
	}
	if len(store.openFailedMarks) != 1 || store.openFailedMarks[0] != "api" {
		t.Fatalf("open-failed marks: got %v, want [api]", store.openFailedMarks)
	}

	resolved := apiView
	resolved.OpenedAt = opened
	resolved.UptimeOpenFailed = true
	if err := n.Notify(ctx, "ops", nil, resolved,
		&alert.Event{Type: alert.EventResolve, At: opened.Add(time.Minute), Downtime: time.Minute},
	); err != nil {
		t.Fatalf("Notify(resolve): %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	edited := false
	for _, r := range f.requests {
		if r.Method == "chat.update" && r.Body["ts"] == "1700000000.000100" {
			edited = true
		}
	}
	if !edited {
		t.Errorf("want the DOWN edited from the remembered ref, got %+v", f.requests)
	}
}

// TestNotifier_UptimeParentPosted_reportsTheIncidentsPostedParent: the
// dispatcher asks this before it treats a marked recovery as an orphan.
// A DOWN that went out answers yes for its own incident, even with its
// ref never stored; another incident, another monitor, or a failed post
// answers no.
func TestNotifier_UptimeParentPosted_reportsTheIncidentsPostedParent(t *testing.T) {
	f, store, _, n := newRecoveryNotifier(t, "")
	store.setUptimeFails = -1
	ctx := context.Background()
	opened := time.Now().UTC().Add(-time.Minute)
	if err := n.Notify(ctx, "ops", nil, apiView, openEvent(opened)); err != nil {
		t.Fatalf("Notify(open): %v", err)
	}
	if !n.UptimeParentPosted("api", opened.Truncate(time.Microsecond)) {
		t.Errorf("posted parent with its ref unstored: want true")
	}
	if n.UptimeParentPosted("api", opened.Add(time.Hour)) {
		t.Errorf("another incident: want false")
	}
	if n.UptimeParentPosted("web", opened) {
		t.Errorf("another monitor: want false")
	}

	f.respond = func(string) (int, string) { return 200, `{"ok":false,"error":"channel_not_found"}` }
	web := apiView
	web.Slug = "web"
	_ = n.Notify(ctx, "ops", nil, web, openEvent(opened))
	if n.UptimeParentPosted("web", opened) {
		t.Errorf("failed post: want false")
	}
}

// TestNotifier_FreshParent_threadRefNeverStored_marksOpenFailed: the
// parent a reminder posts is subject to the same rule.
func TestNotifier_FreshParent_threadRefNeverStored_marksOpenFailed(t *testing.T) {
	_, store, _, n := newRecoveryNotifier(t, "")
	store.setUptimeFails = -1
	now := time.Now().UTC()
	view := apiView
	view.OpenedAt = now.Add(-time.Hour)
	if err := n.Notify(context.Background(), "ops", nil, view,
		&alert.Event{Type: alert.EventReminder, At: now, StatusCode: 503, Error: "still down"},
	); err != nil {
		t.Fatalf("Notify(reminder): %v", err)
	}
	if len(store.openFailedMarks) != 1 {
		t.Errorf("open-failed marks: got %v, want [api]", store.openFailedMarks)
	}
}

// TestNotifier_SSLOpen_threadRefWriteFailsOnce_isRetried: SSL parents
// get the same retry. An SSL recovery with no ref already posts a
// standalone resolve, so no mark is needed when the retry runs out.
func TestNotifier_SSLOpen_threadRefWriteFailsOnce_isRetried(t *testing.T) {
	_, store, _, n := newRecoveryNotifier(t, "")
	store.setSSLFails = 1
	now := time.Now().UTC()
	expires := now.Add(72 * time.Hour)
	if err := n.NotifySSL(context.Background(), "ops", nil, apiView, slack.SSLView{ExpiresAt: expires},
		&alert.SSLEvent{Type: alert.EventSSLOpen, At: now, ExpiresAt: expires},
	); err != nil {
		t.Fatalf("NotifySSL(open): %v", err)
	}
	if store.setSSLCalls != 2 {
		t.Errorf("SetSSLThread calls: got %d, want 2 (one retry)", store.setSSLCalls)
	}
}

// TestNotifier_Open_ofResumedIncident_datesFromOpenedAt: the Open a
// resumed dependsOn child is announced with belongs to an incident that
// opened before its parent went down. The DOWN dates the failure from
// the incident's start, not from the tick that announced it.
func TestNotifier_Open_ofResumedIncident_datesFromOpenedAt(t *testing.T) {
	f, _, _, n := newRecoveryNotifier(t, "")
	now := time.Now().UTC()
	opened := now.Add(-time.Hour)
	view := apiView
	view.OpenedAt = opened
	if err := n.Notify(context.Background(), "ops", nil, view, openEvent(now)); err != nil {
		t.Fatalf("Notify(open): %v", err)
	}
	flat := flattenMrkdwn(t, findPostedMessage(t, f))
	if want := fmt.Sprintf("date^%d^", opened.Unix()); !strings.Contains(flat, want) {
		t.Errorf("want the DOWN dated %s; got:\n%s", want, flat)
	}
}
