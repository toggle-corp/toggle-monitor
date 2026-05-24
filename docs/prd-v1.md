# toggle-monitor — PRD (v1)

Source of truth for the v1 implementation. Synthesized from [`initial-spec.md`](./initial-spec.md), [`design-decisions.md`](./design-decisions.md), and [`config-schema.md`](./config-schema.md). Where this PRD and those docs disagree, those docs win — they are the per-decision and per-field references.

> **Stale section notice (ADR-0002).** The `kube.match` design has been redesigned as a cascading rule tree — see [ADR-0002](./adr/0002-kube-match-tree-cascade.md). User stories, acceptance criteria, observability notes, and test-plan items below that reference `kube.presets`, `kube.pause`, `kube.annotationDomain`, the `kube-paused` status, or per-ingress `/kube.*` and `/config.*` annotations describe the **prior** design and are superseded by ADR-0002. The body text is preserved here for historical context and should not be treated as the current implementation contract.

## Problem Statement

The team runs a Kubernetes cluster fronted by an external bastion that terminates TLS. They need to know when any of their services — both static endpoints (legacy hosts, external SaaS, gateways) and the dynamic set of in-cluster ingresses — stops responding or is about to lose its certificate. Today there is no consolidated view: ingresses appear and disappear with deploys, SSL expiries surprise on-call, and Slack alerts (where they exist) are noisy, scattered across ad-hoc threads, and lack a clear "still down" cadence.

What the team specifically wants:

- A single tool inside the cluster that watches both a hand-written list of HTTP endpoints and every Ingress in the cluster, posts well-formatted Slack alerts to the right channel with the right people pinged, and exposes a read-only UI for current state and history.
- Config that lives in a ConfigMap (with YAML anchors for DRY) and is reviewable in git, not in a UI.
- Behavior the operator can predict: no implicit defaults baked into the binary, no hidden fields, no auto-renaming. If a value matters, it's in the YAML.
- Operationally boring: single binary, single replica, single Postgres (CNPG), restart-on-config-change via reloader, Prometheus metrics, an outbound heartbeat to a deadman service.

## Solution

A single Go binary deployed once per cluster that:

- Loads a YAML ConfigMap on startup (no hot reload — `stakater/reloader` restarts the Deployment on change), validates it strictly, and refuses to start if anything is wrong.
- Runs three concurrent concerns as goroutines: a worker loop that schedules per-monitor HTTP and SSL checks, a `client-go` informer that watches every Ingress in the cluster and materializes monitors from preset+annotation merging, and a server that exposes the UI, probes, and metrics.
- Drives an alert state machine that produces well-defined Slack lifecycle events (parent on down, threaded reminders, edited parent on resolve, dedicated SSL alerts, removed-monitor warnings, in-thread closeouts) using Block Kit with viewer-local timestamps and a single shared mention vocabulary.
- Stores everything in Postgres as event-sourced alert history plus current-state tables; CNPG handles backups; migrations run as an ArgoCD PreSync hook job.
- Surfaces a server-rendered UI (`templ` + HTMX + Tailwind) with homepage stats, per-group and per-monitor pages, an auto-discovery view that explains why each ingress did or did not materialize, and an archive filter for soft-deleted monitors.
- Provides a CLI mode (`validate`, `config show`, `migrate`, `migrate --check`) so the same binary is used for pre-push CI checks, debugging merged config, and applying schema migrations.

## User Stories

### Operator — configuration & deployment

