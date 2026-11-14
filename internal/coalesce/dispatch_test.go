package coalesce

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/alert"
	"github.com/toggle-corp/toggle-monitor/internal/group"
	"github.com/toggle-corp/toggle-monitor/internal/slack"
	"github.com/toggle-corp/toggle-monitor/internal/store"
)

// fakeSink records IndividualSink calls so tests can assert the
// dispatcher emits one per-monitor message per pool entry when a
// pending burst flushes sub-threshold.
type fakeSink struct {
	mu    sync.Mutex
	calls []sinkCall
}

type sinkCall struct {
	channel string
	slug    string
	event   alert.EventType
}

func (f *fakeSink) Notify(_ context.Context, row store.MonitorRow, channel string, _ []string, ev *alert.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, sinkCall{channel: channel, slug: row.Slug, event: ev.Type})
	return nil
}

func (f *fakeSink) countByType(et alert.EventType) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.event == et {
			n++
		}
	}
	return n
}

// newDispatchManager wires a Manager with the burst-dispatcher options
// the new tests exercise. burstThreshold=2 keeps timeline arithmetic
// tight while still distinguishing sub-threshold from at-threshold.
// A zero burstWindow takes the Manager's own default.
func newDispatchManager(t *testing.T, clock *time.Time, burstThreshold int, burstWindow time.Duration) (*Manager, *fakeStore, *fakePoster, *fakeSink) {
	t.Helper()
	fs := newFakeStore()
	fp := &fakePoster{}
	sink := &fakeSink{}
	m := New(Options{
		Store:          fs,
		Poster:         fp,
		Sink:           sink.Notify,
		Config:         group.Config{GroupWait: 0, GroupInterval: 5 * time.Minute, RepeatInterval: 30 * time.Minute},
		PendingWait:    30 * time.Second,
		BurstThreshold: burstThreshold,
		BurstWindow:    burstWindow,
		Now:            func() time.Time { return *clock },
	})
	return m, fs, fp, sink
}

// downEntry builds the dispatcher Entry for a monitor failing now.
func downEntry(slug string, at time.Time) Entry {
	return Entry{
		Member: MemberInfo{Slug: slug, FriendlyName: slug},
		Row:    store.MonitorRow{MonitorSpec: store.MonitorSpec{Slug: slug}},
		Event:  &alert.Event{Type: alert.EventOpen, At: at},
	}
}

// TestDispatch_individualMode_singleFailure_flushesAtExpiry exercises
// the 90% case: one monitor fails, pendingWait elapses, the dispatcher
// emits ONE per-monitor message via the sink. No group is created.
func TestDispatch_individualMode_singleFailure_flushesAtExpiry(t *testing.T) {
	clock := base
	m, fs, fp, sink := newDispatchManager(t, &clock, 5, 0)
	ctx := context.Background()

	m.Route(ctx, "ops", downEntry("a", clock))

	// Inside pendingWait: nothing should fire.
	clock = base.Add(20 * time.Second)
	m.evaluateAll(ctx)
	if got := len(sink.calls); got != 0 {
		t.Fatalf("sink fired inside pendingWait: %d calls", got)
	}
	if fp.posts != 0 {
		t.Fatalf("digest posted inside pendingWait: %d posts", fp.posts)
	}

	// After pendingWait: one individual flush, no group.
	clock = base.Add(31 * time.Second)
	m.evaluateAll(ctx)
	if got := sink.countByType(alert.EventOpen); got != 1 {
		t.Fatalf("want 1 EventOpen flush, got %d", got)
	}
	if fp.posts != 0 {
		t.Fatalf("sub-threshold burst posted a digest: %d", fp.posts)
	}
	if open, _ := fs.ListOpenIncidentGroups(ctx); len(open) != 0 {
		t.Fatalf("sub-threshold burst created a group: %d", len(open))
	}
}

// TestDispatch_subThresholdBurst_flushesAllAsIndividuals exercises the
// in-window cluster case from Scenario 5 of the design: 3 failures in
// 25s, threshold=5 → all 3 flush as separate per-monitor messages.
func TestDispatch_subThresholdBurst_flushesAllAsIndividuals(t *testing.T) {
	clock := base
	m, _, fp, sink := newDispatchManager(t, &clock, 5, 0)
	ctx := context.Background()

	m.Route(ctx, "ops", downEntry("a", clock))
	m.Route(ctx, "ops", downEntry("b", clock.Add(5*time.Second)))
	m.Route(ctx, "ops", downEntry("c", clock.Add(15*time.Second)))

	clock = base.Add(31 * time.Second)
	m.evaluateAll(ctx)

	if got := sink.countByType(alert.EventOpen); got != 3 {
		t.Fatalf("want 3 EventOpen flushes, got %d", got)
	}
	if fp.posts != 0 {
		t.Fatalf("sub-threshold burst posted a digest: %d", fp.posts)
	}
}

// TestDispatch_aboveThreshold_promotesToGroup exercises Scenario 4:
// 5 failures in 25s, threshold=5 → ONE digest, ZERO individuals.
func TestDispatch_aboveThreshold_promotesToGroup(t *testing.T) {
	clock := base
	m, fs, fp, sink := newDispatchManager(t, &clock, 5, 0)
	ctx := context.Background()

	for _, slug := range []string{"a", "b", "c", "d", "e"} {
		m.Route(ctx, "ops", downEntry(slug, clock))
		clock = clock.Add(2 * time.Second)
	}

	clock = base.Add(31 * time.Second)
	m.evaluateAll(ctx)

	if got := len(sink.calls); got != 0 {
		t.Fatalf("above-threshold should not fire individual sink, got %d", got)
	}
	if fp.posts != 1 {
		t.Fatalf("want exactly 1 digest post, got %d", fp.posts)
	}
	if open, _ := fs.ListOpenIncidentGroups(ctx); len(open) != 1 {
		t.Fatalf("want 1 open group, got %d", len(open))
	}
}

// TestDispatch_pendingRecoveryRemovesFromPool: a member that fails
// then recovers inside pendingWait is removed from the pool. The
// remaining count decides flush vs promote. The recovered member must
// NOT generate an individual flush — it was never notified.
func TestDispatch_pendingRecoveryRemovesFromPool(t *testing.T) {
	clock := base
	m, _, fp, sink := newDispatchManager(t, &clock, 5, 0)
	ctx := context.Background()

	for _, slug := range []string{"a", "b", "c", "d", "e"} {
		m.Route(ctx, "ops", downEntry(slug, clock))
	}
	// "a" recovers inside pendingWait.
	m.Up(ctx, "ops", "a", clock.Add(5*time.Second))

	clock = base.Add(31 * time.Second)
	m.evaluateAll(ctx)

	// 5 → 4 after recovery → below threshold → 4 individuals.
	if got := sink.countByType(alert.EventOpen); got != 4 {
		t.Fatalf("want 4 EventOpen flushes (5 down, 1 recovered), got %d", got)
	}
	if got := sink.countByType(alert.EventResolve); got != 0 {
		t.Fatalf("in-pending recovery should not emit a resolve, got %d", got)
	}
	if fp.posts != 0 {
		t.Fatalf("redacted pool should not post a digest, got %d", fp.posts)
	}
}

// TestDispatch_pendingPauseRemovesFromPool: identical to the recovery
// case but for push-propagation Pause. Same invariant — paused entries
// don't count and don't notify.
func TestDispatch_pendingPauseRemovesFromPool(t *testing.T) {
	clock := base
	m, _, fp, sink := newDispatchManager(t, &clock, 5, 0)
	ctx := context.Background()

	for _, slug := range []string{"a", "b", "c", "d", "e"} {
		m.Route(ctx, "ops", downEntry(slug, clock))
	}
	m.Pause(ctx, "ops", "a", clock.Add(5*time.Second))

	clock = base.Add(31 * time.Second)
	m.evaluateAll(ctx)

	if got := sink.countByType(alert.EventOpen); got != 4 {
		t.Fatalf("want 4 EventOpen flushes, got %d", got)
	}
	if fp.posts != 0 {
		t.Fatalf("redacted pool should not post a digest, got %d", fp.posts)
	}
}

