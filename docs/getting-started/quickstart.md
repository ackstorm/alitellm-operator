# Quick Start

This guide creates your first `LiteLLMConnection` plus a sample `LiteLLMTeam`
and `LiteLLMModel`. It assumes you already have a LiteLLM proxy running
somewhere reachable from the cluster.

## Prerequisites

- alitellm-operator [installed](installation.md) in your cluster.
- An existing LiteLLM proxy URL and master key.
- `kubectl` access to the cluster.

!!! note
    `alitellm-operator` does **not** deploy LiteLLM itself. It reconciles
    resources **against** a running LiteLLM proxy. If you need to deploy
    LiteLLM as well, use the upstream Helm chart from
    [BerriAI/litellm](https://github.com/BerriAI/litellm) first.

## 1. Store the master key

```bash
kubectl create namespace litellm
kubectl -n litellm create secret generic litellm-master-key \
  --from-literal=master-key='sk-...'
```

## 2. Declare the Connection

```yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMConnection
metadata:
  name: proxy-prod
  namespace: litellm
spec:
  url: https://litellm.example.com
  masterKeySecretRef:
    name: litellm-master-key
    key:  master-key
```

```bash
kubectl apply -f connection.yaml
kubectl -n litellm wait --for=condition=Ready litellmconnection/proxy-prod --timeout=60s
```

## 3. Declare a Team and a Model

```yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMTeam
metadata:
  name: team-platform
  namespace: litellm
spec:
  connectionRef: { name: proxy-prod }
  teamAlias: platform
  maxBudget: 100.0
---
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMModel
metadata:
  name: gpt-4o-mini
  namespace: litellm
spec:
  connectionRef: { name: proxy-prod }
  modelName: gpt-4o-mini
  litellmParams:
    model:  openai/gpt-4o-mini
    apiKey: ${OPENAI_API_KEY}
```

```bash
kubectl apply -f team.yaml -f model.yaml
kubectl -n litellm get litellmteams,litellmmodels
```

## Next steps

- Walk through the [User Guide](../user-guide/index.md) for the remaining
  resource kinds (A2A agents, MCP servers, discovery).
- See the runnable manifests under
  [`examples/`](https://github.com/ackstorm/alitellm-operator/tree/main/examples).
- Review the [API Reference](../api-reference/litellm.ackstorm.ai.md) for the full schema
  of each resource.

!!! warning "Schema accuracy"
    The field names above (`teamAlias`, `maxBudget`, `litellmParams.model`,
    etc.) are illustrative. Always check the auto-generated
    [API Reference](../api-reference/litellm.ackstorm.ai.md) or the CRDs under
    `config/crd/bases/` for the authoritative schema.
