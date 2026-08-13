# toggle-monitor architecture

A tour of the binary's modules, the data flow at runtime, and the
two state machines (uptime + SSL). Aimed at someone who has read
[`prd-v1.md`](internal/prd-v1.md) and wants to find their way around the
code.

> **Stale section notice (ADR-0002).** The `kube.match` design has
> been redesigned as a cascading rule tree — see
> [ADR-0002](./adr/0002-kube-match-tree-cascade.md). The ASCII
> module diagram, the `merger` description ("preset + annotation
> merge"), and the per-Ingress status table below still reference
> `kube.presets`, `kube.pause`, `kube.annotationDomain`, the
> `kube-paused` status, and per-ingress `/kube.*` / `/config.*`
> annotations — all of which were removed by ADR-0002. The body
> text is preserved here for historical context and should not be
> treated as the current implementation contract.

## High-level shape

A single Go binary running as a single Kubernetes replica. Three
concurrent concerns share one process and one Postgres database:

1. **Scheduler** — drives one goroutine per monitor, performs HTTP
   probes, runs the alert state machines, posts to Slack.
2. **Kube informer** — watches every Ingress cluster-wide, hands
   each one to the materializer, which either turns it into an
   active monitor or records a snapshot row explaining why it
   didn't.
3. **HTTP server** — serves the read-only UI, the `/healthz` and
   `/readyz` probes, and the `/metrics` Prometheus endpoint.

```
                            ┌────────────────────┐
            (YAML)          │   config.Load      │
        ConfigMap ────────▶│   line-numbered    │
                            │   multi-error      │
                            └─────────┬──────────┘
                                      │ Config struct
                                      ▼
   ┌────────────────────────────────────────────────────────────┐
   │                       lifecycle.RunServe                    │
   │                                                             │
   │   ┌─────────────┐   ┌─────────────┐   ┌─────────────────┐  │
   │   │  scheduler  │   │ kube.Watcher│   │   web.Server    │  │
   │   │ (per-mon GR)│   │  (informer) │   │ /  /metrics ... │  │
   │   └──────┬──────┘   └──────┬──────┘   └────────┬────────┘  │
   │          │                  │                   │           │
   │   ┌──────▼──────┐   ┌──────▼──────┐   ┌────────▼────────┐  │
   │   │  httpcheck  │   │   merger    │   │   store (pgx)   │  │
   │   │   sslinsp.  │   │  (preset+   │   │  monitors +     │  │
   │   │   alert SM  │   │   annot.)   │   │  alert_events + │  │
   │   └──────┬──────┘   └──────┬──────┘   │  discovery_snap │  │
   │          │                  │          └────────┬────────┘  │
   │          └────┬─────────────┘                   │           │
   │               ▼                                 ▼           │
   │     ┌──────────────────┐                ┌─────────────────┐ │
   │     │   slack.Notifier │                │   Postgres      │ │
   │     │  (Block Kit +    │                │   (CNPG)        │ │
   │     │   thread refs)   │                └─────────────────┘ │
   │     └─────────┬────────┘                                    │
   │               ▼                                             │
   │      Slack Web API                                          │
   └────────────────────────────────────────────────────────────┘
```

## Module map

Every package is `internal/<name>` unless noted. The split is by
responsibility; each module has a small public surface and is
testable in isolation.

| Package | Responsibility | Imports |
|---|---|---|
| `cmd/toggle-monitor` | `main` + cobra CLI wiring. | `cli`, all internal modules transitively via `lifecycle` |
| `cmd/toggle-monitor/internal/cli` | Subcommand definitions: `serve`, `validate`, `config show`, `migrate`. | `config`, `db`, `lifecycle`, `migrate` |
| `config` | YAML parse + validate. Anchors, `x-*` ignore, `${VAR}` interpolation, multi-error with line numbers. Custom `Duration` type. | `slug` |
| `slug` | Slug regex (`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`) + kube-discovered sanitizer. Pure functions. | — |
| `secret` | `SecretString` with slog log-masking. Used wherever a bot token / DB password is held. | — |
| `db` | pgx pool + exp-backoff startup connect. | — |
| `migrate` | golang-migrate driver over `embed.FS` SQL files. `Up` / `Check` / `LatestVersion`. | — |
| `store` | The repository over Postgres. Owns all SQL. | `alert` (canonical state types) |
| `alert` | Two pure state machines: uptime (`Apply`) + SSL (`ApplySSL`). The single source of truth for transitions. | — |
| `httpcheck` | One HTTP probe → `Result` (status code, duration, TLS info, body). | — |
| `sslinspect` | Reserved for richer cert analysis if needed; today the cert info is captured by `httpcheck` and consumed by `alert.ApplySSL`. | — |
| `kube` | `client-go` informer + reconcile loop. Calls a `Materializer` per (ingress, host); falls back to "observe-only" without one. | `store` |
| `merger` | The `Materializer` implementation. Resolves preset + annotation merge, slug sanitization, static-vs-kube collision detection. Caches a `scheduler.Plan` per kube monitor so the scheduler picks them up. | `config`, `scheduler`, `slack`, `slug`, `store` |
| `scheduler` | Per-monitor goroutines with startup jitter + in-cycle retries + `dependsOn` gating. `RunDynamic` swaps the plan set on a refresh interval. | `alert`, `httpcheck`, `store` |
| `slack` | Block Kit builders, HTTP client (`auth.test`, `chat.postMessage`, `chat.update`, `users.info`, `usergroups.list`), workspace check, notifier dispatch, userMapping validator. | `secret` |
| `heartbeat` | Outbound deadman heartbeat with stalled-worker detection. | — |
| `web` | HTTP server + handlers + `templ` UI. `/healthz`, `/readyz`, `/metrics`, the read-only pages. | `store` |
| `web/templates` | `.templ` sources + generated `_templ.go`. | `store` |
| `lifecycle` | The serve wiring. Builds every module, starts goroutines under a single `ctx`, owns the SIGTERM ordering. | every module |
| `observability` | Prometheus registry, the documented series, `Metrics` struct passed into the scheduler. | — |
| `testpg` | `//go:build integration` helper: spins Postgres via testcontainers. | — |

## Data flow: one check tick

1. The scheduler's per-monitor goroutine ticks at the configured
   `interval` (with startup jitter to avoid thundering-herd).
2. If `DependsOn` has any parent currently in `StatusDown`, the
   tick short-circuits: status flips to `temporary-paused`, no
   probe runs, no alert event lands. Done.
3. Otherwise the scheduler runs `httpcheck.Check`. In-cycle retries
   are bounded by `retries × (timeout + retryBackoff) < interval`.
4. The current `MonitorRow` is loaded from the store. Its embedded
   `alert.State` (uptime) and `alert.SSLState` (SSL) feed two pure
   state-machine calls:

   ```
   nextState, event       = alert.Apply(prev.State(),    Check{outcome, at, code, error, reminderInterval})
   nextSSL,   sslEvent    = alert.ApplySSL(prev.SSL(),   SSLCheck{at, expiresAt, isHTTPS, thresholds...})
   ```

5. `store.ApplyCheck` (and `store.ApplySSLCheck`) update the
   `monitors` row and append to `alert_events` in a single
   transaction. Thread refs are cleared on the resolve transitions.
6. If an event was emitted, the scheduler's `EventSink` (wired to
   `slack.Notifier.Notify`) dispatches the appropriate Slack call:
   - `EventOpen` → `chat.postMessage` (parent), persist the
     returned ts as the thread ref.
   - `EventReminder` → `chat.postMessage` with `thread_ts` set.
   - `EventResolve` → `chat.update` (header swap on the parent) +
     `chat.postMessage` thread reply with the downtime.
   - SSL events follow the same pattern via `NotifySSL`.

