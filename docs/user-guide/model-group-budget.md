# LiteLLMModelGroupBudget

A **model-group budget** is a USD spend cap on a model **access-group tag**
(`model_info.access_groups`) — one budget shared by every caller granted that
tag. It is projected via `GET/PUT/DELETE /access_group/<tag>/budget`.

Requires LiteLLM **1.100.0 or newer**.

It is not a [LiteLLMAccessGroup](access-group.md): an access group is a bundle
of grants; a budget is a cap attached to one *tag*. The tag exists in LiteLLM
only while at least one model carries it.

## Example

```yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMModelGroupBudget
metadata:
  name: anthropic-budget
spec:
  modelGroup: anthropic   # immutable
  limit: 500              # USD hard cap
  softLimit: 400          # USD alert threshold
  period: 30d
```

## Fields

| Field                 | Required | Notes                                                                 |
|-----------------------|----------|-----------------------------------------------------------------------|
| `spec.modelGroup`     | yes      | The tag. Immutable.                                                   |
| `spec.limit`          | no       | USD hard cap (`max_budget`). `0` means **no cap** in LiteLLM.         |
| `spec.softLimit`      | no       | USD alert threshold (`soft_budget`). Requests still succeed.          |
| `spec.period`         | no       | Reset interval (`budget_duration`), e.g. `30d`, `12h`.                |
| `spec.deletionPolicy` | no       | `Orphan` (default) or `Delete`.                                       |

## Who is charged

LiteLLM charges a tag budget only when the tag **literally appears** in the
calling key's or team's `models`. A grant that reaches the model through
`access_group_ids` alone is not charged. The operator therefore mirrors the
tags of every attached `LiteLLMAccessGroup` (`models` ∪ `modelGroups`, sorted,
wildcards never) into `team.models` after the `no-default-models` sentinel — see
[team](team.md). Keys that are not team-scoped must list the tag themselves.

When the cap is hit LiteLLM answers 422
`Budget has been exceeded! Model access group=<tag>`.

## Conditions

| `Ready` reason        | Meaning                                                              |
|-----------------------|----------------------------------------------------------------------|
| `Synced`              | Budget stored in LiteLLM.                                            |
| `LiteLLMUnavailable`  | Connection not usable; retried.                                      |
| `LiteLLMRejected`     | LiteLLM refused the request (4xx).                                   |
| `ModelGroupNotFound`  | No model carries the tag (LiteLLM 404). Heals when one does.         |
| `DuplicateModelGroup` | Another CR already owns this tag.                                    |

## Limits

- **One owner per tag.** A duplicate CR parks `DuplicateModelGroup`; it only
  takes over at the next safety-relist tick after the owner is deleted.
- A hand-managed LiteLLM access group (no `LiteLLMAccessGroup` CR) is picked up
  by attached teams only at the next safety relist.
- If an attached `LiteLLMAccessGroup` row disappears from LiteLLM, the team is
  narrowed to the groups that still resolve (the vanished group's names leave
  `team.models`), then parks `AccessGroupNotFound` until the group exists again.
- LiteLLM cannot clear a single budget field via `PUT`; removing a field from
  the spec does not unset it. Deleting the CR removes the whole budget.
