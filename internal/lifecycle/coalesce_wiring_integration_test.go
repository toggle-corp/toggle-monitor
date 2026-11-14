//go:build integration

package lifecycle

import (
	"context"
	"log/slog"
	"sync"
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
