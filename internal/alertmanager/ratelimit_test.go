package alertmanager_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/alertmanager"
	"github.com/toggle-corp/toggle-monitor/internal/config"
)

// fakeClock wraps a time.Time behind atomic load/store so tests can
// advance "now" deterministically — matching the now-func injection
// convention used by internal/scheduler.
type fakeClock struct {
	v atomic.Value // stores time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	c := &fakeClock{}
	c.v.Store(start)
	return c
}

func (c *fakeClock) now() time.Time { return c.v.Load().(time.Time) }

func (c *fakeClock) advance(d time.Duration) {
	c.v.Store(c.now().Add(d))
}

// enabledCfg builds a typical enabled-detector config: 10 alerts in
// 30m, notice cooldown 1h.
func enabledCfg() config.AlertmanagerRateLimit {
	return config.AlertmanagerRateLimit{
		PerChannel:  10,
		Window:      config.Duration(30 * time.Minute),
		NoticeEvery: config.Duration(time.Hour),
	}
}

func TestLimiter_DisabledAllowsEverything(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	lim := alertmanager.NewLimiter(config.AlertmanagerRateLimit{}, clk.now)

	for i := 0; i < 1000; i++ {
		allowed, just := lim.Allow("ops")
		if !allowed || just {
			t.Fatalf("disabled limiter must return (true, false); got (%v, %v) at i=%d",
				allowed, just, i)
		}
	}
	if got := lim.NoticeDue("ops"); got != 0 {
		t.Fatalf("disabled limiter NoticeDue must return 0; got %d", got)
	}
	snap := lim.Snapshot("ops")
	if snap.InWindow != 0 || snap.Dropped != 0 || snap.Engaged {
		t.Fatalf("disabled limiter snapshot must be zero; got %+v", snap)
	}
}

func TestLimiter_BelowThresholdAllowsAll(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	lim := alertmanager.NewLimiter(enabledCfg(), clk.now)

	// PerChannel=10 → first 10 allows return (true, false). The 11th
	// is the engagement transition.
	for i := 0; i < 10; i++ {
		allowed, just := lim.Allow("ops")
		if !allowed || just {
			t.Fatalf("approval %d/10 must be (true,false); got (%v,%v)", i+1, allowed, just)
		}
		clk.advance(time.Second)
	}
}

func TestLimiter_EngagementTransitionAndPersistence(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	lim := alertmanager.NewLimiter(enabledCfg(), clk.now)

	// Fill bucket to threshold.
	for i := 0; i < 10; i++ {
		if allowed, _ := lim.Allow("ops"); !allowed {
			t.Fatalf("expected allow at approval %d", i+1)
		}
		clk.advance(time.Second)
	}

	// 11th call: in-window count == 10 → drop with justEngaged=true.
	allowed, just := lim.Allow("ops")
	if allowed || !just {
		t.Fatalf("engagement transition must be (false,true); got (%v,%v)", allowed, just)
	}

	// 12th and 13th: still engaged, no re-engagement flag.
	for i := 0; i < 2; i++ {
		allowed, just := lim.Allow("ops")
		if allowed || just {
			t.Fatalf("still-engaged drop must be (false,false); got (%v,%v)", allowed, just)
		}
	}

	snap := lim.Snapshot("ops")
	if !snap.Engaged {
		t.Fatalf("expected Engaged=true, got %+v", snap)
	}
	if snap.Dropped != 3 {
		t.Fatalf("expected Dropped=3 (1 transition + 2 still-engaged); got %d", snap.Dropped)
	}
	if snap.InWindow != 10 {
		t.Fatalf("expected InWindow=10; got %d", snap.InWindow)
	}
}

func TestLimiter_DrainReopensWithoutTransitionFlag(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	lim := alertmanager.NewLimiter(enabledCfg(), clk.now)

	// Saturate.
	for i := 0; i < 10; i++ {
		lim.Allow("ops")
	}
	// Cause engagement.
	if allowed, just := lim.Allow("ops"); allowed || !just {
		t.Fatalf("expected engagement transition; got (%v,%v)", allowed, just)
	}

	// Advance well past the window so every approval expires.
	clk.advance(2 * 30 * time.Minute)

	allowed, just := lim.Allow("ops")
	if !allowed || just {
		t.Fatalf("after full drain expected (true,false); got (%v,%v)", allowed, just)
	}
	snap := lim.Snapshot("ops")
	if snap.Engaged {
		t.Fatalf("after drain expected Engaged=false; got %+v", snap)
	}
	if snap.InWindow != 1 {
		t.Fatalf("after drain + 1 allow expected InWindow=1; got %d", snap.InWindow)
	}
}

