#!/usr/bin/env bash
# scripts/cluster.sh — e2e cluster lifecycle.
#
# All Helm chart installs are pinned via test/e2e/CHART_PINS.md; all tunables
# live in test/e2e/values/*.values.yaml. See test/e2e/README.md for the
# CI gate vs dev iteration workflows.

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-alitellm-operator-test}"
KIND_CONFIG="${KIND_CONFIG:-scripts/kind-config.yaml}"
VALUES_DIR="${VALUES_DIR:-test/e2e/values}"

usage() {
  cat <<'USAGE' >&2
scripts/cluster.sh — e2e cluster lifecycle.

Usage:
  scripts/cluster.sh up        # create kind + install all charts + wait Ready
  scripts/cluster.sh hydrate   # re-apply hydration on an already-up cluster
  scripts/cluster.sh sync      # alias of hydrate (parity with ../ach)
  scripts/cluster.sh verify    # health-gate standing state (no mutation)
  scripts/cluster.sh down      # delete kind cluster
  scripts/cluster.sh keep      # same as up but no EXIT trap (local iteration)
  scripts/cluster.sh status    # print kubectl get on hydration fixtures
USAGE
  exit 1
}

cmd_up()      { create_cluster; create_namespaces; install_all; apply_fixtures; cmd_verify; }
cmd_hydrate() { create_namespaces; install_all; apply_fixtures; cmd_verify; }
cmd_sync()    { cmd_hydrate; }
cmd_down()    { kind delete cluster --name "${CLUSTER_NAME}" || true; }
cmd_keep()    { cmd_up; }
cmd_status()  { print_status; }

create_cluster() {
  if kind get clusters | grep -qx "${CLUSTER_NAME}"; then
    echo "[cluster.sh] kind cluster '${CLUSTER_NAME}' already exists — skipping create"
    return 0
  fi
  echo "[cluster.sh] creating kind cluster '${CLUSTER_NAME}'..."
  kind create cluster \
    --name "${CLUSTER_NAME}" \
    --config "${KIND_CONFIG}" \
    --wait 60s
}

create_namespaces() {
  # default always pre-exists; the other five are declared in the
  # 00-namespaces kustomize phase (e2e=true labelled for `kubectl delete -l`).
  kubectl apply -k test/e2e/cluster/00-namespaces
}
install_toolhive() {
  local crds_version operator_version
  crds_version="$(awk '/^crdsChartVersion:/ {print $2}' "${VALUES_DIR}/toolhive.values.yaml")"
  operator_version="$(awk '/^operatorChartVersion:/ {print $2}' "${VALUES_DIR}/toolhive.values.yaml")"

  echo "[cluster.sh] installing toolhive-operator-crds @ ${crds_version}..."
  helm upgrade --install toolhive-operator-crds \
    oci://ghcr.io/stacklok/toolhive/toolhive-operator-crds \
    --version "${crds_version}" \
    --wait --timeout 60s

  echo "[cluster.sh] installing toolhive-operator @ ${operator_version}..."
  helm upgrade --install toolhive-operator \
    oci://ghcr.io/stacklok/toolhive/toolhive-operator \
    --version "${operator_version}" \
    -n toolhive-system \
    -f "${VALUES_DIR}/toolhive.values.yaml" \
    --wait --timeout 90s

  # Step 2.5 — add v1beta1 versions to ToolHive CRDs (not yet in published charts).
  # The OCI chart above ships only v1alpha1. This fixture (vendored from
  # stacklok/toolhive v0.28.0 @ 748a64228710ce241a225f5530022ce2c96cc23e) adds
  # v1beta1 served=true, storage=false to both MCPServer and VirtualMCPServer so
  # the operator's dual-version informer can register against both vintages.
  # The fixture contains both v1alpha1 (storage: true, preserved) and v1beta1
  # (storage: false, new). kubectl apply replaces the OCI chart's CRD with the
  # multi-version fixture; safe in an ephemeral kind cluster. Idempotent.
  echo "[cluster.sh] adding v1beta1 CRD versions (toolhive dual-vintage fixture)..."
  kubectl apply --server-side --force-conflicts --field-manager=alitellm-cluster-bootstrap -f test/e2e/fixtures/toolhive-v1beta1-crds.yaml
  echo "[cluster.sh] toolhive CRD versions after fixture: $(kubectl get crd mcpservers.toolhive.stacklok.dev -o jsonpath='{.spec.versions[*].name}' 2>/dev/null || echo 'crd-not-found')"
}

