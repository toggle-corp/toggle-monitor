---
status: proposed
date: 2026-08-13
deciders: [monitoring-team]
amends:
  - "0002-kube-match-tree-cascade.md (partial — the `### Annotations: removed` sub-section only)"
---

# ADR 0009 — `*From` value sources for kube discovery

**Status:** Proposed
**Date:** 2026-08-13
**Amends:** ADR-0002 (partial). The cascading `kube.match[]` tree, its
merge rules, `final:`/`ignore:`, and the root-required constraint all
stand unchanged. This record reverses exactly one sub-decision — that
ingress annotations contribute nothing — and replaces it with a
narrower mechanism.

## Context

ADR-0002 deleted every ingress annotation and justified it in one
sentence: "Toggle-monitor's app team and monitoring team are the same
humans; the decentralized-self-service justification for per-ingress
annotations does not apply."

That premise turned out to be false in a way nobody checked. A survey
of the live cluster on 2026-08-13 (114 Ingresses) found the
cluster **already declaring its own monitoring data**:

| annotation | count | values |
| --- | --- | --- |
| `app.example.com/health-check` | 24 | `/health-check/` (13), `/minio/health/live` (10), `/livez` (1) |
| `app.example.com/url` | 32 | `/admin/` on APIs, `/` on frontends |
| `app.example.com/description` | 32 | free text |

Coverage tracks the `app.example.com/stack` label exactly:
`stack=banjo` → 24/24 annotated; `stack=web-app-serve` → 0/17
(frontends, `/` by convention); no stack label → 0/71. It is
**chart-emitted, not hand-written** — a shared-chart bump, not 71
manual edits.

The two values that dominate are precisely the ones ADR-0002 cited as
motivation for the cascade tree: "per-app health-check endpoints
(`minio` wants `/minio/health/live`; `stac-auth-proxy` wants
`stac/_mgmt/ping`)". The tree solved preset explosion, but it did so by
re-deriving, in reviewed YAML, a value the cluster already carried.

Four concrete costs were measured in `tc/uptime-local`'s config:

1. **Duplication.** 17 hand-copied `path: /health-check/` rules.
2. **A live bug.** `acme-api-11/mailpit` was probed at
   `/api/v2/brief` — inherited from the `acme-api-*` namespace rule
   — while its own ingress had declared `app.example.com/health-check:
   /livez` all along.
3. **Tag drift.** Hand-written tags were already inconsistent with the
   labels: 7 minio ingresses carried `backend` (inherited from
   namespace-level rules), 8 label-identified frontends never got
   `web-app`, and `acme-cms-1` resolved to no tags at all — invisible
   on every status page.
4. **Growth with N.** Projects are deployed as numbered instances
   (`acme-service-a-1`, `-2`, …). Every new instance family needs a
   `when:` subtree carrying `notify`, `slack`, and `tags`. At
   `tc/uptime-local`'s current committed revision the `kube.match` tree
   spends 64 lines on tags, 24 on notify and 7 on slack, held up by
   ~145 lines of `when:`/`config:` scaffolding.

One asymmetry constrains the design. `alertmanager.match` carries a
**parallel ownership map** — 18 `notify` rules keyed on the alert's
`namespace` label, with the same values as the kube tree, including the
same `!override [alice, bob, carol]` for one project.
Prometheus alerts have no Ingress in the loop, so an ingress-scoped
annotation cannot feed that tree. Moving kube `notify` to ingress
annotations alone would not remove the ownership map from config; it
would split it across two mechanisms and hide the drift that today is
visible in a single file review.

## Considered Options

1. **Status quo — everything in config.** Keep re-deriving per-app
   paths in the tree.
2. **Restore the pre-ADR-0002 annotation override layer.** Annotations
   apply on top of a resolved preset, with their own CSV union/replace
   semantics.
3. **`*From` value sources referenced by the tree.** The tree stays
   authoritative over *which* field is set where; a rule may declare
   that a field's value comes from an annotation.
