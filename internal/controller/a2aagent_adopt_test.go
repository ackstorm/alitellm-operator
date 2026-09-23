// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// Issue #131 — a LiteLLMA2AAgent whose status.lastRendered.agentID went
// missing could never recover: with no id the CREATE arm fires, LiteLLM
// answers 400 "Agent with name X already exists", and the reconciler retried
// that same doomed POST forever. Reported against v0.8.3 / LiteLLM 1.99.1
// with four of five agents stuck Ready=False/LiteLLMRejected, each still
// carrying lastRendered.at and lastRendered.hash but no agentID.
//
// The two tests below lock the two halves of the fix:
//   - AdoptsByNameOnDuplicateReject: an id-less CR heals itself by adopting
//     the pre-existing agent (recovers CRs already in that state).
//   - VanishProbeClearNotPersisted: a rejected recreate never writes the
//     probe-cleared id through to the status subresource (stops CRs entering
//     that state at all).

// pollA2AAgent re-reads the CR until pred holds or the deadline passes.
func pollA2AAgent(t *testing.T, ctx context.Context, name string, timeout time.Duration,
	pred func(*litellmv1alpha1.LiteLLMA2AAgent) bool,
) *litellmv1alpha1.LiteLLMA2AAgent {
	t.Helper()
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	deadline := time.Now().Add(timeout)
	var a litellmv1alpha1.LiteLLMA2AAgent
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, key, &a); err == nil && pred(&a) {
			return &a
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("A2AAgent %q never satisfied predicate within %s (last status: %+v)", name, timeout, a.Status)
	return nil
}

// TestA2AAgentReconciler_AdoptsByNameOnDuplicateReject — a CR whose
// lastRendered.agentID was lost, against a LiteLLM that still holds the agent.
// The CREATE arm is rejected 400 (duplicate name); the reconciler resolves the
// agent by name, adopts THAT id, and PUTs the rendered state onto it. No
// second agent row is created and the id consumers hold (the /a2a/<agent_id>
// URL is embedded in the agent card) is preserved.
func TestA2AAgentReconciler_AdoptsByNameOnDuplicateReject(t *testing.T) {
	ctx := context.Background()
	const name = "a2a-adopt-dup"
	resetMockA2A()
	ensureNoA2AAgent(t, ctx, name)
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionA2A(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoA2AAgent(t, context.Background(), name)
	})

	cr := a2aSampleCR(name)
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create A2AAgent: %v", err)
	}
	a := pollA2AAgentCondition(t, ctx, name, reasonSynced, 30*time.Second)
	originalID := a.Status.LastRendered.AgentID
	if originalID == "" {
		t.Fatalf("A2AAgent not Synced within 30s")
	}

	// Reproduce the reported status: hash + at intact, agentID gone.
	a.Status.LastRendered.AgentID = ""
	if err := k8sClient.Status().Update(ctx, a); err != nil {
		t.Fatalf("blank agentID in status: %v", err)
	}

	// Nudge the CR so a reconcile runs against the id-less status. The
	// reconciler is writing status concurrently, so retry on conflict.
	key := client.ObjectKeyFromObject(a)
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh litellmv1alpha1.LiteLLMA2AAgent
		if err := k8sClient.Get(ctx, key, &fresh); err != nil {
			return err
		}
		if fresh.Annotations == nil {
			fresh.Annotations = map[string]string{}
		}
		fresh.Annotations["test.ackstorm.ai/nudge"] = "1"
		return k8sClient.Update(ctx, &fresh)
	}); err != nil {
		t.Fatalf("annotate A2AAgent: %v", err)
	}

	healed := pollA2AAgent(t, ctx, name, 30*time.Second, func(x *litellmv1alpha1.LiteLLMA2AAgent) bool {
		return x.Status.LastRendered.AgentID != ""
	})
	if healed.Status.LastRendered.AgentID != originalID {
		t.Errorf("lastRendered.agentID after adoption: want %q (the pre-existing agent), got %q",
			originalID, healed.Status.LastRendered.AgentID)
	}
	if got := mockServer.GetAgentID(name); got != originalID {
		t.Errorf("mock agent id for %q: want %q (adopted, not recreated), got %q", name, originalID, got)
	}

	// The adoption pushes rendered state with a PUT onto the existing id.
	sawAdoptPut := false
	for _, call := range mockServer.Recorded() {
		if call.Method == http.MethodPut && call.Path == "/v1/agents/"+originalID {
			sawAdoptPut = true
		}
	}
	if !sawAdoptPut {
		t.Errorf("no PUT /v1/agents/%s after adoption; rendered state was never pushed", originalID)
	}
}

