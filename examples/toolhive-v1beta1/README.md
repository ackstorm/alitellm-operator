# ToolHive v1beta1 — Sample Manifests

Real-world MCPServer / MCPGroup / VirtualMCPServer manifests for the `toolhive.stacklok.dev/v1beta1` API. Use these as starting points for production deployments; the Tier 2 integration test fixture (`test/tier2/mcpserverdiscovery_test.go`) uses synthetic placeholders instead so it does not depend on registry network egress.

## Prerequisites

- ToolHive operator installed (v0.28.0+ with v1beta1 CRDs present, or operator-crds chart with `--set v1beta1.enabled=true`).
- alitellm-operator installed in the cluster (the operator that discovers these MCPServers via `MCPServerDiscovery`).
- Namespace `mcp` (or change `.metadata.namespace` in each manifest).
- Image-pull network egress from the cluster — every MCPServer below references a public registry image.

## Apply Order

```bash
# 1. Groups first — MCPServers reference groups via spec.groupRef.name
kubectl apply -f mcpgroup-dev.yaml -f mcpgroup-search.yaml

# 2. MCPServers
kubectl apply -f mcpserver-context7.yaml \
              -f mcpserver-deepwiki.yaml \
              -f mcpserver-gofetch.yaml

# 3. VirtualMCPServer aggregators (after their target groups' servers are Ready)
kubectl apply -f virtualmcpserver-dev.yaml -f virtualmcpserver-search.yaml
```

## File Inventory

| File | Kind | Group | Notes |
|------|------|-------|-------|
| `mcpgroup-dev.yaml` | MCPGroup | — | Prerequisite for `context7`, `deepwiki` |
| `mcpgroup-search.yaml` | MCPGroup | — | Prerequisite for `gofetch`, VirtualMCPServer `search` |
| `mcpserver-context7.yaml` | MCPServer | dev | stdio → streamable-http proxied via `npx @upstash/context7-mcp` |
| `mcpserver-deepwiki.yaml` | MCPServer | dev | stdio → streamable-http via supercorp/supergateway → mcp.deepwiki.com |
| `mcpserver-gofetch.yaml` | MCPServer | search | Native `streamable-http` transport (no proxy) |
| `virtualmcpserver-dev.yaml` | VirtualMCPServer | dev | Aggregator over `context7`+`deepwiki` (circuit breaker + prefix dedup) |
| `virtualmcpserver-search.yaml` | VirtualMCPServer | search | Aggregator over `gofetch` (circuit breaker + prefix dedup) |

## Excluded From This Set

- **firecrawl** — requires an external secret (`externalsecret-genai-tokens`) and an API key, which makes the example non-self-contained. Add it manually in your cluster if you have the secret.

## See Also

- `internal/toolhive/informer.go` — v1beta1 informer (v1alpha1 support removed 2026-07-30)
- `test/e2e/mcpserverdiscovery_test.go` — v1beta1 propagation test
- ToolHive upstream: <https://github.com/stacklok/toolhive>
