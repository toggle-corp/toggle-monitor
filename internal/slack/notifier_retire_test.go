package slack_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/alert"
	"github.com/toggle-corp/toggle-monitor/internal/slack"
)

// TestNotifier_NotifyRemoved_dropsDeliveryRecords: kube-discovered
// monitors come and go for the life of the process, so a removed
// monitor's delivery record must not outlive it.
func TestNotifier_NotifyRemoved_dropsDeliveryRecords(t *testing.T) {
	_, _, _, n := newRecoveryNotifier(t, "")
	ctx := context.Background()
	opened := time.Now().UTC().Add(-time.Minute)

	const monitors = 5
	for i := range monitors {
		slug := fmt.Sprintf("app-%d", i)
		if err := n.Notify(ctx, "ops", nil,
			slack.MonitorView{Slug: slug, FriendlyName: slug, URL: "https://" + slug + ".example.test/"},
			&alert.Event{Type: alert.EventOpen, At: opened, StatusCode: 503, Error: "boom"},
		); err != nil {
			t.Fatalf("Notify(%s): %v", slug, err)
		}
	}
	if got := n.DeliveryRecords(); got != monitors {
		t.Fatalf("delivery records after the Opens: got %d, want %d", got, monitors)
	}
	for i := range monitors {
		slug := fmt.Sprintf("app-%d", i)
		n.NotifyRemoved(ctx, "ops",
			slack.MonitorView{Slug: slug, FriendlyName: slug, URL: "https://" + slug + ".example.test/", OpenedAt: opened},
			"k8s ingress removed", "k8s ingress (default/"+slug+")")
	}
	if got := n.DeliveryRecords(); got != 0 {
		t.Errorf("delivery records after the removals: got %d, want 0", got)
	}
}

// inFlightPeak wraps the fake Slack so it tracks how many requests are
// being served at once.
func inFlightPeak(f *fakeSlack) func() int {
	var mu sync.Mutex
	cur, peak := 0, 0
	next := f.respond
	f.respond = func(method string) (int, string) {
		mu.Lock()
		cur++
		peak = max(peak, cur)
		mu.Unlock()
		defer func() {
			mu.Lock()
			cur--
			mu.Unlock()
		}()
		return next(method)
	}
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return peak
	}
}

// TestNotifier_NotifyRemoved_whileOpenInFlight: the monitor is removed
// while its Open is still being posted. The removal waits for that
// post, closes out the parent it produced, and the same slug added
// back later starts from a fresh record.
func TestNotifier_NotifyRemoved_whileOpenInFlight(t *testing.T) {
	f, _, _, n := newRecoveryNotifier(t, "")
	arrived, release := holdFirstPost(f)
	peak := inFlightPeak(f)
	ctx := context.Background()
	opened := time.Now().UTC().Add(-time.Minute)
	view := slack.MonitorView{Slug: "api", FriendlyName: "api", URL: "https://api.example.test/"}

	openDone := make(chan error, 1)
	go func() {
		openDone <- n.Notify(ctx, "ops", nil, view,
			&alert.Event{Type: alert.EventOpen, At: opened, StatusCode: 503, Error: "boom"})
	}()
	<-arrived

	removed := make(chan struct{})
	go func() {
		defer close(removed)
		snapshot := view
		snapshot.OpenedAt = opened
		n.NotifyRemoved(ctx, "ops", snapshot, "k8s ingress removed", "k8s ingress (default/api)")
	}()
	time.Sleep(50 * time.Millisecond)
	if got := peak(); got != 1 {
		t.Errorf("Slack requests in flight while the Open is held: got %d, want 1", got)
	}
	close(release)

	select {
	case err := <-openDone:
		if err != nil {
			t.Fatalf("Notify(open): %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Notify(open) did not return")
	}
	select {
	case <-removed:
	case <-time.After(5 * time.Second):
		t.Fatal("NotifyRemoved did not return")
	}
	if got := peak(); got != 1 {
		t.Errorf("Slack requests in flight at once: got %d, want 1", got)
	}

	f.mu.Lock()
	var edits, closeouts int
	for _, r := range f.requests {
		switch {
		case r.Method == "chat.update" && r.Body["ts"] == "1700000000.000100":
			edits++
		case r.Method == "chat.postMessage" && r.Body["thread_ts"] == "1700000000.000100":
			closeouts++
		}
	}
	f.mu.Unlock()
	if edits != 1 || closeouts != 1 {
		t.Errorf("want the Open's parent edited once and closed out once, got %d edit(s), %d closeout(s)", edits, closeouts)
	}
	if got := n.DeliveryRecords(); got != 0 {
		t.Errorf("delivery records after the removal: got %d, want 0", got)
	}

	// Added back under the same slug, the monitor's next incident is
	// announced as usual.
	before := countMethod(f, "chat.postMessage")
	if err := n.Notify(ctx, "ops", nil, view,
		&alert.Event{Type: alert.EventOpen, At: opened.Add(10 * time.Minute), StatusCode: 503, Error: "boom"},
	); err != nil {
		t.Fatalf("Notify(open after re-add): %v", err)
	}
	if got := countMethod(f, "chat.postMessage") - before; got != 1 {
		t.Errorf("DOWN posts after re-add: got %d, want 1", got)
	}
	if !n.UptimeParentPosted("api", opened.Add(10*time.Minute)) {
		t.Error("UptimeParentPosted after re-add: got false, want true")
	}
}