// TestDispatch_groupMode_newFailureJoinsGroup: once a group is open,
// subsequent failures on the same channel route DIRECTLY into the
// group (no second pendingWait). Mirrors Scenario 4's late-joiner.
func TestDispatch_groupMode_newFailureJoinsGroup(t *testing.T) {
	clock := base
	m, _, fp, sink := newDispatchManager(t, &clock, 2, 0)
	ctx := context.Background()

	m.Route(ctx, "ops", downEntry("a", clock))
	m.Route(ctx, "ops", downEntry("b", clock))
	clock = base.Add(31 * time.Second)
	m.evaluateAll(ctx)
	if fp.posts != 1 {
		t.Fatalf("setup: expected group post, got %d", fp.posts)
	}

	// New failure post-promotion — goes straight into the open group.
	priorPosts := fp.posts
	m.Route(ctx, "ops", downEntry("c", clock))
	if fp.posts != priorPosts {
		t.Fatalf("late joiner triggered an extra PostDigest: %d → %d", priorPosts, fp.posts)
	}

	// Heartbeat tick — the join shows up as a group update, not a
	// separate pending pool.
	clock = base.Add(6 * time.Minute)
	m.evaluateAll(ctx)
	if got := len(sink.calls); got != 0 {
		t.Fatalf("late joiner must not fire the individual sink, got %d", got)
	}
	// Verify "c" is a group member.
	m.mu.Lock()
	lg := m.groups["ops"]
	m.mu.Unlock()
	if _, ok := lg.g.Members["c"]; !ok {
		t.Fatalf("late joiner not present in group members")
	}
}

// TestDispatch_groupClose_revertsToIndividualMode: after the open group
// retires (everyone recovered), the channel goes back to individual
// mode — the next failure starts a fresh pending window.
func TestDispatch_groupClose_revertsToIndividualMode(t *testing.T) {
	clock := base
	m, fs, fp, sink := newDispatchManager(t, &clock, 2, 0)
	ctx := context.Background()

	// Promote to group.
	m.Route(ctx, "ops", downEntry("a", clock))
	m.Route(ctx, "ops", downEntry("b", clock))
	clock = base.Add(31 * time.Second)
	m.evaluateAll(ctx)

	// Both recover; resolve-debounce elapses → group closes.
	m.Up(ctx, "ops", "a", clock)
	m.Up(ctx, "ops", "b", clock)
	clock = base.Add(15 * time.Minute) // well past debounce + heartbeat
	m.evaluateAll(ctx)
	if open, _ := fs.ListOpenIncidentGroups(ctx); len(open) != 0 {
		t.Fatalf("group did not close")
	}

	// Next failure on this channel — must start fresh pending,
	// not auto-attach to the (now-closed) group.
	priorPosts := fp.posts
	m.Route(ctx, "ops", downEntry("z", clock))
	// Inside the new pendingWait: nothing should fire yet.
	clock = clock.Add(15 * time.Second)
	m.evaluateAll(ctx)
	if got := len(sink.calls); got != 0 {
		t.Fatalf("fresh pending fired sink prematurely: %d", got)
	}
	// Past pendingWait → single individual, no new digest.
	clock = clock.Add(20 * time.Second)
	m.evaluateAll(ctx)
	if got := sink.countByType(alert.EventOpen); got != 1 {
		t.Fatalf("post-close failure want 1 individual, got %d", got)
	}
	if fp.posts != priorPosts {
		t.Fatalf("post-close failure should not open a new group: %d → %d", priorPosts, fp.posts)
	}
}

// TestDispatch_allRecoverBeforePendingExpiry_silent: a whole burst
// recovers inside pendingWait → zero Slack output (no individuals, no
// digest). The pool just discards.
func TestDispatch_allRecoverBeforePendingExpiry_silent(t *testing.T) {
	clock := base
	m, _, fp, sink := newDispatchManager(t, &clock, 5, 0)
	ctx := context.Background()

	for _, slug := range []string{"a", "b", "c"} {
		m.Route(ctx, "ops", downEntry(slug, clock))
	}
	m.Up(ctx, "ops", "a", clock.Add(5*time.Second))
	m.Up(ctx, "ops", "b", clock.Add(5*time.Second))
	m.Up(ctx, "ops", "c", clock.Add(5*time.Second))

	clock = base.Add(31 * time.Second)
	m.evaluateAll(ctx)

	if got := len(sink.calls); got != 0 {
		t.Fatalf("blip-recovery should not fire sink, got %d", got)
	}
	if fp.posts != 0 {
		t.Fatalf("blip-recovery should not post, got %d", fp.posts)
	}
}

// downEntryWithDeps is like downEntry but also stamps the row's
// DependsOn list — required for the on-demand probe pass to identify
// shared parents in the pool.
func downEntryWithDeps(slug string, at time.Time, deps ...string) Entry {
	return Entry{
		Member: MemberInfo{Slug: slug, FriendlyName: slug},
		Row: store.MonitorRow{
			MonitorSpec: store.MonitorSpec{Slug: slug, DependsOn: deps},
		},
		Event: &alert.Event{Type: alert.EventOpen, At: at},
	}
}

// newProbeManager wires a dispatcher with a fake parent-probe
// callback. The probe callback receives the parent slug and is
// expected (in production) to drive alert.Apply + push-propagation;
// here we just let the test set up the side effects it expects.
func newProbeManager(t *testing.T, clock *time.Time, threshold int, probe func(ctx context.Context, parentSlug string)) (*Manager, *fakeStore, *fakePoster, *fakeSink) {
	t.Helper()
	fs := newFakeStore()
	fp := &fakePoster{}
	sink := &fakeSink{}
	m := New(Options{
		Store:               fs,
		Poster:              fp,
		Sink:                sink.Notify,
		Config:              group.Config{GroupInterval: 5 * time.Minute, RepeatInterval: 30 * time.Minute},
		PendingWait:         30 * time.Second,
		BurstThreshold:      threshold,
		OnDemandParentProbe: probe,
		Now:                 func() time.Time { return *clock },
	})
	return m, fs, fp, sink
}

// TestDispatch_onDemandProbe_parentDown_drainsPool exercises the
// canonical bastion case: 5 children whose dependsOn list shares one
// parent. At pendingWait expiry, the dispatcher fires one on-demand
// probe of the shared parent. The fake probe simulates "parent down"
// by calling m.Pause for each child (the production hook does this
// via the scheduler push-propagation closure). The redacted pool is
// empty → no group, no individual flushes for children.
func TestDispatch_onDemandProbe_parentDown_drainsPool(t *testing.T) {
	clock := base
	var m *Manager
	probe := func(ctx context.Context, parent string) {
		// Simulate push-propagation: pause every child that depends
		// on this parent. In production the scheduler's push hook
		// does this; here we just touch the channel by name.
		for _, child := range []string{"a", "b", "c", "d", "e"} {
			m.Pause(ctx, "ops", child, *(new(time.Time)))
		}
	}
	var fs *fakeStore
	var fp *fakePoster
	var sink *fakeSink
	m, fs, fp, sink = newProbeManager(t, &clock, 5, probe)
	ctx := context.Background()

	for _, slug := range []string{"a", "b", "c", "d", "e"} {
		m.Route(ctx, "ops", downEntryWithDeps(slug, clock, "bastion"))
	}

	clock = base.Add(31 * time.Second)
	m.evaluateAll(ctx)

	if got := len(sink.calls); got != 0 {
		t.Fatalf("on-demand drain should suppress child individual flushes, got %d", got)
	}
	if fp.posts != 0 {
		t.Fatalf("on-demand drain should suppress group promotion, got %d posts", fp.posts)
	}
	if open, _ := fs.ListOpenIncidentGroups(ctx); len(open) != 0 {
		t.Fatalf("on-demand drain should suppress group creation, got %d", len(open))
	}
}

