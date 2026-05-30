#!/usr/bin/env bash
# Pre-push validation for public-repo publication.
# Run from anywhere inside the repo. Exits 0 if safe to push.
#
# Hard checks (failure blocks push):
#   1. gitleaks    (only commits being pushed; full history on first push)
#   2. trufflehog  (only commits being pushed; full history on first push)
#   3. large tracked files (>2MB)
#   4. sensitive file patterns (.env, *.pem, *.key, kubeconfig, ...)
#   5. LICENSE + README presence
#   6. origin remote matches expected
#  13. govulncheck   (HIGH advisories vs ack-list — wrapper-enforced 1:1)
#  14. go mod tidy   (go.mod / go.sum drift blocks push)
#  14b. helm-sync     (config/crd → deploy/helm/.../crd-sources drift blocks push)
#  15. license-header SPDX gate (every in-scope *.go starts with SPDX line)
#  16. golangci-lint full sweep (defensive — pre-commit runs qa-lint-changed)
#  17. make test-unit (pure-logic regression — ~5-10s warm)
#
# Soft checks (warnings only):
#   7. internal hostnames / private IPv4 in tracked files
#   8. ackstorm.com emails outside LICENSE/NOTICE/AUTHORS
#   9. .gitignore sanity (.env, .claude)
#  10. commit author audit (informational)
#  11. TODO/DO-NOT-COMMIT markers
#  12. uncommitted working-tree changes (informational)

set -uo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || {
  echo "not inside a git repo" >&2; exit 1;
}
cd "$REPO_ROOT"

EXPECTED_REMOTE="git@github.com:ackstorm/alitellm-operator.git"

if [[ -t 1 ]]; then
  RED=$'\033[31m'; YEL=$'\033[33m'; GRN=$'\033[32m'; BLU=$'\033[34m'; RST=$'\033[0m'
else
  RED=""; YEL=""; GRN=""; BLU=""; RST=""
fi

FAIL=0
WARN=0

hdr()  { printf '\n%s== %s ==%s\n' "$BLU" "$*" "$RST"; }
ok()   { printf '%sOK%s   %s\n' "$GRN" "$RST" "$*"; }
warn() { printf '%sWARN%s %s\n' "$YEL" "$RST" "$*"; WARN=$((WARN+1)); }
fail() { printf '%sFAIL%s %s\n' "$RED" "$RST" "$*"; FAIL=$((FAIL+1)); }

# --- preflight: docker available ---
if ! command -v docker >/dev/null 2>&1; then
  echo "${RED}docker is required for secret scanning.${RST}" >&2
  exit 2
fi
if ! docker info >/dev/null 2>&1; then
  echo "${RED}docker daemon not reachable.${RST}" >&2
  exit 2
fi

# --- scan scope: only new commits, full history on first push ---
# Default branch on the public remote; override via PRE_PUSH_BASE_REF.
BASE_REF="${PRE_PUSH_BASE_REF:-origin/main}"
if git rev-parse --verify "$BASE_REF" >/dev/null 2>&1; then
  SCAN_BASE=$(git merge-base "$BASE_REF" HEAD 2>/dev/null || git rev-parse "$BASE_REF")
  SCAN_LABEL="$BASE_REF..HEAD"
  GITLEAKS_LOG_OPTS="--log-opts=${SCAN_BASE}..HEAD"
  TRUFFLEHOG_SINCE="--since-commit=${SCAN_BASE}"
else
  SCAN_BASE=""
  SCAN_LABEL="full history (no $BASE_REF tracking ref — first push bootstrap)"
  GITLEAKS_LOG_OPTS=""
  TRUFFLEHOG_SINCE=""
fi

# --- worktree-safe scan root ---
# The secret scanners run git inside their own containers with only the repo
# dir mounted. A git WORKTREE has a `.git` FILE (not a dir) pointing at an
# external gitdir those containers cannot resolve: gitleaks then scans 0
# commits and trufflehog errors on `.git/index`. When REPO_ROOT is a worktree,
# scan a throwaway self-contained clone (real `.git` dir, full history;
# --no-hardlinks so every object is present in the mount). SCAN_BASE is a SHA
# that exists in the clone, so the range / --since-commit still apply, and the
# clone's HEAD tracks the worktree's branch tip.
SCAN_ROOT="$REPO_ROOT"
SCAN_TMP=""
if [[ -f "$REPO_ROOT/.git" ]]; then
  SCAN_TMP="$(mktemp -d)"
  if git clone --quiet --no-hardlinks "$REPO_ROOT" "$SCAN_TMP/repo" 2>/dev/null; then
    SCAN_ROOT="$SCAN_TMP/repo"
  else
    warn "worktree clone for secret scan failed; scanning worktree dir directly (may under-scan)"
  fi
