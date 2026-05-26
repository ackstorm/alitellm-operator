// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ModelSpec defines the desired state of Model.
//
// MODEL-01: a user can declare a Model CR with spec.params (pass-through bag
// forwarded to LiteLLM litellm_params) and spec.info (pass-through bag
// forwarded to LiteLLM model_info). Both bags are x-kubernetes-preserve-unknown-
// fields: true (any JSON is accepted). spec.secrets[] maps Kubernetes Secrets
// into {{NAME}} placeholders inside the bags (§5.2 substitution).
//
// The shape is flat per _FINALv3: no spec.type discriminator and no
// nested litellm sub-object. The operator's only typed-field overlay is
// model_info.id (null on create, resolved remote ID on update — D-04).
//
// MODEL-02: spec.secrets[].as values are validated at admission via the CEL
// XValidation rule on this struct; uniqueness within a Model is also
// enforced (SEC-03).
//
// MODEL-05: spec.params and spec.info pass-through bags carry
// x-kubernetes-preserve-unknown-fields: true so any future litellm_params /
// model_info fields are accepted without a CRD schema change.
type ModelSpec struct {
	// Params is a pass-through bag of fields forwarded verbatim to
	// LiteLLM's litellm_params on POST /model/new and POST /model/update.
	// Any JSON object is accepted (x-kubernetes-preserve-unknown-fields: true).
	// String-typed leaf values may contain {{NAME}} placeholders that are
	// resolved from spec.secrets[] before the body reaches LiteLLM (§5.2,
	// D-05). Non-string leaves are forwarded unchanged (SEC-02).
	//
	// The operator NEVER adds or removes keys inside this bag; the user's
	// declared keyset is the desired state. On each reconcile, the rendered
	// post-substitution body is hashed (SHA-256) and compared against
	// status.lastRendered.hash to detect drift (D-01).
	//
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Params runtime.RawExtension `json:"params,omitempty"`

	// Info is a pass-through bag of fields forwarded verbatim to
	// LiteLLM's model_info on POST /model/new and POST /model/update,
	// except that the operator overlays model_info.id (null on CREATE,
	// resolved LiteLLM UUID on UPDATE — D-04). Any JSON object is accepted
	// (x-kubernetes-preserve-unknown-fields: true). Same {{NAME}} substitution
	// rules as spec.params apply (§5.2, D-05).
	//
	// If the user supplies spec.info.id, the operator's overlay always wins
	// and a type=Warning, reason=ProjectionOverride Event is emitted per
	// spec §5.1 (Identity tier — operator-set field).
	//
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Info runtime.RawExtension `json:"info,omitempty"`

	// Secrets is the substitution map for resolving {{NAME}} placeholders in
	// spec.params and spec.info string-typed leaves (§5.2, D-05, SEC-03).
	// Each entry maps an uppercase NAME (the as field) to a Kubernetes Secret
	// key (secretRef). Placeholders in the bags are replaced with the resolved
	// plaintext value before the body is forwarded to LiteLLM. Secret material
	// NEVER appears in logs, Events, or status conditions (§9.1, AC-S1).
	//
	// CEL constraints: each as value must match ^[A-Z_][A-Z0-9_]*$ and all as
	// values within a Model must be unique (SEC-03). Violations are rejected
	// at admission — no reconcile is triggered.
	//
	// +optional
	// NOTE: SEC-03 uniqueness of spec.secrets[].as values is enforced as a
	// runtime check in the Model reconciler (see
	// internal/controller/model_controller.go Step 3.5). The CEL XValidation
	// list-uniqueness expression was not expressible in the Kubernetes 1.31
	// CRD CEL environment (toSet and map-then-unique patterns require a
	// newer CEL library version). The admission-time CEL alternative is
	// deferred to v1beta1 when a higher Kubernetes floor can be assumed.
	Secrets []SecretSubstitution `json:"secrets,omitempty"`

	// DeletionPolicy controls finalizer behavior when the LiteLLM-side
	// DELETE cannot be confirmed (LiteLLM unavailable, 401, transient
	// error already retried). Defaults to "Orphan" to preserve REL-06
	// anti-storm: the CR is freed even if the LiteLLM entry may linger.
	// "Delete" blocks finalizer removal until the LiteLLM-side ack
	// succeeds, suitable for GitOps users who must not see "synced"
	// while a backend resource still exists.
	//
	// Annotation override (`litellm.ackstorm.ai/deletion-policy-override`)
	// takes precedence over this field for runtime break-glass without a
	// spec mutation.
	//
	// +kubebuilder:validation:Enum=Orphan;Delete
	// +kubebuilder:default=Orphan
	// +optional
	DeletionPolicy string `json:"deletionPolicy,omitempty"`
}

