---
status: accepted
date: 2026-08-31
deciders: [monitoring-team]
---

# ADR 0015 — The burst count spans a rolling window, not one pending pool

**Status:** Accepted
**Date:** 2026-08-31
**Relates to:** ADR-0004 (burst dispatcher) — amends its promote rule.
ADR-0008 (self-health degraded mode) — restores the `burstThreshold − 1`
bound that ADR-0008's "Out of scope" section already assumed held.

## Context

An internet outage produced one Slack message per monitor on a channel
that should have received one digest, with `burstThreshold: 5` in
effect. The same class of storm ADR-0008 was written to stop, arriving
through a path ADR-0008 does not cover.

A `RunServe` simulation reproduces it exactly: 12 monitors on one
channel, `interval: 24s`, `pendingWait: 2s`, `burstThreshold: 5`, one
outage taking every target down at once. Result: **12 separate DOWN
parent messages, zero digests.**

The cause is an interaction between two behaviours that are each
correct alone:

- `scheduler.runMonitor` applies **startup jitter** — `rand(0,
  interval)` before a monitor's first tick — so monitors are spread
  uniformly across their interval instead of stampeding the estate on
  every cycle.
- `Manager.expirePending` sizes the burst from **one pending pool**: the
  failures that arrived inside a single `pendingWait` anchored to the
  first of them.

So a cluster-wide outage never reaches the dispatcher as a burst. It
reaches it as a **trickle** of roughly

```
N × pendingWait / interval
```

monitors per window. With the production defaults (`pendingWait: 30s`)
and any interval past ~2.5 minutes, that is under `burstThreshold: 5`
for any estate size. Every window flushes sub-threshold, the channel
returns to `modeIndividual`, and the next few failures start a fresh
sub-threshold window. Nothing bounds the total: the storm is one
message per monitor, however large the estate.

ADR-0008 already assumed the opposite. Its "Out of scope" section
bounds the residual Slack-only storm at "≤ `burstThreshold−1` late
fresh-parents *per channel* — bounded below the operator's own burst
line." That bound was never real, because the burst line was measured
against a pool that a jittered estate can never fill.

A second defect rides along. When a trickle *does* eventually promote a
later pool to group-mode, the monitors already paged individually are
not group members. `routeResolve` in `modeGroup` calls `Group.MarkUp`,
which is a documented no-op for an unknown member, so their recovery is
swallowed: the DOWN parent in Slack is never edited to its resolved
form and no resolve reply lands. Red circles stay standing after the
incident closes.

## Decision

### Count the channel, not the pool

`channelState` gains `down: map[slug]time.Time` — every monitor this
channel currently has an open incident for, stamped with when it
opened. Entries are added on `EventOpen` and dropped on recovery, on a
dependsOn pause, and when the group covering them retires. A reminder
deliberately does *not* re-stamp an entry: the window measures failures
that arrived together, so a chronically-down monitor must age out of it
rather than keep counting toward every later burst on the channel.

At `pendingWait` expiry the promote decision reads `len(cs.down)`
instead of `len(pool)`, after pruning entries older than a new
`burstWindow`.

The consequence is a two-tier outcome with a hard bound: the first
`burstThreshold − 1` monitors of an outage page individually — the
dispatcher genuinely cannot know at 30s that more is coming — and the
moment the cumulative count crosses `burstThreshold` the channel
promotes and **everything after it lands in one digest**. The same
simulation goes from 12 notifications to 4.

We take the individual prefix rather than widen `pendingWait` toward a
probe interval, because `pendingWait` is latency paid by *every* alert,
including the lone-monitor case that is 90% of traffic. `burstWindow`
is memory, not latency: it costs nothing when nothing is failing.

### `burstWindow` — new, defaulted, floored

```yaml
slack:
  coalesce:
    burstThreshold: 5     # monitors down inside burstWindow that promote
    burstWindow:    5m    # rolling window the count spans
```

Default `5m`. Validation floors it at `pendingWait` (a window narrower
than the pool it counts is nonsense). Operators must set it above their
widest `monitors[].interval`; the docs say so in the schema, the
example config, and the operations runbook.

Ageing matters in the other direction too: four unrelated failures ten
minutes apart are four incidents, not a burst, and must keep paging
individually. The window is what separates "one outage" from "a bad
afternoon."

### Individual ownership survives promotion

`channelState` also gains `individual: set[slug]` — the monitors whose
incident the per-monitor notifier announced. `routeResolve` and
`routeReminder` consult it *before* the channel mode, so a monitor
paged individually keeps addressing its own Slack message for the rest
of its incident even after the channel promotes to group-mode. Its
parent gets its resolve edit; its reminders keep threading.

The digest correspondingly reports only what it owns. A 12-monitor
outage renders as 3 individual incidents plus a digest of 9 — honest,
and not double-counted.

## Consequences

| Scenario | Before | After |
|---|---|---|
| 12 monitors, 24s interval, one outage, `burstThreshold: 5` | 12 DOWN parents, 0 digests | 3 DOWN parents + 1 digest of 9 |
| N monitors, interval ≫ `pendingWait`, one outage | 1 message per monitor, unbounded | ≤ `burstThreshold − 1` individual + 1 digest |
| Simultaneous burst inside one `pendingWait` | 1 digest | unchanged — `len(down) ≥ len(pool)` |
| Lone monitor fails | 1 individual message | unchanged |
| 4 failures spread over 40m, `burstWindow: 5m` | 4 individual messages | unchanged — they age out, correctly |
| Individually-paged monitor recovers after the channel promoted | recovery swallowed; parent stays red | resolve edit + reply on its own message |

### Out of scope

- **Retroactively folding already-paged monitors into the digest.**
  Would need `chat.delete` of live messages; the two-tier rendering is
  honest as it stands.
- **Cross-channel coalescing.** Still deferred, as in ADR-0004.
- **Deriving `burstWindow` from the configured intervals.** A single
  documented knob with a floor is easier to reason about than an
  implicit derivation that changes when one monitor's interval changes.

## References

- ADR-0004 — burst dispatcher; this amends its promote rule
- ADR-0008 — self-health degraded mode; the layer above
- `internal/coalesce/dispatch.go` — `channelState`, `expirePending`
- `internal/scheduler/scheduler.go` — `runMonitor`, the startup jitter
- `internal/lifecycle/outage_simulation_test.go` — the `RunServe`
  simulation that reproduced the storm and now guards the bound
