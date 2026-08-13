# ADR 0002 — kube.match as a cascading rule tree

**Status:** Accepted
**Date:** 2026-05-24
**Amended by:** [ADR-0009](0009-from-value-sources-for-kube-discovery.md) —
the `### Annotations: removed` sub-section only.
**Supersedes:** the `kube.presets:` + flat `kube.match[]` design captured in
[`config-schema.md`](../config-schema.md) and the per-ingress
`/kube.*` / `/config.*` annotation layer described in
[`design-decisions.md`](../internal/design-decisions.md).

## Context

The original `kube.*` config layered three mechanisms to materialize a
monitor from a discovered Ingress:

1. A `kube.presets:` registry of named, fully-specified monitor templates.
2. A flat `kube.match[]` list whose rules selected a preset by
   `{namespace, host}` glob — first match wins, with a trailing
   wildcard fallback rule.
3. Per-ingress annotation overrides (`/kube.preset`, `/kube.path`,
   `/config.enabled`, `/config.group`, `/config.tags`,
   `/config.dependsOn`, `/config.notify`) for app-team customization
   on top of the preset.

This works for shallow taxonomies (a handful of presets, a handful of
namespace patterns) but breaks down for the realistic shape of a
multi-tenant cluster:

- **Preset explosion.** Real deployments need a per-project notify
  list, per-subproject path overrides, per-app health-check endpoints
  (`minio` wants `/minio/health/live`; `stac-auth-proxy` wants
  `stac/_mgmt/ping`). Expressing each combination as a flat preset
  produces N×M presets where most fields just repeat the parent's.
- **No inheritance.** Two presets for the same project differ only in
  `path` and `tags`; the rest is copy-paste. Editing the shared parts
  means editing every preset.
- **Annotations as the only refinement layer.** App teams refine
  per-ingress through annotations, which is a second mental model
  with different merge semantics (some CSVs union, some replace), no
  type-safety, no review surface (annotation edits don't go through
  config review), and no way to express selectors over groups of
  ingresses.
- **Discovery answer is lossy.** "Why does this monitor have these
  settings?" resolves to one preset slug + a mystery annotation
  layer; debugging requires inspecting both.

Toggle-monitor is greenfield (no production users yet), so the cost
of a clean break is at its minimum. This ADR locks the redesign.

## Decision

`kube.match[]` becomes a tree of rules with config inheritance. The
`kube.presets:` block is deleted; the `kube.pause:` block is deleted;
all ingress annotations are deleted; `kube.annotationDomain` is
deleted. Every monitor field is set somewhere in the tree.

### Rule shape

Each rule is:

```yaml
- when:   {...}    # selector
  ignore: true     # rule-level directive (optional)
  final:  true     # rule-level directive (optional)
  config: {...}    # monitor fields
  nested: [...]    # child rules (optional)
```

`when:`, `config:`, and `nested:` may all be absent. An absent `when:`
or `when: {}` means "match anything reaching this point in the
traversal."

### Selector vocabulary (`when:`)

```yaml
when:
  namespace:      "acme-*"            # glob, path.Match semantics
  namespaceRegex: "acme-\\d+"         # Go regexp, auto-anchored as ^...$
  host:           "*.example.com"       # glob
  hostRegex:      "..."                 # Go regexp, auto-anchored
  labels:                               # exact key=value; all keys AND
    app.kubernetes.io/name: minio
```

- Within a `when:`, all set fields AND together.
- Setting both glob and regex for the same dimension
  (`namespace` + `namespaceRegex`, or `host` + `hostRegex`) is a
  validation error — operator picks one.
- Regex selectors are auto-anchored. `"acme-\\d+"` matches
  `acme-1` but not `acme-1-foo`. Operators wanting substring use
  `.*` explicitly.
- `labels` matches the Ingress's own `.metadata.labels` only — not
  labels on the backing Service, Deployment, or Pod. Operators who
  need to discriminate by app must label the Ingress.
- No `name:` (ingress-name) selector in v1. Namespace + labels cover
  realistic cases; add if a real need surfaces.

### Evaluation: multi-match accumulate

For each (Ingress, host) pair, traverse the entire `kube.match[]` tree
depth-first in document order. **Every** rule whose `when:` matches
contributes its `config:` to a merge stack. The final config for that
materialized monitor is the merge of the stack in stack order
(root-first → deeper later).

The root-level rules are evaluated in declaration order; the first
rule with `when: {}` (or omitted) functions as the always-matching
baseline that supplies defaults.

Rationale for accumulate-over-first-match-wins: the realistic config
trees factor into "default for everything → narrow refinement for a
namespace family → further refinement for an app within that family
→ per-app-component override." A first-match model collapses all
those layers into a single chosen rule; accumulate composes them.

