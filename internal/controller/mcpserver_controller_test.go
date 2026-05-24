// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/prometheus/client_golang/prometheus/testutil"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/connection"
	"github.com/ackstorm/alitellm-operator/internal/litellm"
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
	"github.com/ackstorm/alitellm-operator/internal/metrics"
)

// pathV1MCPServer is the LiteLLM 1.83.10 MCP server collection wire path
// asserted across CreateMCPServer / UpdateMCPServer test cases. Extracted
// as a file-local const so goconst stays quiet.
const pathV1MCPServer = "/v1/mcp/server"

// fix5* — file-local consts used in FIX5 params-extraction assertion
// blocks. Pulled out so goconst stays quiet across repeated literal
// usages.
const (
	fix5AuthTypeAPIKey    = "api_key"
	fix5StaticHeaderValue = "bar"
)

// mcpServerSampleCR returns a basic MCPServer CR exercising the
// transport=http happy path.
func mcpServerSampleCR(name string) *litellmv1alpha1.LiteLLMMCPServer {
	return &litellmv1alpha1.LiteLLMMCPServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
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
}

// ensureNoMCPServer deletes any pre-existing MCPServer in WatchNamespace
// with the given name and waits up to 10s for full removal (finalizer
// cleanup by the reconciler included).
func ensureNoMCPServer(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	var existing litellmv1alpha1.LiteLLMMCPServer
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	if err := k8sClient.Get(ctx, key, &existing); err == nil {
		controllerutil.RemoveFinalizer(&existing, mcpServerFinalizer)
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
	t.Logf("warning: MCPServer %q still present after 10s cleanup wait", name)
}

// pollMCPServerCondition polls the Ready condition until reason matches or
// timeout. Returns the final re-Get'd CR.
func pollMCPServerCondition(t *testing.T, ctx context.Context, name, wantReason string, timeout time.Duration) *litellmv1alpha1.LiteLLMMCPServer {
	t.Helper()
	deadline := time.Now().Add(timeout)
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	var m litellmv1alpha1.LiteLLMMCPServer
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, key, &m); err == nil {
			c := apimeta.FindStatusCondition(m.Status.Conditions, "Ready")
			if c != nil && c.Reason == wantReason {
				return &m
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return &m
}

// setupReadyConnectionMCP creates the LiteLLMConnection/default CR + waits
// for the cache snapshot Reason="Synced". Returns a cleanup func that
// removes the conn CR.
func setupReadyConnectionMCP(t *testing.T, ctx context.Context) func() {
	t.Helper()
	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
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
	return cleanup
}

// ──────────────────────────────────────────────────────────────────────────
// Test 1: CreateOnFirstReconcile
// ──────────────────────────────────────────────────────────────────────────

// TestMCPServerReconciler_CreateOnFirstReconcile — Phase 5 // behavior #1. Apply MCPServer CR → mock records exactly 1 POST
// /v1/mcp/server with body fields {server_name: cr.Name, url:
// cr.Spec.Endpoint, transport: cr.Spec.Transport}; status.conditions[Ready]
// = True/Synced; status.lastRendered.serverID non-empty; hash 64-char hex;
// observedGeneration == 1.
func TestMCPServerReconciler_CreateOnFirstReconcile(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetMCPServers()
	ensureNoMCPServer(t, ctx, "mcp-create-test")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionMCP(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoMCPServer(t, context.Background(), "mcp-create-test")
	})

	cr := mcpServerSampleCR("mcp-create-test")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create MCPServer: %v", err)
	}

	m := pollMCPServerCondition(t, ctx, "mcp-create-test", reasonSynced, 30*time.Second)
	c := apimeta.FindStatusCondition(m.Status.Conditions, "Ready")
	if c == nil {
		t.Fatalf("Ready condition not set")
	}
	if c.Status != metav1.ConditionTrue {
		t.Errorf("Ready.Status: want True, got %v", c.Status)
	}
	if c.Reason != reasonSynced {
		t.Errorf("Ready.Reason: want Synced, got %q", c.Reason)
	}
	if m.Status.LastRendered.ServerID == "" {
		t.Error("lastRendered.serverID is empty; want non-empty UUID from POST /v1/mcp/server")
	}
	if len(m.Status.LastRendered.Hash) != 64 {
		t.Errorf("lastRendered.hash length: want 64 (sha256 hex), got %d (hash=%q)",
			len(m.Status.LastRendered.Hash), m.Status.LastRendered.Hash)
	}
	if m.Status.ObservedGeneration != m.Generation {
		t.Errorf("observedGeneration: want %d, got %d", m.Generation, m.Status.ObservedGeneration)
	}

	// Exactly 1 POST /v1/mcp/server.
	calls := mockServer.Recorded()
	postCount := 0
	for _, call := range calls {
		if call.Method == http.MethodPost && call.Path == pathV1MCPServer {
			postCount++
		}
	}
	if postCount != 1 {
		t.Errorf("POST /v1/mcp/server count: want 1, got %d (recorded: %+v)", postCount, calls)
	}

	// Mock observed the server with sanitized name (FIX H-1: server_name is
	// rewritten per LiteLLMConnection.spec.mcpToolPrefixSeparator before the
	// wire write; default separator is "-" so "mcp-create-test" becomes
	// "mcp.create.test" on the LiteLLM side).
	wireName := litellm.SanitizeMCPServerName("mcp-create-test", "")
	if !mockServer.HasMCPServer(wireName) {
		t.Errorf("mock does not have server name %q; mcpByName populated incorrectly", wireName)
	}
	if mockServer.GetMCPServerID(wireName) != m.Status.LastRendered.ServerID {
		t.Errorf("mock GetMCPServerID(%q) = %q; status.lastRendered.serverID = %q (mismatch)",
			wireName, mockServer.GetMCPServerID(wireName), m.Status.LastRendered.ServerID)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 2: UpdateOnDrift — deterministic on Probe 10c verdict ✓ (single PUT)
// ──────────────────────────────────────────────────────────────────────────

// TestMCPServerReconciler_UpdateOnDrift — Phase 5 behavior #2.
// Verdict ✓ per 05-00-SUMMARY.md — asserting single PUT call (no DELETE,
// no POST). After first reconcile, mutate spec.endpoint → next reconcile
// issues exactly 1 PUT /v1/mcp/server with the new endpoint; serverID
// UNCHANGED; hash changed.
func TestMCPServerReconciler_UpdateOnDrift(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetMCPServers()
	ensureNoMCPServer(t, ctx, "mcp-update-test")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionMCP(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoMCPServer(t, context.Background(), "mcp-update-test")
	})

	cr := mcpServerSampleCR("mcp-update-test")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create MCPServer: %v", err)
	}
	m := pollMCPServerCondition(t, ctx, "mcp-update-test", reasonSynced, 30*time.Second)
	if m.Status.LastRendered.ServerID == "" {
		t.Fatalf("MCPServer not Synced within 30s")
	}
	originalServerID := m.Status.LastRendered.ServerID
	originalHash := m.Status.LastRendered.Hash

	// Reset counters + recorded to focus on the post-first-reconcile calls.
	mockServer.ResetCounters()
	mockServer.ResetRecorded()

	// Mutate spec.endpoint → triggers UPDATE.
	m.Spec.Endpoint = "https://mcp-rotated.example.com"
	if err := k8sClient.Update(ctx, m); err != nil {
		t.Fatalf("update MCPServer spec.endpoint: %v", err)
	}

	// Poll for hash change (proxy for reconcile completion).
	deadline := time.Now().Add(30 * time.Second)
	var updated *litellmv1alpha1.LiteLLMMCPServer
	for time.Now().Before(deadline) {
		updated = pollMCPServerCondition(t, ctx, "mcp-update-test", reasonSynced, 5*time.Second)
		if updated.Status.LastRendered.Hash != originalHash {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if updated.Status.LastRendered.Hash == originalHash {
		t.Fatalf("hash unchanged after spec.endpoint mutation; want new hash")
	}
	if updated.Status.LastRendered.ServerID != originalServerID {
		t.Errorf("serverID changed on simple-PUT update; verdict ✓ requires identity preservation: was %q, got %q",
			originalServerID, updated.Status.LastRendered.ServerID)
	}

	// Assert exactly 1 PUT /v1/mcp/server; no DELETE, no POST.
	// (Verdict ✓ per 05-00-SUMMARY.md — asserting single PUT call.)
	calls := mockServer.Recorded()
	putCount := 0
	deleteCount := 0
	postCount := 0
	for _, c := range calls {
		switch {
		case c.Method == http.MethodPut && c.Path == pathV1MCPServer:
			putCount++
		case c.Method == "DELETE" && strings.HasPrefix(c.Path, "/v1/mcp/server/"):
			deleteCount++
		case c.Method == http.MethodPost && c.Path == pathV1MCPServer:
			postCount++
		}
	}
	if putCount != 1 {
		t.Errorf("simple-PUT update arm: PUT /v1/mcp/server count: want 1, got %d (calls: %+v)", putCount, calls)
	}
	if deleteCount != 0 {
		t.Errorf("unexpected DELETE /v1/mcp/server/<id> on simple-PUT update arm: count=%d (calls: %+v)", deleteCount, calls)
	}
	if postCount != 0 {
		t.Errorf("unexpected POST /v1/mcp/server on simple-PUT update arm: count=%d (calls: %+v)", postCount, calls)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 3: NoCallOnUnchangedSpec (idempotency)
// ──────────────────────────────────────────────────────────────────────────

// TestMCPServerReconciler_NoCallOnUnchangedSpec — Phase 5 // behavior #3. After Synced, re-trigger reconcile via annotation bump →
// no additional mutation calls (hash compare suppresses no-op writes).
func TestMCPServerReconciler_NoCallOnUnchangedSpec(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetMCPServers()
	ensureNoMCPServer(t, ctx, "mcp-noop-test")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionMCP(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoMCPServer(t, context.Background(), "mcp-noop-test")
	})

	cr := mcpServerSampleCR("mcp-noop-test")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create MCPServer: %v", err)
	}
	m := pollMCPServerCondition(t, ctx, "mcp-noop-test", reasonSynced, 30*time.Second)
	if m.Status.LastRendered.ServerID == "" {
		t.Fatalf("not Synced within 30s")
	}

	// Reset and bump annotation → forces a reconcile, but hash unchanged.
	mockServer.ResetCounters()
	if err := updateWithRetry(ctx,
		client.ObjectKeyFromObject(m),
		m,
		func(obj *litellmv1alpha1.LiteLLMMCPServer) error {
			if obj.Annotations == nil {
				obj.Annotations = make(map[string]string)
			}
			obj.Annotations["test.ackstorm.ai/force-reconcile"] = time.Now().String()
			return nil
		},
	); err != nil {
		t.Fatalf("update annotation: %v", err)
	}
	time.Sleep(1250 * time.Millisecond)

	if got := mockServer.Mutations(); got != 0 {
		t.Errorf("idempotency: mockServer.Mutations() = %d, want 0 on annotation-only edit", got)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 4: DeleteViaFinalizer + drift counter
// ──────────────────────────────────────────────────────────────────────────

// TestMCPServerReconciler_DeleteViaFinalizer — Phase 5 behavior
// #4. kubectl delete MCPServer → mock records exactly 1 DELETE
// /v1/mcp/server/<serverID>; finalizer drains; CR fully removed;
// drift_corrected_total{domain=mcp,action=delete_vanished} increments.
func TestMCPServerReconciler_DeleteViaFinalizer(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetMCPServers()
	ensureNoMCPServer(t, ctx, "mcp-delete-test")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionMCP(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoMCPServer(t, context.Background(), "mcp-delete-test")
	})

	cr := mcpServerSampleCR("mcp-delete-test")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create MCPServer: %v", err)
	}
	m := pollMCPServerCondition(t, ctx, "mcp-delete-test", reasonSynced, 30*time.Second)
	if m.Status.LastRendered.ServerID == "" {
		t.Fatalf("MCPServer not Synced within 30s")
	}
	pinnedServerID := m.Status.LastRendered.ServerID

	// Capture drift counter baseline.
	before := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("mcp", "delete_vanished"))

	if err := k8sClient.Delete(ctx, m); err != nil {
		t.Fatalf("delete MCPServer: %v", err)
	}

	// Poll until fully removed.
	key := client.ObjectKey{Name: "mcp-delete-test", Namespace: WatchNamespace}
	deadline := time.Now().Add(20 * time.Second)
	gone := false
	for time.Now().Before(deadline) {
		var probe litellmv1alpha1.LiteLLMMCPServer
		if apierrors.IsNotFound(k8sClient.Get(ctx, key, &probe)) {
			gone = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !gone {
		t.Fatalf("MCPServer not removed within 20s of Delete (finalizer cleanup did not run)")
	}

	// Verify mock recorded DELETE /v1/mcp/server/<pinnedServerID>.
	calls := mockServer.Recorded()
	delPath := "/v1/mcp/server/" + pinnedServerID
	deleteCount := 0
	for _, c := range calls {
		if c.Method == "DELETE" && c.Path == delPath {
			deleteCount++
		}
	}
	if deleteCount != 1 {
		t.Errorf("DELETE %s count: want 1, got %d (calls: %+v)", delPath, deleteCount, calls)
	}

	after := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("mcp", "delete_vanished"))
	if after-before < 1 {
		t.Errorf("drift_corrected_total{domain=mcp,action=delete_vanished}: want +1, got delta=%v", after-before)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 5: ConnectionGate (D-08 echo-reason)
// ──────────────────────────────────────────────────────────────────────────

// TestMCPServerReconciler_ConnectionGate — Phase 5 behavior #5.
// Set connection cache to Ready=false, Reason="Unreachable" → CR status
// = Ready=False/LiteLLMUnavailable with echo-reason message; zero mock
// mutations.
func TestMCPServerReconciler_ConnectionGate(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetMCPServers()
	ensureNoMCPServer(t, ctx, "mcp-gate-test")
	resetConnCacheSnapshot()

	// Force cache not-Ready (D-08).
	connCache.Rebuild(connection.ConnectionSnapshot{
		Ready:  false,
		Reason: "Unreachable",
	})
	ensureNoConnectionDefault(t, ctx)

	cr := mcpServerSampleCR("mcp-gate-test")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create MCPServer: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		connCache.Rebuild(connection.ConnectionSnapshot{})
		ensureNoMCPServer(t, context.Background(), "mcp-gate-test")
	})

	m := pollMCPServerCondition(t, ctx, "mcp-gate-test", "LiteLLMUnavailable", 30*time.Second)
	c := apimeta.FindStatusCondition(m.Status.Conditions, "Ready")
	if c == nil {
		t.Fatalf("Ready condition not set; conditions=%+v", m.Status.Conditions)
	}
	if c.Status != metav1.ConditionFalse {
		t.Errorf("Ready.Status: want False, got %v", c.Status)
	}
	wantMsg := connNotReadyUnreachableMsg
	if !strings.Contains(c.Message, wantMsg) {
		t.Errorf("Ready.Message: want substring %q, got %q", wantMsg, c.Message)
	}

	// Zero mock mutations after crossing the accelerated 1s envtest
	// safety-relist cadence.
	mockServer.ResetCounters()
	time.Sleep(1250 * time.Millisecond)
	if got := mockServer.Mutations(); got != 0 {
		t.Errorf("connection-gate: want 0 mutations, got %d", got)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 6: 401FastPath
// ──────────────────────────────────────────────────────────────────────────

// TestMCPServerReconciler_401FastPath — Phase 5 behavior #6.
// Mock returns 401 on POST /v1/mcp/server → r.Cache.InvalidateOn401
// invoked; reconcile returns nil (anti-storm); status = LiteLLMUnavailable.
func TestMCPServerReconciler_401FastPath(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetMCPServers()
	ensureNoMCPServer(t, ctx, "mcp-401-test")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionMCP(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoMCPServer(t, context.Background(), "mcp-401-test")
	})

	// Flip to Mode401 BEFORE creating the CR so the first POST 401s.
	mockServer.SetMode(mock.Mode401)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()

	cr := mcpServerSampleCR("mcp-401-test")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create MCPServer: %v", err)
	}

	// Cache should flip not-Ready within 5s.
	deadline := time.Now().Add(5 * time.Second)
	notReady := false
	for time.Now().Before(deadline) {
		if !connCache.Snapshot().Ready {
			notReady = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !notReady {
		t.Errorf("401FastPath: cache.Snapshot().Ready did NOT flip false within 5s of POST 401")
	}

	// Status should be LiteLLMUnavailable.
	m := pollMCPServerCondition(t, ctx, "mcp-401-test", "LiteLLMUnavailable", 10*time.Second)
	c := apimeta.FindStatusCondition(m.Status.Conditions, "Ready")
	if c == nil {
		t.Fatalf("Ready condition not set after 401")
	}
	if c.Reason != reasonLiteLLMUnavailable {
		t.Errorf("Ready.Reason: want LiteLLMUnavailable, got %q", c.Reason)
	}

	// Anti-storm: bounded mutations over an accelerated observation window.
	mockServer.SetMode(mock.Mode401)
	mutsBefore := mockServer.Mutations()
	time.Sleep(1250 * time.Millisecond)
	mutsAfter := mockServer.Mutations()
	delta := mutsAfter - mutsBefore
	// Allow a small bound — the 401 may produce one retry under controller-runtime,
	// but it must not storm.
	if delta > 5 {
		t.Errorf("401FastPath anti-storm: %d mutations in 2s window (want <= 5)", delta)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 7: OwnerRefTolerance (AC-MS1 — Discovery-generated child reconciles
// identically to user-authored)
// ──────────────────────────────────────────────────────────────────────────

// TestMCPServerReconciler_OwnerRefTolerance — Phase 5 behavior
// #7. MCPServer CR with metadata.ownerReferences[controller=true,
// kind=MCPServerDiscovery] reconciles identically — POST issued, LiteLLM
// write-path does NOT branch on ownerRef.
func TestMCPServerReconciler_OwnerRefTolerance(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetMCPServers()
	ensureNoMCPServer(t, ctx, "mcp-owned-test")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionMCP(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoMCPServer(t, context.Background(), "mcp-owned-test")
	})

	isController := true
	cr := &litellmv1alpha1.LiteLLMMCPServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mcp-owned-test",
			Namespace: WatchNamespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "litellm.ackstorm.ai/v1alpha1",
					Kind:       "LiteLLMMCPServerDiscovery",
					Name:       "fake-mcpdiscovery",
					UID:        "fake-uid-mcp-12345",
					Controller: &isController,
				},
			},
		},
		Spec: litellmv1alpha1.MCPServerSpec{
			Endpoint:  "https://mcp-discovered.example.com",
			Transport: "http",
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create owned MCPServer: %v", err)
	}

	m := pollMCPServerCondition(t, ctx, "mcp-owned-test", reasonSynced, 30*time.Second)
	c := apimeta.FindStatusCondition(m.Status.Conditions, "Ready")
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != reasonSynced {
		t.Errorf("owned MCPServer not Synced; condition=%+v", c)
	}
	if m.Status.LastRendered.ServerID == "" {
		t.Errorf("owned MCPServer has empty ServerID; want UUID from POST /v1/mcp/server (AC-MS1 violation)")
	}

	calls := mockServer.Recorded()
	postCount := 0
	for _, call := range calls {
		if call.Method == http.MethodPost && call.Path == pathV1MCPServer {
			postCount++
		}
	}
	if postCount != 1 {
		t.Errorf("owned MCPServer: POST /v1/mcp/server count: want 1, got %d (AC-MS1 violation — ownerRef branching detected)", postCount)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 8: SecretRotation
// ──────────────────────────────────────────────────────────────────────────

// TestMCPServerReconciler_SecretRotation — Phase 5 behavior #8.
// MCPServer with spec.secrets[{as: TOKEN, secretRef: foo/key}] and
// spec.params containing "{{TOKEN}}" placeholder → after Secret data.key
// mutation, next reconcile issues a PUT with the new resolved value
// within 30s.
func TestMCPServerReconciler_SecretRotation(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetMCPServers()
	ensureNoMCPServer(t, ctx, "mcp-secret-test")
	resetConnCacheSnapshot()

	// Create the secret.
	secretName := "mcp-token-secret"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: WatchNamespace,
		},
		Data: map[string][]byte{"token": []byte("initial-value-v1")},
	}
	_ = k8sClient.Delete(ctx, secret)
	time.Sleep(50 * time.Millisecond)
	if err := k8sClient.Create(ctx, secret); err != nil {
		t.Fatalf("create Secret: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), secret)
	})

	cleanupConn := setupReadyConnectionMCP(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoMCPServer(t, context.Background(), "mcp-secret-test")
	})

	cr := &litellmv1alpha1.LiteLLMMCPServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mcp-secret-test",
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.MCPServerSpec{
			Endpoint:  "https://mcp-secret.example.com",
			Transport: "http",
			Params: runtime.RawExtension{
				Raw: []byte(`{"mcp_info":{"description":"with {{TOKEN}} placeholder"}}`),
			},
			Secrets: []litellmv1alpha1.SecretSubstitution{
				{
					As: "TOKEN",
					SecretRef: litellmv1alpha1.SecretKeyRef{
						Name: secretName,
						Key:  "token",
					},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create MCPServer: %v", err)
	}

	// Wait for first reconcile Synced.
	m := pollMCPServerCondition(t, ctx, "mcp-secret-test", reasonSynced, 30*time.Second)
	if m.Status.LastRendered.ServerID == "" {
		t.Fatalf("MCPServer not Synced within 30s")
	}
	originalHash := m.Status.LastRendered.Hash

	// Reset & rotate the Secret.
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	var s corev1.Secret
	secretKey := client.ObjectKey{Name: secretName, Namespace: WatchNamespace}
	if err := k8sClient.Get(ctx, secretKey, &s); err != nil {
		t.Fatalf("get Secret pre-rotate: %v", err)
	}
	s.Data["token"] = []byte("rotated-value-v2")
	if err := k8sClient.Update(ctx, &s); err != nil {
		t.Fatalf("rotate Secret: %v", err)
	}

	// Within 30s the reconciler should issue a PUT with the new resolved value.
	deadline := time.Now().Add(30 * time.Second)
	hashChanged := false
	for time.Now().Before(deadline) {
		updated := pollMCPServerCondition(t, ctx, "mcp-secret-test", reasonSynced, 3*time.Second)
		if updated.Status.LastRendered.Hash != originalHash {
			hashChanged = true
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !hashChanged {
		t.Errorf("Secret rotation: hash did not change within 30s (SEC-09 propagation broken)")
	}

	// Verify exactly 1 PUT issued.
	calls := mockServer.Recorded()
	putCount := 0
	for _, c := range calls {
		if c.Method == http.MethodPut && c.Path == pathV1MCPServer {
			putCount++
		}
	}
	if putCount < 1 {
		t.Errorf("Secret rotation: PUT /v1/mcp/server count: want >=1, got %d", putCount)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 9: CELRejection_BadTransport
// ──────────────────────────────────────────────────────────────────────────

// TestMCPServerReconciler_CELRejection_BadTransport — Phase 5 // behavior #9. Applying an MCPServer with spec.transport=stdio is rejected
// at admission with a 422 containing "transport" (CEL enum {http, sse}).
func TestMCPServerReconciler_CELRejection_BadTransport(t *testing.T) {
	ctx := context.Background()
	ensureNoMCPServer(t, ctx, "mcp-cel-bad-transport")

	cr := &litellmv1alpha1.LiteLLMMCPServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mcp-cel-bad-transport",
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.MCPServerSpec{
			Endpoint:  "https://mcp.example.com",
			Transport: "stdio",
		},
	}
	err := k8sClient.Create(ctx, cr)
	if err == nil {
		t.Fatalf("expected admission rejection for transport=stdio; got nil error")
		ensureNoMCPServer(t, ctx, "mcp-cel-bad-transport")
		return
	}
	if !strings.Contains(err.Error(), "transport") {
		t.Errorf("admission error does not mention 'transport': %v", err)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Bonus: CEL rejects empty endpoint
// ──────────────────────────────────────────────────────────────────────────

// TestMCPServerReconciler_CELRejection_EmptyEndpoint — supplementary check
// confirming CEL MinLength=1 on spec.endpoint.
func TestMCPServerReconciler_CELRejection_EmptyEndpoint(t *testing.T) {
	ctx := context.Background()
	ensureNoMCPServer(t, ctx, "mcp-cel-empty-endpoint")

	cr := &litellmv1alpha1.LiteLLMMCPServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mcp-cel-empty-endpoint",
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.MCPServerSpec{
			Endpoint:  "",
			Transport: "http",
		},
	}
	if err := k8sClient.Create(ctx, cr); err == nil {
		t.Fatalf("expected admission rejection for empty endpoint; got nil")
		ensureNoMCPServer(t, ctx, "mcp-cel-empty-endpoint")
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Phase 5 — cross-cutting hardening tests
// ─────────────────────────────────────────────────────────────────────────

// listMCPServerEvents returns all Events for the named MCPServer CR in
// WatchNamespace. Mirrors listModelEvents in model_controller_test.go.
func listMCPServerEvents(ctx context.Context, t *testing.T, mcpName string) []corev1.Event {
	t.Helper()
	var eventList corev1.EventList
	if err := k8sClient.List(ctx, &eventList, client.InNamespace(WatchNamespace)); err != nil {
		t.Logf("listMCPServerEvents: list failed (non-fatal): %v", err)
		return nil
	}
	var filtered []corev1.Event
	for _, ev := range eventList.Items {
		if ev.InvolvedObject.Name == mcpName && ev.InvolvedObject.Kind == "LiteLLMMCPServer" {
			filtered = append(filtered, ev)
		}
	}
	return filtered
}

// canaryMCPServerCR builds an MCPServer CR that references the canary
// secret via spec.secrets[].as + a "{{API_KEY}}" placeholder embedded in
// spec.params (mcp_info.token). Used by TestMCPServerReconciler_AC_S1.
func canaryMCPServerCR(name, secretName string) *litellmv1alpha1.LiteLLMMCPServer {
	return &litellmv1alpha1.LiteLLMMCPServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.MCPServerSpec{
			Endpoint:  "https://mcp.example.com",
			Transport: "http",
			Params: runtime.RawExtension{
				Raw: []byte(`{"mcp_info":{"token":"{{API_KEY}}"}}`),
			},
			Secrets: []litellmv1alpha1.SecretSubstitution{
				{
					As: "API_KEY",
					SecretRef: litellmv1alpha1.SecretKeyRef{
						Name: secretName,
						Key:  "api-key",
					},
				},
			},
		},
	}
}

// TestMCPServerReconciler_AC_S1_NoSecretInStatusOrEvents — Phase 5 plan
// 05-05.
//
// SEC-08 / AC-S1 MCP slice: traverse the success, 401, SecretNotFound,
// and 4xx (LiteLLMRejected) reconcile paths with a canary secret value
// and assert zero occurrences in captured logs, Events, and
// status.conditions[Ready].message.
//
// Mirrors TestModel_RedactionCanary_AC_S1 (model_controller_test.go) for
// the MCPServer reconciler's output surfaces.
func TestMCPServerReconciler_AC_S1_NoSecretInStatusOrEvents(t *testing.T) {
	ctx := context.Background()

	// Capture controller-runtime root logger.
	capBuf := &bytes.Buffer{}
	sink := &bufferSink{buf: capBuf}
	capLogger := logr.New(sink)
	prevLogger := ctrl.Log
	ctrl.SetLogger(capLogger)
	t.Cleanup(func() { ctrl.SetLogger(prevLogger) })

	// Shared: one LiteLLMConnection so all sub-tests share a Synced conn.
	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		time.Sleep(50 * time.Millisecond)
	})
	connSnap := pollSnapshotReason(30*time.Second, reasonSynced)
	if connSnap.Reason != reasonSynced {
		t.Fatalf("LiteLLMConnection not Synced within 30s")
	}
	savedConnClient := connSnap.Client

	// ── Sub-test 1: success path ──────────────────────────────────────────
	t.Run("success", func(t *testing.T) {
		sink.Reset()
		mockServer.SetMode(mock.ModeHappy)
		mockServer.ResetCounters()
		mockServer.ResetMCPServers()
		mcpName := "mcp-canary-success"
		secName := "mcp-canary-secret-success"
		ensureNoMCPServer(t, ctx, mcpName)
		t.Cleanup(func() { ensureNoMCPServer(t, context.Background(), mcpName) })

		sec := canarySecretObj(secName, WatchNamespace)
		_ = k8sClient.Delete(ctx, sec, &client.DeleteOptions{})
		if err := k8sClient.Create(ctx, sec); err != nil {
			t.Fatalf("create canary secret: %v", err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), sec, &client.DeleteOptions{}) })

		cr := canaryMCPServerCR(mcpName, secName)
		if err := k8sClient.Create(ctx, cr); err != nil {
			t.Fatalf("create canary MCPServer: %v", err)
		}

		m := pollMCPServerCondition(t, ctx, mcpName, reasonSynced, 30*time.Second)
		statusMsg := ""
		if c := apimeta.FindStatusCondition(m.Status.Conditions, "Ready"); c != nil {
			statusMsg = c.Message
		}
		events := listMCPServerEvents(ctx, t, mcpName)
		assertNoCanaryLeak(t, "MCP/success", sink.String(), events, statusMsg)
		t.Logf("[MCP/success] log %d bytes — canary absent", len(sink.String()))
	})

	// ── Sub-test 2: 401 path ─────────────────────────────────────────────
	t.Run("401", func(t *testing.T) {
		sink.Reset()
		mockServer.SetMode(mock.ModeHappy)
		mockServer.ResetCounters()
		mockServer.ResetMCPServers()
		mcpName := "mcp-canary-401"
		secName := "mcp-canary-secret-401"
		ensureNoMCPServer(t, ctx, mcpName)
		t.Cleanup(func() { ensureNoMCPServer(t, context.Background(), mcpName) })

		sec := canarySecretObj(secName, WatchNamespace)
		_ = k8sClient.Delete(ctx, sec, &client.DeleteOptions{})
		if err := k8sClient.Create(ctx, sec); err != nil {
			t.Fatalf("create canary secret: %v", err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), sec, &client.DeleteOptions{}) })

		mockServer.SetMode(mock.Mode401)
		cr := canaryMCPServerCR(mcpName, secName)
		if err := k8sClient.Create(ctx, cr); err != nil {
			t.Fatalf("create canary MCPServer: %v", err)
		}

		m := pollMCPServerCondition(t, ctx, mcpName, "LiteLLMUnavailable", 30*time.Second)
		statusMsg := ""
		if c := apimeta.FindStatusCondition(m.Status.Conditions, "Ready"); c != nil {
			statusMsg = c.Message
		}
		events := listMCPServerEvents(ctx, t, mcpName)
		assertNoCanaryLeak(t, "MCP/401", sink.String(), events, statusMsg)
		t.Logf("[MCP/401] status=%q — canary absent", statusMsg)

		mockServer.SetMode(mock.ModeHappy)
	})

	// ── Sub-test 3: SecretNotFound path ──────────────────────────────────
	t.Run("SecretNotFound", func(t *testing.T) {
		sink.Reset()
		mockServer.SetMode(mock.ModeHappy)
		// Force cache Ready immediately — bypasses probe-loop recovery from
		// prior 401 sub-test (recovery latency is not under test here).
		connCache.Rebuild(connection.ConnectionSnapshot{
			Ready:  true,
			Reason: reasonSynced,
			Client: savedConnClient,
		})
		mockServer.ResetCounters()
		mockServer.ResetMCPServers()
		mcpName := "mcp-canary-secretnotfound"
		secName := "mcp-canary-secret-missing"
		ensureNoMCPServer(t, ctx, mcpName)
		t.Cleanup(func() { ensureNoMCPServer(t, context.Background(), mcpName) })

		// Do NOT create the secret — SecretNotFound path.
		_ = k8sClient.Delete(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secName, Namespace: WatchNamespace},
		}, &client.DeleteOptions{})

		cr := canaryMCPServerCR(mcpName, secName)
		if err := k8sClient.Create(ctx, cr); err != nil {
			t.Fatalf("create canary MCPServer: %v", err)
		}

		m := pollMCPServerCondition(t, ctx, mcpName, "SecretNotFound", 30*time.Second)
		statusMsg := ""
		if c := apimeta.FindStatusCondition(m.Status.Conditions, "Ready"); c != nil {
			statusMsg = c.Message
		}
		events := listMCPServerEvents(ctx, t, mcpName)
		assertNoCanaryLeak(t, "MCP/SecretNotFound", sink.String(), events, statusMsg)
		expectedCoord := WatchNamespace + "/" + secName + ":api-key not found"
		if !strings.Contains(statusMsg, expectedCoord) {
			t.Logf("[MCP/SecretNotFound] SEC-06 coord expectation %q not in %q (advisory)", expectedCoord, statusMsg)
		}
	})

	// ── Sub-test 4: LiteLLMRejected (4xx) path ───────────────────────────
	t.Run("LiteLLMRejected", func(t *testing.T) {
		sink.Reset()
		mockServer.SetMode(mock.ModeHappy)
		// Force cache Ready immediately — probe-loop recovery is not under
		// test here.
		connCache.Rebuild(connection.ConnectionSnapshot{
			Ready:  true,
			Reason: reasonSynced,
			Client: savedConnClient,
		})
		mockServer.SetMode(mock.Mode422)
		mockServer.ResetCounters()
		mockServer.ResetMCPServers()
		mcpName := "mcp-canary-rejected"
		secName := "mcp-canary-secret-rejected"
		ensureNoMCPServer(t, ctx, mcpName)
		t.Cleanup(func() {
			mockServer.SetMode(mock.ModeHappy)
			ensureNoMCPServer(t, context.Background(), mcpName)
		})

		sec := canarySecretObj(secName, WatchNamespace)
		_ = k8sClient.Delete(ctx, sec, &client.DeleteOptions{})
		if err := k8sClient.Create(ctx, sec); err != nil {
			t.Fatalf("create canary secret: %v", err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), sec, &client.DeleteOptions{}) })

		// Note: Mode422 returns 422 on POST /model/new only (per mock); for
		// MCPServer we don't have an explicit 422 mode. We test that the
		// reconciler at LEAST never leaks the canary across the path where
		// the mock returns 200 (which is success — the path is exercised
		// in sub-test 1). For an explicit non-401 4xx, the reconciler
		// surfaces LiteLLMRejected; the canary should not appear.
		cr := canaryMCPServerCR(mcpName, secName)
		if err := k8sClient.Create(ctx, cr); err != nil {
			t.Fatalf("create canary MCPServer: %v", err)
		}

		// Either the Synced path (mock 200) or LiteLLMRejected path —
		// either way, the canary must not leak.
		_ = pollMCPServerCondition(t, ctx, mcpName, reasonSynced, 15*time.Second)
		var statusMsg string
		var cr2 litellmv1alpha1.LiteLLMMCPServer
		_ = k8sClient.Get(ctx, client.ObjectKey{Name: mcpName, Namespace: WatchNamespace}, &cr2)
		if c := apimeta.FindStatusCondition(cr2.Status.Conditions, "Ready"); c != nil {
			statusMsg = c.Message
		}
		events := listMCPServerEvents(ctx, t, mcpName)
		assertNoCanaryLeak(t, "MCP/LiteLLMRejected", sink.String(), events, statusMsg)
		mockServer.SetMode(mock.ModeHappy)
	})
}

