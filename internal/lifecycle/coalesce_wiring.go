package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/alert"
	"github.com/toggle-corp/toggle-monitor/internal/coalesce"
	"github.com/toggle-corp/toggle-monitor/internal/depindex"
	"github.com/toggle-corp/toggle-monitor/internal/scheduler"
	"github.com/toggle-corp/toggle-monitor/internal/slack"
	"github.com/toggle-corp/toggle-monitor/internal/store"
)

// digestPoster adapts *slack.Client to coalesce.Poster. It resolves each
// channel slug to its ChannelInfo (ID + bot token) via the same lookup
// the notifier uses, then posts/edits/replies to the digest message.
type digestPoster struct {
	client   *slack.Client
	channels func(slug string) (slack.ChannelInfo, bool)
}

func (p *digestPoster) PostDigest(ctx context.Context, channelSlug string, msg slack.ParentMessage) (string, string, error) {
	ch, ok := p.channels(channelSlug)
	if !ok {
		return "", "", fmt.Errorf("slack channel slug %q is not registered", channelSlug)
	}
	res, err := p.client.PostMessage(ctx, ch.Token, slack.PostMessageInput{
		ChannelID:   ch.ID,
		Blocks:      msg.Blocks,
		Attachments: msg.Attachments,
	})
	if err != nil {
		return "", "", err
	}
	return res.Channel, res.TS, nil
}

func (p *digestPoster) UpdateDigest(ctx context.Context, channelSlug, ts string, msg slack.ParentMessage) error {
	ch, ok := p.channels(channelSlug)
	if !ok {
		return fmt.Errorf("slack channel slug %q is not registered", channelSlug)
	}
	return p.client.UpdateMessage(ctx, ch.Token, slack.UpdateMessageInput{
		ChannelID:   ch.ID,
		TS:          ts,
		Blocks:      msg.Blocks,
		Attachments: msg.Attachments,
	})
}

func (p *digestPoster) Reply(ctx context.Context, channelSlug, ts string, blocks []slack.Block) error {
	ch, ok := p.channels(channelSlug)
	if !ok {
		return fmt.Errorf("slack channel slug %q is not registered", channelSlug)
	}
	_, err := p.client.PostMessage(ctx, ch.Token, slack.PostMessageInput{
		ChannelID: ch.ID,
		ThreadTS:  ts,
		Blocks:    blocks,
	})
	return err
}

// groupSinkAdapter adapts *coalesce.Manager to scheduler.GroupSink,
// translating the scheduler's GroupMember into coalesce.MemberInfo.
type groupSinkAdapter struct{ m *coalesce.Manager }

func (a groupSinkAdapter) Down(ctx context.Context, channel string, gm scheduler.GroupMember, at time.Time) {
	a.m.Route(ctx, channel, coalesce.Entry{
		Member: coalesce.MemberInfo{
			Slug:         gm.Slug,
			FriendlyName: gm.FriendlyName,
			Mentions:     gm.Mentions,
		},
		Event: &alert.Event{Type: alert.EventOpen, At: at},
	})
}

func (a groupSinkAdapter) Up(ctx context.Context, channel, slug string, at time.Time) {
	a.m.Up(ctx, channel, slug, at)
}

func (a groupSinkAdapter) Pause(ctx context.Context, channel, slug string, at time.Time) {
	a.m.Pause(ctx, channel, slug, at)
}

// Route is the ADR-0004 primary entrypoint: it translates the
// scheduler's per-event payload into a coalesce.Entry, which the
// dispatcher branches on. Row + mentions flow through so the
// dispatcher's individual-mode flush can call the per-monitor
// notifier with the same payload the legacy EventSink path used.
func (a groupSinkAdapter) Route(ctx context.Context, channel string, gm scheduler.GroupMember, row store.MonitorRow, mentions []string, event *alert.Event) {
	a.m.Route(ctx, channel, coalesce.Entry{
		Member: coalesce.MemberInfo{
			Slug:         gm.Slug,
			FriendlyName: gm.FriendlyName,
			Mentions:     gm.Mentions,
		},
		Row:      row,
		Event:    event,
		Mentions: mentions,
	})
}

// dispatchPauser is the subset of *coalesce.Manager that
// push-propagation needs — kept narrow so makePushPropagation is
// trivially testable with a fake.
type dispatchPauser interface {
	Pause(ctx context.Context, channel, slug string, at time.Time)
}

// planProvider is the subset of *combinedPlanSource that
// push-propagation needs (a snapshot of currently-active plans for
// the reverse-dependsOn walk and channel lookup).
type planProvider interface {
	CurrentPlans() []scheduler.Plan
}

