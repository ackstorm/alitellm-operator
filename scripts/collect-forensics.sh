#!/usr/bin/env bash
#
# scripts/collect-forensics.sh — capture e2e failure-path forensics.
#
# Invoked from CI on test failure (.github/workflows/ci.yml e2e job's
# `if: failure()` step) and collectable manually with
#   ./scripts/dev.sh bash scripts/collect-forensics.sh
# after a local make e2e-run / make e2e-full red run.
#
# Best-effort — never fails the surrounding step. Designed to run BEFORE
# cluster-down so kubectl logs still resolves; the workflow runs
# `cluster-down` after this with `if: always()`.

set -uo pipefail

OUT_DIR=/tmp/e2e-collected
mkdir -p "${OUT_DIR}"

# Stray suite-level logs the test harness writes locally (no-ops in CI).
cp /tmp/e2e-*.log "${OUT_DIR}/" 2>/dev/null || true

# Operator deployment is named `alitellm-operator` (as of 2026-05-22 —
# previously `alitellm-operator-controller-manager` under the kubebuilder
# scaffolding default; renamed when the kustomize namePrefix was
# dropped). LiteLLM deployment name is `litellm`.
kubectl -n default logs deploy/alitellm-operator \
  --tail=2000 --all-containers=true > "${OUT_DIR}/operator-full.log" 2>&1 || true

kubectl -n litellm-system logs deploy/litellm \
  --tail=2000 --all-containers=true > "${OUT_DIR}/litellm-full.log" 2>&1 || true

# Migration Job logs surface DB-connectivity / Prisma issues (the silent
# exit-0 failure mode that wedged 9.15 from-clean runs before α/β fix).
kubectl -n litellm-system logs job/litellm-migrations \
  --all-containers=true > "${OUT_DIR}/litellm-migrations.log" 2>&1 || true

# Full cluster snapshot for cross-cutting incidents.
./scripts/cluster.sh status > "${OUT_DIR}/cluster-status.txt" 2>&1 || true

# CR dump for stuck-finalizer / status-condition postmortems.
kubectl -n default get \
  litellmconnections,models,modeldiscoveries,mcpservers,mcpserverdiscoveries,a2aagents,teams \
  -o yaml > "${OUT_DIR}/default-crs.yaml" 2>&1 || true

# Recent events across the namespaces involved in e2e.
for ns in default litellm-system toolhive-system mocks dev; do
  kubectl -n "${ns}" get events --sort-by=.lastTimestamp \
    > "${OUT_DIR}/events-${ns}.txt" 2>&1 || true
done

echo "Forensics collected to ${OUT_DIR}/"
ls -la "${OUT_DIR}/"
