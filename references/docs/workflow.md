# Review workflow — push / PR / release map

Single-page reference for "what fires when I push or merge."
Companion to `CLAUDE.md` (sections: Release pipeline, Publication, CI
workflows) and to `.github/workflows/*.yml` (authoritative source).

## Lifecycle table

| Stage                         | Local command                                                                                         | Workflow(s) fired                                                                  | Job-level gate (`if:`)                                              | Outcome                                                                                                            |
| ----------------------------- | ----------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------- | ------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------ |
| Dev branch push               | `git push origin <branch>`                                                                            | `ci.yml` (lint + unit only), `govulncheck.yml`                                     | branch trigger                                                      | Fast feedback (~1m). Heavier gates deferred to PR/main.                                                            |
| PR open / sync                | `gh pr create --base main`                                                                            | `ci.yml` (lint + unit + envtest + security), `docs.yml` build-test, `pr-labeler.yml` | `pull_request: main`                                                | PR blocked until all four are green. E2E NOT run by default — opt-in with the `run-e2e` label.                      |
| Merge PR to main              | `gh pr merge N --merge --delete-branch`                                                               | `ci.yml` (lint + unit + envtest + security + **E2E**), `docs.yml` deploys `latest`+`dev` | Push to main NOT starting with `chore(release):`                    | Post-merge regression catch. If E2E breaks here, the next release commit is blocked until fixed.                    |
| Release commit                | `git commit --allow-empty -m 'chore(release): vX.Y.Z'` → `make pre-push` → `git push origin main`     | `release.yml` (active), `docs.yml`. `ci.yml` skipped (release.yml owns the gate).  | `startsWith(head_commit.message, 'chore(release): v')`              | release.yml runs unit + envtest-fast sanity, bumps manifests, goreleaser, cosign keyless OIDC, CycloneDX SBOM, helm OCI push, **tag created LAST**. |
| Tag push (rare manual)        | `git tag vX.Y.Z && git push origin vX.Y.Z`                                                            | `docs.yml` (mike deploy stable / preview alias)                                    | `startsWith(github.ref, 'refs/tags/v')`                             | Versioned doc set deployed; container/chart NOT rebuilt (those rely on the chore-commit path)                       |

## Trigger matrix

| Trigger                          | `ci.yml` (jobs that run)                  | `release.yml` | `docs.yml`           | `govulncheck.yml` | `pr-labeler.yml` |
| -------------------------------- | ----------------------------------------- | ------------- | -------------------- | ----------------- | ---------------- |
| `push: <feature-branch>`         | ✅ lint + unit                            | n/a           | (no deploy)          | ✅                | n/a              |
| `pull_request: main`             | ✅ lint + unit + envtest + security       | n/a           | ✅ build-test (strict) | ✅              | ✅               |
| `push: main` (regular merge)     | ✅ lint + unit + envtest + security + **e2e** | skipped   | ✅ deploy `latest`+`dev` | ✅            | n/a              |
| `push: main` (`chore(release):`) | skipped (release.yml owns the gate)       | ✅ active     | ✅ deploy `latest`+`dev` (during the run); tag-push step later triggers `docs.yml` again for `stable` | ✅ | n/a |
| `push: tags v*`                  | n/a                                       | n/a           | ✅ deploy stable alias (or `preview` for `-alpha`/`-beta`/`-rc`) | n/a | n/a |

**E2E escape hatch**: PR authors who want pre-merge E2E coverage can
apply the `run-e2e` label to a non-draft PR. Draft PRs never run E2E
regardless of the label. Without the label, E2E first runs against the
PR's changes on the post-merge `push: main` event.

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
git commit --allow-empty -m 'chore(release): v0.2.0'
make pre-push
git push origin main                  # triggers release.yml
gh run watch <release-run-id>         # follow the pipeline

# RELEASE (prerelease, e.g. v0.3.0-rc.1)
# same as above with -rc.1 / -beta.1 / -alpha.1 in the version.
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
