# LiteLLMModelAlias

An **alias** is a stable, human-chosen name that points at a real LiteLLM
model. `ackstorm.smart` today may resolve to `gemini.gemini-pro-latest` and
tomorrow to something else; callers keep using `ackstorm.smart`. Projected into
`router_settings.model_group_alias` via `POST /config/update`.

## Quick reference

| Field                   | Required | Notes                                                                         |
|-------------------------|----------|-------------------------------------------------------------------------------|
| `spec.aliases[].name`   | yes      | The name clients send to LiteLLM. Cluster-wide unique across ALL alias CRs.    |
| `spec.aliases[].value`  | yes      | The LiteLLM `model_name` (or model group) it resolves to. Forwarded verbatim.  |

```yaml
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMModelAlias
metadata:
  name: ackstorm-aliases
  namespace: litellm-system
spec:
  aliases:
    - name: ackstorm.smart
      value: gemini.gemini-pro-latest
    - name: ackstorm.fast
      value: gemini.gemini-flash-latest
```

## One map, many CRs

Every `LiteLLMModelAlias` in the cluster is aggregated into a single
`model_group_alias` map, so all CR events coalesce onto one reconcile and
produce ONE write to LiteLLM per debounce window.

Because the map has one slot per name, two CRs declaring the same alias name
conflict. The winner is decided alphabetically-last on
`(namespace, name, array index)`; losers report `Ready=False` with
`reason=AliasConflict` and name the winner in `status.aliasStatuses[]`. See
[Conflict Resolution](../concepts/conflict-resolution.md).

`applied: true` on an entry means the operator successfully wrote it into
LiteLLM — **not** that the value resolves to a real model. An alias pointing at
a name LiteLLM does not know is written happily and fails only at call time.

## OpenCode catalog

Clients like [OpenCode](https://opencode.ai) do not read model capabilities
from the proxy. They look the model id up in their own catalog
([models.dev](https://models.dev)) and fall back to defaults when it is absent
— and an alias like `ackstorm.smart` is in nobody's catalog. The symptom is a
model that works but is advertised as text-only: no image, audio or PDF attach,
sometimes no tool calling, even though LiteLLM reports all of it correctly.

The operator can close that gap by rendering the alias set into a catalog
document and writing it to a ConfigMap, for a static file server to publish.

```yaml
# Helm values
opencodeCatalog:
  enabled: true
  # Namespace the ConfigMap is written to — normally the namespace of the
  # service that will MOUNT it, since a ConfigMap can only be mounted by a pod
  # in its own namespace.
  namespace: alitellm-auth
  name: opencode-catalog
  # The PUBLIC LiteLLM URL clients reach. The operator cannot derive this: its
  # own endpoint is the in-cluster Service.
  apiBase: https://api.example.com/v1
```

The ConfigMap holds one key, `api.json`. Serve it so that it lands at
`<base>/api.json`, then point clients at the base:

```bash
export OPENCODE_MODELS_URL=https://platform.example.com/public/opencode
```

Users can then delete their hand-written provider block entirely — `npm`, `api`
and `env` all come from the catalog.

### What ends up in it

Every alias whose target resolves to a `mode: chat` model. Embeddings, tts,
stt, image generation and routers are dropped: OpenCode cannot drive them, so
they would be dead rows in its picker. An alias whose target LiteLLM does not
know is dropped too.

Capabilities are read from the **target's** `model_info`, not the alias's —
`supports_vision` → `attachment` plus the `image` modality, `supports_pdf_input`
→ `pdf`, and so on. Per-token prices are converted to the per-million figures
models.dev expects.

### Things worth knowing

**The catalog replaces the upstream one, it does not merge.** While a client
points at it, that client sees *only* this provider — anthropic, openai and the
rest disappear from its picker. That is the intent for a proxy-only setup; if
it is not yours, serve a catalog that includes the upstream document.

**Updates are not instant, by design.** An alias edit reaches a client through
the ConfigMap, the kubelet's volume sync (~60s) and the client's own cache. A
client typically only re-reads at startup. Nothing here is built to be fast,
because aliases change rarely.

**A catalog failure never affects your aliases.** The write happens after the
aliases are already in LiteLLM, so a ConfigMap problem emits a Warning Event
(`CatalogWriteFailed`) and leaves `Ready` alone. Check events on the CR, not
the condition.

**Cross-namespace writes need RBAC.** When `opencodeCatalog.namespace` differs
from the release namespace, the chart adds a Role and RoleBinding scoped to it.
No ClusterRoleBinding is created — the operator does not gain cluster-wide
ConfigMap write for the sake of one file.
