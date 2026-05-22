// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
)

// TestMCPServerReconciler_FIX_H1_DottedName asserts that an MCPServer with a
// dotted metadata.name is sent to LiteLLM with ServerName + Alias sanitized
// via dot-to-dash replacement. LiteLLM v1.83.10+ rejects '.' in server_name
// with HTTP 400; sanitization at the wire boundary closes FIX.txt HIGH-1
// (2026-05-22 EKS prod smoke-test, 22/22 toolhive children blocked).
//
// The K8s metadata.name is left untouched — only the wire payload differs.
func TestMCPServerReconciler_FIX_H1_DottedName(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetMCPServers()

	const dottedName = "test-toolhive-discovery.mcp.context7"
	const sanitizedName = "test-toolhive-discovery-mcp-context7"

	ensureNoMCPServer(t, ctx, dottedName)
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionMCP(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoMCPServer(t, context.Background(), dottedName)
	})

	cr := &litellmv1alpha1.LiteLLMMCPServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dottedName,
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.MCPServerSpec{
			Endpoint:  "https://mcp.example.com",
			Transport: "http",
			Params: runtime.RawExtension{
				Raw: []byte(`{"mcp_info":{"description":"sample"}}`),
			},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create MCPServer with dotted name: %v", err)
	}

	m := pollMCPServerCondition(t, ctx, dottedName, reasonSynced, 30*time.Second)
	c := apimeta.FindStatusCondition(m.Status.Conditions, "Ready")
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != reasonSynced {
		t.Fatalf("Ready not Synced; condition=%+v", c)
	}

	// Mock keyed by what the controller actually sent. Sanitized name MUST
	// be present; the dotted form MUST NOT.
	if !mockServer.HasMCPServer(sanitizedName) {
		t.Fatalf("mock missing sanitized server %q (FIX H-1 regression — sanitize not applied at wire boundary)", sanitizedName)
	}
	if mockServer.HasMCPServer(dottedName) {
		t.Fatalf("mock has dotted server %q — sanitize did not run (LiteLLM would 400 with %q)",
			dottedName, "Server name cannot contain '.'")
	}

	body := mockServer.LastMCPBody(sanitizedName)
	if body == nil {
		t.Fatalf("LastMCPBody(%q) returned nil; expected CREATE body recorded", sanitizedName)
	}
	if got, want := body["server_name"], sanitizedName; got != want {
		t.Errorf("body.server_name: got %v, want %q", got, want)
	}
	if got, want := body["alias"], sanitizedName; got != want {
		t.Errorf("body.alias: got %v, want %q (alias must equal server_name per D-7.1-10)", got, want)
	}
	if s, _ := body["server_name"].(string); strings.Contains(s, ".") {
		t.Errorf("dot leaked into body.server_name=%q", s)
	}
	if s, _ := body["alias"].(string); strings.Contains(s, ".") {
		t.Errorf("dot leaked into body.alias=%q", s)
	}

	// K8s-side identity is unchanged — sanitize is wire-boundary only.
	if m.Name != dottedName {
		t.Errorf("metadata.name mutated: got %q, want %q (sanitize must NOT touch K8s identity)", m.Name, dottedName)
	}
}
