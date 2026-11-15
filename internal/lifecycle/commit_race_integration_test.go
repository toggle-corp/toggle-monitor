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
	slugs  []string // slugs[i] is the monitor of events[i]
}

func (r *recordingSink) sink(_ context.Context, row store.MonitorRow, _ string, _ []string, ev *alert.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev.Type)
	r.slugs = append(r.slugs, row.Slug)
	return nil
}

func (r *recordingSink) got() []alert.EventType {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

// gotFor lists the event types delivered for one monitor.
func (r *recordingSink) gotFor(slug string) []alert.EventType {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []alert.EventType
	for i, s := range r.slugs {
		if s == slug {
			out = append(out, r.events[i])
		}
	}
	return out
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
	return onDemandProbeOf(repo, apiPlan(prober), sink, pushed)
}

// onDemandProbeOf builds the on-demand probe over plan, recording the
// slugs push-propagation runs from.
func onDemandProbeOf(repo *store.Repo, plan scheduler.Plan, sink *recordingSink, pushed *[]string) coalesce.OnDemandParentProbe {
	mgr := coalesce.New(coalesce.Options{Store: repo, Sink: coalesce.Sink(sink.sink)})
	push := func(_ context.Context, parent string, _ time.Time) { *pushed = append(*pushed, parent) }
	return makeOnDemandParentProbe(repo, mgr, staticPlans{plan}, push, sink.sink, 5*time.Second, time.Now, slog.Default())
}

func selfHealthCommit(repo *store.Repo, prober probe.Prober, sink *recordingSink) commitFunc {
	return selfHealthCommitOf(repo, apiPlan(prober), sink)
}

// selfHealthCommitOf builds the self-health commit over plan, made
// non-critical so its Open routes through the dispatcher.
func selfHealthCommitOf(repo *store.Repo, plan scheduler.Plan, sink *recordingSink) commitFunc {
	mgr := coalesce.New(coalesce.Options{Store: repo, Sink: coalesce.Sink(sink.sink)})
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

// childPlan is a critical "child" that dependsOn "parent".
func childPlan(prober probe.Prober) scheduler.Plan {
	p := apiPlan(prober)
	p.Slug, p.FriendlyName, p.URL = "child", "child", "https://child.example.test/"
	p.DependsOn = []string{"parent"}
	return p
}

// parentDownPausesChild commits an Open on "parent" and pauses "child",
// as the parent's tick and its push-propagation do.
func parentDownPausesChild(t *testing.T, repo *store.Repo) {
	t.Helper()
	openParent(t, repo)
	if err := repo.MarkTemporaryPaused(context.Background(), "child"); err != nil {
		t.Errorf("pause child: %v", err)
	}
}

// openParent commits an Open on "parent", as its tick does before its
// push-propagation reaches "child".
func openParent(t *testing.T, repo *store.Repo) {
	t.Helper()
	at := time.Now()
	if err := repo.ApplyCheck(context.Background(), "parent",
		alert.State{Status: alert.StatusDown, OpenedAt: at, LastReminderAt: at},
		at, 503, "down",
		&alert.Event{Type: alert.EventOpen, At: at, StatusCode: 503, Error: "down"},
	); err != nil {
		t.Errorf("open parent: %v", err)
	}
}

// reconcileParentAndChild adds the "parent" and "child" monitors.
func reconcileParentAndChild(t *testing.T, repo *store.Repo) {
	t.Helper()
	for _, slug := range []string{"parent", "child"} {
		if err := repo.ReconcileMonitor(context.Background(), store.MonitorSpec{
			Slug: slug, FriendlyName: slug, URL: "https://" + slug + ".example.test/", Source: store.SourceStatic,
		}); err != nil {
			t.Fatalf("reconcile %s: %v", slug, err)
		}
	}
}

// TestCommitRace_parentPausesChildBeforeTickCommit_childStaysPaused: the
// child's dependsOn checks see its parent up, then the parent goes down
// and push-propagation pauses the child just before the child's tick
// commits. The tick must not read that pause as a resume: with the
// parent still down, the child stays paused, whatever its probe saw.
func TestCommitRace_parentPausesChildBeforeTickCommit_childStaysPaused(t *testing.T) {
	for name, res := range map[string]probe.Result{"fail": probeFail, "ok": probeOK} {
		t.Run(name, func(t *testing.T) {
			repo, trap := newTrappedRepo(t)
			ctx := context.Background()
			reconcileParentAndChild(t, repo)

			sink := &recordingSink{}
			s := scheduler.New(repo, scheduler.WithEventSink(sink.sink))
			prober := raceProber{res: res, during: func() {
				trap.arm(func() { parentDownPausesChild(t, repo) })
			}}
			if err := s.Tick(ctx, childPlan(prober)); err != nil {
				t.Fatalf("tick: %v", err)
			}

			if got := sink.got(); len(got) != 0 {
				t.Errorf("sink: got %v, want nothing while the parent is down", got)
			}
			row, err := repo.GetMonitor(ctx, "child")
			if err != nil {
				t.Fatalf("get child: %v", err)
			}
			if row.Status != alert.StatusTemporaryPaused {
				t.Errorf("child status: got %q, want %q", row.Status, alert.StatusTemporaryPaused)
			}
			if got := alertEventTypes(t, repo, "child"); len(got) != 0 {
				t.Errorf("child alert_events: got %v, want none", got)
			}
		})
	}
}

// TestCommitRace_parentDownBeforeExtraCommit_childStaysSilent: the
// child's parent opens an incident while the on-demand probe or the
// self-health commit probes the child, and push-propagation has not
// paused the child yet. The extra writer reads the parents under the
// child's row lock, so it neither opens the child nor announces it. The
// on-demand probe runs push-propagation from the child instead, since a
// held hot parent holds its own children.
func TestCommitRace_parentDownBeforeExtraCommit_childStaysSilent(t *testing.T) {
	writers := map[string]struct {
		build      func(repo *store.Repo, prober probe.Prober, sink *recordingSink, pushed *[]string) func(ctx context.Context)
		wantPushed []string
	}{
		"on-demand": {
			build: func(repo *store.Repo, prober probe.Prober, sink *recordingSink, pushed *[]string) func(ctx context.Context) {
				p := onDemandProbeOf(repo, childPlan(prober), sink, pushed)
				return func(ctx context.Context) { p(ctx, "child") }
			},
			wantPushed: []string{"child"},
		},
		"self-health": {
			build: func(repo *store.Repo, prober probe.Prober, sink *recordingSink, _ *[]string) func(ctx context.Context) {
				c := selfHealthCommitOf(repo, childPlan(prober), sink)
				return func(ctx context.Context) { c(ctx, "child") }
			},
		},
	}
	for name, w := range writers {
		t.Run(name, func(t *testing.T) {
			repo, trap := newTrappedRepo(t)
			ctx := context.Background()
			reconcileParentAndChild(t, repo)

			sink := &recordingSink{}
			var pushed []string
			prober := raceProber{res: probeFail, during: func() {
				trap.arm(func() { openParent(t, repo) })
			}}
			w.build(repo, prober, sink, &pushed)(ctx)

			if got := sink.got(); len(got) != 0 {
				t.Errorf("sink: got %v, want nothing while the parent is down", got)
			}
			if !slices.Equal(pushed, w.wantPushed) {
				t.Errorf("push-propagation: got %v, want %v", pushed, w.wantPushed)
			}
			row, err := repo.GetMonitor(ctx, "child")
			if err != nil {
				t.Fatalf("get child: %v", err)
			}
			if row.Status != alert.StatusUp {
				t.Errorf("child status: got %q, want %q", row.Status, alert.StatusUp)
			}
			if got := alertEventTypes(t, repo, "child"); len(got) != 0 {
				t.Errorf("child alert_events: got %v, want none", got)
			}
		})
	}
}

// TestCommitRace_onDemandOpenResolvedDuringPush_postsNothing: the
// on-demand probe commits a non-critical parent's Open, and the parent's
// own tick resolves it while push-propagation runs, before the probe
// routes the Open. Nobody was paged, so the recovery posts nothing, and
// the Open the dispatcher receives after it must not post either: that
// DOWN would have no recovery left to edit it.
func TestCommitRace_onDemandOpenResolvedDuringPush_postsNothing(t *testing.T) {
	repo, _ := newTrappedRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &recordingSink{}
	mgr := coalesce.New(coalesce.Options{Store: repo, Sink: coalesce.Sink(sink.sink), PendingWait: 20 * time.Millisecond})
	nonCritical := func(prober probe.Prober) scheduler.Plan {
		p := apiPlan(prober)
		p.Critical = false
		return p
	}
	s := scheduler.New(repo, scheduler.WithGroupSink(groupSinkAdapter{m: mgr}))
	push := func(ctx context.Context, _ string, _ time.Time) {
		if err := s.Tick(ctx, nonCritical(raceProber{res: probeOK})); err != nil {
			t.Errorf("tick: %v", err)
		}
	}
	probeParent := makeOnDemandParentProbe(repo, mgr, staticPlans{nonCritical(raceProber{res: probeFail})},
		push, sink.sink, 5*time.Second, time.Now, slog.Default())
	probeParent(ctx, "api")
	assertMonitor(t, repo, alert.StatusUp, alert.EventOpen, alert.EventResolve)

	// A later failure on the same channel joins the pending pool the
	// late Open would sit in, so once it flushes, the Open would have too.
	mgr.Route(ctx, "ops", coalesce.Entry{
		Member: coalesce.MemberInfo{Slug: "other", FriendlyName: "other"},
		Row:    store.MonitorRow{MonitorSpec: store.MonitorSpec{Slug: "other"}},
		Event:  &alert.Event{Type: alert.EventOpen, At: time.Now()},
	})
	go mgr.RunEvaluator(ctx, 5*time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	for len(sink.gotFor("other")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("pending pool never flushed")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if got := sink.gotFor("api"); len(got) != 0 {
		t.Errorf("api: got %v, want nothing posted for an incident that already resolved", got)
	}
}