// TestDispatch_onDemandProbe_parentUp_poolUnaffected: 5 children, the
// hot parent's probe returns "up" (fake does nothing). The pool stays
// at 5 → promotes to group as normal. Verifies the probe path doesn't
// drain the pool unconditionally.
func TestDispatch_onDemandProbe_parentUp_poolUnaffected(t *testing.T) {
	clock := base
	probe := func(_ context.Context, _ string) { /* parent up: no-op */ }
	m, _, fp, _ := newProbeManager(t, &clock, 5, probe)
	ctx := context.Background()

	for _, slug := range []string{"a", "b", "c", "d", "e"} {
		m.Route(ctx, "ops", downEntryWithDeps(slug, clock, "bastion"))
	}

	clock = base.Add(31 * time.Second)
	m.evaluateAll(ctx)
	if fp.posts != 1 {
		t.Fatalf("parent-up case should still promote to group, got %d posts", fp.posts)
	}
}

// TestDispatch_onDemandProbe_noSharedParents_skipped: 5 children with
// distinct dependsOn parents → no hot parent → probe should NOT fire.
func TestDispatch_onDemandProbe_noSharedParents_skipped(t *testing.T) {
	clock := base
	var probeCalls int
	probe := func(_ context.Context, _ string) { probeCalls++ }
	m, _, fp, _ := newProbeManager(t, &clock, 5, probe)
	ctx := context.Background()

	// Each child depends on its own distinct parent.
	for i, slug := range []string{"a", "b", "c", "d", "e"} {
		m.Route(ctx, "ops", downEntryWithDeps(slug, clock, "parent-"+string(rune('a'+i))))
	}

	clock = base.Add(31 * time.Second)
	m.evaluateAll(ctx)
	if probeCalls != 0 {
		t.Fatalf("no shared parents → probe should not fire, got %d calls", probeCalls)
	}
	if fp.posts != 1 {
		t.Fatalf("pool should promote normally, got %d posts", fp.posts)
	}
}

// TestDispatch_onDemandProbe_parentInPool_skipped: the shared parent
// is itself in the pool (its tick fired during the burst window).
// Push-propagation already ran from the scheduler when the parent's
// EventOpen landed, so we don't re-probe.
func TestDispatch_onDemandProbe_parentInPool_skipped(t *testing.T) {
	clock := base
	var probeCalls int
	probe := func(_ context.Context, _ string) { probeCalls++ }
	m, _, _, _ := newProbeManager(t, &clock, 5, probe)
	ctx := context.Background()

	// Bastion + 4 children share "bastion" as a dependsOn target.
	// Bastion is itself in the pool (its tick fired during the burst).
	m.Route(ctx, "ops", downEntryWithDeps("bastion", clock))
	for _, slug := range []string{"a", "b", "c", "d"} {
		m.Route(ctx, "ops", downEntryWithDeps(slug, clock, "bastion"))
	}

	clock = base.Add(31 * time.Second)
	m.evaluateAll(ctx)
	if probeCalls != 0 {
		t.Fatalf("parent already in pool → no on-demand probe; got %d calls", probeCalls)
	}
}

// fakePosterCapturing extends fakePoster with structured capture of
// every payload so mention-injection tests can assert the broadcast
// marker (<!channel> / <!here>) shows up on open + reminder.
type fakePosterCapturing struct {
	postParents     []slack.ParentMessage
	updateParents   []slack.ParentMessage
	replyBlockLists [][]slack.Block
}

func (p *fakePosterCapturing) PostDigest(_ context.Context, _ string, msg slack.ParentMessage) (string, string, error) {
	p.postParents = append(p.postParents, msg)
	return "C1", "ts1", nil
}

func (p *fakePosterCapturing) UpdateDigest(_ context.Context, _, _ string, msg slack.ParentMessage) error {
	p.updateParents = append(p.updateParents, msg)
	return nil
}

func (p *fakePosterCapturing) Reply(_ context.Context, _, _ string, blocks []slack.Block) error {
	p.replyBlockLists = append(p.replyBlockLists, blocks)
	return nil
}

func parentContainsText(msg slack.ParentMessage, needle string) bool {
	for _, att := range msg.Attachments {
		for _, b := range att.Blocks {
			if marshalContains(b, needle) {
				return true
			}
		}
	}
	for _, b := range msg.Blocks {
		if marshalContains(b, needle) {
			return true
		}
	}
	return false
}

// marshalContains is a brittle-but-adequate way to check that a block
// renders a particular substring without depending on the exact block
// shape. The blocks package's struct fields are private to the slack
// package, so we use fmt.Sprintf on the value.
func marshalContains(v any, needle string) bool {
	return strings.Contains(fmt.Sprintf("%+v", v), needle)
}

// TestDispatch_groupMention_channelInjectedOnOpen verifies that when
// GroupMention="channel" is set, the initial digest's parent message
// carries the <!channel> broadcast marker.
func TestDispatch_groupMention_channelInjectedOnOpen(t *testing.T) {
	clock := base
	fs := newFakeStore()
	fp := &fakePosterCapturing{}
	m := New(Options{
		Store:          fs,
		Poster:         fp,
		Config:         group.Config{GroupInterval: 5 * time.Minute, RepeatInterval: 30 * time.Minute},
		PendingWait:    30 * time.Second,
		BurstThreshold: 2,
		GroupMention:   "channel",
		Now:            func() time.Time { return clock },
	})
	ctx := context.Background()

	m.Route(ctx, "ops", downEntry("a", clock))
	m.Route(ctx, "ops", downEntry("b", clock))
	clock = base.Add(31 * time.Second)
	m.evaluateAll(ctx)

	if len(fp.postParents) != 1 {
		t.Fatalf("want 1 PostDigest call, got %d", len(fp.postParents))
	}
	if !parentContainsText(fp.postParents[0], "<!channel>") {
		t.Fatalf("expected <!channel> in digest open; payload did not contain it")
	}
}

// TestDispatch_groupMention_noneSkipsBroadcast: GroupMention="none"
// suppresses the broadcast marker entirely (operator opt-out for
// personal / dev setups).
func TestDispatch_groupMention_noneSkipsBroadcast(t *testing.T) {
	clock := base
	fs := newFakeStore()
	fp := &fakePosterCapturing{}
	m := New(Options{
		Store:          fs,
		Poster:         fp,
		Config:         group.Config{GroupInterval: 5 * time.Minute, RepeatInterval: 30 * time.Minute},
		PendingWait:    30 * time.Second,
		BurstThreshold: 2,
		GroupMention:   "none",
		Now:            func() time.Time { return clock },
	})
	ctx := context.Background()

	m.Route(ctx, "ops", downEntry("a", clock))
	m.Route(ctx, "ops", downEntry("b", clock))
	clock = base.Add(31 * time.Second)
	m.evaluateAll(ctx)

	if parentContainsText(fp.postParents[0], "<!channel>") || parentContainsText(fp.postParents[0], "<!here>") {
		t.Fatalf("GroupMention=none must suppress broadcast markers")
	}
}

// TestDispatch_individualMode_resolveAfterFlush: a monitor that
// fired an individual EventOpen and later recovers must fire an
// individual EventResolve through the same sink.
func TestDispatch_individualMode_resolveAfterFlush(t *testing.T) {
	clock := base
	m, _, _, sink := newDispatchManager(t, &clock, 5, 0)
	ctx := context.Background()

	m.Route(ctx, "ops", downEntry("a", clock))
	clock = base.Add(31 * time.Second)
	m.evaluateAll(ctx)
	if got := sink.countByType(alert.EventOpen); got != 1 {
		t.Fatalf("setup: want 1 open, got %d", got)
	}

	// Recovery — should fire EventResolve through the sink.
	clock = base.Add(5 * time.Minute)
	m.Route(ctx, "ops", resolveEntry("a", clock))
	if got := sink.countByType(alert.EventResolve); got != 1 {
		t.Fatalf("want 1 EventResolve fired, got %d", got)
	}
}

