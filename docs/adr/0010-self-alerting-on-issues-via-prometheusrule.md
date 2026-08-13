---
status: accepted
date: 2026-08-13
deciders: [monitoring-team]
---

# ADR 0010 — Self-alerting on `/issues` via an exported gauge and a shipped PrometheusRule

**Status:** Accepted
**Date:** 2026-08-13

## Context

`/issues` is the operator-awareness page: everything toggle-monitor
noticed at runtime that a human probably wants to look at. It has
accumulated four sources:

| source | what it means | consequence if unfixed |
| --- | --- | --- |
| `kube-invalid` | an observed ingress host could not be materialized | **that host is not probed at all** |
| `slack-mapping` | a `slack.userMapping` entry failed validation | a mention resolves to nothing, so an alert may reach nobody |
| `missing-parent` | a `dependsOn` slug matches no monitor | the dependency gate is skipped; a parent outage no longer suppresses children |
| `annotation` | an annotation value was rejected (ADR-0009) | the monitor falls back to its config-tree value |

Every one of these is a *silent* failure. The first three are
long-standing; the fourth arrives with ADR-0009, which hands app teams
a new way to be wrong — an unreviewed annotation, written in a chart by
someone who never sees toggle-monitor's UI, that a values typo can
break for every instance of a project family at once.

The page has one structural weakness: **it only exists while someone is
looking at it.** Nothing pages, emails, or posts when a count goes from
zero to nonzero. A `kube-invalid` row means a host is unmonitored, and
the tool whose whole job is noticing unmonitored things does not notice
this one. ADR-0009 makes that worse in a specific way: it deliberately
chooses to *degrade rather than fail* — a bad annotation must never
cost availability monitoring, so the monitor materializes anyway with a
warning. That is the right call, and it also means a misconfiguration
now produces no visible symptom at all. Degrading quietly is only
defensible if something else is loud.

toggle-monitor already runs a Prometheus registry behind `/metrics`,
already ships a `ServiceMonitor` and a Grafana dashboard for
kube-prometheus-stack, and — per ADR-0005 — already *receives*
Alertmanager webhooks and renders them on `/alerts`. The integration
today is inbound-only.

## Considered Options

1. **Metrics only.** Export the gauge, document example rules in
   `docs/operations.md`, let operators write their own.
2. **Export a gauge and ship a `PrometheusRule`.** Prometheus
   evaluates; the operator's existing Alertmanager routes.
3. **Push directly to Alertmanager's `/api/v2/alerts`.** toggle-monitor
   POSTs its own alerts on a ticker.
4. **Route self-issues through the burst dispatcher.** Reuse the
   existing Slack notifier and treat an issue like a monitor going
   down.

## Decision

Chosen: **Option 2.**

A `toggle_monitor_issues{source="…"}` gauge is published once a minute
from the same readers `/issues` renders from, and the Helm chart gains
a `prometheusRule.enabled` template with one alert per source plus a
`ToggleMonitorDown` scrape-health rule.

The four `source` label values are a public contract — anyone's alert
rules match on them — so renaming one is a breaking change.

Option 3 is rejected because it duplicates machinery the project has
already built twice: alert identity, dedup, and resolve lifecycle live
in the burst dispatcher (ADR-0004), and an outbound Alertmanager client
would be a third implementation of them, with its own retry and
back-off story, to deliver information Prometheus can already derive
from a gauge it is scraping anyway.

Option 4 is rejected because the burst dispatcher's vocabulary is
monitor uptime — open/resolve on a slug, dependency suppression,
reminders. A config typo is not an incident on a monitor, has no
parent, and would need its own coalescing rules to avoid re-posting
every reconcile. Worse, three of the four sources are *about the
notification path itself*: alerting about a broken `slack.userMapping`
through the Slack notifier that the broken mapping feeds is a
dependency loop that fails exactly when it is needed.