Slack errors at step 6 do **not** roll back the DB transaction from
step 5: the worker logs and continues. Retry-on-next-tick is the
implicit recovery strategy.

## Data flow: a kube reconcile pass

A pass runs on two triggers: the `kube.resyncInterval` ticker, and an
Ingress add/delete watch event debounced by `kube.watchDebounce`
(default 5s, `0s` disables). Both feed the same single goroutine, so
passes never overlap. The watch trigger is what keeps a removed
Ingress from alerting as a 404 for up to a full resync interval before
the removal is noticed.

1. The `client-go` informer's lister returns every Ingress in the
   cluster.
2. The `kube.Watcher.Reconcile` loop walks every unique `host` in
   `spec.rules[].host` per Ingress.
3. For each `(ingress, host)` pair the watcher calls
   `Materializer.Materialize`, which produces one of these outcomes
   in priority order:

   | Outcome | Snapshot status | Notes |
   |---|---|---|
   | host matches `kube.pause` | `kube-paused` | Materializes a monitor row whose status is `temporary-paused`; preserves history; in-thread closeout if there was an open incident. |
   | no `<base>/kube.preset` annotation | `kube-invalid` (`"no preset annotation"`) | No monitor created. |
   | `<base>/config.enabled=false` | `kube-invalid` (`"opt-out via config.enabled=false"`) | No monitor created. |
   | preset slug unknown | `kube-invalid` (`"unknown preset slug %q"`) | No monitor created. |
   | sanitized slug is empty | `kube-invalid` (`"slug generation failed: ..."`) | No monitor created. |
   | collides with a static monitor slug | `kube-invalid` (`"slug conflicts with static monitor"`) | Static wins; the kube ingress is skipped. |
   | otherwise | `added` | Monitor reconciled (`source = "kube"`); a `scheduler.Plan` is cached in the materializer. |

