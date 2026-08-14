# Acknowledged govulncheck residuals

**Date:** 2026-08-14
**Toolchain at acknowledgement:** Go 1.26.6 (`Dockerfile.devtools` + `go.mod` `toolchain` directive), `golang.org/x/net@v0.53.0`
**Scanner:** `govulncheck@v1.3.0` (pinned in `Dockerfile.devtools::GOVULNCHECK_VERSION`)
**Invocation:** `./scripts/dev.sh govulncheck ./...`

## Purpose

This file lists reachable advisories that have been reviewed and
explicitly acknowledged as accepted residual risk. The gate script
`scripts/govulncheck-gate.sh` enforces a 1:1 match between actual
reachable advisories and the rows below — any deviation (new advisory
appearing or an acknowledged advisory clearing) blocks `make qa-security`
and `make pre-push`.

An empty list means the gate expects ZERO reachable advisories. Any
new reachable advisory must be:

1. Fixed by patching the dependency or stdlib, OR
2. Added below with a reviewer-approved justification.

## Acknowledged advisories (0)

| # | OSV ID | CVE | Module | Symbol the operator touches | Fixed in | Justification |
|---|--------|-----|--------|------------------------------|----------|---------------|
| — | — | — | — | — | — | *(none — the gate expects ZERO reachable advisories)* |

**Reachable count: 0.**

## History

- 2026-08-14: Bumped the Go toolchain 1.26.4 -> 1.26.6 across all four
  pinned surfaces. Cleared SEVEN reachable stdlib advisories at once —
  `GO-2026-5026` + `GO-2026-6089` (net/http), `GO-2026-5972`
  (encoding/asn1), `GO-2026-6088` (encoding/xml), `GO-2026-6218`
  (net/url), `GO-2026-6090` (crypto/tls) and the previously acknowledged
  `GO-2026-5856`. All were reachable through real operator paths
  (`internal/litellm/transport.go`, `internal/providers/{anthropic,bedrock}.go`),
  not test-only, and all are fixed in go1.26.6. The list is now EMPTY:
  any new reachable advisory blocks the gate until fixed or acknowledged.
- 2026-07-10: Acknowledged `GO-2026-5856` (crypto/tls Encrypted Client
  Hello privacy leak, fixed in go1.26.5) as residual — the operator
  never enables ECH, so the leak path is unreachable in practice; drop
  this row when the toolchain is bumped to go1.26.5.
- 2026-05-21: Bumped Go toolchain to 1.26.3 (via `toolchain go1.26.3`
  in `go.mod`) and `golang.org/x/net` to `v0.53.0`. Cleared the only
  remaining residual (`GO-2026-4918` — HTTP/2 `SETTINGS_MAX_FRAME_SIZE`
  infinite loop in `x/net`).

## Verification

Re-run after any dependency bump or Go base change:

```bash
./scripts/dev.sh govulncheck ./... 2>&1 | grep -c '^Vulnerability'
# Expected: 0
```

If any advisory appears, fix it or extend this file with a
reviewer-approved row + justification before merge.

## Cross-references

- `scripts/govulncheck-gate.sh` — the wrapper that enforces this list
  1:1 as a pre-push gate (gate 13 in `scripts/pre-push-check.sh`).
- `Dockerfile.devtools` — pinned `GOVULNCHECK_VERSION` + base Go version.
- `go.mod` — `toolchain go1.26.3` directive pins CI's Go runtime to
  match the devtools image.
