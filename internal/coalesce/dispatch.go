// dispatch.go holds the three-state per-channel routing layer
// introduced by ADR-0004. Per channel, the dispatcher walks:
//
//	individual → pending → individual (sub-threshold flush) or group
//	group      → individual (when the open group closes)
//
// A failure in individual-mode arms a pendingWait timer; further
// failures join the pool. At expiry, the channel's burst count decides:
// < burstThreshold becomes N per-monitor messages (the legacy notifier
// path via Sink), ≥ burstThreshold becomes a single digest (the
// internal/group state machine). A failure that arrives while a group
// is already open joins the group directly, with no second pending
// window.
//
// The burst count is cumulative over burstWindow, not the size of the
// one pending pool. The scheduler jitters each monitor's first tick
// across its whole interval, so a cluster-wide outage reaches the
// dispatcher as a trickle spread over roughly one probe interval —
// commonly a monitor or two per pendingWait. Sizing the burst off a
// single pool therefore under-counts every real outage: each window
// flushes sub-threshold, the channel drops back to individual-mode, and
// the operator gets one message per monitor. Counting every monitor the
// channel currently has down inside burstWindow caps that at
// burstThreshold-1 individual messages, after which the channel
// promotes and the rest of the outage lands in one digest.
//
// The dispatch state is in-memory only. A restart loses any pending
// pools (they re-fill from the next probe) and reattaches open groups
// from incident_groups; the channel mode is derivable on the fly
// from "is there a live group?".

package coalesce

import (
	"context"
	"sort"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/alert"
	"github.com/toggle-corp/toggle-monitor/internal/group"
	"github.com/toggle-corp/toggle-monitor/internal/store"
)

// Sink is the per-monitor "immediate individual notification" seam,
// the same shape as the scheduler's legacy EventSink. The dispatcher
// calls it once per pool entry when a pending pool flushes
// sub-threshold, and once per non-pooled event in individual-mode
// (recovery after a flushed Open, reminders, etc.).
//
// nil Sink disables individual-mode entirely — every non-critical
// failure waits in pending and (if sub-threshold at expiry) silently
// discards. Not generally useful; lifecycle wires the real notifier.
type Sink func(ctx context.Context, row store.MonitorRow, channelSlug string, mentions []string, event *alert.Event) error

// Entry is the dispatcher's per-event input. It carries everything
// both flush paths need: group-mode reads MemberInfo (for the digest
// rendering), individual-mode reads Row+Event (for the legacy
// notifier).
type Entry struct {
	// Member is the digest-side display info used when the entry
	// promotes to group-mode.
	Member MemberInfo
	// Row is the persisted monitor row — required for the individual
	// notifier path.
	Row store.MonitorRow
	// Event is the alert state-machine event that produced this dispatch
	// call. Type drives the routing branch (Open vs Resolve vs Reminder).
	Event *alert.Event
	// Mentions carries the channel's pre-resolved Slack markup for the
	// individual notifier. Group-mode uses Member.Mentions instead.
	Mentions []string
}

// channelMode is the per-channel dispatcher state.
type channelMode int

const (
	modeIndividual channelMode = iota
	modePending
	modeGroup
)

// pendingEntry is one monitor's slot in a per-channel pending pool.
// The whole entry is replayed verbatim into either the individual
// sink (sub-threshold flush) or the group state machine (promote),
// so it must remember everything required by both paths.
type pendingEntry struct {
	entry     Entry
	enteredAt time.Time
}

