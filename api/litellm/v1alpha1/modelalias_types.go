// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ModelAliasEntry is one (name, value) pair contributing to LiteLLM's
// router_settings.model_group_alias map.
//
//   - Name  — the map KEY (what clients send as "model", e.g. "ackstorm.smart").
//   - Value — the map VALUE (an existing LiteLLM model_name or model_group,
//     e.g. "GEMINI.gemini-3-pro-preview").
//
// The operator does NOT validate that Value resolves to a live LiteLLM
// model — LiteLLM resolves at inference time.
type ModelAliasEntry struct {
	// Name is the model_group_alias map KEY — the model name clients send to
	// LiteLLM. Must match `^[A-Za-z0-9][A-Za-z0-9._:/@+\[\]-]{0,252}$`: starts
	// with an alphanumeric, then any of letters/digits/`. _ : / @ + [ ] -`.
	// The charset mirrors real LiteLLM model identifiers — square brackets
	// (`claude-opus-4-8[1m]` context-window variants), colons
	// (`ollama/llama3:8b` tags), and at-signs (`gpt-4@2024-08-06` version
	// pins) — since Name is only ever used as a JSON key in
	// router_settings.model_group_alias, never as a k8s label/index/URL path.
	// Whitespace and control characters stay rejected. Cluster-wide
	// uniqueness across all LiteLLMModelAlias CRs is enforced at reconcile
	// time (alphabetical-last-wins on (CR namespace, CR name, array index));
	// losers report Ready=False reason=AliasConflict in
	// status.aliasStatuses[].
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._:/@+\[\]-]{0,252}$`
	Name string `json:"name"`

	// Value is the resolved LiteLLM model_name or model_group the alias
	// points to. The operator forwards it verbatim into the merged
	// router_settings.model_group_alias map.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Value string `json:"value"`
}

// LiteLLMModelAliasSpec is a collection of model_group_alias entries
// contributed by a single CR. The operator aggregates ALL LiteLLMModelAlias
// CRs cluster-wide into one merged map and writes it via
// POST /config/update, preserving unrelated router_settings keys via
// read-merge-write against GET /get/config/callbacks.
//
// MALIAS-01 — declarative declaration of router_settings.model_group_alias.
// MALIAS-02 — conflict resolution: sort by (CR namespace, CR name) ASC,
//
//	then iterate spec.aliases in declared array order; last write per
//	alias name wins. Losers across CRs surface in
//	status.aliasStatuses[].conflictsWith on the loser CR.
//
// MALIAS-03 — deletion of any LiteLLMModelAlias triggers a full
//
//	rebuild-and-rewrite via the finalizer
//	`modelaliases.litellm.ackstorm.ai/finalizer`, so no orphan entries
//	survive in LiteLLM.
type LiteLLMModelAliasSpec struct {
	// Aliases is the list of (name, value) entries this CR contributes to
	// router_settings.model_group_alias. Intra-CR duplicate names are
	// rejected at admission via CEL (see kubebuilder:validation:XValidation
	// on the parent type) — within one CR, every entry's name must be
	// unique. Cross-CR duplicates are resolved at reconcile time.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=128
	// +listType=map
	// +listMapKey=name
	Aliases []ModelAliasEntry `json:"aliases"`
}

// AliasEntryStatus is the observed state of one ModelAliasEntry.
//
//   - Applied        — true iff this entry currently holds the slot for its
//     Name in LiteLLM router_settings.model_group_alias (i.e. won the
//     alphabetical-last-wins tie-break across all CRs).
//   - ConflictsWith  — when Applied=false, "<namespace>/<name>#<index>" of
//     the CR+entry that won the slot.
//   - AppliedValue   — when Applied=true, the Value last successfully
//     written for this Name.
type AliasEntryStatus struct {
	// Name mirrors spec.aliases[].name for the entry this status row
	// describes.
	Name string `json:"name"`

	// Applied is true iff this entry is the current winner for Name in
	// LiteLLM router_settings.model_group_alias.
	Applied bool `json:"applied"`

	// AppliedValue is the Value the operator last successfully wrote into
	// router_settings.model_group_alias[Name] for this entry. Empty until
	// the first successful POST /config/update on which this entry won.
	//
	// +optional
	AppliedValue string `json:"appliedValue,omitempty"`

	// ConflictsWith is "<namespace>/<name>#<index>" identifying the winning
	// CR+entry when Applied=false; empty when Applied=true.
	//
	// +optional
	ConflictsWith string `json:"conflictsWith,omitempty"`
}

// LiteLLMModelAliasStatus is the observed state of a multi-entry alias CR.
type LiteLLMModelAliasStatus struct {
	// ObservedGeneration is the metadata.generation last reconciled.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions carries the standard metav1.Condition list. The single
	// type defined for LiteLLMModelAlias is `Ready`, with reasons:
	//   - Synced               — every spec.aliases entry won its slot;
	//                            all AliasStatuses[].Applied == true.
	//   - PartialAliasConflict — at least one spec.aliases entry lost the
	//                            slot to another CR; see AliasStatuses[]
	//                            for per-entry detail.
	//   - LiteLLMUnavailable   — LiteLLMConnection/default not Ready.
	//   - LiteLLMRejected      — LiteLLM returned a non-2xx response on
	//                            GET /get/config/callbacks or
	//                            POST /config/update.
	//
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// AliasStatuses carries one entry per spec.aliases item, in the same
	// order as spec.aliases. Surfaces per-entry winner/loser state so
	// users can diagnose conflicts when one CR contributes many aliases.
	//
	// +optional
	// +listType=map
	// +listMapKey=name
	AliasStatuses []AliasEntryStatus `json:"aliasStatuses,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=modelalias,categories=litellm
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].reason"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// LiteLLMModelAlias is the Schema for the litellmmodelaliases API.
//
// Intra-CR uniqueness of spec.aliases[].name is enforced by Kubernetes via
// the +listType=map +listMapKey=name markers on the Aliases field — no CEL
// rule needed.
//
// One CR contributes ONE OR MORE entries to LiteLLM
// router_settings.model_group_alias via spec.aliases. The operator
// aggregates ALL LiteLLMModelAlias CRs cluster-wide:
//
//  1. Sort CRs by (namespace, name) ASC.
//  2. For each CR, iterate spec.aliases in declared array order.
//  3. Last (CR, entry) per alias name wins.
//  4. GET /get/config/callbacks → splice the merged map into the existing
//     router_settings.model_group_alias → POST /config/update.
//
// Per-entry winner/loser state is surfaced in status.aliasStatuses[].
type LiteLLMModelAlias struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LiteLLMModelAliasSpec   `json:"spec,omitempty"`
	Status LiteLLMModelAliasStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LiteLLMModelAliasList contains a list of LiteLLMModelAlias.
type LiteLLMModelAliasList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LiteLLMModelAlias `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LiteLLMModelAlias{}, &LiteLLMModelAliasList{})
}
