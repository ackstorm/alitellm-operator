# LiteLLMMCPServer

Declarative MCP (Model Context Protocol) server registration on a
LiteLLM proxy. Projected via `POST /v1/mcp/server` /
`PUT /v1/mcp/server` / `DELETE /v1/mcp/server/<id>`.

User-authored and `LiteLLMMCPServerDiscovery`-generated CRs reconcile
identically — the controller does not branch on `ownerReferences`.

## Quick reference

| Field             | Required | Notes                                                                                       |
|-------------------|----------|---------------------------------------------------------------------------------------------|
| `metadata.name`   | yes      | Used as LiteLLM `server_name` after sanitization (see "Name sanitization" below).           |
| `spec.endpoint`   | yes      | MCP server URL. Forwarded verbatim as `url`.                                                |
| `spec.transport`  | yes      | Enum: `http` or `sse`. `stdio` is rejected at admission.                                    |
| `spec.params`     | no       | Free-form bag forwarded verbatim to `NewMCPServerRequest`. `{{NAME}}` substitution applied. |
| `spec.secrets[]`  | no       | Substitution map: `{as, secretRef: {name, key}}`.                                           |

After `kubectl apply`, expect:

- `status.conditions[type=Ready].status=True` / `reason=Synced`.
- `status.lastRendered.serverID` pinned with the sanitized name on
  CREATE (equals `server_name`; see "server_id assignment" below).
  Used by the finalizer (`DELETE /v1/mcp/server/<serverID>`).
- `status.lastRendered.hash` = SHA-256 of the rendered body.

## Minimal example

```yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMMCPServer
metadata:
  name: exa-mcp
  namespace: default
spec:
  endpoint: "https://exa.ai/mcp/sse"
  transport: sse
  params:
    description: "Exa search MCP"
```

After apply, callers reach the MCP server through the LiteLLM proxy
under the alias `exa-mcp`.

## With auth headers

LiteLLM forwards `extra_headers` on every MCP call. Use the secret-
substitution path to inject Bearer tokens / API keys without leaking
them through the CR spec or status:

```yaml
spec:
  endpoint: "https://mcp.example.com/sse"
  transport: sse
  params:
    description: "Internal tools MCP"
    auth_type: api_key
    mcp_access_groups: [team-engineering]
    extra_headers:
      Authorization: "Bearer {{MCP_TOKEN}}"
      X-Tenant:      "acme"
  secrets:
    - as: MCP_TOKEN
      secretRef:
        name: mcp-token
        key:  TOKEN
```

## Accepted pass-through fields

Anything modeled in LiteLLM's `litellm.proxy.management_endpoints.mcp_management.MCPServerRequest`
is forwarded:

- `description`, `mcp_info`
- `auth_type`, `extra_headers`, `static_headers`
- `mcp_access_groups`, `access_groups` (alias — `mcp_access_groups`
  wins when both present)
- arbitrary LiteLLM-future keys (`x-kubernetes-preserve-unknown-fields:
  true`)

Reserved structural keys are silently stripped at extraction time —
the operator stamps them from typed CR fields:

```
server_id, server_name, alias, url, transport, spec_path
```

## server_id assignment

When the operator **creates a new** LiteLLM MCP server, it sets
`server_id == server_name` (the sanitized `metadata.name`) instead of
letting LiteLLM mint a random UUID. The value is persisted in
`status.lastRendered.serverID` and surfaces verbatim in the LiteLLM
UI / API. Verified against LiteLLM 1.83.10: `POST /v1/mcp/server` honors a
caller-supplied `server_id`.

This change is **CREATE-only**:

- **New servers** (no existing record under the name) → `server_id =
  sanitized server_name`.
- **Existing servers** (adopted by name lookup, including pre-change
  records under a server-assigned UUID) → take the UPDATE arm and **keep
  their original UUID**. The operator never rewrites an existing server's
  identity.

Caveats:

- **No automatic migration.** Existing UUID-keyed servers are not migrated.
  To adopt the name-id, delete the record in LiteLLM once; the operator
  recreates it via `POST /v1/mcp/server` with `server_id = server_name`.
- **Cross-namespace name collision is unhandled by design.** `server_id`
  derives from `metadata.name` with no namespace prefix (single-namespace
  deployment assumed — the operator watches exactly one namespace). Two
  `LiteLLMMCPServer` CRs sharing a name across namespaces would collide;
  v1alpha1 does not guard against this.

> A2A agents are **not** pinnable: LiteLLM 1.83.10 ignores a caller-supplied
> `agent_id` on `POST /v1/agents` and always mints a UUID.

## Name sanitization (MCP-05)

LiteLLM rejects its own `MCP_TOOL_PREFIX_SEPARATOR` character inside
`server_name` (HTTP 400 `"Server name cannot contain '<sep>'."`).
Default separator is `-`.

The operator rewrites `metadata.name` for the wire payload, swapping
`-` ↔ `.` (whichever is the configured separator → the opposite). The
K8s-side `metadata.name` is left untouched. Discovery-generated names
(`<discovery>.<source-ns>.<source-name>`) survive unchanged.

Override the separator per Connection:

```yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMConnection
metadata:
  name: default
spec:
  url: http://litellm.litellm.svc:4000
  masterKeySecretRef: { name: litellm-master-key, key: master-key }
  mcpToolPrefixSeparator: "."     # default; switch to "-" if you prefer dot-named servers
```

## Transport handling

`spec.transport ∈ {http, sse}` is admitted by CEL enum and shipped
verbatim. `stdio` is rejected. The Discovery-side normalization
`streamable-http → http` is implemented in
`LiteLLMMCPServerDiscovery`, NOT here.

## Drift detection

Per-reconcile SHA-256 hash of the rendered post-substitution body
(including sanitized `server_name`, `url`, `transport`, and
`spec.params`) is compared against `status.lastRendered.hash`:

- Match → no LiteLLM call.
- Mismatch → `PUT /v1/mcp/server` (wholesale-replace per LiteLLM
  1.83.10).
- Row vanished in LiteLLM (out-of-band DELETE) → `POST /v1/mcp/server`,
  re-pin `serverID`, increment
  `drift_corrected_total{domain=mcpserver,action=create_missing}`.

The vanish probe uses the shared LIST cache (`CachedListMCPServers`)
to coalesce probes across sibling CRs — see
`internal/litellm/list_cache.go`.

## Status: what to read

```bash
kubectl get mcpsrv exa-mcp -o jsonpath='{.status.lastRendered}{"\n"}'
# {"at":"...","hash":"abc...","paramsKeys":["description"],"serverID":"<uuid>"}

kubectl get mcpsrv exa-mcp -o jsonpath='{.status.conditions[?(@.type=="Ready")]}{"\n"}'
# {"type":"Ready","status":"True","reason":"Synced"}
```

`Ready=False` reasons:

- `LiteLLMUnavailable` — `LiteLLMConnection/default` not Ready.
- `LiteLLMRejected` — 4xx (non-401) from LiteLLM. Inspect `message`.
- `SecretNotFound` — Secret missing or placeholder unbound.
- `InvalidConfig` — duplicate `spec.secrets[].as` values, or
  malformed `spec.params` JSON.

## See also

- [Example on GitHub](https://github.com/ackstorm/alitellm-operator/tree/main/examples/example-deploy/04-mcpserver.yaml)
- [LiteLLMMCPServerDiscovery](mcp-server-discovery.md) — auto-generate
  MCPServer CRs from ToolHive label-selected servers.
- [API Reference: LiteLLMMCPServer](../api-reference/litellm.ackstorm.ai.md#litellmmcpserver)
