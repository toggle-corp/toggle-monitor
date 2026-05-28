# ADR 0004 — Burst dispatcher supersedes always-coalesce

**Status:** Accepted
**Date:** 2026-05-28
**Supersedes:** the "always coalesce" routing default introduced
2026-05-27 in `internal/coalesce` (per-channel living digest as the
sole non-critical path), the corresponding `slack.coalesce.groupWait`
field name, and the design memo recorded as `MEMORY.md` →
`project-alert-coalescing-design`. Retains and extends the
`internal/group` state machine, the `incident_groups` persistence
schema, and the dependsOn push-propagation *concept* — but reframes
push-propagation as a real synchronous walk instead of the pull-gate
that was named "push" but never wrote that code.

## Context

The 2026-05-27 design coalesced every non-critical monitor into a
per-channel digest by default. The motivating problem was an alert
storm: a bastion outage taking out ~100 children produced ~100 Slack
messages because every child's `Tick` posted independently and a
30-second debounce only flattened co-tick failures. "Always coalesce"
turns the storm into a single living digest — correct for the burst
case but heavy-handed for the routine case, where one or two monitors
failing produces a digest message just to wrap a single row.

Observed weaknesses of always-coalesce (most surfaced before the
design saw production traffic, hence ADR-0004 lands three days after
the original memo):

- **Routine failures get muffled.** A solo monitor going down posts a
  digest with one row. Operators reading the channel see "🚨 1 down"
  framed the same way a 60-monitor outage would be. The signal is
  diluted.
- **Push-propagation is a pull-gate.** The 2026-05-27 memo described
  push-propagation as *"when parent incident opens, immediately mark
  children temporary-paused AND pull them out of the active digest."*
  The actual code (`scheduler.go:344` at the time) only paused a
  child during the *child's* own `Tick` after reading parent status
  from the DB. With 10-minute child intervals, that's up to 10
  minutes of post-burst digest mis-narration before the digest
  flips from "60 services down" to "1 service (bastion) down · 60
  paused." The narrative is wrong at the most paged-on moment.
- **No way to surface a root cause at minute zero.** Even with the
  pull-gate eventually paging in the right answer, the *initial*
  Slack message reads as a fanout outage. Operators start incident
  response under a wrong mental model.
- **`groupWait` conflates two unrelated waits.** The same 30s window
  serves both as "collect-before-post" (a dispatcher decision) and as
  the group state-machine's pre-post wait. Tangled lifetime, tangled
  semantics.

The first three are the load-bearing weaknesses. The fourth is
hygiene that falls out of fixing the first.

This ADR is the redesign that fell out of `/grill-me` 2026-05-28. The
core insight: the *coalescing* mechanism is right; the *default* is
wrong. Coalesce only when a burst actually happened.

## Decision

### Per-channel three-state dispatcher

```
individual ─first failure─▶ pending ─count<N─▶ individual flush
                            pending ─count≥N─▶ group
group ─last member recovers─▶ individual
```

Every channel starts in `individual`. The first non-critical
`EventOpen` arms a pending pool with a `pendingWait` timer (default
30s). Further `EventOpen`s on the same channel during the window join
the pool; in-window recoveries and push-propagation pauses retract
from the pool silently. At expiry:

- pool empty → silent discard, mode → individual.
- pool size `< burstThreshold` → each pool entry flushes through the
  per-monitor `EventSink` (the legacy notifier path). The 90% case.
- pool size `>= burstThreshold` → pool promotes to a `group`. A
  single digest message is posted with `@channel` (configurable),
  carrying every pool member as initial rows. Mode → group.

Once in `group` mode, subsequent failures route directly into the
digest via `MarkDown` (no second pending window). Heartbeats
(`groupInterval`) flush accrued joins/recoveries as one edit +
threaded reply. Reminders (`repeatInterval`) re-mention the
broadcast marker. When the last member recovers and resolve-debounce
elapses, the group closes and the channel reverts to individual.

The pending pool is in-memory only — a process restart loses it. Open
groups still reattach from `incident_groups`. The channel mode is
derivable on the fly ("is there an open group?" → group-mode; "is
there a non-empty pending pool?" → pending-mode; otherwise
individual).

### Real push-propagation

A parent's `EventOpen` now synchronously walks its reverse-dependsOn
list and persists each child as `temporary-paused` + calls
`Manager.Pause` on the child's channel. Children sitting in pending
pools are retracted (the pool's flush decision then sees them
absent); children in open digests render as struck-through paused.

The pull-gate at `scheduler.Tick`'s top stays — it remains the safety
net for monitors added or rediscovered between the parent's failure
and the child's next regular tick.

