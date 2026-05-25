# LiteLLMModelDiscovery

Pipeline B CR. Points the operator at ONE upstream provider and
auto-generates `LiteLLMModel` child CRs for each model the provider
publishes. Discovery NEVER calls LiteLLM directly — each generated
child reconciles into LiteLLM via the `LiteLLMModel` controller
(Pipeline A).

## Quick reference

| Field                       | Required        | Notes                                                                                  |
|-----------------------------|-----------------|----------------------------------------------------------------------------------------|
| `spec.type`                 | yes             | Enum: `anthropic`, `bedrock`, `gemini`, `kubeai`, `openai`.                            |
| `spec.prefix`               | no              | DNS-1123 segment prepended to each child's `metadata.name`. Default: lowercased `spec.type`. |
| `spec.credentialsSecretRef` | per-provider    | Secret holding upstream API key (operator-side ONLY — never propagated to children).   |
| `spec.region`               | bedrock only    | AWS region. One region per CR (multi-region → multiple CRs).                           |
| `spec.baseUrl`              | kubeai (req), openai (opt) | Provider HTTP endpoint. `kubeai` value also auto-overlays into each child's `api_base`. |
| `spec.params`               | no              | Pass-through bag propagated VERBATIM into every child's `spec.params`.                 |
| `spec.info`                 | no              | Pass-through bag propagated into every child's `spec.info`.                            |
| `spec.secrets[]`            | no              | Substitution map propagated into every child's `spec.secrets[]` (NOT resolved here).   |
| `spec.filters.include`      | no              | RE2 patterns — anchored, include-FIRST. Empty → admit all.                             |
| `spec.filters.exclude`      | no              | RE2 patterns — applied AFTER include.                                                  |
| `spec.refresh.interval`     | yes             | Cadence between refreshes. CEL floor `1m` enforced at admission.                       |

## Provider field matrix

| Type        | Requires                    | Forbids                       | Secret keys read                                                          |
|-------------|-----------------------------|-------------------------------|---------------------------------------------------------------------------|
| `anthropic` | `credentialsSecretRef`      | `region`, `baseUrl`           | `ANTHROPIC_API_KEY`                                                       |
| `bedrock`   | `region`                    | `baseUrl`                     | `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` (+ optional `AWS_SESSION_TOKEN`) |
| `gemini`    | `credentialsSecretRef`      | `region`, `baseUrl`           | `GEMINI_API_KEY` (or `GOOGLE_API_KEY`)                                    |
| `kubeai`    | `baseUrl`                   | `credentialsSecretRef`, `region` | none                                                                   |
| `openai`    | `credentialsSecretRef`      | `region`                      | `OPENAI_API_KEY`                                                          |

The five XValidation rules on the CRD enforce this matrix at admission.

## Credential boundary (MDISC-15) — non-negotiable

`spec.credentialsSecretRef` is for the OPERATOR'S `GET /v1/models`
call ONLY. The reconciler MUST NOT propagate any value from it into
child Models. Inference-time credentials flow via `spec.secrets[]` —
the propagation bag — independently.

A post-render canary asserts no `credentialsSecretRef` plaintext
appears in any generated child's rendered fields.

## Minimal example — OpenAI

```yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMModelDiscovery
metadata:
  name: openai
  namespace: default
spec:
  type: openai
  credentialsSecretRef:
    name: openai-api-key
  filters:
    include: ["gpt-4o.*", "gpt-4\\.1.*"]
  refresh:
    interval: 10m
```

Generates child Models named `openai.gpt-4o`, `openai.gpt-4o-mini`,
`openai.gpt-4.1`, … each with `spec.params.model: "openai/<raw-id>"`.

## Anthropic — propagated inference creds + canonical-only filter

```yaml
spec:
  type: anthropic
  prefix: anthropic
  credentialsSecretRef:
    name: anthropic-api-key
  secrets:                                        # propagated to children
    - { as: ANTHROPIC_API_KEY, secretRef: { name: anthropic-api-key, key: ANTHROPIC_API_KEY } }
  params:                                         # propagated to children
    api_key: "{{ANTHROPIC_API_KEY}}"
    rpm: 25
    timeout: 300
  filters:
    exclude: [".*-[0-9]{8}$"]                     # drop dated snapshots; keep rolling aliases
  refresh:
    interval: 10m
```

Note `api_key: "{{ANTHROPIC_API_KEY}}"` lives in
`spec.params` (propagated) AND `as: ANTHROPIC_API_KEY` lives in
`spec.secrets[]` (propagated). The child Model's reconciler resolves
the placeholder at its own reconcile — Discovery itself never reads
the inference key.

## Bedrock — region + AWS creds