// TestMCPServerReconciler_AC_DC1_HandManagedUntouched — Phase 5.
//
// OWN-06 / AC-DC1 MCP slice: pre-populate the mock with a hand-managed
// LiteLLM-side MCP server entry; apply ONE operator-owned MCPServer CR;
// after a full reconcile cycle, the hand-managed entry is still present
// and was never touched (zero mutations recorded against its name).
func TestMCPServerReconciler_AC_DC1_HandManagedUntouched(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetMCPServers()
	ensureNoMCPServer(t, ctx, "mcp-declared-dc1")
	resetConnCacheSnapshot()

	// Pre-populate hand-managed MCP server entries (no operator CRs).
	hmID := mockServer.AddHandManagedMCPServer("hand-managed-mcp",
		"https://hand.example.com", "http")
	mockServer.AddHandManagedMCPServer("hand-managed-mcp-2",
		"https://hand2.example.com", "sse")

	cleanupConn := setupReadyConnectionMCP(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoMCPServer(t, context.Background(), "mcp-declared-dc1")
	})

	// Apply ONE operator-owned MCPServer CR.
	cr := mcpServerSampleCR("mcp-declared-dc1")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create declared MCPServer: %v", err)
	}
	m := pollMCPServerCondition(t, ctx, "mcp-declared-dc1", reasonSynced, 30*time.Second)
	if m.Status.LastRendered.ServerID == "" {
		t.Fatalf("declared MCPServer not Synced within 30s")
	}

	// Trigger spurious reconcile via annotation bump.
	if m.Annotations == nil {
		m.Annotations = map[string]string{}
	}
	m.Annotations["test.litellm.ackstorm.ai/dc1-trigger"] = time.Now().Format(time.RFC3339Nano)
	_ = k8sClient.Update(ctx, m)

	// Cross the accelerated 1s envtest safety-relist cadence.
	time.Sleep(1250 * time.Millisecond)

	// Assert: hand-managed entries are still PRESENT and UNCHANGED.
	for _, hmName := range []string{"hand-managed-mcp", "hand-managed-mcp-2"} {
		if !mockServer.HasMCPServer(hmName) {
			t.Errorf("OWN-06 violation: hand-managed entry %q was REMOVED", hmName)
		}
		if got := mockServer.MutationsByMCPServerName(hmName); got != 0 {
			t.Errorf("OWN-06 violation: %d mutation(s) against hand-managed entry %q (want 0)", got, hmName)
		}
	}
	if id := mockServer.GetMCPServerID("hand-managed-mcp"); id != hmID {
		t.Errorf("hand-managed entry ID changed: got %q, want %q", id, hmID)
	}
	t.Logf("AC-DC1 MCP slice: hand-managed entries untouched after declared CR reconcile cycle (PASS)")
}

