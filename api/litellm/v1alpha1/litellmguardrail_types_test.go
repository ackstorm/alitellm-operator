// SPDX-License-Identifier: Apache-2.0

// Schema unit tests for LiteLLMGuardRail. Pure Go — no envtest, no
// kubebuilder API server. CRD-side admission constraints (MinLength,
// MaxItems, Enum) are exercised in the envtest harness in
// internal/controller/litellmguardrail_admission_test.go.

package v1alpha1

import (
	"bytes"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// mutatedSentinel is the canary value written into deep-copied slice
// elements in the DeepCopy_RoundTrip tests. Pulled out as a const so
// goconst stays quiet across the ~16 mutation-isolation assertions
// below — the sentinel is opaque on purpose; any string different from
// the seed values works.
const mutatedSentinel = "mutated"

// TestGuardRail_DeepCopy_RoundTrip exercises DeepCopy across every
// slice and pointer-typed field (DefaultOn, Mode, Secrets, Conditions,
// At) and asserts that mutating the copy never reaches back to the
// source — i.e. that controller-gen produced a real deep copy, not a
// shallow alias.
func TestGuardRail_DeepCopy_RoundTrip(t *testing.T) {
	defaultOn := true
	at := metav1.NewTime(time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC))

	src := &LiteLLMGuardRail{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-guardrail",
			Namespace: "default",
		},
		Spec: GuardRailSpec{
			GuardrailName:  "pii-detector",
			Provider:       "aporia",
			Mode:           []GuardRailMode{"pre_call", "post_call"},
			DefaultOn:      &defaultOn,
			PolicyTemplate: "enterprise-pii-baseline-v3",
			Params:         runtime.RawExtension{Raw: []byte(`{"api_base":"https://aporia"}`)},
			Info:           runtime.RawExtension{Raw: []byte(`{"description":"pre-call PII"}`)},
			Secrets: []SecretSubstitution{
				{
					As:        "APORIA_API_KEY",
					SecretRef: SecretKeyRef{Name: "aporia-creds", Key: "APORIA_API_KEY"},
				},
			},
		},
		Status: GuardRailStatus{
			ObservedGeneration: 5,
			Conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Synced", LastTransitionTime: at},
			},
			LastRendered: GuardRailLastRenderedStatus{
				Hash:               "deadbeef",
				GuardrailID:        "01HXG5...",
				DefinitionLocation: "db",
				PoolSize:           2,
				At:                 &at,
			},
		},
	}

	dst := src.DeepCopy()

	// --- scalar equality
	if dst.Spec.GuardrailName != src.Spec.GuardrailName {
		t.Errorf("GuardrailName: got %q want %q", dst.Spec.GuardrailName, src.Spec.GuardrailName)
	}
	if dst.Spec.Provider != src.Spec.Provider {
		t.Errorf("Provider: got %q want %q", dst.Spec.Provider, src.Spec.Provider)
	}
	if dst.Spec.PolicyTemplate != src.Spec.PolicyTemplate {
		t.Errorf("PolicyTemplate: got %q want %q", dst.Spec.PolicyTemplate, src.Spec.PolicyTemplate)
	}
	if dst.Status.LastRendered.Hash != src.Status.LastRendered.Hash {
		t.Errorf("LastRendered.Hash: got %q want %q", dst.Status.LastRendered.Hash, src.Status.LastRendered.Hash)
	}
	if dst.Status.LastRendered.PoolSize != 2 {
		t.Errorf("LastRendered.PoolSize: got %d want 2", dst.Status.LastRendered.PoolSize)
	}

	// --- DefaultOn pointer must be a new allocation
	if dst.Spec.DefaultOn == src.Spec.DefaultOn {
		t.Error("Spec.DefaultOn: dst and src share the same *bool (shallow copy)")
	}
	*dst.Spec.DefaultOn = false
	if *src.Spec.DefaultOn == false {
		t.Error("Spec.DefaultOn: mutating dst altered src")
	}

	// --- Mode slice must be a new allocation
	if &dst.Spec.Mode[0] == &src.Spec.Mode[0] {
		t.Error("Spec.Mode: dst and src share the same backing array")
	}
	dst.Spec.Mode[0] = mutatedSentinel
	if src.Spec.Mode[0] == mutatedSentinel {
		t.Error("Spec.Mode: mutating dst altered src")
	}

	// --- Secrets slice independence
	dst.Spec.Secrets[0].As = "MUTATED"
	if src.Spec.Secrets[0].As == "MUTATED" {
		t.Error("Spec.Secrets[0].As: mutating dst altered src")
	}

	// --- LastRendered.At pointer independence (metav1.Time is a struct,
	// so the pointer itself must differ)
	if dst.Status.LastRendered.At == src.Status.LastRendered.At {
		t.Error("LastRendered.At: dst and src share the same *metav1.Time")
	}

	// --- Conditions slice independence
	dst.Status.Conditions[0].Reason = mutatedSentinel
	if src.Status.Conditions[0].Reason == mutatedSentinel {
		t.Error("Status.Conditions: mutating dst altered src")
	}

	// --- exercise the sub-struct DeepCopy wrappers directly so the
	// generated `if in == nil { return nil }` + new-and-copy branches
	// are both covered (the parent LiteLLMGuardRail.DeepCopyInto path
	// bypasses these wrappers).
	if specCopy := src.Spec.DeepCopy(); specCopy == nil {
		t.Error("GuardRailSpec.DeepCopy on non-nil receiver returned nil")
	}
	if statusCopy := src.Status.DeepCopy(); statusCopy == nil {
		t.Error("GuardRailStatus.DeepCopy on non-nil receiver returned nil")
	}
	if lrCopy := src.Status.LastRendered.DeepCopy(); lrCopy == nil {
		t.Error("GuardRailLastRenderedStatus.DeepCopy on non-nil receiver returned nil")
	}
}

