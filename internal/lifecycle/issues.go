package lifecycle

import (
	"context"
	"log/slog"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/observability"
	tmsentry "github.com/toggle-corp/toggle-monitor/internal/sentry"
)

// Local aliases for the gauge's source labels. The constants live in
// internal/observability because they are part of the metric's public
// contract, not this file's private vocabulary.
const (
	issueSourceKubeInvalid   = observability.IssueSourceKubeInvalid
	issueSourceSlackMapping  = observability.IssueSourceSlackMapping
	issueSourceMissingParent = observability.IssueSourceMissingParent
	issueSourceAnnotation    = observability.IssueSourceAnnotation
)

// issuesRefreshInterval is how often the gauge is recomputed. Issues
// are slow-moving — a kube reconcile runs every resyncInterval (30m by
// default) — so this only needs to be well inside a Prometheus scrape's
// staleness window, not near-real-time.
const issuesRefreshInterval = time.Minute

// issuesReporter publishes the /issues sources as a Prometheus gauge so
// they can be alerted on rather than only noticed by an operator who
// happens to open the page. Each source is a closure over the same
// reader the web handler uses, so the gauge and the page cannot drift.
type issuesReporter struct {
	metrics *observability.Metrics
	// Each source reports its count and whether that count is
	// trustworthy this tick. A source that cannot read its input (a DB
	// blip, say) returns false and its series is left alone — writing a
	// zero there would resolve a real alert on nothing more than a
	// transient error.
	counts map[string]func(context.Context) (int, bool)
	log    *slog.Logger
}

// refresh recomputes every source. A source that reports a count writes
// it even when the count is zero — a gauge that stops emitting a series
// leaves any alert on it stuck firing, because the expression has
// nothing left to evaluate to false. A source that cannot read its
// input reports no count at all and its series is left untouched.
func (r *issuesReporter) refresh(ctx context.Context) {
	for source, count := range r.counts {
		n, ok := count(ctx)
		if !ok {
			continue
		}
		r.metrics.Issues.WithLabelValues(source).Set(float64(n))
	}
}

// infallibleSource adapts a source that cannot fail to the counts
// signature. Only kube-invalid reads through the DB and can come back
// unreadable; wrapping the other three here keeps that asymmetry
// visible instead of scattering `return n, true` across every closure.
func infallibleSource(f func() int) func(context.Context) (int, bool) {
	return func(context.Context) (int, bool) { return f(), true }
}

// run refreshes immediately and then on a ticker until ctx is done.
func (r *issuesReporter) run(ctx context.Context) {
	tick := func() {
		defer tmsentry.RecoverPanic(r.log, "lifecycle.issuesReporter")
		r.refresh(ctx)
	}
	tick()
	t := time.NewTicker(issuesRefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}
