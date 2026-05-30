# CLAUDE.md — alitellm-operator

Surgical reference card for AI agents working in this repository. Read the
sections relevant to your task. Reading MANDATORY Reading Table entries
before touching the corresponding code is non-negotiable.

## Documentation hygiene — keep this file and `docs/` in sync with the code

Whenever a change alters behavior, contracts, or workflows that this
`CLAUDE.md` or the user-facing docs describe, update the affected
documentation in the SAME commit as the code change. No "docs follow-up
PR later".

Triggers requiring a docs/CLAUDE.md update:
- New or changed CRD field, condition, or default value → update
  `docs/api-reference/` (`make gen-crd-ref-docs`) and any CRD examples
  under `examples/`.
- New `make` target, renamed target, or changed default behavior →
  update the relevant table in this file (Test phases, Waiting for
  state, ...) and `docs/` if user-facing.
- New `wait-*` target, blessed pattern, or polling rule → update the
  "Waiting for state" table and the bash anti-patterns section.
- New pre-push gate, govulncheck ack-list entry, or SPDX/license rule →
  update the "Publication" section and `references/security/...`.
- Release pipeline change (`release.yml`, goreleaser configs, bump
  flow) → update the "Release pipeline" section.
- New common failure mode encountered while debugging → add a
  `### ❌ ... ✅ ...` entry under "Common failure modes".
- New MANDATORY-read file for a workflow → add a row to the
  "MANDATORY Reading Table".

If a doc claim is found stale during work, fix it in the same change
that revealed the staleness. Drift is a bug, not tech debt.

## Quick context

