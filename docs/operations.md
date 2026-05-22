# toggle-monitor operations guide

Practical reference for running toggle-monitor in production. Aimed
at the SRE on-call who needs to know "what does this endpoint do",
"what does this metric mean", and "why is X happening".

If you're deploying for the first time, start with
[`deploy/helm/toggle-monitor/README.md`](../deploy/helm/toggle-monitor/README.md);
this doc assumes the chart is already installed.

## Endpoints

The binary exposes a single HTTP listener on port `:8080`. Routes:

| Path | Auth | What it returns |
|---|---|---|
| `GET /` | none | Homepage: up/down/paused/ssl stat tiles, latest alerts (paginated), Slack mapping health panel (only when something's flagged). |
| `GET /monitors` | none | Paginated monitor listing with filters: `?q=`, `?status=`, `?group=`, `?archived=true`, `?page=`, `?per_page=` (capped at `ui.maxPerPage`). |
| `GET /monitor/{slug}` | none | Per-monitor detail: current state, last error, alert history, SSL info, gating-parent links when `temporary-paused`. |
| `GET /group/{slug}` | none | Per-group monitor listing. |
| `GET /discovery` | none | Auto-discovery snapshot: every observed Ingress with status + reason + materialized monitor link. |
| `GET /discovery/{ns}/{name}/{host}` | none | Per-ingress detail: raw annotation set, resolved preset, materialization outcome. |
| `GET /healthz` | none | `200 ok` while the listener is up. Used by the Kubernetes liveness probe. |
| `GET /readyz` | none | `200 ready` only after Postgres connected + config loaded. `503 not ready` otherwise. Used by the readiness probe. |
| `GET /metrics` | none | Prometheus exposition format. See [Metrics](#metrics) below. |
| `GET /static/...` | none | Embedded CSS + assets. |

There is **no authentication** in v1. Restrict access via:
- The Ingress's network policy (e.g. `nginx.ingress.kubernetes.io/whitelist-source-range`).
- A private LoadBalancer.
- `kubectl port-forward` for ad-hoc access.

## Metrics

`/metrics` is the Prometheus exposition. Series:

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `toggle_monitor_checks_total` | counter | `monitor`, `status="ok|fail|paused"` | Probes performed per monitor, partitioned by outcome. `paused` covers the dependsOn short-circuit. |
| `toggle_monitor_check_duration_seconds` | histogram | `monitor` | Wall-clock duration of each probe per monitor. |
| `toggle_monitor_active_incidents` | gauge | `type="uptime|ssl"`, `monitor` | `1` while an incident is open, `0` while resolved. |
| `toggle_monitor_config_load_total` | counter | `result="success|fail"` | Registered; **not yet incremented in v0.1**. Tracked as a follow-up. |
| `toggle_monitor_slack_post_total` | counter | `result="success|fail"` | Registered; **not yet incremented in v0.1**. Tracked as a follow-up. |
| `toggle_monitor_ingress_reconcile_total` | counter | `result="added|skipped|removed"` | Registered; **not yet incremented in v0.1**. Tracked as a follow-up. |
| `toggle_monitor_worker_last_tick_seconds` | gauge | — | Unix time of the most recent check completion (success or failure). Drives the heartbeat liveness criterion. |

Plus the Go runtime + process collectors via
`promhttp` (`go_goroutines`, `go_gc_*`, `process_*`, etc.).

### Useful PromQL

```promql
# Down monitors right now:
sum(toggle_monitor_active_incidents{type="uptime"})

# 5m error rate per monitor:
sum(rate(toggle_monitor_checks_total{status="fail"}[5m])) by (monitor)

# Worker liveness: alert if no tick in 6 min.
time() - toggle_monitor_worker_last_tick_seconds > 360
```

## Logs

Structured JSON via stdlib `slog`. One line per log entry.

```json
{"time":"2026-05-22T02:30:39Z","level":"INFO","msg":"http server listening","addr":"[::]:8080"}
{"time":"2026-05-22T02:30:41Z","level":"INFO","msg":"monitor removed from config (soft-deleted)","slug":"legacy-api","was_status":"up"}
{"time":"2026-05-22T02:30:42Z","level":"WARN","msg":"reminder skipped: no parent thread ref","monitor":"api"}
```

Log levels:

- `INFO` (default): state transitions, Slack post outcomes, config
  load summary, ingress reconcile summary, startup + shutdown.
- `WARN`: best-effort failures the worker recovered from (Slack
  retry exhausted, snapshot pruning hiccup, soft-delete lookup
  miss, etc.).
- `ERROR`: things you should investigate (unexpected DB error,
  scheduler tick error after retries, etc.).
- `DEBUG` (opt-in): every check result, full Slack call detail,
  individual ingress events. Toggle via `--log-level=debug`.

### Secret masking

Anything wrapped in `secret.SecretString` (DB password, Slack bot
tokens) gets masked at the slog formatter layer: `SU****RD` for
length ≥ 8, `****` otherwise. The asterisk count is fixed at 4 so
logs don't leak the true secret length.

## Schema migrations

Migrations live in
[`internal/migrate/migrations/`](../internal/migrate/migrations/),
embedded into the binary via `embed.FS`. Run on the same binary:

```bash
toggle-monitor migrate            # apply pending migrations
toggle-monitor migrate --check    # exit 0 if already at the latest version
```

The Helm chart wires this as an ArgoCD `PreSync` hook (or a Helm
`pre-install,pre-upgrade` hook on non-ArgoCD installs), so the
migrate Job runs before the new app pod rolls out.

**The app refuses to start when the schema version doesn't match
its embedded migrations.** If you see `schema at version N; binary
expects M (run toggle-monitor migrate)`, the migrate Job didn't run
— check `kubectl logs job/<release>-migrate-<rev>` in ArgoCD's
sync history.

## Heartbeat

Optional outbound deadman to any healthchecks.io-compatible URL:

```yaml
heartbeat:
  url: https://hc-ping.com/<uuid>
  interval: 1m
  failOnStalledWorker: true
```

Behavior:

- Every `interval`, POST a JSON body `{"openIncidents": N,
  "lastTickAt": "<RFC3339>"}` to `url`.
- Stall criterion: `now - lastTick > max(2 × interval, 6 min)`. The
  6-min floor avoids false stalls when the smallest monitor
  interval is 5 min.
- When stalled AND `failOnStalledWorker: true`, the POST goes to
  `{url}/fail` (healthchecks.io's "fail" convention; other deadman
  services accept the same path).
- On graceful shutdown the binary sends one final
  `{"event":"shutdown"}` POST to `url`.

Omit the `heartbeat:` block to disable the loop entirely.

## Slack message preview (CLI)

Sanity-check the bot token, channel binding, and message rendering
without waiting for a real monitor to flap:

```bash
# Uptime: Down → 2 reminders → prompt → Resolve
toggle-monitor slack test uptime --channel ops-alerts

# SSL: Expiring → 2 reminders → prompt → Renewed
toggle-monitor slack test ssl --channel ops-alerts
```

Useful flags: `--name`, `--reminders N`, `--interval 5s`, `--no-prompt`
(skip the "press Enter to resolve" pause), `--config <path>` (defaults
to `deploy/local/config.yaml`). Both commands hit the real Slack API
against the channel's `tokenEnv`, so pick a low-traffic channel.

## Slack workspace + userMapping health

Two background checks keep the Slack integration honest:

| Check | Cadence | What it does |
|---|---|---|
| Workspace `auth.test` | Hourly | Calls `auth.test` for every distinct bot token. Multi-workspace (different `team_id`s) was already rejected at startup; this catches token rotation / revocation. Cached state surfaces in the UI. |
| `userMapping` revalidation | 24h | Calls `users.info` per `U…` ID and `usergroups.list` once for all `S…` IDs. Flagged entries appear in the homepage "Slack mapping issues" panel until fixed. |

A transient `auth.test` failure at startup is a **warning**, not a
blocker; the cache surfaces in the UI and the next hourly tick
retries. A multi-workspace mismatch refuses to start.

## Common failure modes

### App CrashLoopBackOff with `schema at version N; binary expects M`

The migrate Job didn't run, or partially ran. Check
`kubectl logs job/<release>-migrate-<rev>`. Re-trigger by deleting
the failed Job (ArgoCD will recreate on next sync).

### `/readyz` returns 503 but `/healthz` is 200

The DB hasn't connected yet, OR the config hasn't been loaded.
Check the most recent INFO/ERROR log lines for `database connect
failed, retrying`. Startup tolerates ~60s of backoff before
exiting.

### `slack workspace check: slack tokens span multiple workspaces`

v1 is single-workspace only. Every channel's `tokenEnv` must
resolve to a bot token whose `auth.test` returns the same
`team_id`. Either consolidate to one workspace or wait for the v2
multi-workspace work.

### Monitor stuck at `temporary-paused` after parent recovers

The next tick re-evaluates. If the parent's status flipped to `up`
in the DB, the child's next scheduled tick will probe normally. If
the child is stuck for > one `interval`, check whether its parent
chain still has a down node somewhere (a transitive `dependsOn`).

### Ingresses don't appear under `/discovery`

- `rbac.ingressWatch` must be `true` in the values file
  (default). Without the `ClusterRole`, `client-go` can't list
  Ingress resources.
- The `kube:` block must be present in `config`. Without it,
  auto-discovery is disabled even if RBAC is in place.

### Slack alerts not posting

1. Check `toggle_monitor_slack_post_total{result="fail"}`. A
   non-zero counter is the first clue.
2. Check the WARN logs for `slack chat.postMessage failed:
   <error>`. Common: `not_in_channel` (bot not added to the
   channel), `channel_not_found` (wrong channel ID),
   `invalid_auth` (token rotated/revoked).
3. The homepage Slack mapping panel will flag bad userMapping
   entries within 24h of revocation.

### Long-running Slack outage during a state transition

The DB transition is committed; Slack delivery failed and was
logged. On the next tick:

- For an `Open` event, no parent ts is stored → reminders log a
  warning ("no parent thread ref"). The next state change (resolve)
  posts a fresh, threadless resolve message.
- For a `Resolve` event, the parent edit fails silently; the next
  open creates a fresh thread.

This is intentional: the design favors "DB is the source of truth,
Slack is best-effort" over blocking the worker on a Slack outage.

## Backups + retention

- **Postgres backups** are CNPG's responsibility (WAL archiving +
  scheduled backups). The binary doesn't manage them.
- **Alert history** is kept forever in v1. At ~10–100k rows/year
  for 200 monitors the table stays small.
- **Soft-deleted monitors** are kept indefinitely; slug reuse
  reattaches to the old history.
- **No manual purge CLI/endpoint.** If retention ever matters, run
  SQL directly via `psql`.

## Operator runbooks

### Adding a static monitor

1. Edit the YAML in the ConfigMap (via your usual GitOps path).
2. `stakater/reloader` (if installed) restarts the pod. Otherwise
   `kubectl rollout restart deploy/<release>`.
3. The startup reconcile inserts the monitor; the scheduler picks
   it up immediately.

### Removing a static monitor

1. Delete the entry from the YAML.
2. After the rolling restart, the lifecycle reconcile detects the
   missing slug, soft-deletes the row, and posts:
   - An in-thread closeout reply + parent edit if the monitor was
     down at the time.
   - A non-threaded `⚠️ Monitor removed` warning to the
     last-known channel.

The detail page remains accessible via the slug; the listing
hides it by default (use `?archived=true` to surface).

### Rotating the Slack bot token

1. Replace the Secret's `bot-token` value.
2. The pod restarts via `stakater/reloader`.
3. The startup `auth.test` validates the new token. If multiple
   tokens are in play (multiple channels with distinct `tokenEnv`s),
   all must agree on `team_id`.

### Rotating the Postgres password

1. Replace the Secret's `password` value.
2. The pod restarts. The startup connect retries up to ~60s of
   exp-backoff against the new credential.

### Maintenance window

There is no first-class maintenance mode in v1. The recommended
shape:

1. Pre-pause the healthchecks.io check (so the deadman doesn't fire
   while you're scaled to zero).
2. `kubectl scale deploy/<release> --replicas=0`.
3. Do the maintenance.
4. `kubectl scale deploy/<release> --replicas=1`.
5. Resume the healthchecks.io check.

State transitions that occurred while scaled-to-zero are lost (the
worker wasn't running to observe them).

## See also

- [Architecture](architecture.md) — how the modules fit together.
- [Design decisions](design-decisions.md) — the rationale behind
  every locked choice.
- [Config schema](config-schema.md) — per-field reference.
- [Helm chart README](../deploy/helm/toggle-monitor/README.md) —
  deployment + values reference + production checklist.
