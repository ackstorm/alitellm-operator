# Contributing

Thanks for considering a contribution to `alitellm-operator`. This
page summarizes the local dev loop, gates, and PR flow. Authoritative
files live in the repo root (`CONTRIBUTING.md`, `CLAUDE.md`,
`scripts/pre-push-check.sh`).

## Before you start

- Read the project's [Code of Conduct](https://github.com/ackstorm/alitellm-operator/blob/main/CONTRIBUTING.md).
- For non-trivial changes (new CRD, breaking API surface, controller
  behavior change), open a proposal PR under
  [`docs/proposals/`](https://github.com/ackstorm/alitellm-operator/tree/main/docs/proposals)
  based on the template, prefixed `<YEAR><MONTH>-<title>.md`.

## Host has NO Go toolchain — always via Docker

This repo deliberately keeps the host clean. There is no `go`,
`kubebuilder`, `controller-gen`, `kustomize`, `setup-envtest`, or
`golangci-lint` binary on PATH. Every toolchain invocation goes
through the devtools container:

```bash
./scripts/dev.sh go build ./...
./scripts/dev.sh go test ./internal/controller/...
./scripts/dev.sh make gen-manifests
./scripts/dev.sh bash                # interactive shell
```

The wrapper mounts the repo at `/workspace`, preserves host UID:GID,
persists Go module + build caches under `.gocache/`, and resolves
`KUBEBUILDER_ASSETS`. Image: `litellm-devtools:latest` (built from
`Dockerfile.devtools` on first use; force rebuild with
`LITELLM_DEVTOOLS_REBUILD=1`).

Pinned versions: Go 1.24.13, kubebuilder v4.4.0, controller-runtime
v0.19.4, k8s.io/* v0.31.0, govulncheck v1.3.0.

Targets that only invoke `kubectl`/`docker`/`helm`/`kind`/bash
(`make cluster-up`, `make operator-redeploy`, `make logs-*`, …) run
directly on the host.

## Test phases — pick the lightest that proves your change

| Phase                | Command                              | Use when                                  |
|----------------------|--------------------------------------|-------------------------------------------|
| Unit                 | `./scripts/dev.sh make test-unit`         | Every inner-loop iteration (~5s warm).    |
| Envtest (race)       | `./scripts/dev.sh make test-envtest`  | Before commit on controller changes (~7m).|
| Envtest (no race)    | `./scripts/dev.sh make test-envtest-fast` | Dev inner loop (~3m).                     |
| E2E (kind + Helm)    | `make e2e-full`                      | Final gate before commit (~10m clean).    |
| E2E focused          | `./scripts/dev.sh make e2e-focus FOCUS="..."` | Iterate after `make cluster-keep`. |
| Security             | `./scripts/dev.sh make qa-security`     | Before commit (~6m).                      |
| Pre-push gate        | `make pre-push`                      | Before every push (host-only).            |

Umbrella targets:

- `make test-full` = `unit` + `envtest-run`
- `make verify` = `./scripts/dev.sh make qa-security` + `make pre-push`
- `make hooks` installs `.git/hooks/pre-push -> scripts/pre-push-check.sh`

Inner-loop helpers when iterating on a single package:

```bash
./scripts/dev.sh make test-unit-pkg PKG=./internal/litellm/...
./scripts/dev.sh make test-envtest-pkg PKG=./internal/controller/... [FOCUS=TestX] [TIMEOUT=10m]
./scripts/dev.sh make qa-lint-changed [BASE_REF=...]    # lint only packages touched vs origin/main
```

## E2E debug loop (kept cluster)

`make e2e-full` tears down and recreates the kind cluster every run.
For iteration, keep the cluster up:

```bash
./scripts/dev.sh make e2e-keep                                  # bring up + run e2e, keep cluster
./scripts/dev.sh make e2e-focus FOCUS="rateLimits composite"    # ~30s-2m per iter
./scripts/dev.sh make operator-redeploy                         # hot-reload after code edit
```

When done, `make cluster-down` then the final-gate run.

## Pre-push gate (15 gates)

`make pre-push` is HOST-ONLY (it spawns gitleaks/trufflehog
containers on host docker — never via `./scripts/dev.sh`). Hard
failures block push; do NOT bypass with `--no-verify`.

Gates include:

- gitleaks + trufflehog (scope: `origin/main..HEAD`; full history on
  first push)
- Large files >2 MB
- Sensitive patterns (`.env`, `*.pem`, `*.key`, kubeconfig, …)
- LICENSE + README presence
- Origin remote matches expected
- `govulncheck` ack-list 1:1 match
  (`references/security/govulncheck-acknowledged.md`)
- `go mod tidy` drift
- Per-file SPDX license header

Fix the root cause; if a vulnerability needs an ack, add the entry to
`references/security/govulncheck-acknowledged.md` in the same commit.

## Commit style — conventional commits

```
feat(<scope>): <subject>
fix(<scope>): <subject>
refactor(<scope>): <subject>
docs(<scope>): <subject>
chore(<scope>): <subject>
test(<scope>): <subject>
ci(<scope>): <subject>
```

Subject < 72 chars, imperative. Examples:

```
feat(guardrail): support Bedrock-managed guardrails
fix(mcpserver): rewrite server_name when prefix separator is `-`
docs(user-guide): add LiteLLMTeam page
```

Release commits use `chore(release): v<MAJOR>.<MINOR>.<PATCH>` on
`main` — this triggers `release.yml`. See
[Release Process](release-process.md).

## PR flow

1. Branch off `main`.
2. Iterate; run `./scripts/dev.sh make test-unit` per change.
3. Before push: `./scripts/dev.sh make test-envtest` if touching
   controllers; `make pre-push`.
4. Open PR — CI runs `lint`, `unit`, `envtest`, `security`, `e2e`.
   All five must be green.
5. CodeRabbit auto-review; address findings.
6. At least one human review approval required.
7. Squash-merge (the project keeps `main` linear).

E2E runs once per change — on the PR. Post-merge skips it (already
green on the PR ref). Draft PRs skip the `e2e` job unless the label
`run-e2e` is applied.

## Where to file issues

- Bugs → bug report template.
- Features → feature request template.
- Security disclosures → see
  [SECURITY.md](https://github.com/ackstorm/alitellm-operator/blob/main/SECURITY.md)
  (private disclosure preferred over public issue).

## Where to read next

- [Development](development.md) — environment + inner loop in depth.
- [Architecture](architecture.md) — how the pieces fit together.
- [Release Process](release-process.md) — cutting a release.
- [`CLAUDE.md`](https://github.com/ackstorm/alitellm-operator/blob/main/CLAUDE.md)
  — AI-agent surgical reference card (also useful for humans).
