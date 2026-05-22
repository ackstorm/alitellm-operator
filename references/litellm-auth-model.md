# LiteLLM auth model — master key vs virtual key

> Internal reference. Audience: operator maintainers and AI agents
> working on the operator's LiteLLM HTTP surface. NOT published on
> the public mkdocs site.

## What the operator uses today

The operator authenticates against LiteLLM using the proxy's **master
key** (`LITELLM_MASTER_KEY` env on the proxy at startup; `sk-`-prefixed
value, length 67 in the ackstorm prod cluster). The master key reaches
every endpoint the operator calls today:

| Endpoint                                    | Caller                          |
|---------------------------------------------|---------------------------------|
| `GET /v1/models`                            | `LiteLLMConnection` probe       |
| `POST /model/new`                           | `LiteLLMModel` CREATE           |
| `POST /model/update`                        | `LiteLLMModel` UPDATE / adoption|
| `POST /model/delete`                        | `LiteLLMModel` finalizer        |
| `GET /v1/model/info`                        | safety relist, adoption probe   |
| `POST /v1/mcp/server`, `PUT /v1/mcp/server` | `LiteLLMMCPServer`              |
| `DELETE /v1/mcp/server/{id}`                | `LiteLLMMCPServer` finalizer    |
| `POST /v1/agents`, `PUT /v1/agents/{id}`    | `LiteLLMA2AAgent`               |
| `DELETE /v1/agents/{id}`                    | `LiteLLMA2AAgent` finalizer     |
| `POST /team/new`, `POST /team/update`       | `LiteLLMTeam`                   |
| `GET /v2/team/list`                         | `LiteLLMTeam` name resolution   |
| `POST /team/delete`                         | `LiteLLMTeam` finalizer         |

The master key is fetched from a `LiteLLMConnection.spec.masterKeySecretRef`
secret on every connection-cache rebuild (Phase 2 D-03 / CONN-01); the
env-var entrypoint (`LITELLM_MASTER_KEY`) is no longer consumed by the
manager. A LiteLLMConnection CR + Secret is the single source of truth.

## What LiteLLM offers beyond the master key

LiteLLM has a second key type: **virtual keys**, also `sk-`-prefixed,
minted via `POST /key/generate`. Virtual keys carry per-key scopes
(allowed models, allowed team_id, allowed mcp_servers), quotas, rate
limits, and TTL.

The operator does NOT mint or use virtual keys today.

## FIX4 L-3 evidence — close-out

During the v0.1.2 → v0.2.0 upgrade smoke-test on 2026-05-22, an earlier
probe against `/v1/model/info` returned HTTP 401 with the body

```
"LiteLLM Virtual Key expected. Received=****, expected to start with 'sk-'."
```

This was misdiagnosed as a LiteLLM-side endpoint-policy tightening (the
filed concern was that LiteLLM was moving `/v1/model/info` to a
virtual-key-only scope, which would have broken the operator).

A re-probe against v0.2.0 on the same cluster with the same master key
showed the key works against `/v1/models`, `/model/new`,
`/v1/model/info`, and `/model/delete`. The earlier 401 was therefore
transient — most likely a secret-rotation race where the probe pod's
mounted env was being rewritten while the request was in flight, OR a
LiteLLMConnection 401 cache-invalidation hitting during the probe
window.

**There is no LiteLLM-side policy tightening.** No code change is
required from this finding. This document exists so the next agent
investigating a similar 401 does not waste cycles re-litigating it.

## Future work — per-CR isolation

The LiteLLM Models table's `model_info.created_by` column today shows
a single operator identity (`alitellm-operator/<version>`) for every
row the operator manages. The user has flagged interest in showing
per-CR or per-namespace identities — e.g. `team-alpha/anthropic-claude-3-5-sonnet`
in the Created By column when the row is owned by the `team-alpha`
team's CR.

That capability requires per-CR or per-namespace virtual keys (each
with its own audit identity, minted by the operator and stored in a
sidecar Secret or in-memory cache). Tracking item:

- **Out of scope for v0.3.0.**
- Reconsider during the v1alpha2 → v1beta1 API rev when the operator's
  auth surface is already changing.

## Cross-references

- `internal/identity/identity.go` — the literal stamped today.
- `FIX4.txt` H-1 + L-3 — origin findings.
- `CHANGELOG.md` v0.2.1 — release entry referencing this doc.
