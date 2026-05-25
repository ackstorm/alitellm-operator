#!/usr/bin/env bash
# Pre-commit local-only fast gate.
#
# Runs on every `git commit` (when installed via `make hooks`). Fails the
# commit if either gate trips. Goal: catch the classes of errors that a
# CI lint job would otherwise reject minutes later, while staying fast
# enough not to feel intrusive.
#
# Gates:
#  1. make lint-changed  — golangci-lint scoped to the packages touched
#                          vs origin/main (or `main` if origin is absent).
#                          Fast on a warm cache (~5-15s typical).
#  2. make unit          — pure-logic unit tests, no envtest, no cluster.
#                          ~5-10s warm; ~30s cold.
#
# Bypass: `git commit --no-verify` skips the hook. Use only when you have
# a justified reason (e.g. an in-progress WIP commit you intend to amend
# before push); pre-push will still enforce the full lint sweep.
#
# Idempotent — safe to re-run.
set -uo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || {
  echo "not inside a git repo" >&2; exit 1;
}
cd "$REPO_ROOT"

if [[ -t 1 ]]; then
  RED=$'\033[31m'; YEL=$'\033[33m'; GRN=$'\033[32m'; BLU=$'\033[34m'; RST=$'\033[0m'
else
  RED=""; YEL=""; GRN=""; BLU=""; RST=""
fi

FAIL=0

hdr()  { printf '\n%s== %s ==%s\n' "$BLU" "$*" "$RST"; }
ok()   { printf '%sOK%s   %s\n' "$GRN" "$RST" "$*"; }
fail() { printf '%sFAIL%s %s\n' "$RED" "$RST" "$*"; FAIL=$((FAIL+1)); }

# The host has no Go toolchain — every Go-touching target funnels through
# the devtools container via ./scripts/dev.sh.
if [[ ! -x scripts/dev.sh ]]; then
  echo "${RED}scripts/dev.sh missing or not executable${RST}" >&2
  exit 2
fi

# ── 1. lint-changed ────────────────────────────────────────────────────
hdr "1. golangci-lint (changed packages)"
if ./scripts/dev.sh make lint-changed; then
  ok "lint-changed clean"
else
  fail "lint-changed reported issues — fix or 'git commit --no-verify' (you will still hit pre-push)"
fi

# ── 2. unit ────────────────────────────────────────────────────────────
hdr "2. make unit"
if ./scripts/dev.sh make unit; then
  ok "unit tests passed"
else
  fail "unit tests failed"
fi

# ── Summary ───────────────────────────────────────────────────────────
hdr "Summary"
if [[ "$FAIL" -gt 0 ]]; then
  printf '%sCommit blocked — %d gate(s) failed.%s\n' "$RED" "$FAIL" "$RST"
  printf 'Bypass with: %sgit commit --no-verify%s (pre-push still enforces full lint).\n' "$YEL" "$RST"
  exit 1
fi
printf '%sAll pre-commit gates passed.%s\n' "$GRN" "$RST"
exit 0
