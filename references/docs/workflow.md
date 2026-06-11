# Review workflow — push / PR / release map

Single-page reference for "what fires when I push or merge."
Companion to `CLAUDE.md` (sections: Release pipeline, Publication, CI
workflows) and to `.github/workflows/*.yml` (authoritative source).

## Tiered gating policy — PR-only

`ci.yml` triggers exclusively on `pull_request` against `main`. Branch
protection on `main` enforces PR-only merges (required checks:
Lint / Unit / Envtest / Security / E2E (kind + helm + ginkgo); linear
history; no force-push; no deletions). The post-merge `main` SHA is
content-identical to the PR head SHA — re-running CI on push to main
would be pure dup, so it is intentionally not wired.

| Event                                 | lint | unit | envtest | security | e2e |
| ------------------------------------- | ---- | ---- | ------- | -------- | --- |
| push: feature branch (no PR open)     |  -   |  -   |    -    |    -     |  -  |
| pull_request → main                   |  ✓   |  ✓   |    ✓    |    ✓     |  ✓  |
| pull_request → main (draft)           |  ✓   |  ✓   |    ✓    |    ✓     |  -  |
| push: main (post-merge, non-release)  |  -   |  -   |    -    |    -     |  -  |
| push: main, `chore(release): v*`      |  -   |  -   |    -    |    -     |  -  |

Rationale:

- **Feature branch push (no PR)**: zero CI runs. The local `pre-push`
  hook (`make pre-push`) gates the push (lint + unit + scanners + SPDX
  + govulncheck). Opening the PR fires the full suite once; subsequent
  pushes to the PR branch supersede via `cancel-in-progress: true`.
- **PR → main**: full pre-merge gate INCLUDING e2e. The PR is the
  merge boundary — a broken e2e here blocks merge instead of breaking
  main after the fact. Draft PRs skip e2e (the rest still run).
- **Post-merge main push**: 0 CI runs. Branch protection guarantees
  the merged commit's content was already validated as the PR head.
  Re-running envtest / security on the merge SHA would catch nothing
  net-new — they exercise the same code with the same dependencies.
- **Release commit (`chore(release): v*`)**: `ci.yml` not triggered
  (no push trigger). `release.yml` fires on push main and runs its
  own `make test-unit && make test-envtest-fast` sanity gate before goreleaser.
- **Drift paranoia** (new CVE published overnight, infra rot, etc.):
  not covered by `ci.yml`. `govulncheck.yml`'s weekly cron covers
  advisory drift; if more is ever needed, add a scheduled workflow
  that notifies on failure (a red cron nobody watches is worse than
  no cron — the retired `nightly.yml` failed 21/21 runs unnoticed).

### `paths-ignore` — docs-only PRs skip CI entirely

`ci.yml` declares `paths-ignore` on `pull_request`:

```yaml
paths-ignore:
  - '**/*.md'
  - 'docs/**'
  - '.planning/**'
  - 'references/**'
  - 'FIX*.txt'
  - 'LICENSE'
  - 'NOTICE'
  - 'CODEOWNERS'
  - '.gitignore'
```

A PR that touches ONLY these paths produces no `ci.yml` run
(`docs.yml` still fires for `docs/**` deploys). If a PR mixes a docs
file with code, the workflow fires normally.

**Branch protection caveat**: GitHub treats "workflow did not run"
distinctly from "workflow passed". Required status checks
(Lint / Unit / Envtest / Security / E2E) will appear as
missing-checks on docs-only PRs and block merge. Workarounds:
(a) admin merge (`enforce_admins: false` allows this), or
(b) drop `paths-ignore` and accept full CI cost on docs PRs, or
(c) add a no-op workflow that reports the same check names with
success on docs-only changes (skip-condition pattern).

## Lifecycle table

