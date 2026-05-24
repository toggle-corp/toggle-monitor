# toggle-monitor — design decisions

In-progress design conversation built on top of [`initial-spec.md`](./initial-spec.md). Captures resolved decisions and open branches so the design grilling can be resumed across sessions.

## Architecture Decision Records

Subsystem-scale design changes that supersede sections of this document live as ADRs under [`adr/`](./adr/):

- [ADR-0002 — `kube.match` as a cascading rule tree](./adr/0002-kube-match-tree-cascade.md). Replaces the `kube.presets:` registry, the `kube.pause:` block, the `kube.annotationDomain` field, and the entire per-ingress `/kube.*` / `/config.*` annotation layer with a single tree of `when:` / `config:` / `nested:` rules. The kube subsections of "Auto-discovery from k8s ingress", "Presets & DRY", "`kube.pause:` hard-pause list", "K8s ingress annotation convention", and "Kube monitor statuses & lifecycle" below describe the *prior* design and are superseded in-place by ADR-0002.
- [ADR-0003 — StatusPage replaces Group](./adr/0003-statuspage-replaces-group.md). Deletes `Group` (the entity, the `groups:` block, `monitor.group`, `kube.config.group`, `theme.defaultGroupColor`, `Group.Notify`) in favour of `StatusPage` as the sole collection entity. Section membership is tag-driven via an `any:` / `all:` boolean predicate tree with `tags:` (AND-internally) and `hostRegex:` leaves; N:M monitor↔section. Status pages render with the operator nav + theme toggle; `/group/<slug>` and `/groups` are gone. References to "groups" in this document — sharding, slug regex, annotation overrides, monitor body context lines, etc. — are superseded.

## Resolved decisions

### Language & runtime
- **Go.** Single static binary. Drivers: native k8s ecosystem, low memory footprint, simple ops.

### Process topology
- Single binary, single replica. All concerns (worker, k8s ingress watcher, notifier, API+UI) run as goroutines in one process.
- Future horizontal scale-out (when needed): shard by **Group** — each replica owns a subset of groups.
- UI is read-only; no need to scale it independently at current load.

### State storage
- **Postgres** via existing CNPG (CloudNativePG) setup.
- Persists: current monitor status, alert history (event-sourced), active Slack thread refs, auto-discovery state (added/skipped + reason).
- Config does **not** live in Postgres — it stays in the ConfigMap YAML.

### Retry & alerting semantics
- `Retries` per monitor = **in-cycle retries** (transient failure suppression). Stateless across cycles.
- `RetryBackoff` per monitor = duration between retries (e.g., retry 3× every 5s).
- **No** consecutive-failure threshold (skipped — would slow legitimate alerts and add state).
- **Reminder cadence** per monitor: while a monitor is down, post a thread reminder every N days (default 3).
- Config-load **must validate** `retries × (timeout + retryBackoff) < interval` so check cycles don't overlap.

### Auto-discovery from k8s ingress
- **Slug:** `kube-{namespace}-{ingress-name}-{host-as-slug}` (host dots replaced with dashes — e.g., `foo.example.com` → `foo-example-com`). Always prefixed with `kube-` to clearly identify discovered monitors and always includes host so multi-host ingresses produce stable, collision-free slugs. Invalid characters in namespace/ingress-name are sanitized (replaced with `-`, consecutive `-` collapsed, leading/trailing `-` stripped); empty result rejected.
- **Conflict with static monitor:** static wins. Surface skip reason in the auto-discovery detail page.
- **Group assignment:** via annotation `<annotationDomain>/config.group=<slug>`. Fallback: a group with slug `kube-discovered` which **must** be declared in the `groups:` section (validator throws if missing). No implicit/magic group.
- **Auto-tag:** every ingress-discovered monitor gets a `kube` tag.
- **Annotation overrides allowed beyond `preset`:** path, group, tags (preset is base, annotations override). Other raw config overrides are rare but permitted.
- Monitor detail page must show **config provenance** — what came from preset, what from annotation, what's the merged result.