// channelState tracks the per-channel dispatcher mode plus any pending
// pool. The group itself (when modeGroup) is looked up via
// Manager.groups[channel] — keeping it there means existing reattach
// and evaluator code paths work unmodified.
type channelState struct {
	mode       channelMode
	pending    map[string]*pendingEntry // slug → entry
	pendingDue time.Time                // pendingWait expiry; zero outside modePending

	// down records, per slug, when this channel last saw the monitor open
	// an incident. An entry lives until the monitor recovers or is paused,
	// or until it ages past burstWindow. Its size is the burst count the
	// promote decision reads — see the package comment on why the pending
	// pool alone is the wrong measure.
	down map[string]time.Time
	// individual holds the slugs whose incident was announced through the
	// per-monitor notifier. Their reminders and recovery keep flowing
	// through that same message even after the channel promotes to
	// group-mode, so a parent posted individually always gets its resolve
	// edit instead of being swallowed by a group it was never a member of.
	individual map[string]struct{}
}

// markDown records a monitor as currently down in this channel.
// Ageing is left to pruneDown, which runs against the manager clock at
// every point the count is read; stamping here off the event's own
// timestamp would let a late event rewind the prune anchor.
func (cs *channelState) markDown(slug string, at time.Time) {
	if cs.down == nil {
		cs.down = map[string]time.Time{}
	}
	cs.down[slug] = at
}

// pruneDown drops down-entries older than window. Ageing is what makes
// the count a burst rather than a census: a monitor that has been down
// for hours is not part of the outage that starts now.
func (cs *channelState) pruneDown(now time.Time, window time.Duration) {
	for slug, at := range cs.down {
		if now.Sub(at) >= window {
			delete(cs.down, slug)
		}
	}
}

// forgetDown drops a monitor from the burst count without releasing the
// per-monitor notifier's claim on its incident. A dependsOn pause rolls
// the failure into the parent's narrative, but any message the notifier
// already posted for the child is still the message its recovery has to
// edit.
func (cs *channelState) forgetDown(slug string) {
	delete(cs.down, slug)
}

// clearDown forgets a monitor whose incident ended: it leaves the burst
// count and the per-monitor notifier releases its claim.
func (cs *channelState) clearDown(slug string) {
	cs.forgetDown(slug)
	delete(cs.individual, slug)
}

// ownedByIndividual reports whether the per-monitor notifier owns this
// monitor's current incident.
func (cs *channelState) ownedByIndividual(slug string) bool {
	_, ok := cs.individual[slug]
	return ok
}

// takeIndividual records that the per-monitor notifier now owns this
// monitor's incident.
func (cs *channelState) takeIndividual(slug string) {
	if cs.individual == nil {
		cs.individual = map[string]struct{}{}
	}
	cs.individual[slug] = struct{}{}
}

// digestOwns reports whether the channel's live digest carries this
// monitor. A monitor the digest has no record of cannot be served by
// the group evaluator — Group.MarkUp is a no-op for an unknown member
// — so its recovery and its reminders belong to the per-monitor
// notifier instead. The case arises for a monitor paged individually
// before the channel promoted, and after a restart, where the group
// reattaches from the store while the in-memory ownership set is empty.
// Caller holds m.mu.
func (m *Manager) digestOwns(channel, slug string) bool {
	lg := m.groups[channel]
	return lg != nil && lg.g.Members[slug] != nil
}

// channelStateFor returns the dispatcher state for a channel, creating
// an individual-mode entry if absent. If the channel has an open
// group (e.g., after reattach), the mode is normalized to modeGroup.
// Caller holds m.mu.
func (m *Manager) channelStateFor(channel string) *channelState {
	cs, ok := m.channels[channel]
	if !ok {
		cs = &channelState{mode: modeIndividual}
		m.channels[channel] = cs
	}
	// Reattached open group reads back here as modeGroup.
	if _, hasGroup := m.groups[channel]; hasGroup && cs.mode != modeGroup {
		cs.mode = modeGroup
	}
	return cs
}

// Route is the main dispatcher entry. The scheduler hands every
// non-critical alert.Event here; the dispatcher decides whether to
// stage, pool, or directly act on the group state machine.
func (m *Manager) Route(ctx context.Context, channel string, e Entry) {
	if e.Event == nil {
		return
	}
	switch e.Event.Type {
	case alert.EventOpen:
		m.routeOpen(ctx, channel, e)
	case alert.EventResolve:
		m.routeResolve(ctx, channel, e)
	case alert.EventReminder:
		m.routeReminder(ctx, channel, e)
	}
}