fi
trap '[[ -n "$SCAN_TMP" ]] && rm -rf "$SCAN_TMP"' EXIT

# --- 1. gitleaks ---
hdr "1. gitleaks ($SCAN_LABEL)"
if docker run --rm -v "$SCAN_ROOT:/repo:ro" zricethezav/gitleaks:latest \
     detect --source=/repo --redact --no-banner \
     --config=/repo/.gitleaks.toml $GITLEAKS_LOG_OPTS; then
  ok "no leaks detected"
else
  fail "gitleaks found secrets (see output above)"
fi

# --- 2. trufflehog ---
hdr "2. trufflehog ($SCAN_LABEL)"
if docker run --rm -v "$SCAN_ROOT:/pwd:ro" trufflesecurity/trufflehog:latest \
     git file:///pwd --only-verified --fail --no-update $TRUFFLEHOG_SINCE; then
  ok "no verified live secrets"
else
  fail "trufflehog found verified live secrets"
fi

# --- 3. Large tracked files ---
hdr "3. large tracked files (>2MB)"
# Threshold lifted from 1MB -> 2MB to accommodate the vendored LiteLLM
# OpenAPI spec at spec/litellm_api.json (~1.2 MB). The spec is documentation,
# regenerated verbatim from upstream LiteLLM, and is the only tracked file
# that crossed the prior 1 MB line.
LARGE=$(git ls-files -z | while IFS= read -r -d '' f; do
  [[ -f $f ]] || continue
  sz=$(stat -c%s "$f" 2>/dev/null || stat -f%z "$f" 2>/dev/null || echo 0)
  if (( sz > 2097152 )); then
    printf '  %10d  %s\n' "$sz" "$f"
  fi
done)
if [[ -z $LARGE ]]; then
  ok "no tracked files over 2MB"
else
  fail "large tracked files:"
  printf '%s\n' "$LARGE"
fi

# --- 4. Sensitive file patterns ---
hdr "4. sensitive file patterns in tracked files"
SENSITIVE_PATTERNS=(
  '(^|/)\.env($|\..*)'
  '\.pem$' '\.key$' '\.pfx$' '\.p12$' '\.pkcs12$'
  '(^|/)id_rsa([^/]*)$' '(^|/)id_ed25519([^/]*)$' '(^|/)id_ecdsa([^/]*)$'
  '(^|/)credentials\.json$'
  '(^|/)kubeconfig$' '\.kubeconfig$'
  '(^|/)service-account.*\.json$'
  '(^|/)gcp-key.*\.json$'
  '(^|/)aws-credentials($|\..*)'
  '(^|/)\.npmrc$' '(^|/)\.pypirc$'
)
SENS_HITS=""
for pat in "${SENSITIVE_PATTERNS[@]}"; do
  m=$(git ls-files | grep -E "$pat" || true)
  [[ -n $m ]] && SENS_HITS+="$m"$'\n'
done
if [[ -z $SENS_HITS ]]; then
  ok "no sensitive file patterns tracked"
else
  fail "sensitive file patterns tracked:"
  printf '%s' "$SENS_HITS"
fi

# --- 5. LICENSE + README ---
hdr "5. LICENSE + README presence"
[[ -f LICENSE ]]   && ok "LICENSE present"   || fail "LICENSE missing"
[[ -f README.md ]] && ok "README.md present" || fail "README.md missing"

# --- 6. Remote check ---
hdr "6. origin remote"
ACTUAL=$(git remote get-url origin 2>/dev/null || echo "")
if [[ -z $ACTUAL ]]; then
  warn "no 'origin' remote configured"
  echo "  expected: $EXPECTED_REMOTE"
  echo "  add with: git remote add origin $EXPECTED_REMOTE"