| Stage                         | Local command                                                                                         | Workflow(s) fired                                                                  | Job-level gate (`if:`)                                              | Outcome                                                                                                            |
| ----------------------------- | ----------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------- | ------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------ |
| Dev branch push (no PR)       | `git push origin <branch>`                                                                            | `govulncheck.yml` only                                                             | branch trigger                                                      | Zero `ci.yml` cost. Local `make pre-push` hook gates the push.                                                     |
| PR open / sync                | `gh pr create --base main`                                                                            | `ci.yml` (lint + unit + envtest + security + **e2e**), `docs.yml` build-test, `pr-labeler.yml` | `pull_request: main` (non-draft for e2e)                | PR blocked until all five required checks are green. Draft PRs skip e2e only.                                       |
| Merge PR to main              | `gh pr merge N --merge --delete-branch`                                                               | `docs.yml` deploys `latest`+`dev`. `ci.yml` NOT fired (no push trigger).           | n/a (`ci.yml` only listens to `pull_request`)                       | Zero `ci.yml` cost on merge. Merge SHA content already validated as PR head.                                       |
| Release commit                | `git commit --allow-empty -m 'chore(release): vX.Y.Z'` → `make pre-push` → `git push origin main`     | `release.yml` (active), `docs.yml`. `ci.yml` NOT fired (no push trigger).          | `startsWith(head_commit.message, 'chore(release): v')`              | release.yml runs unit + envtest-fast sanity, bumps manifests, goreleaser, cosign keyless OIDC, CycloneDX SBOM, helm OCI push, **tag created LAST**. |
| Tag push (rare manual)        | `git tag vX.Y.Z && git push origin vX.Y.Z`                                                            | `docs.yml` (mike deploy stable / preview alias)                                    | `startsWith(github.ref, 'refs/tags/v')`                             | Versioned doc set deployed; container/chart NOT rebuilt (those rely on the chore-commit path)                       |

## Trigger matrix

| Trigger                          | `ci.yml` (jobs that run)                  | `release.yml` | `docs.yml`           | `govulncheck.yml` | `pr-labeler.yml` | `devtools-image.yml` |
| -------------------------------- | ----------------------------------------- | ------------- | -------------------- | ----------------- | ---------------- | -------------------- |
| `push: <feature-branch>`         | n/a (not triggered)                       | n/a           | (no deploy)          | n/a               | n/a              | n/a                  |
| `pull_request: main`             | ✅ lint + unit + envtest + security + **e2e** | n/a       | ✅ build-test (strict) | ✅              | ✅               | ✅ only if Dockerfile.devtools changes |
| `push: main` (regular merge)     | n/a (not triggered)                       | skipped       | ✅ deploy `latest`+`dev` | n/a           | n/a              | ✅ only if Dockerfile.devtools changed in merge |
| `push: main` (`chore(release):`) | n/a (not triggered)                       | ✅ active     | ✅ deploy `latest`+`dev` (during the run); tag-push step later triggers `docs.yml` again for `stable` | n/a | n/a | n/a |
| `push: tags v*`                  | n/a                                       | n/a           | ✅ deploy stable alias (or `preview` for `-alpha`/`-beta`/`-rc`) | n/a | n/a | n/a |
| docs-only PR (paths-ignore match) | (none — workflow skipped)                | n/a           | ✅ (the docs path itself) | ✅ (no paths-ignore on govulncheck) | ✅       | n/a |
| schedule (Mon 05:08 UTC)         | n/a                                       | n/a           | n/a                  | ✅ weekly drift   | n/a              | n/a                  |
| `workflow_dispatch` (manual)     | n/a                                       | n/a           | n/a                  | ✅                | n/a              | ✅ (force rebuild)    |

## Devtools image — content-addressed CI base layer

The devtools container image (built from `Dockerfile.devtools`) bundles
Go + kubebuilder + controller-gen + kustomize + setup-envtest + docker
CLI + kind + helm + kubectl + govulncheck. Every CI test job runs inside
it via `./scripts/dev.sh`. Building it cold takes 2-3 min; even a warm
docker cache takes ~30 s of layer hash work — multiplied by five CI
jobs per PR, that is ~10 min of wasted time per PR.

`.github/workflows/devtools-image.yml` solves this by pre-baking the
image and pushing it to GHCR:

- **Tag**: `ghcr.io/<owner>/litellm-devtools:<hash>` where
  `<hash> = sha256(Dockerfile.devtools)[:12]`.
- **`latest`** alias is also pushed on `main` branch pushes only.
- **Triggers**: push:main and pull_request when `Dockerfile.devtools`
  or the workflow/action file changes, plus `workflow_dispatch` for
  manual rebuild (initial seed, force rebuild after registry purge).
- **Buildx registry cache** (`type=registry,ref=...:buildcache`) keeps
  the per-layer cache across runs.