### `final: true` — halt the cascade

A matching rule with `final: true` is the last word. The traversal
descends into the rule's own `nested:` subtree (so further refinement
within the subtree still applies), then halts the entire tree walk
for this (Ingress, host) pair. No later sibling, uncle, or top-level
rule contributes.

Use case from the prototype: when a `minio` label is matched inside a
specific project subtree (with a specific path), `final: true`
prevents a later top-level "global minio" rule from clobbering the
project-specific `path`.

`final: true` requires at least one selector field in `when:`. A
`final: true` with empty `when:` would halt the cascade for every
ingress on first occurrence — almost certainly a typo. Validation
error.

### `ignore: true` — suppress materialization

A rule-level directive. If the resolved value of `ignore` for an
(Ingress, host) pair is `true`, no monitor is created; a discovery
row with `status="kube-ignored"` is recorded so the operator can see
the rule fired (and filter on /discovery).

`ignore:` cascades like a scalar — deepest matching rule wins. A
child can flip `ignore: false` to un-ignore a subset:

```yaml
- when: {namespace: "test-*"}
  ignore: true
  nested:
    - when: {namespace: "test-critical-*"}
      ignore: false
```

`ignore:` lives at the rule level (sibling of `when:` / `config:` /
`nested:` / `final:`), not inside `config:` — it answers "does a
monitor materialize?" not "what config does the monitor have?"

### Merge rules

**Scalars** (string, int, bool, Duration): deeper / later overrides
shallower / earlier. Unset = inherit from the next-shallower rule
that set it.

**Arrays** (`notify`, `tags`, `dependsOn`, …): **union by default**,
deduplicated, shallow-first insertion order (shallow ancestors first,
then deeper additions appended).

```yaml
# root
config: { notify: [thenav56] }
# nested
config: { notify: [barsha] }
# resolved
notify: [thenav56, barsha]
```

**Replace semantics** via the `!override` YAML custom tag. The tag
swaps that single occurrence from union to replace; ancestors'
values for that field are discarded.

```yaml
config:
  notify: !override [uday, ankit]   # ignores anything ancestors set
```

`!override []` is the way to clear an inherited list.

**Exception:** `acceptedStatusCodes` is **replace-by-default**.
Unioning HTTP status codes across levels (parent `[200]` + child
`[301]` → `[200, 301]`) is almost always wrong — child usually means
*only* `[301]`. The field is special-cased; `!override` is not needed.

No `!reset` tag. `!override []` covers clear-to-empty for arrays;
scalars use natural empty values (`slack: ""`). Adding a second tag
doubles cognitive load with no real expressive gain.

No `extends:`. Inheritance is tree-only. Two unrelated subtrees that
need shared baseline lift it to a common ancestor or duplicate it.

### Annotations: removed

> **Amended by:** [ADR-0009](0009-from-value-sources-for-kube-discovery.md)
> (accepted 2026-08-13) reverses this sub-section, narrowly, via `*From`
> value sources. Its context records that this section's premise — app
> team and monitoring team being the same humans — does not match the
> live cluster, which already carries chart-emitted
> `app.example.com/health-check` annotations. A rule's `config:` block
> may now declare that `path`, `slack`, `notify` or `tags` takes its
> value from an ingress or namespace annotation; the tree still decides
> *which* field is set where. Everything else in this record stands.

All ingress annotations (`/kube.preset`, `/kube.path`,
`/config.enabled`, `/config.group`, `/config.tags`,
`/config.dependsOn`, `/config.notify`) are deleted. The
`kube.annotationDomain` config field is deleted. The discovery
snapshot's `Annotations` field is deleted.

Toggle-monitor's app team and monitoring team are the same humans;
the decentralized-self-service justification for per-ingress
annotations does not apply. Driving every monitor field through the
config tree gives:

