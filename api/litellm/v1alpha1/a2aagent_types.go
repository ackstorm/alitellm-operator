// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// A2AAgentSpec defines the desired state of A2AAgent per spec §6.6
// (`_FINALv3` flat shape).
//
// A2A-01.06 collectively: a user can declare an A2AAgent CR registering
// one A2A agent entry against the LiteLLM instance referenced by
// LiteLLMConnection/default. The shape is flat per `_FINALv3`: NO
// `spec.type` discriminator and NO nested `spec.litellm.*` sub-object.
// `spec.endpoint` is the modeled typed field (overlays
// `agent_card_params.url` per spec §6.6); `spec.agentCard` is a REQUIRED
// JSON pass-through bag carrying the agent's published A2A protocol card;
// `spec.params` is an OPTIONAL JSON pass-through bag forwarded verbatim
// to `AgentConfig` top-level (NOT inside `agent_card_params`, per spec
// §6.6 — diverges from MCP and is the source of the `agent_card_params`
// collision in the ProjectionOverride taxonomy).
//
// Two-pass substitution (Phase 5 D-04): on each reconcile, `spec.params`
// is walked first, then `spec.agentCard`, sharing a single resolved
// Secret map. Placeholders `{{NAME}}` in EITHER bag are resolved from
// `spec.secrets[]` before the body reaches LiteLLM. The shared map
// ensures a Secret referenced in both bags is fetched from the
// Kubernetes API exactly once per reconcile.
//
// Four-collision ProjectionOverride taxonomy (Phase 5 D-05) — fired as
// Kubernetes Events with `reason=ProjectionOverride`, `type=Warning`,
// each at most once per reconcile pass:
// - agent_name — `spec.params.agent_name` overlaid by metadata.name.
// - agent_card_params — `spec.params.agent_card_params` overlaid by spec.agentCard.
// - agent_card_params.url — `spec.agentCard.url` overlaid by spec.endpoint.
// - model_info — `spec.params.model_info` reserved by LiteLLM §6.6.
type A2AAgentSpec struct {
	// Endpoint is the A2A agent URL forwarded verbatim as the `url`
	// field inside `agent_card_params` of LiteLLM's `AgentConfig`.
	// Required + non-empty per spec §6.6. The reconciler does NOT
	// validate scheme/host structure beyond MinLength=1; users are
	// responsible for supplying a well-formed URL their A2A agent
	// runtime accepts. Collides with `spec.agentCard.url` if present
	// — the operator overlay always wins and emits a
	// `ProjectionOverride` Event keyed `agent_card_params.url`
	// (Phase 5 D-05).
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Endpoint string `json:"endpoint"`

	// AgentCard is a REQUIRED JSON pass-through bag carrying the
	// agent's A2A protocol "agent card" structure (name, description,
	// capabilities, defaultInputModes, defaultOutputModes, skills,
	// etc.). Any JSON object is accepted
	// (x-kubernetes-preserve-unknown-fields: true) — the operator
	// validates only well-formedness; A2A protocol semantics are
	// enforced by LiteLLM and downstream agent runtimes.
	//
	// String-typed leaves at ANY depth (including nested arrays like
	// `skills[].description`, `defaultInputModes[]`, `capabilities.*`
	// string leaves) may contain `{{NAME}}` placeholders resolved
	// from `spec.secrets[]` (§5.2; Phase 3 D-05; Phase 5 D-04
	// second-pass scope). Non-string leaves pass through unchanged.
	//
	// If `spec.agentCard.url` is set, the operator overlays it with
	// `spec.endpoint` and emits a `ProjectionOverride` Event keyed
	// `agent_card_params.url` (Phase 5 D-05).
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:pruning:PreserveUnknownFields
	AgentCard runtime.RawExtension `json:"agentCard"`

	// Params is an OPTIONAL JSON pass-through bag forwarded verbatim
	// to LiteLLM's `AgentConfig` body at the TOP level (NOT inside
	// `agent_card_params` — diverges from MCP and is the source of
	// the `agent_card_params` collision in the ProjectionOverride
	// taxonomy). Any JSON object is accepted
	// (x-kubernetes-preserve-unknown-fields: true). String-typed leaf
	// values may contain `{{NAME}}` placeholders resolved from
	// `spec.secrets[]` (§5.2; Phase 5 D-04 first-pass scope).
	//
	// The operator NEVER adds or removes keys inside this bag — the
	// user's declared keyset IS the desired state. Per Phase 5 D-05,
	// the operator applies structural overlays after substitution:
	// - `mergedBody.agent_name` is overlaid by metadata.name.
	// - `mergedBody.agent_card_params` is overlaid by spec.agentCard.
	//
	// Collisions surface as `ProjectionOverride` Events keyed by the
	// offending top-level key. `model_info` triggers a warning Event
	// (LiteLLM-reserved per spec §6.6) but is NOT overlaid by the
	// operator (the user's value is preserved on its way to LiteLLM;
	// LiteLLM may itself reject the body).
	//
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Params runtime.RawExtension `json:"params,omitempty"`

	// Secrets is the substitution map for resolving `{{NAME}}`
	// placeholders in `spec.params` AND `spec.agentCard` string-typed
	// leaves (Phase 5 D-04 — two-pass with shared resolvedSecrets
	// map). Each entry maps an uppercase NAME (the `as` field) to a
	// Kubernetes Secret key (`secretRef`). Placeholders in either bag
	// are replaced with the resolved plaintext value before the body
	// is forwarded to LiteLLM. Secret material NEVER appears in logs,
	// Events, or `status.conditions[].message` (§9.1, AC-S1).
	//
	// SEC-03 uniqueness of `spec.secrets[].as` values is enforced as a
	// runtime check in the A2AAgent reconciler (same pattern as Model
	// And MCPServer — CEL list-uniqueness was
	// deferred to v1beta1).
	//
	// SEC-07: if a declared `as` is unreferenced by ANY placeholder
	// across both bags (union of refs from the two-pass), the
	// reconciler emits a `UnusedSecretRef` Event (type=Normal).
	//
	// +optional
	Secrets []SecretSubstitution `json:"secrets,omitempty"`
}

