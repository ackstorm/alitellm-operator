// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// TestA2AAgentReconciler_WriteStatusConflict_PreservesAgentID is the H3
// regression. Before the fix, writeStatus did a plain Status().Update with
// the caller's (possibly stale) resourceVersion; the call site swallowed a
// 409, so the AgentID assigned in-memory after a successful CREATE was
// never persisted — the next reconcile saw an empty AgentID and fired a
// duplicate POST /agent (a duplicate A2A agent). writeStatus now wraps the
// write in RetryOnConflict + re-Get, so a stale-RV write resolves instead
// of dropping the AgentID.
//
// Determinism: a stale copy (rv=R1) is written after the server has
// advanced to R2 via an out-of-band update. Old code → 409 returned (err
// non-nil, AgentID lost); new code → retry re-Gets R2 and persists.
func TestA2AAgentReconciler_WriteStatusConflict_PreservesAgentID(t *testing.T) {
	ctx := context.Background()
	resetMockA2A()
	ensureNoA2AAgent(t, ctx, "a2a-conflict-test")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionA2A(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoA2AAgent(t, context.Background(), "a2a-conflict-test")
	})

	cr := a2aSampleCR("a2a-conflict-test")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create A2AAgent: %v", err)
	}
	ready := pollA2AAgentCondition(t, ctx, "a2a-conflict-test", reasonSynced, 30*time.Second)
	realAgentID := ready.Status.LastRendered.AgentID
	if realAgentID == "" {
		t.Fatal("precondition: AgentID empty after first reconcile")
	}

	key := client.ObjectKey{Name: "a2a-conflict-test", Namespace: WatchNamespace}

	// Stale copy (rv = R1) carrying the real AgentID as the write intent.
	var stale litellmv1alpha1.LiteLLMA2AAgent
	if err := k8sClient.Get(ctx, key, &stale); err != nil {
		t.Fatalf("get stale: %v", err)
	}

	// Advance the server resourceVersion past the stale copy (R1 -> R2) via
	// an out-of-band annotation update, guaranteeing a 409 on a plain
	// Status().Update of the stale object.
	var fresh litellmv1alpha1.LiteLLMA2AAgent
	if err := k8sClient.Get(ctx, key, &fresh); err != nil {
		t.Fatalf("get fresh: %v", err)
	}
	if fresh.Annotations == nil {
		fresh.Annotations = map[string]string{}
	}
	fresh.Annotations["h3-conflict-probe"] = "1"
	if err := k8sClient.Update(ctx, &fresh); err != nil {
		t.Fatalf("out-of-band update: %v", err)
	}

	// Plain Update would 409 here; the RetryOnConflict path re-Gets and
	// succeeds without losing AgentID.
	stale.Status.LastRendered.AgentID = realAgentID
	if err := a2aAgentReconciler.writeStatus(ctx, &stale, metav1.ConditionTrue, reasonSynced, "h3 conflict probe"); err != nil {
		t.Fatalf("writeStatus errored on stale-RV write (retry should resolve): %v", err)
	}

	// H3 invariant: AgentID must still be persisted.
	var after litellmv1alpha1.LiteLLMA2AAgent
	if err := k8sClient.Get(ctx, key, &after); err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.Status.LastRendered.AgentID != realAgentID {
		t.Fatalf("AgentID lost after conflicting status write: want %q, got %q",
			realAgentID, after.Status.LastRendered.AgentID)
	}
}
