// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
)

// TestA2AAgent_AC_N3_NoUserOrKeyCalls exercises the SCOPE-03 / AC-N3
// negative invariant for the A2AAgent kind. Apply A2AAgent/ac-n3-a2aagent
// (minimal agentCard happy path); wait Ready. Trigger a re-reconcile via
// annotation bump. Assert zero new /user/* and /key/* calls across the
// full lifecycle.
func TestA2AAgent_AC_N3_NoUserOrKeyCalls(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetAgents()
	ensureNoA2AAgent(t, ctx, "ac-n3-a2aagent")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionA2A(t, ctx)
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		cleanupConn()
		ensureNoA2AAgent(t, context.Background(), "ac-n3-a2aagent")
	})

	// Capture baseline PathCallCount AFTER Connection is Synced AND
	// separately track the probe path so any in-flight Connection
	// re-reconciles (probes to /key/health) can be subtracted from
	// the /key/ prefix delta below.
	priorUserCalls := mockServer.PathCallCount("/user/")
	priorKeyCalls := mockServer.PathCallCount("/key/")
	priorKeyHealthCalls := mockServer.PathCallCount("/key/health")

	// Apply A2AAgent CR.
	cr := &litellmv1alpha1.LiteLLMA2AAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ac-n3-a2aagent",
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.A2AAgentSpec{
			Endpoint: "https://agent.example.com/a2a",
			AgentCard: runtime.RawExtension{
				Raw: []byte(`{"name":"AC-N3 Agent","description":"ac-n3 test"}`),
			},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create A2AAgent: %v", err)
	}

	// Wait for Ready/Synced — confirms full reconcile path ran.
	a := pollA2AAgentCondition(t, ctx, "ac-n3-a2aagent", reasonSynced, 30*time.Second)
	if a.Status.LastRendered.AgentID == "" {
		t.Fatalf("A2AAgent/ac-n3-a2aagent not Synced within 30s; conditions=%+v", a.Status.Conditions)
	}

	// Trigger a spurious re-reconcile via annotation bump to cover the
	// steady-state hash-equal short-circuit path.
	if a.Annotations == nil {
		a.Annotations = map[string]string{}
	}
	a.Annotations["test.litellm.ackstorm.ai/ac-n3-trigger"] = time.Now().Format(time.RFC3339Nano)
	if err := k8sClient.Update(ctx, a); err != nil {
		t.Fatalf("annotation-bump A2AAgent: %v", err)
	}
	// Safety margin for the re-reconcile to complete.
	time.Sleep(1 * time.Second)

	// ─── LOAD-BEARING zero /user/* and /key/* call assertion ─────────────
	//
	// SCOPE-03 / AC-N3 A2AAgent slice. The A2AAgent reconciler path MUST
	// NOT generate any traffic to externally-owned routes (/user/*, /key/*).
	if got := mockServer.PathCallCount("/user/") - priorUserCalls; got != 0 {
		t.Errorf("AC-N3 violation: A2AAgent reconciler issued %d new /user/* call(s) (want 0)",
			got)
	}
	keyAllDelta := mockServer.PathCallCount("/key/") - priorKeyCalls
	keyHealthDelta := mockServer.PathCallCount("/key/health") - priorKeyHealthCalls
	if got := keyAllDelta - keyHealthDelta; got != 0 {
		t.Errorf("AC-N3 violation: A2AAgent reconciler issued %d new /key/* call(s) excluding Connection probe (want 0; total /key/ delta=%d, /key/health delta=%d)",
			got, keyAllDelta, keyHealthDelta)
	}
}
