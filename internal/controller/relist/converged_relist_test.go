// SPDX-License-Identifier: Apache-2.0

// Isolated envtest proving the RequeueAfter → SafetyRelistRunnable
// convergence actually recovers drift for a newly-converged kind, not just
// a kind (Model, GuardRail) that already had a Runnable before the
// convergence. Mirrors guardrail_relist_test.go's structure and isolation
// rationale (own package/process — no neighbor-test apiserver contention).

package relist

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/metrics"
)

// mcpServerSampleCR returns a minimal valid MCPServer CR.
func mcpServerSampleCR(name string) *litellmv1alpha1.LiteLLMMCPServer {
	return &litellmv1alpha1.LiteLLMMCPServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.MCPServerSpec{
			Endpoint:  "https://mcp.example.com",
			Transport: "http",
		},
	}
}

// ensureNoMCPServerCR removes any pre-existing CR and waits for removal.
func ensureNoMCPServerCR(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	var existing litellmv1alpha1.LiteLLMMCPServer
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	if err := k8sClient.Get(ctx, key, &existing); err == nil {
		existing.SetFinalizers(nil)
		_ = k8sClient.Update(ctx, &existing)
		_ = k8sClient.Delete(ctx, &existing)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, key, &existing); apierrors.IsNotFound(err) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Logf("warning: LiteLLMMCPServer %q still present after 10s cleanup wait", name)
}

// pollMCPServerCondition polls the Ready condition reason for up to 30s.
func pollMCPServerCondition(t *testing.T, ctx context.Context, name, wantReason string) {
	t.Helper()
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var mcp litellmv1alpha1.LiteLLMMCPServer
		if err := k8sClient.Get(ctx, key, &mcp); err == nil {
			c := apimeta.FindStatusCondition(mcp.Status.Conditions, conditionTypeReady)
			if c != nil && c.Reason == wantReason {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("mcpserver %q never reached Ready reason %q within 30s", name, wantReason)
}

// pollMCPServerID polls until status.lastRendered.serverID is non-empty.
func pollMCPServerID(t *testing.T, ctx context.Context, name string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	for time.Now().Before(deadline) {
		var mcp litellmv1alpha1.LiteLLMMCPServer
		if err := k8sClient.Get(ctx, key, &mcp); err == nil && mcp.Status.LastRendered.ServerID != "" {
			return mcp.Status.LastRendered.ServerID
		}
		time.Sleep(50 * time.Millisecond)
	}
	return ""
}

// TestMCPServer_SafetyRelist_CreateMissing — with all RequeueAfter paths
// removed, the MCPServer's SafetyRelistRunnable is the only thing that can
// notice an out-of-band delete in LiteLLM and re-create the entry. Mirrors
// TestGuardRail_SafetyRelist_CreateMissing.
//
// Unlike GuardRail, MCPServer pins server_id to the sanitized CR name on
// CREATE (never server-minted — see mcpserver_controller.go Step 9), so a
// recovered entry keeps the SAME id. No manual status clear is needed
// either: MCPServer's Reconcile has its own Step 7b existence probe that
// clears status.lastRendered.serverID in-memory the moment it is
// re-enqueued and finds the entry missing from LiteLLM — the only question
// this test answers is WHAT re-enqueues it after an out-of-band delete
// with no CR change, and per Task 6 that is exclusively the
// SafetyRelistRunnable tick.
func TestMCPServer_SafetyRelist_CreateMissing(t *testing.T) {
	ctx := context.Background()
	name := "mcp-relist-recover"
	ensureNoMCPServerCR(t, ctx, name)
	t.Cleanup(func() { ensureNoMCPServerCR(t, context.Background(), name) })
	mockServer.ResetMCPServers()
	ensureLiteLLMConnectionDefault(t, ctx)
	readyConnectionForTest(t)

	cr := mcpServerSampleCR(name)
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("Create CR: %v", err)
	}
	pollMCPServerCondition(t, ctx, name, "Synced")
	originalID := pollMCPServerID(t, ctx, name, 5*time.Second)
	if originalID == "" {
		t.Fatal("CR never reached non-empty ServerID")
	}
	if got := mockServer.MutationsByMCPServerName(name); got < 1 {
		t.Fatalf("post-CREATE mutations: got %d want >=1", got)
	}

	before := testutil.ToFloat64(metrics.DriftCorrectedTotal.WithLabelValues("mcp", "create_missing"))

	// Out-of-band DELETE — pull the row from mock state without the
	// operator's DELETE path. No CR change, so no watch event fires —
	// only the relist tick can catch this.
	mockServer.DeleteMCPServerOutOfBand(originalID)

	// Unlike GuardRail, the vanish-probe reads through litellm.Client's
	// CachedListMCPServers, which has a hard DefaultListCacheTTL floor
	// (internal/litellm/list_cache.go, 30s) independent of the 100ms
	// relist tick — recovery cannot land before that cache entry expires.
	// 45s budget (30s floor + margin, mirroring the guardrail relist test's
	// same under-load tightening) — loop still breaks on first success.
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		after := testutil.ToFloat64(metrics.DriftCorrectedTotal.WithLabelValues("mcp", "create_missing"))
		if after-before >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	after := testutil.ToFloat64(metrics.DriftCorrectedTotal.WithLabelValues("mcp", "create_missing"))
	if delta := after - before; delta < 1 {
		t.Fatalf("create_missing NOT incremented after out-of-band DELETE + safety re-list within 45s; delta=%.0f", delta)
	}

	if !mockServer.HasMCPServer(name) {
		t.Errorf("mock missing mcpserver %q after recovery POST", name)
	}
	newID := mockServer.GetMCPServerID(name)
	if newID != originalID {
		t.Errorf("mock ServerID after recovery: got %q want unchanged (pinned) %q", newID, originalID)
	}

	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var got litellmv1alpha1.LiteLLMMCPServer
		if err := k8sClient.Get(ctx, key, &got); err == nil {
			if got.Status.LastRendered.ServerID == newID {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("CR status.lastRendered.serverID never re-confirmed %q after recovery", newID)
}
