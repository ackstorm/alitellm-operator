// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// MCPServerSpec defines the desired state of MCPServer per spec §6.4
// (`_FINALv3` flat shape).
//
// MCP-01: a user can declare an MCPServer CR registering one MCP server
// entry against the LiteLLM instance referenced by LiteLLMConnection/default.
// `spec.endpoint` (required) and `spec.transport ∈ {http, sse}` (CEL enum)
// are the modeled typed fields; `spec.params` is a JSON pass-through bag
// forwarded verbatim to LiteLLM's `NewMCPServerRequest`/`UpdateMCPServerRequest`
// body. The operator NEVER adds operator-side defaults to the bag and never
// branches its LiteLLM-write path on `metadata.ownerReferences[].kind ==
// MCPServerDiscovery` — user-authored and Discovery-generated MCPServers
// reconcile identically (MCP-01 / AC-MS1).
//
// MCP-02: `spec.params` accepts arbitrary nested JSON
// (x-kubernetes-preserve-unknown-fields: true). String-typed leaves may
// contain `{{NAME}}` placeholders resolved from `spec.secrets[]` (§5.2,
// Phase 3 D-05); non-string leaves pass through unchanged.
//
// MCP-03: `spec.transport` is admission-validated against the enum
// `{http, sse}` (spec §6.4). `stdio` and any other value are rejected at
// admission — the MCPServer reconciler ships the value verbatim to LiteLLM
// and does NOT translate (translation belongs to MCPServerDiscovery per
// Phase 5 D-10 — `streamable-http → http`).
//
// MCP-04: the operator updates via `PUT /v1/mcp/server` (wholesale-replace,
// empirically validated by Probe 10c on 1.83.10-stable per Phase 5 plan
// 05-00) and deletes via `DELETE /v1/mcp/server/<id>`.
type MCPServerSpec struct {
	// Endpoint is the MCP server URL forwarded verbatim as the `url`
	// field of LiteLLM's `NewMCPServerRequest`/`UpdateMCPServerRequest`.
	// Required + non-empty per spec §6.4. The reconciler does NOT
	// validate scheme/host structure beyond MinLength=1; users are
	// responsible for supplying a well-formed URL their MCP runtime
	// accepts.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Endpoint string `json:"endpoint"`

	// Transport is the wire transport vocabulary the MCP server speaks.
	// Admitted values: `http` and `sse` (spec §6.4). `stdio` and any
	// other value are rejected at admission by the CEL enum below.
	//
	// The MCPServer reconciler ships this value verbatim to LiteLLM —
	// no operator-side translation. Discovery-side normalization
	// (`streamable-http → http`) is implemented in the MCPServerDiscovery
	// controller per Phase 5 D-10; explicit MCPServer CRs already arrive
	// post-normalization (the user types `http` or `sse` directly).
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=http;sse
	Transport string `json:"transport"`

	// Params is a pass-through bag of fields forwarded verbatim to
	// LiteLLM's `NewMCPServerRequest` / `UpdateMCPServerRequest` body.
	// Any JSON object is accepted (x-kubernetes-preserve-unknown-fields:
	// true). String-typed leaf values may contain `{{NAME}}` placeholders
	// resolved from `spec.secrets[]` before the body reaches LiteLLM
	// (§5.2, Phase 3 D-05). Non-string leaves are forwarded unchanged
	// (Phase 3 SEC-02 contract carry-forward).
	//
	// The operator NEVER adds, defaults, or removes keys inside this
	// bag — the user's declared keyset IS the desired state. Per spec
	// §6.4, "anything outside the modeled set belongs inside `mcp_info`"
	// is the USER's contract, not the operator's: the operator does NOT
	// auto-route unknown top-level keys into `mcp_info`.
	//
	// On each reconcile, the rendered post-substitution body is hashed
	// (SHA-256) and compared against `status.lastRendered.hash` to
	// detect drift (Phase 3 D-01).
	//
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Params runtime.RawExtension `json:"params,omitempty"`

	// Secrets is the substitution map for resolving `{{NAME}}`
	// placeholders in `spec.params` string-typed leaves (§5.2, Phase 3
	// D-05). Each entry maps an uppercase NAME (the `as` field) to a
	// Kubernetes Secret key (`secretRef`). Placeholders in the bag are
	// replaced with the resolved plaintext value before the body is
	// forwarded to LiteLLM. Secret material NEVER appears in logs,
	// Events, or `status.conditions[].message` (§9.1, AC-S1).
	//
	// SEC-03 uniqueness of `spec.secrets[].as` values is enforced as a
	// runtime check in the MCPServer reconciler (same pattern as Model
	// — CEL list-uniqueness was deferred to v1beta1).
	//
	// +optional
	Secrets []SecretSubstitution `json:"secrets,omitempty"`
}