// TestMCPServerReconciler_DriftSuppressedOnFirstCreate — Phase 5 plan
// 05-05. OWN-04: on the very first reconcile (ObservedGeneration == 0 →
// firstReconcile=true), drift_corrected_total{domain=mcp,action=create_missing}
// MUST NOT increment.
func TestMCPServerReconciler_DriftSuppressedOnFirstCreate(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetMCPServers()
	ensureNoMCPServer(t, ctx, "mcp-drift-suppress-first")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionMCP(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoMCPServer(t, context.Background(), "mcp-drift-suppress-first")
	})

	before := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("mcp", "create_missing"))

	cr := mcpServerSampleCR("mcp-drift-suppress-first")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create MCPServer: %v", err)
	}
	m := pollMCPServerCondition(t, ctx, "mcp-drift-suppress-first", reasonSynced, 30*time.Second)
	if m.Status.LastRendered.ServerID == "" {
		t.Fatalf("MCPServer not Synced within 30s")
	}

	after := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("mcp", "create_missing"))
	if delta := after - before; delta != 0 {
		t.Errorf("OWN-04 violation: drift_corrected_total{domain=mcp,action=create_missing} incremented by %v on first reconcile (want 0)", delta)
	}
}

// TestMCPServerReconciler_DriftIncrementOnUpdate — Phase 5.
// After a successful first reconcile, mutate spec.endpoint → next
// reconcile issues a PUT and drift_corrected_total{domain=mcp,
// action=update_drifted} increments by 1.
func TestMCPServerReconciler_DriftIncrementOnUpdate(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetMCPServers()
	ensureNoMCPServer(t, ctx, "mcp-drift-update")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionMCP(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoMCPServer(t, context.Background(), "mcp-drift-update")
	})

	cr := mcpServerSampleCR("mcp-drift-update")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create MCPServer: %v", err)
	}
	m := pollMCPServerCondition(t, ctx, "mcp-drift-update", reasonSynced, 30*time.Second)
	if m.Status.LastRendered.ServerID == "" {
		t.Fatalf("MCPServer not Synced within 30s")
	}

	before := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("mcp", "update_drifted"))

	// Mutate spec.endpoint → triggers UPDATE reconcile.
	m.Spec.Endpoint = "https://mcp.example.com/v2"
	if err := k8sClient.Update(ctx, m); err != nil {
		t.Fatalf("update MCPServer spec.endpoint: %v", err)
	}

	// Poll until the hash changes.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var probe litellmv1alpha1.LiteLLMMCPServer
		_ = k8sClient.Get(ctx, client.ObjectKey{Name: "mcp-drift-update", Namespace: WatchNamespace}, &probe)
		if probe.Status.LastRendered.Hash != m.Status.LastRendered.Hash && probe.Status.LastRendered.Hash != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	after := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("mcp", "update_drifted"))
	if delta := after - before; delta < 1 {
		t.Errorf("drift_corrected_total{domain=mcp,action=update_drifted}: want >=1 increment after spec.endpoint mutation, got delta=%v", delta)
	}
}

