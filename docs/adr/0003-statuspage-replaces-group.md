# ADR 0003 — StatusPage replaces Group; tag-driven membership

**Status:** Accepted
**Date:** 2026-05-24
**Supersedes:** the `groups:` block, the `monitor.group` field, the
`kube.config.group` cascade override, and the `theme.defaultGroupColor`
key as defined in [`config-schema.md`](../config-schema.md); the
`StatusPageMatch` flat selector shape; the work explicitly deferred by
[ADR-0002](0002-kube-match-tree-cascade.md) §"Out of scope" /
"Removing `group:` from monitor config".

## Context

Current state:

- `Group` is a first-class config entity (slug, friendlyName,
  description, logoUrl, color, notify).
- `Monitor.Group` is a required 1:1 reference. Every monitor lives in
  exactly one group; group rollups on `/` partition the monitor set.
- `Group.Notify` unions with `Monitor.Notify` in
  `internal/lifecycle/lifecycle.go:420-431` for alert routing.
- `StatusPage` is a separate, optional public-facing entity.
  `StatusPageSection.Match[]` is a flat OR list of
  `{host, group, groupRegex, tags}` selectors with implicit AND
  between fields, OR between selectors.
- `kube.match` cascade ([ADR-0002](0002-kube-match-tree-cascade.md))
  treats `group:` as one more cascading scalar.

Problems:

- **1:1 group reference does not model the real domain.** A monitor
  `acme-service-a-eoapi-3-api` is simultaneously a "prod monitor,"
  an "ACME monitor," an "ACME/service-a monitor," and an "alpha
  monitor." Forcing one canonical group means dashboards either lose
  granularity or proliferate as flat sibling groups whose names embed
  the conjunction (`prod-acme-service-a-alpha`).
- **Two collection entities cover one need.** Operators define groups
  for dashboard rollups and (separately) status pages for public-ish
  views. Both express the same intent — "which monitors are part of
  this thing?" — through different vocabularies (Group: 1:1
  reference; StatusPage: tags + group + host). The duplication is the
  actual debt.
- **Notify on Group is a side door.** Group exists for rollup-display,
  but it also carries notify routing that compounds with
  `monitor.notify` and the kube cascade's notify. Three places to
  look when reasoning about who gets paged. ADR-0002 already pushed
  notify into the cascade; `Group.Notify` is the last holdout.
- **Flat OR-of-selectors cannot express name variants.** Real configs
  carry legacy/typo variants (`acme` vs `acme`). Selecting a
  section as `(prod AND acme) OR (prod AND acme)` needs a
  boolean tree, not a flat list.

ADR-0002 deliberately deferred removing `group:` because the display
metadata on `Group` (friendlyName, color, logo, description) needed a
new home before deletion. This ADR is that home.

Toggle-monitor is greenfield (no production users yet); ADR-0002's
reasoning about clean-break cost being at its minimum applies.

## Decision

Delete `Group` entirely. Promote `StatusPage` to the sole collection
entity. Redesign section membership around a boolean tag predicate
tree. Reuse the existing internal nav / theme infrastructure: every
route is internal, no separate public/private rendering for now.

### Data model

`groups:` config block — **deleted**. `Group` struct — **deleted**.
`Monitor.Group` field — **deleted**. `KubeConfig.Group` cascade
field — **deleted**. `Theme.DefaultGroupColor` — **deleted**.
`Group.Notify` merge in `lifecycle.go` — **deleted**.

`StatusPage` is the only collection entity:

```yaml
statusPages:
  - slug: acme            # required; ^[a-z][a-z0-9]*(-[a-z0-9]+)*$
    friendlyName: ACME    # required, non-empty
    description: ...        # optional
    logoUrl: https://...    # optional; http/https URL
    color: '#f5333f'        # optional; ^#[0-9a-fA-F]{6}$
    sections:               # required, non-empty
      - title: Production   # required, non-empty
        match: { ... }      # predicate tree, see below
```

No `notify`, no `showSections`, no `showIncidents`. All routes are
internal; the privacy lever those flags provided is unnecessary. A
future externally-exposed surface gets its own entity if it ships.

### Section match: boolean predicate tree

`StatusPageSection.Match` is a single predicate node, not a list. Two
kinds of node.

**Leaf** — selector, AND across its fields:

```yaml
match:
  tags: [prod, acme]      # monitor's tag set must include every listed tag
  hostRegex: '.*\.acme\.org' # Go regexp, auto-anchored ^...$
```

