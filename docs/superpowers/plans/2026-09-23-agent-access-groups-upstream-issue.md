# `agent_access_groups` cannot be set on DB agents via `/v1/agents`

## Summary

LiteLLM's database-backed A2A agents already have an
`agent_access_groups` column and the permission checks consume it, but the
`/v1/agents` request/response and persistence paths do not expose it. A client
cannot create or update a DB agent with access-group tags, and the admin UI
cannot read the tags back.

Verified against LiteLLM `v1.102.1`:

- `litellm/types/agents.py:181` — `AgentConfig` has no
  `agent_access_groups` field.
- `litellm/types/agents.py:194` — `PatchAgentRequest` has no
  `agent_access_groups` field.
- `litellm/types/agents.py:216` — `AgentResponse` does not return the field.
- `litellm/proxy/agent_endpoints/agent_registry.py:475` —
  `add_agent_to_db` does not write it.
- `litellm/proxy/agent_endpoints/agent_registry.py:576` —
  `patch_agent_in_db` does not write it when present.
- `litellm/proxy/agent_endpoints/agent_registry.py:664` —
  `update_agent_in_db` does not write it.

Because FastAPI validates request bodies against these types, the field is
dropped before the registry writers run. The response omission also leaves the
admin UI's agent-tag selector empty.

## Existing consumers

The field is already consumed by the agent permission path for both listing and
invoking agents. Team/key `object_permission.agent_access_groups` is honored,
and the admin UI builds its "Agents / Access Groups" options from agent tags.
The missing behavior is specifically the DB-agent write/read path.

## Related work

- [Issue #32799](https://github.com/BerriAI/litellm/issues/32799)
- [PR #32811](https://github.com/BerriAI/litellm/pull/32811)

Those changes cover config-backed agents, but do not fix the three
`*_agent_in_db` writers or the DB-agent CRUD response path.

## Proposed patch

Until the complete fix lands upstream, the following startup patch applies
anchored replacements before LiteLLM builds its FastAPI routes:

```diff
--- a/litellm/types/agents.py
+++ b/litellm/types/agents.py
@@ AgentConfig / PatchAgentRequest
+    agent_access_groups: list[str] | None
@@ AgentResponse
+    agent_access_groups: list[str] | None = None

--- a/litellm/proxy/agent_endpoints/agent_registry.py
+++ b/litellm/proxy/agent_endpoints/agent_registry.py
@@ add_agent_to_db
+    if agent.get("agent_access_groups") is not None:
+        create_data["agent_access_groups"] = list(agent.get("agent_access_groups") or [])
@@ patch_agent_in_db
+    if "agent_access_groups" in agent:
+        update_data["agent_access_groups"] = list(agent.get("agent_access_groups") or [])
@@ update_agent_in_db
+    "agent_access_groups": list(agent.get("agent_access_groups") or []),
```

The deployed implementation is the idempotent,
`--strict`-verifiable `apps/litellm/base/files/patch_agent_access_groups.py`
startup patch in the Ackstorm gitops repository. It fails open at boot when an
upstream anchor moves and fails loudly in CI with `--strict`.

## Acceptance criteria

- `POST /v1/agents` accepts and persists `agent_access_groups`.
- `PATCH /v1/agents/{id}` persists the field when supplied.
- `PUT /v1/agents/{id}` replaces the field, clearing it when absent.
- `AgentResponse` returns `agent_access_groups` for create, update, and list.
