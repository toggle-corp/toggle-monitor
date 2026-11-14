//go:build integration

package scheduler_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/alert"
	"github.com/toggle-corp/toggle-monitor/internal/probe"
	"github.com/toggle-corp/toggle-monitor/internal/scheduler"
	"github.com/toggle-corp/toggle-monitor/internal/store"
)

// nestedPlan is a monitor in a root ← mid ← leaf tree: it dependsOn
// parents and posts to its own channel.
func nestedPlan(slug string, prober probe.Prober, parents ...string) scheduler.Plan {
	return scheduler.Plan{
		Slug: slug, URL: "http://" + slug + ".example.test", HTTPMethod: "GET",
		AcceptedStatusCodes: []int{200},
		Interval:            5 * time.Minute, Timeout: time.Second,
		ReminderInterval: 24 * time.Hour,
		ChannelSlug:      slug + "-alerts",
		DependsOn:        parents,
		Prober:           prober,
	}
}

// assertPausedSilently checks that slug ended its tick as a paused one:
// temporary-paused, no incident, no alert event, nothing routed or sent,
// and pulled from its channel's dispatcher state.
func assertPausedSilently(t *testing.T, repo *store.Repo, groups *recordingGroupSink, sent []alert.EventType, slug string) {
	t.Helper()
	ctx := context.Background()
	row, err := repo.GetMonitor(ctx, slug)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != alert.StatusTemporaryPaused {
		t.Errorf("%s status: got %q, want %q", slug, row.Status, alert.StatusTemporaryPaused)
	}
	if row.OpenedAt != nil {
		t.Errorf("%s opened an incident at %v; want none", slug, *row.OpenedAt)
	}
	if listing, _ := repo.ListAlertsForMonitor(ctx, slug, 10, 0); len(listing.Items) != 0 {
		t.Errorf("%s alert events: got %d, want 0", slug, len(listing.Items))
	}
	routed, paused := groups.snapshot()
	if len(routed) != 0 || len(sent) != 0 {
		t.Errorf("%s events dispatched: routed %v, sent %v; want none", slug, routed, sent)
	}
	if len(paused) != 1 || paused[0] != slug {
		t.Errorf("dispatcher pauses: got %v, want [%s]", paused, slug)
	}
}

func newRecordingScheduler(repo *store.Repo, groups *recordingGroupSink, sent *[]alert.EventType) *scheduler.Scheduler {
	return scheduler.New(repo,
		scheduler.WithGroupSink(groups),
		scheduler.WithEventSink(func(_ context.Context, _ store.MonitorRow, _ string, _ []string, ev *alert.Event) error {
			*sent = append(*sent, ev.Type)
			return nil
		}),
	)
}

// TestTick_dependsOn_parentPausedBehindItsParent_pausesChild: root is
// down and mid, which dependsOn root, is paused behind it. leaf
// dependsOn mid only. mid is as unreachable as root, so leaf is held
// exactly as if mid were down: no probe, no incident, nothing sent.
func TestTick_dependsOn_parentPausedBehindItsParent_pausesChild(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	reconcileAll(t, repo, "root", "mid", "leaf")
	openIncident(t, repo, "root", time.Now())
	if err := repo.MarkTemporaryPaused(ctx, "mid"); err != nil {
		t.Fatal(err)
	}

	var probes atomic.Int32
	prober := constProber{res: probe.Result{Code: 503, Error: "root's outage"}, calls: &probes}
	groups := &recordingGroupSink{}
	var sent []alert.EventType
	s := newRecordingScheduler(repo, groups, &sent)
	if err := s.Tick(ctx, nestedPlan("leaf", prober, "mid")); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if n := probes.Load(); n != 0 {
		t.Errorf("leaf probed %d time(s) behind a paused parent; want 0", n)
	}
	assertPausedSilently(t, repo, groups, sent, "leaf")
}

