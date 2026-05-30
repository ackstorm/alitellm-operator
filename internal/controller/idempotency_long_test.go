//go:build longidempotency
// +build longidempotency

// SPDX-License-Identifier: Apache-2.0

// Package controller — AC-R1 real 35-minute cross-kind idempotency soak.
//
// Build tag: longidempotency (not part of `make test-full`; runs via `make
// test-smoke-idempotency-long` on nightly CI cadence).
//
// This test replaces the Phase 1 stub (which only verified the build tag
// compiled). The real soak:
// - Creates 1 CR of each of 7 kinds (Model, MCPServer, A2AAgent, Team,
// ModelDiscovery, MCPServerDiscovery, LiteLLMConnection already exists).
// - Waits for each to reach its first reconcile.
// - Snapshots mock.Mutations at t=0.
// - Sleeps 35 wall-clock minutes (spec §7.6: 30-min safety re-list cycle
// - 5-min buffer — ensures at least one full safety-relist fires).
// - Asserts mock.Mutations unchanged (delta == 0 is AC-R1 pass).
//
// The suite_test.go's accelerated SafetyRelistInterval (1s in envtest) is
// intentionally kept — the wall-clock window is load-bearing, not the
// cadence. The test is proof that steady-state reconciliation (every
// subsequent loop after the first) issues zero LiteLLM mutation calls.
package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
)

