// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// MCPServerDiscoverySpec defines the desired state of MCPServerDiscovery —
// the flat `_FINALv3` shape (spec §6.5). One MCPServerDiscovery CR points
// the operator at the cluster's ToolHive deployment (the only allowed
// `spec.type` value in v1alpha1) and generates a fan-out of Kubernetes
// MCPServer child CRs (Pipeline B per spec §3.3).
//
// Discovery NEVER calls LiteLLM directly; each generated child reconciles
// into LiteLLM via the Phase 5 MCPServer controller (Pipeline A). This
// mirrors the Phase 4 ModelDiscovery → Model relationship.
//
// MSDISC-01 narrows spec.type to {toolhive} via the Enum marker. MSDISC-04
// (no upstream-credential reference field — schema-level prohibition) is
// structurally enforced by the *absence* of any such field. MSDISC-05
// (refresh.interval 1m floor) is enforced via CEL XValidation on the
// resource root. MSDISC-14 (toolhive sub-block presence + minItems=1 on
// namespaces) is enforced at the schema level.
type MCPServerDiscoverySpec struct {
	// Prefix is the lowercase DNS-1123 label prepended to every
	// generated child MCPServer's metadata.name (final K8s shape:
	// `<prefix>-<source-name>`; final LiteLLM wire shape:
	// `<prefix>.<source-name>` — see internal/litellm/sanitize.go).
	// Mirrors LiteLLMModelDiscovery.spec.prefix exactly for
	// cross-discovery-kind symmetry.
	//
	// FIX4.txt H-2 (v0.3.0 breaking change): pre-v0.3.0 children were
	// named `<discovery-name>.<source-namespace>.<source-name>` (three
	// dotted components). v0.3.0 drops the source-namespace component
	// entirely; cross-discovery name disambiguation is the user's job
	// via this `prefix` field. The operator no longer auto-disambiguates
	// — name collisions inside a single discovery surface as a
	// `NameCollision=True` status condition and the second occurrence
	// is dropped (loud-fail, not silent-merge).
	//
	// MaxLength=30 caps the prefix, but the final child name is
	// `<prefix>-<source-name>` and the source name can itself be up to 63
	// chars, so the combined name can still exceed the 63-char DNS-1123 label
	// budget. The reconciler enforces the 63-char limit at child-name
	// construction (M-B7): over-budget candidates are skipped with
	// reason=InvalidDiscoveredName rather than failing K8s admission.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=30
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Prefix string `json:"prefix"`

	// Type discriminates the upstream Discovery source. In v1alpha1 the
	// ONLY allowed value is `toolhive`. The Enum marker below admits
	// nothing else; future Discovery sources (e.g. an A2A-side directory,
	// a Kubernetes-API-server-side scan) would expand this Enum without
	// breaking the existing schema (additive Enum values are non-breaking).
	//
	// MSDISC-01 enforces this at admission.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=toolhive
	Type string `json:"type"`

	// Toolhive carries the toolhive-specific configuration sub-block.
	// Required because in v1alpha1 `spec.type` is always `toolhive`; a
	// future discriminator-aware schema (when other Discovery sources
	// land) will gate this with a CEL XValidation rule similar to
	// ModelDiscovery's per-provider matrix.
	//
	// +kubebuilder:validation:Required
	Toolhive MCPServerDiscoveryToolhive `json:"toolhive"`

	// Params is a pass-through bag of fields PROPAGATED VERBATIM into
	// every generated child MCPServer's spec.params (AC-SEC4-PROPAGATE).
	// Discovery does NOT perform substitution itself; the Phase 5 MCPServer
	// reconciler substitutes them on the child's own reconcile (§5.2).
	//
	// Any JSON object is accepted (x-kubernetes-preserve-unknown-fields:
	// true). String-typed leaves may carry {{NAME}} placeholders matched
	// against spec.secrets[] on the child's reconcile.
	//
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Params runtime.RawExtension `json:"params,omitempty"`

	// Secrets is the substitution map PROPAGATED verbatim into every
	// generated child MCPServer's spec.secrets[] (AC-SEC4-PROPAGATE).
	// Discovery does NOT perform substitution itself — the propagated
	// entries ride along and the Phase 5 MCPServer reconciler substitutes
	// them on the child's own reconcile.
	//
	// MSDisc has NO field for upstream-source credentials — ToolHive
	// reads are authorized via the operator's cluster-scoped
	// ServiceAccount RBAC (config/rbac/toolhive_clusterrole.yaml) per
	// Phase 5 D-07. MSDISC-04 makes the schema-level prohibition
	// load-bearing — credentials for the upstream source and credentials
	// for inference MUST be expressed via DIFFERENT mechanisms (cluster
	// RBAC vs spec.secrets[]).
	//
	// +optional
	Secrets []SecretSubstitution `json:"secrets,omitempty"`

	// Filters narrows the post-derivation candidate set via RE2
	// include/exclude patterns matched against the generated child name
	// `<spec.prefix>-<source-name>` (v0.3.0 breaking change; pre-v0.3.0
	// used a dotted three-part name). Empty (absent) Filters means
	// "no filtering" (every discovered ToolHive object becomes a
	// candidate).
	//
	// Per spec §6.5: include FIRST (strict — empty match-set surfaces
	// as Ready=False, reason=UpstreamInvalid), then exclude (lenient —
	// empty match-set is fine). The reconciler in enforces
	// the order; this CRD type only carries the patterns.
	//
	// +optional
	Filters *MCPServerDiscoveryFilters `json:"filters,omitempty"`

	// Refresh controls the periodic ToolHive-list cadence. The
	// reconciler returns ctrl.Result{RequeueAfter: spec.refresh.interval}
	// on success (Phase 4 D-08 inherited). The CEL floor of 1 minute
	// (MSDISC-05) is enforced at the resource root via the
	// +kubebuilder:validation:XValidation marker on MCPServerDiscovery.
	//
	// +kubebuilder:validation:Required
	Refresh MCPServerDiscoveryRefresh `json:"refresh"`
}

