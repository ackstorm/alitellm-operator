// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ModelDiscoverySpec defines the desired state of ModelDiscovery — the
// flat _FINALv3 shape (spec §6.3). One ModelDiscovery CR points the
// operator at a single upstream provider (anthropic, bedrock, gemini,
// kubeai, or openai) and generates a fan-out of Kubernetes Model child
// CRs (Pipeline B per spec §3.3). Discovery NEVER calls LiteLLM directly;
// each generated child reconciles into LiteLLM via the Phase 3 Model
// controller (Pipeline A).
//
// Provider field matrix (CR-level CEL XValidation, see markers on the
// ModelDiscovery struct below) per spec §6.3 provider table:
//
//	anthropic — requires credentialsSecretRef; forbids region, baseUrl.
//	bedrock — requires region; forbids baseUrl; credentialsSecretRef optional.
//	gemini — requires credentialsSecretRef; forbids region, baseUrl.
//	kubeai — requires baseUrl; forbids credentialsSecretRef, region.
//	openai — requires credentialsSecretRef; baseUrl optional; forbids region.
//
// MDISC-01 enforces spec.type ∈ {anthropic, bedrock, gemini, kubeai, openai}
// at admission via the +kubebuilder:validation:Enum marker. MDISC-04
// (prefix), MDISC-05 (refresh.interval floor), MDISC-15 (credential
// surface), and MDISC-22/23 (propagation bags) are all schema-side.
type ModelDiscoverySpec struct {
	// Type discriminates the upstream provider. Enforced at admission via
	// the +kubebuilder:validation:Enum marker (MDISC-01). The reconciler
	// dispatches to internal/providers/<type>.go via the registry; per-type
	// branching outside the registry is prohibited (CONTEXT.md D-01).
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=anthropic;bedrock;gemini;kubeai;openai
	Type string `json:"type"`

	// Prefix is the lowercase DNS-1123 segment prepended to each
	// discovered ID when generating the child Model's metadata.name
	// (final shape: <prefix>.<normalized-id>). The prefix is OPTIONAL at
	// the CRD layer; the reconciler defaults it to lowercased(spec.type)
	// at reconcile time (MDISC-04). The default is NOT a CRD-layer default
	// — keeping the schema thin lets the reconciler own the substitution
	// (matches spec §6.3 prefix semantics line 689-878).
	//
	// Pattern is the DNS-1123 subdomain segment: lowercase alphanumerics
	// with internal hyphens and optional dotted sub-segments. MaxLength=63
	// matches the K8s DNS label budget; the generated child name's full
	// length is validated again at reconcile time against DNS-1123
	// subdomain (253 chars) — see normalization step.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Prefix string `json:"prefix,omitempty"`

	// DisablePrefix opts the Discovery out of the per-provider name prefix
	// entirely. When false (the default), the reconciler prepends
	// <prefix>. to every generated child Model name, where prefix is
	// spec.prefix or — when that is empty — lowercased(spec.type)
	// (MDISC-04). When true, the generated child Model's metadata.name (and
	// therefore the LiteLLM public model_name) is the bare normalized
	// discovered ID with NO prefix segment — e.g. claude-fable-5 instead of
	// anthropic.claude-fable-5.
	//
	// SETTING THIS IS A NAME-COLLISION RISK: the prefix exists to namespace
	// children per-provider so two Discovery CRs cannot collide on a child
	// CR name (and a child cannot collide with a hand-written LiteLLMModel).
	// With DisablePrefix=true, the operator no longer guarantees that
	// separation — a collision surfaces as an SSA conflict /
	// ExplicitModelExists skip on the losing Discovery. Safe for a single
	// Discovery whose normalized IDs are known-unique; otherwise leave it
	// false. Mutually exclusive with a non-empty spec.prefix (CEL-enforced).
	//
	// +optional
	DisablePrefix bool `json:"disablePrefix,omitempty"`

	// CredentialsSecretRef points to the Kubernetes Secret carrying the
	// upstream provider's API credentials. Required for anthropic, gemini,
	// openai; required-or-default-chain for bedrock; FORBIDDEN for kubeai
	// (the kubeai provider runs in-cluster without auth — see CONTEXT.md
	// <specifics> line 278). The Secret MUST reside in the same namespace
	// as the ModelDiscovery CR (no cross-namespace resolution in v1alpha1).
	//
	// Required Secret keys per provider (spec §6.3 line 721-737 normative):
	// anthropic: ANTHROPIC_API_KEY
	// bedrock: AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY (AWS_SESSION_TOKEN optional)
	// gemini: GEMINI_API_KEY (or GOOGLE_API_KEY)
	// openai: OPENAI_API_KEY
	// kubeai: n/a (no credentialsSecretRef)
	//
	// MDISC-15 is non-negotiable: the credential material is operator-side
	// ONLY. The reconciler MUST NOT propagate any value from this Secret
	// into the generated child Model's spec.params / spec.info /
	// spec.secrets[]. Inference-time credentials flow via spec.secrets[]
	// (the propagation bag), NOT via this field.
	//
	// Discovery uses NEW SecretObjectRef (single Name field) — NOT the
	// SecretKeyRef{Name, Key} used by LiteLLMConnection and Model. The
	// Secret keys are fixed per provider per the normative table above,
	// so the user does not pick a per-key lookup; only the Secret's name
	// is parameterized.
	//
	// +optional
	CredentialsSecretRef *SecretObjectRef `json:"credentialsSecretRef,omitempty"`

	// Region is the AWS region for Bedrock control-plane discovery
	// (required when spec.type=bedrock, forbidden otherwise — see the
	// CR-level CEL rule on the ModelDiscovery struct). One region per
	// CR per PROJECT.md Key Decision; multi-region requires multiple CRs
	// with distinct spec.prefix (e.g. bedrock-use1, bedrock-euw1).
	//
	// The value is overlaid as aws_region_name in each generated child
	// Model's spec.params. This is one of two typed-field overlays the
	// reconciler applies per CONTEXT.md D-07: bedrock spec.region →
	// aws_region_name (overwrite-wins) and kubeai spec.baseUrl →
	// api_base (user-supplied wins; see BaseURL doc, FIX.txt H-2). Plain
	// string — AWS region codes are open-ended and not enumerated here;
	// CEL gates presence per provider.
	//
	// +optional
	Region string `json:"region,omitempty"`

	// BaseURL is the upstream provider's base endpoint. Required for
	// kubeai (e.g. "http://kubeai.kubeai.svc/openai/v1"); optional for
	// openai (default OpenAI-platform endpoint applies on omit); forbidden
	// for anthropic, bedrock, gemini.
	//
	// Discovery calls <BaseURL>/models (OpenAI-compatible wire shape) for
	// kubeai + openai variants. For OpenAI-compatible providers (vLLM,
	// Together, Groq, OpenRouter) the user sets spec.type=openai and
	// spec.baseUrl=<provider URL>; the per-request Bearer key comes from
	// spec.credentialsSecretRef. No URL pattern is enforced at the CRD
	// layer; CEL only gates presence/absence per provider type.
	//
	// kubeai-only typed-field overlay (D-07, FIX.txt H-2 2026-05-22):
	// when spec.type=kubeai, the reconciler also overlays
	// spec.baseUrl → spec.params.api_base on each generated child Model,
	// so the LiteLLM proxy can route hosted_vllm/<id> inference requests
	// at runtime. User-supplied params.api_base wins over the auto-overlay
	// (presence check). Diverges from the bedrock region overlay's
	// overwrite-wins semantics on purpose: api_base is a legitimate per-
	// child routing override.
	//
	// +optional
	BaseURL string `json:"baseUrl,omitempty"`

	// Params is a pass-through bag of fields propagated VERBATIM into
	// every generated child Model's spec.params (MDISC-23). On top of this
	// bag, the Discovery reconciler overlays two typed fields per child:
	// - model: "<litellm-provider>/<raw-id>" (e.g. "anthropic/claude-3-5-sonnet-20241022")
	// - aws_region_name: <spec.region> (bedrock only)
	// All other keys are forwarded unchanged. {{NAME}} substitution
	// happens on the child Model's own reconcile (§5.2 propagation rule
	// per AC-SEC4-PROPAGATE), NOT on Discovery's reconcile.
	//
	// Any JSON object is accepted (x-kubernetes-preserve-unknown-fields:
	// true). String-typed leaves may carry {{NAME}} placeholders matched
	// against spec.secrets[] on the child's reconcile.
	//
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Params runtime.RawExtension `json:"params,omitempty"`

	// Info is a pass-through bag of fields propagated VERBATIM into every
	// generated child Model's spec.info (MDISC-23). The Discovery
	// reconciler does NOT overlay any field here — the child's own
	// reconciler handles the model_info.id overlay (D-04 in Phase 3).
	//
	// Any JSON object is accepted (x-kubernetes-preserve-unknown-fields:
	// true). Same {{NAME}} substitution semantics as Params apply on the
	// child's reconcile.
	//
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Info runtime.RawExtension `json:"info,omitempty"`

	// Secrets is the substitution map PROPAGATED verbatim into every
	// generated child Model's spec.secrets[] (MDISC-23). Discovery does
	// NOT perform substitution itself — the propagated entries ride along
	// and the Phase 3 Model reconciler substitutes them on the child's
	// own reconcile (AC-SEC4-PROPAGATE).
	//
	// MDISC-15 enforces a STRICT boundary with spec.credentialsSecretRef:
	// credentials used to call the upstream provider's discovery endpoint
	// are NEVER reused for inference. Users who need an inference-time
	// secret declare it here independently of credentialsSecretRef. A
	// post-render canary asserts no credentialsSecretRef material appears
	// in any generated child's rendered fields (CONTEXT.md anti-pattern
	// line 1021).
	//
	// SEC-03 uniqueness of spec.secrets[].as values is enforced as a
	// runtime check on the child Model's reconcile;
	// the CEL admission alternative is deferred per the same v1alpha1
	// limitation documented in model_types.go:87-93.
	//
	// +optional
	Secrets []SecretSubstitution `json:"secrets,omitempty"`

	// Filters narrows the post-provider-list candidate set via RE2
	// include/exclude patterns matched against the raw provider-returned
	// ID. Empty (absent) Filters means "no filtering" (all provider IDs
	// become candidates). Per spec §6.3: include FIRST (strict — empty
	// match-set surfaces as Ready=False, reason=UpstreamInvalid), then
	// exclude (lenient — empty match-set is fine).
	//
	// Filter-order divergence from autoconfig is intentional: autoconfig
	// applies exclude first then include (src/generator.py:324); the spec
	// mandates include first then exclude. **Spec wins** — see
	// CONTEXT.md D-11 line 118 and PATTERNS.md anti-pattern line 1031.
	// Codes this and ships a regression test exercising the
	// order with overlapping patterns.
	//
	// +optional
	Filters *ModelDiscoveryFilters `json:"filters,omitempty"`

	// Refresh controls the periodic provider-list cadence. The
	// reconciler returns ctrl.Result{RequeueAfter: spec.refresh.interval}
	// on success (D-08); transient errors short-circuit through the
	// controller-runtime workqueue exponential backoff (REL-02 pattern).
	// The CEL floor of 1 minute (MDISC-05) is enforced at the resource
	// root, see the +kubebuilder:validation:XValidation marker on
	// ModelDiscovery.
	//
	// +kubebuilder:validation:Required
	Refresh ModelDiscoveryRefresh `json:"refresh"`
}

