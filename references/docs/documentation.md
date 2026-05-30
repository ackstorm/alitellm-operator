# Documentation system — reference

Agent-facing internal reference describing **how documentation works in
this repository**: where files live, what generates what, how the public
site is built and deployed, and which knobs to turn when something needs
to change.

This file is part of `references/` and is **not** published on the
mkdocs site. Update it whenever the documentation toolchain itself
changes (mkdocs config, crd-ref-docs config, docs.yml workflow, Helm
chart-index publishing).

---

## 1. Surfaces at a glance

The repo ships **three** parallel documentation surfaces. They are
independent: changing one does not auto-update the others.

| Surface | Source | Built by | Published to |
|---|---|---|---|
| Public site (mkdocs-material) | `docs/**/*.md` + `mkdocs.yml` | `mkdocs build` (Docker or local Python) | GitHub Pages `gh-pages` branch, served at `https://ackstorm.github.io/alitellm-operator/` |
| API reference (auto-generated) | `api/litellm/v1alpha1/*_types.go` (Go CRD types) | `crd-ref-docs` ⇒ markdown into `docs/api-reference/` | Embedded into the public site via mkdocs nav entry |
| Helm chart index | `docs/index.yaml` | Updated by `helm-oci-chart-releaser` action in `release.yml` | GitHub Pages, served at `https://ackstorm.github.io/alitellm-operator/index.yaml` |

Agent-facing internal docs (NOT public):

| Path | Purpose |
|---|---|
| `references/security/` | govulncheck ack-list, security notes |
| `references/docs/` | this file |
| `docs/plans/` | per-phase planning docs — physically inside `docs/` but **NOT in `mkdocs.yml` nav**, so not served. Don't rename without auditing build-strict assumptions. |
| `CLAUDE.md` | agent reference card (root + `/home/jcm/.claude/`) |

---

## 2. Public site — mkdocs-material

### Layout (under `docs/`)

```
docs/
├── index.md                      ← landing page
├── index.yaml                    ← Helm chart index (NOT mkdocs content)
├── Makefile                      ← docs-specific targets, included by root Makefile
├── requirements.txt              ← Python deps (mkdocs, mike, plugins)
├── .crd-ref-docs.yaml            ← crd-ref-docs config (NOT mkdocs content)
├── metrics.md
├── assets/                       ← images
├── getting-started/              ← installation.md, quickstart.md
├── user-guide/                   ← one .md per CRD + index.md
├── developer-guide/              ← architecture, development, release-process, contributions
├── community/                    ← code-of-conduct.md, security.md
├── api-reference/                ← AUTO-GENERATED (litellm.ackstorm.ai.md)
└── plans/                        ← internal planning, NOT in nav
```

### Config — `mkdocs.yml` (repo root)

- `site_name`, `site_url`, `repo_url` — point to ackstorm GitHub Pages.
- `nav:` — explicit navigation tree. Adding a new doc file requires
  appending an entry here OR `mkdocs build --strict` will warn (and the
  CI build job sets `--strict`).
- `theme: material` with light/dark palette toggle and `navigation.*`,
  `content.code.copy`, `search.*` features.
- `plugins: [search]` — minimal.
- `markdown_extensions:` — admonition, pymdownx (details, superfences,
  highlight, inlinehilite, snippets, tabbed), tables, toc (with
  permalinks).
- `extra.version.provider: mike` with default alias `latest` — this is
  what makes the version switcher in the site header work.

### Build / serve commands (all defined in `docs/Makefile`,
re-exposed by root Makefile via `include docs/Makefile`)

| Target | What it does | Where it runs |
|---|---|---|
| `make gen-crd-ref-docs` | Runs `bin/crd-ref-docs` against `api/` → writes `docs/api-reference/litellm.ackstorm.ai.md` | Needs Go on PATH ⇒ inside devtools (`./scripts/dev.sh make gen-crd-ref-docs`) |
| `make docs-serve` | `gen-crd-ref-docs` then `docker run squidfunk/mkdocs-material:latest serve -a 0.0.0.0:8000` | Host (uses host docker) |
| `make docs-build` | `gen-crd-ref-docs` then `docker run squidfunk/mkdocs-material:latest build` | Host (uses host docker) |
| `make docs-build-local` | `gen-crd-ref-docs` then `python3 -m mkdocs build` | Host with mkdocs installed |
| `make docs-build-strict` | Same as above + `--strict` flag (PR CI uses `--strict`) | Host with mkdocs installed |
| `make install-docs` / `install-mkdocs` | `pip install -r docs/requirements.txt` | Host Python |
| `make crd-ref-docs` | `go install github.com/elastic/crd-ref-docs@v0.2.0` into `bin/` | Inside devtools (root Makefile target, not in `docs/Makefile`) |

`make docs` (referenced in `docs.yml`) is **not** defined explicitly —
relies on `crd-ref-docs` + the docs-* chain. If broken, check
`docs/Makefile` line 38–48 and confirm root `Makefile` includes
`docs/Makefile` (line 373).

