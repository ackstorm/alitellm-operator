# LiteLLMModelDiscovery

Pipeline B CR. Points the operator at ONE upstream provider and
auto-generates `LiteLLMModel` child CRs for each model the provider
publishes. Discovery NEVER calls LiteLLM directly — each generated
child reconciles into LiteLLM via the `LiteLLMModel` controller
(Pipeline A).

## Quick reference

| Field                       | Required        | Notes                                                                                  |
|-----------------------------|-----------------|----------------------------------------------------------------------------------------|
| `spec.type`                 | yes             | Enum: `anthropic`, `bedrock`, `elevenlabs`, `gemini`, `kubeai`, `openai`.              |
| `spec.prefix`               | no              | DNS-1123 segment prepended to each child's `metadata.name`. Default: lowercased `spec.type`. |
| `spec.credentialsSecretRef` | per-provider    | Secret holding upstream API key (operator-side ONLY — never propagated to children).   |
| `spec.region`               | bedrock only    | AWS region. One region per CR (multi-region → multiple CRs).                           |
| `spec.baseUrl`              | kubeai (req), openai (opt) | Provider HTTP endpoint. Any non-empty value auto-overlays into each child's `api_base` (so LiteLLM routes inference to the same endpoint models were discovered from). |
| `spec.litellmProvider`      | no (openai only) | Overrides the LiteLLM pricing-prefix provider stamped on each child's `litellm_params.model` (default: derived from `spec.type`). E.g. `openrouter` to bill under OpenRouter's cost table. CEL-restricted to `type: openai`. |
| `spec.params`               | no              | Pass-through bag propagated VERBATIM into every child's `spec.params`.                 |
| `spec.info`                 | no              | Pass-through bag propagated into every child's `spec.info`.                            |
| `spec.secrets[]`            | no              | Substitution map propagated into every child's `spec.secrets[]` (NOT resolved here).   |
| `spec.filters.include`      | no              | RE2 patterns — anchored, include-FIRST. Empty → admit all. Matched against the raw ID, the normalized ID, AND the child name. |
| `spec.filters.exclude`      | no              | RE2 patterns — applied AFTER include. Same three-form match surface as `include`.      |
| `spec.refresh.interval`     | yes             | Cadence between refreshes. CEL floor `1m` enforced at admission.                       |

## Provider field matrix

| Type        | Requires                    | Forbids                       | Secret keys read                                                          |
|-------------|-----------------------------|-------------------------------|---------------------------------------------------------------------------|
| `anthropic` | `credentialsSecretRef`      | `region`, `baseUrl`           | `ANTHROPIC_API_KEY`                                                       |
| `bedrock`   | `region`                    | `baseUrl`                     | `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` (+ optional `AWS_SESSION_TOKEN`) |
| `elevenlabs`| `credentialsSecretRef`      | `region`, `baseUrl`           | `ELEVENLABS_API_KEY`                                                      |
| `gemini`    | `credentialsSecretRef`      | `region`, `baseUrl`           | `GEMINI_API_KEY` (or `GOOGLE_API_KEY`)                                    |
| `kubeai`    | `baseUrl`                   | `credentialsSecretRef`, `region` | none                                                                   |
| `openai`    | `credentialsSecretRef`      | `region`                      | `OPENAI_API_KEY`                                                          |

The six per-type XValidation rules on the CRD enforce this matrix at admission.

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
implements OpenAI's `GET /v1/models` + chat-completions. The reconciler
overlays `api_base: <spec.baseUrl>` into each child's `spec.params`
(user-supplied `api_base` wins on collision), so LiteLLM routes inference
to the third-party endpoint instead of `api.openai.com`.

### Correct cost tracking — `spec.litellmProvider`

By default a `type: openai` Discovery stamps `openai/<id>` on every child's
`litellm_params.model`, so LiteLLM bills them under OpenAI's price table even
when they actually run on a third-party gateway. Set `spec.litellmProvider`
to the gateway's LiteLLM provider name so the pricing prefix (and thus the
cost table) matches:

```yaml
spec:
  type: openai
  baseUrl: https://openrouter.ai/api/v1
  litellmProvider: openrouter        # children stamp `openrouter/<id>`
  credentialsSecretRef: { name: openrouter-api-key }
  refresh: { interval: 10m }
```

Only the pricing prefix on `litellm_params.model` changes — the child CR
NAME still derives from `spec.type`/`spec.prefix`, so enabling this on an
existing Discovery updates the children **in place** (`POST /model/update`),
never recreates them, and LiteLLM applies the new cost table on the next
request (cost is computed per-request from the live `litellm_params.model`).
Valid provider names follow `^[a-z0-9_]+$` (e.g. `openrouter`, `together_ai`,
`groq`, `hosted_vllm`). The field is CEL-restricted to `type: openai`.

