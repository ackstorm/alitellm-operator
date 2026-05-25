# LiteLLMTeam

Declarative team in LiteLLM. Owns the LiteLLM team alias plus optional
budget and RPM/TPM rate limits, projected via
`POST /team/{new,update}`.

`metadata.name` IS the LiteLLM `team_alias` — bare name, no prefix, no
overlay indirection. Two-level naming is intentionally NOT supported in
v1alpha1.

The operator does NOT manage team membership, models allow-list, or
object permissions. Those are delegated to an external identity / user-
management system; see [Out of Scope](../index.md) in the home page.

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
- `status.lastRendered.teamID` pinned with the LiteLLM-assigned UUID
  (used directly by the finalizer DELETE path — no re-resolve by alias).
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

## Reserved overlay keys — `ProjectionOverride` warning

The operator stamps 7 structural keys onto the request body and ALWAYS
wins over `spec.params`:

```
team_alias, max_budget, budget_duration, rpm_limit, tpm_limit,
rpm_limit_type, tpm_limit_type
```

Setting any of these inside `spec.params` is silently overridden and
emits a `reason=ProjectionOverride` Warning Event per colliding key.
Use the typed `spec.budget` / `spec.rateLimits` sub-blocks instead.

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
# {"at":"2026-05-24T...","hash":"abc...","paramsKeys":["metadata"],"teamID":"t-uuid"}

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
