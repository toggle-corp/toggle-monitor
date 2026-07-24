package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/coalesce"
	"github.com/toggle-corp/toggle-monitor/internal/selfhealth"
	"github.com/toggle-corp/toggle-monitor/internal/slack"
)

// selfHealthMetrics is the slim seam the notifier uses to emit the
// self-degraded gauge + entry counter (ADR-0008). Satisfied by
// *observability.Metrics; faked in tests. Kept narrow so the notice
// logic is testable without the whole registry.
type selfHealthMetrics interface {
	SetSelfDegraded(degraded bool)
	SelfDegradedEntry()
}

// commitFunc runs a deferred DNS provisional through the normal alert
// path (re-probe → alert.Apply → route), pages it ~W late. Supplied by
// lifecycle; nil is tolerated (the failure is simply not committed).
type commitFunc func(ctx context.Context, slug string)

// selfHealthNotifier drives one self-health incident (open/close) off
// the detector's per-window Decision. It posts a single digest-style
// notice to the configured channel via the shared coalesce.Poster, with
// an ensurePosted-style self-heal (Slack usually needs DNS too, so the
// open post normally cannot land until connectivity returns, at which
// point it lands as one post-hoc summary). With no channel configured
// it is metric + log only. Not fanned out to per-service channels — when
// blind we don't even know which services are truly affected.
type selfHealthNotifier struct {
	det     *selfhealth.Detector
	poster  coalesce.Poster
	channel string // empty → metric + log only
	mention string // optional raw escalation markup
	metrics selfHealthMetrics
	commit  commitFunc
	log     *slog.Logger

	// degraded mirrors the detector state across ticks so the notifier
	// knows whether it still owes an open post (self-heal) or a close.
	degraded   bool
	degradedAt time.Time
	suppressed int    // checks suppressed since entry (for the close summary)
	openTS     string // digest ts of the posted open notice; "" = not yet posted
}

// newSelfHealthNotifier constructs the notifier. metrics may be nil
// (no-op); commit may be nil.
func newSelfHealthNotifier(
	det *selfhealth.Detector,
	poster coalesce.Poster,
	channel, mention string,
	metrics selfHealthMetrics,
	commit commitFunc,
	log *slog.Logger,
) *selfHealthNotifier {
	return &selfHealthNotifier{
		det:     det,
		poster:  poster,
		channel: channel,
		mention: mention,
		metrics: metrics,
		commit:  commit,
		log:     log,
	}
}

// run drives the notifier: one Decide + notice pass per window. Blocks
// until ctx is cancelled. This is the ADR-0008 "central evaluator" for
// self-health, analogous to coalesce.Manager.RunEvaluator.
func (n *selfHealthNotifier) run(ctx context.Context, window time.Duration) {
	t := time.NewTicker(window)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.tick(ctx, time.Now())
		}
	}
}

// tick runs one decision + notice pass at time now. Exported-to-tests
// via the package-internal test; the run loop calls it on the window
// cadence.
func (n *selfHealthNotifier) tick(ctx context.Context, now time.Time) {
	dec := n.det.Decide(now)

	switch {
	case dec.Entered:
		n.degraded = true
		n.degradedAt = now
		n.suppressed = 0
		n.openTS = ""
		if n.metrics != nil {
			n.metrics.SetSelfDegraded(true)
			n.metrics.SelfDegradedEntry()
		}
		n.log.Warn("self-health: entered degraded mode (monitor blind)", "at", now)
	case dec.Exited:
		n.suppressed = dec.Suppressed
		n.postClose(ctx, now)
		n.degraded = false
		n.openTS = ""
		if n.metrics != nil {
			n.metrics.SetSelfDegraded(false)
		}
		n.log.Info("self-health: exited degraded mode (connectivity restored)",
			"blind_for", now.Sub(n.degradedAt), "suppressed", n.suppressed)
	}

	// Committed isolated failures page normally, ~W late.
	if len(dec.Commit) > 0 && n.commit != nil {
		for _, slug := range dec.Commit {
			n.commit(ctx, slug)
		}
	}

	// While degraded, (re)post the open notice. It is a no-op once landed
	// (openTS set); on the entry tick and on any tick where a prior post
	// failed (Slack needs DNS too), this lands it — ensurePosted-style.
	if n.degraded {
		n.postOpen(ctx)
	}
}

// postOpen posts the "monitoring degraded" open notice, unless it is
// already posted (openTS set) or no channel is configured. A failed
// post leaves openTS empty so the next tick retries.
func (n *selfHealthNotifier) postOpen(ctx context.Context) {
	if n.channel == "" || n.openTS != "" {
		return
	}
	_, ts, err := n.poster.PostDigest(ctx, n.channel, n.buildOpen())
	if err != nil {
		n.log.Warn("self-health: post degraded notice (will retry)", "channel", n.channel, "error", err)
		return
	}
	n.openTS = ts
}

// postClose edits the open notice to a resolution, or posts a fresh
// close if the open never landed. No-op when no channel is configured.
func (n *selfHealthNotifier) postClose(ctx context.Context, now time.Time) {
	if n.channel == "" {
		return
	}
	if n.openTS == "" {
		// The open never landed (blind the whole time); post the close
		// as a single post-hoc summary instead.
		if _, _, err := n.poster.PostDigest(ctx, n.channel, n.buildClose(now)); err != nil {
			n.log.Warn("self-health: post close notice", "channel", n.channel, "error", err)
		}
		return
	}
	if err := n.poster.UpdateDigest(ctx, n.channel, n.openTS, n.buildClose(now)); err != nil {
		n.log.Warn("self-health: edit close notice", "channel", n.channel, "error", err)
	}
}

func (n *selfHealthNotifier) buildOpen() slack.ParentMessage {
	body := fmt.Sprintf("Lost connectivity at %s; probe results suppressed.",
		n.degradedAt.Format(time.RFC1123))
	if n.mention != "" {
		body = n.mention + " " + body
	}
	return slack.ParentMessage{
		Blocks: []slack.Block{
			{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "⚠️ *Monitoring degraded*"}},
			{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": body}},
		},
	}
}

func (n *selfHealthNotifier) buildClose(now time.Time) slack.ParentMessage {
	body := fmt.Sprintf("Blind for %s, %d checks suppressed, resuming.",
		now.Sub(n.degradedAt).Round(time.Second), n.suppressed)
	return slack.ParentMessage{
		Blocks: []slack.Block{
			{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "✅ *Connectivity restored*"}},
			{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": body}},
		},
	}
}
