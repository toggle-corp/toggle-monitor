---
status: accepted
date: 2026-11-14
deciders: [monitoring-team]
---

# ADR 0017 — Recoveries whose Open never reached Slack post as one message

**Status:** Accepted
**Date:** 2026-11-14
**Relates to:** ADR-0004 (burst dispatcher): adds a recovery-side pool.
ADR-0008 (self-health degraded mode): closes part of its "Recovery-path
coalescing" out-of-scope item. ADR-0015 (cumulative burst window): reuses
`burstWindow` for the same trickle.

## Context

When Slack is unreachable while a monitor's Open flushes, no uptime
thread ts is stored. If the monitor recovers before a reminder posts a
fresh parent, `Notifier.notifyResolve` has nothing to edit and posts a
standalone "is UP" message with the late-notice banner. After an outage
that also took Slack down, the channel gets one such message per monitor.

The dispatcher routed every resolve straight to the per-monitor sink. It
never counted resolves toward a burst and did not know the Open had
failed. The scheduler does hand it the pre-commit row, though, and that
row's `UptimeThreadTS` is empty for exactly these monitors.

Recoveries trickle in just as failures do: each monitor's next tick sits
at its own jittered phase, so the end of an outage reaches the dispatcher
spread over roughly one probe interval (ADR-0015).

## Decision

`routeResolve` sends a recovery bound for the per-monitor sink to a
per-channel **orphan pool** when the row has no uptime thread ts. That
covers individually-owned monitors, individual-mode, and group-mode
monitors the digest does not own. A recovery with a ts keeps today's path.

- The first orphan arms a due time of `resolveAt + burstWindow`. Later
  orphans join that window without extending it.
- `evaluateAll` drains due pools. One entry goes through the sink
  unchanged, which posts the standalone banner resolve. Two or more post
  **one** message through `Poster.PostDigest`: the closed digest shape,
  one struck-through row per monitor with its detail link, the banner
  `⚠️ Initial notification delivery failed. These monitors are back up.`,
  and a downtime from the earliest `OpenedAt` to the latest recovery. No
  mentions, no thread, no incident group, no reminders.
- A new Open, a dependsOn pause, or a removal of a pooled monitor drops
  it from the pool, matching what the pending pool does. Pools due on
  the same pass post one channel at a time, each taken just before its
  post, so a pool waiting behind another channel's post can still lose
  entries.
- The pool is in-memory. A restart loses it. Those monitors are up in the
  DB and their Open never reached Slack, so the channel never hears of
  that incident.

The window is `burstWindow`, not `pendingWait`. A window anchored to the
first recovery has to span the trickle, and `burstWindow` is the knob
operators already size above their widest interval. The latency lands
only on recoveries nobody was paged for.

## Consequences

| Scenario | Before | After |
|---|---|---|
| 4 monitors down with Slack down, all recover before a reminder | 4 standalone "is UP" posts | 1 grouped recovery message |
| One monitor's Open failed (Slack blip), later recovers | standalone resolve at once | standalone resolve after `burstWindow` |
| Recovery of a monitor whose Open reached Slack | parent edit + reply | unchanged |
| Pooled monitor fails again before the flush | n/a | dropped from the pool |

### Out of scope

- **Fresh-parent reminders.** A monitor still down when Slack returns
  posts a fresh parent on its next reminder, one per monitor, as before.
- **Critical monitors and SSL.** They bypass the dispatcher and keep
  posting standalone resolves.
- **Cross-channel grouping.** Still deferred, as in ADR-0004.

## Amendment (2026-11-14): only failed deliveries are orphans

A missing thread ts alone does not mean the Open failed. A `dependsOn`
child that fails, sits in the pending pool and is paused out of it by
push-propagation never had a message sent. Treating its recovery as an
orphan posted "Initial notification delivery failed" for a page nobody
was owed. In an estate where every monitor `dependsOn` one root, that
was every child.

- The notifier sets `monitors.uptime_open_failed_at` when an uptime
  parent post fails (Open, or a reminder's fresh parent), or when a
  delivered parent's thread ts still cannot be stored after a short
  retry. Storing a thread ts and resolving clear it.
- A recovery with the mark whose parent this process did post, its ts
  only held in the notifier's delivery record (ADR-0018), goes to the
  notifier, which edits that parent. The dispatcher asks the notifier
  before it pools a marked recovery as an orphan.
- A recovery with no ts **and** the mark is an orphan, as above. With no
  ts and no mark, nobody was paged: the dispatcher drops it (no pool, no
  sink) and `Notifier.Notify` posts nothing for it, which also covers
  critical monitors. A monitor whose Open the dispatcher already
  flushed is the exception: its row can predate the post, so the
  recovery goes to the notifier, which knows the outcome (ADR-0018).
- A reminder with no ts and no mark posts a plain Down parent: it is the
  monitor's first announcement, so it carries no banner and does not
  count as a fresh-parent fallback. This is how a child resumed still
  down after its parent recovered gets paged.
- In pending-mode, a recovery of a monitor outside the pool goes to the
  per-monitor path instead of being dropped, so a parent first posted by
  a reminder gets its resolve edit.

SSL is unchanged. SSL events skip the dispatcher, and a paused monitor
is not probed so it emits none: every SSL Open is sent.

Incidents open across the upgrade carry no mark, so the recovery of one
whose Open had already failed is silent.

## References

- `internal/coalesce/dispatch.go`: `routeResolve`, `resolveIndividually`,
  `flushOrphans`
- `internal/coalesce/manager.go`: `evaluateAll`, `buildOrphanParent`
- `internal/slack/notifier.go`: `notifyResolve`, `OrphanResolvesBanner`
- `internal/lifecycle/outage_simulation_test.go`:
  `TestOutageSimulation_orphanResolvesGroupIntoOneMessage`,
  `TestOutageSimulation_dependsOnTreeAnnouncesOnlyTheRoot`
- `internal/migrate/migrations/0014_uptime_open_failed.up.sql`
