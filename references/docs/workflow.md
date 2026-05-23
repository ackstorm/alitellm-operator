# Review workflow — push / PR / release map

Single-page reference for "what fires when I push or merge."
Companion to `CLAUDE.md` (sections: Release pipeline, Publication, CI
workflows) and to `.github/workflows/*.yml` (authoritative source).

## Tiered gating policy

Each test job activates only when its cost is justified by the event
class. Per-job `if:` filters in `.github/workflows/ci.yml` are
authoritative; the table below is the human-readable contract.

| Event                                 | lint | unit | envtest | security | e2e |
| ------------------------------------- | ---- | ---- | ------- | -------- | --- |
| push: feature branch (any non-main)   |  ✓   |  ✓   |    -    |    -     |  -  |
| pull_request → main                   |  ✓   |  ✓   |    ✓    |    ✓     |  ✓  |
| push: main (post-merge, non-release)  |  ✓   |  ✓   |    ✓    |    ✓     |  -  |
| push: main, `chore(release): v*`      |  -   |  -   |    -    |    -     |  -  |

Rationale:

- **Feature branch push**: lint + unit catch ~90% of issues in ~1m;
  envtest / security / e2e are wasteful on WIP commits.
- **PR → main**: full pre-merge gate INCLUDING e2e. The PR is the
  merge boundary — a broken e2e here blocks merge instead of breaking
  main after the fact. Draft PRs skip e2e (the rest still run).
- **Post-merge main push**: lint + unit + envtest + security as the
  "PR was green at last push but stale vs main" regression catch.
  E2E is intentionally NOT re-run — it already ran on the PR against
  the same merge ref, so re-running just doubles cost per merge.
- **Release commit (`chore(release): v*`)**: `ci.yml` skips entirely.
  `release.yml` runs its own unit + envtest-fast sanity gate before
  shipping, so duplicating it in `ci.yml` would waste CI minutes.

### `paths-ignore` — docs-only commits skip CI entirely

`ci.yml` declares `paths-ignore` on both `push` and `pull_request`:

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

A commit that touches ONLY these paths produces no CI run at all
(`docs.yml` still fires for `docs/**` deploys). If a PR mixes a docs
file with code, the workflow fires normally.

