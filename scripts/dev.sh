#!/usr/bin/env bash
#
# scripts/dev.sh — run a command inside the devtools container.
#
# Host has no Go toolchain. All `go`, `kubebuilder`, `controller-gen`,
# `kustomize`, `setup-envtest`, `make`, etc. invocations must go through
# this wrapper.
#
# The wrapper:
#   * mounts the repo at /workspace (read-write, host UID:GID preserved)
#   * mounts /var/run/docker.sock so the container can drive the host
#     Docker daemon (plan 01-01 spawns the spike's docker-compose stack)
#   * adds the docker group so the in-container user can write to the socket
#   * persists Go module and build caches under .gocache/ for fast reruns
#   * resolves the envtest assets path (KUBEBUILDER_ASSETS)
#
# Usage:
#   ./scripts/dev.sh go build ./...
#   ./scripts/dev.sh kubebuilder init --domain ackstorm.ai --multigroup
#   ./scripts/dev.sh make gen-manifests
#   ./scripts/dev.sh bash         # interactive shell inside the container

set -euo pipefail

IMAGE="${LITELLM_DEVTOOLS_IMAGE:-litellm-devtools:latest}"
WORKSPACE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# If we are already inside the devtools container, run the command directly.
# Auto-wrapping make targets (Makefile `container_target` macro) re-invoke
# ./scripts/dev.sh from within; this prevents nested containers.
if [[ "${LITELLM_IN_DEVTOOLS:-0}" == "1" ]]; then
    exec "${@:-bash}"
fi

# Host requirement: docker only. Fail fast with a clear message rather than
# letting a downstream `kind`/`helm` call die cryptically.
if ! docker info >/dev/null 2>&1; then
    echo "scripts/dev.sh: docker daemon not reachable — is Docker running and is your user in the docker group?" >&2
    exit 1
fi

# Persisted caches (gitignored). Pre-create so docker doesn't mkdir them as root.
mkdir -p "${WORKSPACE}/.gocache/gopath" \
         "${WORKSPACE}/.gocache/build" \
         "${WORKSPACE}/.gocache/envtest" \
         "${WORKSPACE}/.gocache/kube"

DOCKER_GID="$(getent group docker 2>/dev/null | cut -d: -f3 || true)"
DOCKER_GROUP_ADD=()
if [[ -n "${DOCKER_GID}" ]]; then
    DOCKER_GROUP_ADD=(--group-add "${DOCKER_GID}")
fi

# TTY only if stdin is a terminal — keeps CI / non-interactive callers working.
TTY_ARGS=()
if [[ -t 0 && -t 1 ]]; then
    TTY_ARGS=(-it)
fi

# Build image on first use (or when forced).
if [[ "${LITELLM_DEVTOOLS_REBUILD:-0}" = "1" ]] || \
   ! docker image inspect "${IMAGE}" >/dev/null 2>&1; then
    echo "scripts/dev.sh: building ${IMAGE} (first run or rebuild requested)" >&2
    docker build -t "${IMAGE}" -f "${WORKSPACE}/Dockerfile.devtools" "${WORKSPACE}"
fi

# Worktree gitdir mount: when WORKSPACE is a git worktree, its `.git` is a
# FILE (`gitdir: /path/to/main/.git/worktrees/agent-XXX`), not a directory.
# The referenced gitdir lives OUTSIDE the worktree, so without a second mount
# any `git` invocation inside the container fatals with "not a git repository".
# Detect the worktree case and also bind-mount the main repo's `.git/` dir at
# the SAME absolute path the `.git` file points at.
WORKTREE_GIT_MOUNT=()
if [[ -f "${WORKSPACE}/.git" ]]; then
    GITDIR_PATH="$(awk '/^gitdir: /{print $2; exit}' "${WORKSPACE}/.git")"
    if [[ -n "${GITDIR_PATH}" ]]; then
        MAIN_GIT_DIR="$(dirname "$(dirname "${GITDIR_PATH}")")"
        if [[ -d "${MAIN_GIT_DIR}" ]]; then
            WORKTREE_GIT_MOUNT=(-v "${MAIN_GIT_DIR}:${MAIN_GIT_DIR}:ro")
        fi
    fi
fi

# Default command: drop into bash if no args.
if [[ $# -eq 0 ]]; then
    set -- bash
fi

exec docker run --rm "${TTY_ARGS[@]}" \
    --user "$(id -u):$(id -g)" \
    "${DOCKER_GROUP_ADD[@]}" \
    --add-host=host.docker.internal:host-gateway \
    --network=host \
    -v "${WORKSPACE}:/workspace" \
    "${WORKTREE_GIT_MOUNT[@]}" \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -e LITELLM_IN_DEVTOOLS=1 \
    -e DELETE_ON_FAILURE="${DELETE_ON_FAILURE:-0}" \
    -e HOME=/workspace/.gocache \
    -e GOPATH=/workspace/.gocache/gopath \
    -e GOCACHE=/workspace/.gocache/build \
    -e GOMODCACHE=/workspace/.gocache/gopath/pkg/mod \
    `# ENVTEST_BIN_DIR intentionally NOT overridden: inherit the image's` \
    `# ENV ENVTEST_BIN_DIR=/opt/envtest so make uses the pre-baked envtest` \
    `# assets (Makefile ENVTEST_ASSET_DIR) instead of re-downloading.` \
    -e KUBECONFIG=/workspace/.gocache/kube/config \
    -e HOST_PWD="${WORKSPACE}" \
    -e LITELLM_SPIKE_URL="${LITELLM_SPIKE_URL:-http://localhost:4000}" \
    -e LITELLM_SPIKE_MASTER_KEY="${LITELLM_SPIKE_MASTER_KEY:-}" \
    -e LITELLM_VERSION="${LITELLM_VERSION:-}" \
    -w /workspace \
    "${IMAGE}" \
    "$@"
