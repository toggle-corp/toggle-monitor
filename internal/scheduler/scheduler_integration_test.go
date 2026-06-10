//go:build integration

package scheduler_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toggle-corp/toggle-monitor/internal/alert"
	"github.com/toggle-corp/toggle-monitor/internal/migrate"
	"github.com/toggle-corp/toggle-monitor/internal/probe"
	"github.com/toggle-corp/toggle-monitor/internal/scheduler"
	"github.com/toggle-corp/toggle-monitor/internal/store"
	"github.com/toggle-corp/toggle-monitor/internal/testpg"
)

// seqProber returns a programmable sequence of probe.Results, repeating
// the last entry once exhausted. Counts calls. Safe for concurrent use.
type seqProber struct {
	mu      sync.Mutex
	results []probe.Result
	idx     int
	calls   *atomic.Int32
}

func (p *seqProber) Probe(context.Context) probe.Result {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calls != nil {
		p.calls.Add(1)
	}
	i := p.idx
	if i >= len(p.results) {
		i = len(p.results) - 1
	}
	p.idx++
	return p.results[i]
}

// constProber always returns the same result, optionally counting calls.
type constProber struct {
	res   probe.Result
	calls *atomic.Int32
}

func (p constProber) Probe(context.Context) probe.Result {
	if p.calls != nil {
		p.calls.Add(1)
	}
	return p.res
}

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
		Tags: []string{"prod"}, Source: store.SourceStatic,
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Programmable prober: fail / fail / ok across the three ticks.
	prober := &seqProber{results: []probe.Result{
		{Code: 500, Error: "boom"},
		{Code: 500, Error: "boom"},
		{Code: 200},
	}}

	// Inject deterministic clock advancing 1 minute per tick.
	now := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	tickClock := func() time.Time {
		c := now
		now = now.Add(time.Minute)
		return c
	}

	s := scheduler.New(repo,
		scheduler.WithNow(tickClock),
	)

	plan := scheduler.Plan{
		Slug: "api", URL: "http://api/health", HTTPMethod: "GET",
		AcceptedStatusCodes: []int{200},
		Interval:            5 * time.Minute, Timeout: 2 * time.Second,
		Retries: 0, RetryBackoff: time.Second,
		Prober: prober,
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
			Slug: slug, FriendlyName: slug, URL: "http://x", Tags: []string{"g"}, Source: store.SourceStatic,
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

	// Prober that fails — but should not be invoked while paused.
	var called atomic.Int32
	s := scheduler.New(repo)

	plan := scheduler.Plan{
		Slug: "child", URL: "x", HTTPMethod: "GET",
		AcceptedStatusCodes: []int{200},
		Interval:            5 * time.Minute, Timeout: time.Second,
		Retries: 0, RetryBackoff: time.Second,
		DependsOn: []string{"parent"},
		Prober:    constProber{res: probe.Result{Code: 500, Error: "would-fail"}, calls: &called},
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

// TestTick_dependsOn_resumeFromPaused_preservesOpenIncident reproduces
// the user-reported scenario: B already has an open incident when its
// parent A goes down (so B is marked temporary-paused without losing
// its prior down classification). When A recovers and B's probe runs,
// B is *still* failing — the resume must NOT emit a duplicate Open;
// the original incident should continue (no event, or at most a
// reminder if the interval elapsed). Without the fix, the resume code
// forces prev=up and the next failing tick emits a fresh Open.
func TestTick_dependsOn_resumeFromPaused_preservesOpenIncident(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()

	for _, slug := range []string{"parent", "child"} {
		if err := repo.ReconcileMonitor(ctx, store.MonitorSpec{
			Slug: slug, FriendlyName: slug, URL: "http://x", Tags: []string{"g"}, Source: store.SourceStatic,
		}); err != nil {
			t.Fatalf("reconcile %s: %v", slug, err)
		}
	}

	// 1. Child goes down first (independent of any parent issue).
	t0 := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	if err := repo.ApplyCheck(ctx, "child",
		alert.State{Status: alert.StatusDown, OpenedAt: t0, LastReminderAt: t0},
		t0, 503, "down",
		&alert.Event{Type: alert.EventOpen, At: t0, StatusCode: 503, Error: "down"},
	); err != nil {
		t.Fatal(err)
	}

	// 2. Parent goes down a minute later.
	t1 := t0.Add(time.Minute)
	if err := repo.ApplyCheck(ctx, "parent",
		alert.State{Status: alert.StatusDown, OpenedAt: t1},
		t1, 503, "down",
		&alert.Event{Type: alert.EventOpen, At: t1, StatusCode: 503, Error: "down"},
	); err != nil {
		t.Fatal(err)
	}

	// 3. Child's next tick: parent is down → child gets paused. The
	//    gate must not lose the child's prior open incident.
	s := scheduler.New(repo)
	plan := scheduler.Plan{
		Slug: "child", URL: "x", HTTPMethod: "GET",
		AcceptedStatusCodes: []int{200},
		Interval:            5 * time.Minute, Timeout: time.Second,
		Retries: 0, RetryBackoff: time.Second,
		DependsOn: []string{"parent"},
		Prober:    constProber{res: probe.Result{Code: 500, Error: "still failing"}},
	}
	if err := s.Tick(ctx, plan); err != nil {
		t.Fatalf("paused tick: %v", err)
	}

	// 4. Parent recovers.
	t2 := t1.Add(2 * time.Minute)
	if err := repo.ApplyCheck(ctx, "parent",
		alert.State{Status: alert.StatusUp},
		t2, 200, "",
		&alert.Event{Type: alert.EventResolve, At: t2, Downtime: 2 * time.Minute},
	); err != nil {
		t.Fatal(err)
	}

	// 5. Child's next tick: child still fails. With the bug, this
	//    emits a fresh Open (the duplicate notification). Correct
	//    behavior: no new event — the original incident continues.
	if err := s.Tick(ctx, plan); err != nil {
		t.Fatalf("resumed tick: %v", err)
	}

	events, err := repo.ListAlertsForMonitor(ctx, "child", 10)
	if err != nil {
		t.Fatal(err)
	}
	opens := 0
	for _, ev := range events {
		if ev.Type == alert.EventOpen {
			opens++
		}
	}
	if opens != 1 {
		t.Errorf("expected exactly 1 Open event for child across the whole flow, got %d (events=%+v)", opens, events)
	}

	row, err := repo.GetMonitor(ctx, "child")
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != alert.StatusDown {
		t.Errorf("child status after resume: got %q, want %q (still failing)", row.Status, alert.StatusDown)
	}
	if row.OpenedAt == nil || !row.OpenedAt.Equal(t0) {
		t.Errorf("child OpenedAt: got %v, want original incident time %v", row.OpenedAt, t0)
	}
}

// mutableSource lets a test swap the plan set mid-flight.
type mutableSource struct {
	mu    sync.Mutex
	plans []scheduler.Plan
}

func (m *mutableSource) CurrentPlans() []scheduler.Plan {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]scheduler.Plan(nil), m.plans...)
}

