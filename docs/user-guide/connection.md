# LiteLLMConnection

A `LiteLLMConnection` declares how the operator reaches a LiteLLM proxy:
its base URL plus a reference to the Secret holding the master key.

All other resources (Team, Model, Discovery kinds, …) must reference a
Connection via `spec.connectionRef`.

!!! note
    Detailed reconciliation semantics, finalizer behavior, and status
    fields are documented in the auto-generated [API
    Reference](../api-reference/litellm.ackstorm.ai.md).

## Minimal example

```yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMConnection
metadata:
  name: proxy-prod
  namespace: litellm-system
spec:
  url: https://litellm.example.com
  masterKeySecretRef:
    name: litellm-master-key
    key:  master-key
```

## See also

- [Examples on GitHub](https://github.com/ackstorm/alitellm-operator/tree/main/examples)
- [API Reference: LiteLLMConnection](../api-reference/litellm.ackstorm.ai.md#litellmconnection)