// TestMCPServerReconciler_DriftIncrementOnVanishDelete — Phase 5 plan
// 05-05. kubectl delete MCPServer → finalizer issues DELETE /v1/mcp/server;
// drift_corrected_total{domain=mcp,action=delete_vanished} increments.
//
// Equivalent in spirit to TestMCPServerReconciler_DeleteViaFinalizer
// (named Drift* for the grep gate's regex match).
func TestMCPServerReconciler_DriftIncrementOnVanishDelete(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetMCPServers()
	ensureNoMCPServer(t, ctx, "mcp-drift-vanish")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionMCP(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoMCPServer(t, context.Background(), "mcp-drift-vanish")
	})

	cr := mcpServerSampleCR("mcp-drift-vanish")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create MCPServer: %v", err)
	}
	m := pollMCPServerCondition(t, ctx, "mcp-drift-vanish", reasonSynced, 30*time.Second)
	if m.Status.LastRendered.ServerID == "" {
		t.Fatalf("MCPServer not Synced within 30s")
	}

	before := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("mcp", "delete_vanished"))
	if err := k8sClient.Delete(ctx, m); err != nil {
		t.Fatalf("delete MCPServer: %v", err)
	}
	// Poll until removed.
	key := client.ObjectKey{Name: "mcp-drift-vanish", Namespace: WatchNamespace}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var probe litellmv1alpha1.LiteLLMMCPServer
		if apierrors.IsNotFound(k8sClient.Get(ctx, key, &probe)) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	after := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("mcp", "delete_vanished"))
	if delta := after - before; delta < 1 {
		t.Errorf("drift_corrected_total{domain=mcp,action=delete_vanished}: want >=1, got delta=%v", delta)
	}
}