4. **Derive values from labels via an ops-owned mapping table.** A flat
   `orgs:` table keyed on `app.example.com/part-of` supplying slack
   channel and tag prefix, plus templated composition for
   project-identity tags.

## Decision

Chosen: **Option 3**, with two annotation scopes.

Option 2 is rejected for the reasons ADR-0002 gave, which still hold: a
second mental model with different merge semantics, no type safety, no
review surface, and a "tree resolves, then mystery layer applies"
debugging story.

Option 4 was prototyped and measured against the live cluster: deriving
tags from `part-of` + `name` + `component` reproduced current tags
exactly for only **10 of 43** labeled ingress-hosts. Of the 33
differences, roughly 14 were improvements and 19 were vocabulary
mismatches, two of which break status pages (`acme/service-a-web` →
`acme/service-a` + `web-app`; `betaco/alias-x` → `betaco/project-b`).
It also needed label aliasing, slugification and template composition.
Rejected as disproportionate machinery for the benefit.

### The seam

A rule's `config:` block may set a field either literally or from an
annotation:

```yaml
<field>From:
  annotation: app.example.com/<key>          # ingress-scoped
  namespaceAnnotation: app.example.com/<key> # namespace-scoped
  default: <value>                           # optional
```

Exactly one of `annotation:` / `namespaceAnnotation:` per block.

Six keys, no boolean flags:

| field | scalar form | list forms |
| --- | --- | --- |
| `path` | `pathFrom` | — |
| `slack` | `slackFrom` | — |
| `notify` | — | `notifyFrom`, `notifyOverrideFrom` |
| `tags` | — | `tagsFrom`, `tagsOverrideFrom` |

**Merge semantics are identical to the literal field.** `pathFrom` and
`slackFrom` are scalars (the deepest layer that set the field wins);
`notifyFrom` and `tagsFrom` union; `notifyOverrideFrom` and
`tagsOverrideFrom` replace the baseline **at that rule's position**,
exactly as the `!override` YAML tag does today, with later rules still
unioning on top. No new merge concept is introduced, and no
post-cascade special case exists.

Override is positional rather than final-value-replacing because the
tree's trailing host rules own environment tags:

```yaml
- when: { host: "*.dev.example.com" }   config: { tags: [public] }
- when: { host: "*.local.example.com" } config: { tags: [local] }
```

The `public` and `local` status pages bind on those tags, and a chart
cannot reliably declare them — they are DNS properties, not app
properties. Final-value replacement would strip them.

### Scope assignment

- **`path` is ingress-scoped.** It genuinely varies within a namespace:
  `acme-api-11` has three ingresses declaring `/health-check/`,
  `/livez` and `/minio/health/live` under one owner.
- **`notify` and `slack` are namespace-scoped.** They never vary within
  a namespace in the current config, and the namespace is the only scope
  `alertmanager.match` can also read. One `Namespace` annotation serves
  both trees, so the ownership map is stated once:

```yaml
# Namespace object
metadata:
  name: acme-service-a-1
  annotations:
    app.example.com/notify: alice,bob,carol
    app.example.com/slack: ops-alerts
```

`tags` may use either scope; project identity is namespace-scoped in
practice, while `component`-derived tags are per-ingress.

### Validation and degradation

Annotation values are unreviewed runtime input, so they are validated
like config — but a bad value must never cost availability monitoring.
**The monitor always materializes as `added` and keeps probing.**

- `notify` entries must be `slack.userMapping` slugs. Raw `<…>` Slack
  markup is **rejected** from annotations, so the roster of who can be
  paged stays in reviewed config and an annotation can only select from
  it.
- `slack` must name a configured `slack.channels[].slug`.
- `path` must begin with `/`.
- **Partial validity is preserved:** one valid and one invalid `notify`
  entry keeps the valid one and warns.
- **An empty or whitespace-only annotation is treated as absent.**
  These values are Helm-templated (`{{ .Values.monitoring.notify | join
  "," }}`), and an empty list renders `""`; reading that as "notify
  nobody" would silently orphan a monitor on a values typo.
- **An override annotation that yields no valid entries is ignored
  entirely**, falling back to the cascade value rather than replacing
  real recipients with nothing.
