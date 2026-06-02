#!/usr/bin/env bash
# Install git hooks for alitellm-operator.
#
# Installs:
#   pre-push   -> scripts/pre-push-check.sh
#     Runs the full 17-gate pre-publication check before every `git push`.
#     Includes gitleaks/trufflehog/SPDX/govulncheck plus the defensive
#     full-sweep golangci-lint + make test-unit.
#
# Idempotent — safe to re-run.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || {
  echo "not inside a git repo" >&2; exit 1;
}
cd "$REPO_ROOT"

HOOKS_DIR="$(git rev-parse --git-path hooks)"
mkdir -p "$HOOKS_DIR"

install_hook() {
  local name="$1" target="$2"
  local hook_path="${HOOKS_DIR}/${name}"
  if [[ ! -x "scripts/${target##*/}" ]]; then
    echo "scripts/${target##*/} missing or not executable" >&2
    return 1
  fi
  # Backup any prior non-symlink hook.
  if [[ -e "$hook_path" && ! -L "$hook_path" ]]; then
    local backup="${hook_path}.bak.$(date -u +%Y%m%dT%H%M%SZ)"
    mv "$hook_path" "$backup"
    echo "backed up existing $hook_path -> $backup"
  fi
  ln -sf "$target" "$hook_path"
  if [[ -L "$hook_path" ]]; then
    echo "installed: $hook_path -> $(readlink "$hook_path")"
  else
    echo "failed to install $hook_path" >&2
    return 1
  fi
}

# Remove any stale pre-commit hook from a prior install (pre-commit gate retired).
stale_pc="${HOOKS_DIR}/pre-commit"
if [[ -L "$stale_pc" ]]; then
  rm -f "$stale_pc"
  echo "removed stale pre-commit hook: $stale_pc"
fi

install_hook pre-push   "../../scripts/pre-push-check.sh"