// resolveEntry builds the dispatcher Entry for a monitor recovering now
// whose Open reached Slack: the row carries the parent's thread ts.
func resolveEntry(slug string, at time.Time) Entry {
	e := orphanResolveEntry(slug, at.Add(-time.Minute), at)
	ch, ts := "C1", "ts-"+slug
	e.Row.UptimeThreadChannel, e.Row.UptimeThreadTS = &ch, &ts
	return e
}

// orphanResolveEntry builds the Entry for a monitor recovering at `at`
// whose Open, at `opened`, failed to reach Slack: no thread ts on file,
// and the delivery failure is marked on the row.
func orphanResolveEntry(slug string, opened, at time.Time) Entry {
	e := unannouncedResolveEntry(slug, opened, at)
	failedAt := opened
	e.Row.UptimeOpenFailedAt = &failedAt
	return e
}

// unannouncedResolveEntry builds the Entry for a monitor recovering at
// `at` whose Open, at `opened`, was never sent to Slack: no thread ts
// and no delivery-failure mark.
func unannouncedResolveEntry(slug string, opened, at time.Time) Entry {
	return Entry{
		Member: MemberInfo{Slug: slug, FriendlyName: slug, DetailURL: "https://monitor.example.test/monitor/" + slug},
		Row:    store.MonitorRow{MonitorSpec: store.MonitorSpec{Slug: slug}, OpenedAt: &opened},
		Event:  &alert.Event{Type: alert.EventResolve, At: at, Downtime: at.Sub(opened)},
	}
}

// TestDispatch_trickleAcrossWindows_promotesOnCumulativeCount is the
// regression guard for the storm: a cluster-wide outage reaches the
// dispatcher one monitor at a time, because the scheduler jitters each
// monitor's first tick across its whole interval. Every pending pool is
// therefore size 1 — far under burstThreshold — and sizing the burst off
// one pool would page all five separately. Counting what the channel has
// down across burstWindow caps it at burstThreshold-1 individual pages,
// after which the channel promotes and the rest lands in one digest.
func TestDispatch_trickleAcrossWindows_promotesOnCumulativeCount(t *testing.T) {
	clock := base
	m, _, fp, sink := newDispatchManager(t, &clock, 3, 5*time.Minute)
	ctx := context.Background()

	// Five monitors fail one per pendingWait window — never two at once.
	for i, slug := range []string{"a", "b", "c", "d", "e"} {
		clock = base.Add(time.Duration(i) * 40 * time.Second)
		m.Route(ctx, "ops", downEntry(slug, clock))
		clock = clock.Add(31 * time.Second)
		m.evaluateAll(ctx)
	}

	// a and b page individually (cumulative count 1, then 2). c crosses
	// the threshold, so it and everything after it land in the digest.
	if got := sink.countByType(alert.EventOpen); got != 2 {
		t.Errorf("want 2 individual pages (burstThreshold-1), got %d", got)
	}
	if fp.posts != 1 {
		t.Errorf("want exactly 1 digest for the outage, got %d", fp.posts)
	}
}

// TestDispatch_individuallyPagedMonitor_resolvesThroughItsOwnMessage
// covers the other half of the trickle: a monitor paged individually
// before the channel promoted still owns a Slack parent message. Its
// recovery must edit that message rather than be swallowed by a group
// it was never a member of, which would leave a red circle standing in
// the channel after the incident closed.
func TestDispatch_individuallyPagedMonitor_resolvesThroughItsOwnMessage(t *testing.T) {
	clock := base
	m, _, fp, sink := newDispatchManager(t, &clock, 3, 5*time.Minute)
	ctx := context.Background()

	for i, slug := range []string{"a", "b", "c"} {
		clock = base.Add(time.Duration(i) * 40 * time.Second)
		m.Route(ctx, "ops", downEntry(slug, clock))
		clock = clock.Add(31 * time.Second)
		m.evaluateAll(ctx)
	}
	if fp.posts != 1 {
		t.Fatalf("setup: want the channel promoted to group-mode, got %d digests", fp.posts)
	}

	clock = clock.Add(time.Minute)
	m.Route(ctx, "ops", resolveEntry("a", clock))

	if got := sink.countByType(alert.EventResolve); got != 1 {
		t.Errorf("individually-paged monitor's recovery was swallowed by the group: %d sink resolves", got)
	}
}

// TestDispatch_burstWindowAgesOut_keepsUnrelatedFailuresIndividual
// guards the other direction: failures spread wider than burstWindow are
// separate incidents, not one burst, and must keep paging individually.
func TestDispatch_burstWindowAgesOut_keepsUnrelatedFailuresIndividual(t *testing.T) {
	clock := base
	m, _, fp, sink := newDispatchManager(t, &clock, 3, 2*time.Minute)
	ctx := context.Background()

	for i, slug := range []string{"a", "b", "c", "d"} {
		clock = base.Add(time.Duration(i) * 10 * time.Minute)
		m.Route(ctx, "ops", downEntry(slug, clock))
		clock = clock.Add(31 * time.Second)
		m.evaluateAll(ctx)
	}

	if got := sink.countByType(alert.EventOpen); got != 4 {
		t.Errorf("want 4 individual pages for 4 unrelated failures, got %d", got)
	}
	if fp.posts != 0 {
		t.Errorf("failures spread beyond burstWindow must not group: %d digests", fp.posts)
	}
}

// TestDispatch_burstWindowEqualToPendingWait_stillPromotes guards the
// boundary the config validator allows: burstWindow may equal
// pendingWait, and at expiry the pool's own entries are exactly
// pendingWait old — right on the ageing edge. The burst count is the
// union of the aged window and the pool, so a simultaneous
// at-threshold burst still promotes instead of paging every monitor.
func TestDispatch_burstWindowEqualToPendingWait_stillPromotes(t *testing.T) {
	clock := base
	m, _, fp, sink := newDispatchManager(t, &clock, 3, 30*time.Second)
	ctx := context.Background()

	for _, slug := range []string{"a", "b", "c"} {
		m.Route(ctx, "ops", downEntry(slug, clock))
	}
	clock = base.Add(31 * time.Second)
	m.evaluateAll(ctx)

	if got := len(sink.calls); got != 0 {
		t.Errorf("at-threshold burst paged individually: %d sink calls", got)
	}
	if fp.posts != 1 {
		t.Errorf("want exactly 1 digest, got %d", fp.posts)
	}
}

// TestDispatch_pausedIndividualMonitor_keepsItsOwnMessage: a monitor
// already paged individually, then paused by push-propagation, still
// owns its Slack parent. The pause drops it from the burst count only —
// its recovery has to edit that message even once the channel promotes.
func TestDispatch_pausedIndividualMonitor_keepsItsOwnMessage(t *testing.T) {
	clock := base
	m, _, fp, sink := newDispatchManager(t, &clock, 3, 5*time.Minute)
	ctx := context.Background()

	// "a" pages individually, then its parent goes down and pauses it.
	m.Route(ctx, "ops", downEntry("a", clock))
	clock = base.Add(31 * time.Second)
	m.evaluateAll(ctx)
	m.Pause(ctx, "ops", "a", clock)

	// A later trickle promotes the channel to group-mode without "a".
	for i, slug := range []string{"b", "c", "d"} {
		clock = base.Add(time.Duration(i+2) * 40 * time.Second)
		m.Route(ctx, "ops", downEntry(slug, clock))
		clock = clock.Add(31 * time.Second)
		m.evaluateAll(ctx)
	}
	if fp.posts != 1 {
		t.Fatalf("setup: want the channel promoted to group-mode, got %d digests", fp.posts)
	}

	clock = clock.Add(time.Minute)
	m.Route(ctx, "ops", resolveEntry("a", clock))

	if got := sink.countByType(alert.EventResolve); got != 1 {
		t.Errorf("paused-then-recovered individual page lost its resolve: %d sink resolves", got)
	}
}