1. As an operator, I want to declare every monitoring parameter in a YAML ConfigMap, so that all changes go through normal git review.
2. As an operator, I want to use YAML anchors to DRY my static monitors, so that I don't repeat the same defaults across dozens of entries.
3. As an operator, I want top-level `x-*` keys to be ignored by the validator, so that I can host anchor-only blocks without polluting the schema.
4. As an operator, I want non-secret values to support `${VAR}` and `${VAR:-default}` interpolation, so that I can vary host/port/channel-ID per environment without forking the config.
5. As an operator, I want all secrets sourced from environment variables (named via `tokenEnv` / `passwordEnv`), so that no secret ever sits in YAML.
6. As an operator, I want secret values to be wrapped in `SecretString` with partial-mask logging (`SU****RD` / `****`), so that I can confirm the right secret loaded without leaking it.
7. As an operator, I want startup to fail loudly if any referenced env var is unset, so that misconfiguration surfaces immediately.
8. As an operator, I want a `toggle-monitor validate <path>` CLI subcommand, so that I can validate config in CI before pushing.
9. As an operator, I want a `toggle-monitor config show [--monitor <slug>]` CLI subcommand, so that I can see the fully merged final config (preset + annotation + monitor) for one or all monitors.
10. As an operator, I want validation errors to include file line numbers and report multiple errors per run, so that I can fix typos in one pass.
11. As an operator, I want the validator to be strict on unknown top-level keys (except `x-*`), so that typos like `monitor:` are caught instead of silently dropped.
12. As an operator, I want config changes to trigger a restart automatically via `stakater/reloader`, so that I don't have to scale or roll the Deployment by hand.
13. As an operator, I want the binary to refuse to start on invalid config, so that I never run partially-loaded.
14. As an operator, I want the validator to enforce `retries × (timeout + retryBackoff) < interval`, so that I cannot deploy a config whose check cycles overlap.
15. As an operator, I want slugs to follow a single strict regex with a max length of 255, so that they are URL-safe and predictable.
16. As an operator, I want no auto-generation of slugs from friendly names, so that I always know the URL a monitor will live at.
17. As an operator, I want slug uniqueness enforced across the entire static monitor set, with kube-discovered conflicts surfaced in the auto-discovery snapshot at reconcile time, so that there is never a silent collision.

### Operator — running, observing, recovering

18. As an operator, I want a single static binary that runs as one Deployment with one replica, so that ops is simple at our scale.
19. As an operator, I want `/healthz` to report process liveness and `/readyz` to report DB + config readiness, so that Kubernetes probes do the right thing.
20. As an operator, I want a `/metrics` Prometheus endpoint with documented series (`toggle_monitor_checks_total`, `_check_duration_seconds`, `_active_incidents`, `_config_load_total`, `_slack_post_total`, `_ingress_reconcile_total`, `_worker_last_tick_seconds`, Go runtime), so that I can build dashboards and alerts.
21. As an operator, I want structured JSON logs via `slog` at `info` by default with a `--log-level=debug` toggle, so that I can investigate without changing the binary.
22. As an operator, I want the worker to send a heartbeat POST to a deadman URL every `heartbeat.interval` with `{openIncidents, lastTickAt}`, so that the cluster being down or the worker being deadlocked still pages someone.
23. As an operator, I want the heartbeat to actively POST to `{url}/fail` when the worker is stalled (no check completion within `max(2 × interval, 6 min)`) and `failOnStalledWorker: true`, so that I can tell the difference between "all quiet" and "tool is broken."
24. As an operator, I want graceful shutdown on SIGTERM to drain in-flight checks via context (not record them as failures), cancel the ingress watcher, flush pending DB writes, and emit a final `{"event":"shutdown"}` heartbeat, so that rolling restarts don't generate false negatives.
25. As an operator, I want Postgres outages at runtime to be tolerated: worker check writes retry 3× over 30s then log loudly and continue, UI reads render a "DB unavailable" page, Slack thread-ref lookup failures retry and then post a fresh parent, so that a transient DB blip doesn't crash the tool.
26. As an operator, I want Postgres unreachable at startup to retry with exponential backoff for ~60s and then exit (k8s restartPolicy continues), so that I don't have to babysit boot order.
27. As an operator, I want schema migrations to run as an ArgoCD PreSync hook Job invoking `toggle-monitor migrate`, so that I see migration logs in the ArgoCD UI before the new app pod rolls out.
28. As an operator, I want the app to refuse to start if the schema version doesn't match the binary, so that I never run against a half-migrated DB.
29. As an operator, I want `toggle-monitor migrate --check` to verify without applying, so that I can sanity-check before deploys.
30. As an operator, I want a single Slack workspace enforced at startup (refuse to start if `auth.test` across tokens returns mismatched `team_id`), so that I cannot accidentally configure multi-workspace alerting in v1.
31. As an operator, I want a transient Slack `auth.test` failure to be a warning (surfaced in the UI and re-checked hourly), not a startup blocker, so that a Slack outage doesn't ground my deploys.
32. As an operator, I want maintenance mode to be performed by scaling the Deployment to zero replicas with a pre-pause of the healthchecks.io check, so that v1 doesn't ship a half-baked maintenance feature.

### Operator — static monitors

