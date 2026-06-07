# ADR 0005 — Alertmanager webhook receiver

**Status:** Accepted
**Date:** 2026-06-04
**Supersedes:** nothing — net-new feature.

## Context

Toggle-monitor's HTTP/SMTP probe loop covers what we control: an
endpoint we can hit, a TLS chain we can inspect, a port we can dial.
Anything emitted by the cluster's own monitoring stack — kube-state
counters, node-pressure rules, recording-rule firings — sits behind
Prometheus + Alertmanager and never gets touched. Today that
half-of-the-picture is bolted on by PagerDuty: AM webhooks to PD, PD
fans out to Slack, operators read two unrelated message styles in the
same channel. The seam shows up as inconsistent formatting, two
retention policies, two audit trails, and a Slack token that has
nothing to do with the rest of the binary.

This ADR adds a first-class Alertmanager webhook receiver to
toggle-monitor so the AM-side pipe disappears. The endpoint accepts
AM webhooks v4, persists every fingerprint+incident, routes each
fingerprint to a Slack channel via a `kube.match`-style cascading
match tree, and edits the parent message on resolve. The pipeline is
deliberately thin: no `alert.Apply`, no `coalesce.Manager`, no
`MonitorRow` — AM alerts are not probes and don't share the monitor
state machine.

The settled design fell out of /grill-me 2026-06-04. The receiver
sits beside `kube:` as a sibling top-level block, reuses
`slack.channels[]` and `slack.userMapping`, and ships behind two
in-binary tables (`am_alerts`, `am_alert_events`) plus a `/alerts`
listing in the existing UI.

## Decision

### Integration shape

Thin pass-through. AM webhook arrives → handler validates the
envelope → match tree resolves a `slack:` channel + `notify:` list per
fingerprint → idempotency row goes into `am_alerts` → Slack post fires
→ `slack_ts` is recorded back. Resolves edit the parent and reply in
thread. No `alert.Apply`, no `coalesce.Manager` participation, no
`MonitorRow`. AM alerts are first-class but separate from probes.

### Config block

Sibling of `kube:` at the top level:

```yaml
alertmanager:
  endpoint:
    path: /webhooks/alertmanager
    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN
  retentionDays: 180
  rateLimit:
    perChannel: 10
    window: 30m
    noticeEvery: 1d
  match:
    - when: {}
      config:
        slack: ops-alerts
        notify: [ops-team]
    - when: { alertname: "Watchdog" }
      ignore: true
      final: true
    - when:
        labels: { severity: "critical" }
      config:
        slack: ops-critical
```

### Identity & idempotency

Per-fingerprint, per-incident. One row in `am_alerts` is keyed by AM's
`fingerprint` plus a synthetic `incident_id`; a partial unique index
on `(fingerprint) WHERE ended_at IS NULL` makes "one open Slack
message per fingerprint" enforceable at the database. Repeated
firing webhooks for the same fingerprint find the existing row and
no-op; resolves stamp `ended_at` and edit the parent. The DB-INSERT
happens before the Slack-post — if the process crashes between the
two, the next AM redelivery sees `slack_ts IS NULL` and re-posts. The
worst case is at-most-twice delivery on crash, acceptable for v1.

### Endpoint, auth, body cap

The endpoint path is a single-segment suffix under a hardcoded
`/webhooks/` prefix:

- Default `/webhooks/alertmanager`.
- Validated as `^/webhooks/[a-z0-9_-]+$` — one segment, lowercase
  alnum + `-` / `_`.
- The hardcoded prefix removes a class of "operator misconfigures the
  receiver to overlap an existing UI route" mistakes and makes the
  surface trivially discoverable (`grep /webhooks/ docs/`).

Auth is a single Bearer token in the `Authorization` header. The
config field names the env var (`tokenEnv`); the runtime reads the
env var, constant-time-compares it against the header, and is fatal
at startup if the env var is empty. No mTLS, no IP allow-list, no
multi-token rotation grace — adding either is a v2 conversation.

Body cap is 10 MiB at the toggle-monitor handler. Operators behind
Traefik (or any other ingress that buffers) get a separate
documentation note in `docs/alertmanager.md` (later slice) for the
matching buffering middleware. Payloads larger than the cap return
413; AM retries are well-behaved against that.

### Persistence

Two tables, both AM-specific (no `backend` column — added when the
first concrete per-backend feature shows up):

- `am_alerts` — one row per `(fingerprint, incident_id)`. Columns
  include `external_url` and `receiver` (AM-native provenance),
  `slack_channel_id`, `slack_ts`, `started_at`, `ended_at`,
  resolved-at, the resolved match-chain, and the raw payload (for
  the detail page).
- `am_alert_events` — append-only audit trail; one row per AM
  webhook delivery referencing the originating `am_alerts` row.

