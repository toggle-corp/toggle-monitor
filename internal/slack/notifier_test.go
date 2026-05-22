package slack_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/alert"
	"github.com/toggle-corp/toggle-monitor/internal/secret"
	"github.com/toggle-corp/toggle-monitor/internal/slack"
)

// fakeStore is a minimal ThreadStore the notifier can be wired to.
// Records SetUptimeThread / SetSSLThread calls so tests can confirm
// thread refs would be persisted, and answers ListChildrenOf from a
// pre-seeded map.
type fakeStore struct {
	children map[string][]string

	setUptimeCalls int
	setSSLCalls    int
}

func (f *fakeStore) SetUptimeThread(_ context.Context, _, _, _ string) error {
	f.setUptimeCalls++
	return nil
}
func (f *fakeStore) SetSSLThread(_ context.Context, _, _, _ string) error {
	f.setSSLCalls++
	return nil
}
func (f *fakeStore) ListChildrenOf(_ context.Context, slug string) ([]string, error) {
	return f.children[slug], nil
}

// findPostedMessage returns the body of the first chat.postMessage
// received by the fake Slack recorder. Fails the test if none was
// observed.
func findPostedMessage(t *testing.T, f *fakeSlack) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, req := range f.requests {
		if req.Method == "chat.postMessage" {
			return req.Body
		}
	}
	t.Fatalf("no chat.postMessage observed; got %d requests", len(f.requests))
	return nil
}

// flattenMrkdwn pulls every "text" string out of the message
// payload — across blocks, attachments, and nested context elements —
// so a test can grep across the whole rendered structure with a
// single Contains.
func flattenMrkdwn(t *testing.T, body map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal posted body: %v", err)
	}
	return string(raw)
}

// TestNotifier_EventOpen_includesDependentsNoteWhenChildrenExist
// is the user-facing scenario: parent monitor A has children B and C
// that depend on it. When A goes down and a Slack alert posts, the
// alert must carry a small "Pauses dependents: `B`, `C`" note so
// operators understand the cascading effect.
func TestNotifier_EventOpen_includesDependentsNoteWhenChildrenExist(t *testing.T) {
	f, srv := newFakeSlack(t)
	client := slack.NewClient(slack.WithBaseURL(srv.URL))

	store := &fakeStore{children: map[string][]string{
		"parent-api": {"child-a", "child-b"},
	}}

	notifier := slack.NewNotifier(slack.NotifierOptions{
		Client: client,
		Store:  store,
		Channels: func(string) (slack.ChannelInfo, bool) {
			return slack.ChannelInfo{ID: "C0123", Token: secret.SecretString("xoxb-test")}, true
		},
		BodyMaxChars: 500,
	})

	if err := notifier.Notify(context.Background(), "ops",
		[]string{"<!here>"},
		slack.MonitorView{
			Slug: "parent-api", FriendlyName: "parent-api",
			GroupSlug: "core", URL: "https://api/health",
			StatusCode: 503, StatusText: "Service Unavailable",
		},
		&alert.Event{
			Type: alert.EventOpen, At: time.Now().UTC(),
			StatusCode: 503, Error: "timeout",
		},
	); err != nil {
		t.Fatalf("Notify(open): %v", err)
	}

	body := findPostedMessage(t, f)
	flat := flattenMrkdwn(t, body)

	if !strings.Contains(flat, "Pauses dependents") {
		t.Errorf("expected 'Pauses dependents' note in posted message; got:\n%s", flat)
	}
	if !strings.Contains(flat, "`child-a`") || !strings.Contains(flat, "`child-b`") {
		t.Errorf("expected backtick-wrapped child slugs in note; got:\n%s", flat)
	}
	// Sanity: thread ref was persisted.
	if store.setUptimeCalls != 1 {
		t.Errorf("expected SetUptimeThread to be called once; got %d", store.setUptimeCalls)
	}
}

// TestNotifier_EventOpen_omitsDependentsNoteWhenNoChildren confirms
// the common case (most monitors are leaves) — no note line should
// appear when the store reports zero children.
func TestNotifier_EventOpen_omitsDependentsNoteWhenNoChildren(t *testing.T) {
	f, srv := newFakeSlack(t)
	client := slack.NewClient(slack.WithBaseURL(srv.URL))

	store := &fakeStore{children: map[string][]string{}} // no entries

	notifier := slack.NewNotifier(slack.NotifierOptions{
		Client: client,
		Store:  store,
		Channels: func(string) (slack.ChannelInfo, bool) {
			return slack.ChannelInfo{ID: "C0123", Token: secret.SecretString("xoxb-test")}, true
		},
	})

	if err := notifier.Notify(context.Background(), "ops", nil,
		slack.MonitorView{
			Slug: "leaf-api", FriendlyName: "leaf-api",
			GroupSlug: "core", URL: "https://api/health",
			StatusCode: 500, StatusText: "x",
		},
		&alert.Event{Type: alert.EventOpen, At: time.Now().UTC(), StatusCode: 500, Error: "boom"},
	); err != nil {
		t.Fatalf("Notify(open): %v", err)
	}

	flat := flattenMrkdwn(t, findPostedMessage(t, f))
	if strings.Contains(flat, "Pauses dependents") || strings.Contains(flat, "Resumes dependents") {
		t.Errorf("leaf monitor should not surface a dependents note; got:\n%s", flat)
	}
}