33. As an operator, I want every static monitor to declare its own group, with the validator rejecting unknown or missing groups, so that group membership is never ambiguous.
34. As an operator, I want each static monitor to fully declare its parameters (no hidden defaults), so that the YAML is the source of truth for what's monitored and how.
35. As an operator, I want `monitors[].dependsOn` to list static parent monitors, so that downstream services pause (status `temporary-paused`, no HTTP, no SSL, no DB write) while a gateway is down.
36. As an operator, I want the validator to detect cycles in `dependsOn`, so that I can't deploy an unresolvable graph.
37. As an operator, I want HTTP-only static monitors to behave with status `ssl-skipped`, so that I see "monitored, no SSL check" instead of false success.
38. As an operator, I want SSL fields on HTTP-only monitors to be allowed but ignored, so that I can reuse the same anchor across HTTP and HTTPS monitors.
39. As an operator, I want a removed-from-YAML monitor with an open incident to get a thread closeout reply, the parent edited to "✅ Resolved (monitor removed)", and a separate non-threaded warning posted to the destination channel, so that humans see the closeout in-thread but also have a noticeable "this is gone" notice.

### Operator — kube auto-discovery

40. As an operator, I want every Ingress in the cluster to be observed (cluster-wide RBAC on `networking.k8s.io/v1`), so that I never miss one due to namespace plumbing.
41. As an operator, I want the trigger for opting an Ingress in to be the presence of a single annotation `<base>/kube.preset`, so that ingresses unrelated to monitoring are silently ignored.
42. As an operator, I want the annotation base domain to be configurable via `kube.annotationDomain`, so that we can rebrand without touching the binary.
43. As an operator, I want each unique `host` in `spec.rules[].host` to produce its own monitor (URL = `{scheme}://{host}{preset.path}`), so that multi-host ingresses are fully covered.
44. As an operator, I want kube-discovered slugs to follow `kube-{ns}-{name}-{host-as-slug}` with strict sanitization and the result rejected to "slug generation failed" if empty, so that I get stable, collision-free slugs without surprise.
45. As an operator, I want every kube-discovered monitor to receive the `kube` tag automatically, so that I can filter by source.
46. As an operator, I want the scheme to default to `https` (with `scheme: http` override per preset), so that the bastion-terminates-TLS topology is the happy path.
47. As an operator, I want `ingress.spec.tls` ignored in URL construction, so that internal-HTTP/external-HTTPS topology isn't accidentally mis-monitored.
48. As an operator, I want a `kube.pause:` list of hosts (with glob support) to materialize matched ingresses as `kube-paused` monitors that skip all checks but preserve history, so that I can declaratively silence noisy ingresses.
49. As an operator, I want a `kube-discovered` group required in the config, so that there is no implicit/magic group anywhere.
50. As an operator, I want preset and ingress-annotation values to merge with preset as base and annotation as override (list fields union except `acceptedStatusCodes` which replaces), so that the merge rules are predictable.
51. As an operator, I want ingresses without `kube.preset` recorded in the auto-discovery snapshot with reason `"no preset annotation"`, so that the UI shows me "everything we saw and why it didn't materialize."
52. As an operator, I want ingresses with `kube.preset` pointing to an unknown slug recorded with that reason, so that I can fix the annotation or add the preset.
53. As an operator, I want a static slug and a kube-discovered slug colliding to be surfaced in the auto-discovery snapshot with "static wins, this was skipped," so that I don't lose visibility.
54. As an operator, I want the snapshot table updated on each reconcile (current state, no history), so that "discovery state" stays small and current.
55. As an operator, I want `kube.resyncInterval` configurable (default 30m, min 1m), so that I can tune informer load.
56. As an operator, I want ingress disappearance to soft-delete the monitor (archive flag + history preserved), post an in-thread closeout if the monitor was down, and post a non-threaded "monitor removed" warning to the destination channel with reason "k8s ingress removed," so that a deploy that retires an endpoint produces a closed incident, not a permanently-firing alert.
57. As an operator, I want the discovery annotation set to be the documented one (`kube.preset`, `kube.path`, `config.group`, `config.dependsOn`, `config.notify`, `config.tags`, `config.enabled=false`) and only annotations (no labels), so that opt-out and overrides are one consistent mechanism.

### Operator — Slack

