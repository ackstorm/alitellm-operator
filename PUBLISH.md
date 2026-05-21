# PUBLISH — making this repo public

Authoritative checklist for pushing **alitellm-operator** to its public
GitHub home at `git@github.com:ackstorm/alitellm-operator.git`.

Public-repo publication is **irreversible**: once a commit is pushed,
assume it is permanently indexed by mirrors, search engines, and training
corpora — even if the repo is later deleted or made private. The
procedure below exists so we catch leaks, large binaries, and internal
references *before* that happens.

---

## TL;DR

```bash
# from repo root
make pre-push           # or ./scripts/pre-push-check.sh — must exit 0
git push -u origin main # only after the script passes
```

First-time setup:

```bash
git remote add origin git@github.com:ackstorm/alitellm-operator.git
make hooks              # symlinks .git/hooks/pre-push -> scripts/pre-push-check.sh
```

---

## Prerequisites

- **Docker** running and reachable by the current user (sandboxes the
  secret scanners and pins their versions).
- **SSH key** on this machine with push access to the `ackstorm` org.
- **Working tree** committed or stashed — uncommitted changes are not
  pushed; the script warns about them.

---

## The 15 hard gates

`scripts/pre-push-check.sh` runs 15 hard checks (failure blocks push) and
5 soft checks (informational warnings). The gate list:

| #  | Check                          | Block reason                                                  |
|----|--------------------------------|---------------------------------------------------------------|
| 1  | gitleaks (`origin/main..HEAD`; full history on first push) | API keys, tokens, high-entropy strings. Allowlist: `.gitleaks.toml`. |
| 2  | trufflehog `--only-verified`   | Confirms suspected secrets are actually live.                 |
| 3  | Large tracked files (>2 MB)    | Public repos amplify accidental binary commits.               |
| 4  | Sensitive file patterns        | `.env`, `*.pem`, `*.key`, kubeconfig, `credentials.json`, ... |
| 5  | `LICENSE` and `README.md`      | Required for any public OSS repo.                             |
| 6  | `origin` remote matches        | Prevents pushing to the wrong remote.                         |
| 13 | govulncheck ack-list 1:1       | Routes through `scripts/govulncheck-gate.sh` (devtools binary; pinned `v1.3.0`). The wrapper enforces an exact match between the reachable advisories and `docs/security/govulncheck-acknowledged.md`. Any NEW HIGH or ack-list drift blocks. |
| 14 | `go mod tidy` drift            | Snapshots `go.mod`/`go.sum`, runs tidy, diffs; restores on drift so the working tree stays clean. |
| 15 | Per-file SPDX license header   | Every tracked `*.go` outside `vendor/`, `zz_generated*.go`, `mock_*.go`, `.claude/` must carry `// SPDX-License-Identifier: Apache-2.0` within its first 5 lines. Build-tag-first files (`//go:build ...` line 1) satisfy on line 3. |

Soft checks (7-12): internal hostnames / private IPv4, `@ackstorm.com` /
`@ackstorm.es` emails, `.gitignore` sanity, commit author audit,
`DO NOT COMMIT` markers, working-tree status.

Per [CLAUDE.md](./CLAUDE.md), **never** pass `--no-verify` to `git push`.
If a gate fails, fix the root cause — rotate credentials, rewrite
history with `git filter-repo`, bump the dependency, regenerate the SPDX
header.

### CI parity

The same supply-chain gates run server-side in the `Security` job of
`.github/workflows/ci.yml`. The local pre-push script is the fast
feedback loop; the CI `Security` job is the authoritative merge gate.

---

## Standard push procedure (every push after the first)

1. **Sync.** `git fetch` (if a remote exists) and confirm `main` is the
   branch you intend to publish.
2. **Run the script.**
   ```bash
   make pre-push
   ```
   Or rely on the installed git hook (`make hooks`).
3. **If a hard check fails:** do not push. Common remediations:
   - Leak detected: **rotate the credential first**, then remove from
     history with `git filter-repo` — `git rm` alone is not enough.
   - Large file: `git rm` + `git filter-repo --strip-blobs-bigger-than 2M`.
   - Sensitive file pattern: `git rm --cached <file>`, add to
     `.gitignore`, commit, rewrite history if it existed in earlier
     commits.
   - Wrong `origin`: `git remote set-url origin <expected>`.

   After any history rewrite, **re-run the script from scratch.**
