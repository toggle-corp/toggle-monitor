package slack_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/alert"
	"github.com/toggle-corp/toggle-monitor/internal/slack"
)

// holdFirstPost makes the fake Slack hold the first chat.postMessage
// until release is closed, and closes arrived when that request lands.
func holdFirstPost(f *fakeSlack) (arrived, release chan struct{}) {
	arrived, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	ok := f.respond
	f.respond = func(method string) (int, string) {
		if method == "chat.postMessage" {
			held := false
			once.Do(func() { held = true })
			if held {
				close(arrived)
				<-release
			}
		}
		return ok(method)
	}
	return arrived, release
}

func countMethod(f *fakeSlack, method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r.Method == method {
			n++
		}
	}
	return n
}

// TestNotifier_Resolve_whileOpenInFlight_editsTheOpen: the monitor
// recovers while its Open is still being posted, so the recovery's row
// snapshot carries neither a thread ts nor a failed-delivery mark. The
// recovery must wait for that post and edit the message it produced,
// not decide from the snapshot that nobody was paged and leave the DOWN
// standing.
func TestNotifier_Resolve_whileOpenInFlight_editsTheOpen(t *testing.T) {
	f, _, _, n := newRecoveryNotifier(t, "")
	arrived, release := holdFirstPost(f)
	ctx := context.Background()
	opened := time.Now().UTC().Add(-time.Minute)

	openDone := make(chan error, 1)
	go func() {
		openDone <- n.Notify(ctx, "ops", nil,
			slack.MonitorView{Slug: "api", FriendlyName: "api", URL: "https://api.example.test/"},
			&alert.Event{Type: alert.EventOpen, At: opened, StatusCode: 503, Error: "boom"},
		)
	}()
	<-arrived

	resolveDone := make(chan error, 1)
	go func() {
		resolveDone <- n.Notify(ctx, "ops", nil,
			slack.MonitorView{Slug: "api", FriendlyName: "api", URL: "https://api.example.test/", OpenedAt: opened},
			&alert.Event{Type: alert.EventResolve, At: opened.Add(time.Minute), Downtime: time.Minute},
		)
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)

	for _, done := range []chan error{openDone, resolveDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Notify: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Notify did not return")
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	var edits, replies int
	for _, r := range f.requests {
		switch {
		case r.Method == "chat.update" && r.Body["ts"] == "1700000000.000100":
			edits++
		case r.Method == "chat.postMessage" && r.Body["thread_ts"] == "1700000000.000100":
			replies++
		}
	}
	if edits != 1 || replies != 1 {
		t.Errorf("want the Open's parent edited once and replied to once, got %d edit(s), %d reply(ies) in %+v",
			edits, replies, f.requests)
	}
}

// TestNotifier_Open_afterItsResolve_postsNothing: the recovery reached
// the notifier before its Open did. The incident is over and nobody was
// paged, so the late Open must not post a DOWN that nothing will ever
// edit.
func TestNotifier_Open_afterItsResolve_postsNothing(t *testing.T) {
	f, _, _, n := newRecoveryNotifier(t, "")
	ctx := context.Background()
	opened := time.Now().UTC().Add(-time.Minute)

	if err := n.Notify(ctx, "ops", nil,
		slack.MonitorView{Slug: "api", FriendlyName: "api", URL: "https://api.example.test/", OpenedAt: opened},
		&alert.Event{Type: alert.EventResolve, At: opened.Add(time.Minute), Downtime: time.Minute},
	); err != nil {
		t.Fatalf("Notify(resolve): %v", err)
	}
	if err := n.Notify(ctx, "ops", nil,
		slack.MonitorView{Slug: "api", FriendlyName: "api", URL: "https://api.example.test/"},
		&alert.Event{Type: alert.EventOpen, At: opened, StatusCode: 503, Error: "boom"},
	); err != nil {
		t.Fatalf("Notify(open): %v", err)
	}
	if got := slackCalls(f); got != 0 {
		t.Errorf("Slack calls: got %d, want 0", got)
	}

	// A later incident of the same monitor is announced as usual.
	if err := n.Notify(ctx, "ops", nil,
		slack.MonitorView{Slug: "api", FriendlyName: "api", URL: "https://api.example.test/"},
		&alert.Event{Type: alert.EventOpen, At: opened.Add(5 * time.Minute), StatusCode: 503, Error: "boom"},
	); err != nil {
		t.Fatalf("Notify(second open): %v", err)
	}
	if got := countMethod(f, "chat.postMessage"); got != 1 {
		t.Errorf("second incident's DOWN posts: got %d, want 1", got)
	}
}

// TestNotifier_Reminder_afterOpenWithStaleRow_threadsOntoTheOpen: a
// reminder whose row snapshot predates the stored thread ref replies in
// the Open's thread rather than posting a second DOWN parent.
func TestNotifier_Reminder_afterOpenWithStaleRow_threadsOntoTheOpen(t *testing.T) {
	f, _, _, n := newRecoveryNotifier(t, "")
	ctx := context.Background()
	opened := time.Now().UTC().Add(-time.Hour)

	if err := n.Notify(ctx, "ops", nil,
		slack.MonitorView{Slug: "api", FriendlyName: "api", URL: "https://api.example.test/"},
		&alert.Event{Type: alert.EventOpen, At: opened, StatusCode: 503, Error: "boom"},
	); err != nil {
		t.Fatalf("Notify(open): %v", err)
	}
	if err := n.Notify(ctx, "ops", nil,
		slack.MonitorView{Slug: "api", FriendlyName: "api", URL: "https://api.example.test/", OpenedAt: opened},
		&alert.Event{Type: alert.EventReminder, At: opened.Add(time.Hour), StatusCode: 503, Error: "still down"},
	); err != nil {
		t.Fatalf("Notify(reminder): %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var parents, replies int
	for _, r := range f.requests {
		if r.Method != "chat.postMessage" {
			continue
		}
		if r.Body["thread_ts"] == "1700000000.000100" {
			replies++
		} else {
			parents++
		}
	}
	if parents != 1 || replies != 1 {
		t.Errorf("want 1 DOWN parent and 1 threaded reminder, got %d parent(s), %d reply(ies)", parents, replies)
	}
}
