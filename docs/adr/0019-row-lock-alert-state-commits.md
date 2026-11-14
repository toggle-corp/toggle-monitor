---
status: accepted
date: 2026-11-14
deciders: [monitoring-team]
---

# ADR 0019 — Alert state commits lock the monitor row

**Status:** Accepted
**Date:** 2026-11-14
**Relates to:** ADR-0004 (on-demand parent probe), ADR-0008
(self-health commit), ADR-0018 (per-monitor delivery lock).

## Context

Three writers commit a monitor's uptime state (`alert.Apply` then a
`monitors` update plus an `alert_events` row):

| Writer | Goroutine |
|---|---|
| Scheduler tick | the monitor's own loop |
| On-demand parent probe | dispatcher evaluator |
| Self-health commit | self-health evaluator |

Each read the row, applied its check, then wrote the result in a
separate statement. A writer that committed in between was lost. The
common case: the on-demand probe opens an incident (Slack DOWN posted)
between the tick's read and its write, and the tick writes its OK
result as plain "up" with no resolve. The monitor shows up, the
incident never resolves, the DOWN is never edited and its open
`alert_events` row has no resolve. The reverse also happened: a
self-health commit read the row, the tick resolved it, and the
self-health write put the stale incident back.

The deployment runs one replica (`replicas: 1`, `strategy: Recreate`),
so an in-process lock per slug would also work.

## Decision

`store.CommitCheck` runs the read, the decision and the write in one
transaction. It reads the row with `SELECT … FOR UPDATE`, passes it to
a caller-supplied `decide` function, and writes what `decide` returns.
Every alert state writer goes through it. `decide` is pure: it never
blocks, calls Slack or touches the database.

- The tick probes without any lock, then applies its result to the row
  as it is under the lock. It stamps the check there too, so a resolve
  never predates the incident it closes.
- The tick also reads its dependsOn parents' statuses inside that
  transaction, after the lock. While one holds the child, the tick
  writes nothing, failure or success, and ends as a paused tick. A
  parent can go down and push-propagation pause the child after the
  tick's own parent checks. Without this read the tick would take that
  pause for one to resume from: open the child and announce it, or
  release a paused intermediate parent on a successful probe.
- The on-demand probe and the self-health commit only ever open an
  incident. Under the lock they write nothing unless the monitor is up,
  not archived, and has had no check committed since they read it
  before their probe. A monitor already down keeps its incident, a
  paused one stays silent behind its parent, and a newer check
  supersedes their older result. They read the monitor's dependsOn
  parents under the lock too: while one holds the monitor, they write
  nothing, even before push-propagation has paused it. The on-demand
  probe then runs push-propagation from that held parent, as it does
  for a paused one, so its pooled children drain.
- Sinks and the dispatcher are called after the commit, with the row
  `decide` saw.

We chose the row lock over an in-process lock because it costs about
the same, needs no lock registry shared between the scheduler and
lifecycle, and stays correct if two processes ever write the same
database. It adds no in-process lock, so it adds no lock order to
reason about.

## Consequences

- The row lock is held for one short transaction. The notifier's
  `SetUptimeThread` and `MarkUptimeOpenFailed`, and push-propagation's
  `MarkTemporaryPaused`, wait for it like any other update of the row.
- The parent rows are read without a lock, so a tick never holds one
  row lock while waiting on another. Push-propagation pauses a child
  with a single update after its parent's Open committed: if the pause
  lands before the child's lock, the parent read after it sees the
  parent down; if after, the child's commit came first and the pause
  then holds it.
- Lock order: the notifier takes its per-monitor delivery lock
  (ADR-0018), then the row lock for its thread ref write. Writers take
  the row lock and release it before any sink call. Nothing takes the
  delivery lock while holding the row lock, so the two cannot deadlock.
- An on-demand or self-health failure that a newer tick check
  supersedes is dropped. The monitor's next failing tick opens the
  incident.
- The self-health commit no longer emits reminders for a monitor
  already down. Its own ticks do.

## References

- `internal/store/store.go`: `CommitCheck`
- `internal/scheduler/scheduler.go`: `Tick`
- `internal/lifecycle/coalesce_wiring.go`: `commitExtraFailure`
- `internal/lifecycle/commit_race_integration_test.go`
