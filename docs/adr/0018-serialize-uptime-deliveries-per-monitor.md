---
status: accepted
date: 2026-11-14
deciders: [monitoring-team]
---

# ADR 0018 — A recovery waits for its monitor's Open delivery

**Status:** Accepted
**Date:** 2026-11-14
**Relates to:** ADR-0017 (orphan resolves), whose amendment makes a
recovery with no thread ts and no failed-delivery mark post nothing.
ADR-0004 (burst dispatcher), whose evaluator flushes pending Opens.

## Context

Every sink call gets a row snapshot that the caller read before its own
transition committed. A recovery decides what to post from that
snapshot: edit the parent (thread ts), post a late-notice resolve
(failed-delivery mark), or post nothing (neither).

An Open can still be in flight when its recovery is routed. The Slack
client retries for up to `DefaultRetryBudget` (10s), and the paths that
deliver one monitor's events run on different goroutines:

| Delivers | Goroutine |
|---|---|
| Recovery, reminder, critical Open | the monitor's scheduler tick |
| Non-critical Open (pending pool flush) | dispatcher evaluator |
| Critical parent Open found by the on-demand probe | dispatcher evaluator |
| Critical Open committed by self-health | self-health evaluator |

A monitor's own ticks run one after another, but nothing orders them
against the two evaluators. So a recovery can read its row before the
Open's post finishes. It sees no ts and no mark, posts nothing, and the
DOWN that lands a moment later is never edited. The Open's
`SetUptimeThread` can also land after the recovery cleared the ref,
leaving a thread ts on an up monitor for the next incident to edit.

## Decision

The notifier orders uptime deliveries per monitor and remembers each
incident's parent message.

- `Notifier.Notify` holds a per-monitor lock for the whole delivery, so
  a recovery that arrives while its Open is being posted waits for it.
- The notifier records, per monitor, the incident (keyed by `OpenedAt`)
  and what its parent post produced: thread ts, or failure. A reminder
  or recovery whose snapshot has no ts takes it from that record.
- A recovery that finds nothing posts nothing, as before, and records
  the incident as resolved. An Open for an incident already resolved or
  already posted then posts nothing. That covers a recovery that wins
  the lock before its Open does.
- The dispatcher hands the recovery of a monitor whose Open it flushed
  to the sink even when the row has no ts and no mark, instead of
  dropping it. The notifier decides with the outcome in hand.
- `SetUptimeThread` and `MarkUptimeOpenFailed` take the incident's
  `OpenedAt` and only write while the row's `opened_at` still matches
  it. A delivery that finishes after its incident resolved writes
  nothing, even once a later incident has opened.

We chose this over passing the thread ref back through the sinks. The
dispatcher's sink returns only an error, and the scheduler and three
other callers share its signature. Keeping the record next to the code
that posts the parent changes no signature and covers all four paths.

## Consequences

| Scenario | Before | After |
|---|---|---|
| Recovery routed while its Open is still posting | DOWN posted, never edited | DOWN posted, then edited to UP with a reply |
| Recovery delivered before its Open's delivery starts | DOWN posted, never edited | nothing posted |
| Open's ref stored after its recovery cleared it | stale ts on an up monitor | not stored |
| Open's ref or mark written after a later incident opened | lands on the later incident | not stored |
| Reminder with a snapshot older than the stored ref | second DOWN parent | reply in the Open's thread |

- A recovery can wait up to one Slack retry budget behind its Open.
  That delays only this monitor's next check.
- The record is in memory. After a restart, recoveries rely on the row
  again, as before.
- A recovery that waited behind a failed Open posts its standalone
  late-notice resolve directly instead of joining the orphan pool.

## References

- `internal/slack/notifier.go`: `Notify`, `monitorDelivery`
- `internal/coalesce/dispatch.go`: `routeResolve`
- `internal/store/store.go`: `SetUptimeThread`
- `internal/slack/notifier_inflight_test.go`
