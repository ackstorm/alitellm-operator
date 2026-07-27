#!/usr/bin/env bash
# Toggle a mock instance's auth mode via /__mock/auth-mode.
#
# Usage: scripts/mock-set-mode.sh <instance> <accept|reject-401>
#
# Plan amendment A1: mock image is distroless (gcr.io/distroless/static-debian12);
# has no shell and no wget. Instead of `kubectl exec ... -- wget`, we run a
# short-lived curlimages/curl Pod in the mocks namespace and POST to the
# in-cluster Service.

set -euo pipefail

instance="${1:?instance required (openai-mock|kubeai-mock)}"
mode="${2:?mode required (accept|reject-401)}"

case "${mode}" in
  accept|reject-401) ;;
  *) echo "mode must be accept or reject-401, got: ${mode}" >&2; exit 2 ;;
esac

# This script CREATES a Pod, so it must never be pointed at the wrong cluster.
# A bare `kubectl` resolves the host's default context, which on a developer
# machine is routinely a real production cluster. Route through the devtools
# container, whose kubeconfig only ever knows the ephemeral kind cluster, so a
# misfire fails closed instead of mutating prod. Override with KUBECTL= when
# already inside the container (dev.sh sets LITELLM_IN_DEVTOOLS=1).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ -n "${KUBECTL:-}" ]]; then
  read -r -a KUBECTL_CMD <<<"${KUBECTL}"
elif [[ -n "${LITELLM_IN_DEVTOOLS:-}" ]]; then
  KUBECTL_CMD=(kubectl)
else
  KUBECTL_CMD=("${SCRIPT_DIR}/dev.sh" kubectl)
fi

pod="mock-mode-poke-$$"
"${KUBECTL_CMD[@]}" -n mocks run "${pod}" --rm -i --restart=Never --quiet \
  --image=curlimages/curl:8.10.1 -- \
  curl -sS -X POST -H "Content-Type: application/json" \
    --data "{\"mode\":\"${mode}\"}" \
    "http://${instance}.mocks.svc.cluster.local:8080/__mock/auth-mode"

echo "[mock-mode] ${instance} -> ${mode}"