// MCPServerDiscoveryToolhive carries the toolhive-source-specific
// configuration. Per Phase 5 D-06: the ToolHive API group is
// `toolhive.stacklok.dev/v1beta1` (NOT the autoconfig-divergent
// v1alpha1); the informer is cluster-scoped per kind (D-07);
// `Namespaces[]` is an in-memory filter on event handlers — no
// informer reconfig on live namespace-list change.
type MCPServerDiscoveryToolhive struct {
	// Namespaces enumerates the Kubernetes namespaces from which
	// ToolHive `MCPServer` / `VirtualMCPServer` objects should be
	// considered for discovery. ToolHive objects in OTHER namespaces
	// are silently ignored.
	//
	// MUST contain at least one entry (MinItems=1). Each entry MUST be
	// a non-empty DNS-1123-friendly string (MinLength=1); CRD-layer
	// validation does NOT enforce DNS-1123 format because the cluster's
	// own namespace creation already enforces that contract.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +listType=atomic
	Namespaces []string `json:"namespaces"`

	// Kinds enumerates the ToolHive resource kinds to watch. Defaults
	// to both `MCPServer` and `VirtualMCPServer` (per spec §6.5). A user
	// who wants Discovery limited to one kind (e.g. only `MCPServer`)
	// specifies a singleton list.
	//
	// Empty list is REJECTED at admission via the Enum constraint on
	// each item — but the default ensures the omitted-field case is
	// "watch both", not "watch nothing".
	//
	// +optional
	// +kubebuilder:default={MCPServer,VirtualMCPServer}
	// +kubebuilder:validation:items:Enum=MCPServer;VirtualMCPServer
	// +listType=atomic
	Kinds []string `json:"kinds,omitempty"`
}

// MCPServerDiscoveryFilters carries the RE2 include/exclude pattern lists
// applied to the generated child name `<spec.prefix>-<source-name>`
// (v0.3.0 breaking change; pre-v0.3.0 used a dotted three-part name).
// The filter target is the prefixed child name, NOT the bare ToolHive
// object name — the most common source of user confusion at runtime.
//
// RE2 compile validity is a RUNTIME concern (CEL has no regex-compile
// primitive). codes the compile + classification — invalid
// patterns surface as Ready=False, reason=InvalidConfig with a message
// naming the offending pattern.
type MCPServerDiscoveryFilters struct {
	// Include narrows the candidate set: a candidate dotted name is
	// admitted only if it matches at least one pattern in Include. Empty
	// (or absent) Include means "admit all". If Include is non-empty
	// and matches ZERO candidates, the reconcile surfaces Ready=False,
	// reason=UpstreamInvalid (operator-intent vs upstream-reality drift).
	//
	// +optional
	// +listType=atomic
	Include []string `json:"include,omitempty"`

	// Exclude removes candidates from the post-Include set: a candidate
	// is filtered out if it matches any pattern in Exclude. Empty (or
	// absent) Exclude means "exclude nothing". Exclude is forward-looking
	// defense — zero matches is fine (lenient semantics per spec §6.5).
	//
	// +optional
	// +listType=atomic
	Exclude []string `json:"exclude,omitempty"`
}

