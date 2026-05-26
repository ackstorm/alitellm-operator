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
	// The endpoint is used for both the periodic POST /key/health probe
	// (CONN-03, see internal/litellm/keyinfo.go for the §6.1 deviation
	// note) and every Phase 3+ domain mutation call. The value is
	// trimmed of any trailing slash by litellm.NewClient at the wire
	// layer; users may include or omit the trailing slash without
	// observable effect.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https?://[^@\s?#]+(:[0-9]{1,5})?(/[^\s?#]*)?$`
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

	// MCPToolPrefixSeparator names the character that the target LiteLLM
	// instance REJECTS inside `server_name` at `POST /v1/mcp/server` time
	// (HTTP 400 "Server name cannot contain '<sep>'."). The opposite
	// member of the {".", "-"} pair is allowed inside `server_name`.
	//
	// Empirically (FIX2.txt HIGH-1, 2026-05-22, against LiteLLM v1.85.1
	// upstream image), the rejection character is "." regardless of the
	// LiteLLM-side MCP_TOOL_PREFIX_SEPARATOR env var. The "." default
	// matches stock LiteLLM out-of-the-box; users running a non-stock
	// LiteLLM that forbids "-" must set this field explicitly to "-".
	//
	// The operator reads this field to sanitize the LiteLLM-side
	// `server_name` and `alias` for every MCPServer routed through this
	// Connection. The K8s `metadata.name` is left untouched —
	// sanitization is wire-boundary only.
	//
	// Valid values:
	//   - "." (default; matches LiteLLM v1.85.1 stock validator). Forbids
	//     '.' in server_name; the operator rewrites '.' → '-' in the wire
	//     payload when (and only when) the input contains '.'.
	//   - "-" Legacy / non-stock LiteLLM that forbids '-' in server_name;
	//     the operator rewrites '-' → '.' in the wire payload when (and
	//     only when) the input contains '-'.
	//
	// FIX.txt HIGH-1 (2026-05-22): added the field after dotted Discovery
	//   children failed at the LiteLLM-side validator on default deploys.
	// FIX2.txt HIGH-1 (2026-05-22): default flipped from "-" to "." to
	//   match the empirically-observed LiteLLM v1.85.1 behavior.
	// FIX2.txt HIGH-9 (2026-05-22): sanitizer paired with this field
	//   became a no-op on safe inputs, preventing upgrade-orphan of
	//   pre-v0.1.2 hyphenated MCPServers.
	//
	// +optional
	// +kubebuilder:default:="."
	// +kubebuilder:validation:Enum=-;.
	MCPToolPrefixSeparator string `json:"mcpToolPrefixSeparator,omitempty"`

	// RequeueOnRejectedAfter is the retry cadence used by every dependent
	// reconciler (Team, Model, A2AAgent, MCPServer, ModelDiscovery,
	// MCPServerDiscovery) when a reconcile lands on a deterministic
	// upstream error (LiteLLMRejected, SecretNotFound). Without this,
	// controller-runtime's rate-limited queue drops the item after its
	// initial backoff exhausts; only an external mutation or operator
	// restart retries. After an upstream fix lands but operator state is
	// not externally poked, CRs stay Ready=False indefinitely (FIX2.txt
	// HIGH-2, 2026-05-22, observed on v0.1.2 EKS deploy).
	//
	// Reconcilers read this from the Connection snapshot and apply via:
	//   return ctrl.Result{RequeueAfter: snap.RequeueOnRejectedAfter}, nil
	//
	// Default 5m. Range [1m, 1h] enforced operator-side by
	// connection.NormalizedRequeueOnRejectedAfter (envtest revealed CEL
	// validation on metav1.Duration interacts poorly with the apiserver
	// default-then-validate ordering — values outside the range are
	// clamped at read time instead).
	//
	// +optional
	// +kubebuilder:default:="5m"
	RequeueOnRejectedAfter metav1.Duration `json:"requeueOnRejectedAfter,omitempty"`

	// MaxRequestsPerSecond caps the sustained rate of outbound HTTP
	// requests from the operator's LiteLLM client. Default 5. Set to 0
	// to disable rate limiting (NOT recommended — boot-time thundering
	// herd can push a modestly-stressed LiteLLM proxy into 5xx territory
	// and trigger the operator's own backoff loop). FIX2.txt MEDIUM-10
	// (2026-05-22).
	//
	// +optional
	// +kubebuilder:default:=5
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000
	MaxRequestsPerSecond int32 `json:"maxRequestsPerSecond,omitempty"`

	// MaxBurst is the token-bucket burst paired with MaxRequestsPerSecond.
	// Default 10. Set to 0 to fall back to a burst of MaxRequestsPerSecond
	// (i.e. one bucket-fill at a time).
	//
	// +optional
	// +kubebuilder:default:=10
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000
	MaxBurst int32 `json:"maxBurst,omitempty"`
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

	// Conditions carries the standard metav1.Condition list. Two
	// condition types are defined for LiteLLMConnection:
	//
	// Ready (primary connect / auth signal) — reason drawn from the
	// §6.0 reason set:
	// - Synced — probe succeeded; cache is fresh.
	// - Connecting — entry state, no probe outcome yet.
	// - Unreachable — transient probe failure (5xx, network).
	// - BadMasterKey — 401 from the LiteLLM master-key probe.
	// - SecretNotFound — masterKeySecretRef Secret or key missing.
	//
	// LoggingHealthy (secondary — sourced from POST /key/health response
	// body's logging_callbacks.status field):
	// - Healthy — proxy reports logging callbacks healthy.
	// - Unhealthy — proxy reports at least one logging callback unhealthy.
	// - Unknown — probe succeeded but proxy did not report a status.
	// - ProbeError — probe failed before logging health could be read.
	//
	// Phase 3+ dependents read the Ready condition via the cache snapshot
	// (internal/connection.Cache), never via direct CR Get. LoggingHealthy
	// is informational only; not consumed by other reconcilers.
	//
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=llmconn,categories=litellm
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'default'",message="LiteLLMConnection name must be 'default' (singleton per spec §6.1)"
// +kubebuilder:validation:XValidation:rule="self.spec.endpoint.startsWith('http://') || self.spec.endpoint.startsWith('https://')",message="spec.endpoint must use http:// or https:// scheme"
// +kubebuilder:validation:XValidation:rule="!self.spec.endpoint.contains('@')",message="spec.endpoint must not contain userinfo (user:pass@host); use spec.masterKeySecretRef instead"
// +kubebuilder:validation:XValidation:rule="!self.spec.endpoint.matches('\\\\s')",message="spec.endpoint must not contain whitespace"
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
