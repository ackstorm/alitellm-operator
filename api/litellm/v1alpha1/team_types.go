// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BudgetSpec is the optional sub-block on TeamSpec that carries the LiteLLM
// team budget (per spec §6.7). It MUST be modeled as a pointer at the
// TeamSpec level (`Budget *BudgetSpec`) so that whole-block absence is
// distinguishable from `Budget{}` — when the user omits `spec.budget`
// entirely, the reconciler emits `max_budget: null` AND
// `budget_duration: null` on the `POST /team/update` body (§6.7 "Clearing
// budget" — relies on the wholesale-replace semantic of POST /team/update
// per §5.1, Q10). Both LiteLLM fields are `anyOf: [<type>, null]` in the
// 1.82.6 OpenAPI, so the null form is the canonical "no budget set"
// signal.
//
// Float64 precision is adopted for v1alpha1 per spec §6.7; a string-encoded
// resource.Quantity-style form is deferred unless penny-precision-at-scale
// becomes a concern.
type BudgetSpec struct {
	// Limit is the USD budget cap for the LiteLLM team, projected
	// verbatim onto `max_budget` (number). Modeled as `*float64` so the
	// reconciler can distinguish "user set 0.0" from "user omitted the
	// field" — the former projects to `max_budget: 0.0`, the latter to
	// `max_budget: null` (per spec §6.7 "Clearing budget").
	//
	// +optional
	Limit *float64 `json:"limit,omitempty"`

	// Period is the budget reset interval as a duration string, projected
	// verbatim onto `budget_duration` (string). CEL admission rejects any
	// value that does not match `^[0-9]+[smhd]$` (seconds | minutes |
	// hours | days — per spec §6.7). MinLength=1 is NOT applied
	// independently: the regex pattern + `omitempty` together cover
	// absence (no pattern check fires when the field is absent), and a
	// present-but-empty value (`""`) is rejected by the regex itself.
	//
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+[smhd]$`
	Period string `json:"period,omitempty"`
}

// RateLimitsSpec is the optional sub-block on TeamSpec that carries the LiteLLM
// team RPM/TPM rate limits (per Feature 01 §1). It MUST be modeled as a
// pointer at the TeamSpec level (`RateLimits *RateLimitsSpec`) so that
// whole-block absence is distinguishable from `RateLimitsSpec{}` — when the
// user omits `spec.rateLimits` entirely OR sets `spec.rateLimits: {}` with no
// leaves, the reconciler emits `rpm_limit: null` AND `tpm_limit: null` on the
// `POST /team/update` body AND OMITS both `rpm_limit_type` and
// `tpm_limit_type` keys (Feature 01 §2.1). The empty-block-equals-absent
// semantic mirrors `BudgetSpec` precedent (spec §6.7) so the two parallel
// sub-blocks behave identically.
//
// The `*_type` keys are NEVER exposed as CR fields — they are hardcoded to
// `best_effort_throughput` by the operator whenever the corresponding
// `*_limit` is non-null (Feature 01 §1.2, §1.3, §2.1). Promoting them to
// typed CR fields is deferred until LiteLLM models them on
// `UpdateTeamRequest` (currently only `NewTeamRequest` carries them).
//
// Per-leaf admission uses an OpenAPI `minimum: 0` schema constraint
// (kubebuilder Minimum annotation, NOT CEL XValidation) — equivalent to
// Feature 01 §1.1's `self.rpm >= 0` / `self.tpm >= 0` constraint but rendered
// as a built-in OpenAPI schema constraint. The K8s API server still rejects
// negative values at admission.
type RateLimitsSpec struct {
	// RPM is the requests-per-minute cap, projected verbatim onto
	// `rpm_limit` (integer) on the POST /team/{new,update} body. Modeled
	// as `*int32` so the reconciler can distinguish `0` (explicit zero —
	// projects to `rpm_limit: 0` + `rpm_limit_type: "best_effort_throughput"`)
	// from omitted (user did not set the field — projection emits
	// `rpm_limit: null` and OMITS `rpm_limit_type` per Feature 01 §2.1).
	//
	// +optional
	// +kubebuilder:validation:Minimum=0
	RPM *int32 `json:"rpm,omitempty"`

	// TPM is the tokens-per-minute cap, projected verbatim onto
	// `tpm_limit` (integer) on the POST /team/{new,update} body. Modeled
	// as `*int32` so the reconciler can distinguish `0` (explicit zero —
	// projects to `tpm_limit: 0` + `tpm_limit_type: "best_effort_throughput"`)
	// from omitted (user did not set the field — projection emits
	// `tpm_limit: null` and OMITS `tpm_limit_type` per Feature 01 §2.1).
	//
	// +optional
	// +kubebuilder:validation:Minimum=0
	TPM *int32 `json:"tpm,omitempty"`
}

// TeamSpec defines the desired state of a LiteLLM team.
//
// A LiteLLMTeam is a CONTAINER, not a grant: it holds budget / rate limits and
// attaches unified access groups. The operator always projects a fully closed
// team (models ["no-default-models"], null-UUID agents, [] for every MCP
// field) and access is opened ONLY by the attached groups, so what a team can
// reach is defined entirely by its LiteLLMAccessGroup CRs.
//
// `metadata.name` IS the LiteLLM `team_alias` (and the `team_id` on CREATE).
//
// With no `LiteLLMTeam/default` CR the operator keeps an implicit `default`
// team — closed, no access groups. Deleting the CR falls back to that state.
type TeamSpec struct {
	// AccessGroups is the list of LiteLLMAccessGroup NAMES this team is
	// attached to, resolved to ids via GET /v1/access_group and projected
	// onto the team's `access_group_ids`. Empty → the team reaches nothing.
	// An unresolved name parks the Team Ready=False reason=AccessGroupNotFound
	// and requeues (ordering dependency with LiteLLMAccessGroup CRs).
	//
	// SECURITY: groups only ADD. Every attached group widens the team.
	//
	// +optional
	AccessGroups []string `json:"accessGroups,omitempty"`

	// Budget is the optional budget sub-block. Modeled as `*BudgetSpec`
	// (pointer) so the reconciler can distinguish whole-block absence
	// from an empty `BudgetSpec{}`. When absent, the reconciler emits
	// `max_budget: null` AND `budget_duration: null` on the
	// `POST /team/update` body (spec §6.7 "Clearing budget").
	//
	// +optional
	Budget *BudgetSpec `json:"budget,omitempty"`

	// RateLimits is the optional rate-limits sub-block (per Feature 01 §1,
	// parallel to `spec.budget`). Modeled as `*RateLimitsSpec` (pointer)
	// so the reconciler can distinguish whole-block absence from an empty
	// `RateLimitsSpec{}`. When absent (whole-block or empty-struct —
	// mirrors Budget §6.7 precedent), the reconciler emits `rpm_limit:
	// null` AND `tpm_limit: null` on the `POST /team/update` body AND
	// OMITS both `rpm_limit_type` and `tpm_limit_type` keys (Feature 01
	// §2.1). The `*_type` keys are hardcoded to `best_effort_throughput`
	// by the operator whenever the corresponding `*_limit` is non-null —
	// they are never exposed as CR fields (Feature 01 §1.2, §1.3).
	//
	// +optional
	RateLimits *RateLimitsSpec `json:"rateLimits,omitempty"`

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

// TeamLastRenderedStatus records the post-substitution rendered Team state
// last successfully applied to LiteLLM. Structural analog of
// `MCPServerLastRenderedStatus` (Phase 5 D-03) with `ServerID` → `TeamID`.
//
// Per Phase 5 D-02 (extended to Team as the third member of the ID-pin
// family alongside MCPServer and A2AAgent — see `spec/DEFECTS-1.82.6.md`
// row `DEF-§6.4/§6.6-ID-PERSIST`), `TeamID` is pinned across reconciles:
// the reconciler resolves the LiteLLM-assigned `team_id` UUID once (via
// `ListTeamsByAlias` + smallest-`team_id` duplicate rule from spec §7.1)
// then reads from status thereafter. The finalizer DELETE path (plan
// 06-04) issues `POST /team/delete` against the pinned `TeamID` directly,
// without re-resolving by alias.
//
// `Hash` is informational on this path — `POST /team/update` IS
// wholesale-replace per spec §5.1 (Q10), so no delete-and-recreate path
// is committed in the Team reconciler. The field is retained for
// observability and forward-compat (mirrors the Phase 5 D-01 rationale).
type TeamLastRenderedStatus struct {
	// Hash is the SHA-256 hex of the RFC 8785-canonicalized rendered body
	// (team_alias, budget + rate-limit keys, the closed baseline and the
	// resolved access_group_ids).
	// An empty hash indicates the Team has not yet been successfully
	// reconciled (Phase 3 D-01, Phase 5 D-03).
	//
	// +optional
	Hash string `json:"hash,omitempty"`

	// TeamID is the LiteLLM-assigned UUID (`team_id`) for this team
	// entry. Pinned per Phase 3 D-04 + Phase 5 D-02 so the reconciler
	// can call `POST /team/delete` (with body `{"team_ids": [.]}`)
	// directly on the finalizer path without re-resolving by alias. On
	// first reconcile, resolved via `ListTeamsByAlias` + smallest-
	// `team_id` duplicate rule from spec §7.1.
	//
	// Diverges from spec §6.7 (silent on persistence — the spec says
	// "the operator resolves the LiteLLM team ID by alias" on deletion;
	// pinning saves the list call). Documented in
	// `spec/DEFECTS-1.82.6.md` row `DEF-§6.4/§6.6-ID-PERSIST` (Team is
	// the third member of the ID-pin family).
	//
	// +optional
	TeamID string `json:"teamID,omitempty"`

	// At is the timestamp of the last SUCCESSFUL render (NOT every
	// reconcile attempt — transient failures do not update this field).
	//
	// +optional
	At *metav1.Time `json:"at,omitempty"`
}

// TeamStatus defines the observed state of Team per spec §6.7 +
// Phase 5 D-03 (nested `lastRendered` substruct).
type TeamStatus struct {
	// ObservedGeneration is the metadata.generation of the Team CR the
	// reconciler most recently processed successfully. Consumers compare
	// this against `metadata.generation` to detect whether the current
	// spec has been reconciled yet (Phase 3 OWN-08 carry-forward).
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions carries the standard metav1.Condition list. The single
	// type defined for Team is `Ready`, with reason values drawn from
	// §6.0 (spec line 521):
	// - Synced — rendered body matches LiteLLM; no drift.
	// - LiteLLMUnavailable — LiteLLMConnection/default not Ready
	// (Phase 3 D-08 echo-reason from the
	// connection cache snapshot).
	// - LiteLLMRejected — LiteLLM returned a 4xx (non-401) on
	// mutation.
	// - SecretNotFound — a `spec.secrets[].secretRef` is missing
	// OR a `{{NAME}}` placeholder has no
	// matching `spec.secrets[].as` entry.
	//
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// LastRendered is the operator-side drift source of truth (Phase 3
	// D-01 / Phase 5 D-03). It records the post-substitution rendered
	// state that was last successfully applied to LiteLLM. The reconciler
	// compares the current desired-state hash against `lastRendered.hash`
	// to detect drift without querying the LiteLLM API on every reconcile.
	//
	// +optional
	LastRendered TeamLastRenderedStatus `json:"lastRendered,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=team,categories=litellm
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].reason"
// +kubebuilder:printcolumn:name="TeamID",type=string,JSONPath=".status.lastRendered.teamID"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// LiteLLMTeam is the Schema for the litellmteams API per spec §6.7 (`_FINALv3`).
//
// TEAM-01.04 acceptance criteria + AC-T1 (projection assertions) + AC-T6
// (`Team/default` carve-out behavior — implemented in reconciler,
// NOT in this type).
//
// The operator uses the bare `metadata.name` as the LiteLLM `team_alias`
// for every Team, including the reserved `default`. There is no
// team-name prefix and no overlay metadata. Spec §6.7 explicitly allows
// a user-authored `Team/default` override (ownership transition handled by
// the reconciler); a CEL singleton-by-name rule would block AC-T2 and is
// NOT applied to this resource.
//
// Default-team carve-out:
// - If no `Team/default` CR exists, the operator bootstraps the LiteLLM
// `team_alias=default` with no budget via a synthetic reconcile on
// manager start (after the cached `LiteLLMConnection/default` first
// reaches `Ready=True`) and on each 30-min safety re-list (§7.4, §7.6).
// - If a `Team/default` CR exists, the operator reconciles it normally
// (ownership transition: re-uses the LiteLLM team, does NOT recreate).
// - Deletion of `Team/default` is suppressed: the operator re-applies
// the implicit empty spec to the LiteLLM team `default`, then removes
// the finalizer. `POST /team/delete` is NEVER called for the alias
// `default`.
//
// Finalizer (spec §7.5): `teams.litellm.ackstorm.ai/finalizer` — issues
// `POST /team/delete` with body `{"team_ids": [<lastRendered.teamID>]}`
// before the CR is removed from etcd. The default-team
// carve-out short-circuits the LiteLLM call.
type LiteLLMTeam struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TeamSpec   `json:"spec,omitempty"`
	Status TeamStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LiteLLMTeamList contains a list of LiteLLMTeam.
type LiteLLMTeamList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LiteLLMTeam `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LiteLLMTeam{}, &LiteLLMTeamList{})
}
