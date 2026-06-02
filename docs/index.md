# alitellm-operator

A Kubernetes operator that reconciles
[LiteLLM](https://github.com/BerriAI/litellm) proxy resources declaratively.

## Overview

**a**(nother) **litellm**-operator — the leading `a` acknowledges the
existing ecosystem of LiteLLM operators
([bbdsoftware/litellm-operator](https://github.com/bbdsoftware/litellm-operator),
[PalenaAI/litellm-operator](https://github.com/PalenaAI/litellm-operator),
and others). This one focuses on the GitOps-facing declarative surface and
on coexistence with hand-managed LiteLLM entries and an external identity
system.

`alitellm-operator` manages an existing LiteLLM proxy deployment from the
Kubernetes side: connection metadata, teams, models, and discovery surfaces
are expressed as Custom Resources and reconciled against the proxy's API.

CRDs in API group `litellm.ackstorm.ai/v1alpha1`:

- **LiteLLMConnection** — connection to a LiteLLM proxy (URL + master-key Secret)
- **LiteLLMTeam** — team with quota, rate-limit, and member metadata
- **LiteLLMModel** — model registration (provider, params) under a Connection
- **LiteLLMA2AAgent** — Agent-to-Agent endpoint surface
- **LiteLLMMCPServer** — MCP (Model Context Protocol) server registration
- **LiteLLMMCPServerDiscovery** — auto-discover MCP servers via selectors
- **LiteLLMModelDiscovery** — auto-discover available models from a Connection
- **LiteLLMGuardRail** — content filter / DLP guardrail (BLOCK / MASK rules,
  pre_call or post_call mode; `litellm_content_filter`, Aporia, Bedrock, etc.)

## Architecture

![Architecture](assets/lite-llm-architecture2.png)

## Quick Start

```bash
# Helm — from OCI registry (recommended)
helm install --namespace litellm alitellm-operator \
  oci://ghcr.io/ackstorm/charts/alitellm-operator --version <version>

# Helm — from local chart
helm install --namespace litellm alitellm-operator ./deploy/helm/alitellm-operator

# Kustomize — from local manifests
kubectl --namespace litellm apply -k config/default
```

See the [Installation Guide](getting-started/installation.md) for prerequisites
and the [User Guide](user-guide/index.md) for per-resource usage.

## Community

- [Report Issues](https://github.com/ackstorm/alitellm-operator/issues)
- [Discussions](https://github.com/ackstorm/alitellm-operator/discussions)
- [Contributing](developer-guide/contributions.md)

## License

Apache License 2.0 — see [LICENSE](https://github.com/ackstorm/alitellm-operator/blob/main/LICENSE).
This project derives from the [bbdsoftware/litellm-operator](https://github.com/bbdsoftware/litellm-operator)
project (Apache-2.0). See [NOTICE](https://github.com/ackstorm/alitellm-operator/blob/main/NOTICE)
for attribution.
