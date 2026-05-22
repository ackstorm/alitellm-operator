// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
)

// TestModel_AC_N3_NoUserOrKeyCalls exercises the SCOPE-03 / AC-N3
// negative invariant for the Model kind. Apply Model/ac-n3-model
// (basic openai/gpt-4o-mini spec); wait Ready. Trigger a re-reconcile
// via annotation bump. Assert zero new /user/* and /key/* calls across
// the full lifecycle.
func TestModel_AC_N3_NoUserOrKeyCalls(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetModels()
	ensureNoModel(t, ctx, "ac-n3-model")
	resetConnCacheSnapshot()

	// Capture baseline PathCallCount BEFORE the LiteLLMConnection setup
	// (which probes GET /models — separate from the /user/* and /key/*
	// surfaces we are asserting here).
	priorUserCalls := mockServer.PathCallCount("/user/")
	priorKeyCalls := mockServer.PathCallCount("/key/")

	// Set up a ready LiteLLMConnection so the Model reconciler can
	// proceed past connection-gating.
	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		time.Sleep(50 * time.Millisecond)
		ensureNoModel(t, context.Background(), "ac-n3-model")
	})
	snap := pollSnapshotReason(30*time.Second, reasonSynced)
	if snap.Reason != reasonSynced {
		t.Fatalf("LiteLLMConnection not Synced within 30s; reason=%q", snap.Reason)
	}

	// Apply Model CR.
	cr := &litellmv1alpha1.LiteLLMModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ac-n3-model",
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.ModelSpec{
			Params: runtime.RawExtension{
				Raw: []byte(`{"model":"openai/gpt-4o-mini","api_key":"test-fake-key-not-real"}`),
			},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Model: %v", err)
	}

	// Wait for Ready/Synced — confirms full reconcile path ran.
	m := pollModelCondition(t, ctx, "ac-n3-model", reasonSynced, 30*time.Second)
	if m.Status.LastRendered.LiteLLMModelID == "" {
		t.Fatalf("Model/ac-n3-model not Synced within 30s; conditions=%+v", m.Status.Conditions)
	}

	// Trigger a spurious re-reconcile via annotation bump to cover the
	// steady-state hash-equal short-circuit path.
	if m.Annotations == nil {
		m.Annotations = map[string]string{}
	}
	m.Annotations["test.litellm.ackstorm.ai/ac-n3-trigger"] = time.Now().Format(time.RFC3339Nano)
	if err := k8sClient.Update(ctx, m); err != nil {
		t.Fatalf("annotation-bump Model: %v", err)
	}
	// Safety margin for the re-reconcile to complete.
	time.Sleep(1 * time.Second)

	// ─── LOAD-BEARING zero /user/* and /key/* call assertion ─────────────
	//
	// SCOPE-03 / AC-N3 Model slice. The Model reconciler path MUST NOT
	// generate any traffic to externally-owned routes (/user/*, /key/*).
	if got := mockServer.PathCallCount("/user/") - priorUserCalls; got != 0 {
		t.Errorf("AC-N3 violation: Model reconciler issued %d new /user/* call(s) (want 0)",
			got)
	}
	if got := mockServer.PathCallCount("/key/") - priorKeyCalls; got != 0 {
		t.Errorf("AC-N3 violation: Model reconciler issued %d new /key/* call(s) (want 0)",
			got)
	}
}
