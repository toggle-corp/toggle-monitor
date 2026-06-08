---
status: accepted
date: 2026-06-07
deciders: [thenav56]
supersedes:
  - "0005-alertmanager-webhook-receiver.md (partial — the `### Slack rendering` sub-section only)"
refined_by:
  - "0007-fenced-error-bodies-for-mobile-url-rendering.md (the body-block formatting only)"
---

# ADR 0006 — Slack rendering: blocks-only parent shape

**Status:** Accepted
**Date:** 2026-06-07
**Supersedes:** the `### Slack rendering` sub-section of
[ADR 0005](0005-alertmanager-webhook-receiver.md) — severity-emoji
header, summary body, View-details and Runbook button row — and the
unADR'd in-code rendering design in `internal/slack/blocks.go`,
`internal/slack/ssl_blocks.go`, and `internal/alertmanager/blocks.go`:
big `header` block plus colored `attachment` with labeled-row context
body. ADR 0005 itself stays accepted; only its rendering sub-section
is replaced.

## Context

Slack notifications come from two renderers today. `internal/slack`
handles monitor probe outcomes (DOWN, resolve, SSL expiring, SSL renewed,
reminder replies); `internal/alertmanager` handles AM webhook alerts
(firing, resolve edit, late-resolve, throttle notice). They share a
structural pattern that has aged poorly:

- **Big `header` block** for the title (~22px, prominent).
- **Colored `attachments[0]`** wrapping the body, with one `context` block
  carrying 4–5 labeled rows: `*CC:*`, `*Monitor URL:*`, `*Reason:*`,
  `*Error:*`, `*Tags:*`.
- **Action row** (AM only): View-details (primary) + Runbook button.
- **Context footer** with timestamp + view-details link.

The problems with this shape, roughly in the order operators feel them:

- **Attachments are fixed-width.** Slack renders attachment content at a
  narrower max-width than top-level blocks. On mobile and on narrow
  desktop (sidebar open) the colored content wraps badly — sometimes
  mid-URL, often mid-label-value. The same payload looks tight on a wide
  screen and busted on a phone.
- **Labeled rows are visually heavy.** Five `*Label:* value` lines in a
  single context block stack as a wall of bold-then-dim repetition; the
  signal — what failed, where — is buried in the lattice.
- **The `header` block is loud.** It dominates a busy channel even when
  the underlying event isn't (e.g. an SSL warning).
- **Action buttons are duplicative.** "View details" appears once as a
  button (large) and once as a context-footer link (small), targeting the
  same URL. The button adds a block for no information gain.

A four-round design grilling (memory:
`project_slack_message_format_iA2.md`) settled a single shape — the
"iA2" variant — and stress-tested it against long URLs, long monitor
names, long error strings, multi-line stack traces, and combinations of
the above at a realistic 760px channel width. This ADR codifies that
shape as the binding contract for current and future Slack-emitting
modules.

## Considered Options

The grilling walked through eight shapes over four rounds; the
spectrum is preserved here because each entry rules out a specific
choice that would otherwise re-surface.

- **v1 — Conservative (kept attachments).** The current shape with the
  labeled-row context block tightened (`Monitor URL:` → `URL:`, Reason
  and Error merged). Cheapest change, but the fixed-width attachment
  problem and the labeled-row visual heaviness both remain. Rejected.
- **v2 — Balanced (kept attachments, smaller title).** Big `header`
  swapped for a bold `section`; body collapsed to two context lines
  inside the attachment. Tighter than v1, still attachment-bound.
  Strong candidate mid-grilling; rejected once attachment width was
  named as the load-bearing problem.
- **v3 (attachments) — Aggressive with stripe.** Title folded inside
  the attachment as a bold section, footer context below — one
  attachment, two blocks. Solves the labeled-row weight but still
  fixed-width. Rejected for the same reason as v2.
- **v3 (blocks-only) — Aggressive without stripe.** v3 with the
  attachment dropped entirely. Solves the width issue; loses the
  colored stripe. Promoted to the iA / iB / iC base.
- **iA1 — Title = name only (blocks-only).** Title section carries
  only `:emoji: *name*`; body carries error + URL together. Cleanest
  title; URL drops out of the at-a-glance line.
- **iA2 — Title = name + URL (blocks-only). Chosen.** Title carries
  `:emoji: *name* · <url>`; body carries the error string only.
  "What failed" reads in one glance from the title; the error block
  is free to grow (inline code → fenced multi-line) without crowding
  the title.
- **iB — Title in a `header` block (blocks-only).** Returns to the
  big `header` block but drops attachments. Loudest title; but
  `header` blocks are `plain_text` only, so the URL and severity chip
  can't live there. Rejected as too loud for non-critical events and
  awkward for AM (no inline code in headers).