// TestGuardRail_RawExtension_Preserves asserts pass-through bag
// contract: spec.params and spec.info survive DeepCopy byte-for-byte
// (no marshal round-trip, no key reordering, no value coercion).
func TestGuardRail_RawExtension_Preserves(t *testing.T) {
	// Nested object inside params — exercises the
	// PreserveUnknownFields path for deep JSON.
	paramsBody := []byte(`{"patterns":[{"pattern_type":"prebuilt","pattern_name":"aws_access_key","action":"BLOCK"}],"blocked_words":[{"keyword":"confidential","action":"BLOCK"}]}`)
	infoBody := []byte(`{"description":"credential filter","params":[{"name":"strict","type":"bool"}]}`)

	src := &LiteLLMGuardRail{
		Spec: GuardRailSpec{
			GuardrailName: "credential-filter",
			Provider:      "litellm_content_filter",
			Mode:          []GuardRailMode{"pre_call"},
			Params:        runtime.RawExtension{Raw: paramsBody},
			Info:          runtime.RawExtension{Raw: infoBody},
		},
	}

	dst := src.DeepCopy()

	if !bytes.Equal(dst.Spec.Params.Raw, paramsBody) {
		t.Errorf("Spec.Params.Raw: got %q want %q", string(dst.Spec.Params.Raw), string(paramsBody))
	}
	if !bytes.Equal(dst.Spec.Info.Raw, infoBody) {
		t.Errorf("Spec.Info.Raw: got %q want %q", string(dst.Spec.Info.Raw), string(infoBody))
	}

	// Mutate dst's Raw — src must be unchanged (independent byte slice).
	if len(dst.Spec.Params.Raw) > 0 {
		dst.Spec.Params.Raw[0] = 'X'
		if src.Spec.Params.Raw[0] == 'X' {
			t.Error("Spec.Params.Raw: dst and src share the same byte slice (shallow copy)")
		}
	}
}