58. As an operator, I want a single `slack:` config block holding `channels`, `userMapping`, `bodyMaxChars`, and an optional `summaryChannel`, so that all Slack mechanics live in one place.
59. As an operator, I want each Slack channel referenced by slug (not channel ID) from monitors and presets, with the channel ID inline in a YAML comment, so that the config is human-readable but unambiguous.
60. As an operator, I want DMs (`D…`) rejected as channel IDs, so that we cannot accidentally alert into a DM.
61. As an operator, I want each monitor to send to exactly one Slack destination in v1, so that the data model stays simple.
62. As an operator, I want a `slack.userMapping` slug → `U…`/`S…` table referenced by `notify:` lists, so that we mention people by name (not opaque IDs) in config.
63. As an operator, I want `notify:` entries that look like `<...>` (e.g., `<!here>`, `<!channel>`) to pass through verbatim, so that channel-wide mentions still work.
64. As an operator, I want unknown `notify:` slugs rejected at config-load, so that typos don't silently drop mentions.
65. As an operator, I want `userMapping` entries validated against Slack at startup and every 24h with a cached "last-checked" timestamp, so that revoked or renamed IDs are caught.
66. As an operator, I want a UI section listing invalid/unknown `userMapping` entries with reason and last-checked time, so that I can fix them.
67. As an operator, I want the body to be included inline in Slack only when its length ≤ `slack.bodyMaxChars`, so that giant HTML error pages don't blow up the channel.
68. As an operator, I want the body stored in Postgres truncated to `dbBodyMaxChars`, so that multi-MB responses don't bloat the table.
69. As an operator, I want HTTP error display in Slack to always include status code and short status text but never the response body inline above the threshold, with a `[View details]` button linking to the monitor detail page when `publicBaseURL` is configured, so that the channel stays scannable.
70. As an operator, I want notifications resolved as the union of monitor + group + preset `notify:` lists, with mentions fired only on the parent-down message (reminders/resolves/warnings are quiet), so that we get one ping per incident, not three.

### Operator — alert lifecycle (uptime & SSL)

71. As an operator, I want `Retries` to be in-cycle retries that suppress transient failure within a single tick, with `RetryBackoff` between them, so that a single blip doesn't page.
72. As an operator, I want no consecutive-failure threshold, so that legitimate alerts are not slowed down.
73. As an operator, I want each monitor to have its own `reminderInterval`, so that while a monitor is down I get a thread reply at that cadence (default 3 days).
74. As an operator, I want the uptime parent message to use Block Kit with a header `🔴 <Name> is DOWN`, context line (group · URL), mention block, fields (status code+text, failure time, last error), an optional inline body, and a `[View details]` button, so that the alert has all key info at a glance.
75. As an operator, I want the reminder to be a short scannable thread reply with no mentions, so that subscribers see updates without re-pinging the channel.
76. As an operator, I want resolve to edit the original parent (preserving all content, swapping the header to `✅ … is UP (was down for Xd Yh Zm)` and appending a "Resolved at" field) and post a thread reply with the downtime, so that the channel reflects the final state in place.
77. As an operator, I want SSL handled as its own incident class with its own thread in the same channel, with three thresholds (`sslAlertThreshold`, `sslEscalationThreshold`, `sslReminderInterval`), so that uptime and cert hygiene don't get muddled.
78. As an operator, I want SSL auto-resolve when the cert renews (TTL jumps back up), preserving the parent and changing the header to `✅ Certificate renewed`, so that cert renewals show as a closure, not a new alert.
79. As an operator, I want all Slack timestamps rendered via `<!date^{unix}^…>` so that each viewer sees their own local time, regardless of `displayTimezone`.
80. As an operator, I want the UI to render in a single `displayTimezone` (not browser-derived), so that screenshots and shared URLs are unambiguous.

### Operator — UI consumer

81. As a viewer, I want a homepage with stats (`up / down / temporary-paused / ssl-expiring / ssl-skipped`) and a paginated list of latest alerts, so that I can see system state at a glance.
82. As a viewer, I want a monitor listing page sorted by status (down → paused → ssl-expiring → up) then group then name, so that problems surface at the top.
83. As a viewer, I want a per-group listing at `/group/<slug>` and a per-monitor detail page at `/monitor/<slug>` with current status, the merged final config, a per-field provenance section (preset / annotation / monitor), and paginated alert history, so that I can debug exactly what's running for any monitor.
84. As a viewer, I want an auto-discovery listing showing every observed ingress with status (added / `kube-paused` / `kube-invalid` with sub-reason: no preset, unknown preset, slug failure, static collision), so that I can debug a missing monitor.
85. As a viewer, I want per-listing default page sizes (20 / 50 / 50 / 50), a `?per_page=` override capped at `ui.maxPerPage` (default 200), and search/filter state encoded in URL params (with HTMX `hx-push-url="true"`), so that views are linkable and shareable.
86. As a viewer, I want HTMX-driven filters and pagination without a SPA build, so that the UI is server-rendered, small, and fast.
87. As a viewer, I want a "no results" empty state with a "clear filters" link, so that aggressive filtering is easy to undo.
88. As a viewer, I want an archive filter on the monitor listing to expose soft-deleted monitors, so that I can find the history of a retired endpoint.
89. As a viewer, I want each group to render in a single hex color (with auto-derived contrasting text), defaulting to `theme.defaultGroupColor`, so that visual identity is consistent without per-theme color pairs.
90. As a viewer, I want no charts in v1, so that we ship the v1 scope and add latency views later (via Prometheus, not Postgres).
91. As a viewer, I want the UI to be exposed via an ingress restricted to the local network with no auth in v1, so that we don't ship half-built RBAC.

