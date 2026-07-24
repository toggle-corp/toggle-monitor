package selfhealth_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/probe"
	"github.com/toggle-corp/toggle-monitor/internal/selfhealth"
)

// cfg is the standard test config: window 90s, trip at 3 distinct
// DNS-failing monitors.
func cfg() selfhealth.Config {
	return selfhealth.Config{Window: 90 * time.Second, MinMonitors: 3}
}

// TestEnter_dnsBurstTripsDegraded is the tracer bullet: once N_min
// distinct monitors report FailKindDNS within the window and nothing
// has succeeded, the detector enters degraded mode.
func TestEnter_dnsBurstTripsDegraded(t *testing.T) {
	d := selfhealth.New(cfg())
	t0 := time.Unix(1000, 0)

	d.Report("a", probe.FailKindDNS, false, t0)
	d.Report("b", probe.FailKindDNS, false, t0)
	if d.Degraded() {
		t.Fatal("must not be degraded below MinMonitors distinct DNS failures")
	}
	d.Report("c", probe.FailKindDNS, false, t0)

	// Decide drives the enter/exit evaluation at the evaluator cadence.
	d.Decide(t0)
	if !d.Degraded() {
		t.Fatal("expected degraded after 3 distinct DNS monitors, zero successes")
	}
}

// TestEnter_successVetoesTrip: if even one monitor can reach a target
// within the window, the monitor is not network-isolated, so a DNS
// burst must NOT trip degraded mode.
func TestEnter_successVetoesTrip(t *testing.T) {
	d := selfhealth.New(cfg())
	t0 := time.Unix(1000, 0)

	d.Report("a", probe.FailKindDNS, false, t0)
	d.Report("b", probe.FailKindDNS, false, t0)
	d.Report("c", probe.FailKindDNS, false, t0)
	d.Report("healthy", probe.FailKindNone, true, t0)

	d.Decide(t0)
	if d.Degraded() {
		t.Fatal("a success in the window must veto the trip")
	}
}

// TestDecide_notTripped_commitsIsolatedDNSFailure: an isolated
// single-target DNS failure (below N_min) is not a blindness signal, so
// Decide returns it in Commit — the scheduler runs alert.Apply and pages
// it as a normal EventOpen ~W late. After committing, the provisional is
// cleared so it isn't committed twice.
func TestDecide_notTripped_commitsIsolatedDNSFailure(t *testing.T) {
	d := selfhealth.New(cfg())
	t0 := time.Unix(1000, 0)

	d.Report("lonely", probe.FailKindDNS, false, t0)

	dec := d.Decide(t0)
	if d.Degraded() {
		t.Fatal("one DNS failure must not trip degraded")
	}
	if got := dec.Commit; len(got) != 1 || got[0] != "lonely" {
		t.Fatalf("Commit: got %v, want [lonely]", got)
	}

	// A second Decide with no new reports must not re-commit.
	dec2 := d.Decide(t0)
	if len(dec2.Commit) != 0 {
		t.Fatalf("committed provisional must not be re-emitted, got %v", dec2.Commit)
	}
}

// TestDecide_tripped_discardsProvisionals: when the burst trips
// degraded mode, provisionals are discarded — never committed, never
// replayed — so there are zero phantom incidents.
func TestDecide_tripped_discardsProvisionals(t *testing.T) {
	d := selfhealth.New(cfg())
	t0 := time.Unix(1000, 0)

	d.Report("a", probe.FailKindDNS, false, t0)
	d.Report("b", probe.FailKindDNS, false, t0)
	d.Report("c", probe.FailKindDNS, false, t0)

	dec := d.Decide(t0)
	if !d.Degraded() {
		t.Fatal("expected degraded")
	}
	if len(dec.Commit) != 0 {
		t.Fatalf("tripped decision must discard provisionals, got Commit=%v", dec.Commit)
	}
	if !dec.Entered {
		t.Fatal("expected Entered=true on the trip pass")
	}
}

