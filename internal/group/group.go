// Package group holds the alert-coalescing state machine. A Group is a
// living, per-Slack-channel incident: many monitors going down within
// one window collapse into a single digest message that edits in place
// as monitors recover, instead of one Slack message per monitor.
//
// The design (settled 2026-05-27) borrows Prometheus Alertmanager
// semantics:
//
//   - groupWait      — after the first member goes down, wait this long
//     collecting the initial burst before posting the digest once.
//   - groupInterval  — the heartbeat: joins / recoveries / flaps that
//     accrue between heartbeats are flushed as ONE delta (an edit to the
//     digest plus a single threaded reply), never one message per event.
//   - repeatInterval — cadence of the "still down" reminder.
//   - resolveDebounce — a recovered member must stay up at least this
//     long before it is *rendered* recovered. This is what dampens flap
//     chatter: a monitor that recovers and fails again inside the window
//     never emits a recovery (or a re-down) delta at all.
//
// Group is a deterministic state machine driven by an injected clock:
// callers feed it member transitions (MarkDown / MarkUp / MarkPaused)
// and then call Evaluate(now) to advance timers and collect the Slack
// side-effects to perform. Every field maps to a persisted column so a
// process restart can reload an open group and reattach to its existing
// digest message rather than re-storming.
package group

import (
	"sort"
	"time"
)

// MemberState classifies one monitor within a group.
type MemberState string

const (
	// MemberDown — currently failing. Rendered un-struck in the digest.
	MemberDown MemberState = "down"
	// MemberRecovering — came back up but still inside resolveDebounce;
	// not yet rendered as recovered (so a flap inside the window is
	// invisible). Physically up, but counts as "not yet recovered" for
	// close purposes.
	MemberRecovering MemberState = "recovering"
	// MemberRecovered — stayed up past resolveDebounce. Rendered with a
	// strike-through but kept visible until the whole group closes.
	MemberRecovered MemberState = "recovered"
	// MemberPaused — pulled out of the digest by dependsOn
	// push-propagation: a parent went down, so this child's failure is
	// not actionable and rolls into the parent's "pausing N" count.
	MemberPaused MemberState = "paused"
)

// Member is one monitor's membership in a group.
type Member struct {
	Slug      string
	State     MemberState
	JoinedAt  time.Time // first time this monitor entered the group
	DownSince time.Time // most recent down transition (zero if never down)
	UpSince   time.Time // most recent up transition (zero while down)
	ChangedAt time.Time // last state change (observability/persistence)

	// Rendered is the render-class this member had in the digest as of
	// the last flush ("" | "active" | "recovered" | "paused"). Deltas
	// are computed against it, so a flap that never reached a rendered
	// state (recover→re-down inside resolveDebounce) produces no churn.
	Rendered string
}

// render-class strings: how a member appears in the digest. Down and
// Recovering both render as "active" (un-struck) — a recovering member
// is shown as still-down until resolveDebounce confirms it.
const (
	renderNone      = ""
	renderActive    = "active"
	renderRecovered = "recovered"
	renderPaused    = "paused"
)

func renderClass(s MemberState) string {
	switch s {
	case MemberDown, MemberRecovering:
		return renderActive
	case MemberRecovered:
		return renderRecovered
	case MemberPaused:
		return renderPaused
	}
	return renderNone
}

// Config carries the four coalescing intervals. Zero values are filled
// with the documented defaults by (*Config).withDefaults.
type Config struct {
	GroupWait       time.Duration // initial collect window (default 30s)
	GroupInterval   time.Duration // delta heartbeat (default 5m)
	RepeatInterval  time.Duration // still-down reminder cadence (default 30m)
	ResolveDebounce time.Duration // up-for-this-long before rendered recovered (default = GroupInterval)
}

const (
	defaultGroupWait      = 30 * time.Second
	defaultGroupInterval  = 5 * time.Minute
	defaultRepeatInterval = 30 * time.Minute
)

func (c Config) withDefaults() Config {
	if c.GroupWait <= 0 {
		c.GroupWait = defaultGroupWait
	}
	if c.GroupInterval <= 0 {
		c.GroupInterval = defaultGroupInterval
	}
	if c.RepeatInterval <= 0 {
		c.RepeatInterval = defaultRepeatInterval
	}
	if c.ResolveDebounce <= 0 {
		c.ResolveDebounce = c.GroupInterval
	}
	return c
}

// Group is the living per-channel incident.
type Group struct {
	Channel        string             // Slack channel slug
	OpenedAt       time.Time          // first member down (group birth)
	DigestTS       string             // Slack ts of the digest message; empty until posted
	DigestChannel  string             // resolved channel ID the digest lives in
	Posted         bool               // initial digest posted (past groupWait)
	Closed         bool               // every member recovered/paused; incident over
	LastFlushAt    time.Time          // last delta heartbeat
	LastReminderAt time.Time          // last still-down reminder
	Members        map[string]*Member // keyed by monitor slug

	cfg Config
}