## Implementation Decisions

### Process topology

- One Go binary, one Deployment, one replica. Three concurrent concerns as goroutines: worker, ingress watcher, HTTP server.
- Future scale-out (deferred): shard by group.

### Configuration & validation

- Single YAML ConfigMap; reloaded by `stakater/reloader` restart (no hot reload).
- Anchors-only DRY for static monitors; `x-*` top-level keys ignored by the validator.
- `${VAR}` / `${VAR:-fallback}` interpolation on any scalar; `$$` escapes; secrets never interpolatable (always via `tokenEnv` / `passwordEnv`).
- Validator runs the same code path as the binary boot and the CLI: parse → expand env → schema check → cross-field check → reference resolution (slugs, groups, channels, mappings, dependsOn) → cycle detection.
- Errors report file line numbers (from `yaml.v3` node positions) and accumulate (multi-error per run).
- Strict on unknown top-level keys (with `x-*` exception).
- CLI subcommands: `validate <path>`, `config show [--monitor <slug>]`, `migrate`, `migrate --check`. Default mode (no subcommand) is "serve."

### Slug rules

- Regex: `^[a-z][a-z0-9]*(-[a-z0-9]+)*$`, max 255 chars, applied uniformly to every slug type.
- Slugs are explicit and required everywhere; never derived from friendly names.
- Kube-discovered slug = `kube-{ns}-{name}-{host-as-slug}`; sanitization: lowercase, invalid chars → `-`, collapse consecutive `-`, strip leading/trailing `-`, reject empty. Failure produces a discovery snapshot row with reason `"slug generation failed"` and no monitor.

### State storage

- Postgres via CNPG. `timestamptz` everywhere (UTC internally).
- Tables (logical): monitors (current state + archive flag), alert events (event-sourced — append-only on state changes/alert events only, no per-check rows), Slack thread refs, auto-discovery snapshot (one row per observed ingress; overwritten on reconcile).
- Migrations: `golang-migrate/migrate` library with SQL files in `embed.FS`. Driven by `toggle-monitor migrate` (PreSync hook job in chart). App refuses to start if schema version doesn't match.
- Postgres unreachable at startup: exp backoff ~60s then exit. At runtime: writes retry 3× over 30s then log loudly and continue; UI reads render "DB unavailable"; Slack thread-ref retry then post fresh parent.

### Secret handling

- All secrets via env vars named in `tokenEnv` / `passwordEnv`. Env var format `^[A-Z][A-Z0-9_]*$`. Verified set & non-empty at startup.
- `SecretString` type implements `slog.LogValuer`:
  - Length ≥ 8 → `<first2>****<last2>`, asterisk count fixed at 4.
  - Length < 8 → `****`.

### HTTP check execution

- One ticker per monitor, one goroutine per check; ~200 (max ~500) monitors comfortably.
- Startup jitter: sleep `rand(0, interval)` before first tick.
- In-cycle retries gated by `retries × (timeout + retryBackoff) < interval` validator rule.
- `followRedirects: false` by default per monitor; override per monitor.
- `User-Agent` from `httpClient.userAgent` (globally configurable).
- Event-sourced history — only state transitions and alert events written; no per-check rows.

### Alert state machine

