---
status: accepted
date: 2026-08-14
deciders: [monitoring-team]
amends:
  - "0005-alertmanager-webhook-receiver.md (partial — the `AlertmanagerMatchConfig` field set only)"
extends:
  - "0009-from-value-sources-for-kube-discovery.md"
---

# ADR 0013 — `*From` value sources for alertmanager routing

**Status:** Accepted
**Date:** 2026-08-14
**Amends:** ADR-0005 (partial). The webhook envelope, endpoint auth,
rate limiter, retention sweeper, renderer and the `alertmanager.match`
tree grammar all stand unchanged. This record widens exactly one thing:
the field set a rule's `config:` block may carry.
**Extends:** ADR-0009, which introduced `*From` value sources and
scoped them to `kube.match` only.

## Context

ADR-0009 let a `kube.match[]` rule source a field from a Namespace
annotation, and a survey of the live cluster on 2026-08-14 showed the
mechanism working: 114 of 124 ingress-hosts now resolve `slack`,
`notify` and `tags` from `app.example.test/{slack,notify,tags}` on the
namespace, and stripping the literal rules that duplicated those
annotations removed 31 of 73 `kube.match` rules with **byte-identical
resolution across all 124 hosts**.

`alertmanager.match` was not part of that change, and it routes on the
same ownership facts. The two trees now disagree, because they are
maintained by hand in two places:

| namespace family | `kube.match` (sourced) | `alertmanager.match` (literal) |
| --- | --- | --- |
| project A | 3 handles, `!override` | 1 handle |
| project B | 4 handles | 3 handles |
| 5 more families | covered | no rule — falls to the root |
| every project | per-project channel | root channel only |

The last row may be a deliberate choice (Prometheus alerts and probe
alerts need not share a channel). The rest is drift: a namespace whose
on-call list changed got updated in one tree and not the other, and
nothing in the config or the validator can detect that, because both
trees are internally valid.

The asymmetry is also a trap for the next operator. Having just learned
that ownership lives on the namespace, they will reasonably assume
`slackFrom:` works in `alertmanager.match` too. It does not: the
unknown-key walker rejects it at load time, which is at least a loud
failure — but the fix is then to hand-copy the ownership a second time,
which is how the drift above happened.

Three facts constrain any design here.

**An AM alert has no Ingress.** ADR-0009's two scopes are
`annotation:` (the Ingress) and `namespaceAnnotation:` (the Namespace).
An alert carries its own `annotations` map, but those are authored by
the PrometheusRule that emitted the alert — `summary`, `description`,
`runbook_url` — not by whoever owns the workload. They are not an
ownership source, and treating them as one would let any rule author
redirect another team's alerts.

**The namespace is a label, and its key is not fixed.** Prometheus
convention is `namespace`, but exporters relabel: `exported_namespace`
when a scrape job collides with a target label, `kubernetes_namespace`
in older setups. A design that hardcodes `namespace` works for the
common case and is unfixable for the rest.

**The Namespace informer belongs to the kube watcher.** It is created
in `RunServe` only when `opts.Config.Kube != nil`, and it is created
*after* `buildAMHandler`. An AM value source therefore depends on a
config block that has nothing else to do with it, and cannot be wired
at handler-construction time.

## Considered Options

- **A. Do nothing.** Document that ownership must be maintained in both
  trees, and rely on review to keep them in step.
- **B. Share one tree.** Route AM alerts through `kube.match` by
  synthesizing a pseudo-Ingress from the alert's labels.
- **C. Add `*From` to `AlertmanagerMatchConfig`,
  `namespaceAnnotation:`-only,** with the namespace-bearing label
  named per source.
- **D. As C, plus an `annotation:` scope** reading the alert's own
  annotations.
- **E. Generate `alertmanager.match` from `kube.match`** at config-load
  time, so the operator writes ownership once.

## Decision

Chosen: **Option C.**

`AlertmanagerMatchConfig` gains three keys, mirroring the two fields it
already carries:

| field | scalar form | list forms |
| --- | --- | --- |
| `slack` | `slackFrom` | — |
| `notify` | — | `notifyFrom`, `notifyOverrideFrom` |

