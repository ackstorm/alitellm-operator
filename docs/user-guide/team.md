# LiteLLMTeam

Declarative team in LiteLLM. Owns the LiteLLM team alias plus optional
budget and RPM/TPM rate limits, projected via
`POST /team/{new,update}`.

`metadata.name` IS the LiteLLM `team_alias` — bare name, no prefix, no
overlay indirection. Two-level naming is intentionally NOT supported in
v1alpha1.

The operator does NOT manage team membership (delegated to external identity).
Models allow-list and object permissions ARE managed via the typed
`spec.permission` block (see below); when that block is absent they remain
raw `spec.params` passthrough.

## Quick reference

| Field                | Required | Notes                                                                                       |
|----------------------|----------|---------------------------------------------------------------------------------------------|
| `metadata.name`      | yes      | Used verbatim as LiteLLM `team_alias`. Bare — no `team-` prefix.                            |
| `spec.budget.limit`  | no       | USD cap → `max_budget` (float). Pointer: `0.0` projects, `omit` clears.                     |
| `spec.budget.period` | no       | Reset interval → `budget_duration`. Regex `^[0-9]+[smhd]$` (e.g. `30d`, `12h`, `60s`).      |
| `spec.rateLimits.rpm`| no       | Requests/min → `rpm_limit` (int). Pointer: `0` projects, omit clears. `Minimum=0`.          |
| `spec.rateLimits.tpm`| no       | Tokens/min → `tpm_limit`. Same pointer semantics as `rpm`.                                  |
| `spec.params`        | no       | Free-form bag, merged into `NewTeamRequest` top-level. `{{NAME}}` substitution in strings.  |
| `spec.secrets[]`     | no       | Substitution map for `spec.params` placeholders. `{as, secretRef: {name, key}}`.            |

After `kubectl apply`, expect:

- `status.conditions[type=Ready].status=True` / `reason=Synced`.
- `status.lastRendered.teamID` pinned with the team's `team_id`. For teams
  the operator creates new, this equals `metadata.name` (see
  [team_id assignment](#team_id-assignment)); teams that already existed
  under a server-assigned UUID keep that UUID. Used directly by the
  finalizer DELETE path — no re-resolve by alias.
- `status.lastRendered.hash` = SHA-256 of the rendered merged body.

## Minimal example

```yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMTeam
metadata:
  name: finance
  namespace: default
spec: {}
```

Result: LiteLLM team `team_alias=finance`, no budget, no rate limits.
Both `max_budget` / `budget_duration` are sent as `null`; both
`rpm_limit` / `tpm_limit` as `null`; the `*_limit_type` keys are
omitted.

## With budget and rate limits

```yaml
spec:
  budget:
    limit: 500.0
    period: "30d"
  rateLimits:
    rpm: 6000
    tpm: 1000000
```

Projection on the wire (`POST /team/update` body, abbreviated):

```json
{
  "team_alias": "finance",
  "max_budget": 500.0,
  "budget_duration": "30d",
  "rpm_limit": 6000,
  "rpm_limit_type": "best_effort_throughput",
  "tpm_limit": 1000000,
  "tpm_limit_type": "best_effort_throughput"
}
```

The `*_limit_type` keys are operator-controlled (always
`best_effort_throughput`) and not exposed as CR fields.

## Clearing budget / rate limits

Remove the sub-block from the spec — `apply` again:

```yaml
spec:
  budget: null
  rateLimits: null
```

Wire body emits explicit nulls (`max_budget: null`, `budget_duration:
null`, `rpm_limit: null`, `tpm_limit: null`). Per spec §6.7, LiteLLM
treats `null` as "no limit set" via wholesale-replace on
`POST /team/update`.

## team_id assignment

When the operator **creates a new** LiteLLM team, it sets
`team_id == metadata.name` (human-readable, structurally collision-free)
instead of letting LiteLLM mint a random UUID. The chosen `team_id` is
persisted in `status.lastRendered.teamID` and surfaces verbatim in the
LiteLLM UI / API.

The CR's `metadata.name` already projects to `team_alias`, so after an
operator restart (which loses CR status) the team is re-adopted by alias
lookup (`GET /v2/team/list?team_alias=`) — it reaches the **UPDATE** arm
and is never recreated.

This change is **CREATE-only**:

- **New teams** (no existing team under the alias) → `team_id =
  metadata.name`.
- **Existing teams** (already created, found by alias — including the
  pre-change teams that hold a server-assigned UUID) → take the UPDATE
  arm and **keep their original UUID**. The operator never rewrites an
  existing team's identity.

Caveats:

- **No automatic migration.** Existing UUID teams are not migrated to a
  name-id. Migrating one manually means delete + recreate, which orphans
  the team's virtual keys (they foreign-key on `team_id`). Out of scope.
- **Cross-namespace name collision is unhandled by design.** `team_id`
  is `metadata.name` verbatim, with no namespace prefix (single-namespace
  deployment assumed). Two `LiteLLMTeam` CRs sharing a `metadata.name`
  across namespaces would collide on the same `team_id`. v1alpha1 does
  not guard against this.

## Reserved overlay keys — `ProjectionOverride` warning

The operator stamps these structural keys onto the request body and
ALWAYS wins over `spec.params`:

```
team_alias, max_budget, budget_duration, rpm_limit, tpm_limit,
rpm_limit_type, tpm_limit_type
```

`team_id` is ALSO operator-controlled per the
[team_id assignment](#team_id-assignment) rules (CREATE arm → name,
UPDATE arm → existing id); setting it in `spec.params` has no effect.
Unlike the seven keys above, a `spec.params.team_id` is overwritten
SILENTLY — it does NOT emit a `ProjectionOverride` Warning Event.

Setting any of these inside `spec.params` is silently overridden and
emits a `reason=ProjectionOverride` Warning Event per colliding key.
Use the typed `spec.budget` / `spec.rateLimits` sub-blocks instead.

## Resource permissions — `spec.permission`

`spec.permission` is a typed, operator-MANAGED block that controls which
models, MCP servers, and A2A agents a team may use. When present, the
operator OWNS the projected LiteLLM `models` and `object_permission` fields —
out-of-band UI edits to them do NOT survive reconciliation.

```yaml
spec:
  permission:
    models:      ["gpt-4o"]        # specific model names
    modelGroups: ["anthropic"]     # model access-group names (merged into models)
    mcpServers:  ["hindsight"]     # specific MCP server names/aliases
    mcpGroups:   ["team-a"]        # MCP access-group names
    agents:      ["planner"]       # A2A agent NAMES (resolved to UUIDs by the operator)
    agentGroups: ["grp-a"]         # A2A agent access-group names (see no-op note)
```

Projection to LiteLLM:

| CR field       | LiteLLM target                              |
|----------------|---------------------------------------------|
| `models` + `modelGroups` | top-level `team.models` (one merged list) |
| `mcpServers`   | `object_permission.mcp_servers`             |
| `mcpGroups`    | `object_permission.mcp_access_groups`       |
| `agents`       | `object_permission.agents` (name→UUID resolved) |
| `agentGroups`  | `object_permission.agent_access_groups` (**no-op**, see below) |

**Agent name resolution.** LiteLLM enforces `object_permission.agents` on
agent `agent_id` UUIDs and silently ignores names. The operator resolves each
name via `GET /v1/agents`. If a referenced agent is not yet registered (the
`LiteLLMA2AAgent` CR has not reconciled), the team is parked
`Ready=False, reason=AgentNotFound` (listing the missing names) and requeued —
it recovers automatically once the agent appears.

**`agentGroups` is a no-op.** LiteLLM 1.83.10 has no API to tag an agent into
an access group, so `object_permission.agent_access_groups` is never enforced.
The field is retained for forward-compat; the operator emits a Warning
`AgentGroupsNoOp` Event when it is non-empty.

**Empty vs absent.** An absent `spec.permission` block leaves any raw
`spec.params.models` / `spec.params.object_permission` untouched (passthrough).
A **present** block makes the operator OWN `models` and all four
`object_permission` sub-fields: every one is sent to LiteLLM on each update,
and a sublist you leave empty is sent as an explicit `[]` (a clear), never
omitted. Shrinking a list — including down to empty (`mcpServers: []`, or
removing the last agent) — therefore **does** revoke access. This is
security-critical: LiteLLM's `POST /team/update` merges per-field on the
persistent `object_permission` row, so an *omitted* field keeps its stale
value; the operator always emits `[]` so a revocation is never silently lost.

**Precedence.** With `spec.permission` present, any `models` or
`object_permission` key inside `spec.params` is dropped and a
`ProjectionOverride` Warning Event fires — the typed block always wins.

### Migration from `spec.params.object_permission`

Teams currently using `spec.params.object_permission` (or
`spec.params.models`) continue to work unchanged as long as `spec.permission`
is absent. To adopt the typed block, move each value:

```yaml
# before
spec:
  params:
    models: ["gpt-4o"]
    object_permission:
      mcp_servers: ["hindsight"]
# after
spec:
  permission:
    models:     ["gpt-4o"]
    mcpServers: ["hindsight"]
```

Note that `object_permission.agents` in the old form required raw UUIDs; the
new `spec.permission.agents` takes human-friendly NAMES instead.

## `Team/default` carve-out

`metadata.name=default` is reserved. The operator:

- Bootstraps the LiteLLM `team_alias=default` on manager start (after
  `LiteLLMConnection/default` reaches `Ready=True`) with empty spec —
  no Kubernetes CR is created.
- Reconciles a user-authored `Team/default` CR normally (ownership
  transition — re-uses the LiteLLM team, does not recreate).
- Suppresses `POST /team/delete` when the CR is deleted: the implicit
  empty spec is re-applied and the finalizer is removed. The default
  team is never destroyed in LiteLLM.

## `spec.params` pass-through + `tags` (Enterprise gate)

`spec.params` is forwarded verbatim to the top-level
`NewTeamRequest` body. Common pitfall:

```yaml
spec:
  params:
    tags: [prod, finance]   # HTTP 403 on OSS LiteLLM
```

LiteLLM 1.83.10 OSS rejects `tags` on `/team/new` as Enterprise-only.
Holders of a LiteLLM Enterprise license can keep `tags` in `params`
unchanged — the operator pass-through forwards it without code change.

For free-form metadata that the operator does NOT touch, use the
LiteLLM `metadata` map inside `params`:

```yaml
spec:
  params:
    metadata:
      cost-center: "1234"
      env: prod
```

## Status: what to read

```bash
kubectl get team finance -o jsonpath='{.status.lastRendered}{"\n"}'
# {"at":"2026-05-24T...","hash":"abc...","paramsKeys":["metadata"],"teamID":"finance"}
# (teamID == metadata.name for operator-created teams; a UUID for teams
#  that already existed under a server-assigned id — see team_id assignment)

kubectl get team finance -o jsonpath='{.status.conditions[?(@.type=="Ready")]}{"\n"}'
# {"type":"Ready","status":"True","reason":"Synced","message":""}
```

Other `Ready=False` reasons:

- `LiteLLMUnavailable` — the cached `LiteLLMConnection/default` is not
  `Ready=True`. Reconciler retries on the connection's next status
  transition.
- `LiteLLMRejected` — LiteLLM returned a 4xx (non-401). Inspect
  `message` for the upstream error.
- `SecretNotFound` — a `spec.secrets[].secretRef` is missing OR a
  `{{NAME}}` placeholder in `spec.params` has no matching `as` binding.

## See also

- [Example on GitHub](https://github.com/ackstorm/alitellm-operator/tree/main/examples/example-deploy/07-team.yaml)
- [API Reference: LiteLLMTeam](../api-reference/litellm.ackstorm.ai.md#litellmteam)