// TestDispatch_reattachedGroup_recoversUnknownMonitorIndividually:
// after a restart the in-memory ownership set is empty while an open
// group reattaches from the store. A monitor the digest has no record
// of must still recover through the per-monitor notifier — MarkUp would
// drop it and leave its DOWN message red forever.
func TestDispatch_reattachedGroup_recoversUnknownMonitorIndividually(t *testing.T) {
	clock := base
	m, fs, _, sink := newDispatchManager(t, &clock, 3, 5*time.Minute)
	ctx := context.Background()

	// A group exists on the channel, covering "b" only.
	id, err := fs.CreateIncidentGroup(ctx, "ops", base)
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := fs.SaveIncidentGroup(ctx, store.IncidentGroupRow{
		ID: id, ChannelSlug: "ops", OpenedAt: base, Posted: true,
		Members: []store.IncidentGroupMemberRow{{
			MonitorSlug: "b", State: "down", JoinedAt: base, ChangedAt: base,
		}},
	}); err != nil {
		t.Fatalf("save group: %v", err)
	}
	if err := m.Reattach(ctx); err != nil {
		t.Fatalf("reattach: %v", err)
	}

	clock = base.Add(time.Minute)
	m.Route(ctx, "ops", resolveEntry("a", clock))

	if got := sink.countByType(alert.EventResolve); got != 1 {
		t.Errorf("recovery of a non-member was swallowed by the digest: %d sink resolves", got)
	}
}

// TestDispatch_groupMode_removedMonitorStopsCountingAsDown is the
// reported bug: a big outage promotes the channel to a digest, then one
// of the down monitors is removed from the estate (Ingress deleted).
// The digest must stop reporting it as down so the incident can close on
// what remains, instead of standing red forever.
func TestDispatch_groupMode_removedMonitorStopsCountingAsDown(t *testing.T) {
	clock := base
	m, fs, fp, sink := newDispatchManager(t, &clock, 2, 5*time.Minute)
	ctx := context.Background()

	m.Route(ctx, "ops", downEntry("gone", clock))
	m.Route(ctx, "ops", downEntry("recovers", clock))
	clock = base.Add(31 * time.Second)
	m.evaluateAll(ctx)
	if fp.posts != 1 {
		t.Fatalf("setup: want 1 digest post, got %d", fp.posts)
	}

	// The Ingress behind "gone" is deleted; "recovers" comes back.
	clock = base.Add(1 * time.Minute)
	m.Remove(ctx, "ops", "gone", clock)
	m.Up(ctx, "ops", "recovers", clock)

	m.mu.Lock()
	sb := m.groups["ops"].g.Scoreboard()
	m.mu.Unlock()
	if sb.Down != 0 || sb.Removed != 1 {
		t.Fatalf("removed monitor still counts as down: %+v", sb)
	}

	// Past the resolve debounce, nothing is down → the group closes.
	clock = base.Add(10 * time.Minute)
	m.evaluateAll(ctx)
	if open, _ := fs.ListOpenIncidentGroups(ctx); len(open) != 0 {
		t.Fatalf("digest with a removed member never closed: %d open", len(open))
	}
	if got := len(sink.calls); got != 0 {
		t.Fatalf("removal must not fire the individual sink, got %d", got)
	}
}

// TestDispatch_groupMode_removalPersistsImmediately: removals arrive off
// the evaluator's goroutine, so the digest row has to be written when the
// removal happens — a restart before the next evaluator pass would
// otherwise reattach a digest that still counts the monitor down.
func TestDispatch_groupMode_removalPersistsImmediately(t *testing.T) {
	clock := base
	m, fs, _, _ := newDispatchManager(t, &clock, 2, 5*time.Minute)
	ctx := context.Background()

	m.Route(ctx, "ops", downEntry("gone", clock))
	m.Route(ctx, "ops", downEntry("stays", clock))
	clock = base.Add(31 * time.Second)
	m.evaluateAll(ctx)

	m.Remove(ctx, "ops", "gone", base.Add(1*time.Minute))

	open, _ := fs.ListOpenIncidentGroups(ctx)
	if len(open) != 1 {
		t.Fatalf("want 1 open group row, got %d", len(open))
	}
	var state string
	for _, mem := range open[0].Members {
		if mem.MonitorSlug == "gone" {
			state = mem.State
		}
	}
	if state != string(group.MemberRemoved) {
		t.Fatalf("persisted member state = %q, want %q", state, group.MemberRemoved)
	}
}

// TestDispatch_pendingRemovalRemovesFromPool closes ADR-0011's residual
// race: a monitor whose failure is still waiting in the pending pool
// when its Ingress disappears is never paged — the removal notice is the
// only message.
func TestDispatch_pendingRemovalRemovesFromPool(t *testing.T) {
	clock := base
	m, _, fp, sink := newDispatchManager(t, &clock, 5, 0)
	ctx := context.Background()

	for _, slug := range []string{"a", "b", "gone"} {
		m.Route(ctx, "ops", downEntry(slug, clock))
	}
	m.Remove(ctx, "ops", "gone", clock.Add(5*time.Second))

	clock = base.Add(31 * time.Second)
	m.evaluateAll(ctx)

	if got := sink.countByType(alert.EventOpen); got != 2 {
		t.Fatalf("want 2 EventOpen flushes, got %d", got)
	}
	for _, c := range sink.calls {
		if c.slug == "gone" {
			t.Fatalf("removed monitor was paged from the pending pool")
		}
	}
	if fp.posts != 0 {
		t.Fatalf("sub-threshold pool posted a digest: %d", fp.posts)
	}
}

// TestDispatch_removalDropsBurstCount: a removed monitor is not part of
// the outage any more, so it must not push the channel over
// burstThreshold on its own.
func TestDispatch_removalDropsBurstCount(t *testing.T) {
	clock := base
	m, _, fp, sink := newDispatchManager(t, &clock, 2, 5*time.Minute)
	ctx := context.Background()

	m.Route(ctx, "ops", downEntry("gone", clock))
	m.Remove(ctx, "ops", "gone", clock.Add(5*time.Second))

	// A fresh, unrelated failure: with the removal forgotten the channel
	// is back to a burst of one, so it flushes individually.
	clock = base.Add(1 * time.Minute)
	m.Route(ctx, "ops", downEntry("z", clock))
	clock = clock.Add(31 * time.Second)
	m.evaluateAll(ctx)

	if got := sink.countByType(alert.EventOpen); got != 1 {
		t.Fatalf("want 1 individual flush, got %d", got)
	}
	if fp.posts != 0 {
		t.Fatalf("removed monitor still counted toward the burst: %d digest posts", fp.posts)
	}
}

