#!/usr/bin/env bash
# Install git hooks for alitellm-operator.
#
# Currently installs:
#   pre-push -> scripts/pre-push-check.sh
#     Runs the full 15-gate pre-publication check before every `git push`.
#     Idempotent — safe to re-run.
#
# Usage: ./scripts/install-hooks.sh
#        (or via `make hooks`)
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || {
  echo "not inside a git repo" >&2; exit 1;
}
cd "$REPO_ROOT"

HOOKS_DIR="$(git rev-parse --git-path hooks)"
HOOK_PATH="${HOOKS_DIR}/pre-push"
TARGET="../../scripts/pre-push-check.sh"

if [[ ! -x scripts/pre-push-check.sh ]]; then
  echo "scripts/pre-push-check.sh missing or not executable" >&2
  exit 1
fi

mkdir -p "$HOOKS_DIR"

# Replace any existing pre-push (back up if non-symlink + non-empty).
if [[ -e "$HOOK_PATH" && ! -L "$HOOK_PATH" ]]; then
  BACKUP="${HOOK_PATH}.bak.$(date -u +%Y%m%dT%H%M%SZ)"
  mv "$HOOK_PATH" "$BACKUP"
  echo "backed up existing $HOOK_PATH -> $BACKUP"
fi

ln -sf "$TARGET" "$HOOK_PATH"

if [[ -L "$HOOK_PATH" ]]; then
  echo "installed: $HOOK_PATH -> $(readlink "$HOOK_PATH")"
else
  echo "failed to install $HOOK_PATH" >&2
  exit 1
fi
