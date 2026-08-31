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
func newDispatchManager(t *testing.T, clock *time.Time, burstThreshold int) (*Manager, *fakeStore, *fakePoster, *fakeSink) {
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
	m, fs, fp, sink := newDispatchManager(t, &clock, 5)
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
	m, _, fp, sink := newDispatchManager(t, &clock, 5)
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
	m, fs, fp, sink := newDispatchManager(t, &clock, 5)
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
	m, _, fp, sink := newDispatchManager(t, &clock, 5)
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
	m, _, fp, sink := newDispatchManager(t, &clock, 5)
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
	m, _, fp, sink := newDispatchManager(t, &clock, 2)
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
	m, fs, fp, sink := newDispatchManager(t, &clock, 2)
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
	m, _, fp, sink := newDispatchManager(t, &clock, 5)
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
	m, _, _, sink := newDispatchManager(t, &clock, 5)
	ctx := context.Background()

	m.Route(ctx, "ops", downEntry("a", clock))
	clock = base.Add(31 * time.Second)
	m.evaluateAll(ctx)
	if got := sink.countByType(alert.EventOpen); got != 1 {
		t.Fatalf("setup: want 1 open, got %d", got)
	}

	// Recovery — should fire EventResolve through the sink.
	clock = base.Add(5 * time.Minute)
	m.Route(ctx, "ops", Entry{
		Member: MemberInfo{Slug: "a", FriendlyName: "a"},
		Row:    store.MonitorRow{MonitorSpec: store.MonitorSpec{Slug: "a"}},
		Event:  &alert.Event{Type: alert.EventResolve, At: clock},
	})
	if got := sink.countByType(alert.EventResolve); got != 1 {
		t.Fatalf("want 1 EventResolve fired, got %d", got)
	}
}

// resolveEntry builds the dispatcher Entry for a monitor recovering now.
func resolveEntry(slug string, at time.Time) Entry {
	return Entry{
		Member: MemberInfo{Slug: slug, FriendlyName: slug},
		Row:    store.MonitorRow{MonitorSpec: store.MonitorSpec{Slug: slug}},
		Event:  &alert.Event{Type: alert.EventResolve, At: at},
	}
}

// newTrickleManager wires a Manager with an explicit burst window so
// the cumulative-count tests can age entries in and out deliberately.
func newTrickleManager(t *testing.T, clock *time.Time, burstThreshold int, burstWindow time.Duration) (*Manager, *fakePoster, *fakeSink) {
	t.Helper()
	fp := &fakePoster{}
	sink := &fakeSink{}
	m := New(Options{
		Store:          newFakeStore(),
		Poster:         fp,
		Sink:           sink.Notify,
		Config:         group.Config{GroupWait: 0, GroupInterval: 5 * time.Minute, RepeatInterval: 30 * time.Minute},
		PendingWait:    30 * time.Second,
		BurstThreshold: burstThreshold,
		BurstWindow:    burstWindow,
		Now:            func() time.Time { return *clock },
	})
	return m, fp, sink
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
	m, fp, sink := newTrickleManager(t, &clock, 3, 5*time.Minute)
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
	m, fp, sink := newTrickleManager(t, &clock, 3, 5*time.Minute)
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
	m, fp, sink := newTrickleManager(t, &clock, 3, 2*time.Minute)
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
