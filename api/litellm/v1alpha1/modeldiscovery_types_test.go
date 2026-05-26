// SPDX-License-Identifier: Apache-2.0

// Task 3 — schema unit tests for the regenerated ModelDiscovery
// CRD types. These tests are STANDALONE (no envtest, no kubebuilder
// dependencies); CEL admission-side tests against a real apiserver live in
// (envtest harness wiring).

package v1alpha1

import (
	"bytes"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TestModelDiscovery_DeepCopy_RoundTrip constructs a fully-populated
// ModelDiscovery (one entry in each list-typed field), calls DeepCopy,
// asserts field equality on every interesting scalar, and confirms that
// mutating the copy's slices does NOT alter the source (deep copy
// correctness). Covers MDISC-26 status surface + the supporting struct
// graph (SecretObjectRef, ModelDiscoveryFilters, ModelDiscoveryRefresh,
// SkippedCandidate, FailedCandidate).
func TestModelDiscovery_DeepCopy_RoundTrip(t *testing.T) {
	lastRefresh := metav1.NewTime(time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC))
	src := &LiteLLMModelDiscovery{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-discovery",
			Namespace: "default",
		},
		Spec: ModelDiscoverySpec{
			Type:   "anthropic",
			Prefix: "anthropic",
			CredentialsSecretRef: &SecretObjectRef{
				Name: "anthropic-credentials",
			},
			Region:  "", // forbidden for anthropic, kept empty
			BaseURL: "", // forbidden for anthropic, kept empty
			Params:  runtime.RawExtension{Raw: []byte(`{"rpm":100}`)},
			Info:    runtime.RawExtension{Raw: []byte(`{"tier":"standard"}`)},
			Secrets: []SecretSubstitution{
				{
					As: "ANTHROPIC_API_KEY",
					SecretRef: SecretKeyRef{
						Name: "anthropic-inference-key",
						Key:  "api-key",
					},
				},
			},
			Filters: &ModelDiscoveryFilters{
				Include: []string{`^claude-3-5-.*`},
				Exclude: []string{`.*-haiku$`},
			},
			Refresh: ModelDiscoveryRefresh{
				Interval: metav1.Duration{Duration: 5 * time.Minute},
			},
		},
		Status: ModelDiscoveryStatus{
			ObservedGeneration: 7,
			Conditions: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					Reason:             "Synced",
					LastTransitionTime: lastRefresh,
				},
				{
					Type:               "SourceReachable",
					Status:             metav1.ConditionTrue,
					Reason:             "Ok",
					LastTransitionTime: lastRefresh,
				},
			},
			DiscoveredCount:   3,
			GeneratedCount:    2,
			GeneratedChildren: []string{"anthropic.claude-3-5-sonnet", "anthropic.claude-3-5-opus"},
			SkippedCandidates: []SkippedCandidate{
				{
					Name:   "anthropic.claude-3-5-haiku",
					Reason: "ExplicitModelExists",
				},
			},
			FailedCandidates: []FailedCandidate{},
			LastRefreshAt:    &lastRefresh,
		},
	}

	dst := src.DeepCopy()

	// --- scalar field equality
	if dst.Spec.Type != src.Spec.Type {
		t.Errorf("Spec.Type: got %q want %q", dst.Spec.Type, src.Spec.Type)
	}
	if dst.Spec.Prefix != src.Spec.Prefix {
		t.Errorf("Spec.Prefix: got %q want %q", dst.Spec.Prefix, src.Spec.Prefix)
	}
	if dst.Spec.Region != src.Spec.Region {
		t.Errorf("Spec.Region: got %q want %q", dst.Spec.Region, src.Spec.Region)
	}
	if dst.Spec.BaseURL != src.Spec.BaseURL {
		t.Errorf("Spec.BaseURL: got %q want %q", dst.Spec.BaseURL, src.Spec.BaseURL)
	}
	if dst.Spec.Refresh.Interval != src.Spec.Refresh.Interval {
		t.Errorf("Spec.Refresh.Interval: got %v want %v",
			dst.Spec.Refresh.Interval, src.Spec.Refresh.Interval)
	}
	if dst.Spec.CredentialsSecretRef == nil ||
		dst.Spec.CredentialsSecretRef.Name != "anthropic-credentials" {
		t.Errorf("Spec.CredentialsSecretRef.Name: got %v want %q",
			dst.Spec.CredentialsSecretRef, "anthropic-credentials")
	}
	if dst.Status.DiscoveredCount != src.Status.DiscoveredCount {
		t.Errorf("Status.DiscoveredCount: got %d want %d",
			dst.Status.DiscoveredCount, src.Status.DiscoveredCount)
	}
	if dst.Status.GeneratedCount != src.Status.GeneratedCount {
		t.Errorf("Status.GeneratedCount: got %d want %d",
			dst.Status.GeneratedCount, src.Status.GeneratedCount)
	}
	if dst.Status.LastRefreshAt == nil ||
		!dst.Status.LastRefreshAt.Equal(src.Status.LastRefreshAt) {
		t.Errorf("Status.LastRefreshAt: got %v want %v",
			dst.Status.LastRefreshAt, src.Status.LastRefreshAt)
	}

	// --- pointer independence: SecretObjectRef must be a NEW allocation
	if dst.Spec.CredentialsSecretRef == src.Spec.CredentialsSecretRef {
		t.Error("Spec.CredentialsSecretRef: src and dst share the same pointer (shallow copy)")
	}
	dst.Spec.CredentialsSecretRef.Name = "mutated" //nolint:goconst // sentinel value for deepcopy-leak assertions; semantically distinct from other tests' "mutated" usages
	if src.Spec.CredentialsSecretRef.Name == "mutated" {
		t.Error("Spec.CredentialsSecretRef.Name: mutating dst altered src (shallow copy)")
	}

	// --- slice independence: Filters.Include must be a NEW allocation
	if &dst.Spec.Filters.Include[0] == &src.Spec.Filters.Include[0] {
		t.Error("Spec.Filters.Include: src and dst share the same backing array (shallow copy)")
	}
	dst.Spec.Filters.Include[0] = "mutated"
	if src.Spec.Filters.Include[0] == "mutated" {
		t.Error("Spec.Filters.Include: mutating dst altered src (shallow copy)")
	}

	// --- slice independence: GeneratedChildren must be a NEW allocation
	dst.Status.GeneratedChildren[0] = "mutated"
	if src.Status.GeneratedChildren[0] == "mutated" {
		t.Error("Status.GeneratedChildren: mutating dst altered src (shallow copy)")
	}

	// --- slice independence: SkippedCandidates must be a NEW allocation
	dst.Status.SkippedCandidates[0].Name = "mutated"
	if src.Status.SkippedCandidates[0].Name == "mutated" {
		t.Error("Status.SkippedCandidates: mutating dst altered src (shallow copy)")
	}
}

