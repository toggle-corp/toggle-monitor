// Package scheduler drives one ticker per monitor with startup jitter
// and in-cycle retries, honoring dependsOn gating and context cancel.
package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"reflect"
	"sort"
	"sync"
	"time"

	"golang.org/x/net/proxy"

	"github.com/toggle-corp/toggle-monitor/internal/alert"
	"github.com/toggle-corp/toggle-monitor/internal/probe"
	tmsentry "github.com/toggle-corp/toggle-monitor/internal/sentry"
	"github.com/toggle-corp/toggle-monitor/internal/slack"
	"github.com/toggle-corp/toggle-monitor/internal/store"
)

// Plan is everything the scheduler needs to drive one monitor. It is
// derived from the YAML config (config.Monitor) plus the global
// httpClient.userAgent — the config package owns the source of truth;
// the scheduler operates on this flatter shape so it doesn't need to
// import config.
type Plan struct {
	Slug string
	// Kind is the probe kind: "http" (default) or "smtp". Display +
	// plan-equality only; the actual probe behaviour lives in Prober.
	Kind         string
	FriendlyName string
	URL          string
	HTTPMethod   string
	// Host / Port / TLSMode describe an SMTP monitor; empty for HTTP.
	// Carried for the detail UI; the probe itself reads them via Prober.
	Host                string
	Port                int
	TLSMode             string
	AcceptedStatusCodes []int
	Interval            time.Duration
	Timeout             time.Duration
	Retries             int
	RetryBackoff        time.Duration
	FollowRedirects     bool
	// Prober runs one probe tick and returns the neutral probe.Result.
	// Resolved by the wiring layer (httpcheck.Config / smtpcheck.Config
	// both satisfy probe.Prober). The scheduler is probe-agnostic: it
	// never inspects Kind to decide how to probe, only Prober.
	Prober probe.Prober
	// TLSInsecureSkipVerify disables Go's TLS chain verification on
	// the probe — for endpoints with self-signed certs we intentionally
	// trust. Implies SSL state is forced to skipped.
	TLSInsecureSkipVerify bool
	// ProxyDialer routes the probe through an outbound proxy
	// (currently SOCKS5). Resolved from the YAML `proxies:` block at
	// startup, looked up by the lifecycle per monitor's proxy slug.
	// nil → direct dial. Carried for plan-equality; the Prober already
	// holds its own dialer.
	ProxyDialer proxy.Dialer
	// Proxy is the slug of the configured outbound proxy (or empty
	// for direct dial). Carried alongside ProxyDialer so the UI can
	// surface *which* proxy applies without reflecting on the dialer.
	Proxy string
	// Preset is the kube preset slug that produced this plan. Empty
	// for YAML-static monitors. Surfaced in the detail UI so the
	// operator can tell at a glance which preset drove a given
	// auto-discovered monitor's config.
	Preset           string
	UserAgent        string
	ReminderInterval time.Duration
	ChannelSlug      string   // slack destination slug; empty disables Slack output
	Mentions         []string // pre-resolved raw Slack markup (parent-only)
	DependsOn        []string // upstream static-monitor slugs; any of them down pauses this monitor

	// SSL thresholds; SSL evaluation is skipped when all are zero
	// (which is also the case for plaintext monitors).
	SSLAlertThreshold      time.Duration
	SSLEscalationThreshold time.Duration
	SSLReminderInterval    time.Duration
	// TLSBearing marks a monitor whose probe presents a certificate we
	// track for expiry (HTTPS, or SMTP with starttls/implicit). False →
	// SSL state is ssl-skipped. Generalizes the former IsHTTPS flag.
	TLSBearing bool
}

// EventSink is the seam the scheduler uses to dispatch alert events
// (open / reminder / resolve). Production wires
// slack.Notifier.Notify; tests can pass nil to disable. The monitor
// row is the snapshot read BEFORE ApplyCheck so callers see thread
// refs that are about to be cleared on resolve.
type EventSink func(ctx context.Context, m store.MonitorRow, channelSlug string, mentions []string, event *alert.Event) error

// SSLSink is the analogous seam for SSL events.
type SSLSink func(ctx context.Context, m store.MonitorRow, channelSlug string, mentions []string, event *alert.SSLEvent) error

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
	sink    EventSink
	sslSink SSLSink
	metrics Metrics
	log     *slog.Logger
	now     func() time.Time

	// missingParents tracks dependsOn references that didn't resolve
	// to a known monitor at lookup time. Surfaced on /issues so the
	// operator notices a typo or drift between config and DB without
	// having to grep the logs.
	missingMu      sync.RWMutex
	missingParents map[string]map[string]time.Time // parent slug → child slug → last-seen
}

