# Contributing to alitellm-operator

Thanks for considering a contribution. Please read the
[Code of Conduct](CODE_OF_CONDUCT.md) first.

## Setting up

Your host needs Docker and nothing else. Every toolchain `make` target re-invokes itself
inside the devtools container (`scripts/dev.sh`). See
[development.md](docs/developer-guide/development.md).

## Before you open a pull request

```sh
make qa-lint       # golangci-lint
make test-unit     # pure logic
make test-envtest  # controllers against a real API server
make e2e-full      # kind + Helm + Ginkgo
make hooks         # once per clone: installs the pre-push gate
```

## House rules

- **Every `.go` file starts with** `// SPDX-License-Identifier: MIT`.
- **Conventional commits**, imperative subject under 72 characters.
- **No naked polling loops** — use the `wait-*` make targets.
- Significant changes (new CRD, breaking change, controller behaviour) start as a design
  doc under `docs/` in a pull request, before the code.

See [contributions.md](docs/developer-guide/contributions.md) for the full workflow.

## Reporting

- Bugs and features: GitHub issues.
- Security vulnerabilities: **not** an issue — see [SECURITY.md](SECURITY.md).