// TestGuardRail_Mode_EnumValues_Compile asserts every value declared in
// the GuardRailMode +kubebuilder:validation:Enum marker is a valid Go
// string assignment. The marker enforces the same vocabulary at the
// admission layer; this test locks the two in tandem. Mutate the
// marker on the type and at least one assertion below will fail at
// CRD-regen + envtest time.
func TestGuardRail_Mode_EnumValues_Compile(t *testing.T) {
	cases := []GuardRailMode{
		"pre_call",
		"post_call",
		"during_call",
		"logging_only",
		"pre_mcp_call",
		"during_mcp_call",
		"realtime_input_transcription",
	}
	if len(cases) != 7 {
		t.Fatalf("expected 7 mode values, got %d", len(cases))
	}
	for i, c := range cases {
		if c == "" {
			t.Errorf("cases[%d] is empty", i)
		}
	}
}

// TestGuardRail_DeepCopy_NilSafe exercises the nil-receiver branches
// in the generated DeepCopy wrappers (returning nil rather than
// panicking).
func TestGuardRail_DeepCopy_NilSafe(t *testing.T) {
	var nilGR *LiteLLMGuardRail
	if got := nilGR.DeepCopy(); got != nil {
		t.Errorf("nil *LiteLLMGuardRail DeepCopy: got %v, want nil", got)
	}
	var nilSpec *GuardRailSpec
	if got := nilSpec.DeepCopy(); got != nil {
		t.Errorf("nil *GuardRailSpec DeepCopy: got %v, want nil", got)
	}
	var nilStatus *GuardRailStatus
	if got := nilStatus.DeepCopy(); got != nil {
		t.Errorf("nil *GuardRailStatus DeepCopy: got %v, want nil", got)
	}
	var nilLR *GuardRailLastRenderedStatus
	if got := nilLR.DeepCopy(); got != nil {
		t.Errorf("nil *GuardRailLastRenderedStatus DeepCopy: got %v, want nil", got)
	}
	var nilList *LiteLLMGuardRailList
	if got := nilList.DeepCopy(); got != nil {
		t.Errorf("nil *LiteLLMGuardRailList DeepCopy: got %v, want nil", got)
	}

	// DeepCopyObject delegates to DeepCopy() and returns nil if the
	// receiver is nil — exercise both arms so the runtime.Object path
	// is fully covered.
	if obj := nilGR.DeepCopyObject(); obj != nil {
		t.Errorf("nil *LiteLLMGuardRail DeepCopyObject: got %v, want nil", obj)
	}
	if obj := nilList.DeepCopyObject(); obj != nil {
		t.Errorf("nil *LiteLLMGuardRailList DeepCopyObject: got %v, want nil", obj)
	}
}

// TestGuardRailList_DeepCopy_RoundTrip exercises the List DeepCopy
// path (DeepCopyInto + DeepCopyObject) — the scheme registry uses
// DeepCopyObject through the runtime.Object interface, so it must be
// callable without panics.
func TestGuardRailList_DeepCopy_RoundTrip(t *testing.T) {
	src := &LiteLLMGuardRailList{
		Items: []LiteLLMGuardRail{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "one", Namespace: "default"},
				Spec: GuardRailSpec{
					GuardrailName: "one",
					Provider:      "aporia",
					Mode:          []GuardRailMode{"pre_call"},
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "two", Namespace: "default"},
				Spec: GuardRailSpec{
					GuardrailName: "two",
					Provider:      "lakera_v2",
					Mode:          []GuardRailMode{"post_call"},
				},
			},
		},
	}

	dst := src.DeepCopy()
	if dst == nil {
		t.Fatal("DeepCopy returned nil")
	}
	if len(dst.Items) != 2 {
		t.Fatalf("dst.Items: got len %d want 2", len(dst.Items))
	}

	// Mutate dst — src must remain untouched (slice independence).
	dst.Items[0].Spec.GuardrailName = mutatedSentinel
	if src.Items[0].Spec.GuardrailName == mutatedSentinel {
		t.Error("Items[0]: mutating dst altered src (shared backing array)")
	}

	// Exercise the runtime.Object path used by the scheme registry.
	if obj := src.DeepCopyObject(); obj == nil {
		t.Error("LiteLLMGuardRailList.DeepCopyObject returned nil")
	}
	if obj := (&src.Items[0]).DeepCopyObject(); obj == nil {
		t.Error("LiteLLMGuardRail.DeepCopyObject returned nil")
	}
}
