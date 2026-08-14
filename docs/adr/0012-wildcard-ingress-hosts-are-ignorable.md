---
status: accepted
date: 2026-08-14
deciders: [monitoring-team]
---

# ADR 0012 — Wildcard ingress hosts are ignorable, and classified in the resolve kernel

**Status:** Accepted
**Date:** 2026-08-14

## Context

Kubernetes permits `*` as the leftmost label of an Ingress rule host
(`*.static.example.test`). No prober can resolve such a name — probing
`https://*.static.example.test/` is a perpetual "no such host" — so it
must never materialize into a monitor. Commit `184a722` implemented
that as a guard in `Materializer.Materialize` placed deliberately
*before* the `kube.match` cascade walk, on the reasoning that
structural invalidity should outrank any rule the operator wrote.

Two consequences followed, neither of them recorded in an ADR (the
behaviour appeared only in `CHANGELOG.md` and a code comment).

**A wildcard Ingress was permanently un-actionable.** Because the guard
short-circuited before `Resolve`, a matching `ignore: true` rule could
never reach it. There is no other opt-out: the informer is cluster-wide
and unfiltered, `kube:` has no namespace or ingressClass exclusion,
there is no host override, and the operator UI has no acknowledge
state (`/issues` is read-only and `discovery_snapshot` is overwritten
on every reconcile). So a correctly-deployed wildcard router — a
static-hosting Ingress with no single hostname to probe — pinned a
`kube-invalid` row forever, kept `toggle_monitor_issues{source=
"kube-invalid"}` non-zero, and kept the shipped
`ToggleMonitorKubeInvalidIngresses` rule firing. A panel that is
permanently non-zero stops meaning "something needs attention": the
third, genuinely-broken Ingress would not stand out from the two known
ones.

**The three consumers of the cascade disagreed.** `merger.Resolve`
documents itself as the shared kernel — "keep them in lockstep by
routing every consumer through this helper". The guard sat outside it,
so `toggle-monitor explain` and the `/discovery` detail page both
reported a wildcard host as `materialized` with a slug, while the
daemon recorded `kube-invalid` for the same input. An operator using
`explain` to reason about a wildcard got a confidently wrong answer.

`ignore: true` already means "this Ingress is not to be monitored"
(ADR-0002). A wildcard host is precisely a case where that is the
correct operator judgment.

## Considered Options

- **A. Keep the pre-walk guard.** Suppress downstream instead:
  Alertmanager silences, or `prometheusRule.issues.kubeInvalid: false`
  in the chart.
- **B. Global `kube.wildcardHosts: invalid | ignore` knob.** Keep the
  guard pre-walk; let it choose which status it emits.
- **C. Let `ignore:` reach wildcards.** Classify the wildcard inside
  the shared `Resolve` kernel, after the walk, as a `Resolution.Err`.
  An explicit `ignore: true` outranks it.
- **D. Make wildcards probeable.** Add a concrete-host override so
  `*.static.example.test` is probed as e.g.
  `canary.static.example.test`.

## Decision

Chosen: **Option C.**

`Resolve` and `ResolveWithTrace` gain a shared `classify(host)` tail
that replaces the duplicated `checkResolved` call in each. It sets
`Resolution.WildcardHost` unconditionally, and, for a non-ignored row,
sets `Resolution.Err = ErrWildcardHost` in preference to running
`checkResolved`. `Materialize` loses its pre-walk guard entirely.

The resulting precedence is: **`ignore:` > wildcard > resolved-value
validation.** An `ignore: true` that the operator wrote is a decision
already made; a wildcard host is more actionable than "timeout >=
interval" on a monitor that could never have run.

All three consumers already switch on `!Matched → Ignored → Err != nil
→ default`, so routing the classification through `Err` fixes
`explain` and the discovery detail page with no changes in either.

An acknowledged wildcard records `kube-ignored` and still names the
wildcard in its reason — the row is the only place an operator learns
that this Ingress could never have been probed, ignore rule or not.

Operators acknowledge with a rule such as:

```yaml
- when: { hostRegex: '\*\..*' }   # hostRegex is auto-anchored ^…$
  ignore: true
```

Option A was rejected because it moves a toggle-monitor concept into
the operator's alerting stack and leaves `/issues` permanently dirty —
the panel's value is that it is normally empty. Option B adds a config
concept for something the existing `ignore:` mechanism already
expresses, and is all-or-nothing cluster-wide: it cannot acknowledge
one team's wildcards while still flagging a new one elsewhere. Option
D is the only option that yields actual coverage and remains open
(`TODO.md`, "Add support for wildcard probe testing"); it is a larger
change — new config field, `*From` source, slug and identity
implications — and is orthogonal to whether an unprobed wildcard
should be an issue.

This does not supersede ADR-0002. It restores that ADR's `ignore:`
semantics over a case a later commit had carved out, and records the
precedence explicitly so it is not re-litigated from a code comment.

## Consequences

- **Good: a wildcard Ingress is actionable.** The operator has a
  documented, in-config way to say "known, not monitorable", and
  `/issues` returns to meaning "something needs attention".
- **Good: the resolve kernel is genuinely shared.** `explain`, the
  discovery detail page, and the daemon now agree on every wildcard
  input. The invariant `Resolve` claims for itself is true again.
- **Good: no new config surface.** The escape hatch is the `ignore:`
  directive operators already know.
- **Bad: a wildcard can be silenced by an over-broad `ignore:` rule.**
  A rule written as `when: { namespace: "static-*" }, ignore: true`
  now also quiets any wildcard in that namespace, where previously it
  would still have surfaced. This is the accepted cost of treating
  `ignore:` as authoritative. Two mitigations: `docs/config-schema.md`
  advises scoping the selector as tightly as the situation allows, and
  `/issues` gained a "Skipped ingresses" section listing `kube-ignored`
  rows — below the fold and outside the issue count, but visible, so
  what a rule swallowed is reviewable rather than invisible. That list
  is capped (`templates.IgnoredPreviewMax`) and defers to
  `/discovery?status=kube-ignored`, since a broad rule can match
  hundreds of Ingresses and the page must not grow with the cluster.
- **Bad: a wildcard with no matching rule at all reports "no matching
  kube.match rule"** rather than the wildcard reason, since `!Matched`
  is checked first. Production configs always carry a matching root
  (validator-enforced), so this is reachable only from hand-built
  trees in tests.
- **Bad: wildcards are still not monitored.** Acknowledging one hides
  a real coverage gap rather than closing it.
- **Revisit if:** wildcard probing lands (Option D). A concrete-host
  override would make `ignore:` the wrong default answer for these
  Ingresses, and this ADR should then be superseded rather than
  amended.

## References

- [ADR 0002](0002-kube-match-tree-cascade.md) — establishes the
  `kube.match` cascade and the `ignore:` directive whose semantics
  this ADR restores.
- [ADR 0009](0009-from-value-sources-for-kube-discovery.md) — the
  `*From` value sources that share the same resolve kernel.
- [ADR 0010](0010-self-alerting-on-issues-via-prometheusrule.md) —
  defines `toggle_monitor_issues{source="kube-invalid"}` and the
  shipped `ToggleMonitorKubeInvalidIngresses` rule this ADR keeps
  meaningful.
- [`docs/config-schema.md`](../config-schema.md) — §4 "Wildcard hosts"
  documents the operator-facing behaviour.
- Commit `184a722` — introduced the pre-walk wildcard guard this ADR
  relocates.