// TestSweep_reattachedDigestDropsArchivedMember reproduces the state
// found in production: a monitor was torn down while the daemon was
// stopped (or before the removal hook existed), so the digest reattaches
// still counting it down and no removal event will ever revisit it —
// kubeRemovalSink early-returns on an already-archived row and the kube
// prune routes each vacated slug exactly once. The sweep is what clears
// it.
func TestSweep_reattachedDigestDropsArchivedMember(t *testing.T) {
	clock := base
	m, fs, fp, _ := newDispatchManager(t, &clock, 2, 5*time.Minute)
	ctx := context.Background()

	id, err := fs.CreateIncidentGroup(ctx, "ops", base)
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := fs.SaveIncidentGroup(ctx, store.IncidentGroupRow{
		ID: id, ChannelSlug: "ops", OpenedAt: base, Posted: true,
		DigestChannel: "C1", DigestTS: "ts1",
		Members: []store.IncidentGroupMemberRow{
			{MonitorSlug: "gone", State: "down", JoinedAt: base, ChangedAt: base, Rendered: "active"},
			{MonitorSlug: "alive", State: "down", JoinedAt: base, ChangedAt: base, Rendered: "active"},
		},
	}); err != nil {
		t.Fatalf("save group: %v", err)
	}
	// "gone" was soft-deleted while this process was not running.
	fs.markGone("gone")

	if err := m.Reattach(ctx); err != nil {
		t.Fatalf("reattach: %v", err)
	}

	m.mu.Lock()
	sb := m.groups["ops"].g.Scoreboard()
	m.mu.Unlock()
	if sb.Down != 1 || sb.Removed != 1 {
		t.Fatalf("reattach should drop the archived member from the down count: %+v", sb)
	}

	// The surviving monitor recovers → the incident closes instead of
	// standing red on a monitor nobody probes.
	m.Up(ctx, "ops", "alive", base.Add(1*time.Minute))
	clock = base.Add(10 * time.Minute)
	m.evaluateAll(ctx)
	if open, _ := fs.ListOpenIncidentGroups(ctx); len(open) != 0 {
		t.Fatalf("digest never closed: %d open", len(open))
	}
	if fp.updates == 0 {
		t.Fatalf("close should edit the digest")
	}
}

// TestSweep_evaluatorClearsMemberArchivedWhileRunning: the removal event
// is the fast path, but a missed one must not be permanent. The sweep
// runs on the evaluator, so the digest self-heals within sweepInterval.
func TestSweep_evaluatorClearsMemberArchivedWhileRunning(t *testing.T) {
	clock := base
	m, fs, _, _ := newDispatchManager(t, &clock, 2, 5*time.Minute)
	ctx := context.Background()

	m.Route(ctx, "ops", downEntry("gone", clock))
	m.Route(ctx, "ops", downEntry("stays", clock))
	clock = base.Add(31 * time.Second)
	m.evaluateAll(ctx)

	// Archived without the removal hook firing (Remove is never called).
	fs.markGone("gone")

	// Inside sweepInterval: nothing yet.
	clock = base.Add(50 * time.Second)
	m.evaluateAll(ctx)
	m.mu.Lock()
	sb := m.groups["ops"].g.Scoreboard()
	m.mu.Unlock()
	if sb.Down != 2 {
		t.Fatalf("swept before the interval elapsed: %+v", sb)
	}

	// Past sweepInterval: the archived member is marked removed.
	clock = base.Add(2 * time.Minute)
	m.evaluateAll(ctx)
	m.mu.Lock()
	sb = m.groups["ops"].g.Scoreboard()
	m.mu.Unlock()
	if sb.Down != 1 || sb.Removed != 1 {
		t.Fatalf("sweep did not clear the archived member: %+v", sb)
	}
}

// TestSweep_reattachedDigestRecoversMemberWhoseMonitorIsUp reproduces
// the state found in production after a node reboot: the monitor's
// resolve committed to the monitors table, but the process died before
// the digest persisted the MarkUp. The reattached digest counts it down
// and, with no second resolve ever coming, reminds the channel forever.
func TestSweep_reattachedDigestRecoversMemberWhoseMonitorIsUp(t *testing.T) {
	clock := base
	m, fs, fp, _ := newDispatchManager(t, &clock, 2, 5*time.Minute)
	ctx := context.Background()

	id, err := fs.CreateIncidentGroup(ctx, "ops", base)
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := fs.SaveIncidentGroup(ctx, store.IncidentGroupRow{
		ID: id, ChannelSlug: "ops", OpenedAt: base, Posted: true,
		DigestChannel: "C1", DigestTS: "ts1",
		LastFlushAt: &base, LastReminderAt: &base,
		Members: []store.IncidentGroupMemberRow{
			{MonitorSlug: "stale", State: "down", JoinedAt: base, ChangedAt: base, Rendered: "active"},
			{MonitorSlug: "done", State: "recovered", JoinedAt: base, ChangedAt: base, Rendered: "recovered"},
		},
	}); err != nil {
		t.Fatalf("save group: %v", err)
	}
	fs.markUp("stale", "done")

	clock = base.Add(1 * time.Hour)
	if err := m.Reattach(ctx); err != nil {
		t.Fatalf("reattach: %v", err)
	}

	m.mu.Lock()
	sb := m.groups["ops"].g.Scoreboard()
	m.mu.Unlock()
	if sb.Down != 0 || sb.Recovering != 1 {
		t.Fatalf("reattach should recover the member whose monitor is up: %+v", sb)
	}

	clock = clock.Add(6 * time.Minute)
	m.evaluateAll(ctx)
	if open, _ := fs.ListOpenIncidentGroups(ctx); len(open) != 0 {
		t.Fatalf("digest never closed: %d open", len(open))
	}
	if fp.updates == 0 {
		t.Fatalf("close should edit the digest")
	}
}

// TestSweep_evaluatorRecoversMemberWhoseResolveWasLost: a resolve that
// committed to the monitors table without reaching the digest must not
// leave the member down for the life of the process.
func TestSweep_evaluatorRecoversMemberWhoseResolveWasLost(t *testing.T) {
	clock := base
	m, fs, _, _ := newDispatchManager(t, &clock, 2, 5*time.Minute)
	ctx := context.Background()

	m.Route(ctx, "ops", downEntry("lost", clock))
	m.Route(ctx, "ops", downEntry("stays", clock))
	clock = base.Add(31 * time.Second)
	m.evaluateAll(ctx)

	// Resolved in the monitors table; the Route never arrived.
	fs.markUp("lost")

	clock = base.Add(2 * time.Minute)
	m.evaluateAll(ctx)
	m.mu.Lock()
	sb := m.groups["ops"].g.Scoreboard()
	m.mu.Unlock()
	if sb.Down != 1 || sb.Recovering != 1 {
		t.Fatalf("sweep did not recover the member whose monitor is up: %+v", sb)
	}
}

// TestSweep_leavesLiveMembersAlone guards the obvious way to break this:
// a sweep that marks anything it cannot confirm would silence real
// outages.
func TestSweep_leavesLiveMembersAlone(t *testing.T) {
	clock := base
	m, fs, _, _ := newDispatchManager(t, &clock, 2, 5*time.Minute)
	ctx := context.Background()

	m.Route(ctx, "ops", downEntry("a", clock))
	m.Route(ctx, "ops", downEntry("b", clock))
	clock = base.Add(31 * time.Second)
	m.evaluateAll(ctx)

	clock = base.Add(10 * time.Minute)
	m.evaluateAll(ctx)

	m.mu.Lock()
	sb := m.groups["ops"].g.Scoreboard()
	m.mu.Unlock()
	if sb.Down != 2 || sb.Removed != 0 {
		t.Fatalf("sweep touched live members: %+v", sb)
	}
	if open, _ := fs.ListOpenIncidentGroups(ctx); len(open) != 1 {
		t.Fatalf("live incident should stay open: %d", len(open))
	}
}

// pageIndividually flushes one failure per slug through the per-monitor
// sink, leaving the channel in individual-mode with each monitor owned
// by the notifier. Returns the clock after the flush.
func pageIndividually(t *testing.T, m *Manager, clock *time.Time, channel string, slugs ...string) {
	t.Helper()
	ctx := context.Background()
	for _, slug := range slugs {
		m.Route(ctx, channel, downEntry(slug, *clock))
	}
	*clock = clock.Add(31 * time.Second)
	m.evaluateAll(ctx)
}