The hook is wired by lifecycle, which owns the reverse-dependsOn
index (`internal/depindex.Build` from a fresh `CurrentPlans()` snapshot
on each invocation — kube discovery can add/remove monitors at any
time and push-propagation is rare enough that an O(N·D) rebuild per
call beats maintaining a live inverted index).

### On-demand parent probe at pendingWait expiry

The push above only fires when the *parent's* tick has run. If the
parent has a longer interval than `pendingWait` and its tick hasn't
landed yet by the time the children's pool is about to flush, the
parent isn't in the pool and push hasn't fired — children would flush
as N individual messages (still a storm, just a smaller one).

The dispatcher closes this gap with one bounded probe per "hot
parent" — defined as a dependsOn target referenced by ≥2 pool entries
that isn't itself in the pool. At pendingWait expiry, before the
flush decision:

1. Identify hot parents from the pool's `Row.DependsOn` aggregation.
2. For each, fire one synchronous probe through its configured
   `Prober` with `onDemandProbeTimeout` budget (default 5s).
3. If down: drive `alert.Apply` + `repo.ApplyCheck` to persist the
   `EventOpen`, fire push-propagation (drains children), and route
   the parent's failure through the dispatcher for its own channel
   (so the parent's narrative lands in Slack).
4. Re-read the pool. Decide flush vs promote on the post-redaction
   pool.

The lock is released around the probe callbacks; after the unlocked
window, the dispatcher re-reads channel mode and bails out gracefully
if the channel has been promoted or retired in between.

### Config (`slack.coalesce`)

```yaml
slack:
  coalesce:
    pendingWait:          30s    # dispatcher wait window
    burstThreshold:         5    # promote at-or-above; 0 disables groups; 1 rejected
    groupInterval:         5m    # digest heartbeat (also resolve-debounce window)
    repeatInterval:       10m    # group-mode reminder cadence
    groupMention:    channel    # broadcast on group open + reminder; channel | here | none
    onDemandProbeTimeout:  5s    # hot-parent probe budget
```

- `groupWait` is accepted as a deprecated alias for `pendingWait` for
  one release. Setting both is a validation error.
- `burstThreshold: 0` disables group-mode entirely (every failure →
  individual). The dispatcher still pools failures inside
  `pendingWait` and silently retracts in-window recoveries; it just
  never promotes.
- `burstThreshold: 1` is rejected — pathological, would trip on any
  single failure and is equivalent to "always coalesce" which this
  ADR exists to undo.
- `groupMention: none` suppresses the broadcast marker entirely
  (operator opt-out; for personal / dev setups where `@channel` is
  meaningless).
- `repeatInterval` default tightened from 30m → 10m: every group IS
  the burst case under this design, so the louder cadence is
  universal. The legacy 30m default referenced a "minor digest"
  context that no longer exists.

### Critical monitors unchanged

`monitors[].critical: true` still bypasses the dispatcher entirely —
the scheduler routes the event directly through `EventSink` for
immediate per-monitor posting. dependsOn pause still wins (a paused
critical monitor stays silent).

### Renderer simplification

Every group is a burst incident; there is no normal-vs-major tier
inside group-mode. The digest header reads
`🚨 N down · M recovered (of T)` — single severity, no "Major outage"
phrase. The broadcast marker (`<!channel>` / `<!here>`) is injected at
open + reminder; edits never re-mention. Close reads
`✅ All clear — M recovered (was down for X)`.

`Group.Open` is added to `internal/group` for the pre-warmed
promotion path: the dispatcher has already done the pendingWait
collection, so the group's own `Posted=false` branch is bypassed.

## Consequences

### Slack volume (illustrative)

| Scenario | Before (always coalesce) | After (this ADR) |
|---|---|---|
| 1 monitor fails routinely | 1 digest message with 1 row | 1 per-monitor message |
| 3 unrelated failures in 30s | 1 digest with 3 rows | 3 per-monitor messages (sub-threshold) |
| 50 children of bastion fail in 30s, bastion's tick lands within window | 1 digest with 51 rows; 50 mis-named as independent failures for ~10m | 1 per-monitor message naming `bastion` (push-propagation drains children) |
| 50 children of bastion fail; bastion interval is 60s, tick hasn't landed | 1 digest with 50 rows; bastion appears 60s+ later | 1 per-monitor message naming `bastion` (on-demand probe fires it) |
| 8 unrelated monitors fail in 30s | 1 digest with 8 rows | 1 digest with 8 rows + @channel |
| 8 fail; one channel pools 5, another 3 | 2 digests | 1 digest (5-cluster channel) + 3 individuals (3-cluster channel) |

### Code (this commit-set)

- **`internal/config/config.go`** — `Coalesce` struct expanded:
  `PendingWait`, `BurstThreshold *int`, `GroupMention`,
  `OnDemandProbeTimeout`. `GroupWait` retained as deprecated alias
  with `EffectivePendingWait` collapsing. New `validateCoalesce`
  enforces alias-exclusion + threshold/mention rules.
- **`internal/depindex/`** — new tiny package holding the
  reverse-dependsOn index. Pure function + `Children(parent)` lookup.
- **`internal/group/group.go`** — adds `Group.Open(now)` for
  pre-warmed promotion. No semantic change to the existing
  `Evaluate` state machine.
- **`internal/coalesce/dispatch.go`** — new. Three-state per-channel
  dispatcher, `Entry` payload type, `Sink` per-monitor seam,
  `Manager.Route`. `expirePending` runs the on-demand probe pass.
- **`internal/coalesce/manager.go`** — extended `Manager` with
  per-channel mode state and the probe / mention plumbing. The
  central evaluator now runs a pending-pass before the group-pass.
  `Down` is kept as a deprecated shim → `Route(EventOpen)`. `Up` and
  `Pause` route by channel mode.
- **`internal/scheduler/scheduler.go`** — adds `GroupSink.Route`,
  `PushPropagation` callback, `WithPushPropagation` option. Tick's
  post-`alert.Apply` switch becomes "critical → EventSink; else →
  Route". Push-propagation fires synchronously on EventOpen.
- **`internal/lifecycle/coalesce_wiring.go`** — adds
  `makePushPropagation` (closure over `repo` + dispatcher +
  planSource), `makeOnDemandParentProbe` (closure capturing the
  Manager itself via post-construction `SetOnDemandParentProbe`), and
  the `Route` adapter into `coalesce.Entry`.
- **`internal/lifecycle/lifecycle.go`** — wires the new accessors
  through to the Manager; reorders planSource / scheduler
  construction so the closures can capture the right pointers.

### Operator-visible breaking changes (greenfield, but document them)

- `slack.coalesce.groupWait` is accepted but logs no warning yet —
  one-release deprecation window. Operators should rename to
  `pendingWait` at their next config edit. Setting both is an error
  at startup.
- The default `repeatInterval` changes from 30m to 10m. Operators
  who relied on the 30m default see a louder cadence; configure
  explicitly to keep the old value.
- The digest is no longer the default for non-critical monitors:
  routine single-failure incidents now post as per-monitor messages
  via the existing Slack notifier. Operators who liked the "always
  one consolidated channel message" framing must lower
  `burstThreshold` to 2 (the minimum permitted value) to restore
  approximately the old behavior — but the dispatcher still won't
  digest a true solo failure (count=1 < 2).

### Out of scope for this ADR

- **Cross-channel "major outage" broadcast.** Deferred per the
  /grill-me Q6 conclusion. A real cross-system incident touching N
  team channels currently fires N independent `@channel` pings, one
  per affected room. If operators report that as painful, v2 adds an
  append-only broadcast to a configured channel; the dispatcher is
  positioned to emit a cross-channel event but no consumer exists
  yet.
- **Persistent severity tier inside a group.** The 2026-05-28
  /grill-me explored a normal/major split on top of group-mode
  (Q3/Q5/Q7); the inverted model collapsed it because every group is
  the burst case. If burst sub-categorization becomes useful later
  (e.g., "20+ down is *catastrophic*"), it can be a second threshold
  on top of `burstThreshold`. Not designed here.
- **Adaptive `pendingWait`.** A pendingWait that extends when the
  pool grows fast (back-pressure) was considered and rejected — it
  delays the flush decision unpredictably and the on-demand probe
  already covers the "parent hasn't ticked yet" case. Fixed
  `pendingWait`.
- **Per-channel override of dispatcher knobs.** v1 has only global
  `slack.coalesce.*`. If a channel needs a different threshold or
  cadence, raise an issue with a concrete operator scenario.
- **Pending-pool persistence.** A process restart loses pending
  state. The pool re-fills on the next probe tick of each affected
  monitor; the worst case is a slightly delayed dispatch decision
  immediately post-restart. Persisting pending state would add a
  table and reattach logic for a ~30-second window — not worth it.

## References

- `MEMORY.md` → `project-alert-coalescing-design` — the superseded
  2026-05-27 design memo.
- `MEMORY.md` → `project-toggle-monitor-design-grilling` — the
  original storm-problem framing.
- `internal/group/group.go` — the digest state machine, retained
  largely unchanged.
- `internal/scheduler/scheduler.go:332-348` — the dependsOn pull-gate
  that remains as the post-startup safety net beneath the new push.
- /grill-me 2026-05-28 session — the full design conversation,
  including the cross-channel-broadcast / severity-tier branches
  that this ADR deliberately doesn't ship.
