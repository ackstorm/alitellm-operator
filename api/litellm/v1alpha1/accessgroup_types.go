// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// AccessGroupSpec defines the desired state of a LiteLLM access group — a
// reusable bundle of models, MCP servers, and A2A agents that a team reaches
// through LiteLLMTeam.spec.permission.accessGroups.
//
// SCOPE: this CRD owns the three RESOURCE dimensions only. It never writes
// assigned_team_ids or assigned_key_ids. Team attachment is written from the
// team side (team.access_group_ids), which is the face LiteLLM enforces on;
// keeping a single writer per surface is what lets the operator skip
// delta-repair machinery entirely.
//
// SECURITY: access groups only ADD. A group grant OVERRIDES a team's
// deny-by-default sentinel (models: ["__deny_all__"]) — verified 2026-08-06 on
// LiteLLM 1.93.0. Granting a model here makes it reachable by every team that
// attaches this group, regardless of that team's own spec.permission.models.
type AccessGroupSpec struct {
	// Description is free text forwarded to LiteLLM's `description` field.
	//
	// +optional
	Description string `json:"description,omitempty"`

	// Models is the list of LiteLLM model NAMES this group grants. Forwarded
	// verbatim to access_model_names — LiteLLM matches on model_name, so no
	// resolution step is needed and no CR reference is required.
	//
	// +optional
	Models []string `json:"models,omitempty"`

	// MCPServers is the list of MCP server NAMES this group grants. Each name
	// is resolved to a server_id via GET /v1/mcp/server before projection,
	// because access_mcp_server_ids matches on ids and silently ignores names.
	// An unresolved name parks the CR Ready=False reason=MCPServerNotFound and
	// requeues (ordering dependency with LiteLLMMCPServer CRs — it self-heals
	// once the server exists).
	//
	// +optional
	MCPServers []string `json:"mcpServers,omitempty"`

	// Agents is the list of A2A agent NAMES this group grants. Each name is
	// resolved to an agent_id via GET /v1/agents, same reason and same
	// parking behaviour as MCPServers (reason=AgentNotFound).
	//
	// +optional
	Agents []string `json:"agents,omitempty"`

	// DeletionPolicy controls finalizer behavior when the LiteLLM-side DELETE
	// cannot be confirmed. Defaults to "Orphan" per REL-06 anti-storm.
	//
	// +kubebuilder:validation:Enum=Orphan;Delete
	// +kubebuilder:default=Orphan
	// +optional
	DeletionPolicy string `json:"deletionPolicy,omitempty"`
}

// AccessGroupStatus defines the observed state of a LiteLLMAccessGroup.
type AccessGroupStatus struct {
	// ObservedGeneration is the metadata.generation most recently processed.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions carries the standard metav1.Condition list. The single type
	// is `Ready`, with reasons:
	//   - Synced             — rendered group matches LiteLLM.
	//   - LiteLLMUnavailable — LiteLLMConnection/default not usable.
	//   - LiteLLMRejected    — LiteLLM returned a 4xx (non-401) on mutation.
	//   - MCPServerNotFound  — a spec.mcpServers name does not resolve yet.
	//   - AgentNotFound      — a spec.agents name does not resolve yet.
	//   - RecreateThrottled  — created-but-not-listed storm breaker tripped.
	//
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// LastRendered is the operator-side drift source of truth.
	//
	// +optional
	LastRendered AccessGroupLastRenderedStatus `json:"lastRendered,omitempty"`
}

// AccessGroupLastRenderedStatus records the rendered state last applied.
//
// AccessGroupID is server-minted: LiteLLM 1.93.0 IGNORES a caller-supplied
// access_group_id and mints a UUID (verified). Same posture as MCP toolset_id
// and A2A agent_id, unlike team_id / MCP server_id which the operator pins to
// metadata.name. Adoption of a pre-existing group therefore goes through the
// unique `access_group_name`, which is metadata.name.
type AccessGroupLastRenderedStatus struct {
	// Hash is the SHA-256 hex of the RFC 8785–canonicalized rendered body.
	//
	// +optional
	Hash string `json:"hash,omitempty"`

	// AccessGroupID is the LiteLLM-assigned UUID, read from the POST response
	// or re-resolved by name via GET /v1/access_group.
	//
	// +optional
	AccessGroupID string `json:"accessGroupID,omitempty"`

	// At is the timestamp of the last SUCCESSFUL render.
	//
	// +optional
	At *metav1.Time `json:"at,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=llag,categories=litellm
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].reason"
// +kubebuilder:printcolumn:name="GroupID",type=string,JSONPath=".status.lastRendered.accessGroupID"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// LiteLLMAccessGroup is the Schema for the litellmaccessgroups API.
//
// metadata.name IS the LiteLLM `access_group_name` (unique server-side — a
// duplicate create returns 409), which is how the operator adopts a
// pre-existing group after a restart.
type LiteLLMAccessGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AccessGroupSpec   `json:"spec,omitempty"`
	Status AccessGroupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LiteLLMAccessGroupList contains a list of LiteLLMAccessGroup.
type LiteLLMAccessGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LiteLLMAccessGroup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LiteLLMAccessGroup{}, &LiteLLMAccessGroupList{})
}