- **iC — Stacked title (blocks-only).** iA1 with an explicit `\n`
  before the URL so the URL always wraps to its own line.
  Wrap-predictable but adds a line to every message even when the URL
  is short. Rejected: not worth the per-message vertical cost when
  iA2 wraps acceptably at typical channel widths.

## Decision

### One shape, three blocks

Every parent message — monitor DOWN, monitor resolve edit, SSL parent,
SSL renewed edit, AM firing, AM resolve edit, AM late-resolve — uses the
same three-block shape:

1. **Title** — `section` block, mrkdwn:

   ```text
   :red_circle: *<friendlyName> is DOWN*  ·  <url|url>
   ```

   The leading emoji is the severity signal (🔴 down, 🟢 up / resolved,
   ⚠️ SSL warn, 🔥 AM critical, ✅ AM resolved). The bold friendly-name
   and state word come next. The URL (probe URL for monitors, omitted for
   AM) sits after a middle-dot separator so "what failed" is one read.

2. **Body** — `section` block, mrkdwn:

   ```text
   `<error text>`
   ```

   For monitor parents: the failure reason (HTTP status + status text, or
   the transport-level error string when StatusCode is 0). For AM: the
   `annotations.summary`. Inline code (single backticks) is the default
   render. For multi-line content (stack traces, multi-line probe
   responses) escalate to a triple-backtick fenced block inside the same
   section.

   **Refined by [ADR 0007](0007-fenced-error-bodies-for-mobile-url-rendering.md):**
   the HTTP/SMTP monitor body always renders fenced, not inline, because
   Slack mobile auto-extracts URLs out of inline-code spans. AM and SSL
   body formatting are unaffected.

