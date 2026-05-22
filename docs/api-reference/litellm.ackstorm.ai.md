# API Reference

## Packages
- [litellm.ackstorm.ai/v1alpha1](#litellmackstormaiv1alpha1)


## litellm.ackstorm.ai/v1alpha1

Package v1alpha1 contains API Schema definitions for the litellm v1alpha1 API group.

### Resource Types
- [LiteLLMA2AAgent](#litellma2aagent)
- [LiteLLMA2AAgentList](#litellma2aagentlist)
- [LiteLLMConnection](#litellmconnection)
- [LiteLLMConnectionList](#litellmconnectionlist)
- [LiteLLMMCPServer](#litellmmcpserver)
- [LiteLLMMCPServerDiscovery](#litellmmcpserverdiscovery)
- [LiteLLMMCPServerDiscoveryList](#litellmmcpserverdiscoverylist)
- [LiteLLMMCPServerList](#litellmmcpserverlist)
- [LiteLLMModel](#litellmmodel)
- [LiteLLMModelDiscovery](#litellmmodeldiscovery)
- [LiteLLMModelDiscoveryList](#litellmmodeldiscoverylist)
- [LiteLLMModelList](#litellmmodellist)
- [LiteLLMTeam](#litellmteam)
- [LiteLLMTeamList](#litellmteamlist)



#### A2AAgentSpec



A2AAgentSpec defines the desired state of A2AAgent per spec §6.6
(`_FINALv3` flat shape).

A2A-01.06 collectively: a user can declare an A2AAgent CR registering
one A2A agent entry against the LiteLLM instance referenced by
LiteLLMConnection/default. The shape is flat per `_FINALv3`: NO
`spec.type` discriminator and NO nested `spec.litellm.*` sub-object.
`spec.endpoint` is the modeled typed field (overlays
`agent_card_params.url` per spec §6.6); `spec.agentCard` is a REQUIRED
JSON pass-through bag carrying the agent's published A2A protocol card;
`spec.params` is an OPTIONAL JSON pass-through bag forwarded verbatim
to `AgentConfig` top-level (NOT inside `agent_card_params`, per spec
§6.6 — diverges from MCP and is the source of the `agent_card_params`
collision in the ProjectionOverride taxonomy).

Two-pass substitution (Phase 5 D-04): on each reconcile, `spec.params`
is walked first, then `spec.agentCard`, sharing a single resolved
Secret map. Placeholders `{{NAME}}` in EITHER bag are resolved from
`spec.secrets[]` before the body reaches LiteLLM. The shared map
ensures a Secret referenced in both bags is fetched from the
Kubernetes API exactly once per reconcile.

Four-collision ProjectionOverride taxonomy (Phase 5 D-05) — fired as
Kubernetes Events with `reason=ProjectionOverride`, `type=Warning`,
each at most once per reconcile pass:
- agent_name — `spec.params.agent_name` overlaid by metadata.name.
- agent_card_params — `spec.params.agent_card_params` overlaid by spec.agentCard.
- agent_card_params.url — `spec.agentCard.url` overlaid by spec.endpoint.
- model_info — `spec.params.model_info` reserved by LiteLLM §6.6.



_Appears in:_
- [LiteLLMA2AAgent](#litellma2aagent)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `endpoint` _string_ | Endpoint is the A2A agent URL forwarded verbatim as the `url`<br />field inside `agent_card_params` of LiteLLM's `AgentConfig`.<br />Required + non-empty per spec §6.6. The reconciler does NOT<br />validate scheme/host structure beyond MinLength=1; users are<br />responsible for supplying a well-formed URL their A2A agent<br />runtime accepts. Collides with `spec.agentCard.url` if present<br />— the operator overlay always wins and emits a<br />`ProjectionOverride` Event keyed `agent_card_params.url`<br />(Phase 5 D-05). |  | MinLength: 1 <br />Required: \{\} <br /> |
| `agentCard` _[RawExtension](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#rawextension-runtime-pkg)_ | AgentCard is a REQUIRED JSON pass-through bag carrying the<br />agent's A2A protocol "agent card" structure (name, description,<br />capabilities, defaultInputModes, defaultOutputModes, skills,<br />etc.). Any JSON object is accepted<br />(x-kubernetes-preserve-unknown-fields: true) — the operator<br />validates only well-formedness; A2A protocol semantics are<br />enforced by LiteLLM and downstream agent runtimes.<br />String-typed leaves at ANY depth (including nested arrays like<br />`skills[].description`, `defaultInputModes[]`, `capabilities.*`<br />string leaves) may contain `\{\{NAME\}\}` placeholders resolved<br />from `spec.secrets[]` (§5.2; Phase 3 D-05; Phase 5 D-04<br />second-pass scope). Non-string leaves pass through unchanged.<br />If `spec.agentCard.url` is set, the operator overlays it with<br />`spec.endpoint` and emits a `ProjectionOverride` Event keyed<br />`agent_card_params.url` (Phase 5 D-05). |  | Required: \{\} <br /> |
| `params` _[RawExtension](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#rawextension-runtime-pkg)_ | Params is an OPTIONAL JSON pass-through bag forwarded verbatim<br />to LiteLLM's `AgentConfig` body at the TOP level (NOT inside<br />`agent_card_params` — diverges from MCP and is the source of<br />the `agent_card_params` collision in the ProjectionOverride<br />taxonomy). Any JSON object is accepted<br />(x-kubernetes-preserve-unknown-fields: true). String-typed leaf<br />values may contain `\{\{NAME\}\}` placeholders resolved from<br />`spec.secrets[]` (§5.2; Phase 5 D-04 first-pass scope).<br />The operator NEVER adds or removes keys inside this bag — the<br />user's declared keyset IS the desired state. Per Phase 5 D-05,<br />the operator applies structural overlays after substitution:<br />- `mergedBody.agent_name` is overlaid by metadata.name.<br />- `mergedBody.agent_card_params` is overlaid by spec.agentCard.<br />Collisions surface as `ProjectionOverride` Events keyed by the<br />offending top-level key. `model_info` triggers a warning Event<br />(LiteLLM-reserved per spec §6.6) but is NOT overlaid by the<br />operator (the user's value is preserved on its way to LiteLLM;<br />LiteLLM may itself reject the body). |  |  |
| `secrets` _[SecretSubstitution](#secretsubstitution) array_ | Secrets is the substitution map for resolving `\{\{NAME\}\}`<br />placeholders in `spec.params` AND `spec.agentCard` string-typed<br />leaves (Phase 5 D-04 — two-pass with shared resolvedSecrets<br />map). Each entry maps an uppercase NAME (the `as` field) to a<br />Kubernetes Secret key (`secretRef`). Placeholders in either bag<br />are replaced with the resolved plaintext value before the body<br />is forwarded to LiteLLM. Secret material NEVER appears in logs,<br />Events, or `status.conditions[].message` (§9.1, AC-S1).<br />SEC-03 uniqueness of `spec.secrets[].as` values is enforced as a<br />runtime check in the A2AAgent reconciler (same pattern as Model<br />And MCPServer — CEL list-uniqueness was<br />deferred to v1beta1).<br />SEC-07: if a declared `as` is unreferenced by ANY placeholder<br />across both bags (union of refs from the two-pass), the<br />reconciler emits a `UnusedSecretRef` Event (type=Normal). |  |  |


#### A2AAgentStatus



A2AAgentStatus defines the observed state of A2AAgent per spec §6.6 +
Phase 5 D-03 (nested `lastRendered` substruct).



_Appears in:_
- [LiteLLMA2AAgent](#litellma2aagent)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the metadata.generation of the A2AAgent CR<br />the reconciler most recently processed successfully (OWN-08<br />carry-forward). |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#condition-v1-meta) array_ | Conditions carries the standard metav1.Condition list. The single<br />type defined for A2AAgent is `Ready`, with reason values from §6.0:<br />- Synced — rendered body matches LiteLLM; no drift.<br />- LiteLLMUnavailable — LiteLLMConnection/default not Ready<br />(D-08 echo-reason from connection cache).<br />- LiteLLMRejected — LiteLLM returned a 4xx (non-401) on mutation.<br />- SecretNotFound — a spec.secrets[].secretRef is missing OR<br />a `\{\{NAME\}\}` placeholder has no matching<br />spec.secrets[].as entry.<br />- InvalidConfig — spec.params or spec.agentCard not valid<br />JSON, or duplicate spec.secrets[].as<br />values. |  |  |
| `lastRendered` _[A2ALastRenderedStatus](#a2alastrenderedstatus)_ | LastRendered is the operator-side drift source of truth per Phase 3<br />D-01 / D-07 (extended for A2AAgent per Phase 5 D-03). It records<br />the post-substitution rendered state that was last successfully<br />applied to LiteLLM. The reconciler compares the current desired<br />state hash against `lastRendered.hash` to detect drift without<br />querying the LiteLLM API on every reconcile. |  |  |


#### A2ALastRenderedStatus



A2ALastRenderedStatus records the post-substitution rendered A2AAgent
state last successfully applied to LiteLLM (Phase 5 D-03).

Per Phase 5 D-02, `AgentID` is pinned across reconciles — the
reconciler resolves the LiteLLM-assigned agent_id once (via POST
/v1/agents response or, on the stale-status fallback path, via
`GET /v1/agents?health_check=false` + in-memory name filter) then
reads from status thereafter. Diverges from spec §6.6 (silent on
persistence); documented in spec/DEFECTS-1.82.6.md row
`DEF-§6.4/§6.6-ID-PERSIST`.

`PUT /v1/agents/{agent_id}` IS wholesale-replace per Phase 1 Probe 7
(verified on 1.82.6; not impacted by the Prisma defect that gated
MCP's PUT semantics). `AgentCardKeys` is therefore informational
only — no shrinkage delete-and-recreate path is committed for
A2AAgent.



_Appears in:_
- [A2AAgentStatus](#a2aagentstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `hash` _string_ | Hash is the SHA-256 hex of the RFC 8785–canonicalized merged<br />post-substitution body (spec.params merged with spec.agentCard<br />and structural overlays \{agent_name, agent_card_params,<br />agent_card_params.url\}). An empty hash indicates the A2AAgent<br />has not yet been successfully reconciled. |  |  |
| `paramsKeys` _string array_ | ParamsKeys is the sorted list of top-level keys present in<br />spec.params at the time of the last successful render. Phase 5<br />D-04 informational field — not load-bearing for shrinkage<br />detection (Probe 7 ✓ — PUT IS wholesale-replace on A2A). |  |  |
| `agentCardKeys` _string array_ | AgentCardKeys is the sorted list of top-level keys present in<br />spec.agentCard at the time of the last successful render.<br />Phase 5 D-04 informational field — not load-bearing for<br />shrinkage detection (Probe 7 ✓ — PUT IS wholesale-replace on<br />A2A). |  |  |
| `agentID` _string_ | AgentID is the LiteLLM-assigned UUID (agent_id) for this A2A<br />agent entry. Pinned per Phase 5 D-02 so the reconciler can call<br />`DELETE /v1/agents/<agent_id>` directly on the finalizer path<br />without re-resolving by name. On first reconcile, resolved from<br />the POST /v1/agents response body's `agent_id` field.<br />Diverges from spec §6.6: documented in<br />spec/DEFECTS-1.82.6.md row `DEF-§6.4/§6.6-ID-PERSIST`. |  |  |
| `at` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | At is the timestamp of the last SUCCESSFUL render (NOT every<br />reconcile attempt — transient failures do not update this field). |  |  |


#### BudgetSpec



BudgetSpec is the optional sub-block on TeamSpec that carries the LiteLLM
team budget (per spec §6.7). It MUST be modeled as a pointer at the
TeamSpec level (`Budget *BudgetSpec`) so that whole-block absence is
distinguishable from `Budget{}` — when the user omits `spec.budget`
entirely, the reconciler emits `max_budget: null` AND
`budget_duration: null` on the `POST /team/update` body (§6.7 "Clearing
budget" — relies on the wholesale-replace semantic of POST /team/update
per §5.1, Q10). Both LiteLLM fields are `anyOf: [<type>, null]` in the
1.82.6 OpenAPI, so the null form is the canonical "no budget set"
signal.

Float64 precision is adopted for v1alpha1 per spec §6.7; a string-encoded
resource.Quantity-style form is deferred unless penny-precision-at-scale
becomes a concern.



_Appears in:_
- [TeamSpec](#teamspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `limit` _float_ | Limit is the USD budget cap for the LiteLLM team, projected<br />verbatim onto `max_budget` (number). Modeled as `*float64` so the<br />reconciler can distinguish "user set 0.0" from "user omitted the<br />field" — the former projects to `max_budget: 0.0`, the latter to<br />`max_budget: null` (per spec §6.7 "Clearing budget"). |  |  |
| `period` _string_ | Period is the budget reset interval as a duration string, projected<br />verbatim onto `budget_duration` (string). CEL admission rejects any<br />value that does not match `^[0-9]+[smhd]$` (seconds \| minutes \|<br />hours \| days — per spec §6.7). MinLength=1 is NOT applied<br />independently: the regex pattern + `omitempty` together cover<br />absence (no pattern check fires when the field is absent), and a<br />present-but-empty value (`""`) is rejected by the regex itself. |  | Pattern: `^[0-9]+[smhd]$` <br /> |


#### FailedCandidate



FailedCandidate records a candidate whose K8s apiserver write
(Server-Side Apply patch) failed for a non-collision reason. The
Reason enum is intentionally single-valued in _FINALv3: Discovery
never calls LiteLLM, so LiteLLMRejected / LiteLLMUnavailable are NOT
Discovery-level reasons (MDISC-26 / D-10). See CONTEXT.md
<specifics> line 284 and PATTERNS.md line 1037.



_Appears in:_
- [ModelDiscoveryStatus](#modeldiscoverystatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the normalized candidate name (post-prefix +<br />normalization, the value that would have become the child Model's<br />metadata.name). |  | MinLength: 1 <br />Required: \{\} <br /> |
| `reason` _string_ | Reason classifies the failure. Single-valued enum per _FINALv3<br />(MDISC-26): only ChildCRWriteFailed is valid; LiteLLMRejected and<br />LiteLLMUnavailable have been retired from Discovery-level reasons<br />(MDISC-27 — Discovery never calls LiteLLM; those reasons surface<br />on the child Model's status instead). |  | Enum: [ChildCRWriteFailed] <br />Required: \{\} <br /> |
| `message` _string_ | Message is a free-form diagnostic. Per §9.1, MUST NOT contain<br />secret material — AWS error strings are sanitized via the<br />reconciler's sanitizeAWSError helper before surfacing here. |  |  |


#### LastRenderedStatus



LastRenderedStatus records the post-substitution rendered state that was
last successfully applied to LiteLLM. This is the operator-side drift
source of truth (D-01): the reconciler compares the current desired hash
against Hash to decide whether a LiteLLM mutation is needed.

D-02 (delete-and-recreate pivot): ParamsKeys and InfoKeys are required for
per-bag shrinkage detection. If any key is removed from either bag
(persistedKeys \ desiredKeys is non-empty), the reconciler deletes the
existing LiteLLM entry and re-creates it, then re-pins LiteLLMModelID to
the freshly-assigned UUID. See spec/DEFECTS-1.82.6.md row 2.

D-04: LiteLLMModelID diverges from spec §6.2 ("operator does not persist
the assigned model_info.id"). Persistence is a pragmatic win for a low-rate
operator — saves a GET /model/info per reconcile. Documented deviation.



_Appears in:_
- [ModelStatus](#modelstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `hash` _string_ | Hash is the SHA-256 hex of the RFC 8785–canonicalized merged<br />post-substitution body (spec.params merged with spec.info, after<br />\{\{NAME\}\} substitution, before the model_info.id overlay). The hash<br />deliberately excludes model_info.id so that CREATE vs UPDATE reconciles<br />do not oscillate (D-01). An empty hash indicates the Model has not yet<br />been successfully reconciled. |  |  |
| `paramsKeys` _string array_ | ParamsKeys is the sorted list of dotted-path keys present in spec.params<br />at the time of the last successful render (D-02). The reconciler diffs<br />this against the current spec.params keyset: if any key is absent in the<br />desired state (a shrinkage), the full delete-and-recreate path is taken<br />instead of POST /model/update (D-02, see spec/DEFECTS-1.82.6.md row 2). |  |  |
| `infoKeys` _string array_ | InfoKeys is the sorted list of dotted-path keys present in spec.info at<br />the time of the last successful render (D-02). Same shrinkage-detection<br />semantics as ParamsKeys — any key removal in EITHER bag triggers<br />delete-and-recreate. |  |  |
| `litellmModelID` _string_ | LiteLLMModelID is the LiteLLM-assigned UUID (model_info.id) for this<br />Model entry. Persisted here so the reconciler can reference it directly<br />on subsequent reconciles without an extra GET /model/info call (D-04).<br />IMPORTANT (D-02 consequence): on every delete-and-recreate cycle, LiteLLM<br />assigns a fresh UUID. The reconciler MUST re-pin this field to the new<br />UUID inside the same reconcile that performed the delete+recreate, before<br />returning success, to avoid a stale-ID 404 on the next reconcile.<br />Diverges from spec §6.2: documented in spec/DEFECTS-1.82.6.md row 7 (D-04). |  |  |
| `at` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | At is the timestamp of the last SUCCESSFUL render (NOT every reconcile<br />attempt — transient failures do not update this field). Reconcile<br />attempt frequency is observable via controller-runtime workqueue metrics<br />and cr_status_age_seconds (§10 / OBS-03). |  |  |


#### LiteLLMA2AAgent



LiteLLMA2AAgent is the Schema for the litellma2aagents API per spec §6.6 (`_FINALv3`).

A2A-01.06: a LiteLLMA2AAgent CR registers one A2A agent entry against
LiteLLM via the admin-immediate path (`POST /v1/agents`).

Finalizer (spec §7.5): `a2aagents.litellm.ackstorm.ai/finalizer` —
issues `DELETE /v1/agents/<agent_id>` before the CR is removed from
etcd. The finalizer constant lives in
internal/controller/a2aagent_controller.go.



_Appears in:_
- [LiteLLMA2AAgentList](#litellma2aagentlist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `litellm.ackstorm.ai/v1alpha1` | | |
| `kind` _string_ | `LiteLLMA2AAgent` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[A2AAgentSpec](#a2aagentspec)_ |  |  |  |
| `status` _[A2AAgentStatus](#a2aagentstatus)_ |  |  |  |


#### LiteLLMA2AAgentList



LiteLLMA2AAgentList contains a list of LiteLLMA2AAgent.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `litellm.ackstorm.ai/v1alpha1` | | |
| `kind` _string_ | `LiteLLMA2AAgentList` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[LiteLLMA2AAgent](#litellma2aagent) array_ |  |  |  |


#### LiteLLMConnection



LiteLLMConnection is the Schema for the litellmconnections API.

CONN-02: the CEL XValidation rule above (placed on the resource root,
not on Spec) rejects any LiteLLMConnection with metadata.name != "default"
at admission time — the operator never sees the CR. This is the v1alpha1
singleton-by-name enforcement strategy (no admission webhook needed).



_Appears in:_
- [LiteLLMConnectionList](#litellmconnectionlist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `litellm.ackstorm.ai/v1alpha1` | | |
| `kind` _string_ | `LiteLLMConnection` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[LiteLLMConnectionSpec](#litellmconnectionspec)_ |  |  |  |
| `status` _[LiteLLMConnectionStatus](#litellmconnectionstatus)_ |  |  |  |


#### LiteLLMConnectionList



LiteLLMConnectionList contains a list of LiteLLMConnection.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `litellm.ackstorm.ai/v1alpha1` | | |
| `kind` _string_ | `LiteLLMConnectionList` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[LiteLLMConnection](#litellmconnection) array_ |  |  |  |


#### LiteLLMConnectionSpec



LiteLLMConnectionSpec defines the desired state of LiteLLMConnection.

CONN-01: a user can declare a LiteLLMConnection with spec.endpoint and
spec.masterKeySecretRef{name, key}, where both `name` and `key` are
required and non-empty. Field-level CEL constraints are enforced by
the +kubebuilder:validation:Required + MinLength=1 markers below.



_Appears in:_
- [LiteLLMConnection](#litellmconnection)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `endpoint` _string_ | Endpoint is the base URL of the LiteLLM instance the operator<br />will probe and mutate against. Example:<br />"http://litellm.default.svc.cluster.local:4000".<br />The endpoint is used for both the periodic GET /models probe<br />(CONN-03, see internal/litellm/keyinfo.go for the §6.1 deviation<br />note) and every Phase 3+ domain mutation call. The value is<br />trimmed of any trailing slash by litellm.NewClient at the wire<br />layer; users may include or omit the trailing slash without<br />observable effect. |  | MinLength: 1 <br />Required: \{\} <br /> |
| `masterKeySecretRef` _[SecretKeyRef](#secretkeyref)_ | MasterKeySecretRef points to the Kubernetes Secret that carries<br />the LiteLLM master key (sk-.). Both `name` and `key` are<br />required per CONN-01; the SecretKeyRef type enforces non-empty<br />values via its own MinLength=1 markers. The Secret MUST live in<br />the same namespace as the LiteLLMConnection CR (no cross-namespace<br />resolution in v1alpha1).<br />The reconciler reads the Secret with the operator's ServiceAccount<br />at probe time; missing Secret or missing key surfaces as<br />Ready=False, reason=SecretNotFound (§6.0 reason set). |  | Required: \{\} <br /> |
| `mcpToolPrefixSeparator` _string_ | MCPToolPrefixSeparator mirrors LiteLLM's `MCP_TOOL_PREFIX_SEPARATOR`<br />environment variable on the target LiteLLM instance. LiteLLM uses<br />this character as the delimiter between server prefix and tool name<br />in fully-qualified MCP tool identifiers, and it REJECTS the same<br />character inside `server_name` at `POST /v1/mcp/server` time (HTTP<br />400 "Server name cannot contain '<sep>'."). The opposite member of<br />the \{".", "-"\} pair is allowed inside `server_name`.<br />The operator reads this field to sanitize the LiteLLM-side<br />`server_name` and `alias` for every MCPServer routed through this<br />Connection. The K8s `metadata.name` is left untouched — sanitization<br />is wire-boundary only.<br />Valid values:<br />  - "-" (default; matches LiteLLM's own default). Forbids '-' in<br />    server_name; the operator rewrites '-' → '.' in the wire payload.<br />  - "." Forbids '.' in server_name (the ackstorm prod config since<br />    the MCPServerDiscovery dotted-child template requires '-' inside<br />    server_name). The operator rewrites '.' → '-' in the wire payload.<br />FIX.txt HIGH-1 (2026-05-22): without this field, dotted Discovery<br />children failed at the LiteLLM-side validator on default deploys. | - | Enum: [- .] <br /> |


#### LiteLLMConnectionStatus



LiteLLMConnectionStatus defines the observed state of LiteLLMConnection.

The status surface is intentionally minimal per Claude's Discretion
default in 02-CONTEXT.md: only `observedGeneration` and `conditions`
are surfaced. Diagnostic fields (lastProbeAt, probeCount, echoed
endpoint) are deferred — envtests cover probe outcomes
via the Ready condition itself, no extra fields needed.



_Appears in:_
- [LiteLLMConnection](#litellmconnection)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the metadata.generation of the CR the<br />reconciler most recently processed. Surfaced here even though<br />OWN-08 itself lands in Phase 3 — the reconciler<br />populates this so Phase 3's contract is in place from day one. |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#condition-v1-meta) array_ | Conditions carries the standard metav1.Condition list. The single<br />type defined for LiteLLMConnection is `Ready`, with reason values<br />drawn from the §6.0 reason set:<br />- Synced — probe succeeded; cache is fresh.<br />- Connecting — entry state, no probe outcome yet.<br />- Unreachable — transient probe failure (5xx, network).<br />- BadMasterKey — 401 from the LiteLLM master-key probe.<br />- SecretNotFound — masterKeySecretRef Secret or key missing.<br />Phase 3+ dependents read this condition via the cache snapshot<br />(internal/connection.Cache), never via direct CR Get. |  |  |


#### LiteLLMMCPServer



LiteLLMMCPServer is the Schema for the litellmmcpservers API per spec §6.4 (`_FINALv3`).

MCP-01: a LiteLLMMCPServer CR registers one MCP server entry against LiteLLM
via the admin-immediate path (`POST /v1/mcp/server`). User-authored and
LiteLLMMCPServerDiscovery-generated CRs are reconciled identically — the
reconciler does NOT branch on ownerRef state (AC-MS1).

Finalizer (spec §7.5): `mcpservers.litellm.ackstorm.ai/finalizer` —
issues `DELETE /v1/mcp/server/<server_id>` before the CR is removed
from etcd. The finalizer constant lives in
internal/controller/mcpserver_controller.go (Phase 5 PATTERNS.md L129).



_Appears in:_
- [LiteLLMMCPServerList](#litellmmcpserverlist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `litellm.ackstorm.ai/v1alpha1` | | |
| `kind` _string_ | `LiteLLMMCPServer` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[MCPServerSpec](#mcpserverspec)_ |  |  |  |
| `status` _[MCPServerStatus](#mcpserverstatus)_ |  |  |  |


#### LiteLLMMCPServerDiscovery



LiteLLMMCPServerDiscovery is the Schema for the litellmmcpserverdiscoveries API — the
second Pipeline B CRD in the operator (after LiteLLMModelDiscovery). It points
the operator at the cluster's ToolHive deployment and reconciles
discovered ToolHive `MCPServer` / `VirtualMCPServer` objects into a
fan-out of Kubernetes LiteLLMMCPServer child CRs in WATCH_NAMESPACE.

Discovery NEVER calls LiteLLM directly; each child reconciles into
LiteLLM via the Phase 5 LiteLLMMCPServer controller (Pipeline A).

The two CR-level XValidation rules above enforce:
- Defensive Type == 'toolhive' (Enum already enforces this; the CEL
rule documents intent and survives any future Enum expansion).
- MSDISC-05: refresh.interval >= 1m floor.

Per Phase 5 D-04 (and MSDISC-04 specifically): MCPServerDiscovery has
NO upstream-credential reference field. ToolHive reads are authorized
via the operator's cluster-scoped ServiceAccount RBAC (Phase 5 D-07,
config/rbac/toolhive_clusterrole.yaml). This is a SCHEMA-LEVEL
prohibition — the field is structurally absent.

The Discovery finalizer is `mcpserverdiscoveries.litellm.ackstorm.ai/
finalizer`. It issues NO LiteLLM call — its only work is waiting for
owned children to drain via blockOwnerDeletion=true cascade, then
removing itself. Each child MCPServer's own finalizer issues
`DELETE /v1/mcp/server/<server_id>` on the LiteLLM side (Phase 5 plan
05-01 mcpserver_controller.go).



_Appears in:_
- [LiteLLMMCPServerDiscoveryList](#litellmmcpserverdiscoverylist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `litellm.ackstorm.ai/v1alpha1` | | |
| `kind` _string_ | `LiteLLMMCPServerDiscovery` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[MCPServerDiscoverySpec](#mcpserverdiscoveryspec)_ |  |  |  |
| `status` _[MCPServerDiscoveryStatus](#mcpserverdiscoverystatus)_ |  |  |  |


#### LiteLLMMCPServerDiscoveryList



LiteLLMMCPServerDiscoveryList contains a list of LiteLLMMCPServerDiscovery.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `litellm.ackstorm.ai/v1alpha1` | | |
| `kind` _string_ | `LiteLLMMCPServerDiscoveryList` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[LiteLLMMCPServerDiscovery](#litellmmcpserverdiscovery) array_ |  |  |  |


#### LiteLLMMCPServerList



LiteLLMMCPServerList contains a list of LiteLLMMCPServer.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `litellm.ackstorm.ai/v1alpha1` | | |
| `kind` _string_ | `LiteLLMMCPServerList` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[LiteLLMMCPServer](#litellmmcpserver) array_ |  |  |  |


#### LiteLLMModel



LiteLLMModel is the Schema for the litellmmodels API.

MODEL-01: a LiteLLMModel CR registers one LiteLLM model entry (a litellm_params +
model_info pair) against the LiteLLM instance referenced by
LiteLLMConnection/default. User-authored and Discovery-generated CRs are
treated identically by the reconciler (OWN-04 silent-overwrite on
first-reconcile name collision).

Shape: flat _FINALv3 — no spec.type discriminator, no nested litellm
sub-object. spec.params and spec.info are pass-through bags. spec.secrets[]
provides {{NAME}} substitution (§5.2). See ModelSpec for field details.

LiteLLMModel names are chosen freely by the user (MODEL-01 — not a singleton).
There is NO CEL singleton-by-name rule on this resource. The spec §7.5
finalizer name is "models.litellm.ackstorm.ai/finalizer".



_Appears in:_
- [LiteLLMModelList](#litellmmodellist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `litellm.ackstorm.ai/v1alpha1` | | |
| `kind` _string_ | `LiteLLMModel` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[ModelSpec](#modelspec)_ |  |  |  |
| `status` _[ModelStatus](#modelstatus)_ |  |  |  |


#### LiteLLMModelDiscovery



LiteLLMModelDiscovery is the Schema for the litellmmodeldiscoveries API — the
first Pipeline B CRD (spec §3.3 / §7.1, _FINALv3 two-pipeline model).
A LiteLLMModelDiscovery CR points the operator at one upstream provider
(anthropic, bedrock, gemini, kubeai, or openai) and reconciles
discovered IDs into a fan-out of Kubernetes LiteLLMModel child CRs in
WATCH_NAMESPACE. Discovery NEVER calls LiteLLM directly; each child
reconciles into LiteLLM via the Phase 3 LiteLLMModel controller.

The six CR-level XValidation rules above enforce the per-type
required/forbidden field matrix from spec §6.3 (provider table) plus
the MDISC-05 refresh-interval 1-minute floor. SEC-03 list-uniqueness
for spec.secrets[].as is deferred to the child LiteLLMModel's runtime check
(same Kubernetes 1.31 CEL-library limitation documented in
model_types.go:87-93 — reuse the same runtime check from).

MDISC-22 — required Secret keys per provider are FIXED per spec §6.3:

	anthropic: ANTHROPIC_API_KEY
	bedrock: AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY (AWS_SESSION_TOKEN optional)
	gemini: GEMINI_API_KEY (or GOOGLE_API_KEY per provider docs)
	openai: OPENAI_API_KEY
	kubeai: n/a (no credentialsSecretRef)

The reconciler validates required keys at credential-resolution time
and surfaces SecretNotFound if any required key is missing.

The Discovery finalizer is "modeldiscoveries.litellm.ackstorm.ai/
finalizer" (mirrors Phase 3's models.litellm.ackstorm.ai/finalizer).
It issues NO LiteLLM call — its only work is waiting for owned
children to drain via blockOwnerDeletion=true cascade, then removing
itself. Each child LiteLLMModel's own finalizer issues POST /model/delete on
the LiteLLM side (Phase 3 model_controller.go).



_Appears in:_
- [LiteLLMModelDiscoveryList](#litellmmodeldiscoverylist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `litellm.ackstorm.ai/v1alpha1` | | |
| `kind` _string_ | `LiteLLMModelDiscovery` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[ModelDiscoverySpec](#modeldiscoveryspec)_ |  |  |  |
| `status` _[ModelDiscoveryStatus](#modeldiscoverystatus)_ |  |  |  |


#### LiteLLMModelDiscoveryList



LiteLLMModelDiscoveryList contains a list of LiteLLMModelDiscovery.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `litellm.ackstorm.ai/v1alpha1` | | |
| `kind` _string_ | `LiteLLMModelDiscoveryList` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[LiteLLMModelDiscovery](#litellmmodeldiscovery) array_ |  |  |  |


#### LiteLLMModelList



LiteLLMModelList contains a list of LiteLLMModel.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `litellm.ackstorm.ai/v1alpha1` | | |
| `kind` _string_ | `LiteLLMModelList` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[LiteLLMModel](#litellmmodel) array_ |  |  |  |


#### LiteLLMTeam



LiteLLMTeam is the Schema for the litellmteams API per spec §6.7 (`_FINALv3`).

TEAM-01.04 acceptance criteria + AC-T1 (projection assertions) + AC-T6
(`Team/default` carve-out behavior — implemented in reconciler,
NOT in this type).

The operator uses the bare `metadata.name` as the LiteLLM `team_alias`
for every Team, including the reserved `default`. There is no
team-name prefix and no overlay metadata. Spec §6.7 explicitly allows
a user-authored `Team/default` override (ownership transition handled by
the reconciler); a CEL singleton-by-name rule would block AC-T2 and is
NOT applied to this resource.

Default-team carve-out:
- If no `Team/default` CR exists, the operator bootstraps the LiteLLM
`team_alias=default` with no budget via a synthetic reconcile on
manager start (after the cached `LiteLLMConnection/default` first
reaches `Ready=True`) and on each 30-min safety re-list (§7.4, §7.6).
- If a `Team/default` CR exists, the operator reconciles it normally
(ownership transition: re-uses the LiteLLM team, does NOT recreate).
- Deletion of `Team/default` is suppressed: the operator re-applies
the implicit empty spec to the LiteLLM team `default`, then removes
the finalizer. `POST /team/delete` is NEVER called for the alias
`default`.

Finalizer (spec §7.5): `teams.litellm.ackstorm.ai/finalizer` — issues
`POST /team/delete` with body `{"team_ids": [<lastRendered.teamID>]}`
before the CR is removed from etcd. The default-team
carve-out short-circuits the LiteLLM call.



_Appears in:_
- [LiteLLMTeamList](#litellmteamlist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `litellm.ackstorm.ai/v1alpha1` | | |
| `kind` _string_ | `LiteLLMTeam` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[TeamSpec](#teamspec)_ |  |  |  |
| `status` _[TeamStatus](#teamstatus)_ |  |  |  |


#### LiteLLMTeamList



LiteLLMTeamList contains a list of LiteLLMTeam.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `litellm.ackstorm.ai/v1alpha1` | | |
| `kind` _string_ | `LiteLLMTeamList` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[LiteLLMTeam](#litellmteam) array_ |  |  |  |


#### MCPServerDiscoveryFilters



MCPServerDiscoveryFilters carries the RE2 include/exclude pattern lists
applied to the POST-DERIVATION dotted three-part name (per spec §6.5).
The filter target is the DOTTED name, NOT the bare ToolHive object
name; this is the most common source of user confusion at runtime and
Will land a regression test exercising the dotted form.

RE2 compile validity is a RUNTIME concern (CEL has no regex-compile
primitive). codes the compile + classification — invalid
patterns surface as Ready=False, reason=InvalidConfig with a message
naming the offending pattern.



_Appears in:_
- [MCPServerDiscoverySpec](#mcpserverdiscoveryspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `include` _string array_ | Include narrows the candidate set: a candidate dotted name is<br />admitted only if it matches at least one pattern in Include. Empty<br />(or absent) Include means "admit all". If Include is non-empty<br />and matches ZERO candidates, the reconcile surfaces Ready=False,<br />reason=UpstreamInvalid (operator-intent vs upstream-reality drift). |  |  |
| `exclude` _string array_ | Exclude removes candidates from the post-Include set: a candidate<br />is filtered out if it matches any pattern in Exclude. Empty (or<br />absent) Exclude means "exclude nothing". Exclude is forward-looking<br />defense — zero matches is fine (lenient semantics per spec §6.5). |  |  |


#### MCPServerDiscoveryRefresh



MCPServerDiscoveryRefresh controls the periodic refresh cadence. The
reconciler returns ctrl.Result{RequeueAfter: Interval} on every
successful refresh. The 1-minute floor is enforced at the resource
root via a +kubebuilder:validation:XValidation CEL rule (MSDISC-05):
duration(self.spec.refresh.interval) >= duration('1m').



_Appears in:_
- [MCPServerDiscoverySpec](#mcpserverdiscoveryspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `interval` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#duration-v1-meta)_ | Interval is the cadence between two successive ToolHive-list<br />reconciles. metav1.Duration accepts kubectl-friendly strings like<br />"5m", "1h", "30m". CEL floor of 1m is enforced at admission. |  | Required: \{\} <br /> |


#### MCPServerDiscoverySpec



MCPServerDiscoverySpec defines the desired state of MCPServerDiscovery —
the flat `_FINALv3` shape (spec §6.5). One MCPServerDiscovery CR points
the operator at the cluster's ToolHive deployment (the only allowed
`spec.type` value in v1alpha1) and generates a fan-out of Kubernetes
MCPServer child CRs (Pipeline B per spec §3.3).

Discovery NEVER calls LiteLLM directly; each generated child reconciles
into LiteLLM via the Phase 5 MCPServer controller (Pipeline A). This
mirrors the Phase 4 ModelDiscovery → Model relationship.

MSDISC-01 narrows spec.type to {toolhive} via the Enum marker. MSDISC-04
(no upstream-credential reference field — schema-level prohibition) is
structurally enforced by the *absence* of any such field. MSDISC-05
(refresh.interval 1m floor) is enforced via CEL XValidation on the
resource root. MSDISC-14 (toolhive sub-block presence + minItems=1 on
namespaces) is enforced at the schema level.



_Appears in:_
- [LiteLLMMCPServerDiscovery](#litellmmcpserverdiscovery)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _string_ | Type discriminates the upstream Discovery source. In v1alpha1 the<br />ONLY allowed value is `toolhive`. The Enum marker below admits<br />nothing else; future Discovery sources (e.g. an A2A-side directory,<br />a Kubernetes-API-server-side scan) would expand this Enum without<br />breaking the existing schema (additive Enum values are non-breaking).<br />MSDISC-01 enforces this at admission. |  | Enum: [toolhive] <br />Required: \{\} <br /> |
| `toolhive` _[MCPServerDiscoveryToolhive](#mcpserverdiscoverytoolhive)_ | Toolhive carries the toolhive-specific configuration sub-block.<br />Required because in v1alpha1 `spec.type` is always `toolhive`; a<br />future discriminator-aware schema (when other Discovery sources<br />land) will gate this with a CEL XValidation rule similar to<br />ModelDiscovery's per-provider matrix. |  | Required: \{\} <br /> |
| `params` _[RawExtension](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#rawextension-runtime-pkg)_ | Params is a pass-through bag of fields PROPAGATED VERBATIM into<br />every generated child MCPServer's spec.params (AC-SEC4-PROPAGATE).<br />Discovery does NOT perform substitution itself; the Phase 5 MCPServer<br />reconciler substitutes them on the child's own reconcile (§5.2).<br />Any JSON object is accepted (x-kubernetes-preserve-unknown-fields:<br />true). String-typed leaves may carry \{\{NAME\}\} placeholders matched<br />against spec.secrets[] on the child's reconcile. |  |  |
| `secrets` _[SecretSubstitution](#secretsubstitution) array_ | Secrets is the substitution map PROPAGATED verbatim into every<br />generated child MCPServer's spec.secrets[] (AC-SEC4-PROPAGATE).<br />Discovery does NOT perform substitution itself — the propagated<br />entries ride along and the Phase 5 MCPServer reconciler substitutes<br />them on the child's own reconcile.<br />MSDisc has NO field for upstream-source credentials — ToolHive<br />reads are authorized via the operator's cluster-scoped<br />ServiceAccount RBAC (config/rbac/toolhive_clusterrole.yaml) per<br />Phase 5 D-07. MSDISC-04 makes the schema-level prohibition<br />load-bearing — credentials for the upstream source and credentials<br />for inference MUST be expressed via DIFFERENT mechanisms (cluster<br />RBAC vs spec.secrets[]). |  |  |
| `filters` _[MCPServerDiscoveryFilters](#mcpserverdiscoveryfilters)_ | Filters narrows the post-derivation candidate set via RE2<br />include/exclude patterns matched against the POST-DERIVATION<br />dotted three-part name (`<discovery-name>.<toolhive-namespace>.<br /><toolhive-name>`) per spec §6.5. Empty (absent) Filters means<br />"no filtering" (every discovered ToolHive object becomes a<br />candidate).<br />Per spec §6.5: include FIRST (strict — empty match-set surfaces<br />as Ready=False, reason=UpstreamInvalid), then exclude (lenient —<br />empty match-set is fine). The reconciler in enforces<br />the order; this CRD type only carries the patterns. |  |  |
| `refresh` _[MCPServerDiscoveryRefresh](#mcpserverdiscoveryrefresh)_ | Refresh controls the periodic ToolHive-list cadence. The<br />reconciler returns ctrl.Result\{RequeueAfter: spec.refresh.interval\}<br />on success (Phase 4 D-08 inherited). The CEL floor of 1 minute<br />(MSDISC-05) is enforced at the resource root via the |  | Required: \{\} <br /> |


#### MCPServerDiscoveryStatus



MCPServerDiscoveryStatus defines the observed state of MCPServerDiscovery —
the `_FINALv3` Pipeline B status surface. Mirrors ModelDiscoveryStatus
with MCP-side renames (MCPServerSkippedCandidate / MCPServerFailedCandidate)
to avoid Go name collision with the Phase 4 ModelDiscovery types in the
same package.

Spec §6.5 invariant:

	discoveredCount == generatedCount

(Filtered-out names are NOT counted in discoveredCount.)

Two condition types are written on every reconcile (each idempotent
via apimeta.SetStatusCondition):

	Ready — top-level readiness with reasons:
	 {Synced, SourceUnreachable, SecretNotFound,
	 InvalidConfig, UpstreamInvalid}
	 NOTE: LiteLLMUnavailable / LiteLLMRejected are
	 NOT Discovery-level reasons (Discovery never
	 calls LiteLLM — MSDISC-16). Those reasons surface
	 on the child MCPServer's status.

	SourceReachable — ToolHive-list reachability with reasons:
	 {Ok, Unreachable}
	 Used as the gate for vanish-detection (Phase 4
	 D-09 inherited): diff-and-delete is skipped when
	 this is False.



_Appears in:_
- [LiteLLMMCPServerDiscovery](#litellmmcpserverdiscovery)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the metadata.generation of the<br />MCPServerDiscovery the reconciler most recently processed<br />successfully (OWN-08 carry-forward). |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#condition-v1-meta) array_ | Conditions carries the metav1.Condition list. Two condition types<br />are populated on every reconcile:<br />Ready — reasons: Synced, SourceUnreachable,<br />SecretNotFound, InvalidConfig, UpstreamInvalid.<br />LiteLLMUnavailable / LiteLLMRejected are NOT<br />valid here (MSDISC-16).<br />SourceReachable — reasons: Ok, Unreachable.<br />The atomic-refresh-snapshot vanish guard<br />(Phase 4 D-09 inherited) gates diff-and-delete<br />on SourceReachable=True. |  |  |
| `discoveredCount` _integer_ | DiscoveredCount is the size of the post-filter candidate set on<br />the most recent successful refresh (filtered-out IDs are NOT<br />counted). Maintains the invariant noted on<br />MCPServerDiscoveryStatus's godoc above. | 0 |  |
| `generatedCount` _integer_ | GeneratedCount is len(GeneratedChildren) — the number of child<br />MCPServer CRs the Discovery currently owns (SSA-applied with<br />ownerReferences[controller=true, blockOwnerDeletion=true] +<br />labels[litellm.ackstorm.ai/generated-by=<this>]). | 0 |  |
| `generatedChildren` _string array_ | GeneratedChildren lists the metadata.name of every owned child<br />MCPServer CR (sorted for deterministic kubectl get -o yaml<br />output). On the next reconcile, the reconciler uses a label-<br />selector (litellm.ackstorm.ai/generated-by=<this>) for ACTUAL<br />ownership enumeration; this list is a status echo for human<br />inspection, not the source of truth. |  |  |
| `skippedCandidates` _[MCPServerSkippedCandidate](#mcpserverskippedcandidate) array_ | SkippedCandidates records candidates that were NOT generated as<br />child MCPServers because of K8s-native conflict resolution OR<br />because of a Discovery-side validation skip. Each entry names<br />the candidate and the reason.<br />Reason enum per spec §6.5 + Phase 5 D-10 (exhaustive):<br />ExplicitMCPServerExists — a child with the same name already<br />exists and its controller ownerRef<br />does NOT point at this Discovery.<br />DuplicateDiscovery — a child with the same name exists and<br />is owned by a DIFFERENT Discovery<br />(structurally rare under dotted naming).<br />OwnedBy names the winner.<br />EndpointUnknown — the ToolHive object has empty/absent<br />status.url (MSDISC-12).<br />InvalidTransport — the ToolHive object's status.transport<br />value is not in the normalization map<br />(`streamable-http → http`, `sse → sse`,<br />absent → `http`). Anything else<br />(e.g. `stdio`, `unknown`, custom) is<br />skipped. Added per Phase 5 D-10. |  |  |
| `failedCandidates` _[MCPServerFailedCandidate](#mcpserverfailedcandidate) array_ | FailedCandidates records candidates whose SSA write to the K8s<br />apiserver failed for a reason other than name collision. Each<br />entry names the candidate and the reason.<br />Reason enum (single-valued under `_FINALv3`):<br />ChildCRWriteFailed — the K8s apiserver rejected the SSA patch<br />(server timeout, rate-limit, service<br />unavailable, SSA field conflict, etc.). |  |  |
| `lastRefreshAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | LastRefreshAt is the timestamp of the most recent SUCCESSFUL<br />ToolHive-list reconcile (NOT every reconcile attempt — transient<br />failures do not update this field). Mirrors Phase 4<br />ModelDiscoveryStatus.LastRefreshAt semantics. |  |  |


#### MCPServerDiscoveryToolhive



MCPServerDiscoveryToolhive carries the toolhive-source-specific
configuration. Per Phase 5 D-06: the ToolHive API group is
`toolhive.stacklok.dev/v1beta1` (NOT the autoconfig-divergent
v1alpha1); the informer is cluster-scoped per kind (D-07);
`Namespaces[]` is an in-memory filter on event handlers — no
informer reconfig on live namespace-list change.



_Appears in:_
- [MCPServerDiscoverySpec](#mcpserverdiscoveryspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `namespaces` _string array_ | Namespaces enumerates the Kubernetes namespaces from which<br />ToolHive `MCPServer` / `VirtualMCPServer` objects should be<br />considered for discovery. ToolHive objects in OTHER namespaces<br />are silently ignored.<br />MUST contain at least one entry (MinItems=1). Each entry MUST be<br />a non-empty DNS-1123-friendly string (MinLength=1); CRD-layer<br />validation does NOT enforce DNS-1123 format because the cluster's<br />own namespace creation already enforces that contract. |  | MinItems: 1 <br />Required: \{\} <br /> |
| `kinds` _string array_ | Kinds enumerates the ToolHive resource kinds to watch. Defaults<br />to both `MCPServer` and `VirtualMCPServer` (per spec §6.5). A user<br />who wants Discovery limited to one kind (e.g. only `MCPServer`)<br />specifies a singleton list.<br />Empty list is REJECTED at admission via the Enum constraint on<br />each item — but the default ensures the omitted-field case is<br />"watch both", not "watch nothing". | [MCPServer VirtualMCPServer] | items:Enum: [MCPServer VirtualMCPServer] <br /> |


#### MCPServerFailedCandidate



MCPServerFailedCandidate records a candidate whose K8s apiserver write
(SSA patch) failed for a non-collision reason. Single-valued enum per
`_FINALv3`: Discovery never calls LiteLLM, so LiteLLM-side reasons are
NOT valid here.

The MCPServer- prefix on the Go type avoids a name collision with the
Phase 4 ModelDiscovery `FailedCandidate` type in the same package.



_Appears in:_
- [MCPServerDiscoveryStatus](#mcpserverdiscoverystatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the candidate dotted three-part name<br />(`<discovery-name>.<toolhive-namespace>.<toolhive-name>`). |  | MinLength: 1 <br />Required: \{\} <br /> |
| `reason` _string_ | Reason classifies the failure. Single-valued enum: only<br />ChildCRWriteFailed is valid; LiteLLM-side reasons surface on<br />the child MCPServer's status instead. |  | Enum: [ChildCRWriteFailed] <br />Required: \{\} <br /> |
| `message` _string_ | Message is a free-form diagnostic. |  |  |


#### MCPServerLastRenderedStatus



MCPServerLastRenderedStatus records the post-substitution rendered MCPServer
state last successfully applied to LiteLLM (Phase 5 D-03).

Per Phase 5 D-02, `ServerID` is pinned across reconciles — the reconciler
resolves the LiteLLM-assigned server_id once (via `GET /v1/mcp/server` +
in-memory name filter on first reconcile) then reads from status
thereafter. Diverges from spec §6.4 (silent on persistence); documented
in spec/DEFECTS-1.82.6.md row `DEF-§6.4/§6.6-ID-PERSIST`.

Per Phase 5 D-01, the Probe 10c verdict on LiteLLM 1.83.10-stable is ✓
(PUT /v1/mcp/server IS wholesale-replace). `ParamsKeys` is therefore
informational only on this path — it is recorded for observability /
the create→update boundary but is NOT load-bearing for shrinkage
detection (no delete-and-recreate path is committed in the MCPServer
reconciler — see 05-CONTEXT.md D-01 "If positive" branch).



_Appears in:_
- [MCPServerStatus](#mcpserverstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `hash` _string_ | Hash is the SHA-256 hex of the RFC 8785–canonicalized merged<br />post-substitution body (spec.params merged with structural<br />overlays \{server_name, url, transport\}). `server_name` in this<br />overlay is the LiteLLM-side sanitized name per MCP-05 (the<br />`litellm.SanitizeMCPServerName` rewrite of metadata.name driven by<br />the parent Connection's spec.mcpToolPrefixSeparator). An empty<br />hash indicates the MCPServer has not yet been successfully<br />reconciled. |  |  |
| `paramsKeys` _string array_ | ParamsKeys is the sorted list of top-level keys present in<br />spec.params at the time of the last successful render. Per Phase 5<br />D-01 (✓ verdict on Probe 10c — PUT IS wholesale-replace on<br />1.83.10-stable), this field is informational only: the simple PUT<br />update path does not need per-bag shrinkage detection. The field<br />is retained for observability and forward-compat with any future<br />downgrade path. |  |  |
| `serverID` _string_ | ServerID is the LiteLLM-assigned UUID (server_id) for this MCP<br />server entry. Pinned per Phase 5 D-02 so the reconciler can call<br />`DELETE /v1/mcp/server/<server_id>` directly on the finalizer<br />path without re-resolving by name. On first reconcile, resolved<br />via `GET /v1/mcp/server` + in-memory filter on the LiteLLM-side<br />sanitized name (MCP-05; the rewrite of metadata.name driven by<br />the parent Connection's spec.mcpToolPrefixSeparator).<br />Diverges from spec §6.4: documented in<br />spec/DEFECTS-1.82.6.md row `DEF-§6.4/§6.6-ID-PERSIST`. |  |  |
| `at` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | At is the timestamp of the last SUCCESSFUL render (NOT every<br />reconcile attempt — transient failures do not update this field). |  |  |


#### MCPServerSkippedCandidate



MCPServerSkippedCandidate records a candidate that was NOT generated
as a child MCPServer due to K8s-native conflict resolution or a
Discovery-side validation skip. The Reason enum is exhaustive per spec
§6.5 + Phase 5 D-10 (which added the `InvalidTransport` value).

The MCPServer- prefix on the Go type avoids a name collision with the
Phase 4 ModelDiscovery `SkippedCandidate` type in the same package.



_Appears in:_
- [MCPServerDiscoveryStatus](#mcpserverdiscoverystatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the candidate dotted three-part name<br />(`<discovery-name>.<toolhive-namespace>.<toolhive-name>`) that<br />would have become the child MCPServer's metadata.name. |  | MinLength: 1 <br />Required: \{\} <br /> |
| `reason` _string_ | Reason classifies the skip. Exhaustive enum per spec §6.5 +<br />Phase 5 D-10:<br />ExplicitMCPServerExists — name collides with a user-authored<br />MCPServer (no controller ownerRef<br />back at this Discovery).<br />DuplicateDiscovery — name collides with a child owned by<br />a different MCPServerDiscovery.<br />OwnedBy names the winning Discovery<br />(<namespace>/<name>).<br />EndpointUnknown — ToolHive object's status.url is empty<br />or absent (MSDISC-12).<br />InvalidTransport — ToolHive object's status.transport<br />value is not in the normalization<br />map; the candidate is dropped to<br />avoid emitting an MCPServer with a<br />transport that fails CEL admission<br />on the child CR (CRD enum is<br />\{http, sse\}; ToolHive may emit<br />`streamable-http`, `sse`, `stdio`,<br />custom strings). |  | Enum: [ExplicitMCPServerExists DuplicateDiscovery EndpointUnknown InvalidTransport] <br />Required: \{\} <br /> |
| `ownedBy` _string_ | OwnedBy is the <namespace>/<name> of the MCPServerDiscovery<br />winning a DuplicateDiscovery collision (or the explicit MCPServer<br />owner for ExplicitMCPServerExists). Empty for EndpointUnknown<br />and InvalidTransport (no collision — the candidate's own data<br />was rejected). |  |  |
| `message` _string_ | Message is a free-form diagnostic. Per §9.1, MUST NOT contain<br />secret material (the operator only handles ToolHive metadata<br />fields, not secret payloads, so this is structurally easy to<br />satisfy). |  |  |


#### MCPServerSpec



MCPServerSpec defines the desired state of MCPServer per spec §6.4
(`_FINALv3` flat shape).

MCP-01: a user can declare an MCPServer CR registering one MCP server
entry against the LiteLLM instance referenced by LiteLLMConnection/default.
`spec.endpoint` (required) and `spec.transport ∈ {http, sse}` (CEL enum)
are the modeled typed fields; `spec.params` is a JSON pass-through bag
forwarded verbatim to LiteLLM's `NewMCPServerRequest`/`UpdateMCPServerRequest`
body. The operator NEVER adds operator-side defaults to the bag and never
branches its LiteLLM-write path on `metadata.ownerReferences[].kind ==
MCPServerDiscovery` — user-authored and Discovery-generated MCPServers
reconcile identically (MCP-01 / AC-MS1).

MCP-02: `spec.params` accepts arbitrary nested JSON
(x-kubernetes-preserve-unknown-fields: true). String-typed leaves may
contain `{{NAME}}` placeholders resolved from `spec.secrets[]` (§5.2,
Phase 3 D-05); non-string leaves pass through unchanged.

MCP-03: `spec.transport` is admission-validated against the enum
`{http, sse}` (spec §6.4). `stdio` and any other value are rejected at
admission — the MCPServer reconciler ships the value verbatim to LiteLLM
and does NOT translate (translation belongs to MCPServerDiscovery per
Phase 5 D-10 — `streamable-http → http`).

MCP-04: the operator updates via `PUT /v1/mcp/server` (wholesale-replace,
empirically validated by Probe 10c on 1.83.10-stable per Phase 5 plan
05-00) and deletes via `DELETE /v1/mcp/server/<id>`.

MCP-05: LiteLLM rejects its `MCP_TOOL_PREFIX_SEPARATOR` env value inside
`server_name` at `POST /v1/mcp/server` time (HTTP 400 "Server name
cannot contain '<sep>'."). The MCPServer reconciler sanitizes the
LiteLLM-side `server_name` and `alias` per the parent
LiteLLMConnection's `spec.mcpToolPrefixSeparator` (default `-`), swapping
the configured separator for the opposite valid character (`-` ↔ `.`)
via `litellm.SanitizeMCPServerName`. The K8s-side `metadata.name` is
left untouched — the dotted MCPServerDiscovery child template
(`<discovery>.<source-ns>.<source-name>`) survives unchanged, and the
divergence between K8s identity and LiteLLM identity is confined to
the wire boundary (FIX.txt HIGH-1, 2026-05-22). The canonical hash
input and the finalizer name-resolve fallback both use the sanitized
form so drift detection stays consistent with what LiteLLM stores.



_Appears in:_
- [LiteLLMMCPServer](#litellmmcpserver)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `endpoint` _string_ | Endpoint is the MCP server URL forwarded verbatim as the `url`<br />field of LiteLLM's `NewMCPServerRequest`/`UpdateMCPServerRequest`.<br />Required + non-empty per spec §6.4. The reconciler does NOT<br />validate scheme/host structure beyond MinLength=1; users are<br />responsible for supplying a well-formed URL their MCP runtime<br />accepts. |  | MinLength: 1 <br />Required: \{\} <br /> |
| `transport` _string_ | Transport is the wire transport vocabulary the MCP server speaks.<br />Admitted values: `http` and `sse` (spec §6.4). `stdio` and any<br />other value are rejected at admission by the CEL enum below.<br />The MCPServer reconciler ships this value verbatim to LiteLLM —<br />no operator-side translation. Discovery-side normalization<br />(`streamable-http → http`) is implemented in the MCPServerDiscovery<br />controller per Phase 5 D-10; explicit MCPServer CRs already arrive<br />post-normalization (the user types `http` or `sse` directly). |  | Enum: [http sse] <br />Required: \{\} <br /> |
| `params` _[RawExtension](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#rawextension-runtime-pkg)_ | Params is a pass-through bag of fields forwarded verbatim to<br />LiteLLM's `NewMCPServerRequest` / `UpdateMCPServerRequest` body.<br />Any JSON object is accepted (x-kubernetes-preserve-unknown-fields:<br />true). String-typed leaf values may contain `\{\{NAME\}\}` placeholders<br />resolved from `spec.secrets[]` before the body reaches LiteLLM<br />(§5.2, Phase 3 D-05). Non-string leaves are forwarded unchanged<br />(Phase 3 SEC-02 contract carry-forward).<br />The operator NEVER adds, defaults, or removes keys inside this<br />bag — the user's declared keyset IS the desired state. Per spec<br />§6.4, "anything outside the modeled set belongs inside `mcp_info`"<br />is the USER's contract, not the operator's: the operator does NOT<br />auto-route unknown top-level keys into `mcp_info`.<br />On each reconcile, the rendered post-substitution body is hashed<br />(SHA-256) and compared against `status.lastRendered.hash` to<br />detect drift (Phase 3 D-01). |  |  |
| `secrets` _[SecretSubstitution](#secretsubstitution) array_ | Secrets is the substitution map for resolving `\{\{NAME\}\}`<br />placeholders in `spec.params` string-typed leaves (§5.2, Phase 3<br />D-05). Each entry maps an uppercase NAME (the `as` field) to a<br />Kubernetes Secret key (`secretRef`). Placeholders in the bag are<br />replaced with the resolved plaintext value before the body is<br />forwarded to LiteLLM. Secret material NEVER appears in logs,<br />Events, or `status.conditions[].message` (§9.1, AC-S1).<br />SEC-03 uniqueness of `spec.secrets[].as` values is enforced as a<br />runtime check in the MCPServer reconciler (same pattern as Model<br />— CEL list-uniqueness was deferred to v1beta1). |  |  |


#### MCPServerStatus



MCPServerStatus defines the observed state of MCPServer per spec §6.4 +
Phase 5 D-03 (nested `lastRendered` substruct).



_Appears in:_
- [LiteLLMMCPServer](#litellmmcpserver)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the metadata.generation of the MCPServer CR<br />the reconciler most recently processed successfully. Consumers can<br />compare this against metadata.generation to detect whether the<br />current spec has been reconciled yet (Phase 3 OWN-08 carry-forward). |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#condition-v1-meta) array_ | Conditions carries the standard metav1.Condition list. The single<br />type defined for MCPServer is `Ready`, with reason values from §6.0:<br />- Synced — rendered body matches LiteLLM; no drift.<br />- LiteLLMUnavailable — LiteLLMConnection/default not Ready<br />(D-08 echo-reason from connection cache).<br />- LiteLLMRejected — LiteLLM returned a 4xx (non-401) on mutation.<br />- SecretNotFound — a spec.secrets[].secretRef is missing OR<br />a `\{\{NAME\}\}` placeholder has no matching<br />spec.secrets[].as entry.<br />- InvalidConfig — spec.params not valid JSON, or duplicate<br />spec.secrets[].as values. |  |  |
| `lastRendered` _[MCPServerLastRenderedStatus](#mcpserverlastrenderedstatus)_ | LastRendered is the operator-side drift source of truth per Phase 3<br />D-01 / D-07 (extended for MCPServer per Phase 5 D-03). It records<br />the post-substitution rendered state that was last successfully<br />applied to LiteLLM. The reconciler compares the current desired<br />state hash against `lastRendered.hash` to detect drift without<br />querying the LiteLLM API on every reconcile. |  |  |


#### ModelDiscoveryFilters



ModelDiscoveryFilters carries the RE2 include/exclude pattern lists
applied to the raw provider-returned candidate IDs. Empty (absent)
means "no filtering"; an empty slice on either side is identical to
absent. Per spec §6.3, both lists use RE2 syntax and are
anchored-from-start (the autoconfig matches_any semantics).

RE2 compile validity is a RUNTIME concern (CEL has no regex-compile
primitive). codes the compile + classification — invalid
patterns surface as Ready=False, reason=InvalidConfig with a message
naming the offending pattern.



_Appears in:_
- [ModelDiscoverySpec](#modeldiscoveryspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `include` _string array_ | Include narrows the candidate set: a candidate ID is admitted only<br />if it matches at least one pattern in Include. Empty (or absent)<br />Include means "admit all" (no narrowing). If Include is non-empty<br />and matches ZERO provider IDs, the reconcile surfaces Ready=False,<br />reason=UpstreamInvalid (operator-intent vs upstream-reality drift). |  |  |
| `exclude` _string array_ | Exclude removes candidates from the post-Include set: a candidate<br />is filtered out if it matches any pattern in Exclude. Empty (or<br />absent) Exclude means "exclude nothing". Exclude is forward-looking<br />defense — zero matches is fine (lenient semantics per spec §6.3). |  |  |


#### ModelDiscoveryRefresh



ModelDiscoveryRefresh controls the periodic refresh cadence. The
reconciler returns ctrl.Result{RequeueAfter: Interval} on every
successful refresh (D-08). The 1-minute floor is enforced at the
resource root via a +kubebuilder:validation:XValidation CEL rule
(MDISC-05): duration(self.spec.refresh.interval) >= duration('1m').



_Appears in:_
- [ModelDiscoverySpec](#modeldiscoveryspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `interval` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#duration-v1-meta)_ | Interval is the cadence between two successive provider-list<br />reconciles. metav1.Duration accepts kubectl-friendly strings like<br />"5m", "1h", "30m". CEL floor of 1m is enforced at admission. |  | Required: \{\} <br /> |


#### ModelDiscoverySpec



ModelDiscoverySpec defines the desired state of ModelDiscovery — the
flat _FINALv3 shape (spec §6.3). One ModelDiscovery CR points the
operator at a single upstream provider (anthropic, bedrock, gemini,
kubeai, or openai) and generates a fan-out of Kubernetes Model child
CRs (Pipeline B per spec §3.3). Discovery NEVER calls LiteLLM directly;
each generated child reconciles into LiteLLM via the Phase 3 Model
controller (Pipeline A).

Provider field matrix (CR-level CEL XValidation, see markers on the
ModelDiscovery struct below) per spec §6.3 provider table:

	anthropic — requires credentialsSecretRef; forbids region, baseUrl.
	bedrock — requires region; forbids baseUrl; credentialsSecretRef optional.
	gemini — requires credentialsSecretRef; forbids region, baseUrl.
	kubeai — requires baseUrl; forbids credentialsSecretRef, region.
	openai — requires credentialsSecretRef; baseUrl optional; forbids region.

MDISC-01 enforces spec.type ∈ {anthropic, bedrock, gemini, kubeai, openai}
at admission via the +kubebuilder:validation:Enum marker. MDISC-04
(prefix), MDISC-05 (refresh.interval floor), MDISC-15 (credential
surface), and MDISC-22/23 (propagation bags) are all schema-side.



_Appears in:_
- [LiteLLMModelDiscovery](#litellmmodeldiscovery)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _string_ | Type discriminates the upstream provider. Enforced at admission via<br />the +kubebuilder:validation:Enum marker (MDISC-01). The reconciler<br />dispatches to internal/providers/<type>.go via the registry; per-type<br />branching outside the registry is prohibited (CONTEXT.md D-01). |  | Enum: [anthropic bedrock gemini kubeai openai] <br />Required: \{\} <br /> |
| `prefix` _string_ | Prefix is the lowercase DNS-1123 segment prepended to each<br />discovered ID when generating the child Model's metadata.name<br />(final shape: <prefix>.<normalized-id>). The prefix is OPTIONAL at<br />the CRD layer; the reconciler defaults it to lowercased(spec.type)<br />at reconcile time (MDISC-04). The default is NOT a CRD-layer default<br />— keeping the schema thin lets the reconciler own the substitution<br />(matches spec §6.3 prefix semantics line 689-878).<br />Pattern is the DNS-1123 subdomain segment: lowercase alphanumerics<br />with internal hyphens and optional dotted sub-segments. MaxLength=63<br />matches the K8s DNS label budget; the generated child name's full<br />length is validated again at reconcile time against DNS-1123<br />subdomain (253 chars) — see normalization step. |  | MaxLength: 63 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$` <br /> |
| `credentialsSecretRef` _[SecretObjectRef](#secretobjectref)_ | CredentialsSecretRef points to the Kubernetes Secret carrying the<br />upstream provider's API credentials. Required for anthropic, gemini,<br />openai; required-or-default-chain for bedrock; FORBIDDEN for kubeai<br />(the kubeai provider runs in-cluster without auth — see CONTEXT.md<br /><specifics> line 278). The Secret MUST reside in the same namespace<br />as the ModelDiscovery CR (no cross-namespace resolution in v1alpha1).<br />Required Secret keys per provider (spec §6.3 line 721-737 normative):<br />anthropic: ANTHROPIC_API_KEY<br />bedrock: AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY (AWS_SESSION_TOKEN optional)<br />gemini: GEMINI_API_KEY (or GOOGLE_API_KEY)<br />openai: OPENAI_API_KEY<br />kubeai: n/a (no credentialsSecretRef)<br />MDISC-15 is non-negotiable: the credential material is operator-side<br />ONLY. The reconciler MUST NOT propagate any value from this Secret<br />into the generated child Model's spec.params / spec.info /<br />spec.secrets[]. Inference-time credentials flow via spec.secrets[]<br />(the propagation bag), NOT via this field.<br />Discovery uses NEW SecretObjectRef (single Name field) — NOT the<br />SecretKeyRef\{Name, Key\} used by LiteLLMConnection and Model. The<br />Secret keys are fixed per provider per the normative table above,<br />so the user does not pick a per-key lookup; only the Secret's name<br />is parameterized. |  |  |
| `region` _string_ | Region is the AWS region for Bedrock control-plane discovery<br />(required when spec.type=bedrock, forbidden otherwise — see the<br />CR-level CEL rule on the ModelDiscovery struct). One region per<br />CR per PROJECT.md Key Decision; multi-region requires multiple CRs<br />with distinct spec.prefix (e.g. bedrock-use1, bedrock-euw1).<br />The value is overlaid as aws_region_name in each generated child<br />Model's spec.params (the operator's only typed-field overlay per<br />CONTEXT.md D-07 line 79-85). Plain string — AWS region codes are<br />open-ended and not enumerated here; CEL gates presence per provider. |  |  |
| `baseUrl` _string_ | BaseURL is the upstream provider's base endpoint. Required for<br />kubeai (e.g. "http://kubeai.kubeai.svc/openai/v1"); optional for<br />openai (default OpenAI-platform endpoint applies on omit); forbidden<br />for anthropic, bedrock, gemini.<br />Discovery calls <BaseURL>/models (OpenAI-compatible wire shape) for<br />kubeai + openai variants. For OpenAI-compatible providers (vLLM,<br />Together, Groq, OpenRouter) the user sets spec.type=openai and<br />spec.baseUrl=<provider URL>; the per-request Bearer key comes from<br />spec.credentialsSecretRef. No URL pattern is enforced at the CRD<br />layer; CEL only gates presence/absence per provider type. |  |  |
| `params` _[RawExtension](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#rawextension-runtime-pkg)_ | Params is a pass-through bag of fields propagated VERBATIM into<br />every generated child Model's spec.params (MDISC-23). On top of this<br />bag, the Discovery reconciler overlays two typed fields per child:<br />- model: "<litellm-provider>/<raw-id>" (e.g. "anthropic/claude-3-5-sonnet-20241022")<br />- aws_region_name: <spec.region> (bedrock only)<br />All other keys are forwarded unchanged. \{\{NAME\}\} substitution<br />happens on the child Model's own reconcile (§5.2 propagation rule<br />per AC-SEC4-PROPAGATE), NOT on Discovery's reconcile.<br />Any JSON object is accepted (x-kubernetes-preserve-unknown-fields:<br />true). String-typed leaves may carry \{\{NAME\}\} placeholders matched<br />against spec.secrets[] on the child's reconcile. |  |  |
| `info` _[RawExtension](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#rawextension-runtime-pkg)_ | Info is a pass-through bag of fields propagated VERBATIM into every<br />generated child Model's spec.info (MDISC-23). The Discovery<br />reconciler does NOT overlay any field here — the child's own<br />reconciler handles the model_info.id overlay (D-04 in Phase 3).<br />Any JSON object is accepted (x-kubernetes-preserve-unknown-fields:<br />true). Same \{\{NAME\}\} substitution semantics as Params apply on the<br />child's reconcile. |  |  |
| `secrets` _[SecretSubstitution](#secretsubstitution) array_ | Secrets is the substitution map PROPAGATED verbatim into every<br />generated child Model's spec.secrets[] (MDISC-23). Discovery does<br />NOT perform substitution itself — the propagated entries ride along<br />and the Phase 3 Model reconciler substitutes them on the child's<br />own reconcile (AC-SEC4-PROPAGATE).<br />MDISC-15 enforces a STRICT boundary with spec.credentialsSecretRef:<br />credentials used to call the upstream provider's discovery endpoint<br />are NEVER reused for inference. Users who need an inference-time<br />secret declare it here independently of credentialsSecretRef. A<br />post-render canary asserts no credentialsSecretRef material appears<br />in any generated child's rendered fields (CONTEXT.md anti-pattern<br />line 1021).<br />SEC-03 uniqueness of spec.secrets[].as values is enforced as a<br />runtime check on the child Model's reconcile;<br />the CEL admission alternative is deferred per the same v1alpha1<br />limitation documented in model_types.go:87-93. |  |  |
| `filters` _[ModelDiscoveryFilters](#modeldiscoveryfilters)_ | Filters narrows the post-provider-list candidate set via RE2<br />include/exclude patterns matched against the raw provider-returned<br />ID. Empty (absent) Filters means "no filtering" (all provider IDs<br />become candidates). Per spec §6.3: include FIRST (strict — empty<br />match-set surfaces as Ready=False, reason=UpstreamInvalid), then<br />exclude (lenient — empty match-set is fine).<br />Filter-order divergence from autoconfig is intentional: autoconfig<br />applies exclude first then include (src/generator.py:324); the spec<br />mandates include first then exclude. **Spec wins** — see<br />CONTEXT.md D-11 line 118 and PATTERNS.md anti-pattern line 1031.<br />Codes this and ships a regression test exercising the<br />order with overlapping patterns. |  |  |
| `refresh` _[ModelDiscoveryRefresh](#modeldiscoveryrefresh)_ | Refresh controls the periodic provider-list cadence. The<br />reconciler returns ctrl.Result\{RequeueAfter: spec.refresh.interval\}<br />on success (D-08); transient errors short-circuit through the<br />controller-runtime workqueue exponential backoff (REL-02 pattern).<br />The CEL floor of 1 minute (MDISC-05) is enforced at the resource<br />root, see the +kubebuilder:validation:XValidation marker on<br />ModelDiscovery. |  | Required: \{\} <br /> |


#### ModelDiscoveryStatus



ModelDiscoveryStatus defines the observed state of ModelDiscovery —
the _FINALv3 status surface (MDISC-26, renamed from _FINALv2's
"registeredNames[]" vocabulary to "generatedChildren[]" to reflect
the Pipeline B K8s-child-CR-emission model).

Spec §6.3 invariant:

	discoveredCount == generatedCount

(Filtered-out names are NOT counted in discoveredCount.)

Two condition types are written on every reconcile (each idempotent
via apimeta.SetStatusCondition):

	Ready — top-level readiness with reasons:
	 {Synced, SourceUnreachable, AuthFailed,
	 SecretNotFound, InvalidConfig, UpstreamInvalid}
	 NOTE: LiteLLMUnavailable and LiteLLMRejected are
	 NOT Discovery-level reasons (MDISC-27 — Discovery
	 never calls LiteLLM). Those reasons surface on the
	 child Model's status.

	SourceReachable — provider-list reachability with reasons:
	 {Ok, Unreachable, AuthFailed}
	 Used as the gate for vanish-detection (D-09): the
	 diff-and-delete step is skipped when this is False.



_Appears in:_
- [LiteLLMModelDiscovery](#litellmmodeldiscovery)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the metadata.generation of the ModelDiscovery<br />the reconciler most recently processed successfully (OWN-08). |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#condition-v1-meta) array_ | Conditions carries the metav1.Condition list. Two condition types<br />are populated on every reconcile:<br />Ready — reasons: Synced, SourceUnreachable, AuthFailed,<br />SecretNotFound, InvalidConfig, UpstreamInvalid.<br />LiteLLMUnavailable and LiteLLMRejected are<br />NOT valid here (MDISC-27 + spec §6.0).<br />SourceReachable — reasons: Ok, Unreachable, AuthFailed.<br />The atomic-refresh-snapshot vanish guard<br />(D-09) gates diff-and-delete on<br />SourceReachable=True. |  |  |
| `discoveredCount` _integer_ | DiscoveredCount is the size of the post-filter candidate set on<br />the most recent successful refresh (filtered-out IDs are NOT<br />counted). Maintains the invariant noted on ModelDiscoveryStatus's<br />godoc above. | 0 |  |
| `generatedCount` _integer_ | GeneratedCount is len(GeneratedChildren) — the number of child<br />Model CRs the Discovery currently owns (SSA-applied with<br />ownerReferences[controller=true, blockOwnerDeletion=true] +<br />labels[litellm.ackstorm.ai/generated-by=<this>]). | 0 |  |
| `generatedChildren` _string array_ | GeneratedChildren lists the metadata.name of every owned child<br />Model CR (sorted for deterministic kubectl get -o yaml output).<br />On the next reconcile, the reconciler uses a label-selector<br />(litellm.ackstorm.ai/generated-by=<this>) for ACTUAL ownership<br />enumeration; this list is a status echo for human inspection, not<br />the source of truth. |  |  |
| `skippedCandidates` _[SkippedCandidate](#skippedcandidate) array_ | SkippedCandidates records candidates that were NOT generated as<br />child Models because of K8s-native conflict resolution. Each entry<br />names the candidate and the reason.<br />Reason enum (spec §6.3 line 870 normative — exhaustive):<br />ExplicitModelExists — a child with the same name already exists<br />and its controller ownerRef does NOT point<br />at this Discovery (MDISC-14).<br />DuplicateDiscovery — a child with the same name exists and is<br />owned by a DIFFERENT ModelDiscovery<br />(MDISC-13). OwnedBy names the winner.<br />InvalidDiscoveredName — the candidate's normalized name failed<br />DNS-1123 subdomain validation (MDISC-11). |  |  |
| `failedCandidates` _[FailedCandidate](#failedcandidate) array_ | FailedCandidates records candidates whose SSA write to the K8s<br />apiserver failed for a reason other than name collision. Each<br />entry names the candidate and the reason.<br />Reason enum (spec §6.3 + MDISC-26 _FINALv3 narrowing):<br />ChildCRWriteFailed — the K8s apiserver rejected the SSA patch<br />(server timeout, rate-limit, service<br />unavailable, SSA field conflict, etc.).<br />_FINALv3 narrowed this enum to a SINGLE value (MDISC-26 / D-10):<br />LiteLLMRejected and LiteLLMUnavailable are NOT valid here because<br />Discovery never calls LiteLLM (MDISC-27). Those reasons surface on<br />the child Model's status instead. |  |  |
| `lastRefreshAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | LastRefreshAt is the timestamp of the most recent SUCCESSFUL<br />provider-list reconcile (NOT every reconcile attempt — transient<br />failures do not update this field). Mirrors Phase 3's<br />LastRendered.At pattern (model_types.go:237-243). |  |  |


#### ModelSpec



ModelSpec defines the desired state of Model.

MODEL-01: a user can declare a Model CR with spec.params (pass-through bag
forwarded to LiteLLM litellm_params) and spec.info (pass-through bag
forwarded to LiteLLM model_info). Both bags are x-kubernetes-preserve-unknown-
fields: true (any JSON is accepted). spec.secrets[] maps Kubernetes Secrets
into {{NAME}} placeholders inside the bags (§5.2 substitution).

The shape is flat per _FINALv3: no spec.type discriminator and no
nested litellm sub-object. The operator's only typed-field overlay is
model_info.id (null on create, resolved remote ID on update — D-04).

MODEL-02: spec.secrets[].as values are validated at admission via the CEL
XValidation rule on this struct; uniqueness within a Model is also
enforced (SEC-03).

MODEL-05: spec.params and spec.info pass-through bags carry
x-kubernetes-preserve-unknown-fields: true so any future litellm_params /
model_info fields are accepted without a CRD schema change.



_Appears in:_
- [LiteLLMModel](#litellmmodel)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `params` _[RawExtension](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#rawextension-runtime-pkg)_ | Params is a pass-through bag of fields forwarded verbatim to<br />LiteLLM's litellm_params on POST /model/new and POST /model/update.<br />Any JSON object is accepted (x-kubernetes-preserve-unknown-fields: true).<br />String-typed leaf values may contain \{\{NAME\}\} placeholders that are<br />resolved from spec.secrets[] before the body reaches LiteLLM (§5.2,<br />D-05). Non-string leaves are forwarded unchanged (SEC-02).<br />The operator NEVER adds or removes keys inside this bag; the user's<br />declared keyset is the desired state. On each reconcile, the rendered<br />post-substitution body is hashed (SHA-256) and compared against<br />status.lastRendered.hash to detect drift (D-01). |  |  |
| `info` _[RawExtension](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#rawextension-runtime-pkg)_ | Info is a pass-through bag of fields forwarded verbatim to<br />LiteLLM's model_info on POST /model/new and POST /model/update,<br />except that the operator overlays model_info.id (null on CREATE,<br />resolved LiteLLM UUID on UPDATE — D-04). Any JSON object is accepted<br />(x-kubernetes-preserve-unknown-fields: true). Same \{\{NAME\}\} substitution<br />rules as spec.params apply (§5.2, D-05).<br />If the user supplies spec.info.id, the operator's overlay always wins<br />and a type=Warning, reason=ProjectionOverride Event is emitted per<br />spec §5.1 (Identity tier — operator-set field). |  |  |
| `secrets` _[SecretSubstitution](#secretsubstitution) array_ | Secrets is the substitution map for resolving \{\{NAME\}\} placeholders in<br />spec.params and spec.info string-typed leaves (§5.2, D-05, SEC-03).<br />Each entry maps an uppercase NAME (the as field) to a Kubernetes Secret<br />key (secretRef). Placeholders in the bags are replaced with the resolved<br />plaintext value before the body is forwarded to LiteLLM. Secret material<br />NEVER appears in logs, Events, or status conditions (§9.1, AC-S1).<br />CEL constraints: each as value must match ^[A-Z_][A-Z0-9_]*$ and all as<br />values within a Model must be unique (SEC-03). Violations are rejected<br />at admission — no reconcile is triggered.<br />NOTE: SEC-03 uniqueness of spec.secrets[].as values is enforced as a<br />runtime check in the Model reconciler (see<br />internal/controller/model_controller.go Step 3.5). The CEL XValidation<br />list-uniqueness expression was not expressible in the Kubernetes 1.31<br />CRD CEL environment (toSet and map-then-unique patterns require a<br />newer CEL library version). The admission-time CEL alternative is<br />deferred to v1beta1 when a higher Kubernetes floor can be assumed. |  |  |


#### ModelStatus



ModelStatus defines the observed state of Model.

The status surface per D-07 (CONTEXT.md): a nested lastRendered struct
carries the operator-side drift source of truth (D-01) alongside the
standard observedGeneration + conditions fields. The flat D-07 structure
is the rejected alternative (Q3.1 Option B); nested is preferred for
kubectl get -o yaml readability.

OWN-08: observedGeneration is populated on every successful reconcile so
that consumers can detect whether the reconciler has processed the latest
spec version.



_Appears in:_
- [LiteLLMModel](#litellmmodel)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the metadata.generation of the Model CR the<br />reconciler most recently processed successfully. Consumers can compare<br />this against metadata.generation to detect whether the current spec<br />has been reconciled yet (OWN-08). |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#condition-v1-meta) array_ | Conditions carries the standard metav1.Condition list. The single type<br />defined for Model is `Ready`, with reason values from §6.0:<br />- Synced — rendered body matches LiteLLM; no drift.<br />- LiteLLMUnavailable — LiteLLMConnection/default not Ready (CONN-06).<br />- LiteLLMRejected — LiteLLM returned a 4xx/5xx on mutation.<br />- SecretNotFound — a spec.secrets[].secretRef is missing (SEC-06).<br />- UnresolvedPlaceholder — a \{\{NAME\}\} in spec.params/info has no matching<br />spec.secrets[] entry (SEC-05).<br />Phase 3+ dependents read this via kubectl or by listing Model CRs; the<br />Model reconciler does not expose a cache equivalent to the connection cache. |  |  |
| `lastRendered` _[LastRenderedStatus](#lastrenderedstatus)_ | LastRendered is the operator-side drift source of truth (D-01, D-07).<br />It records the post-substitution rendered state that was last<br />successfully applied to LiteLLM. The reconciler compares the current<br />desired state hash against lastRendered.hash to detect drift without<br />querying the LiteLLM API (bypassing Probe 4a/4b encrypted-response<br />issues — see spec/DEFECTS-1.82.6.md). |  |  |


#### RateLimitsSpec



RateLimitsSpec is the optional sub-block on TeamSpec that carries the LiteLLM
team RPM/TPM rate limits (per Feature 01 §1). It MUST be modeled as a
pointer at the TeamSpec level (`RateLimits *RateLimitsSpec`) so that
whole-block absence is distinguishable from `RateLimitsSpec{}` — when the
user omits `spec.rateLimits` entirely OR sets `spec.rateLimits: {}` with no
leaves, the reconciler emits `rpm_limit: null` AND `tpm_limit: null` on the
`POST /team/update` body AND OMITS both `rpm_limit_type` and
`tpm_limit_type` keys (Feature 01 §2.1). The empty-block-equals-absent
semantic mirrors `BudgetSpec` precedent (spec §6.7) so the two parallel
sub-blocks behave identically.

The `*_type` keys are NEVER exposed as CR fields — they are hardcoded to
`best_effort_throughput` by the operator whenever the corresponding
`*_limit` is non-null (Feature 01 §1.2, §1.3, §2.1). Promoting them to
typed CR fields is deferred until LiteLLM models them on
`UpdateTeamRequest` (currently only `NewTeamRequest` carries them).

Per-leaf admission uses an OpenAPI `minimum: 0` schema constraint
(kubebuilder Minimum annotation, NOT CEL XValidation) — equivalent to
Feature 01 §1.1's `self.rpm >= 0` / `self.tpm >= 0` constraint but rendered
as a built-in OpenAPI schema constraint. The K8s API server still rejects
negative values at admission.



_Appears in:_
- [TeamSpec](#teamspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `rpm` _integer_ | RPM is the requests-per-minute cap, projected verbatim onto<br />`rpm_limit` (integer) on the POST /team/\{new,update\} body. Modeled<br />as `*int32` so the reconciler can distinguish `0` (explicit zero —<br />projects to `rpm_limit: 0` + `rpm_limit_type: "best_effort_throughput"`)<br />from omitted (user did not set the field — projection emits<br />`rpm_limit: null` and OMITS `rpm_limit_type` per Feature 01 §2.1). |  | Minimum: 0 <br /> |
| `tpm` _integer_ | TPM is the tokens-per-minute cap, projected verbatim onto<br />`tpm_limit` (integer) on the POST /team/\{new,update\} body. Modeled<br />as `*int32` so the reconciler can distinguish `0` (explicit zero —<br />projects to `tpm_limit: 0` + `tpm_limit_type: "best_effort_throughput"`)<br />from omitted (user did not set the field — projection emits<br />`tpm_limit: null` and OMITS `tpm_limit_type` per Feature 01 §2.1). |  | Minimum: 0 <br /> |


#### SecretKeyRef



SecretKeyRef identifies a key inside a Kubernetes Secret living in
the same namespace as the referring CR. The shape is intentionally
minimal — no namespace field, no optional fallback — because v1alpha1
requires both fields to be present and same-namespace per CONN-01.
Phase 3 may promote this type to a shared package if other kinds reuse
it; today it is internal to the connection types.



_Appears in:_
- [LiteLLMConnectionSpec](#litellmconnectionspec)
- [SecretSubstitution](#secretsubstitution)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the Kubernetes Secret resource in the LiteLLMConnection's<br />namespace. |  | MinLength: 1 <br />Required: \{\} <br /> |
| `key` _string_ | Key inside the referenced Secret's `data` map whose value is the<br />LiteLLM master key. |  | MinLength: 1 <br />Required: \{\} <br /> |


#### SecretObjectRef



SecretObjectRef is the v1alpha1 ModelDiscovery-only shape for
referencing a Kubernetes Secret by Name (NO Key field). The Secret
MUST reside in the same namespace as the referring CR.

Diverges intentionally from SecretKeyRef{Name, Key} (used by
LiteLLMConnection.masterKeySecretRef and Model.spec.secrets[]
SecretSubstitution.SecretRef): the keys are FIXED per provider per
spec §6.3 (e.g. ANTHROPIC_API_KEY for anthropic, AWS_ACCESS_KEY_ID
per-key lookup; only the Secret's name is parameterized. See
CONTEXT.md <canonical_refs> line 227 and PATTERNS.md line 104.



_Appears in:_
- [ModelDiscoverySpec](#modeldiscoveryspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the Kubernetes Secret resource in the ModelDiscovery's<br />namespace. Required and non-empty. |  | MinLength: 1 <br />Required: \{\} <br /> |


#### SecretSubstitution



SecretSubstitution maps a Kubernetes Secret key to an uppercase NAME that
can be referenced as {{NAME}} in spec.params and spec.info string-typed
leaves (§5.2). The as value is the NAME; the secretRef points to the
Kubernetes Secret and key that carry the plaintext value (SEC-01.SEC-03).

SEC-03: the as value is validated at admission by the CEL pattern
^[A-Z_][A-Z0-9_]*$; lowercase letters, digits-first names, and whitespace
are rejected. Uniqueness of as values within a Model is enforced by a CEL
XValidation on ModelSpec.Secrets (see above).



_Appears in:_
- [A2AAgentSpec](#a2aagentspec)
- [MCPServerDiscoverySpec](#mcpserverdiscoveryspec)
- [MCPServerSpec](#mcpserverspec)
- [ModelDiscoverySpec](#modeldiscoveryspec)
- [ModelSpec](#modelspec)
- [TeamSpec](#teamspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `as` _string_ | As is the placeholder NAME used in \{\{NAME\}\} substitution expressions<br />within spec.params and spec.info string-typed leaves. Must match the<br />pattern ^[A-Z_][A-Z0-9_]*$ (§5.2, SEC-03). Unique within the enclosing<br />Model CR (SEC-03 uniqueness — enforced via CEL on spec.secrets).<br />Example: as=ANTHROPIC_API_KEY matches the placeholder<br />"\{\{ANTHROPIC_API_KEY\}\}" anywhere in spec.params or spec.info strings. |  | MinLength: 1 <br />Pattern: `^[A-Z_][A-Z0-9_]*$` <br />Required: \{\} <br /> |
| `secretRef` _[SecretKeyRef](#secretkeyref)_ | SecretRef identifies the Kubernetes Secret and key whose plaintext<br />value replaces the \{\{As\}\} placeholder at reconcile time. The Secret<br />MUST reside in the same namespace as the Model CR (no cross-namespace<br />resolution in v1alpha1). A missing Secret or key surfaces as<br />Ready=False, reason=SecretNotFound (§6.0).<br />Reuses SecretKeyRef from litellmconnection_types.go (same package —<br />no additional type definition needed). SEC-01, SEC-06. |  | Required: \{\} <br /> |


#### SkippedCandidate



SkippedCandidate records a candidate that was NOT generated as a
child Model due to K8s-native conflict resolution (MDISC-13 / 14 / 11).
The Reason enum is exhaustive per spec §6.3 line 870.



_Appears in:_
- [ModelDiscoveryStatus](#modeldiscoverystatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the normalized candidate name (post-prefix +<br />normalization, the value that would have become the child Model's<br />metadata.name). |  | MinLength: 1 <br />Required: \{\} <br /> |
| `reason` _string_ | Reason classifies the skip. Exhaustive enum per spec §6.3:<br />ExplicitModelExists — name collides with a user-authored Model<br />(no controller ownerRef back at this<br />Discovery) — MDISC-14.<br />DuplicateDiscovery — name collides with a child owned by a<br />different ModelDiscovery — MDISC-13. The<br />OwnedBy field names the winning<br />Discovery (<namespace>/<name>).<br />InvalidDiscoveredName — normalized name failed DNS-1123 subdomain<br />validation — MDISC-11. |  | Enum: [ExplicitModelExists DuplicateDiscovery InvalidDiscoveredName] <br />Required: \{\} <br /> |
| `ownedBy` _string_ | OwnedBy is the <namespace>/<name> of the ModelDiscovery winning a<br />DuplicateDiscovery collision. Empty for ExplicitModelExists (no<br />Discovery owns the conflicting child) and InvalidDiscoveredName<br />(no collision — the candidate's own name was rejected). |  |  |
| `message` _string_ | Message is a free-form diagnostic. Per §9.1, MUST NOT contain<br />secret material (no leaked AWS keys, no Anthropic API keys, no<br />Bearer tokens — the post-render canary asserts this). |  |  |


#### TeamLastRenderedStatus



TeamLastRenderedStatus records the post-substitution rendered Team state
last successfully applied to LiteLLM. Structural analog of
`MCPServerLastRenderedStatus` (Phase 5 D-03) with `ServerID` → `TeamID`.

Per Phase 5 D-02 (extended to Team as the third member of the ID-pin
family alongside MCPServer and A2AAgent — see `spec/DEFECTS-1.82.6.md`
row `DEF-§6.4/§6.6-ID-PERSIST`), `TeamID` is pinned across reconciles:
the reconciler resolves the LiteLLM-assigned `team_id` UUID once (via
`ListTeamsByAlias` + smallest-`team_id` duplicate rule from spec §7.1)
then reads from status thereafter. The finalizer DELETE path (plan
06-04) issues `POST /team/delete` against the pinned `TeamID` directly,
without re-resolving by alias.

`Hash` and `ParamsKeys` are informational on this path — `POST /team/update`
IS wholesale-replace per spec §5.1 (Q10), so no delete-and-recreate path
is committed in the Team reconciler. The fields are retained for
observability and forward-compat (mirrors the Phase 5 D-01 rationale).



_Appears in:_
- [TeamStatus](#teamstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `hash` _string_ | Hash is the SHA-256 hex of the RFC 8785-canonicalized merged<br />post-substitution body (`spec.params` merged with the seven operator<br />overlays `\{team_alias, max_budget, budget_duration, rpm_limit,<br />tpm_limit, rpm_limit_type, tpm_limit_type\}` — the two `*_type` keys<br />are conditional-add per Feature 01 §2.1, so the hash incorporates<br />5–7 overlay keys depending on which `*_limit` leaves are non-nil).<br />An empty hash indicates the Team has not yet been successfully<br />reconciled (Phase 3 D-01, Phase 5 D-03). |  |  |
| `paramsKeys` _string array_ | ParamsKeys is the sorted list of top-level keys present in<br />`spec.params` at the time of the last successful render.<br />Informational only on this path — `POST /team/update` IS<br />wholesale-replace per spec §5.1 (Q10), so the simple update path<br />does not need per-bag shrinkage detection. The field is retained<br />for observability and forward-compat with any future downgrade<br />path. |  |  |
| `teamID` _string_ | TeamID is the LiteLLM-assigned UUID (`team_id`) for this team<br />entry. Pinned per Phase 3 D-04 + Phase 5 D-02 so the reconciler<br />can call `POST /team/delete` (with body `\{"team_ids": [.]\}`)<br />directly on the finalizer path without re-resolving by alias. On<br />first reconcile, resolved via `ListTeamsByAlias` + smallest-<br />`team_id` duplicate rule from spec §7.1.<br />Diverges from spec §6.7 (silent on persistence — the spec says<br />"the operator resolves the LiteLLM team ID by alias" on deletion;<br />pinning saves the list call). Documented in<br />`spec/DEFECTS-1.82.6.md` row `DEF-§6.4/§6.6-ID-PERSIST` (Team is<br />the third member of the ID-pin family). |  |  |
| `at` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | At is the timestamp of the last SUCCESSFUL render (NOT every<br />reconcile attempt — transient failures do not update this field). |  |  |


#### TeamSpec



TeamSpec defines the desired state of Team per spec §6.7 (`_FINALv3` shape).

TEAM-01: a user can declare a Team CR that projects a LiteLLM team alias
(taken bare-from-`metadata.name`, no `team-` prefix) plus an optional
budget. `metadata.name` IS the LiteLLM `team_alias` — there is no
`spec.alias` and no overlay-metadata indirection.

TEAM-02: `spec.budget.limit` (USD float64, pointer so absence → null
on wire) and `spec.budget.period` (duration string, CEL-validated against
`^[0-9]+[smhd]$`) project verbatim onto LiteLLM's `max_budget` and
`budget_duration` fields. Whole-block `spec.budget` absence clears BOTH
LiteLLM fields by emitting explicit nulls in the `POST /team/update` body
(§6.7 "Clearing budget" + §5.1 wholesale-replace, Q10).

TEAM-03: TeamSpec carries EXACTLY four fields — `Budget`,
`RateLimits`, `Params`, `Secrets` — and explicitly omits the
following Go-level fields that `_FINALv3` removed from earlier
scaffolds (spec changelog lines 37–38):
- any resource-allowlist field projecting to LiteLLM `models` /
`object_permission.*`: runtime resource gating is delegated to an
external system at the per-Environment level, NOT on the Team. Spec §6.7.
- any team-membership field projecting to LiteLLM
`members_with_roles`: user-to-team assignment is delegated to an
external system, not represented in GitOps. Spec §6.7 "Semantics".
- any access-control field projecting to LiteLLM `object_permission`
or per-team-member permissions: unmanaged LiteLLM Team fields per
spec §5.1 + §7.4.
- any overlay naming field — the bare `metadata.name` IS the
`team_alias`; no two-level naming indirection.

TEAM-04: `spec.params` is a JSON pass-through bag
(x-kubernetes-preserve-unknown-fields: true) merged into the LiteLLM
`POST /team/new` / `POST /team/update` body at the top level of
`NewTeamRequest`. The seven operator structural overlays
(`team_alias`, `max_budget`, `budget_duration`, `rpm_limit`,
`tpm_limit`, `rpm_limit_type`, `tpm_limit_type`) WIN over
`spec.params` per spec §5.1 + Feature 01 §2.1 (typed-field overlay
tier) — collisions emit a `reason=ProjectionOverride` Event from the
reconciler (06-02 + Phase 10). Unmanaged top-level fields
(`members_with_roles`, `models`, `object_permission`) are still
unmanaged if the user puts them inside `params`; LiteLLM accepts them on
create, the operator does not enumerate or revert them on subsequent
reconciles.

`spec.secrets[]` is the standard substitution map (§5.2, Phase 3 D-05)
shared with Model / MCPServer / A2AAgent; same `{{NAME}}` placeholder
semantics inside `params` string-typed leaves.

Phase 10 (TRL-01..TRL-07) adds `spec.rateLimits.{rpm,tpm}` — a typed
sub-block parallel to `spec.budget`, projecting onto top-level `rpm_limit`
and `tpm_limit` (with operator-hardcoded `rpm_limit_type` /
`tpm_limit_type` overlays — see Feature 01 §1.2/§1.3 for why the *_type
fields are not exposed as CR knobs). Pointer-modeled (so `0` is
distinguishable from omitted), an OpenAPI minimum-0 schema constraint
admits only non-negative values, and clearing follows the same
explicit-null contract as Budget (§6.7 + Feature 01 §2.1). The 4 new
top-level overlay keys join the existing 3 (`team_alias`, `max_budget`,
`budget_duration`) for 7 structural overlays total — worst-case 7
ProjectionOverride Warning Events per reconcile when `spec.params`
collides on all 7 keys.

Forward-reference (NOT codified in this type): implements the
`Team/default` carve-out — synthetic reconcile on manager start +
30-min safety re-list, plus deletion protection (`POST /team/delete`
suppressed when `metadata.name == "default"` — operator re-applies the
implicit empty spec instead). implements the finalizer DELETE
path for non-default teams, keyed on `status.lastRendered.teamID`.



_Appears in:_
- [LiteLLMTeam](#litellmteam)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `budget` _[BudgetSpec](#budgetspec)_ | Budget is the optional budget sub-block. Modeled as `*BudgetSpec`<br />(pointer) so the reconciler can distinguish whole-block absence<br />from an empty `BudgetSpec\{\}`. When absent, the reconciler emits<br />`max_budget: null` AND `budget_duration: null` on the<br />`POST /team/update` body (spec §6.7 "Clearing budget"). |  |  |
| `rateLimits` _[RateLimitsSpec](#ratelimitsspec)_ | RateLimits is the optional rate-limits sub-block (per Feature 01 §1,<br />parallel to `spec.budget`). Modeled as `*RateLimitsSpec` (pointer)<br />so the reconciler can distinguish whole-block absence from an empty<br />`RateLimitsSpec\{\}`. When absent (whole-block or empty-struct —<br />mirrors Budget §6.7 precedent), the reconciler emits `rpm_limit:<br />null` AND `tpm_limit: null` on the `POST /team/update` body AND<br />OMITS both `rpm_limit_type` and `tpm_limit_type` keys (Feature 01<br />§2.1). The `*_type` keys are hardcoded to `best_effort_throughput`<br />by the operator whenever the corresponding `*_limit` is non-null —<br />they are never exposed as CR fields (Feature 01 §1.2, §1.3). |  |  |
| `params` _[RawExtension](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#rawextension-runtime-pkg)_ | Params is a pass-through bag of fields forwarded verbatim to the<br />LiteLLM `POST /team/new` / `POST /team/update` body at the top<br />level of `NewTeamRequest`. Any JSON object is accepted<br />(x-kubernetes-preserve-unknown-fields: true). String-typed leaf<br />values may contain `\{\{NAME\}\}` placeholders resolved from<br />`spec.secrets[]` before the body reaches LiteLLM (§5.2, Phase 3<br />D-05). Non-string leaves are forwarded unchanged (Phase 3 SEC-02<br />carry-forward).<br />The operator NEVER adds, defaults, or removes keys inside this bag<br />— the user's declared keyset IS the desired state. The operator's<br />seven structural overlays (`team_alias`, `max_budget`,<br />`budget_duration`, `rpm_limit`, `tpm_limit`, `rpm_limit_type`,<br />`tpm_limit_type`) ALWAYS win over `spec.params` per spec §5.1 +<br />Feature 01 §2.1; if the user sets any of those keys inside `params`,<br />the reconciler emits a per-key `reason=ProjectionOverride` Event<br />after the merge (worst-case 7 events on one reconcile).<br />On each reconcile, the rendered post-substitution body is hashed<br />(SHA-256) and compared against `status.lastRendered.hash` to detect<br />drift without polling LiteLLM (Phase 3 D-01). |  |  |
| `secrets` _[SecretSubstitution](#secretsubstitution) array_ | Secrets is the substitution map for resolving `\{\{NAME\}\}`<br />placeholders in `spec.params` string-typed leaves (§5.2, Phase 3<br />D-05). Each entry maps an uppercase NAME (the `as` field) to a<br />Kubernetes Secret key (`secretRef`). Placeholders in the bag are<br />replaced with the resolved plaintext value before the body is<br />forwarded to LiteLLM. Secret material NEVER appears in logs,<br />Events, or `status.conditions[].message` (§9.1, AC-S1 — exercised<br />in envtest redaction canaries).<br />SEC-03 uniqueness of `spec.secrets[].as` values is enforced as a<br />runtime check in the Team reconciler (same pattern as Model plan<br />03-06 and MCPServer — CEL list-uniqueness was deferred<br />to v1beta1). |  |  |


#### TeamStatus



TeamStatus defines the observed state of Team per spec §6.7 +
Phase 5 D-03 (nested `lastRendered` substruct).



_Appears in:_
- [LiteLLMTeam](#litellmteam)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the metadata.generation of the Team CR the<br />reconciler most recently processed successfully. Consumers compare<br />this against `metadata.generation` to detect whether the current<br />spec has been reconciled yet (Phase 3 OWN-08 carry-forward). |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#condition-v1-meta) array_ | Conditions carries the standard metav1.Condition list. The single<br />type defined for Team is `Ready`, with reason values drawn from<br />§6.0 (spec line 521):<br />- Synced — rendered body matches LiteLLM; no drift.<br />- LiteLLMUnavailable — LiteLLMConnection/default not Ready<br />(Phase 3 D-08 echo-reason from the<br />connection cache snapshot).<br />- LiteLLMRejected — LiteLLM returned a 4xx (non-401) on<br />mutation.<br />- SecretNotFound — a `spec.secrets[].secretRef` is missing<br />OR a `\{\{NAME\}\}` placeholder has no<br />matching `spec.secrets[].as` entry. |  |  |
| `lastRendered` _[TeamLastRenderedStatus](#teamlastrenderedstatus)_ | LastRendered is the operator-side drift source of truth (Phase 3<br />D-01 / Phase 5 D-03). It records the post-substitution rendered<br />state that was last successfully applied to LiteLLM. The reconciler<br />compares the current desired-state hash against `lastRendered.hash`<br />to detect drift without querying the LiteLLM API on every reconcile. |  |  |


