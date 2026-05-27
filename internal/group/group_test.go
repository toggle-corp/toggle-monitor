package group

import (
	"testing"
	"time"
)

// testCfg uses round numbers so timeline arithmetic in the tests reads
// cleanly: 30s wait, 5m heartbeat, 30m reminder, 5m resolve-debounce.
func testCfg() Config {
	return Config{
		GroupWait:       30 * time.Second,
		GroupInterval:   5 * time.Minute,
		RepeatInterval:  30 * time.Minute,
		ResolveDebounce: 5 * time.Minute,
	}
}

var t0 = time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)

// kinds extracts the ActionKind sequence for terse assertions.
func kinds(as []Action) []ActionKind {
	out := make([]ActionKind, len(as))
	for i, a := range as {
		out[i] = a.Kind
	}
	return out
}

func only(t *testing.T, as []Action, want ActionKind) Action {
	t.Helper()
	if len(as) != 1 || as[0].Kind != want {
		t.Fatalf("want single %q, got %v", want, kinds(as))
	}
	return as[0]
}

func none(t *testing.T, as []Action) {
	t.Helper()
	if len(as) != 0 {
		t.Fatalf("want no actions, got %v", kinds(as))
	}
}

func TestGroupWaitHoldsThenPosts(t *testing.T) {
	g := New("ops", t0, testCfg())
	g.MarkDown("a", t0)

	none(t, g.Evaluate(t0))                            // born, within wait
	none(t, g.Evaluate(t0.Add(29*time.Second)))        // still within wait
	only(t, g.Evaluate(t0.Add(30*time.Second)), ActionPostDigest)

	if !g.Posted {
		t.Fatal("group should be posted")
	}
	if got := g.Scoreboard(); got.Down != 1 || got.Total != 1 {
		t.Fatalf("scoreboard = %+v", got)
	}
}

func TestBlipAbsorbedDuringWait(t *testing.T) {
	g := New("ops", t0, testCfg())
	g.MarkDown("a", t0)
	g.MarkUp("a", t0.Add(10*time.Second)) // recovered before the wait elapses

	as := g.Evaluate(t0.Add(30 * time.Second))
	only(t, as, ActionDiscard)
	if !g.Closed || g.Posted {
		t.Fatalf("blip should discard without posting: closed=%v posted=%v", g.Closed, g.Posted)
	}
}

func TestHeartbeatBatchesJoinsIntoOneDelta(t *testing.T) {
	g := New("ops", t0, testCfg())
	g.MarkDown("a", t0)
	only(t, g.Evaluate(t0.Add(30*time.Second)), ActionPostDigest) // LastFlush = t0+30s

	// Three more fail at staggered times — all before the next heartbeat.
	g.MarkDown("b", t0.Add(1*time.Minute))
	g.MarkDown("c", t0.Add(2*time.Minute))
	g.MarkDown("d", t0.Add(3*time.Minute))

	none(t, g.Evaluate(t0.Add(2*time.Minute))) // heartbeat not due yet

	// Heartbeat due at t0+30s+5m = t0+5m30s.
	a := only(t, g.Evaluate(t0.Add(5*time.Minute+30*time.Second)), ActionUpdate)
	if got := a.Delta.NewlyDown; len(got) != 3 || got[0] != "b" || got[2] != "d" {
		t.Fatalf("want one delta batching [b c d], got %v", got)
	}
	if got := g.Scoreboard(); got.Down != 4 {
		t.Fatalf("scoreboard down = %d, want 4", got.Down)
	}
}

func TestFlapWithinDebounceIsInvisible(t *testing.T) {
	g := New("ops", t0, testCfg())
	g.MarkDown("a", t0)
	only(t, g.Evaluate(t0.Add(30*time.Second)), ActionPostDigest)

	// a recovers then fails again, both inside the 5m debounce window.
	g.MarkUp("a", t0.Add(1*time.Minute))
	g.MarkDown("a", t0.Add(3*time.Minute))

	// At the heartbeat, a was never *rendered* recovered, so there is no
	// recovery and no flap delta — nothing to say, no action.
	none(t, g.Evaluate(t0.Add(5*time.Minute+30*time.Second)))
	if got := g.Scoreboard(); got.Down != 1 || got.Recovered != 0 {
		t.Fatalf("scoreboard = %+v, want still 1 down", got)
	}
}

