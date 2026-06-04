package alertmanager

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSweepRepo is a sweepRepo stand-in that records each call. The
// implementation is intentionally tiny — the sweeper only ever calls
// SweepAMResolved, so the fake doesn't need to satisfy *store.Repo.
type fakeSweepRepo struct {
	mu      sync.Mutex
	calls   []time.Time
	rows    int64
	err     error
	onCall  func(t time.Time)
	calledN atomic.Int32
}

func (f *fakeSweepRepo) SweepAMResolved(_ context.Context, olderThan time.Time) (int64, error) {
	f.mu.Lock()
	f.calls = append(f.calls, olderThan)
	cb := f.onCall
	f.mu.Unlock()
	if cb != nil {
		cb(olderThan)
	}
	f.calledN.Add(1)
	return f.rows, f.err
}

// recordingSweeperObserver records observer callbacks for assertions.
type recordingSweeperObserver struct {
	mu       sync.Mutex
	deleted  []int64
	errCount int
}

func (o *recordingSweeperObserver) AMRetentionSwept(rowsDeleted int64, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.deleted = append(o.deleted, rowsDeleted)
	if err != nil {
		o.errCount++
	}
}

// TestSweeper_disabledReturnsImmediately checks that RetentionDays=0
// short-circuits Run without sweeping or waiting on the timer.
func TestSweeper_disabledReturnsImmediately(t *testing.T) {
	repo := &fakeSweepRepo{}
	s := newSweeperFromRepo(sweepOpts{
		Repo:          repo,
		RetentionDays: 0,
		// Long interval — if the sweeper accidentally enters the loop
		// this test would hang on it.
		Interval: time.Hour,
		Now:      func() time.Time { return time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC) },
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not return immediately when retention is disabled")
	}
	if got := repo.calledN.Load(); got != 0 {
		t.Errorf("SweepAMResolved calls: got %d, want 0", got)
	}
}

// TestSweeper_oneIterationSweeps drives one sweep and asserts the
// olderThan cutoff is now − retention*24h.
func TestSweeper_oneIterationSweeps(t *testing.T) {
	repo := &fakeSweepRepo{rows: 7}
	fixedNow := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	obs := &recordingSweeperObserver{}
	swept := make(chan struct{}, 1)
	repo.onCall = func(time.Time) {
		select {
		case swept <- struct{}{}:
		default:
		}
	}

	s := newSweeperFromRepo(sweepOpts{
		Repo:          repo,
		RetentionDays: 180,
		Interval:      10 * time.Millisecond,
		Now:           func() time.Time { return fixedNow },
		Observer:      obs,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()

	select {
	case <-swept:
	case <-time.After(2 * time.Second):
		t.Fatal("sweep never fired")
	}
	cancel()
	<-done

	repo.mu.Lock()
	defer repo.mu.Unlock()
	if len(repo.calls) == 0 {
		t.Fatal("expected at least one sweep call")
	}
	wantCutoff := fixedNow.Add(-180 * 24 * time.Hour)
	if !repo.calls[0].Equal(wantCutoff) {
		t.Errorf("olderThan: got %v, want %v", repo.calls[0], wantCutoff)
	}

	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.deleted) == 0 || obs.deleted[0] != 7 {
		t.Errorf("observer.deleted: got %v, want first=7", obs.deleted)
	}
	if obs.errCount != 0 {
		t.Errorf("observer.errCount: got %d, want 0", obs.errCount)
	}
}

// TestSweeper_errorIsLoggedAndLoopContinues confirms a sweep error
// doesn't tear down the goroutine.
func TestSweeper_errorIsLoggedAndLoopContinues(t *testing.T) {
	repo := &fakeSweepRepo{err: errors.New("boom")}
	obs := &recordingSweeperObserver{}

	hit := make(chan struct{}, 2)
	repo.onCall = func(time.Time) {
		select {
		case hit <- struct{}{}:
		default:
		}
	}

	s := newSweeperFromRepo(sweepOpts{
		Repo:          repo,
		RetentionDays: 30,
		Interval:      10 * time.Millisecond,
		Now:           time.Now,
		Observer:      obs,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()

	// Wait for at least two sweeps to confirm the loop kept running
	// after the first error.
	for i := 0; i < 2; i++ {
		select {
		case <-hit:
		case <-time.After(2 * time.Second):
			t.Fatalf("did not observe sweep call #%d after error", i+1)
		}
	}
	cancel()
	<-done

	obs.mu.Lock()
	defer obs.mu.Unlock()
	if obs.errCount == 0 {
		t.Errorf("observer.errCount: got 0, want >0 (sweep returned an error)")
	}
}

// TestSweeper_contextCancelUnblocks confirms ctx.Done() exits Run
// within one interval.
func TestSweeper_contextCancelUnblocks(t *testing.T) {
	repo := &fakeSweepRepo{}
	s := newSweeperFromRepo(sweepOpts{
		Repo:          repo,
		RetentionDays: 30,
		Interval:      time.Hour, // long enough that exit must be ctx-driven
		Now:           time.Now,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit promptly after ctx cancel")
	}
}

// sweepOpts mirrors SweeperOptions but parameterized for the
// fake-repo-driven tests above. Defined as a local alias so the
// production constructor's signature can stay tied to *store.Repo
// while the tests skip Postgres entirely.
type sweepOpts struct {
	Repo          sweepRepo
	RetentionDays int
	Interval      time.Duration
	Now           func() time.Time
	Observer      SweeperObserver
}

// newSweeperFromRepo builds a Sweeper from a sweepRepo (production
// uses NewSweeper with the full *store.Repo). Exposed as an unexported
// constructor for the sweeper's own unit tests; lifecycle wiring uses
// NewSweeper.
func newSweeperFromRepo(opts sweepOpts) *Sweeper {
	interval := opts.Interval
	if interval == 0 {
		interval = 24 * time.Hour
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Sweeper{
		repo:          opts.Repo,
		retentionDays: opts.RetentionDays,
		interval:      interval,
		now:           now,
		log:           slog.Default(),
		observer:      opts.Observer,
	}
}