### Monitor lifecycle (disappearance)
Soft-delete with archived flag. Slug reuse reattaches to old history.
- **Static monitor removed from YAML:** if currently down, post thread reply ("monitor removed") and edit parent to resolved-with-note; separately, post a warning Slack message (with detail page link, URL, method). The thread closeout and the warning are not cross-linked.
- **Ingress removed from cluster:** same behavior, flagged as k8s-sourced.
- **Discovery annotation removed:** same behavior, include reason in the message.
- **Archive filter** in the listing UI.

### Resilience & lifecycle
- **Postgres unreachable at startup:** retry-with-exponential-backoff for ~60s, then exit. K8s restart policy handles continued retries naturally.
- **Postgres unreachable at runtime:**
  - Worker writes: retry 3× over ~30s, then log loudly and continue (lose this check's record, don't crash).
  - UI reads: render a friendly "DB temporarily unavailable" page.
  - Slack thread refs unreachable: retry + log; if persistent, post a fresh parent (no thread linkage).
  - Sustained DB outage is caught by the heartbeat liveness criterion (Q19).
- **Graceful shutdown** on SIGTERM (k8s rolling restart, scale-down, config change):
  1. Stop accepting new HTTP requests (close listener).
  2. Cancel in-flight monitor checks via context — **do not** record them as failures (context-cancelled ≠ signal about the monitored service).
  3. Cancel the ingress watcher.
  4. Wait up to `terminationGracePeriodSeconds - 5s` for in-flight work.
  5. Flush pending DB writes; close DB connection.
  6. Send a final heartbeat with `{"event": "shutdown"}`.
  7. Exit 0.

### Database migrations
- **`golang-migrate/migrate`** library with SQL files. Migrations embedded via `embed.FS`.
- **Run as an ArgoCD PreSync hook Job**, not auto-run on startup. The deployment chart includes a Job manifest with `argocd.argoproj.io/hook: PreSync`; the Job invokes `toggle-monitor migrate` and exits.
  - Easier to view/debug migration logs in ArgoCD UI / Job pod logs.
  - Migrations run before the new app pod rolls out, so the app always sees a matching schema.
- **App refuses to start** if schema version doesn't match (clear error pointing the operator to run migrations). No silent in-place migration.
- **`toggle-monitor migrate`** subcommand performs the migration; `toggle-monitor migrate --check` verifies without applying (used as a sanity check).

### Config versioning & slug rules
- **No `version:` field in config.** The binary version implicitly defines the schema. Breaking schema changes require the operator to update the YAML by hand (LLM-assisted in practice). No migration subcommand.
- **Slug regex (applies to every slug in the config — monitor, group, kubePreset, slack channel, slack.userMapping, kube-discovered):**
  ```
  ^[a-z][a-z0-9]*(-[a-z0-9]+)*$
  ```
  - Lowercase a–z, digits 0–9, hyphen `-`.
  - Must start with a letter.
  - Must not end with hyphen; no consecutive hyphens.
  - **Max 255 chars** (consistent for all slug types).
- **No auto-generation** of slugs from friendly names. User must declare every slug explicitly in YAML; CLI validator rejects missing.
- **Kube-discovered slug sanitization:** namespace + ingress-name + host are lowercased; invalid characters replaced with `-`; consecutive `-` collapsed; leading/trailing `-` stripped. If the resulting slug is empty or otherwise invalid, the ingress is recorded in the auto-discovery snapshot with reason `"slug generation failed"` (no monitor created).

