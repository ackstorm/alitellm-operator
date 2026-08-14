// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

// PermissionSpec is the optional typed resource-permission sub-block on
// TeamSpec. Unlike the pre-existing `spec.params` passthrough, this block is
// operator-MANAGED and reconciled: the operator OWNS the projected LiteLLM
// `team.models` and `object_permission` fields whenever this block is
// present, so out-of-band UI edits to those fields do NOT survive
// reconciliation (see the TEAM-03/TEAM-04 note on TeamSpec).
//
// Modeled as a pointer at the TeamSpec level (`Permission *PermissionSpec`)
// so whole-block absence (nil) is distinguishable from a present-but-empty
// block. Absent block → the operator manages nothing here and the raw
// `spec.params.models` / `spec.params.object_permission` (if any) pass
// through unchanged (migration path). Present block → the operator projects
// every non-empty sublist and deletes any colliding `spec.params` key
// (emitting a ProjectionOverride Event).
//
// Empty-vs-absent: an ABSENT block (`Permission == nil`) means the operator
// manages nothing here (raw `spec.params` passthrough preserved). A PRESENT
// block means the operator OWNS `models` and ALL FOUR `object_permission`
// sub-fields and emits every one on the wire UNCONDITIONALLY, NEVER omitted.
// This is security-critical: LiteLLM's POST /team/update merges per-field on
// the persistent object_permission row, so an OMITTED field keeps its stale
// value — omitting a shrunk-to-empty list silently fails to revoke access.
//
// Deny-by-default: a PRESENT block is a fail-CLOSED grant. LiteLLM activates
// its `models` / object_permission.agents filter as soon as the list is
// non-empty and never validates the elements exist — but it reads an EMPTY
// list on those two fields as "no filter", so an empty grant fails OPEN (the
// team inherits the full master-key ceiling: verified in prod, a new team with
// models=[] object_permission=None saw all 427 models / 7 agents). To close
// that hole the operator projects a deny-all SENTINEL — `["__deny_all__"]` for
// `models`, the null UUID for `agents` — whenever a present block leaves those
// lists empty. The three fail-CLOSED fields (mcp_servers, mcp_access_groups,
// agent_access_groups) are still sent as an explicit `[]` (a clear). A
// populated list on any field replaces verbatim; the sentinel appears ONLY on
// the empty case of the two fail-open fields.
//
// Projection to LiteLLM (verified empirically against LiteLLM 1.83.10):
//   - Models + ModelGroups → merged into the top-level `models` list (LiteLLM
//     accepts specific model names AND model-access-group names mixed there).
//   - McpServers → object_permission.mcp_servers (LiteLLM resolves name→id).
//   - McpGroups  → object_permission.mcp_access_groups.
//   - Agents     → object_permission.agents. LiteLLM enforces on agent_id
//     UUIDs and SILENTLY IGNORES names, so the operator resolves each name to
//     its agent_id via GET /v1/agents before projecting. An unresolved name
//     (A2A agent not registered yet) requeues the Team with
//     reason=AgentNotFound rather than hard-failing.
//   - AgentGroups → object_permission.agent_access_groups. DEAD FIELD in
//     LiteLLM 1.83.10 (no API tags an agent into a group), retained for
//     forward-compat; the reconciler emits a Warning/AgentGroupsNoOp Event
//     when this sublist is non-empty.
type PermissionSpec struct {
	// Models is the list of specific LiteLLM model NAMES this team may use.
	// Merged with ModelGroups into the single top-level `models` list. When a
	// present permission block leaves BOTH Models and ModelGroups empty the
	// operator projects the deny-all sentinel `["__deny_all__"]` (fail-closed)
	// — an empty `models` list fails OPEN in LiteLLM. See the deny-by-default
	// note above.
	//
	// +optional
	Models []string `json:"models,omitempty"`

	// ModelGroups is the list of model ACCESS-GROUP names this team may use.
	// Merged with Models into the single top-level `models` list.
	//
	// +optional
	ModelGroups []string `json:"modelGroups,omitempty"`

	// AccessGroups is the list of LiteLLMAccessGroup NAMES this team is
	// attached to. Each name is resolved to an access_group_id via
	// GET /v1/access_group and projected onto the team's TOP-LEVEL
	// `access_group_ids` — NOT onto object_permission.
	//
	// Distinct from ModelGroups: that field carries legacy model-TAG names
	// and merges into `models`. This one carries unified access-group names
	// from the /v1/access_group object family. The two namespaces are
	// disjoint (a unified group does not appear in /access_group/list).
	//
	// SECURITY: an attached group only ADDS. A group granting a model
	// OVERRIDES this team's deny-by-default sentinel — verified 2026-08-06
	// on LiteLLM 1.93.0: a team with models:["__deny_all__"] plus an
	// attached group granting a model stops being denied. Treat every
	// attached group as a widening of this team's ceiling.
	//
	// An unresolved name parks the Team Ready=False
	// reason=AccessGroupNotFound and requeues (ordering dependency with
	// LiteLLMAccessGroup CRs, same shape as AgentNotFound).
	//
	// +optional
	AccessGroups []string `json:"accessGroups,omitempty"`

	// McpServers is the list of specific MCP server NAMES (aliases) this team
	// may use. Projected onto object_permission.mcp_servers; LiteLLM resolves
	// names to server ids automatically.
	//
	// +optional
	McpServers []string `json:"mcpServers,omitempty"`

	// McpGroups is the list of MCP access-group names this team may use.
	// Projected onto object_permission.mcp_access_groups.
	//
	// +optional
	McpGroups []string `json:"mcpGroups,omitempty"`

	// Agents is the list of A2A agent NAMES (human-friendly) this team may
	// use. The operator resolves each name to its agent_id UUID via
	// GET /v1/agents before projecting onto object_permission.agents — LiteLLM
	// enforces on UUIDs and ignores names. An unresolved name requeues the
	// Team (reason=AgentNotFound). When a present permission block leaves this
	// list empty the operator projects the null-UUID deny-all sentinel
	// (fail-closed) — an empty agents list fails OPEN in LiteLLM. The sentinel
	// is scoped to the empty case only; it never substitutes for an unresolved
	// name. See the deny-by-default note above.
	//
	// +optional
	Agents []string `json:"agents,omitempty"`

	// AgentGroups is the list of A2A agent access-group names. Projected onto
	// object_permission.agent_access_groups for forward-compat, but this is a
	// NO-OP in LiteLLM 1.83.10 (the API never tags an agent into a group). The
	// reconciler emits a Warning/AgentGroupsNoOp Event when this is non-empty.
	//
	// +optional
	AgentGroups []string `json:"agentGroups,omitempty"`

	// McpToolsets is the list of LiteLLMMCPToolset NAMES this team may use.
	// The operator resolves each name to its toolset_id UUID via
	// GET /v1/mcp/toolset before projecting onto
	// object_permission.mcp_toolsets — LiteLLM matches on the UUID. An
	// unresolved name requeues the Team (reason=ToolsetNotFound), mirroring
	// the agents ordering dependency.
	//
	// Multiple toolsets are UNIONED by LiteLLM, not last-wins, so listing
	// several here composes their tool grants. There is no access-group
	// concept for toolsets in LiteLLM 1.93.0 — the toolset IS the grouping
	// primitive, and listing several here is the group.
	//
	// NO deny-all sentinel: unlike `models` and `agents`, LiteLLM's toolset
	// check is fail-CLOSED ("None means no grants configured → deny"), so an
	// empty list correctly grants nothing and is emitted as a plain `[]`.
	//
	// +optional
	McpToolsets []string `json:"mcpToolsets,omitempty"`
}