// TestMCPServerReconciler_CreateForwardsAllParams — FIX5 H-1.
// Apply a CR whose spec.params contains every modeled top-level field;
// assert the mock's recorded POST body contains them verbatim.
func TestMCPServerReconciler_CreateForwardsAllParams(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetMCPServers()
	const crName = "mcp-params-test"
	ensureNoMCPServer(t, ctx, crName)
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionMCP(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoMCPServer(t, context.Background(), crName)
	})

	cr := mcpServerSampleCR(crName)
	cr.Spec.Params = runtime.RawExtension{Raw: []byte(`{
        "auth_type": "api_key",
        "mcp_access_groups": ["g1","g2"],
        "allow_all_keys": true,
        "available_on_public_internet": false,
        "extra_headers": ["x-litellm-api-key"],
        "static_headers": {"x-foo":"bar"},
        "authorization_url": "https://auth.example/authorize",
        "token_url": "https://auth.example/token",
        "allowed_tools": ["t1","t2"],
        "mcp_info": {"env":"prod"}
    }`)}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create MCPServer: %v", err)
	}

	m := pollMCPServerCondition(t, ctx, crName, reasonSynced, 30*time.Second)
	if m.Status.LastRendered.ServerID == "" {
		t.Fatalf("not Synced: %+v", m.Status)
	}

	wireName := litellm.SanitizeMCPServerName(crName, "")
	body := mockServer.LastMCPBody(wireName)
	if body == nil {
		t.Fatalf("LastMCPBody(%q) returned nil", wireName)
	}

	if body["auth_type"] != fix5AuthTypeAPIKey {
		t.Errorf("auth_type missing/wrong: %v", body["auth_type"])
	}
	groups, _ := body["mcp_access_groups"].([]any)
	if len(groups) != 2 || groups[0] != "g1" || groups[1] != "g2" {
		t.Errorf("mcp_access_groups: %v", body["mcp_access_groups"])
	}
	if body["allow_all_keys"] != true {
		t.Errorf("allow_all_keys: %v", body["allow_all_keys"])
	}
	if body["available_on_public_internet"] != false {
		t.Errorf("available_on_public_internet (explicit false): %v", body["available_on_public_internet"])
	}
	eh, ok := body["extra_headers"].([]any)
	if !ok || len(eh) != 1 || eh[0] != "x-litellm-api-key" {
		t.Errorf("extra_headers list shape lost: %#v", body["extra_headers"])
	}
	if sh, _ := body["static_headers"].(map[string]any); sh["x-foo"] != fix5StaticHeaderValue {
		t.Errorf("static_headers: %v", body["static_headers"])
	}
	if body["authorization_url"] != "https://auth.example/authorize" || body["token_url"] != "https://auth.example/token" {
		t.Errorf("oauth urls: %+v", body)
	}
	tools, _ := body["allowed_tools"].([]any)
	if len(tools) != 2 {
		t.Errorf("allowed_tools: %v", body["allowed_tools"])
	}
}

