// SPDX-License-Identifier: Apache-2.0

// CRD-side admission tests for LiteLLMGuardRail. Exercises the
// +kubebuilder:validation:* markers (MinLength, MinItems, MaxItems,
// MaxLength, Enum) against the API server in the envtest harness —
// schema-only, no reconciler runs against these CRs.
//
// No LiteLLMGuardRail reconciler exists yet (deferred to a follow-up
// phase); the CRs created here are torn down in t.Cleanup without
// the help of a finalizer.

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// Shared literals across the admission + reconciler test files (both
// live in package `controller`). Extracted as file-local const block so
// goconst stays quiet — the provider name appears in 5 places and the
// LB-pool name in 3.
const (
	guardrailContentFilterProvider = "litellm_content_filter"
	guardrailLBPoolName            = "content-filter-lb"
)

// guardrailSampleCR returns a minimal valid LiteLLMGuardRail — used as
// a starting point for negative tests that mutate one field at a time.
func guardrailSampleCR(name string) *litellmv1alpha1.LiteLLMGuardRail {
	return &litellmv1alpha1.LiteLLMGuardRail{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.GuardRailSpec{
			GuardrailName: name,
			Provider:      guardrailContentFilterProvider,
			Mode:          []litellmv1alpha1.GuardRailMode{"pre_call"},
		},
	}
}

// ensureNoGuardrail removes any pre-existing LiteLLMGuardRail in
// WatchNamespace with the given name and waits up to 10s for full
// removal. No finalizer is expected today; the helper tolerates one
// for forward-compat with a future reconciler.
func ensureNoGuardrail(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	var existing litellmv1alpha1.LiteLLMGuardRail
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	if err := k8sClient.Get(ctx, key, &existing); err == nil {
		existing.SetFinalizers(nil)
		_ = k8sClient.Update(ctx, &existing)
		_ = k8sClient.Delete(ctx, &existing)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, key, &existing); apierrors.IsNotFound(err) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Logf("warning: LiteLLMGuardRail %q still present after 10s cleanup wait", name)
}

// TestGuardRailAdmission_MinimalCR_Accepted is the happy path: a CR
// satisfying every required field with no optional values is accepted
// by the API server (compile-time check that the marker set permits
// the documented minimum spec).
func TestGuardRailAdmission_MinimalCR_Accepted(t *testing.T) {
	ctx := context.Background()
	name := "gr-minimal"
	ensureNoGuardrail(t, ctx, name)
	t.Cleanup(func() { ensureNoGuardrail(t, context.Background(), name) })

	if err := k8sClient.Create(ctx, guardrailSampleCR(name)); err != nil {
		t.Fatalf("minimal CR was rejected: %v", err)
	}
}