// makeOnDemandParentProbe builds the hot-parent probe hook the
// dispatcher invokes at pendingWait expiry (ADR-0004 Q11a). For each
// candidate parent: fire one bounded probe through the parent's
// configured Prober; if down, drive alert.Apply + persist its
// EventOpen, fire push-propagation so the pool's children drain, and
// route the parent's failure through the dispatcher for its own
// channel (so the parent ends up named in some Slack artifact rather
// than silently failing).
//
// Lifecycle owns construction so the callback can capture the same
// repo / planSource / push closure used elsewhere. Errors are logged
// (best-effort) — a probe failure must never crash the evaluator
// goroutine.
func makeOnDemandParentProbe(
	repo *store.Repo,
	mgr *coalesce.Manager,
	plans planProvider,
	push scheduler.PushPropagation,
	timeout time.Duration,
	now func() time.Time,
	log *slog.Logger,
) coalesce.OnDemandParentProbe {
	return func(ctx context.Context, parentSlug string) {
		var pl scheduler.Plan
		var found bool
		for _, p := range plans.CurrentPlans() {
			if p.Slug == parentSlug {
				pl = p
				found = true
				break
			}
		}
		if !found || pl.Prober == nil {
			return
		}

		probeCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		res := pl.Prober.Probe(probeCtx)
		if probeCtx.Err() != nil || res.Error == "" {
			return // up, or probe timed out inconclusively
		}

		row, err := repo.GetMonitor(ctx, parentSlug)
		if err != nil {
			log.Warn("on-demand probe: get monitor", "parent", parentSlug, "error", err)
			return
		}
		// Mirror scheduler.Tick's resume-from-paused logic so a parent
		// that was temporary-paused (because IT had a dependsOn parent
		// down) gets the right prev-status.
		prev := row.State()
		if row.Status == alert.StatusTemporaryPaused {
			if row.OpenedAt != nil {
				prev.Status = alert.StatusDown
			} else {
				prev.Status = alert.StatusUp
			}
		}

		at := now()
		check := alert.Check{
			Outcome:          alert.OutcomeFail,
			At:               at,
			StatusCode:       res.Code,
			Error:            res.Error,
			ReminderInterval: pl.ReminderInterval,
		}
		next, event := alert.Apply(prev, check)

		if event != nil && event.Type == alert.EventOpen {
			if err := repo.ApplyCheck(ctx, parentSlug, next, at, res.Code, res.Error, event); err != nil {
				log.Warn("on-demand probe: apply check", "parent", parentSlug, "error", err)
				return
			}
			// Push-propagation drains the children from the dispatcher's pool.
			if push != nil {
				push(ctx, parentSlug, at)
			}
			// Route the parent's EventOpen through its own channel so the
			// parent shows up in Slack (critical → caller's separate
			// EventSink path; non-critical → dispatcher).
			if !pl.Critical && pl.ChannelSlug != "" {
				mgr.Route(ctx, pl.ChannelSlug, coalesce.Entry{
					Member: coalesce.MemberInfo{
						Slug:         pl.Slug,
						FriendlyName: pl.FriendlyName,
						Mentions:     pl.Mentions,
					},
					Row:      row,
					Event:    event,
					Mentions: pl.Mentions,
				})
			}
			return
		}

		// Parent already known-down (no fresh EventOpen). Push-propagation
		// may not have fired from its own tick yet — fire it now so the
		// children get drained.
		if push != nil {
			push(ctx, parentSlug, at)
		}
	}
}

// makePushPropagation builds the parent-EventOpen → reverse-deps hook
// described in ADR-0004. The closure rebuilds the depindex from a
// fresh CurrentPlans snapshot on each invocation: kube discovery can
// add/remove monitors at any time, and push-propagation is rare enough
// that an O(N·D) rebuild per call is cheaper than maintaining a live
// inverted index in sync with discovery.
//
// For each child of the parent: MarkTemporaryPaused in the store
// (best-effort — a store error is logged but the loop continues so a
// single bad row doesn't strand the whole burst), then call
// dispatcher.Pause on the child's channel to drain the pending pool
// (or render the child as paused in an already-open digest).
func makePushPropagation(repo *store.Repo, disp dispatchPauser, plans planProvider, log *slog.Logger) scheduler.PushPropagation {
	return func(ctx context.Context, parentSlug string, at time.Time) {
		current := plans.CurrentPlans()
		specs := make([]depindex.Spec, len(current))
		channelOf := make(map[string]string, len(current))
		for i, p := range current {
			specs[i] = depindex.Spec{Slug: p.Slug, DependsOn: p.DependsOn}
			channelOf[p.Slug] = p.ChannelSlug
		}
		idx := depindex.Build(specs)
		for _, child := range idx.Children(parentSlug) {
			if err := repo.MarkTemporaryPaused(ctx, child); err != nil {
				log.Warn("push-propagation: mark paused",
					"parent", parentSlug, "child", child, "error", err)
				continue
			}
			if channel := channelOf[child]; channel != "" {
				disp.Pause(ctx, channel, child, at)
			}
		}
	}
}
