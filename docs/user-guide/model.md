# LiteLLMModel

Declarative model entry in LiteLLM. Owns one `(litellm_params,
model_info)` pair, projected via `POST /model/{new,update}` against
the `LiteLLMConnection/default` instance.

Shape is flat: `spec.params` (→ `litellm_params`) and `spec.info`
(→ `model_info`) are both free-form pass-through bags. No `spec.type`
discriminator. User-authored and Discovery-generated CRs are
reconciled identically.

## Quick reference

| Field             | Required | Notes                                                                          |
|-------------------|----------|--------------------------------------------------------------------------------|
| `metadata.name`   | yes      | LiteLLM `model_name` (the alias clients call). Unique per Connection.          |
| `spec.params`     | yes      | `litellm_params` bag. Must at minimum carry a `model:` key.                    |
| `spec.params.model` | yes    | `<provider>/<id>` (e.g. `anthropic/claude-sonnet-4.5`, `gemini/gemini-2.5-flash`). |
| `spec.params.api_key` | varies | Usually `{{NAME}}` resolved from `spec.secrets[]`.                          |
| `spec.params.api_base` | no  | Override upstream base URL.                                                    |
| `spec.info`       | no       | `model_info` bag — `description`, `mode`, custom metadata.                     |
| `spec.secrets[]`  | no       | `{as, secretRef: {name, key}}` for `{{NAME}}` substitution in `params`/`info`. |

After `kubectl apply`, expect:

- `status.conditions[type=Ready].status=True` / `reason=Synced`.
- `status.lastRendered.modelID` pinned with the LiteLLM-assigned UUID
  (`model_info.id`). Pinned to skip a `GET /model/info` on every
  reconcile.
- `status.lastRendered.hash` = SHA-256 of the rendered body.

## Minimal example

```yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMModel
metadata:
  name: claude-sonnet-4-5
  namespace: default
spec:
  params:
    model: "anthropic/claude-sonnet-4.5"
    api_key: "{{ANTHROPIC_API_KEY}}"
    api_base: "https://api.anthropic.com"
  info:
    description: "Anthropic Sonnet 4.5"
  secrets:
    - as: ANTHROPIC_API_KEY
      secretRef:
        name: anthropic-api-key
        key:  ANTHROPIC_API_KEY
```

After apply, callers reach the model with:

```bash
curl $LITELLM/chat/completions \
  -H "Authorization: Bearer $KEY" \
  -d '{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}'
```

(`model` field in the request is `metadata.name`, not the upstream
provider id.)

## Secret substitution

`spec.secrets[].as` declares the placeholder name; `{{NAME}}` inside
ANY string leaf in `spec.params` or `spec.info` is replaced with the
plaintext value at reconcile time. Constraints:

- `as` must match `^[A-Z_][A-Z0-9_]*$` (admission-time CEL).
- `as` values unique within the CR (runtime check, `Ready=False`
  `reason=DuplicateSecretAs` on violation).
- Secret must live in the SAME namespace as the Model CR. No cross-NS
  resolution.
- Secret material NEVER appears in logs, Events, or
  `status.conditions[].message`.

Secret rotation propagates within ~30s (Secret-watch path on the
controller).

Missing Secret / key → `Ready=False`, `reason=SecretNotFound`.
Placeholder without binding → `Ready=False`,
`reason=UnresolvedPlaceholder`.

## Common provider shapes

**OpenAI / Anthropic / xAI** (HTTP API, key-based):

```yaml
spec:
  params:
    model:   "openai/gpt-4o"
    api_key: "{{OPENAI_API_KEY}}"
```

**Google Gemini**:

```yaml
spec:
  params:
    model:   "gemini/gemini-2.5-flash"
    api_key: "{{GEMINI_API_KEY}}"
```

**AWS Bedrock** (no `api_key`, uses AWS creds):

```yaml
spec:
  params:
    model:                 "bedrock/anthropic.claude-3-5-sonnet-20241022-v2:0"
    aws_access_key_id:     "{{AWS_ACCESS_KEY_ID}}"
    aws_secret_access_key: "{{AWS_SECRET_ACCESS_KEY}}"
    aws_region_name:       "us-east-1"
  secrets:
    - { as: AWS_ACCESS_KEY_ID,     secretRef: { name: aws-creds, key: AWS_ACCESS_KEY_ID } }
    - { as: AWS_SECRET_ACCESS_KEY, secretRef: { name: aws-creds, key: AWS_SECRET_ACCESS_KEY } }
```

**Ollama / local OpenAI-compatible** (no key, custom base):

```yaml
spec:
  params:
    model:    "openai/llama3.3"
    api_base: "http://ollama.default.svc:11434/v1"
    api_key:  "ollama"
```

## Drift detection + correction

On every reconcile the rendered post-substitution body is hashed
(SHA-256, RFC 8785 canonical JSON, excluding `model_info.id`). The
hash is compared against `status.lastRendered.hash`:

