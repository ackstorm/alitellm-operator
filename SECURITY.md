# Security Policy

## Supported Versions

Only the latest minor release receives security fixes.

| Version        | Supported |
| -------------- | --------- |
| latest minor   | yes       |
| older releases | no        |

## Reporting a Vulnerability

**Please do not report security vulnerabilities through public GitHub issues.**

Use GitHub's [private vulnerability reporting](https://github.com/ackstorm/alitellm-operator/security/advisories/new)
to open a draft security advisory. It reaches the maintainers privately and keeps the
disclosure coordinated.

### What to include

- Type of issue (e.g. credential exposure, privilege escalation, SSRF, injection)
- Full paths of the source files involved
- The affected revision (tag, branch or commit)
- Any configuration required to reproduce
- Step-by-step reproduction instructions
- Proof-of-concept or exploit code, if you have it
- Impact — how an attacker would use this

### Response timeline

- **Initial response**: within 48 hours of the report
- **Status updates**: at least every 7 days while the issue is open
- **Resolution**: we aim to resolve critical vulnerabilities within 30 days
- **Disclosure**: coordinated, 90 days from initial report by default

We will acknowledge your contribution in the advisory unless you prefer to remain
anonymous.

## Deployment recommendations

- **Least privilege**: run the operator with the namespaced RBAC it ships with; do not
  grant cluster-admin.
- **Network policy**: restrict operator egress to the LiteLLM proxy and the discovery
  endpoints you configure.
- **Transport**: reach a remote LiteLLM over `https://`; set
  `LITELLM_OPERATOR_REQUIRE_HTTPS_REMOTE=true` to reject plaintext remotes.
- **Secrets**: the LiteLLM master key and provider credentials belong in Kubernetes
  Secrets or an external secret manager, never inline in a CR.
- **Updates**: track releases; LiteLLM endpoint behaviour changes between versions.