// MissingParent is one entry in the dependsOn-failure listing.
type MissingParent struct {
	Parent   string    // the slug that didn't resolve
	Children []string  // monitors that reference it
	LastSeen time.Time // most recent time the lookup failed
}

// Option configures a Scheduler. Used by tests to inject a deterministic
// clock or fake check function.
type Option func(*Scheduler)

// WithNow overrides the time source (defaults to time.Now).
func WithNow(f func() time.Time) Option { return func(s *Scheduler) { s.now = f } }

// WithLogger overrides the logger (defaults to slog.Default()).
func WithLogger(l *slog.Logger) Option { return func(s *Scheduler) { s.log = l } }

// WithEventSink wires the Slack notifier (or any other consumer of
// alert events). Defaults to a no-op.
func WithEventSink(sink EventSink) Option { return func(s *Scheduler) { s.sink = sink } }

// WithSSLSink wires the Slack notifier for SSL events. Defaults to nil.
func WithSSLSink(sink SSLSink) Option { return func(s *Scheduler) { s.sslSink = sink } }

// WithMetrics wires the Prometheus metrics sink. Defaults to a no-op
// (tests that don't care about metrics need no setup).
func WithMetrics(m Metrics) Option { return func(s *Scheduler) { s.metrics = m } }

// New constructs a Scheduler with sensible defaults.
func New(repo *store.Repo, opts ...Option) *Scheduler {
	s := &Scheduler{
		repo: repo,
		log:  slog.Default(),
		now:  time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// PlanSource produces the current desired plan set. Called by
// RunDynamic each refresh interval to drive add/remove decisions.
// Implementations are expected to be cheap and safe to call from
// any goroutine.
type PlanSource interface {
	CurrentPlans() []Plan
}

// staticSource is the trivial PlanSource that always returns the
// same slice — used by Run() for back-compat.
type staticSource struct{ plans []Plan }

func (s staticSource) CurrentPlans() []Plan { return s.plans }

// Run starts one goroutine per plan and blocks until ctx is cancelled
// AND every goroutine has exited. Equivalent to RunDynamic with a
// static source.
func (s *Scheduler) Run(ctx context.Context, plans []Plan) {
	s.RunDynamic(ctx, staticSource{plans: plans}, 0)
}

// RunDynamic drives a *changing* plan set: every refreshInterval (or
// once if 0) it pulls the latest plans from source, spawns new
// goroutines for newly-appeared slugs, cancels goroutines whose
// slugs disappeared, and respawns the rest only when their plan
// changed (deep equal). Blocks until ctx is cancelled AND every
// goroutine has exited.
func (s *Scheduler) RunDynamic(ctx context.Context, source PlanSource, refreshInterval time.Duration) {
	type entry struct {
		plan   Plan
		cancel context.CancelFunc
	}
	running := map[string]entry{}
	var wg sync.WaitGroup

	spawn := func(p Plan) {
		monCtx, cancel := context.WithCancel(ctx)
		running[p.Slug] = entry{plan: p, cancel: cancel}
		wg.Add(1)
		go func(p Plan) {
			defer wg.Done()
			s.runMonitor(monCtx, p)
		}(p)
	}

	reconcile := func() {
		plans := source.CurrentPlans()
		desired := make(map[string]Plan, len(plans))
		for _, p := range plans {
			desired[p.Slug] = p
		}
		// Cancel removed.
		for slug, e := range running {
			if _, kept := desired[slug]; !kept {
				e.cancel()
				delete(running, slug)
			}
		}
		// Spawn new + restart changed.
		for slug, p := range desired {
			if existing, ok := running[slug]; ok {
				if plansEqual(existing.plan, p) {
					continue
				}
				existing.cancel()
				delete(running, slug)
			}
			spawn(p)
		}
	}

	reconcile()

	if refreshInterval <= 0 {
		// Static-set fast path: wait for ctx cancel then drain.
		<-ctx.Done()
		wg.Wait()
		return
	}

	t := time.NewTicker(refreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-t.C:
			reconcile()
		}
	}
}

// plansEqual reports whether two Plans match on every field used by
// the worker. Used to decide whether a slug whose entry is already
// running needs to be respawned with the new params.
func plansEqual(a, b Plan) bool {
	return reflect.DeepEqual(a, b)
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
		func() {
			// Recover panics per-tick so a single bad monitor cannot kill
			// the scheduler. The panic is reported to Sentry + logged;
			// the next tick proceeds normally.
			defer tmsentry.RecoverPanic(s.log, "scheduler.tick")
			if err := s.Tick(ctx, p); err != nil && !errors.Is(err, context.Canceled) {
				// WARN, not ERROR: a probed endpoint failing is the
				// tool's expected business signal — Prometheus +
				// Slack carry the operator-actionable surface. Routing
				// this through ERROR would flood Sentry for every
				// down-tick of every down monitor.
				s.log.Warn("tick error", "monitor", p.Slug, "error", err)
			}
		}()
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
		if s.anyParentDown(ctx, p.Slug, p.DependsOn) {
			if s.metrics != nil {
				s.metrics.ObserveCheck(p.Slug, "paused", 0)
			}
			return s.repo.MarkTemporaryPaused(ctx, p.Slug)
		}
	}

	var res probe.Result
	attempts := p.Retries + 1
	tickStart := time.Now()
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if !sleep(ctx, p.RetryBackoff) {
				return ctx.Err()
			}
		}
		res = p.Prober.Probe(ctx)
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

	// Resuming from temporary-paused: restore the pre-pause uptime
	// classification so the state machine continues the prior story
	// instead of double-emitting transitions. MarkTemporaryPaused
	// only overwrites `status` — opened_at / last_reminder_at are
	// preserved, so a non-nil OpenedAt means the child was already
	// in an open incident when the parent went down. Forcing prev=up
	// in that case would produce a duplicate EventOpen on the next
	// failing tick (the original user-reported bug).
	if row.Status == alert.StatusTemporaryPaused {
		if row.OpenedAt != nil {
			row.Status = alert.StatusDown
		} else {
			row.Status = alert.StatusUp
		}
	}

	outcome := alert.OutcomeFail
	if res.Error == "" {
		outcome = alert.OutcomeOK
	}
	now := s.now()
	check := alert.Check{
		Outcome:          outcome,
		At:               now,
		StatusCode:       res.Code,
		Error:            res.Error,
		ReminderInterval: p.ReminderInterval,
	}
	nextState, event := alert.Apply(row.State(), check)
	if err := s.repo.ApplyCheck(ctx, p.Slug, nextState, now, res.Code, res.Error, event); err != nil {
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
			logSinkError(s.log, "event sink", p.Slug, string(event.Type), err)
			// Don't propagate: the DB transition is committed; on a
			// transient/exhausted failure the next reminder tick
			// synthesizes a fresh parent via the notifier.
		}
	}

	// SSL state machine, driven independently from the uptime side.
	// Plaintext monitors (HTTP, or SMTP tls:none) get ssl-skipped;
	// TLS-bearing monitors check against the configured thresholds when
	// the probe captured cert info (it won't have if the probe
	// transport-failed). tlsInsecureSkipVerify implies "don't track SSL
	// expiry": present the SSL state machine with TLSBearing=false so it
	// routes to SSLStatusSkipped and emits no events.
	tlsBearingForSSL := p.TLSBearing && !p.TLSInsecureSkipVerify
	sslCheck := alert.SSLCheck{
		At:                  now,
		TLSBearing:          tlsBearingForSSL,
		AlertThreshold:      p.SSLAlertThreshold,
		EscalationThreshold: p.SSLEscalationThreshold,
		ReminderInterval:    p.SSLReminderInterval,
	}
	var issuer, subject string
	if res.TLS != nil {
		sslCheck.ExpiresAt = res.TLS.NotAfter
		issuer = res.TLS.Issuer
		subject = res.TLS.Subject
	}
	prevSSL := row.SSL()
	nextSSL, sslEvent := alert.ApplySSL(prevSSL, sslCheck)
	if err := s.repo.ApplySSLCheck(ctx, p.Slug, nextSSL, sslCheck.ExpiresAt, issuer, subject, sslEvent); err != nil {
		// WARN, not ERROR: the uptime side is already committed and the
		// next tick will retry. Not Sentry-worthy.
		s.log.Warn("apply ssl check", "monitor", p.Slug, "error", err)
		return nil // not fatal — uptime side already committed
	}

	if s.metrics != nil && sslEvent != nil {
		switch sslEvent.Type {
		case alert.EventSSLOpen:
			s.metrics.SetActiveIncident("ssl", p.Slug, true)
		case alert.EventSSLResolve:
			s.metrics.SetActiveIncident("ssl", p.Slug, false)
		}
	}

	if sslEvent != nil && s.sslSink != nil && p.ChannelSlug != "" {
		if err := s.sslSink(ctx, row, p.ChannelSlug, p.Mentions, sslEvent); err != nil {
			logSinkError(s.log, "ssl sink", p.Slug, string(sslEvent.Type), err)
		}
	}
	return nil
}