---

## 3. API reference generation — crd-ref-docs

Generator: `github.com/elastic/crd-ref-docs@v0.2.0` (pinned in root
`Makefile` as `CRD_REF_DOCS_VERSION`).

Config: `docs/.crd-ref-docs.yaml`

Key behavior:
- `processing.onlyGroups: [litellm.ackstorm.ai]` — only this CRD group
  is rendered. Adding a new API group means appending it here.
- `processor.ignoreGroupVersionKinds:` — strips upstream k8s types
  (`ConfigMap`, `Secret`, `Service`, `Deployment`, `Ingress`) that
  appear in field schemas.
- `processor.ignoreFields:` — drops `metadata.{creationTimestamp,
  generation, resourceVersion, uid, managedFields}` and
  `status.observedGeneration`.
- `output-mode=group` (from the Makefile invocation) ⇒ one file per
  API group: `docs/api-reference/litellm.ackstorm.ai.md`.
- Custom templates in the config (`processor.templates.group/type/properties`)
  emit a YAML usage example and prepend a "Quick Navigation" block.

**Trigger to regenerate:** any change to `api/litellm/v1alpha1/*_types.go`,
including struct tags (`+kubebuilder:validation:...`, `+optional`,
`json:"..."`). After regen, commit `docs/api-reference/litellm.ackstorm.ai.md`
in the same change. The `docs.yml` workflow runs `make docs` on every
push to `main`, so a stale committed file gets overwritten in CI — but
PRs that build with `--strict` will fail if generated output drifts
from what was committed AND mkdocs picks up a broken link.

---

## 4. CI/CD — `.github/workflows/docs.yml`

Triggers:
- `push: main` ⇒ deploy as `dev` + `latest` aliases (mike).
- `push: tags v*` ⇒ deploy as versioned doc set; alias `stable` for
  stable tags, `preview` for `alpha|beta|rc` prereleases.
- `pull_request: main` ⇒ build with `--strict` and upload `site/`
  artifact; **no deploy**.

Jobs:
- `deploy-documentation` (runs only on main / tags):
  1. Checkout with `fetch-depth: 0` (mike needs full history).
  2. `make docs` — generates `docs/api-reference/`.
  3. Validate semver tag (only on tag push) via `matt-usurp/validate-semver@v2`.
  4. Determine release type (`stable` / `prerelease` / `other_prerelease`).
  5. Install Python 3.11 + `pip install -r docs/requirements.txt`.
  6. `git config` github-actions[bot] identity (mike commits to `gh-pages`).
  7. Branch on event:
     - **main push** ⇒ `mike deploy --push --update-aliases dev latest`
       then `mike set-default --push latest`.
     - **stable tag** ⇒ `mike deploy --push --update-aliases $VERSION stable`
       then `mike set-default --push stable`.
     - **prerelease tag** ⇒ `mike deploy --push $VERSION`; if alpha/beta/rc
       also alias `preview` to that version.

- `documentation-build-test` (PR only):
  - Setup Python 3.11, install deps, `mkdocs build --strict`,
    upload `site/` artifact.

**Mike alias semantics:**
- `latest` — head of `main` (dev docs).
- `dev` — same as `latest` (kept as a duplicate alias for clarity).
- `stable` — most recent stable release.
- `preview` — most recent alpha/beta/rc.
- The site's version switcher (from `extra.version.provider: mike`)
  shows every deployed version + aliases.

---

## 5. Helm chart catalogue — `docs/index.yaml`

`docs/index.yaml` is **NOT mkdocs content** — it's the Helm chart
repository index served from `https://ackstorm.github.io/alitellm-operator/index.yaml`.

Current state: `entries: {}` (placeholder). It is **regenerated** by
`appany/helm-oci-chart-releaser@v0.5.0` in `release.yml` whenever a
release runs. The action also pushes the chart to
`oci://ghcr.io/ackstorm/charts/alitellm-operator:<X.Y.Z>`.

Consumers add it as a Helm repo:

```bash
helm repo add alitellm https://ackstorm.github.io/alitellm-operator
helm repo update
helm install ...
```

OCI registry path is the **primary** install method (see
`docs/getting-started/installation.md`); `index.yaml` is the
classic-HTTP-repo fallback.

---

## 6. Common operations / triggers