install_litellm() {
  local version image_tag image
  version="$(awk '/^chartVersion:/ {print $2}' "${VALUES_DIR}/litellm.values.yaml")"
  image_tag="$(awk '/^[[:space:]]*tag:/ {print $2; exit}' "${VALUES_DIR}/litellm.values.yaml")"
  image="ghcr.io/berriai/litellm-database:${image_tag}"

  # β: pre-pull + kind load the LiteLLM image so the migrations Job (and
  # the runtime Deployment) hit a locally-resident image. Combined with
  # image.pullPolicy=IfNotPresent in litellm.values.yaml, this eliminates
  # the ghcr.io round-trip on cold-cache that previously left the PreSync
  # Job in ImagePullBackOff → DB unmigrated → 500s on /model/new + friends.
  echo "[cluster.sh] pre-pulling ${image} on host..."
  docker pull "${image}"
  echo "[cluster.sh] kind-loading ${image} into ${CLUSTER_NAME}..."
  kind load docker-image "${image}" --name "${CLUSTER_NAME}"

  local tmpdir; tmpdir="$(mktemp -d)"
  echo "[cluster.sh] pulling litellm-helm @ ${version} into ${tmpdir}..."
  ( cd "${tmpdir}" && helm pull oci://docker.litellm.ai/berriai/litellm-helm --version "${version}" --untar )

  echo "[cluster.sh] installing litellm @ ${version}..."
  helm upgrade --install litellm "${tmpdir}/litellm-helm" \
    -n litellm-system \
    -f "${VALUES_DIR}/litellm.values.yaml" \
    --wait --timeout 240s

  # α: helm --wait covers Deployment readiness + PreSync hook completion,
  # but a Job stuck in ImagePullBackOff can leave helm to time out silently
  # while the chart reports STATUS=deployed. Re-verify the migrations Job
  # explicitly so a regression here fails loud (vs producing 500s in the
  # e2e suite ten minutes later).
  echo "[cluster.sh] verifying litellm-migrations Job Complete..."
  kubectl -n litellm-system wait --for=condition=complete \
    job/litellm-migrations --timeout=180s

  rm -rf "${tmpdir}"
}

install_mocks() {
  echo "[cluster.sh] building + loading litellm-mock:e2e..."
  make build-image-mock
  kind load docker-image litellm-mock:e2e --name "${CLUSTER_NAME}"

  echo "[cluster.sh] installing openai-mock + kubeai-mock..."
  helm upgrade --install openai-mock test/e2e/charts/mocks/ \
    -n mocks -f "${VALUES_DIR}/openai-mock.values.yaml" \
    --wait --timeout 60s
  helm upgrade --install kubeai-mock test/e2e/charts/mocks/ \
    -n mocks -f "${VALUES_DIR}/kubeai-mock.values.yaml" \
    --wait --timeout 60s
}

install_operator() {
  echo "[cluster.sh] building alitellm-operator:e2e..."
  make build-image IMG=alitellm-operator:e2e
  kind load docker-image alitellm-operator:e2e --name "${CLUSTER_NAME}"

  echo "[cluster.sh] helm install alitellm-operator..."
  helm upgrade --install alitellm-operator ./deploy/helm/alitellm-operator/ \
    -n default -f "${VALUES_DIR}/operator.values.yaml" \
    --wait --timeout 90s
}

install_all() {
  install_toolhive
  install_litellm
  install_mocks
  install_operator
}
apply_fixtures() {
  # kubectl apply -k kind-sorts core types (Secret) ahead of CRs
  # (LiteLLMConnection), preserving the secret-before-CR ordering that
  # previously needed two explicit applies — otherwise the reconciler fires
  # once against a missing Secret and flickers connection_ready 0→1.
  # The 04-hydration phase is e2e=true labelled for `kubectl delete -l` cleanup.
  #
  # FIXTURE_WAIT_TIMEOUT 180s: first reconcile after operator-up needs
  # informer caches synced + LiteLLM probed + status written; <30s warm
  # locally, 60-120s cold on GitHub runners. 3x headroom keeps CI signal
  # positive without flaking.
  kubectl apply -k test/e2e/cluster/04-hydration
  kubectl -n default wait --for=condition=Ready \
    litellmconnection/default --timeout="${FIXTURE_WAIT_TIMEOUT:-180s}"
}
print_status() (
  # Subshell scope so `set +e` does not leak to callers. All probes below are
  # informational — never fail the status command. `kubectl get ns a b c`
  # returns non-zero if ANY listed namespace is absent (common in fresh
  # clusters or partial hydration); under `set -e` that aborts print_status
  # mid-execution and the helm/CR/condition sections never render.
  set +e
  echo "== kind clusters =="
  kind get clusters
  echo
  echo "== nodes =="
  kubectl get nodes
  echo
  echo "== namespaces (e2e layout) =="
  kubectl get ns default litellm-system toolhive-system mocks dev prod 2>/dev/null
  echo
  echo "== hydration =="
  helm ls -A
  echo
  echo "== operator-managed CRs =="
  kubectl -n default get \
    litellmconnections,models,modeldiscoveries,mcpservers,mcpserverdiscoveries,a2aagents,teams \
    2>/dev/null
  echo
  echo "== conditions (one-liner) =="
  kubectl -n default get litellmconnections,models,modeldiscoveries,mcpservers,mcpserverdiscoveries,a2aagents,teams \
    -o jsonpath='{range .items[*]}{.kind}/{.metadata.name}: {.status.conditions[?(@.type=="Ready")].status}={.status.conditions[?(@.type=="Ready")].reason}{"\n"}{end}' \
    2>/dev/null
)

cmd_verify() {
  # Consolidated health gate over the STANDING state — the parity equivalent
  # of ach's verify_all. Everything below was already gated by helm --wait /
  # the apply_fixtures wait during up, so on a healthy cluster this returns
  # near-instantly; it exists so a regression fails loud HERE instead of as
  # opaque 500s inside the e2e suite minutes later. VERIFY_TIMEOUT overridable.
  local t="${VERIFY_TIMEOUT:-300s}"
  echo "[cluster.sh] verify: toolhive-operator..."
  kubectl -n toolhive-system rollout status deploy/toolhive-operator --timeout="${t}"
  echo "[cluster.sh] verify: litellm..."
  kubectl -n litellm-system  rollout status deploy/litellm           --timeout="${t}"
  echo "[cluster.sh] verify: mocks..."
  kubectl -n mocks wait --for=condition=Ready pod --all              --timeout="${t}"
  echo "[cluster.sh] verify: operator..."
  kubectl -n default rollout status deploy/alitellm-operator          --timeout="${t}"
  echo "[cluster.sh] verify: connection seam..."
  kubectl -n default wait --for=condition=Ready litellmconnection/default --timeout="${t}"
  echo "[cluster.sh] verify: OK — standing state healthy"
}

case "${1:-}" in
  up|hydrate|sync|down|keep|status|verify) "cmd_${1}" ;;
  *) usage ;;
esac
