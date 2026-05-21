// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/providers"
)

// TestModelDiscovery_AC_N3_NoUserOrKeyCalls exercises the SCOPE-03 / AC-N3
// negative invariant for the ModelDiscovery kind. Injects a fakeProvider
// that returns zero candidates (no cloud calls). Apply
// ModelDiscovery/ac-n3-modeldiscovery; wait Synced. Trigger a re-reconcile
// via annotation bump. Assert zero new /user/* and /key/* calls across the
// full lifecycle.
func TestModelDiscovery_AC_N3_NoUserOrKeyCalls(t *testing.T) {
	ctx := context.Background()
	const mdName = "ac-n3-modeldiscovery"

	ensureNoModelDiscovery(t, ctx, mdName)
	t.Cleanup(func() { ensureNoModelDiscovery(t, context.Background(), mdName) })

	// Capture baseline PathCallCount. ModelDiscovery never calls LiteLLM
	// (/user/*, /key/*, or /model/* routes) — it calls cloud provider
	// APIs. This assertion documents that invariant.
	priorUserCalls := mockServer.PathCallCount("/user/")
	priorKeyCalls := mockServer.PathCallCount("/key/")

	// Inject a deterministic fakeProvider with zero candidates. This
	// bypasses all cloud-SDK middleware and allows the reconciler to
	// complete a full cycle without external network calls.
	//
	// Using "kubeai" type because it requires only spec.baseURL (no
	// credentials secret) — the simplest path to a Ready reconcile.
	fake := newFakeProvider("kubeai", nil)
	providers.RegisterTestProvider(t, "kubeai", fake)

	// Apply ModelDiscovery CR using kubeai type (no credentials needed).
	md := &litellmv1alpha1.LiteLLMModelDiscovery{
		ObjectMeta: metav1.ObjectMeta{
			Name:      mdName,
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.ModelDiscoverySpec{
			Type:    "kubeai",
			BaseURL: "http://kubeai.test.svc/openai/v1",
			Refresh: litellmv1alpha1.ModelDiscoveryRefresh{
				Interval: metav1.Duration{Duration: time.Minute},
			},
		},
	}
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create ModelDiscovery: %v", err)
	}

	// Wait for Ready/Synced — confirms full reconcile path ran.
	result := pollDiscoveryStatusReady(t, ctx, mdName, reasonSynced, 30*time.Second)
	c := apimeta.FindStatusCondition(result.Status.Conditions, "Ready")
	if c == nil || c.Reason != reasonSynced {
		t.Fatalf("ModelDiscovery/ac-n3-modeldiscovery not Synced within 30s; conditions=%+v",
			result.Status.Conditions)
	}

	// Trigger a spurious re-reconcile via annotation bump.
	if result.Annotations == nil {
		result.Annotations = map[string]string{}
	}
	result.Annotations["test.litellm.ackstorm.ai/ac-n3-trigger"] = time.Now().Format(time.RFC3339Nano)
	if err := k8sClient.Update(ctx, result); err != nil {
		t.Fatalf("annotation-bump ModelDiscovery: %v", err)
	}
	// Safety margin for the re-reconcile to complete.
	time.Sleep(2 * time.Second)

	// ─── LOAD-BEARING zero /user/* and /key/* call assertion ─────────────
	//
	// SCOPE-03 / AC-N3 ModelDiscovery slice. The ModelDiscovery reconciler
	// path MUST NOT generate any traffic to externally-owned routes
	// (/user/*, /key/*) — it contacts cloud provider APIs only.
	if got := mockServer.PathCallCount("/user/") - priorUserCalls; got != 0 {
		t.Errorf("AC-N3 violation: ModelDiscovery reconciler issued %d new /user/* call(s) (want 0)",
			got)
	}
	if got := mockServer.PathCallCount("/key/") - priorKeyCalls; got != 0 {
		t.Errorf("AC-N3 violation: ModelDiscovery reconciler issued %d new /key/* call(s) (want 0)",
			got)
	}
}