// routeOpen dispatches a new failure based on the channel's current
// mode. Caller does not hold m.mu.
func (m *Manager) routeOpen(ctx context.Context, channel string, e Entry) {
	m.mu.Lock()
	cs := m.channelStateFor(channel)
	cs.markDown(e.Member.Slug, e.Event.At)
	switch cs.mode {
	case modeGroup:
		// Open group absorbs the failure directly.
		lg := m.ensureGroupLocked(ctx, channel, e.Event.At)
		if lg != nil {
			lg.info[e.Member.Slug] = e.Member
			lg.g.MarkDown(e.Member.Slug, e.Event.At)
		}
		m.mu.Unlock()
	case modeIndividual:
		// First failure of a burst — arm pendingWait.
		cs.mode = modePending
		cs.pending = map[string]*pendingEntry{}
		cs.pendingDue = e.Event.At.Add(m.pendingWait)
		cs.pending[e.Member.Slug] = &pendingEntry{entry: e, enteredAt: e.Event.At}
		m.mu.Unlock()
	case modePending:
		// Add to existing pool — timer is NOT restarted (the burst
		// window stays anchored to the first failure).
		cs.pending[e.Member.Slug] = &pendingEntry{entry: e, enteredAt: e.Event.At}
		m.mu.Unlock()
	}
}

// routeResolve dispatches a recovery. A monitor the per-monitor
// notifier owns recovers through that same message whatever the
// channel's mode, so its parent gets its resolve edit. Otherwise: in
// group-mode, MarkUp on the live group — or, for a monitor the digest
// has no record of, the per-monitor sink, since MarkUp would drop the
// recovery and leave the DOWN message standing; in pending-mode, the
// slug is dropped from the pool silently (no individual was ever
// notified); in individual-mode, the recovery flushes through the
// per-monitor sink.
func (m *Manager) routeResolve(ctx context.Context, channel string, e Entry) {
	m.mu.Lock()
	cs := m.channelStateFor(channel)
	owned := cs.ownedByIndividual(e.Member.Slug)
	cs.clearDown(e.Member.Slug)
	if owned {
		m.mu.Unlock()
		m.flushSink(ctx, channel, e)
		return
	}
	switch cs.mode {
	case modeGroup:
		if !m.digestOwns(channel, e.Member.Slug) {
			m.mu.Unlock()
			m.flushSink(ctx, channel, e)
			return
		}
		m.groups[channel].g.MarkUp(e.Member.Slug, e.Event.At)
		m.mu.Unlock()
	case modePending:
		// A failure that recovered before its pool ever flushed —
		// silently retract. The pool size shrinks; the flush decision
		// at expiry re-reads it.
		delete(cs.pending, e.Member.Slug)
		// If the pool emptied, retire the pending state entirely so
		// the next failure starts fresh.
		if len(cs.pending) == 0 {
			cs.mode = modeIndividual
			cs.pending = nil
			cs.pendingDue = time.Time{}
		}
		m.mu.Unlock()
	case modeIndividual:
		// Recovery of a previously individually-notified monitor —
		// flush through the sink.
		m.mu.Unlock()
		m.flushSink(ctx, channel, e)
	}
}

