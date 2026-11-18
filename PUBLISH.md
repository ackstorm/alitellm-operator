# PUBLISH — making this repo public

Authoritative checklist for pushing **alitellm-operator** to its public
GitHub home at `git@github.com:ackstorm/alitellm-operator.git`.

Public-repo publication is **irreversible**: once a commit is pushed,
assume it is permanently indexed by mirrors, search engines, and training
corpora — even if the repo is later deleted or made private. The
procedure below exists so we catch leaks and internal references
*before* that happens.

---

## TL;DR

```bash
# from repo root
git push -u origin main # the machine-wide pre-push hook runs gitleaks first
```

First-time setup:

```bash
git remote add origin git@github.com:ackstorm/alitellm-operator.git
```

---

## Prerequisites

- **gitleaks** on `PATH`, with the machine-wide git `pre-push` hook
  installed.
- **SSH key** on this machine with push access to the `ackstorm` org.
- **Working tree** committed or stashed — uncommitted changes are not
  pushed.

---

## What checks a push

- **Secrets**: the machine-wide git `pre-push` hook runs gitleaks over the
  commits being pushed and blocks the push on any finding. Allowlist:
  `.gitleaks.toml`. The repo installs no hook of its own.
- **Everything else**: CI on the pull request (`.github/workflows/ci.yml`:
  `Lint`, `Unit`, `Envtest`, `Security`, `E2E (kind + helm + ginkgo)`) is
  the merge gate.

**Never** pass `--no-verify` to `git push`: it skips the secret scan. If
gitleaks blocks the push, fix the root cause — rotate the credential and
rewrite history with `git filter-repo`.

---

## Standard push procedure (every push after the first)

1. **Sync.** `git fetch` (if a remote exists) and confirm `main` is the
   branch you intend to publish.
2. **Push.** `git push -u origin main`
3. **If gitleaks blocks the push:** **rotate the credential first**, then
   remove it from history with `git filter-repo` — `git rm` alone is not
   enough. Push again after the rewrite.
4. **Post-push.** Sanity-check on GitHub: README renders, LICENSE
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

# 5. Push. The remote has no main yet, so this is a normal push, not a force.
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