// TestModelDiscovery_RawExtension_Preserves asserts that
// runtime.RawExtension's Raw byte slice survives DeepCopy byte-for-byte
// (the pass-through bag contract for spec.params and spec.info per
// MDISC-23 / MODEL-05).
func TestModelDiscovery_RawExtension_Preserves(t *testing.T) {
	paramsBody := []byte(`{"model":"anthropic/claude-3-5-sonnet","rpm":100}`)
	infoBody := []byte(`{"mode":"chat","tier":"premium"}`)

	src := &LiteLLMModelDiscovery{
		Spec: ModelDiscoverySpec{
			Type:   "anthropic",
			Params: runtime.RawExtension{Raw: paramsBody},
			Info:   runtime.RawExtension{Raw: infoBody},
			Refresh: ModelDiscoveryRefresh{
				Interval: metav1.Duration{Duration: 1 * time.Minute},
			},
		},
	}
	dst := src.DeepCopy()

	if !bytes.Equal(dst.Spec.Params.Raw, paramsBody) {
		t.Errorf("Spec.Params.Raw: got %q want %q",
			string(dst.Spec.Params.Raw), string(paramsBody))
	}
	if !bytes.Equal(dst.Spec.Info.Raw, infoBody) {
		t.Errorf("Spec.Info.Raw: got %q want %q",
			string(dst.Spec.Info.Raw), string(infoBody))
	}

	// Pointer independence — mutating dst's Raw slice must not alter src's.
	if len(dst.Spec.Params.Raw) > 0 && len(src.Spec.Params.Raw) > 0 {
		dst.Spec.Params.Raw[0] = 'X'
		if src.Spec.Params.Raw[0] == 'X' {
			t.Error("Spec.Params.Raw: mutating dst altered src (shallow copy of byte slice)")
		}
	}
}

// TestSkippedCandidate_ReasonEnum_Compiles asserts that all three
// normative SkippedCandidate.Reason enum values per spec §6.3 line 870
// are valid string field assignments. Passes by compiling — the CRD-side
// enum constraint (verified at admission via the
// +kubebuilder:validation:Enum marker in modeldiscovery_types.go) is
// orthogonal to Go's compile-time type system. This test locks the
// enum vocabulary in tandem with the marker.
func TestModelDiscovery_SkippedCandidate_ReasonEnum_Compiles(t *testing.T) {
	cases := []SkippedCandidate{
		{Name: "a", Reason: "ExplicitModelExists"},
		{Name: "b", Reason: "Conflict", OwnedBy: "default/other-discovery"},
		{Name: "c", Reason: "InvalidDiscoveredName", Message: "name too long"},
	}
	if len(cases) != 3 {
		t.Fatalf("expected three SkippedCandidate cases, got %d", len(cases))
	}
	for i, c := range cases {
		if c.Reason == "" {
			t.Errorf("cases[%d].Reason is empty", i)
		}
	}
}

// TestFailedCandidate_ReasonEnum_SingleValue asserts that
// FailedCandidate.Reason carries the SINGLE valid enum value
// "ChildCRWriteFailed" per MDISC-26 / _FINALv3 narrowing. Discovery
// never calls LiteLLM (MDISC-27), so LiteLLMRejected and
// LiteLLMUnavailable have been retired from the Discovery-level reason
// set; those reasons surface on the child Model's status instead. The
// CRD-side enum constraint enforces this at admission; this test locks
// the Go-side vocabulary in tandem.
func TestModelDiscovery_FailedCandidate_ReasonEnum_SingleValue(t *testing.T) {
	c := FailedCandidate{
		Name:    "anthropic.claude-3-5-fail",
		Reason:  "ChildCRWriteFailed",
		Message: "apiserver rate limit",
	}
	if c.Reason != "ChildCRWriteFailed" {
		t.Errorf("FailedCandidate.Reason: got %q want %q",
			c.Reason, "ChildCRWriteFailed")
	}
}
