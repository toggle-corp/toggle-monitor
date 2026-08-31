# toggle-monitor — config schema (v1)

Locked field-by-field schema for the YAML ConfigMap. Companion to [`design-decisions.md`](./internal/design-decisions.md). Built incrementally during the design grilling.

The binary refuses to start if any required field is missing or fails validation. The CLI subcommand `toggle-monitor validate <path>` runs the same validation locally for CI use.

> **Partially superseded by ADRs.** Treat the linked ADRs as authoritative where they conflict with the prose below:
>
> - **[ADR-0009](./adr/0009-from-value-sources-for-kube-discovery.md)** partially amends ADR-0002's "annotations contribute nothing": a `config:` block may now declare that `path`, `slack`, `notify` or `tags` takes its value from an Ingress or Namespace annotation via a `*From` block. The tree still decides *which* field is set where. See §"Annotation value sources (`*From`)" below.
> - **[ADR-0014](./adr/0014-annotation-selectors-in-the-kube-match-tree.md)** adds `annotations:` and `namespaceAnnotations:` to a `kube.match` rule's `when:`, alongside `labels:`. Pairing one with `ignore: true` is how an app team opts its own object out of monitoring. The annotation selects; the operator's rule still decides what selecting means. See the selector table below.
> - **[ADR-0013](./adr/0013-from-value-sources-for-alertmanager-routing.md)** carries the same `*From` mechanism into `alertmanager.match`: a rule's `config:` may source `slack` / `notify` from a Namespace annotation, keyed off the alert's namespace label. Namespace scope only, and it requires a `kube:` block. See §"Annotation value sources under `alertmanager.match`" below.
> - **[ADR-0010](./adr/0010-self-alerting-on-issues-via-prometheusrule.md)** exports the `/issues` sources as a `toggle_monitor_issues{source="…"}` gauge and ships an optional `PrometheusRule` in the Helm chart (`prometheusRule.enabled`).
> - **[ADR-0011](./adr/0011-watch-driven-kube-removal-detection.md)** adds `kube.watchDebounce`: Ingress add/delete events trigger a debounced reconcile, so `kube.resyncInterval` is no longer the only thing that decides how late a removal is noticed.
> - **[ADR-0002](./adr/0002-kube-match-tree-cascade.md)** rewrites `kube.*`: presets/pause/annotationDomain are gone; `kube.match[]` is now a cascading tree with `when:`/`config:`/`nested:`/`final:`/`ignore:`. The kube section below is the implementation reference for that ADR.
> - **[ADR-0003](./adr/0003-statuspage-replaces-group.md)** deletes the `Group` entity entirely. `groups:`, `monitor.group`, `kube.config.group`, `theme` (and `theme.defaultGroupColor`), and `Group.Notify` are removed. `StatusPage` is the sole collection entity; sections use an `any:`/`all:` boolean predicate tree with `tags:` (AND-internally) and `hostRegex:` leaves. The `groups:`, `monitor.group`, `kube.config.group`, `theme.defaultGroupColor`, and the flat `statusPages[].sections[].match[]` shape described later in this doc are all gone — see ADR-0003 and `docs/config-example.yaml` for the canonical shape.

---

## 1. Top-level

```yaml
displayTimezone: Asia/Kathmandu
publicBaseURL: https://monitor.internal.example.com    # optional; omit to hide Slack [View details] button
dbBodyMaxChars: 4000

kube:
  resyncInterval: 30m

ui:
  pageSize:
    homepageAlerts: 20
    monitorListing: 50
    monitorHistory: 50
    discoveryListing: 50
  maxPerPage: 200

theme:
  defaultGroupColor: "#64748b"

httpClient:
  userAgent: "toggle-monitor/0.1 (+https://github.com/toggle-corp/toggle-monitor)"

heartbeat:                                              # optional block; omit to disable
  url: https://hc-ping.com/<uuid>
  interval: 1m
  failOnStalledWorker: true

sentry:                                                 # optional block; omit to disable
  enabled: true                                          # required when block is present
  dsnEnv: SENTRY_DSN                                     # env var holding the DSN; SecretString-masked in logs
  environment: production                                # default: "production"
  sampleRate: 1.0                                        # default: 1.0; [0.0..1.0]
  tracesSampleRate: 0.0                                  # default: 0.0; performance tracing off in v1
  serverName: ""                                         # default: os.Hostname()

database:
  host: cnpg-cluster-rw.cnpg-system.svc.cluster.local
  port: 5432
  user: toggle_monitor
  name: toggle_monitor
  sslMode: require                                      # disable | require | verify-ca | verify-full (default: require)
  passwordEnv: DB_PASSWORD                              # env var name; mapped from k8s Secret in Deployment
```

**Secret sourcing — via environment variables:**

All secrets (Slack bot tokens, database password) are injected as **environment variables** by the Deployment (`valueFrom.secretKeyRef`). The config names the env var; the validator at startup verifies every referenced env var is set and non-empty (otherwise refuse to start).

```yaml
# Deployment snippet
env:
  - name: SLACK_BOT_TOKEN
    valueFrom:
      secretKeyRef:
        name: toggle-monitor-slack
        key: bot-token
  - name: DB_PASSWORD
    valueFrom:
      secretKeyRef:
        name: toggle-monitor-db
        key: password
```

- Env var name format: `^[A-Z][A-Z0-9_]*$` (uppercase + digits + underscore).
- **Log masking:** secret values are wrapped in `SecretString` (implements `slog.LogValuer`). Emits a partial-mask form:
  - Length ≥ 8: `<first 2 chars>****<last 2 chars>` (e.g., `SUPER_STRONG_PASSWORD` → `SU****RD`).
  - Length < 8: `****` only (no chars shown — too short to safely reveal any).
  - Asterisk count is fixed at 4 regardless of hidden length, so logs don't leak the true secret length.

  Validator/debug output shows the **env var name** alongside the masked value (e.g., `tokenEnv=SLACK_BOT_TOKEN value=xo****1a`) so an operator can confirm the right secret was loaded without exposing it.
- Works outside k8s too (dev `.env`, docker-compose, plain shell — same env var contract).

| Field | Type | Req | Validation | Notes |
|---|---|---|---|---|
| `displayTimezone` | string | ✓ | valid IANA TZ | UI rendering only; Slack uses viewer's TZ |
| `publicBaseURL` | string | — | valid URL | If set, Slack messages include a `[View details]` button |
| `dbBodyMaxChars` | int | ✓ | >= `slack.bodyMaxChars` | Truncate stored body to this length |
| `kube.resyncInterval` | duration | ✓ | >= 1m | k8s informer resync |
| `kube.watchDebounce` | duration | — | `0s` or 1s–1m | Wait after the first Ingress event of a burst before reconciling. Default `5s`; `0s` disables watch-driven reconciles. |
| `ui.pageSize.homepageAlerts` | int | ✓ | 1–`maxPerPage` | |
| `ui.pageSize.monitorListing` | int | ✓ | 1–`maxPerPage` | |
| `ui.pageSize.monitorHistory` | int | ✓ | 1–`maxPerPage` | |
| `ui.pageSize.discoveryListing` | int | ✓ | 1–`maxPerPage` | |
| `ui.maxPerPage` | int | ✓ | >= 1 | Cap on `?per_page=` query param |
| `theme.defaultGroupColor` | string | ✓ | `^#[0-9a-fA-F]{6}$` | Fallback color for groups without `color:` |
| `httpClient.userAgent` | string | ✓ | non-empty | Sent on every outbound check |
| `heartbeat` | object | — | (the whole block) | Omit to disable. If present, all sub-fields required |
| `heartbeat.url` | string | ✓ (if block present) | valid URL | |
| `heartbeat.interval` | duration | ✓ (if block present) | >= 30s | |
| `heartbeat.failOnStalledWorker` | bool | ✓ (if block present) | | |
| `database.host` | string | ✓ | non-empty | |
| `database.port` | int | ✓ | 1–65535 | |
| `database.user` | string | ✓ | non-empty | |
| `database.name` | string | ✓ | non-empty | |
| `database.sslMode` | enum | ✓ | one of: `disable`, `require`, `verify-ca`, `verify-full` (default `require`) | |
| `database.passwordEnv` | string | ✓ | env var name regex `^[A-Z][A-Z0-9_]*$`; env var must be set and non-empty at startup | |

---

## 2. Slack

All Slack-related config nested under a single top-level `slack:` block.