- Status set: `up / down / temporary-paused / kube-paused / kube-invalid / ssl-expiring / ssl-skipped`.
- `temporary-paused`: any monitor whose `dependsOn` parent is currently down. Skip the entire check (no HTTP, no SSL, no DB write).
- `kube-paused`: matched by `kube.pause:` host (glob); persistent until config changes; preserves history.
- `kube-invalid`: ingress observed but no monitor materialized; three sub-reasons (no preset / unknown preset / slug failure). No per-ingress Slack alerts; surfaced in the weekly summary (design deferred).
- `ssl-skipped`: only on static `http://` monitors (kube-discovered defaults to https).
- Transitions emit alert events: parent-down, reminder (per `reminderInterval`), resolve (edit parent + thread reply); SSL parent / reminder (cadence per `sslReminderInterval` above escalation, daily below `sslEscalationThreshold`) / resolve (cert renewed); removed-monitor warning + in-thread closeout.
- Mentions fire on the parent-down message only; reminders, resolves, warnings are quiet.

### Preset + annotation merger (kube)

- Merging precedence:
  - Kube-discovered monitor: `kube.presets[slug]` → ingress annotations → final.
  - Static monitor: monitor's own fields → final (no preset layer).
- List fields merge as union except `acceptedStatusCodes`, which replaces.
- Auto-tag `kube` always appended to kube-discovered monitors.
- Every preset must declare a complete set of monitoring params: `httpMethod`, `acceptedStatusCodes`, `interval`, `timeout`, `retries`, `retryBackoff`, `followRedirects`, `slack`, `reminderInterval`, `sslAlertThreshold`, `sslEscalationThreshold`, `sslReminderInterval`, `path`. Optional: `notify`, `tags`, `dependsOn`, `group`, `scheme`.
- UI exposes per-field provenance (preset / annotation / monitor) on the monitor detail page.

### Kube ingress watcher

- Cluster-wide RBAC on `networking.k8s.io/v1` only.
- `client-go` informer with configurable resync (`kube.resyncInterval`, default 30m, min 1m).
- Every observed Ingress is recorded in the auto-discovery snapshot table; only those carrying `<base>/kube.preset` materialize active monitors.
- Multi-host: one monitor per unique `host` in `spec.rules[].host`.
- Scheme: default `https`; preset can override to `http`. `ingress.spec.tls` ignored.
- Annotation set (all on `<base>/`): `kube.preset` (opt-in), `kube.path`, `config.group`, `config.dependsOn`, `config.notify`, `config.tags`, `config.enabled=false`. Annotations only (no labels).
- `config.enabled=false`: opt-out without removing other annotations (treated as kube-invalid for v1 purposes).
- Static-vs-kube slug collision: static wins; the kube slug is recorded in the snapshot as skipped with reason.

### Slack

- Bot token (`xoxb-`) from env var. Single workspace v1 (`auth.test` across tokens must share `team_id`).
- Block Kit for all messages. Timestamps via `<!date^{unix}^{format}|fallback>`.
- Channel slug ↔ channel ID + tokenEnv mapping in `slack.channels[]`. Channel IDs must match `^[CG][A-Z0-9]{8,}$`; DMs rejected.
- `userMapping`: slug → `U…` (user) or `S…` (subteam). Emits `<@U…>` or `<!subteam^S…>` markup respectively. Periodic Slack `auth.test` revalidation every 24h with cached "last-checked" surfaced in UI.
- `bodyMaxChars` gates inline body in Slack; `dbBodyMaxChars` gates stored body in Postgres (with `dbBodyMaxChars >= bodyMaxChars` enforced).
- Resolve preserves parent content; only the header and an appended "Resolved at" field change.
- Removed-monitor warning is a separate non-threaded post; the in-thread closeout (if open incident) is a thread reply + parent edit.

### Heartbeat & probes

- Optional top-level `heartbeat:` block. Every `interval`, POST `{openIncidents, lastTickAt}` to `url` while the worker is healthy.
- Liveness criterion: most recent check completion within `max(2 × interval, 6 min)`. On stall, POST to `{url}/fail` if `failOnStalledWorker: true`.
- `/healthz` (process+listener), `/readyz` (DB + config loaded once).
- `/metrics` exposes the documented Prometheus series + Go runtime metrics via `promhttp`.

### UI

- Server-rendered `templ` components, HTMX for partial updates (filters, pagination, expandable rows), precompiled Tailwind CSS, all embedded via `embed.FS`. No Node at runtime, no SPA build.
- URL state for filters via `hx-push-url="true"`.
- No auth in v1; ingress restricted to local network.
- No charts in v1.

### Observability

