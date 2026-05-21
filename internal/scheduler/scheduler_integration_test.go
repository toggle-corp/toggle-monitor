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