4. Each pass ends with `store.PruneDiscoverySnapshot(startedAt)`,
   which deletes rows we didn't observe this pass and returns the
   monitor slugs they pointed at.
5. The watcher's `RemovalSink` (wired by lifecycle to a soft-delete
   + Slack-notify helper) runs once per pruned monitor — same
   closeout-and-warning flow used for static removal.
6. The materializer's `Prune(startedAt)` drops in-memory plans for
   slugs that disappeared, so the scheduler's next refresh stops
   probing them.
7. The scheduler refreshes its plan set on its own cadence
   (`scheduler.RunDynamic(refresh = kube.resyncInterval)`), pulling
   the union of static plans + `materializer.CurrentPlans()`. New
   slugs spawn goroutines; removed slugs have their per-monitor
   context cancelled.

## State machines

### Uptime (`internal/alert/alert.go`)

```
                  ┌──────────────────────────────────────┐
                  │                                      │
                  ▼                                      │
                ┌───┐  fail   ┌───────┐  ok  (resolve)   │
   start ──▶  │ up │ ───────▶│ down  │ ─────────────────┘
              └───┘            └───┬───┘
                                   │
                                   │  fail + cadence ≥ reminderInterval
                                   ▼ (reminder event; LastReminderAt advances)
                              ┌───────┐
                              │ down  │
                              └───────┘
```

- `up + ok`  → no event, status stays `up`.
- `up + fail` → status becomes `down`, emits `EventOpen`, stamps
  `OpenedAt` + `LastReminderAt = OpenedAt`.
- `down + ok` → status becomes `up`, emits `EventResolve` with
  `Downtime = now - OpenedAt`.
- `down + fail` and
  `now - LastReminderAt < reminderInterval` → no event.
- `down + fail` and
  `now - LastReminderAt ≥ reminderInterval` → emits
  `EventReminder`, advances `LastReminderAt`.

In-cycle retries are the scheduler's job — by the time
`alert.Apply` sees a `Check`, the retries have already collapsed
into a single outcome.

### SSL (`internal/alert/ssl.go`)

Independent of the uptime SM. A monitor can be `up` AND
`ssl-expiring` at the same time.

