#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Post-processes config/rbac/role.yaml after controller-gen runs.
#
# controller-gen always emits `kind: ClusterRole` with the name passed
# to `rbac:roleName=`. Issue #21 requires the manager-role to be a
# namespace-scoped Role with the new name `alitellm-operator-role`.
# This script rewrites the generated file in place to that shape; the
# rules block (the only part that depends on +kubebuilder:rbac markers)
# is preserved verbatim.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
FILE="${REPO_ROOT}/config/rbac/role.yaml"

[[ -f "${FILE}" ]] || { echo "normalize-manager-role.sh: ${FILE} missing" >&2; exit 1; }

# 1. kind: ClusterRole -> kind: Role
# 2. name: alitellm-operator-manager-role -> name: alitellm-operator-role
# 3. inject namespace: system on the line after the new name (idempotent:
#    skip if namespace: already present in the metadata block).
python3 - "$FILE" <<'PY'
import sys, pathlib
p = pathlib.Path(sys.argv[1])
src = p.read_text()
src = src.replace("kind: ClusterRole", "kind: Role", 1)
src = src.replace("name: alitellm-operator-manager-role",
                  "name: alitellm-operator-role", 1)
# Inject namespace: system after the (now renamed) name line. Idempotent:
# skip if the next non-empty line already declares a namespace.
lines = src.splitlines(keepends=True)
out = []
injected = False
i = 0
while i < len(lines):
    out.append(lines[i])
    if (not injected
            and lines[i].strip() == "name: alitellm-operator-role"):
        # Peek next non-empty line for an existing namespace: key.
        j = i + 1
        while j < len(lines) and lines[j].strip() == "":
            j += 1
        if j >= len(lines) or not lines[j].lstrip().startswith("namespace:"):
            out.append("  namespace: system\n")
        injected = True
    i += 1
p.write_text("".join(out))
PY

echo "normalize-manager-role.sh: rewrote ${FILE} to kind: Role, name: alitellm-operator-role"