// SecretObjectRef is the v1alpha1 ModelDiscovery-only shape for
// referencing a Kubernetes Secret by Name (NO Key field). The Secret
// MUST reside in the same namespace as the referring CR.
//
// Diverges intentionally from SecretKeyRef{Name, Key} (used by
// LiteLLMConnection.masterKeySecretRef and Model.spec.secrets[]
// SecretSubstitution.SecretRef): the keys are FIXED per provider per
// spec §6.3 (e.g. ANTHROPIC_API_KEY for anthropic, AWS_ACCESS_KEY_ID
// + AWS_SECRET_ACCESS_KEY for bedrock), so the user does not pick a
// per-key lookup; only the Secret's name is parameterized. See
// CONTEXT.md <canonical_refs> line 227 and PATTERNS.md line 104.
type SecretObjectRef struct {
	// Name of the Kubernetes Secret resource in the ModelDiscovery's
	// namespace. Required and non-empty.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// ModelDiscoveryFilters carries the RE2 include/exclude pattern lists
// applied to the raw provider-returned candidate IDs. Empty (absent)
// means "no filtering"; an empty slice on either side is identical to
// absent. Per spec §6.3, both lists use RE2 syntax and are
// anchored-from-start (the autoconfig matches_any semantics).
//
// RE2 compile validity is a RUNTIME concern (CEL has no regex-compile
// primitive). codes the compile + classification — invalid
// patterns surface as Ready=False, reason=InvalidConfig with a message
// naming the offending pattern.
type ModelDiscoveryFilters struct {
	// Include narrows the candidate set: a candidate ID is admitted only
	// if it matches at least one pattern in Include. Empty (or absent)
	// Include means "admit all" (no narrowing). If Include is non-empty
	// and matches ZERO provider IDs, the reconcile surfaces Ready=False,
	// reason=UpstreamInvalid (operator-intent vs upstream-reality drift).
	//
	// +optional
	// +listType=atomic
	Include []string `json:"include,omitempty"`

	// Exclude removes candidates from the post-Include set: a candidate
	// is filtered out if it matches any pattern in Exclude. Empty (or
	// absent) Exclude means "exclude nothing". Exclude is forward-looking
	// defense — zero matches is fine (lenient semantics per spec §6.3).
	//
	// +optional
	// +listType=atomic
	Exclude []string `json:"exclude,omitempty"`
}

// ModelDiscoveryRefresh controls the periodic refresh cadence. The
// reconciler returns ctrl.Result{RequeueAfter: Interval} on every
// successful refresh (D-08). The 1-minute floor is enforced at the
// resource root via a +kubebuilder:validation:XValidation CEL rule
// (MDISC-05): duration(self.spec.refresh.interval) >= duration('1m').
type ModelDiscoveryRefresh struct {
	// Interval is the cadence between two successive provider-list
	// reconciles. metav1.Duration accepts kubectl-friendly strings like
	// "5m", "1h", "30m". CEL floor of 1m is enforced at admission.
	//
	// +kubebuilder:validation:Required
	Interval metav1.Duration `json:"interval"`
}

// ModelDiscoveryStatus defines the observed state of ModelDiscovery —
// the _FINALv3 status surface (MDISC-26, renamed from _FINALv2's
// "registeredNames[]" vocabulary to "generatedChildren[]" to reflect
// the Pipeline B K8s-child-CR-emission model).
//
// Spec §6.3 invariant:
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
//	 {Synced, SourceUnreachable, AuthFailed,
//	 SecretNotFound, InvalidConfig, UpstreamInvalid}
//	 NOTE: LiteLLMUnavailable and LiteLLMRejected are
//	 NOT Discovery-level reasons (MDISC-27 — Discovery
//	 never calls LiteLLM). Those reasons surface on the
//	 child Model's status.
//
//	SourceReachable — provider-list reachability with reasons:
//	 {Ok, Unreachable, AuthFailed}
//	 Used as the gate for vanish-detection (D-09): the
//	 diff-and-delete step is skipped when this is False.
type ModelDiscoveryStatus struct {
	// ObservedGeneration is the metadata.generation of the ModelDiscovery
	// the reconciler most recently processed successfully (OWN-08).
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions carries the metav1.Condition list. Two condition types
	// are populated on every reconcile:
	//
	// Ready — reasons: Synced, SourceUnreachable, AuthFailed,
	// SecretNotFound, InvalidConfig, UpstreamInvalid.
	// LiteLLMUnavailable and LiteLLMRejected are
	// NOT valid here (MDISC-27 + spec §6.0).
	//
	// SourceReachable — reasons: Ok, Unreachable, AuthFailed.
	// The atomic-refresh-snapshot vanish guard
	// (D-09) gates diff-and-delete on
	// SourceReachable=True.
	//
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// DiscoveredCount is the size of the post-filter candidate set on
	// the most recent successful refresh (filtered-out IDs are NOT
	// counted). Maintains the invariant noted on ModelDiscoveryStatus's
	// godoc above.
	//
	// Always serialized (value type, defaults to 0). The +optional marker
	// only relaxes CRD required-field validation; it is never absent.
	// +optional
	// +kubebuilder:default=0
	DiscoveredCount int32 `json:"discoveredCount"`

	// GeneratedCount is len(GeneratedChildren) — the number of child
	// Model CRs the Discovery currently owns (SSA-applied with
	// ownerReferences[controller=true, blockOwnerDeletion=true] +
	// labels[litellm.ackstorm.ai/generated-by=<this>]).
	//
	// Always serialized (value type, defaults to 0). The +optional marker
	// only relaxes CRD required-field validation; it is never absent.
	// +optional
	// +kubebuilder:default=0
	GeneratedCount int32 `json:"generatedCount"`

	// GeneratedChildren lists the metadata.name of every owned child
	// Model CR (sorted for deterministic kubectl get -o yaml output).
	// On the next reconcile, the reconciler uses a label-selector
	// (litellm.ackstorm.ai/generated-by=<this>) for ACTUAL ownership
	// enumeration; this list is a status echo for human inspection, not
	// the source of truth.
	//
	// +optional
	GeneratedChildren []string `json:"generatedChildren,omitempty"`

	// SkippedCandidates records candidates that were NOT generated as
	// child Models because of K8s-native conflict resolution. Each entry
	// names the candidate and the reason.
	//
	// Reason enum (spec §6.3 line 870 normative — exhaustive):
	// ExplicitModelExists — a child with the same name already exists
	// and its controller ownerRef does NOT point
	// at this Discovery (MDISC-14).
	// Conflict — a child with the same name exists and is
	// owned by a DIFFERENT ModelDiscovery
	// (MDISC-13). OwnedBy names the winner.
	// Renamed from `DuplicateDiscovery` for
	// cross-kind consistency (ADR-0001).
	// InvalidDiscoveredName — the candidate's normalized name failed
	// DNS-1123 subdomain validation (MDISC-11).
	//
	// +optional
	SkippedCandidates []SkippedCandidate `json:"skippedCandidates,omitempty"`

	// FailedCandidates records candidates whose SSA write to the K8s
	// apiserver failed for a reason other than name collision. Each
	// entry names the candidate and the reason.
	//
	// Reason enum (spec §6.3 + MDISC-26 _FINALv3 narrowing):
	// ChildCRWriteFailed — the K8s apiserver rejected the SSA patch
	// (server timeout, rate-limit, service
	// unavailable, SSA field conflict, etc.).
	//
	// _FINALv3 narrowed this enum to a SINGLE value (MDISC-26 / D-10):
	// LiteLLMRejected and LiteLLMUnavailable are NOT valid here because
	// Discovery never calls LiteLLM (MDISC-27). Those reasons surface on
	// the child Model's status instead.
	//
	// +optional
	FailedCandidates []FailedCandidate `json:"failedCandidates,omitempty"`

	// LastRefreshAt is the timestamp of the most recent SUCCESSFUL
	// provider-list reconcile (NOT every reconcile attempt — transient
	// failures do not update this field). Mirrors Phase 3's
	// LastRendered.At pattern (model_types.go:237-243).
	//
	// +optional
	LastRefreshAt *metav1.Time `json:"lastRefreshAt,omitempty"`
}

// SkippedCandidate records a candidate that was NOT generated as a
// child Model due to K8s-native conflict resolution (MDISC-13 / 14 / 11).
// The Reason enum is exhaustive per spec §6.3 line 870.
type SkippedCandidate struct {
	// Name is the normalized candidate name (post-prefix +
	// normalization, the value that would have become the child Model's
	// metadata.name).
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Reason classifies the skip. Exhaustive enum per spec §6.3:
	// ExplicitModelExists — name collides with a user-authored Model
	// (no controller ownerRef back at this
	// Discovery) — MDISC-14.
	// Conflict — name collides with a child owned by a
	// different ModelDiscovery — MDISC-13.
	// OwnedBy names the winner
	// (<Kind>/<Name>/<UID>). Renamed from
	// `DuplicateDiscovery` for cross-kind
	// consistency (ADR-0001).
	// InvalidDiscoveredName — normalized name failed DNS-1123 subdomain
	// validation — MDISC-11.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=ExplicitModelExists;Conflict;InvalidDiscoveredName
	Reason string `json:"reason"`

	// OwnedBy is the <namespace>/<name> of the ModelDiscovery winning a
	// Conflict collision. Empty for ExplicitModelExists (no Discovery
	// owns the conflicting child) and InvalidDiscoveredName (no
	// collision — the candidate's own name was rejected).
	//
	// +optional
	OwnedBy string `json:"ownedBy,omitempty"`

	// Message is a free-form diagnostic. Per §9.1, MUST NOT contain
	// secret material (no leaked AWS keys, no Anthropic API keys, no
	// Bearer tokens — the post-render canary asserts this).
	//
	// +optional
	Message string `json:"message,omitempty"`
}

// FailedCandidate records a candidate whose K8s apiserver write
// (Server-Side Apply patch) failed for a non-collision reason. The
// Reason enum is intentionally single-valued in _FINALv3: Discovery
// never calls LiteLLM, so LiteLLMRejected / LiteLLMUnavailable are NOT
// Discovery-level reasons (MDISC-26 / D-10). See CONTEXT.md
// <specifics> line 284 and PATTERNS.md line 1037.
type FailedCandidate struct {
	// Name is the normalized candidate name (post-prefix +
	// normalization, the value that would have become the child Model's
	// metadata.name).
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Reason classifies the failure. Single-valued enum per _FINALv3
	// (MDISC-26): only ChildCRWriteFailed is valid; LiteLLMRejected and
	// LiteLLMUnavailable have been retired from Discovery-level reasons
	// (MDISC-27 — Discovery never calls LiteLLM; those reasons surface
	// on the child Model's status instead).
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=ChildCRWriteFailed
	Reason string `json:"reason"`

	// Message is a free-form diagnostic. Per §9.1, MUST NOT contain
	// secret material — AWS error strings are sanitized via the
	// reconciler's sanitizeAWSError helper before surfacing here.
	//
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=mdisc,categories=litellm
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=".spec.type"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].reason"
// +kubebuilder:printcolumn:name="Discovered",type=integer,JSONPath=".status.discoveredCount"
// +kubebuilder:printcolumn:name="Generated",type=integer,JSONPath=".status.generatedCount"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
// +kubebuilder:validation:XValidation:rule="self.spec.type != 'anthropic' || (has(self.spec.credentialsSecretRef) && !has(self.spec.region) && !has(self.spec.baseUrl))",message="anthropic requires spec.credentialsSecretRef and forbids spec.region/spec.baseUrl"
// +kubebuilder:validation:XValidation:rule="self.spec.type != 'bedrock' || (has(self.spec.region) && !has(self.spec.baseUrl))",message="bedrock requires spec.region and forbids spec.baseUrl"
// +kubebuilder:validation:XValidation:rule="self.spec.type != 'gemini' || (has(self.spec.credentialsSecretRef) && !has(self.spec.region) && !has(self.spec.baseUrl))",message="gemini requires spec.credentialsSecretRef and forbids spec.region/spec.baseUrl"
// +kubebuilder:validation:XValidation:rule="self.spec.type != 'kubeai' || (has(self.spec.baseUrl) && !has(self.spec.credentialsSecretRef) && !has(self.spec.region))",message="kubeai requires spec.baseUrl and forbids spec.credentialsSecretRef/spec.region"
// +kubebuilder:validation:XValidation:rule="self.spec.type != 'openai' || (has(self.spec.credentialsSecretRef) && !has(self.spec.region))",message="openai requires spec.credentialsSecretRef and forbids spec.region"
// +kubebuilder:validation:XValidation:rule="duration(self.spec.refresh.interval) >= duration('1m')",message="spec.refresh.interval must be >= 1m"
// +kubebuilder:validation:XValidation:rule="!(has(self.spec.disablePrefix) && self.spec.disablePrefix) || !has(self.spec.prefix)",message="spec.prefix and spec.disablePrefix are mutually exclusive"

// LiteLLMModelDiscovery is the Schema for the litellmmodeldiscoveries API — the
// first Pipeline B CRD (spec §3.3 / §7.1, _FINALv3 two-pipeline model).
// A LiteLLMModelDiscovery CR points the operator at one upstream provider
// (anthropic, bedrock, gemini, kubeai, or openai) and reconciles
// discovered IDs into a fan-out of Kubernetes LiteLLMModel child CRs in
// WATCH_NAMESPACE. Discovery NEVER calls LiteLLM directly; each child
// reconciles into LiteLLM via the Phase 3 LiteLLMModel controller.
//
// The six CR-level XValidation rules above enforce the per-type
// required/forbidden field matrix from spec §6.3 (provider table) plus
// the MDISC-05 refresh-interval 1-minute floor. SEC-03 list-uniqueness
// for spec.secrets[].as is deferred to the child LiteLLMModel's runtime check
// (same Kubernetes 1.31 CEL-library limitation documented in
// model_types.go:87-93 — reuse the same runtime check from).
//
// MDISC-22 — required Secret keys per provider are FIXED per spec §6.3:
//
//	anthropic: ANTHROPIC_API_KEY
//	bedrock: AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY (AWS_SESSION_TOKEN optional)
//	gemini: GEMINI_API_KEY (or GOOGLE_API_KEY per provider docs)
//	openai: OPENAI_API_KEY
//	kubeai: n/a (no credentialsSecretRef)
//
// The reconciler validates required keys at credential-resolution time
// and surfaces SecretNotFound if any required key is missing.
//
// The Discovery finalizer is "modeldiscoveries.litellm.ackstorm.ai/
// finalizer" (mirrors Phase 3's models.litellm.ackstorm.ai/finalizer).
// It issues NO LiteLLM call — its only work is waiting for owned
// children to drain via blockOwnerDeletion=true cascade, then removing
// itself. Each child LiteLLMModel's own finalizer issues POST /model/delete on
// the LiteLLM side (Phase 3 model_controller.go).
type LiteLLMModelDiscovery struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelDiscoverySpec   `json:"spec,omitempty"`
	Status ModelDiscoveryStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LiteLLMModelDiscoveryList contains a list of LiteLLMModelDiscovery.
type LiteLLMModelDiscoveryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LiteLLMModelDiscovery `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LiteLLMModelDiscovery{}, &LiteLLMModelDiscoveryList{})
}
