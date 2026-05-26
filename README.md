# alitellm-operator

**a**(nother) **litellm**-operator — the leading `a` acknowledges the
existing ecosystem of LiteLLM operators
([bbdsoftware/litellm-operator](https://github.com/bbdsoftware/litellm-operator),
[PalenaAI/litellm-operator](https://github.com/PalenaAI/litellm-operator),
and others). Each of those targets a different slice of the problem;
this one focuses on the GitOps-facing declarative surface and on
coexistence with hand-managed LiteLLM entries and an external identity
system.

A Kubernetes operator that owns the GitOps-facing desired state for LiteLLM
execution capabilities and reconciles it into a running LiteLLM proxy via its
REST API.

## What This Is

A declarative, name-scoped GitOps surface for LiteLLM. It manages nine
custom resource kinds — LiteLLMConnection, LiteLLMModel,
LiteLLMModelAlias, LiteLLMModelDiscovery, LiteLLMMCPServer,
LiteLLMMCPServerDiscovery, LiteLLMA2AAgent, LiteLLMTeam, and
LiteLLMGuardRail — across two reconciliation pipelines:

- **Pipeline A** reconciles explicit CRs directly into LiteLLM via its
  REST API using wholesale-replace semantics.
- **Pipeline B** (Discovery kinds) queries upstream provider APIs or
  ToolHive and projects Kubernetes child CRs, which Pipeline A then
  reconciles.

Name-scoped ownership lets the operator coexist with hand-managed LiteLLM
entries and with an external identity / user-management system that owns
User and VirtualKey lifecycle.

## Status

Early-access. API surface may change before v1beta1. The current
version and changelog are on the
[Releases page](https://github.com/ackstorm/alitellm-operator/releases/latest).
The authoritative contract is the operator spec; see
[PUBLISH.md](PUBLISH.md) for the release/publication procedure.

## Quick Start

Prerequisites:

- Kubernetes >= 1.27 cluster
- A LiteLLM 1.83.10 (or compatible) deployment reachable in-cluster at a
  known Service DNS address
- A Kubernetes Secret in the `default` namespace holding the LiteLLM
  master key

Install the operator via Helm (OCI). The command below installs the
latest published chart; for reproducible deploys, pin `--version
<X.Y.Z>` to a release listed on the
[Releases page](https://github.com/ackstorm/alitellm-operator/releases).

```bash
helm install lo oci://ghcr.io/ackstorm/charts/alitellm-operator \
  --namespace default --create-namespace
```

Then create a `LiteLLMConnection/default` CR pointing to your LiteLLM
endpoint and referencing the master-key Secret:

```yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMConnection
metadata:
  name: default
  namespace: default
spec:
  endpoint: http://litellm.default.svc.cluster.local:4000
  masterKeySecretRef:
    name: litellm-master-key
    key:  master-key
```

See [`examples/example-deploy/`](examples/example-deploy/) for runnable
samples covering every CRD.

## Documentation

- Publication / release procedure: [PUBLISH.md](PUBLISH.md)
- Working with this codebase as an AI agent: [CLAUDE.md](CLAUDE.md)
- E2E test environment: [test/e2e/README.md](test/e2e/README.md)
- Deletion semantics (Orphan vs Delete, break-glass annotation):
  [docs/concepts/deletion-semantics.md](docs/concepts/deletion-semantics.md)
- Attribution: [NOTICE](NOTICE)

### Deletion semantics (Issue #23)

By default, deleting a `LiteLLM*` CR removes the operator's finalizer
even if the LiteLLM-side delete cannot be confirmed (`spec.deletionPolicy:
Orphan`, REL-06 anti-storm). GitOps users should set
`spec.deletionPolicy: Delete` so the CR stays in `Terminating` until
LiteLLM acks. Break-glass at runtime via the annotation
`litellm.ackstorm.ai/deletion-policy-override`. See
[docs/concepts/deletion-semantics.md](docs/concepts/deletion-semantics.md)
for the full trade-off, recovery procedures, and the
Discovery-owned-children rule.

## CI overview

`ci.yml` triggers exclusively on `pull_request` against `main`. Pushes to
feature branches and post-merge pushes to `main` do not fire `ci.yml`
(branch protection guarantees the merged commit's content was already
validated as the PR head). Authoritative trigger reference:
[`references/docs/workflow.md`](references/docs/workflow.md).

| Trigger | Workflow | Wall-clock budget |
|---|---|---|
| Push to feature branch (no PR) | none (local `make pre-commit` covers WIP) | n/a |
| PR → main (non-draft) | `ci.yml` (lint + unit + envtest + security + e2e) | ≤ 12 min |
| PR → main (draft) | `ci.yml` (lint + unit + envtest + security; e2e skipped) | ≤ 8 min |
| PR → main (any) | `govulncheck.yml` (HIGH advisory gate vs ack-list) | ≤ 3 min |
| Push to main (post-merge, non-release) | none | n/a |
| Cron Mon 05:08 UTC | `govulncheck.yml` (drift detection) | ≤ 3 min |
| Cron 04:00 UTC + workflow_dispatch | `nightly.yml` (long-soak + leak-soak + fuzz, parallel) | ≤ 60 min |
| `chore(release): v*` commit on main | `release.yml` (tests → bump → image + chart push → gh release → tag) | ≤ 20 min |

Branch protection on `main` requires `Lint`, `Unit`, `Envtest`, `Security`,
and `E2E (kind + helm + ginkgo)` to be green before a PR can merge.
Draft PRs skip e2e unconditionally; the rest of the checks still run.
Docs-only PRs (paths matching `**/*.md`, `docs/**`, `.planning/**`,
`references/**`, `FIX*.txt`, `LICENSE`, `NOTICE`, `CODEOWNERS`,
`.gitignore`) skip `ci.yml` entirely — landing them needs an admin merge
since required checks will show as missing.

## Cutting a release

Releases are driven by a commit on `main` whose message starts with
`chore(release): v<MAJOR>.<MINOR>.<PATCH>`. The workflow does the rest
(manifest bumps, build, sign, publish, tag), in that order — the tag
is created as the LAST step so failed runs never leave orphan tags
on origin.

```bash
git commit --allow-empty -m 'chore(release): v0.1.0'
make pre-push                              # publication safety gates
git push origin main                       # triggers release.yml
```

Pre-releases use the `-alpha`/`-beta`/`-rc` suffix
(e.g. `0.2.0-rc1`) which routes through `.goreleaser.prerelease.yml`.

If you prefer to bundle the release intent with real edits, do so —
`make bump VERSION=X.Y.Z` is available locally and the workflow will
detect a clean tree and skip its own bump step.

A failed pipeline does NOT leave a tag on origin. The bot bump commit
may land on `main` if the failure happened post-bump; the next
`chore(release): v0.1.0` push is idempotent and will re-attempt the
build.

## Out of Scope

The following are explicitly out of scope for v1alpha1:

- `auth.litellm.ai/v1` group (User, VirtualKey, TeamMemberAssociation) —
  delegated to an external identity / user-management system
- `LiteLLMInstance` — replaced by `LiteLLMConnection`
- Multi-namespace and cluster-scoped deployments — deferred to v1beta1
- ValidatingAdmissionWebhook / MutatingAdmissionWebhook — deferred to v1beta1
- Cross-team access groups — delegated to an external identity / user-
  management system

## Attribution

Copyright 2026 ACKstorm. Licensed under the Apache License, Version 2.0.
Portions of the LiteLLM REST client are derived from the
bbdsoftware/litellm-operator project (Apache-2.0). See [NOTICE](NOTICE) for
full attribution details.