Kubernetes operator that reconciles LiteLLM proxy state — teams, models,
model-discovery, MCP servers, A2A agents — from declarative CRDs into
LiteLLM HTTP API calls. Written in Go (controller-runtime v0.19.4,
k8s.io/* v0.31.0). Ships as a container image + Helm chart.

Selected release plumbing grafted from
[bbdsoftware/litellm-operator](https://github.com/bbdsoftware/litellm-operator)
(Apache-2.0). See `NOTICE` for attribution. The `bbd` graft is restricted
to non-code surfaces: CI workflows, goreleaser configs, mkdocs site
scaffold, community files. All Go code, CRDs, and config are original
ackstorm material.

## Architecture

```
┌────────────┐ reconcile ┌────────────────┐  HTTP   ┌──────────────┐
│   CRDs     │──────────▶│   Operator     │────────▶│ LiteLLM API  │
│ (LiteLLM*) │           │ (controllers/) │         │ (in cluster) │
└────────────┘           └───────┬────────┘         └──────────────┘
                                 │
                                 ▼
                       ┌──────────────────┐
                       │  Status updates  │
                       │  + events        │
                       └──────────────────┘
```

Owned CRDs: `LiteLLMTeam`, `LiteLLMModel`, `LiteLLMModelDiscovery`,
`LiteLLMMCPServer`, `LiteLLMMCPServerDiscovery`, `LiteLLMA2AAgent`,
`LiteLLMConnection`. API group `litellm.ackstorm.ai`, version `v1alpha1`.

Critical paths:
- CRD apply → reconciler → LiteLLM HTTP call → status condition update
- Secret rotation → reconciler re-reads → LiteLLM update with new auth
- Operator restart → informer resync → drift reconciliation

## Repository layout (post-graft)

```
alitellm-operator/
├── .github/                 ← bbd-derived; reconciled for ackstorm
│   ├── workflows/           CI / release / docs / govulncheck / labeler / gotests
│   ├── CODEOWNERS, dependabot.yml, labeler.yml, ISSUE_TEMPLATE/, PR template
├── .goreleaser.yml          ← bbd-derived; stable release config
├── .goreleaser.prerelease.yml   ← prerelease (alpha/beta/rc tags)
├── .goreleaser.snapshot.yml ← main-branch snapshot builds
├── .coderabbit.yaml         ← CodeRabbit PR auto-review config
├── Dockerfile               ← runtime image (golang:1.24 builder → distroless)
├── Dockerfile.devtools      ← devtools container (scripts/dev.sh)
├── Dockerfile.goreleaser    ← release image, consumed by goreleaser
├── api/litellm/v1alpha1/    ← CRD Go types (litellm.ackstorm.ai)
├── cmd/main.go              ← operator entrypoint
├── internal/                ← all controller + LiteLLM + helper code
├── config/                  ← kubebuilder kustomize overlays
├── deploy/helm/alitellm-operator/   ← Helm chart shipped on release
├── docs/                    ← mkdocs site (api-reference auto-gen)
├── docs/Makefile            ← docs targets (gen-crd-ref-docs, docs-serve, ...)
├── docs/.crd-ref-docs.yaml  ← crd-ref-docs config (litellm.ackstorm.ai)
├── examples/                ← runnable CR samples
├── hack/boilerplate.go.txt  ← SPDX one-liner, prepended to generated files
├── references/              ← agent-facing internal docs (NOT on public site)
│   └── security/govulncheck-acknowledged.md
├── scripts/                 ← dev.sh, cluster.sh, pre-push-check.sh, ...
├── spec/                    ← frozen LiteLLM OpenAPI + design spec
├── test/                    ← e2e (Ginkgo) + utils
├── ROADMAP.md, CHANGELOG.md, SECURITY.md, MAINTAINERS.md, CONTRIBUTING.md
└── PROJECT, README.md, LICENSE, NOTICE, PUBLISH.md
```

## MANDATORY Reading Table

| Working on...                          | MUST read first                          |
|----------------------------------------|------------------------------------------|
| `make` interface / target routing / contexts | `references/makefile.md` (command list + 3-context model + `container_target`) |
| E2E tests (kind cluster + Helm)        | `test/e2e/README.md`                     |
| CI workflows (ci, docs, release, ...)  | `references/docs/workflow.md` (matrix + rationale); `.github/workflows/*.yml` is authoritative |
| Release tooling (goreleaser, signing)  | `.goreleaser.yml` + `release.yml` workflow |
| Pre-push gate logic                    | `scripts/pre-push-check.sh` (gate list)  |
| Publication / first-push procedure     | `PUBLISH.md`                             |
| Helm chart values + defaults           | `deploy/helm/alitellm-operator/values.yaml` |
| API reference rendering                | `docs/Makefile` (`gen-crd-ref-docs`) + `docs/.crd-ref-docs.yaml` |
| Docs site, mkdocs, mike, gh-pages flow | `references/docs/documentation.md`       |
| CI / PR / release lifecycle (push/PR matrix) | `references/docs/workflow.md`        |
| OLM packaging                          | NOT supported — explicit scope decision (no OperatorHub) |

## CI gating — one-line summary

| Event                                 | lint | unit | envtest | security | e2e |
|---------------------------------------|------|------|---------|----------|-----|
| push: feature branch (no PR)          |  -   |  -   |   -     |    -     |  -  |
| pull_request → main                   |  ✓   |  ✓   |   ✓     |    ✓     |  ✓  |
| pull_request → main (draft)           |  ✓   |  ✓   |   ✓     |    ✓     |  -  |
| push: main (post-merge)               |  -   |  -   |   -     |    -     |  -  |
| push: main `chore(release): v*`       |  -   |  -   |   -     |    -     |  -  (release.yml owns it) |

**PR-only invariant**: `ci.yml` triggers exclusively on `pull_request`.
Branch protection on `main` enforces PR workflow (required checks:
Lint / Unit / Envtest / Security / E2E (kind + helm + ginkgo); linear
history required; no force-push; no deletions). Post-merge SHA on main
has identical content to the PR head SHA, so re-running CI on push
main would be dup. Release commits are handled by `release.yml`.
Soak / leak / fuzz harnesses live in `nightly.yml` (04:00 UTC +
workflow_dispatch).

Docs-only PRs (paths-ignore: `**/*.md`, `docs/**`, `.planning/**`,
`references/**`, `FIX*.txt`, `LICENSE`, `NOTICE`, `CODEOWNERS`,
`.gitignore`) skip `ci.yml` entirely — required status checks therefore
will not report on docs-only PRs, so an admin merge or status check
override is needed to land them.

Detail and tradeoffs: `references/docs/workflow.md`.

## Toolchain — host has NO Go (always Docker)

The host has no `go`, `kubebuilder`, `controller-gen`, `kustomize`,
`setup-envtest`, or `golangci-lint` binary on PATH. **Every toolchain
target self-routes — the host needs only docker.** Run a bare
`make <target>` on the host; if the target needs the Go/helm toolchain it
re-invokes itself inside the devtools container via the `container_target`
macro (`LITELLM_IN_DEVTOOLS=1` short-circuits the nesting). See
`references/makefile.md` for the 3-context model.

```bash
make build-operator              # auto-routes into devtools
make gen-manifests               # auto-routes into devtools
make shell                       # interactive shell in the devtools container
./scripts/dev.sh go build ./...  # raw go, when no make target fits
```

- Wrapper mounts repo at `/workspace`, mounts `/var/run/docker.sock`,
  preserves host UID:GID, persists Go module + build caches under
  `.gocache/`, resolves `KUBEBUILDER_ASSETS`.
- Image: `litellm-devtools:latest` (built from `Dockerfile.devtools` on
  first use locally; force rebuild with `LITELLM_DEVTOOLS_REBUILD=1`).
- CI consumes a pre-baked image from GHCR
  (`ghcr.io/<owner>/litellm-devtools:<hash>`, where hash =
  `sha256(Dockerfile.devtools)[:12]`). `.github/workflows/devtools-image.yml`
  builds + pushes when `Dockerfile.devtools` changes;
  `.github/actions/setup-devtools` pulls in each CI job (~30s warm /
  2-3min cold saved per job × 5 jobs = ~10min/PR). On miss (first push,
  PR that changes Dockerfile.devtools racing the image workflow, GHCR
  unavailable), the composite action falls back to a local build — slower
  but always correct.
- Pinned: Go 1.26 (Dockerfile.devtools, Dockerfile, release.yml; go.mod
  `go 1.26.0` / `toolchain go1.26.3`), kubebuilder v4.4.0,
  controller-runtime v0.19.4, k8s.io/* v0.31.0, govulncheck v1.3.0. PR
  CI and release run the same toolchain (issue #43 close); any future
  bump MUST update all four surfaces in the same change.

Every toolchain target self-routes via the `container_target` macro — run
bare `make <target>` on the host; it re-invokes itself inside the devtools
container, and `LITELLM_IN_DEVTOOLS=1` short-circuits the nesting (so
`./scripts/dev.sh make <target>` from the host still runs ONE container,
not two). Never prefix a routed target with `./scripts/dev.sh` out of
habit — the prefix is redundant.

Only context-B/C targets run directly on the host: `docker-*` (host
docker), `wait-*` / `logs-*` / `watch-crs` / `pf-*` / `mock-mode` /
`operator-redeploy` (host kubectl against the kind kubeconfig), the gate
orchestrators (`pre-commit`, `pre-push`, `verify`), `release-*`, and
`ensure-inotify`. The `cluster-*` targets are NO LONGER host-direct: they
now route THROUGH the devtools container (they drive kind/helm via the
mounted docker socket), so run them bare too (`make cluster-up`).

## Test phases

| Phase              | Command                                | When                                  |
|--------------------|----------------------------------------|---------------------------------------|
| `make test-unit`        | pure-logic, ~5s warm                   | every iteration                       |
| `make qa-lint-changed`| golangci-lint scoped to touched pkgs   | every iteration                       |
| `make qa-lint`        | golangci-lint full sweep               | before commit (pre-commit hook)       |
| `make test-envtest` | controller-runtime envtest (race), ~7m | before commit on controller changes   |
| `make test-envtest-fast`| envtest without -race, ~3m            | dev inner loop                        |
| `make e2e-full`    | kind + Helm + Ginkgo, ~6m             | final gate before commit              |
| `make qa-security`    | gosec + govulncheck + fuzz-short, ≤6m | in-container; before push             |
| `make pre-commit`  | lint-changed + unit                   | host-only; runs on every `git commit` once `make hooks` installed |
| `make pre-push`    | full publication gate (secrets, filesystem, build hygiene, SPDX, lint + unit, ...) | host-only; runs on every `git push` once `make hooks` installed — manual invocation is a dry-run / verification only |

Umbrella targets:
- `make test-full` = `test-unit` + `test-envtest`
- `make verify` = `make qa-lint` + `make test-unit` + `make qa-security` + `make pre-push` (each self-routes; the gates stay host-only)
- `make hooks` installs `.git/hooks/pre-commit -> scripts/pre-commit-check.sh`
  AND `.git/hooks/pre-push -> scripts/pre-push-check.sh`

`make pre-push` is host-only — it spawns gitleaks/trufflehog containers
on host docker. Do NOT call it via `./scripts/dev.sh` (would nest docker
mounts that don't resolve). The same applies to `make pre-commit`.

Inner-loop iteration helpers:
- `make test-unit-pkg PKG=./internal/litellm/...`
- `make test-envtest-pkg PKG=./internal/controller/... [FOCUS=TestX] [TIMEOUT=10m]`
- `make qa-lint-changed [BASE_REF=...]` (lints only packages touched vs `origin/main`)

## Documentation site (mkdocs)

The public docs site at `docs/` is mkdocs-material based.

```bash
make gen-crd-ref-docs   # regenerate docs/api-reference/ from CRDs (auto-routes into devtools)
make docs-build         # build site/ via docker (host)
make docs-serve         # local preview at :8000
```

`docs/.crd-ref-docs.yaml` is the config for the `crd-ref-docs` tool
(installed via `make crd-ref-docs`); it targets the single API group
`litellm.ackstorm.ai`.

`docs/index.yaml` is the Helm chart index served by GitHub Pages; the
release workflow uses it as the chart catalogue at
`https://ackstorm.github.io/alitellm-operator/`.

`docs/.github/workflows/docs.yml` deploys the site to `gh-pages` on
pushes to `main` and on `v*` tags. PRs build the site (no deploy) to
catch broken links and missing pages.

## Release pipeline

Release artifacts are produced by **goreleaser** orchestrated by
`.github/workflows/release.yml`. The flow is **commit-message-driven
with tag-last**: a push to `main` whose head commit message starts with
`chore(release): v<MAJOR>.<MINOR>.<PATCH>` fires the pipeline. The
workflow then runs the tests, bumps manifests itself, builds + signs
artifacts, and creates the git tag as the final step — so a failure
anywhere upstream leaves origin with no orphan tag.

Cutting a release (stable example, `v0.1.0`):

```bash
# Most common — empty release commit (no manifest pre-bump).
# `make release-cut` runs preconditions (on main, clean tree, in-sync
# with origin/main), creates `chore(release): v0.1.0` as an empty
# commit, runs the pre-push gate, and pushes to main.
make release-cut VERSION=0.1.0

# Bundle the release intent with a real change:
# (edit, then commit the change yourself, then:)
git commit -am 'chore(release): v0.1.0'
git push origin main   # installed pre-push hook runs the gate automatically
```

There is no need to `make release-bump` locally or to create the tag yourself.
`make release-bump VERSION=X.Y.Z` is still available as the internal target
release.yml invokes; it can also be run by hand if you want to pre-bump
manifests in the same commit (the workflow detects the clean tree and
skips its own bump step), but it is not the expected workflow.

Per-release flow (after the `chore(release): v0.1.0` push):

1. **parse** job (job-level `if` skips non-release pushes): pulls
   `X.Y.Z` from the head commit message via regex.
2. **run-tests** job: `make test-full` (`test-unit` + `test-envtest` = race-enabled).
   Failures stop the pipeline here — no manifest mutation, no tag.
3. **build-and-release** job:
   - Configures the github-actions[bot] identity.
   - Runs `make release-bump VERSION=X.Y.Z`, commits the four bumped manifests
     to `main` with a `[skip ci]` marker, and pushes the bot commit.
     If the tree is already clean (user pre-bumped), this is a no-op.
   - Picks the goreleaser config:
     - `vX.Y.Z`                  → `.goreleaser.yml`            (stable)
     - `vX.Y.Z-{alpha,beta,rc}*` → `.goreleaser.prerelease.yml`
   - `make gen-code gen-manifests` regenerates CRDs (sanity).
   - cosign + cyclonedx-gomod installed on PATH (HRD-09).
   - goreleaser runs with `GORELEASER_CURRENT_TAG=v<X.Y.Z>` (no git
     tag at HEAD yet). The GitHub release-create API call auto-creates
     the tag at default-branch HEAD, which is the bot-bump commit.
     - cross-builds amd64 + arm64 (CGO_ENABLED=0, distroless runtime).
     - builds multi-arch manifest list at
       `ghcr.io/ackstorm/alitellm-operator:vX.Y.Z` (+ `:latest` on
       stable).
     - `sboms:` block generates the CycloneDX SBOM via cyclonedx-gomod.
     - `signs:` block signs the checksums file with cosign keyless OIDC.
     - `docker_signs:` block signs all image artifacts (per-arch +
       manifest list) with cosign keyless OIDC.
   - Pushes the chart to
     `oci://ghcr.io/ackstorm/charts/alitellm-operator:<X.Y.Z>`.
   - **LAST**: idempotently creates and pushes the annotated git tag
     `v<X.Y.Z>`. If goreleaser's release API call already implicitly
     created the tag, this is a no-op.

Orphan-tag posture: tag-creation is the LAST step. A failure in tests
or bump or goreleaser leaves no tag on origin and no GH release
attached to one. The bot bump commit may be on `main` if the failure
happened in goreleaser — that is reversible by reverting the bot
commit or by simply running the next release attempt, since `make release-bump`
inside the workflow is idempotent.

Snapshot builds (`.goreleaser.snapshot.yml`) are intentionally NOT
signed and do NOT generate SBOMs — they are ephemeral dev artifacts
pushed as `ghcr.io/ackstorm/alitellm-operator:main` +
`:main-<shortcommit>`.

`docker_signs:` and `signs:` blocks require:
- `id-token: write` in the workflow (already set).
- cosign on PATH (release.yml installs via `sigstore/cosign-installer`).

## Publication — pre-commit and pre-push gates are non-negotiable

Public remote: `git@github.com:ackstorm/alitellm-operator.git`. The
local gate strategy splits across two hook stages so the cost of
"oops, CI failed lint" is paid locally before the commit even lands:

- `pre-commit` (`make pre-commit`) — fast: `make qa-lint-changed`
  (golangci-lint scoped to touched packages) + `make test-unit`. Runs on
  every `git commit` once `make hooks` is installed. Bypass with
  `--no-verify` only for justified WIP commits; full lint + unit still
  fire on push as defensive gates.
- `pre-push` (`make pre-push`) — full publication check. Runs on
  every `git push` once `make hooks` is installed; do NOT invoke
  `make pre-push` manually before `git push` as a "belt and braces"
  step — the hook will fire it automatically and a manual call just
  double-spends the ~6 min gate. Manual invocation is reserved for
  dry-runs (verifying a WIP branch is push-ready without actually
  pushing). The authoritative gate list lives in
  `scripts/pre-push-check.sh`; the categories below are intent, not
  inventory, so the script can evolve without doc drift.

Gate categories — any failure blocks push:
- **Secret scanners**: gitleaks + trufflehog over `origin/main..HEAD`
  (full history on first push). Allowlist: `.gitleaks.toml`.
- **Filesystem hygiene**: large-file caps, sensitive patterns
  (`.env`, `*.pem`, `*.key`, kubeconfig, ...), required top-level files
  (LICENSE, README), `.gitignore` coverage.
- **Repo identity**: origin remote matches expected, branch up to date.
- **Build hygiene**: `go mod tidy` drift, govulncheck ack-list 1:1
  match (`scripts/govulncheck-gate.sh` against
  `references/security/govulncheck-acknowledged.md`).
- **Code provenance**: per-file SPDX header
  (`// SPDX-License-Identifier: Apache-2.0`).
- **Defense in depth**: full `make qa-lint` + `make test-unit` re-run inside
  the devtools container, even when `pre-commit` already covered the
  touched packages.
- **Informational warns** (do not block): commit-author summary,
  urgent TODO / DO-NOT-COMMIT markers, working-tree status.

If a gate fails, fix the root cause — never `--no-verify` or otherwise
bypass. Note: `--no-verify` skips ONLY the local hook; it does not
exempt CI, which reruns the same gates.

## Waiting for state — use blessed make targets

Naked polling loops (`until ...; do sleep N; done`) are banned: a
disappearing target makes the predicate unreachable, hanging the agent.
Use these Makefile targets instead:

| Need                                    | Target                                              |
|-----------------------------------------|-----------------------------------------------------|
| CR condition Ready                      | `make wait-cr-ready KIND=... NAME=... NS=...`       |
| Operator Deployment Ready               | `make wait-operator`                                |
| LiteLLM Deployment Ready                | `make wait-litellm`                                 |
| Mock pods Ready                         | `make wait-mocks`                                   |
| Container exit + PASS/FAIL marker       | `make wait-container NAME=<container>`              |
| Full cluster hydration                  | `make cluster-up` (synchronous; do not poll after)  |
| Operator hot-reload + Ready             | `make operator-redeploy` (bounded `rollout status`) |

Default `WAIT_TIMEOUT=300s` (override per call). `wait-container` takes
`TIMEOUT=<seconds>` (default 600).

Blessed wait patterns (every `wait-*` uses one):
1. `kubectl wait --timeout=...`
2. `kubectl rollout status --timeout=...`
3. `timeout N docker logs -f <cid> | grep -m1 ...`
4. `docker wait <cid>`

If a needed wait isn't covered, **add a new `wait-*` target** — targets
are the contract; ad-hoc loops aren't.

## Common failure modes

### ❌ Prefixing a `make` target with `./scripts/dev.sh` out of habit
```bash
./scripts/dev.sh make test-unit
```
✅ Run the target bare — it auto-routes:
```bash
make test-unit          # routes itself into devtools via container_target
```
The prefix is redundant: every toolchain target wraps itself with the
`container_target` macro, and the `LITELLM_IN_DEVTOOLS=1` guard in
`scripts/dev.sh` prevents a nested container, so the prefixed form still
runs ONE container and succeeds — it just adds noise. If docker is down
you get a clear preflight error from `scripts/dev.sh`, not a cryptic
missing-`go`-binary failure.

### ❌ Naked polling loop
```bash
until docker logs $(docker ps -q -f ancestor=mock) | grep -q PASS; do
  sleep 10
done
```
✅ Bounded wait via blessed pattern:
```bash
timeout 600 docker logs -f $cid 2>&1 | grep -m1 -E "PASS|FAIL" || {
  echo "FAIL: marker not seen within 600s" >&2; exit 1;
}
```
WHY IT FAILS: When the container exits and is removed, `docker ps -q`
returns empty; `docker logs` errors forever; the loop never exits.

### ❌ Enterprise-only LiteLLM fields on OSS image
```yaml
spec:
  tags: [team-alpha, prod]   # rejected — HTTP 403 "Enterprise users only"
```
✅ Use `metadata` for free-form pass-through:
```yaml
spec:
  metadata:
    team: alpha
    env: prod
```
WHY IT FAILS: LiteLLM 1.83.10 OSS rejects `tags:` on `POST /team/new`.
Same for any `*-enterprise-*` Helm value.

### ❌ Re-running the full E2E suite from scratch for every code change
```bash
make e2e-full       # full cluster-up + suite each time
```
✅ Use the dev loop against the kept cluster:
```bash
make cluster-up                # once (cluster KEPT)
make e2e-focus FOCUS="rateLimits"   # ~30s-2min per iter
make operator-redeploy         # hot-reload after a code edit
```
WHY: `e2e-full` runs `cluster-up` (slow) before the suite. After the
first run the cluster is KEPT (no teardown), so re-run only the focused
test or hot-reload the operator instead of paying cluster-up again.
Teardown is explicit: `make cluster-down`.

### ❌ Pushing without the pre-push hook installed
```bash
git push origin main --no-verify
```
✅ Install the hook once per clone; never use `--no-verify`:
```bash
make hooks          # idempotent; symlinks .git/hooks/pre-push -> scripts/pre-push-check.sh
git push origin main   # the hook runs make pre-push automatically before the push leaves the host
```
Manual `make pre-push` is a dry-run / sanity-check only — `git push`
already invokes the gate via the installed hook, so running it
explicitly before pushing is redundant.
WHY IT FAILS: Pushed secrets, license-header drift, govulncheck advisory
regressions cannot be untrue-d from public history. The pre-push gate
script is the contract; the git hook makes it unmissable.

### ❌ Kubectl from host against the kind cluster
```bash
kubectl get pods -n default
# context not found
```
✅ Go through the devtools container:
```bash
./scripts/dev.sh kubectl get pods -n default
```
WHY IT FAILS: The kind kubeconfig lives at
`/workspace/.gocache/kube/config` — inside the container. Host kubectl
has no context for the kind cluster.

### ❌ Editing files via relative paths when cwd is the wrong repo
```bash
cat > internal/controller/scope_ac_n4_test.go <<EOF ...
# silently writes to a sibling repo if that is cwd
```
✅ Always use absolute paths for Edit/Write, and `cd /home/jcm/Projects/alitellm-operator`
before bash operations. Verify with `pwd && git log --oneline | head -3`.
WHY IT FAILS: Sibling repos with similar layouts live next to this one.
Relative-path edits to the wrong tree leave this repo unchanged while
appearing to "succeed."

### ❌ `spec.params` top-level keys silently dropped on MCPServer
```yaml
spec:
  params:
    auth_type: api_key       # forwarded since v0.3.1
    access_groups: [team-a]  # accepted alias of mcp_access_groups
    server_name: hijack      # IGNORED (reserved structural key)
```
✅ As of v0.3.1, every key modeled in `litellm.MCPServerRequest` is
forwarded. Reserved structural keys (`server_id, server_name, alias, url,
transport, spec_path`) are dropped at extraction. Pre-v0.3.1 the
controller forwarded only `mcp_info`, `extra_headers` (map form),
`static_headers`, `description`.
WHY IT FAILED PRE-v0.3.1: `mcpserver_controller.go` extracted only four
typed fields and dropped everything else, even though the request struct
already modeled the full set.

### ❌ Expecting LiteLLM error detail in CR condition.Message
```yaml
status:
  conditions:
  - type: Ready
    status: "False"
    reason: LiteLLMRejected
    message: 'LiteLLM rejected model create: 400 (code=400)'   # generic
```
✅ Opt in if your environment is non-secret-bearing:
```bash
kubectl set env -n litellm-system deploy/alitellm-operator \
  LITELLM_OPERATOR_DANGEROUSLY_INCLUDE_REJECTED_BODY=true
```
Note: `LITELLM_OPERATOR_DANGEROUSLY_INCLUDE_REJECTED_BODY` (controls CR
`status.conditions[].message` content) is distinct from
`LITELLM_OPERATOR_DANGEROUSLY_LOG_BODIES` (controls transport-layer log
redaction) — they govern different surfaces and must be flipped
independently.
WHY IT IS GENERIC BY DEFAULT: LiteLLM error envelopes can echo inbound
payload fields (param echo, JSON-decode error citing a value). Operator
cannot know in general which fields carry provider secrets, so the
envelope body never lands in CR status. The actionable detail is in
operator logs (transport-layer-redacted).

## Repository-specific patterns

- **Reconciler shape**: each controller in `internal/controller/<kind>_controller.go`
  follows `Reconcile(ctx, req) (Result, error)`, calls `internal/litellm/<kind>_request.go`
  for HTTP construction, applies status conditions via `meta.SetStatusCondition`.
  WHY: separates k8s reconcile loop from LiteLLM HTTP surface for unit
  testability — the request constructor is pure data; the reconciler
  owns side effects.

- **Field-selector index constants**: `internal/controller/<kind>_controller.go`
  declares e.g. `const teamNameIndexerKey = "spec.params.team_alias"`
  for indexer registration. `// #nosec G101` comments justify each
  (gosec misidentifies them as credentials).

- **Fuzz seed corpus** at `internal/<pkg>/testdata/fuzz/<TargetName>/`
  carries one entry per SEC regression test. Adding a new regression
  test? Add a matching corpus entry.

- **SPDX-only license headers**: every `*.go` outside `vendor/`,
  `zz_generated*.go`, `mock_*.go` starts with
  `// SPDX-License-Identifier: Apache-2.0`. Pre-push gate 15 enforces.
  `hack/boilerplate.go.txt` provides the header for controller-gen
  output; `make gen-code` wires it in via `object:headerFile=`.

- **govulncheck ack-list**: stdlib HIGH advisories awaiting Go 1.25.x
  fixes live in `references/security/govulncheck-acknowledged.md` (note:
  `references/`, not `docs/`, since the ack-list is agent-facing internal
  documentation that is not published on the mkdocs site). The gate
  script enforces the reachable set matches this list exactly — drift
  in either direction blocks push.

- **AC-N4 (SCOPE-04) envtest fixture**: `internal/controller/scope_ac_n4_test.go`
  creates a Model CR in `outOfWatchNamespace = "ac-n4-out-of-watch"`
  (NOT `WatchNamespace`) and asserts no reconcile fires. The namespace
  is provisioned on demand because envtest's apiserver does not
  pre-create custom namespaces. Do not collapse the namespace to
  `WatchNamespace` — the test becomes vacuous.

## E2E debug loop

`make e2e-full` is the final gate (~10 min). It runs `cluster-up` then
the suite and KEEPS the cluster (teardown is explicit `make cluster-down`),
so the same command doubles as the entry point to the kept-cluster loop:

```bash
# 1. Bring cluster up + run the suite once; cluster is KEPT after the run.
make e2e-full
# = cluster-up (with ensure-inotify) + e2e-run (NO teardown after)

# 2. Diagnose live (cluster is up)
make logs-operator                       # tail operator logs (host kubectl)
make wait-cr-ready KIND=team NAME=<name> NS=default

# 3. Iterate with focused tests
make e2e-focus FOCUS="rateLimits composite"
make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestTeamReconciler_AC_T4

# 4. Code change → hot-reload → re-test (~30s)
make operator-redeploy
make e2e-focus FOCUS="..."

# 5. Final gate before commit (full suite from a fresh cluster)
make cluster-reset       # down + up
make e2e-full
```

Never push a change touching `internal/controller/`, `internal/litellm/`,
`api/litellm/v1alpha1/`, `deploy/helm/alitellm-operator/`, or `test/e2e/`
without confirming E2E green.

## External references

For up-to-date API info beyond this project's docs:
- **controller-runtime / kubebuilder**: Context7 or DeepWiki for v0.19.x
  APIs (reconciler patterns, manager setup, indexer builders).
- **client-go / k8s.io/* @ v0.31.0**: Context7 for typed-client method
  signatures.
- **LiteLLM HTTP API**: WebFetch against the upstream OpenAPI at
  `https://docs.litellm.ai/` — the bundled spec in this repo is a
  frozen snapshot.
- **Ginkgo v2**: Context7 for current `Describe/Context/It` patterns and
  the `--label-filter` syntax.
- **goreleaser v2**: https://goreleaser.com — pay attention to the
  `dockers` → `dockers_v2` migration warning (currently deferred; both
  configs validate today).