// A2AAgentStatus defines the observed state of A2AAgent per spec §6.6 +
// Phase 5 D-03 (nested `lastRendered` substruct).
type A2AAgentStatus struct {
	// ObservedGeneration is the metadata.generation of the A2AAgent CR
	// the reconciler most recently processed successfully (OWN-08
	// carry-forward).
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions carries the standard metav1.Condition list. The single
	// type defined for A2AAgent is `Ready`, with reason values from §6.0:
	// - Synced — rendered body matches LiteLLM; no drift.
	// - LiteLLMUnavailable — LiteLLMConnection/default not Ready
	// (D-08 echo-reason from connection cache).
	// - LiteLLMRejected — LiteLLM returned a 4xx (non-401) on mutation.
	// - SecretNotFound — a spec.secrets[].secretRef is missing OR
	// a `{{NAME}}` placeholder has no matching
	// spec.secrets[].as entry.
	// - InvalidConfig — spec.params or spec.agentCard not valid
	// JSON, or duplicate spec.secrets[].as
	// values.
	//
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// LastRendered is the operator-side drift source of truth per Phase 3
	// D-01 / D-07 (extended for A2AAgent per Phase 5 D-03). It records
	// the post-substitution rendered state that was last successfully
	// applied to LiteLLM. The reconciler compares the current desired
	// state hash against `lastRendered.hash` to detect drift without
	// querying the LiteLLM API on every reconcile.
	//
	// +optional
	LastRendered A2ALastRenderedStatus `json:"lastRendered,omitempty"`
}