```yaml
slack:
  bodyMaxChars: 200                              # include response body inline in Slack only when smaller
  summaryChannel: ops-summary                    # optional; receives the weekly operational summary
  channels:
    - slug: ops-alerts
      channelId: C0123ABC                        # #ops-alerts
      tokenEnv: SLACK_BOT_TOKEN
    - slug: ops-summary
      channelId: C0789EFG                        # #monitor-summary
      tokenEnv: SLACK_BOT_TOKEN
  userMapping:                                   # optional
    alice: U0123ABC
    ops-team: S0456DEF                           # S-prefix = subteam (emits `<!subteam^...>` markup)
  coalesce:                                      # optional; burst-dispatcher tunables (ADR-0004)
    pendingWait: 30s                              # dispatcher wait window before deciding individual vs group
    burstThreshold: 5                             # monitors down in burstWindow that promote to a digest; 0 disables groups
    burstWindow: 5m                               # rolling window the burst count spans; set above your widest interval
    groupInterval: 5m                             # digest heartbeat
    repeatInterval: 10m                           # still-down reminder cadence (group-mode only)
    groupMention: channel                         # broadcast on group open/reminder: channel | here | none
    onDemandProbeTimeout: 5s                      # per-probe budget for the hot-parent probe pass
```

| Field | Type | Req | Validation | Notes |
|---|---|---|---|---|
| `slack.bodyMaxChars` | int | ✓ | >= 0; <= `dbBodyMaxChars` | Include response body inline in Slack only when smaller |
| `slack.summaryChannel` | string | — | resolves to a `slack.channels[].slug` | Optional. Channel for the weekly operational summary (content/cadence design deferred). Omit to disable summary |
| `slack.channels` | list | ✓ | non-empty | At least one destination |
| `slack.channels[].slug` | string | ✓ | slug regex; unique across `slack.channels:` | Referenced by `monitors[].slack` and by `slack:` in any `kube.match[].config` block |
| `slack.channels[].channelId` | string | ✓ | `^[CG][A-Z0-9]{8,}$` | Inline YAML comment recommended for human label. DMs (`D…`) rejected |
| `slack.channels[].tokenEnv` | string | ✓ | env var name regex; env var set and non-empty at startup | |
| `slack.userMapping` | map | — | optional | Without it, only raw `<!here>`/`<!channel>`/`<@U…>` markup is accepted in `notify:` |
| `slack.userMapping[<slug>]` | string | ✓ when present | key: slug regex; value: `^[US][A-Z0-9]{8,}$` | |
| `slack.coalesce.pendingWait` | duration | — | default `30s` | Burst dispatcher's pool wait window. At expiry, the channel's burst count vs `burstThreshold` decides individual flush vs group promotion. See [ADR-0004](adr/0004-burst-dispatch-supersedes-always-coalesce.md) |
| `slack.coalesce.groupWait` | duration | — | deprecated alias for `pendingWait` | Accepted for one release; setting both is a validation error |
| `slack.coalesce.burstThreshold` | int | — | default `5`; `0` disables group-mode; `1` is rejected (pathological); `>= 2` otherwise | Monitors the channel has down inside `burstWindow` that promote it to a digest |
| `slack.coalesce.burstWindow` | duration | — | default `5m`; must be `>= pendingWait` | Rolling window the burst count spans. Set it above your widest `monitors[].interval` — see [ADR-0015](adr/0015-cumulative-burst-window.md) |
| `slack.coalesce.groupInterval` | duration | — | default `5m` | Digest heartbeat: batch joins/recoveries/flaps into one edit + threaded reply per interval. Also the resolve-debounce/flap-dampening window |
| `slack.coalesce.repeatInterval` | duration | — | default `10m` | Cadence of the per-group "still down" reminder (group-mode only; individual-mode uses each monitor's `reminderInterval`) |
| `slack.coalesce.groupMention` | string | — | one of `channel`, `here`, `none`; default `channel` | Broadcast marker injected at group open + each reminder. Edits never re-mention regardless |
| `slack.coalesce.onDemandProbeTimeout` | duration | — | default `5s` | Per-probe budget for the hot-parent probe pass at pendingWait expiry |

**Burst dispatcher (ADR-0004).** Per channel, the dispatcher walks three modes:

1. **individual** — every failure posts immediately as a per-monitor message; recoveries fire individual resolves. The 90% case.
2. **pending** — first failure starts a `pendingWait` timer; further failures join the pool. At expiry the dispatcher counts every monitor this channel currently has down inside `burstWindow`, unioned with this pool: `< burstThreshold` flushes the pool as N individual messages; `>= burstThreshold` promotes to **group**.
3. **group** — a single living digest with `@channel` (or configured marker) on open and on each `repeatInterval` reminder. Subsequent failures on the same channel join the digest directly. Heartbeat (`groupInterval`) edits batch joins/recoveries. When the last member recovers, the channel returns to individual.

**Why the count spans `burstWindow`, not one pool.** The scheduler jitters each monitor's first tick across its whole `interval`, so a cluster-wide outage does not arrive as a burst — it arrives as a trickle of roughly `N x pendingWait / interval` monitors per window. Sizing the burst off a single pool under-counts every real outage: each window flushes sub-threshold and the operator gets one message per monitor. Counting cumulatively bounds that at `burstThreshold - 1` individual messages, after which the channel promotes and the rest of the outage lands in one digest. Set `burstWindow` above your widest `monitors[].interval`; lower `burstThreshold` to shorten the individual prefix.

At pendingWait expiry the dispatcher also fires one bounded probe (within `onDemandProbeTimeout`) of any dependsOn parent referenced by ≥2 pool entries that isn't already in the pool. If the parent probes down, push-propagation drains its children from the pool — leaving the parent as the named root cause instead of a digest of symptoms.

`monitors[].critical: true` opts a monitor out of the dispatcher entirely — it pages immediately as an individual message regardless of channel mode. A `dependsOn` pause still wins over `critical` (a paused monitor stays silent). Give shared `dependsOn` parents a **short interval**; the startup logs a WARN for any parent whose interval is slower than a child's.

**Validation behavior:**
- At startup the app calls Slack's `auth.test` for every distinct token (resolved from `tokenEnv` values). **All tokens must resolve to the same `team_id` (workspace).** Different workspaces → refuse to start. (Single-workspace only in v1.)
- A failing `auth.test` (transient API blip or revoked token) is a **warning, not a startup blocker.** Surfaced in the UI's invalid-config section and re-checked hourly.
- User/subteam IDs in `slack.userMapping` are workspace-agnostic in the schema. The runtime catches workspace mismatches at first post attempt and logs an error.

---

## 2b. Proxies (optional)

Declares outbound proxies that monitors can route their probes
through. Currently only SOCKS5 is supported.

```yaml
proxies:
  - slug: corp                              # referenced by monitors[].proxy and by `proxy:` in any kube.match[].config block
    protocol: socks5                        # only supported value in v1
    server: proxy.internal.example
    port: 1080                              # optional; defaults to 1080 for socks5
    username: monitor-bot                   # optional, plain text
    passwordEnv: PROXY_PASSWORD             # optional; env-resolved like every other secret
```

| Field | Type | Required | Constraint | Notes |
|---|---|---|---|---|
| `proxies[].slug` | string | ✓ | slug regex; unique across `proxies[]` | |
| `proxies[].protocol` | string | ✓ | `socks5` (only supported in v1) | |
| `proxies[].server` | string | ✓ | non-empty | hostname or IP |
| `proxies[].port` | int | — | 1..65535 | `0` / omitted → protocol default (1080 for socks5) |
| `proxies[].username` | string | — | | plain text; optional |
| `proxies[].passwordEnv` | string | — if `username` is absent | env-var name; requires `username` | env-resolved (consistent with `tokenEnv` / `database.passwordEnv`) |

Pool is built once at startup; an empty / unset env var fails the
startup, not the runtime tick.

---

## 2c. Self-health degraded mode (optional) — ADR-0008

When a cluster-internal DNS/network outage takes out the monitor's own
resolver, *every* probe fails with a DNS-resolution error and Slack
delivery fails too. Treating that as N service outages pages the wrong
people under a wrong mental model. The self-health detector recognizes
the pattern and emits **one** self-health notice instead.

```yaml
selfHealth:                                 # optional; omit to disable
  window: 90s                               # W — rolling detect/decide window
  minMonitors: 3                            # N_min — distinct DNS-failing monitors to trip (>= 2)
  channel: ops-alerts                       # self-health notice channel; omit → metric + log only
  mention: <!subteam^S02ABCDEF34>           # optional @mention on the degraded notice
```

| Field | Type | Required | Constraint | Notes |
|---|---|---|---|---|
| `selfHealth.window` | duration | — | default `90s` | Rolling detection/decision window `W`. A DNS failure is held provisional this long before it can page |
| `selfHealth.minMonitors` | int | — | default `3`; `< 2` is rejected (pathological) | `N_min` — distinct monitors that must DNS-fail (with zero successes) within `W` to trip degraded mode |
| `selfHealth.channel` | string | — | must resolve to a `slack.channels[]` slug when set | Self-health notice destination. Empty → metric + log only (no Slack). Never fanned out to per-service channels |
| `selfHealth.mention` | string | — | raw Slack markup | Optional on-call escalation `@mention` on the degraded notice |

**Trigger.** Degraded mode enters iff, within `W`: (1) `>= minMonitors`
distinct monitors reported a **DNS-class** failure, **and** (2) zero
probes succeeded. The DNS-class key separates "I'm blind" from "targets
genuinely down" — a real total outage yields `connection refused` / dial
timeouts (classified `dial`, not `dns`), so it does **not** trip and the
burst dispatcher handles it as one grouped incident. A single success in
`W` vetoes the trip (the monitor can reach *something*, so it is not
network-isolated).

**Mechanics (defer-and-decide).** A DNS-class tick does not call
`alert.Apply` — it is held provisional (no DB write, no dispatch),
mirroring the SIGTERM "not signal about the monitored service"
precedent. Once per `W` the evaluator decides: **tripped** → discard the
provisionals (fully silent, no phantom incident history); **not
tripped** → commit the isolated failure and page it as a normal
`EventOpen`, ~`W` late. Freeze is uniform — `critical` monitors
included: a DNS failure while blind carries zero information about a
critical service. Critical monitors keep their bypass for real signal
(application errors during degraded mode, and anything once connectivity
returns).

**Notice + dead-man's-switch.** The notice is best-effort (Slack usually
needs DNS too, so it normally lands as a single post-hoc summary once
connectivity returns). The authoritative signal is Prometheus, which
scrapes pods by IP and lives outside the DNS failure domain: the
`toggle_monitor_self_degraded` gauge and `toggle_monitor_self_degraded_entries_total`
counter are always emitted. See [operations.md](operations.md) for the
alert rules.