`.github/actions/setup-devtools/action.yml` is the consumer side. Each
`ci.yml` job calls it after `actions/checkout`. The action:

1. Restores Go module + build caches under `.gocache/`.
2. Computes the same `<hash>` from `Dockerfile.devtools`.
3. Logs into GHCR with the workflow's `GITHUB_TOKEN`.
4. Tries `docker pull ghcr.io/<owner>/litellm-devtools:<hash>`.
5. On hit: tags it `litellm-devtools:latest` so `scripts/dev.sh` finds
   the image locally and skips its own build.
6. On miss: emits a notice and falls through; `scripts/dev.sh true`
   (the action's last step) builds the image locally.

The miss case is rare but well-defined: first PR/push touching
`Dockerfile.devtools` before `devtools-image.yml` finishes, GHCR
unavailable, or fork PR without `packages: read`. CI still completes
correctly, just slower.

## `release.yml` job sequence (commit-message-driven, tag-last)

```
parse (job-level `if`) ─ extract X.Y.Z from commit msg via regex
   │
   └─► run-tests ─ make test-unit && make test-envtest-fast
          │
          └─► build-and-release
                 ├─ checkout main with fetch-depth: 0
                 ├─ git-config github-actions[bot] identity
                 ├─ make release-bump VERSION=X.Y.Z (idempotent — no-op if already bumped)
                 ├─ commit + push manifest bumps to main with `[skip ci]`
                 ├─ pick goreleaser config: .yml (stable) | .prerelease.yml (alpha/beta/rc)
                 ├─ install cosign + cyclonedx-gomod + goreleaser
                 ├─ Create + push annotated tag `v<X.Y.Z>` (LAST-but-one)
                 ├─ goreleaser release --clean (cross-build amd64+arm64, sign, SBOM)
                 └─ appany/helm-oci-chart-releaser: push chart to ghcr.io/ackstorm/charts/
```

The tag push is intentionally near the end: a failure in tests / bump
/ goreleaser leaves origin with NO orphan tag. Recovery = re-push
another `chore(release): vX.Y.Z` commit.

## What lands where on a stable release `vX.Y.Z`

| Artifact                                                           | Where                                                                   |
| ------------------------------------------------------------------ | ----------------------------------------------------------------------- |
| Container image (multi-arch manifest list)                         | `ghcr.io/ackstorm/alitellm-operator:vX.Y.Z` + `:latest`                 |
| Helm chart (OCI artifact)                                          | `oci://ghcr.io/ackstorm/charts/alitellm-operator:X.Y.Z` (no `v` prefix) |
| GitHub Release assets (linux amd64+arm64 tarballs + source + sums) | `https://github.com/ackstorm/alitellm-operator/releases/tag/vX.Y.Z`     |
| Cosign signatures                                                  | `checksums.txt.sig` next to assets; container image OIDC signed         |
| CycloneDX SBOM                                                     | Bundled into the goreleaser archive (`sboms:` block)                    |
| Helm chart catalogue                                               | `https://ackstorm.github.io/alitellm-operator/index.yaml`               |
| Versioned docs                                                     | `https://ackstorm.github.io/alitellm-operator/vX.Y.Z/` (mike)           |
| `stable` alias                                                     | Points to the latest stable                                             |

Prerelease tags `vX.Y.Z-{alpha,beta,rc}*` follow the same flow with
`.goreleaser.prerelease.yml` and the `preview` mike alias instead of
`stable`.

## Pre-push gates — host-only

`make pre-push` runs `scripts/pre-push-check.sh` (authoritative source
for the exact gate list and order; this section describes intent, not
inventory, so the script can evolve without doc drift).

Categories covered:

- **Secret scanners** (`gitleaks`, `trufflehog`) over
  `origin/main..HEAD`, full history on first push.
- **Filesystem hygiene** — large-file caps, sensitive patterns
  (`.env`, `*.pem`, `*.key`, kubeconfig, ...), required top-level files
  (LICENSE, README), `.gitignore` coverage.
- **Repo identity** — origin remote matches expected, branch is up to
  date with remote.
- **Information warns** (do not block) — commit-author summary,
  urgent TODO / DO-NOT-COMMIT markers, working-tree status.
- **Build hygiene** — `go mod tidy` drift, govulncheck residuals
  match the ack-list (`references/security/govulncheck-acknowledged.md`)
  1:1.
- **Code provenance** — per-file SPDX license header
  (`// SPDX-License-Identifier: Apache-2.0`).
- **Defense in depth** — full `make qa-lint` + `make test-unit` run
  inside the devtools container on every push.

Bypass is banned. If a gate fails, fix the root cause, never
`--no-verify`. Run `scripts/pre-push-check.sh` directly (or
`bash -x` it) when you need to see the live count and exact order.

## Recovery procedures

| Symptom                                                                | What to do                                                                                                                                                  |
| ---------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Release tests failed → no tag, no release                              | Fix the code, push another `chore(release): vX.Y.Z` commit. `make release-bump` is idempotent — re-runs are safe.                                                   |
| goreleaser succeeded but chart push failed                             | Re-run the `Push Helm chart to OCI registry` step from the Actions UI. Or push a follow-up `chore(release): vX.Y.Z+1` commit.                                |
| Tag pushed but release marked Draft                                    | GH UI: open the draft release, click Publish. Inspect goreleaser log for the cause.                                                                          |
| `docs.yml` mike deploy failed                                          | Re-run the `Deploy docs` job from the Actions UI. Verify `extra.version.provider: mike` is still in `mkdocs.yml`.                                            |
| Manifest bump commit landed but pipeline died mid-run                  | The bot bump is on `main`. Re-push `chore(release): vX.Y.Z` again (or a clean revert + new commit). `make release-bump` detects a clean tree and skips.              |
| Stale draft release in GH UI                                           | Manual cleanup: `gh release delete vX.Y.Z --cleanup-tag --yes`. Then re-push the release commit.                                                              |
| Post-upgrade `CR apply` errors with `field not declared in schema`     | Pre-v0.3.2 install only. Helm `crds/` was install-only — `helm upgrade` did NOT re-apply CRDs. Fixed in v0.3.2: CRDs moved to `templates/`, so `helm upgrade` upgrades them with everything else. Stuck on v0.3.0/v0.3.1: `kubectl apply -f deploy/helm/alitellm-operator/crd-sources/` once, then bump to v0.3.2+. |

## Quick reference

```bash
# DEV LOOP — feature branch + PR
git checkout -b fix/foo
# ... edit ...
make pre-push                         # host-side publication gate
git push -u origin fix/foo
gh pr create --base main --title "..." --body "..."
gh pr checks --watch                  # wait for CI
gh pr merge --merge --delete-branch   # squash if preferred

# RELEASE (stable, e.g. v0.2.0)
git checkout main && git pull --ff-only
make release-cut VERSION=0.2.0            # empty chore commit + pre-push + push
                                       # → triggers release.yml
                                       # (preconditions: on main, clean tree, in-sync with origin)

# RELEASE (prerelease, e.g. v0.3.0-rc.1)
make release-cut VERSION=0.3.0-rc.1       # same wrapper; semver suffix supported
```

## Authoritative sources

- `.github/workflows/release.yml` — release pipeline (commit-message-driven).
- `.github/workflows/ci.yml` — unit + envtest + lint + security + gosec + E2E.
- `.github/workflows/devtools-image.yml` — builds + pushes the pre-baked
  devtools image (`ghcr.io/<owner>/litellm-devtools:<hash>`) consumed by
  ci.yml via `.github/actions/setup-devtools`.
- `.github/actions/setup-devtools/action.yml` — composite action that
  restores Go caches, logs into GHCR, pulls the hash-tagged devtools
  image, falls back to a local build if the registry image is missing.
- `.github/workflows/docs.yml` — mkdocs build + mike deploy.
- `.github/workflows/govulncheck.yml` — vuln scan.
- `.github/workflows/pr-labeler.yml` — auto-label by file paths.
- `.goreleaser.yml`, `.goreleaser.prerelease.yml`, `.goreleaser.snapshot.yml`
- `scripts/pre-push-check.sh` — host-side publication gate (authoritative gate list).
- `CLAUDE.md` — agent-facing single source of truth (sections: Release pipeline, Publication).

Any divergence between this file and the YAML workflows: trust the
YAML. Update this file in the same commit per the
"Documentation hygiene" rule in `CLAUDE.md`.
