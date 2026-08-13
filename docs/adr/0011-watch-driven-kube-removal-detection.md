---
status: accepted
date: 2026-08-13
deciders: [monitoring-team]
---

# ADR 0011 — Watch-driven kube removal detection, debounced

**Status:** Accepted
**Date:** 2026-08-13

## Context

Deleting an Ingress produced two Slack messages in the wrong order: a
false incident, then the truth.

1. `t+0` — the Ingress is deleted. DNS, or the ingress controller's
   default backend, starts answering the monitor's probe with a 404
   (or 502/503, or NXDOMAIN — it depends on the controller and whether
   the DNS record outlives the Ingress).
2. `t+interval` — the next probe fails. `alert.Apply` opens an incident
   on the **first** failing tick (`internal/alert/alert.go`), and the
   burst dispatcher posts it one `pendingWait` later (default 30s,
   `internal/coalesce/dispatch.go`).
3. `t+up to 30m` — the reconcile ticker finally runs. The observed-set
   prune notices the tuple is gone, `RemovalSink` soft-deletes the
   monitor and posts the removal notice.

So the operator is paged for a service that was deliberately removed,
and the correction arrives minutes later. The timing asymmetry is the
whole bug: incidents are detected on a per-probe cadence, removals on
`kube.resyncInterval` (default `30m`).

The information needed to prevent it was already in the process. The
watcher's lister is backed by a `client-go` SharedIndexInformer whose
cache receives the watch DELETE within seconds — nothing was looking at
it until the ticker fired.

## Considered Options

1. **Suppress on status code.** When a probe returns 404, check whether
   the Ingress still exists before alerting.
2. **Gate at dispatch.** At `pendingWait` expiry, drop pool entries
   whose monitor is no longer materialized, next to the existing
   on-demand parent probe.
3. **Targeted teardown from the event.** The DeleteFunc handler reads
   the deleted object's hosts and tears down exactly those snapshot
   rows via a new store method.
4. **Nudge the existing reconcile, debounced.** Informer events ask
   `Run` to reconcile after a short window; the pass itself is
   unchanged.

Option 1 keys on the wrong signal. A removed Ingress does not reliably
present as 404, and a live Ingress serving a real 404 is a regression
worth alerting on — the check would both miss cases and suppress true
positives.

Option 2 treats the symptom while the several-minute detection gap
stays. It remains attractive *after* the gap closes, as a guard for the
residual race, but on its own it means the dispatcher is asking about
state that is still stale.

Option 3 is the cheapest per event (`O(hosts)` instead of `O(cluster)`)
but introduces a second teardown path that has to stay consistent with
the prune path forever, plus tombstone unwrapping
(`cache.DeletedFinalStateUnknown` carries a possibly-stale object).

## Decision

Adopt option 4. Ingress `Add`/`Delete` events call `Watcher.Nudge()`,
which asks `Run` to reconcile once `kube.watchDebounce` (default `5s`,
range `1s`–`1m`, `0s` disables) has elapsed. Removal teardown therefore
lands inside the dispatcher's 30s `pendingWait`, and the operator gets
one message: the removal notice.

The design constraint was that the new trigger must add no new failure
mode and no meaningful load. What each choice buys:

- **Nudge, not act.** Handlers only do a non-blocking send into a
  capacity-1 channel. Informer events are delivered to a handler
  sequentially, so any real work there would stall delivery for
  everything else on that informer. Nothing is read off the event
  object, so tombstones need no special handling.
- **One goroutine.** Reconcile still runs only on `Run`'s goroutine, so
  the pass keeps its existing invariants (observed-set prune,
  vacated-slug routing, the empty-list guard against a transiently
  empty cache) with no locking and no tick/nudge overlap.
- **Trailing debounce, anchored to the first event.** A burst — a
  rolling deploy, or a watch reconnect replaying deletes — collapses
  into one pass, capping the added rate at one reconcile per window
  regardless of event volume. Later events join the open window rather
  than extending it, so a busy cluster cannot starve the pass.
- **The debounce is also a settle window.** The pass re-lists the cache
  when the window expires instead of acting on the event that woke it,
  so a delete immediately followed by a recreate (a `helm apply`
  replacing an Ingress) changes nothing.
- **`Add` is handled too.** Without it, a slower delete→recreate would
  tear the monitor down and leave it archived until the next tick — a
  new hole in exchange for closing the old one. `ReconcileMonitor`
  already clears `archived` on conflict, so a recreate heals itself.
- **`Update` is not handled.** The informer emits one Update per object
  per resync, so subscribing would let a resync trigger reconciles. A
  host change already reaches teardown through the vacated-slug path.
- **Startup replay is dropped.** Handlers are registered after the
  factory syncs, which makes client-go replay the cache as synthetic
  Adds; `ResourceEventHandlerDetailedFuncs` marks them
  `isInInitialList` and they are ignored, so startup does not queue a
  redundant pass behind `Run`'s own first reconcile.
- **No added apiserver load.** `Reconcile` lists from the informer
  cache, never the API. The added cost is Postgres only — the same
  per-host upserts and single prune that already ran per resync, just
  more often, bounded by the debounce.

The resync ticker is unchanged and remains the backstop, so the watch
path is purely additive: `watchDebounce: 0s` reverts to the previous
behaviour by config alone.

## Consequences

**An intentional deletion no longer pages anyone.** That is the point,
but it is a real trade: `kubectl delete ingress` against the wrong
context now produces a removal notice rather than an incident. The
notice must stay prominent enough to carry that weight; if it is ever
softened, this decision gets quieter in a way nobody asked for.

**A residual race remains.** A probe that fails and expires its
`pendingWait` inside the debounce window can still post before the
teardown. Option 2 (drop archived monitors from the pending pool at
expiry) closes it and is cheap now that detection is fast; it is
deliberately left as follow-up rather than folded in here.

**Reconcile frequency is now cluster-driven.** A cluster with constant
Ingress churn reconciles up to once per `watchDebounce` instead of once
per `resyncInterval`. The pass is bounded (`O(ingress × host)` upserts
against Postgres) and the window is the throttle, but the reconcile log
line now carries a `trigger` field (`startup`/`ticker`/`watch`/`manual`)
so a nudge storm is visible rather than inferred.

**Teardown may run twice for one deletion** — once from the nudged pass,
once from a later tick. `kubeRemovalSink.OnKubeMonitorRemoved` already
early-returns on an archived monitor, so the second is a no-op and no
duplicate Slack message is posted. That guard is now load-bearing.

**A handler panic would kill the shared processor** and leave the
watcher blind to events while still ticking. Both handlers recover via
`tmsentry.RecoverPanic`, matching what the reconcile wrapper already
does.

## References

- `internal/kube/kube.go` — `Nudge`, `Run`'s debounce arm,
  `watchIngressEvents`
- `internal/kube/nudge_test.go` — burst collapse, settle window,
  disabled-by-config
- [ADR 0002 — `kube.match` as a cascading rule tree](./0002-kube-match-tree-cascade.md)
  — the reconcile pass this trigger feeds
- [ADR 0004 — burst dispatch supersedes always-coalesce](./0004-burst-dispatch-supersedes-always-coalesce.md)
  — `pendingWait`, the window teardown now beats