3. **Footer** — `context` block, mrkdwn:

   ```text
   <!here> <@U…>  ·  _<prefix> <!date^…>_  ·  <url|View details>
   ```

   Mentions come first (they're the call-to-action). The timestamp uses
   Slack's `<!date^…>` token so each viewer sees their own locale.
   View-details and (for AM) Runbook are inline mrkdwn links, not buttons.
   The cascade-effect note (`⏸ Pauses dependents: …`) and the AM
   late-resolve banner (`ℹ️ Resolved without an open incident on file…`)
   remain as separate `context` blocks when present, sitting between
   title and body.

### What changes from the existing renderers

- **No `attachments`.** Every `slack.Message` / `slack.ParentMessage`
  returned by the renderers has `Attachments: nil`. The colored left
  stripe goes away.
- **No `header` block.** The title moves into a `section` (~15px bold).
  Intentional: the eye should land on the title text and emoji, not on a
  block element's size.
- **No labeled-row body.** The 4–5 row labeled context block is gone.
  Fields that were on labeled rows either fold into the title (URL), into
  the body (error), into the footer (mentions, timestamp, view-details),
  or drop from the parent entirely (tags — they remain on the
  `DownInput` / `SSLDownInput` structs for the detail page, but don't
  render on the parent).
- **No `actions` block in AM.** View-details and Runbook become inline
  mrkdwn links in the footer context, alongside the timestamp.
- **Resolve phrasing.** Monitor resolve headers read
  `"is UP (down around %s)"`, not `"was down for %s"`. The check interval
  is the resolution limit; "for" overclaims precision the probe can't
  possibly have.

### Severity without the stripe

The colored attachment stripe was the only place severity was rendered
graphically; it goes away here. Severity is now carried by:

- **Leading emoji** in the title — 🔴 / 🟢 / ⚠️ / 🔥 / ✅. Operators read
  this in <100ms, well before the bold name.
- **Inline severity chip** on AM messages — `` `critical` `` / `` `warning` ``
  / `` `info` `` as inline code, rendered next to the alertname.
  Duplicates the emoji's information but adds a textual handle for
  grepping the channel.

If operators report missing the stripe in busy channels (it was a useful
peripheral-vision signal during cascading outages), the fallback isn't
re-introducing attachments — it's adding a second emoji or a leading
`[CRITICAL]` prefix. That's a v2 conversation.

### Long-content rules

The mockup stress-tested long URLs, long monitor names, long error
strings, and multi-line stack traces at 760px. The rules:

- **Long monitor names** are bold by default; that makes the title
  visually loud and is accepted. If real-world names commonly exceed
  ~40 chars and the boldness becomes a problem, the mrkdwn changes from
  `*<name> is <STATE>*` to `*<name>* is <STATE>` (only the name bold).
  Don't pre-shorten the name; operators rely on the exact identifier to
  grep the config.
- **Long URLs** wrap break-anywhere inside the title section. Acceptable.
  Don't truncate at render time — the URL must be clickable and complete.
- **Long error strings** wrap inside the body section's inline code span.
  Acceptable up to a few wrapped lines.
- **Multi-line errors** (stack traces, multi-line probe responses)
  escalate to a triple-backtick fenced block in the same body `section`.
  The existing `DownInput.BodyMaxChars` cap (today applied to HTTP
  response bodies) extends to cover fenced multi-line errors; a parallel
  line-count cap is added (no-op for typical operation, prevents a
  200-line Java stack from dominating the channel).

### Reminder and thread replies

Out of scope for the parent-shape contract. `BuildReminderReply`,
`BuildResolveReply`, `BuildSSLReminderReply`, `BuildSSLResolveReply`,
and `BuildAMResolveReply` are already terse (one or two `section` /
`context` blocks, no attachments) and remain unchanged. Future modules
adding new thread-reply types follow the same blocks-only convention
but may use one or two blocks as the content warrants.

### What this binds for future modules

Any future code path that emits a Slack message — a new monitor kind
(TCP, DNS, cert-transparency log watcher), a new alert source
(Prometheus-direct integration that bypasses Alertmanager), an admin
notification (config reload failure, retention sweep summary), a
broadcast (release notes, scheduled-maintenance announcement) — uses the
three-block shape:

1. Title section: emoji + bold subject + optional URL / context.
2. Body section: the substance. Inline code for short errors; fenced for
   multi-line; plain mrkdwn for narrative bodies (e.g. AM summaries).
3. Footer context: mentions + timestamp + view-details (and any other
   destination link) as inline mrkdwn.

No attachments. No `header` blocks. No `actions` blocks. The shared
`slack.Message` envelope (already exported from `internal/slack`) is the
single composition surface; new callers compose blocks directly rather
than wrapping in attachments.

## Consequences

- **Good:** messages render at full channel width on mobile and narrow
  desktop. Visual noise drops sharply (typical DOWN parent is 3 blocks
  vs the prior 4–6). The contract is one shape across every emitter,
  which makes new-module rendering a copy-paste exercise rather than a
  design decision.
- **Good:** the `actions` block + button helpers (`slack.LinkButton`,
  `slack.ActionsBlock`) become unused in the parent path. They stay
  exported for any future interactive block-kit flow but are no longer
  load-bearing for parent rendering.
- **Bad:** the colored stripe — a useful peripheral signal in busy
  channels — is gone. Operators who relied on it have to relearn the
  emoji vocabulary. Mitigation is documentation, not re-introducing
  attachments.
- **Bad:** test rewrite. Current assertions in
  `internal/slack/blocks_test.go`,
  `internal/slack/ssl_blocks_test.go`, and
  `internal/alertmanager/blocks_test.go` pattern-match on labeled rows
  (`*Monitor URL:*`, `*Tags:*`, `*Reason:*`) and on the `actions` block
  shape. All have to be rewritten to assert the new contract.
- **Bad:** the integration tests in `internal/lifecycle/...` exercise the
  real Slack-post path and may need to update payload-shape assertions.
  The lifecycle suite is the only thing that catches an end-to-end Slack
  regression (per `CLAUDE.md`); it must pass after the rewire.
- **Revisit if:** operators report (a) missing the stripe color as a
  peripheral signal during cascading outages, (b) multi-line errors
  dominating the channel beyond what a line-cap can fix, or (c) inline
  links in the footer (View details, Runbook) get clicked materially less
  than the prior buttons. Any of these would shift the trade-off and could
  justify either selective re-introduction of `actions` (e.g. Runbook on
  AM) or a non-attachment alternative for the stripe (e.g. a leading
  colored emoji block).

## References

- Mockup: `/tmp/design-mockups/slack-monitor-compact.html` — iA2 column
  with stress matrix. Every message has a `↗ blockit-preview` deep-link
  that loads the payload into Slack's Block Kit Builder.
- Memory note: `project_slack_message_format_iA2.md` — settled design
  decisions and per-package wiring guidance.
- Renderers rewired by this ADR:
  - `internal/slack/blocks.go::BuildDownParent`, `BuildResolveEdit`
  - `internal/slack/ssl_blocks.go::BuildSSLParent`, `BuildSSLResolveEdit`
  - `internal/alertmanager/blocks.go::BuildAMOpen`,
    `BuildAMResolveEdit`, `BuildAMLateResolve`, `BuildAMThrottleNotice`
- Adjacent ADRs:
  - [ADR 0004](0004-burst-dispatch-supersedes-always-coalesce.md) — burst
    dispatcher; this ADR doesn't change dispatch logic, only the render.
  - [ADR 0005](0005-alertmanager-webhook-receiver.md) — AM receiver; the
    `### Slack rendering` sub-section there is partially superseded by
    this record (the surrounding receiver architecture stays accepted).
