// Package selfhealth implements the ADR-0008 self-health degraded-mode
// detector: a small, concurrency-safe aggregator that decides whether a
// burst of DNS-resolution failures means the monitor itself has gone
// blind (a cluster-internal DNS/network outage) rather than N genuine
// service outages.
//
// The scheduler reports every tick's outcome here. A FailKindDNS tick
// is held *provisional* — no alert, no DB write, no dispatch — until the
// central evaluator calls Decide once per window W. If the burst tripped
// degraded mode the provisionals are discarded (fully silent, no phantom
// incidents); otherwise the isolated DNS failure is committed and pages
// normally, ~W late. This mirrors the scheduler's existing precedent
// that a SIGTERM mid-probe is "not signal about the monitored service."
package selfhealth

import (
	"sort"
	"sync"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/probe"
)

// Config tunes the detector. A zero Window or MinMonitors < 2 is
// pathological and rejected by config validation before New is called.
type Config struct {
	// Window is W: the rolling detection/decision window.
	Window time.Duration
	// MinMonitors is N_min: the number of distinct monitors that must
	// report a DNS failure within W (with zero successes) to trip.
	MinMonitors int
}

// Detector is the concurrency-safe self-health aggregator. Ticks run in
// parallel scheduler goroutines and all call Report; the single
// evaluator goroutine calls Decide.
type Detector struct {
	cfg Config

	mu       sync.Mutex
	degraded bool
	// enteredAt stamps the moment degraded mode was entered, gating the
	// minimum-dwell exit hysteresis.
	enteredAt time.Time
	// dnsLast maps a monitor slug to the timestamp of its most recent
	// DNS failure (only failures still inside the window count).
	dnsLast map[string]time.Time
	// lastSuccess is the timestamp of the most recent successful probe
	// from any monitor; the zero time means none observed yet.
	lastSuccess time.Time
	// suppressed counts DNS-failure ticks discarded while degraded — the
	// ones dropped at trip plus every DNS failure reported during the
	// blind window. Reported back on the exit Decision for the close
	// summary, then reset.
	suppressed int
}

// New builds a Detector from cfg.
func New(cfg Config) *Detector {
	return &Detector{
		cfg:     cfg,
		dnsLast: map[string]time.Time{},
	}
}

// Report records one tick outcome. success is true iff the probe
// succeeded; kind is the classified failure (probe.FailKindNone on
// success). Safe to call from many goroutines concurrently.
func (d *Detector) Report(slug string, kind probe.FailKind, success bool, at time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if success {
		if at.After(d.lastSuccess) {
			d.lastSuccess = at
		}
		return
	}
	if kind == probe.FailKindDNS {
		d.dnsLast[slug] = at
		if d.degraded {
			// Already blind: this failure will be discarded, not paged.
			// Count it toward the close summary.
			d.suppressed++
		}
	}
}

// Degraded reports whether the detector currently considers the monitor
// blind. Safe to call from any goroutine.
func (d *Detector) Degraded() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.degraded
}

// Decision is the outcome of one Decide pass — what the evaluator must
// act on this window.
type Decision struct {
	// Entered is true only on the pass that transitioned into degraded
	// mode (so the caller posts the "monitoring degraded" notice once).
	Entered bool
	// Exited is true only on the pass that transitioned out of degraded
	// mode (so the caller closes the notice once and resumes alerting).
	Exited bool
	// Commit lists the provisional DNS-failing monitor slugs the caller
	// must now run through the normal alert path (isolated failures that
	// did not trip degraded mode). Empty on a tripped pass — tripped
	// provisionals are discarded, never committed.
	Commit []string
	// Suppressed is the count of DNS-failure ticks discarded across the
	// whole blind window. Meaningful only on the Exited pass; zero
	// otherwise. Feeds the close notice's "N checks suppressed" summary.
	Suppressed int
}

// Decide runs one enter/exit evaluation pass at time now and returns the
// resulting Decision. Called once per window by the central evaluator.
func (d *Detector) Decide(now time.Time) Decision {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pruneLocked(now)

	var dec Decision
	if d.degraded {
		// Exit hysteresis: ≥1 success in the window AND DNS count below
		// N_min, subject to a minimum dwell of one full window.
		if now.Sub(d.enteredAt) >= d.cfg.Window &&
			d.successInWindowLocked(now) &&
			len(d.dnsLast) < d.cfg.MinMonitors {
			d.degraded = false
			d.enteredAt = time.Time{}
			dec.Exited = true
			// Discard-don't-replay: drop the suppressed window's DNS
			// provisionals; whatever is genuinely still down re-asserts
			// on each monitor's next normal tick. Any survivors count
			// toward the suppressed tally reported on exit.
			dec.Suppressed = d.suppressed + len(d.dnsLast)
			d.suppressed = 0
			d.dnsLast = map[string]time.Time{}
		}
		return dec
	}
	if d.tripLocked(now) {
		d.degraded = true
		d.enteredAt = now
		dec.Entered = true
		// Tripped: discard all provisionals so no phantom incident is
		// ever committed — but they were still suppressed, so seed the
		// tally with them for the eventual close summary.
		d.suppressed = len(d.dnsLast)
		d.dnsLast = map[string]time.Time{}
		return dec
	}
	// Not tripped: commit every held provisional as a normal failure,
	// then clear them so the next pass doesn't re-commit.
	if len(d.dnsLast) > 0 {
		dec.Commit = make([]string, 0, len(d.dnsLast))
		for slug := range d.dnsLast {
			dec.Commit = append(dec.Commit, slug)
		}
		sort.Strings(dec.Commit)
		d.dnsLast = map[string]time.Time{}
	}
	return dec
}

// tripLocked reports whether the enter condition holds: ≥ N_min distinct
// monitors with a DNS failure inside W, and zero successes inside W.
func (d *Detector) tripLocked(now time.Time) bool {
	if d.successInWindowLocked(now) {
		return false
	}
	return len(d.dnsLast) >= d.cfg.MinMonitors
}

// successInWindowLocked reports whether any probe succeeded within the
// trailing window ending at now.
func (d *Detector) successInWindowLocked(now time.Time) bool {
	return !d.lastSuccess.IsZero() && now.Sub(d.lastSuccess) < d.cfg.Window
}

// pruneLocked drops DNS-failure records that have aged out of the
// window so a stale blip can't keep the count elevated.
func (d *Detector) pruneLocked(now time.Time) {
	for slug, at := range d.dnsLast {
		if now.Sub(at) >= d.cfg.Window {
			delete(d.dnsLast, slug)
		}
	}
}