4. **If only warnings remain:** read each one. Confirm matches are test
   fixtures or doc examples. Re-run if you change anything.
5. **Push.** `git push -u origin main`
6. **Post-push.** Sanity-check on GitHub: README renders, LICENSE
   detected, no large blobs flagged, no Dependabot or secret-scanning
   alerts waiting in the Security tab.

---

## First publication — flatten history into one Initial commit

**This section applies once, before the very first push.** Skip it on
every subsequent push (use the standard procedure above instead).

The private development history of this repo contains commit messages
with detailed internal commentary — planning IDs, audit paths, internal
product names, and other context written for the team, not the public.
Once pushed, those messages are permanent. The recommended first
publication is therefore a **single fresh "Initial commit"** built from
the current tree, with internal paths excluded.

### Paths to exclude from the public repo

The orphan flatten removes the following from the working tree before
the initial commit. Re-add to `.gitignore` so they stay out:

| Path                  | Why exclude                                          |
|-----------------------|------------------------------------------------------|
| `.planning/`          | GSD planning artifacts — ROADMAP, STATE, audits, phases, notes, intel, graphs |
| `docs/plans/`         | Dated phase implementation plans (internal)          |
| `spec/`               | Internal spec drafts + vendored frozen OpenAPI snapshot |
| `.antigravitycli/`    | Agent IDE state                                      |
| `.claude/`            | Claude Code worktree state (already gitignored)      |
| `.gocache/`           | Go module + build caches (already gitignored)        |

### Procedure

```bash
# 0. Be on main, working tree clean.
git checkout main
git status   # must show "nothing to commit"

# 1. Safety net — full private history kept locally.
git branch backup-pre-public-flatten

# 2. Remove internal paths from the tree and ignore them going forward.
git rm -r .planning docs/plans spec
printf '\n.planning/\ndocs/plans/\nspec/\n' >> .gitignore
git add .gitignore

# 3. Create the single Initial commit on an orphan branch.
git checkout --orphan tmp-public
git add -A
git commit -m "Initial commit"

# 4. Replace main with the flattened branch.
git branch -D main
git branch -m tmp-public main

# 5. Re-run the pre-push validation against the new single-commit history.
make pre-push

# 6. Push. The remote has no main yet, so this is a normal push, not a force.
git push -u origin main
```

### After the flatten

- Old commits live in local `.git` until garbage collection reaches them
  (kept alive by `backup-pre-public-flatten` for as long as that branch
  exists). To purge:
  `git branch -D backup-pre-public-flatten && git gc --aggressive --prune=now`.
- From this point on, every push follows the standard procedure above.

---

## Branch protection — required CI checks

Once published, the `main` branch protection rule should require these
CI checks before merge. Apply via the GitHub web UI:

    Repo Settings -> Branches -> Branch protection rule -> "main" -> Require status checks

| Job name                       | Workflow file | Purpose |
|--------------------------------|---------------|---------|
| `Lint`                         | `ci.yml` | golangci-lint, incl. gosec HIGH-gate |
| `Unit`                         | `ci.yml` | unit tests + coverage |
| `Envtest`                      | `ci.yml` | controller envtest (race-enabled) |
| `E2E (kind + helm + ginkgo)`   | `ci.yml` | full hydrated cluster suite |
| `Security`                     | `ci.yml` | gosec + govulncheck + fuzz-short; SARIF upload; dependency-review on PR diffs |

The `Security` job prevents new HIGH-severity gosec or govulncheck
findings from being merged. Its SARIF upload populates the repository's
GitHub Security tab. `actions/dependency-review-action` runs on PR diffs
only and blocks PRs that add HIGH-severity dependency advisories.

Optional but recommended:

- Require pull-request reviews before merge.
- Dismiss stale pull-request approvals when new commits are pushed.
- Restrict who can dismiss pull-request reviews.
- Require signed commits.

Branch-protection rules require admin scope beyond what `GITHUB_TOKEN`
carries; they are applied by a human via the web UI, not via the API.
