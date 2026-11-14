//go:build integration

package lifecycle

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toggle-corp/toggle-monitor/internal/alert"
	"github.com/toggle-corp/toggle-monitor/internal/coalesce"
	"github.com/toggle-corp/toggle-monitor/internal/migrate"
	"github.com/toggle-corp/toggle-monitor/internal/probe"
	"github.com/toggle-corp/toggle-monitor/internal/scheduler"
	"github.com/toggle-corp/toggle-monitor/internal/store"
	"github.com/toggle-corp/toggle-monitor/internal/testpg"
)

// writeTrap is a pgx tracer that runs a function once, just before the
// next statement that writes or row-locks a monitors row. That is the
// moment a writer commits what it read, so the function runs another
// writer exactly inside that window.
type writeTrap struct {
	mu sync.Mutex
	fn func()
}

// arm makes fn run before the next monitors write or lock.
func (w *writeTrap) arm(fn func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.fn = fn
}

func (w *writeTrap) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !strings.Contains(data.SQL, "UPDATE monitors") && !strings.Contains(data.SQL, "FOR UPDATE") {
		return ctx
	}
	w.mu.Lock()
	fn := w.fn
	w.fn = nil
	w.mu.Unlock()
	if fn != nil {
		fn()
	}
	return ctx
}