**Known gap (out of scope for v1):** egress loss *without* DNS loss (DNS
cached, all dials time out) is `dial`, not `dns`, so it does not trip.
Broadening to `dial` risks the total-outage false positive; kube-dns
failure is the dominant real case.

---

## 3. Groups

```yaml
groups:
  - slug: production-apis
    friendlyName: Production APIs
    description: Customer-facing API services           # optional
    logoUrl: https://example.com/logos/prod.png         # optional
    color: "#ef4444"                                    # optional; falls back to theme.defaultGroupColor

  - slug: kube-discovered                               # REQUIRED — fallback for kube-discovered monitors whose resolved config doesn't set `group:`
    friendlyName: Kube Discovered
    description: Auto-discovered ingresses
```

| Field | Type | Req | Validation | Notes |
|---|---|---|---|---|
| `groups` | list | ✓ | non-empty; must include a group with slug `kube-discovered` | |
| `groups[].slug` | string | ✓ | slug regex; unique across `groups:` | Referenced by `monitors[].group` and by `group:` in any `kube.match[].config` block |
| `groups[].friendlyName` | string | ✓ | non-empty | |
| `groups[].description` | string | — | | |
| `groups[].logoUrl` | string | — | valid URL | Optional logo for group cards |
| `groups[].color` | string | — | `^#[0-9a-fA-F]{6}$` | Hex color; if absent uses `theme.defaultGroupColor` |

**Behavior:**
- Validator rejects start if no group with slug `kube-discovered` exists.
- Validator rejects orphan references — every `monitors[].group` and every `group:` set inside a `kube.match[].config` block must resolve to a declared slug.
- Group with zero monitors is allowed silently.
- Display order in the UI follows the array order in `groups:`.

---

## 4. Kube auto-discovery (`kube.match` cascade)

Every monitor materialized from a discovered Ingress is the merge of one or more rules in the `kube.match[]` tree. There is no preset registry, no `kube.pause:` block, no `kube.annotationDomain`, and no per-ingress annotations — every monitor field is set somewhere in the tree.

Authoritative design: [ADR-0002 — `kube.match` as a cascading rule tree](./adr/0002-kube-match-tree-cascade.md).

```yaml
kube:
  resyncInterval: 30m
  watchDebounce: 5s                                  # reconcile this long after an Ingress add/delete; 0s disables (default: 5s)
  friendlyName: compact                              # compact | plain | dedupe | title (default: compact)

  # The match tree. Every (Ingress, host) pair traverses the entire
  # tree depth-first in document order; every rule whose `when:`
  # matches contributes its `config:` to a merge stack.
  match:
    # Root rule — `when: {}` matches every (Ingress, host) and is
    # MANDATORY at the top level. Supplies all required monitor fields
    # so any leaf-most match still produces a valid monitor.
    - when: {}
      config:
        scheme: https
        path: /
        httpMethod: GET
        acceptedStatusCodes: [200]
        interval: 5m
        timeout: 10s
        retries: 2
        retryBackoff: 5s
        followRedirects: false
        reminderInterval: 3d
        sslAlertThreshold: 30d
        sslEscalationThreshold: 7d
        sslReminderInterval: 3d
        slack: ops-alerts
        notify: [ops-team]

    # Kill-switch convention: any Ingress carrying this label is
    # suppressed; halt the cascade so nothing later un-ignores it.
    - when:
        labels:
          monitor.togglecorp.com/disabled: "true"
      ignore: true
      final: true

    # Narrow refinement by namespace glob.
    - when: { namespace: "acme-*" }
      config:
        group: acme
        notify: [alice]                              # union with the root's [ops-team]
      nested:
        - when: { namespaceRegex: "acme-service-a-eoapi-\\d+" }
          config:
            tags: [service-a, eoapi]
          nested:
            - when:
                labels:
                  app.kubernetes.io/name: minio
              config:
                path: /minio/health/live
                acceptedStatusCodes: [200]          # replace-by-default for this field
              final: true                            # nothing further (e.g., a later top-level
                                                     # "global minio" rule) may clobber `path`
```

### Rule shape

Each rule is a map with up to five keys:

| Key | Type | Notes |
|---|---|---|
| `when` | object | Selector — see vocabulary below. Absent or empty (`{}`) means "match anything reaching this point in the traversal." |
| `config` | object | Monitor fields contributed to the merge stack when this rule matches. Optional. |
| `nested` | list[rule] | Child rules, traversed only when this rule matches. Optional. |
| `ignore` | bool | Rule-level directive. Cascades like a scalar; deepest matching rule wins. When the resolved value is `true`, no monitor is created and a discovery row with `status="kube-ignored"` is recorded instead. |
| `final` | bool | Rule-level directive. When this rule matches, traversal descends into its own `nested:` subtree, then halts the entire tree walk for this `(Ingress, host)` pair. No later sibling, uncle, or top-level rule contributes. |

`when:`, `config:`, and `nested:` may all be absent on the same rule (e.g., a marker rule that only carries `ignore: true`).

### Selector vocabulary (`when:`)

| Field | Type | Match semantics |
|---|---|---|
| `namespace` | string | Glob (`path.Match`). |
| `namespaceRegex` | string | Go regexp, **auto-anchored** as `^…$`. Use `.*` explicitly for substring. |
| `host` | string | Glob (`path.Match`) against the Ingress host. |
| `hostRegex` | string | Go regexp, auto-anchored. |
| `labels` | map[string]string | Exact key=value pairs on `ingress.metadata.labels`. Multiple keys AND together. |
| `annotations` | map[string]string | Exact key=value pairs on `ingress.metadata.annotations`. Multiple keys AND together. |
| `namespaceAnnotations` | map[string]string | Exact key=value pairs on the **Namespace's** `metadata.annotations`. Multiple keys AND together. |

- Within a single `when:`, all set fields AND together.
- `labels` and `annotations` match the Ingress's **own** metadata only — not that of the backing Service / Deployment / Pod.
- An absent key never matches. `skip: ""` selects objects carrying the key set to the empty string, not objects missing it.
- No `name:` selector in v1 — namespace + labels cover realistic cases.

