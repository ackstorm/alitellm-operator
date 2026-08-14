# LiteLLMAccessGroup

An **access group** is a reusable bundle of models, MCP servers, and A2A agents
that teams attach by name. Define the bundle once, attach it to as many teams as
you like, and change the bundle in one place. Projected via
`POST /v1/access_group` / `PUT /v1/access_group/<id>` /
`DELETE /v1/access_group/<id>`.

Requires LiteLLM **1.93.0 or newer** — the `/v1/access_group` endpoints do not
exist before that.

!!! danger "Access groups only ADD — they never restrict"

    Attaching a group **widens** a team's ceiling and **overrides** the
    operator's deny-by-default sentinel. See
    [Access groups only ADD](#access-groups-only-add) before you use one.

## Quick reference

| Field                  | Required | Notes                                                                     |
|------------------------|----------|---------------------------------------------------------------------------|
| `metadata.name`        | yes      | Used verbatim as LiteLLM `access_group_name`. Unique server-side.         |
| `spec.description`     | no       | Free text, forwarded verbatim.                                            |
| `spec.models[]`        | no       | Model **names**, forwarded as-is. No resolution, no validation.           |
| `spec.mcpServers[]`    | no       | MCP server **names**, resolved to `server_id` UUIDs.                      |
| `spec.agents[]`        | no       | A2A agent **names**, resolved to `agent_id` UUIDs.                        |
| `spec.deletionPolicy`  | no       | `Orphan` (default) or `Delete`.                                           |

After `kubectl apply`, expect:

- `status.conditions[type=Ready].status=True` / `reason=Synced`.
- `status.lastRendered.accessGroupID` — the LiteLLM-assigned UUID, used by the
  finalizer and by every team that attaches this group.
- `status.lastRendered.hash` = SHA-256 of the rendered body.

## Minimal example

```yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMAccessGroup
metadata:
  name: shared-tooling
  namespace: default
spec:
  description: Models and tools shared across product teams
  models:
    - gpt-4o
    - claude-sonnet-4
  mcpServers:
    - hindsight
  agents: []
```

## The three dimensions

| CR field      | LiteLLM field           | Resolved?                                    |
|---------------|-------------------------|----------------------------------------------|
| `models`      | `access_model_names`    | **No** — LiteLLM matches on `model_name`.    |
| `mcpServers`  | `access_mcp_server_ids` | **Yes** — name → `server_id` via `GET /v1/mcp/server`. |
| `agents`      | `access_agent_ids`      | **Yes** — name → `agent_id` via `GET /v1/agents`.      |

`models` needs no resolution because LiteLLM keys model access on the
human-readable `model_name`, so a name written here works whether or not a
`LiteLLMModel` CR manages it.

The other two are matched on UUIDs and LiteLLM silently ignores a name, so the
operator resolves them. A name that does not resolve parks the CR
`Ready=False, reason=MCPServerNotFound` (or `AgentNotFound`) and lists the
missing names in the condition message. This is an ordering dependency with the
`LiteLLMMCPServer` / `LiteLLMA2AAgent` CRs — it self-heals once they exist, on
the next safety-relist tick.

**No validation of `models`.** LiteLLM does not check that a listed model
exists, and neither does the operator. A typo yields an inert grant, not an
error — the same deliberate non-validation as
[`LiteLLMMCPToolset`](mcp-toolset.md). Read back `GET /v1/access_group` to
diagnose an empty-looking grant.

## Naming and ids

`metadata.name` **is** the LiteLLM `access_group_name`, and it is unique
server-side: a duplicate create answers `409`.

`access_group_id` is **server-minted**. LiteLLM 1.93.0 ignores a
caller-supplied one, so the operator reads the id back from the create response
and stores it in `status.lastRendered.accessGroupID` — the same posture as
`LiteLLMMCPToolset`'s `toolset_id` and `LiteLLMA2AAgent`'s `agent_id`, and
unlike `LiteLLMTeam`'s `team_id` / `LiteLLMMCPServer`'s `server_id`, which the
operator pins to `metadata.name`.

Because the id cannot be derived from the name, **adoption is by name**: on an
operator restart (or when a group with that name was created out of band), the
`409` is caught, the group is looked up by name, and the operator's rendered
state is pushed onto it with a `PUT`. Nothing is orphaned and no duplicate is
created.

## Attaching a group to a team

A group grants nobody anything on its own. Reference it from a team via
[`spec.permission.accessGroups`](team.md#accessgroups), which takes group
**names**:

```yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMTeam
metadata:
  name: research-team
spec:
  permission:
    models: ["gpt-4o"]
    accessGroups: ["shared-tooling"]
```

The operator resolves each name to its `access_group_id` and writes the list to
the team's **top-level** `access_group_ids` (not to `object_permission`). An
unresolved name parks the team `Ready=False, reason=AccessGroupNotFound` and
requeues — create the `LiteLLMAccessGroup` first and the team self-heals.

**Verify an attachment from the team side only.** Read
`GET /team/info?team_id=<id>` and look at `team_info.access_group_ids`:

```bash
curl -H "Authorization: Bearer $MASTER_KEY" \
  "$LITELLM/team/info?team_id=<team_id>" | jq '.team_info.access_group_ids'
```

The group side does **not** mirror it: after a team-side write,
`access_group.assigned_team_ids` keeps reading `[]` (measured on 1.93.0).
That non-propagation is why the operator writes only the team mirror — the team
field is the one LiteLLM actually enforces on, and keeping a single writer per
surface avoids a repair problem LiteLLM makes unsolvable: it rewrites the mirror
only on an ENTER/LEAVE delta, so an idempotent re-`PUT` cannot fix a mirror that
is already broken.

## Access groups only ADD

!!! danger "A group grant overrides the team's deny-by-default sentinel"

    A `spec.permission` block that leaves `models` empty projects the
    `["__deny_all__"]` sentinel, which denies every model
    ([Deny-by-default](team.md#resource-permissions-specpermission)). An
    attached access group **overrides** that: LiteLLM composes group grants
    additively, so a group granting `gpt-4o` makes `gpt-4o` reachable by that
    team's keys even though `team.models` is still `["__deny_all__"]`.

    Measured on stock LiteLLM 1.93.0 and covered by the `AG-04` e2e spec: the
    same team is denied `team_model_access_denied` before the attachment and
    succeeds after it, with its own `models` list unchanged.

Consequences to plan around:

- **A group is a grant, never a filter.** Adding a group can only widen a team.
  There is no way to use one to narrow access.
- **Editing a group edits every attached team, at once.** Adding a model to
  `spec.models` grants it to every team carrying the group, in one reconcile.
  Treat a widely-attached group as a production change.
- **Revocation goes through the group.** Shrinking `spec.models` — including to
  empty — really does revoke, because the operator always sends the full list
  on every `PUT` (an omitted list would be a *keep*, not a *clear*). Removing
  the group from a team's `accessGroups` revokes for that team alone.
- **The team's own sentinel keeps protecting whatever the group does not
  grant.** The bypass is scoped to the group's contents, not a blanket
  disabling of the filter.

This behaviour is LiteLLM's, not the operator's, and it is documented rather
than worked around: suppressing it would mean the operator second-guessing an
explicit grant the user wrote.

## Two access-group namespaces

"Access group" names two unrelated things in LiteLLM. They are **disjoint** —
a group created in one never appears in the other.

| | **Unified access group** (this CRD) | **Legacy model tag** |
|---|---|---|
| Endpoints | `/v1/access_group` (GET, POST) and `/v1/access_group/<id>` (PUT, DELETE) | `GET /access_group/list`, `POST /access_group/new` |
| Object | A first-class row with an id, holding models + MCP servers + agents | A free-text tag stamped on a single model |
| Written by | `LiteLLMAccessGroup` CRs | `LiteLLMModel.spec.info.access_groups`, and the `DEFAULT_ACCESS_GROUP` env default |
| Attached via | `LiteLLMTeam.spec.permission.accessGroups` → `team.access_group_ids` | `LiteLLMTeam.spec.permission.modelGroups` → merged into `team.models` |
| Covers | Models, MCP servers, A2A agents | Models only |

So:

- `spec.permission.accessGroups` takes **unified** group names — the ones this
  CRD creates.
- `spec.permission.modelGroups` takes **legacy tag** names — the ones
  `model_info.access_groups` / `DEFAULT_ACCESS_GROUP` write. They merge into the
  team's `models` list, which means they are subject to deny-by-default like any
  other model entry; unified groups are not.

A `LiteLLMAccessGroup` named `anthropic` and a model tagged
`model_info.access_groups: ["anthropic"]` are **unrelated**. Listing `anthropic`
under `accessGroups` grants nothing from the tag, and listing it under
`modelGroups` grants nothing from the CR.

## Deletion

`spec.deletionPolicy` behaves as it does on the other kinds:

- `Orphan` (default) — if the LiteLLM-side `DELETE` cannot be confirmed
  (LiteLLM unreachable, `401`, deterministic `4xx`), the finalizer still drains
  and a Warning Event records the un-confirmed delete. The CR never wedges.
- `Delete` — the finalizer is held until the delete is confirmed.

`DELETE /v1/access_group/<id>` answers a clean `404` when the row is already
gone, which the client folds into success — so a confirmed-absent group drains
under either policy. (Contrast `/v1/mcp/toolset`, which raises a `500` on an
absent row; see the [MCP toolset guide](mcp-toolset.md).)

Deleting a group does **not** edit the teams that referenced it; LiteLLM drops
the now-dangling id from enforcement.

## Troubleshooting

| Symptom | Cause |
|---|---|
| `Ready=False reason=MCPServerNotFound` / `AgentNotFound` | A `spec.mcpServers` / `spec.agents` name is not registered yet. Create the CR; it self-heals. |
| Team `Ready=False reason=AccessGroupNotFound` | The team names a group that does not exist yet. Create the `LiteLLMAccessGroup` first. |
| Group is `Synced` but grants nothing | A `spec.models` entry names no live model. Nothing validates it; check `GET /v1/access_group`. |
| Group does not appear in `GET /access_group/list` | Expected — that is the legacy tag namespace. See [Two access-group namespaces](#two-access-group-namespaces). |
| `assigned_team_ids` is `[]` despite an attached team | Expected — the group side does not mirror a team-side write. Read `GET /team/info`. |
| A team can reach a model its `spec.permission.models` excludes | An attached group grants it. Groups only ADD. |