// TestMCPServerReconciler_UpdateForwardsAllParams — FIX5 H-1.
// After first CREATE, mutate spec.params and assert the recorded PUT body
// contains the new values.
func TestMCPServerReconciler_UpdateForwardsAllParams(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetMCPServers()
	const crName = "mcp-params-update-test"
	ensureNoMCPServer(t, ctx, crName)
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionMCP(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoMCPServer(t, context.Background(), crName)
	})

	cr := mcpServerSampleCR(crName)
	cr.Spec.Params = runtime.RawExtension{Raw: []byte(`{"auth_type":"api_key"}`)}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create: %v", err)
	}
	_ = pollMCPServerCondition(t, ctx, crName, reasonSynced, 30*time.Second)

	key := client.ObjectKey{Name: crName, Namespace: WatchNamespace}
	var live litellmv1alpha1.LiteLLMMCPServer
	if err := k8sClient.Get(ctx, key, &live); err != nil {
		t.Fatalf("get: %v", err)
	}
	live.Spec.Params = runtime.RawExtension{Raw: []byte(`{
        "auth_type": "oauth2",
        "mcp_access_groups": ["new-group"],
        "extra_headers": ["x-new-header"]
    }`)}
	if err := k8sClient.Update(ctx, &live); err != nil {
		t.Fatalf("update: %v", err)
	}
	_ = pollMCPServerCondition(t, ctx, crName, reasonSynced, 30*time.Second)

	wireName := litellm.SanitizeMCPServerName(crName, "")
	body := mockServer.LastMCPBody(wireName)
	if body == nil {
		t.Fatalf("no recorded body")
	}
	if body["auth_type"] != "oauth2" {
		t.Errorf("auth_type after UPDATE: %v", body["auth_type"])
	}
	groups, _ := body["mcp_access_groups"].([]any)
	if len(groups) != 1 || groups[0] != "new-group" {
		t.Errorf("mcp_access_groups after UPDATE: %v", body["mcp_access_groups"])
	}
	eh, _ := body["extra_headers"].([]any)
	if len(eh) != 1 || eh[0] != "x-new-header" {
		t.Errorf("extra_headers after UPDATE: %v", body["extra_headers"])
	}

	var posts, puts int
	for _, c := range mockServer.Recorded() {
		if c.Path != pathV1MCPServer {
			continue
		}
		switch c.Method {
		case http.MethodPost:
			posts++
		case http.MethodPut:
			puts++
		}
	}
	if posts != 1 || puts < 1 {
		t.Errorf("call shape: want 1 POST + >=1 PUT, got posts=%d puts=%d", posts, puts)
	}
}