### Config change handling
- **Restart on change** (don't hot-reload).
- **Restart trigger:** `stakater/reloader` operator (already installed in the cluster). Annotate the Deployment with `reloader.stakater.com/auto: "true"` (or scoped annotations naming the ConfigMap and Secrets). Reloader watches both ConfigMap and Secret resources, so Slack-token rotation also triggers a restart automatically.
- **Don't start with invalid config.**
- **CLI validator** subcommand of the same binary: `toggle-monitor validate <path>` for pre-push validation in CI.
- **CLI config-show** subcommand: `toggle-monitor config show [--monitor <slug>]` — prints the fully merged final config (preset + annotations + monitor block all resolved) for all monitors, or just the one specified. Analogous to `docker compose config`.

### Slack mechanics
- **Bot token** (`xoxb-`), stored as k8s Secret, referenced from config.
- **Channel ID** in config (not name); inline YAML comment carries the human label.
- **Single Slack destination per monitor** (spec allowed multiple, simplified for v1; revisit later).
- **`slack:` section in config:** nested block with `channels:`, `userMapping:`, and `bodyMaxChars:`. Monitors reference channels by slug.
  ```yaml
  slack:
    bodyMaxChars: 200
    channels:
      - slug: ops-alerts
        channelId: C0123ABC          # #ops-alerts
        tokenEnv: SLACK_BOT_TOKEN
  ```
- **Secrets via env vars:** the config names the env var (`tokenEnv:`, `database.passwordEnv:`); the Deployment maps k8s Secrets into env vars via `valueFrom.secretKeyRef`. Single-workspace only for v1 (validator refuses multi-workspace token sets).
- **Alert lifecycle in Slack:** parent message on down; reply per reminder; edit parent to ✅ + post reply with downtime duration on resolve. Specific message content TBD.
- **HTTP error display:** status code always; short status text inline; never response body in Slack (link to detail page); truncate.

### SSL expiry alerts
- Treated as **alerts** (own incident class, own thread), same Slack channel as uptime for the monitor.
- **Three params**, all global-with-overrides per-monitor and per-preset:
  - `sslAlertThreshold` (default 30 days) — start alerting below this
  - `sslEscalationThreshold` (default 7 days) — switch to daily reminders below this
  - `sslReminderInterval` (default 3 days) — cadence above the escalation threshold
- Auto-resolve when cert renews (TTL jumps back up).
- **Non-HTTPS URLs:** skip SSL check silently. Surface in monitor detail page, add `SSL-SKIPPED` listing filter, count in homepage stats.

### Check execution
- **One ticker per monitor**, goroutine per check. Fine for current ~200 / max ~500 monitors.
- **Startup jitter:** sleep `rand(0, interval)` before first tick to avoid thundering herd.
- **Don't follow redirects** by default (silent redirects hide misconfiguration); per-monitor `followRedirects: true` override.
- **User-Agent:** `toggle-monitor/<version>` identifier, globally configurable.
- **Event-sourced history** — write to Postgres only on state changes and alert events; no per-check rows. (If latency graphs are wanted later, that's a Prometheus job.)

### Suggested values (NOT defaults — must be explicit in config)
Reference values for writing the first config / kubePreset; the binary does not apply them.
- Interval: 5m
- Timeout: 10s
- Retries: 2
- RetryBackoff: 5s
- ReminderInterval: 3 days
- Accepted status codes: `[200]`
- HTTP method: `GET`
- SSL alert threshold: 30 days
- SSL escalation threshold: 7 days
- Follow redirects: false

### Homepage stats
- **up / down / temporary-paused / ssl-expiring / ssl-skipped**.

### Slack message content
All messages use **Block Kit** (richer layout, fields, action buttons). All timestamps use Slack's `<!date^{unix-ts}^{format}|fallback>` syntax so each viewer sees their own local time.

**Uptime parent (down):**
- Header: `🔴 <Monitor Friendly Name> is DOWN`
- Context line: group · URL
- Mentions block (per `notify:` resolution)
- Fields: status code + text, failure time, last error
- Action button: `[View details]` linking to `{publicBaseURL}/monitor/{slug}` (button omitted if `publicBaseURL` not configured)
- Optional response-body code block — see below

**Uptime reminder (thread reply):** short scannable line — `⏰ Still down for Xd. Last checked: …. Last error: ….` No mentions.

**Uptime resolve:** **preserve all original content of the parent**. Only modify:
- Header line: `🔴 … is DOWN` → `✅ … is UP (was down for Xd Yh Zm)`
- Append `Resolved at: <time>` to the fields block

Then post a thread reply: `✅ Resolved at <time>. Total downtime: Xd Yh Zm.`

**SSL parent (entering alert threshold):** ⚠️ header, fields = expires-at, issuer, subject, days remaining; action button to detail page.

**SSL reminder:** `⚠️ Still expiring — N days remaining. Renewal needed.` No mentions. Cadence per `sslReminderInterval`/`sslEscalationThreshold` rules.

**SSL resolve (cert renewed):** preserve parent, change header to `✅ Certificate renewed. New expiry: <date> (in N days).` Thread reply.

**Monitor-removed warning** (posted to the monitor's slack destination — separate message, not a thread reply):
- `⚠️ Monitor removed: <Friendly Name>`
- Group · Method · URL · Source (static config / k8s ingress with namespace/name) · Reason
- Action: link to detail page (still accessible — soft-delete preserves history)
- No mentions

**In-thread closeout** (when the removed monitor had an open incident): `ℹ️ Monitor was removed. Closing incident.` Parent gets edited to `✅ Resolved (monitor removed)` per Q6 policy.

**Response body inclusion:**
- Global config knobs:
  - `slack.bodyMaxChars`: include response body inline in Slack parent message only if body length ≤ this threshold (e.g., 200). Above the threshold, omit from Slack (user sees it on detail page).
  - `dbBodyMaxChars`: truncate body to this length before storing in Postgres (e.g., 4000). Protects against multi-MB HTML pages.
- No per-monitor flag.

**Timezone:**
- Postgres `timestamptz` for all timestamp columns (UTC internally regardless of TZ config).
- **UI** renders in a static config-defined TZ (`displayTimezone:` field, e.g., `Asia/Kathmandu`). Not derived from browser.
- **Slack** renders per viewer via `<!date^{unix-ts}^…>` syntax. Server sends raw unix timestamps; no TZ information is forwarded.

### Pagination
- **Offset-based** (`LIMIT/OFFSET`, `?page=`) for v1. Fine at expected scale; revisit only if a table outgrows ~100k rows.
- **Per-listing default page sizes** (overridable globally via config; suggested starting points):
  - Homepage latest alerts: 20
  - Monitor listing: 50
  - Per-monitor alert history: 50
  - Auto-discovery listing: 50
  - All four configurable via top-level `ui.pageSize.{listing}:` block; each value is the configurable default.
- **`?per_page=` query param** overrides per request, capped at a configurable max (suggested 200) to bound queries.
- **Sort order:**
  - Latest alerts: newest first (descending event timestamp).
  - Monitor listing: by status (down → paused → ssl-expiring → up), then group, then name — problems surface at the top.
  - Per-monitor history: newest first.
  - Auto-discovery listing: by ingress namespace, then name.
- **Search/filter state in URL params** so views are linkable/shareable. HTMX `hx-push-url="true"` on filter forms.
- **Empty state:** "no results" message with a "clear filters" link.

### Observability
- **Prometheus `/metrics` endpoint** with these series:
  - `toggle_monitor_checks_total{monitor, status="ok|fail|paused"}` (counter)
  - `toggle_monitor_check_duration_seconds{monitor}` (histogram)
  - `toggle_monitor_active_incidents{type="uptime|ssl", monitor}` (gauge)
  - `toggle_monitor_config_load_total{result="success|fail"}` (counter)
  - `toggle_monitor_slack_post_total{result="success|fail"}` (counter)
  - `toggle_monitor_ingress_reconcile_total{result="added|skipped|removed"}` (counter)
  - `toggle_monitor_worker_last_tick_seconds` (gauge)
  - Go runtime metrics via `promhttp` (goroutines, GC, memory)
- **Logging:** structured JSON via `slog` (Go stdlib). Default level `info`; toggle via `--log-level=debug` or env.
- **What's logged:**
  - INFO: state transitions, Slack post outcomes, config (re-)load summary, ingress reconcile summary, startup/shutdown.
  - DEBUG: every check result, full Slack API call detail, individual ingress events.
- **No tracing** for v1 (OTel deferred).
- **No external error reporting** (Sentry) for v1 — rely on cluster log aggregator. Easy to add later via `sentry-go`.

### Data retention
- **No retention policy for v1.** Alert history kept forever (event-sourced model keeps the table small at expected scale — ~10–100k rows/year for 200 monitors).
- **Soft-deleted monitors:** archive flag, history preserved indefinitely.
- **Auto-discovery skip/add log:** **snapshot model** — one row per observed ingress, current state (added vs skipped + reason). Updated on each reconcile; old states overwritten. Discovery UI is about "current state," not history.
- **Backups:** handled by CNPG (WAL archiving + scheduled backups). No application-side concern.
- **No manual purge CLI/endpoint.** If retention ever becomes a need, run SQL directly via psql.

### Auth, health probes, heartbeat
- **No auth.** UI exposed via ingress restricted to local network only. Anyone with network access sees the full UI.
- **K8s probe endpoints** (unauthenticated, cluster-local):
  - `/healthz` — process alive + HTTP listener responding.
  - `/readyz` — DB connection healthy + config successfully loaded at least once.
- **Outbound heartbeat** to a deadman service (healthchecks.io or equivalent):
  ```yaml
  heartbeat:
    url: https://hc-ping.com/<uuid>
    interval: 1m
    failOnStalledWorker: true
  ```
  - Every `interval`, send a **POST** to `url` with body `{"openIncidents": N, "lastTickAt": "..."}` if the worker loop is healthy.
  - **Liveness criterion:** most recent check completion (success or failure) within `max(2 × heartbeat.interval, 6 min)`. Catches scheduler stalls, DB-write deadlocks, pure deadlocks. The 6 min floor avoids false-stall alerts when the smallest monitor interval is 5 min.
  - If stalled and `failOnStalledWorker: true`, POST to `{url}/fail` (healthchecks.io convention for active-fail).
  - Generic enough to work with any deadman service that accepts a ping URL (Better Stack, Dead Man's Snitch, BetterUptime, etc.).

### UI tech stack
- **Server-rendered Go templates + HTMX**, no SPA, no separate frontend build.
- **`templ`** for type-safe components (Go-native templating language with codegen via `go generate`).
- **HTMX** for sprinkled interactivity — pagination, filters, search, expandable rows. Adds ~14KB, no NPM.
- **Tailwind (precompiled)** for styling. CSS is built once and checked in / embedded; no Node at runtime.
- **`embed.FS`** for templates, CSS, and any small static assets — single static binary, no runtime FS dependency.
- **No charts in v1.** If added later, server-render SVG from Go (no JS lib).

### Groups
- **`group:` is required on every static monitor.** No implicit fallback group for statics. CLI validator rejects monitors without a group reference, or with a group reference that doesn't resolve to a declared slug.
- **The `kube-discovered` group must be declared** in the `groups:` section (validator throws if missing) — k8s auto-discovery falls back to it when an ingress doesn't specify `config.group`.
- **Display order:** YAML array order in the `groups:` section determines listing order. No `sortIndex` field.
- **No `hidden` field.** Group cleanup happens via ArgoCD/git — removing a group entry from the config removes it.
- **Validation:** slug uniqueness across groups; every monitor's `group:` resolves to a declared slug; groups with zero monitors are allowed (no warning).

### Theme & group color
- Each group declares a **single hex string**: `color: "#3b82f6"`. UI auto-derives readable text color via contrast formula (`text-white` vs `text-black` switching based on luminance). No background/text pair, no dark/light variants in config — UI chrome handles dark mode.
- Top-level `theme.defaultGroupColor:` (also a single hex) is the fallback when a group omits `color:`. Per-group `color:` overrides it.
- This is a UI/display concern, distinct from the "no monitor defaults" stance.

### Presets & DRY
- **Renamed:** `kube.presets:` (nested under the `kube:` top-level section). Exclusively for ingress auto-discovery.
- **Static monitors** use YAML anchors (`&name` / `*name` / `<<: *name`) for DRY — no custom default mechanism. List fields don't merge across anchors (YAML behavior); they replace.
- **Anchor host blocks:** any top-level key starting with `x-` (docker-compose convention, e.g., `x-monitor-defaults:`) is ignored by the validator. Use these to hold anchored blocks without polluting the schema.
- **No hardcoded defaults** in the binary. **No global `defaults:` block.** Every required field must be explicitly set somewhere (preset, monitor, or annotation).
- **Each `kubePreset` must declare a complete set of monitoring params** so any ingress that opts in via `kube.preset` produces a valid monitor without further input. Required fields in a preset:
  - `httpMethod`, `acceptedStatusCodes`, `interval`, `timeout`, `retries`, `retryBackoff`, `followRedirects`
  - `slack` (destination slug into the `slack:` section)
  - `reminderInterval`
  - `sslAlertThreshold`, `sslEscalationThreshold`, `sslReminderInterval`
  - `path` (URL path appended to ingress host)
- **Optional fields in a preset:** `notify`, `tags`, `dependsOn`, `group` (kube-discovered monitors without group fall back to the `kube-discovered` group declared in the config).
- **Merging precedence:**
  - **kube-discovered monitor:** `kubePreset field` → `ingress annotation` → final.
  - **Static monitor:** `monitor's own field` → final. (No preset layer.)
  - **List fields** merge as a union, *except* `acceptedStatusCodes` which replaces (status codes are intentional, not additive).
- **UI:** no dedicated preset page. The monitor detail page shows the merged final config plus a provenance section (per-field: came from preset / annotation / monitor / hardcoded — though there is no "hardcoded" tier now).

### Kube monitor statuses & lifecycle
- **Full status set:** `up` / `down` / `temporary-paused` / `kube-paused` / `kube-invalid` / `ssl-expiring` / `ssl-skipped`.
- **`temporary-paused`** — any monitor whose `dependsOn` parent is currently down (not kube-specific).
- **`kube-paused`** — kube-discovered monitor whose host matched an entry in `kube.pause:`. Persistent until config changes.
- **`kube-invalid`** — every ingress in the cluster that didn't materialize a monitor. Three sub-reasons (visible in the auto-discovery UI):
  - No `kube.preset` annotation (the common case for ingresses unrelated to toggle-monitor).
  - `kube.preset` present but slug not found in `kube.presets:`.
  - Slug generation failed (per [Kube-discovered slug sanitization](#config-versioning--slug-rules)).

  No Slack alerts on individual `kube-invalid` transitions — they're surfaced via the **weekly Slack summary** (see below).
- **`ssl-skipped`** — possible only on static `http://` monitors (kube-discovered defaults to `https`).

### `kube.pause:` hard-pause list
- Declarative entries identify ingresses by **host** (not slug), with optional reason and glob support:
  ```yaml
  kube:
    pause:
      - host: api.foo.example.com
        reason: "Maintenance until 2026-06-01"
      - host: "*.staging.example.com"
  ```
- Matched ingresses materialize a monitor row with `kube-paused` status (keeps history accessible by slug) but skip all HTTP/SSL checks.
- If a monitor had an open incident when added to the pause list, run the same closeout flow as Q6 (post thread reply + edit parent with "paused via config" note).

### Weekly Slack summary (planned)
- A new `slack.summaryChannel:` slug points to the channel for periodic operational summaries.
- Initial content (subject to change): counts of `kube-invalid` ingresses, `kube-paused` count, currently-down count, recent SSL expirations, recent ingress removals.
- Full design (cadence configurability, content shape) **deferred to a later session**.

### Maintenance mode (operational, v1)
- **No first-class maintenance feature** in v1.
- During known maintenance, operator **scales the toggle-monitor Deployment to zero replicas** (`kubectl scale deploy/toggle-monitor --replicas=0`), waits for maintenance to complete, then scales back up.
- **Important:** the heartbeat to healthchecks.io will stop while scaled down — pre-pause the corresponding healthchecks.io check (their UI / API supports a "pause" action) before scaling, then resume after. Otherwise the deadman alert fires.
- Any state transitions that occurred during the gap are lost (the worker wasn't running to observe them).
- A proper time-bounded maintenance-mode feature is deferred (see open branches).

### K8s auto-discovery scope
- **Cluster-wide RBAC.** ServiceAccount with `ClusterRole` granting `get/list/watch` on `networking.k8s.io/v1` Ingress objects. No namespace restriction.
- **Single cluster.** Multi-cluster aggregation is out of scope; deploy one instance per cluster if needed.
- **Ingress API version:** `networking.k8s.io/v1` only (stable since k8s 1.19). No support for deprecated `extensions/v1beta1`.
- **No filtering at the watch level.** Every ingress is observed and recorded in the auto-discovery snapshot table.
  - Ingresses with `<base>/kube.preset` materialize into active monitors.
  - Ingresses without are recorded with reason `"no preset annotation"`. The auto-discovery UI exposes both views per the original spec ("list monitors auto added" / "list monitors not added").
- **Resync interval:** default 30 min via `client-go` informer. **Configurable** via `kube.resyncInterval:` in config.
- **Multi-host ingresses:** create one monitor per **unique** `host` in `spec.rules[].host`. URL = `{scheme}://{host}{preset.path}`.
- **Scheme:** kube-discovered URLs default to `https://` (cluster sits behind external bastion handling TLS termination). Preset can override with `scheme: http` for clusters without bastion TLS. Earlier `SSL-SKIPPED` rule still applies to static monitors with `http://` URLs; kube-discovered with default https never triggers it.
- **TLS info from `ingress.spec.tls` is ignored** — the bastion architecture means the ingress is HTTP internally but HTTPS publicly.

### K8s ingress annotation convention
- **Base domain:** `monitor.togglecorp.com` by default, **configurable via `kube.annotationDomain:`** in the config.
- **Nested keying:**
  - `{baseDomain}/kube.<field>` — discovery-time hints (used only at ingress watch / URL construction)
  - `{baseDomain}/config.<field>` — monitor-config overrides (same fields you'd set in a YAML `Monitor:` block)
- **Annotations only, no labels.** Trade-off: gives up `kubectl get -l ...` queryability, but avoids label+annotation confusion and keeps a single consistent mechanism. The auto-discovery UI provides preset/tag filtering in lieu of kubectl querying.
- **Annotation set:**
  | Annotation | Purpose |
  |---|---|
  | `<base>/kube.preset` | Preset slug. **Presence of this annotation is the opt-in trigger** — without it, the ingress is ignored. |
  | `<base>/kube.path` | URL path override during discovery |
  | `<base>/config.group` | Group slug override |
  | `<base>/config.dependsOn` | Comma-separated parent monitor slugs |
  | `<base>/config.notify` | Comma-separated mention slugs |
  | `<base>/config.tags` | Comma-separated extra tags (`kube` is always auto-added) |
  | `<base>/config.enabled=false` | Opt out without removing other annotations |
- **Merging:** preset is base; annotation overrides/extends per Q5d.

### Slack user mentions
- **Per-monitor + per-group + per-preset**, merged as a **union** of all applicable mention values.
- **`slack.userMapping:` section** in config maps a user-defined slug to a Slack user ID (`U…`) or subteam ID (`S…`):
  ```yaml
  slack:
    userMapping:
      alice: U0123ABC
      ops-team: S0456DEF
  ```
- **Resolution algorithm** in a `notify:` list:
  1. Value matches a slug in `slack.userMapping` → resolve to ID, emit `<@U…>` (user) or `<!subteam^S…>` (subteam) markup based on prefix.
  2. Value wrapped in `<…>` → pass through as raw Slack markup (supports `<!here>`, `<!channel>`, etc.).
  3. Otherwise → reject at config-load.
- **When mentions fire:** parent down message only. Reminder/resolve/warning messages are quiet. (Mentioned users who engage with the thread become followers; if reminder visibility proves insufficient in practice, revisit.)
- **SSL alerts** use the same audience as uptime for v1.
- **Auto-discovery:** annotation `toggle-monitor.notify=<slug>[,<slug>...]` on the ingress; preset can declare `notify:`; merging follows the same preset-base + annotation-overrides rules.
- **Validation:**
  - CLI validator format-checks slugs and raw `<…>` tokens.
  - Runtime validates `slack.userMapping` IDs against Slack on startup and re-validates every 24h. Caches result with last-checked timestamp.
  - If local Slack auth isn't available (CLI run without token), format check only; emit a warning.
  - UI exposes a section listing invalid/unknown mapping entries with reason + last-checked time (uses the cache; never hits Slack on UI request).

### Dependency monitors (avalanche suppression)
- **`dependsOn: [slugs]`** on a regular monitor lists upstream parents that gate it. **Parents must be static monitors** (kube-discovered monitors cannot be parents) so the dependency graph is fully resolvable at config-load. No separate "global monitor" class — keeps the data model unified. (A `role: gateway` UI-sugar field was considered and deferred.)
- **Any-down pauses:** if any listed parent is currently down, the child is paused.
- **Pause semantics:** skip the check entirely (no HTTP request, no SSL check, no DB write).
- **Status:** `temporary-paused`. Detail page surfaces which parent(s) is gating. Homepage stat tile counts these.
- **Existing incidents when a parent goes down:** no special handling for v1 — when the parent recovers, child checks resume as normal. (Revisit if this proves noisy in practice.)
- **Cycle detection** at config-load. CLI validator refuses cyclic graphs.
- **Auto-discovery interaction:** preset can declare `dependsOn`; ingress annotation `<annotationDomain>/config.dependsOn=<slug>[,...]` can override/extend. Same merging rules as other preset/annotation fields.

## Open branches

Design grilling complete. Full per-field schema lives in [`config-schema.md`](./config-schema.md).

**Deferred to implementation phase:**

1. **Snapshot tests for kube ingress → monitor materialization** — guard against regressions in the discovery pipeline (preset merging, annotation overrides, slug sanitization, multi-host fan-out).

**Deferred to revisit when there's a concrete need:**

2. **Weekly Slack summary** — exact content, cadence configurability, format. `slack.summaryChannel:` is the destination; the implementation/design is open.
3. **First-class maintenance mode** — time-bounded ad-hoc pause via CLI or YAML windows. v1 uses scale-to-zero instead.
4. `role: gateway` field as UI sugar to visually group dependency parents.
5. Multiple Slack destinations per monitor (currently single per Q8c).
6. Special handling for in-flight child incidents when an upstream parent goes down (currently no-op per Q11e).
7. Multi-workspace Slack support (currently rejected at startup if tokens span workspaces).
8. Latency graphs / per-check rows (event-sourced model deliberately drops this; if added, do it via Prometheus, not Postgres).
9. Data retention policy (currently no expiry; revisit when alert history table grows large enough to matter).