// A2ALastRenderedStatus records the post-substitution rendered A2AAgent
// state last successfully applied to LiteLLM (Phase 5 D-03).
//
// Per Phase 5 D-02, `AgentID` is pinned across reconciles — the
// reconciler resolves the LiteLLM-assigned agent_id once (via POST
// /v1/agents response or, on the stale-status fallback path, via
// `GET /v1/agents?health_check=false` + in-memory name filter) then
// reads from status thereafter. Diverges from spec §6.6 (silent on
// persistence); documented in spec/DEFECTS-1.82.6.md row
// `DEF-§6.4/§6.6-ID-PERSIST`.
//
// `PUT /v1/agents/{agent_id}` IS wholesale-replace per Phase 1 Probe 7
// (verified on 1.82.6; not impacted by the Prisma defect that gated
// MCP's PUT semantics). `AgentCardKeys` is therefore informational
// only — no shrinkage delete-and-recreate path is committed for
// A2AAgent.
type A2ALastRenderedStatus struct {
	// Hash is the SHA-256 hex of the RFC 8785–canonicalized merged
	// post-substitution body (spec.params merged with spec.agentCard
	// and structural overlays {agent_name, agent_card_params,
	// agent_card_params.url}). An empty hash indicates the A2AAgent
	// has not yet been successfully reconciled.
	//
	// +optional
	Hash string `json:"hash,omitempty"`

	// ParamsKeys is the sorted list of top-level keys present in
	// spec.params at the time of the last successful render. Phase 5
	// D-04 informational field — not load-bearing for shrinkage
	// detection (Probe 7 ✓ — PUT IS wholesale-replace on A2A).
	//
	// +optional
	ParamsKeys []string `json:"paramsKeys,omitempty"`

	// AgentCardKeys is the sorted list of top-level keys present in
	// spec.agentCard at the time of the last successful render.
	// Phase 5 D-04 informational field — not load-bearing for
	// shrinkage detection (Probe 7 ✓ — PUT IS wholesale-replace on
	// A2A).
	//
	// +optional
	AgentCardKeys []string `json:"agentCardKeys,omitempty"`

	// AgentID is the LiteLLM-assigned UUID (agent_id) for this A2A
	// agent entry. Pinned per Phase 5 D-02 so the reconciler can call
	// `DELETE /v1/agents/<agent_id>` directly on the finalizer path
	// without re-resolving by name. On first reconcile, resolved from
	// the POST /v1/agents response body's `agent_id` field.
	//
	// Diverges from spec §6.6: documented in
	// spec/DEFECTS-1.82.6.md row `DEF-§6.4/§6.6-ID-PERSIST`.
	//
	// +optional
	AgentID string `json:"agentID,omitempty"`

	// At is the timestamp of the last SUCCESSFUL render (NOT every
	// reconcile attempt — transient failures do not update this field).
	//
	// +optional
	At *metav1.Time `json:"at,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=a2a
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].reason"
// +kubebuilder:printcolumn:name="AgentID",type=string,JSONPath=".status.lastRendered.agentID"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// LiteLLMA2AAgent is the Schema for the litellma2aagents API per spec §6.6 (`_FINALv3`).
//
// A2A-01.06: a LiteLLMA2AAgent CR registers one A2A agent entry against
// LiteLLM via the admin-immediate path (`POST /v1/agents`).
//
// Finalizer (spec §7.5): `a2aagents.litellm.ackstorm.ai/finalizer` —
// issues `DELETE /v1/agents/<agent_id>` before the CR is removed from
// etcd. The finalizer constant lives in
// internal/controller/a2aagent_controller.go.
type LiteLLMA2AAgent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   A2AAgentSpec   `json:"spec,omitempty"`
	Status A2AAgentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LiteLLMA2AAgentList contains a list of LiteLLMA2AAgent.
type LiteLLMA2AAgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LiteLLMA2AAgent `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LiteLLMA2AAgent{}, &LiteLLMA2AAgentList{})
}
