# LiteLLMA2AAgent

Declarative A2A (Agent-to-Agent) endpoint exposed via the LiteLLM
proxy. Projected via `POST /v1/agents` / `PUT /v1/agents/<id>` /
`DELETE /v1/agents/<id>`.

The A2A "agent card" (capabilities, skills, default I/O modes) is
declared inside the CR; the operator stamps `agent_name` from
`metadata.name` and overlays the `agent_card_params.url` from
`spec.endpoint`.

## Quick reference

| Field             | Required | Notes                                                                                       |
|-------------------|----------|---------------------------------------------------------------------------------------------|
| `metadata.name`   | yes      | Used as LiteLLM `agent_name`. Overrides any `spec.params.agent_name`.                       |
| `spec.endpoint`   | yes      | Agent URL reachable from the LiteLLM pod. Overlays `agent_card_params.url`.                 |
| `spec.agentCard`  | yes      | A2A protocol card (name, version, description, capabilities, skills, defaultInputModes, …). |
| `spec.params`     | no       | Top-level `AgentConfig` bag (NOT inside `agent_card_params`).                               |
| `spec.secrets[]`  | no       | Substitution map — placeholders work in `spec.params` AND `spec.agentCard`.                 |
| `spec.exposeAsModel` | no    | Projects a generated `LiteLLMModel` named `agent.<name>` so the agent appears in `/v1/models`. |

After `kubectl apply`, expect:

- `status.conditions[type=Ready].status=True` / `reason=Synced`.
- `status.lastRendered.agentID` pinned with the LiteLLM-assigned
  UUID. Used by the finalizer (`DELETE /v1/agents/<agentID>`).
- `status.lastRendered.hash` = SHA-256 of the rendered body.

## Minimal example

```yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMA2AAgent
metadata:
  name: vendor-research-agent
  namespace: default
spec:
  endpoint: "http://vendor-research-agent.default.svc:9001"
  agentCard:
    name: "vendor-research"
    version: "1.0"
    description: "Vendor research A2A agent"
  params:
    timeout: 30
```

Wire body (abbreviated):

```json
{
  "agent_name": "vendor-research-agent",
  "agent_card_params": {
    "name": "vendor-research",
    "version": "1.0",
    "description": "Vendor research A2A agent",
    "url": "http://vendor-research-agent.default.svc:9001"
  },
  "timeout": 30
}
```

Note `spec.params.timeout` lands at the top level — `AgentConfig`
diverges from MCP where `spec.params` would nest under a wrapper.

## With secret-substituted auth

Placeholders work in BOTH bags (two-pass substitution, shared secret
map):

```yaml
spec:
  endpoint: "https://research-agent.example.com"
  agentCard:
    name: "research"
    description: "Research agent — Bearer-auth"
    authorization:
      type: bearer
      bearer: "{{AGENT_TOKEN}}"
  params:
    extra_headers:
      X-Tenant: "{{TENANT_ID}}"
  secrets:
    - { as: AGENT_TOKEN, secretRef: { name: agent-creds, key: TOKEN } }
    - { as: TENANT_ID,   secretRef: { name: agent-creds, key: TENANT } }
```

`spec.secrets[].as` declared but unreferenced in either bag → Normal
Event `reason=UnusedSecretRef` (advisory, not a failure).

## ProjectionOverride collisions (4)

The operator stamps four structural keys on top of user input. Each
collision emits a Warning Event keyed by the offending field — at
most one per reconcile pass per key:

| Key                       | Source of truth                    | What the operator overlays                              |
|---------------------------|------------------------------------|---------------------------------------------------------|
| `agent_name`              | `metadata.name`                    | Drops `spec.params.agent_name`.                         |
| `agent_card_params`       | `spec.agentCard`                   | Drops `spec.params.agent_card_params`.                  |
| `agent_card_params.url`   | `spec.endpoint`                    | Drops `spec.agentCard.url`.                             |
| `model_info`              | LiteLLM-reserved (per spec §6.6)   | NOT overlaid — passes through, warning only.            |

To avoid the warnings, do not set the colliding keys in `spec.params`
/ `spec.agentCard` — let the operator stamp them.

## Access-group tags

Use `access_groups` as the alias of `agent_access_groups` in `spec.params`:

```yaml
spec:
  params:
    access_groups: ["agents"]
```

The operator forwards the selected tags as `agent_access_groups`. Teams reach
tagged agents through `LiteLLMTeam.spec.permission.agentGroups` or
`LiteLLMAccessGroup.spec.agentGroups`. The LiteLLM startup patch is required
until upstream accepts, persists, and returns the field on `/v1/agents`.

## Drift detection

Per-reconcile SHA-256 hash over the rendered merged body (params +
agentCard + overlays). On mismatch the reconciler issues
`PUT /v1/agents/<agentID>` (wholesale-replace per LiteLLM 1.83.10).
Vanish-probe path: row missing → `POST /v1/agents`, re-pin `agentID`,
increment `alitellm_operator_drift_corrected_total{domain=a2aagent,action=create_missing}`.