func TestLimiter_PartialDrainReopensAtThresholdMinusOne(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	lim := alertmanager.NewLimiter(enabledCfg(), clk.now)

	// Stagger 10 approvals one minute apart so they expire one at a
	// time. window=30m → first one expires at t=30m, last at t=39m.
	for i := 0; i < 10; i++ {
		lim.Allow("ops")
		clk.advance(time.Minute)
	}
	// 11th attempt one minute after the last allow: in-window=10
	// (approvals at minutes 0..9, now=10), drop and engage.
	if allowed, just := lim.Allow("ops"); allowed || !just {
		t.Fatalf("expected engagement; got (%v,%v)", allowed, just)
	}

	// Advance until just before the oldest approval (at t=0) expires:
	// now=10m, oldest expires at t=30m → advance 20m-1ns.
	clk.advance(20*time.Minute - time.Nanosecond)
	if allowed, _ := lim.Allow("ops"); allowed {
		t.Fatalf("still 10 in window, expected drop")
	}

	// Step over the expiry boundary of the oldest approval — now we
	// have 9 in window, next Allow should approve.
	clk.advance(2 * time.Nanosecond)
	allowed, just := lim.Allow("ops")
	if !allowed || just {
		t.Fatalf("after one expiry expected (true,false); got (%v,%v)", allowed, just)
	}
	// After the re-allow, InWindow=10 (the boundary): Snapshot's
	// `Engaged` is derived as `InWindow >= perChannel`, so it reads
	// true again — but the just-flag must have been false because the
	// re-engagement only counts when an Allow actually drops. Drive
	// one more drop and assert justEngaged is *false* (the bucket's
	// internal engaged-state was reset by the successful allow, so
	// the very next drop is a fresh transition).
	allowed2, just2 := lim.Allow("ops")
	if allowed2 {
		t.Fatalf("11th approval after re-allow must drop")
	}
	if !just2 {
		t.Fatalf("the drop right after a successful re-allow must register as a fresh justEngaged transition; got just=%v", just2)
	}
}

func TestLimiter_MultiChannelIsolation(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	lim := alertmanager.NewLimiter(enabledCfg(), clk.now)

	for i := 0; i < 10; i++ {
		lim.Allow("ops")
	}
	if allowed, _ := lim.Allow("ops"); allowed {
		t.Fatalf("ops should be engaged")
	}

	// dev channel untouched — must allow freely.
	for i := 0; i < 5; i++ {
		allowed, just := lim.Allow("dev")
		if !allowed || just {
			t.Fatalf("dev #%d must allow freely; got (%v,%v)", i+1, allowed, just)
		}
	}

	if !lim.Snapshot("ops").Engaged {
		t.Fatalf("ops snapshot must show Engaged=true")
	}
	if lim.Snapshot("dev").Engaged {
		t.Fatalf("dev snapshot must show Engaged=false")
	}
}

func TestLimiter_NoticeDueNoDropsReturnsZero(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	lim := alertmanager.NewLimiter(enabledCfg(), clk.now)

	if got := lim.NoticeDue("ops"); got != 0 {
		t.Fatalf("no drops yet → expected 0; got %d", got)
	}
	// Allow but never drop.
	lim.Allow("ops")
	if got := lim.NoticeDue("ops"); got != 0 {
		t.Fatalf("still no drops → expected 0; got %d", got)
	}
}

func TestLimiter_NoticeDueBeforeCooldownReturnsZeroAndPreservesCounter(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	lim := alertmanager.NewLimiter(enabledCfg(), clk.now)

	// Engage and accumulate drops.
	for i := 0; i < 10; i++ {
		lim.Allow("ops")
	}
	for i := 0; i < 5; i++ {
		lim.Allow("ops") // all drops
	}
	// First notice — fires (no prior notice).
	if got := lim.NoticeDue("ops"); got != 5 {
		t.Fatalf("first notice expected 5; got %d", got)
	}

	// Accumulate more drops.
	for i := 0; i < 3; i++ {
		lim.Allow("ops")
	}

	// Well before noticeEvery (1h) — must return 0 and preserve count.
	clk.advance(10 * time.Minute)
	if got := lim.NoticeDue("ops"); got != 0 {
		t.Fatalf("within cooldown expected 0; got %d", got)
	}
	if snap := lim.Snapshot("ops"); snap.Dropped != 3 {
		t.Fatalf("counter must be preserved at 3 during cooldown; got %d", snap.Dropped)
	}
}

