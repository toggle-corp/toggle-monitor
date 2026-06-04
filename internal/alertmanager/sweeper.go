package alertmanager

import (
	"context"
	"log/slog"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/store"
)

// defaultSweepInterval is the cadence at which the retention sweeper
// fires. ADR-0005 §"Persistence" calls for "swept once daily"; the
// constant is hardcoded rather than operator-configurable in v1
// because retention itself is the surface (RetentionDays) and the
// cadence has no operator-visible effect beyond "rows linger up to a
// day past the cutoff before they go".
const defaultSweepInterval = 24 * time.Hour

// SweeperObserver is the optional metrics seam for the retention
// sweep. Production passes *observability.Metrics if it grows an
// AMRetentionSwept counter; tests pass a recording stub. nil disables
// the callback entirely.
type SweeperObserver interface {
	AMRetentionSwept(rowsDeleted int64, err error)
}

// sweepRepo is the slim slice of *store.Repo the sweeper depends on.
// Keeps the goroutine testable without spinning up Postgres — the
// unit tests pass a fake; production wires the real repo.
type sweepRepo interface {
	SweepAMResolved(ctx context.Context, olderThan time.Time) (int64, error)
}

// Sweeper periodically purges resolved AM incidents whose ended_at is
// older than RetentionDays. Owns a single goroutine started by
// lifecycle; cancellation flows through the passed ctx.
//
// RetentionDays == 0 disables the sweeper entirely — Run logs a
// one-line INFO and returns immediately so the lifecycle goroutine
// drains on startup instead of leaking forever.
type Sweeper struct {
	repo          sweepRepo
	retentionDays int
	interval      time.Duration
	now           func() time.Time
	log           *slog.Logger
	observer      SweeperObserver
}

// SweeperOptions configures a Sweeper.
type SweeperOptions struct {
	Repo          *store.Repo
	RetentionDays int
	// Interval defaults to 24h when 0. Test code passes a short value
	// to keep iterations cheap; production callers should leave it
	// zero so the default applies.
	Interval time.Duration
	Now      func() time.Time
	Logger   *slog.Logger
	Observer SweeperObserver
}

// NewSweeper builds a Sweeper around the real *store.Repo. Failing
// to provide a repo when retention is enabled would panic on the first
// tick; the constructor accepts a nil repo only when RetentionDays is
// also 0 (the disabled-fast-path case).
func NewSweeper(opts SweeperOptions) *Sweeper {
	interval := opts.Interval
	if interval == 0 {
		interval = defaultSweepInterval
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Sweeper{
		repo:          opts.Repo,
		retentionDays: opts.RetentionDays,
		interval:      interval,
		now:           now,
		log:           log,
		observer:      opts.Observer,
	}
}

// Run drives the sweep loop until ctx is cancelled. The first sweep
// fires after `interval`, not immediately — startup shouldn't do
// heavy work before /readyz flips. Errors are logged and the loop
// continues; a single bad sweep must not strand the goroutine.
func (s *Sweeper) Run(ctx context.Context) {
	if s.retentionDays == 0 {
		s.log.Info("am retention sweeper disabled (retentionDays == 0)")
		return
	}
	s.log.Info("am retention sweeper started",
		"retention_days", s.retentionDays,
		"interval", s.interval.String(),
	)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tickOnce(ctx)
		}
	}
}

// tickOnce performs one sweep iteration. Extracted for readability;
// production code only calls it from Run.
func (s *Sweeper) tickOnce(ctx context.Context) {
	cutoff := s.now().Add(-time.Duration(s.retentionDays) * 24 * time.Hour)
	rows, err := s.repo.SweepAMResolved(ctx, cutoff)
	if s.observer != nil {
		s.observer.AMRetentionSwept(rows, err)
	}
	if err != nil {
		s.log.Warn("am retention sweep failed",
			"retention_days", s.retentionDays,
			"cutoff", cutoff,
			"error", err,
		)
		return
	}
	if rows > 0 {
		s.log.Info("am retention sweep",
			"retention_days", s.retentionDays,
			"cutoff", cutoff,
			"rows_deleted", rows,
		)
	}
}
