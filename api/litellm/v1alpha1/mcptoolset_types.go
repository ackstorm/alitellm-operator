// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MCPToolsetServerTools is one server's contribution to a toolset: the MCP
// server it draws from, and the explicit tool names taken from it.
//
// NO GLOBS, BY DESIGN. Expanding a pattern would require the operator to
// enumerate the server's tools via GET /v1/mcp/tools, which needs the MCP
// server to be live, reachable, and readable by the operator's key — often
// false in the target deployments. Tool names are declared explicitly.
type MCPToolsetServerTools struct {
	// Server is the MCP server this toolset draws tools from. Give the NAME
	// of a LiteLLMMCPServer CR in this namespace; the operator reads that
	// CR's status.lastRendered.serverID and sends the resolved id to LiteLLM.
	//
	// BEST-EFFORT, NEVER-FAILING resolution: if no such CR exists (an
	// adopted/out-of-band server, or a plain typo) the string is forwarded to
	// LiteLLM VERBATIM — which is exactly right when the user supplies a raw
	// server_id UUID. The operator performs NO validation and NEVER parks the
	// CR on an unresolvable server. LiteLLM itself accepts a nonexistent
	// server_id with 201 and simply grants nothing (verified on 1.93.0), so
	// the failure mode is an inert toolset, not an error.
	//
	// The value is forwarded WITHOUT sanitization. Do NOT apply
	// SanitizeMCPServerName here: with MCP_TOOL_PREFIX_SEPARATOR="-" it maps
	// `-` to `.` and would mangle a UUID.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Server string `json:"server"`

	// Tools is the explicit list of tool names taken from Server. Each entry
	// becomes one {server_id, tool_name} pair in the LiteLLM body.
	//
	// LiteLLM does not validate that a tool exists — a bogus name is accepted
	// and contributes nothing. An empty list contributes no pairs.
	//
	// Tool names may be bare or server-prefixed
	// (`<server><MCP_TOOL_PREFIX_SEPARATOR><tool>`); LiteLLM strips a KNOWN
	// server prefix on resolution. Note the strip only happens when the
	// server_id resolves, so a prefixed name plus an unresolvable server is
	// doubly inert.
	//
	// +optional
	Tools []string `json:"tools,omitempty"`
}

// MCPToolsetSpec defines the desired state of a LiteLLM MCP toolset — a
// named, curated collection of specific tools drawn from one or more MCP
// servers, granted to teams via LiteLLMTeam.spec.permission.mcpToolsets.
//
// The reconciler is a near-pure data transform. There is deliberately no
// validation, no tool enumeration, and no glob expansion (see
// MCPToolsetServerTools.Server / .Tools).
type MCPToolsetSpec struct {
	// Description is free text forwarded to LiteLLM's `description` field.
	//
	// +optional
	Description string `json:"description,omitempty"`

	// From is the list of per-server tool selections composing this toolset.
	// Flattened in declaration order into LiteLLM's `tools` array of
	// {server_id, tool_name} pairs. Duplicate pairs are de-duplicated (first
	// occurrence wins) so the rendered hash is stable.
	//
	// An empty/absent From renders `tools: []`, which LiteLLM accepts. The
	// resulting toolset grants NOTHING (its resolved mcp_servers set is
	// empty, and LiteLLM's server-access check is fail-CLOSED on an empty
	// list). Silent by design — LiteLLM emits no error.
	//
	// +optional
	From []MCPToolsetServerTools `json:"from,omitempty"`

	// DeletionPolicy controls finalizer behavior when the LiteLLM-side DELETE
	// cannot be confirmed. Defaults to "Orphan" per REL-06 anti-storm.
	//
	// +kubebuilder:validation:Enum=Orphan;Delete
	// +kubebuilder:default=Orphan
	// +optional
	DeletionPolicy string `json:"deletionPolicy,omitempty"`
}

// MCPToolsetStatus defines the observed state of a LiteLLMMCPToolset.
type MCPToolsetStatus struct {
	// ObservedGeneration is the metadata.generation most recently processed.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions carries the standard metav1.Condition list. The single type
	// is `Ready`, with reasons:
	//   - Synced             — rendered toolset matches LiteLLM.
	//   - LiteLLMUnavailable — LiteLLMConnection/default not usable.
	//   - LiteLLMRejected    — LiteLLM returned a 4xx (non-401) on mutation.
	//   - RecreateThrottled  — created-but-not-listed storm breaker tripped.
	//
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// LastRendered is the operator-side drift source of truth.
	//
	// +optional
	LastRendered MCPToolsetLastRenderedStatus `json:"lastRendered,omitempty"`
}

// MCPToolsetLastRenderedStatus records the rendered state last applied.
//
// ToolsetID is server-minted: LiteLLM 1.93.0 IGNORES a caller-supplied
// toolset_id and mints a UUID (verified). Same posture as A2A agent_id, and
// unlike team_id / MCP server_id which the operator pins to metadata.name.
// Adoption of a pre-existing toolset therefore goes through the unique
// `toolset_name`, which is metadata.name.
type MCPToolsetLastRenderedStatus struct {
	// Hash is the SHA-256 hex of the RFC 8785–canonicalized rendered body.
	//
	// +optional
	Hash string `json:"hash,omitempty"`

	// ToolsetID is the LiteLLM-assigned UUID, read from the POST response or
	// re-resolved by name via GET /v1/mcp/toolset.
	//
	// +optional
	ToolsetID string `json:"toolsetID,omitempty"`

	// At is the timestamp of the last SUCCESSFUL render.
	//
	// +optional
	At *metav1.Time `json:"at,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=mcpts,categories=litellm
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].reason"
// +kubebuilder:printcolumn:name="ToolsetID",type=string,JSONPath=".status.lastRendered.toolsetID"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// LiteLLMMCPToolset is the Schema for the litellmmcptoolsets API.
//
// metadata.name IS the LiteLLM `toolset_name` (unique server-side — a
// duplicate create returns 409), which is how the operator adopts a
// pre-existing toolset after a restart.
//
// Finalizer: `mcptoolsets.litellm.ackstorm.ai/finalizer` — issues
// DELETE /v1/mcp/toolset/<toolset_id> before the CR leaves etcd.
type LiteLLMMCPToolset struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MCPToolsetSpec   `json:"spec,omitempty"`
	Status MCPToolsetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LiteLLMMCPToolsetList contains a list of LiteLLMMCPToolset.
type LiteLLMMCPToolsetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LiteLLMMCPToolset `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LiteLLMMCPToolset{}, &LiteLLMMCPToolsetList{})
}