Option 1 is what this record delivers *minus* the shipped rules. The
rules are shipped because the alert thresholds encode judgment that
belongs with the code — `kube-invalid` warns after 15m, a rejected
annotation only after 1h — and an operator who has to derive that from
prose will usually skip it. Every rule is individually toggleable and
the block is off by default, so Option 1 remains available by leaving
`prometheusRule.enabled: false`.

### Two properties the implementation must keep

- **Zero is published, not dropped.** A source that returns to zero
  keeps emitting its series. A gauge that stops emitting leaves any
  alert on it stuck firing, because the expression has nothing left to
  evaluate against.
- **An unreadable source holds its last value.** Each source reports
  `(count, ok)`; on `ok == false` — a DB blip reading `kube-invalid`,
  say — the series is left alone. Writing a zero there would resolve a
  real alert on nothing more than a transient error.

## Consequences

- **Good: silent misconfigurations become loud**, through the operator's
  existing routing, escalation, and silencing — no new notification
  path to configure or maintain.
- **Good: the page and the alert cannot drift.** Each gauge source is a
  closure over the same reader the web handler uses; a fifth source
  added to one is visibly missing from the other.
- **Good: the loop closes.** An operator already routing Alertmanager
  into toggle-monitor's ADR-0005 webhook sees self-issues on `/alerts`
  next to everything else, with no extra wiring.
- **Bad: self-alerting cannot cover its own absence.** While
  toggle-monitor is down, every `toggle_monitor_issues` rule is
  silent — the gauge is only published by the process being alerted
  about. `ToggleMonitorDown` is the mitigation and is why it ships
  enabled with `severity: critical`, but it depends on Prometheus
  scraping, which is a second thing that can be down. Genuine external
  dead-man's-switch coverage is the existing outbound heartbeat's job,
  not this record's.
- **Bad: a hard dependency on Prometheus for alerting.** A deployment
  that scrapes nothing gets the gauge and no alerts. Acceptable: the
  chart already assumes kube-prometheus-stack for `ServiceMonitor` and
  the dashboard, and the rules are off by default.
- **Bad: alert-fatigue surface.** Four always-on rules against
  long-lived conditions — an unfixed `dependsOn` typo fires until
  someone fixes it. Mitigated by per-source `for` windows tuned to
  blast radius and by each rule being individually disableable, but a
  team that ignores `/issues` today will be a team that silences these.
- **Neutral: one-minute refresh, 30-minute inputs.** The reconcile that
  feeds `kube-invalid` and `annotation` runs on `resyncInterval` (30m
  default), so the gauge's own cadence is not the limiting factor.
  ADR-0009 already flags the informer-event-driven reconcile as a
  separate change; it would tighten this too.
- **Revisit if** a source needs per-object identity in the alert (which
  ingress, which slug). Prometheus labels are the wrong place for
  unbounded cardinality, so that would mean linking to `/issues` from
  the annotation rather than expanding the label set.

## References

- [ADR-0004](0004-burst-dispatch-supersedes-always-coalesce.md) — the
  burst dispatcher owns monitor-uptime alert lifecycle; the reason
  self-issues do not route through it.
- [ADR-0005](0005-alertmanager-webhook-receiver.md) — the inbound
  webhook these alerts can loop back into.
- [ADR-0008](0008-self-health-degraded-mode.md) — the other
  self-observation surface; degraded mode is about the monitor's view of
  the network, this record is about its view of its own config.
- [ADR-0009](0009-from-value-sources-for-kube-discovery.md) — the
  `annotation` source, and the degrade-don't-fail choice that makes
  loud reporting necessary.
- `internal/lifecycle/issues.go` — `issuesReporter`, the source labels.
- `internal/observability/observability.go` — the `Issues` gauge.
- `internal/web/web.go` — `issueCount`, `AnnotationIssueReader`; the
  readers the gauge closes over.
- `deploy/helm/toggle-monitor/templates/prometheusrule.yaml` — the
  shipped rules.