- Single source of truth (git review covers every monitoring change).
- No "tree resolves, then mystery layer applies" debugging.
- Labels (which the tree's `when:` selects on) are k8s-native and
  pay rent across other tooling (ServiceMonitor, NetworkPolicy, …);
  annotations were toggle-monitor-specific.

Quick kill-switch use case is covered by a standardized label
convention plus one rule near the root:

```yaml
- when: {labels: {"monitor.togglecorp.com/disabled": "true"}}
  ignore: true
  final: true
```

Any ingress can then be disabled by a one-line YAML edit on the
ingress itself. Per-ingress path overrides become "add a tree rule
with a labels selector" — same review surface as any other change.

### Root rule required, with all required fields

A rule with `when: {}` at the top level is **mandatory** and must
carry all required monitor fields: `path`, `httpMethod`,
`acceptedStatusCodes`, `interval`, `timeout`, `retries`,
`retryBackoff`, `followRedirects`, `reminderInterval`,
`sslAlertThreshold`, `sslEscalationThreshold`, `sslReminderInterval`,
`slack`. (See [`config-schema.md`](../config-schema.md) §
"`kube.match[].config` fields" for the authoritative `Req at root`
column.) Children override selectively but don't have to set
anything. Missing root or missing required fields at the root is a
validation error at startup.

The "no rule matched → kube-invalid" safety net of the old design
disappears: under the new model the root always matches, so every
(Ingress, host) materializes with at least the root config. Operators
wanting a deliberate-decision signal can write the root as `ignore:
true` and explicitly un-ignore vetted namespaces — that stance is
available, not forced.

### Validation

**Structural (errors).** Selector glob/regex conflict, regex parse
failure, glob parse failure, invalid label key syntax, `final: true`
with empty `when:`, missing root rule, missing required fields at the
root.

**Structural (warnings).** Empty `when:` deeper than root (redundant
— equivalent to lifting `config:` into parent), `ignore: true` at a
leaf rule with a non-empty `config:` (dead config unless a child
un-ignores).

**Resolved-value (errors at materialization time).** `interval >=
timeout`, `SSL alert > escalation > 0`, a required-at-root field
that root did set was overridden to an empty / invalid value deeper
in the tree. These produce `status="kube-invalid"` discovery rows
pointing at the rule chain that produced the bad config — they don't
block startup, because they depend on which ingresses actually exist
at materialization time.

**Reachability validation is not performed.** Under multi-match
accumulate, every matching rule contributes; "unreachable rule" only
means "no ingress in the universe can match this." That's hard to
prove statically (the universe is dynamic). The trade-off is
documented; operators learn it.

### Identity: monitor slug derives from the ingress only

Monitor slug is `kube-<namespace>__<ingress-name>__<host>` (double
underscore separator avoids collision with single-dash content in
any of the three parts; the `kube-` prefix is the reserved namespace
that distinguishes auto-discovered monitors — the config validator
rejects any static `monitor.slug` that starts with `kube-`). No
rule-derived component.

Rationale: the slug's job is stable identity for the monitored thing.
The thing is an (Ingress, host) pair, which already has a stable
k8s identity. Pulling slug from config would mean a config edit can
rename a monitor (breaking history, bookmarks, dependsOn references).

Friendly display name remains controlled by `kube.friendlyName:`
(compact / dedupe / title styles — see existing code).

### Debug surface

**Discovery `reason` field.** Carries the compact rule chain:

```
match[1] (ns=acme-*) → [2] (ns=acme-service-a-*)
  → [0] (nsRegex=acme-service-a-eoapi-\d+)
  → [1] (labels.app.kubernetes.io/name=minio) [final]
```

Operators find the rules in the YAML and reason from there.

**/discovery detail view.** Shows the rule chain alongside the full
resolved config (the merged config that drives the monitor). Two
glances answer "wait, why is `path` `/minio/health/live`?"

**No per-field provenance v1.** Per-field "which rule contributed
`notify[2]`?" is a real-but-not-yet-acute debugging need. Defer
until a real session demands it; the rule chain gets 80% of the way
for free.

**CLI `toggle-monitor explain` subcommand.** Two modes:

```
toggle-monitor explain --ingress acme-service-a-eoapi-3/web
toggle-monitor explain --ingress acme-service-a-eoapi-3/web --host api.example.com

toggle-monitor explain \
  --namespace acme-service-a-eoapi-3 \
  --labels app.kubernetes.io/name=minio \
  --host api.example.com
```

The `--ingress` mode fetches the live Ingress from the cluster (in-
cluster config when run via `kubectl exec`, kubeconfig otherwise),
enumerates its (Ingress, host) pairs, and prints the resolved config
+ rule chain for each. The `--namespace`+`--labels`+`--host` mode
needs no cluster contact — pure tree resolution against the loaded
config.

Output is human-readable YAML by default. `--json`, `--from-file <ingress.yaml>`,
and per-field provenance are deferred until a real need surfaces.

### Out of scope for this ADR

- **Status page selectors** (`statusPage.match[]`) are *display
  filters* over already-materialized monitors, not config cascades.
  They keep their current flat OR-of-selectors model. The naming
  convention from commit 32abbed (`group:` exact + `groupRegex:`
  regex) is the precedent that `kube.match` now adopts for
  `namespaceRegex:` / `hostRegex:` — consistency without rework.
- **Removing `group:` from monitor config / removing the `groups:`
  config block.** Under the cascade, group-level notify is replaced
  by ancestor `notify:` in the tree, so the field's load-bearing
  role shrinks. But `group:` also carries display metadata
  (friendlyName, color, logo, description) that needs a new home
  before deletion. Deferred to a separate ADR. The current ADR
  preserves `group:` as one more scalar field that cascades like any
  other.

## Consequences

### Code changes (greenfield; no migration)

- `internal/config/config.go`
  - Delete: `KubePreset`, `KubeMatch`, `KubeMatchWhen`, `KubePause`,
    `Kube.Presets`, `Kube.Pause`, `Kube.AnnotationDomain`.
  - Add: `KubeMatchRule` with `When`, `Config`, `Nested`, `Ignore`,
    `Final`; `KubeMatchWhen` with `Namespace`, `NamespaceRegex`,
    `Host`, `HostRegex`, `Labels`; `KubeConfig` (the inline
    monitor-fields block, mirroring today's `KubePreset` fields
    minus `Slug`).
  - Custom `UnmarshalYAML` on the array fields of `KubeConfig` to
    detect the `!override` tag and stash a per-field "replace flag"
    that the merger consumes.
  - Validator rewritten per the rule list above.

- `internal/merger/merger.go`
  - Delete: `resolvePreset`, `formatIgnoredReason`,
    `formatAddedReason`, `mergeNotify`, `splitAndTrim`,
    `copyAnnotations`, every annotation-override branch in
    `Materialize`.
  - Add: tree-walker that produces the merge stack for an (Ingress,
    host) pair, plus a stack-resolver that applies the scalar /
    union / replace / acceptedStatusCodes rules and yields a final
    `KubeConfig`. Carries the rule chain alongside for the discovery
    `reason`.

- `internal/kube/kube.go`
  - Drop `annotationDomain` field and threading.

- `cmd/toggle-monitor/internal/cli/`
  - New `explain` subcommand (or new file under the existing CLI
    package).

- Tests in `internal/config/config_test.go`,
  `internal/merger/merger_integration_test.go`,
  `internal/merger/friendly_name_test.go` are largely rewritten.

Net: roughly 400–500 LOC deleted, 500–600 LOC added.

### Documentation changes

- `docs/config-schema.md` — `kube.*` section rewritten end-to-end.
  `kube.presets`, `kube.pause`, `kube.annotationDomain`, and all
  ingress-annotation tables are removed. New tree spec, selector
  vocabulary, merge rules, `!override` semantics, root-required
  rule, `final:` / `ignore:` semantics, debug surface, slug format.
- `docs/config-example.yaml` — `kube:` block replaced with a
  realistic cascading-tree example (the prototype from this design
  conversation, lightly cleaned).
- `docs/design-decisions.md` — short pointer entry: "`kube.match`
  redesigned as cascading tree; see ADR-0002."

### Operator-visible breaking changes (greenfield, but document them)

- Existing `kube.presets:` configs do not load. Operators rewrite as
  inline `config:` blocks under appropriate match rules.
- Existing `/kube.*` and `/config.*` annotations are ignored
  (treated as ordinary, unused annotations on the Ingress). The
  config equivalents must move into the tree.
- Existing `kube.pause:` entries do not load. Operators either fold
  them into the tree as `ignore: true` rules or, for actual
  operational pause, scale the Deployment to zero (the project's
  pre-existing operational pattern).

### Trade-offs accepted

- **Multi-match accumulate is more expressive but harder to reason
  about than first-match-wins.** Mitigated by the debug surface
  (rule chain in discovery, `explain` CLI) and by the operator-
  written root with all required fields making the merge floor
  predictable.
- **No reachability validation.** A rule that never fires won't be
  flagged. Mitigated by the `explain` CLI for spot-checking and by
  rule chains in discovery (operator notices "huh, this rule never
  appears in any chain").
- **`!override` is a YAML custom tag.** Custom tags are less
  familiar than plain values; `yaml.v3` handles them but requires
  per-field UnmarshalYAML plumbing. Acceptable cost for the
  syntactic clarity (`notify: !override [...]` reads cleaner than
  any object-form alternative).

## References

- [`docs/config-schema.md`](../config-schema.md) — to be rewritten
  following this ADR.
- [`docs/config-example.yaml`](../config-example.yaml) — to be
  rewritten following this ADR.
- [`docs/design-decisions.md`](../internal/design-decisions.md) — to be
  cross-linked to this ADR.
- Commit `32abbed` — established the `<field>` / `<field>Regex`
  selector convention in `statusPage.match[]` that this ADR adopts.
- Commit `1a55224` — current per-field validation of `KubePreset`
  fields, the basis for the per-`config:`-block static validation
  rules in this ADR.