// TestGuardRailAdmission_MissingProvider_Rejected asserts that omitting
// spec.provider (MinLength=1) triggers an admission rejection.
func TestGuardRailAdmission_MissingProvider_Rejected(t *testing.T) {
	ctx := context.Background()
	name := "gr-no-provider"
	ensureNoGuardrail(t, ctx, name)
	t.Cleanup(func() { ensureNoGuardrail(t, context.Background(), name) })

	cr := guardrailSampleCR(name)
	cr.Spec.Provider = ""

	err := k8sClient.Create(ctx, cr)
	if err == nil {
		t.Fatalf("expected admission rejection with empty provider, but Create succeeded")
	}
	if !apierrors.IsInvalid(err) {
		t.Errorf("expected apierrors.IsInvalid for empty provider; got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "provider") {
		t.Errorf("expected rejection to mention 'provider'; got %q", err.Error())
	}
}

// TestGuardRailAdmission_MissingGuardrailName_Rejected — MinLength=1.
func TestGuardRailAdmission_MissingGuardrailName_Rejected(t *testing.T) {
	ctx := context.Background()
	name := "gr-no-grname"
	ensureNoGuardrail(t, ctx, name)
	t.Cleanup(func() { ensureNoGuardrail(t, context.Background(), name) })

	cr := guardrailSampleCR(name)
	cr.Spec.GuardrailName = ""

	err := k8sClient.Create(ctx, cr)
	if err == nil {
		t.Fatalf("expected admission rejection with empty guardrailName, but Create succeeded")
	}
	if !apierrors.IsInvalid(err) {
		t.Errorf("expected apierrors.IsInvalid for empty guardrailName; got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "guardrailName") {
		t.Errorf("expected rejection to mention 'guardrailName'; got %q", err.Error())
	}
}

// TestGuardRailAdmission_EmptyMode_Rejected — MinItems=1.
func TestGuardRailAdmission_EmptyMode_Rejected(t *testing.T) {
	ctx := context.Background()
	name := "gr-empty-mode"
	ensureNoGuardrail(t, ctx, name)
	t.Cleanup(func() { ensureNoGuardrail(t, context.Background(), name) })

	cr := guardrailSampleCR(name)
	cr.Spec.Mode = []litellmv1alpha1.GuardRailMode{}

	err := k8sClient.Create(ctx, cr)
	if err == nil {
		t.Fatalf("expected admission rejection with empty mode list, but Create succeeded")
	}
	if !apierrors.IsInvalid(err) {
		t.Errorf("expected apierrors.IsInvalid for empty mode; got %T: %v", err, err)
	}
}

// TestGuardRailAdmission_InvalidMode_Rejected — Enum constraint.
func TestGuardRailAdmission_InvalidMode_Rejected(t *testing.T) {
	ctx := context.Background()
	name := "gr-bad-mode"
	ensureNoGuardrail(t, ctx, name)
	t.Cleanup(func() { ensureNoGuardrail(t, context.Background(), name) })

	cr := guardrailSampleCR(name)
	cr.Spec.Mode = []litellmv1alpha1.GuardRailMode{"not_a_real_slot"}

	err := k8sClient.Create(ctx, cr)
	if err == nil {
		t.Fatalf("expected admission rejection with invalid mode enum, but Create succeeded")
	}
	if !apierrors.IsInvalid(err) {
		t.Errorf("expected apierrors.IsInvalid for invalid mode; got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "mode") {
		t.Errorf("expected rejection to mention 'mode'; got %q", err.Error())
	}
}

// TestGuardRailAdmission_SixModes_Accepted — H8: all six non-realtime
// hook slots in a single guardrail are now admitted (MaxItems=6).
func TestGuardRailAdmission_SixModes_Accepted(t *testing.T) {
	ctx := context.Background()
	name := "gr-six-modes"
	ensureNoGuardrail(t, ctx, name)
	t.Cleanup(func() { ensureNoGuardrail(t, context.Background(), name) })

	cr := guardrailSampleCR(name)
	cr.Spec.Mode = []litellmv1alpha1.GuardRailMode{
		"pre_call", "post_call", "during_call", "logging_only", "pre_mcp_call", "during_mcp_call",
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("expected 6-mode guardrail to be admitted; got %v", err)
	}
}

// TestGuardRailAdmission_TooManyModes_Rejected — MaxItems=6 (seven
// elements, the full enum vocabulary, should be rejected on count).
func TestGuardRailAdmission_TooManyModes_Rejected(t *testing.T) {
	ctx := context.Background()
	name := "gr-too-many-modes"
	ensureNoGuardrail(t, ctx, name)
	t.Cleanup(func() { ensureNoGuardrail(t, context.Background(), name) })

	cr := guardrailSampleCR(name)
	// Seven valid enum values (all six hooks + realtime) — exceeds MaxItems=6.
	cr.Spec.Mode = []litellmv1alpha1.GuardRailMode{
		"pre_call",
		"post_call",
		"during_call",
		"logging_only",
		"pre_mcp_call",
		"during_mcp_call",
		"realtime_input_transcription",
	}

	err := k8sClient.Create(ctx, cr)
	if err == nil {
		t.Fatalf("expected admission rejection with 7 mode elements, but Create succeeded")
	}
	if !apierrors.IsInvalid(err) {
		t.Errorf("expected apierrors.IsInvalid for too many modes; got %T: %v", err, err)
	}
}

// TestGuardRailAdmission_AllValidModes_Accepted exercises every value
// in the Enum marker as a single-element mode — locks the vocabulary
// against drift between the Go enum constant set and the marker.
func TestGuardRailAdmission_AllValidModes_Accepted(t *testing.T) {
	ctx := context.Background()
	all := []litellmv1alpha1.GuardRailMode{
		"pre_call",
		"post_call",
		"during_call",
		"logging_only",
		"pre_mcp_call",
		"during_mcp_call",
		"realtime_input_transcription",
	}
	for _, m := range all {
		mode := m
		t.Run(string(mode), func(t *testing.T) {
			name := "gr-mode-" + strings.ReplaceAll(string(mode), "_", "-")
			ensureNoGuardrail(t, ctx, name)
			t.Cleanup(func() { ensureNoGuardrail(t, context.Background(), name) })

			cr := guardrailSampleCR(name)
			cr.Spec.Mode = []litellmv1alpha1.GuardRailMode{mode}
			if err := k8sClient.Create(ctx, cr); err != nil {
				t.Fatalf("mode %q was rejected by admission: %v", mode, err)
			}
		})
	}
}

// TestGuardRailAdmission_GuardrailName_TooLong_Rejected — MaxLength=253.
func TestGuardRailAdmission_GuardrailName_TooLong_Rejected(t *testing.T) {
	ctx := context.Background()
	name := "gr-grname-too-long"
	ensureNoGuardrail(t, ctx, name)
	t.Cleanup(func() { ensureNoGuardrail(t, context.Background(), name) })

	cr := guardrailSampleCR(name)
	// 254 chars: one over MaxLength=253.
	cr.Spec.GuardrailName = strings.Repeat("x", 254)

	err := k8sClient.Create(ctx, cr)
	if err == nil {
		t.Fatalf("expected admission rejection with 254-char guardrailName, but Create succeeded")
	}
	if !apierrors.IsInvalid(err) {
		t.Errorf("expected apierrors.IsInvalid for over-long guardrailName; got %T: %v", err, err)
	}
}

// TestGuardRailAdmission_FullPassthroughBags_Accepted asserts that a
// CR with deeply nested spec.params (litellm_content_filter rules)
// and spec.info, plus a spec.secrets[] substitution, round-trips
// through the API server byte-for-byte — proving the
// PreserveUnknownFields marker is wired correctly.
func TestGuardRailAdmission_FullPassthroughBags_Accepted(t *testing.T) {
	ctx := context.Background()
	name := "gr-passthrough"
	ensureNoGuardrail(t, ctx, name)
	t.Cleanup(func() { ensureNoGuardrail(t, context.Background(), name) })

	defaultOn := true
	cr := guardrailSampleCR(name)
	cr.Spec.DefaultOn = &defaultOn
	cr.Spec.PolicyTemplate = "enterprise-pii-baseline-v3"
	cr.Spec.Params = runtime.RawExtension{
		Raw: []byte(`{
			"patterns": [
				{"pattern_type":"prebuilt","pattern_name":"aws_access_key","action":"BLOCK"},
				{"pattern_type":"regex","pattern":"\\b[A-Z]{3}-\\d{4}\\b","name":"employee_id","action":"MASK"}
			],
			"blocked_words":[{"keyword":"confidential","action":"BLOCK"}],
			"unreachable_fallback":"fail_closed"
		}`),
	}
	cr.Spec.Info = runtime.RawExtension{
		Raw: []byte(`{"description":"credential filter","params":[{"name":"strict","type":"bool"}]}`),
	}
	cr.Spec.Secrets = []litellmv1alpha1.SecretSubstitution{
		{
			As: "APORIA_API_KEY",
			SecretRef: litellmv1alpha1.SecretKeyRef{
				Name: "aporia-creds",
				Key:  "APORIA_API_KEY",
			},
		},
	}

	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("full-passthrough CR was rejected: %v", err)
	}

	// Re-fetch and confirm the nested bag survived round-trip with the
	// patterns array intact (a marshal-strip would replace the array
	// with the bare object).
	var fetched litellmv1alpha1.LiteLLMGuardRail
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: WatchNamespace}, &fetched); err != nil {
		t.Fatalf("Get after Create: %v", err)
	}
	if !strings.Contains(string(fetched.Spec.Params.Raw), `"patterns"`) {
		t.Errorf("Spec.Params.Raw lost patterns key after round-trip: %q", string(fetched.Spec.Params.Raw))
	}
	if !strings.Contains(string(fetched.Spec.Params.Raw), `"aws_access_key"`) {
		t.Errorf("Spec.Params.Raw lost nested aws_access_key after round-trip")
	}
	if !strings.Contains(string(fetched.Spec.Info.Raw), `"description"`) {
		t.Errorf("Spec.Info.Raw lost description after round-trip")
	}
	if fetched.Spec.DefaultOn == nil || *fetched.Spec.DefaultOn != true {
		t.Errorf("Spec.DefaultOn: got %v want true", fetched.Spec.DefaultOn)
	}
	if fetched.Spec.PolicyTemplate != "enterprise-pii-baseline-v3" {
		t.Errorf("Spec.PolicyTemplate: got %q want %q", fetched.Spec.PolicyTemplate, "enterprise-pii-baseline-v3")
	}
	if len(fetched.Spec.Secrets) != 1 || fetched.Spec.Secrets[0].As != "APORIA_API_KEY" {
		t.Errorf("Spec.Secrets: got %#v want one entry with As=APORIA_API_KEY", fetched.Spec.Secrets)
	}
}

