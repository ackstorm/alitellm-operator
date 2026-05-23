# Installation

This guide will help you install the alitellm-operator in your Kubernetes cluster.

## Prerequisites

Before installing the operator, ensure you have:

- **Kubernetes v1.11.3+** - A running Kubernetes cluster
- **kubectl v1.11.3+** - Configured to access your cluster
- **Go v1.22.0+** - For building from source (optional)
- **Docker v17.03+** - For building container images (optional)

## Quick Installation

### 1. Install the operator

#### Helm

```bash
helm install --namespace litellm alitellm-operator oci://ghcr.io/ackstorm/charts/alitellm-operator:<version>
```

#### Kustomize

```bash
kubectl --namespace litellm apply -k config/default
```

### 2. Verify Installation

Check that the operator is running:

```bash
kubectl get pods --namespace litellm
```

You should see the operator pod in `Running` status.

## Installation from Source

### 1. Clone the Repository

```bash
git clone https://github.com/ackstorm/alitellm-operator.git
cd alitellm-operator
```

### 2. Build and Push Image

```bash
make docker-build docker-push IMG=<your-registry>/alitellm-operator:tag
```

### 3. Install CRDs

```bash
make install
```

### 4. Deploy Operator

```bash
make deploy IMG=<your-registry>/alitellm-operator:tag
```

## Configuration

### Environment Variables

The operator supports the following environment variables:

| Variable | Description | Default |
|----------|-------------|---------|
| `LITELLM_BASE_URL` | Base URL for LiteLLM API | `http://litellm:4000` |
| `LITELLM_API_KEY` | API key for LiteLLM authentication | Required |
| `LITELLM_URL_OVERRIDE` | Overrides the base URL for LiteLLM instances (internal service URL) | None |
| `METRICS_BIND_ADDRESS` | Address for metrics server | `:8443` |
| `HEALTH_PROBE_BIND_ADDRESS` | Address for health probes | `:8081` |

### Helm Configuration

When using Helm, you can set the URL override in your `values.yaml`:

```yaml
controllerManager:
  manager:
    litellmUrlOverride: "http://custom-litellm-service:4000"
```

## Upgrading

`helm upgrade` upgrades the CRDs along with everything else — no manual
`kubectl apply -f crds/` step is required:

```bash
helm upgrade alitellm-operator oci://ghcr.io/ackstorm/charts/alitellm-operator \
    --version <X.Y.Z> -n <namespace>
```

How it works (since v0.3.2):

- CRDs ship under `templates/` (rendered through Helm's normal template
  pipeline), NOT under the legacy `crds/` directory (which Helm treats
  as install-only).
- Each CRD carries `helm.sh/resource-policy: keep`, so `helm uninstall`
  leaves CRDs (and any CRs the user created) intact.
- The `installCRDs` value (default `true`) gates the bundle. Set
  `installCRDs: false` when CRDs are managed out-of-band (Flux/ArgoCD
  Kustomization, kubectl apply in a CD pipeline, etc.) to avoid two
  owners for the same object.

Verify the new schema landed:

```bash
kubectl get crd litellmmcpserverdiscoveries.litellm.ackstorm.ai \
    -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.properties.spec.required}'
# Expected: ["prefix","refresh","toolhive","type"]  (v0.3.0+)
```

### Migrating an existing install (pre-v0.3.2)

Existing v0.3.0 / v0.3.1 installs have CRDs in the cluster that Helm
does NOT own (they were applied by the install-only `crds/` path). The
first `helm upgrade` to v0.3.2+ has to take ownership of those objects
or it will error with `apiextensions.k8s.io/v1, Kind=CustomResourceDefinition "..." in namespace "" exists and cannot be imported into the current release: invalid ownership metadata`.

One-time adoption:

```bash
# Adopt every CRD into the release's ownership before the upgrade.
for crd in $(kubectl get crd -o name | grep '.litellm.ackstorm.ai$'); do
  kubectl annotate $crd \
      meta.helm.sh/release-name=alitellm-operator \
      meta.helm.sh/release-namespace=<release-ns> --overwrite
  kubectl label $crd app.kubernetes.io/managed-by=Helm --overwrite
done

helm upgrade alitellm-operator oci://ghcr.io/ackstorm/charts/alitellm-operator \
    --version <X.Y.Z> -n <release-ns>
```

After this one-time dance, future upgrades are plain `helm upgrade`.

For GitOps users (Flux, ArgoCD) we recommend a separate `Kustomization` /
`Application` for `deploy/helm/alitellm-operator/crds/` that reconciles
ahead of the chart release, so CRD upgrades are part of the declared state.

## Troubleshooting

### Common Issues

**Permission Denied Errors**
- Ensure you have cluster-admin privileges
- Check RBAC configuration

**Image Pull Errors**
- Verify the image registry is accessible
- Check image tag and repository URL

**CRD Installation Failures**
- Ensure you have permission to create CRDs
- Check for existing CRDs that might conflict

### Getting Help

- View operator logs: `kubectl logs -n litellm deployment/alitellm-operator`
- Submit an issue on [GitHub](https://github.com/ackstorm/alitellm-operator/issues/new/choose)

## Next Steps

Once installed, proceed to the [Quick Start Guide](quickstart.md) to create your first resources.