**Annotation selectors are how an app team opts out (ADR-0014).** The
binary attaches no meaning to any annotation key; the operator's rule
does, by pairing one with `ignore: true`:

```yaml
- when:
    annotations:
      monitor.example.test/skip: "true"
  ignore: true
  final: true
```

The team then sets that annotation on the Ingress it owns. Skipped
hosts are listed under "Skipped ingresses" on `/issues` with the rule
chain that produced them, so a too-broad rule is visible.

### Evaluation: multi-match accumulate

For each `(Ingress, host)` pair the entire `kube.match[]` tree is walked depth-first in document order. **Every** rule whose `when:` matches contributes its `config:` to a merge stack. The final config for that monitor is the merge of the stack in stack order — root-first → deeper later.

The first top-level rule with `when: {}` (or omitted) is the always-matching baseline that supplies defaults; it is mandatory (see below).

### Merge rules

| Field type | Rule |
|---|---|
| Scalars (string, int, bool, Duration) | Deeper / later rules override shallower / earlier. Unset = inherit from the next-shallower rule that set it. |
| Arrays (`notify`, `tags`, `dependsOn`, …) | **Union by default**, deduplicated, shallow-first insertion order. |
| Arrays tagged `!override` | The `!override` YAML custom tag swaps that single occurrence from union to replace; ancestors' values for that field are discarded. `!override []` clears an inherited list. |
| `acceptedStatusCodes` | **Replace by default** — unioning HTTP status codes is almost always wrong. No `!override` needed. |

```yaml
config:
  notify: !override [uday, ankit]   # ignores anything ancestors set for `notify`
```

There is no `!reset` tag (covered by `!override []` / natural empty scalars) and no `extends:` — inheritance is tree-only.

### Root rule required, with all required fields

A rule with `when: {}` at the top level is **mandatory** and must carry every required monitor field (`path`, `httpMethod`, `acceptedStatusCodes`, `interval`, `timeout`, `retries`, `retryBackoff`, `followRedirects`, `reminderInterval`, `sslAlertThreshold`, `sslEscalationThreshold`, `sslReminderInterval`, `slack`). Children override selectively but don't have to set anything. Missing root or missing required fields at the root is a validation error at startup.

The root always matches, so every `(Ingress, host)` materializes with at least the root config. Operators wanting a deliberate-decision signal can write the root as `ignore: true` and explicitly un-ignore vetted namespaces — that stance is available, not forced.

### `final: true` — halt the cascade

A matching rule with `final: true` descends into its own `nested:` subtree (so further refinement within the subtree still applies), then halts the entire tree walk for this `(Ingress, host)` pair.

`final: true` requires at least one selector field in `when:`. `final: true` with empty `when:` would halt the cascade for every Ingress on first occurrence — almost certainly a typo. Validation error.

### `ignore: true` — suppress materialization

`ignore:` lives at the rule level (sibling of `when:` / `config:` / `nested:` / `final:`), not inside `config:`. It cascades like a scalar (deepest matching rule wins), so a child can flip `ignore: false` to un-ignore a subset:

```yaml
- when: { namespace: "test-*" }
  ignore: true
  nested:
    - when: { namespace: "test-critical-*" }
      ignore: false
```

A resolved `ignore: true` means no monitor is created; a `status="kube-ignored"` discovery row is recorded so the operator sees the rule fired and can filter on `/discovery`.

### Wildcard hosts

Kubernetes permits `*` as the leftmost label of an Ingress rule host (`*.static.example.test`). No prober can resolve such a name, so it never materializes into a monitor.

By default it records `status="kube-invalid"` with reason `wildcard host not probeable`, which counts toward the `/issues` badge and the `toggle_monitor_issues{source="kube-invalid"}` gauge. A matching `ignore: true` rule acknowledges it: the row becomes `kube-ignored` (still unprobed, still explained in the reason) and drops out of both counts. See ADR-0012.

```yaml
- when: { hostRegex: '\*\..*' }   # hostRegex is auto-anchored ^…$
  ignore: true
```

Scope the selector as tightly as the situation allows — `when: { namespace: "static-*", hostRegex: '\*\..*' }` acknowledges one team's wildcards without silencing the next one that appears elsewhere.

### `kube.*` field reference

| Field | Type | Req | Validation | Notes |
|---|---|---|---|---|
| `kube.resyncInterval` | duration | ✓ | >= 1m | k8s informer resync (also referenced in §1). |
| `kube.watchDebounce` | duration | — | `0s` or 1s–1m | Debounce for watch-driven reconciles: Ingress add/delete events trigger a pass after this window instead of waiting for `resyncInterval`, so a removed Ingress is torn down before the burst dispatcher reports the resulting 404. Also the settle window — a delete followed by a recreate inside it changes nothing. Default `5s`; `0s` leaves `resyncInterval` as the only trigger. |
| `kube.friendlyName` | enum | — | one of: `compact`, `plain`, `dedupe`, `title` (default `compact`) | Auto-generated display name style for kube-discovered monitors. |
| `kube.match` | list | ✓ | non-empty; first top-level rule must have empty (`{}` or omitted) `when:` | The cascading rule tree. |
| `kube.match[].when` | object | — | see selector table | Absent or `{}` means "match anything." |
| `kube.match[].when.namespace` | string | — | valid glob | Mutually exclusive with `namespaceRegex` in the same `when:`. |
| `kube.match[].when.namespaceRegex` | string | — | valid Go regexp; auto-anchored `^…$` | Mutually exclusive with `namespace`. |
| `kube.match[].when.host` | string | — | valid glob | Mutually exclusive with `hostRegex`. |
| `kube.match[].when.hostRegex` | string | — | valid Go regexp; auto-anchored | Mutually exclusive with `host`. |
| `kube.match[].when.labels` | map[string]string | — | valid k8s label key syntax for each key | Matched against `ingress.metadata.labels`; all keys AND. |
| `kube.match[].when.annotations` | map[string]string | — | valid k8s annotation key syntax for each key; values unconstrained | Matched against `ingress.metadata.annotations`; all keys AND. |
| `kube.match[].when.namespaceAnnotations` | map[string]string | — | valid k8s annotation key syntax for each key; values unconstrained | Matched against the Namespace's `metadata.annotations`; all keys AND. |
| `kube.match[].config` | object | — | see config-field table below | Optional contribution to the merge stack. |
| `kube.match[].nested` | list[rule] | — | each entry follows the same rule shape | Recursive. |
| `kube.match[].ignore` | bool | — | default unset | Cascades; deepest matching rule wins. Resolved `true` → no monitor, `kube-ignored` discovery row. |
| `kube.match[].final` | bool | — | requires non-empty `when:` | Halts the tree walk after descending into this rule's `nested:`. |

### `kube.match[].config` fields

All monitor fields below are settable inside any `config:` block. The root must set all required fields; descendants override selectively.

| Field | Type | Req at root | Validation | Notes |
|---|---|---|---|---|
| `scheme` | enum | — (defaults to `https` at materialization) | `https` or `http` | URL scheme for the built URL. |
| `path` | string | ✓ | starts with `/` | Appended to the Ingress host. |
| `httpMethod` | enum | ✓ | one of `GET`, `HEAD`, `POST`, `PUT`, `DELETE` | |
| `acceptedStatusCodes` | list[int] | ✓ | non-empty; each 100–599 | **Replace by default** across the cascade (no `!override` needed). |
| `interval` | duration | ✓ | >= 30s | |
| `timeout` | duration | ✓ | < `interval` | |
| `retries` | int | ✓ | >= 0 | |
| `retryBackoff` | duration | ✓ | >= 1s | |
| `followRedirects` | bool | ✓ | | |
| `tlsInsecureSkipVerify` | bool | — | default `false` | Same semantics as `monitors[].tlsInsecureSkipVerify`. |
| `proxy` | string | — | resolves to a `proxies[].slug` | Same semantics as `monitors[].proxy`. |
| `reminderInterval` | duration | ✓ | >= 1h | |
| `sslAlertThreshold` | duration | ✓ | > `sslEscalationThreshold` | |
| `sslEscalationThreshold` | duration | ✓ | > 0 | |
| `sslReminderInterval` | duration | ✓ | >= 1h | |
| `slack` | string | ✓ | resolves to a `slack.channels[].slug` | |
| `notify` | list[string] | — | each entry: a `slack.userMapping` slug OR `<...>` raw markup | Union across cascade by default; tag with `!override` to replace. |
| `group` | string | — | resolves to a `groups[].slug` | If unset across the entire cascade, falls back to `kube-discovered`. |
| `tags` | list[string] | — | each: slug regex | Union across cascade by default; `kube` is always auto-added at materialization. |
| `dependsOn` | list[string] | — | each: resolves to a **static** `monitors[].slug` (kube-discovered cannot be a parent) | Union across cascade; validator detects cycles. |
| `pathFrom` | object | ✓ (with a `default:`) | see below | Sources `path` from an annotation. |
| `slackFrom` | object | ✓ (with a `default:`) | see below | Sources `slack` from an annotation. |
| `notifyFrom` | object | — | see below | Sources `notify`; unions like the literal. |
| `notifyOverrideFrom` | object | — | see below | Sources `notify`; replaces the baseline at this rule's position. |
| `tagsFrom` | object | — | see below | Sources `tags`; unions like the literal. |
| `tagsOverrideFrom` | object | — | see below | Sources `tags`; replaces the baseline at this rule's position. |
| `acceptedStatusCodesFrom` | object | ✓ (with a `default:`) | see below | Sources `acceptedStatusCodes`. No `Override` twin — the literal is already replace-by-default. |

