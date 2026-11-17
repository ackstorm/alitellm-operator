// SPDX-License-Identifier: MIT

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ModelGroupBudgetSpec declares a LiteLLM budget on a model access-group TAG
// (model_info.access_groups — NOT a LiteLLMAccessGroup). Requires LiteLLM
// 1.100.0+. LiteLLM charges a request against it only when the tag appears
// by name in the caller's key/team models; operator teams get it there via
// LiteLLMAccessGroup.spec.modelGroups (mirrored into team.models).
//
// +kubebuilder:validation:XValidation:rule="has(self.limit) || has(self.softLimit) || has(self.period)",message="set at least one of limit, softLimit, period"
type ModelGroupBudgetSpec struct {
	// ModelGroup is the model access-group tag the budget applies to.
	// Immutable: a rename would orphan the old tag's budget.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="modelGroup is immutable"
	ModelGroup string `json:"modelGroup"`

	// Limit is the USD hard cap shared by every caller granted the tag,
	// projected onto max_budget. 0 means NO cap in LiteLLM (check skipped).
	//
	// +kubebuilder:validation:Minimum=0
	// +optional
	Limit *float64 `json:"limit,omitempty"`

	// SoftLimit is the USD alert threshold (soft_budget). Requests still succeed.
	//
	// +kubebuilder:validation:Minimum=0
	// +optional
	SoftLimit *float64 `json:"softLimit,omitempty"`

	// Period is the reset interval (budget_duration), e.g. `30d`.
	//
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+[smhd]$`
	Period string `json:"period,omitempty"`

	// DeletionPolicy controls finalizer behavior when the LiteLLM-side DELETE
	// cannot be confirmed. Defaults to "Orphan" per REL-06 anti-storm.
	//
	// +kubebuilder:validation:Enum=Orphan;Delete
	// +kubebuilder:default=Orphan
	// +optional
	DeletionPolicy string `json:"deletionPolicy,omitempty"`
}

// ModelGroupBudgetStatus is deliberately minimal: the operator declares the
// budget; spend and reset time are LiteLLM's to report.
type ModelGroupBudgetStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions: single type `Ready`, reasons Synced, LiteLLMUnavailable,
	// LiteLLMRejected, ModelGroupNotFound, DuplicateModelGroup.
	//
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=llmgb,categories=litellm
// +kubebuilder:printcolumn:name="ModelGroup",type=string,JSONPath=".spec.modelGroup"
// +kubebuilder:printcolumn:name="Limit",type=number,JSONPath=".spec.limit"
// +kubebuilder:printcolumn:name="Period",type=string,JSONPath=".spec.period"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].reason"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// LiteLLMModelGroupBudget is the Schema for the litellmmodelgroupbudgets API.
type LiteLLMModelGroupBudget struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelGroupBudgetSpec   `json:"spec,omitempty"`
	Status ModelGroupBudgetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LiteLLMModelGroupBudgetList contains a list of LiteLLMModelGroupBudget.
type LiteLLMModelGroupBudgetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LiteLLMModelGroupBudget `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LiteLLMModelGroupBudget{}, &LiteLLMModelGroupBudgetList{})
}
