# toggle-monitor — config schema (v1)

Locked field-by-field schema for the YAML ConfigMap. Companion to [`design-decisions.md`](./design-decisions.md). Built incrementally during the design grilling.

The binary refuses to start if any required field is missing or fails validation. The CLI subcommand `toggle-monitor validate <path>` runs the same validation locally for CI use.

---

## 1. Top-level

```yaml
displayTimezone: Asia/Kathmandu
publicBaseURL: https://monitor.internal.example.com    # optional; omit to hide Slack [View details] button
dbBodyMaxChars: 4000

kube:
  annotationDomain: monitor.togglecorp.com
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
| `kube.annotationDomain` | string | ✓ | DNS-name format | Base for `<domain>/kube.*` and `<domain>/config.*` ingress annotations |
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
| `slack.channels[].slug` | string | ✓ | slug regex; unique across `slack.channels:` | Referenced by `monitors[].slack` and `kube.presets[].slack` |
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
  - slug: corp                              # referenced by monitors[].proxy / kube.presets[].proxy
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

  - slug: kube-discovered                               # REQUIRED — fallback for ingress monitors without config.group
    friendlyName: Kube Discovered
    description: Auto-discovered ingresses
```

| Field | Type | Req | Validation | Notes |
|---|---|---|---|---|
| `groups` | list | ✓ | non-empty; must include a group with slug `kube-discovered` | |
| `groups[].slug` | string | ✓ | slug regex; unique across `groups:` | Referenced by `monitors[].group`, `kube.presets[].group`, `<base>/config.group` annotation |
| `groups[].friendlyName` | string | ✓ | non-empty | |
| `groups[].description` | string | — | | |
| `groups[].logoUrl` | string | — | valid URL | Optional logo for group cards |
| `groups[].color` | string | — | `^#[0-9a-fA-F]{6}$` | Hex color; if absent uses `theme.defaultGroupColor` |

**Behavior:**
- Validator rejects start if no group with slug `kube-discovered` exists.
- Validator rejects orphan references — every `monitors[].group` and `kube.presets[].group` value must resolve to a declared slug.
- Group with zero monitors is allowed silently.
- Display order in the UI follows the array order in `groups:`.

---

## 4. Kube presets (auto-discovery)

```yaml
kube:
  annotationDomain: monitor.togglecorp.com
  resyncInterval: 30m
  pause:                                            # optional; hard-pause list (host-based)
    - host: api.foo.example.com
      reason: "Maintenance until 2026-06-01"        # optional
    - host: "*.staging.example.com"                 # glob supported
  match:                                            # optional; first match wins
    - when: { namespace: "test-*" }                 # ignore short-lived test namespaces
      ignore: true
    - when: { namespace: "prod-*" }
      preset: internal-api
    - preset: public-web                            # wildcard fallback (no when:)
  presets:
    - slug: internal-api
      # URL construction
      scheme: https                                     # https | http (default https)
      path: /health

      # HTTP check params
      httpMethod: GET                                   # GET | HEAD | POST | PUT | DELETE
      acceptedStatusCodes: [200]
      interval: 5m
      timeout: 10s
      retries: 2
      retryBackoff: 5s
      followRedirects: false

      # Reminders & SSL
      reminderInterval: 3d
      sslAlertThreshold: 30d
      sslEscalationThreshold: 7d
      sslReminderInterval: 3d

      # Slack & mentions
      slack: ops-alerts
      notify: [alice]                                    # optional

      # Group & metadata
      group: production-apis                             # optional (falls back to kube-discovered)
      tags: [internal]                                   # optional
      dependsOn: [bastion-proxy]                         # optional; must resolve to a static monitor
```

| Field | Type | Req | Validation | Notes |
|---|---|---|---|---|
| `kube.pause` | list | — | optional | Hard-pause kube-discovered monitors by host; matched monitors get `kube-paused` status |
| `kube.pause[].host` | string | ✓ | non-empty; may include `*` glob | Matched against `ingress.spec.rules[].host` |
| `kube.pause[].reason` | string | — | | Surfaces in the auto-discovery UI |
| `kube.match` | list | — | optional | Conditional preset/ignore rules; evaluated in order, first match wins. A rule with no `when:` is the wildcard fallback (must be last). |
| `kube.match[].when.namespace` | string | — | glob (path.Match) | Both `namespace`/`host` are optional; an empty `when` (no namespace, no host) marks the rule as the wildcard fallback |
| `kube.match[].when.host` | string | — | glob (path.Match) | AND-ed with `namespace` when both set |
| `kube.match[].preset` | string | — | resolves to a `kube.presets[].slug` | **Exactly one** of `preset` or `ignore` must be set per rule |
| `kube.match[].ignore` | bool | — | default `false` | When `true`, the ingress is skipped entirely (no monitor); a `kube-ignored` snapshot row still lands on `/discovery` so the rule remains visible/filterable |
| `kube.presets` | list | — | optional (no kube auto-discovery if absent) | Each preset materializes when an ingress carries `<base>/kube.preset: <slug>` |
| `kube.presets[].slug` | string | ✓ | slug regex; unique across `kube.presets:` | |
| `kube.presets[].scheme` | enum | ✓ | `https` or `http` | URL scheme for built URL |
| `kube.presets[].path` | string | ✓ | starts with `/` | Appended to ingress host |
| `kube.presets[].httpMethod` | enum | ✓ | one of: `GET`, `HEAD`, `POST`, `PUT`, `DELETE` | |
| `kube.presets[].acceptedStatusCodes` | list[int] | ✓ | non-empty; each 100–599 | **Replaced (not merged)** on override |
| `kube.presets[].interval` | duration | ✓ | >= 30s | |
| `kube.presets[].timeout` | duration | ✓ | < interval | |
| `kube.presets[].retries` | int | ✓ | >= 0 | |
| `kube.presets[].retryBackoff` | duration | ✓ | >= 1s | |
| `kube.presets[].followRedirects` | bool | ✓ | | |
| `kube.presets[].tlsInsecureSkipVerify` | bool | — | default `false` | Same semantics as `monitors[].tlsInsecureSkipVerify`. |
| `kube.presets[].proxy` | string | — | resolves to a `proxies[].slug` | Same semantics as `monitors[].proxy`. |
| `kube.presets[].reminderInterval` | duration | ✓ | >= 1h | |
| `kube.presets[].sslAlertThreshold` | duration | ✓ | > `sslEscalationThreshold` | |
| `kube.presets[].sslEscalationThreshold` | duration | ✓ | > 0 | |
| `kube.presets[].sslReminderInterval` | duration | ✓ | >= 1h | |
| `kube.presets[].slack` | string | ✓ | resolves to a `slack.channels[].slug` | |
| `kube.presets[].notify` | list[string] | — | each entry: a `slack.userMapping` slug OR `<...>` raw markup | Merged (union) on override |
| `kube.presets[].group` | string | — | resolves to a `groups[].slug` | If absent, falls back to `kube-discovered` |
| `kube.presets[].tags` | list[string] | — | each: slug regex | Merged (union) on override; `kube` always auto-added |
| `kube.presets[].dependsOn` | list[string] | — | each: resolves to a **static** `monitors[].slug` (kube-discovered cannot be a parent) | Merged (union); validator detects cycles |

**Cross-field validation:**
- `retries × (timeout + retryBackoff) < interval` (per Q10c). Refuse on violation.
- `acceptedStatusCodes` must be non-empty (empty would mean "accept nothing" → useless).

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