A leaf with both `tags:` and `hostRegex:` ANDs them. A leaf with
neither (`match: {}`) is a validation error. The `tags:` list, if
present, must be non-empty and each tag a non-empty string.

**Branch** — `{ any: [...] }` or `{ all: [...] }`, recurse to
arbitrary depth:

```yaml
match:
  any:
    - { tags: [prod, acme] }
    - { tags: [prod, acme] }   # name variant
```

```yaml
match:
  all:
    - { tags: [alpha] }
    - any:
        - { hostRegex: '.*\.acme\.org' }
        - { hostRegex: '.*\.acme\.org' }
```

Mixing `any:` and `all:` at the same node (`{ any: [...], all: [...] }`)
is a validation error. Empty branch (`{ any: [] }`) is a validation
error.

No `mode:` keyword, no `not:` / negation, no `host:` glob (one regex
engine — `hostRegex:` covers it), no `groupRegex:` (Group is gone),
no `name:` selector.

Rationale for `any:` / `all:`:

- Reads as English; mirrors JSON Schema (`anyOf` / `allOf`).
- No `mode:` keyword to misspell or forget; the key *is* the operator.
- Flat common case (`match: { tags: [prod, acme] }`) needs no
  wrapper — leaf alone is its own root.

Rationale for tag AND-by-default:

- Specificity composes by listing more tags. `[prod]` is broader than
  `[prod, acme]` is broader than `[prod, acme, service-a]`.
- A monitor's tag set lands it in many nested-specificity sections.
  OR-tag semantics ("any of these") inverts the relationship and
  surprises.

### Membership cardinality: N:M

A monitor matches a section if its tag set satisfies the section's
predicate tree (under tag-AND-by-default and recursive any/all
composition). The same monitor can match multiple sections across
multiple status pages. There is no priority resolution.

Consequences:

- Page-level counts dedupe by monitor slug — a monitor in two
  sections of one page counts once for the page badge, once per
  section in the sub-tiles.
- Sum across pages on `/` does **not** equal total monitor count.
  The header on `/` (global stats) is the authoritative total.
- The monitor detail page surfaces backlinks (which status pages
  select this monitor); see "Web layer".

The `/` dashboard is the sysadmin's full-system view; status pages
are the per-app views. Orphan monitors (matching zero sections) are
intentionally not surfaced on `/` — the global header already
accounts for them; operators triage orphans via `/monitors`.

### Web layer

**Routes.**

- `GET /status` — index of all configured pages, in config-file
  order (operators control the surface by ordering the YAML).