- Every rejected value produces a discovery-row warning naming both the
  annotation key and the value, plus a `WARN` log. Not Sentry — it is
  app-team input error, not a toggle-monitor fault.

`pathFrom` carrying a `default` satisfies ADR-0002's root-required
`path` constraint; the validator is extended to accept it.

## Consequences

- **Good: the config stops growing with N.** A new `acme-service-a-12`
  needs no config change. This is the primary win — not a smaller file
  today, but one that no longer grows.
- **Good: one ownership map, not two.** Namespace-scoped `notify`/`slack`
  collapses 24 kube `notify` lines and 18 `alertmanager.match` ones to
  two declarations each, and makes drift between them impossible.
- **Good: bugs fixed on adoption.** The `mailpit` mis-probe, the 7
  mis-tagged minio, the 8 untagged frontends and `acme-cms-1`'s
  empty tag set all resolve from data the cluster already holds.
- **Bad: app teams gain reach over status pages.** Tags drive
  `statusPages[].sections[].match`, and `SectionMatch` binds only on
  `tags` and `hostRegex` — there is no label dimension — so status-page
  membership flows through what a chart declares. Acceptable while app
  and monitoring teams are the same humans, which is the premise this
  record is otherwise correcting; it is the sharpest edge here.
- **Bad: a Namespace informer and `get/list/watch namespaces` RBAC.**
  New cluster-scoped read permission beyond today's Ingress watch.
- **Bad: ADR-0002's single-source-of-truth property weakens.** "Why
  does this monitor have these settings?" now has two inputs. Mitigated
  by requiring provenance in the rule chain and `explain` output
  (`path=/health-check/ ← annotation app.example.com/health-check`),
  without which ADR-0002's debugging complaint returns intact.
- **Bad: feedback latency up to `resyncInterval` (30m).** There are no
  informer event handlers in `internal/kube`; reconcile runs on initial
  start and the ticker only. Labels have the same latency today, but
  annotation self-service raises the expectation of a fast loop. Wiring
  informer updates to a debounced reconcile is a separate change.
- **Neutral: migration is chart-gated, and ordered.** 71 of 114
  ingress-hosts carry neither labels nor annotations and remain
  config-driven. Broad parent rules must be narrowed as their children
  migrate: a `notifyFrom` at the root plus a surviving
  `notify: [dave, erin]` on `acme-*` resolves an annotated
  project to 5 recipients instead of today's 3.
- **Revisit if** annotation-sourced fields expand past this set —
  particularly if `interval`, `dependsOn`, `critical` or
  `tlsInsecureSkipVerify` are proposed. Those were deliberately excluded:
  scheduling is cluster capacity rather than app knowledge, `dependsOn`
  references slugs the app cannot see and feeds the ADR-0004 dispatch
  graph where a bad edge suppresses real alerts, `critical` opts out of
  coalescing entirely, and `tlsInsecureSkipVerify` also bypasses SSL
  expiry tracking.

## References

- [ADR-0002](0002-kube-match-tree-cascade.md) — the cascading
  `kube.match[]` tree; this record amends its `### Annotations: removed`
  sub-section only.
- [ADR-0004](0004-burst-dispatch-supersedes-always-coalesce.md) —
  `notify` and `dependsOn` feed the burst dispatcher; the reason
  `dependsOn` and `critical` stay config-only.
- [ADR-0008](0008-self-health-degraded-mode.md) — the discovery-row
  warning surface follows its operator-notice pattern.
- `internal/config/config.go` — `KubeConfig`, `KubeMatchWhen`,
  `isValidNotifyEntry`, `KubeRequiredAtRoot`.
- `internal/merger/merger.go` — `resolveStack` (merge rules),
  `whenMatches`, `checkResolved`.
- `internal/kube/kube.go` — `Watcher`, `IngressLister`; the Namespace
  informer lands here.
- `internal/slack/mentions.go` — `ResolveMentions` silently drops
  unknown slugs on the render path, which is why validation must happen
  at materialize time.
