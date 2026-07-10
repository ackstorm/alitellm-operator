# Acknowledged govulncheck residuals

**Date:** 2026-05-21
**Toolchain at acknowledgement:** Go 1.26.3 (`Dockerfile.devtools` + `go.mod` `toolchain` directive), `golang.org/x/net@v0.53.0`
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

## Acknowledged advisories (1)

| # | OSV ID | CVE | Module | Symbol the operator touches | Fixed in | Justification |
|---|--------|-----|--------|------------------------------|----------|---------------|
| 1 | GO-2026-5856 | — | crypto/tls (stdlib) | generic TLS handshake via `http.Transport.RoundTrip` / `httptest.NewServer` (`internal/litellm/transport.go`, `internal/providers/elevenlabs.go`, mock server, test utils) | go1.26.5 | Encrypted Client Hello privacy leak in `crypto/tls`. The operator never configures ECH (`tls.Config.EncryptedClientHelloConfigList` is unset everywhere), so the leak path is not exercised; reachable only through ordinary outbound TLS. Fixed in go1.26.5 — accept as residual until the toolchain bump lands across the four pinned surfaces. |

**Reachable count: 1.**

## History

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
