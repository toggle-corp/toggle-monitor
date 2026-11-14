//go:build integration

package lifecycle

import (
	"context"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toggle-corp/toggle-monitor/internal/alert"
	"github.com/toggle-corp/toggle-monitor/internal/coalesce"
	"github.com/toggle-corp/toggle-monitor/internal/migrate"
	"github.com/toggle-corp/toggle-monitor/internal/probe"
	"github.com/toggle-corp/toggle-monitor/internal/scheduler"
	"github.com/toggle-corp/toggle-monitor/internal/store"
	"github.com/toggle-corp/toggle-monitor/internal/testpg"
)

type failingProber struct{}

func (failingProber) Probe(context.Context) probe.Result {
	return probe.Result{Code: 503, Error: "503 Service Unavailable", FailKind: probe.FailKindHTTP}
}

type staticPlans []scheduler.Plan

func (p staticPlans) CurrentPlans() []scheduler.Plan { return p }

type recordedSinkCall struct {
	slug    string
	channel string
	event   alert.EventType
}

// TestOnDemandParentProbe_criticalParentDown_notifiesThroughSink: a
// critical parent the dispatcher's on-demand probe finds down is
// announced through the per-monitor sink, exactly as its own scheduler
// tick would announce it. The dispatcher never sees it.
func TestOnDemandParentProbe_criticalParentDown_notifiesThroughSink(t *testing.T) {
	ctx := context.Background()
	dsn := testpg.Start(t)
	if err := migrate.Up(dsn); err != nil {
		t.Fatalf("migrate.Up: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	repo := store.New(pool)
	if err := repo.ReconcileMonitor(ctx, store.MonitorSpec{
		Slug: "root", FriendlyName: "root", URL: "https://root.example.test/", Source: store.SourceStatic,
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var mu sync.Mutex
	var calls []recordedSinkCall
	sink := func(_ context.Context, row store.MonitorRow, channel string, _ []string, ev *alert.Event) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, recordedSinkCall{slug: row.Slug, channel: channel, event: ev.Type})
		return nil
	}
	var pushed []string
	push := func(_ context.Context, parent string, _ time.Time) { pushed = append(pushed, parent) }

	mgr := coalesce.New(coalesce.Options{Store: repo, Sink: coalesce.Sink(sink)})
	plans := staticPlans{{
		Slug: "root", FriendlyName: "root", ChannelSlug: "ops", Critical: true,
		Prober: failingProber{}, Interval: time.Minute,
	}}
	probeParent := makeOnDemandParentProbe(repo, mgr, plans, push, sink, time.Second, time.Now, slog.Default())

	probeParent(ctx, "root")

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0] != (recordedSinkCall{slug: "root", channel: "ops", event: alert.EventOpen}) {
		t.Fatalf("sink calls: got %+v, want one EventOpen for root on ops", calls)
	}
	if len(pushed) != 1 || pushed[0] != "root" {
		t.Errorf("push-propagation: got %v, want [root]", pushed)
	}
	row, err := repo.GetMonitor(ctx, "root")
	if err != nil {
		t.Fatalf("get monitor: %v", err)
	}
	if row.Status != alert.StatusDown {
		t.Errorf("root status: got %q, want down", row.Status)
	}
}

// recordingPauser records the dispatcher pauses push-propagation makes.
type recordingPauser struct {
	mu     sync.Mutex
	paused []string // "channel/slug"
}

func (r *recordingPauser) Pause(_ context.Context, channel, slug string, _ time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paused = append(r.paused, channel+"/"+slug)
}

// countingProber counts its probes and always fails.
type countingProber struct{ calls *atomic.Int32 }

func (p countingProber) Probe(context.Context) probe.Result {
	p.calls.Add(1)
	return probe.Result{Code: 503, Error: "503 Service Unavailable", FailKind: probe.FailKindHTTP}
}

func newWiringRepo(t *testing.T, slugs ...string) *store.Repo {
	t.Helper()
	ctx := context.Background()
	dsn := testpg.Start(t)
	if err := migrate.Up(dsn); err != nil {
		t.Fatalf("migrate.Up: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	repo := store.New(pool)
	for _, slug := range slugs {
		if err := repo.ReconcileMonitor(ctx, store.MonitorSpec{
			Slug: slug, FriendlyName: slug, URL: "https://" + slug + ".example.test/", Source: store.SourceStatic,
		}); err != nil {
			t.Fatalf("reconcile %s: %v", slug, err)
		}
	}
	return repo
}

// TestPushPropagation_pausesEveryDescendant: root ← mid ← leaf, plus a
// diamond child of mid and side, and a child of side alone. root's
// outage pauses mid, so everything that dependsOn mid, directly or
// further down, is held too, each pulled from its own channel once.
// Monitors outside root's subtree are untouched.
func TestPushPropagation_pausesEveryDescendant(t *testing.T) {
	ctx := context.Background()
	repo := newWiringRepo(t, "root", "mid", "leaf", "diamond", "side", "side-child")
	plans := staticPlans{
		{Slug: "root", ChannelSlug: "ops"},
		{Slug: "mid", ChannelSlug: "mid-alerts", DependsOn: []string{"root"}},
		{Slug: "leaf", ChannelSlug: "leaf-alerts", DependsOn: []string{"mid"}},
		{Slug: "diamond", ChannelSlug: "leaf-alerts", DependsOn: []string{"mid", "side"}},
		{Slug: "side", ChannelSlug: "ops"},
		{Slug: "side-child", ChannelSlug: "ops", DependsOn: []string{"side"}},
	}
	disp := &recordingPauser{}
	push := makePushPropagation(repo, disp, plans, slog.Default())

	push(ctx, "root", time.Now())

	want := []string{"mid-alerts/mid", "leaf-alerts/diamond", "leaf-alerts/leaf"}
	disp.mu.Lock()
	got := append([]string(nil), disp.paused...)
	disp.mu.Unlock()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dispatcher pauses: got %v, want %v", got, want)
	}
	for slug, wantStatus := range map[string]alert.Status{
		"root":       alert.StatusUp,
		"mid":        alert.StatusTemporaryPaused,
		"leaf":       alert.StatusTemporaryPaused,
		"diamond":    alert.StatusTemporaryPaused,
		"side":       alert.StatusUp,
		"side-child": alert.StatusUp,
	} {
		row, err := repo.GetMonitor(ctx, slug)
		if err != nil {
			t.Fatal(err)
		}
		if row.Status != wantStatus {
			t.Errorf("%s status: got %q, want %q", slug, row.Status, wantStatus)
		}
	}
}

// TestOnDemandParentProbe_pausedParent_drainsChildren: the pool's hot
// parent mid is paused behind root's outage. mid is not probed or
// opened; push-propagation runs from mid so its pending children drain
// instead of flushing as individual alerts.
func TestOnDemandParentProbe_pausedParent_drainsChildren(t *testing.T) {
	ctx := context.Background()
	repo := newWiringRepo(t, "root", "mid")
	if err := repo.MarkTemporaryPaused(ctx, "mid"); err != nil {
		t.Fatal(err)
	}

	var sinkCalls atomic.Int32
	sink := func(context.Context, store.MonitorRow, string, []string, *alert.Event) error {
		sinkCalls.Add(1)
		return nil
	}
	var pushed []string
	push := func(_ context.Context, parent string, _ time.Time) { pushed = append(pushed, parent) }
	var probes atomic.Int32
	mgr := coalesce.New(coalesce.Options{Store: repo, Sink: coalesce.Sink(sink)})
	plans := staticPlans{{
		Slug: "mid", FriendlyName: "mid", ChannelSlug: "mid-alerts", Critical: true,
		DependsOn: []string{"root"}, Prober: countingProber{calls: &probes}, Interval: time.Minute,
	}}
	probeParent := makeOnDemandParentProbe(repo, mgr, plans, push, sink, time.Second, time.Now, slog.Default())

	probeParent(ctx, "mid")

	if n := probes.Load(); n != 0 {
		t.Errorf("mid probed %d time(s) while paused; want 0", n)
	}
	if n := sinkCalls.Load(); n != 0 {
		t.Errorf("sink called %d time(s); mid must not be announced", n)
	}
	if len(pushed) != 1 || pushed[0] != "mid" {
		t.Errorf("push-propagation: got %v, want [mid]", pushed)
	}
	row, err := repo.GetMonitor(ctx, "mid")
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != alert.StatusTemporaryPaused || row.OpenedAt != nil {
		t.Errorf("mid: status %q, openedAt %v; want temporary-paused with no incident", row.Status, row.OpenedAt)
	}
	if listing, _ := repo.ListAlertsForMonitor(ctx, "mid", 10, 0); len(listing.Items) != 0 {
		t.Errorf("mid alert events: got %d, want 0", len(listing.Items))
	}
}
