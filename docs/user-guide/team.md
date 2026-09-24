# LiteLLMTeam

Declarative team in LiteLLM. A team is a **container**: it owns the LiteLLM
team alias, optional budget and RPM/TPM rate limits, and the list of
[access groups](access-group.md) it attaches. It grants nothing by itself —
everything a team can reach is defined by its access groups. Projected via
`POST /team/{new,update}`.

`metadata.name` IS the LiteLLM `team_alias` — bare name, no prefix, no
overlay indirection. Two-level naming is intentionally NOT supported in
v1alpha1.

The operator does NOT manage team membership (delegated to external identity).

## Quick reference

| Field                | Required | Notes                                                                                       |
|----------------------|----------|---------------------------------------------------------------------------------------------|
| `metadata.name`      | yes      | Used verbatim as LiteLLM `team_alias`. Bare — no `team-` prefix.                            |
| `spec.budget.limit`  | no       | USD cap → `max_budget` (float). Pointer: `0.0` projects, `omit` clears.                     |
| `spec.budget.period` | no       | Reset interval → `budget_duration`. Regex `^[0-9]+[smhd]$` (e.g. `30d`, `12h`, `60s`).      |
| `spec.rateLimits.rpm`| no       | Requests/min → `rpm_limit` (int). Pointer: `0` projects, omit clears. `Minimum=0`.          |
| `spec.rateLimits.tpm`| no       | Tokens/min → `tpm_limit`. Same pointer semantics as `rpm`.                                  |
| `spec.accessGroups`  | no       | `LiteLLMAccessGroup` names, resolved to ids → `access_group_ids`. Empty → reaches nothing.  |
| `spec.deletionPolicy`| no       | `Orphan` (default) keeps the LiteLLM team on CR delete; `Delete` removes it.                |

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
spec:
  accessGroups: [finance]   # a LiteLLMAccessGroup named finance
```

Result: LiteLLM team `team_alias=finance`, no budget, no rate limits,
attached to the `finance` access group.
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

## Access: a closed team opened by access groups

The operator always sends the same **closed baseline** on every create and
update, whatever the spec says:

| LiteLLM field                            | Value                                        |
|------------------------------------------|----------------------------------------------|
| `models`                                 | `["no-default-models"]`                      |
| `object_permission.agents`               | `["00000000-0000-0000-0000-000000000000"]`   |
| `object_permission.mcp_servers`          | `[]`                                         |
| `object_permission.mcp_access_groups`    | `[]`                                         |
| `object_permission.agent_access_groups`  | `[]`                                         |
| `object_permission.mcp_toolsets`         | `[]`                                         |
| `access_group_ids`                       | ids of `spec.accessGroups` (`[]` when empty) |

Why the sentinels: LiteLLM reads an **empty** `models` or `agents` list as
"no filter", so a team with `models: []` sees every model on the proxy.
`no-default-models` is LiteLLM's own sentinel — it is dropped from every
model catalog, so a closed team lists zero models instead of a phantom row.
Every field is sent every time (never omitted) because `POST /team/update`
keeps an omitted field's old value.

Access groups only **add**: an attached group overrides the sentinels for
what it grants. A team without `accessGroups` reaches nothing.

```yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMAccessGroup
metadata:
  name: dream
spec:
  modelGroups: [default, openai, anthropic]
  mcpServerGroups: [default, gitlab]
  agentGroups: [agents]
---
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMTeam
metadata:
  name: dream
spec:
  accessGroups: [dream]
```

A name that is not registered yet parks the team `Ready=False,
reason=AccessGroupNotFound` and requeues — create the `LiteLLMAccessGroup`
and the team heals itself.

MCP **toolsets** cannot be granted through a team: LiteLLM access groups
have no toolset field (verified on 1.102.0), and the team always sends
`mcp_toolsets: []`. See [MCP toolset](mcp-toolset.md).

### Migrating from `spec.permission` (≤ v0.8.x)

`spec.permission`, `spec.params` and `spec.secrets` were removed in v0.9.0.
Move each team's grant into an access group and attach it:

```yaml
# before
kind: LiteLLMTeam
metadata: { name: dream }
spec:
  permission:
    modelGroups: [default, openai]
    mcpGroups: [default]
# after
---
kind: LiteLLMAccessGroup
metadata: { name: dream }
spec:
  modelGroups: [default, openai]
  mcpServerGroups: [default]
---
kind: LiteLLMTeam
metadata: { name: dream }
spec:
  accessGroups: [dream]
```

| `spec.permission` field | `LiteLLMAccessGroup` field |
|-------------------------|----------------------------|
| `models`                | `models`                   |
| `modelGroups`           | `modelGroups`              |
| `mcpServers`            | `mcpServers`               |
| `mcpGroups`             | `mcpServerGroups`          |
| `agents`                | `agents`                   |
| `agentGroups`           | `agentGroups`              |
| `accessGroups`          | → `LiteLLMTeam.spec.accessGroups` |
| `mcpToolsets`           | none (not grantable)       |

## Implicit defaults: `Team/default` + access group `default`

With no CRs declared, the operator keeps two **empty, unlinked** objects:

- LiteLLM team `default` — closed baseline, **no** access groups.
- Unified access group `default` — grants nothing.

Declare the CRs to link and fill them:

```yaml
kind: LiteLLMTeam
metadata: { name: default }
spec:
  accessGroups: [default]
---
kind: LiteLLMAccessGroup
metadata: { name: default }
spec:
  models: [ackstorm.smart, ackstorm.fast]
```

A declared CR always wins over the implicit state. Deleting it falls back to
the empty state — the LiteLLM row (and its id) is **kept**, never deleted:
`POST /team/delete` is suppressed for `Team/default`, and the `default` group
is emptied rather than removed. Anything granted to `default` by hand in the
LiteLLM UI, with no CR declaring it, is cleared by the operator.

## Status: what to read

```bash
kubectl get team finance -o jsonpath='{.status.lastRendered}{"\n"}'
# {"at":"2026-05-24T...","hash":"abc...","teamID":"finance"}
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
- `AccessGroupNotFound` — a `spec.accessGroups` name is not registered in
  LiteLLM yet. Requeued; heals once the `LiteLLMAccessGroup` exists.

## See also

- [Example on GitHub](https://github.com/ackstorm/alitellm-operator/tree/main/examples/example-deploy/07-team.yaml)
- [API Reference: LiteLLMTeam](../api-reference/litellm.ackstorm.ai.md#litellmteam)
