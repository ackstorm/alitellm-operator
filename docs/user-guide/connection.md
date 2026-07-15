# LiteLLMConnection

A `LiteLLMConnection` declares how the operator reaches a LiteLLM proxy:
its base URL plus a reference to the Secret holding the master key.

In v1alpha1 the operator reconciles against the SINGLETON
`LiteLLMConnection/default` (name `default`, in the watch namespace).
Per-CR `connectionRef` is NOT modeled — every Team / Model / MCPServer
/ A2AAgent / GuardRail / Discovery binds implicitly to that one
Connection. Multi-Connection support is deferred to v1beta1.

## Quick reference

| Field                              | Required | Default | Notes                                                                             |
|------------------------------------|----------|---------|-----------------------------------------------------------------------------------|
| `metadata.name`                    | yes      |         | Must be `default` for any dependent CR to bind to it.                             |
| `spec.endpoint`                    | yes      |         | Base URL (e.g. `http://litellm.default.svc.cluster.local:4000`).                  |
| `spec.masterKeySecretRef.{name,key}` | yes    |         | Secret holding the LiteLLM master key. Must live in the SAME namespace as the CR. |
| `spec.mcpToolPrefixSeparator`      | no       | `.`     | Enum `{".", "-"}`. Character LiteLLM REJECTS inside `server_name`; the operator rewrites the wire payload to swap it. |
| `spec.requeueOnRejectedAfter`      | no       | `5m`    | Retry cadence for dependent reconcilers after `LiteLLMRejected` / `SecretNotFound`. Clamped to `[1m, 1h]`. |
| `spec.maxRequestsPerSecond`        | no       | `5`     | Sustained outbound HTTP rate cap. `0` disables (not recommended).                 |
| `spec.maxBurst`                    | no       | `10`    | Token-bucket burst paired with `maxRequestsPerSecond`.                            |

## Minimal example

```yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMConnection
metadata:
  name: default                          # MUST be `default` in v1alpha1
  namespace: default                     # matches operator's WATCH_NAMESPACE
spec:
  endpoint: http://litellm.default.svc.cluster.local:4000
  masterKeySecretRef:
    name: litellm-master-key
    key:  master-key
```

## With rate-limit + MCP separator override

```yaml
spec:
  endpoint: https://litellm.example.com
  masterKeySecretRef:
    name: litellm-master-key
    key:  master-key
  mcpToolPrefixSeparator: "-"            # non-stock LiteLLM that forbids "-" in server_name
  maxRequestsPerSecond: 20               # raise for higher CR counts
  maxBurst: 50
  requeueOnRejectedAfter: 1m             # faster retry after upstream fix lands
```

## What the operator does

On reconcile, the operator probes `GET <endpoint>/key/info` with the
master key:

- 200 → `Ready=True, reason=Probed`. Connection cache snapshot
  refreshes; dependent reconcilers wake up via fan-in watch.
- 401 → `Ready=False, reason=AuthFailed`. Master-key Secret rotated
  out-of-band → re-apply.
- network / 5xx → `Ready=False, reason=Unreachable`. Reconciler retries
  on `requeueOnRejectedAfter`.

`status.conditions[type=Ready]` is the gate every dependent reconciler
reads (echoed as `LiteLLMUnavailable` when `False`). The Connection
cache snapshot also carries `mcpToolPrefixSeparator`,
`requeueOnRejectedAfter`, and the rate-limit knobs — dependents read
the same values via the cache, not by re-reading the CR.

### Enforcing HTTPS for remote endpoints

By default a remote `http://` endpoint only logs a warning
(`MasterKeyOverPlaintextHTTP`) — in-cluster `http://litellm.<ns>.svc`
is the common, acceptable deployment. To hard-reject plaintext-HTTP
remotes instead (`Ready=False`, `reason=InsecureEndpoint`, terminal
until `spec.endpoint` is edited):

```bash
kubectl set env -n litellm-system deploy/alitellm-operator \
  LITELLM_OPERATOR_REQUIRE_HTTPS_REMOTE=true
```

Loopback, `*.svc`, and bare service-name hosts are always classified
in-cluster and exempt (see `litellm.ClassifyEndpointTransport`).

## See also

- [Example on GitHub](https://github.com/ackstorm/alitellm-operator/tree/main/examples/example-deploy/01-litellmconnection.yaml)
- [API Reference: LiteLLMConnection](../api-reference/litellm.ackstorm.ai.md#litellmconnection)
