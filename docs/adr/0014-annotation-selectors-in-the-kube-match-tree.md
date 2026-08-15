---
status: accepted
date: 2026-08-15
deciders: [monitoring-team]
---

# ADR 0014 — Annotation selectors in the kube.match tree

**Status:** Accepted
**Date:** 2026-08-15

## Context

An app team needs a way to say "do not monitor this Ingress" from the
object they own. Today they cannot.

`kube.match` rules select on `namespace`, `namespaceRegex`, `host`,
`hostRegex`, and `labels` — where `labels` matches the **Ingress**
labels only. `ignore: true` (ADR-0002) is the opt-out, and it is
rule-level metadata, not a `config:` field. So the only ways to skip a
host today are for an operator to name it in `config.yaml` — by
namespace glob, host glob, or Ingress label — or for the chart to emit
the label the config already looks for:

```yaml
- when: { labels: { monitor.togglecorp.com/disabled: "true" } }
  ignore: true
  final: true
```

That label channel works, but it is not the channel this cluster's
charts actually use for monitoring metadata. Every other opt-in an app
team has is an **annotation**: `app.togglecorp.com/health-check`
(43 Ingresses), `app.togglecorp.com/accepted-status-codes` (3), and the
Namespace-scoped `app.togglecorp.com/{slack,notify,tags}` (72
Namespaces each). Some charts hardcode their Ingress annotations with
no values hook for labels at all. Asking for a label here means asking
for a chart change in exactly the cases where the team wants the
quickest possible "stop paging me".

ADR-0009 established the two annotation scopes (`annotation:` for the
Ingress, `namespaceAnnotation:` for the Namespace) and one rule that
governs them: **an annotation selects from reviewed config; it never
introduces new authority.** Slack markup is rejected from a `notify`
annotation, an unknown channel slug is rejected from a `slack`
annotation, and every accepted value must already exist in
`config.yaml`. Whatever shape a skip annotation takes has to hold that
line, because "stop monitoring this" is the single most consequential
thing an annotation could say: it removes coverage, and unlike a bad
`path` it produces no failing probe to notice.

## Considered Options

- **A. `annotations:` / `namespaceAnnotations:` selector keys on
  `when:`.** Symmetric with the existing `labels:` selector. The
  annotation is matched; the operator's rule decides what matching
  means.
- **B. `ignoreFrom:` value source.** An annotation drives the `ignore`
  boolean directly, in the `*From` idiom of ADR-0009 / ADR-0013.
- **C. Nothing — use the existing label kill switch.** Charts that want
  to opt out add `monitor.togglecorp.com/disabled: "true"` as an
  Ingress label.
- **D. A fixed, hardcoded skip annotation.** The binary knows one key
  (say `monitor.togglecorp.com/skip`) and honours it with no config
  rule at all.

## Decision

Chosen: **Option A**, both scopes.

`KubeMatchWhen` gains two map fields:

```yaml
- when:
    annotations:                                   # Ingress-scoped
      monitor.togglecorp.com/skip: "true"
  ignore: true
  final: true

- when:
    namespaceAnnotations:                          # Namespace-scoped
      monitor.togglecorp.com/skip: "true"
  ignore: true
  final: true
```

Semantics match `labels:` exactly: every key/value pair in the map must
be present and equal on the object, all pairs AND together, and the map
ANDs with the other selector dimensions. Absent annotation ≠ empty
string, as for labels.

Six decisions inside that:

1. **The selector matches; the config decides.** Nothing in the binary
   knows what `monitor.togglecorp.com/skip` means. An operator writes a
   rule pairing that key with `ignore: true`, and that rule is the
   authority — exactly the ADR-0009 line. Option D fails it outright: a
   hardcoded key lets any app team delete a monitor with no config
   change and nothing in `config.yaml` naming the key, so a reviewer of
   the config could not tell which hosts are skippable.

2. **Not `ignoreFrom:` (Option B).** `ignore` is rule metadata, not a
   `config:` field, so it sits outside the lowering pass both `*From`
   ADRs are built on — `ignoreFrom` would need new machinery rather
   than reusing any. It is also weaker: it can only express skipping,
   where a selector serves every rule in the tree. And it repeats
   Option D's problem in softer form, since `ignoreFrom` with no
   `default:` hands the decision to the annotation.

3. **Both scopes.** The Namespace scope is not redundant with a
   `namespace:` glob: a glob is a config-side statement about a name,
   while a Namespace annotation is the namespace owner's own
   declaration, and it survives the namespace being renamed or a new
   one appearing. `merger.Env` already carries
   `NamespaceAnnotations`, so the plumbing is a parameter, not a new
   source.

4. **A selector-only rule is not the root rule.** `kubeWhenIsEmpty`
   decides which rule carries the required-at-root fields and gates
   `final:`. Both new maps count as selectors there; otherwise a rule
   selecting only on annotations would be mistaken for the root
   baseline and `final: true` on it would be rejected.

5. **Keys are validated as k8s qualified names, values are not.**
   Same as `labels:`. An annotation *value* may be any string
   (annotations have no value grammar, unlike labels), so validating it
   would reject legitimate config.

6. **No Alertmanager twin.** `alertmanager.match` selects on alert
   labels. Its annotation equivalent would be the alert's own
   annotations, which ADR-0013 already refuses as a value source —
   "written by the rule that emitted it, not by the workload's owner".
   Nothing changes there.

## Consequences

**An app team can now remove its own monitoring.** That is the point,
and it is also the risk. The mitigations are that the operator's config
names the key (so the capability is visible in review), and that
skipped hosts are not silent: ADR-0012 added the "Skipped ingresses"
section to `/issues`, which lists every `kube-ignored` row with the
rule chain that produced it, precisely so a too-broad ignore is
noticed. A host skipped by annotation appears there like any other.

**A monitor can disappear without a config change.** Removal is already
handled — ADR-0011's watch-driven reconcile soft-deletes a monitor that
stops materializing, and the annotation is a watch trigger like any
other edit. The `kube-ignored` reason string will name the rule that
matched, so `/discovery` explains why.

**The rule chain gets longer.** `selectorSummary` renders each matched
pair as `annotations.k=v` / `namespaceAnnotations.k=v`, sorted for a
reproducible chain across reconciles. Rules that match on several
annotations produce correspondingly wide chain entries in
`discovery_snapshot.reason`.

**Annotation churn now costs a reconcile.** `whenMatches` reads
annotations, so an unrelated annotation edit on a watched Ingress can
change a resolution. The debounce from `kube.watchDebounce` already
bounds how often that turns into work.

**`explain` hypothetical mode needs the new input.** `--labels` has an
`--annotations` counterpart, or the tool cannot answer "would this
Ingress be skipped?" for an object that does not exist yet. Live mode
already reads both from the cluster.

## References

- ADR-0002 — `kube.match` cascade; `ignore:` semantics
- ADR-0009 — `*From` value sources; the two annotation scopes and the
  "annotations select, config decides" rule
- ADR-0011 — watch-driven removal detection
- ADR-0012 — ignorable hosts and the `/issues` skipped-ingress listing
- ADR-0013 — why alert annotations are not a routing input
