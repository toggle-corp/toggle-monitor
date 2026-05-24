# toggle-monitor — config schema (v1)

Locked field-by-field schema for the YAML ConfigMap. Companion to [`design-decisions.md`](./design-decisions.md). Built incrementally during the design grilling.

The binary refuses to start if any required field is missing or fails validation. The CLI subcommand `toggle-monitor validate <path>` runs the same validation locally for CI use.

> **Partially superseded by ADRs.** Treat the linked ADRs as authoritative where they conflict with the prose below:
>
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

- Within a single `when:`, all set fields AND together.
- `labels` matches the Ingress's **own** `metadata.labels` only — not labels on the backing Service / Deployment / Pod.
- No `name:` selector in v1 — namespace + labels cover realistic cases.

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

### `kube.*` field reference

| Field | Type | Req | Validation | Notes |
|---|---|---|---|---|
| `kube.resyncInterval` | duration | ✓ | >= 1m | k8s informer resync (also referenced in §1). |
| `kube.friendlyName` | enum | — | one of: `compact`, `plain`, `dedupe`, `title` (default `compact`) | Auto-generated display name style for kube-discovered monitors. |
| `kube.match` | list | ✓ | non-empty; first top-level rule must have empty (`{}` or omitted) `when:` | The cascading rule tree. |
| `kube.match[].when` | object | — | see selector table | Absent or `{}` means "match anything." |
| `kube.match[].when.namespace` | string | — | valid glob | Mutually exclusive with `namespaceRegex` in the same `when:`. |
| `kube.match[].when.namespaceRegex` | string | — | valid Go regexp; auto-anchored `^…$` | Mutually exclusive with `namespace`. |
| `kube.match[].when.host` | string | — | valid glob | Mutually exclusive with `hostRegex`. |
| `kube.match[].when.hostRegex` | string | — | valid Go regexp; auto-anchored | Mutually exclusive with `host`. |
| `kube.match[].when.labels` | map[string]string | — | valid k8s label key syntax for each key | Matched against `ingress.metadata.labels`; all keys AND. |
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
<namespace>__<ingress-name>__<host>
```

Double-underscore separator avoids collision with single-dash content in any of the three parts. The slug carries no rule-derived component: the monitored thing is an `(Ingress, host)` pair and already has stable k8s identity. Pulling slug from config would mean a config edit can rename a monitor (breaking history, bookmarks, `dependsOn` references).

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

**CLI `toggle-monitor explain` subcommand.** Resolves the rule chain + final config either for a live cluster Ingress or for a hypothetical `(namespace, labels, host)` tuple:

```
toggle-monitor explain --ingress acme-service-a-eoapi-3/web
toggle-monitor explain --ingress acme-service-a-eoapi-3/web --host api.example.com
toggle-monitor explain \
  --namespace acme-service-a-eoapi-3 \
  --labels app.kubernetes.io/name=minio \
  --host api.example.com
```

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
| `monitors[].dependsOn` | list[string] | — | each: resolves to a **static** `monitors[].slug` | Validator detects cycles |

**Cross-field validation:**
- `retries × (timeout + retryBackoff) < interval`.
- SSL fields (`sslAlertThreshold`, `sslEscalationThreshold`, `sslReminderInterval`) are required when `url` is HTTPS; optional but allowed when `url` is HTTP (ignored at runtime). Allows anchor reuse across HTTP and HTTPS monitors without splitting anchors.
- Slug uniqueness across **all** monitors. At config-load static slugs are checked; kube-discovered slug conflicts (e.g., a kube slug colliding with a static slug) surface in the auto-discovery snapshot at reconcile time.

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

## 6. Schema-level rules

**Recognized top-level keys** (anything else is a typo unless prefixed `x-`):
- `displayTimezone`, `publicBaseURL`, `dbBodyMaxChars`
- `kube`, `ui`, `theme`, `httpClient`, `heartbeat`, `database`
- `slack`
- `proxies`
- `groups`, `monitors`
- `statusPages`
- `x-*` — ignored (docker-compose-style anchor host)

**Validator behavior:**
- **Strict on unknown top-level keys** — any key not in the list above and not prefixed `x-` is a hard error (catches typos like `monitor:` instead of `monitors:`).
- **Multiple errors reported per run** — not first-error-and-exit. Errors include file line numbers from `yaml.v3` node positions, format: `config.yaml:42: monitors[0].interval must be >= 30s, got 10s`.

**Comments:** YAML `# ...` comments are stripped by the parser before validation. Recommended next to channel IDs / user IDs as human labels.

**Duration format:** Go-style strings extended to support `d` (days). Examples: `30s`, `5m`, `1h`, `3d`, `30d`.

**URL fields:** require valid scheme; `monitors[].url` and `publicBaseURL` accept only `http`/`https`.

**Versioning:** no `version:` field for v1. Schema is implicitly tied to the binary version; breaking changes require manual config update (LLM-assisted in practice). If a version field is ever needed later, `x-version:` is the natural choice (initially ignored, promoted once meaningful).
