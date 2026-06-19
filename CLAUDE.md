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
│   ├── workflows/           CI / release / docs / govulncheck / labeler
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
│   └── e2e/cluster/         standing-hydration kustomize phases (00-namespaces,
│                            01-deps, 02-operator, 03-mocks, 04-hydration);
│                            test CRs stay dynamic in test/e2e/*_test.go
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
- Pinned: Go 1.26.4 (Dockerfile.devtools `golang:1.26.4-bookworm`,
  Dockerfile `golang:1.26.4`, release.yml `GO_VERSION: '1.26.4'`; go.mod
  `go 1.26.0` / `toolchain go1.26.4`), kubebuilder v4.4.0,
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
docker), `wait-*` / `logs-*` / `watch-crs` / `pf-*` / `mock-mode`, the gate
orchestrators (`pre-push`, `verify`), `release-*`, and
`ensure-inotify`. `operator-redeploy` builds + `kind load`s on the host but
runs its `kubectl rollout restart/status` THROUGH the devtools container
(`./scripts/dev.sh kubectl`) — host kubectl has no context for the kind
cluster (kubeconfig lives at `/workspace/.gocache/kube/config`). The `cluster-*` targets are NO LONGER host-direct: they
now route THROUGH the devtools container (they drive kind/helm via the
mounted docker socket), so run them bare too (`make cluster-up`).

## Test phases

| Phase              | Command                                | When                                  |
|--------------------|----------------------------------------|---------------------------------------|
| `make test-unit`        | pure-logic, ~5s warm                   | every iteration                       |
| `make qa-lint-changed`| golangci-lint scoped to touched pkgs   | every iteration                       |
| `make qa-lint`        | golangci-lint full sweep               | before commit; re-run in pre-push gate |
| `make test-envtest` | controller-runtime envtest (race), ~7m | before commit on controller changes   |
| `make test-envtest-fast`| envtest without -race, ~3m            | dev inner loop                        |
| `make e2e-full`    | kind + Helm + Ginkgo, ~6m             | final gate before commit              |
| `make qa-security`    | gosec + govulncheck + fuzz-short, ≤6m | in-container; before push             |
| `make pre-push`    | full publication gate (secrets, filesystem, build hygiene, SPDX, lint + unit, ...) | host-only; runs on every `git push` once `make hooks` installed — manual invocation is a dry-run / verification only |

Umbrella targets:
- `make test-full` = `test-unit` + `test-envtest`
- `make verify` = `make qa-lint` + `make test-unit` + `make qa-security` + `make pre-push` (each self-routes; the gates stay host-only)
- `make hooks` installs `.git/hooks/pre-push -> scripts/pre-push-check.sh`
  (and removes any stale pre-commit hook from a prior install)

`make pre-push` is host-only — it spawns gitleaks/trufflehog containers
on host docker. Do NOT call it via `./scripts/dev.sh` (would nest docker
mounts that don't resolve).

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

## Publication — the pre-push gate is non-negotiable

Public remote: `git@github.com:ackstorm/alitellm-operator.git`. A single
local hook stage pays the cost of "oops, CI failed lint" before the push
leaves the host (the former fast `pre-commit` gate has been retired —
lint + unit now run only in the pre-push gate and in CI):

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
- **Defense in depth**: full `make qa-lint` + `make test-unit` run inside
  the devtools container on every push.
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
| Re-apply phases in place (kept cluster) | `make cluster-sync` (re-applies all phases + `verify`) |
| Health-gate standing state (no mutation)| `make cluster-verify`                               |
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

### ❌ Assuming `ModelDiscovery.spec.baseUrl` is fully SSRF-guarded
```yaml
spec:
  type: kubeai
  baseUrl: http://169.254.169.254/   # DENIED (cloud metadata) → Ready=False InvalidConfig
  # baseUrl: http://10.0.0.5:8000/v1 # ALLOWED by design (private RFC1918)
  # baseUrl: http://svc.ns.svc/v1    # ALLOWED by design (in-cluster)
```
✅ `providers.ValidateBaseURL` (M-SEC1) is a **denylist**, not full SSRF
prevention. It rejects only cloud-metadata (`169.254.169.254`,
`fd00:ec2::254`), loopback, and link-local hosts plus structural problems
(non-http(s) scheme, userinfo, query, fragment). Private RFC1918 and `*.svc`
hosts remain reachable BY DESIGN — KubeAI points `baseUrl` at in-cluster
service DNS, so a blanket internal-deny would break the supported use case.
RESIDUAL RISK: a namespaced user can still aim the operator's HTTP client at
other internal/private services. A host-allowlist is intentionally deferred.

### ❌ Master key over plaintext `http://` to a remote LiteLLM (M-SEC2)
`spec.endpoint: http://api.example.com` sends the master key
(`Authorization: Bearer`) in cleartext. By default the connection reconciler
only WARNS (log marker `MasterKeyOverPlaintextHTTP`) — it does NOT flip
Ready to False, because in-cluster `http://litellm.<ns>.svc` is the common,
acceptable deployment and is classified secure-enough (loopback + `*.svc` +
bare service names are exempt). To hard-reject plaintext-http remotes:
```bash
kubectl set env -n litellm-system deploy/alitellm-operator \
  LITELLM_OPERATOR_REQUIRE_HTTPS_REMOTE=true
```
With the flag set, a remote `http://` endpoint yields
`Ready=False, reason=InsecureEndpoint` (terminal; edit spec.endpoint to
retrigger). Classification logic: `litellm.ClassifyEndpointTransport`.

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

### ❌ E2E cross-check via a one-shot `kubectl run --rm -i` curl pod
```go
out, err := exec.Command("kubectl", "-n", ns, "run", podName,
    "--rm", "-i", "--restart=Never", "--quiet",
    "--image=curlimages/curl:8.10.1", "--", "curl", "-sS", url,
).CombinedOutput()
Expect(err).NotTo(HaveOccurred())      // passes — exit 0
idx := bytes.IndexByte(out, '{')       // -1: body is empty
```
✅ Probe through the retrying helpers in `test/e2e/curl_helpers_test.go`:
```go
body := curlPodJSON(ns, "probe", '{', "curl", "-sS", url)   // retries until marker
// non-JSON payloads (Prometheus text): curlPodBody(ns, "probe", accept, ...)
```
WHY IT FAILS: `kubectl run --rm -i` attaches via an HTTP connection upgrade
that can lose the short-lived curl container — `unable to upgrade
connection: container <p> not found in pod <p>` — and the log-streaming
fallback then captures nothing, yielding EMPTY stdout with exit code 0. The
curl process still ran (POST side effects land), but a body the spec needs
to PARSE is gone. Different specs flake each run under cluster load.
`curlPodJSON` / `curlPodBody` retry until the expected payload appears.
POST-only sites (no body parse) don't need the helper — the side effect
lands regardless of attach.

### ❌ Routing a *confirmed-absent* LiteLLM delete through `onAckMissing`
```go
// deletion path, status.lastRendered.modelID == ""
case err == nil && resolved == nil:   // name-resolve: 404 / empty data[]
    onAckMissing("name-resolve returned not-found")  // under Delete → blocks
```
A CR rejected on create (HTTP 422, e.g. missing `model`) never gets a
`modelID`, so the finalizer-time delete falls to name-resolve, which
returns empty → the entry is **confirmed absent**. Routing that through
`onAckMissing` strands the CR in `Terminating` forever under
`deletionPolicy: Delete` (`error="delete blocked: name-resolve returned
not-found; entry already absent"`, controller-runtime backoff loop).
✅ Confirmed-absent drains the finalizer regardless of policy:
```go
case err == nil && resolved == nil:
    onConfirmedAbsent("name-resolve returned not-found", model.Name)  // falls through to RemoveFinalizer
```
WHY: `onAckMissing` exists to gate `Delete` on *cannot-confirm* states
(LiteLLM unavailable, 401) — we genuinely don't know if the entry still
exists. A definitive 404 / empty `data[]` (or 404 on `POST /model/delete`,
`litellm.IsNotFound`) is positive proof the entry is gone, so the Delete
goal is already met. The sibling controllers (a2aagent/mcpserver) already
drain unconditionally on name-resolve-empty; the model controller now
matches. Break-glass for an already-stuck CR (no operator redeploy):
annotate `litellm.ackstorm.ai/deletion-policy-override=Orphan`.

### ❌ Gating a LiteLLM mutation on `snap.Ready` alone
```go
snap := r.Cache.Snapshot()
if !snap.Ready {              // passes a Ready+nil-Client snapshot through
    return notReady(...)
}
snap.Client.GetRouterSettings(ctx)   // nil deref → recovered panic, poisoned reconcile
```
✅ Gate on `ConnectionSnapshot.Usable()` (Ready AND Client != nil):
```go
if !snap.Usable() {          // Ready+nil-Client takes the not-Ready path
    return notReady(...)
}
snap.Client.GetRouterSettings(ctx)   // Client guaranteed non-nil here
```
WHY IT FAILS (#74): `Cache.Rebuild` does NOT enforce the
`Ready=true ⇒ Client!=nil` invariant — it stores whatever snapshot it is
handed. Production always pairs Ready with a fresh client, but envtest
cleanup that rebuilt the shared singleton with
`ConnectionSnapshot{Ready: true, Reason: "Synced"}` (no Client) poisoned
the manager-level cache; the next reconcile of an always-on singleton
(`modelalias`) dereferenced the nil `snap.Client` and panicked
(controller-runtime recovers it, so it surfaced as a shuffle-dependent
flake, not a crash). All six dependent reconcilers now gate on
`snap.Usable()`; tests restore Ready state via `setConnCacheReady()`
(suite-mock-backed client), never a bare `Ready:true` literal. NOTE: a
SEPARATE class of `-shuffle` flakes (tracked under #74) lives in the
controller suite — it surfaces on the release gate (`make test-full`,
`-race -shuffle=on` over the WHOLE controller package at once, heavier
than the per-package PR Envtest job), not on PR CI.
- **mockServer POST-count bleed → at-least-once over-count.** The
  reconcile loop is at-least-once: the 100ms safety-relist runnables can
  fire a redundant idempotent mutation off cache-stale status before the
  prior write propagates. Tests that asserted an EXACT mutation count
  (`POST /model/update == 1`, `PUT .../mcp|agents == 1`, post-CREATE
  `MutationsBy*Name == 1`) flaked to 2. FIX (2026-06-11): assert the
  drift-correction SHAPE — `>=1` of the expected verb, with `delete`/`new`
  kept exact-zero — never an exact mutation count. A redundant idempotent
  write is harmless in production (relist is 30m there).
- **Relist-recovery deadline too tight under release load.**
  `TestGuardRail_SafetyRelist_CreateMissing` waited 15s for safety-relist
  recovery; the full-suite gate's workqueue contention slipped past it.
  Bumped to 30s (loop still breaks on first success).
- **connection-reason `Absent` vs `Unreachable`** ordering flake — still
  open under #74, not yet addressed.
- **Suite-global relist gated OFF by default (2026-06-14, #74 systemic
  fix).** The model + guardrail `SafetyRelistRunnable`s ticked at 100ms for
  the whole package run (~30 enqueues/s into the shared workqueue) — the
  contention floor behind the timing flakes. They now carry a nil-safe
  `Gate func() bool` (production leaves it nil → identical behavior) and the
  envtest suite defaults them OFF via `suiteRelistEnabled`; only the ~2
  drift-recovery tests opt in with `enableSuiteRelist(t)`. The
  `TeamDefaultRunnable` is NOT gated (it enqueues one deduped `Team/default`
  request per tick — implicit-default/bootstrap tests depend on it). This
  fix alone removed the two headline #74 flakes
  (`TestModel_SpecParamsKeyRemoval_DeleteAndRecreate` at-least-once
  over-count; `TestConnectionProbeLoop_BadMasterKey` anti-storm) on a fixed
  `-race -shuffle` seed (7→5 failures, 0 regressions). New controller tests
  must NOT assume background relist fires unless they call
  `enableSuiteRelist(t)`. The remaining 5 `-race -shuffle` failures are
  PRE-EXISTING test-design/contamination races (MCPServer
  `CreateOnFirstReconcile` / `OwnerRefTolerance` /
  `DriftSuppressedOnFirstCreate`, Team `DriftSuppressedOnFirstCreate`,
  MCPServer `UpdateForwardsAllParams`) — tracked for a dedicated debug pass,
  NOT caused by the gating change.
NOTE: local envtest on a resource-starved host can fail at suite SETUP
(`WaitForCacheSync: did not sync within 30s`) or mass ~30s poll-timeouts
— that is environmental host starvation, NOT a code regression; verify on
CI's Envtest job, which is the reliable signal.

### ❌ LiteLLM proxy without `STORE_MODEL_IN_DB` → every Model 500s
```yaml
status:
  conditions:
  - type: Ready
    status: "False"
    reason: LiteLLMRejected   # POST /model/new returned 500
```
LiteLLM logs:
```
model_management_endpoints.py:957 - add_new_model(): Exception occured - 500: {'error': "Set `'STORE_MODEL_IN_DB='True'` in your env to enable this feature."}
```
✅ The proxy must persist models in its DB before it will accept
`POST /model/{new,update}`. Set ONE of these on the LiteLLM deployment
(not the operator):
```yaml
# env var
env:
  STORE_MODEL_IN_DB: "True"
# OR general_settings (proxy config.yaml / Helm values)
general_settings:
  store_model_in_db: true
```
WHY IT FAILS: with `STORE_MODEL_IN_DB` unset, LiteLLM's
`add_new_model` rejects all model-create/update calls before touching
the body — so EVERY `LiteLLMModel` (and Discovery-generated model) goes
`Ready=False`. Operator-side config is correct; the gap is upstream
LiteLLM. The e2e cluster already sets this
(`test/e2e/cluster/01-deps/litellm.values.yaml`). Teams/MCP/A2A CRs are
unaffected — only model endpoints gate on the flag.

### ❌ Expecting LiteLLM to fill model `created_at` → UI shows "Unknown date"
```
Models UI row:  alitellm-operator/0.7.8   Unknown date
```
✅ The operator must stamp `model_info.created_at` / `model_info.updated_at`
itself on CREATE — same mechanism as the `created_by` stamp (FIX2 M-8).
`model_controller.go` sets all four on the `POST /model/new` body
(RFC3339 UTC). Adopted/out-of-band rows cannot be back-stamped.
WHY IT FAILS: in OSS (non-premium) LiteLLM, `proxy_server.get_model_info_with_id`
copies the DB `created_at`/`updated_at` columns into `model_info` ONLY when
`premium_user is True` (Enterprise). Non-premium UIs read the date straight
from the `model_info` JSON blob, so a blob without `created_at` renders
"Unknown date". `POST /model/new` (`_add_model_to_db`) is the ONLY endpoint
that persists the `model_info` blob — `POST /model/update` (`update_model`)
rewrites `litellm_params` + the `updated_by` DB column ONLY and never touches
the blob, so the timestamp cannot be added/refreshed on the UPDATE path.
Stamp it on CREATE or it never appears.

### ❌ `model_info` changes (e.g. `access_groups`) silently dropped on UPDATE
```yaml
# Adding a model_info value/key to an existing model:
spec:
  info:
    access_groups: ["anthropic"]   # added after the model was first created
```
Symptom: the CR reports `Ready=Synced`, but LiteLLM's DB `model_info` blob
still lacks the new value — because a non-shrinking `model_info` change used
to take the plain `POST /model/update` path, which never rewrites the blob
(same root cause as the `created_at` entry above — only `POST /model/new`
persists `model_info`).
✅ The operator now tracks `status.lastRendered.infoHash` (SHA-256 of the
rendered `model_info`, excluding the `id` overlay) and forces a
delete+recreate (`POST /model/delete` + `POST /model/new`) on ANY `model_info`
change, so the blob lands in LiteLLM. The `currentRenderedHash` already covered
`model_info` values, but it only short-circuits the steady state; the UPDATE
branch needs `infoHash` to distinguish a blob change from a params-only change.
MIGRATION: an empty stored `infoHash` (pre-upgrade status) is backfilled
silently (treated as unchanged) to avoid a mass recreate of every model on
operator upgrade. CONSEQUENCE: models whose blob is ALREADY stale in LiteLLM
(e.g. `access_groups` added before this fix shipped) are NOT auto-healed —
delete the entry in LiteLLM once and the operator recreates it fresh via
`POST /model/new`. Forward changes are handled correctly.

### ❌ Model created with no access group → unreachable by group-scoped keys
Symptom: a `LiteLLMModel` (or Discovery-generated child) with no
`spec.info.access_groups` lands in LiteLLM belonging to NO access group, so
virtual keys scoped by access group can't see it.
✅ The operator injects a default access group into the rendered `model_info`
when the model declares none (absent OR empty list). The group name is set by
`DEFAULT_ACCESS_GROUP` (default **`default`**); empty disables injection. The
injection happens at a single point in `model_controller.go` BEFORE the
`infoHash`/`renderedHash` are computed, so it covers both standalone
`LiteLLMModel` CRs AND Discovery-generated children (children are
`LiteLLMModel`s reconciled by the same controller), is persisted on the
create/update body, and survives the steady-state hash compare. An explicit
non-empty `spec.info.access_groups` is never overridden.
```bash
kubectl set env -n litellm-system deploy/alitellm-operator \
  DEFAULT_ACCESS_GROUP=default   # or "" to disable injection
```

### ❌ Router model (`auto_router/…`) recreates forever, new modelID every reconcile
```yaml
spec:
  params:
    model: auto_router/complexity_router   # or any auto_router/* pseudo-model
    complexity_router_config: { tiers: { SIMPLE: ackstorm.fast, ... } }
    complexity_router_default_model: ackstorm.lite
```
Symptom: CR flips a fresh `status.lastRendered.modelID` every ~second,
never visible in the LiteLLM UI / `GET /model/info`, operator log spams
`safety re-list detected out-of-band delete; clearing ID` then
`model created in LiteLLM`. **The model actually works for inference** —
it is only invisible to the operator.
✅ Handled since the router-aware reconcile: `isRouterModel` (litellm_params
`model` prefix `auto_router/`) skips the Step 7b existence probe, so the CR
reaches a stable `Ready=Synced` with the first-create id retained.
WHY IT FAILED: LiteLLM stores router/auto-router/complexity-router
deployments in its in-memory router, NOT the DB model table, so
`GET /model/info?model_name=` (and `?litellm_model_id=`) never returns
them. The existence probe read empty → cleared the ModelID → re-POSTed
`/model/new` (LiteLLM answers *"already exists, ignoring"*, 200 + a fresh
id) → infinite churn. Routers are the canonical *created-but-not-listed*
class.

### ❌ A non-router model storms LiteLLM with `/model/new` (created-but-not-listed)
Same churn shape as the router case but for a model that genuinely never
persists — e.g. `use_in_pass_through: true` on a LiteLLM build that 200s
the create but discards it. These DON'T work for inference and can't be
made to via the operator.
✅ The recreate circuit breaker caps recreates at
`LITELLM_OPERATOR_RECREATE_LIMIT_PER_MIN` (default **10**) per CR per 60s
sliding window; on trip the CR is parked `Ready=False`,
`reason=RecreateThrottled` and requeued after 5m (self-heals if LiteLLM
behavior changes). Tune / inspect:
```bash
kubectl set env -n litellm-system deploy/alitellm-operator \
  LITELLM_OPERATOR_RECREATE_LIMIT_PER_MIN=10
kubectl get litellmmodel <name> -o jsonpath='{.status.conditions[?(@.type=="Ready")]}'
```
Router models never reach the breaker — they bypass the probe (above) and
sit Ready. NOTE: the breaker is wired on the **Model, MCPServer, Team, and
A2AAgent** controllers (the four that share the `probeVanishedResourceID`
vanish path + a CREATE-after-clear recreate site). Each parks its CR
`Ready=False/RecreateThrottled` and backs off `recreateThrottleBackoff` on
trip; `LITELLM_OPERATOR_RECREATE_LIMIT_PER_MIN` tunes all four. The Guardrail
controller does not have a vanish-recreate site and is unaffected.

## Repository-specific patterns

- **E2E standing hydration = numbered kustomize phases**: the e2e cluster's
  *standing* state (namespaces, helm values, mock backends, master-key Secret,
  LiteLLMConnection seam) lives under `test/e2e/cluster/` as numbered phase
  dirs (`00-namespaces`, `01-deps`, `02-operator`, `03-mocks`, `04-hydration`),
  applied by `scripts/cluster.sh` via `kubectl apply -k`. Each phase labels its
  objects `e2e: "true"` (kustomize `labels` with `includeSelectors: false`, so
  pod/Service selectors stay clean) for `kubectl delete -l e2e=true` cleanup on
  the KEPT cluster. **Adding standing state = drop a manifest + wire one
  `kustomization.yaml` line.** Test CRs are NOT here — they stay dynamic in
  `test/e2e/*_test.go` (created/patched/deleted at runtime by the specs). WHY
  hybrid: manifests give declarative standing state + label-scoped cleanup; Go
  keeps the create→patch→delete lifecycle test CRs need.

- **Reconciler shape**: each controller in `internal/controller/<kind>_controller.go`
  follows `Reconcile(ctx, req) (Result, error)`, calls `internal/litellm/<kind>_request.go`
  for HTTP construction, applies status conditions via `meta.SetStatusCondition`.
  WHY: separates k8s reconcile loop from LiteLLM HTTP surface for unit
  testability — the request constructor is pure data; the reconciler
  owns side effects.

- **Cross-controller shared helpers** live ONCE in
  `internal/controller/shared_helpers.go` — behavior shared by the
  Model/MCPServer/Team/A2AAgent/GuardRail reconcilers is NOT copy-pasted per
  controller (finding #14 consolidation): typed 4xx classification
  (`is4xxStatus` / `rejectedStatus`, `errors.As` on
  `litellm.RejectedError.Status` — survives error wrapping, unlike the retired
  error-string prefix scan), the deletion-path ack-missing factory
  (`newAckMissingFn`), the LiteLLM-mutation error classifier
  (`classifyMutationError` — each reconciler keeps a thin method that binds its
  CR via a `writeStatus` closure and delegates; team's closure no-ops on a nil
  CR for the synthetic implicit-default reconcile), the SEC-03 duplicate-`as`
  check (`checkDuplicateSecretAs`, taking the kind-specific message phrase
  verbatim), the secret-ref indexer extraction (`secretRefNames`), the
  optimistic-locked status-write core (`writeStatusWithRetry[T]` — all five
  `writeStatus` methods delegate; mcp/team standardized onto conflict-retry),
  and the safety-relist runnable (`SafetyRelistRunnable` + per-kind
  `ListModelRequests` / `ListGuardRailRequests`). Touching this behavior? Edit
  the shared helper, not five copies. The per-kind field-selector index
  CONSTANTS and `Index*SecretRefs` funcs stay per-controller (concrete
  `client.Object` required); only their extraction loop is shared.

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

# 4b. Standing-state change (values/manifests under test/e2e/cluster/) →
#     re-apply phases in place (no node recreate) then re-gate health
make cluster-sync        # re-applies all phases + verify
make cluster-verify      # standalone health gate (no mutation)

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