```
   start ──▶ (no cert observed yet — pass-through, no transition)

   IsHTTPS=false ──▶ ssl-skipped (HTTP-only static monitors)

   TTL > sslAlertThreshold        ──▶ ok
   TTL ≤ sslAlertThreshold        ──▶ ssl-expiring (EventSSLOpen)
   TTL > sslAlertThreshold (renew)──▶ ok (EventSSLResolve)

   ssl-expiring + TTL > sslEscalationThreshold:
       cadence = sslReminderInterval
   ssl-expiring + TTL ≤ sslEscalationThreshold:
       cadence = 24h (daily reminders)
```

Reminders fire when `now - LastReminderAt ≥ cadence`.

### temporary-paused

Driven by the scheduler, not by `alert.Apply`. Any monitor whose
`dependsOn` chain has at least one down parent enters
`StatusTemporaryPaused`. The probe is skipped entirely (no HTTP,
no SSL, no `alert_event` row, no `last_*` write). On resumption the
state machine treats the prior state as `up` so a failing first
tick produces a clean `EventOpen` rather than a double transition.

## Single-replica rationale

The worker is a single in-process loop on purpose:

- Avoids partitioning the monitor set across replicas — keeps the
  data model and the metrics simple.
- Avoids leader election complexity. ~200–500 monitors is the
  target scale; a single replica handles that easily.
- The Deployment uses `strategy: Recreate` so the old worker fully
  exits before the new one starts — no two workers race over the
  same monitor row.

If horizontal scale-out ever becomes load-bearing, the planned
sharding axis is **group** (each replica owns a subset of groups).

## Soft-delete and reconcile

Static and kube monitors share a soft-delete shape:

- Removal **doesn't** delete rows. The `monitors` row gets
  `archived = TRUE`, `archived_at`, `archive_reason` (e.g.
  `"removed from config"` or `"kube ingress removed"`).
- History (`alert_events`) is preserved indefinitely.
- Slug reuse on a future reconcile resurrects the row
  (`ReconcileMonitor` clears the archive flags).
- On removal, the Slack notifier dispatches:
  - An in-thread closeout reply + parent edit IF the monitor had an
    open uptime incident (`uptime_thread_ts` is set).
  - A non-threaded `⚠️ Monitor removed` warning to the
    last-known channel (resolved via the persisted
    `slack_channel_slug` against the current YAML).

Slack delivery failures during removal log and continue; the
DB-side soft-delete is the source of truth.

## Where the secrets live

Two secrets cross the process boundary:

- **DB password** — read from the env var named by
  `database.passwordEnv`. The Deployment maps it from a Secret via
  `valueFrom.secretKeyRef`.
- **Slack bot token(s)** — read from the env var named by each
  `slack.channels[].tokenEnv`. Same `secretKeyRef` pattern.

Both flow through `secret.SecretString`, which masks the value in
slog output (`SU****RD` for length ≥ 8, `****` below). The raw
value is only exposed via the explicit `Reveal()` method, which is
called in three places: DSN construction, HTTP `Authorization`
header, and the workspace-check call.

## Observability touchpoints

- `/metrics` exposes the Prometheus series documented in
  [`docs/operations.md`](operations.md#metrics).
- `/healthz` (process + listener alive) and `/readyz` (DB connected
  + config loaded once) drive Kubernetes probes.
- Optional outbound heartbeat (`heartbeat:` block in config) POSTs
  `{openIncidents, lastTickAt}` every `interval` to any
  healthchecks.io-compatible URL; switches to `{url}/fail` when the
  worker is stalled.
- The web layer's homepage surfaces flagged `slack.userMapping`
  entries via a panel that's quiet when everything's healthy.

## See also

- [PRD](internal/prd-v1.md) — the v1 problem statement and user stories.
- [Design decisions](internal/design-decisions.md) — every locked choice.
- [Config schema](config-schema.md) — per-field reference.
- [ADRs](adr/) — bootstrap dependencies + tooling.
- [Operations](operations.md) — endpoints, metrics, log format,
  troubleshooting.
