# LiteLLMMCPServerDiscovery

Pipeline B CR. Points the operator at the cluster's ToolHive
deployment and auto-generates `LiteLLMMCPServer` child CRs for each
`MCPServer` / `VirtualMCPServer` object ToolHive owns. Discovery
NEVER calls LiteLLM — each generated child reconciles into LiteLLM
via the `LiteLLMMCPServer` controller (Pipeline A).

In v1alpha1 only `spec.type: toolhive` is supported.

## Quick reference

| Field                         | Required | Notes                                                                                       |
|-------------------------------|----------|---------------------------------------------------------------------------------------------|
| `spec.type`                   | yes      | Only `toolhive` in v1alpha1.                                                                |
| `spec.prefix`                 | yes      | DNS-1123 label prepended to each child's `metadata.name`. MaxLength=30.                     |
| `spec.toolhive.namespaces`    | yes      | Namespaces to watch for ToolHive objects. MinItems=1.                                       |
| `spec.toolhive.kinds`         | no       | List of `MCPServer`, `VirtualMCPServer`. Default: both.                                     |
| `spec.params`                 | no       | Pass-through bag propagated VERBATIM into every child's `spec.params`.                      |
| `spec.secrets[]`              | no       | Substitution map propagated into every child's `spec.secrets[]`.                            |
| `spec.filters.include`        | no       | RE2 patterns — anchored, include-FIRST, against the dotted post-derivation name.            |
| `spec.filters.exclude`        | no       | RE2 patterns — applied AFTER include.                                                       |
| `spec.refresh.interval`       | yes      | Cadence between refreshes. CEL floor `1m` enforced at admission.                            |

## No upstream-source credentials (MSDISC-04)

The CR has NO field for ToolHive credentials. ToolHive reads are
authorized via the operator's cluster-scoped ServiceAccount RBAC
(`config/rbac/toolhive_clusterrole.yaml`). The schema-level absence is
intentional — credentials for the discovery source vs credentials for
inference MUST flow through different mechanisms.

## Minimal example

```yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMMCPServerDiscovery
metadata:
  name: toolhive
  namespace: default
spec:
  type: toolhive
  prefix: th
  toolhive:
    namespaces: [default]
  refresh:
    interval: 1m
```

A ToolHive `MCPServer/ctx7` in `default` becomes a child CR named
`th-ctx7` whose `spec.endpoint` mirrors the ToolHive object's
`status.url` and whose `spec.transport` is normalized per:

| ToolHive `status.transport` | Child `spec.transport` |
|----------------------------|------------------------|
| `streamable-http`          | `http`                 |
| `sse`                      | `sse`                  |
| absent                     | `http`                 |
| anything else (`stdio`, …) | SKIPPED (`InvalidTransport`) |

## With propagated inference auth

If the upstream MCP servers expect Bearer tokens, declare them once
on the Discovery — every generated child inherits the substitution
map:

```yaml
spec:
  type: toolhive
  prefix: th
  toolhive:
    namespaces: [default, tools]
  params:
    extra_headers:
      Authorization: "Bearer {{MCP_TOKEN}}"
  secrets:
    - { as: MCP_TOKEN, secretRef: { name: mcp-token, key: TOKEN } }
  refresh:
    interval: 5m
```

The placeholder is resolved on EACH CHILD's reconcile (not
Discovery's). Secret material never enters Discovery's logs / status.

## Lazy informer — ToolHive CRDs may be absent

If `toolhive.stacklok.dev/v1beta1` is NOT installed in the cluster,
the reconciler surfaces `Ready=False, reason=SourceUnreachable` and
retries every minute. When ToolHive lands, the lazy informer
registers and discovery converges automatically — no operator restart
required.

## Child name generation (FIX4 H-2 v0.3.0 — breaking)

```
<spec.prefix>-<toolhive-object-name>
```

Pre-v0.3.0 used three dotted components
(`<discovery>.<source-ns>.<source-name>`). v0.3.0 dropped the source
namespace; cross-namespace name disambiguation is now the user's job
via `spec.prefix`. Two ToolHive objects in different namespaces
producing the same `<prefix>-<name>` → first wins, second appears in
`status.skippedCandidates[reason=NameCollision]`. Fix: split into
prefix-distinct Discoveries.

## Filter target

`spec.filters.{include,exclude}` patterns match the POST-DERIVATION
dotted name `<discovery-name>.<toolhive-namespace>.<toolhive-name>`,
NOT the bare ToolHive object name. Anchored RE2 syntax. Include-first
(strict), exclude-second (lenient) — identical semantics to
`LiteLLMModelDiscovery`.

## Status — what to read

```bash
kubectl get msdisc toolhive -o jsonpath='{.status}{"\n"}'
# {"discoveredCount":3,"generatedCount":2,"generatedChildren":["th-ctx7","th-fetch"],
#  "skippedCandidates":[{"name":"toolhive.default.local-stdio","reason":"InvalidTransport"}],
#  "lastRefreshAt":"...","conditions":[...]}

kubectl get msdisc                  # printer cols: Type Ready Reason Discovered Generated Age
```

`Ready` reasons:

- `Synced` — happy path.
- `SourceUnreachable` — ToolHive CRDs absent or apiserver returned
  error. Reconciler retries every minute.
- `SecretNotFound` — a propagated `spec.secrets[].secretRef` is
  missing IN THE DISCOVERY'S NAMESPACE (validated at propagation time).
- `InvalidConfig` — bad RE2 pattern, duplicate `spec.secrets[].as`.
- `UpstreamInvalid` — non-empty `filters.include` produced zero
  matches.

`SourceReachable` reasons: `Ok`, `Unreachable`. Used as the gate for
the diff-and-delete vanish path — when `False`, deletions are SKIPPED
to prevent flapping during ToolHive outages.

Invariant:
`discoveredCount == generatedCount + len(skippedCandidates) + len(failedCandidates)`
(filtered-out IDs do NOT count in `discoveredCount`).

## Deletion

The finalizer waits for owned children to drain via
`ownerReferences.blockOwnerDeletion=true`. Each child MCPServer's own
finalizer issues `DELETE /v1/mcp/server/<server_id>` against LiteLLM.
Discovery itself issues NO LiteLLM call.

## See also

- [Example on GitHub](https://github.com/ackstorm/alitellm-operator/tree/main/examples/example-deploy/05-mcpserverdiscovery.yaml)
- [LiteLLMMCPServer](mcp-server.md) — child shape.
- [API Reference: LiteLLMMCPServerDiscovery](../api-reference/litellm.ackstorm.ai.md#litellmmcpserverdiscovery)
