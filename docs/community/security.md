# Security Policy

The canonical security policy lives in
[`SECURITY.md`](https://github.com/ackstorm/alitellm-operator/blob/main/SECURITY.md)
at the repository root. Please read it before reporting a vulnerability.

## Quick reference

**Do NOT** open public issues for security vulnerabilities.

**Do** use GitHub's
[private vulnerability reporting](https://github.com/ackstorm/alitellm-operator/security/advisories/new),
or email the maintainers (contacts are listed in
[`MAINTAINERS.md`](https://github.com/ackstorm/alitellm-operator/blob/main/MAINTAINERS.md)).

## Operator-specific hardening tips

- **RBAC** — review the role rendered by `make gen-manifests` and grant only the
  namespaces/resources you actually reconcile.
- **Master key Secret** — the Secret referenced by `LiteLLMConnection.spec.masterKeySecretRef`
  is the LiteLLM proxy's privileged credential. Restrict who can `get` it.
- **Network policies** — the operator only needs egress to the proxy URL.
  See `config/network-policy/` for a sample.
- **Supply chain** — release images are signed with cosign keyless OIDC and
  ship a CycloneDX SBOM; verify both before deploying production images
  (see [Release Process](../developer-guide/release-process.md)).
