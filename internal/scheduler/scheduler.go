// Package scheduler drives one ticker per monitor with startup jitter
// and in-cycle retries, honoring dependsOn gating and context cancel.
package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/alert"
	"github.com/toggle-corp/toggle-monitor/internal/httpcheck"
	"github.com/toggle-corp/toggle-monitor/internal/store"
)

// Plan is everything the scheduler needs to drive one monitor. It is
// derived from the YAML config (config.Monitor) plus the global
// httpClient.userAgent — the config package owns the source of truth;
// the scheduler operates on this flatter shape so it doesn't need to
// import config.
type Plan struct {
	Slug                string
	FriendlyName        string
	URL                 string
	HTTPMethod          string
	AcceptedStatusCodes []int
	Interval            time.Duration
	Timeout             time.Duration
	Retries             int
	RetryBackoff        time.Duration
	FollowRedirects     bool
	UserAgent           string
}

// CheckFunc is the seam used to run a probe; production wires
// httpcheck.Check, tests wire a fake.
type CheckFunc func(ctx context.Context, cfg httpcheck.Config) httpcheck.Result

// Scheduler drives the configured set of monitors.
type Scheduler struct {
	repo  *store.Repo
	check CheckFunc
	log   *slog.Logger
	now   func() time.Time
}

// Option configures a Scheduler. Used by tests to inject a deterministic
// clock or fake check function.
type Option func(*Scheduler)

// WithCheck overrides the probe function (defaults to httpcheck.Check).
func WithCheck(c CheckFunc) Option { return func(s *Scheduler) { s.check = c } }

// WithNow overrides the time source (defaults to time.Now).
func WithNow(f func() time.Time) Option { return func(s *Scheduler) { s.now = f } }

// WithLogger overrides the logger (defaults to slog.Default()).
func WithLogger(l *slog.Logger) Option { return func(s *Scheduler) { s.log = l } }

// New constructs a Scheduler with sensible defaults.
func New(repo *store.Repo, opts ...Option) *Scheduler {
	s := &Scheduler{
		repo:  repo,
		check: httpcheck.Check,
		log:   slog.Default(),
		now:   time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Run starts one goroutine per plan and blocks until ctx is cancelled
// AND every goroutine has exited. Each goroutine performs startup
// jitter, then ticks at the monitor's interval.
func (s *Scheduler) Run(ctx context.Context, plans []Plan) {
	var wg sync.WaitGroup
	for _, p := range plans {
		wg.Add(1)
		go func(p Plan) {
			defer wg.Done()
			s.runMonitor(ctx, p)
		}(p)
	}
	wg.Wait()
}

func (s *Scheduler) runMonitor(ctx context.Context, p Plan) {
	// Startup jitter: rand(0, interval) before the first tick.
	if p.Interval > 0 {
		jitter := time.Duration(rand.Int64N(int64(p.Interval)))
		if !sleep(ctx, jitter) {
			return
		}
	}

	for {
		if err := s.Tick(ctx, p); err != nil && !errors.Is(err, context.Canceled) {
			s.log.Error("tick error", "monitor", p.Slug, "error", err)
		}
		if !sleep(ctx, p.Interval) {
			return
		}
	}
}

// Tick performs one check cycle (including in-cycle retries) and
// applies the resulting transition (if any) to the store. Exported so
// integration tests can drive the scheduler without running the
// jittered loop.
func (s *Scheduler) Tick(ctx context.Context, p Plan) error {
	cfg := httpcheck.Config{
		URL:                 p.URL,
		Method:              p.HTTPMethod,
		AcceptedStatusCodes: p.AcceptedStatusCodes,
		Timeout:             p.Timeout,
		FollowRedirects:     p.FollowRedirects,
		UserAgent:           p.UserAgent,
	}

	var res httpcheck.Result
	attempts := p.Retries + 1
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if !sleep(ctx, p.RetryBackoff) {
				return ctx.Err()
			}
		}
		res = s.check(ctx, cfg)
		if ctx.Err() != nil {
			// SIGTERM mid-check: do NOT record context cancellation as
			// failure (per design — it's not signal about the
			// monitored service).
			return ctx.Err()
		}
		if res.Error == "" {
			break
		}
	}

	row, err := s.repo.GetMonitor(ctx, p.Slug)
	if err != nil {
		return err
	}

	outcome := alert.OutcomeFail
	if res.Error == "" {
		outcome = alert.OutcomeOK
	}
	now := s.now()
	check := alert.Check{
		Outcome:    outcome,
		At:         now,
		StatusCode: res.StatusCode,
		Error:      res.Error,
	}
	nextState, event := alert.Apply(row.State(), check)
	return s.repo.ApplyCheck(ctx, p.Slug, nextState, now, res.StatusCode, res.Error, event)
}

// sleep returns false if ctx was cancelled before the duration elapsed.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
