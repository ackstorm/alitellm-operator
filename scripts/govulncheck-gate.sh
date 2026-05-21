#!/usr/bin/env bash
# govulncheck-gate.sh — run govulncheck and compare its CALLED-set against
# the acknowledged residuals in references/security/govulncheck-acknowledged.md.
#
# Exits 0 iff the reachable advisory set EXACTLY matches the acknowledged
# list (no missing, no extra). Any deviation = block.
#
# Used by `make security` (Phase 13 HRD-04). Invoked inside the devtools
# container — relies on the bare `govulncheck` binary being on PATH.

set -euo pipefail

ACK_FILE="references/security/govulncheck-acknowledged.md"

if [[ ! -f "$ACK_FILE" ]]; then
  echo "FAIL: acknowledged file not found: $ACK_FILE" >&2
  exit 1
fi

# Extract the GO-XXXX-XXXX IDs from the table rows in the ack file.
# Table format: `| <#> | GO-YYYY-NNNN | ...` — we grep the second column.
# Empty ack-list is valid and means "expect zero reachable advisories";
# `|| true` keeps `pipefail` from aborting on the no-match grep exit.
EXPECTED=$( { grep -oE '^\| [0-9]+ \| GO-[0-9]{4}-[0-9]+' "$ACK_FILE" || true; } \
  | awk -F'|' '{print $3}' | tr -d ' ' | sort -u)

# Run govulncheck; capture full output so we can both inspect IDs and surface
# the report to humans on mismatch. govulncheck exits 3 when reachable
# advisories exist — that's expected; we override the exit code via the
# comparison below.
set +e
RAW=$(govulncheck ./... 2>&1)
GOVULN_EXIT=$?
set -e

# Extract reachable (CALLED) advisory IDs. The text-mode output lists each
# reachable advisory under a `Vulnerability #N: GO-XXXX-XXXX` header.
# Zero reachable is the happy path; `|| true` keeps pipefail from aborting.
ACTUAL=$( { echo "$RAW" | grep -oE '^Vulnerability #[0-9]+: GO-[0-9]{4}-[0-9]+' || true; } \
  | awk '{print $NF}' | sort -u)

if [[ "$EXPECTED" == "$ACTUAL" ]]; then
  if [[ -z "$EXPECTED" ]]; then
    echo "govulncheck-gate: PASS — 0 reachable advisories (ack-list is empty)."
  else
    COUNT=$(echo "$EXPECTED" | wc -l | tr -d ' ')
    echo "govulncheck-gate: PASS — ${COUNT} reachable advisories match the acknowledged set."
  fi
  exit 0
fi

echo "govulncheck-gate: FAIL — reachable advisory set does not match acknowledged list." >&2
echo "--- expected (from $ACK_FILE) ---" >&2
echo "$EXPECTED" >&2
echo "--- actual (from govulncheck) ---" >&2
echo "$ACTUAL" >&2
echo "" >&2
echo "If a NEW advisory appeared: fix the underlying issue OR extend $ACK_FILE with a reviewer-approved row + justification, then re-run." >&2
echo "If an EXISTING advisory cleared: REMOVE the row from $ACK_FILE so the gate stays honest." >&2
echo "" >&2
echo "Full govulncheck output (exit $GOVULN_EXIT):" >&2
echo "$RAW" >&2
exit 1
