#!/usr/bin/env bash
# Print the envtest KUBEBUILDER_ASSETS path.
#
# Sourcing pattern (inside the devtools container):
#     export KUBEBUILDER_ASSETS="$(./scripts/envtest-assets-path.sh)"
#
# Then `go test ./internal/controller/...` runs without going through
# `make envtest-run`, skipping the per-invocation `setup-envtest use`
# call (saves ~1s per invocation). Re-export after upgrading
# controller-runtime — the asset version may change.
set -euo pipefail

LOCALBIN="${LOCALBIN:-$(pwd)/bin}"
ENVTEST="$LOCALBIN/setup-envtest"
K8S_VERSION="${ENVTEST_K8S_VERSION:-1.31.0}"

if [[ ! -x "$ENVTEST" ]]; then
    echo "ERROR: $ENVTEST not found. Run 'make setup-envtest' first." >&2
    exit 1
fi

exec "$ENVTEST" use "$K8S_VERSION" --bin-dir "$LOCALBIN" -p path
