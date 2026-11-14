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
- The on-demand probe and the self-health commit only ever open an
  incident. Under the lock they write nothing unless the monitor is up,
  not archived, and has had no check committed since they read it
  before their probe. A monitor already down keeps its incident, a
  paused one stays silent behind its parent, and a newer check
  supersedes their older result.
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