| Change | What to update |
|---|---|
| Add/rename CRD field, type, validation marker | (a) regenerate manifests: `./scripts/dev.sh make gen-manifests` (b) regenerate API ref: `./scripts/dev.sh make gen-crd-ref-docs` (c) commit `docs/api-reference/litellm.ackstorm.ai.md` (d) update relevant `docs/user-guide/<kind>.md` example if the field shows there |
| Add new CRD kind | Steps above PLUS add `mkdocs.yml` nav entry under `User Guide:` PLUS create `docs/user-guide/<kind>.md` PLUS add row to root `CLAUDE.md` "Quick context" CRD list |
| Add new doc page | (a) create `docs/<section>/<file>.md` (b) add entry to `mkdocs.yml` `nav:` (omit ⇒ `--strict` warns) (c) run `make docs-build-strict` locally |
| Change API group | Update `docs/.crd-ref-docs.yaml` `processing.onlyGroups:` AND `mkdocs.yml` nav under `API Reference:` AND `docs/.crd-ref-docs.yaml` template literal `eq .Group "litellm.ackstorm.ai"` |
| Bump crd-ref-docs | Edit `CRD_REF_DOCS_VERSION` in root `Makefile`. Force `make crd-ref-docs` rebuild by removing `bin/crd-ref-docs`. |
| Bump mkdocs / plugins | Edit `docs/requirements.txt`. CI rebuilds with cache key `hashFiles('**/requirements.txt')`. |
| Change site URL / repo URL | `mkdocs.yml` (`site_url`, `repo_url`, `repo_name`, `extra.social.link`) AND `docs/index.md` if hard-coded |
| Change release deploy behavior | `.github/workflows/docs.yml` deploy-* steps; the alias matrix (`latest`/`dev`/`stable`/`preview`) is the contract |
| Change PR strictness | `.github/workflows/docs.yml` job `documentation-build-test` uses `mkdocs build --strict`. Loosening this is a deliberate decision — strict is what catches broken nav links and orphaned files. |

---

## 7. Failure modes & gotchas

### ❌ `make docs-build-strict` fails with "doc file not in nav"
Cause: new `.md` under `docs/` not added to `mkdocs.yml` `nav:`.
Fix: add nav entry OR move file out of `docs/` (e.g. `references/`,
`docs/plans/` is special-cased — not in nav and intentionally so, but
strict mode still warns about it as an orphan unless you place it
under a path mkdocs ignores; current setup tolerates it because
`docs/plans/*.md` is in nav-less subdir and material doesn't hard-fail
on orphans in strict mode for non-error warnings).

### ❌ API reference is stale on the site
Cause: `make gen-crd-ref-docs` not run after `*_types.go` change.
Fix: run it inside devtools (Go binary needed), commit the regenerated
`docs/api-reference/litellm.ackstorm.ai.md`. CI will overwrite on
push to main anyway but PR previews will be wrong.

### ❌ `mike deploy` fails on tag push: "could not find version X.Y.Z"
Cause: tag pushed but `release.yml` failed somewhere and there is no
matching docs build. Mike doesn't care about goreleaser — it just
deploys the current `site/`. Likely root cause: Python install failed
or `make docs` errored. Inspect the `Deploy docs` job log.

### ❌ Chart `index.yaml` missing the just-released version
Cause: `appany/helm-oci-chart-releaser` step in `release.yml` failed
or was skipped. The chart is still on GHCR OCI registry. Re-run the
release workflow or manually regenerate `docs/index.yaml` from chart
metadata.

### ❌ `docs.yml` deploys `latest` but the version dropdown is empty
Cause: site rebuilt without `extra.version.provider: mike` in
`mkdocs.yml`. Confirm the block at the bottom of `mkdocs.yml` (lines
78–82) is intact.

### ❌ `mkdocs build` works locally but fails in CI
Cause: CI uses `--strict`. Local `docs-build` doesn't. Always run
`make docs-build-strict` before pushing docs changes.

### ❌ `crd-ref-docs` emits empty output
Cause: `processing.onlyGroups` doesn't match the actual group in the
Go types (`+kubebuilder:object:root=true` markers and the
`groupName=litellm.ackstorm.ai` in `groupversion_info.go`). Verify
`api/litellm/v1alpha1/groupversion_info.go` matches the config.

---

## 8. Cross-references

- Root `Makefile` lines 318, 337, 367–371, 373 — crd-ref-docs install
  and the `include docs/Makefile` line that hangs the docs targets off
  the root.
- `CLAUDE.md` (root) "Documentation site (mkdocs)" section — agent
  quick-reference for the same surface.
- `CLAUDE.md` (root) "Documentation hygiene" — the in-commit-update rule.
- `.github/workflows/release.yml` lines 213–223 — Helm chart push that
  updates `docs/index.yaml`.
- `docs/developer-guide/release-process.md` — user-facing version of
  release flow (keep in sync with `release.yml` reality).

---

## 9. When this file goes stale

This file is the contract for **how docs work**. If you change any of
the following, update this file in the same commit:

- `mkdocs.yml` (theme, plugins, nav structure, version provider)
- `docs/.crd-ref-docs.yaml`
- `docs/Makefile` (target names or behavior)
- `docs/requirements.txt` (toolchain bumps)
- `.github/workflows/docs.yml` (deploy strategy, alias matrix, strict mode)
- Root `Makefile` lines around `CRD_REF_DOCS*`, `include docs/Makefile`
- Helm chart publishing step in `.github/workflows/release.yml`
- Introduction of a new doc surface (e.g. an OperatorHub catalog, an
  ADR archive, a separate API-spec site)

Stale-doc-as-bug: see "Documentation hygiene" rule at the top of root
`CLAUDE.md`.