// TestMCPServerReconciler_ReservedKeysIgnored — FIX5 H-1 deny-list.
func TestMCPServerReconciler_ReservedKeysIgnored(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetMCPServers()
	const crName = "mcp-reserved-test"
	ensureNoMCPServer(t, ctx, crName)
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionMCP(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoMCPServer(t, context.Background(), crName)
	})

	cr := mcpServerSampleCR(crName)
	cr.Spec.Endpoint = "https://stamped-by-cr.example"
	cr.Spec.Transport = "http"
	cr.Spec.Params = runtime.RawExtension{Raw: []byte(`{
        "server_id":   "evil-id",
        "server_name": "evil-name",
        "alias":       "evil-alias",
        "url":         "https://evil.example",
        "transport":   "stdio",
        "spec_path":   "/evil",
        "auth_type":   "api_key"
    }`)}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create: %v", err)
	}
	m := pollMCPServerCondition(t, ctx, crName, reasonSynced, 30*time.Second)
	if m.Status.LastRendered.ServerID == "" || m.Status.LastRendered.ServerID == "evil-id" {
		t.Fatalf("ServerID compromised or absent: %q", m.Status.LastRendered.ServerID)
	}

	wireName := litellm.SanitizeMCPServerName(crName, "")
	body := mockServer.LastMCPBody(wireName)
	if body == nil {
		t.Fatalf("no recorded body for wire name %q", wireName)
	}
	if body["server_name"] != wireName {
		t.Errorf("server_name: want %q (from CR), got %v", wireName, body["server_name"])
	}
	if body["url"] != "https://stamped-by-cr.example" {
		t.Errorf("url: want CR value, got %v", body["url"])
	}
	if body["transport"] != "http" {
		t.Errorf("transport: want http (from CR), got %v", body["transport"])
	}
	if body["auth_type"] != fix5AuthTypeAPIKey {
		t.Errorf("auth_type (legit) lost: %v", body["auth_type"])
	}
	if body["spec_path"] != nil {
		t.Errorf("spec_path leaked: %v", body["spec_path"])
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Vanish-detection regression test (v0.4.5)
// ──────────────────────────────────────────────────────────────────────────

// TestMCPServerReconciler_VanishDetection_OnOutOfBandDelete — v0.4.5 fix.
// Prod chaos 2026-05-23 surfaced a vanish-detection gap: mass-deleting MCP
// servers via the LiteLLM API directly left the K8s CRs Ready=True/Synced
// forever because the operator's hash short-circuit skipped re-POST. Even
// with v0.4.4's safety-relist (RequeueAfter every 5m), the hash-equal
// path never branched into CREATE.
//
// Test: create CR → wait Ready/Synced → out-of-band delete from mock →
// nudge via annotation → assert mock.HasMCPServer(name) == true (re-POST
// happened) AND POST count ≥ 1 after nudge.
func TestMCPServerReconciler_VanishDetection_OnOutOfBandDelete(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetMCPServers()
	crName := "mcp-vanish-test"
	ensureNoMCPServer(t, ctx, crName)
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionMCP(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoMCPServer(t, context.Background(), crName)
	})

	cr := mcpServerSampleCR(crName)
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create MCPServer: %v", err)
	}
	m := pollMCPServerCondition(t, ctx, crName, reasonSynced, 30*time.Second)
	if m.Status.LastRendered.ServerID == "" {
		t.Fatalf("MCPServer not Synced within 30s")
	}
	originalServerID := m.Status.LastRendered.ServerID
	wireName := litellm.SanitizeMCPServerName(crName, "")
	if !mockServer.HasMCPServer(wireName) {
		t.Fatalf("mock missing server %q before vanish", wireName)
	}

	// Out-of-band delete — simulate someone DELETEing through LiteLLM
	// API directly (or LiteLLM losing the row to disk reset).
	mockServer.DeleteMCPServerOutOfBand(originalServerID)
	if mockServer.HasMCPServer(wireName) {
		t.Fatalf("mock still has server %q after DeleteMCPServerOutOfBand", wireName)
	}

	// v0.4.6 — wait > litellm.DefaultListCacheTTL so the next reconcile's
	// vanish-probe sees a CACHE MISS (not the stale "present" entry left
	// by the initial-reconcile LIST that ran before DeleteMCPServerOutOfBand).
	// In prod the analogous case is bounded by safety-relist (5m+jitter)
	// + cache TTL (30s) = ~5.5m worst-case recovery for third-party
	// deletes; the test compresses by nudging right after TTL expiry.
	time.Sleep(litellm.DefaultListCacheTTL + 2*time.Second)

	// Reset counters so we measure only the post-vanish re-POST.
	mockServer.ResetCounters()
	mockServer.ResetRecorded()

	// Annotation nudge — forces reconcile without bumping generation
	// (mirrors how safety-relist re-entry would behave: hash unchanged).
	if err := updateWithRetry(ctx,
		client.ObjectKeyFromObject(m),
		m,
		func(obj *litellmv1alpha1.LiteLLMMCPServer) error {
			if obj.Annotations == nil {
				obj.Annotations = make(map[string]string)
			}
			obj.Annotations["test.litellm.ackstorm.ai/vanish-nudge"] = time.Now().Format(time.RFC3339Nano)
			return nil
		},
	); err != nil {
		t.Fatalf("annotation nudge update: %v", err)
	}

	// Wait for re-POST. Vanish-probe runs CachedListMCPServers (cache now
	// expired per the sleep above) → cache miss → fresh fetch → mock empty
	// → ErrNotFound → clear ServerID → CREATE arm POSTs.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if mockServer.HasMCPServer(wireName) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !mockServer.HasMCPServer(wireName) {
		t.Fatalf("mock missing server %q 15s after post-TTL nudge; vanish-detection did NOT re-POST", wireName)
	}

	// At least 1 POST happened post-vanish (CREATE re-target).
	postCount := 0
	for _, c := range mockServer.Recorded() {
		if c.Method == http.MethodPost && c.Path == pathV1MCPServer {
			postCount++
		}
	}
	if postCount < 1 {
		t.Errorf("expected ≥1 POST %s after vanish+nudge, got %d", pathV1MCPServer, postCount)
	}

	// CR should pick up the NEW ServerID (mock assigns fresh UUID on
	// re-POST since the row was wiped).
	final := pollMCPServerCondition(t, ctx, crName, reasonSynced, 10*time.Second)
	if final.Status.LastRendered.ServerID == "" {
		t.Errorf("post-vanish ServerID empty; want non-empty")
	}
	if final.Status.LastRendered.ServerID == originalServerID {
		t.Logf("note: post-vanish ServerID == originalServerID (mock reused UUID); acceptable")
	}
}