Schema is defined in the next slice (not this commit). Retention is
180 days by default, swept once daily, only purging incidents whose
`ended_at` is older than the window. Active incidents never get
purged.

### Match tree

Mirrors `kube.match` exactly: `when` / `config` / `nested` / `ignore`
/ `final`, accumulate-merge with deeper-overrides-shallower for
scalars and union-with-`!override` for arrays. The root rule
(top-level `when: {}` or absent) is required and must set `slack:`.

Selector vocabulary inside `when:`:

```yaml
when:
  alertname:      "HighCPU"               # glob (path.Match)
  alertnameRegex: "Pod.*"                 # Go regex, auto-anchored ^...$
  labels:                                  # per-key twin convention
    severity:       "critical"             # glob value
    namespace:      "acme-*"               # glob value
    instanceRegex:  "^pod-\\d+$"           # regex value (key suffix)
  receiver:       "toggle_monitor"         # exact, payload envelope
  externalURL:    "https://am.prod.example.test"  # exact, payload envelope
```

`config:` carries only two fields in v1: `slack` (required at root)
and `notify` (union/`!override`). No `tags`, `mention`, `group`,
`friendlyName` — AM alerts aren't probes; the rendered Slack message
is hardcoded per the renderer section below.

### Rate limit (flood detector, not a coalescer)

