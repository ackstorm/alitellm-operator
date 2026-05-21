# Acknowledged govulncheck residuals

**Date:** 2026-05-21
**Toolchain at acknowledgement:** Go 1.26.3 (`Dockerfile.devtools`), `golang.org/x/net@v0.46.0`
**Scanner:** `govulncheck@v1.3.0` (pinned in `Dockerfile.devtools::GOVULNCHECK_VERSION`)
**Invocation:** `./scripts/dev.sh govulncheck ./...`

## Purpose

After bumping the devtools base from Go 1.24.13 to Go 1.26.3, seven of
the eight previously-acknowledged stdlib advisories cleared (they were
fixed in Go 1.25.x stdlib releases that 1.26.3 includes). The single
remaining advisory is in `golang.org/x/net`, which is a third-party
module and not part of the stdlib bump.

`x/net@v0.53.0` and later declare `go >= 1.25.0` and now build cleanly
under our Go 1.26.3 base — bumping `golang.org/x/net` to v0.53.0+ is
the way to clear the last residual. That bump is deferred until the
next dependency-sync phase because it touches `controller-runtime`'s
indirect dep graph and warrants its own envtest run.

The `./scripts/dev.sh govulncheck ./...` output MUST match this list 1:1 — any new entry is a regression and must be addressed (either by patching, by bumping a dependency, or by an updated entry in this file with reviewer-approved justification).

## Acknowledged advisories (1)

| # | OSV ID | CVE | Module | Symbol the operator touches | Fixed in | Justification |
|---|--------|-----|--------|------------------------------|----------|---------------|
| 1 | GO-2026-4918 | CVE-2026-33814 | `golang.org/x/net` (HTTP/2 transport) | bad `SETTINGS_MAX_FRAME_SIZE` infinite loop | `x/net@v0.53.0` | The operator's outbound HTTP/2 connections terminate at LiteLLM, which is co-located in-cluster behind a trusted ClusterIP. No untrusted HTTP/2 server is reachable from production. Bump scheduled in the next dependency-sync phase. |

**Reachable count: 1.**

## Verification

Re-run after any dependency bump or Go base change:

```bash
./scripts/dev.sh govulncheck ./... 2>&1 | grep '^Vulnerability' | wc -l
# Expected: 1
```

If the count exceeds 1, a new advisory has been introduced — fix or extend this file with a new reviewer-approved row before merge.

## Revisit trigger

Open a follow-up plan to bump `golang.org/x/net` to `v0.53.0` (or
later) in the same wave as the next batched dependency-sync. Expected
post-bump residual: 0.

## Cross-references

- `scripts/govulncheck-gate.sh` — the wrapper that enforces this list 1:1
  as a pre-push gate (gate 13 in `scripts/pre-push-check.sh`).
- `Dockerfile.devtools` — pinned `GOVULNCHECK_VERSION` + base Go version.