elif [[ $ACTUAL != "$EXPECTED_REMOTE" ]]; then
  fail "origin is '$ACTUAL', expected '$EXPECTED_REMOTE'"
else
  ok "origin = $EXPECTED_REMOTE"
fi

# --- 7. Internal hostnames / private IPs ---
hdr "7. internal hostnames / private IPv4 (informational)"
INTERNAL_RE='(ackstorm\.internal|\.ackstorm\.local|jira\.ackstorm|confluence\.ackstorm|gitlab\.ackstorm)'
PRIVIP_RE='(^|[^0-9.])(10\.[0-9]+\.[0-9]+\.[0-9]+|172\.(1[6-9]|2[0-9]|3[01])\.[0-9]+\.[0-9]+|192\.168\.[0-9]+\.[0-9]+)'
INT_HITS=$(git grep -EnI "$INTERNAL_RE" -- ':!*.lock' ':!go.sum' ':!*.svg' 2>/dev/null || true)
IP_HITS=$(git grep -EnI  "$PRIVIP_RE"   -- ':!*.lock' ':!go.sum' ':!*.svg' 2>/dev/null || true)
if [[ -z $INT_HITS && -z $IP_HITS ]]; then
  ok "no internal hostname/private-IP matches"
else
  if [[ -n $INT_HITS ]]; then
    warn "internal hostnames found:"
    printf '%s\n' "$INT_HITS" | head -20
  fi
  if [[ -n $IP_HITS ]]; then
    warn "private IPv4 found (test fixtures often legitimate — review):"
    printf '%s\n' "$IP_HITS" | head -20
  fi
fi

# --- 8. Personal/company email leak ---
hdr "8. ackstorm emails in tracked files"
MAIL_HITS=$(git grep -EnI '[a-zA-Z0-9._%+-]+@(ackstorm\.com|ackstorm\.es)' \
              -- ':!LICENSE' ':!NOTICE' ':!AUTHORS' ':!CONTRIBUTORS*' 2>/dev/null || true)
if [[ -z $MAIL_HITS ]]; then
  ok "no ackstorm emails in code"
else
  warn "ackstorm emails in tracked files (review):"
  printf '%s\n' "$MAIL_HITS" | head -20
fi

# --- 9. .gitignore sanity ---
hdr "9. .gitignore sanity"
if [[ ! -f .gitignore ]]; then
  warn ".gitignore missing"
else
  for p in '.env' '.claude'; do
    if grep -qE "(^|/)${p//./\\.}(/|$)" .gitignore; then
      ok ".gitignore covers $p"
    else
      warn ".gitignore does not mention $p"
    fi
  done
fi

# --- 10. Author audit ---
hdr "10. commit authors (informational — confirm all are intended)"
git log --all --format='%aN <%aE>' | sort -u | sed 's/^/  /'

# --- 11. Urgent TODO markers ---
hdr "11. urgent TODO / DO-NOT-COMMIT markers"
# Exclude this gate script itself (it names the literal markers in its
# own comments) to avoid self-reflection false-positives.
TODO_HITS=$(git grep -nE '(DO[ _-]?NOT[ _-]?COMMIT|XXX[ _-]?REMOVE|TODO:?[ ]?(remove|delete|drop|secret))' \
              -- ':!scripts/pre-push-check.sh' 2>/dev/null || true)
if [[ -z $TODO_HITS ]]; then
  ok "no urgent TODO markers"
else
  warn "urgent TODO markers found:"
  printf '%s\n' "$TODO_HITS"
fi

# --- 12. Uncommitted changes ---
hdr "12. working tree status"
DIRTY=$(git status --porcelain)
if [[ -z $DIRTY ]]; then
  ok "working tree clean"
else
  warn "uncommitted changes present — they will NOT be pushed:"
  printf '%s\n' "$DIRTY" | head -20
fi

