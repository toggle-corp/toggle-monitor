# ADR 0008 — Self-health degraded mode (blind-monitor storm)

**Status:** Accepted
**Date:** 2026-07-24
**Relates to:** ADR-0004 (burst dispatcher). Does not supersede it —
adds a layer *above* the dispatcher that decides whether a probe
failure is signal at all, before the dispatcher ever sees it.

## Context

A cluster-internal DNS/network outage takes out the monitor's own
resolver:

```
⚠️ Initial notification delivery failed. This alert is current.
Get "https://web-app-2.example.test/": dial tcp: lookup
web-app-2.example.test on 10.96.0.10:53: read udp
10.244.0.57:35114->10.96.0.10:53: i/o timeout
```

When kube-dns (`10.96.0.10:53`) is unreachable, *every* probe fails
with a DNS-resolution error and Slack delivery fails too (Slack also
needs DNS). The observed result was N per-monitor "fresh parent"
messages — one per monitor — instead of a single notice.

Two dispatch paths, only one is outage-resilient:

- **Group path** (`internal/coalesce/manager.go`) self-heals. A failed
  `PostDigest` leaves `DigestTS==""` (`postDigest`), the group is still
  persisted via `SaveIncidentGroup`, and `ensurePosted` re-posts one
  digest on the next heartbeat once Slack returns.
- **Individual / sub-threshold path** does not. Each monitor's
  `EventOpen` flushes through `flushSink` → notifier, the post fails
  (no `ts` persisted), and later each monitor's `EventReminder` finds
  no parent `ts` and posts a fresh parent. N monitors → N fresh
  parents. `routeReminder` (`dispatch.go`) never pools, so there is no
  coalescing on the recovery path.

But the deeper problem is upstream of dispatch: a DNS-resolution
failure to the monitor's own resolver is **one fact about the
monitor** (it went blind), not N facts about N services. The services
may be perfectly healthy. Treating blindness as N service outages
pages the wrong people under a wrong mental model, and no amount of
recovery-path coalescing makes "N services down" *true*.

The insight (from the 2026-07-24 `/grilling` session): fix the cause,
not the symptom. Detect self-isolation and emit *one* self-health
notice; do not manufacture per-service outages the monitor cannot
actually observe.

## Decision

### Trigger — DNS-keyed, zero-success-vetoed ratio

A new `probe.Result.FailKind` enum classifies each failure at the
prober (which still holds the real error chain), not by string-matching
the flattened `Error`:

```
FailKind: none | dns | dial | tls | timeout | http
```

Each `Prober` sets it via `errors.As(*net.DNSError)` and friends. The
neutral `probe.Result` carries it so the scheduler/aggregator never
re-parses an error string.

**Enter degraded** ⟺ within a rolling window `W`:

1. ≥ `N_min` distinct monitors reported `FailKind==dns`, **and**
2. zero probes succeeded in `W`.

Each clause earns its place:

- **DNS-class key** separates "I'm blind" from "targets genuinely
  down." A real total outage yields `connection refused` / dial
  timeouts *after* resolution — `Code==0` but `FailKind != dns` — so it
  does **not** trip degraded mode; the burst dispatcher handles it as
  one real grouped incident.
- **Zero-success veto** — if the monitor can resolve+reach even one
  target, it is not network-isolated; do not suppress. Near-eliminates
  false positives.
- **`N_min` floor** stops a 1–2 monitor deployment inferring global
  blindness off a single flaky lookup.

**Known gap (out of scope for v1):** egress loss *without* DNS loss
(DNS cached, all dials time out) is `FailKind==dial`, not `dns`, so it
does not trip. Broadening to `dial` risks the total-outage false
positive; kube-dns failure is the dominant real case. Documented, not
handled.

### Mechanics — defer-and-decide (D2)

A `FailKind==dns` tick does **not** call `alert.Apply`. It is held
*provisional* in a shared aggregator: no DB write, no dispatch. This
mirrors the existing precedent that a SIGTERM mid-probe is "not signal
about the monitored service" (`scheduler.go`, the `ctx.Err()` early
return) rather than a failure.

A central evaluator (the `Manager.RunEvaluator` cadence, or a dedicated
self-health loop) decides once per `W`:

- **tripped** → discard the provisionals. `alert.Apply` never runs; DB
  untouched; monitors stay `up`. Fully silent, no phantom incident
  history.
- **not tripped** (isolated single-target DNS blip) → commit: run
  `alert.Apply` and route the one monitor as a normal `EventOpen`
  (~`W` late). A genuine broken DNS record still pages.

This deliberately sidesteps pending-pool retraction: because a DNS
failure is intercepted *before* routing, it never enters the
dispatcher's pending pool, so there is nothing to purge on trip. The
dispatcher's pool only ever holds real, committed failures.

Cost: `W` becomes a uniform added latency (~90s) before a DNS failure
can page. Negligible against probe intervals + retries, and it unifies
"burst-collection wait" with "am-I-blind wait."