func (w *writeTrap) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func newTrappedRepo(t *testing.T) (*store.Repo, *writeTrap) {
	t.Helper()
	ctx := context.Background()
	dsn := testpg.Start(t)
	if err := migrate.Up(dsn); err != nil {
		t.Fatalf("migrate.Up: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	trap := &writeTrap{}
	cfg.ConnConfig.Tracer = trap
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	repo := store.New(pool)
	if err := repo.ReconcileMonitor(ctx, store.MonitorSpec{
		Slug: "api", FriendlyName: "api", URL: "https://api.example.test/", Source: store.SourceStatic,
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return repo, trap
}

// raceProber runs during the probe and returns res.
type raceProber struct {
	during func()
	res    probe.Result
}

func (p raceProber) Probe(context.Context) probe.Result {
	if p.during != nil {
		p.during()
	}
	return p.res
}

var (
	probeOK   = probe.Result{Code: 200}
	probeFail = probe.Result{Code: 503, Error: "503 Service Unavailable", FailKind: probe.FailKindHTTP}
)

// recordingSink is a scheduler.EventSink that records event types.
type recordingSink struct {
	mu     sync.Mutex
	events []alert.EventType
}

func (r *recordingSink) sink(_ context.Context, _ store.MonitorRow, _ string, _ []string, ev *alert.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev.Type)
	return nil
}

func (r *recordingSink) got() []alert.EventType {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

// apiPlan is the plan every writer in these tests uses for "api".
func apiPlan(prober probe.Prober) scheduler.Plan {
	return scheduler.Plan{
		Slug: "api", FriendlyName: "api", URL: "https://api.example.test/",
		Interval: 5 * time.Minute, Timeout: time.Second,
		ReminderInterval: 24 * time.Hour,
		ChannelSlug:      "ops",
		Critical:         true,
		Prober:           prober,
	}
}

// alertEventTypes lists a monitor's alert_events oldest first.
func alertEventTypes(t *testing.T, repo *store.Repo, slug string) []alert.EventType {
	t.Helper()
	page, err := repo.ListAlertsForMonitor(context.Background(), slug, 100, 0)
	if err != nil {
		t.Fatalf("list alerts: %v", err)
	}
	out := make([]alert.EventType, 0, len(page.Items))
	for i := len(page.Items) - 1; i >= 0; i-- {
		out = append(out, page.Items[i].Type)
	}
	return out
}

func assertMonitor(t *testing.T, repo *store.Repo, wantStatus alert.Status, wantEvents ...alert.EventType) {
	t.Helper()
	row, err := repo.GetMonitor(context.Background(), "api")
	if err != nil {
		t.Fatalf("get monitor: %v", err)
	}
	if row.Status != wantStatus {
		t.Errorf("status: got %q, want %q", row.Status, wantStatus)
	}
	if wantStatus == alert.StatusUp && row.OpenedAt != nil {
		t.Errorf("opened_at: got %v on an up monitor, want NULL", *row.OpenedAt)
	}
	if got := alertEventTypes(t, repo, "api"); !slices.Equal(got, wantEvents) {
		t.Errorf("alert_events: got %v, want %v", got, wantEvents)
	}
}

func onDemandProbe(repo *store.Repo, prober probe.Prober, sink *recordingSink, pushed *[]string) coalesce.OnDemandParentProbe {
	mgr := coalesce.New(coalesce.Options{Store: repo, Sink: coalesce.Sink(sink.sink)})
	push := func(_ context.Context, parent string, _ time.Time) { *pushed = append(*pushed, parent) }
	return makeOnDemandParentProbe(repo, mgr, staticPlans{apiPlan(prober)}, push, sink.sink, 5*time.Second, time.Now, slog.Default())
}

func selfHealthCommit(repo *store.Repo, prober probe.Prober, sink *recordingSink) commitFunc {
	mgr := coalesce.New(coalesce.Options{Store: repo, Sink: coalesce.Sink(sink.sink)})
	plan := apiPlan(prober)
	plan.Critical = false
	return makeSelfHealthCommit(repo, mgr, staticPlans{plan}, nil, nil, time.Now, slog.Default())
}

// TestCommitRace_onDemandOpenInsideTickCommit_tickResolves: the
// on-demand parent probe commits an Open after the tick's probe came
// back OK and before the tick commits. The tick applies its OK result
// to the Open, so the incident resolves instead of being overwritten
// with "up" and left open.
func TestCommitRace_onDemandOpenInsideTickCommit_tickResolves(t *testing.T) {
	repo, trap := newTrappedRepo(t)
	ctx := context.Background()

	odSink := &recordingSink{}
	var pushed []string
	probeParent := onDemandProbe(repo, raceProber{res: probeFail}, odSink, &pushed)

	tickSink := &recordingSink{}
	s := scheduler.New(repo, scheduler.WithEventSink(tickSink.sink))
	tickProber := raceProber{res: probeOK, during: func() {
		trap.arm(func() { probeParent(ctx, "api") })
	}}
	if err := s.Tick(ctx, apiPlan(tickProber)); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if got := odSink.got(); !slices.Equal(got, []alert.EventType{alert.EventOpen}) {
		t.Fatalf("on-demand probe sink: got %v, want [open]", got)
	}
	if got := tickSink.got(); !slices.Equal(got, []alert.EventType{alert.EventResolve}) {
		t.Errorf("tick sink: got %v, want [resolve]", got)
	}
	assertMonitor(t, repo, alert.StatusUp, alert.EventOpen, alert.EventResolve)
}

// TestCommitRace_tickCommitInsideOnDemandProbe_onDemandSkips: the
// monitor's own tick commits an OK check while the on-demand probe is
// still out. The on-demand failure is older than that check, so it
// commits nothing and announces nothing.
func TestCommitRace_tickCommitInsideOnDemandProbe_onDemandSkips(t *testing.T) {
	repo, _ := newTrappedRepo(t)
	ctx := context.Background()

	tickSink := &recordingSink{}
	s := scheduler.New(repo, scheduler.WithEventSink(tickSink.sink))
	odSink := &recordingSink{}
	var pushed []string
	probeParent := onDemandProbe(repo, raceProber{res: probeFail, during: func() {
		if err := s.Tick(ctx, apiPlan(raceProber{res: probeOK})); err != nil {
			t.Errorf("tick: %v", err)
		}
	}}, odSink, &pushed)

	probeParent(ctx, "api")

	if got := odSink.got(); len(got) != 0 {
		t.Errorf("on-demand probe sink: got %v, want nothing", got)
	}
	if len(pushed) != 0 {
		t.Errorf("push-propagation: got %v, want none", pushed)
	}
	assertMonitor(t, repo, alert.StatusUp)
}

// TestCommitRace_selfHealthOpenInsideTickCommit_tickResolves mirrors
// the on-demand case for the self-health commit.
func TestCommitRace_selfHealthOpenInsideTickCommit_tickResolves(t *testing.T) {
	repo, trap := newTrappedRepo(t)
	ctx := context.Background()

	commit := selfHealthCommit(repo, raceProber{res: probeFail}, &recordingSink{})

	tickSink := &recordingSink{}
	s := scheduler.New(repo, scheduler.WithEventSink(tickSink.sink))
	tickProber := raceProber{res: probeOK, during: func() {
		trap.arm(func() { commit(ctx, "api") })
	}}
	if err := s.Tick(ctx, apiPlan(tickProber)); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if got := tickSink.got(); !slices.Equal(got, []alert.EventType{alert.EventResolve}) {
		t.Errorf("tick sink: got %v, want [resolve]", got)
	}
	assertMonitor(t, repo, alert.StatusUp, alert.EventOpen, alert.EventResolve)
}

// TestCommitRace_tickCommitInsideSelfHealthProbe_selfHealthSkips
// mirrors the on-demand case for the self-health commit.
func TestCommitRace_tickCommitInsideSelfHealthProbe_selfHealthSkips(t *testing.T) {
	repo, _ := newTrappedRepo(t)
	ctx := context.Background()

	tickSink := &recordingSink{}
	s := scheduler.New(repo, scheduler.WithEventSink(tickSink.sink))
	commit := selfHealthCommit(repo, raceProber{res: probeFail, during: func() {
		if err := s.Tick(ctx, apiPlan(raceProber{res: probeOK})); err != nil {
			t.Errorf("tick: %v", err)
		}
	}}, &recordingSink{})

	commit(ctx, "api")

	assertMonitor(t, repo, alert.StatusUp)
}

// TestCommitRace_extraWritersSkipPausedArchivedAndDown: the on-demand
// probe and the self-health commit only open an incident on a monitor
// that is up. A paused or archived monitor stays as it is, and one
// already down gets no second write.
func TestCommitRace_extraWritersSkipPausedArchivedAndDown(t *testing.T) {
	setups := map[string]func(t *testing.T, repo *store.Repo) (alert.Status, []alert.EventType){
		"paused": func(t *testing.T, repo *store.Repo) (alert.Status, []alert.EventType) {
			if err := repo.MarkTemporaryPaused(context.Background(), "api"); err != nil {
				t.Fatalf("pause: %v", err)
			}
			return alert.StatusTemporaryPaused, []alert.EventType{}
		},
		"archived": func(t *testing.T, repo *store.Repo) (alert.Status, []alert.EventType) {
			if err := repo.SoftDeleteMonitor(context.Background(), "api", "test"); err != nil {
				t.Fatalf("archive: %v", err)
			}
			return alert.StatusUp, []alert.EventType{}
		},
		"down": func(t *testing.T, repo *store.Repo) (alert.Status, []alert.EventType) {
			at := time.Now().Add(-48 * time.Hour)
			if err := repo.ApplyCheck(context.Background(), "api",
				alert.State{Status: alert.StatusDown, OpenedAt: at, LastReminderAt: at},
				at, 503, "down",
				&alert.Event{Type: alert.EventOpen, At: at, StatusCode: 503, Error: "down"},
			); err != nil {
				t.Fatalf("open: %v", err)
			}
			return alert.StatusDown, []alert.EventType{alert.EventOpen}
		},
	}
	writers := map[string]func(repo *store.Repo, sink *recordingSink) func(ctx context.Context){
		"on-demand": func(repo *store.Repo, sink *recordingSink) func(ctx context.Context) {
			var pushed []string
			p := onDemandProbe(repo, raceProber{res: probeFail}, sink, &pushed)
			return func(ctx context.Context) { p(ctx, "api") }
		},
		"self-health": func(repo *store.Repo, sink *recordingSink) func(ctx context.Context) {
			c := selfHealthCommit(repo, raceProber{res: probeFail}, sink)
			return func(ctx context.Context) { c(ctx, "api") }
		},
	}
	for setupName, setup := range setups {
		for writerName, writer := range writers {
			t.Run(setupName+"/"+writerName, func(t *testing.T) {
				repo, _ := newTrappedRepo(t)
				wantStatus, wantEvents := setup(t, repo)
				before, err := repo.GetMonitor(context.Background(), "api")
				if err != nil {
					t.Fatalf("get monitor: %v", err)
				}
				sink := &recordingSink{}
				writer(repo, sink)(context.Background())

				if got := sink.got(); len(got) != 0 {
					t.Errorf("sink: got %v, want nothing", got)
				}
				after, err := repo.GetMonitor(context.Background(), "api")
				if err != nil {
					t.Fatalf("get monitor: %v", err)
				}
				if after.Status != wantStatus {
					t.Errorf("status: got %q, want %q", after.Status, wantStatus)
				}
				if !equalTimePtr(before.LastCheckedAt, after.LastCheckedAt) {
					t.Errorf("last_checked_at moved: %v → %v", before.LastCheckedAt, after.LastCheckedAt)
				}
				if got := alertEventTypes(t, repo, "api"); !slices.Equal(got, wantEvents) {
					t.Errorf("alert_events: got %v, want %v", got, wantEvents)
				}
			})
		}
	}
}

func equalTimePtr(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}