// TeamSpec defines the desired state of Team per spec §6.7 (`_FINALv3` shape).
//
// TEAM-01: a user can declare a Team CR that projects a LiteLLM team alias
// (taken bare-from-`metadata.name`, no `team-` prefix) plus an optional
// budget. `metadata.name` IS the LiteLLM `team_alias` — there is no
// `spec.alias` and no overlay-metadata indirection.
//
// TEAM-02: `spec.budget.limit` (USD float64, pointer so absence → null
// on wire) and `spec.budget.period` (duration string, CEL-validated against
// `^[0-9]+[smhd]$`) project verbatim onto LiteLLM's `max_budget` and
// `budget_duration` fields. Whole-block `spec.budget` absence clears BOTH
// LiteLLM fields by emitting explicit nulls in the `POST /team/update` body
// (§6.7 "Clearing budget" + §5.1 wholesale-replace, Q10).
//
// TEAM-03: TeamSpec carries EXACTLY four fields — `Budget`,
// `RateLimits`, `Params`, `Secrets` — and explicitly omits the
// following Go-level fields that `_FINALv3` removed from earlier
// scaffolds (spec changelog lines 37–38):
// - resource gating projecting to LiteLLM `models` and
// `object_permission.*` is now MANAGED via the typed `spec.permission`
// sub-block (see PermissionSpec). This REVERSES the original _FINALv3
// delegation: when `spec.permission` is present the operator OWNS those
// LiteLLM fields, so out-of-band UI edits to `models` / `object_permission`
// do NOT survive reconciliation. When the block is ABSENT the original
// delegated/passthrough behavior is preserved (raw `spec.params.models` /
// `spec.params.object_permission` forward unchanged).
// - any team-membership field projecting to LiteLLM
// `members_with_roles`: user-to-team assignment is delegated to an
// external system, not represented in GitOps. Spec §6.7 "Semantics".
// - any access-control field projecting to LiteLLM `object_permission`
// or per-team-member permissions: unmanaged LiteLLM Team fields per
// spec §5.1 + §7.4.
// - any overlay naming field — the bare `metadata.name` IS the
// `team_alias`; no two-level naming indirection.
//
// TEAM-04: `spec.params` is a JSON pass-through bag
// (x-kubernetes-preserve-unknown-fields: true) merged into the LiteLLM
// `POST /team/new` / `POST /team/update` body at the top level of
// `NewTeamRequest`. The seven operator structural overlays
// (`team_alias`, `max_budget`, `budget_duration`, `rpm_limit`,
// `tpm_limit`, `rpm_limit_type`, `tpm_limit_type`) WIN over
// `spec.params` per spec §5.1 + Feature 01 §2.1 (typed-field overlay
// tier) — collisions emit a `reason=ProjectionOverride` Event from the
// reconciler (06-02 + Phase 10). `members_with_roles` remains unmanaged if
// placed inside `params`. `models` and `object_permission` inside `params`
// are ALSO passthrough-unmanaged ONLY when `spec.permission` is absent; when
// `spec.permission` is present the operator deletes those params keys
// (ProjectionOverride Event) and owns the projected values (see
// PermissionSpec).
//
// `spec.secrets[]` is the standard substitution map (§5.2, Phase 3 D-05)
// shared with Model / MCPServer / A2AAgent; same `{{NAME}}` placeholder
// semantics inside `params` string-typed leaves.
//
// Phase 10 (TRL-01..TRL-07) adds `spec.rateLimits.{rpm,tpm}` — a typed
// sub-block parallel to `spec.budget`, projecting onto top-level `rpm_limit`
// and `tpm_limit` (with operator-hardcoded `rpm_limit_type` /
// `tpm_limit_type` overlays — see Feature 01 §1.2/§1.3 for why the *_type
// fields are not exposed as CR knobs). Pointer-modeled (so `0` is
// distinguishable from omitted), an OpenAPI minimum-0 schema constraint
// admits only non-negative values, and clearing follows the same
// explicit-null contract as Budget (§6.7 + Feature 01 §2.1). The 4 new
// top-level overlay keys join the existing 3 (`team_alias`, `max_budget`,
// `budget_duration`) for 7 structural overlays total — worst-case 7
// ProjectionOverride Warning Events per reconcile when `spec.params`
// collides on all 7 keys.
//
// Forward-reference (NOT codified in this type): implements the
// `Team/default` carve-out — synthetic reconcile on manager start +
// 30-min safety re-list, plus deletion protection (`POST /team/delete`
// suppressed when `metadata.name == "default"` — operator re-applies the
// implicit empty spec instead). implements the finalizer DELETE
// path for non-default teams, keyed on `status.lastRendered.teamID`.
type TeamSpec struct {
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

	// Permission is the optional typed, operator-MANAGED resource-permission
	// sub-block (see PermissionSpec). When present, the operator OWNS the
	// projected LiteLLM `models` and `object_permission` fields and deletes any
	// colliding `spec.params.models` / `spec.params.object_permission` key
	// (emitting a ProjectionOverride Event). When absent, those raw params keys
	// pass through unchanged (migration path). Modeled as a pointer so
	// whole-block absence is distinguishable from an empty block.
	//
	// +optional
	Permission *PermissionSpec `json:"permission,omitempty"`

	// Params is a pass-through bag of fields forwarded verbatim to the
	// LiteLLM `POST /team/new` / `POST /team/update` body at the top
	// level of `NewTeamRequest`. Any JSON object is accepted
	// (x-kubernetes-preserve-unknown-fields: true). String-typed leaf
	// values may contain `{{NAME}}` placeholders resolved from
	// `spec.secrets[]` before the body reaches LiteLLM (§5.2, Phase 3
	// D-05). Non-string leaves are forwarded unchanged (Phase 3 SEC-02
	// carry-forward).
	//
	// The operator NEVER adds, defaults, or removes keys inside this bag
	// — the user's declared keyset IS the desired state. The operator's
	// seven structural overlays (`team_alias`, `max_budget`,
	// `budget_duration`, `rpm_limit`, `tpm_limit`, `rpm_limit_type`,
	// `tpm_limit_type`) ALWAYS win over `spec.params` per spec §5.1 +
	// Feature 01 §2.1; if the user sets any of those keys inside `params`,
	// the reconciler emits a per-key `reason=ProjectionOverride` Event
	// after the merge (worst-case 7 events on one reconcile).
	//
	// On each reconcile, the rendered post-substitution body is hashed
	// (SHA-256) and compared against `status.lastRendered.hash` to detect
	// drift without polling LiteLLM (Phase 3 D-01).
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
	// Events, or `status.conditions[].message` (§9.1, AC-S1 — exercised
	// in envtest redaction canaries).
	//
	// SEC-03 uniqueness of `spec.secrets[].as` values is enforced as a
	// runtime check in the Team reconciler (same pattern as Model plan
	// 03-06 and MCPServer — CEL list-uniqueness was deferred
	// to v1beta1).
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
	// Hash is the SHA-256 hex of the RFC 8785-canonicalized merged
	// post-substitution body (`spec.params` merged with the seven operator
	// overlays `{team_alias, max_budget, budget_duration, rpm_limit,
	// tpm_limit, rpm_limit_type, tpm_limit_type}` — the two `*_type` keys
	// are conditional-add per Feature 01 §2.1, so the hash incorporates
	// 5–7 overlay keys depending on which `*_limit` leaves are non-nil).
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