`agent_name` is unique server-side, so that recreate can be rejected with
`400 Agent with name <name> already exists` — LiteLLM's create-time check and
its `GET /v1/agents` listing both read the in-memory `AGENT_REGISTRY`, and the
two have been observed disagreeing (1.99.1). The operator then **adopts the
existing agent by name** and `PUT`s the rendered state onto its `agent_id`
rather than parking the CR, incrementing
`alitellm_operator_drift_corrected_total{domain=a2a,action=adopted_by_name}`.
Adoption preserves the `agent_id` embedded in
`agent_card_params.supportedInterfaces[].url` (`/a2a/<agent_id>`), which
consumers may hold. A rejection the adoption cannot resolve leaves
`status.lastRendered.agentID` intact, so the next reconcile goes back to the
`PUT` path once LiteLLM agrees the agent exists.

## Exposing an agent as a model

LiteLLM keeps agents and models in two **disjoint registries**. `GET /v1/agents`
lists agents; `GET /v1/models` lists model rows; nothing bridges them. So a
registered agent is callable, but invisible to any client that builds its model
list from `/v1/models` — which is most of them, including LibreChat and Open
WebUI.

`spec.exposeAsModel` closes that gap by projecting the agent into a generated
`LiteLLMModel` named `agent.<metadata.name>`:

```yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMA2AAgent
metadata:
  name: support-triage
spec:
  endpoint: "http://achagent-classifier.ach.svc.cluster.local:8080/a2a/classify"
  agentCard:
    name: support-triage
    description: "Classifies a support conversation."
  exposeAsModel:
    accessGroups: ["a2a"]
```

produces:

```yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMModel
metadata:
  name: agent.support-triage          # <- what clients call
  labels:
    litellm.ackstorm.ai/generated-by-agent: support-triage
  ownerReferences: [ { kind: LiteLLMA2AAgent, name: support-triage, ... } ]
spec:
  params:
    model: "a2a1/support-triage"      # <- provider prefix + agent_name
  info:
    mode: chat
    description: "Classifies a support conversation."
    access_groups: ["a2a"]
```

Users call it as **`agent.support-triage`** — the CR name. `a2a1/support-triage`
is only the `litellm_params.model`; calling it directly returns
`400 Invalid model name`, because the proxy resolves the public model name
before it ever routes to a provider.

### The provider prefix must exist in LiteLLM

The operator does not ship the provider that speaks A2A — it only names it.
`A2A_MODEL_PROVIDER` (default `a2a1`) must match a provider your LiteLLM
deployment can actually route:

```bash
kubectl set env -n litellm-system deploy/alitellm-operator A2A_MODEL_PROVIDER=a2a1
```

LiteLLM ships a built-in `a2a/` model prefix, but on 1.99.1 it is unusable for
agents that need auth or speak A2A 1.0: it re-resolves the agent through an
in-memory registry (so per-agent headers are dropped) and pins the wire format
to A2A 0.3. Deployments therefore register their own handler via
`litellm_settings.custom_provider_map` and point `A2A_MODEL_PROVIDER` at it. If
upstream later fixes the built-in route, switch the env var to `a2a` — no new
operator image.

### Access groups

`accessGroups` populates `model_info.access_groups`, the **legacy per-model tag
namespace** that teams grant through `LiteLLMTeam.spec.permission.modelGroups`.
It is *not* the `LiteLLMAccessGroup` object namespace — see
[Access groups](access-group.md#two-access-group-namespaces).

Leave it empty and the Model reconciler's `DEFAULT_ACCESS_GROUP` injection
applies, which is typically granted to every team. That is why `exposeAsModel`
is a block and not a bool: publishing an agent to the whole instance should be
something you typed, not something you got by default.

### Lifecycle

| Action | Effect on the generated model |
|---|---|
| Add `exposeAsModel` | Child created |
| Edit `accessGroups` or the card description | Child updated in place (server-side apply) |
| Remove `exposeAsModel` | Child pruned |
| Delete the agent | Child cascades via owner reference |

The prune path deletes **only** a model it generated — verified by both the
`litellm.ackstorm.ai/generated-by-agent` label and a controller owner reference
with the agent's UID. A hand-written `LiteLLMModel` that happens to occupy the
name `agent.<something>` is left untouched.

The projection runs early in the reconcile, before the LiteLLM connection gate,
so it does not depend on the proxy being reachable. Drift on the child (someone
edits it by hand) is corrected on the next reconcile or safety re-list tick,
whichever comes first.

## Status: what to read

```bash
kubectl get a2a vendor-research-agent -o jsonpath='{.status.lastRendered}{"\n"}'
# {"agentCardKeys":["description","name","version"],"agentID":"<uuid>","at":"...","hash":"abc...","paramsKeys":["timeout"]}

kubectl get a2a vendor-research-agent -o jsonpath='{.status.conditions[?(@.type=="Ready")]}{"\n"}'
# {"type":"Ready","status":"True","reason":"Synced"}
```

`Ready=False` reasons:

- `LiteLLMUnavailable` — `LiteLLMConnection/default` not Ready.
- `LiteLLMRejected` — 4xx from LiteLLM (e.g. malformed agent card).
- `SecretNotFound` — Secret/placeholder unbound.
- `InvalidConfig` — duplicate `spec.secrets[].as`, or invalid JSON in
  either bag.

## See also

- [Example on GitHub](https://github.com/ackstorm/alitellm-operator/tree/main/examples/example-deploy/06-a2aagent.yaml)
- [API Reference: LiteLLMA2AAgent](../api-reference/litellm.ackstorm.ai.md#litellma2aagent)
