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
	ReminderInterval    time.Duration
	ChannelSlug         string   // slack destination slug; empty disables Slack output
	Mentions            []string // pre-resolved raw Slack markup (parent-only)
	DependsOn           []string // upstream static-monitor slugs; any of them down pauses this monitor
}

// CheckFunc is the seam used to run a probe; production wires
// httpcheck.Check, tests wire a fake.
type CheckFunc func(ctx context.Context, cfg httpcheck.Config) httpcheck.Result

// EventSink is the seam the scheduler uses to dispatch alert events
// (open / reminder / resolve). Production wires
// slack.Notifier.Notify; tests can pass nil to disable. The monitor
// row is the snapshot read BEFORE ApplyCheck so callers see thread
// refs that are about to be cleared on resolve.
type EventSink func(ctx context.Context, m store.MonitorRow, channelSlug string, mentions []string, event *alert.Event) error

// Metrics is the slim seam the scheduler uses to emit Prometheus
// data points. Production wires observability.Metrics; tests pass
// nil to disable.
type Metrics interface {
	ObserveCheck(monitor string, status string, duration time.Duration)
	SetWorkerLastTick(unixSeconds float64)
	SetActiveIncident(typeLabel, monitor string, active bool)
}

// Scheduler drives the configured set of monitors.
type Scheduler struct {
	repo    *store.Repo
	check   CheckFunc
	sink    EventSink
	metrics Metrics
	log     *slog.Logger
	now     func() time.Time
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

// WithEventSink wires the Slack notifier (or any other consumer of
// alert events). Defaults to a no-op.
func WithEventSink(sink EventSink) Option { return func(s *Scheduler) { s.sink = sink } }

// WithMetrics wires the Prometheus metrics sink. Defaults to a no-op
// (tests that don't care about metrics need no setup).
func WithMetrics(m Metrics) Option { return func(s *Scheduler) { s.metrics = m } }

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
	// dependsOn gate: any parent down → skip the probe and mark
	// the child temporary-paused. No HTTP, no DB write to last_*,
	// no alert event.
	if len(p.DependsOn) > 0 {
		paused, err := s.anyParentDown(ctx, p.DependsOn)
		if err != nil {
			return err
		}
		if paused {
			if s.metrics != nil {
				s.metrics.ObserveCheck(p.Slug, "paused", 0)
			}
			return s.repo.MarkTemporaryPaused(ctx, p.Slug)
		}
	}

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
	tickStart := time.Now()
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

	if s.metrics != nil {
		status := "ok"
		if res.Error != "" {
			status = "fail"
		}
		s.metrics.ObserveCheck(p.Slug, status, time.Since(tickStart))
		s.metrics.SetWorkerLastTick(float64(s.now().Unix()))
	}

	row, err := s.repo.GetMonitor(ctx, p.Slug)
	if err != nil {
		return err
	}

	// Resuming from temporary-paused: treat the SM's "previous"
	// status as up so a failing first tick produces a fresh open,
	// not a doubled transition. Pre-pause runtime fields are
	// already cleared/irrelevant because MarkTemporaryPaused doesn't
	// touch last_reminder_at / opened_at; ensure OpenedAt is zero
	// here so the open event picks up the current tick time.
	if row.Status == alert.StatusTemporaryPaused {
		row.Status = alert.StatusUp
		row.OpenedAt = nil
		row.LastReminderAt = nil
	}

	outcome := alert.OutcomeFail
	if res.Error == "" {
		outcome = alert.OutcomeOK
	}
	now := s.now()
	check := alert.Check{
		Outcome:          outcome,
		At:               now,
		StatusCode:       res.StatusCode,
		Error:            res.Error,
		ReminderInterval: p.ReminderInterval,
	}
	nextState, event := alert.Apply(row.State(), check)
	if err := s.repo.ApplyCheck(ctx, p.Slug, nextState, now, res.StatusCode, res.Error, event); err != nil {
		return err
	}

	// Active-incident gauge: 1 while down, 0 while up.
	if s.metrics != nil && event != nil {
		switch event.Type {
		case alert.EventOpen:
			s.metrics.SetActiveIncident("uptime", p.Slug, true)
		case alert.EventResolve:
			s.metrics.SetActiveIncident("uptime", p.Slug, false)
		}
	}

	// Dispatch to the event sink AFTER the DB transaction has
	// committed. We pass the *pre*-update row so the resolve handler
	// still sees the uptime thread refs that ApplyCheck just cleared.
	if event != nil && s.sink != nil && p.ChannelSlug != "" {
		if err := s.sink(ctx, row, p.ChannelSlug, p.Mentions, event); err != nil {
			s.log.Error("event sink", "monitor", p.Slug, "event", event.Type, "error", err)
			// Don't propagate: the DB transition is committed; the
			// Slack post can be retried on a later tick. Issue 16
			// owns the full retry policy.
		}
	}
	return nil
}

// anyParentDown reports whether any of the listed monitor slugs is
// currently in StatusDown. Missing parents are skipped (logged for
// visibility) so a transient outage of a single dependency lookup
// doesn't ripple into the wrong gating decision.
func (s *Scheduler) anyParentDown(ctx context.Context, parents []string) (bool, error) {
	for _, slug := range parents {
		row, err := s.repo.GetMonitor(ctx, slug)
		if err != nil {
			s.log.Warn("dependsOn parent lookup failed", "parent", slug, "error", err)
			continue
		}
		if row.Status == alert.StatusDown {
			return true, nil
		}
	}
	return false, nil
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