// SecretSubstitution maps a Kubernetes Secret key to an uppercase NAME that
// can be referenced as {{NAME}} in spec.params and spec.info string-typed
// leaves (§5.2). The as value is the NAME; the secretRef points to the
// Kubernetes Secret and key that carry the plaintext value (SEC-01.SEC-03).
//
// SEC-03: the as value is validated at admission by the CEL pattern
// ^[A-Z_][A-Z0-9_]*$; lowercase letters, digits-first names, and whitespace
// are rejected. Uniqueness of as values within a Model is enforced by a CEL
// XValidation on ModelSpec.Secrets (see above).
type SecretSubstitution struct {
	// As is the placeholder NAME used in {{NAME}} substitution expressions
	// within spec.params and spec.info string-typed leaves. Must match the
	// pattern ^[A-Z_][A-Z0-9_]*$ (§5.2, SEC-03). Unique within the enclosing
	// Model CR (SEC-03 uniqueness — enforced via CEL on spec.secrets).
	//
	// Example: as=ANTHROPIC_API_KEY matches the placeholder
	// "{{ANTHROPIC_API_KEY}}" anywhere in spec.params or spec.info strings.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[A-Z_][A-Z0-9_]*$`
	As string `json:"as"`

	// SecretRef identifies the Kubernetes Secret and key whose plaintext
	// value replaces the {{As}} placeholder at reconcile time. The Secret
	// MUST reside in the same namespace as the Model CR (no cross-namespace
	// resolution in v1alpha1). A missing Secret or key surfaces as
	// Ready=False, reason=SecretNotFound (§6.0).
	//
	// Reuses SecretKeyRef from litellmconnection_types.go (same package —
	// no additional type definition needed). SEC-01, SEC-06.
	//
	// +kubebuilder:validation:Required
	SecretRef SecretKeyRef `json:"secretRef"`
}

// ModelStatus defines the observed state of Model.
//
// The status surface per D-07 (CONTEXT.md): a nested lastRendered struct
// carries the operator-side drift source of truth (D-01) alongside the
// standard observedGeneration + conditions fields. The flat D-07 structure
// is the rejected alternative (Q3.1 Option B); nested is preferred for
// kubectl get -o yaml readability.
//
// OWN-08: observedGeneration is populated on every successful reconcile so
// that consumers can detect whether the reconciler has processed the latest
// spec version.
type ModelStatus struct {
	// ObservedGeneration is the metadata.generation of the Model CR the
	// reconciler most recently processed successfully. Consumers can compare
	// this against metadata.generation to detect whether the current spec
	// has been reconciled yet (OWN-08).
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions carries the standard metav1.Condition list. The single type
	// defined for Model is `Ready`, with reason values from §6.0:
	// - Synced — rendered body matches LiteLLM; no drift.
	// - LiteLLMUnavailable — LiteLLMConnection/default not Ready (CONN-06).
	// - LiteLLMRejected — LiteLLM returned a 4xx/5xx on mutation.
	// - SecretNotFound — a spec.secrets[].secretRef is missing (SEC-06).
	// - UnresolvedPlaceholder — a {{NAME}} in spec.params/info has no matching
	// spec.secrets[] entry (SEC-05).
	//
	// Phase 3+ dependents read this via kubectl or by listing Model CRs; the
	// Model reconciler does not expose a cache equivalent to the connection cache.
	//
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// LastRendered is the operator-side drift source of truth (D-01, D-07).
	// It records the post-substitution rendered state that was last
	// successfully applied to LiteLLM. The reconciler compares the current
	// desired state hash against lastRendered.hash to detect drift without
	// querying the LiteLLM API (bypassing Probe 4a/4b encrypted-response
	// issues — see spec/DEFECTS-1.82.6.md).
	//
	// +optional
	LastRendered LastRenderedStatus `json:"lastRendered,omitempty"`
}