## ElevenLabs — audio (TTS / STT)

ElevenLabs is a hosted SaaS audio provider. Like `anthropic`/`gemini` it
requires `credentialsSecretRef` and forbids `region`/`baseUrl` (single
public endpoint). The reconciler calls `GET https://api.elevenlabs.io/v1/models`
with the `xi-api-key` header and generates children with
`spec.params.model: "elevenlabs/<raw-id>"`.

```yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMModelDiscovery
metadata:
  name: elevenlabs
spec:
  type: elevenlabs
  prefix: elevenlabs
  credentialsSecretRef: { name: elevenlabs-credentials }   # key: ELEVENLABS_API_KEY
  secrets:
    - { as: ELEVENLABS_API_KEY, secretRef: { name: elevenlabs-credentials, key: ELEVENLABS_API_KEY } }
  params:
    api_key: "{{ELEVENLABS_API_KEY}}"
  filters:
    include: [ "^eleven_.*_v2$", "^eleven_v3$", "^scribe_v1$" ]
  refresh:
    interval: 10m
```

`/v1/models` returns TTS, STT, and voice-conversion models mixed together;
use `filters.include` to narrow. The discovery-time `credentialsSecretRef`
key is operator-side only — the inference-time key for each child flows via
`secrets[]` + `params.api_key` (MDISC-15 separation). The LiteLLM proxy must
run with `STORE_MODEL_IN_DB=True` or each child's `POST /model/new` 500s.

## Filter order — include first, then exclude

Per spec §6.3:

1. **Include** (strict) — admit only IDs matching at least one
   pattern. Empty list = admit all. Non-empty + zero matches →
   `Ready=False`, `reason=UpstreamInvalid`.
2. **Exclude** (lenient) — drop IDs matching any pattern. Empty list =
   exclude nothing. Zero matches is fine.

RE2 syntax, anchored from start. Invalid pattern → `Ready=False`,
`reason=InvalidConfig` with offending pattern in `message`.

RE2 is NOT glob: in `openai-*` the `*` means "zero or more `-`", so the
pattern is equivalent to `^openai`. Glob-style patterns usually still do
what you meant by accident — `.*` is the literal wildcard.

### What a pattern is matched against

Each candidate is matched against **three** forms — a pattern hitting any
one of them keeps (include) or drops (exclude) that candidate:

| Form | Example | Where you see it |
|---|---|---|
| raw upstream ID | `~anthropic/claude-sonnet-latest` | `GET <baseUrl>/models` → `data[].id`; child `spec.params.model` |
| normalized ID | `anthropic-claude-sonnet-latest` | — |
| child name | `openrouter.anthropic-claude-sonnet-latest` | `status.generatedChildren`; LiteLLM's model name |

So a pattern copied out of `status.generatedChildren` works, and so does one
written against the provider's raw ID. The kept set is always rendered from
the raw ID — matching on a name form changes nothing downstream.

Why three forms: normalization is lossy (it lowercases, maps every
non-`[a-z0-9.-]` char to `-`, collapses runs, and trims leading/trailing
non-alphanumerics), so a character that distinguishes two raw IDs can vanish
from the visible name. OpenRouter is the sharp case — it publishes rolling
aliases under a `~` prefix:

```text
anthropic/claude-sonnet-5        → openrouter.anthropic-claude-sonnet-5
~anthropic/claude-sonnet-latest  → openrouter.anthropic-claude-sonnet-latest
```

`exclude: ["anthropic-.*"]` drops **both**: it misses the second candidate's
raw ID (which starts with `~`) but matches its normalized name. Matching the
raw form only — the behavior up to v0.8.1 — kept it, with nothing in the status
to explain why.

To list the raw IDs a provider publishes:

```bash
curl -s -H "Authorization: Bearer $KEY" https://openrouter.ai/api/v1/models \
  | jq -r '.data[].id' | sort
```

or, for models already generated, read each child's `spec.params.model` (the
raw ID verbatim, behind the `<litellmProvider>/` pricing prefix):

```bash
kubectl get litellmmodel -l litellm.ackstorm.ai/generated-by=openrouter-discovery \
  -o custom-columns=CHILD:.metadata.name,UPSTREAM:.spec.params.model
```

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
Discovery's child → `reason=Conflict` (renamed from `DuplicateDiscovery`
per ADR-0001 for cross-kind consistency; first-create-wins until a
follow-up PR adds alpha-last-wins ownership transfer).

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