// TestGuardRailAdmission_LoadBalancingPool_TwoCRsSameGuardrailName_Accepted
// asserts that two CRs with the same spec.guardrailName but different
// metadata.name coexist (LB pool pattern — duplicate CR-name is not a
// constraint, only metadata.name uniqueness within the namespace is).
func TestGuardRailAdmission_LoadBalancingPool_TwoCRsSameGuardrailName_Accepted(t *testing.T) {
	ctx := context.Background()
	primary := "gr-lb-primary"
	secondary := "gr-lb-secondary"
	ensureNoGuardrail(t, ctx, primary)
	ensureNoGuardrail(t, ctx, secondary)
	t.Cleanup(func() {
		ensureNoGuardrail(t, context.Background(), primary)
		ensureNoGuardrail(t, context.Background(), secondary)
	})

	cr1 := guardrailSampleCR(primary)
	cr1.Spec.GuardrailName = guardrailLBPoolName
	cr2 := guardrailSampleCR(secondary)
	cr2.Spec.GuardrailName = guardrailLBPoolName

	if err := k8sClient.Create(ctx, cr1); err != nil {
		t.Fatalf("primary LB CR rejected: %v", err)
	}
	if err := k8sClient.Create(ctx, cr2); err != nil {
		t.Fatalf("secondary LB CR rejected (same guardrailName, different metadata.name): %v", err)
	}
}