Per-channel sliding-window detector: count fingerprints posted into
each channel in the last `rateLimit.window` (default 30m); when the
count exceeds `rateLimit.perChannel` (default 10), emit one
operator-visible notice into the channel ("AM has posted ≥10 alerts
to #ops-critical in the last 30m") and suppress further notices for
`rateLimit.noticeEvery` (default 1d). This is a flood detector — it
does not batch, drop, or rewrite alerts; it just nudges the operator
when something is misconfigured.

### Multi-alert batching

AM webhooks carry an `alerts[]` array. Each alert is processed
independently as if it were its own webhook (same idempotency, same
match cascade, same persistence), with a bounded-worker pool
(concurrency 8) and a 3s per-post timeout against Slack. Any
transient failure inside the array returns HTTP 5xx so AM retries
the entire delivery — combined with the partial unique index, that
gives at-most-twice posting on crash and exactly-once on success.

### Slack rendering

> **Superseded by [ADR 0006](0006-slack-rendering-blocks-only-parent-shape.md)
> (2026-06-07).** The block-kit shape sketched below — severity-emoji
> header, summary body, View-details / Runbook button row — is replaced
> by the three-block iA2 contract (title section + body section + footer
> context, no attachments, inline mrkdwn links instead of buttons). The
> rest of this ADR (receiver architecture, persistence, match tree,
> rate limit, etc.) stays accepted as-is.

Hardcoded format, not operator-configurable in v1:

- Header: severity emoji + `alertname` + the two or three key labels
  (cluster, namespace, instance).
- Body: `annotations.summary`.
- Buttons: "View details" → toggle-monitor `/alert/{incident_id}`
  page, "Runbook" → `annotations.runbook_url` if set.
- On resolve: edit the parent message in place (status flips), reply
  in thread with "Resolved at HH:MM" and the firing duration.

If the resolve webhook arrives before any firing webhook (a real AM
edge case: short-lived alerts), the receiver posts a standalone
message with a banner — mirrors the existing
`freshParentBanner` convention in the probe path. The banner says
"This alert resolved before we ever heard it fire."

### UI

- `/alerts` — listing page, mirrors the existing `/monitors` style
  (paged, sortable, filter by channel / receiver / severity).
- `/alert/{incident_id}` — detail page with header / routing /
  Slack / fingerprint history / raw payload sections.

The UI lives in `internal/web`. A new `web.Server.RegisterRoute`
method lets the AM package install its handler without
`internal/web` reaching back into `internal/alertmanager`.

### Observability

- AM-scoped Prometheus metrics — separate counters from the existing
  `slack_post_total` so the AM volume is distinguishable. Names live
  in the next slice.
- Per-request `request_id` in structured logs.
- INFO on accept, WARN on rate-limit notice, ERROR on persistent
  failure. Sentry forward on protocol parse failure and on
  irrecoverable persistence errors.

### Package layout

```
internal/alertmanager/
  handler.go          # webhook handler
  match.go            # cascade evaluator
  blocks.go           # Slack block rendering
  ratelimit.go        # per-channel flood detector
  sweeper.go          # retention sweeper
internal/store/
  am_alerts.go        # store methods
internal/web/
  alerts.go           # /alerts listing + /alert/{id} detail
db/migrations/
  XX_am_alerts.up.sql # later slice
```

`internal/alertmanager` is the new package; this slice doesn't touch
it. The receiver reuses `slack.channels[]`, `slack.userMapping`, and
the existing Slack client plumbing — no new Slack config surface.

### Out of scope for v1

- **Async / queued Slack delivery.** The synchronous post is simple
  and the structure leaves a clear seam for a queue; defer.
- **Multi-token rotation with overlap window.** v1 takes a single
  token; rotation is "stop traffic, swap the env var, redeploy".
- **Per-receiver multi-endpoint isolation.** One endpoint; if
  operators want to distinguish payloads from two Alertmanagers they
  do it inside `when:` via `externalURL` and `receiver`.
- **A `backend` column on the AM tables.** Adds with the first
  concrete per-backend feature (e.g., a Grafana-OnCall receiver).
- **AM alerts as `dependsOn` participants.** AM alerts are
  independent of the probe state machine.
- **mTLS / IP allow-list auth.** Bearer token only.
- **AM webhook payload `version != "4"`.** The receiver requires v4
  and returns 400 on anything else.

## Consequences

### Code changes (this commit-set is one slice — see TODO for the rest)

- **`internal/config/alertmanager.go`** (this slice) — new file.
  `AlertmanagerConfig` and its substructures, top-level `Config.Alertmanager`
  field, defaults at load time, `validateAlertmanager` covering the
  rules below.
- **`internal/alertmanager/`** (next slice) — handler + cascade
  evaluator + rate-limit + sweeper.
- **`internal/store/am_alerts.go`** (next slice) — DB-INSERT-first
  idempotency.
- **`internal/web/alerts.go`** (later slice) — `/alerts` listing +
  `/alert/{id}` detail.
- **`db/migrations/`** (next slice) — `am_alerts`, `am_alert_events`,
  partial unique index on open incidents.
- **`docs/alertmanager.md`** (later slice) — operator-side AM helm-values
  integration guide.

### Documentation changes (this commit)

- `docs/adr/0005-alertmanager-webhook-receiver.md` — this file.
- `docs/config-schema.md` — new `alertmanager:` section covering
  endpoint, retention, rate limit, match grammar, validation rules.
- `docs/config-example.yaml` — `alertmanager:` block with a working
  match tree (Watchdog ignore, critical-severity routing, multi-AM
  `externalURL` selector).

### Validation rules (enforced in the validator added this slice)

- `endpoint.path` matches `^/webhooks/[a-z0-9_-]+$`.
- `endpoint.tokenEnv` is non-empty (field-level check only — the env
  var resolution itself is a runtime concern).
- `retentionDays >= 0`.
- `rateLimit.perChannel >= 0`; when `> 0`, both `window > 0` and
  `noticeEvery > 0`.
- `match` has a root rule (the first top-level rule with empty/absent
  `when:`); the root must set `config.slack`.
- Every `config.slack` slug resolves to a `slack.channels[].slug`.
- Every non-`@…` `config.notify` entry resolves to a
  `slack.userMapping` slug.
- Within `when:`, `alertname` and `alertnameRegex` are mutually
  exclusive.
- Within `when.labels`, a key `K` and a key `KRegex` for the same
  base key are mutually exclusive.
- `final: true` requires a non-empty `when:`.
- Regexes must compile (`regexp.Compile`); globs must parse
  (`path.Match`); label keys must satisfy k8s label-key syntax.

### Operator-visible breaking changes

None — this is net new. Operators wanting the feature add an
`alertmanager:` block; absent block leaves the binary's behaviour
unchanged.

### Trade-offs accepted

- **Per-fingerprint identity, not per-AM-group.** AM groups alerts
  for fan-out; we explicitly disaggregate. Operators who liked
  PagerDuty's group view will find this verbose at first. Justified
  by: groups already have a unstable identity (group_key changes when
  labels change), and per-fingerprint maps cleanly onto the
  toggle-monitor incident concept.
- **No queue, synchronous Slack post inside the webhook handler.**
  AM tolerates 5s response times; our 3s per-post + bounded
  concurrency 8 stays inside that budget for any realistic
  `alerts[]` size. If a real production AM ever bursts past that,
  the structure leaves room for a queue with minimal refactor.
- **At-most-twice posting on crash.** A crash between DB-INSERT and
  Slack-post leaves a row with `slack_ts IS NULL`; the next AM
  redelivery re-posts. Picked over "transactional outbox" because the
  outbox + worker would more than double the moving parts for a case
  that fires on the order of once per binary-lifetime.

## References

- ADR-0002 — `kube.match` cascading tree; the AM match tree mirrors
  its grammar verbatim.
- /grill-me 2026-06-04 — the settled-design conversation, including
  the trade-offs deliberately deferred to v2.
- `docs/config-schema.md` §"Alertmanager webhook receiver" — the
  field-level reference for this ADR.
- `docs/alertmanager.md` (later slice) — the AM-side helm-values
  integration guide.