// TestA2AAgentReconciler_VanishProbeClearNotPersisted — the vanish probe
// reports the agent gone (LiteLLM's LIST omits it) while the create-time
// name check still holds it, so the recreate is rejected 400 and adoption
// cannot resolve the name either. The CR goes Ready=False/LiteLLMRejected —
// and MUST keep its agentID, or nothing ever puts it back on the UPDATE arm.
func TestA2AAgentReconciler_VanishProbeClearNotPersisted(t *testing.T) {
	ctx := context.Background()
	const name = "a2a-vanish-keep-id"
	resetMockA2A()
	ensureNoA2AAgent(t, ctx, name)
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionA2A(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoA2AAgent(t, context.Background(), name)
	})

	cr := a2aSampleCR(name)
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create A2AAgent: %v", err)
	}
	a := pollA2AAgentCondition(t, ctx, name, reasonSynced, 30*time.Second)
	originalID := a.Status.LastRendered.AgentID
	if originalID == "" {
		t.Fatalf("A2AAgent not Synced within 30s")
	}

	// LIST stops reporting the agent; POST still rejects its name as taken.
	mockServer.HideAgentFromList(name)

	// Re-render so the reconciler leaves the steady state and re-probes.
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(a), a); err != nil {
		t.Fatalf("re-get A2AAgent: %v", err)
	}
	a.Spec.AgentCard = runtime.RawExtension{Raw: []byte(`{"name":"Sample Agent","description":"changed"}`)}
	if err := k8sClient.Update(ctx, a); err != nil {
		t.Fatalf("update spec.agentCard: %v", err)
	}

	// reasonModelAliasRejected is just the shared "LiteLLMRejected" literal —
	// every reconciler writes that reason on a deterministic 4xx.
	rejected := pollA2AAgentCondition(t, ctx, name, reasonModelAliasRejected, 30*time.Second)
	if c := apimeta.FindStatusCondition(rejected.Status.Conditions, conditionTypeReady); c == nil ||
		c.Reason != reasonModelAliasRejected {
		t.Fatalf("A2AAgent never reached Ready=False/LiteLLMRejected; condition=%+v", c)
	}
	if rejected.Status.LastRendered.AgentID != originalID {
		t.Fatalf("lastRendered.agentID after a rejected recreate: want %q retained, got %q — "+
			"the CR can no longer reach the UPDATE arm and is permanently stuck (#131)",
			originalID, rejected.Status.LastRendered.AgentID)
	}
}

// TestA2AAgentReconciler_UnknownParamKeyEvent — spec.params is a verbatim
// pass-through, but LiteLLM's AgentConfig models a fixed key set and drops
// the rest.
func TestA2AAgentReconciler_UnknownParamKeyEvent(t *testing.T) {
	ctx := context.Background()
	const name = "a2a-unknown-key"
	resetMockA2A()
	ensureNoA2AAgent(t, ctx, name)
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionA2A(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoA2AAgent(t, context.Background(), name)
	})

	cr := a2aSampleCR(name)
	cr.Spec.Params = runtime.RawExtension{Raw: []byte(`{"future_key":["dream"],"tpm_limit":10}`)}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create A2AAgent: %v", err)
	}
	if a := pollA2AAgentCondition(t, ctx, name, reasonSynced, 30*time.Second); a.Status.LastRendered.AgentID == "" {
		t.Fatalf("A2AAgent not Synced within 30s")
	}

	found := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !found {
		var events corev1.EventList
		if err := k8sClient.List(ctx, &events, client.InNamespace(WatchNamespace)); err != nil {
			t.Fatalf("list events: %v", err)
		}
		for _, e := range events.Items {
			if e.InvolvedObject.Name == name && e.InvolvedObject.Kind == a2aAgentKind &&
				e.Reason == eventReasonUnknownParamKey &&
				strings.Contains(e.Message, `"future_key"`) {
				if e.Type != corev1.EventTypeWarning {
					t.Errorf("UnknownParamKey Event type: want Warning, got %q", e.Type)
				}
				found = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !found {
		t.Errorf("no Warning/UnknownParamKey Event naming %q within 10s", "future_key")
	}

	// tpm_limit IS modeled — it must not be warned about, and it must ship.
	var events corev1.EventList
	if err := k8sClient.List(ctx, &events, client.InNamespace(WatchNamespace)); err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, e := range events.Items {
		if e.InvolvedObject.Name == name && e.Reason == eventReasonUnknownParamKey &&
			strings.Contains(e.Message, `"tpm_limit"`) {
			t.Errorf("UnknownParamKey Event fired for modeled key tpm_limit: %s", e.Message)
		}
	}
	if body := mockServer.LastAgentBody(name); body != nil {
		if _, ok := body["tpm_limit"]; !ok {
			t.Errorf("body.tpm_limit missing; modeled keys must still ship")
		}
	}
}
