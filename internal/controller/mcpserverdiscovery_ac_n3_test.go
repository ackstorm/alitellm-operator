// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"

	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
	"github.com/ackstorm/alitellm-operator/internal/toolhive"
)

// TestMCPServerDiscovery_AC_N3_NoUserOrKeyCalls exercises the SCOPE-03 /
// AC-N3 negative invariant for the MCPServerDiscovery kind. Creates a
// ToolHive MCPServer object and an MCPServerDiscovery CR; waits for the
// child MCPServer to land (Synced). Triggers a re-reconcile via annotation
// bump. Asserts zero new /user/* and /key/* calls across the full lifecycle.
func TestMCPServerDiscovery_AC_N3_NoUserOrKeyCalls(t *testing.T) {
	ctx := context.Background()
	const mdName = "ac-n3-msd"
	const thNamespace = "ac-n3-ns"
	const thName = "ac-n3-tool"

	mockServer.SetMode(mock.ModeHappy)
	ensureNoMCPServerDiscovery(t, ctx, mdName)
	ensureNoToolhiveObject(t, ctx, toolhive.MCPServerGVK, thNamespace, thName)
	t.Cleanup(func() {
		ensureNoMCPServerDiscovery(t, context.Background(), mdName)
		ensureNoToolhiveObject(t, context.Background(), toolhive.MCPServerGVK, thNamespace, thName)
	})

	// Capture baseline PathCallCount. MCPServerDiscovery never calls
	// LiteLLM (/user/*, /key/*, or any other LiteLLM route) — it reads
	// from ToolHive only. This assertion documents that invariant.
	priorUserCalls := mockServer.PathCallCount("/user/")
	priorKeyCalls := mockServer.PathCallCount("/key/")

	// Create a ToolHive MCPServer object so the informer has something to
	// return (exercises the full generate-child path, not just
	// SourceUnreachable).
	createToolhiveMCPServer(t, ctx, thNamespace, thName, "https://mcp.example.com", "http")

	// Apply MCPServerDiscovery CR.
	md := msDiscSampleCR(mdName, []string{thNamespace})
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create MCPServerDiscovery: %v", err)
	}

	// Wait for the child to land (confirming the full reconcile path ran).
	children := pollMCPServerDiscoveryChildren(t, ctx, mdName, 1, 30*time.Second)
	if len(children) != 1 {
		t.Fatalf("MCPServerDiscovery/ac-n3-msd: expected 1 child after 30s, got %d", len(children))
	}

	// Wait for Synced condition.
	mdAfter := pollMCPServerDiscoveryCondition(t, ctx, mdName, reasonSynced, 10*time.Second)
	c := apimeta.FindStatusCondition(mdAfter.Status.Conditions, conditionTypeReady)
	if c == nil || c.Reason != reasonSynced {
		t.Logf("MCPServerDiscovery not Synced within 10s (conditions=%+v) — proceeding with assertions", mdAfter.Status.Conditions)
	}

	// Trigger a spurious re-reconcile via annotation bump.
	if mdAfter.Annotations == nil {
		mdAfter.Annotations = map[string]string{}
	}
	mdAfter.Annotations["test.litellm.ackstorm.ai/ac-n3-trigger"] = time.Now().Format(time.RFC3339Nano)
	if err := k8sClient.Update(ctx, mdAfter); err != nil {
		t.Fatalf("annotation-bump MCPServerDiscovery: %v", err)
	}
	// Safety margin for the re-reconcile to complete.
	time.Sleep(1 * time.Second)

	// ─── LOAD-BEARING zero /user/* and /key/* call assertion ─────────────
	//
	// SCOPE-03 / AC-N3 MCPServerDiscovery slice. The MCPServerDiscovery
	// reconciler path MUST NOT generate any traffic to externally-owned
	// routes (/user/*, /key/*) — it reads from ToolHive only, never LiteLLM.
	if got := mockServer.PathCallCount("/user/") - priorUserCalls; got != 0 {
		t.Errorf("AC-N3 violation: MCPServerDiscovery reconciler issued %d new /user/* call(s) (want 0)",
			got)
	}
	if got := mockServer.PathCallCount("/key/") - priorKeyCalls; got != 0 {
		t.Errorf("AC-N3 violation: MCPServerDiscovery reconciler issued %d new /key/* call(s) (want 0)",
			got)
	}
}
