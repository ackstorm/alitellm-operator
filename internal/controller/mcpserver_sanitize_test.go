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
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
)

// TestMCPServerReconciler_FIX_H1_DotSeparator asserts that when the
// LiteLLMConnection's spec.mcpToolPrefixSeparator is "." (the ackstorm
// prod config), MCPServer wire-side server_name + alias are rewritten
// with "." replaced by "-". K8s metadata.name unchanged.
//
// Regression for FIX.txt HIGH-1 (2026-05-22 EKS prod smoke-test, 22/22
// toolhive children blocked by LiteLLM "Server name cannot contain '.'.").
func TestMCPServerReconciler_FIX_H1_DotSeparator(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetMCPServers()

	const dottedName = "test-toolhive-discovery.mcp.context7"
	const sanitizedName = "test-toolhive-discovery-mcp-context7"

	ensureNoMCPServer(t, ctx, dottedName)
	resetConnCacheSnapshot()
	cleanupConn := setupReadyConnectionMCPWithSeparator(t, ctx, ".")
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
	c := apimeta.FindStatusCondition(m.Status.Conditions, conditionTypeReady)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != reasonSynced {
		t.Fatalf("Ready not Synced; condition=%+v", c)
	}

	if !mockServer.HasMCPServer(sanitizedName) {
		t.Fatalf("mock missing sanitized server %q (FIX H-1 regression — sanitize not applied for dot-separator)", sanitizedName)
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
		t.Errorf("dot leaked into body.server_name=%q (sep='.' MUST be sanitized)", s)
	}

	if m.Name != dottedName {
		t.Errorf("metadata.name mutated: got %q, want %q (sanitize must NOT touch K8s identity)", m.Name, dottedName)
	}
}

// TestMCPServerReconciler_FIX_H1_DashSeparator asserts that when the
// LiteLLMConnection's spec.mcpToolPrefixSeparator is "-" (LiteLLM default),
// MCPServer wire-side server_name + alias are rewritten with "-" replaced
// by ".". K8s metadata.name unchanged.
func TestMCPServerReconciler_FIX_H1_DashSeparator(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetMCPServers()

	const hyphenName = "context7-test-server"
	const sanitizedName = "context7.test.server"

	ensureNoMCPServer(t, ctx, hyphenName)
	resetConnCacheSnapshot()
	cleanupConn := setupReadyConnectionMCPWithSeparator(t, ctx, "-")
	t.Cleanup(func() {
		cleanupConn()
		ensureNoMCPServer(t, context.Background(), hyphenName)
	})

	cr := &litellmv1alpha1.LiteLLMMCPServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      hyphenName,
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
		t.Fatalf("create MCPServer with hyphenated name: %v", err)
	}

	m := pollMCPServerCondition(t, ctx, hyphenName, reasonSynced, 30*time.Second)
	c := apimeta.FindStatusCondition(m.Status.Conditions, conditionTypeReady)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != reasonSynced {
		t.Fatalf("Ready not Synced; condition=%+v", c)
	}

	if !mockServer.HasMCPServer(sanitizedName) {
		t.Fatalf("mock missing sanitized server %q (FIX H-1 regression — sanitize not applied for dash-separator)", sanitizedName)
	}
	if mockServer.HasMCPServer(hyphenName) {
		t.Fatalf("mock has hyphenated server %q — sanitize did not run (LiteLLM default would 400 with %q)",
			hyphenName, "Server name cannot contain '-'")
	}

	body := mockServer.LastMCPBody(sanitizedName)
	if body == nil {
		t.Fatalf("LastMCPBody(%q) returned nil; expected CREATE body recorded", sanitizedName)
	}
	if s, _ := body["server_name"].(string); strings.Contains(s, "-") {
		t.Errorf("dash leaked into body.server_name=%q (sep='-' MUST be sanitized)", s)
	}

	if m.Name != hyphenName {
		t.Errorf("metadata.name mutated: got %q, want %q", m.Name, hyphenName)
	}
}

// setupReadyConnectionMCPWithSeparator is a variant of setupReadyConnectionMCP
// that sets spec.mcpToolPrefixSeparator on the Connection CR. Returns a
// cleanup func that removes the conn CR.
func setupReadyConnectionMCPWithSeparator(t *testing.T, ctx context.Context, separator string) func() {
	t.Helper()
	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	connCR.Spec.MCPToolPrefixSeparator = separator
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	cleanup := func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		time.Sleep(50 * time.Millisecond)
	}
	snap := pollSnapshotReason(30*time.Second, reasonSynced)
	if snap.Reason != reasonSynced {
		cleanup()
		t.Fatalf("LiteLLMConnection not Synced within 30s; reason=%q", snap.Reason)
	}
	if snap.MCPToolPrefixSeparator != separator {
		cleanup()
		t.Fatalf("snapshot separator: got %q, want %q (Rebuild plumbing regression)",
			snap.MCPToolPrefixSeparator, separator)
	}
	return cleanup
}