// routeReminder forwards a per-monitor reminder. Reminders are only
// meaningful for a monitor the per-monitor notifier owns — the group
// evaluator owns the reminders of every monitor its digest carries;
// pending-mode entries haven't been notified yet so a reminder is
// nonsensical (and the scheduler shouldn't emit one for a monitor whose
// Open wasn't yet flushed). Defensive: otherwise the reminder is
// silently dropped.
//
// A reminder does not re-stamp the monitor's burst-window entry. The
// window measures failures that arrived together, so a chronically-down
// monitor ages out of it rather than counting toward every later burst
// on the channel.
func (m *Manager) routeReminder(ctx context.Context, channel string, e Entry) {
	m.mu.Lock()
	cs := m.channelStateFor(channel)
	// The two mode arms also cover a monitor the dispatcher has no
	// record of — after a restart the in-memory state is empty while the
	// incident is still open in the DB, and its reminders must keep
	// flowing whether or not a group reattached on the same channel.
	forward := cs.ownedByIndividual(e.Member.Slug) ||
		cs.mode == modeIndividual ||
		(cs.mode == modeGroup && !m.digestOwns(channel, e.Member.Slug))
	m.mu.Unlock()
	if forward {
		m.flushSink(ctx, channel, e)
	}
}

// flushSink invokes the per-monitor sink, swallowing nil-sink. Caller
// must NOT hold m.mu (the sink may take time / fail).
func (m *Manager) flushSink(ctx context.Context, channel string, e Entry) {
	if m.sink == nil {
		// A nil sink in production silently destroys every sub-threshold
		// (individual-mode) notification — the exact failure mode that
		// caused a past regression where the lifecycle constructor
		// omitted Options.Sink. Scream on the first dropped
		// alert so a future wiring omission surfaces in logs immediately
		// instead of in an incident postmortem. lifecycle additionally
		// refuses to boot with a nil sink (see Manager.SinkWired); this
		// log covers any other caller that legitimately runs sink-less.
		m.log.Error("individual notification dropped: no sink wired",
			"channel", channel, "slug", e.Row.Slug, "event", string(e.Event.Type))
		return
	}
	if err := m.sink(ctx, e.Row, channel, e.Mentions, e.Event); err != nil {
		m.log.Warn("dispatch sink", "channel", channel, "slug", e.Row.Slug, "event", string(e.Event.Type), "error", err)
	}
}