// TestFormatDependentsNote_truncatesPastMax confirms the cap kicks in
// at `max` entries and the remainder collapses into "…and N more".
func TestFormatDependentsNote_truncatesPastMax(t *testing.T) {
	all := []string{"a", "b", "c", "d", "e", "f", "g", "h"}

	// max = 3 → first 3 shown, 5 collapsed.
	line := slack.FormatDependentsNote("⏸ Pauses dependents", all, 3)
	for _, want := range []string{"`a`", "`b`", "`c`", "…and 5 more"} {
		if !strings.Contains(line, want) {
			t.Errorf("expected %q in %q", want, line)
		}
	}
	for _, gone := range []string{"`d`", "`e`", "`f`", "`g`", "`h`"} {
		if strings.Contains(line, gone) {
			t.Errorf("did not expect %q in truncated line %q", gone, line)
		}
	}

	// max ≥ len → no tail.
	full := slack.FormatDependentsNote("⏸ Pauses dependents", all, 10)
	if strings.Contains(full, "more") {
		t.Errorf("did not expect a '…and N more' tail when max ≥ len; got %q", full)
	}

	// Empty slice → empty line.
	if got := slack.FormatDependentsNote("⏸ Pauses dependents", nil, 3); got != "" {
		t.Errorf("empty slugs → empty line, got %q", got)
	}

	// max=0 → falls back to DefaultDependentsNoteMax.
	defaulted := slack.FormatDependentsNote("⏸ Pauses dependents", all, 0)
	if !strings.Contains(defaulted, "…and") {
		t.Errorf("expected default cap to truncate 8 entries; got %q", defaulted)
	}
}

// TestNotifier_EventOpen_truncatesDependentsNoteAtConfiguredMax wires
// the DependentsNoteMax option through NewNotifier and confirms the
// posted message respects the configured cap.
func TestNotifier_EventOpen_truncatesDependentsNoteAtConfiguredMax(t *testing.T) {
	f, srv := newFakeSlack(t)
	client := slack.NewClient(slack.WithBaseURL(srv.URL))

	store := &fakeStore{children: map[string][]string{
		"parent-api": {"a", "b", "c", "d", "e", "f", "g"},
	}}

	notifier := slack.NewNotifier(slack.NotifierOptions{
		Client: client,
		Store:  store,
		Channels: func(string) (slack.ChannelInfo, bool) {
			return slack.ChannelInfo{ID: "C0123", Token: secret.SecretString("xoxb-test")}, true
		},
		DependentsNoteMax: 2,
	})

	if err := notifier.Notify(context.Background(), "ops", nil,
		slack.MonitorView{
			Slug: "parent-api", FriendlyName: "parent-api",
			GroupSlug: "core", URL: "https://api/health",
			StatusCode: 503, StatusText: "Service Unavailable",
		},
		&alert.Event{Type: alert.EventOpen, At: time.Now().UTC(), StatusCode: 503, Error: "boom"},
	); err != nil {
		t.Fatalf("Notify(open): %v", err)
	}

	flat := flattenMrkdwn(t, findPostedMessage(t, f))
	if !strings.Contains(flat, "`a`") || !strings.Contains(flat, "`b`") {
		t.Errorf("expected first 2 child slugs in note; got:\n%s", flat)
	}
	if strings.Contains(flat, "`c`") {
		t.Errorf("did not expect 3rd slug past the cap; got:\n%s", flat)
	}
	if !strings.Contains(flat, "…and 5 more") {
		t.Errorf("expected '…and 5 more' tail; got:\n%s", flat)
	}
}

// TestNotifier_EventResolve_includesResumesNote confirms the resolve
// edit also carries the cascading-effect line, with the resume verb.
func TestNotifier_EventResolve_includesResumesNote(t *testing.T) {
	f, srv := newFakeSlack(t)
	client := slack.NewClient(slack.WithBaseURL(srv.URL))

	store := &fakeStore{children: map[string][]string{
		"parent-api": {"child-a"},
	}}

	notifier := slack.NewNotifier(slack.NotifierOptions{
		Client: client,
		Store:  store,
		Channels: func(string) (slack.ChannelInfo, bool) {
			return slack.ChannelInfo{ID: "C0123", Token: secret.SecretString("xoxb-test")}, true
		},
	})

	mv := slack.MonitorView{
		Slug: "parent-api", FriendlyName: "parent-api",
		GroupSlug: "core", URL: "https://api/health",
		StatusCode: 503, StatusText: "Service Unavailable",
		OpenedAt:            time.Now().UTC().Add(-12 * time.Minute),
		UptimeThreadChannel: "C0123", UptimeThreadTS: "1700000000.000100",
	}
	if err := notifier.Notify(context.Background(), "ops", nil, mv,
		&alert.Event{Type: alert.EventResolve, At: time.Now().UTC(), Downtime: 12 * time.Minute},
	); err != nil {
		t.Fatalf("Notify(resolve): %v", err)
	}

	// The resolve path does both an UpdateMessage (parent edit) and a
	// PostMessage (thread reply). The cascading note lives on the
	// edited parent.
	f.mu.Lock()
	defer f.mu.Unlock()
	var editBody map[string]any
	for _, req := range f.requests {
		if req.Method == "chat.update" {
			editBody = req.Body
			break
		}
	}
	if editBody == nil {
		t.Fatalf("no chat.update observed for resolve edit; got %d requests", len(f.requests))
	}
	flat := flattenMrkdwn(t, editBody)
	if !strings.Contains(flat, "Resumes dependents") {
		t.Errorf("expected 'Resumes dependents' note in resolve-edit; got:\n%s", flat)
	}
	if !strings.Contains(flat, "`child-a`") {
		t.Errorf("expected backtick-wrapped child slug in resolve-edit; got:\n%s", flat)
	}
}