func TestRecoveryRendersAfterDebounce(t *testing.T) {
	g := New("ops", t0, testCfg())
	g.MarkDown("a", t0)
	g.MarkDown("b", t0)
	only(t, g.Evaluate(t0.Add(30*time.Second)), ActionPostDigest)

	g.MarkUp("a", t0.Add(2*time.Minute)) // a recovers; debounce ends t0+7m

	// Heartbeat at t0+5m30s: a is still Recovering (debounce not done) →
	// no recovered delta yet.
	none(t, g.Evaluate(t0.Add(5*time.Minute+30*time.Second)))

	// Next heartbeat at t0+10m30s: debounce elapsed → a is Recovered.
	a := only(t, g.Evaluate(t0.Add(10*time.Minute+30*time.Second)), ActionUpdate)
	if got := a.Delta.Recovered; len(got) != 1 || got[0] != "a" {
		t.Fatalf("want recovered [a], got %v", got)
	}
	if sb := g.Scoreboard(); sb.Down != 1 || sb.Recovered != 1 {
		t.Fatalf("scoreboard = %+v", sb)
	}
}

func TestFlapAfterRenderedRecoveryShows(t *testing.T) {
	g := New("ops", t0, testCfg())
	g.MarkDown("a", t0)
	g.MarkDown("keeper", t0) // stays down so the group never closes
	only(t, g.Evaluate(t0.Add(30*time.Second)), ActionPostDigest)

	g.MarkUp("a", t0.Add(1*time.Minute))
	// Confirm recovery is rendered (debounce ends t0+6m; heartbeat t0+5m30s
	// too early, so use a later eval).
	a := only(t, g.Evaluate(t0.Add(10*time.Minute+30*time.Second)), ActionUpdate)
	if len(a.Delta.Recovered) != 1 {
		t.Fatalf("expected rendered recovery first, got %v", a.Delta)
	}

	// Now it fails again — a genuine flap after a visible recovery.
	g.MarkDown("a", t0.Add(11*time.Minute))
	b := only(t, g.Evaluate(t0.Add(15*time.Minute+30*time.Second)), ActionUpdate)
	if got := b.Delta.Flapped; len(got) != 1 || got[0] != "a" {
		t.Fatalf("want flapped [a], got %v (delta %+v)", got, b.Delta)
	}
}

func TestReminderAtRepeatInterval(t *testing.T) {
	g := New("ops", t0, testCfg())
	g.MarkDown("a", t0)
	only(t, g.Evaluate(t0.Add(30*time.Second)), ActionPostDigest) // LastReminder = t0+30s

	none(t, g.Evaluate(t0.Add(20*time.Minute))) // reminder not due
	// Reminder due at t0+30s+30m.
	only(t, g.Evaluate(t0.Add(30*time.Minute+30*time.Second)), ActionRemind)
	// Not again until another 30m.
	none(t, g.Evaluate(t0.Add(40 * time.Minute)))
}

func TestCloseWhenAllRecovered(t *testing.T) {
	g := New("ops", t0, testCfg())
	g.MarkDown("a", t0)
	g.MarkDown("b", t0)
	only(t, g.Evaluate(t0.Add(30*time.Second)), ActionPostDigest)

	g.MarkUp("a", t0.Add(1*time.Minute))
	g.MarkUp("b", t0.Add(1*time.Minute))

	// Before debounce elapses, both are Recovering → group stays open.
	none(t, g.Evaluate(t0.Add(3 * time.Minute)))
	if g.Closed {
		t.Fatal("group closed prematurely while members recovering")
	}

	// After debounce (ends t0+6m), both Recovered → close.
	a := only(t, g.Evaluate(t0.Add(7*time.Minute)), ActionClose)
	if !g.Closed {
		t.Fatal("group should be closed")
	}
	if got := a.Delta.Recovered; len(got) != 2 {
		t.Fatalf("close delta should carry final recoveries, got %v", got)
	}
	// Evaluating a closed group is inert.
	none(t, g.Evaluate(t0.Add(20*time.Minute)))
}

func TestPausedPulledFromDigest(t *testing.T) {
	g := New("ops", t0, testCfg())
	g.MarkDown("child-a", t0)
	g.MarkDown("child-b", t0) // stays down so the group stays open
	only(t, g.Evaluate(t0.Add(30*time.Second)), ActionPostDigest)

	// Push-propagation pulls child-a once its parent is detected down.
	g.MarkPaused("child-a", t0.Add(1*time.Minute))

	a := only(t, g.Evaluate(t0.Add(5*time.Minute+30*time.Second)), ActionUpdate)
	if got := a.Delta.Paused; len(got) != 1 || got[0] != "child-a" {
		t.Fatalf("want paused [child-a], got %v", got)
	}
	if sb := g.Scoreboard(); sb.Down != 1 || sb.Paused != 1 || sb.Total != 1 {
		t.Fatalf("paused member must leave the active scoreboard: %+v", sb)
	}
}

func TestConfigDefaultsApplied(t *testing.T) {
	g := New("ops", t0, Config{}) // all zero
	if g.cfg.GroupWait != defaultGroupWait ||
		g.cfg.GroupInterval != defaultGroupInterval ||
		g.cfg.RepeatInterval != defaultRepeatInterval ||
		g.cfg.ResolveDebounce != defaultGroupInterval {
		t.Fatalf("defaults not applied: %+v", g.cfg)
	}
}