// MCPServerDiscoveryRefresh controls the periodic refresh cadence. The
// reconciler returns ctrl.Result{RequeueAfter: Interval} on every
// successful refresh. The 1-minute floor is enforced at the resource
// root via a +kubebuilder:validation:XValidation CEL rule (MSDISC-05):
// duration(self.spec.refresh.interval) >= duration('1m').
type MCPServerDiscoveryRefresh struct {
	// Interval is the cadence between two successive ToolHive-list
	// reconciles. metav1.Duration accepts kubectl-friendly strings like
	// "5m", "1h", "30m". CEL floor of 1m is enforced at admission.
	//
	// +kubebuilder:validation:Required
	Interval metav1.Duration `json:"interval"`
}

// MCPServerDiscoveryStatus defines the observed state of MCPServerDiscovery —
// the `_FINALv3` Pipeline B status surface. Mirrors ModelDiscoveryStatus
// with MCP-side renames (MCPServerSkippedCandidate / MCPServerFailedCandidate)
// to avoid Go name collision with the Phase 4 ModelDiscovery types in the
// same package.
//
// Spec §6.5 invariant:
//
//	discoveredCount == generatedCount
//	 + len(skippedCandidates)
//	 + len(failedCandidates)
//
// (Filtered-out names are NOT counted in discoveredCount.)
//
// Two condition types are written on every reconcile (each idempotent
// via apimeta.SetStatusCondition):
//
//	Ready — top-level readiness with reasons:
//	 {Synced, SourceUnreachable, SecretNotFound,
//	 InvalidConfig, UpstreamInvalid}
//	 NOTE: LiteLLMUnavailable / LiteLLMRejected are
//	 NOT Discovery-level reasons (Discovery never
//	 calls LiteLLM — MSDISC-16). Those reasons surface
//	 on the child MCPServer's status.
//
//	SourceReachable — ToolHive-list reachability with reasons:
//	 {Ok, Unreachable}
//	 Used as the gate for vanish-detection (Phase 4
//	 D-09 inherited): diff-and-delete is skipped when
//	 this is False.
type MCPServerDiscoveryStatus struct {
	// ObservedGeneration is the metadata.generation of the
	// MCPServerDiscovery the reconciler most recently processed
	// successfully (OWN-08 carry-forward).
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions carries the metav1.Condition list. Two condition types
	// are populated on every reconcile:
	//
	// Ready — reasons: Synced, SourceUnreachable,
	// SecretNotFound, InvalidConfig, UpstreamInvalid.
	// LiteLLMUnavailable / LiteLLMRejected are NOT
	// valid here (MSDISC-16).
	//
	// SourceReachable — reasons: Ok, Unreachable.
	// The atomic-refresh-snapshot vanish guard
	// (Phase 4 D-09 inherited) gates diff-and-delete
	// on SourceReachable=True.
	//
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// DiscoveredCount is the size of the post-filter candidate set on
	// the most recent successful refresh (filtered-out IDs are NOT
	// counted). Maintains the invariant noted on
	// MCPServerDiscoveryStatus's godoc above.
	//
	// +optional
	// +kubebuilder:default=0
	DiscoveredCount int32 `json:"discoveredCount"`

	// GeneratedCount is len(GeneratedChildren) — the number of child
	// MCPServer CRs the Discovery currently owns (SSA-applied with
	// ownerReferences[controller=true, blockOwnerDeletion=true] +
	// labels[litellm.ackstorm.ai/generated-by=<this>]).
	//
	// +optional
	// +kubebuilder:default=0
	GeneratedCount int32 `json:"generatedCount"`

	// GeneratedChildren lists the metadata.name of every owned child
	// MCPServer CR (sorted for deterministic kubectl get -o yaml
	// output). On the next reconcile, the reconciler uses a label-
	// selector (litellm.ackstorm.ai/generated-by=<this>) for ACTUAL
	// ownership enumeration; this list is a status echo for human
	// inspection, not the source of truth.
	//
	// +optional
	GeneratedChildren []string `json:"generatedChildren,omitempty"`

	// SkippedCandidates records candidates that were NOT generated as
	// child MCPServers because of K8s-native conflict resolution OR
	// because of a Discovery-side validation skip. Each entry names
	// the candidate and the reason.
	//
	// Reason enum per spec §6.5 + Phase 5 D-10 (exhaustive):
	// ExplicitMCPServerExists — a child with the same name already
	// exists and its controller ownerRef
	// does NOT point at this Discovery.
	// Conflict — a child with the same name exists and
	// is owned by a DIFFERENT Discovery.
	// OwnedBy names the winner. Renamed from
	// `DuplicateDiscovery` for cross-kind
	// consistency (ADR-0001).
	// EndpointUnknown — the ToolHive object has empty/absent
	// status.url (MSDISC-12).
	// InvalidTransport — the ToolHive object's status.transport
	// value is not in the normalization map
	// (`streamable-http → http`, `sse → sse`,
	// absent → `http`). Anything else
	// (e.g. `stdio`, `unknown`, custom) is
	// skipped. Added per Phase 5 D-10.
	//
	// +optional
	SkippedCandidates []MCPServerSkippedCandidate `json:"skippedCandidates,omitempty"`

	// FailedCandidates records candidates whose SSA write to the K8s
	// apiserver failed for a reason other than name collision. Each
	// entry names the candidate and the reason.
	//
	// Reason enum (single-valued under `_FINALv3`):
	// ChildCRWriteFailed — the K8s apiserver rejected the SSA patch
	// (server timeout, rate-limit, service
	// unavailable, SSA field conflict, etc.).
	//
	// +optional
	FailedCandidates []MCPServerFailedCandidate `json:"failedCandidates,omitempty"`

	// LastRefreshAt is the timestamp of the most recent SUCCESSFUL
	// ToolHive-list reconcile (NOT every reconcile attempt — transient
	// failures do not update this field). Mirrors Phase 4
	// ModelDiscoveryStatus.LastRefreshAt semantics.
	//
	// +optional
	LastRefreshAt *metav1.Time `json:"lastRefreshAt,omitempty"`
}

