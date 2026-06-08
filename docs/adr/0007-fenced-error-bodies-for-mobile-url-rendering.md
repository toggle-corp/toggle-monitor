---
status: accepted
date: 2026-06-08
deciders: [thenav56]
refines:
  - "0006-slack-rendering-blocks-only-parent-shape.md (the body-block sub-decision only)"
---

# ADR 0007 — Render HTTP/SMTP error bodies in fenced code blocks

**Status:** Accepted
**Date:** 2026-06-08
**Refines:** [ADR 0006](0006-slack-rendering-blocks-only-parent-shape.md) — the
body-block sub-decision of the iA2 shape. ADR 0006's three-block parent
contract is unchanged; only the mrkdwn formatting of the body content (and
the reminder reply's body line) is refined here.

## Context

ADR 0006 specified the iA2 body block as a `section` carrying the failure
reason wrapped in **inline code** — single backticks around the whole
string. For HTTP/SMTP probe failures the body string is the Go
`error.Error()` text, and that text routinely contains a URL: e.g.

```text
Get "https://api.example.com/health": context deadline exceeded
```

On Slack mobile (iOS and Android tested), URLs inside an inline-code span
are auto-extracted by Slack's mobile renderer **out of the code span** and
re-rendered as a separate auto-linked URL chip, leaving the original code
span with an empty quoted region where the URL used to be. The same body
on desktop renders as a single inline-code run containing the URL as
literal text. The mobile output for the example above is:

```text
https://api.example.com/health  https://api.example.com/health  Get "": context deadline exceeded
```

— two URL chips followed by a code span with empty quotes. This is wrong
for two reasons: the URL appears as a clickable link the operator could
follow into a live endpoint (sometimes a production URL paged at 3am),
and the body's "what failed" reads as `Get ""` with the failing target
erased.

Verification probes posted to `#nav-test` over multiple rounds confirmed:

- Inline code (single backticks) around a string containing a URL → mobile
  extracts the URL into a separate chip; desktop renders correctly.
- Fenced code (triple backticks, newline-delimited) around the same
  string → URL renders as literal text on both mobile and desktop.

Slack's documented behaviour treats fenced blocks as preformatted content
that is not URL-scanned; inline spans are URL-scanned. The mobile
extractor is consistent with that contract, even though the desktop
client doesn't enforce it. We do not control the mobile renderer; we
control the mrkdwn we emit.

The same surface exists in two other renderers:

- **SSL parent body** (`internal/slack/ssl_blocks.go`) carries Issuer /
  Subject as labeled rows with inline-code values. Those values are DN
  strings (e.g. `CN=*.example.com, O=Example, L=…`) — no URL component,
  no extraction surface. Not affected.
- **Alertmanager body** (`internal/alertmanager/blocks.go`) carries the
  `annotations.summary` as curated prose (no inline code). Not affected.

The body content most likely to carry a URL is the HTTP/SMTP probe
failure reason, which is exactly the path ADR 0006 specified as inline-
coded. The `BuildReminderReply` thread reply has the same exposure — its
its `*Last error:*` line wraps the same probe error in inline code on
a single line alongside the labels.

## Considered Options

- **Keep inline code; sanitize URLs out of error text before emit.**
  Strip or replace any URL substring before wrapping in inline code (e.g.
  reduce `Get "https://api.example.com/health"` to `Get "<URL>"`). Cheap
  on the render side but loses operationally useful information — the
  exact URL the probe was hitting is one of the few signals that survives
  every transport-level failure mode. Rejected: don't lose data to work
  around a renderer quirk.
- **Keep inline code; emit the URL as a Slack link instead.** Wrap the
  URL substring in `<URL|URL>` before wrapping the surrounding string in
  inline code. Doesn't work — inline code spans are not parsed for mrkdwn
  link syntax, so `<…|…>` renders literally on desktop and the mobile
  extractor still kicks in. Rejected.
- **Drop the inline-code wrapper on the error body entirely.** Render
  the error as plain mrkdwn. Solves the extraction (no code span to
  break out of), but loses the visual cue that this is machine output,
  not narrative — and a plain-text URL would still auto-link on both
  clients (the desktop render becomes inconsistent with itself).
  Rejected as a regression of the iA2 shape's intent.
- **Wrap the error body in a fenced code block.** Triple-backtick
  fence, newline-delimited content. Slack mobile and desktop both treat
  fenced content as preformatted; URLs render as literal text on both.
  Adds a small amount of vertical space (gray code box vs an inline run)
  and a slightly different visual register. Chosen.

## Decision

### Body block: fenced, not inline

The HTTP/SMTP uptime error body — the substance of ADR 0006's "Block 2"
in the iA2 shape — is rendered as a fenced code block: the mrkdwn is
opening triple-backticks, then a newline, then the error string, then a
newline, then closing triple-backticks (i.e. `` "```\n" + err + "\n```" ``).

This applies to both `BuildDownParent` and `BuildResolveEdit` (they share
the body via `buildParentBlocks` → `downBodyText`). The body still
collapses to nothing when both `StatusCode == 0` and `LastError == ""`,
preserving the existing defensive behaviour for callers with no failure
reason.

The pre-existing fenced path in `buildParentBlocks` that wraps a
`ResponseBody` within `BodyMaxChars` is unchanged — that path was already
fenced and didn't have the leak.

### Reminder reply: two blocks

`BuildReminderReply` previously emitted one `section` carrying three
labeled lines:

```text
⏰ *Still down for:* `3d`
*Last checked:* <date>
*Last error:* `<err>`
```

The third line had the same inline-code-with-URL exposure as the parent
body. Splitting it across two blocks fixes the leak and lets the error
breathe vertically:

- **Block 1** (`section`, mrkdwn) — labels only:

  ```text
  ⏰ *Still down for:* `<dur>`
  *Last checked:* <date>
  ```

  The `*Last error:*` label is removed; the fenced error block that
  follows is self-evidently the error and doesn't need labelling.

- **Block 2** (`section`, mrkdwn) — fenced error, only emitted when
  `LastError != ""`. Same triple-backtick / newline / payload / newline /
  triple-backtick shape as the parent body above.

When `LastError` is empty the reminder collapses to a single
labels-only block (the existing "no error available" case).

### What is unchanged

- **ADR 0006's three-block iA2 contract.** Title section, body section,
  footer context. This ADR refines block 2's mrkdwn formatting only.
- **`internal/slack/ssl_blocks.go`.** The SSL parent body stays as
  labeled Issuer/Subject rows with inline-code values. No URL surface
  in DN strings; no leak to fix.
- **`internal/alertmanager/blocks.go`.** The AM body is curated prose,
  plain mrkdwn. No inline code, no leak.
- **The title link.** `<URL|URL>` in the title section renders cleanly
  on mobile (it's an explicit mrkdwn link, not a URL inside a code
  span). Stays.
- **`downBodyText`'s data shape.** Same inputs, same selection between
  `StatusCode + StatusText` and `LastError`. Only the wrapper changes.

## Consequences

- **Good:** URLs inside error bodies render as literal text on Slack
  mobile, matching desktop. No more empty-quote bodies in the on-call
  channel; no more accidental auto-link to a failing production URL.
  The fix is the smallest possible mrkdwn change that resolves the
  observed bug.
- **Good:** the renderer is now consistent in its escalation rule —
  short single-line errors and long multi-line errors both go through a
  fenced block, removing the inline/fenced split ADR 0006 left in
  place. Future contributors don't have to choose between the two
  forms; there is one.
- **Bad:** slightly heavier vertical footprint on desktop. A fenced
  block draws a gray box and reserves more vertical space than an
  inline-code run inside a section. In the typical 1–2 line error case
  the extra space is ~1 line; in the cascade-effect or banner case it
  stacks with the existing context blocks. Accepted: a correctly-
  rendered mobile message at a slightly higher desktop cost beats a
  visually tighter desktop message that misrenders on mobile.
- **Bad:** the reminder reply doubles its block count when an error is
  present (1 → 2). The reminder fires at `reminderInterval` while a
  monitor is down, so the channel sees more blocks over a long
  outage. Accepted on the same trade-off as above; the second block
  only appears when `LastError != ""`, which is also the only case the
  bug applied to.
- **Revisit if:** Slack changes the mobile renderer to stop extracting
  URLs from inline-code spans (returning the body to a 2-block 1-line
  reminder would be a one-line change). Or if a future body kind needs
  to carry both an inline-code error and a clickable URL on the same
  line (we have no such case today and the title is the canonical place
  for the URL).

## References

- ADR being refined:
  [ADR 0006](0006-slack-rendering-blocks-only-parent-shape.md) — the
  three-block iA2 shape; this ADR refines block 2's formatting only.
- Adjacent ADR:
  [ADR 0005](0005-alertmanager-webhook-receiver.md) — the AM body path,
  which is not affected (curated prose, no inline code).
- Renderers updated by this ADR:
  - `internal/slack/blocks.go::downBodyText` — inline → fenced.
  - `internal/slack/blocks.go::BuildReminderReply` — one block → two
    blocks (when `LastError != ""`).
- Test guards: `internal/slack/blocks_test.go` —
  `TestBuildDownParent_urlInErrorBodyStaysLiteral` and
  `TestBuildReminderReply_urlInErrorStaysLiteral` pin the regression
  shape so the inline-code wrapper can't be reintroduced silently.
- Probe rounds: verification posts to `#nav-test`
  (`~/tmp/toggle-monitor/slack-mobile-probe/`).
