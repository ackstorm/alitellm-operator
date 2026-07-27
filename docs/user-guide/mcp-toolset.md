# LiteLLMMCPToolset

A **toolset** is a named, curated subset of MCP tools drawn from one or more
MCP servers — instead of granting a team a whole server, you grant it exactly
the tools you picked. Projected via `POST /v1/mcp/toolset` /
`PUT /v1/mcp/toolset` / `DELETE /v1/mcp/toolset/<id>`.

Requires LiteLLM **1.93.0 or newer** — the `/v1/mcp/toolset` endpoints do not
exist before that.

## Quick reference

| Field                   | Required | Notes                                                                        |
|-------------------------|----------|------------------------------------------------------------------------------|
| `metadata.name`         | yes      | Used verbatim as LiteLLM `toolset_name`. Unique server-side.                 |
| `spec.from[]`           | no       | Per-server tool selections. Flattened into LiteLLM's `tools` pair list.      |
| `spec.from[].server`    | yes      | `LiteLLMMCPServer` CR name **or** a raw `server_id` UUID. See below.         |
| `spec.from[].tools[]`   | no       | Explicit tool names. **No globs.**                                           |
| `spec.description`      | no       | Free text, forwarded verbatim.                                              |
| `spec.deletionPolicy`   | no       | `Orphan` (default) or `Delete`.                                             |

After `kubectl apply`, expect:

- `status.conditions[type=Ready].status=True` / `reason=Synced`.
- `status.lastRendered.toolsetID` — the LiteLLM-assigned UUID. Used by the
  finalizer (`DELETE /v1/mcp/toolset/<toolsetID>`).
- `status.lastRendered.hash` = SHA-256 of the rendered body.

## Minimal example

```yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMMCPToolset
metadata:
  name: research-tools
  namespace: default
spec:
  description: "Curated research subset"
  from:
    - server: hindsight          # a LiteLLMMCPServer CR in this namespace
      tools:
        - web_search
        - fetch_page
    - server: confluence
      tools:
        - search_pages
```

This produces one LiteLLM toolset named `research-tools` carrying three
`{server_id, tool_name}` pairs.

## A toolset is inert until a team grants it

Creating the CR registers the toolset but grants nobody access. Reference it
from a team:

```yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMTeam
metadata:
  name: research-team
spec:
  permission:
    models: ["gpt-4o"]
    mcpToolsets: ["research-tools"]
```

A key that has not been granted the toolset gets:

```
403 API key does not have access to toolset '<uuid>'
```

Multiple toolsets listed on one team are **unioned** by LiteLLM, not
last-wins. There is no access-group concept for toolsets in LiteLLM 1.93.0 —
listing several in `mcpToolsets` IS the grouping mechanism.

Ordering matters: the toolset must exist in LiteLLM before the team
references it, or the team parks `Ready=False, reason=ToolsetNotFound` and
requeues. It recovers on its own once the toolset appears.

## No globs — tool names are explicit

`spec.from[].tools` takes literal tool names. There is no `*`, no `include:`,
no `exclude:`.

This is deliberate. Expanding a pattern would require the operator to
enumerate a server's tools via `GET /v1/mcp/tools`, which needs the MCP server
to be live, reachable, and readable by the operator's key — frequently false
in real deployments (a server behind an unreachable endpoint, or one the
operator key cannot introspect). Rather than make toolset reconciliation
depend on MCP server availability, the operator treats `spec.from` as pure
data.

## No validation — bad names are inert, not fatal

Neither the operator nor LiteLLM validates that a server or tool exists:

```yaml
spec:
  from:
    - server: hindsigt          # typo — accepted, CR goes Ready=Synced
      tools: [web_serch]        # typo — accepted, CR goes Ready=Synced
```

LiteLLM accepts a nonexistent `server_id` or `tool_name` with `201` and the
toolset simply grants nothing. Deleting a referenced MCP server leaves a
dangling reference with no cascade — the toolset survives and resolves to an
empty tool list.

**Diagnosing an empty toolset.** If a toolset seems to grant nothing, read it
back and check the pairs actually landed:

```bash
curl -H "Authorization: Bearer $LITELLM_KEY" $LITELLM/v1/mcp/toolset
```

Why it is silent: LiteLLM's toolset resolution wraps the whole lookup in
`try/except → return {}`, and an unknown `server_id` never matches a real
server in the fail-closed server-access check. No exception reaches the proxy
logs.

## `spec.from[].server` — CR name or raw UUID

The operator translates the value best-effort, and **never fails**:

1. If a `LiteLLMMCPServer` CR with that name exists in the same namespace, its
   `status.lastRendered.serverID` is sent to LiteLLM.
2. Otherwise the string is forwarded **verbatim** — which is exactly right
   when you supply a raw `server_id` UUID for a server the operator does not
   manage (adopted / out-of-band).

A CR that exists but has not reconciled yet (empty `serverID`) also forwards
verbatim rather than sending an empty `server_id`. The rendered hash covers
the *resolved* ids, so once the MCPServer CR syncs the toolset re-renders with
the real id automatically.

The value is forwarded **without sanitization** — a raw UUID must survive
untouched.

## `toolset_id` assignment and adoption

Unlike `LiteLLMTeam` (`team_id` = `metadata.name`) and `LiteLLMMCPServer`
(`server_id` = sanitized `metadata.name`), the toolset id is **server-minted**:
LiteLLM 1.93.0 ignores a caller-supplied `toolset_id` and mints its own UUID.
The operator reads it from the create response. Same posture as A2A
`agent_id`.

Adoption therefore goes through `toolset_name`, which is unique server-side. If
a toolset with the CR's name already exists (operator restart, or an
out-of-band create), the create returns `409` and the operator adopts the
existing entry — pushing the CR's rendered state onto it via `PUT` rather than
parking.

## Revocation

Shrinking `spec.from` — including down to empty — **does** revoke. The
operator sends `tools` on every update, as an explicit `[]` when empty, never
omitted. An omitted field would let LiteLLM's per-field merge keep the stale
tool list, which would be a silent authorization leak.

```yaml
spec:
  from: []      # revokes every tool; the toolset survives, granting nothing
```

## Deletion

The finalizer `mcptoolsets.litellm.ackstorm.ai/finalizer` issues
`DELETE /v1/mcp/toolset/<toolsetID>` before the CR leaves etcd.
`spec.deletionPolicy` controls what happens when that delete cannot be
confirmed (LiteLLM unreachable, 401): `Orphan` (default) drains the finalizer
anyway; `Delete` blocks until the removal is confirmed. See
[Deletion Semantics](../concepts/deletion-semantics.md).

Deleting a toolset does **not** cascade to the teams that granted it — their
`object_permission.mcp_toolsets` keeps the now-dangling UUID until the Team CR
reconciles again.
