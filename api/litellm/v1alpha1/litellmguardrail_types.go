// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// GuardRailMode is one execution lifecycle slot recognized by LiteLLM's
// GuardrailEventHooks enum (litellm/types/guardrails.py). The operator
// emits the mode as a scalar string when the user supplies exactly one
// element, and as a JSON array when more than one is given —
// LiteLLM accepts both shapes on POST /guardrails.
//
// pre_call / post_call / during_call / logging_only are the standard
// slots. pre_mcp_call / during_mcp_call are MCP-specific. The realtime
// slot is mutually exclusive with the others (validation in the
// reconciler).
// +kubebuilder:validation:Enum=pre_call;post_call;during_call;logging_only;pre_mcp_call;during_mcp_call;realtime_input_transcription
type GuardRailMode string

// GuardRailSpec defines the desired state of LiteLLMGuardRail.
//
// A LiteLLMGuardRail CR registers one LiteLLM guardrail entry against
// the LiteLLMConnection/default instance (POST /guardrails). Two CRs
// sharing spec.guardrailName form a load-balancing pool: LiteLLM
// dispatches across all entries with identical guardrail_name.
//
// Per-key / per-team opt-in is set on LiteLLMVirtualKey /
// LiteLLMTeam.spec.guardrails []string by guardrailName (LiteLLM
// Enterprise runtime feature, out of scope here).
//
// Shape mirrors LiteLLMModel: pass-through spec.params (litellm_params)
// and spec.info (guardrail_info), with {{NAME}} placeholders resolved
// from spec.secrets[] at reconcile time. String values matching
// "os.environ/<VAR>" are forwarded verbatim — the LiteLLM Deployment
// admin owns the corresponding env var (this operator is an HTTP-API
// client and never touches the LiteLLM Deployment).
type GuardRailSpec struct {
	// GuardrailName is the litellm_params.guardrail_name path key used
	// by LiteLLMVirtualKey.spec.guardrails / LiteLLMTeam.spec.guardrails
	// to reference this guardrail. Two LiteLLMGuardRail CRs sharing
	// this name form a load-balancing pool (operator POSTs both;
	// LiteLLM dispatches across the pool).
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	GuardrailName string `json:"guardrailName"`

	// Provider is forwarded verbatim as litellm_params.guardrail.
	// Open-string — any value LiteLLM accepts is allowed so the
	// operator does not require a release when upstream adds a new
	// provider. Common values (2026): litellm_content_filter, aporia,
	// lakera_v2, bedrock/guardrail, presidio, azure/text_moderations,
	// openai_moderation, model_armor, generic_guardrail_api,
	// prompt_guard, hide_secrets, custom_guardrail, custom_code.
	// See https://docs.litellm.ai/docs/guardrail_providers.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Provider string `json:"provider"`

	// Mode is one or more execution slots from GuardrailEventHooks.
	// The operator emits a scalar string on the wire when len == 1
	// and an array otherwise. realtime_input_transcription must be
	// the only element (enforced in the reconciler — invalid combos
	// surface as Ready=False, reason=InvalidMode).
	//
	// MaxItems=6 admits all six non-realtime hook slots (pre_call,
	// post_call, during_call, logging_only, pre_mcp_call, during_mcp_call)
	// in a single guardrail. realtime_input_transcription is the
	// mutually-exclusive 7th enum value, rejected here by count and
	// enforced as single-element in the reconciler.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=6
	Mode []GuardRailMode `json:"mode"`

	// DefaultOn renders as litellm_params.default_on. When true the
	// guardrail evaluates on every request even when keys/teams have
	// not explicitly opted in. Server-side default is false; omit to
	// inherit. A *bool is used so a nil value omits the field from the
	// rendered body (rather than sending the Go zero value).
	//
	// +optional
	DefaultOn *bool `json:"defaultOn,omitempty"`

	// PolicyTemplate names a reusable rule bundle stored server-side
	// (Guardrail.policy_template — top-level field, NOT inside
	// litellm_params). When set, the named template is merged with
	// litellm_params at evaluation time.
	//
	// +optional
	PolicyTemplate string `json:"policyTemplate,omitempty"`

	// Params is the litellm_params pass-through bag (any JSON object
	// accepted via x-kubernetes-preserve-unknown-fields). Forwarded
	// verbatim to LiteLLM on POST /guardrails and PUT
	// /guardrails/{guardrail_id}, after {{NAME}} substitution from
	// spec.secrets[] is applied to string-typed leaves.
	//
	// String leaves matching "os.environ/<VAR>" pass through unchanged
	// — LiteLLM resolves them at runtime against its own process env.
	// The LiteLLM Deployment admin owns provisioning of these env
	// vars; the operator does not touch the LiteLLM Deployment.
	//
	// Reserved keys are stripped from this bag before send and
	// surfaced via a Warning Event (reason=ReservedKeyStripped) — the
	// canonical home for each is the typed spec field shown:
	//
	//   - guardrail        → spec.provider
	//   - mode             → spec.mode
	//   - default_on       → spec.defaultOn
	//   - policy_template  → spec.policyTemplate
	//   - guardrail_name   → spec.guardrailName
	//
	// All other keys flow through untouched, including but not limited
	// to: api_base, api_key, weight (load-balancing), category_thresholds
	// (Lakera), patterns, blocked_words, categories, image_model
	// (litellm_content_filter), unreachable_fallback
	// (Literal["fail_closed","fail_open"]), extra_headers,
	// skip_system_message_in_guardrail, skip_tool_message_in_guardrail,
	// mask_request_content, mask_response_content,
	// violation_message_template, end_session_after_n_fails,
	// on_violation, realtime_violation_message,
	// experimental_use_latest_role_message_only,
	// additional_provider_specific_params, custom_code,
	// pangea_input_recipe, pangea_output_recipe, template_id / location
	// / credentials / api_endpoint / fail_on_error (Model Armor),
	// guardrailIdentifier / guardrailVersion / aws_region_name /
	// aws_access_key_id / aws_secret_access_key (Bedrock),
	// presidio_analyzer_api_base / presidio_anonymizer_api_base /
	// pii_entities / output_parse_response (Presidio), ...
	//
	// LiteLLM's litellm_params Pydantic model is extra="allow" so any
	// future field flows through without a CRD schema change.
	//
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Params runtime.RawExtension `json:"params,omitempty"`

	// Info is the guardrail_info pass-through bag (top-level on the
	// Guardrail body, alongside litellm_params). Surfaced through
	// GET /v2/guardrails/list; typically carries a free-form
	// description and an optional dynamic-request parameter schema
	// consumed by clients. Same {{NAME}} substitution rules as
	// spec.params apply.
	//
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Info runtime.RawExtension `json:"info,omitempty"`

	// Secrets is the substitution map for {{NAME}} placeholders
	// embedded in spec.params and spec.info string-typed leaves.
	// Each entry maps an uppercase NAME (the as field) to a
	// Kubernetes Secret key (secretRef). Placeholders are replaced
	// with the resolved plaintext value before the body is forwarded
	// to LiteLLM. Secret material never appears in logs, Events, or
	// status conditions.
	//
	// Secrets MUST reside in the same namespace as the
	// LiteLLMGuardRail CR (no cross-namespace resolution in v1alpha1).
	// A missing Secret or key surfaces as Ready=False,
	// reason=SecretNotFound; a {{NAME}} with no matching
	// spec.secrets[] entry surfaces as Ready=False,
	// reason=UnresolvedPlaceholder.
	//
	// String leaves of the form "os.environ/<VAR>" are NOT
	// substituted — they pass through unchanged.
	//
	// +optional
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

// GuardRailStatus defines the observed state of LiteLLMGuardRail.
type GuardRailStatus struct {
	// ObservedGeneration is the metadata.generation of the
	// LiteLLMGuardRail CR the reconciler most recently processed
	// successfully (OWN-08).
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions carries the standard metav1.Condition list. The
	// single type defined for LiteLLMGuardRail is Ready, with reason
	// values:
	//
	//   - Synced — rendered body matches LiteLLM; no drift.
	//   - LiteLLMUnavailable — LiteLLMConnection/default not Ready.
	//   - LiteLLMRejected — LiteLLM returned a 4xx/5xx on mutation.
	//   - SecretNotFound — a spec.secrets[].secretRef is missing.
	//   - UnresolvedPlaceholder — a {{NAME}} in spec.params/info has
	//     no matching spec.secrets[] entry.
	//   - InvalidMode — spec.mode combination is rejected
	//     (e.g. realtime_input_transcription not alone).
	//   - ConflictsWithConfigGuardrail — a guardrail of the same name
	//     is already loaded from the LiteLLM config file
	//     (guardrail_definition_location=config); such rows are not
	//     addressable via POST/PUT/DELETE /guardrails.
	//   - PoolProviderMismatch — two CRs share guardrailName but
	//     declare different providers.
	//
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// LastRendered is the operator-side drift source of truth (D-01,
	// D-07 — same pattern as LiteLLMModel). On each reconcile the
	// rendered post-substitution body is hashed and compared against
	// LastRendered.Hash; a mismatch triggers a PUT /guardrails/{id}
	// (drift correction) and increments
	// drift_corrected_total{domain=guardrail,action=update_drifted}.
	//
	// +optional
	LastRendered GuardRailLastRenderedStatus `json:"lastRendered,omitempty"`
}

// GuardRailLastRenderedStatus records the post-substitution rendered
// state that was last successfully applied to LiteLLM via POST
// /guardrails or PUT /guardrails/{guardrail_id}. Operator-side drift
// source of truth — the reconciler compares the current desired hash
// against Hash to decide whether a mutation is needed (D-01).
//
// ParamsKeys and InfoKeys carry the dotted-path keyset present at the
// last successful render. If any key is removed from either bag
// (persistedKeys \ desiredKeys is non-empty), the reconciler can take
// the safer delete-and-recreate path rather than relying on PUT to
// strip keys it never explicitly clears (D-02 pattern, same as Model).
type GuardRailLastRenderedStatus struct {
	// Hash is the SHA-256 hex of the RFC 8785-canonicalized
	// post-substitution Guardrail body (top-level guardrail_name,
	// guardrail_info, policy_template, plus litellm_params) — minus
	// server-assigned fields (guardrail_id, created_at, updated_at).
	// An empty hash means the CR has not yet been successfully
	// reconciled.
	//
	// +optional
	Hash string `json:"hash,omitempty"`

	// ParamsKeys is the sorted list of dotted-path keys present in
	// spec.params at the last successful render. Used for shrinkage
	// detection (D-02).
	//
	// +optional
	ParamsKeys []string `json:"paramsKeys,omitempty"`

	// InfoKeys is the sorted list of dotted-path keys present in
	// spec.info at the last successful render. Same shrinkage
	// semantics as ParamsKeys.
	//
	// +optional
	InfoKeys []string `json:"infoKeys,omitempty"`

	// GuardrailID is the server-assigned UUID returned by POST
	// /guardrails. The reconciler persists it here so subsequent PUT
	// and DELETE calls can address the row directly without an extra
	// GET /v2/guardrails/list lookup. Empty until the first
	// successful create.
	//
	// +optional
	GuardrailID string `json:"guardrailID,omitempty"`

	// DefinitionLocation mirrors LiteLLM's
	// guardrail_definition_location enum: "db" for rows created via
	// POST /guardrails (operator-addressable), or "config" for rows
	// loaded from the LiteLLM config file (NOT addressable via the
	// CRUD API). When "config" the reconciler MUST NOT attempt
	// mutation; instead it sets Ready=False,
	// reason=ConflictsWithConfigGuardrail.
	//
	// +optional
	DefinitionLocation string `json:"definitionLocation,omitempty"`

	// PoolSize is the number of LiteLLMGuardRail CRs sharing
	// spec.guardrailName on the owning connection at the time of the
	// last successful render (load-balancing pool member count, for
	// observability).
	//
	// +optional
	PoolSize int32 `json:"poolSize,omitempty"`

	// At is the timestamp of the last SUCCESSFUL render (not every
	// reconcile attempt — transient failures do not update this
	// field).
	//
	// +optional
	At *metav1.Time `json:"at,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=lgr,categories=litellm
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].reason"
// +kubebuilder:printcolumn:name="Guardrail",type=string,JSONPath=".spec.guardrailName"
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=".spec.provider"
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=".spec.mode"
// +kubebuilder:printcolumn:name="DefaultOn",type=boolean,JSONPath=".spec.defaultOn"
// +kubebuilder:printcolumn:name="ID",type=string,JSONPath=".status.lastRendered.guardrailID"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// LiteLLMGuardRail is the Schema for the litellmguardrails API.
//
// One LiteLLMGuardRail CR maps to one LiteLLM guardrail entry created
// via POST /guardrails against LiteLLMConnection/default. Two CRs
// sharing spec.guardrailName form a load-balancing pool.
//
// Finalizer name: "guardrails.litellm.ackstorm.ai/finalizer".
type LiteLLMGuardRail struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GuardRailSpec   `json:"spec,omitempty"`
	Status GuardRailStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LiteLLMGuardRailList contains a list of LiteLLMGuardRail.
type LiteLLMGuardRailList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LiteLLMGuardRail `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LiteLLMGuardRail{}, &LiteLLMGuardRailList{})
}
