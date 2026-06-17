# Installation

Install `alitellm-operator` into a Kubernetes cluster. The operator
manages an EXISTING LiteLLM proxy — it does NOT deploy LiteLLM itself.

## Prerequisites

- **Kubernetes >= 1.27** with `kubectl` configured.
- **Helm 3.13+** (for the OCI install path).
- A running **LiteLLM 1.83.10** (or compatible) proxy reachable from
  the cluster at a known Service DNS address.
- A Kubernetes **Secret** in the watch namespace holding the LiteLLM
  master key (`sk-...`).

Source builds additionally need: Docker (for the devtools container),
Go 1.24.13, kubebuilder v4.4.0, controller-runtime v0.19.4. See
[Development](../developer-guide/development.md) — the host needs only
Docker; the devtools container ships the rest.

## Helm — OCI registry (recommended)

The command below installs the latest published chart. For
reproducible deploys, pin `--version <X.Y.Z>` to a release listed on
the [Releases page](https://github.com/ackstorm/alitellm-operator/releases).

```bash
helm install alitellm-operator \
  oci://ghcr.io/ackstorm/charts/alitellm-operator \
  --namespace litellm --create-namespace
```

`helm upgrade` is the standard upgrade path; the chart bundles the CRDs
under `templates/` (with `helm.sh/resource-policy: keep`) so upgrades
roll CRDs forward and `helm uninstall` preserves any CRs you authored.

## Kustomize — local clone (alternative)

```bash
git clone https://github.com/ackstorm/alitellm-operator.git
cd alitellm-operator
kubectl --namespace litellm apply -k config/default
```

## Verify

```bash
kubectl -n litellm get pods
# alitellm-operator-... 1/1 Running
kubectl -n litellm get crd | grep litellm.ackstorm.ai
```

## Helm values

The chart exposes a deliberately small surface. Defaults are
production-suitable for a small dogfood cluster.

| Value                          | Default                                       | Notes                                                                                          |
|--------------------------------|-----------------------------------------------|------------------------------------------------------------------------------------------------|
| `installCRDs`                  | `true`                                        | Set `false` when CRDs are managed out-of-band (Flux/ArgoCD CRD reconciler).                    |
| `image.repo`                   | `ghcr.io/ackstorm/alitellm-operator`          | Container image.                                                                               |
| `image.tag`                    | matches chart `appVersion`                    | Auto-bumped by release CI; see [Releases](https://github.com/ackstorm/alitellm-operator/releases). |
| `image.pullPolicy`             | `IfNotPresent`                                |                                                                                                |
| `watchNamespace`               | `""` → install namespace                      | Single namespace the operator reconciles; CRs elsewhere are ignored (SCOPE-04). Empty defaults to the install namespace (`.Release.Namespace`) so RBAC, the leader-election lease, and `LiteLLMConnection/default` stay co-located. Set explicitly only to watch a *different* namespace — you must then provision matching RBAC there (this chart binds RBAC in the install namespace). |
| `toolhive.enabled`             | `true`                                        | Enables the ClusterRole granting read on ToolHive `MCPServer` / `VirtualMCPServer`.            |
| `metrics.serviceMonitor.enabled` | `false`                                     | Set `true` when prometheus-operator is installed; renders the ServiceMonitor stub.             |
| `safetyRelistInterval`         | `""` (operator default `10m`)                 | Per-reconciler vanish-probe cadence. Maps to `LITELLM_OPERATOR_SAFETY_RELIST_INTERVAL`. Floor 5s. |
| `extraEnv`                     | `[]`                                          | Extra `{name, value}` env entries on the manager container.                                    |
| `resources`                    | unset → kustomize defaults                    | Manager container CPU / memory.                                                                |

Other knobs (replicas, nodeSelector, tolerations, leader-election, …)
are kustomize-locked. To expose more, edit
`scripts/kustomize-to-helm.sh` + `values.yaml` and run `make helm-sync`.

Example `values.yaml` (omit `image.tag` to use the chart's bundled
default, which matches `appVersion`):

```yaml
# watchNamespace omitted → operator watches its install namespace.
# Install into the namespace you want watched: `helm install -n litellm ...`.
toolhive:
  enabled: false           # cluster has no ToolHive CRDs
safetyRelistInterval: 30m  # large CR catalogue, prefer fewer LiteLLM probes
extraEnv:
  - name: LITELLM_OPERATOR_LOG_LEVEL
    value: debug
```

## Environment variables (manager container)

The operator reads runtime config from a small env-var surface:

| Variable                                    | Default     | Description                                                                                            |
|---------------------------------------------|-------------|--------------------------------------------------------------------------------------------------------|
| `WATCH_NAMESPACE`                           | `default` (raw manifest); Helm sets it from `watchNamespace`, which defaults to the install namespace | Single namespace the operator reconciles. Also pins the leader-election lease and `LiteLLMConnection/default`. |
| `LITELLM_OPERATOR_SAFETY_RELIST_INTERVAL`   | unset → `10m` | Vanish-probe cadence per reconciler kind. Floor `5s`; sub-floor values are rejected at startup.       |
| `METRICS_BIND_ADDRESS`                      | `:8080`     | Prometheus metrics listener.                                                                           |
| `HEALTH_PROBE_BIND_ADDRESS`                 | `:8081`     | controller-runtime healthz / readyz.                                                                   |

The operator does NOT read `LITELLM_BASE_URL` / `LITELLM_API_KEY` —
LiteLLM connectivity is declared via the `LiteLLMConnection/default`
CR + its `masterKeySecretRef`.

## Upgrading

```bash
helm upgrade alitellm-operator \
  oci://ghcr.io/ackstorm/charts/alitellm-operator \
  --version <new-version> -n <namespace>
```

CRDs roll forward automatically (since v0.3.2). Verify a new schema
landed with e.g.:

```bash
kubectl get crd litellmmcpserverdiscoveries.litellm.ackstorm.ai \
  -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.properties.spec.required}'
# Expected: ["prefix","refresh","toolhive","type"]  (v0.3.0+)
```

### Migrating from pre-v0.3.2 installs

Pre-v0.3.2 installs have CRDs that Helm does NOT own. The first
upgrade to v0.3.2+ errors with `apiextensions.k8s.io/v1, Kind=CustomResourceDefinition "..." exists and cannot be imported into the current release: invalid ownership metadata`.

One-time adoption:

```bash
for crd in $(kubectl get crd -o name | grep '.litellm.ackstorm.ai$'); do
  kubectl annotate $crd \
    meta.helm.sh/release-name=alitellm-operator \
    meta.helm.sh/release-namespace=<release-ns> --overwrite
  kubectl label $crd app.kubernetes.io/managed-by=Helm --overwrite
done

helm upgrade alitellm-operator \
  oci://ghcr.io/ackstorm/charts/alitellm-operator \
  --version <X.Y.Z> -n <release-ns>
```

For GitOps users (Flux, ArgoCD), reconcile
`deploy/helm/alitellm-operator/crds/` via a separate
Kustomization/Application that runs ahead of the chart release.

## Troubleshooting

**Pod CrashLoopBackOff at startup with `invalid safety-relist interval override`**

`safetyRelistInterval` is below the 5s floor. Raise it or unset to
use the 10m default.

**CRs stay `Ready=False, reason=LiteLLMUnavailable`**

`LiteLLMConnection/default` is not `Ready=True`. Check:

```bash
kubectl get litellmconnection default -o jsonpath='{.status.conditions[?(@.type=="Ready")]}{"\n"}'
```

Common causes: master-key Secret missing in watch namespace, wrong
`spec.endpoint`, network policy blocking the operator → LiteLLM path.

**MCPServerDiscovery `Ready=False, reason=SourceUnreachable`**

ToolHive CRDs (`toolhive.stacklok.dev/v1beta1`) are absent. Either
install ToolHive or remove the discovery CR — the lazy informer
converges automatically when ToolHive lands.

**Logs / events**

```bash
kubectl -n litellm logs deploy/alitellm-operator -f
kubectl -n <watch-ns> get events --sort-by=.lastTimestamp | tail -20
```

For deeper investigation, see
[Architecture](../developer-guide/architecture.md).

## Next Steps

- [Quick Start](quickstart.md) — your first Connection + Team + Model.
- [User Guide](../user-guide/index.md) — per-CR reference.
- [API Reference](../api-reference/litellm.ackstorm.ai.md) — full schema.
