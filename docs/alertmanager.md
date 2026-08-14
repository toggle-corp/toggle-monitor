# Alertmanager integration

Toggle-monitor exposes an Alertmanager webhook receiver at
`/webhooks/alertmanager`. Each AM fingerprint maps 1:1 to a Slack
message; resolves edit the parent and post a thread reply. This
document covers configuring kube-prometheus-stack (or any standalone
Alertmanager) to deliver to that endpoint, and walks an operator
through verification + the failure modes worth knowing about.

Authoritative design: [ADR-0005 — Alertmanager webhook
receiver](./adr/0005-alertmanager-webhook-receiver.md). The pipeline
is deliberately thin — AM alerts are not monitor probes; they don't
participate in `alert.Apply`, `coalesce.Manager`, or the monitor
state machine. They are first-class but separate.

## Toggle-monitor side

Add the `alertmanager:` top-level block to your config. Brief
example below; see
[config-schema.md §5c](./config-schema.md#5c-alertmanager-webhook-receiver-optional)
for the per-field reference and
[config-example.yaml](./config-example.yaml) for a complete working
match tree.

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

The `tokenEnv` value names the env var holding the Bearer token the
runtime constant-time-compares against the inbound `Authorization`
header. The receiver fails to start if the env var is unset or
empty.

### Routing from namespace annotations

If your namespaces already declare who owns them — the same
annotations `kube.match` reads — an `alertmanager.match` rule can
source `slack` / `notify` from them instead of duplicating the
ownership tree ([ADR-0013](./adr/0013-from-value-sources-for-alertmanager-routing.md)):

```yaml
    - when: {}
      config:
        # A root slackFrom needs a default: — it stands in for the
        # root's required slack:, and an unannotated namespace would
        # otherwise resolve to no channel at all.
        slackFrom:
          namespaceAnnotation: app.example.com/slack
          default: ops-alerts
        notifyFrom:
          namespaceAnnotation: app.example.com/notify
```

Put this rule near the top of the tree. Document order is precedence, so
a `when: {}` rule placed last overrides every `slack:` a more specific
rule set above it.

The namespace name comes from the alert's `namespace` label; set
`namespaceLabel:` on the source if your exporter relabels it
(`exported_namespace`, `kubernetes_namespace`). This requires the
`kube:` block — the Namespace informer belongs to the kube watcher —
and only the namespace scope is available, because an alert's own
annotations are written by whoever authored the alerting rule.

A value that cannot be used never costs you the alert: it routes to the
cascade's channel, logs `am.value_source.rejected`, and increments
`toggle_monitor_am_value_source_rejections_total`. The resolved
provenance is appended to the `am_alerts.rule_chain` column.

## kube-prometheus-stack — full helm values

Drop-in replacement for a typical `pagerduty_configs` setup. The
configuration assumes:

- A k8s `Secret` named `toggle-monitor-webhook` in the same namespace
  as Alertmanager (`monitoring` below), holding the same Bearer
  token toggle-monitor reads via `tokenEnv`.
- Toggle-monitor reachable in-cluster at
  `toggle-monitor.toggle-monitor.svc.cluster.local:8080`.

```yaml
alertmanager:
  enabled: true
  ingress:
    enabled: false
    ingressClassName: "nginx"
    hosts: [alertmanager.k8s.local.example.com]
    paths: [/]
  alertmanagerSpec:
    secrets:
      - toggle-monitor-webhook        # mounted at /etc/alertmanager/secrets/<name>/
  config:
    route:
      group_by: ["namespace", "alertname"]
      group_wait:       30s
      group_interval:   5m
      repeat_interval:  4h
      receiver: "toggle_monitor"
      routes:
        - receiver: "default_receiver"
          matchers:
            - alertname = "Watchdog"
    receivers:
      - name: "default_receiver"
      - name: "toggle_monitor"
        webhook_configs:
          - url: "http://toggle-monitor.toggle-monitor.svc.cluster.local:8080/webhooks/alertmanager"
            send_resolved: true
            http_config:
              authorization:
                type: Bearer
                credentials_file: /etc/alertmanager/secrets/toggle-monitor-webhook/token
```

The Secret that backs `alertmanagerSpec.secrets` — mounted by the
operator at `/etc/alertmanager/secrets/<name>/<key>`:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: toggle-monitor-webhook
  namespace: monitoring          # same namespace as alertmanager
type: Opaque
stringData:
  token: "<output of openssl rand -hex 32>"
```

### Generating the token

Use `openssl rand -hex 32` — 64 hex chars, 256 bits of entropy,
header-safe characters that survive shell quoting, YAML, and
Alertmanager's `credentials_file` framing without escaping. The
validator only requires the env var to be non-empty; there is no
length floor, but hex-256 is the conventional default for long-lived
secrets like this one.

```bash
# 1. Generate once.
TOKEN=$(openssl rand -hex 32)

# 2. AM-side Secret (kube-prometheus-stack mounts it).
kubectl create secret generic toggle-monitor-webhook \
  --namespace monitoring \
  --from-literal=token="$TOKEN" \
  --dry-run=client -o yaml | kubectl apply -f -

# 3. toggle-monitor-side Secret (fed via env var).
kubectl create secret generic toggle-monitor-am-token \
  --namespace toggle-monitor \
  --from-literal=token="$TOKEN" \
  --dry-run=client -o yaml | kubectl apply -f -

# 4. Paranoia check — confirm both Secrets carry the same value.
diff \
  <(kubectl get -n monitoring secret toggle-monitor-webhook \
      -o jsonpath='{.data.token}' | base64 -d) \
  <(kubectl get -n toggle-monitor secret toggle-monitor-am-token \
      -o jsonpath='{.data.token}' | base64 -d) \
  && echo "tokens match"
```

If `openssl` is unavailable, equivalent alternatives:

```bash
head -c 32 /dev/urandom | xxd -p -c 64                       # pure unix
python3 -c 'import secrets; print(secrets.token_hex(32))'    # python
```

Avoid `uuidgen` (only 122 bits and includes dashes), `date | md5sum`
(predictable entropy), and base64 forms (the `+/=` characters are
valid in `Authorization: Bearer` headers but routinely break in
operator-edited YAML and in `kubectl create secret --from-literal`
when the value isn't quoted exactly right).

The matching toggle-monitor env var, fed from a Secret in the
toggle-monitor namespace:

```yaml
env:
  - name: ALERTMANAGER_WEBHOOK_TOKEN
    valueFrom:
      secretKeyRef:
        name: toggle-monitor-am-token
        key: token
```

The two Secrets — AM-side and toggle-monitor-side — must carry the
same byte-for-byte value but live in different namespaces (k8s
doesn't share Secrets cross-namespace). Operators must keep them in
sync; rotation = update both, then restart both pods (see "Token
rotation" below).

## Standalone Alertmanager

Equivalent `alertmanager.yml` for non-helm setups:

```yaml
global:
  resolve_timeout: 5m

route:
  group_by: [namespace, alertname]
  group_wait: 30s
  group_interval: 5m
  repeat_interval: 4h
  receiver: toggle_monitor
  routes:
    - receiver: default_receiver
      matchers:
        - alertname="Watchdog"

receivers:
  - name: default_receiver
  - name: toggle_monitor
    webhook_configs:
      - url: "https://toggle-monitor.example.test/webhooks/alertmanager"
        send_resolved: true
        http_config:
          authorization:
            type: Bearer
            credentials_file: /etc/alertmanager/toggle-monitor.token
```

Place the token file at `/etc/alertmanager/toggle-monitor.token`
(`chmod 0400`, owned by the AM process user). Reload Alertmanager
after rotation so it re-reads the file.

## Critical setup points

Read these before deploying. Each is a real failure mode operators
have hit.

- **`send_resolved: true` is mandatory.** Without it, Alertmanager
  only delivers firings — toggle-monitor never sees resolves, parent
  messages never get edited, threads never close. If you see the
  `am.alert.resolve_no_thread` warning in toggle-monitor's logs
  while resolves are clearly happening upstream, this is the first
  thing to check.

- **Tune `group_by` / `group_wait` / `repeat_interval` upstream.**
  Toggle-monitor's match tree cannot un-collapse alerts that AM has
  already grouped, and its per-channel rate limiter
  (`rateLimit.perChannel`) is a flood detector, not a smoother. If
  50 pods firing `HighCPU` produces 50 Slack messages, widen
  `group_by` here — most teams want `group_by: [alertname]` at
  minimum so per-pod variation collapses into one notification
  before it ever reaches the webhook.

- **Null-route `Watchdog`.** kube-prometheus-stack ships a
  `Watchdog` alert that fires every 30s as a keep-alive ("if you see
  this, Alertmanager is alive"). Route it to a `default_receiver`
  with no notifiers, or ignore it inside toggle-monitor's match tree
  (`when: { alertname: "Watchdog" }, ignore: true, final: true`).
  Both work; null-routing upstream is cheaper because the webhook
  request never crosses the network.

- **Token rotation requires restart.** The token is read once at
  process start. Rotation = regenerate via `openssl rand -hex 32`
  (see [Generating the token](#generating-the-token)), roll both
  Secrets, restart toggle-monitor (so it re-reads the env var),
  restart Alertmanager (so it re-reads the `credentials_file`). v1
  does not support a grace-period dual-token mode; that's deferred
  per ADR-0005's "out of scope" list.

- **PagerDuty severity mapping does not carry over.** The `severity`
  you set in PagerDuty's `pagerduty_configs` mapped to PD's incident
  urgency. Toggle-monitor instead surfaces severity via Slack
  mentions configured in `alertmanager.match[].config.notify` —
  operators choose `@here` / `@channel` / user mentions explicitly,
  per match-tree branch. The example above demonstrates that pattern
  via the `severity: "critical"` branch routing to `ops-critical`.

## Traefik ingress middleware

Toggle-monitor accepts request bodies up to 10 MiB. Traefik defaults
to 4 MiB, which clips realistic batches and produces
`413 Request Entity Too Large` on the AM side. Attach a buffering
middleware to the route:

```yaml
apiVersion: traefik.io/v1alpha1
kind: Middleware
metadata:
  name: toggle-monitor-large-body
  namespace: toggle-monitor
spec:
  buffering:
    maxRequestBodyBytes: 10485760   # 10 MiB
```

Attach it via `IngressRoute`:

```yaml
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: toggle-monitor
  namespace: toggle-monitor
spec:
  routes:
    - match: Host(`monitor.example.test`) && PathPrefix(`/webhooks/`)
      kind: Rule
      middlewares:
        - name: toggle-monitor-large-body
      services:
        - name: toggle-monitor
          port: 8080
```

Or, on a plain `Ingress`, via the annotation
`traefik.ingress.kubernetes.io/router.middlewares:
toggle-monitor-toggle-monitor-large-body@kubernetescrd`.

The 10 MiB cap inside toggle-monitor is hardcoded. If you genuinely
have AM batches bigger than that (very unusual — would imply 200+
alerts in one webhook call), fix `group_by` upstream first.

## Verification

After deploying, confirm the path works end-to-end. Set
`$AM_WEBHOOK_TOKEN` to the same value that's in the Secret.

1. **Send a firing payload.** Save the JSON below as `sample.json`,
   then POST it:

   ```bash
   curl -X POST \
     -H "Authorization: Bearer $AM_WEBHOOK_TOKEN" \
     -H "Content-Type: application/json" \
     --data @sample.json \
     http://toggle-monitor:8080/webhooks/alertmanager
   ```

   Expected response: `200 OK` with body
   `{"status":"ok","processed":1}`.

2. **Check `/alerts`.** Visit toggle-monitor's `/alerts` page; a row
   for the `DiskFull` fingerprint should appear with status
   `firing`.

3. **Check Slack.** Open the channel routed to by the match tree
   (e.g. `ops-alerts`). The message should land with the severity
   header, summary, and a "View details" button pointing at
   `/alert/{id}`. Click it; the detail page should render.

4. **Send a resolve.** Re-POST the same payload with `status:
   resolved` and `endsAt` populated (e.g. the current RFC3339
   timestamp). The Slack parent should get edited (status flips)
   and a thread reply should land with the firing duration.

Sample firing payload (minimal valid v4 webhook):

```json
{
  "version": "4",
  "groupKey": "{}:{alertname=\"DiskFull\"}",
  "status": "firing",
  "receiver": "toggle_monitor",
  "groupLabels": {"alertname": "DiskFull"},
  "commonLabels": {"alertname": "DiskFull", "severity": "critical"},
  "commonAnnotations": {},
  "externalURL": "https://am.prod.example.test",
  "alerts": [
    {
      "status": "firing",
      "labels": {
        "alertname": "DiskFull",
        "severity": "critical",
        "namespace": "prod",
        "instance": "node-1"
      },
      "annotations": {
        "summary": "Disk usage above 90% on node-1",
        "description": "Disk usage has been above 90% for 15 minutes…",
        "runbook_url": "https://runbooks.example.test/diskfull"
      },
      "startsAt": "2026-06-04T14:23:01Z",
      "endsAt": "0001-01-01T00:00:00Z",
      "generatorURL": "https://prom.example.test/graph?…",
      "fingerprint": "0123456789abcdef"
    }
  ]
}
```

## Troubleshooting

| Symptom | First diagnostic |
|---|---|
| AM logs show 401 on the webhook | Token mismatch. Confirm `ALERTMANAGER_WEBHOOK_TOKEN` in toggle-monitor matches the AM-side `credentials_file` byte-for-byte (trailing newlines in the file count). |
| AM logs show 413 | Traefik buffering middleware missing or set below 10 MiB. See [Traefik ingress middleware](#traefik-ingress-middleware). |
| AM webhook succeeds (`/alerts` shows the row) but no Slack message | Check toggle-monitor logs for `am.webhook.batch.partial_failure` or `am.alert.rate_limited`. Likely `rateLimit.perChannel` engaged or a Slack API outage. |
| Slack message lands but the parent does not get edited on resolve | `send_resolved: true` missing on the AM-side webhook config. |
| Duplicate Slack messages for one alert | Likely toggle-monitor crashed mid-batch; AM redelivered; the at-most-twice path re-posted (documented in ADR-0005 §"Identity & idempotency"). Acceptable as a once-per-crash event. If it's persistent, check that the partial unique index on `am_alerts(fingerprint) WHERE ended_at IS NULL` is present (migration 0013). |
| "Late-resolve" Slack messages appearing without a preceding firing | Likely a process restart between firing and resolve, OR AM's `repeat_interval` is firing a resolve for an alert whose firing happened before toggle-monitor was deployed. Cross-reference the toggle-monitor logs by `request_id` to confirm. |
| `/alerts` page empty despite confirmed AM webhooks | Token correct? Check toggle-monitor logs for 401s. Also check the match tree — an early `ignore: true` / `final: true` rule may be swallowing everything before more-specific rules lower in the tree get a chance (review the cascade semantics in [ADR-0002](./adr/0002-kube-match-tree-cascade.md), which the AM match tree mirrors). |

## See also

- [ADR-0005 — Alertmanager webhook receiver](./adr/0005-alertmanager-webhook-receiver.md) — the design.
- [config-schema.md §5c](./config-schema.md#5c-alertmanager-webhook-receiver-optional) — per-field reference.
- [config-example.yaml](./config-example.yaml) — complete example with a working match tree.