# --- 13. govulncheck (HIGH advisory block via ack-list wrapper) ---
hdr "13. govulncheck (HIGH advisories vs ack-list)"
# Wrapper `scripts/govulncheck-gate.sh` (Phase 13-02) runs govulncheck and
# enforces a 1:1 match against `references/security/govulncheck-acknowledged.md`.
# Exits 0 only when the reachable advisory set matches the ack-list exactly;
# any NEW HIGH advisory OR any ack-list drift fails the gate.
#
# The wrapper itself relies on `govulncheck` being on PATH; on the host this
# is only true inside the devtools container, so we route the call through
# `./scripts/dev.sh`.
if ./scripts/dev.sh bash scripts/govulncheck-gate.sh > /tmp/govulncheck-prepush.txt 2>&1; then
  ok "govulncheck residuals match ack-list 1:1"
else
  fail "govulncheck reported NEW HIGH advisories or ack-list drift"
  sed -n '1,40p' /tmp/govulncheck-prepush.txt
fi

# --- 14. go mod tidy drift ---
hdr "14. go mod tidy drift"
# Snapshot go.mod / go.sum BEFORE tidy so we can restore them on drift —
# pre-push must not mutate the working tree. Use cp (not bash $(cat) +
# printf '%s') because the latter strips trailing newlines, which then
# manifests as a phantom 'No newline at end of file' diff on the next
# run. The snapshot lives in a tempdir until the gate exits.
SNAP_DIR=$(mktemp -d)
trap 'rm -rf "$SNAP_DIR"' EXIT
cp go.mod "$SNAP_DIR/go.mod" 2>/dev/null || true
cp go.sum "$SNAP_DIR/go.sum" 2>/dev/null || true
if ./scripts/dev.sh go mod tidy >/tmp/gomod-tidy.txt 2>&1; then
  if git diff --quiet -- go.mod go.sum 2>/dev/null; then
    ok "go.mod / go.sum are tidy"
  else
    fail "go mod tidy produced uncommitted drift in go.mod / go.sum"
    git --no-pager diff -- go.mod go.sum | head -40
    # Restore byte-for-byte so the pre-push check does not leave dirty state.
    [[ -f "$SNAP_DIR/go.mod" ]] && cp "$SNAP_DIR/go.mod" go.mod
    [[ -f "$SNAP_DIR/go.sum" ]] && cp "$SNAP_DIR/go.sum" go.sum
  fi
else
  fail "go mod tidy exited non-zero (see /tmp/gomod-tidy.txt)"
  sed -n '1,20p' /tmp/gomod-tidy.txt
fi

# --- 14b. chart / CRD drift (helm-sync) ---
hdr "14b. helm-sync drift (config/crd → deploy/helm/.../crd-sources)"
# `make helm-sync` regenerates CRDs (controller-gen → config/crd/bases),
# rebuilds dist/install.yaml, copies CRDs into the chart's crd-sources/,
# and re-renders templates/install.yaml. Any divergence means a PR landed
# a schema change in api/ or RBAC change in kustomize without refreshing
# the published chart — exactly how v0.7.0 shipped stale CRDs for PR #25
# (endpoint validation) and PR #38 (DuplicateDiscovery → Conflict rename).
#
# Snapshot the touched files BEFORE syncing so we can restore on drift.
# `make helm-sync` also flips config/manager/kustomization.yaml back to
# `controller:latest` (build-installer dep), so it is included in the
# snapshot/restore set.
HELM_SNAP=$(mktemp -d)
mkdir -p "$HELM_SNAP/crd-sources" "$HELM_SNAP/templates" "$HELM_SNAP/config-manager"
cp -a deploy/helm/alitellm-operator/crd-sources/.        "$HELM_SNAP/crd-sources/"      2>/dev/null || true
cp    deploy/helm/alitellm-operator/templates/install.yaml "$HELM_SNAP/templates/install.yaml" 2>/dev/null || true
cp    config/manager/kustomization.yaml                  "$HELM_SNAP/config-manager/kustomization.yaml" 2>/dev/null || true
if ./scripts/dev.sh make helm-sync >/tmp/helm-sync.txt 2>&1; then
  # build-installer always rewrites config/manager/kustomization.yaml's
  # image pin to controller:latest. Restore it unconditionally before
  # diffing the chart — the kustomization edit is a side effect, not
  # drift we care about.
  cp "$HELM_SNAP/config-manager/kustomization.yaml" config/manager/kustomization.yaml
  if git diff --quiet -- deploy/helm/alitellm-operator/crd-sources deploy/helm/alitellm-operator/templates/install.yaml 2>/dev/null; then
    ok "chart crd-sources + templates/install.yaml in sync"
  else
    fail "make helm-sync produced uncommitted drift — run 'make helm-sync' and commit the result"
    git --no-pager diff --stat -- deploy/helm/alitellm-operator/crd-sources deploy/helm/alitellm-operator/templates/install.yaml | head -20
    # Restore so pre-push does not mutate the working tree.
    rm -rf deploy/helm/alitellm-operator/crd-sources
    mkdir -p deploy/helm/alitellm-operator/crd-sources
    cp -a "$HELM_SNAP/crd-sources/." deploy/helm/alitellm-operator/crd-sources/
    [[ -f "$HELM_SNAP/templates/install.yaml" ]] && cp "$HELM_SNAP/templates/install.yaml" deploy/helm/alitellm-operator/templates/install.yaml
  fi