```yaml
spec:
  type: bedrock
  prefix: bedrock
  region: eu-north-1
  credentialsSecretRef:
    name: bedrock-credentials
  secrets:
    - { as: AWS_ACCESS_KEY_ID,     secretRef: { name: bedrock-credentials, key: AWS_ACCESS_KEY_ID } }
    - { as: AWS_SECRET_ACCESS_KEY, secretRef: { name: bedrock-credentials, key: AWS_SECRET_ACCESS_KEY } }
  params:
    aws_access_key_id:     "{{AWS_ACCESS_KEY_ID}}"
    aws_secret_access_key: "{{AWS_SECRET_ACCESS_KEY}}"
  filters:
    exclude: [".*embed.*", ".*titan.*"]
  refresh: { interval: 10m }
```

The reconciler also overlays `aws_region_name: eu-north-1` into each
child's `spec.params` (typed overlay — overwrite-wins).

## KubeAI — in-cluster OpenAI-compatible

```yaml
spec:
  type: kubeai
  prefix: kubeai
  baseUrl: http://kubeai.kubeai.svc/openai/v1
  credentialsSecretRef: { name: kubeai-credentials }   # may be empty
  params:
    rpm: 25
    timeout: 300
  refresh: { interval: 10m }
```

The reconciler overlays `api_base: <spec.baseUrl>` into each child's
`spec.params` (user-supplied `api_base` wins on collision). No `model:`
prefix translation — children land as `hosted_vllm/<raw-id>` or
`openai/<raw-id>` per provider semantics.

## OpenAI-compatible third-parties

Set `type: openai` + `baseUrl: <provider's /v1>`:

```yaml
spec:
  type: openai
  baseUrl: https://api.together.xyz/v1
  credentialsSecretRef: { name: together-api-key }
  refresh: { interval: 10m }
```

Works for vLLM, Together, Groq, OpenRouter, Anyscale — anything that
implements OpenAI's `GET /v1/models` + chat-completions.

## Filter order — include first, then exclude

Per spec §6.3:

1. **Include** (strict) — admit only IDs matching at least one
   pattern. Empty list = admit all. Non-empty + zero matches →
   `Ready=False`, `reason=UpstreamInvalid`.
2. **Exclude** (lenient) — drop IDs matching any pattern. Empty list =
   exclude nothing. Zero matches is fine.

RE2 syntax, anchored from start. Invalid pattern → `Ready=False`,
`reason=InvalidConfig` with offending pattern in `message`.

## Child name generation

```
<prefix>.<normalized-raw-id>
```

- `prefix` defaults to lowercased(`spec.type`).
- `normalized-raw-id` is the provider-returned ID with all non-DNS-
  1123 characters mapped to `-`; collapsed; truncated to fit 253 chars
  total with prefix.

DNS-1123 validation failure → entry in `status.skippedCandidates` with
`reason=InvalidDiscoveredName`. Name collision with a user-authored
Model whose ownerRef does NOT point at this Discovery → `skippedCandidates`
entry `reason=ExplicitModelExists`. Collision with another
Discovery's child → `reason=DuplicateDiscovery` (smallest UID wins).

## Status — what to read

```bash
kubectl get mdisc openai -o jsonpath='{.status}{"\n"}'
# {"discoveredCount":7,"generatedCount":7,"generatedChildren":["openai.gpt-4.1",...],"lastRefreshAt":"...","conditions":[...]}

kubectl get mdisc                       # printer columns: Type Ready Reason Discovered Generated Age
```

Conditions written every reconcile:

- `Ready` reasons: `Synced`, `SourceUnreachable`, `AuthFailed`,
  `SecretNotFound`, `InvalidConfig`, `UpstreamInvalid`.
- `SourceReachable` reasons: `Ok`, `Unreachable`, `AuthFailed`. Used
  as the gate for vanish-detection — when `SourceReachable=False`, the
  diff-and-delete step is SKIPPED to prevent flapping deletes during
  provider outages.

Invariant:
`discoveredCount == generatedCount + len(skippedCandidates) + len(failedCandidates)`
(filtered-out IDs do NOT count in `discoveredCount`).

## Deletion

The Discovery's finalizer waits for owned children to drain via
`ownerReferences.blockOwnerDeletion=true` cascade — each child Model's
own finalizer issues `POST /model/delete` against LiteLLM. Discovery
itself issues NO LiteLLM call.

## See also

- [Example on GitHub](https://github.com/ackstorm/alitellm-operator/tree/main/examples/example-deploy/03-modeldiscovery.yaml)
- [LiteLLMModel](model.md) — what the children look like after generation.
- [API Reference: LiteLLMModelDiscovery](../api-reference/litellm.ackstorm.ai.md#litellmmodeldiscovery)