**Freeze is uniform — critical monitors included.** `monitors[].critical`
governs *burst-grouping* (immediate per-monitor page, bypass
dispatcher), not *"is this even signal."* A DNS-resolution failure
while blind carries zero information about a critical service; letting
it page produces a confident-but-false critical alert at the worst
moment. Critical monitors keep their bypass for real signal —
application errors (`Code!=0`, `FailKind==http`) during degraded mode,
and anything at all once connectivity returns.

### Notice — one self-health incident

Posted to `selfHealth.channel` (fallback: a configured default alert
channel; if neither set → metric + log only, no Slack). **Not** fanned
out to per-service channels — when blind we do not even know which
services are truly affected, and fanout re-creates the storm.

Digest-style lifecycle with `ensurePosted`-style self-heal. Because
Slack usually needs DNS too, the notice normally cannot be delivered
until connectivity returns, at which point it lands as a single
post-hoc summary:

- open: `⚠️ Monitoring degraded — lost connectivity at T; probe
  results suppressed`
- close: `✅ Connectivity restored — blind for Xm, N checks
  suppressed, resuming`

Optional `selfHealth.mention` for on-call escalation.

### Dead-man's-switch — Prometheus, no new dependency

The notice is best-effort. The authoritative "the monitor can't tell
anyone" signal is Prometheus, which scrapes pods **by IP** (k8s service
discovery) and therefore lives *outside* the DNS failure domain the
notice cannot escape.

Always emit, independent of Slack:

- `toggle_monitor_self_degraded` gauge (1 while blind, 0 otherwise)
- a self-degraded entry counter

Documented alert rules (both failure modes):

- `toggle_monitor_self_degraded == 1` → pod alive but blind.
- `absent(up{job="toggle-monitor"})` or stale
  `toggle_monitor_worker_last_tick_seconds` → pod dead / unscrapeable /
  totally partitioned.

A push-heartbeat to a third party (healthchecks.io, Dead Man's Snitch)
is documented as an option for Prometheus-less operators but not built
— it would itself need egress the outage may have killed.

### Exit — success-driven, hysteresis, discard-don't-replay

We keep probing while degraded (freeze, not skip), so a *successful*
probe is the recovery signal.

**Exit** ⟺ the evaluator sees ≥1 success in the recent window **and**
DNS-failure count < `N_min`, subject to a **minimum dwell** (stay
degraded ≥ one full `W`) and, optionally, clean state on two
consecutive evaluator passes.

On exit: close the self-health incident, resume normal `alert.Apply`,
and **discard all deferred DNS provisionals — never replay**. Whatever
is genuinely still down re-asserts on each monitor's next normal tick;
a real burst there groups via the existing dispatcher. Not replaying
the suppressed window is what keeps recovery quiet.

### Config (`selfHealth`)

```yaml
selfHealth:
  window:      90s        # W — rolling detection/decision window
  minMonitors:   3        # N_min — distinct DNS-failing monitors to trip
  channel:  ops-health    # self-health incident channel; empty → metric+log only
  mention:  ops-team      # optional @mention on the degraded notice
```

Omitting the whole block disables the feature (individual DNS failures
commit immediately, as today). `minMonitors < 2` is rejected
(pathological).

## Consequences

### Behavior (illustrative)

| Scenario | Before | After |
|---|---|---|
| kube-dns dies; 40 monitors DNS-fail; Slack unreachable | 40 fresh-parent messages on recovery | 1 self-health notice (post-hoc summary); zero phantom incidents |
| One target's A record deleted (single DNS failure) | 1 alert | 1 alert, ~`W` later (committed, not tripped) |
| Real total outage — all targets `connection refused`, DNS fine | 1 digest (burst) or ≤N individual | unchanged — `FailKind != dns`, degraded not tripped |
| Critical monitor DNS-fails while blind | immediate false critical page | frozen; covered by self-health notice `@mention` |
| Slack-only outage (probes fine, Slack down) | group self-heals; ≤`burstThreshold−1` fresh-parents/channel | unchanged (see below) |

### Out of scope

- **Recovery-path coalescing.** D2 removes the storm from the
  network-blind case. The only residual is a pure Slack-only outage,
  where the sub-threshold path can still emit ≤ `burstThreshold−1` late
  fresh-parents *per channel* — bounded below the operator's own burst
  line, and cross-channel coalescing was already deferred in ADR-0004.
  Documented in `operations.md`; not fixed here.
- **Egress-loss-without-DNS-loss detection** (`FailKind==dial` storm).
- **Push-heartbeat dead-man's-switch.**
- **Per-resolver / partial-isolation** nuance.

## References

- `MEMORY.md` → `project-toggle-monitor-design-grilling`
- ADR-0004 — burst dispatcher (the layer this sits above)
- `internal/probe/probe.go` — `Result`; gains `FailKind`
- `internal/scheduler/scheduler.go` — `Tick`; the SIGTERM
  "not-signal" precedent this generalizes
- `internal/coalesce/manager.go` — `RunEvaluator`, `ensurePosted`
- 2026-07-24 `/grilling` session — the full design conversation