// TestDispatch_orphanResolves_groupIntoOneMessage is the post-outage
// flood: Slack was down when the Opens flushed, so no monitor has a
// parent to edit, and each recovery would post its own standalone "is
// UP". Recoveries arriving inside one window must land as ONE grouped
// message instead.
func TestDispatch_orphanResolves_groupIntoOneMessage(t *testing.T) {
	clock := base
	m, _, fp, sink := newDispatchManager(t, &clock, 5, 5*time.Minute)
	ctx := context.Background()
	opened := clock
	pageIndividually(t, m, &clock, "ops", "a", "b", "c")
	sink.calls = nil

	resolvedAt := clock.Add(10 * time.Minute)
	for i, slug := range []string{"a", "b", "c"} {
		clock = resolvedAt.Add(time.Duration(i) * 40 * time.Second)
		m.Route(ctx, "ops", orphanResolveEntry(slug, opened, clock))
		m.evaluateAll(ctx)
	}
	if got := len(sink.calls); got != 0 {
		t.Fatalf("orphan resolves reached the sink inside the window: %d calls", got)
	}
	if fp.posts != 0 {
		t.Fatalf("orphan pool flushed inside the window: %d posts", fp.posts)
	}

	clock = resolvedAt.Add(5 * time.Minute)
	m.evaluateAll(ctx)

	if got := len(sink.calls); got != 0 {
		t.Errorf("want zero per-monitor resolves, got %d", got)
	}
	if fp.posts != 1 {
		t.Fatalf("want exactly 1 grouped recovery post, got %d", fp.posts)
	}
	msg := fp.lastPost
	for _, needle := range []string{
		"Initial notification delivery failed",
		"3 recovered",
		"https://monitor.example.test/monitor/a|a",
		"https://monitor.example.test/monitor/c|c",
	} {
		if !parentContainsText(msg, needle) {
			t.Errorf("grouped recovery missing %q", needle)
		}
	}
	// Earliest Open to latest recovery: 31s + 10m + 80s.
	if !parentContainsText(msg, "was down for 11m") {
		t.Errorf("grouped recovery downtime should span earliest Open to latest recovery")
	}

	// The pool is spent: a later tick posts nothing more.
	clock = clock.Add(10 * time.Minute)
	m.evaluateAll(ctx)
	if fp.posts != 1 || len(sink.calls) != 0 {
		t.Errorf("spent pool re-flushed: %d posts, %d sink calls", fp.posts, len(sink.calls))
	}
}

// TestDispatch_singleOrphanResolve_keepsStandaloneResolve: a lone
// orphan has nothing to group with, so at expiry it goes through the
// per-monitor sink, which posts the standalone resolve with its banner.
func TestDispatch_singleOrphanResolve_keepsStandaloneResolve(t *testing.T) {
	clock := base
	m, _, fp, sink := newDispatchManager(t, &clock, 5, 5*time.Minute)
	ctx := context.Background()
	opened := clock
	pageIndividually(t, m, &clock, "ops", "a")
	sink.calls = nil

	resolvedAt := clock.Add(10 * time.Minute)
	m.Route(ctx, "ops", orphanResolveEntry("a", opened, resolvedAt))
	clock = resolvedAt.Add(5 * time.Minute)
	m.evaluateAll(ctx)

	if got := sink.countByType(alert.EventResolve); got != 1 {
		t.Errorf("want 1 standalone resolve through the sink, got %d", got)
	}
	if fp.posts != 0 {
		t.Errorf("single orphan posted a grouped message: %d posts", fp.posts)
	}
}

// TestDispatch_resolveWithThread_flushesImmediately: a recovery whose
// Open reached Slack edits its own parent at once, even while an
// orphan pool is open on the same channel.
func TestDispatch_resolveWithThread_flushesImmediately(t *testing.T) {
	clock := base
	m, _, fp, sink := newDispatchManager(t, &clock, 5, 5*time.Minute)
	ctx := context.Background()
	opened := clock
	pageIndividually(t, m, &clock, "ops", "a", "b")
	sink.calls = nil

	clock = clock.Add(10 * time.Minute)
	m.Route(ctx, "ops", orphanResolveEntry("a", opened, clock))
	m.Route(ctx, "ops", resolveEntry("b", clock))

	if got := sink.countByType(alert.EventResolve); got != 1 {
		t.Fatalf("want the threaded resolve flushed immediately, got %d sink resolves", got)
	}
	if sink.calls[0].slug != "b" {
		t.Errorf("immediate flush went to %q, want b", sink.calls[0].slug)
	}
	if fp.posts != 0 {
		t.Errorf("threaded resolve posted a grouped message: %d posts", fp.posts)
	}
}

// TestDispatch_orphanDownAgain_leavesPool: a monitor that fails again
// before the pool flushes has a stale recovery — it must not be
// reported as back up.
func TestDispatch_orphanDownAgain_leavesPool(t *testing.T) {
	clock := base
	m, _, fp, sink := newDispatchManager(t, &clock, 5, 5*time.Minute)
	ctx := context.Background()
	opened := clock
	pageIndividually(t, m, &clock, "ops", "a", "b", "c")
	sink.calls = nil

	clock = clock.Add(10 * time.Minute)
	for _, slug := range []string{"a", "b", "c"} {
		m.Route(ctx, "ops", orphanResolveEntry(slug, opened, clock))
	}
	clock = clock.Add(time.Minute)
	m.Route(ctx, "ops", downEntry("c", clock))

	clock = clock.Add(5 * time.Minute)
	m.evaluateAll(ctx)

	if fp.posts != 1 {
		t.Fatalf("want 1 grouped recovery post, got %d", fp.posts)
	}
	if !parentContainsText(fp.lastPost, "2 recovered") {
		t.Errorf("grouped recovery should cover a and b only")
	}
	if parentContainsText(fp.lastPost, "monitor/c|c") {
		t.Errorf("re-failed monitor reported as recovered")
	}
	if got := sink.countByType(alert.EventResolve); got != 0 {
		t.Errorf("want zero per-monitor resolves, got %d", got)
	}
}

// TestDispatch_orphanPools_areIndependentPerChannel: each channel
// groups only its own recoveries, on its own window.
func TestDispatch_orphanPools_areIndependentPerChannel(t *testing.T) {
	clock := base
	m, _, fp, sink := newDispatchManager(t, &clock, 5, 5*time.Minute)
	ctx := context.Background()
	opened := clock
	pageIndividually(t, m, &clock, "ops", "a", "b")
	pageIndividually(t, m, &clock, "web", "x", "y", "z")
	sink.calls = nil

	clock = clock.Add(10 * time.Minute)
	m.Route(ctx, "ops", orphanResolveEntry("a", opened, clock))
	m.Route(ctx, "ops", orphanResolveEntry("b", opened, clock))
	clock = clock.Add(2 * time.Minute)
	for _, slug := range []string{"x", "y", "z"} {
		m.Route(ctx, "web", orphanResolveEntry(slug, opened, clock))
	}

	// ops is due, web is not.
	clock = clock.Add(3 * time.Minute)
	m.evaluateAll(ctx)
	if fp.posts != 1 || !parentContainsText(fp.lastPost, "2 recovered") {
		t.Fatalf("want ops' grouped post of 2, got %d posts", fp.posts)
	}

	clock = clock.Add(2 * time.Minute)
	m.evaluateAll(ctx)
	if fp.posts != 2 || !parentContainsText(fp.lastPost, "3 recovered") {
		t.Fatalf("want web's grouped post of 3, got %d posts", fp.posts)
	}
	if len(sink.calls) != 0 {
		t.Errorf("want zero per-monitor resolves, got %d", len(sink.calls))
	}
}

// hookPoster is a fakePoster that records the channel of each
// PostDigest and runs onPost before it returns, while the dispatcher
// does not hold its lock.
type hookPoster struct {
	fakePoster
	channels []string
	onPost   func(channel string)
}

func (p *hookPoster) PostDigest(ctx context.Context, channel string, msg slack.ParentMessage) (string, string, error) {
	p.channels = append(p.channels, channel)
	if p.onPost != nil {
		p.onPost(channel)
	}
	return p.fakePoster.PostDigest(ctx, channel, msg)
}