- Match → no LiteLLM call (steady state).
- Mismatch → `POST /model/update`, increments
  `alitellm_operator_drift_corrected_total{domain=model,action=update_drifted}`.
- Vanish (row missing in LiteLLM) → `POST /model/new`, re-pin
  `modelID`, increment
  `alitellm_operator_drift_corrected_total{domain=model,action=create_missing}`.

`spec.params` shrinkage (a key removed) triggers delete-and-recreate;
`modelID` is re-pinned in the same reconcile.

ANY `spec.info` (`model_info`) change — value edit, key addition, OR key
removal — also triggers delete-and-recreate, NOT a bare `POST /model/update`.
LiteLLM's `POST /model/update` rewrites `litellm_params` + `updated_by` only
and never persists the `model_info` blob; only `POST /model/new` does. The
operator tracks `status.lastRendered.infoHash` (SHA-256 of the rendered
`model_info`, excluding the `id` overlay) to detect these changes. An empty
stored `infoHash` (pre-upgrade status) is backfilled silently — a model whose
blob is already stale in LiteLLM needs a one-time manual recreate (delete the
LiteLLM entry; the operator recreates it fresh via `POST /model/new`).

### Duplicate deployment rows (id-drift churn)

LiteLLM allows multiple deployment rows per `model_name` — `POST /model/new`
is **not** idempotent. The safety re-list (existence probe, Step 7b) resolves
"does our row still exist?" by checking whether the tracked `modelID` is
**present in the full set** of rows LiteLLM lists under the name — an
order-independent membership test — rather than reading the first row only.

- Tracked id present → no drift, no mutation (even if duplicates exist).
- Tracked id absent but another row exists → adopt it (`POST /model/update`
  re-target), NOT a new create.
- All rows gone → recreate (`create_missing`).

When more than one row exists for a name, the operator prunes the extras on the
relist tick — deleting every row except the tracked id — and increments
`alitellm_operator_drift_corrected_total{domain=model,action=duplicate_pruned}` per deleted row.
The prune is best-effort: a failed delete is retried on the next relist and
never blocks the reconcile. Router models (`litellm_params.model` starting
`auto_router/`) are exempt — they live in LiteLLM's in-memory router, are
invisible to `GET /model/info`, and are never probed or pruned.

Historical context: resolving by the first row (pre-fix) read any duplicate
whose id differed from the tracked id as "id drift", clearing the tracked id →
CREATE → another duplicate → self-amplifying churn (`claude-opus-4-7`
accumulated 1718 rows over 9 days). To audit for residual duplicates:

```sql
SELECT model_name, count(*) FROM "LiteLLM_ProxyModelTable"
GROUP BY 1 HAVING count(*) > 1;
```

## Default access group

If `spec.info.access_groups` is absent or empty, the operator injects the
`DEFAULT_ACCESS_GROUP` value (default `default`; set the env var to `""` to
disable) so every model belongs to at least one LiteLLM access group. This
covers standalone models and Discovery-generated children alike. An explicit
non-empty `access_groups` is never overridden.

## `spec.info.id` is operator-controlled

Setting `spec.info.id` is silently overridden by the operator's
overlay (the pinned `modelID`) and emits a Warning Event
`reason=ProjectionOverride`. Do not set it.

## Status: what to read

```bash
kubectl get mdl claude-sonnet-4-5 -o jsonpath='{.status.lastRendered}{"\n"}'
# {"at":"...","hash":"abc...","infoHash":"def...","infoKeys":["access_groups","description"],"modelID":"<uuid>","paramsKeys":["api_base","api_key","model"]}

kubectl get mdl claude-sonnet-4-5 -o jsonpath='{.status.conditions[?(@.type=="Ready")]}{"\n"}'
# {"type":"Ready","status":"True","reason":"Synced"}
```

`Ready=False` reasons beyond the secret-related ones above:

- `LiteLLMUnavailable` — `LiteLLMConnection/default` not Ready.
- `LiteLLMRejected` — LiteLLM returned 4xx/5xx on mutation. Inspect
  `message` for the upstream payload. A common first-install case is a
  500 with `Set 'STORE_MODEL_IN_DB='True'' in your env to enable this
  feature`: the proxy needs `STORE_MODEL_IN_DB=True` (env) or
  `general_settings.store_model_in_db: true` (config) before it accepts
  `POST /model/{new,update}`. This is upstream LiteLLM config, not an
  operator setting — see the [quickstart prerequisites](../getting-started/quickstart.md#prerequisites).

## See also

- [Example on GitHub](https://github.com/ackstorm/alitellm-operator/tree/main/examples/example-deploy/02-model.yaml)
- [LiteLLMModelDiscovery](model-discovery.md) — auto-generate Model CRs from a Connection's `/models`.
- [API Reference: LiteLLMModel](../api-reference/litellm.ackstorm.ai.md#litellmmodel)