- Prometheus series:
  - `toggle_monitor_checks_total{monitor, status="ok|fail|paused"}` (counter)
  - `toggle_monitor_check_duration_seconds{monitor}` (histogram)
  - `toggle_monitor_active_incidents{type="uptime|ssl", monitor}` (gauge)
  - `toggle_monitor_config_load_total{result="success|fail"}` (counter)
  - `toggle_monitor_slack_post_total{result="success|fail"}` (counter)
  - `toggle_monitor_ingress_reconcile_total{result="added|skipped|removed"}` (counter)
  - `toggle_monitor_worker_last_tick_seconds` (gauge)
- `slog` JSON logging; `info` default; `debug` toggle. INFO: state transitions, Slack outcomes, config load summary, ingress reconcile summary, lifecycle. DEBUG: every check, full Slack call detail, individual ingress events.
- No tracing, no external error reporting in v1.

### Lifecycle

- Startup: load + validate config → connect DB (exp backoff, fail after ~60s) → check schema version → start informer → start worker → start HTTP server → emit first heartbeat.
- Shutdown on SIGTERM, in order: stop accepting HTTP, cancel in-flight checks via context (do not record as failures), cancel watcher, wait up to `terminationGracePeriodSeconds - 5s`, flush DB writes, close DB, send final heartbeat with `{"event":"shutdown"}`, exit 0.

### Modules

Major modules (all internal to a single Go binary):

1. **config** — YAML parsing (anchors, `x-*`, env interpolation, line-numbered errors), schema validation, cross-field rules, reference resolution, cycle detection. Powers `validate` and `config show`.
2. **slug** — regex enforcement + kube-discovered sanitization.
3. **secret** — `SecretString` with partial-mask logging.
4. **db** — connection mgmt, retry, schema-version check.
5. **migrate** — golang-migrate driver over `embed.FS`; `migrate` and `migrate --check` subcommands.
6. **store** — repository over `db` for monitors, alert events, Slack thread refs, discovery snapshot, archive flag.
7. **kube** — `client-go` informer, annotation parsing, pause-list matching, snapshot maintenance.
8. **merger** — preset + annotation merging (union vs replace), provenance trail.
9. **scheduler** — per-monitor tickers, jitter, `dependsOn` gating, context-cancel-aware retries.
10. **httpcheck** — single HTTP probe (method, codes, timeout, redirects, UA).
11. **sslinspect** — TLS cert extraction + threshold evaluation.
12. **alert** — state machine; consumes check + cert results, emits state transitions and alert lifecycle events.
13. **slack** — token mgmt, workspace check, Block Kit builders, thread-ref handling, mention resolution, `<!date^…>` rendering, body inclusion/truncation, periodic `userMapping` revalidation.
14. **heartbeat** — periodic POST loop, stalled-worker detection, `/fail` mode.
15. **web** — `templ` + HTMX + Tailwind UI, `/healthz`, `/readyz`, `/metrics`.
16. **lifecycle** — signal handling, ordered shutdown, startup orchestration.
17. **observability** — Prometheus registration + slog setup.

## Testing Decisions

A good test for this codebase exercises **observable, external behavior** of a module: given an input (config text, an Ingress object, a check result, a state transition), assert the output (validation error set, materialized monitor, emitted alert event, rendered Block Kit JSON, snapshot row) — never call private functions, never assert against in-memory implementation state.

Modules to cover with isolated tests:

- **config (loader + validator).** Unit tests over:
  - Slug regex (positive + negative).
  - Anchor merging behavior and `x-*` ignore.
  - `${VAR}` / `${VAR:-fallback}` / `$$` interpolation with set, unset, and empty values.
  - Cross-field rules: `retries × (timeout + retryBackoff) < interval`; `dbBodyMaxChars >= slack.bodyMaxChars`; `sslAlertThreshold > sslEscalationThreshold`; `timeout < interval`.
  - Reference resolution: every `group`, `slack`, `notify`, `dependsOn`, `summaryChannel` resolves; missing `kube-discovered` group rejected.
  - Cycle detection in `dependsOn`.
  - Multi-error reporting and `yaml.v3` line numbers preserved.
  - Strict-unknown-top-level-keys behavior (and `x-*` allowed).
  - Conditional SSL fields (required when URL is HTTPS, optional+ignored when HTTP).

- **merger (preset + annotation).** Unit tests over:
  - Union for `notify`, `tags`, `dependsOn`; replace for `acceptedStatusCodes`.
  - Annotation override of every documented field.
  - `kube` tag auto-appended.
  - Group fallback to `kube-discovered` when neither preset nor annotation supplies one.
  - Provenance trail correctness (per-field source).

