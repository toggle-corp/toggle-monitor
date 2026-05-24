# toggle-monitor Helm chart

Deploys toggle-monitor into a Kubernetes cluster. Single replica by
design (the worker is a single in-process loop) — horizontal scale-out
is deferred.

## What gets installed

- `Deployment` — the long-running `serve` loop. Pod template carries a
  `checksum/config` annotation when the chart owns the ConfigMap, so any
  `helm upgrade` that changes the inline config triggers a rolling
  restart automatically.
- `Service` (ClusterIP) — exposes `/`, `/healthz`, `/readyz`, `/metrics`.
- `Ingress` (optional) — restrict via
  `nginx.ingress.kubernetes.io/whitelist-source-range` or your
  equivalent. There is **no auth** in v1.
- `ConfigMap` (when `config.existingConfigMap.name` is empty) — the
  entire `config.inline:` block rendered through `toYaml`. If set, the
  chart mounts the named CM instead and skips this resource.
- `ClusterRole` + `ClusterRoleBinding` — cluster-wide
  `networking.k8s.io/v1` Ingress `get/list/watch` for kube
  auto-discovery (toggle via `rbac.ingressWatch`).
- `ServiceAccount`.
- `Job` (`PreSync` hook by default) — runs `toggle-monitor migrate`
  before every ArgoCD sync, or as a Helm `pre-install` / `pre-upgrade`
  hook on non-ArgoCD setups (`migrate.argoCDPreSyncHook: false`).
- `ServiceMonitor` (optional) — scrape `/metrics` via
  kube-prometheus-stack.

The chart **never creates Secrets**. DB passwords, Slack tokens, proxy
passwords, and any other secret-shaped values must exist as Secrets
before `helm install` — see "Secrets" below.

## Two config-provisioning paths

| Path | When to use | Tradeoff |
|---|---|---|
| `config.inline:` (default) | Quickstart, simple configs | Anchors expand and `!override` tags + comments do NOT survive `toYaml` |
| `config.existingConfigMap.name:` | Production, complex configs | Full YAML fidelity; operator owns the CM |

If your config uses `!override` (typically in `kube.match[].config.notify`
or any other array you want to *replace* rather than union down the
cascade), you **must** use the `existingConfigMap` path — see
`examples/values-external-cm.yaml`.

## Quickstart

Create the Secrets the chart will bind:

```bash
kubectl -n monitoring create secret generic toggle-monitor-db \
  --from-literal=password='YOUR_DB_PASSWORD'

kubectl -n monitoring create secret generic toggle-monitor-slack \
  --from-literal=bot-token='xoxb-...'
```

Install, overriding the REPLACE_ME entries inline:

```bash
helm upgrade --install toggle-monitor deploy/helm/toggle-monitor \
  --namespace monitoring --create-namespace \
  --set "envFromSecrets[0].name=DB_PASSWORD" \
  --set "envFromSecrets[0].secret.name=toggle-monitor-db" \
  --set "envFromSecrets[0].secret.key=password" \
  --set "envFromSecrets[1].name=SLACK_BOT_TOKEN" \
  --set "envFromSecrets[1].secret.name=toggle-monitor-slack" \
  --set "envFromSecrets[1].secret.key=bot-token" \
  --set "config.inline.database.host=YOUR_PG_HOST" \
  --set "config.inline.slack.channels[0].slug=ops-alerts" \
  --set "config.inline.slack.channels[0].channelId=C0123ABCD" \
  --set "config.inline.slack.channels[0].tokenEnv=SLACK_BOT_TOKEN"
```

In practice, copy `values.yaml` to `my-values.yaml`, edit, and
`-f my-values.yaml` — that's what the two patterns below do.

## Production patterns

### CNPG-backed Postgres

`examples/values-cnpg.yaml` shows the full mapping. CNPG generates a
Secret containing `password`, `host`, `port`, `username`, `dbname`,
etc. The chart binds all those keys to env vars, and the binary's
`${VAR}` interpolation plugs the non-secret ones into
`config.inline.database.*` at parse time. The password flows through
`passwordEnv: DB_PASSWORD` so the binary wraps it in `SecretString`
and masks it in logs.

```bash
helm upgrade --install toggle-monitor deploy/helm/toggle-monitor \
  --namespace monitoring --create-namespace \
  -f deploy/helm/toggle-monitor/examples/values-cnpg.yaml
```

