// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LiteLLMConnectionSpec defines the desired state of LiteLLMConnection.
//
// CONN-01: a user can declare a LiteLLMConnection with spec.endpoint and
// spec.masterKeySecretRef{name, key}, where both `name` and `key` are
// required and non-empty. Field-level CEL constraints are enforced by
// the +kubebuilder:validation:Required + MinLength=1 markers below.
type LiteLLMConnectionSpec struct {
	// Endpoint is the base URL of the LiteLLM instance the operator
	// will probe and mutate against. Example:
	// "http://litellm.default.svc.cluster.local:4000".
	//
	// The endpoint is used for both the periodic GET /models probe
	// (CONN-03, see internal/litellm/keyinfo.go for the §6.1 deviation
	// note) and every Phase 3+ domain mutation call. The value is
	// trimmed of any trailing slash by litellm.NewClient at the wire
	// layer; users may include or omit the trailing slash without
	// observable effect.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Endpoint string `json:"endpoint"`

	// MasterKeySecretRef points to the Kubernetes Secret that carries
	// the LiteLLM master key (sk-.). Both `name` and `key` are
	// required per CONN-01; the SecretKeyRef type enforces non-empty
	// values via its own MinLength=1 markers. The Secret MUST live in
	// the same namespace as the LiteLLMConnection CR (no cross-namespace
	// resolution in v1alpha1).
	//
	// The reconciler reads the Secret with the operator's ServiceAccount
	// at probe time; missing Secret or missing key surfaces as
	// Ready=False, reason=SecretNotFound (§6.0 reason set).
	//
	// +kubebuilder:validation:Required
	MasterKeySecretRef SecretKeyRef `json:"masterKeySecretRef"`
}

// SecretKeyRef identifies a key inside a Kubernetes Secret living in
// the same namespace as the referring CR. The shape is intentionally
// minimal — no namespace field, no optional fallback — because v1alpha1
// requires both fields to be present and same-namespace per CONN-01.
// Phase 3 may promote this type to a shared package if other kinds reuse
// it; today it is internal to the connection types.
type SecretKeyRef struct {
	// Name of the Kubernetes Secret resource in the LiteLLMConnection's
	// namespace.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Key inside the referenced Secret's `data` map whose value is the
	// LiteLLM master key.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// LiteLLMConnectionStatus defines the observed state of LiteLLMConnection.
//
// The status surface is intentionally minimal per Claude's Discretion
// default in 02-CONTEXT.md: only `observedGeneration` and `conditions`
// are surfaced. Diagnostic fields (lastProbeAt, probeCount, echoed
// endpoint) are deferred — envtests cover probe outcomes
// via the Ready condition itself, no extra fields needed.
type LiteLLMConnectionStatus struct {
	// ObservedGeneration is the metadata.generation of the CR the
	// reconciler most recently processed. Surfaced here even though
	// OWN-08 itself lands in Phase 3 — the reconciler
	// populates this so Phase 3's contract is in place from day one.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions carries the standard metav1.Condition list. The single
	// type defined for LiteLLMConnection is `Ready`, with reason values
	// drawn from the §6.0 reason set:
	// - Synced — probe succeeded; cache is fresh.
	// - Connecting — entry state, no probe outcome yet.
	// - Unreachable — transient probe failure (5xx, network).
	// - BadMasterKey — 401 from the LiteLLM master-key probe.
	// - SecretNotFound — masterKeySecretRef Secret or key missing.
	// Phase 3+ dependents read this condition via the cache snapshot
	// (internal/connection.Cache), never via direct CR Get.
	//
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=llmconn
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'default'",message="LiteLLMConnection name must be 'default' (singleton per spec §6.1)"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].reason"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// LiteLLMConnection is the Schema for the litellmconnections API.
//
// CONN-02: the CEL XValidation rule above (placed on the resource root,
// not on Spec) rejects any LiteLLMConnection with metadata.name != "default"
// at admission time — the operator never sees the CR. This is the v1alpha1
// singleton-by-name enforcement strategy (no admission webhook needed).
type LiteLLMConnection struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LiteLLMConnectionSpec   `json:"spec,omitempty"`
	Status LiteLLMConnectionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LiteLLMConnectionList contains a list of LiteLLMConnection.
type LiteLLMConnectionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LiteLLMConnection `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LiteLLMConnection{}, &LiteLLMConnectionList{})
}