// MCPServerSkippedCandidate records a candidate that was NOT generated
// as a child MCPServer due to K8s-native conflict resolution or a
// Discovery-side validation skip. The Reason enum is exhaustive per spec
// §6.5 + Phase 5 D-10 (which added the `InvalidTransport` value).
//
// The MCPServer- prefix on the Go type avoids a name collision with the
// Phase 4 ModelDiscovery `SkippedCandidate` type in the same package.
type MCPServerSkippedCandidate struct {
	// Name is the candidate child name `<spec.prefix>-<source-name>`
	// that would have become the child MCPServer's metadata.name
	// (v0.3.0 breaking change; pre-v0.3.0 used a dotted three-part name).
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Reason classifies the skip. Exhaustive enum per spec §6.5 +
	// Phase 5 D-10:
	// ExplicitMCPServerExists — name collides with a user-authored
	// MCPServer (no controller ownerRef
	// back at this Discovery).
	// Conflict — name collides with a child owned by
	// a different MCPServerDiscovery.
	// OwnedBy names the winning Discovery
	// (<Kind>/<Name>/<UID>). Renamed from
	// `DuplicateDiscovery` for cross-kind
	// consistency (ADR-0001).
	// EndpointUnknown — ToolHive object's status.url is empty
	// or absent (MSDISC-12).
	// InvalidTransport — ToolHive object's status.transport
	// value is not in the normalization
	// map; the candidate is dropped to
	// avoid emitting an MCPServer with a
	// transport that fails CEL admission
	// on the child CR (CRD enum is
	// {http, sse}; ToolHive may emit
	// `streamable-http`, `sse`, `stdio`,
	// custom strings).
	// NameCollision — two upstream ToolHive objects from
	// different namespaces produced the same
	// `<spec.prefix>-<source-name>` child name
	// within a single discovery. Alpha-last-wins
	// (ADR-0001) — the entry with the alpha-LAST
	// `(sourceNamespace, sourceName)` ASC key
	// survives; earlier occurrences are skipped.
	// Rename one upstream or split the discovery
	// into prefix-distinct ones to resolve.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=ExplicitMCPServerExists;Conflict;EndpointUnknown;InvalidTransport;NameCollision
	Reason string `json:"reason"`

	// OwnedBy is the <namespace>/<name> of the MCPServerDiscovery
	// winning a Conflict collision — or the explicit MCPServer owner
	// for ExplicitMCPServerExists. Empty for EndpointUnknown and
	// InvalidTransport (no collision — the candidate's own data was
	// rejected).
	//
	// +optional
	OwnedBy string `json:"ownedBy,omitempty"`

	// Message is a free-form diagnostic. Per §9.1, MUST NOT contain
	// secret material (the operator only handles ToolHive metadata
	// fields, not secret payloads, so this is structurally easy to
	// satisfy).
	//
	// +optional
	Message string `json:"message,omitempty"`
}

