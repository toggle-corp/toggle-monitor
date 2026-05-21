//go:build integration

package scheduler_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toggle-corp/toggle-monitor/internal/alert"
	"github.com/toggle-corp/toggle-monitor/internal/httpcheck"
	"github.com/toggle-corp/toggle-monitor/internal/migrate"
	"github.com/toggle-corp/toggle-monitor/internal/scheduler"
	"github.com/toggle-corp/toggle-monitor/internal/store"
	"github.com/toggle-corp/toggle-monitor/internal/testpg"
)

func newRepo(t *testing.T) *store.Repo {
	t.Helper()
	dsn := testpg.Start(t)
	if err := migrate.Up(dsn); err != nil {
		t.Fatalf("migrate.Up: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return store.New(pool)
}

// TestTick_endToEndUptimeLifecycle drives the scheduler with a
// programmable check function across three ticks: down, still down,
// then resolved. Verifies that:
//   - the first failure transitions the monitor to down and writes one
//     alert_event of type 'open'
//   - the second failure does NOT write an additional event
//   - the recovery writes one alert_event of type 'resolve' with a
//     downtime spanning the two failed ticks.
func TestTick_endToEndUptimeLifecycle(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()

	if err := repo.ReconcileMonitor(ctx, store.MonitorSpec{
		Slug: "api", FriendlyName: "API", URL: "http://api/health",
		GroupSlug: "prod", Source: store.SourceStatic,
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Programmable check: alternates fail / fail / ok based on the
	// per-test call count.
	results := []httpcheck.Result{
		{StatusCode: 500, Error: "boom"},
		{StatusCode: 500, Error: "boom"},
		{StatusCode: 200},
	}
	var calls atomic.Int32
	check := func(ctx context.Context, _ httpcheck.Config) httpcheck.Result {
		i := calls.Add(1) - 1
		return results[i]
	}

	// Inject deterministic clock advancing 1 minute per tick.
	now := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	tickClock := func() time.Time {
		c := now
		now = now.Add(time.Minute)
		return c
	}

	s := scheduler.New(repo,
		scheduler.WithCheck(check),
		scheduler.WithNow(tickClock),
	)

	plan := scheduler.Plan{
		Slug: "api", URL: "http://api/health", HTTPMethod: "GET",
		AcceptedStatusCodes: []int{200},
		Interval:            5 * time.Minute, Timeout: 2 * time.Second,
		Retries: 0, RetryBackoff: time.Second,
	}

	for tick := 1; tick <= 3; tick++ {
		if err := s.Tick(ctx, plan); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
	}

	events, err := repo.ListAlertsForMonitor(ctx, "api", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("event count: got %d, want 2 (open + resolve)", len(events))
	}
	if events[0].Type != alert.EventResolve {
		t.Errorf("events[0].Type: got %q, want resolve", events[0].Type)
	}
	if events[1].Type != alert.EventOpen {
		t.Errorf("events[1].Type: got %q, want open", events[1].Type)
	}
	// Downtime = 2 minutes (down at minute 0, up at minute 2).
	if events[0].DowntimeSeconds == nil || *events[0].DowntimeSeconds != 120 {
		t.Errorf("downtime: got %v, want 120", events[0].DowntimeSeconds)
	}

	final, err := repo.GetMonitor(ctx, "api")
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != alert.StatusUp {
		t.Errorf("final status: got %q, want up", final.Status)
	}
}

// TestTick_dependsOn_pausesChildWhenParentDown verifies the runtime
// gate: while any parent is StatusDown the child is marked
// temporary-paused, the probe is skipped, and no alert event is
// written. When the parent recovers, the child resumes and a fresh
// failing tick produces an open event (i.e. resuming-from-paused does
// not double-emit transitions).
func TestTick_dependsOn_pausesChildWhenParentDown(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()

	for _, slug := range []string{"parent", "child"} {
		if err := repo.ReconcileMonitor(ctx, store.MonitorSpec{
			Slug: slug, FriendlyName: slug, URL: "http://x", GroupSlug: "g", Source: store.SourceStatic,
		}); err != nil {
			t.Fatalf("reconcile %s: %v", slug, err)
		}
	}
	// Knock the parent down.
	t0 := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	if err := repo.ApplyCheck(ctx, "parent",
		alert.State{Status: alert.StatusDown, OpenedAt: t0},
		t0, 503, "down",
		&alert.Event{Type: alert.EventOpen, At: t0, StatusCode: 503, Error: "down"},
	); err != nil {
		t.Fatal(err)
	}

	// CheckFunc that fails — but should not be invoked while paused.
	var called atomic.Int32
	check := func(ctx context.Context, _ httpcheck.Config) httpcheck.Result {
		called.Add(1)
		return httpcheck.Result{StatusCode: 500, Error: "would-fail"}
	}
	s := scheduler.New(repo, scheduler.WithCheck(check))

	plan := scheduler.Plan{
		Slug: "child", URL: "x", HTTPMethod: "GET",
		AcceptedStatusCodes: []int{200},
		Interval:            5 * time.Minute, Timeout: time.Second,
		Retries: 0, RetryBackoff: time.Second,
		DependsOn: []string{"parent"},
	}
	if err := s.Tick(ctx, plan); err != nil {
		t.Fatalf("paused tick: %v", err)
	}
	if called.Load() != 0 {
		t.Errorf("probe should NOT be called while paused, but was called %d time(s)", called.Load())
	}

	row, err := repo.GetMonitor(ctx, "child")
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != alert.StatusTemporaryPaused {
		t.Errorf("child status: got %q, want %q", row.Status, alert.StatusTemporaryPaused)
	}
	if events, _ := repo.ListAlertsForMonitor(ctx, "child", 10); len(events) != 0 {
		t.Errorf("paused tick must not write alert_events; got %d", len(events))
	}

	// Bring the parent back up.
	t1 := t0.Add(5 * time.Minute)
	if err := repo.ApplyCheck(ctx, "parent",
		alert.State{Status: alert.StatusUp},
		t1, 200, "",
		&alert.Event{Type: alert.EventResolve, At: t1, Downtime: 5 * time.Minute},
	); err != nil {
		t.Fatal(err)
	}

	// Child's next tick should actually run the probe and, since
	// it's failing, produce a fresh open.
	if err := s.Tick(ctx, plan); err != nil {
		t.Fatalf("resumed tick: %v", err)
	}
	if called.Load() != 1 {
		t.Errorf("probe should run once after resume, called %d time(s)", called.Load())
	}
	events, _ := repo.ListAlertsForMonitor(ctx, "child", 10)
	if len(events) != 1 || events[0].Type != alert.EventOpen {
		t.Errorf("expected exactly 1 open event after resume, got %d", len(events))
	}
}

// TestTick_inCycleRetriesSuppressTransientFailure: if the first probe
// fails but a retry within the same tick succeeds, the tick records
// success — no alert event is emitted for the transient failure.
func TestTick_inCycleRetriesSuppressTransientFailure(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	if err := repo.ReconcileMonitor(ctx, store.MonitorSpec{
		Slug: "api", FriendlyName: "API", URL: "http://api/health",
		GroupSlug: "prod", Source: store.SourceStatic,
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	calls := []httpcheck.Result{
		{StatusCode: 500, Error: "transient"},
		{StatusCode: 200},
	}
	var idx atomic.Int32
	check := func(ctx context.Context, _ httpcheck.Config) httpcheck.Result {
		return calls[idx.Add(1)-1]
	}

	s := scheduler.New(repo, scheduler.WithCheck(check))

	plan := scheduler.Plan{
		Slug: "api", URL: "x", HTTPMethod: "GET",
		AcceptedStatusCodes: []int{200},
		Interval:            5 * time.Minute, Timeout: time.Second,
		Retries: 1, RetryBackoff: time.Millisecond,
	}

	if err := s.Tick(ctx, plan); err != nil {
		t.Fatalf("tick: %v", err)
	}

	events, err := repo.ListAlertsForMonitor(ctx, "api", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("expected zero events (transient failure suppressed by retry), got %d", len(events))
	}
	if got := idx.Load(); got != 2 {
		t.Errorf("expected 2 probes (initial + 1 retry), got %d", got)
	}

	row, err := repo.GetMonitor(ctx, "api")
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != alert.StatusUp {
		t.Errorf("status: got %q, want up", row.Status)
	}
}
