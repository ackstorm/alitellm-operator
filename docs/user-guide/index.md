# User Guide

`alitellm-operator` reconciles seven Kubernetes Custom Resources under the
`litellm.ackstorm.ai/v1alpha1` API group. Each CR maps to one declarative
intent against an upstream LiteLLM proxy.

| Resource                       | Purpose                                                                |
| ------------------------------ | ---------------------------------------------------------------------- |
| [LiteLLMConnection](connection.md)             | Connection to a LiteLLM proxy instance (URL + master-key secret).      |
| [LiteLLMTeam](team.md)                         | Team in LiteLLM with quota, rate-limit, and member metadata.           |
| [LiteLLMModel](model.md)                       | Model registration (provider, params) under a Connection.              |
| [LiteLLMA2AAgent](a2a-agent.md)                | Agent-to-Agent endpoint surface exposed via the proxy.                 |
| [LiteLLMMCPServer](mcp-server.md)              | MCP (Model Context Protocol) server registration.                      |
| [LiteLLMMCPServerDiscovery](mcp-server-discovery.md) | Auto-discover MCP servers via labels/selectors.                  |
| [LiteLLMModelDiscovery](model-discovery.md)    | Auto-discover available models from a Connection.                      |

See [API Reference](../api-reference/litellm.ackstorm.ai.md) for the full schema of each
resource (generated from the CRD bases).
