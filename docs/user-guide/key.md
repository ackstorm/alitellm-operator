# LiteLLMKey

A `LiteLLMKey` creates a LiteLLM virtual key for one service and writes its
value to a Secret in that service's namespace. The key has no user and no
team: it can call the models you list, on the routes you allow, within the
budget and rate limits you set. Verified on LiteLLM 1.111.0.

Use it so a service does not need the LiteLLM master key.

```yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMKey
metadata:
  name: billing-service
  namespace: litellm-system        # the operator's namespace
spec:
  models: [gpt-4o, fast]           # model names or LiteLLMModelAlias names
  budget: {limit: 5, period: 30d}  # optional
  rateLimits: {rpm: 60}            # optional
  secretRef: {namespace: billing, name: billing-llm-key, key: api-key}
  deletionPolicy: Delete           # Orphan (default) | Delete
```

The service reads the key from the Secret:

```yaml
env:
  - name: LITELLM_API_KEY
    valueFrom:
      secretKeyRef: {name: billing-llm-key, key: api-key}
```

## Fields

| Field | Notes |
|---|---|
| `spec.models` | Models the key may call. A key with none can call no model: LiteLLM lets a teamless key with an empty list call every model, so the operator sends LiteLLM's `no-default-models` in its place. Names are not checked against LiteLLM. |
| `spec.allowedRoutes` | Proxy routes the key is confined to, sent verbatim as `allowed_routes`. See [Routes](#routes). Removing it clears the restriction. |
| `spec.budget` | `limit` in USD and `period` such as `30d`. Removing it clears the budget. |
| `spec.rateLimits` | `rpm` and `tpm`. Removing it clears the limits. |
| `spec.secretRef` | Where the key is written. Immutable. To move the Secret, delete the `LiteLLMKey` and create it again. |
| `spec.deletionPolicy` | What happens when LiteLLM cannot confirm the delete. See [Deletion Semantics](../concepts/deletion-semantics.md). |

The key's alias in LiteLLM is the `LiteLLMKey` name. LiteLLM requires an alias
of at least two characters.

## Routes

Without `allowedRoutes` a key reaches the routes LiteLLM gives any virtual
key: the model APIs and a few read-only ones. With it, the key reaches only
the routes listed. An entry is one of:

- a path, which matches itself and everything under it (`/metrics` covers
  `/metrics/`);
- a pattern with `*` (`/v1/*`);
- a LiteLLM route group name (`llm_api_routes`, `info_routes`).

Entries are not checked against LiteLLM.

!!! warning "An entry also opens an admin-only route"
    A route that LiteLLM reserves for admins becomes reachable by the key
    when an entry matches it. That is what makes a scrape key work, and it
    applies to every other admin route too. List only what the consumer
    calls.

`allowedRoutes` does not grant models. A key that lists a model route and no
`models` is refused there with `key_model_access_denied`.

### A key for Prometheus

With `litellm_settings.require_auth_for_metrics_endpoint` on, LiteLLM
refuses a plain virtual key on `/metrics` with 401. This key can scrape and
do nothing else:

```yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMKey
metadata:
  name: prometheus-scrape
  namespace: litellm-system
spec:
  allowedRoutes: [/metrics]
  secretRef: {namespace: monitoring, name: litellm-scrape-key, key: api-key}
  deletionPolicy: Delete
```

LiteLLM serves the metrics at `/metrics/` and answers `/metrics` with a 307
redirect to it.

## Allow the target namespace

The operator may only touch Secrets in its own namespace. List every namespace
a `LiteLLMKey` writes to in the Helm value:

```yaml
keySecretNamespaces: [billing, search]
```

Each listed namespace gets a RoleBinding that grants the operator `get`,
`create` and `delete` on Secrets. A `LiteLLMKey` that targets a namespace not
on the list reports `Ready=False`, reason `SecretAccessDenied`.

!!! warning "The operator can read and delete every Secret in a listed namespace"
    Kubernetes cannot limit `get`, `create` or `delete` to the Secrets the
    operator created, and `create` covers Secrets of any type. List only
    namespaces whose services need a key.

Without Helm, create the RoleBinding yourself in each target namespace,
bound to a ClusterRole with `get`, `create`, `delete` on `secrets` and to the
operator's ServiceAccount.

## Rotation

Delete the Secret. On its next reconcile the operator writes a new value,
deletes the old key in LiteLLM and registers the new one. The operator does
not watch Secrets outside its own namespace, so that reconcile is the next
safety relist (10 minutes by default). To rotate at once, also touch the
`LiteLLMKey`:

```bash
kubectl -n billing delete secret billing-llm-key
kubectl -n litellm-system annotate litellmkey billing-service rotated-at="$(date +%s)" --overwrite
```

Pods that read the key from an environment variable must be restarted.

## What the operator does

- Generates the value itself (`sk-` plus 26 random characters) and passes it
  to `POST /key/generate`. If LiteLLM loses the key, the operator registers
  the same value again and the service keeps working.
- Corrects models, routes, budget and limits changed outside the CR.
- Never writes to a Secret it did not create. An existing Secret with the
  same name gives `Ready=False`, reason `SecretNotOwned`.
- Never deletes a LiteLLM key it did not create. A key that already holds the
  alias gives `Ready=False`, reason `Conflict`.
- Deletes the key and the Secret when the `LiteLLMKey` is deleted.

The key value appears only in the Secret. It is not in the CR status, in
events or in the operator's logs, and the operator identifies the key to
LiteLLM by its SHA-256 hash.

Two debugging switches break that rule and must stay off wherever real keys exist: `LITELLM_OPERATOR_DANGEROUSLY_LOG_BODIES=true` logs the `/key/generate` request and response, which carry the key value, and `LITELLM_OPERATOR_DANGEROUSLY_INCLUDE_REJECTED_BODY=true` copies LiteLLM's rejection message into the CR status.

## Conditions

| Reason | Meaning |
|---|---|
| `Synced` | The key exists in LiteLLM with the declared settings. |
| `LiteLLMUnavailable` | The `LiteLLMConnection` is not ready. |
| `LiteLLMRejected` | LiteLLM refused the request. With `general_settings.disable_custom_api_keys: true` on the proxy, every `LiteLLMKey` ends here. |
| `SecretAccessDenied` | The target namespace is not in `keySecretNamespaces`. |
| `SecretNotOwned` | The target Secret exists and was not created for this `LiteLLMKey`. |
| `Conflict` | Another key, not created by the operator, holds the alias. |

A namespace that does not exist has no RoleBinding, so a `LiteLLMKey` that targets it reports `SecretAccessDenied`. `helm install` and `helm upgrade` fail when a namespace listed in `keySecretNamespaces` does not exist: create the namespace first.

## Limits

- The key restricts models and routes only. It carries no agent or MCP permissions of its own, so LiteLLM's defaults for a key without a team apply: A2A agents registered on the proxy are reachable with it unless `general_settings.agent_access_default_deny` is set on the proxy. What such a key can reach on MCP servers is not verified.
- One operator per LiteLLM. Two operators that share a LiteLLM and each have a `LiteLLMKey` with the same name replace and delete each other's key: a key is matched by its alias and the `managed-by` mark, not by which operator created it.
- No team attachment, expiry or automatic rotation.