// New creates an empty group born at `now`. The first MarkDown is what
// gives it a member; OpenedAt anchors the groupWait timer.
func New(channel string, now time.Time, cfg Config) *Group {
	return &Group{
		Channel:  channel,
		OpenedAt: now,
		Members:  map[string]*Member{},
		cfg:      cfg.withDefaults(),
	}
}

// SetConfig re-applies intervals (used when reloading a persisted group,
// since cfg is not itself persisted — it comes from current config).
func (g *Group) SetConfig(cfg Config) { g.cfg = cfg.withDefaults() }

// Open transitions a freshly-constructed Group into Posted state and
// returns the initial ActionPostDigest. It bypasses the legacy
// groupWait check used by Evaluate's !Posted branch — the burst
// dispatcher (ADR-0004) already did the waiting in its pending pool
// before MarkDown'ing every member here, so the digest posts
// immediately. No-op (returns zero Action) if already posted or
// closed.
func (g *Group) Open(now time.Time) Action {
	if g.Posted || g.Closed {
		return Action{}
	}
	g.Posted = true
	g.LastFlushAt = now
	g.LastReminderAt = now
	g.commitRender()
	return Action{Kind: ActionPostDigest}
}

// MarkDown records that `slug` is failing. A new slug joins the group;
// an existing recovered/recovering member transitions back to down (a
// flap). A no-op if the member is already down.
func (g *Group) MarkDown(slug string, now time.Time) {
	m, ok := g.Members[slug]
	if !ok {
		g.Members[slug] = &Member{
			Slug:      slug,
			State:     MemberDown,
			JoinedAt:  now,
			DownSince: now,
			ChangedAt: now,
		}
		return
	}
	if m.State == MemberDown {
		return
	}
	m.State = MemberDown
	m.DownSince = now
	m.UpSince = time.Time{}
	m.ChangedAt = now
}

// MarkUp records that `slug` recovered. It enters MemberRecovering; the
// recovery is only *rendered* after resolveDebounce elapses (see
// Evaluate). No-op for unknown or already-up members.
func (g *Group) MarkUp(slug string, now time.Time) {
	m, ok := g.Members[slug]
	if !ok || m.State != MemberDown {
		return
	}
	m.State = MemberRecovering
	m.UpSince = now
	m.ChangedAt = now
}

// MarkPaused records that dependsOn push-propagation pulled `slug` out
// of the digest because a parent went down. No-op for unknown members.
func (g *Group) MarkPaused(slug string, now time.Time) {
	m, ok := g.Members[slug]
	if !ok || m.State == MemberPaused {
		return
	}
	m.State = MemberPaused
	m.ChangedAt = now
}

// ActionKind is one Slack side-effect Evaluate asks the caller to do.
type ActionKind string

const (
	// ActionPostDigest — post the initial digest (groupWait elapsed,
	// at least one member still down).
	ActionPostDigest ActionKind = "post_digest"
	// ActionUpdate — edit the digest scoreboard and post one threaded
	// delta reply (carries Delta).
	ActionUpdate ActionKind = "update"
	// ActionRemind — post a "still down" reminder reply, re-pinging the
	// union of down owners.
	ActionRemind ActionKind = "remind"
	// ActionClose — every member recovered/paused: render the final
	// all-resolved digest, post the closing reply, retire the group.
	ActionClose ActionKind = "close"
	// ActionDiscard — the whole burst recovered inside groupWait; never
	// posted anything. The caller should drop the group silently.
	ActionDiscard ActionKind = "discard"
)

// Delta buckets the membership changes since the last flush, for the
// threaded reply. Slugs are sorted.
type Delta struct {
	NewlyDown []string // joined this window
	Recovered []string // confirmed recovered this window
	Flapped   []string // were recovered/recovering, went down again
	Paused    []string // pulled out by push-propagation this window
}

// Empty reports whether the delta carries no changes.
func (d Delta) Empty() bool {
	return len(d.NewlyDown) == 0 && len(d.Recovered) == 0 &&
		len(d.Flapped) == 0 && len(d.Paused) == 0
}

// Action is one item of work returned by Evaluate.
type Action struct {
	Kind  ActionKind
	Delta Delta // populated for ActionUpdate (and ActionClose's final reply)
}