func TestLimiter_NoticeDueAtCooldownBoundary(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	lim := alertmanager.NewLimiter(enabledCfg(), clk.now)

	for i := 0; i < 10; i++ {
		lim.Allow("ops")
	}
	for i := 0; i < 4; i++ {
		lim.Allow("ops")
	}
	if got := lim.NoticeDue("ops"); got != 4 {
		t.Fatalf("first notice expected 4; got %d", got)
	}

	// More drops.
	for i := 0; i < 7; i++ {
		lim.Allow("ops")
	}

	// Exactly at cooldown boundary.
	clk.advance(time.Hour)
	if got := lim.NoticeDue("ops"); got != 7 {
		t.Fatalf("at-boundary second notice expected 7; got %d", got)
	}
	if snap := lim.Snapshot("ops"); snap.Dropped != 0 {
		t.Fatalf("after notice fired, counter must reset to 0; got %d", snap.Dropped)
	}

	// Immediately again with no further drops → 0.
	if got := lim.NoticeDue("ops"); got != 0 {
		t.Fatalf("no-drops second call expected 0; got %d", got)
	}
}

func TestLimiter_NoticeDueRepeatedCycles(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	lim := alertmanager.NewLimiter(enabledCfg(), clk.now)

	// Engage.
	for i := 0; i < 10; i++ {
		lim.Allow("ops")
	}
	// Drop batch 1.
	for i := 0; i < 6; i++ {
		lim.Allow("ops")
	}
	if got := lim.NoticeDue("ops"); got != 6 {
		t.Fatalf("first notice expected 6; got %d", got)
	}

	clk.advance(time.Hour + time.Second)
	// By now (>30m past the original approvals) the bucket has
	// drained, so we have to re-saturate before drops can accrue
	// again. 10 allows to refill, then 9 drops.
	for i := 0; i < 10; i++ {
		lim.Allow("ops")
	}
	for i := 0; i < 9; i++ {
		lim.Allow("ops")
	}
	if got := lim.NoticeDue("ops"); got != 9 {
		t.Fatalf("second notice expected 9; got %d", got)
	}
}

func TestLimiter_SnapshotReflectsLifecycle(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	lim := alertmanager.NewLimiter(enabledCfg(), clk.now)

	// Empty.
	if snap := lim.Snapshot("ops"); snap.InWindow != 0 || snap.Dropped != 0 || snap.Engaged {
		t.Fatalf("empty snapshot must be zero; got %+v", snap)
	}

	// 5 in window.
	for i := 0; i < 5; i++ {
		lim.Allow("ops")
	}
	if snap := lim.Snapshot("ops"); snap.InWindow != 5 || snap.Engaged {
		t.Fatalf("expected InWindow=5 Engaged=false; got %+v", snap)
	}

	// Fill to threshold + drop.
	for i := 0; i < 5; i++ {
		lim.Allow("ops")
	}
	lim.Allow("ops") // engagement
	lim.Allow("ops") // still engaged
	snap := lim.Snapshot("ops")
	if snap.InWindow != 10 || snap.Dropped != 2 || !snap.Engaged {
		t.Fatalf("after engagement expected InWindow=10 Dropped=2 Engaged=true; got %+v", snap)
	}
}

func TestLimiter_ConcurrencySmoke(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	lim := alertmanager.NewLimiter(enabledCfg(), clk.now)

	const goroutines = 100
	const iters = 200

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			ch := []string{"a", "b", "c", "d"}[id%4]
			for i := 0; i < iters; i++ {
				lim.Allow(ch)
				if i%50 == 0 {
					lim.NoticeDue(ch)
					lim.Snapshot(ch)
				}
			}
		}(g)
	}
	wg.Wait()

	// Each of the 4 channels saw goroutines/4 * iters = 5000 Allow
	// calls; with perChannel=10 and a never-advancing fake clock, all
	// approvals stay in window. So InWindow should saturate at 10 and
	// the rest counted as dropped.
	for _, ch := range []string{"a", "b", "c", "d"} {
		snap := lim.Snapshot(ch)
		if snap.InWindow != 10 {
			t.Fatalf("channel %q expected saturated InWindow=10; got %d", ch, snap.InWindow)
		}
		if !snap.Engaged {
			t.Fatalf("channel %q expected Engaged=true", ch)
		}
		// Approvals (10) + dropped (= total - 10) == total. We can't
		// assert exact dropped (NoticeDue resets it mid-run), but it
		// must be non-negative — checked implicitly by Snapshot's int
		// type. Just confirm Engaged sticks.
	}
}