Edit the file first to replace `my-cnpg-cluster-app` with your actual
CNPG cluster name and `my-slack-secret` with your Slack-token Secret.

### Externally-managed ConfigMap (GitOps)

`examples/values-external-cm.yaml` is the pattern when the config
file lives in a separate Git repo (Argo, Flux, Kustomize), or when
the config relies on YAML anchors / `!override` / inline comments.

You create the ConfigMap and Secret yourself; the chart mounts and
reads them:

```bash
kubectl -n monitoring create configmap toggle-monitor-config \
  --from-file=config.yaml=./my-config.yaml

# ExternalSecret / SealedSecret resources go here too — the example
# file's header sketches an ExternalSecret that pulls a CNPG password
# into the Secret the chart binds.

helm upgrade --install toggle-monitor deploy/helm/toggle-monitor \
  --namespace monitoring --create-namespace \
  -f deploy/helm/toggle-monitor/examples/values-external-cm.yaml
```

## Secrets

Every env var the binary reads (`database.passwordEnv`,
`slack.channels[].tokenEnv`, `proxies[].passwordEnv`, and anything
referenced via `${VAR}` interpolation) is bound through
`envFromSecrets:`:

```yaml
envFromSecrets:
  - name: DB_PASSWORD                # env var name on the pod
    secret:
      name: toggle-monitor-db        # Secret resource that must exist
      key: password                  # key within that Secret
  - name: SLACK_BOT_TOKEN
    secret:
      name: toggle-monitor-slack
      key: bot-token
```

Both the serve Deployment and the migrate Job receive every entry —
no per-pod scoping. Slack tokens on the migrate Job pod are unused at
runtime but don't represent a meaningful leak surface beyond the
Secret being referenced from another pod anyway.

## Picking up changes

| Change kind | How it's detected |
|---|---|
| Edit to `values.yaml` `config.inline:` | `checksum/config` annotation on the Deployment pod template differs after `helm upgrade` → rolling restart |
| Edit to an externally-managed ConfigMap | **Not detected by the chart.** Requires Stakater Reloader (`reloader.enabled: true`) OR a manual `kubectl rollout restart`. |
| Secret rotation (any Secret bound via `envFromSecrets`) | **Not detected by the chart.** Same options as above. |

Stakater Reloader is **opt-in**: set `reloader.enabled: true` only if
you've installed Reloader cluster-wide and want it to watch the
Deployment's referenced ConfigMaps and Secrets.

## Production checklist

- [ ] Create every Secret named in `envFromSecrets` ahead of install.
- [ ] If using CNPG: point `envFromSecrets` at the CNPG-generated
      Secret (`<cluster>-app`); use `${VAR}` in `config.inline.database`.
- [ ] If using anchors / `!override` / comments: switch to
      `config.existingConfigMap.name`.
- [ ] Set `config.inline.publicBaseURL` (or your external config's
      `publicBaseURL`) so Slack `[View details]` buttons work.
- [ ] If using ArgoCD, leave `migrate.argoCDPreSyncHook: true`.
- [ ] Restrict the UI ingress to internal networks (no auth in v1).
- [ ] Enable `serviceMonitor.enabled` if you run kube-prometheus-stack.
- [ ] If Secret rotation matters, install Stakater Reloader and set
      `reloader.enabled: true`.

## Troubleshooting

- **Pod stuck Pending on migrate Job**: PreSync hook hasn't completed
  yet. Check `kubectl logs -n <ns> job/<release>-migrate-<rev>`.
- **App pod CrashLoopBackOff with "schema at version N; binary expects M"**:
  migrations didn't run. Re-trigger the Job manually, or
  `kubectl exec` into a pod and run `toggle-monitor migrate`.
- **Validation error referring to `${VAR}`**: the env var named by
  the failing field isn't bound by `envFromSecrets`, or the Secret /
  key it points at doesn't exist. The error includes the field path
  and line number.
- **Slack workspace check warning in /readyz**: surface — readyz still
  returns 200 because `auth.test` failures are non-fatal. Investigate
  the Slack token.
- **Ingresses not appearing in /discovery**: the ClusterRole only
  applies if `rbac.ingressWatch: true` (default). Without it, the
  watcher can't list Ingress resources.
- **`helm upgrade` doesn't roll the pod after a config edit**: either
  you're on the `existingConfigMap` path (no chart-side checksum;
  needs Reloader or a manual rollout) or the values change didn't
  reach the rendered ConfigMap (`helm get manifest` to verify).