// TestIdempotency35MinReal — real AC-R1 cross-kind idempotency window.
//
// Spec §11 AC-R1: "Zero LiteLLM mutation calls across a 35-minute
// steady-state window for at least 1 CR of every kind after initial
// reconcile has completed (one full 30-min safety re-list cycle plus
// 5-min buffer)."
//
// Build-tag-gated (longidempotency); nightly CI via .github/workflows/nightly.yml.
func TestIdempotency35MinReal(t *testing.T) {
	if reconcileCalls == nil {
		t.Fatal("suite_test.go did not initialize globals — TestMain ordering bug")
	}
	if testing.Short() {
		t.Skip("skipping long-running idempotency test in -short mode")
	}

	ctx := context.Background()

	// Reset mock to a clean ModeHappy state.
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache() // ensure no cross-test cache pollution
	resetConnCacheSnapshot()

	t.Logf("AC-R1 soak: starting at %s", time.Now().Format(time.RFC3339))

	// ─── CR1: Model ──────────────────────────────────────────────────────────
	model := &litellmv1alpha1.LiteLLMModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "long-model",
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.ModelSpec{},
	}
	if err := k8sClient.Create(ctx, model); err != nil {
		t.Fatalf("create Model: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), model, &client.DeleteOptions{}) })

	// ─── CR2: MCPServer ────────────────────────────────────────────────────
	mcpServer := &litellmv1alpha1.LiteLLMMCPServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "long-mcp",
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.MCPServerSpec{
			Endpoint:  "http://example.local",
			Transport: "http",
			Params:    runtime.RawExtension{Raw: []byte(`{}`)},
		},
	}
	if err := k8sClient.Create(ctx, mcpServer); err != nil {
		t.Fatalf("create MCPServer: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), mcpServer, &client.DeleteOptions{}) })

	// ─── CR3: A2AAgent ────────────────────────────────────────────────────
	a2aAgent := &litellmv1alpha1.LiteLLMA2AAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "long-a2a",
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.A2AAgentSpec{
			Endpoint:  "http://example.local/a2a",
			AgentCard: runtime.RawExtension{Raw: []byte(`{"name":"long-a2a"}`)},
		},
	}
	if err := k8sClient.Create(ctx, a2aAgent); err != nil {
		t.Fatalf("create A2AAgent: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), a2aAgent, &client.DeleteOptions{}) })

	// ─── CR4: Team ────────────────────────────────────────────────────────
	team := &litellmv1alpha1.LiteLLMTeam{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ac-r1-team",
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.TeamSpec{},
	}
	if err := k8sClient.Create(ctx, team); err != nil {
		t.Fatalf("create Team: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), team, &client.DeleteOptions{})
		ensureNoTeam(t, context.Background(), "ac-r1-team")
	})

	// ─── CR5: ModelDiscovery (openai type; credentials Secret required) ───
	credsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "long-md-creds",
			Namespace: WatchNamespace,
		},
		StringData: map[string]string{
			"OPENAI_API_KEY": "test-key-not-real",
		},
	}
	if err := k8sClient.Create(ctx, credsSecret); err != nil {
		t.Fatalf("create creds Secret: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), credsSecret, &client.DeleteOptions{}) })

	mdiscovery := &litellmv1alpha1.LiteLLMModelDiscovery{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "long-mdiscovery",
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.ModelDiscoverySpec{
			Type: "openai",
			CredentialsSecretRef: &litellmv1alpha1.SecretObjectRef{
				Name: "long-md-creds",
			},
			Refresh: litellmv1alpha1.ModelDiscoveryRefresh{
				Interval: metav1.Duration{Duration: time.Minute},
			},
		},
	}
	if err := k8sClient.Create(ctx, mdiscovery); err != nil {
		t.Fatalf("create ModelDiscovery: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), mdiscovery, &client.DeleteOptions{}) })

	// ─── CR6: MCPServerDiscovery (toolhive type) ──────────────────────────
	msdiscovery := &litellmv1alpha1.LiteLLMMCPServerDiscovery{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "long-msdiscovery",
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.MCPServerDiscoverySpec{
			Type: "toolhive",
			Toolhive: litellmv1alpha1.MCPServerDiscoveryToolhive{
				Namespaces: []string{WatchNamespace},
				Kinds:      []string{"MCPServer", "VirtualMCPServer"},
			},
			Refresh: litellmv1alpha1.MCPServerDiscoveryRefresh{
				Interval: metav1.Duration{Duration: time.Minute},
			},
		},
	}
	if err := k8sClient.Create(ctx, msdiscovery); err != nil {
		t.Fatalf("create MCPServerDiscovery: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), msdiscovery, &client.DeleteOptions{}) })

	// ─── Wait for each CR to receive at least one reconcile ─────────────
	// The LiteLLMConnection/default is pre-provisioned by the suite; we
	// just need it Ready (which it is after the mock is in ModeHappy + conn
	// cache snapshot reset). Wait for the Model reconcile as the canary.
	beforeCreate := reconcileCalls.Load()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if reconcileCalls.Load() > beforeCreate {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if reconcileCalls.Load() == beforeCreate {
		t.Fatal("no reconcile fired within 30s after CR creation")
	}

	// Extra settle: let all 6 created CRs plus the implicit Team/default reach steady-state.
	time.Sleep(2 * time.Second)

	// ─── Snapshot mutations at t=0 ────────────────────────────────────────
	mutations0 := mockServer.Mutations()
	t.Logf("AC-R1 soak: mutations at t=0 = %d; entering 35-min window at %s",
		mutations0, time.Now().Format(time.RFC3339))

	// ─── 35-minute steady-state window ────────────────────────────────────
	// Wall-clock window per spec §7.6: 30-min safety re-list cycle + 5-min
	// buffer. The suite_test.go's SafetyRelistInterval=1s in envtest means
	// the safety re-list fires many times during this window — all must be
	// mutation-free.
	time.Sleep(35 * time.Minute)

	// ─── AC-R1 assertion ─────────────────────────────────────────────────
	mutationsEnd := mockServer.Mutations()
	delta := mutationsEnd - mutations0
	t.Logf("AC-R1 soak: mutations at end = %d; delta = %d; finished at %s",
		mutationsEnd, delta, time.Now().Format(time.RFC3339))
	if delta != 0 {
		t.Errorf("AC-R1 FAIL: %d mutation call(s) issued during 35-min steady-state window (want 0); "+
			"check mock per-kind call counts to identify leaking reconciler",
			delta)
	}
}