// TestExit_successAfterDwellClosesDegraded: once connectivity returns
// (≥1 success, DNS count below N_min) and the minimum dwell of one full
// window has elapsed, Decide exits degraded mode and reports Exited.
func TestExit_successAfterDwellClosesDegraded(t *testing.T) {
	d := selfhealth.New(cfg())
	t0 := time.Unix(1000, 0)

	d.Report("a", probe.FailKindDNS, false, t0)
	d.Report("b", probe.FailKindDNS, false, t0)
	d.Report("c", probe.FailKindDNS, false, t0)
	d.Decide(t0)
	if !d.Degraded() {
		t.Fatal("expected degraded")
	}

	// A success arrives, but before the min dwell elapses — must stay
	// degraded.
	tEarly := t0.Add(30 * time.Second)
	d.Report("a", probe.FailKindNone, true, tEarly)
	if dec := d.Decide(tEarly); dec.Exited {
		t.Fatal("must not exit before the minimum dwell of one window")
	}
	if !d.Degraded() {
		t.Fatal("still within dwell — must stay degraded")
	}

	// After a full window with a recent success and no fresh DNS burst,
	// exit.
	tLate := t0.Add(120 * time.Second)
	d.Report("a", probe.FailKindNone, true, tLate)
	dec := d.Decide(tLate)
	if !dec.Exited {
		t.Fatal("expected Exited after dwell with connectivity restored")
	}
	if d.Degraded() {
		t.Fatal("expected degraded cleared after exit")
	}
}

// TestExit_reportsSuppressedCount: the exit Decision must tally every
// DNS-failure tick discarded across the blind window — the ones dropped
// at trip plus each DNS failure reported while degraded — so the close
// notice's "N checks suppressed" summary is truthful (regression: the
// count was previously always 0).
func TestExit_reportsSuppressedCount(t *testing.T) {
	d := selfhealth.New(cfg())
	t0 := time.Unix(1000, 0)

	// Trip on 3 DNS failures → seeds suppressed with those 3.
	d.Report("a", probe.FailKindDNS, false, t0)
	d.Report("b", probe.FailKindDNS, false, t0)
	d.Report("c", probe.FailKindDNS, false, t0)
	if dec := d.Decide(t0); !dec.Entered {
		t.Fatal("expected Entered")
	}

	// A further DNS failure while degraded is discarded but still counted.
	d.Report("d", probe.FailKindDNS, false, t0.Add(10*time.Second))

	// After the dwell, connectivity returns and the stale DNS records age
	// out → exit.
	tLate := t0.Add(120 * time.Second)
	d.Report("a", probe.FailKindNone, true, tLate)
	dec := d.Decide(tLate)
	if !dec.Exited {
		t.Fatal("expected Exited")
	}
	if dec.Suppressed != 4 {
		t.Fatalf("suppressed count: got %d, want 4 (3 at trip + 1 while degraded)", dec.Suppressed)
	}

	// The tally resets after exit — a subsequent non-exit pass reports 0.
	if dec2 := d.Decide(tLate.Add(time.Second)); dec2.Suppressed != 0 {
		t.Fatalf("suppressed must reset after exit; got %d", dec2.Suppressed)
	}
}

// TestEnter_dialBurstDoesNotTrip: a real total outage yields dial-class
// failures after resolution, not DNS, so it must not trip degraded mode
// (the burst dispatcher handles it as one real grouped incident).
func TestEnter_dialBurstDoesNotTrip(t *testing.T) {
	d := selfhealth.New(cfg())
	t0 := time.Unix(1000, 0)

	d.Report("a", probe.FailKindDial, false, t0)
	d.Report("b", probe.FailKindDial, false, t0)
	d.Report("c", probe.FailKindDial, false, t0)

	d.Decide(t0)
	if d.Degraded() {
		t.Fatal("dial-class failures must not trip degraded mode")
	}
}

// TestConcurrentReports exercises the detector under the real access
// pattern: many scheduler goroutines Report in parallel while the
// evaluator goroutine Decides. Run with -race to catch data races.
func TestConcurrentReports(t *testing.T) {
	d := selfhealth.New(cfg())
	t0 := time.Unix(1000, 0)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d.Report(fmt.Sprintf("mon-%d", i), probe.FailKindDNS, false, t0)
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			d.Decide(t0)
			_ = d.Degraded()
		}
	}()
	wg.Wait()
}
