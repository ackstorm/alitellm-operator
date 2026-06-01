// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"
)

// TestA2AAgentReconciler_AgentCardKeys_ExcludesInjectedURL is the M-B10
// regression: status.lastRendered.agentCardKeys is computed from the user's
// spec.agentCard, but the operator injects agent_card_params.url =
// spec.endpoint before the keys were snapshotted, so "url" leaked into the
// recorded keys even when the user never declared it. The snapshot now runs
// BEFORE the url overlay.
func TestA2AAgentReconciler_AgentCardKeys_ExcludesInjectedURL(t *testing.T) {
	ctx := context.Background()
	resetMockA2A()
	ensureNoA2AAgent(t, ctx, "a2a-cardkeys-test")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionA2A(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoA2AAgent(t, context.Background(), "a2a-cardkeys-test")
	})

	// a2aSampleCR's agentCard = {"name","description"} — no "url".
	cr := a2aSampleCR("a2a-cardkeys-test")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create A2AAgent: %v", err)
	}

	a := pollA2AAgentCondition(t, ctx, "a2a-cardkeys-test", reasonSynced, 30*time.Second)
	if len(a.Status.LastRendered.AgentCardKeys) == 0 {
		t.Fatal("expected user agentCard keys (name, description) to be recorded")
	}
	for _, k := range a.Status.LastRendered.AgentCardKeys {
		if k == "url" {
			t.Fatalf("agentCardKeys leaked operator-injected 'url': %v", a.Status.LastRendered.AgentCardKeys)
		}
	}
}