### Annotation value sources (`*From`)

Authoritative design: [ADR-0009](./adr/0009-from-value-sources-for-kube-discovery.md).

A `config:` block may set a field either literally or from an annotation:

```yaml
<field>From:
  annotation: app.example.com/<key>          # read from the Ingress
  namespaceAnnotation: app.example.com/<key> # read from the Ingress's Namespace
  default: <value>                           # optional; used when the annotation is absent
```

Exactly one of `annotation:` / `namespaceAnnotation:` per block. `default:` accepts a scalar or, for the list fields, a YAML sequence; a scalar default for a list field is split on commas, matching what `{{ .Values.notify | join "," }}` renders.

**Merge semantics are identical to the literal field.** `pathFrom` and `slackFrom` are scalars (deepest layer that set the field wins); `notifyFrom` / `tagsFrom` union; the `*OverrideFrom` twins replace the baseline **at that rule's position**, exactly as the `!override` YAML tag does, with later rules still unioning on top. `acceptedStatusCodesFrom` replaces, matching the literal field's replace-by-default behaviour — which is why it has no `Override` twin. No new merge concept is introduced.

`acceptedStatusCodesFrom` reads a comma-separated list of HTTP status codes (`"303"`, `"200,303"`). Its `default:` accepts a YAML sequence of ints (`default: [200, 303]`) or the same CSV scalar.

Namespace-scoped sources require `get/list/watch namespaces` RBAC — the chart adds it alongside the Ingress watch when `rbac.ingressWatch` is on.

**Load-time errors** (refuse to start):

- A `*From` block and the literal it supplies in the same `config:` block (e.g. `path` + `pathFrom`).
- `notifyFrom` + `notifyOverrideFrom` (or the `tags` pair) in the same block.
- Neither or both of `annotation:` / `namespaceAnnotation:`.
- An annotation key that isn't a valid k8s qualified name.
- A `default:` that fails the literal field's own validation. Defaults are reviewed config, so these are hard errors. For `acceptedStatusCodesFrom` that means a non-empty list of integers in 100..599.

**Runtime degradation.** Annotation values are unreviewed input, so a bad one never blocks monitoring — **the monitor always materializes and keeps probing**:

| situation | behaviour |
|---|---|
| annotation absent, empty, or whitespace-only | treated as absent → `default:` if present, else the source contributes nothing and the cascade value stands |
| `notify` entry not a `slack.userMapping` slug | that entry is dropped and warned; valid entries in the same value are kept |
| `notify` entry is raw `<…>` Slack markup | rejected — the roster of who can be paged stays in reviewed config |
| `slack` value not a configured channel slug | rejected; the cascade value stands |
| `path` value not starting with `/` | rejected; the cascade value stands |
| an `*OverrideFrom` yielding no valid entries | ignored entirely rather than replacing real recipients with nothing |
| `acceptedStatusCodes` entry not an integer in 100..599 | that entry is dropped and warned; the rest are kept |
| `acceptedStatusCodesFrom` yielding no valid code | ignored entirely — an empty list fails resolved-value validation and would cost the monitor |

Every rejected value produces a `WARN` log, a `warn:` note on the discovery row's reason, an entry in the "Rejected annotation values" section of `/issues`, and a point on `toggle_monitor_issues{source="annotation"}`. Not Sentry — it is app-team input error, not a toggle-monitor fault.

Accepted values are recorded as provenance: the discovery row's reason and the discovery detail page both show `path=/livez ← annotation app.example.com/health-check`, and `toggle-monitor explain` emits `provenance:` and `warnings:` blocks.

`namespaceLabel:` is rejected under `kube.match` — a kube source reads the Ingress's own namespace. It exists only for `alertmanager.match`; see §"Annotation value sources under `alertmanager.match`".

### Validation

**Structural errors** (refuse to start):
- Glob/regex conflict in the same `when:` (`namespace` + `namespaceRegex`, `host` + `hostRegex`).
- Glob parse failure, regex parse failure, invalid label-key syntax.
- `final: true` with empty `when:`.
- Missing root rule (no top-level rule with empty `when:`).
- Missing required fields at the root.
- Orphan slug references inside any `config:` block (`slack`, `group`, `proxy`, `dependsOn`).

**Structural warnings** (surface in UI invalid-config section, do not block startup):
- Empty `when:` deeper than root (redundant — equivalent to lifting `config:` into the parent).
- `ignore: true` at a leaf rule with a non-empty `config:` (dead config unless a child un-ignores).