Each takes a `namespaceAnnotation:` key, an optional `default:`, and an
optional `namespaceLabel:` naming the alert label that carries the
namespace name — defaulting to `namespace`. `annotation:` is rejected
at load time with a message that names the reason, rather than being
silently accepted and never matching.

```yaml
alertmanager:
  match:
    - when: {}
      config:
        slackFrom:
          namespaceAnnotation: app.example.test/slack
          default: tc-k8s-alerts
        notifyFrom:
          namespaceAnnotation: app.example.test/notify
          default: [oncall]
```

Six decisions follow from ADR-0009's precedent, and are restated here
because this cascade is a different one:

**Lowering, not a new merge concept.** As in ADR-0009, a `*From` block
is converted to its literal equivalent *per rule, before the merge
stack sees it*. `resolveStack` and `mergeStrings` are untouched, so the
deepest-wins / union / `!override` semantics ADR-0005 documents remain
the only merge rules in the package.

**`slackFrom` with a `default:` satisfies the root requirement.**
ADR-0005 requires `config.slack` on the root rule. A root
`slackFrom` without `default:` does not satisfy it — a namespace that
forgets the annotation would resolve to no channel at all, and the
handler's only recourse would be the 5xx it raises today for an unknown
slug. With `default:`, the requirement is met, matching how ADR-0009
treats `kube.match`'s root-required fields.

**Degrade, never drop.** Annotation values are unreviewed runtime
input. An unknown channel slug, an unknown notify handle, raw `<…>`
mention markup, an absent annotation, an unreadable Namespace, or a
namespace label that is missing from the alert all fall back to
`default:` if present, otherwise to the value the cascade already
resolved. An alert is never dropped and never misrouted to a channel
that does not exist: the resolved channel is always one the validator
has already checked, because the fallback chain terminates at the
root's literal.

**Only actionable degradations are reported.** Two of those cases are
ordinary traffic rather than misconfiguration, and are silent: an alert
with no namespace label (every cluster-scoped rule — `Watchdog`, node
pressure — has none) and a namespace with no annotations (the informer
lister returns nil both for a namespace it has never seen and for one
that carries none, and 10 of this cluster's 78 ingress-bearing
namespaces are unannotated). Reporting either would keep the counter
permanently non-zero, which is the failure mode ADR-0012 argued
against for `/issues`. The cost is that a misspelled `namespaceLabel:`
is not flagged; it is diagnosable from the rule chain, which shows no
provenance for the field.

**The `kube:` dependency is enforced, not discovered.** Using
`namespaceAnnotation:` anywhere under `alertmanager.match` while
`kube:` is absent is a load-time error. When `kube:` is present, the
annotation source is wired onto the handler after the watcher is built,
by the same late-setter route the materializer already uses
(`SetNamespaceAnnotationSource`). The endpoint serves before that
wiring happens, and a source that is still nil reads as "annotation
unreadable" — warn and fall back — so a webhook arriving in that window
routes to the root channel instead of erroring. The informer's cache is
synced before the source is published, so there is no separate
wired-but-unsynced window.

**Misconfigurations surface as a counter and in the rule chain, not on
`/issues`.** `/issues` lists a *current set*: the kube materializer
recomputes every monitor on each reconcile, so its rejected-annotation
list is complete and self-evicting (ADR-0009). AM routing has no
reconcile loop — a rule is evaluated once per inbound alert, and there
is no defensible answer to "which alerts are current" for a page that
must not grow with cluster traffic. So instead:

- a `warn`-level log line per rejected value, with the alert
  fingerprint, and
- `toggle_monitor_am_value_source_rejections_total{field,reason}`, a
  counter, alerted on by a new rule in the shipped `PrometheusRule`
  alongside ADR-0010's gauge rules, and
- the provenance appended to the `am_alerts.rule_chain` column, which
  is the AM tree's only debugging surface — there is no `explain`
  subcommand for it.

Option A was rejected because the drift in the table above already
happened, silently, and review demonstrably did not catch it. Option B
is the most tempting and the worst: the two `when:` vocabularies are
disjoint (`alertname`/`receiver`/`externalURL` versus
`namespace`/`host`/`labels`/`ingressClass`), a synthesized Ingress
would have to be excluded from discovery, slug generation and the
`/discovery` page, and every rule would have to be written defensively
against both callers. Option D was rejected on the authorship argument
above — alert annotations are written by whoever wrote the alerting
rule, and making them a routing source lets a rule author page another
team. Option E was rejected because it presumes the two trees *should*
be identical; they legitimately differ (the channel row in the table
above, `Watchdog` being ignored, per-alertname routing), and a
generator that permits exceptions is just this ADR's mechanism with a
worse error surface.