// TestDispatch_orphanPoolsDueTogether_laterChannelStaysCancellable: two
// channels' orphan pools fall due on the same pass. While the first
// channel's grouped post is out, the second channel's monitors fail
// again or are paused. Those recoveries are stale by the time the
// second channel posts, so nothing goes out for them.
func TestDispatch_orphanPoolsDueTogether_laterChannelStaysCancellable(t *testing.T) {
	clock := base
	fp := &hookPoster{}
	sink := &fakeSink{}
	m := New(Options{
		Store:          newFakeStore(),
		Poster:         fp,
		Sink:           sink.Notify,
		Config:         group.Config{GroupWait: 0, GroupInterval: 5 * time.Minute, RepeatInterval: 30 * time.Minute},
		PendingWait:    30 * time.Second,
		BurstThreshold: 5,
		BurstWindow:    5 * time.Minute,
		Now:            func() time.Time { return clock },
	})
	ctx := context.Background()
	opened := clock
	pageIndividually(t, m, &clock, "ops", "a", "b")
	pageIndividually(t, m, &clock, "web", "x", "y")
	sink.calls = nil

	clock = clock.Add(10 * time.Minute)
	for _, slug := range []string{"a", "b"} {
		m.Route(ctx, "ops", orphanResolveEntry(slug, opened, clock))
	}
	for _, slug := range []string{"x", "y"} {
		m.Route(ctx, "web", orphanResolveEntry(slug, opened, clock))
	}
	fp.onPost = func(channel string) {
		if channel != "ops" {
			return
		}
		m.Route(ctx, "web", downEntry("x", clock))
		m.Pause(ctx, "web", "y", clock)
	}

	clock = clock.Add(5 * time.Minute)
	m.evaluateAll(ctx)

	if strings.Join(fp.channels, ",") != "ops" {
		t.Errorf("grouped recovery posts: got %v, want [ops]", fp.channels)
	}
	if got := sink.countByType(alert.EventResolve); got != 0 {
		t.Errorf("want zero per-monitor resolves, got %+v", sink.calls)
	}
}

// TestDispatch_orphanRemovedOrPaused_leavesPool: a removal or a
// dependsOn pause is the last word on a pooled monitor, as it is for a
// pending one.
func TestDispatch_orphanRemovedOrPaused_leavesPool(t *testing.T) {
	clock := base
	m, _, fp, sink := newDispatchManager(t, &clock, 5, 5*time.Minute)
	ctx := context.Background()
	opened := clock
	pageIndividually(t, m, &clock, "ops", "a", "b", "c")
	sink.calls = nil

	clock = clock.Add(10 * time.Minute)
	for _, slug := range []string{"a", "b", "c"} {
		m.Route(ctx, "ops", orphanResolveEntry(slug, opened, clock))
	}
	m.Remove(ctx, "ops", "a", clock)
	m.Pause(ctx, "ops", "b", clock)

	clock = clock.Add(5 * time.Minute)
	m.evaluateAll(ctx)

	if fp.posts != 0 {
		t.Errorf("want no grouped post for one remaining orphan, got %d", fp.posts)
	}
	if got := sink.countByType(alert.EventResolve); got != 1 || sink.calls[0].slug != "c" {
		t.Errorf("want c's standalone resolve only, got %+v", sink.calls)
	}
}

// TestDispatch_resolveNeverAnnounced_postsNothing is the dependsOn
// shape: children fail, sit in the pending pool, and are paused out of
// it when their parent is found down. No message about them was ever
// attempted, so their recoveries must post nothing — not a standalone
// resolve, not a grouped "delivery failed" message.
func TestDispatch_resolveNeverAnnounced_postsNothing(t *testing.T) {
	clock := base
	m, _, fp, sink := newDispatchManager(t, &clock, 5, 5*time.Minute)
	ctx := context.Background()
	opened := clock
	for _, slug := range []string{"a", "b", "c"} {
		m.Route(ctx, "ops", downEntry(slug, clock))
	}
	for _, slug := range []string{"a", "b", "c"} {
		m.Pause(ctx, "ops", slug, clock)
	}
	clock = clock.Add(31 * time.Second)
	m.evaluateAll(ctx)

	clock = clock.Add(10 * time.Minute)
	for i, slug := range []string{"a", "b", "c"} {
		clock = clock.Add(time.Duration(i) * 40 * time.Second)
		m.Route(ctx, "ops", unannouncedResolveEntry(slug, opened, clock))
		m.evaluateAll(ctx)
	}
	clock = clock.Add(10 * time.Minute)
	m.evaluateAll(ctx)

	if got := len(sink.calls); got != 0 {
		t.Errorf("want no per-monitor messages for never-announced monitors, got %+v", sink.calls)
	}
	if fp.posts != 0 {
		t.Errorf("want no grouped recovery for never-announced monitors, got %d posts", fp.posts)
	}
}

// TestDispatch_resolveNeverAnnounced_leavesOrphansAlone: a
// never-announced recovery neither joins nor flushes a pool of real
// orphans on the same channel.
func TestDispatch_resolveNeverAnnounced_leavesOrphansAlone(t *testing.T) {
	clock := base
	m, _, fp, sink := newDispatchManager(t, &clock, 5, 5*time.Minute)
	ctx := context.Background()
	opened := clock
	pageIndividually(t, m, &clock, "ops", "a", "b")
	sink.calls = nil

	clock = clock.Add(10 * time.Minute)
	m.Route(ctx, "ops", orphanResolveEntry("a", opened, clock))
	m.Route(ctx, "ops", orphanResolveEntry("b", opened, clock))
	m.Route(ctx, "ops", unannouncedResolveEntry("x", opened, clock))
	clock = clock.Add(5 * time.Minute)
	m.evaluateAll(ctx)

	if fp.posts != 1 || !parentContainsText(fp.lastPost, "2 recovered") {
		t.Fatalf("want one grouped recovery of a and b, got %d posts", fp.posts)
	}
	if parentContainsText(fp.lastPost, "monitor/x|x") {
		t.Errorf("never-announced monitor listed in the grouped recovery")
	}
	if len(sink.calls) != 0 {
		t.Errorf("want zero per-monitor resolves, got %+v", sink.calls)
	}
}

// reminderEntry builds the Entry for a still-down monitor's reminder.
// No thread ts is on file, so the notifier posts the monitor's parent.
func reminderEntry(slug string, opened, at time.Time) Entry {
	return Entry{
		Member: MemberInfo{Slug: slug, FriendlyName: slug},
		Row:    store.MonitorRow{MonitorSpec: store.MonitorSpec{Slug: slug}, OpenedAt: &opened},
		Event:  &alert.Event{Type: alert.EventReminder, At: at},
	}
}

// TestDispatch_reminderAnnouncedMonitor_resolvesWhileChannelPending: a
// child paused out of the pending pool and resumed still down is first
// announced by its reminder. Its recovery must edit that parent even
// when it lands while the channel holds a pending pool for something
// else.
func TestDispatch_reminderAnnouncedMonitor_resolvesWhileChannelPending(t *testing.T) {
	clock := base
	m, _, fp, sink := newDispatchManager(t, &clock, 5, 5*time.Minute)
	ctx := context.Background()
	opened := clock
	m.Route(ctx, "ops", downEntry("a", clock))
	m.Pause(ctx, "ops", "a", clock)
	clock = clock.Add(31 * time.Second)
	m.evaluateAll(ctx)

	clock = clock.Add(time.Hour)
	m.Route(ctx, "ops", reminderEntry("a", opened, clock))
	if got := sink.countByType(alert.EventReminder); got != 1 {
		t.Fatalf("want the reminder forwarded to the notifier, got %d", got)
	}

	clock = clock.Add(time.Minute)
	m.Route(ctx, "ops", downEntry("b", clock))
	m.Route(ctx, "ops", resolveEntry("a", clock.Add(time.Second)))

	if got := sink.countByType(alert.EventResolve); got != 1 {
		t.Errorf("want a's resolve to edit its parent at once, got %d sink resolves", got)
	}
	if fp.posts != 0 {
		t.Errorf("want no digest, got %d posts", fp.posts)
	}
}