**Resolved-value errors at materialization time** (per-monitor, recorded as `status="kube-invalid"` discovery rows pointing at the rule chain — they don't block startup because they depend on which Ingresses actually exist):
- `interval >= timeout`.
- `sslAlertThreshold` / `sslEscalationThreshold` invariants.
- `retries × (timeout + retryBackoff) >= interval`.
- A required field that the root did set was overridden to an invalid value deeper in the tree.

**Reachability is not validated.** Under multi-match accumulate, "unreachable rule" only means "no Ingress in the universe matches" — which is dynamic. The `toggle-monitor explain` CLI is the spot-check tool; rule chains in discovery surface rules that never fire in practice.

### Identity: monitor slug derives from the Ingress only

Monitor slug is:

```
kube-<namespace>__<ingress-name>__<host>
```

Double-underscore separator avoids collision with single-dash content in any of the three parts. The `kube-` prefix is reserved: the config validator rejects any static `monitor.slug` starting with `kube-`, so a `kube-…` slug always means auto-discovered. The slug carries no rule-derived component: the monitored thing is an `(Ingress, host)` pair and already has stable k8s identity. Pulling slug from config would mean a config edit can rename a monitor (breaking history, bookmarks, `dependsOn` references).

Friendly display name remains controlled by `kube.friendlyName:` (compact / plain / dedupe / title styles).

### Debug surface

**Discovery `reason` field.** Carries the compact rule chain for each materialized (or ignored) monitor, e.g.:

```
match[1] (ns=acme-*) → [2] (ns=acme-service-a-*)
  → [0] (nsRegex=acme-service-a-eoapi-\d+)
  → [1] (labels.app.kubernetes.io/name=minio) [final]
```

Operators find the rules in the YAML and reason from there.

**/discovery detail view.** Shows the rule chain alongside the full resolved config (the merged config that drives the monitor) so "why is `path` `/minio/health/live`?" answers in two glances.

**No per-field provenance in v1.** Deferred until a real debugging session demands it; the rule chain gets 80% of the way for free.

**CLI `toggle-monitor explain` subcommand.** Resolves the rule chain + final config either for a live cluster Ingress or for a hypothetical `(namespace, labels, annotations, host)` tuple:

```
toggle-monitor explain --ingress acme-service-a-eoapi-3/web
toggle-monitor explain --ingress acme-service-a-eoapi-3/web --host api.example.com
toggle-monitor explain \
  --namespace acme-service-a-eoapi-3 \
  --labels app.kubernetes.io/name=minio \
  --host api.example.com
toggle-monitor explain \
  --namespace acme-service-a-eoapi-3 \
  --annotations monitor.example.test/skip=true \
  --host api.example.com
```

Live mode reads both annotation scopes off the cluster;
`--annotations` / `--namespace-annotations` supply them for an object
that does not exist yet.

Output is human-readable YAML by default. `--from-file`, `--json`, and per-field provenance are deferred.

---

## 5. Static monitors

```yaml
# YAML anchors for DRY; any top-level key starting with `x-` is ignored by the validator (docker-compose convention).
x-monitor-defaults: &staticDefaults
  httpMethod: GET
  acceptedStatusCodes: [200]
  interval: 5m
  timeout: 10s
  retries: 2
  retryBackoff: 5s
  followRedirects: false
  reminderInterval: 3d
  sslAlertThreshold: 30d
  sslEscalationThreshold: 7d
  sslReminderInterval: 3d
  slack: ops-alerts

monitors:
  - <<: *staticDefaults
    slug: bastion-proxy
    friendlyName: Bastion Proxy
    url: https://bastion.internal/health
    group: gateways
    interval: 1m                                       # override
    notify: [ops-team]
    tags: [gateway]

  - <<: *staticDefaults
    slug: legacy-api
    friendlyName: Legacy API
    url: https://legacy.example.com/health
    group: production-apis
    dependsOn: [bastion-proxy]
    notify: [alice]
    tags: [legacy]
```

| Field | Type | Req | Validation | Notes |
|---|---|---|---|---|
| `x-*` (top-level keys with `x-` prefix) | any | — | **ignored** | Docker-compose-style convention for anchor-only blocks |
| `monitors` | list | — | optional (could be all-kube) | |
| `monitors[].slug` | string | ✓ | slug regex; unique across all monitors (static + kube-discovered namespace) | |
| `monitors[].friendlyName` | string | ✓ | non-empty | Display name in UI/Slack |
| `monitors[].url` | string | ✓ | valid URL with scheme `http`/`https` | The actual URL to monitor |
| `monitors[].group` | string | ✓ | resolves to a `groups[].slug` | Required for static monitors (no fallback) |
| `monitors[].httpMethod` | enum | ✓ | one of GET/HEAD/POST/PUT/DELETE | |
| `monitors[].acceptedStatusCodes` | list[int] | ✓ | non-empty; each 100–599 | |
| `monitors[].interval` | duration | ✓ | >= 30s | |
| `monitors[].timeout` | duration | ✓ | < interval | |
| `monitors[].retries` | int | ✓ | >= 0 | |
| `monitors[].retryBackoff` | duration | ✓ | >= 1s | |
| `monitors[].followRedirects` | bool | ✓ | | |
| `monitors[].tlsInsecureSkipVerify` | bool | — | default `false` | Skips Go's TLS chain verification on the probe. Use only for HTTPS endpoints with self-signed certs you intentionally trust. Implies "do not track SSL expiry": SSL state stays `ssl-skipped`. |
| `monitors[].proxy` | string | — | resolves to a `proxies[].slug` | Routes the probe through that proxy (SOCKS5). Omit / empty for direct dial. |
| `monitors[].reminderInterval` | duration | ✓ | >= 1h | |
| `monitors[].sslAlertThreshold` | duration | ✓ if URL is HTTPS and `tlsInsecureSkipVerify: false` | > `sslEscalationThreshold` | Conditionally required |
| `monitors[].sslEscalationThreshold` | duration | ✓ if URL is HTTPS and `tlsInsecureSkipVerify: false` | > 0 | Conditionally required |
| `monitors[].sslReminderInterval` | duration | ✓ if URL is HTTPS and `tlsInsecureSkipVerify: false` | >= 1h | Conditionally required |
| `monitors[].slack` | string | ✓ | resolves to a `slack.channels[].slug` | |
| `monitors[].notify` | list[string] | — | each entry: a `slack.userMapping` slug OR `<...>` raw markup | |
| `monitors[].tags` | list[string] | — | each: slug regex | |
| `monitors[].dependsOn` | list[string] | — | each: resolves to a **static** `monitors[].slug` | Validator detects cycles. Startup logs a WARN if a parent's interval is slower than this monitor's (widens the dependsOn race) |
| `monitors[].critical` | bool | — | default `false` | Opt out of alert coalescing: page immediately as an individual message instead of joining the per-channel digest. A `dependsOn` pause still wins (paused → silent) |

**Cross-field validation:**
- `retries × (timeout + retryBackoff) < interval`.
- SSL fields (`sslAlertThreshold`, `sslEscalationThreshold`, `sslReminderInterval`) are required when `url` is HTTPS; optional but allowed when `url` is HTTP (ignored at runtime). Allows anchor reuse across HTTP and HTTPS monitors without splitting anchors.
- Slug uniqueness across **all** monitors. At config-load static slugs are checked; kube-discovered slug conflicts (e.g., a kube slug colliding with a static slug) surface in the auto-discovery snapshot at reconcile time.

---

## 5b. SMTP monitors

`smtpMonitors:` declares SMTP service probes. They are **static-only**
(kube auto-discovery never produces them) and confirm a server is
answering and speaking SMTP, optionally negotiating TLS and tracking the
certificate's expiry via the same SSL state machine HTTPS monitors use.
They do **not** authenticate or send mail. See the SMTP monitoring
design.

SMTP monitors share the slug namespace, the `dependsOn` graph (an SMTP
monitor may depend on an HTTP monitor and vice-versa), the status-page
tag matching, and the alert/SSL/Slack/metrics machinery with HTTP
monitors. They are stored in the same `monitors` table with `kind:
smtp`; the `url` column holds a synthesized `smtp://host:port` identity.

```yaml
smtpMonitors:
  - slug: mail-relay
    friendlyName: Mail Relay
    host: smtp.example.test
    port: 2525
    tls: starttls              # starttls (default) | implicit | none
    ehloName: toggle-monitor   # optional; hostname sent in EHLO/HELO
    interval: 5m
    timeout: 10s
    retries: 1
    retryBackoff: 5s
    reminderInterval: 3d
    slack: ops-alerts
    tags: [mail]
    sslAlertThreshold: 14d
    sslEscalationThreshold: 3d
    sslReminderInterval: 24h
```

| Field | Type | Req | Validation | Notes |
|---|---|---|---|---|
| `smtpMonitors` | list | — | optional | |
| `smtpMonitors[].slug` | string | ✓ | slug regex; unique across **all** monitors (HTTP + SMTP + kube namespace) | |
| `smtpMonitors[].friendlyName` | string | ✓ | non-empty | |
| `smtpMonitors[].host` | string | ✓ | non-empty | SMTP server hostname |
| `smtpMonitors[].port` | int | ✓ | 1–65535 | e.g. 25 / 465 / 587 / 2525 |
| `smtpMonitors[].tls` | enum | — | one of `starttls`/`implicit`/`none`; default `starttls` | `starttls`/`implicit` capture the cert; `none` skips SSL |
| `smtpMonitors[].ehloName` | string | — | default `toggle-monitor` | Hostname sent in EHLO/HELO |
| `smtpMonitors[].interval` | duration | ✓ | | |
| `smtpMonitors[].timeout` | duration | ✓ | < interval; covers the whole conversation | |
| `smtpMonitors[].retries` | int | ✓ | >= 0 | |
| `smtpMonitors[].retryBackoff` | duration | ✓ | | |
| `smtpMonitors[].tlsInsecureSkipVerify` | bool | — | default `false` | Skips cert verification; implies SSL state `ssl-skipped` |
| `smtpMonitors[].proxy` | string | — | resolves to a `proxies[].slug` | Routes the TCP connect through that proxy (SOCKS5) |
| `smtpMonitors[].reminderInterval` | duration | ✓ | | |
| `smtpMonitors[].sslAlertThreshold` | duration | ✓ if `tls` is `starttls`/`implicit` and `tlsInsecureSkipVerify: false` | > `sslEscalationThreshold` | Conditionally required |
| `smtpMonitors[].sslEscalationThreshold` | duration | ✓ (same condition) | > 0 | Conditionally required |
| `smtpMonitors[].sslReminderInterval` | duration | ✓ (same condition) | > 0 | Conditionally required |
| `smtpMonitors[].slack` | string | ✓ | resolves to a `slack.channels[].slug` | |
| `smtpMonitors[].notify` | list[string] | — | each: a `slack.userMapping` slug OR `<...>` raw markup | |
| `smtpMonitors[].tags` | list[string] | — | each: slug regex | |
| `smtpMonitors[].dependsOn` | list[string] | — | each: resolves to a declared static monitor (HTTP or SMTP) | Shares the cycle-checked graph |

**Cross-field validation:**
- `timeout < interval` and `retries × (timeout + retryBackoff) < interval`.
- The decisive reply code persisted on success is the EHLO `250`; transport/TLS failures persist code `0` with the reason in the error.

---

## Env var interpolation in values

Any YAML scalar value can contain `${VAR}` references (docker-compose style). Env vars are expanded **before** YAML deserialization, so the rest of the schema sees the resolved string.

| Form | Behavior |
|---|---|
| `${VAR}` | Strict — error at parse time if `VAR` is unset (validator reports file/line). |
| `${VAR:-fallback}` | Use `fallback` if `VAR` is unset **or empty**. |
| `$$` | Literal `$` (escape). |

```yaml
database:
  host: ${DB_HOST:-toggle-monitor-pg-rw.cnpg-system.svc.cluster.local}
  port: ${DB_PORT:-5432}                            # quoted not needed when default is numeric-shaped
  name: ${DB_NAME:-toggle_monitor}

slack:
  channels:
    - slug: ops-alerts
      channelId: ${SLACK_OPS_CHANNEL_ID:-C0123ABCD}
      tokenEnv: SLACK_BOT_TOKEN                     # secrets STILL use *Env (not interpolation)
```

**Rules:**
- Works on **any string scalar** in YAML. For non-string target fields (int, duration, bool, list), wrap the value in quotes so the YAML parser sees a string first: `port: "${DB_PORT:-5432}"` — type coercion happens after interpolation.
- **Secrets are not interpolatable.** Use `tokenEnv` / `passwordEnv` for tokens and passwords — these names are the only way to pass a secret. This way:
  - Secrets can never accidentally end up in a YAML literal (e.g., `password: hunter2`).
  - The runtime knows by field name to wrap the value in `SecretString` for log masking.

---

## 5b. Status pages (optional)

Public, read-only pages served outside the operator nav. Each entry gets a unique slug and is served at `/status/<slug>`; `/status` itself lists every configured page. Omit the block (or set it to `[]`) to keep `/status` at the empty placeholder.

```yaml
statusPages:
  - slug: public
    title: "Toggle status"
    showSections: true
    showIncidents: false
    sections:
      - title: "Public APIs"
        match:
          - host: "*.example.com"
          - group: gateways
  - slug: internal
    title: "Internal tools"
    sections:
      - title: "Internal"
        match:
          - tags: [internal-tools]
```

| Field | Type | Required | Constraints | Notes |
|---|---|---|---|---|
| `statusPages[].slug` | string | ✓ | kebab-case (same rules as monitor/group slugs); unique across the list | URL segment for the page |
| `statusPages[].title` | string | optional | non-empty if set | Displayed as the page heading; falls back to `Status` |
| `statusPages[].showSections` | bool | optional (default `true`) | | Renders the section headings |
| `statusPages[].showIncidents` | bool | optional (default `false`) | | Opt-in surfacing of the last few alert events filtered to monitors in the page |
| `statusPages[].sections` | list | ✓ | non-empty | At least one section per page |
| `statusPages[].sections[].title` | string | ✓ | non-empty | |
| `statusPages[].sections[].match` | list | ✓ | non-empty | OR across selectors; AND within a selector |
| `statusPages[].sections[].match[].host` | string | optional | glob (`path.Match`) | Matched against monitor URL host |
| `statusPages[].sections[].match[].group` | string | optional | must reference a declared group | Exact slug match; mutually exclusive with `groupRegex` |
| `statusPages[].sections[].match[].groupRegex` | string | optional | valid Go regexp | Matched against monitor group slug |
| `statusPages[].sections[].match[].tags` | list[string] | optional | | Set overlap against `monitors[].tags` |

A monitor lands in a section when any one selector fires; within a selector the listed fields all have to match. The same monitor may appear in multiple sections — the status page is a curated view, not a strict partition.

---

## 5c. Alertmanager webhook receiver (optional)

First-class receiver for Prometheus Alertmanager webhooks. Omit the block to disable the endpoint entirely. Authoritative design: [ADR-0005 — Alertmanager webhook receiver](./adr/0005-alertmanager-webhook-receiver.md).

```yaml
alertmanager:
  endpoint:
    path: /webhooks/alertmanager                       # single-segment suffix under hardcoded /webhooks/
    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN               # Bearer token, env-resolved like every other secret
  retentionDays: 180                                   # daily sweep; only purges resolved incidents
  rateLimit:
    perChannel: 10                                     # flood threshold per Slack channel
    window: 30m                                        # sliding-window size
    noticeEvery: 1d                                    # min interval between flood-notice messages
  match:
    # Root rule — `when: {}` matches every alert and is MANDATORY.
    # Must set `config.slack`. `config.notify` cascades (union by
    # default; tag with `!override` to replace).
    - when: {}
      config:
        slack: ops-alerts
        notify: [ops-team]

    # Watchdog: Prometheus's keep-alive rule. Ignore so it never lands
    # in Slack; `final: true` keeps later rules from un-ignoring it.
    - when: { alertname: "Watchdog" }
      ignore: true
      final: true

    # Critical severity branches to a louder channel.
    - when:
        labels: { severity: "critical" }
      config:
        slack: ops-critical
```

The receiver's pipeline is deliberately thin — it persists per-fingerprint, posts to Slack, edits the parent message on resolve. AM alerts are not monitor probes; they share the `slack.channels[]` / `slack.userMapping` config but do not participate in `alert.Apply`, `coalesce.Manager`, or the `MonitorRow` machinery.

| Field | Type | Req | Validation | Notes |
|---|---|---|---|---|
| `alertmanager` | object | — | (the whole block) | Omit to disable the webhook endpoint. All fields below apply only when the block is present. |
| `alertmanager.endpoint.path` | string | — | `^/webhooks/[a-z0-9_-]+$`; default `/webhooks/alertmanager` | Single-segment suffix; the `/webhooks/` prefix is hardcoded. |
| `alertmanager.endpoint.tokenEnv` | string | ✓ | env var name regex `^[A-Z][A-Z0-9_]*$`; env var must be set and non-empty at startup | Bearer token compared in constant time against the `Authorization` header. |
| `alertmanager.retentionDays` | int | — | `>= 0`; default `180` | Daily sweeper purges incidents whose `ended_at` is older than the window. `0` keeps everything. Active incidents are never purged. |
| `alertmanager.rateLimit.perChannel` | int | — | `>= 0`; default `10` | Per-channel sliding-window flood detector. `0` disables the detector entirely. |
| `alertmanager.rateLimit.window` | duration | — | `> 0` when `perChannel > 0`; default `30m` | Sliding-window size for the detector. |
| `alertmanager.rateLimit.noticeEvery` | duration | — | `> 0` when `perChannel > 0`; default `1d` | Minimum gap between flood-notice messages on the same channel. |
| `alertmanager.match` | list | ✓ (when block present) | non-empty; first top-level rule must have empty (`{}` or omitted) `when:` and set `config.slack` (or a `slackFrom` with a `default:`) | The cascading rule tree. Mirrors `kube.match` grammar — see [ADR-0002](./adr/0002-kube-match-tree-cascade.md) for the merge / `!override` / `final` / `ignore` semantics. |
| `alertmanager.match[].when` | object | — | see selector table below | Absent or `{}` means "match anything." |
| `alertmanager.match[].when.alertname` | string | — | valid glob | Matched against the alert's `labels.alertname`. Mutually exclusive with `alertnameRegex` in the same `when:`. |
| `alertmanager.match[].when.alertnameRegex` | string | — | valid Go regexp; auto-anchored `^…$` | Mutually exclusive with `alertname`. |
| `alertmanager.match[].when.labels` | map[string]string | — | per-key twin convention (see below); each base key must satisfy k8s label-key syntax | A key suffixed `Regex` is the regex variant for the bare key; values are globs / regexes. All set keys AND together. |
| `alertmanager.match[].when.receiver` | string | — | exact match | Matched against the AM webhook envelope's `receiver`. |
| `alertmanager.match[].when.externalURL` | string | — | exact match | Matched against the AM webhook envelope's `externalURL`. Use to discriminate between multiple Alertmanagers feeding the same endpoint. |
| `alertmanager.match[].config.slack` | string | ✓ at root (or `slackFrom`) | resolves to a `slack.channels[].slug` | The root rule must set exactly one of `slack` or `slackFrom` (every alert inherits it). Descendants override selectively. |
| `alertmanager.match[].config.notify` | list[string] | — | each entry: a `slack.userMapping` slug OR raw `<…>` Slack markup | Union across cascade by default; tag with `!override` to replace. |
| `alertmanager.match[].config.slackFrom` | object | — (satisfies the root requirement only with a `default:`) | see below | Sources `slack` from a Namespace annotation. |
| `alertmanager.match[].config.notifyFrom` | object | — | see below | Sources `notify`; unions like the literal. |
| `alertmanager.match[].config.notifyOverrideFrom` | object | — | see below | Sources `notify`; replaces the baseline at this rule's position. |
| `alertmanager.match[].nested` | list[rule] | — | each entry follows the same rule shape | Recursive. |
| `alertmanager.match[].ignore` | bool | — | default unset | Cascades like a scalar; deepest matching rule wins. Resolved `true` → no Slack post, no `am_alerts` row. |
| `alertmanager.match[].final` | bool | — | requires non-empty `when:` | Halts the tree walk after descending into this rule's `nested:`. |

### Annotation value sources under `alertmanager.match`

Authoritative design: [ADR-0013](./adr/0013-from-value-sources-for-alertmanager-routing.md).

Ownership lives on the Namespace, and both cascades read it from there. An `alertmanager.match` rule sources a routing field the same way a `kube.match` rule does, with two differences: only the Namespace scope is available, and the namespace name comes from one of the alert's labels.

```yaml
alertmanager:
  match:
    - when: {}
      config:
        slackFrom:
          namespaceAnnotation: app.example.com/slack
          default: ops-alerts                 # required when slackFrom stands in for the root's slack:
        notifyFrom:
          namespaceAnnotation: app.example.com/notify
          namespaceLabel: exported_namespace  # optional; default `namespace`
```

`namespaceLabel:` names the alert label carrying the namespace, defaulting to `namespace`. Set it when an exporter relabels (`exported_namespace`, `kubernetes_namespace`); different rules may read different label keys.

Merge semantics and the degradation table above apply unchanged. `default:` parsing is the same except that a scalar source rejects a sequence `default:` here, rather than silently reading it as empty. Three keys exist here (`slackFrom`, `notifyFrom`, `notifyOverrideFrom`) because those are the two fields an AM rule carries.

**Load-time errors** (refuse to start), in addition to the shared ones above:

- `annotation:` — an alert's own annotations are written by whoever authored the alerting rule, not by the workload's owner, so they are not a routing source. Only `namespaceAnnotation:` is accepted.
- `namespaceAnnotation:` with no `kube:` block. The Namespace informer belongs to the kube watcher; without it there is nothing to read through.
- A root `slackFrom` with no `default:`. ADR-0005 requires the root to set a channel for every alert, and an unannotated namespace would otherwise resolve to none.
- A `namespaceLabel:` that isn't a valid Prometheus label name (`^[a-zA-Z_][a-zA-Z0-9_]*$`). This is the alert's label grammar, not the k8s one: no dots, dashes or slashes, but a leading underscore is fine.
- `notifyFrom` / `notifyOverrideFrom` with an empty `slack.userMapping`. An annotation may only select handles from the roster and may never set raw `<…>` markup, so such a source could never contribute a value.
- A sequence `default:` on `slackFrom` (a scalar field takes one value).

**Runtime degradation** is the same principle — an alert is never dropped and never routed to a channel that does not exist, because the fallback chain terminates at the root's literal. Four AM-specific cases:

| situation | behaviour | reported? |
|---|---|---|
| the alert carries no `namespaceLabel` label | `default:` if present, else the cascade value stands | no — cluster-scoped rules (`Watchdog`, node pressure) have no namespace by nature |
| the namespace carries no annotations, or is not in the informer cache | same | no — these are indistinguishable, and an unannotated namespace is ordinary |
| kube discovery disabled, or the watcher not yet wired (the endpoint serves before the informer is attached at startup) | same — a webhook arriving in that window routes to the root channel | yes, `no_annotation_source` |
| the annotation is present but its value is unusable | as in the shared table above | yes, `value_rejected` |

Only the last two are reported, as a `WARN` `am.value_source.rejected` log line (with the alert fingerprint and reason) and a point on `toggle_monitor_am_value_source_rejections_total{field,reason}`. The first two are silent on purpose: they occur on ordinary traffic, and counting them would keep the counter permanently non-zero. The cost is that a misspelled `namespaceLabel:` is not flagged — diagnose it from the `am_alerts.rule_chain` column, which will show no provenance for that field.

Rejections do **not** appear on `/issues`: that page lists a current set, and AM rules are evaluated once per inbound alert with no reconcile loop to define one. The shipped `PrometheusRule` (`prometheusRule.amValueSources`) alerts on `increase(…) > 0` over a long window, because rejections arrive only as often as Alertmanager re-delivers the offending alert. The counter counts deliveries, not broken namespaces — a busy namespace increments far more than a quiet one with the same mistake, so read it as a yes/no signal and go to the logs for the detail.

Accepted values are appended to the `am_alerts.rule_chain` debug column as provenance — `match[0] → match[1] (labels.namespace=acme-*) | slack=acme-alerts ← namespaceAnnotation app.example.com/slack` — which is the AM tree's only debugging surface; there is no `explain` subcommand for it.

### `alertmanager.match[].when.labels` — per-key twin convention

Inside `when.labels`, a key `K` selects on `labels.K` with glob semantics; a key `KRegex` selects on `labels.K` with Go-regexp semantics (auto-anchored `^…$`). Setting both `K` and `KRegex` on the same `when:` is a validation error — pick one. Example:

```yaml
when:
  labels:
    severity:       "critical"             # glob value
    namespace:      "acme-*"               # glob value
    instanceRegex:  "^pod-\\d+$"           # regex value; selects on `labels.instance`
```

Base keys must satisfy k8s label-key syntax (the same check applied to `kube.match[].when.labels` keys).

### Validation

**Structural errors** (refuse to start):
- `endpoint.path` does not match `^/webhooks/[a-z0-9_-]+$`.
- `endpoint.tokenEnv` empty or fails the env-var name regex.
- `retentionDays < 0`.
- `rateLimit.perChannel < 0`.
- `rateLimit.perChannel > 0` with `window <= 0` or `noticeEvery <= 0`.
- Missing root rule or root rule's `config.slack` missing.
- Orphan slug references inside any `config:` block (`slack`, `notify` entries that don't resolve to `slack.userMapping`).
- Within any `when:`, both `alertname` and `alertnameRegex` set.
- Within any `when.labels`, the same base key set as both `K` and `KRegex`.
- `final: true` with empty `when:`.
- Invalid Go regex (per `regexp.Compile`), invalid glob (per `path.Match`), or invalid label-key syntax.

The receiver inherits `slack.channels[]` and `slack.userMapping` — no new Slack config surface is introduced.

---

## 6. Schema-level rules

**Recognized top-level keys** (anything else is a typo unless prefixed `x-`):
- `displayTimezone`, `publicBaseURL`, `dbBodyMaxChars`
- `kube`, `ui`, `theme`, `httpClient`, `heartbeat`, `database`
- `slack`
- `proxies`
- `groups`, `monitors`
- `statusPages`
- `alertmanager`
- `x-*` — ignored (docker-compose-style anchor host)

**Validator behavior:**
- **Strict on unknown keys at every level** — any key absent from the corresponding struct's yaml tag set is a hard error. Catches typos like `monitor:` (vs `monitors:`) at the top level, `nestedd:` inside a `kube.match` rule, `final:` placed inside `config:` instead of as a sibling, and the same for `slack`, `monitors[]`, `statusPages[]`, etc. Keys merged in via YAML `<<: *anchor` are validated against the destination struct's allowlist too. The `x-*` escape hatch is honoured **only at the top level**; user-keyed maps (`when.labels`, `slack.userMapping`) accept arbitrary keys.
- **Multiple errors reported per run** — not first-error-and-exit. Errors include file line + column numbers from `yaml.v3` node positions, format: `line 42, col 7: monitors[0].interval: must be >= 30s, got 10s`.

**Comments:** YAML `# ...` comments are stripped by the parser before validation. Recommended next to channel IDs / user IDs as human labels.

**Duration format:** Go-style strings extended to support `d` (days). Examples: `30s`, `5m`, `1h`, `3d`, `30d`.

**URL fields:** require valid scheme; `monitors[].url` and `publicBaseURL` accept only `http`/`https`.

**Versioning:** no `version:` field for v1. Schema is implicitly tied to the binary version; breaking changes require manual config update (LLM-assisted in practice). If a version field is ever needed later, `x-version:` is the natural choice (initially ignored, promoted once meaningful).
