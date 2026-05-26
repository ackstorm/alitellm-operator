# TODO

Outstanding items surfaced during recent work. Not blocking; track here
so they do not get lost between sessions.

## CI / devtools image

- **Bootstrap GHCR devtools image** — `.github/workflows/devtools-image.yml`
  will not fire automatically until `Dockerfile.devtools` (or the
  workflow/action file) changes again. Trigger a manual seed once GH
  infra is healthy:

  ```bash
  gh workflow run devtools-image.yml --ref main
  gh run watch  # confirms image push to ghcr.io/ackstorm/litellm-devtools:<hash> + :latest
  ```

  Until seeded, every `ci.yml` job falls back to a local devtools
  build (~30s warm / 2-3min cold per job). Functional but slow.

- **Verify composite action end-to-end on next non-docs PR** —
  `.github/actions/setup-devtools` is wired in but has not yet been
  exercised against a real PR. First non-docs PR after the GHCR seed
  should confirm:
  1. `docker pull ghcr.io/<owner>/litellm-devtools:<hash>` succeeds.
  2. Image is tagged as `litellm-devtools:latest` locally.
  3. `./scripts/dev.sh true` is a no-op (image already present).
  4. Job wall-clock drops by ~30s per job vs prior baseline.

- **Rerun failed workflows on `7fb7de3`** — GH infra was "Partially
  Degraded Service" during the push of commit `7fb7de3`
  (chore(ci): PR-only triggers + pre-baked devtools image). Two runs
  failed on transient SHA / 403 errors unrelated to our changes:

  - `devtools-image` run `26448424582` — codeload 404 on
    `docker/setup-buildx-action@v3`, then 403 on `actions/checkout@v6`
    in the rerun. Reissue once `status.indicator == "none"`.
  - `Documentation` run `26448424559` — codeload 404 on
    `matt-usurp/validate-semver@v3`. Pre-existing flake; rerun or pin
    the action to a fixed SHA if it keeps recurring.

  Check status: `curl -s https://www.githubstatus.com/api/v2/status.json`.

## Possible follow-ups

- Pin `runs-on: ubuntu-latest` → `ubuntu-24.04` across workflows for
  build predictability (no perf change; just stops surprise breakage
  when GH rolls the `latest` alias forward).
- Consider an `ubuntu-24.04-arm` probe for the Go-heavy jobs (envtest,
  unit) — arm runners are free and may shave wall-clock for native
  Go builds.
- Larger runners (`ubuntu-latest-4-cores`, $0.016/min) would cut
  envtest / e2e wall-clock ~30-50%; defer until dollars vs latency
  tradeoff is worth measuring.