// Evaluate advances the group's timers to `now`, mutates state
// (resolve-debounce promotions, posted/closed flags, flush/reminder
// stamps) and returns the ordered Slack side-effects to perform. It is
// the only method that emits actions; MarkDown/MarkUp/MarkPaused only
// stage transitions.
func (g *Group) Evaluate(now time.Time) []Action {
	if g.Closed {
		return nil
	}

	// 1. Resolve-debounce: promote Recovering members that have stayed
	//    up long enough into Recovered (this is the moment a recovery
	//    becomes renderable). A flap before this never gets here.
	for _, m := range g.Members {
		if m.State == MemberRecovering && !now.Before(m.UpSince.Add(g.cfg.ResolveDebounce)) {
			m.State = MemberRecovered
			m.ChangedAt = now
		}
	}

	down := g.countByState(MemberDown)
	recovering := g.countByState(MemberRecovering)

	// 2. Pre-post: still inside groupWait, or just elapsed.
	if !g.Posted {
		if now.Before(g.OpenedAt.Add(g.cfg.GroupWait)) {
			return nil
		}
		// groupWait elapsed.
		if down == 0 {
			// The burst recovered before we ever posted — absorb it.
			g.Closed = true
			return []Action{{Kind: ActionDiscard}}
		}
		g.Posted = true
		g.LastFlushAt = now
		g.LastReminderAt = now
		g.commitRender()
		return []Action{{Kind: ActionPostDigest}}
	}

	// 3. Posted. Close first: nothing down and nothing pending recovery.
	if down == 0 && recovering == 0 {
		g.Closed = true
		// The final close render reads full state; surface the residual
		// delta (the last recoveries) in the closing reply too.
		d := g.delta()
		g.commitRender()
		return []Action{{Kind: ActionClose, Delta: d}}
	}

	var actions []Action

	// 4. Heartbeat: flush accrued changes as one edit + one reply.
	if !now.Before(g.LastFlushAt.Add(g.cfg.GroupInterval)) {
		if d := g.delta(); !d.Empty() {
			actions = append(actions, Action{Kind: ActionUpdate, Delta: d})
		}
		g.commitRender()
		g.LastFlushAt = now
	}

	// 5. Reminder: still-down nag at repeatInterval.
	if down > 0 && !now.Before(g.LastReminderAt.Add(g.cfg.RepeatInterval)) {
		actions = append(actions, Action{Kind: ActionRemind})
		g.LastReminderAt = now
	}

	return actions
}

// delta buckets membership changes since the last rendered digest into
// the threaded-reply Delta, comparing each member's current render-class
// against the one captured at the previous flush.
func (g *Group) delta() Delta {
	var d Delta
	for _, m := range g.Members {
		cur := renderClass(m.State)
		prev := m.Rendered
		if cur == prev {
			continue
		}
		switch {
		case cur == renderPaused:
			d.Paused = append(d.Paused, m.Slug)
		case cur == renderActive && (prev == renderNone || prev == renderPaused):
			d.NewlyDown = append(d.NewlyDown, m.Slug)
		case cur == renderActive && prev == renderRecovered:
			d.Flapped = append(d.Flapped, m.Slug)
		case cur == renderRecovered && prev == renderActive:
			d.Recovered = append(d.Recovered, m.Slug)
		}
	}
	sort.Strings(d.NewlyDown)
	sort.Strings(d.Recovered)
	sort.Strings(d.Flapped)
	sort.Strings(d.Paused)
	return d
}

// commitRender snapshots every member's current render-class so the
// next delta() is computed relative to what the operator last saw.
func (g *Group) commitRender() {
	for _, m := range g.Members {
		m.Rendered = renderClass(m.State)
	}
}

func (g *Group) countByState(s MemberState) int {
	n := 0
	for _, m := range g.Members {
		if m.State == s {
			n++
		}
	}
	return n
}

// DownSlugs returns the currently-down monitor slugs (Down state only,
// not Recovering), sorted. Used to render the digest scoreboard and to
// build the reminder mention union.
func (g *Group) DownSlugs() []string { return g.slugsByState(MemberDown) }

// ActiveSlugs returns the slugs still rendered as "not recovered" (Down
// or Recovering), sorted.
func (g *Group) ActiveSlugs() []string {
	out := append(g.slugsByState(MemberDown), g.slugsByState(MemberRecovering)...)
	sort.Strings(out)
	return out
}

func (g *Group) slugsByState(s MemberState) []string {
	var out []string
	for _, m := range g.Members {
		if m.State == s {
			out = append(out, m.Slug)
		}
	}
	sort.Strings(out)
	return out
}

// Counts is a render-friendly snapshot of the scoreboard.
type Counts struct {
	Down       int
	Recovering int
	Recovered  int
	Paused     int
	Total      int
}

// Scoreboard returns the header counts ("X down · Y recovered (of N)").
// Paused members are excluded from Total since they belong to the
// parent's incident, not this digest.
func (g *Group) Scoreboard() Counts {
	c := Counts{}
	for _, m := range g.Members {
		switch m.State {
		case MemberDown:
			c.Down++
		case MemberRecovering:
			c.Recovering++
		case MemberRecovered:
			c.Recovered++
		case MemberPaused:
			c.Paused++
		}
	}
	c.Total = c.Down + c.Recovering + c.Recovered
	return c
}