// TestTick_dependsOn_parentPausedMidProbe_pausesChild: leaf passes its
// gate while mid is up, and root's outage pauses mid while leaf's probe
// is out. leaf's failure is root's outage, so the re-check after the
// probe holds it like a paused tick.
func TestTick_dependsOn_parentPausedMidProbe_pausesChild(t *testing.T) {
	for _, critical := range []bool{false, true} {
		name := "coalesced"
		if critical {
			name = "critical"
		}
		t.Run(name, func(t *testing.T) {
			repo := newRepo(t)
			ctx := context.Background()
			reconcileAll(t, repo, "root", "mid", "leaf")

			prober := proberFunc(func(context.Context) probe.Result {
				openIncident(t, repo, "root", time.Now())
				if err := repo.MarkTemporaryPaused(ctx, "mid"); err != nil {
					t.Error(err)
				}
				return probe.Result{Code: 503, Error: "root's outage"}
			})
			groups := &recordingGroupSink{}
			var sent []alert.EventType
			s := newRecordingScheduler(repo, groups, &sent)
			plan := nestedPlan("leaf", prober, "mid")
			plan.Critical = critical
			if err := s.Tick(ctx, plan); err != nil {
				t.Fatalf("tick: %v", err)
			}
			assertPausedSilently(t, repo, groups, sent, "leaf")
		})
	}
}

// TestTick_dependsOn_diamond_anyHeldParentPausesChild: a child with two
// parents is held when either one is down or paused behind its own
// parent, and probes normally once both are up.
func TestTick_dependsOn_diamond_anyHeldParentPausesChild(t *testing.T) {
	cases := []struct {
		name      string
		left      alert.Status
		right     alert.Status
		wantPause bool
	}{
		{name: "both up", left: alert.StatusUp, right: alert.StatusUp},
		{name: "left down", left: alert.StatusDown, right: alert.StatusUp, wantPause: true},
		{name: "left paused", left: alert.StatusTemporaryPaused, right: alert.StatusUp, wantPause: true},
		{name: "right paused", left: alert.StatusUp, right: alert.StatusTemporaryPaused, wantPause: true},
		{name: "both paused", left: alert.StatusTemporaryPaused, right: alert.StatusTemporaryPaused, wantPause: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newRepo(t)
			ctx := context.Background()
			reconcileAll(t, repo, "left", "right", "leaf")
			for slug, status := range map[string]alert.Status{"left": tc.left, "right": tc.right} {
				switch status {
				case alert.StatusDown:
					openIncident(t, repo, slug, time.Now())
				case alert.StatusTemporaryPaused:
					if err := repo.MarkTemporaryPaused(ctx, slug); err != nil {
						t.Fatal(err)
					}
				}
			}

			var probes atomic.Int32
			prober := constProber{res: probe.Result{Code: 503, Error: "leaf's own failure"}, calls: &probes}
			groups := &recordingGroupSink{}
			var sent []alert.EventType
			s := newRecordingScheduler(repo, groups, &sent)
			if err := s.Tick(ctx, nestedPlan("leaf", prober, "left", "right")); err != nil {
				t.Fatalf("tick: %v", err)
			}
			if tc.wantPause {
				if n := probes.Load(); n != 0 {
					t.Errorf("leaf probed %d time(s) behind a held parent; want 0", n)
				}
				assertPausedSilently(t, repo, groups, sent, "leaf")
				return
			}
			routed, paused := groups.snapshot()
			if len(routed) != 1 || routed[0] != alert.EventOpen {
				t.Errorf("routed: got %v, want [open]", routed)
			}
			if len(paused) != 0 {
				t.Errorf("paused: got %v, want none", paused)
			}
		})
	}
}

// TestTick_dependsOn_missingParentDoesNotHold: a parent with no row is
// skipped and reported on /issues; a child failing on its own behind it
// opens its incident.
func TestTick_dependsOn_missingParentDoesNotHold(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	reconcileAll(t, repo, "leaf")

	groups := &recordingGroupSink{}
	var sent []alert.EventType
	s := newRecordingScheduler(repo, groups, &sent)
	prober := constProber{res: probe.Result{Code: 503, Error: "leaf's own failure"}}
	if err := s.Tick(ctx, nestedPlan("leaf", prober, "ghost")); err != nil {
		t.Fatalf("tick: %v", err)
	}
	routed, _ := groups.snapshot()
	if len(routed) != 1 || routed[0] != alert.EventOpen {
		t.Errorf("routed: got %v, want [open]", routed)
	}
	missing := s.MissingParents()
	if len(missing) != 1 || missing[0].Parent != "ghost" || len(missing[0].Children) != 1 || missing[0].Children[0] != "leaf" {
		t.Errorf("missing parents: got %+v, want ghost ← leaf", missing)
	}
}
