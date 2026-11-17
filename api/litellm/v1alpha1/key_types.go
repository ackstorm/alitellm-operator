// SPDX-License-Identifier: MIT

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// KeySecretRef names the Secret entry the operator writes the key value to.
// Unlike SecretKeyRef it carries a namespace: the LiteLLMKey lives in the
// operator's namespace and the Secret in the consuming service's.
type KeySecretRef struct {
	// Namespace of the Secret. The operator needs get/create/delete on
	// Secrets there (Helm value keySecretNamespaces).
	//
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`

	// Name of the Secret. The operator creates it and refuses one it did
	// not create.
	//
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Key is the entry inside the Secret's data that holds the key value.
	//
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// KeySpec declares a LiteLLM virtual key with no user and no team.
type KeySpec struct {
	// Models the key may call, projected onto `models`. LiteLLM model names
	// or LiteLLMModelAlias names; not validated against LiteLLM. At least
	// one is required: a teamless key with no models can call every model.
	//
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:MinLength=1
	Models []string `json:"models"`

	// Budget caps the key's spend (max_budget, budget_duration).
	//
	// +optional
	Budget *BudgetSpec `json:"budget,omitempty"`

	// RateLimits caps the key's request and token rate (rpm_limit, tpm_limit).
	//
	// +optional
	RateLimits *RateLimitsSpec `json:"rateLimits,omitempty"`

	// SecretRef is where the key value is written. Immutable: to move the
	// Secret, delete and recreate the LiteLLMKey.
	//
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="secretRef is immutable"
	SecretRef KeySecretRef `json:"secretRef"`

	// DeletionPolicy controls finalizer behavior when the LiteLLM-side DELETE
	// cannot be confirmed. Defaults to "Orphan" per REL-06 anti-storm.
	//
	// +kubebuilder:validation:Enum=Orphan;Delete
	// +kubebuilder:default=Orphan
	// +optional
	DeletionPolicy string `json:"deletionPolicy,omitempty"`
}

// KeyLastRenderedStatus records what was last applied to LiteLLM. It never
// holds the key value.
type KeyLastRenderedStatus struct {
	// Hash is the SHA-256 of the rendered models, budget and limits.
	//
	// +optional
	Hash string `json:"hash,omitempty"`

	// KeyAlias is the key's alias in LiteLLM (metadata.name).
	//
	// +optional
	KeyAlias string `json:"keyAlias,omitempty"`

	// +optional
	At *metav1.Time `json:"at,omitempty"`
}

type KeyStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions: single type `Ready`, reasons Synced, LiteLLMUnavailable,
	// LiteLLMRejected, SecretAccessDenied, SecretNotOwned, Conflict.
	//
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// +optional
	LastRendered *KeyLastRenderedStatus `json:"lastRendered,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=llmk,categories=litellm
// +kubebuilder:printcolumn:name="SecretNamespace",type=string,JSONPath=".spec.secretRef.namespace"
// +kubebuilder:printcolumn:name="Secret",type=string,JSONPath=".spec.secretRef.name"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].reason"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// LiteLLMKey is the Schema for the litellmkeys API.
type LiteLLMKey struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KeySpec   `json:"spec,omitempty"`
	Status KeyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LiteLLMKeyList contains a list of LiteLLMKey.
type LiteLLMKeyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LiteLLMKey `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LiteLLMKey{}, &LiteLLMKeyList{})
}
