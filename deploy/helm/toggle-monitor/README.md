# toggle-monitor Helm chart

Deploys toggle-monitor into a Kubernetes cluster. Single replica by
design (the worker is a single in-process loop) — horizontal scale-out
is deferred.

## What gets installed

- `Deployment` — the long-running `serve` loop. Annotated with
  `reloader.stakater.com/auto: "true"` so config or secret changes
  trigger a rolling restart.
- `Service` (ClusterIP) — exposes `/`, `/healthz`, `/readyz`, `/metrics`.
- `Ingress` (optional) — restrict via `nginx.ingress.kubernetes.io/whitelist-source-range`
  or your equivalent. There is **no auth** in v1.
- `ConfigMap` — the entire `config.yaml` rendered from `values.yaml`.
- `Secret` (optional inline fallback) — DB password + Slack token.
  External Secrets via `passwordSecret.name` / `tokenSecret.name` are
  strongly recommended in production.
- `ClusterRole` + `ClusterRoleBinding` — cluster-wide
  `networking.k8s.io/v1` Ingress `get/list/watch` for kube
  auto-discovery.
- `ServiceAccount`.
- `Job` (`PreSync` hook) — runs `toggle-monitor migrate` before every
  ArgoCD sync (or as a Helm pre-install/pre-upgrade hook on non-ArgoCD
  setups).
- `ServiceMonitor` (optional) — scrape `/metrics` via kube-prometheus-stack.

## Quickstart

```bash
# Render the manifests for review:
helm template my-toggle deploy/helm/toggle-monitor \
  -f deploy/helm/toggle-monitor/examples/values-prod.yaml

# Install:
helm upgrade --install toggle-monitor deploy/helm/toggle-monitor \
  --namespace monitoring \
  --create-namespace \
  -f my-values.yaml
```

## Production checklist

- [ ] Point `postgres.host` at the right CNPG service.
- [ ] Set `postgres.passwordSecret.name` to an existing Secret (do NOT use
      `postgres.password`).
- [ ] Set `slack.tokenSecret.name` similarly.
- [ ] Fill in `config.slack.channels[].channelId` with a real channel ID
      (not a DM).
- [ ] Set `config.publicBaseURL` so Slack `[View details]` buttons work.
- [ ] If using ArgoCD, leave `migrate.argoCDPreSyncHook: true` — the
      migrate Job will run before each sync.
- [ ] Restrict the UI ingress to internal networks (no auth in v1).
- [ ] Enable `serviceMonitor.enabled` if you run kube-prometheus-stack.

## Customizing the YAML config

The whole `config:` block in `values.yaml` is rendered into the
ConfigMap verbatim. Edit there — not in the ConfigMap.

Secret references in the YAML use `passwordEnv: DB_PASSWORD` /
`tokenEnv: SLACK_BOT_TOKEN`. Those env vars are populated by the
Deployment from the configured Secrets.

## Troubleshooting

- **Pod stuck Pending on migrate Job**: PreSync hook hasn't completed
  yet. Check `kubectl logs -n <ns> job/<release>-migrate-<rev>`.
- **App pod CrashLoopBackOff with "schema at version N; binary expects M"**:
  migrations didn't run. Re-trigger the Job manually or run
  `kubectl exec` with `toggle-monitor migrate`.
- **Slack workspace check warning in /readyz**: surface — readyz still
  returns 200 because `auth.test` failures are non-fatal. Investigate
  the Slack token.
- **Ingresses not appearing in /discovery**: the ClusterRole only
  applies if `rbac.ingressWatch: true` (default). Without it, the
  watcher can't list Ingress resources.