- `GET /status/<slug>` — page detail. Renders inside the standard
  operator nav with theme toggle (not the bare layout the current
  template uses). The deliberate stripping in
  `templates/status.templ` ("no operator nav, no theme toggle, no
  link back into the rest of …") is reverted.
- `GET /group/<slug>` and `GET /groups` — **deleted** along with
  their handlers and templates.
- `GET /monitors?page=<slug>` and `GET /monitors?page=<slug>&section=<n>`
  — filtered listing. Replaces `?group=<slug>`.

**Dashboard `/` tiles.**

- One tile per configured status page, in **config order** (same
  rule as `/status` — operators control the surface by ordering the
  YAML).
- Tile click target: `/status/<slug>`.
- Counts shown match today's group tile: Total / Up / Down / Paused /
  SSL-expiring / SSL-skipped.
- A status page with a single section renders one tile only (no
  duplicate page-tile + lone-section sub-tile).
- A status page with N>1 sections renders one page-level tile on `/`;
  the per-section sub-tile breakdown lives on `/status/<slug>`.

**Page detail (`/status/<slug>`).**

- Header: `friendlyName`, optional `logoUrl`, optional `description`,
  optional `color` accent stripe (left border or thin band), plus a
  3-state badge:
  - **Operational** (emerald) — all selected monitors `up`, no
    warnings.
  - **Degraded** (amber) — any selected monitor is `paused` /
    `SSL expiring` / `SSL skipped`, but none `down`.
  - **Down** (rose) — any selected monitor is `down`.
  - **No monitors** (slate) — the page's match tree selects zero
    monitors. Truthful empty state; avoids hollow-green misreading.
- Body: per-section sub-tile breakdown, same metric vocabulary as
  the page tile, one row per section in config order. Sub-tile click
  target: `/monitors?page=<slug>&section=<n>`.

**Monitor detail (`/monitor/<slug>`).**

- "Group" chip + link removed (current `kvGroup("Group", ...)` at
  `templates/monitor.templ:177,260`).
- Existing tags chip list stays.
- New "Appears on" section below tags: chips linking to up to 5
  matching status pages, in config order; `+N more` overflow chip
  when the monitor lands in >5. Computed at render time by
  evaluating each configured page's match trees against the
  monitor's tag set.

**Theme + color.**

- Reuse existing `themeBoot()` / `themeToggle()` from
  `templates/layout.templ:7-50`. The status page template gains them
  by switching to the standard layout.
- Per-page `color` is a single project-primary hex. Applied
  identically in light and dark mode. Renders on two surfaces only:
  - the header accent stripe on `/status/<slug>`,
  - the chip on `/` tiles.
  Not applied to section headers, table rows, or background fill —
  those stay in the slate/state vocabulary so the project colour
  does not compete with status colours.
- No `colorDark`, no auto-derivation, no contrast validator.
  Operator picks a hex that reads on both backgrounds, or accepts
  that it does not.

### Notify routing

Removing `Group.Notify` collapses three-layer routing (monitor + group
+ cascade) to two layers (monitor + cascade). Notify lives on:

- `monitor.notify` (static monitors).
- `kube.config.notify` cascading through `kube.match` per ADR-0002.

`Monitor.SlackChannelSlug` (`slack:`) routing is unchanged. The
`groupNotify` map and its merge loop in
`internal/lifecycle/lifecycle.go:420-431` are deleted.

### Validation

**Structural (errors).**

- `statusPages[].slug` matches the canonical slug pattern; unique
  across the list.
- `statusPages[].friendlyName` non-empty.
- `statusPages[].sections` non-empty.
- `statusPages[].sections[].title` non-empty.
- `statusPages[].sections[].match` non-empty (no `{}`).
- `any:` and `all:` are mutually exclusive at the same node; both
  set is an error.
- Branch with empty children list is an error.
- Leaf must contain at least one selector field (`tags` or
  `hostRegex`).
- `tags:` if present is non-empty; each tag a non-empty string.
- `hostRegex:` if present compiles as a Go regexp (auto-anchored at
  use time, mirroring `kube.match.when.hostRegex`).
- `color:` if present matches `^#[0-9a-fA-F]{6}$` (no 3-digit
  shorthand, no named colours).
- `logoUrl:` if present parses as a URL with scheme `http` or
  `https`.

**Structural (errors, removed from old code).**

- `groups[]` block, `groups[].slug`, `groups[].notify`,
  `monitor.group`, `kube.config.group`, `StatusPageMatch.Group`,
  `StatusPageMatch.GroupRegex`, `StatusPageMatch.Host`,
  `theme.defaultGroupColor` — all removed; presence in YAML is a
  structural error with a clear pointer to this ADR (see
  "Operator-visible breaking changes").

**Reachability validation is not performed.** Same trade-off as
ADR-0002's cascade: declaring a section that matches zero monitors
is not an error (kube discovery is dynamic). The page-level "No
monitors" badge surfaces this at runtime.

### Out of scope for this ADR

- **Public-facing variant of status pages.** When toggle-monitor needs
  an externally-exposed status surface (i.e., when auth lands), a new
  entity is added — likely sharing the predicate-tree mechanism but
  with stripped chrome and gated incident visibility. Not designed
  here.
- **Per-section badge.** Sections render counts only; the page badge
  covers state. If operators request per-section state later, derive
  from the section's monitor set with the same 3-state rule.
- **Section / page ordering by anything other than config order.**
  Operators control the surface by writing YAML in display order.
- **Page ordering on `/` or `/status`.** Config order, full stop.
  Operators control display order by editing the YAML.
- **Migration tool.** Greenfield; hard break. Loud error message at
  config load is the migration UX.
- **`explain --monitor <slug>`** sub-mode tracing predicate-tree
  matches. Real diagnostic need, but the monitor detail page's
  "Appears on" backlinks answer the same question empirically. Defer
  until a real session demands it.

## Consequences

### Code changes (greenfield; no migration)

- **`internal/config/config.go`**
  - Delete: `Group` struct, `Config.Groups`, `Theme.DefaultGroupColor`,
    `Monitor.Group`, `KubeConfig.Group`, `StatusPageMatch`,
    `StatusPage.ShowSections`, `StatusPage.ShowIncidents`,
    `ShowSectionsEnabled()`, `ShowIncidentsEnabled()`.
  - Add: `SectionMatch` (recursive node) with `Tags`, `HostRegex`,
    `Any`, `All`. Custom `UnmarshalYAML` to disambiguate leaf vs
    branch and enforce "exactly one of `any:` / `all:` for branches,
    at least one of `tags:` / `hostRegex:` for leaves."
  - Add: `Description`, `LogoURL`, `Color` fields on `StatusPage`.
  - Rewrite `validateStatusPages`: slug uniqueness, predicate-tree
    recursion, regex compile, color/URL syntax.
  - Delete all `groups:` validation: `seenGroups`,
    `validateKubeDependsOnRefs` group lookup, `monitor.group`
    "unknown group" check, `statusPage.match[].group` / `.groupRegex`
    validation, the kube-discovered group requirement.

- **`internal/merger/merger.go`**
  - Delete: `monitor.group` resolution (line 206), `GroupSlug` field
    on the resolved output (line 217), `KubeConfig.Group` cascading
    (line 400).
  - Add: predicate-tree evaluator (`func (m SectionMatch) Matches(tags []string, host string) bool`) — pure function, used by web layer to compute tile populations and backlinks.

- **`internal/lifecycle/lifecycle.go`**
  - Delete: `groupNotify` map (~lines 419–421), the union with
    `m.Notify` (~lines 429–431). Monitor notify is taken as-is from
    the resolved monitor.

- **`internal/store/`**
  - Drop `Monitor.GroupSlug` field. Replace any `GroupSlug` filter
    in queries with a tag-set / hostname filter the web layer uses
    to compute page/section membership.

- **`internal/web/web.go`**
  - Delete: `handleGroupsIndex`, `handleGroupPage`, their routes
    (lines 195, 196), the `groups` nav-wrap usages.
  - Rename `filter.Group` → `filter.Page` (or similar); add
    `filter.SectionIndex` for `?page=…&section=<n>`.
  - Update `handleStatusBySlug` to use the new shape and render
    with `navWrap` (operator nav + theme toggle).
  - Update `handleStatusIndex` to list every page in config order.
  - Update `handleHomepage` to tile per status page (in config
    order) instead of per group.
  - Update `handleMonitorDetail` to compute "appears on" backlinks
    (eval each page's predicate tree against the monitor).

- **`internal/web/templates/`**
  - Delete: `groups.templ` / `group.templ` analogues; the
    `GroupLink` helper in `layout.templ:195`; the `kvGroup` Group
    cells in `monitor.templ:177,260`; group-specific stat cells if
    not reusable.
  - Update `monitors.templ`: dashboard tile loop iterates status
    pages; tile uses page `color` chip; tile click → `/status/<slug>`.
  - Update `status.templ`: switch from bare layout to standard
    layout (operator nav + theme toggle); render header (logo +
    friendlyName + description + color stripe + 3-state badge);
    per-section sub-tiles in config order; click → `/monitors?page=…&section=…`.
  - Update `monitor.templ`: drop Group chip rows; add "Appears on"
    block with overflow.

- **`internal/kube/`**
  - Drop `Group` from `KubeConfig` plumbing.

- **`cmd/toggle-monitor/internal/cli/`**
  - `explain` subcommand: remove `group:` line from
    resolved-config output. No new flags.

- **Tests:**
  - `internal/config/config_test.go` — delete group fixtures; add
    predicate-tree fixtures covering leaf, AND (`all:`), OR
    (`any:`), nested mix, and each validation error path.
  - `internal/web/web_integration_test.go` — replace group-related
    cases (most fixtures) with status-page equivalents; add
    backlink and tile-population assertions.
  - `internal/merger/*_test.go` — drop group-resolution cases.

Net: ballpark 600–800 LOC deleted, 400–500 LOC added.

### Documentation changes

- `docs/config-schema.md` — `groups:` section deleted; `statusPages:`
  rewritten end-to-end with predicate-tree spec (leaf/branch grammar,
  validation rules, examples). `monitor.group` row deleted from the
  monitor table.
- `docs/config-example.yaml` — `groups:` block deleted, all
  `monitor.group:` references removed, `statusPages:` rewritten with
  realistic predicate-tree examples (Production / Alpha / Local-only)
  mirroring the design-conversation YAML.
- `docs/design-decisions.md` — short pointer: "Group removed;
  StatusPage is the sole collection entity; tag-driven membership;
  see ADR-0003."
- `docs/architecture.md` — Group entity mention removed; replace
  with StatusPage role.
- `docs/adr/0002-kube-match-tree-cascade.md` — unchanged. ADR is
  historical record; the "Out of scope … Removing `group:`"
  paragraph is delivered by this ADR.

### Operator-visible breaking changes (greenfield, but document them)

Config load fails on legacy shapes with explicit error messages:

- `groups:` → `"groups: no longer supported; define statusPages instead. See docs/adr/0003-statuspage-replaces-group.md."`
- `monitor[<slug>].group:` → `"monitor[<slug>].group: no longer supported; assign tags and define a matching statusPage section."`
- `kube.match[…].config.group:` → `"kube.match[…].config.group: no longer supported; use config.tags."`
- `theme.defaultGroupColor:` → `"theme.defaultGroupColor: no longer supported; set color: per statusPage."`
- `statusPages[].match[].host` / `.group` / `.groupRegex` /
  `.showSections` / `.showIncidents` →
  `"statusPages.sections[].match: flat selector list replaced by predicate tree; use any:/all: with tags:/hostRegex: leaves. See ADR-0003."`

URL changes:

- `/group/<slug>` and `/groups` — 404 after deployment. No redirect.
- `/monitors?group=<slug>` — query parameter renamed to `?page=`;
  the old form is unrecognised (no 1:1 mapping under N:M).

### Trade-offs accepted

- **N:M membership breaks rollup arithmetic.** Sum of per-page tile
  totals on `/` ≠ total monitor count. The global stats header on
  `/` is the source of truth. Operators must learn this; tile labels
  are page-specific (e.g., "ACME: 12 monitors") and the header
  carries "Total: 47 monitors" as the unambiguous global number.
- **Predicate trees are harder to debug than flat lists.** A nested
  `any/all` tree at depth 3 is non-trivial to mentally evaluate.
  Mitigated by the monitor detail page's "Appears on" backlinks
  ("where does this monitor land?" answered empirically) and by the
  page-level "No monitors" badge surfacing zero-match sections.
- **Orphan monitors are invisible on `/`.** Deliberate — `/` is the
  sysadmin view, the global header counts everything, and operators
  triage orphans via `/monitors`. Trade-off: tag-stamping bugs in
  `kube.match` do not surface as `/` artifacts; they show up as
  global totals not matching expectations or by routine `/monitors`
  review.
- **No reachability validation on predicate trees.** Same as
  ADR-0002 — declaring a section that matches zero monitors is not
  an error.

### Addendum (2026-05-24) — tag syntax broadened to allow `/`

Tags are validated by a dedicated `slug.ValidateTag` pattern, separate
from `slug.Validate`. A tag is one or more slug segments joined by
`/`: `^[a-z][a-z0-9]*(-[a-z0-9]+)*(/[a-z][a-z0-9]*(-[a-z0-9]+)*)*$`.
This permits operator-readable namespaced labels such as
`acme/app-a`, `betaco/data-server`, `gamma/web-app` without
loosening any slug-shaped identifier elsewhere (URLs, store keys,
kube-discovered identity stay strict).

The `/` is opaque to matching. Predicate-tree leaves continue to
match by exact set membership against the monitor's tag set —
`{ tags: [acme] }` does **not** match a monitor tagged
`acme/app-a`. Operators encode org-level rollups by stamping
both tags (typically via the kube cascade: parent `match` stamps
`acme`, nested `match` stamps `acme/app-a`, the union lands on
the monitor as `{acme, acme/app-a}`).

Call sites updated: `monitor.tags` validation, `statusPages[].sections[].match.tags`
validation, and `kube.match[].config.tags` validation now call
`slug.ValidateTag`. Tag length cap is shared with `slug.MaxLen`.

## References

- [ADR-0002](0002-kube-match-tree-cascade.md) §"Out of scope" — work
  explicitly deferred to this ADR.
- [ADR-0002](0002-kube-match-tree-cascade.md) §"Selector vocabulary" —
  the `<field>` / `<field>Regex` convention. This ADR drops `host:`
  glob in favour of `hostRegex:` only (one regex engine instead of
  two) at the predicate-tree leaf level.
- `docs/config-schema.md` — current `groups:` and `statusPages:`
  schemas; to be rewritten following this ADR.
- `docs/config-example.yaml` — current Group + `monitor.group` +
  StatusPage examples; to be rewritten.
- `internal/lifecycle/lifecycle.go:420-431` — `Group.Notify` merge
  loop to be deleted.
- `internal/web/templates/layout.templ:7-50` — existing
  `themeBoot()` / `themeToggle()` to be reused on the status page.
- `internal/web/templates/status.templ:302` — comment ("no operator
  nav, no theme toggle, no link back into the rest of …")
  documenting the current bare layout; reverted by this ADR.
