# toggle-monitor — v1 issues

Tracer-bullet vertical slices derived from [`prd-v1.md`](./prd-v1.md). Each slice cuts end-to-end through config → DB → scheduler/checker → state → output, and is independently demoable.

Two are marked **HITL** (bootstrap + tracer bullet — both crystallize architectural choices and deserve human review). The rest are **AFK**.

References:
- [`docs/prd-v1.md`](./prd-v1.md) — the v1 PRD
- [`docs/design-decisions.md`](./design-decisions.md) — every resolved design decision
- [`docs/config-schema.md`](./config-schema.md) — per-field schema
- [`docs/initial-spec.md`](./initial-spec.md) — original brief
- [`docs/config-example.yaml`](./config-example.yaml) — hand-written example

---

## Issue 1 — Project bootstrap & scaffolding (HITL)

### What to build

Initialize the Go module and lock the dependency surface for v1. No functional behavior — the binary should build, boot, parse subcommands, print help, and exit. This issue exists so every later slice has a stable foundation.

Includes:

- Go module init; pin a single Go toolchain version.
- Locked dependency list: `pgx` (Postgres driver), `golang-migrate/migrate` (library form, SQL files), `client-go` (Ingress informer), `templ` (templating, with `go generate`), `promhttp` (Prometheus), `slog` (stdlib), `yaml.v3` (config), `cobra` or stdlib `flag` (CLI — decide here).
- Directory layout (internal modules per PRD: `config`, `slug`, `secret`, `db`, `migrate`, `store`, `kube`, `merger`, `scheduler`, `httpcheck`, `sslinspect`, `alert`, `slack`, `heartbeat`, `web`, `lifecycle`, `observability`). Empty packages OK.
- `justfile` with `build`, `test`, `lint`, `templ`, `tailwind` recipes (originally a Makefile; see ADR 0001 for the switch).
- CI workflow (GitHub Actions) running build + lint + tests on PR.
- Tailwind precompile pipeline checked in or scripted (no Node at runtime; CSS embedded via `embed.FS`).
- CLI structure: `toggle-monitor` (default = `serve` placeholder), `validate`, `config show`, `migrate`, `migrate --check` — all subcommands wired to placeholder handlers that print "not yet implemented" and exit 0.
- Linter config (e.g., `golangci-lint`) with sensible defaults.
- Empty `embed.FS` placeholders for templates, CSS, migrations.

### Acceptance criteria

- [ ] `just build` produces a single static binary
- [ ] `just test` and `just lint` pass on an empty test suite
- [ ] `toggle-monitor --help` lists every documented subcommand
- [ ] Every subcommand prints "not yet implemented" and exits 0
- [ ] CI runs build + lint + tests on PR
- [ ] No runtime dependency on Node or any external CSS pipeline; CSS is embedded
- [ ] All 17 module package directories exist (even if empty)
- [ ] Dependency choices documented (a brief ADR or README section) — covers `cobra` vs stdlib `flag`, `pgx` vs `pq`, and the `templ` + HTMX + Tailwind approach

### Blocked by

None — can start immediately.

---

## Issue 2 — Tracer bullet: single static monitor → UI status (HITL)

### What to build

The thinnest end-to-end slice that proves the architecture. Load a YAML with one static HTTP monitor and the required `kube-discovered` group placeholder, run a recurring HTTP check, persist transitions in Postgres, and render current state in the UI. No Slack, no SSL alerts, no kube, no anchors, no env interpolation, no provenance, no archive UI.

Includes:

- Config loader (subset): parse YAML, schema-validate the required fields per `config-schema.md` for `displayTimezone`, `publicBaseURL` (optional), `dbBodyMaxChars`, `database.*`, `ui.pageSize.*`, `ui.maxPerPage`, `theme.defaultGroupColor`, `httpClient.userAgent`, `groups[]` (must include `kube-discovered`), `monitors[]`. Reject missing/unknown fields.
- Cross-field validator: `retries × (timeout + retryBackoff) < interval`; `timeout < interval`; SSL fields required iff URL is HTTPS (but SSL alerts are out of this slice).
- Schema migration v1 (golang-migrate, SQL files in `embed.FS`): tables for `monitors` (current state + archive flag), `alert_events` (event-sourced append-only), `schema_version`. App refuses to start if schema version doesn't match.
- DB connection: startup backoff (~60s exp) then exit; runtime tolerance — worker writes retry 3× over 30s then log loudly and continue; UI reads render a "DB unavailable" page on failure.
- Scheduler: one ticker per monitor, startup jitter `rand(0, interval)` before first tick, in-cycle retries gated by validator rule, ctx-cancel-aware.
- HTTP check executor: respects `httpMethod`, `acceptedStatusCodes`, `timeout`, `followRedirects`, `httpClient.userAgent`.
- Alert state machine (uptime only): `up` ↔ `down` transitions emit alert events to Postgres. No reminders yet (Slack is out of scope here).
- Web UI: homepage with stats tiles (`up`, `down`); paginated monitor listing sorted by status → group → name; monitor detail page showing current status + last error + raw final config (no provenance yet).
- `/healthz` (process+listener) and `/readyz` (DB + config loaded once).
- `templ` components + precompiled Tailwind CSS + HTMX loaded (even if filters aren't wired yet).
- Single static binary; everything embedded.

### Acceptance criteria

- [ ] Given a YAML with one static monitor pointing at `http://localhost:<port>` and a running mock, the worker performs HTTP checks at the configured interval
- [ ] On the first failure, an `alert_event` row is written for `up → down`; on recovery, another for `down → up`
- [ ] In-cycle retries within one tick do not produce intermediate alert events
- [ ] Homepage shows correct up/down stats and the monitor appears in the listing under the right group
- [ ] Monitor detail page at `/monitor/<slug>` shows current state and the merged final config
- [ ] `/healthz` returns 200 when listener is up; `/readyz` returns 200 only after DB is connected and config has loaded once
- [ ] Schema version mismatch refuses to start with a clear error pointing the operator at `toggle-monitor migrate`
- [ ] Runtime DB outage does not crash the worker (writes log and continue); UI shows a friendly error page
- [ ] Validator rejects a config that violates `retries × (timeout + retryBackoff) < interval`
- [ ] Integration test covers the full path: YAML → check → DB → UI

### Blocked by

- Issue 1 — Project bootstrap & scaffolding

---

## Issue 3 — Slack uptime alert lifecycle (AFK)

### What to build

Add Slack output to the uptime state machine. Implement the parent-down / threaded-reminder / resolve-via-parent-edit lifecycle from the PRD. Mention rendering is limited to raw `<…>` markup in this slice; the `userMapping` slug vocabulary lands in Issue 13.

Includes:

- `slack:` config block: `bodyMaxChars`, `channels[]` (slug, channelId, tokenEnv). Validate `channelId` matches `^[CG][A-Z0-9]{8,}$`, DMs (`D…`) rejected.
- `SecretString` type with `slog.LogValuer`: length ≥ 8 → `<first2>****<last2>`, length < 8 → `****`; fixed 4 asterisks.
- Env var contract: every `tokenEnv` / `passwordEnv` env var must be set and non-empty at startup.
- Single-workspace check at startup: `auth.test` for every distinct token; all must return the same `team_id`. Transient `auth.test` failure → warning (cached, hourly re-check, UI surfaced), not a startup blocker.
- Block Kit message builders for uptime:
  - Parent (down): header `🔴 <Name> is DOWN`, context (group · URL), mention block (raw `<…>` only for now), fields (status code+text, failure time, last error), optional inline body (if size ≤ `bodyMaxChars`), `[View details]` button when `publicBaseURL` is configured.
  - Reminder: short thread reply per `reminderInterval` while still down; no mentions.
  - Resolve: edit the parent (preserve all content; only header → `✅ … is UP (was down for Xd Yh Zm)`; append "Resolved at"); thread reply with downtime total.
- Thread refs in DB (table or columns; persist message ts + channel for each open incident). Slack thread-ref retry-then-fresh-parent on persistent lookup failure.
- All timestamps emitted as `<!date^{unix}^{format}|fallback>`.
- Body truncation for storage: cap at `dbBodyMaxChars`; enforce `dbBodyMaxChars >= slack.bodyMaxChars`.
- Wire `notify:` lists at monitor/group level (raw `<…>` markup only; merging is union; reject unknown non-markup entries at config-load for now).

### Acceptance criteria

- [ ] `up → down` transition posts a Block Kit parent message; thread ref stored in DB
- [ ] While down, a thread reply is posted at `reminderInterval` cadence (no mentions on reminders)
- [ ] `down → up` transition edits the parent message in place (content preserved, header rewritten, "Resolved at" appended) and posts a thread reply with downtime
- [ ] In-cycle retries within one tick produce no Slack output
- [ ] Inline body included only when ≤ `bodyMaxChars`; never above that threshold
- [ ] DM channel IDs rejected at config-load
- [ ] Startup refuses if tokens span multiple Slack workspaces
- [ ] Transient `auth.test` failure surfaces in UI but does not block startup
- [ ] `SecretString` masks values in logs per the documented form
- [ ] `[View details]` button omitted when `publicBaseURL` is unset
- [ ] Slack timestamps render as viewer-local time via `<!date^…>`

### Blocked by

- Issue 2 — Tracer bullet

---

## Issue 4 — SSL inspection + SSL alert thread (AFK)

### What to build

Add SSL as its own incident class with its own thread on the same channel as uptime. Three thresholds drive the cadence; auto-resolve when the cert renews.

Includes:

- TLS cert extraction during the HTTP check (capture issuer, subject, NotAfter).
- SSL state machine with thresholds: `sslAlertThreshold` (begin alerting below this, default 30d), `sslEscalationThreshold` (daily reminders below this, default 7d), `sslReminderInterval` (cadence above the escalation threshold, default 3d).
- Status `ssl-expiring` while in the alert window; `ssl-skipped` for static `http://` monitors (kube-discovered defaults to https and never triggers this).
- SSL parent message (Block Kit): `⚠️` header, fields = expires-at, issuer, subject, days remaining; `[View details]` button.
- SSL reminder: `⚠️ Still expiring — N days remaining. Renewal needed.` No mentions.
- SSL auto-resolve when cert TTL jumps back up (cert renewed): preserve parent, change header to `✅ Certificate renewed. New expiry: <date> (in N days).` Thread reply.
- Validator: SSL fields required when URL is HTTPS, allowed-but-ignored when HTTP (so anchors can be shared); `sslAlertThreshold > sslEscalationThreshold > 0`; `sslReminderInterval >= 1h`.
- Homepage stats now include `ssl-expiring` and `ssl-skipped`.

### Acceptance criteria

- [ ] Cert TTL crossing `sslAlertThreshold` posts an SSL parent (separate thread from uptime)
- [ ] Cert TTL crossing `sslEscalationThreshold` switches reminder cadence to daily
- [ ] Cert renewal auto-resolves (parent edit + thread reply)
- [ ] Static `http://` monitors show `ssl-skipped`; kube-discovered with default https never shows `ssl-skipped`
- [ ] SSL fields ignored at runtime for HTTP-only monitors (no startup error if present)
- [ ] Validator rejects an HTTPS monitor missing required SSL fields
- [ ] Validator rejects `sslAlertThreshold <= sslEscalationThreshold`
- [ ] Homepage stats correctly tally `ssl-expiring` and `ssl-skipped`

### Blocked by

- Issue 3 — Slack uptime alert lifecycle

---

## Issue 5 — Config polish: anchors, `x-*` ignore, env interpolation, multi-error reporting (AFK)

### What to build

Lift the minimal Issue-2 config loader to the full schema behavior: YAML anchors, `x-*` top-level ignore, `${VAR}` / `${VAR:-default}` / `$$` interpolation, line-numbered errors, and multi-error reporting.

Includes:

- YAML anchors (`&name`, `*name`, `<<: *name`) work for static monitors and any other repeated blocks. Document the YAML default: list fields don't merge across anchors — they replace.
- Top-level keys prefixed `x-` are ignored by the validator (docker-compose convention). Any other unknown top-level key is a hard error.
- `${VAR}` strict — error at parse time if unset (validator reports file/line). `${VAR:-fallback}` uses fallback if unset or empty. `$$` escapes to literal `$`. Works on any string scalar. For non-string fields, quote the value (`port: "${DB_PORT:-5432}"`) so YAML sees a string first.
- Secrets are not interpolatable — `tokenEnv` / `passwordEnv` remain the only way to pass a secret.
- Errors include file line numbers from `yaml.v3` node positions. Format: `config.yaml:42: monitors[0].interval must be >= 30s, got 10s`.
- Multi-error reporting: do not exit on the first error; accumulate and report all in one run.

### Acceptance criteria

- [ ] A YAML using `x-monitor-defaults: &staticDefaults …` and `<<: *staticDefaults` loads correctly
- [ ] `x-foo:` at top level is silently ignored; `monitor:` (typo for `monitors:`) is a hard error
- [ ] `${HOME}` resolves to the env value at parse time
- [ ] `${UNSET_VAR}` produces a line-numbered error
- [ ] `${UNSET_VAR:-fallback}` resolves to `fallback`
- [ ] `${SET_BUT_EMPTY:-fallback}` resolves to `fallback`
- [ ] `$$` resolves to a literal `$`
- [ ] An attempt to interpolate a value into a `tokenEnv` or `passwordEnv` field is rejected
- [ ] A config with three distinct validation errors reports all three with line numbers in one run

### Blocked by

- Issue 2 — Tracer bullet

---

## Issue 6 — `validate` and `config show` CLI subcommands (AFK)

### What to build

Wire the full config loader + validator + merger to the standalone CLI subcommands.

Includes:

- `toggle-monitor validate <path>` — run the same validator as boot; exit non-zero with the same multi-error output if invalid; intended for pre-push CI.
- `toggle-monitor config show [--monitor <slug>]` — print the fully merged final config for every monitor (preset + annotations + monitor block resolved). With `--monitor <slug>`, print only that monitor. Analogous to `docker compose config`. For kube-discovered monitors, this requires loading the current ingress snapshot (use the live cluster or accept a `--from-snapshot <file>` flag — decide and document).
- Both subcommands work without DB or Slack connectivity (validator format-checks slugs and raw `<…>` markup, warns on missing Slack auth instead of erroring).

### Acceptance criteria

- [ ] `toggle-monitor validate path/to/config.yaml` exits 0 on valid input and prints nothing
- [ ] `toggle-monitor validate path/to/bad.yaml` exits non-zero and reports all errors with line numbers
- [ ] `toggle-monitor config show` prints YAML of every monitor's merged final config
- [ ] `toggle-monitor config show --monitor foo` prints just `foo`'s merged config; missing slug → non-zero exit with clear error
- [ ] Both commands run with no Slack connectivity; format-checks only, with a warning printed for any check that needs Slack
- [ ] Both commands run with no DB connection

### Blocked by

- Issue 5 — Config polish

---

## Issue 7 — `dependsOn` + `temporary-paused` (AFK)

### What to build

Add cross-monitor dependencies. Parents must be static monitors (kube-discovered cannot be parents). When any parent is down, the child status becomes `temporary-paused` and the check is skipped entirely.

Includes:

- `monitors[].dependsOn: [slug, …]` accepted in static monitor config; each must resolve to a static-monitor slug.
- Cycle detection at config-load; refuse cyclic graphs with a clear multi-error report.
- Runtime gating: before each tick, evaluate parent state. If any parent is `down`, skip the check entirely (no HTTP, no SSL, no DB write); set status `temporary-paused`.
- Monitor detail page surfaces which parent(s) is gating.
- Homepage stats include `temporary-paused` tile.
- No special handling for in-flight child incidents when a parent goes down (per design Q11e). When the parent recovers, child checks resume.

### Acceptance criteria

- [ ] Validator rejects `dependsOn` pointing to an unknown slug
- [ ] Validator rejects a cyclic dependency graph and reports the cycle
- [ ] Validator rejects `dependsOn` pointing to a kube-discovered slug (when materialized) — that surfaces at reconcile time for kube monitors
- [ ] While a parent is `down`, child monitors show `temporary-paused`; no HTTP request is made; no `alert_event` rows are written
- [ ] On parent recovery, the next tick performs a normal check for the child
- [ ] Detail page lists the parent(s) currently gating the child

### Blocked by

- Issue 2 — Tracer bullet

---

## Issue 8 — Kube informer + auto-discovery snapshot (observe-only) (AFK)

### What to build

Stand up the Kubernetes informer and the auto-discovery snapshot table. Every observed Ingress is recorded; no monitor is materialized yet (all rows are `kube-invalid: no preset annotation` until Issue 9). This isolates the watcher plumbing from the merger logic.

Includes:

- `client-go` informer over `networking.k8s.io/v1` Ingress resources, cluster-wide.
- ClusterRole + ClusterRoleBinding manifest in the deployment chart for `get/list/watch` on Ingress.
- Config: `kube.annotationDomain` (default `monitor.togglecorp.com`), `kube.resyncInterval` (default 30m, min 1m).
- Schema migration: `discovery_snapshot` table (one row per observed Ingress, identified by `ns/name/host`), current state only — overwritten on reconcile.
- On every reconcile, write rows with status `kube-invalid` and reason `"no preset annotation"` (the actual materialization comes in Issue 9).
- Metric `toggle_monitor_ingress_reconcile_total{result="added|skipped|removed"}` wired (numbers will be all "skipped" until Issue 9).
- Multi-host Ingress: produce one snapshot row per unique `host` in `spec.rules[].host`.

### Acceptance criteria

- [ ] On startup, every Ingress in the cluster appears in `discovery_snapshot` with status `kube-invalid` and reason `no preset annotation`
- [ ] Adding or removing an Ingress in the cluster updates the snapshot on the next reconcile (or sooner via the informer)
- [ ] A multi-host Ingress produces N rows, one per `host`
- [ ] Resync interval is configurable; default 30m
- [ ] ClusterRole shipped in chart and verified in an integration test or kind-cluster smoke test
- [ ] `toggle_monitor_ingress_reconcile_total{result="skipped"}` increments per reconcile

### Blocked by

- Issue 2 — Tracer bullet

---

## Issue 9 — Preset + annotation merging → materialized kube monitors (AFK)

### What to build

Promote the snapshot-only watcher into a real materializer. Parse the documented annotations, merge with `kube.presets[]`, and produce active monitor rows for ingresses that opt in.

Includes:

- `kube.presets[]` config block. Required preset fields: `scheme`, `path`, `httpMethod`, `acceptedStatusCodes`, `interval`, `timeout`, `retries`, `retryBackoff`, `followRedirects`, `slack`, `reminderInterval`, `sslAlertThreshold`, `sslEscalationThreshold`, `sslReminderInterval`. Optional: `notify`, `tags`, `dependsOn`, `group`.
- Annotation set on `<kube.annotationDomain>/`: `kube.preset` (opt-in trigger), `kube.path`, `config.group`, `config.dependsOn`, `config.notify`, `config.tags`, `config.enabled=false`. Annotations only (no labels).
- Merger module: preset is base, annotations override or extend per field. List fields union *except* `acceptedStatusCodes`, which replaces. Track per-field provenance.
- Kube-discovered slug = `kube-{ns}-{name}-{host-as-slug}` with sanitization (lowercase, invalid → `-`, collapse `-`, strip leading/trailing `-`). Empty result → snapshot row with reason `slug generation failed`, no monitor.
- Multi-host fan-out: one monitor per unique `host`.
- `kube` tag auto-appended.
- `scheme: https` default; preset can override to `http`. `ingress.spec.tls` ignored.
- `config.enabled=false`: snapshot row with reason `opt-out via config.enabled=false`, no monitor.
- Static-vs-kube slug collision: static wins; kube slug recorded in snapshot with reason `slug conflicts with static monitor`.
- Snapshot row reasons enumerated: `no preset annotation`, `unknown preset slug`, `slug generation failed`, `opt-out via config.enabled=false`, `slug conflicts with static monitor`, `added` (when materialized).
- `dependsOn` annotations restricted to static parents; validator rejects at config-load time when the preset declares a kube parent; runtime rejects at reconcile when an annotation supplies a kube-discovered parent (surfaces as `kube-invalid` with reason).
- `kube-discovered` group fallback when neither preset nor `config.group` annotation supplies a group (group must be declared per Issue 2's required `kube-discovered` group).
- Metric `toggle_monitor_ingress_reconcile_total{result="added|skipped|removed"}` now counts correctly.
- Snapshot-test golden files for the merger (per design-decisions Open branches #1).

### Acceptance criteria

- [ ] An Ingress with `<base>/kube.preset: internal-api` materializes into an active monitor with the preset's params
- [ ] `<base>/config.group=production-apis` overrides the preset's group
- [ ] `<base>/config.tags=foo,bar` unions with the preset's tags (and `kube` is always present)
- [ ] `<base>/config.acceptedStatusCodes=[200,201]` would NOT be supported via annotation in v1 (status codes only come from preset, replace semantics within the preset itself) — confirm in code with an explicit annotation rejection if attempted
- [ ] An Ingress with `kube.preset: unknown` produces a snapshot row with reason `unknown preset slug` and no monitor
- [ ] An Ingress whose namespace/name/host produces an empty sanitized slug → snapshot reason `slug generation failed`
- [ ] A kube slug colliding with a static slug → static wins, snapshot reason `slug conflicts with static monitor`
- [ ] `<base>/config.enabled=false` → snapshot reason `opt-out via config.enabled=false`, no monitor
- [ ] Multi-host Ingress with two hosts produces two monitors; each gets its own slug
- [ ] `kube` tag is automatically present on every materialized kube monitor
- [ ] Golden snapshot tests cover: single-host happy path, multi-host fan-out, unknown preset, slug failure, `enabled=false`, scheme override
- [ ] `toggle_monitor_ingress_reconcile_total{result="added"}` reflects materialized monitors

### Blocked by

- Issue 3 — Slack uptime alert lifecycle (materialized monitors will alert via Slack)
- Issue 8 — Kube informer + auto-discovery snapshot

---

## Issue 10 — `kube.pause:` hard-pause list (AFK)

### What to build

Add a declarative pause list keyed by host (with glob support). Matching ingresses materialize a monitor row with status `kube-paused` (preserves history) but skip all checks. If a monitor had an open incident when added to the pause list, run the closeout flow.

Includes:

- `kube.pause[]` config: list of entries with `host` (required, may include `*` glob), `reason` (optional).
- Matching: glob applied to `ingress.spec.rules[].host`. Order matters? No — any match wins.
- Status `kube-paused` for matched monitors; persistent until config changes; preserve history.
- Skip all HTTP and SSL checks; no DB transition writes per tick.
- On adding to the pause list while an incident is open: post a thread reply (`ℹ️ Monitor paused via config. Closing incident.`) and edit the parent to `✅ Resolved (paused via config)`.
- Snapshot row reason `kube-paused: <reason>` (or just `kube-paused` if reason omitted).
- UI: monitor detail surfaces the pause reason.

### Acceptance criteria

- [ ] An Ingress matching `kube.pause[].host` exactly materializes with status `kube-paused`
- [ ] A glob entry like `*.staging.example.com` matches multi-host wildcards
- [ ] Paused monitors do not perform HTTP or SSL checks
- [ ] Adding a host with an open incident to the pause list closes the incident (parent edit + thread reply)
- [ ] Snapshot row reflects the pause reason

### Blocked by

- Issue 9 — Preset + annotation merging

---

## Issue 11 — Monitor removal: soft-delete, warning, closeout (AFK)

### What to build

Handle the disappearance of a monitor — both static (removed from YAML) and kube-discovered (ingress removed from cluster). Soft-delete with archive flag, slug reuse reattaches to old history, post the right Slack messages.

Includes:

- Soft-delete: archive flag on `monitors`; history preserved; new monitor with the same slug reattaches.
- Static monitor removed from YAML at restart: on detection, if currently down, post a thread reply (`ℹ️ Monitor was removed. Closing incident.`) and edit the parent to `✅ Resolved (monitor removed)`. Separately, post a non-threaded warning message to the monitor's Slack destination (with friendly name, group, method, URL, source = "static config", link to detail page if `publicBaseURL` set).
- Ingress removed from cluster: same behavior; warning's source field is `k8s ingress (ns/name)`; reason `k8s ingress removed`.
- Discovery annotation removed (`kube.preset` removed from a previously-discovered ingress): same behavior; reason `kube.preset annotation removed`.
- The thread closeout and the warning are NOT cross-linked (intentional, per design Q6).
- No mentions on the warning or the closeout.
- Detail page remains accessible (soft-delete preserves the row).

### Acceptance criteria

- [ ] Removing a static monitor from YAML at restart soft-deletes it (archive flag set)
- [ ] If the removed static monitor was down, a thread closeout reply is posted and the parent is edited
- [ ] A non-threaded warning is also posted to the monitor's Slack destination with source = static config
- [ ] Removing an Ingress with `kube.preset` from the cluster produces the same flow with source = `k8s ingress (ns/name)` and reason `k8s ingress removed`
- [ ] Removing the `kube.preset` annotation from an Ingress (Ingress still present) produces the same flow with reason `kube.preset annotation removed`
- [ ] Slug reuse: a removed monitor's slug coming back in YAML/cluster reattaches to the old history (archive flag cleared, history preserved)
- [ ] Detail page for an archived monitor remains accessible

### Blocked by

- Issue 9 — Preset + annotation merging

---

## Issue 12 — Auto-discovery UI + monitor-detail provenance + archive filter (AFK)

### What to build

UI surface for the discovery snapshot and the merged-config provenance, plus an archive filter on the monitor listing.

Includes:

- Auto-discovery listing page (`/discovery`): paginated table of every snapshot row with columns for ingress (ns/name/host), status (added / kube-paused / kube-invalid:reason), preset, materialized monitor link (if added), pause reason. Filters by status and by sub-reason.
- Per-ingress detail page (`/discovery/<ns>/<name>/<host>`): full annotation set as read from the Ingress, the resolved preset (if any), the materialized monitor link, the reason if not materialized.
- Monitor detail provenance section: per-field source (came from `preset:<slug>` / `annotation:<full annotation key>` / `monitor`).
- Archive filter on monitor listing: a query-string filter that includes/excludes soft-deleted monitors.
- Stats on the auto-discovery page: total, added, kube-paused, kube-invalid (with sub-reason breakdown).

### Acceptance criteria

- [ ] `/discovery` lists every observed Ingress with current status
- [ ] Filtering by status (`added` / `kube-paused` / `kube-invalid:no preset annotation` etc.) works and is encoded in URL params
- [ ] Per-ingress detail page shows the raw annotation set and the resolved decision
- [ ] Monitor detail page now includes a provenance section showing each field's source
- [ ] Monitor listing supports `?archived=true` (or equivalent) to include archived monitors; default hides them
- [ ] Stats tiles on `/discovery` reflect current snapshot

### Blocked by

- Issue 9 — Preset + annotation merging
- Issue 11 — Monitor removal

---

## Issue 13 — `slack.userMapping` + mention resolution + periodic revalidation (AFK)

### What to build

Lift the Issue-3 raw-markup-only `notify:` behavior into the full vocabulary. Add the `userMapping` config block, the resolution algorithm, and periodic revalidation against Slack.

Includes:

- `slack.userMapping` config: slug → `^[US][A-Z0-9]{8,}$` ID. `U…` = user (emit `<@U…>`); `S…` = subteam (emit `<!subteam^S…>`).
- Resolution algorithm in a `notify:` list:
  1. Value matches a `userMapping` slug → resolve to ID, emit `<@U…>` or `<!subteam^S…>` based on prefix.
  2. Value wrapped in `<…>` → pass through verbatim (supports `<!here>`, `<!channel>`, etc.).
  3. Otherwise → reject at config-load.
- `notify:` lists merge as union across monitor + group + preset (group-level `notify:` introduced here — extend the `groups[]` schema accordingly).
- Mentions fire on the parent-down message only; reminders / resolves / warnings remain quiet.
- SSL alerts use the same `notify:` audience as uptime for v1.
- Startup + every 24h: validate every `userMapping` ID against Slack (`users.info` / `usergroups.list`); cache result with `last_checked_at`.
- If local Slack auth isn't available (CLI run without token), format-check only and emit a warning.
- UI: invalid/unknown mapping section listing entries with reason and last-checked time. Uses cache; never hits Slack on UI request.
- Runtime catches workspace mismatch (a `userMapping` ID that's not in our workspace) at first post attempt and logs an error.

### Acceptance criteria

- [ ] A `notify: [alice]` entry resolves to `<@U…>` markup when `slack.userMapping.alice` is set
- [ ] A `notify: [ops-team]` entry resolves to `<!subteam^S…>` markup when the value starts with `S`
- [ ] A `notify: ["<!here>"]` entry passes through verbatim
- [ ] A `notify: [unknown-slug]` entry is rejected at config-load with a line-numbered error
- [ ] Mentions are present only on the parent-down message; verified absent on reminder, resolve, warning, SSL parent/reminder/resolve
- [ ] `userMapping` IDs are re-validated against Slack every 24h; the UI shows last-checked timestamp
- [ ] UI invalid-mapping panel renders correctly when an ID is unknown
- [ ] CLI `validate` runs without Slack auth and emits a warning instead of erroring on the mapping check
- [ ] Union merging of `notify:` across preset + group + monitor verified by snapshot test

### Blocked by

- Issue 3 — Slack uptime alert lifecycle

---

## Issue 14 — Prometheus `/metrics` (AFK)

### What to build

Expose all documented Prometheus series plus Go runtime metrics.

Includes:

- `/metrics` endpoint via `promhttp`.
- Series (with the documented labels):
  - `toggle_monitor_checks_total{monitor, status="ok|fail|paused"}` (counter)
  - `toggle_monitor_check_duration_seconds{monitor}` (histogram)
  - `toggle_monitor_active_incidents{type="uptime|ssl", monitor}` (gauge)
  - `toggle_monitor_config_load_total{result="success|fail"}` (counter)
  - `toggle_monitor_slack_post_total{result="success|fail"}` (counter)
  - `toggle_monitor_ingress_reconcile_total{result="added|skipped|removed"}` (counter — already partially wired by Issue 8/9; finalize labels here)
  - `toggle_monitor_worker_last_tick_seconds` (gauge — also feeds the heartbeat liveness in Issue 15)
- Default Go runtime metrics (goroutines, GC, memory).
- Document the metric reference in the repo (a small `docs/metrics.md` or a section in the README).

### Acceptance criteria

- [ ] `curl /metrics` returns Prometheus text format with all documented series
- [ ] Every series uses the documented labels
- [ ] Go runtime metrics are present
- [ ] A documented metric reference exists in the repo
- [ ] Cardinality concern (`monitor` label) acknowledged — series scoped to known slugs only

### Blocked by

- Issue 2 — Tracer bullet

---

## Issue 15 — Heartbeat + stalled-worker detection (AFK)

### What to build

Outbound deadman heartbeat to a generic ping URL with `{openIncidents, lastTickAt}`. Detect a stalled worker and POST to `{url}/fail` (healthchecks.io convention) when configured.

Includes:

- Optional top-level `heartbeat:` block: `url`, `interval` (min 30s), `failOnStalledWorker` (bool). Omit block to disable.
- Periodic POST to `url` every `interval` with JSON body `{"openIncidents": N, "lastTickAt": "<RFC3339>"}` while the worker is healthy.
- Liveness criterion: most recent check completion (success OR failure) within `max(2 × heartbeat.interval, 6 min)`. The 6-min floor avoids false-stall alerts when the smallest monitor interval is 5 min.
- When stalled and `failOnStalledWorker: true`, POST to `{url}/fail` instead of `url`.
- Generic enough to work with any deadman service (healthchecks.io, Better Stack, Dead Man's Snitch, BetterUptime, etc.).
- Sustained DB outage will manifest as a stall (worker can't write); design doc Q19 — confirm behavior in an integration test.

### Acceptance criteria

- [ ] With `heartbeat:` configured, a POST hits the URL every `interval` carrying the documented JSON body
- [ ] Omitting the `heartbeat:` block disables the loop entirely
- [ ] Synthetic "no check completed for X" condition triggers a POST to `{url}/fail` when `failOnStalledWorker: true`
- [ ] Liveness floor honored: the loop does not declare stalled if the smallest interval is 5 min and the worker just ticked
- [ ] Validator rejects `heartbeat.interval < 30s`
- [ ] Integration test: sustained DB outage triggers stall detection

### Blocked by

- Issue 2 — Tracer bullet

---

## Issue 16 — Graceful SIGTERM ordering (AFK)

### What to build

Implement the documented shutdown sequence so rolling restarts and scale-downs don't generate false negatives.

Includes:

- SIGTERM handler invokes ordered shutdown:
  1. Stop accepting new HTTP requests (close listener).
  2. Cancel in-flight monitor checks via context. **Do not** record them as failures — context-cancelled is not signal about the monitored service.
  3. Cancel the ingress informer.
  4. Wait up to `terminationGracePeriodSeconds - 5s` for in-flight work.
  5. Flush pending DB writes; close the connection.
  6. Send a final heartbeat POST with body `{"event": "shutdown"}` (only if heartbeat is configured).
  7. Exit 0.
- Cancelled checks log at debug level; do not count toward `toggle_monitor_checks_total{status="fail"}`.

### Acceptance criteria

- [ ] Sending SIGTERM during an in-flight check cancels the check via context and does NOT write an `alert_event` row for it
- [ ] HTTP listener stops accepting new requests immediately on SIGTERM
- [ ] Final heartbeat POST carries body `{"event": "shutdown"}`
- [ ] Process exits 0 within the grace period
- [ ] `toggle_monitor_checks_total{status="fail"}` does not increment for cancelled checks
- [ ] Integration test covers SIGTERM during an active check

### Blocked by

- Issue 2 — Tracer bullet
- Issue 15 — Heartbeat (final heartbeat POST relies on it)

---

## Issue 17 — HTMX filters, URL state, pagination, empty state (AFK)

### What to build

Polish the UI surface so filtering and pagination are linkable and shareable, respect per-listing defaults, and gracefully handle empty results.

Includes:

- HTMX-driven filter forms with `hx-push-url="true"` so URL params reflect filter state.
- Per-listing default page sizes from config (`ui.pageSize.{homepageAlerts,monitorListing,monitorHistory,discoveryListing}`).
- `?per_page=` query param overrides per request, capped at `ui.maxPerPage`.
- Sort order per listing per PRD:
  - Latest alerts: newest first.
  - Monitor listing: status (down → paused → ssl-expiring → up) → group (YAML array order) → name.
  - Per-monitor history: newest first.
  - Auto-discovery listing: by ingress namespace, then name.
- Empty state: "no results" message with a "clear filters" link that drops query params.
- Search box on monitor listing and per-group page (substring match on friendly name + slug + tag).

### Acceptance criteria

- [ ] Changing a filter pushes new URL params and reloads the partial via HTMX (no full page navigation)
- [ ] Sharing a URL with filter params reproduces the filtered view
- [ ] `?per_page=10` overrides the listing's default
- [ ] `?per_page=99999` is silently clamped to `ui.maxPerPage`
- [ ] Monitor listing sort order matches the documented order
- [ ] Empty filtered result renders the "no results" message with a working "clear filters" link
- [ ] Search box on monitor listing finds by name, slug, and tag

### Blocked by

- Issue 2 — Tracer bullet