// anyParentDown reports whether any of the listed monitor slugs is
// currently in StatusDown. Missing parents are skipped (recorded
// in-memory for the /issues page, and logged at most once per
// (parent, child) pair to avoid hammering stdout on every check)
// so a transient outage of a single dependency lookup doesn't
// ripple into the wrong gating decision.
func (s *Scheduler) anyParentDown(ctx context.Context, child string, parents []string) bool {
	for _, slug := range parents {
		row, err := s.repo.GetMonitor(ctx, slug)
		if err != nil {
			if s.recordMissingParent(slug, child) {
				s.log.Warn("dependsOn parent lookup failed", "parent", slug, "child", child, "error", err)
			}
			continue
		}
		// Successful lookup → drop any stale missing-parent entry
		// so the operator stops seeing the issue once they've
		// reconciled the config (no restart needed).
		s.ClearMissingParent(slug)
		if row.Status == alert.StatusDown {
			return true
		}
	}
	return false
}

// recordMissingParent stamps the (parent, child) pair into the
// in-memory map. Returns true the first time we see this pair (so
// the caller can log it once) and false on subsequent ticks.
func (s *Scheduler) recordMissingParent(parent, child string) bool {
	s.missingMu.Lock()
	defer s.missingMu.Unlock()
	if s.missingParents == nil {
		s.missingParents = map[string]map[string]time.Time{}
	}
	kids, ok := s.missingParents[parent]
	if !ok {
		kids = map[string]time.Time{}
		s.missingParents[parent] = kids
	}
	_, seen := kids[child]
	kids[child] = s.now()
	return !seen
}