// MCPServerStatus defines the observed state of MCPServer per spec §6.4 +
// Phase 5 D-03 (nested `lastRendered` substruct).
type MCPServerStatus struct {
	// ObservedGeneration is the metadata.generation of the MCPServer CR
	// the reconciler most recently processed successfully. Consumers can
	// compare this against metadata.generation to detect whether the
	// current spec has been reconciled yet (Phase 3 OWN-08 carry-forward).
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions carries the standard metav1.Condition list. The single
	// type defined for MCPServer is `Ready`, with reason values from §6.0:
	// - Synced — rendered body matches LiteLLM; no drift.
	// - LiteLLMUnavailable — LiteLLMConnection/default not Ready
	// (D-08 echo-reason from connection cache).
	// - LiteLLMRejected — LiteLLM returned a 4xx (non-401) on mutation.
	// - SecretNotFound — a spec.secrets[].secretRef is missing OR
	// a `{{NAME}}` placeholder has no matching
	// spec.secrets[].as entry.
	// - InvalidConfig — spec.params not valid JSON, or duplicate
	// spec.secrets[].as values.
	//
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// LastRendered is the operator-side drift source of truth per Phase 3
	// D-01 / D-07 (extended for MCPServer per Phase 5 D-03). It records
	// the post-substitution rendered state that was last successfully
	// applied to LiteLLM. The reconciler compares the current desired
	// state hash against `lastRendered.hash` to detect drift without
	// querying the LiteLLM API on every reconcile.
	//
	// +optional
	LastRendered MCPServerLastRenderedStatus `json:"lastRendered,omitempty"`
}

// MCPServerLastRenderedStatus records the post-substitution rendered MCPServer
// state last successfully applied to LiteLLM (Phase 5 D-03).
//
// Per Phase 5 D-02, `ServerID` is pinned across reconciles — the reconciler
// resolves the LiteLLM-assigned server_id once (via `GET /v1/mcp/server` +
// in-memory name filter on first reconcile) then reads from status
// thereafter. Diverges from spec §6.4 (silent on persistence); documented
// in spec/DEFECTS-1.82.6.md row `DEF-§6.4/§6.6-ID-PERSIST`.
//
// Per Phase 5 D-01, the Probe 10c verdict on LiteLLM 1.83.10-stable is ✓
// (PUT /v1/mcp/server IS wholesale-replace). `ParamsKeys` is therefore
// informational only on this path — it is recorded for observability /
// the create→update boundary but is NOT load-bearing for shrinkage
// detection (no delete-and-recreate path is committed in the MCPServer
// reconciler — see 05-CONTEXT.md D-01 "If positive" branch).
type MCPServerLastRenderedStatus struct {
	// Hash is the SHA-256 hex of the RFC 8785–canonicalized merged
	// post-substitution body (spec.params merged with structural
	// overlays {server_name, url, transport}). An empty hash indicates
	// the MCPServer has not yet been successfully reconciled.
	//
	// +optional
	Hash string `json:"hash,omitempty"`

	// ParamsKeys is the sorted list of top-level keys present in
	// spec.params at the time of the last successful render. Per Phase 5
	// D-01 (✓ verdict on Probe 10c — PUT IS wholesale-replace on
	// 1.83.10-stable), this field is informational only: the simple PUT
	// update path does not need per-bag shrinkage detection. The field
	// is retained for observability and forward-compat with any future
	// downgrade path.
	//
	// +optional
	ParamsKeys []string `json:"paramsKeys,omitempty"`

	// ServerID is the LiteLLM-assigned UUID (server_id) for this MCP
	// server entry. Pinned per Phase 5 D-02 so the reconciler can call
	// `DELETE /v1/mcp/server/<server_id>` directly on the finalizer
	// path without re-resolving by name. On first reconcile, resolved
	// via `GET /v1/mcp/server` + in-memory filter on metadata.name.
	//
	// Diverges from spec §6.4: documented in
	// spec/DEFECTS-1.82.6.md row `DEF-§6.4/§6.6-ID-PERSIST`.
	//
	// +optional
	ServerID string `json:"serverID,omitempty"`

	// At is the timestamp of the last SUCCESSFUL render (NOT every
	// reconcile attempt — transient failures do not update this field).
	//
	// +optional
	At *metav1.Time `json:"at,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=mcpsrv
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].reason"
// +kubebuilder:printcolumn:name="ServerID",type=string,JSONPath=".status.lastRendered.serverID"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// LiteLLMMCPServer is the Schema for the litellmmcpservers API per spec §6.4 (`_FINALv3`).
//
// MCP-01: a LiteLLMMCPServer CR registers one MCP server entry against LiteLLM
// via the admin-immediate path (`POST /v1/mcp/server`). User-authored and
// LiteLLMMCPServerDiscovery-generated CRs are reconciled identically — the
// reconciler does NOT branch on ownerRef state (AC-MS1).
//
// Finalizer (spec §7.5): `mcpservers.litellm.ackstorm.ai/finalizer` —
// issues `DELETE /v1/mcp/server/<server_id>` before the CR is removed
// from etcd. The finalizer constant lives in
// internal/controller/mcpserver_controller.go (Phase 5 PATTERNS.md L129).
type LiteLLMMCPServer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MCPServerSpec   `json:"spec,omitempty"`
	Status MCPServerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LiteLLMMCPServerList contains a list of LiteLLMMCPServer.
type LiteLLMMCPServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LiteLLMMCPServer `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LiteLLMMCPServer{}, &LiteLLMMCPServerList{})
}