else
  cp "$HELM_SNAP/config-manager/kustomization.yaml" config/manager/kustomization.yaml 2>/dev/null || true
  fail "make helm-sync exited non-zero (see /tmp/helm-sync.txt)"
  sed -n '1,30p' /tmp/helm-sync.txt
fi
rm -rf "$HELM_SNAP"

# --- 15. license-header SPDX gate (HRD-10) ---
hdr "15. license-header SPDX gate (HRD-10)"
# Every in-scope *.go file MUST carry `// SPDX-License-Identifier: Apache-2.0`
# in its first 5 lines. Exempt: vendor/, zz_generated*, mock_*, .claude/.
# Build-tag-first files have SPDX on line 3 (after //go:build + blank line).
MISSING_SPDX=""
while IFS= read -r f; do
  [[ "$f" == */vendor/* ]] && continue
  [[ "$f" == .claude/* ]] && continue
  [[ "$(basename "$f")" == zz_generated* ]] && continue
  [[ "$(basename "$f")" == mock_* ]] && continue
  # Match SPDX in the first 5 lines of the file.
  if ! head -5 "$f" 2>/dev/null | grep -qx "// SPDX-License-Identifier: Apache-2.0"; then
    MISSING_SPDX+="  $f"$'\n'
  fi
done < <(git ls-files '*.go')
if [[ -z $MISSING_SPDX ]]; then
  ok "every in-scope *.go file carries the SPDX header"
else
  fail "files missing SPDX header:"
  printf '%s' "$MISSING_SPDX" | head -20
fi

# --- 16. golangci-lint full sweep ---
# Defensive gate: pre-commit runs `make qa-lint-changed` (scoped to touched
# packages) on every commit; this is the FULL sweep, catching anything
# a `--no-verify` commit or a stale BASE_REF would have masked. Runs in
# the devtools container.
hdr "16. golangci-lint full sweep"
if [[ -x scripts/dev.sh ]]; then
  if ./scripts/dev.sh make qa-lint >/tmp/pre-push-lint.log 2>&1; then
    ok "golangci-lint clean"
  else
    fail "golangci-lint reported issues — see /tmp/pre-push-lint.log"
  fi
else
  warn "scripts/dev.sh missing — skipping lint gate (rebuild devtools image)"
fi

# --- 17. unit tests ---
# Catches the simplest class of breakage that CI would otherwise flag.
# Runs via devtools container; ~5-10s warm.
hdr "17. unit tests"
if [[ -x scripts/dev.sh ]]; then
  if ./scripts/dev.sh make test-unit >/tmp/pre-push-unit.log 2>&1; then
    ok "make test-unit clean"
  else
    fail "make test-unit failed — see /tmp/pre-push-unit.log"
  fi
else
  warn "scripts/dev.sh missing — skipping unit gate (rebuild devtools image)"
fi

# --- Summary ---
printf '\n%s== Summary ==%s\n' "$BLU" "$RST"
printf 'Failures: %d\n' "$FAIL"
printf 'Warnings: %d\n' "$WARN"
echo
if (( FAIL > 0 )); then
  printf '%sBLOCKED: fix failures before pushing.%s\n' "$RED" "$RST"
  exit 1
fi
if (( WARN > 0 )); then
  printf '%sWarnings present — review each before pushing.%s\n' "$YEL" "$RST"
fi
printf '%sAll hard checks passed.%s\n' "$GRN" "$RST"
exit 0