// expirePending processes a pool whose pendingWait has elapsed. It
// runs the on-demand parent-probe pass (which may drain children via
// push-propagation), then decides individual-flush vs group-promote,
// emits the side-effects, and returns the channel to a steady-state
// mode. Caller holds m.mu; this method releases and re-acquires it
// around the probe pass (the probe callback drives alert.Apply +
// push-propagation, which re-enters Manager methods on m.mu).
func (m *Manager) expirePending(ctx context.Context, channel string, cs *channelState, now time.Time) {
	// 1. On-demand parent-probe pass. Run before the flush decision so
	//    push-propagation has a chance to redact children that share a
	//    failing parent. Skipped when no probe is wired or no parent is
	//    shared by ≥2 pool entries.
	if m.parentProbe != nil {
		hot := hotParents(cs.pending)
		if len(hot) > 0 {
			m.mu.Unlock()
			for _, parent := range hot {
				m.parentProbe(ctx, parent)
			}
			m.mu.Lock()
			// cs may have transitioned during the unlocked window
			// (children paused-out via push-propagation; parent's own
			// EventOpen routed via Route). Re-read mode and bail out
			// if we're no longer in pending — the channel may have
			// promoted, retired, or been wiped while we were probing.
			if cs.mode != modePending {
				return
			}
		}
	}

	pool := cs.pending
	// Pool emptied during pending (recoveries + pauses drained it): no
	// output. Treat the same as the pre-post group-discard semantic.
	if len(pool) == 0 {
		cs.mode = modeIndividual
		cs.pending = nil
		cs.pendingDue = time.Time{}
		return
	}

	// Burst size is every monitor this channel has down inside
	// burstWindow, unioned with this pool: a jittered estate delivers one
	// outage as a trickle of small pools, so a single pool under-counts
	// the outage. The union keeps the count at or above the pool size —
	// burstWindow may be as narrow as pendingWait, in which case the
	// pool's own entries sit exactly on the ageing boundary.
	cs.pruneDown(now, m.burstWindow)
	burst := len(cs.down)
	for slug := range pool {
		if _, counted := cs.down[slug]; !counted {
			burst++
		}
	}
	if m.burstThreshold > 0 && burst >= m.burstThreshold {
		// Promote to group: pre-warm the group state machine with every
		// pool member, then call Open to bypass the legacy groupWait
		// (the dispatcher already did the waiting in pending). Dispatch
		// the post synchronously so the digest message lands this tick.
		earliest := now
		for _, pe := range pool {
			if pe.enteredAt.Before(earliest) {
				earliest = pe.enteredAt
			}
		}
		lg := m.ensureGroupLocked(ctx, channel, earliest)
		if lg != nil {
			for _, pe := range pool {
				lg.info[pe.entry.Member.Slug] = pe.entry.Member
				lg.g.MarkDown(pe.entry.Member.Slug, pe.enteredAt)
			}
			if action := lg.g.Open(now); action.Kind != "" {
				m.dispatch(ctx, lg, []group.Action{action}, now)
				if err := m.store.SaveIncidentGroup(ctx, m.toRow(lg)); err != nil {
					m.log.Warn("save incident group", "channel", channel, "error", err)
				}
			}
		}
		cs.mode = modeGroup
		cs.pending = nil
		cs.pendingDue = time.Time{}
		return
	}

	// Sub-threshold: flush each pool entry through the per-monitor
	// sink as the legacy EventSink would. Release the lock around the
	// sink calls to keep them out of the critical section.
	entries := make([]Entry, 0, len(pool))
	for _, pe := range pool {
		entries = append(entries, pe.entry)
		// The per-monitor notifier now owns this incident: its reminders
		// and its recovery must keep addressing the message about to be
		// posted, even once a later trickle promotes the channel to
		// group-mode.
		cs.takeIndividual(pe.entry.Member.Slug)
	}
	cs.mode = modeIndividual
	cs.pending = nil
	cs.pendingDue = time.Time{}
	m.mu.Unlock()
	for _, e := range entries {
		m.flushSink(ctx, channel, e)
	}
	m.mu.Lock()
}

// hotParents identifies dependsOn targets shared by ≥2 entries in the
// pool that are not themselves in the pool. These are the candidates
// the on-demand probe should investigate at pendingWait expiry — a
// confirmed-down parent collapses the children's narrative into the
// parent's incident.
//
// Returns slugs sorted for deterministic probe ordering (useful in
// tests and logs). A parent that is itself in the pool is skipped
// because its own scheduler tick already fired its EventOpen, which
// runs push-propagation through the scheduler before pendingWait
// expires (so its children are already drained from this pool).
func hotParents(pool map[string]*pendingEntry) []string {
	if len(pool) < 2 {
		return nil
	}
	counts := map[string]int{}
	for _, pe := range pool {
		for _, parent := range pe.entry.Row.DependsOn {
			if _, alreadyInPool := pool[parent]; alreadyInPool {
				continue
			}
			counts[parent]++
		}
	}
	var hot []string
	for parent, n := range counts {
		if n >= 2 {
			hot = append(hot, parent)
		}
	}
	sort.Strings(hot)
	return hot
}

// retireGroupChannel collapses a channel back to individual-mode when
// its group retires (closed or discarded). Caller holds m.mu.
func (m *Manager) retireGroupChannel(channel string) {
	cs := m.channels[channel]
	if cs == nil {
		return
	}
	cs.mode = modeIndividual
	cs.pending = nil
	cs.pendingDue = time.Time{}
	// The digest closed, so every monitor it covered is accounted for.
	// Anything still down that the per-monitor notifier owns keeps its
	// entry — its own resolve will clear it.
	for slug := range cs.down {
		if !cs.ownedByIndividual(slug) {
			delete(cs.down, slug)
		}
	}
}
