//go:build integration

package scheduler_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/alert"
	"github.com/toggle-corp/toggle-monitor/internal/probe"
	"github.com/toggle-corp/toggle-monitor/internal/scheduler"
	"github.com/toggle-corp/toggle-monitor/internal/store"
)

// recordingGroupSink records what the scheduler hands the dispatcher.
type recordingGroupSink struct {
	mu     sync.Mutex
	routed []alert.EventType
	paused []string
}

func (r *recordingGroupSink) Down(context.Context, string, scheduler.GroupMember, time.Time) {}
func (r *recordingGroupSink) Up(context.Context, string, string, time.Time)                  {}

func (r *recordingGroupSink) Pause(_ context.Context, _, slug string, _ time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paused = append(r.paused, slug)
}

func (r *recordingGroupSink) Route(_ context.Context, _ string, _ scheduler.GroupMember, _ store.MonitorRow, _ []string, ev *alert.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routed = append(r.routed, ev.Type)
}

func (r *recordingGroupSink) snapshot() (routed []alert.EventType, paused []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]alert.EventType(nil), r.routed...), append([]string(nil), r.paused...)
}

// proberFunc adapts a func to probe.Prober.
type proberFunc func(context.Context) probe.Result

func (f proberFunc) Probe(ctx context.Context) probe.Result { return f(ctx) }

func reconcileAll(t *testing.T, repo *store.Repo, slugs ...string) {
	t.Helper()
	for _, slug := range slugs {
		if err := repo.ReconcileMonitor(context.Background(), store.MonitorSpec{
			Slug: slug, FriendlyName: slug, URL: "http://" + slug + ".example.test", Source: store.SourceStatic,
		}); err != nil {
			t.Fatalf("reconcile %s: %v", slug, err)
		}
	}
}

func openIncident(t *testing.T, repo *store.Repo, slug string, at time.Time) {
	t.Helper()
	if err := repo.ApplyCheck(context.Background(), slug,
		alert.State{Status: alert.StatusDown, OpenedAt: at, LastReminderAt: at},
		at, 503, "down",
		&alert.Event{Type: alert.EventOpen, At: at, StatusCode: 503, Error: "down"},
	); err != nil {
		t.Fatalf("open %s: %v", slug, err)
	}
}

func childPlan(prober probe.Prober, critical bool) scheduler.Plan {
	return scheduler.Plan{
		Slug: "child", URL: "http://child.example.test", HTTPMethod: "GET",
		AcceptedStatusCodes: []int{200},
		Interval:            5 * time.Minute, Timeout: time.Second,
		ReminderInterval: 24 * time.Hour,
		ChannelSlug:      "ops",
		Critical:         critical,
		DependsOn:        []string{"parent"},
		Prober:           prober,
	}
}

// TestTick_dependsOn_parentDownMidProbe_pausesChild: the child passes
// the dependsOn gate while its parent is up, and the parent goes down
// while the child's probe (with its retries) is still running. The
// child's failure belongs to the parent's outage, so the tick must end
// exactly as a paused tick would: no incident, nothing routed or sent,
// the child marked temporary-paused and pulled from the dispatcher.
func TestTick_dependsOn_parentDownMidProbe_pausesChild(t *testing.T) {
	for _, critical := range []bool{false, true} {
		name := "coalesced"
		if critical {
			name = "critical"
		}
		t.Run(name, func(t *testing.T) {
			repo := newRepo(t)
			ctx := context.Background()
			reconcileAll(t, repo, "parent", "child")

			prober := proberFunc(func(context.Context) probe.Result {
				openIncident(t, repo, "parent", time.Now())
				return probe.Result{Code: 503, Error: "parent's outage"}
			})
			groups := &recordingGroupSink{}
			var sent []alert.EventType
			s := scheduler.New(repo,
				scheduler.WithGroupSink(groups),
				scheduler.WithEventSink(func(_ context.Context, _ store.MonitorRow, _ string, _ []string, ev *alert.Event) error {
					sent = append(sent, ev.Type)
					return nil
				}),
			)
			if err := s.Tick(ctx, childPlan(prober, critical)); err != nil {
				t.Fatalf("tick: %v", err)
			}

			row, err := repo.GetMonitor(ctx, "child")
			if err != nil {
				t.Fatal(err)
			}
			if row.Status != alert.StatusTemporaryPaused {
				t.Errorf("child status: got %q, want %q", row.Status, alert.StatusTemporaryPaused)
			}
			if row.OpenedAt != nil {
				t.Errorf("child opened an incident at %v; want none", *row.OpenedAt)
			}
			if listing, _ := repo.ListAlertsForMonitor(ctx, "child", 10, 0); len(listing.Items) != 0 {
				t.Errorf("child alert events: got %d, want 0", len(listing.Items))
			}
			routed, paused := groups.snapshot()
			if len(routed) != 0 || len(sent) != 0 {
				t.Errorf("child events dispatched: routed %v, sent %v; want none", routed, sent)
			}
			if len(paused) != 1 || paused[0] != "child" {
				t.Errorf("dispatcher pauses: got %v, want [child]", paused)
			}
		})
	}
}

// TestTick_dependsOn_parentUpAfterProbe_opensChild: the re-check after
// the probe only pauses for a parent that is down; a child failing on
// its own with every parent up opens its incident as usual.
func TestTick_dependsOn_parentUpAfterProbe_opensChild(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	reconcileAll(t, repo, "parent", "child")

	groups := &recordingGroupSink{}
	s := scheduler.New(repo, scheduler.WithGroupSink(groups))
	prober := constProber{res: probe.Result{Code: 503, Error: "child's own failure"}}
	if err := s.Tick(ctx, childPlan(prober, false)); err != nil {
		t.Fatalf("tick: %v", err)
	}
	routed, paused := groups.snapshot()
	if len(routed) != 1 || routed[0] != alert.EventOpen {
		t.Errorf("routed: got %v, want [open]", routed)
	}
	if len(paused) != 0 {
		t.Errorf("paused: got %v, want none", paused)
	}
}