This does not supersede ADR-0005 or ADR-0009. It widens one field set
and reuses the other's semantics verbatim.

## Consequences

- **Good: ownership is declared once.** A namespace's channel and
  on-call list live on the Namespace object, and both cascades read
  them. The drift class in the Context table stops being possible for
  annotated namespaces.
- **Good: no new merge semantics.** Lowering keeps ADR-0005's merge
  rules the only ones in `internal/alertmanager`, so the AM cascade and
  the kube cascade stay readable side by side.
- **Good: `namespaceLabel:` covers relabelled exporters** without a
  cluster-wide knob, and different rules may read different label keys
  for different alert sources.
- **Bad: `alertmanager` now depends on `kube:`.** A config that wants
  annotation-sourced AM routing but no ingress discovery cannot have
  it. The validator says so explicitly rather than failing at runtime,
  but it is a real coupling: the Namespace informer is the kube
  watcher's, and giving `alertmanager` its own would mean a second
  informer factory and a second RBAC grant for the same reads.
- **Bad: AM misconfigurations are less visible than kube ones.** A
  counter and a log line are weaker than a list on `/issues`. An
  operator who is not watching the new alert learns about a bad
  annotation only from the rule chain of an alert that already routed
  to the wrong place — though it routed *somewhere*, by the degrade
  rule.
- **Bad: a namespace annotation now has two blast radii.** Editing
  `app.example.test/slack` moves both the probe alerts and the
  Prometheus alerts for that namespace. That is the intent, but it
  makes the annotation a higher-stakes edit than when only `kube.match`
  read it. It is also no longer only that namespace's own alerts at
  stake: the value is checked for channel *existence*, not for whether
  this namespace may use that channel, and the rate limiter's window is
  per channel. A namespace that annotates itself into another team's
  channel and then storms can exhaust that channel's budget and get the
  other team's alerts dropped. Anyone who can `kubectl annotate ns` can
  do this; the mitigation available today is that the annotation is as
  reviewable as any other cluster resource.
- **Bad: routing is no longer stable across an incident.** The channel
  is resolved per delivery, so a namespace re-annotated between an
  alert's firing and its resolve would edit the parent message as the
  wrong channel's bot. The resolve path pins to the channel recorded on
  the incident row to avoid it, which means a handover mid-incident
  finishes that incident in the old channel and starts the next one in
  the new.
- **Bad: the rejection counter measures deliveries, not
  misconfigurations.** A rule is evaluated per inbound alert, so one bad
  annotation on a busy namespace increments far more than the same
  mistake on a quiet one. The shipped alert only tests `> 0`, so the
  magnitude is not load-bearing, but it must not be read as a count of
  broken namespaces.
- **Revisit if:** the AM tree gains an `explain`-style surface, at
  which point the rule-chain provenance should move behind it; or if
  a non-kube deployment needs annotation-sourced AM routing, at which
  point the Namespace informer must be lifted out of the kube watcher.

## References

- [ADR 0005](0005-alertmanager-webhook-receiver.md) — establishes the
  `alertmanager.match` tree and the `AlertmanagerMatchConfig` field set
  this ADR widens.
- [ADR 0009](0009-from-value-sources-for-kube-discovery.md) — introduces
  `*From` value sources, the lowering pass, and the degrade-don't-fail
  rule this ADR reuses.
- [ADR 0002](0002-kube-match-tree-cascade.md) — the cascade semantics
  both trees mirror.
- [ADR 0010](0010-self-alerting-on-issues-via-prometheusrule.md) —
  defines the shipped `PrometheusRule` this ADR adds a rejection-counter
  rule to.
- [`docs/alertmanager.md`](../alertmanager.md) — operator-facing
  documentation for the AM receiver.
- [`docs/config-schema.md`](../config-schema.md) — the `*From` value
  source reference, extended here to the `alertmanager.match` scope.
