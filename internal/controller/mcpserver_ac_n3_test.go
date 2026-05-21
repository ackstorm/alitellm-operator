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

// TestMCPServer_AC_N3_NoUserOrKeyCalls exercises the SCOPE-03 / AC-N3
// negative invariant for the MCPServer kind. Apply MCPServer/ac-n3-mcpserver
// (transport=http happy path); wait Ready. Trigger a re-reconcile via
// annotation bump. Assert zero new /user/* and /key/* calls across the
// full lifecycle.
func TestMCPServer_AC_N3_NoUserOrKeyCalls(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetMCPServers()
	ensureNoMCPServer(t, ctx, "ac-n3-mcpserver")
	resetConnCacheSnapshot()

	// Capture baseline PathCallCount BEFORE the LiteLLMConnection setup
	// (which probes GET /models — separate from the /user/* and /key/*
	// surfaces we are asserting here).
	priorUserCalls := mockServer.PathCallCount("/user/")
	priorKeyCalls := mockServer.PathCallCount("/key/")

	cleanupConn := setupReadyConnectionMCP(t, ctx)
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		cleanupConn()
		ensureNoMCPServer(t, context.Background(), "ac-n3-mcpserver")
	})

	// Apply MCPServer CR.
	cr := &litellmv1alpha1.LiteLLMMCPServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ac-n3-mcpserver",
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.MCPServerSpec{
			Endpoint:  "https://mcp.example.com",
			Transport: "http",
			Params: runtime.RawExtension{
				Raw: []byte(`{"mcp_info":{"description":"ac-n3 test"}}`),
			},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create MCPServer: %v", err)
	}

	// Wait for Ready/Synced — confirms full reconcile path ran.
	m := pollMCPServerCondition(t, ctx, "ac-n3-mcpserver", reasonSynced, 30*time.Second)
	if m.Status.LastRendered.ServerID == "" {
		t.Fatalf("MCPServer/ac-n3-mcpserver not Synced within 30s; conditions=%+v", m.Status.Conditions)
	}

	// Trigger a spurious re-reconcile via annotation bump to cover the
	// steady-state hash-equal short-circuit path.
	if m.Annotations == nil {
		m.Annotations = map[string]string{}
	}
	m.Annotations["test.litellm.ackstorm.ai/ac-n3-trigger"] = time.Now().Format(time.RFC3339Nano)
	if err := k8sClient.Update(ctx, m); err != nil {
		t.Fatalf("annotation-bump MCPServer: %v", err)
	}
	// Safety margin for the re-reconcile to complete.
	time.Sleep(2 * time.Second)

	// ─── LOAD-BEARING zero /user/* and /key/* call assertion ─────────────
	//
	// SCOPE-03 / AC-N3 MCPServer slice. The MCPServer reconciler path MUST
	// NOT generate any traffic to externally-owned routes (/user/*, /key/*).
	if got := mockServer.PathCallCount("/user/") - priorUserCalls; got != 0 {
		t.Errorf("AC-N3 violation: MCPServer reconciler issued %d new /user/* call(s) (want 0)",
			got)
	}
	if got := mockServer.PathCallCount("/key/") - priorKeyCalls; got != 0 {
		t.Errorf("AC-N3 violation: MCPServer reconciler issued %d new /key/* call(s) (want 0)",
			got)
	}
}
