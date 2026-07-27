// SPDX-License-Identifier: Apache-2.0

// Isolated envtest for the GuardRail safety-relist drift-recovery path.
//
// Moved out of internal/controller (litellmguardrail_controller_test.go)
// to its own package + process so the background SafetyRelistRunnable
// recovers a single out-of-band-deleted guardrail without competing with
// the parent suite's 100ms relist flood + ~290 neighbor tests on a shared
// apiserver (the #74 -race -shuffle release-gate flake).

package relist

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/metrics"
)

// guardrailSampleCR returns a minimal valid GuardRail CR.
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

// ensureNoGuardrailCR removes any pre-existing CR and waits for removal.
func ensureNoGuardrailCR(t *testing.T, ctx context.Context, name string) {
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

// pollGuardrailCondition polls the Ready condition reason for up to 30s.
func pollGuardrailCondition(t *testing.T, ctx context.Context, name, wantReason string) {
	t.Helper()
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var gr litellmv1alpha1.LiteLLMGuardRail
		if err := k8sClient.Get(ctx, key, &gr); err == nil {
			c := apimeta.FindStatusCondition(gr.Status.Conditions, conditionTypeReady)
			if c != nil && c.Reason == wantReason {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("guardrail %q never reached Ready reason %q within 30s", name, wantReason)
}

// pollGuardrailID polls until status.lastRendered.guardrailID is non-empty.
func pollGuardrailID(t *testing.T, ctx context.Context, name string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	for time.Now().Before(deadline) {
		var gr litellmv1alpha1.LiteLLMGuardRail
		if err := k8sClient.Get(ctx, key, &gr); err == nil && gr.Status.LastRendered.GuardrailID != "" {
			return gr.Status.LastRendered.GuardrailID
		}
		time.Sleep(50 * time.Millisecond)
	}
	return ""
}

// readyConnectionForTest waits until connCache.Snapshot flips Ready with a
// non-nil client.
func readyConnectionForTest(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		snap := connCache.Snapshot()
		if snap.Ready && snap.Client != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("connCache.Snapshot did not become Ready within 10s")
}

// ensureLiteLLMConnectionDefault writes the LiteLLMConnection/default CR
// pointing at the mock so the connection cache flips Ready. Idempotent.
func ensureLiteLLMConnectionDefault(t *testing.T, ctx context.Context) {
	t.Helper()
	conn := &litellmv1alpha1.LiteLLMConnection{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: WatchNamespace},
		Spec: litellmv1alpha1.LiteLLMConnectionSpec{
			Endpoint: mockServer.URL(),
			MasterKeySecretRef: litellmv1alpha1.SecretKeyRef{
				Name: "litellm-master-key",
				Key:  "masterKey",
			},
		},
	}
	if err := k8sClient.Create(ctx, conn); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("ensure LiteLLMConnection/default: %v", err)
	}
}

// TestGuardRail_SafetyRelist_CreateMissing locks the §7.6 safety-relist
// drift recovery: an out-of-band-deleted guardrail (mock row gone +
// status.guardrailID cleared) is re-created by the background relist
// runnable, bumping alitellm_operator_drift_corrected_total{guardrail,create_missing}.
//
// Runs in this isolated package/process, so the relist over a single
// guardrail CR recovers in ~2-3s — no shared-apiserver contention to slip
// past (the #74 release-gate flake this move eliminates). A 30s fast-break
// budget is ample.
func TestGuardRail_SafetyRelist_CreateMissing(t *testing.T) {
	ctx := context.Background()
	name := "gr-relist-recover"
	ensureNoGuardrailCR(t, ctx, name)
	t.Cleanup(func() { ensureNoGuardrailCR(t, context.Background(), name) })
	mockServer.ResetGuardrails()
	ensureLiteLLMConnectionDefault(t, ctx)
	readyConnectionForTest(t)

	cr := guardrailSampleCR(name)
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("Create CR: %v", err)
	}
	pollGuardrailCondition(t, ctx, name, "Synced")
	originalID := pollGuardrailID(t, ctx, name, 5*time.Second)
	if originalID == "" {
		t.Fatal("CR never reached non-empty GuardrailID")
	}
	if got := mockServer.MutationsByGuardrailName(name); got < 1 {
		t.Fatalf("post-CREATE mutations: got %d want >=1", got)
	}

	before := testutil.ToFloat64(metrics.DriftCorrectedTotal.WithLabelValues("guardrail", "create_missing"))

	// Out-of-band DELETE — pull the row from mock state without the
	// operator's DELETE path.
	mockServer.DeleteGuardrailOutOfBand(originalID)

	// Force the CREATE branch by clearing GuardrailID via a Status UPDATE.
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	var fresh litellmv1alpha1.LiteLLMGuardRail
	if err := k8sClient.Get(ctx, key, &fresh); err != nil {
		t.Fatalf("re-get CR: %v", err)
	}
	if fresh.Status.LastRendered.Hash == "" {
		t.Fatalf("expected non-empty Hash after first reconcile — firstReconcile suppression would mask create_missing")
	}
	fresh.Status.LastRendered.GuardrailID = ""
	if err := k8sClient.Status().Update(ctx, &fresh); err != nil {
		t.Fatalf("clear GuardrailID via Status().Update: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		after := testutil.ToFloat64(metrics.DriftCorrectedTotal.WithLabelValues("guardrail", "create_missing"))
		if after-before >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	after := testutil.ToFloat64(metrics.DriftCorrectedTotal.WithLabelValues("guardrail", "create_missing"))
	if delta := after - before; delta < 1 {
		t.Fatalf("create_missing NOT incremented after out-of-band DELETE + safety re-list within 30s; delta=%.0f", delta)
	}

	if !mockServer.HasGuardrail(name) {
		t.Errorf("mock missing guardrail %q after recovery POST", name)
	}
	newID := mockServer.GetGuardrailID(name)
	if newID == "" || newID == originalID {
		t.Errorf("mock GuardrailID after recovery: got %q want fresh != %q", newID, originalID)
	}

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var got litellmv1alpha1.LiteLLMGuardRail
		if err := k8sClient.Get(ctx, key, &got); err == nil {
			if got.Status.LastRendered.GuardrailID == newID {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("CR status.lastRendered.guardrailID never advanced to %q after recovery", newID)
}