func (m *mutableSource) Set(plans []scheduler.Plan) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.plans = plans
}

// TestRunDynamic_addsAndRemovesMonitorsOnRefresh: starts the
// scheduler with a single plan, swaps in a second plan mid-flight,
// and asserts both fire; then removes the first plan and asserts it
// stops firing. The fake check function records per-slug call
// counts.
func TestRunDynamic_addsAndRemovesMonitorsOnRefresh(t *testing.T) {
	repo := newRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	for _, slug := range []string{"alpha", "beta"} {
		if err := repo.ReconcileMonitor(ctx, store.MonitorSpec{
			Slug: slug, FriendlyName: slug, URL: "http://x", Tags: []string{"g"}, Source: store.SourceStatic,
		}); err != nil {
			t.Fatalf("reconcile %s: %v", slug, err)
		}
	}

	calls := map[string]*atomic.Int32{
		"alpha": {},
		"beta":  {},
	}

	mkPlan := func(slug, url string) scheduler.Plan {
		return scheduler.Plan{
			Slug: slug, URL: url, HTTPMethod: "GET",
			AcceptedStatusCodes: []int{200},
			Interval:            80 * time.Millisecond,
			Timeout:             50 * time.Millisecond,
			Retries:             0,
			RetryBackoff:        time.Second,
			Prober:              constProber{res: probe.Result{Code: 200}, calls: calls[slug]},
		}
	}

	src := &mutableSource{plans: []scheduler.Plan{mkPlan("alpha", "http://alpha")}}
	s := scheduler.New(repo)
	done := make(chan struct{})
	go func() {
		s.RunDynamic(ctx, src, 100*time.Millisecond)
		close(done)
	}()

	// Let alpha tick a few times.
	time.Sleep(350 * time.Millisecond)
	if calls["alpha"].Load() < 1 {
		t.Errorf("alpha should have fired at least once; got %d", calls["alpha"].Load())
	}
	if calls["beta"].Load() != 0 {
		t.Errorf("beta should not have fired yet; got %d", calls["beta"].Load())
	}

	// Add beta to the plan set; the scheduler should pick it up at the
	// next refresh tick.
	src.Set([]scheduler.Plan{
		mkPlan("alpha", "http://alpha"),
		mkPlan("beta", "http://beta"),
	})
	time.Sleep(400 * time.Millisecond)
	if calls["beta"].Load() < 1 {
		t.Errorf("beta should have fired after refresh; got %d", calls["beta"].Load())
	}

	// Remove alpha. After the next refresh + a couple of intervals,
	// alpha's call count should stop advancing.
	src.Set([]scheduler.Plan{mkPlan("beta", "http://beta")})
	time.Sleep(250 * time.Millisecond)
	alphaAfter := calls["alpha"].Load()
	time.Sleep(300 * time.Millisecond)
	if got := calls["alpha"].Load(); got > alphaAfter+1 {
		// allow at most one in-flight tick after cancel
		t.Errorf("alpha should stop firing after removal; %d → %d", alphaAfter, got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunDynamic did not exit after cancel")
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
		Tags: []string{"prod"}, Source: store.SourceStatic,
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var idx atomic.Int32
	prober := &seqProber{
		results: []probe.Result{
			{Code: 500, Error: "transient"},
			{Code: 200},
		},
		calls: &idx,
	}

	s := scheduler.New(repo)

	plan := scheduler.Plan{
		Slug: "api", URL: "x", HTTPMethod: "GET",
		AcceptedStatusCodes: []int{200},
		Interval:            5 * time.Minute, Timeout: time.Second,
		Retries: 1, RetryBackoff: time.Millisecond,
		Prober: prober,
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

// TestTick_dependsOn_pushPropagation_onParentOpen verifies the real
// push-propagation introduced in ADR-0004: when a parent's tick emits
// EventOpen, the scheduler synchronously calls the configured
// push-propagation hook with the parent's slug. The hook in production
// (wired by lifecycle) walks the reverse-dependsOn index and
// MarkTemporaryPaused's every child + Pauses the dispatcher.
//
// Test substitutes a fake hook that records its invocations; the
// reverse-deps walk itself is covered by depindex tests.
func TestTick_dependsOn_pushPropagation_onParentOpen(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()

	if err := repo.ReconcileMonitor(ctx, store.MonitorSpec{
		Slug: "parent", FriendlyName: "Parent", URL: "http://x", Source: store.SourceStatic,
	}); err != nil {
		t.Fatal(err)
	}

	var (
		hookMu     sync.Mutex
		hookCalled []string
	)
	hook := func(_ context.Context, parentSlug string, _ time.Time) {
		hookMu.Lock()
		defer hookMu.Unlock()
		hookCalled = append(hookCalled, parentSlug)
	}

	s := scheduler.New(repo, scheduler.WithPushPropagation(hook))

	plan := scheduler.Plan{
		Slug: "parent", URL: "http://x", HTTPMethod: "GET",
		AcceptedStatusCodes: []int{200},
		Interval:            5 * time.Minute, Timeout: time.Second,
		Prober: constProber{res: probe.Result{Code: 500, Error: "boom"}},
	}
	if err := s.Tick(ctx, plan); err != nil {
		t.Fatalf("tick: %v", err)
	}

	hookMu.Lock()
	defer hookMu.Unlock()
	if len(hookCalled) != 1 || hookCalled[0] != "parent" {
		t.Fatalf("push-propagation hook want [parent], got %v", hookCalled)
	}
}

// TestTick_dependsOn_pushPropagation_notFiredOnReminder verifies that
// the hook fires only on EventOpen, not on subsequent reminder events
// (children are already paused by the first call).
func TestTick_dependsOn_pushPropagation_notFiredOnReminder(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()

	if err := repo.ReconcileMonitor(ctx, store.MonitorSpec{
		Slug: "parent", FriendlyName: "Parent", URL: "http://x", Source: store.SourceStatic,
	}); err != nil {
		t.Fatal(err)
	}

	var hookCalls atomic.Int32
	hook := func(_ context.Context, _ string, _ time.Time) { hookCalls.Add(1) }

	s := scheduler.New(repo, scheduler.WithPushPropagation(hook))

	plan := scheduler.Plan{
		Slug: "parent", URL: "http://x", HTTPMethod: "GET",
		AcceptedStatusCodes: []int{200},
		Interval:            5 * time.Minute, Timeout: time.Second,
		ReminderInterval: time.Nanosecond, // ensures the second tick emits a reminder
		Prober:           constProber{res: probe.Result{Code: 500, Error: "boom"}},
	}
	if err := s.Tick(ctx, plan); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	// Second tick — also failing, reminder interval elapsed → EventReminder.
	if err := s.Tick(ctx, plan); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if got := hookCalls.Load(); got != 1 {
		t.Fatalf("push-propagation should fire only on EventOpen; got %d calls", got)
	}
}
