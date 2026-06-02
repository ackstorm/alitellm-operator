# Quick Start

End-to-end: install the operator, declare the singleton
`LiteLLMConnection/default`, create your first `LiteLLMTeam` and
`LiteLLMModel`, then call the model through the LiteLLM proxy.

## Prerequisites

- alitellm-operator [installed](installation.md) in the cluster
  (default watch namespace: `default`).
- An existing LiteLLM proxy reachable from the cluster.
- `kubectl` access.

!!! note
    `alitellm-operator` does NOT deploy LiteLLM. Use the
    [BerriAI/litellm](https://github.com/BerriAI/litellm) upstream
    Helm chart first if you need a proxy.

!!! warning "LiteLLM must persist models in its DB"
    The proxy needs `STORE_MODEL_IN_DB=True` (env var) **or**
    `general_settings.store_model_in_db: true` (config) before it will
    accept `POST /model/{new,update}`. Without it, every `LiteLLMModel`
    goes `Ready=False, reason=LiteLLMRejected` and LiteLLM logs
    `Set 'STORE_MODEL_IN_DB='True'' in your env to enable this feature`.
    This is upstream LiteLLM config, not an operator setting. Teams,
    MCP servers, and A2A agents are unaffected — only model endpoints
    gate on the flag.

The watch namespace is whatever you set as Helm `watchNamespace`
(default `default`). Every CR below lands there.

## 1. Master-key Secret

```bash
NS=default
kubectl -n $NS create secret generic litellm-master-key \
  --from-literal=master-key='sk-...your-master-key...'
```

## 2. LiteLLMConnection/default

In v1alpha1 every dependent CR binds implicitly to
`LiteLLMConnection/default` — there is no per-CR `connectionRef`. The
name MUST be `default`.

```yaml
# connection.yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMConnection
metadata:
  name: default
  namespace: default
spec:
  endpoint: http://litellm.default.svc.cluster.local:4000
  masterKeySecretRef:
    name: litellm-master-key
    key:  master-key
```

```bash
kubectl apply -f connection.yaml
kubectl wait --for=condition=Ready litellmconnection/default --timeout=60s
```

Expected: `Ready=True, reason=Probed`. The operator just hit
`GET <endpoint>/key/info` with the master key.

## 3. Provider API key

Most providers need a Secret. OpenAI example:

```bash
kubectl -n default create secret generic openai-api-key \
  --from-literal=OPENAI_API_KEY='sk-...your-openai-key...'
```

## 4. LiteLLMModel

`metadata.name` IS the alias clients call. `spec.params` becomes the
`litellm_params` body verbatim; `{{NAME}}` placeholders are resolved
from `spec.secrets[]` before the body reaches LiteLLM.

```yaml
# model.yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMModel
metadata:
  name: gpt-4o-mini
  namespace: default
spec:
  params:
    model:   openai/gpt-4o-mini
    api_key: "{{OPENAI_API_KEY}}"
  info:
    description: "OpenAI gpt-4o-mini via dogfood operator"
  secrets:
    - as: OPENAI_API_KEY
      secretRef:
        name: openai-api-key
        key:  OPENAI_API_KEY
```

```bash
kubectl apply -f model.yaml
kubectl wait --for=condition=Ready mdl/gpt-4o-mini --timeout=60s
kubectl get mdl gpt-4o-mini -o jsonpath='{.status.lastRendered.modelID}{"\n"}'
# <litellm-assigned UUID>
```

## 5. LiteLLMTeam (optional — used for budget / rate-limit)

`metadata.name` IS the LiteLLM `team_alias`. There is no
`spec.teamAlias`. Budget and rate limits live in typed sub-blocks.

```yaml
# team.yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMTeam
metadata:
  name: platform
  namespace: default
spec:
  budget:
    limit: 100.0
    period: "30d"
  rateLimits:
    rpm: 600
    tpm: 100000
```

```bash
kubectl apply -f team.yaml
kubectl wait --for=condition=Ready team/platform --timeout=60s
kubectl get team platform -o jsonpath='{.status.lastRendered.teamID}{"\n"}'
```

## 6. Call the model

```bash
kubectl -n default port-forward svc/litellm 4000:4000 &

curl http://localhost:4000/chat/completions \
  -H "Authorization: Bearer sk-...master-key..." \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o-mini",
    "messages": [{"role": "user", "content": "ping"}]
  }'
```

Note `"model"` in the body is the CR's `metadata.name`
(`gpt-4o-mini`), NOT the upstream provider id.

## What just happened

- Operator reconciled `LiteLLMConnection/default` → probed LiteLLM →
  cached the snapshot.
- `LiteLLMModel/gpt-4o-mini` reconciler resolved `{{OPENAI_API_KEY}}`
  from the Secret, hashed the rendered body, called
  `POST /model/new`, pinned the assigned UUID to
  `status.lastRendered.modelID`.
- `LiteLLMTeam/platform` reconciler stamped `team_alias=platform`,
  projected budget + rate-limits, called `POST /team/update`, pinned
  `status.lastRendered.teamID`.

Subsequent reconciles short-circuit when the hash matches; the
operator only re-issues HTTP calls on spec edit, Secret rotation, or
drift detection (LiteLLM-side out-of-band mutation).

## Next steps

- [User Guide](../user-guide/index.md) — every CRD, real field shapes,
  status semantics.
- [Examples](https://github.com/ackstorm/alitellm-operator/tree/main/examples/example-deploy)
  — runnable manifests for every kind including A2A agents, MCP servers,
  Discovery, and GuardRails.
- [API Reference](../api-reference/litellm.ackstorm.ai.md) — auto-generated
  schema for every CRD.