- **kube ingress → monitor materialization (snapshot tests).** End-to-end transformation tests, golden-file style: feed in fixture Ingress objects (single host, multi-host, `kube.preset` missing, unknown preset, slug sanitization edge cases, `config.enabled=false`, `kube.pause` match, static-slug collision, `https` default vs `scheme: http` override, all documented annotations) and snapshot:
  - The materialized monitor(s) (final merged config).
  - The discovery snapshot row(s) (status + reason).
  - The provenance trail.
  - Already called out in design-decisions.md "Open branches" #1.

- **alert state machine.** Table-driven tests over transitions:
  - up → down (parent-down event with mentions); down → down (reminder cadence respect; no premature reminder); down → up (resolve event + thread reply; parent header rewrite).
  - In-cycle retries do not transition state.
  - `dependsOn` parent goes down → child `temporary-paused`, no check executed, no DB write. Parent recovers → child resumes (no special handling of in-flight child incidents per Q11e).
  - `kube.pause` match closes an open incident (thread reply + parent edit).
  - SSL: TTL drops below `sslAlertThreshold` → SSL parent; below `sslEscalationThreshold` → daily reminder cadence; TTL jumps up → resolve.
  - Monitor removed (static or ingress) with open incident → in-thread closeout + parent edit + separate warning.
  - `kube-invalid` transitions produce no Slack events.

Prior art: this is a greenfield repo (only docs today), so there is no existing test pattern to mirror. The conventions to establish on first PR: Go's standard `testing` package + `github.com/google/go-cmp/cmp` for diffs; golden files under `testdata/` named to match the test case; for kube fixtures, a small builder package that produces `*networkingv1.Ingress` objects so tests stay readable.

## Out of Scope

The following are explicitly out of v1 and deferred for later (per design-decisions.md open branches):

- Weekly Slack summary content/cadence design. `slack.summaryChannel` is plumbed but the message itself is not built.
- First-class maintenance mode (time-bounded ad-hoc pause via CLI or YAML windows). v1 uses scale-to-zero with manual healthchecks.io pause.
- Multiple Slack destinations per monitor.
- Multi-workspace Slack support.
- Latency graphs / per-check rows. Event-sourced history deliberately drops this; if added later, do it via Prometheus.
- Data retention / purge policy.
- `role: gateway` UI sugar for dependency parents.
- Special handling for in-flight child incidents when an upstream parent goes down.
- Auth / RBAC on the UI (relies on network restriction).
- Distributed tracing (OTel).
- External error reporting (Sentry et al).
- Multi-cluster aggregation; one instance per cluster.
- Hot config reload (intentional — restart-on-change via reloader).
- Slug auto-generation from friendly names.
- Hardcoded defaults in the binary; global `defaults:` block.

## Further Notes

- Reference docs in this repo, in order of authority for v1 implementation:
  1. [`docs/config-schema.md`](./config-schema.md) — locked per-field schema.
  2. [`docs/design-decisions.md`](./design-decisions.md) — every resolved design decision with rationale.
  3. [`docs/initial-spec.md`](./initial-spec.md) — original brief; superseded where it conflicts.
  4. [`docs/config-example.yaml`](./config-example.yaml) — hand-written example exercising the schema.

- Suggested values (interval 5m, timeout 10s, retries 2, retryBackoff 5s, reminderInterval 3d, accepted codes `[200]`, method `GET`, SSL alert 30d / escalation 7d, followRedirects false) are reference values for writing the first config; the binary applies no defaults.

- The dependency graph for `dependsOn` is fully resolvable at config-load because parents must be static monitors. Kube-discovered monitors cannot be parents (intentional simplification).

- Deployment chart responsibilities (not in the binary):
  - Map `SLACK_BOT_TOKEN` / `DB_PASSWORD` env vars from k8s Secrets via `valueFrom.secretKeyRef`.
  - Annotate the Deployment with `reloader.stakater.com/auto: "true"` (or scoped annotations).
  - Ship a PreSync hook Job that invokes `toggle-monitor migrate`.
  - Provide cluster-wide `ClusterRole` for `networking.k8s.io/v1` Ingress `get/list/watch`.
  - Restrict the UI ingress to local network.

- Soft-delete preserves history; slug reuse reattaches a new monitor to old history. There is no manual purge CLI/endpoint in v1.