// LastRenderedStatus records the post-substitution rendered state that was
// last successfully applied to LiteLLM. This is the operator-side drift
// source of truth (D-01): the reconciler compares the current desired hash
// against Hash to decide whether a LiteLLM mutation is needed.
//
// D-02 (delete-and-recreate pivot): ParamsKeys and InfoKeys are required for
// per-bag shrinkage detection. If any key is removed from either bag
// (persistedKeys \ desiredKeys is non-empty), the reconciler deletes the
// existing LiteLLM entry and re-creates it, then re-pins ModelID to
// the freshly-assigned UUID. See spec/DEFECTS-1.82.6.md row 2.
//
// D-04: ModelID diverges from spec §6.2 ("operator does not persist
// the assigned model_info.id"). Persistence is a pragmatic win for a low-rate
// operator — saves a GET /model/info per reconcile. Documented deviation.
type LastRenderedStatus struct {
	// Hash is the SHA-256 hex of the RFC 8785–canonicalized merged
	// post-substitution body (spec.params merged with spec.info, after
	// {{NAME}} substitution, before the model_info.id overlay). The hash
	// deliberately excludes model_info.id so that CREATE vs UPDATE reconciles
	// do not oscillate (D-01). An empty hash indicates the Model has not yet
	// been successfully reconciled.
	//
	// +optional
	Hash string `json:"hash,omitempty"`

	// ParamsKeys is the sorted list of dotted-path keys present in spec.params
	// at the time of the last successful render (D-02). The reconciler diffs
	// this against the current spec.params keyset: if any key is absent in the
	// desired state (a shrinkage), the full delete-and-recreate path is taken
	// instead of POST /model/update (D-02, see spec/DEFECTS-1.82.6.md row 2).
	//
	// +optional
	ParamsKeys []string `json:"paramsKeys,omitempty"`

	// InfoKeys is the sorted list of dotted-path keys present in spec.info at
	// the time of the last successful render (D-02). Same shrinkage-detection
	// semantics as ParamsKeys — any key removal in EITHER bag triggers
	// delete-and-recreate.
	//
	// +optional
	InfoKeys []string `json:"infoKeys,omitempty"`

	// ModelID is the LiteLLM-assigned UUID (model_info.id) for this
	// Model entry. Persisted here so the reconciler can reference it directly
	// on subsequent reconciles without an extra GET /model/info call (D-04).
	//
	// IMPORTANT (D-02 consequence): on every delete-and-recreate cycle, LiteLLM
	// assigns a fresh UUID. The reconciler MUST re-pin this field to the new
	// UUID inside the same reconcile that performed the delete+recreate, before
	// returning success, to avoid a stale-ID 404 on the next reconcile.
	//
	// Diverges from spec §6.2: documented in spec/DEFECTS-1.82.6.md row 7 (D-04).
	//
	// +optional
	ModelID string `json:"modelID,omitempty"`

	// At is the timestamp of the last SUCCESSFUL render (NOT every reconcile
	// attempt — transient failures do not update this field). Reconcile
	// attempt frequency is observable via controller-runtime workqueue metrics
	// and cr_status_age_seconds (§10 / OBS-03).
	//
	// +optional
	At *metav1.Time `json:"at,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=mdl,categories=litellm
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].reason"
// +kubebuilder:printcolumn:name="ModelID",type=string,JSONPath=".status.lastRendered.modelID"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// LiteLLMModel is the Schema for the litellmmodels API.
//
// MODEL-01: a LiteLLMModel CR registers one LiteLLM model entry (a litellm_params +
// model_info pair) against the LiteLLM instance referenced by
// LiteLLMConnection/default. User-authored and Discovery-generated CRs are
// treated identically by the reconciler (OWN-04 silent-overwrite on
// first-reconcile name collision).
//
// Shape: flat _FINALv3 — no spec.type discriminator, no nested litellm
// sub-object. spec.params and spec.info are pass-through bags. spec.secrets[]
// provides {{NAME}} substitution (§5.2). See ModelSpec for field details.
//
// LiteLLMModel names are chosen freely by the user (MODEL-01 — not a singleton).
// There is NO CEL singleton-by-name rule on this resource. The spec §7.5
// finalizer name is "models.litellm.ackstorm.ai/finalizer".
type LiteLLMModel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelSpec   `json:"spec,omitempty"`
	Status ModelStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LiteLLMModelList contains a list of LiteLLMModel.
type LiteLLMModelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LiteLLMModel `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LiteLLMModel{}, &LiteLLMModelList{})
}