// MCPServerFailedCandidate records a candidate whose K8s apiserver write
// (SSA patch) failed for a non-collision reason. Single-valued enum per
// `_FINALv3`: Discovery never calls LiteLLM, so LiteLLM-side reasons are
// NOT valid here.
//
// The MCPServer- prefix on the Go type avoids a name collision with the
// Phase 4 ModelDiscovery `FailedCandidate` type in the same package.
type MCPServerFailedCandidate struct {
	// Name is the candidate dotted three-part name
	// (`<discovery-name>.<toolhive-namespace>.<toolhive-name>`).
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Reason classifies the failure. Single-valued enum: only
	// ChildCRWriteFailed is valid; LiteLLM-side reasons surface on
	// the child MCPServer's status instead.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=ChildCRWriteFailed
	Reason string `json:"reason"`

	// Message is a free-form diagnostic.
	//
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=msdisc,categories=litellm
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=".spec.type"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].reason"
// +kubebuilder:printcolumn:name="Discovered",type=integer,JSONPath=".status.discoveredCount"
// +kubebuilder:printcolumn:name="Generated",type=integer,JSONPath=".status.generatedCount"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
// +kubebuilder:validation:XValidation:rule="self.spec.type == 'toolhive'",message="spec.type must be 'toolhive' in v1alpha1"
// +kubebuilder:validation:XValidation:rule="duration(self.spec.refresh.interval) >= duration('1m')",message="spec.refresh.interval must be >= 1m"

// LiteLLMMCPServerDiscovery is the Schema for the litellmmcpserverdiscoveries API — the
// second Pipeline B CRD in the operator (after LiteLLMModelDiscovery). It points
// the operator at the cluster's ToolHive deployment and reconciles
// discovered ToolHive `MCPServer` / `VirtualMCPServer` objects into a
// fan-out of Kubernetes LiteLLMMCPServer child CRs in WATCH_NAMESPACE.
//
// Discovery NEVER calls LiteLLM directly; each child reconciles into
// LiteLLM via the Phase 5 LiteLLMMCPServer controller (Pipeline A).
//
// The two CR-level XValidation rules above enforce:
// - Defensive Type == 'toolhive' (Enum already enforces this; the CEL
// rule documents intent and survives any future Enum expansion).
// - MSDISC-05: refresh.interval >= 1m floor.
//
// Per Phase 5 D-04 (and MSDISC-04 specifically): MCPServerDiscovery has
// NO upstream-credential reference field. ToolHive reads are authorized
// via the operator's cluster-scoped ServiceAccount RBAC (Phase 5 D-07,
// config/rbac/toolhive_clusterrole.yaml). This is a SCHEMA-LEVEL
// prohibition — the field is structurally absent.
//
// The Discovery finalizer is `mcpserverdiscoveries.litellm.ackstorm.ai/
// finalizer`. It issues NO LiteLLM call — its only work is waiting for
// owned children to drain via blockOwnerDeletion=true cascade, then
// removing itself. Each child MCPServer's own finalizer issues
// `DELETE /v1/mcp/server/<server_id>` on the LiteLLM side (Phase 5 plan
// 05-01 mcpserver_controller.go).
type LiteLLMMCPServerDiscovery struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MCPServerDiscoverySpec   `json:"spec,omitempty"`
	Status MCPServerDiscoveryStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LiteLLMMCPServerDiscoveryList contains a list of LiteLLMMCPServerDiscovery.
type LiteLLMMCPServerDiscoveryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LiteLLMMCPServerDiscovery `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LiteLLMMCPServerDiscovery{}, &LiteLLMMCPServerDiscoveryList{})
}