// MissingParents returns a snapshot of dependsOn references that
// didn't resolve. Sorted by parent slug for stable rendering. The
// returned slice is safe to read without holding any scheduler lock.
func (s *Scheduler) MissingParents() []MissingParent {
	s.missingMu.RLock()
	defer s.missingMu.RUnlock()
	if len(s.missingParents) == 0 {
		return nil
	}
	out := make([]MissingParent, 0, len(s.missingParents))
	for parent, kids := range s.missingParents {
		children := make([]string, 0, len(kids))
		var last time.Time
		for child, t := range kids {
			children = append(children, child)
			if t.After(last) {
				last = t
			}
		}
		sort.Strings(children)
		out = append(out, MissingParent{Parent: parent, Children: children, LastSeen: last})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Parent < out[j].Parent })
	return out
}

// ClearMissingParent drops the entry for `parent` from the in-memory
// map. Called when a fresh plan reconciliation observes the parent
// is now present, so a resolved typo stops showing on /issues
// without an operator restart.
func (s *Scheduler) ClearMissingParent(parent string) {
	s.missingMu.Lock()
	defer s.missingMu.Unlock()
	delete(s.missingParents, parent)
}

// logSinkError routes a Slack sink failure to WARN or ERROR depending
// on its kind. Transient errors (DNS races, 5xx, 429 — eventually
// exhausted by the client's retry loop) are operator-noise and stay
// at WARN, off the Sentry path. Persistent (auth/channel/scope) and
// permanent-bug (malformed payload) errors are operator-actionable
// and route to ERROR → Sentry.
//
// Context cancellation surfaces as a clean cancel — neither WARN nor
// ERROR; it just means we're shutting down.
func logSinkError(log *slog.Logger, where, monitorSlug, eventType string, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	var se *slack.SlackError
	if errors.As(err, &se) {
		args := []any{
			"monitor", monitorSlug,
			"event", eventType,
			"slack_method", se.Method,
			"slack_code", se.Code,
			"attempts", se.Attempts,
			"elapsed_ms", se.Elapsed.Milliseconds(),
			"error", err,
		}
		if se.Kind == slack.KindTransient {
			log.Warn(where+": transient slack failure", args...)
			return
		}
		log.Error(where+": slack failure", args...)
		return
	}
	log.Error(where, "monitor", monitorSlug, "event", eventType, "error", err)
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