**Branch protection caveat**: GitHub treats "workflow did not run"
distinctly from "workflow passed". If `required: true` is set on a
job name in branch protection, a paths-ignored PR will appear as
missing-checks and block merge. Either (a) keep `lint` non-required
(it's the cheapest signal anyway) or (b) drop the paths-ignore on
`pull_request` and rely solely on the push filter — current trade-off
prioritises cost savings over the require-on-PR convenience.

## Lifecycle table

| Stage                         | Local command                                                                                         | Workflow(s) fired                                                                  | Job-level gate (`if:`)                                              | Outcome                                                                                                            |
| ----------------------------- | ----------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------- | ------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------ |
| Dev branch push               | `git push origin <branch>`                                                                            | `ci.yml` (lint + unit only), `govulncheck.yml`                                     | branch trigger                                                      | Fast feedback (~1m). Heavier gates deferred to PR/main.                                                            |
| PR open / sync                | `gh pr create --base main`                                                                            | `ci.yml` (lint + unit + envtest + security + **e2e**), `docs.yml` build-test, `pr-labeler.yml` | `pull_request: main` (non-draft for e2e)                | PR blocked until all five are green. Draft PRs skip e2e only.                                                       |
| Merge PR to main              | `gh pr merge N --merge --delete-branch`                                                               | `ci.yml` (lint + unit + envtest + security), `docs.yml` deploys `latest`+`dev`     | Push to main NOT starting with `chore(release):`                    | Post-merge regression catch (no e2e — already ran on PR).                                                          |
| Release commit                | `git commit --allow-empty -m 'chore(release): vX.Y.Z'` → `make pre-push` → `git push origin main`     | `release.yml` (active), `docs.yml`. `ci.yml` skipped (release.yml owns the gate).  | `startsWith(head_commit.message, 'chore(release): v')`              | release.yml runs unit + envtest-fast sanity, bumps manifests, goreleaser, cosign keyless OIDC, CycloneDX SBOM, helm OCI push, **tag created LAST**. |
| Tag push (rare manual)        | `git tag vX.Y.Z && git push origin vX.Y.Z`                                                            | `docs.yml` (mike deploy stable / preview alias)                                    | `startsWith(github.ref, 'refs/tags/v')`                             | Versioned doc set deployed; container/chart NOT rebuilt (those rely on the chore-commit path)                       |

## Trigger matrix

| Trigger                          | `ci.yml` (jobs that run)                  | `release.yml` | `docs.yml`           | `govulncheck.yml` | `pr-labeler.yml` |
| -------------------------------- | ----------------------------------------- | ------------- | -------------------- | ----------------- | ---------------- |
| `push: <feature-branch>`         | ✅ lint + unit                            | n/a           | (no deploy)          | ✅                | n/a              |
| `pull_request: main`             | ✅ lint + unit + envtest + security + **e2e** | n/a       | ✅ build-test (strict) | ✅              | ✅               |
| `push: main` (regular merge)     | ✅ lint + unit + envtest + security       | skipped       | ✅ deploy `latest`+`dev` | ✅            | n/a              |
| `push: main` (`chore(release):`) | skipped (release.yml owns the gate)       | ✅ active     | ✅ deploy `latest`+`dev` (during the run); tag-push step later triggers `docs.yml` again for `stable` | ✅ | n/a |
| `push: tags v*`                  | n/a                                       | n/a           | ✅ deploy stable alias (or `preview` for `-alpha`/`-beta`/`-rc`) | n/a | n/a |
| any docs-only commit (paths-ignore match) | (none — workflow skipped)         | n/a           | ✅ (the docs path itself) | ✅ (if non-docs go.* drift) | n/a |

## `release.yml` job sequence (commit-message-driven, tag-last)

```
parse (job-level `if`) ─ extract X.Y.Z from commit msg via regex
   │
   └─► run-tests ─ make unit && make envtest-fast
          │
          └─► build-and-release
                 ├─ checkout main with fetch-depth: 0
                 ├─ git-config github-actions[bot] identity
                 ├─ make bump VERSION=X.Y.Z (idempotent — no-op if already bumped)
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

## Pre-push gates (15) — host-only

`make pre-push` runs `scripts/pre-push-check.sh`. Hard gates (failure
blocks push):

1.  gitleaks (`origin/main..HEAD`; full history on first push)
2.  trufflehog (same scope)
3.  Large files >2 MB
4.  Sensitive file patterns (`.env`, `*.pem`, `*.key`, kubeconfig, ...)
5.  LICENSE + README presence
6.  Origin remote matches expected
7.  ackstorm email leak scan
8.  Branch is up-to-date with remote
9.  `.gitignore` sanity (covers `.env`, `.claude`)
10. Commit-author informational
11. Urgent TODO / DO-NOT-COMMIT markers (informational warn)
12. Working tree status (informational warn)
13. govulncheck residuals match `references/security/govulncheck-acknowledged.md` 1:1
14. `go mod tidy` drift
15. Per-file SPDX license header (`// SPDX-License-Identifier: Apache-2.0`)

Bypass is banned. If a gate fails, fix the root cause, never
`--no-verify`.

## Recovery procedures

| Symptom                                                                | What to do                                                                                                                                                  |
| ---------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Release tests failed → no tag, no release                              | Fix the code, push another `chore(release): vX.Y.Z` commit. `make bump` is idempotent — re-runs are safe.                                                   |
| goreleaser succeeded but chart push failed                             | Re-run the `Push Helm chart to OCI registry` step from the Actions UI. Or push a follow-up `chore(release): vX.Y.Z+1` commit.                                |
| Tag pushed but release marked Draft                                    | GH UI: open the draft release, click Publish. Inspect goreleaser log for the cause.                                                                          |
| `docs.yml` mike deploy failed                                          | Re-run the `Deploy docs` job from the Actions UI. Verify `extra.version.provider: mike` is still in `mkdocs.yml`.                                            |
| Manifest bump commit landed but pipeline died mid-run                  | The bot bump is on `main`. Re-push `chore(release): vX.Y.Z` again (or a clean revert + new commit). `make bump` detects a clean tree and skips.              |
| Stale draft release in GH UI                                           | Manual cleanup: `gh release delete vX.Y.Z --cleanup-tag --yes`. Then re-push the release commit.                                                              |
| Post-upgrade `CR apply` errors with `field not declared in schema`     | Helm `crds/` is install-only — `helm upgrade` does NOT re-apply CRDs. Run `kubectl apply -f deploy/helm/alitellm-operator/crds/` (or pull the OCI chart + apply its `crds/` dir). See CLAUDE.md "Common failure modes". |

## Quick reference

```bash
# DEV LOOP — feature branch + PR
git checkout -b fix/foo
# ... edit ...
make pre-push                         # 15-gate host check
git push -u origin fix/foo
gh pr create --base main --title "..." --body "..."
gh pr checks --watch                  # wait for CI
gh pr merge --merge --delete-branch   # squash if preferred

# RELEASE (stable, e.g. v0.2.0)
git checkout main && git pull --ff-only
make release VERSION=0.2.0            # empty chore commit + pre-push + push
                                       # → triggers release.yml
                                       # (preconditions: on main, clean tree, in-sync with origin)

# RELEASE (prerelease, e.g. v0.3.0-rc.1)
make release VERSION=0.3.0-rc.1       # same wrapper; semver suffix supported
```

## Authoritative sources

- `.github/workflows/release.yml` — release pipeline (commit-message-driven).
- `.github/workflows/ci.yml` — unit + envtest + lint + security + gosec + E2E.
- `.github/workflows/docs.yml` — mkdocs build + mike deploy.
- `.github/workflows/govulncheck.yml` — vuln scan.
- `.github/workflows/pr-labeler.yml` — auto-label by file paths.
- `.goreleaser.yml`, `.goreleaser.prerelease.yml`, `.goreleaser.snapshot.yml`
- `scripts/pre-push-check.sh` — 15-gate host check.
- `CLAUDE.md` — agent-facing single source of truth (sections: Release pipeline, Publication).

Any divergence between this file and the YAML workflows: trust the
YAML. Update this file in the same commit per the
"Documentation hygiene" rule in `CLAUDE.md`.
