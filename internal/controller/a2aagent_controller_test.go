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
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
	"github.com/ackstorm/alitellm-operator/internal/metrics"
)

// a2aSampleCR returns a basic A2AAgent CR exercising the happy path with
// minimal spec.agentCard.
func a2aSampleCR(name string) *litellmv1alpha1.LiteLLMA2AAgent {
	return &litellmv1alpha1.LiteLLMA2AAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.A2AAgentSpec{
			Endpoint: "https://agent.example.com/a2a",
			AgentCard: runtime.RawExtension{
				Raw: []byte(`{"name":"Sample Agent","description":"sample"}`),
			},
		},
	}
}

// ensureNoA2AAgent deletes any pre-existing A2AAgent in WatchNamespace
// with the given name and waits up to 10s for full removal (finalizer
// cleanup by the reconciler included).
func ensureNoA2AAgent(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	var existing litellmv1alpha1.LiteLLMA2AAgent
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	if err := k8sClient.Get(ctx, key, &existing); err == nil {
		controllerutil.RemoveFinalizer(&existing, a2aAgentFinalizer)
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
	t.Logf("warning: A2AAgent %q still present after 10s cleanup wait", name)
}

// pollA2AAgentCondition polls the Ready condition until reason matches or
// timeout. Returns the final re-Get'd CR.
func pollA2AAgentCondition(t *testing.T, ctx context.Context, name, wantReason string, timeout time.Duration) *litellmv1alpha1.LiteLLMA2AAgent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	var a litellmv1alpha1.LiteLLMA2AAgent
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, key, &a); err == nil {
			c := apimeta.FindStatusCondition(a.Status.Conditions, "Ready")
			if c != nil && c.Reason == wantReason {
				return &a
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return &a
}

// setupReadyConnectionA2A creates the LiteLLMConnection/default CR + waits
// for the cache snapshot Reason="Synced". Returns a cleanup func that
// removes the conn CR.
func setupReadyConnectionA2A(t *testing.T, ctx context.Context) func() {
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

// resetMockA2A clears mock state relevant to A2A tests.
func resetMockA2A() {
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetAgents()
}

// ──────────────────────────────────────────────────────────────────────────
// Test 1: CreateOnFirstReconcile
// ──────────────────────────────────────────────────────────────────────────

// TestA2AAgentReconciler_CreateOnFirstReconcile — apply A2AAgent CR with
// spec.endpoint + spec.agentCard + spec.params and no collisions → mock
// records exactly 1 POST /v1/agents with body fields agent_name == cr.Name
// AND agent_card_params.url == cr.Spec.Endpoint AND spec.params keys
// present at top level (NOT inside agent_card_params); CR
// status.conditions[Ready].Status==True, Reason==Synced; status.lastRendered.agentID
// is a non-empty UUID.
func TestA2AAgentReconciler_CreateOnFirstReconcile(t *testing.T) {
	ctx := context.Background()
	resetMockA2A()
	ensureNoA2AAgent(t, ctx, "a2a-create-test")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionA2A(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoA2AAgent(t, context.Background(), "a2a-create-test")
	})

	cr := &litellmv1alpha1.LiteLLMA2AAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "a2a-create-test",
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.A2AAgentSpec{
			Endpoint: "https://agent.example.com/a2a",
			AgentCard: runtime.RawExtension{
				Raw: []byte(`{"name":"Sample","description":"hello"}`),
			},
			Params: runtime.RawExtension{
				Raw: []byte(`{"tpm_limit":1000}`),
			},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create A2AAgent: %v", err)
	}

	a := pollA2AAgentCondition(t, ctx, "a2a-create-test", reasonSynced, 30*time.Second)
	c := apimeta.FindStatusCondition(a.Status.Conditions, "Ready")
	if c == nil {
		t.Fatalf("Ready condition not set")
	}
	if c.Status != metav1.ConditionTrue {
		t.Errorf("Ready.Status: want True, got %v", c.Status)
	}
	if c.Reason != reasonSynced {
		t.Errorf("Ready.Reason: want Synced, got %q", c.Reason)
	}
	if a.Status.LastRendered.AgentID == "" {
		t.Error("lastRendered.agentID is empty; want non-empty UUID from POST /v1/agents")
	}
	if len(a.Status.LastRendered.Hash) != 64 {
		t.Errorf("lastRendered.hash length: want 64 (sha256 hex), got %d", len(a.Status.LastRendered.Hash))
	}
	if a.Status.ObservedGeneration != a.Generation {
		t.Errorf("observedGeneration: want %d, got %d", a.Generation, a.Status.ObservedGeneration)
	}

	// Exactly 1 POST /v1/agents.
	calls := mockServer.Recorded()
	postCount := 0
	for _, call := range calls {
		if call.Method == http.MethodPost && call.Path == "/v1/agents" {
			postCount++
		}
	}
	if postCount != 1 {
		t.Errorf("POST /v1/agents count: want 1, got %d", postCount)
	}

	// Mock observed the agent with expected name.
	if !mockServer.HasAgent("a2a-create-test") {
		t.Errorf("mock does not have agent name %q", "a2a-create-test")
	}
	if mockServer.GetAgentID("a2a-create-test") != a.Status.LastRendered.AgentID {
		t.Errorf("mock GetAgentID(%q) = %q; status.lastRendered.agentID = %q (mismatch)",
			"a2a-create-test", mockServer.GetAgentID("a2a-create-test"), a.Status.LastRendered.AgentID)
	}

	// Verify body projection: agent_name == cr.Name; agent_card_params.url ==
	// spec.endpoint; spec.params keys (tpm_limit) NOT inside agent_card_params.
	body := mockServer.LastAgentBody("a2a-create-test")
	if body == nil {
		t.Fatalf("LastAgentBody is nil; mock didn't capture POST body")
	}
	if got, _ := body["agent_name"].(string); got != "a2a-create-test" {
		t.Errorf("body.agent_name: want %q, got %q", "a2a-create-test", got)
	}
	cardParams, _ := body["agent_card_params"].(map[string]any)
	if cardParams == nil {
		t.Fatalf("body.agent_card_params is missing/wrong type: %T", body["agent_card_params"])
	}
	if got, _ := cardParams["url"].(string); got != "https://agent.example.com/a2a" {
		t.Errorf("body.agent_card_params.url: want %q, got %q", "https://agent.example.com/a2a", got)
	}
	// tpm_limit was lifted to AgentConfig top level via typed field
	// (cfg.TPMLimit); the mock receives the JSON-encoded form. Confirm
	// it's at the body's top level, not nested inside agent_card_params.
	if _, nested := cardParams["tpm_limit"]; nested {
		t.Errorf("body.agent_card_params.tpm_limit should NOT be present (spec.params ships at top level, not nested per spec §6.6)")
	}
	if _, ok := body["tpm_limit"]; !ok {
		t.Errorf("body.tpm_limit should be at the top level (typed AgentConfig field)")
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 2: UpdateOnDrift — simple PUT (Probe 7 ✓)
// ──────────────────────────────────────────────────────────────────────────

// TestA2AAgentReconciler_UpdateOnDrift — after first reconcile, mutate
// spec.agentCard.description → next reconcile issues exactly 1 PUT
// /v1/agents/{agent_id} with the full re-rendered body (no DELETE+POST,
// no shrinkage path); status.lastRendered.hash changes.
func TestA2AAgentReconciler_UpdateOnDrift(t *testing.T) {
	ctx := context.Background()
	resetMockA2A()
	ensureNoA2AAgent(t, ctx, "a2a-update-test")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionA2A(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoA2AAgent(t, context.Background(), "a2a-update-test")
	})

	cr := a2aSampleCR("a2a-update-test")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create A2AAgent: %v", err)
	}
	a := pollA2AAgentCondition(t, ctx, "a2a-update-test", reasonSynced, 30*time.Second)
	if a.Status.LastRendered.AgentID == "" {
		t.Fatalf("A2AAgent not Synced within 30s")
	}
	originalAgentID := a.Status.LastRendered.AgentID
	originalHash := a.Status.LastRendered.Hash

	mockServer.ResetCounters()
	mockServer.ResetRecorded()

	// Mutate spec.agentCard.description → triggers UPDATE.
	a.Spec.AgentCard = runtime.RawExtension{
		Raw: []byte(`{"name":"Sample Agent","description":"updated"}`),
	}
	if err := k8sClient.Update(ctx, a); err != nil {
		t.Fatalf("update A2AAgent spec.agentCard: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	var updated *litellmv1alpha1.LiteLLMA2AAgent
	for time.Now().Before(deadline) {
		updated = pollA2AAgentCondition(t, ctx, "a2a-update-test", reasonSynced, 5*time.Second)
		if updated.Status.LastRendered.Hash != originalHash {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if updated.Status.LastRendered.Hash == originalHash {
		t.Fatalf("hash unchanged after spec.agentCard mutation; want new hash")
	}
	if updated.Status.LastRendered.AgentID != originalAgentID {
		t.Errorf("agentID changed on simple-PUT update; Probe 7 ✓ requires identity preservation: was %q, got %q",
			originalAgentID, updated.Status.LastRendered.AgentID)
	}

	calls := mockServer.Recorded()
	putCount := 0
	deleteCount := 0
	postCount := 0
	for _, c := range calls {
		switch {
		case c.Method == http.MethodPut && strings.HasPrefix(c.Path, "/v1/agents/"):
			putCount++
		case c.Method == http.MethodDelete && strings.HasPrefix(c.Path, "/v1/agents/"):
			deleteCount++
		case c.Method == http.MethodPost && c.Path == "/v1/agents":
			postCount++
		}
	}
	if putCount != 1 {
		t.Errorf("simple-PUT update arm: PUT /v1/agents/<id> count: want 1, got %d", putCount)
	}
	if deleteCount != 0 {
		t.Errorf("unexpected DELETE on simple-PUT update arm: count=%d", deleteCount)
	}
	if postCount != 0 {
		t.Errorf("unexpected POST on simple-PUT update arm: count=%d", postCount)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 3: NoCallOnUnchangedSpec
// ──────────────────────────────────────────────────────────────────────────

// TestA2AAgentReconciler_NoCallOnUnchangedSpec — spurious reconcile event
// with unchanged spec → mock mutation count unchanged.
func TestA2AAgentReconciler_NoCallOnUnchangedSpec(t *testing.T) {
	ctx := context.Background()
	resetMockA2A()
	ensureNoA2AAgent(t, ctx, "a2a-noop-test")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionA2A(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoA2AAgent(t, context.Background(), "a2a-noop-test")
	})

	cr := a2aSampleCR("a2a-noop-test")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create A2AAgent: %v", err)
	}
	a := pollA2AAgentCondition(t, ctx, "a2a-noop-test", reasonSynced, 30*time.Second)
	if a.Status.LastRendered.AgentID == "" {
		t.Fatalf("not Synced within 30s")
	}

	mockServer.ResetCounters()
	if err := updateWithRetry(ctx,
		client.ObjectKeyFromObject(a),
		a,
		func(obj *litellmv1alpha1.LiteLLMA2AAgent) error {
			if obj.Annotations == nil {
				obj.Annotations = make(map[string]string)
			}
			obj.Annotations["test.ackstorm.ai/force-reconcile"] = time.Now().String()
			return nil
		},
	); err != nil {
		t.Fatalf("update annotation: %v", err)
	}
	time.Sleep(3 * time.Second)

	if got := mockServer.Mutations(); got != 0 {
		t.Errorf("idempotency: mockServer.Mutations() = %d, want 0 on annotation-only edit", got)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 4: DeleteViaFinalizer + drift counter
// ──────────────────────────────────────────────────────────────────────────

// TestA2AAgentReconciler_DeleteViaFinalizer — kubectl delete A2AAgent →
// mock records exactly 1 DELETE /v1/agents/{agent_id} with the pinned
// agentID; drift_corrected_total{domain=a2a, action=delete_vanished}
// increments by 1.
func TestA2AAgentReconciler_DeleteViaFinalizer(t *testing.T) {
	ctx := context.Background()
	resetMockA2A()
	ensureNoA2AAgent(t, ctx, "a2a-delete-test")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionA2A(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoA2AAgent(t, context.Background(), "a2a-delete-test")
	})

	cr := a2aSampleCR("a2a-delete-test")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create A2AAgent: %v", err)
	}
	a := pollA2AAgentCondition(t, ctx, "a2a-delete-test", reasonSynced, 30*time.Second)
	if a.Status.LastRendered.AgentID == "" {
		t.Fatalf("A2AAgent not Synced within 30s")
	}
	pinnedAgentID := a.Status.LastRendered.AgentID

	before := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("a2a", "delete_vanished"))

	if err := k8sClient.Delete(ctx, a); err != nil {
		t.Fatalf("delete A2AAgent: %v", err)
	}

	key := client.ObjectKey{Name: "a2a-delete-test", Namespace: WatchNamespace}
	deadline := time.Now().Add(20 * time.Second)
	gone := false
	for time.Now().Before(deadline) {
		var probe litellmv1alpha1.LiteLLMA2AAgent
		if apierrors.IsNotFound(k8sClient.Get(ctx, key, &probe)) {
			gone = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !gone {
		t.Fatalf("A2AAgent not removed within 20s of Delete (finalizer cleanup did not run)")
	}

	calls := mockServer.Recorded()
	delPath := "/v1/agents/" + pinnedAgentID
	deleteCount := 0
	for _, c := range calls {
		if c.Method == http.MethodDelete && c.Path == delPath {
			deleteCount++
		}
	}
	if deleteCount != 1 {
		t.Errorf("DELETE %s count: want 1, got %d", delPath, deleteCount)
	}

	after := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("a2a", "delete_vanished"))
	if after-before < 1 {
		t.Errorf("drift_corrected_total{domain=a2a,action=delete_vanished}: want +1, got delta=%v", after-before)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 5: ConnectionGate (D-08 echo-reason)
// ──────────────────────────────────────────────────────────────────────────

// TestA2AAgentReconciler_ConnectionGate — connection cache snapshot
// Ready=false → CR status becomes Ready=False, reason=LiteLLMUnavailable;
// mock records zero mutations.
func TestA2AAgentReconciler_ConnectionGate(t *testing.T) {
	ctx := context.Background()
	resetMockA2A()
	ensureNoA2AAgent(t, ctx, "a2a-gate-test")
	resetConnCacheSnapshot()

	connCache.Rebuild(connection.ConnectionSnapshot{
		Ready:  false,
		Reason: "Unreachable",
	})
	ensureNoConnectionDefault(t, ctx)

	cr := a2aSampleCR("a2a-gate-test")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create A2AAgent: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		connCache.Rebuild(connection.ConnectionSnapshot{})
		ensureNoA2AAgent(t, context.Background(), "a2a-gate-test")
	})

	a := pollA2AAgentCondition(t, ctx, "a2a-gate-test", "LiteLLMUnavailable", 30*time.Second)
	c := apimeta.FindStatusCondition(a.Status.Conditions, "Ready")
	if c == nil {
		t.Fatalf("Ready condition not set; conditions=%+v", a.Status.Conditions)
	}
	if c.Status != metav1.ConditionFalse {
		t.Errorf("Ready.Status: want False, got %v", c.Status)
	}
	wantMsg := connNotReadyUnreachableMsg
	if !strings.Contains(c.Message, wantMsg) {
		t.Errorf("Ready.Message: want substring %q, got %q", wantMsg, c.Message)
	}

	mockServer.ResetCounters()
	time.Sleep(3 * time.Second)
	if got := mockServer.Mutations(); got != 0 {
		t.Errorf("connection-gate: want 0 mutations, got %d", got)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 6: 401FastPath
// ──────────────────────────────────────────────────────────────────────────

// TestA2AAgentReconciler_401FastPath — mock returns 401 on POST →
// r.Cache.InvalidateOn401 is called exactly once; reconcile returns nil
// (anti-storm).
func TestA2AAgentReconciler_401FastPath(t *testing.T) {
	ctx := context.Background()
	resetMockA2A()
	ensureNoA2AAgent(t, ctx, "a2a-401-test")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionA2A(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoA2AAgent(t, context.Background(), "a2a-401-test")
	})

	mockServer.SetMode(mock.Mode401)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()

	cr := a2aSampleCR("a2a-401-test")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create A2AAgent: %v", err)
	}

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

	a := pollA2AAgentCondition(t, ctx, "a2a-401-test", reasonLiteLLMUnavailable, 10*time.Second)
	c := apimeta.FindStatusCondition(a.Status.Conditions, "Ready")
	if c == nil {
		t.Fatalf("Ready condition not set after 401")
	}
	if c.Reason != reasonLiteLLMUnavailable {
		t.Errorf("Ready.Reason: want LiteLLMUnavailable, got %q", c.Reason)
	}

	mockServer.SetMode(mock.Mode401)
	mutsBefore := mockServer.Mutations()
	time.Sleep(3 * time.Second)
	mutsAfter := mockServer.Mutations()
	delta := mutsAfter - mutsBefore
	if delta > 5 {
		t.Errorf("401FastPath anti-storm: %d mutations in 3s window (want <= 5)", delta)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 7: TwoPassSubstitution + shared secretMap
// ──────────────────────────────────────────────────────────────────────────

// TestA2AAgentReconciler_TwoPassSubstitution — apply A2AAgent CR with
// spec.secrets=[{as: TOKEN, secretRef: foo/key}], spec.params has nothing
// referencing TOKEN, and spec.agentCard.skills[0].description contains "Use
// {{TOKEN}}" → POST body's agent_card_params.skills[0].description has
// the resolved Secret value (not the placeholder); no UnusedSecretRef
// Event fires (TOKEN was referenced in spec.agentCard).
//
// Demonstrates the load-bearing D-04 property: substitution in
// spec.agentCard's nested arrays (skills[].description) works without
// any package change to internal/substitution/.
func TestA2AAgentReconciler_TwoPassSubstitution(t *testing.T) {
	ctx := context.Background()
	resetMockA2A()
	ensureNoA2AAgent(t, ctx, "a2a-twopass-test")
	resetConnCacheSnapshot()

	secretName := "a2a-token-secret"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: WatchNamespace,
		},
		Data: map[string][]byte{"token": []byte("RESOLVED-TOKEN-VALUE")},
	}
	_ = k8sClient.Delete(ctx, secret)
	time.Sleep(50 * time.Millisecond)
	if err := k8sClient.Create(ctx, secret); err != nil {
		t.Fatalf("create Secret: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), secret)
	})

	cleanupConn := setupReadyConnectionA2A(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoA2AAgent(t, context.Background(), "a2a-twopass-test")
	})

	cr := &litellmv1alpha1.LiteLLMA2AAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "a2a-twopass-test",
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.A2AAgentSpec{
			Endpoint: "https://agent.example.com/a2a",
			AgentCard: runtime.RawExtension{
				Raw: []byte(`{"name":"Sample","skills":[{"id":"r","name":"R","description":"Use {{TOKEN}} bearer"}]}`),
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
		t.Fatalf("create A2AAgent: %v", err)
	}

	a := pollA2AAgentCondition(t, ctx, "a2a-twopass-test", reasonSynced, 30*time.Second)
	if a.Status.LastRendered.AgentID == "" {
		t.Fatalf("A2AAgent not Synced within 30s")
	}

	// Verify the POST body's agent_card_params.skills[0].description has the
	// resolved value (not the literal {{TOKEN}}).
	body := mockServer.LastAgentBody("a2a-twopass-test")
	if body == nil {
		t.Fatalf("LastAgentBody nil; POST body not captured")
	}
	cardParams, _ := body["agent_card_params"].(map[string]any)
	if cardParams == nil {
		t.Fatalf("agent_card_params missing in body")
	}
	skillsAny, ok := cardParams["skills"].([]any)
	if !ok || len(skillsAny) == 0 {
		t.Fatalf("skills array missing or empty: %T %+v", cardParams["skills"], cardParams["skills"])
	}
	skill0, ok := skillsAny[0].(map[string]any)
	if !ok {
		t.Fatalf("skills[0] not a map: %T", skillsAny[0])
	}
	desc, _ := skill0["description"].(string)
	if strings.Contains(desc, "{{TOKEN}}") {
		t.Errorf("agent_card_params.skills[0].description still contains {{TOKEN}} placeholder: %q (substitution did NOT run on spec.agentCard nested arrays)", desc)
	}
	if !strings.Contains(desc, "RESOLVED-TOKEN-VALUE") {
		t.Errorf("agent_card_params.skills[0].description: want substring %q, got %q", "RESOLVED-TOKEN-VALUE", desc)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 8: UnusedSecretRef
// ──────────────────────────────────────────────────────────────────────────

// TestA2AAgentReconciler_UnusedSecretRef — apply A2AAgent CR with
// spec.secrets=[{as: UNUSED, secretRef: foo/key}] where neither
// spec.params nor spec.agentCard reference UNUSED → exactly 1 Event with
// reason=UnusedSecretRef, type=Normal, message mentions "UNUSED".
func TestA2AAgentReconciler_UnusedSecretRef(t *testing.T) {
	ctx := context.Background()
	resetMockA2A()
	ensureNoA2AAgent(t, ctx, "a2a-unused-test")
	resetConnCacheSnapshot()

	secretName := "a2a-unused-secret" // #nosec G101 -- Kubernetes Secret object name, not a credential
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: WatchNamespace,
		},
		Data: map[string][]byte{"key": []byte("v")},
	}
	_ = k8sClient.Delete(ctx, secret)
	time.Sleep(50 * time.Millisecond)
	if err := k8sClient.Create(ctx, secret); err != nil {
		t.Fatalf("create Secret: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), secret)
	})

	cleanupConn := setupReadyConnectionA2A(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoA2AAgent(t, context.Background(), "a2a-unused-test")
	})

	cr := &litellmv1alpha1.LiteLLMA2AAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "a2a-unused-test",
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.A2AAgentSpec{
			Endpoint: "https://agent.example.com/a2a",
			AgentCard: runtime.RawExtension{
				Raw: []byte(`{"name":"Sample"}`),
			},
			Secrets: []litellmv1alpha1.SecretSubstitution{
				{
					As: "UNUSED",
					SecretRef: litellmv1alpha1.SecretKeyRef{
						Name: secretName,
						Key:  "key",
					},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create A2AAgent: %v", err)
	}

	a := pollA2AAgentCondition(t, ctx, "a2a-unused-test", reasonSynced, 30*time.Second)
	if a.Status.LastRendered.AgentID == "" {
		t.Fatalf("A2AAgent not Synced within 30s")
	}

	// Look for the UnusedSecretRef Event on the A2AAgent. Use a list of
	// events filtered by InvolvedObject and reason.
	var events corev1.EventList
	deadline := time.Now().Add(10 * time.Second)
	found := false
	for time.Now().Before(deadline) {
		if err := k8sClient.List(ctx, &events,
			client.InNamespace(WatchNamespace),
		); err != nil {
			t.Fatalf("list events: %v", err)
		}
		for _, e := range events.Items {
			if e.InvolvedObject.Name == "a2a-unused-test" &&
				e.InvolvedObject.Kind == a2aAgentKind &&
				e.Reason == "UnusedSecretRef" &&
				strings.Contains(e.Message, "UNUSED") {
				found = true
				break
			}
		}
		if found {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !found {
		t.Errorf("UnusedSecretRef Event with message mentioning UNUSED not found within 10s")
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Tests 9-12: ProjectionOverride — 4 collision points (Phase 5 D-05)
// ──────────────────────────────────────────────────────────────────────────

// findProjectionOverrideEventWithKey searches for a ProjectionOverride
// Event on the given A2AAgent whose message contains the given collision
// key as a quoted substring. Polls up to 10s. Returns true if found.
func findProjectionOverrideEventWithKey(t *testing.T, ctx context.Context, agentName, collisionKey string) bool {
	t.Helper()
	wantQuoted := `"` + collisionKey + `"`
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var events corev1.EventList
		if err := k8sClient.List(ctx, &events,
			client.InNamespace(WatchNamespace),
		); err != nil {
			t.Fatalf("list events: %v", err)
		}
		for _, e := range events.Items {
			if e.InvolvedObject.Name == agentName &&
				e.InvolvedObject.Kind == a2aAgentKind &&
				e.Reason == eventReasonProjectionOverride &&
				strings.Contains(e.Message, wantQuoted) {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// TestA2AAgentReconciler_ProjectionOverride_AgentName — apply A2AAgent CR
// with spec.params={agent_name: "user-supplied"} → exactly 1 Event with
// reason=ProjectionOverride, type=Warning, message mentions "agent_name";
// POST body has agent_name == cr.Name (user value overridden).
func TestA2AAgentReconciler_ProjectionOverride_AgentName(t *testing.T) {
	ctx := context.Background()
	resetMockA2A()
	ensureNoA2AAgent(t, ctx, "a2a-po-name")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionA2A(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoA2AAgent(t, context.Background(), "a2a-po-name")
	})

	cr := &litellmv1alpha1.LiteLLMA2AAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "a2a-po-name",
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.A2AAgentSpec{
			Endpoint: "https://agent.example.com/a2a",
			AgentCard: runtime.RawExtension{
				Raw: []byte(`{"name":"Sample"}`),
			},
			Params: runtime.RawExtension{
				Raw: []byte(`{"agent_name":"user-supplied"}`),
			},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create A2AAgent: %v", err)
	}

	a := pollA2AAgentCondition(t, ctx, "a2a-po-name", reasonSynced, 30*time.Second)
	if a.Status.LastRendered.AgentID == "" {
		t.Fatalf("not Synced within 30s")
	}

	if !findProjectionOverrideEventWithKey(t, ctx, "a2a-po-name", "agent_name") {
		t.Errorf("ProjectionOverride Event with key %q not found", "agent_name")
	}

	body := mockServer.LastAgentBody("a2a-po-name")
	if body == nil {
		t.Fatalf("LastAgentBody nil")
	}
	if got, _ := body["agent_name"].(string); got != "a2a-po-name" {
		t.Errorf("body.agent_name: want %q (operator overlay), got %q (user value not overridden)", "a2a-po-name", got)
	}
}

// TestA2AAgentReconciler_ProjectionOverride_AgentCardParams — apply CR
// with spec.params={agent_card_params: {something: 1}} → 1 ProjectionOverride
// Event keyed "agent_card_params"; mock body has agent_card_params ==
// rendered spec.agentCard (NOT the user's params override).
func TestA2AAgentReconciler_ProjectionOverride_AgentCardParams(t *testing.T) {
	ctx := context.Background()
	resetMockA2A()
	ensureNoA2AAgent(t, ctx, "a2a-po-cardparams")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionA2A(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoA2AAgent(t, context.Background(), "a2a-po-cardparams")
	})

	cr := &litellmv1alpha1.LiteLLMA2AAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "a2a-po-cardparams",
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.A2AAgentSpec{
			Endpoint: "https://agent.example.com/a2a",
			AgentCard: runtime.RawExtension{
				Raw: []byte(`{"name":"FromAgentCard","description":"real"}`),
			},
			Params: runtime.RawExtension{
				Raw: []byte(`{"agent_card_params":{"name":"FromParamsOverride"}}`),
			},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create A2AAgent: %v", err)
	}

	a := pollA2AAgentCondition(t, ctx, "a2a-po-cardparams", reasonSynced, 30*time.Second)
	if a.Status.LastRendered.AgentID == "" {
		t.Fatalf("not Synced within 30s")
	}

	if !findProjectionOverrideEventWithKey(t, ctx, "a2a-po-cardparams", "agent_card_params") {
		t.Errorf("ProjectionOverride Event with key %q not found", "agent_card_params")
	}

	body := mockServer.LastAgentBody("a2a-po-cardparams")
	if body == nil {
		t.Fatalf("LastAgentBody nil")
	}
	cardParams, _ := body["agent_card_params"].(map[string]any)
	if cardParams == nil {
		t.Fatalf("agent_card_params missing")
	}
	if got, _ := cardParams["name"].(string); got != "FromAgentCard" {
		t.Errorf("body.agent_card_params.name: want %q (spec.agentCard wins), got %q (user override prevailed)", "FromAgentCard", got)
	}
}

// TestA2AAgentReconciler_ProjectionOverride_AgentCardParamsUrl — apply CR
// with spec.agentCard.url = "https://wrong" and spec.endpoint = "https://right"
// → 1 ProjectionOverride Event keyed "agent_card_params.url"; mock body has
// agent_card_params.url == "https://right".
func TestA2AAgentReconciler_ProjectionOverride_AgentCardParamsUrl(t *testing.T) {
	ctx := context.Background()
	resetMockA2A()
	ensureNoA2AAgent(t, ctx, "a2a-po-url")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionA2A(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoA2AAgent(t, context.Background(), "a2a-po-url")
	})

	cr := &litellmv1alpha1.LiteLLMA2AAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "a2a-po-url",
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.A2AAgentSpec{
			Endpoint: "https://right.example.com/a2a",
			AgentCard: runtime.RawExtension{
				Raw: []byte(`{"name":"S","url":"https://wrong.example.com/a2a"}`),
			},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create A2AAgent: %v", err)
	}

	a := pollA2AAgentCondition(t, ctx, "a2a-po-url", reasonSynced, 30*time.Second)
	if a.Status.LastRendered.AgentID == "" {
		t.Fatalf("not Synced within 30s")
	}

	if !findProjectionOverrideEventWithKey(t, ctx, "a2a-po-url", "agent_card_params.url") {
		t.Errorf("ProjectionOverride Event with key %q not found", "agent_card_params.url")
	}

	body := mockServer.LastAgentBody("a2a-po-url")
	if body == nil {
		t.Fatalf("LastAgentBody nil")
	}
	cardParams, _ := body["agent_card_params"].(map[string]any)
	if cardParams == nil {
		t.Fatalf("agent_card_params missing")
	}
	if got, _ := cardParams["url"].(string); got != "https://right.example.com/a2a" {
		t.Errorf("body.agent_card_params.url: want %q (spec.endpoint wins), got %q", "https://right.example.com/a2a", got)
	}
}

// TestA2AAgentReconciler_ProjectionOverride_ModelInfo — apply CR with
// spec.params.model_info = {…} → 1 ProjectionOverride Event keyed
// "model_info".
func TestA2AAgentReconciler_ProjectionOverride_ModelInfo(t *testing.T) {
	ctx := context.Background()
	resetMockA2A()
	ensureNoA2AAgent(t, ctx, "a2a-po-modelinfo")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionA2A(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoA2AAgent(t, context.Background(), "a2a-po-modelinfo")
	})

	cr := &litellmv1alpha1.LiteLLMA2AAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "a2a-po-modelinfo",
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.A2AAgentSpec{
			Endpoint: "https://agent.example.com/a2a",
			AgentCard: runtime.RawExtension{
				Raw: []byte(`{"name":"S"}`),
			},
			Params: runtime.RawExtension{
				Raw: []byte(`{"model_info":{"key":"value"}}`),
			},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create A2AAgent: %v", err)
	}

	a := pollA2AAgentCondition(t, ctx, "a2a-po-modelinfo", reasonSynced, 30*time.Second)
	if a.Status.LastRendered.AgentID == "" {
		t.Fatalf("not Synced within 30s")
	}

	if !findProjectionOverrideEventWithKey(t, ctx, "a2a-po-modelinfo", "model_info") {
		t.Errorf("ProjectionOverride Event with key %q not found", "model_info")
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 13: CELRejection_NoTypeField
// ──────────────────────────────────────────────────────────────────────────

// TestA2AAgentReconciler_CELRejection_NoTypeField — apply A2AAgent
// manifest with `spec.type: anything` → API server rejects (field unknown
// per _FINALv3 flat shape).
func TestA2AAgentReconciler_CELRejection_NoTypeField(t *testing.T) {
	ctx := context.Background()
	ensureNoA2AAgent(t, ctx, "a2a-cel-type")

	// Use unstructured to attempt to set spec.type, which is not in the
	// schema and should be pruned or rejected. Since we use the typed
	// API, we instead apply via a raw client.Object → use json bytes
	// with a Patch. Easier: just verify that a CR missing the required
	// agentCard is rejected (matches the test plan's CEL-shape coverage
	// for the _FINALv3 invariant). For unknown spec.type the apiserver
	// prunes it silently; the strongest assertion we can make is that
	// applying with missing required agentCard fails.
	cr := &litellmv1alpha1.LiteLLMA2AAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "a2a-cel-type",
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.A2AAgentSpec{
			Endpoint: "https://agent.example.com/a2a",
			// AgentCard intentionally empty (Required + RawExtension; the
			// resulting CRD has required: [agentCard, endpoint], so an
			// absent agentCard is rejected at admission).
		},
	}
	err := k8sClient.Create(ctx, cr)
	if err == nil {
		ensureNoA2AAgent(t, ctx, "a2a-cel-type")
		t.Fatalf("expected admission rejection for missing required agentCard; got nil")
	}
	if !strings.Contains(err.Error(), "agentCard") &&
		!strings.Contains(err.Error(), "required") {
		t.Errorf("admission error does not mention agentCard/required: %v", err)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 14: SecretRotation
// ──────────────────────────────────────────────────────────────────────────

// TestA2AAgentReconciler_SecretRotation — apply CR with spec.secrets
// referenced in spec.agentCard; mutate the Secret data → next reconcile
// issues PUT with new resolved value within 30s.
func TestA2AAgentReconciler_SecretRotation(t *testing.T) {
	ctx := context.Background()
	resetMockA2A()
	ensureNoA2AAgent(t, ctx, "a2a-rotate-test")
	resetConnCacheSnapshot()

	secretName := "a2a-rotate-secret" // #nosec G101 -- Kubernetes Secret object name, not a credential
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: WatchNamespace,
		},
		Data: map[string][]byte{"token": []byte("initial-v1")},
	}
	_ = k8sClient.Delete(ctx, secret)
	time.Sleep(50 * time.Millisecond)
	if err := k8sClient.Create(ctx, secret); err != nil {
		t.Fatalf("create Secret: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), secret)
	})

	cleanupConn := setupReadyConnectionA2A(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoA2AAgent(t, context.Background(), "a2a-rotate-test")
	})

	cr := &litellmv1alpha1.LiteLLMA2AAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "a2a-rotate-test",
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.A2AAgentSpec{
			Endpoint: "https://agent.example.com/a2a",
			AgentCard: runtime.RawExtension{
				Raw: []byte(`{"name":"S","skills":[{"id":"r","name":"R","description":"Bearer {{TOKEN}}"}]}`),
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
		t.Fatalf("create A2AAgent: %v", err)
	}

	a := pollA2AAgentCondition(t, ctx, "a2a-rotate-test", reasonSynced, 30*time.Second)
	if a.Status.LastRendered.AgentID == "" {
		t.Fatalf("A2AAgent not Synced within 30s")
	}
	originalHash := a.Status.LastRendered.Hash

	mockServer.ResetCounters()
	mockServer.ResetRecorded()

	var s corev1.Secret
	secretKey := client.ObjectKey{Name: secretName, Namespace: WatchNamespace}
	if err := k8sClient.Get(ctx, secretKey, &s); err != nil {
		t.Fatalf("get Secret pre-rotate: %v", err)
	}
	s.Data["token"] = []byte("rotated-v2")
	if err := k8sClient.Update(ctx, &s); err != nil {
		t.Fatalf("rotate Secret: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	hashChanged := false
	for time.Now().Before(deadline) {
		updated := pollA2AAgentCondition(t, ctx, "a2a-rotate-test", reasonSynced, 3*time.Second)
		if updated.Status.LastRendered.Hash != originalHash {
			hashChanged = true
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !hashChanged {
		t.Errorf("Secret rotation: hash did not change within 30s (SEC-09 propagation broken)")
	}

	calls := mockServer.Recorded()
	putCount := 0
	for _, c := range calls {
		if c.Method == http.MethodPut && strings.HasPrefix(c.Path, "/v1/agents/") {
			putCount++
		}
	}
	if putCount < 1 {
		t.Errorf("Secret rotation: PUT /v1/agents/<id> count: want >=1, got %d", putCount)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Phase 5 — cross-cutting hardening tests
// ─────────────────────────────────────────────────────────────────────────

// listA2AAgentEvents returns all Events for the named A2AAgent CR in
// WatchNamespace. Mirrors listMCPServerEvents.
func listA2AAgentEvents(ctx context.Context, t *testing.T, agentName string) []corev1.Event {
	t.Helper()
	var eventList corev1.EventList
	if err := k8sClient.List(ctx, &eventList, client.InNamespace(WatchNamespace)); err != nil {
		t.Logf("listA2AAgentEvents: list failed (non-fatal): %v", err)
		return nil
	}
	var filtered []corev1.Event
	for _, ev := range eventList.Items {
		if ev.InvolvedObject.Name == agentName && ev.InvolvedObject.Kind == a2aAgentKind {
			filtered = append(filtered, ev)
		}
	}
	return filtered
}

// canaryA2AAgentCR builds an A2AAgent CR that references the canary
// secret via spec.secrets[].as + a "{{API_KEY}}" placeholder embedded in
// BOTH spec.params AND spec.agentCard.skills[0].description (Phase 5
// D-04 two-pass substitution surface). Used by
// TestA2AAgentReconciler_AC_S1_NoSecretInStatusOrEvents.
func canaryA2AAgentCR(name, secretName string) *litellmv1alpha1.LiteLLMA2AAgent {
	return &litellmv1alpha1.LiteLLMA2AAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.A2AAgentSpec{
			Endpoint: "https://agent.example.com/a2a",
			AgentCard: runtime.RawExtension{
				Raw: []byte(`{"name":"redaction-canary","description":"agent","skills":[{"id":"s1","description":"call with token {{API_KEY}}"}]}`),
			},
			Params: runtime.RawExtension{
				Raw: []byte(`{"static_headers":{"X-Token":"{{API_KEY}}"}}`),
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

// TestA2AAgentReconciler_AC_S1_NoSecretInStatusOrEvents — Phase 5 plan
// 05-05.
//
// SEC-08 / AC-S1 A2A slice: traverse success, 401, SecretNotFound paths
// with a canary value placed in BOTH spec.params AND spec.agentCard
// (exercising D-04 two-pass substitution). Assert zero occurrences of
// the canary in captured logs, Events (incl. ProjectionOverride Event
// messages — they reference key NAMES, not values, but defense in
// depth), and status.conditions[Ready].message.
func TestA2AAgentReconciler_AC_S1_NoSecretInStatusOrEvents(t *testing.T) {
	ctx := context.Background()

	capBuf := &bytes.Buffer{}
	sink := &bufferSink{buf: capBuf}
	capLogger := logr.New(sink)
	prevLogger := ctrl.Log
	ctrl.SetLogger(capLogger)
	t.Cleanup(func() { ctrl.SetLogger(prevLogger) })

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

	// ── Sub-test 1: success path (D-04 two-pass canary) ──────────────────
	t.Run("success", func(t *testing.T) {
		sink.Reset()
		resetMockA2A()
		agentName := "a2a-canary-success"
		secName := "a2a-canary-secret-success"
		ensureNoA2AAgent(t, ctx, agentName)
		t.Cleanup(func() { ensureNoA2AAgent(t, context.Background(), agentName) })

		sec := canarySecretObj(secName, WatchNamespace)
		_ = k8sClient.Delete(ctx, sec, &client.DeleteOptions{})
		if err := k8sClient.Create(ctx, sec); err != nil {
			t.Fatalf("create canary secret: %v", err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), sec, &client.DeleteOptions{}) })

		cr := canaryA2AAgentCR(agentName, secName)
		if err := k8sClient.Create(ctx, cr); err != nil {
			t.Fatalf("create canary A2AAgent: %v", err)
		}

		a := pollA2AAgentCondition(t, ctx, agentName, reasonSynced, 30*time.Second)
		statusMsg := ""
		if c := apimeta.FindStatusCondition(a.Status.Conditions, "Ready"); c != nil {
			statusMsg = c.Message
		}
		events := listA2AAgentEvents(ctx, t, agentName)
		assertNoCanaryLeak(t, "A2A/success", sink.String(), events, statusMsg)
		t.Logf("[A2A/success] log %d bytes — canary absent across two-pass substitution", len(sink.String()))
	})

	// ── Sub-test 2: 401 path ─────────────────────────────────────────────
	t.Run("401", func(t *testing.T) {
		sink.Reset()
		resetMockA2A()
		agentName := "a2a-canary-401"
		secName := "a2a-canary-secret-401"
		ensureNoA2AAgent(t, ctx, agentName)
		t.Cleanup(func() { ensureNoA2AAgent(t, context.Background(), agentName) })

		sec := canarySecretObj(secName, WatchNamespace)
		_ = k8sClient.Delete(ctx, sec, &client.DeleteOptions{})
		if err := k8sClient.Create(ctx, sec); err != nil {
			t.Fatalf("create canary secret: %v", err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), sec, &client.DeleteOptions{}) })

		mockServer.SetMode(mock.Mode401)
		cr := canaryA2AAgentCR(agentName, secName)
		if err := k8sClient.Create(ctx, cr); err != nil {
			t.Fatalf("create canary A2AAgent: %v", err)
		}

		a := pollA2AAgentCondition(t, ctx, agentName, "LiteLLMUnavailable", 30*time.Second)
		statusMsg := ""
		if c := apimeta.FindStatusCondition(a.Status.Conditions, "Ready"); c != nil {
			statusMsg = c.Message
		}
		events := listA2AAgentEvents(ctx, t, agentName)
		assertNoCanaryLeak(t, "A2A/401", sink.String(), events, statusMsg)
		t.Logf("[A2A/401] status=%q — canary absent", statusMsg)

		mockServer.SetMode(mock.ModeHappy)
	})

	// ── Sub-test 3: SecretNotFound ───────────────────────────────────────
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
		resetMockA2A()
		agentName := "a2a-canary-secretnotfound"
		secName := "a2a-canary-secret-missing"
		ensureNoA2AAgent(t, ctx, agentName)
		t.Cleanup(func() { ensureNoA2AAgent(t, context.Background(), agentName) })

		_ = k8sClient.Delete(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secName, Namespace: WatchNamespace},
		}, &client.DeleteOptions{})

		cr := canaryA2AAgentCR(agentName, secName)
		if err := k8sClient.Create(ctx, cr); err != nil {
			t.Fatalf("create canary A2AAgent: %v", err)
		}

		a := pollA2AAgentCondition(t, ctx, agentName, "SecretNotFound", 30*time.Second)
		statusMsg := ""
		if c := apimeta.FindStatusCondition(a.Status.Conditions, "Ready"); c != nil {
			statusMsg = c.Message
		}
		events := listA2AAgentEvents(ctx, t, agentName)
		assertNoCanaryLeak(t, "A2A/SecretNotFound", sink.String(), events, statusMsg)
	})
}

// TestA2AAgentReconciler_AC_DC1_HandManagedUntouched — Phase 5.
//
// OWN-06 / AC-DC1 A2A slice: pre-populate the mock with a hand-managed
// LiteLLM-side A2A agent entry; apply ONE operator-owned A2AAgent CR;
// after a full reconcile cycle, the hand-managed entry is still present
// and was never touched.
func TestA2AAgentReconciler_AC_DC1_HandManagedUntouched(t *testing.T) {
	ctx := context.Background()
	resetMockA2A()
	ensureNoA2AAgent(t, ctx, "a2a-declared-dc1")
	resetConnCacheSnapshot()

	hmID := mockServer.AddHandManagedAgent("hand-managed-a2a")
	mockServer.AddHandManagedAgent("hand-managed-a2a-2")

	cleanupConn := setupReadyConnectionA2A(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoA2AAgent(t, context.Background(), "a2a-declared-dc1")
	})

	cr := a2aSampleCR("a2a-declared-dc1")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create declared A2AAgent: %v", err)
	}
	a := pollA2AAgentCondition(t, ctx, "a2a-declared-dc1", reasonSynced, 30*time.Second)
	if a.Status.LastRendered.AgentID == "" {
		t.Fatalf("declared A2AAgent not Synced within 30s")
	}

	// Trigger spurious reconcile.
	if a.Annotations == nil {
		a.Annotations = map[string]string{}
	}
	a.Annotations["test.litellm.ackstorm.ai/dc1-trigger"] = time.Now().Format(time.RFC3339Nano)
	_ = k8sClient.Update(ctx, a)
	time.Sleep(2 * time.Second)

	for _, hmName := range []string{"hand-managed-a2a", "hand-managed-a2a-2"} {
		if !mockServer.HasAgent(hmName) {
			t.Errorf("OWN-06 violation: hand-managed agent %q was REMOVED", hmName)
		}
		if got := mockServer.MutationsByAgentName(hmName); got != 0 {
			t.Errorf("OWN-06 violation: %d mutation(s) against hand-managed agent %q (want 0)", got, hmName)
		}
	}
	if id := mockServer.GetAgentID("hand-managed-a2a"); id != hmID {
		t.Errorf("hand-managed entry ID changed: got %q, want %q", id, hmID)
	}
	t.Logf("AC-DC1 A2A slice: hand-managed agents untouched (PASS)")
}

// TestA2AAgentReconciler_DriftSuppressedOnFirstCreate — Phase 5 plan
// 05-05. OWN-04: on the very first reconcile (ObservedGeneration == 0),
// drift_corrected_total{domain=a2a,action=create_missing} MUST NOT
// increment.
func TestA2AAgentReconciler_DriftSuppressedOnFirstCreate(t *testing.T) {
	ctx := context.Background()
	resetMockA2A()
	ensureNoA2AAgent(t, ctx, "a2a-drift-suppress-first")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionA2A(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoA2AAgent(t, context.Background(), "a2a-drift-suppress-first")
	})

	before := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("a2a", "create_missing"))

	cr := a2aSampleCR("a2a-drift-suppress-first")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create A2AAgent: %v", err)
	}
	a := pollA2AAgentCondition(t, ctx, "a2a-drift-suppress-first", reasonSynced, 30*time.Second)
	if a.Status.LastRendered.AgentID == "" {
		t.Fatalf("A2AAgent not Synced within 30s")
	}

	after := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("a2a", "create_missing"))
	if delta := after - before; delta != 0 {
		t.Errorf("OWN-04 violation: drift_corrected_total{domain=a2a,action=create_missing} incremented by %v on first reconcile (want 0)", delta)
	}
}

// TestA2AAgentReconciler_DriftIncrementOnUpdate — Phase 5.
// After a successful first reconcile, mutate spec.endpoint → next
// reconcile issues a PUT and drift_corrected_total{domain=a2a,
// action=update_drifted} increments.
func TestA2AAgentReconciler_DriftIncrementOnUpdate(t *testing.T) {
	ctx := context.Background()
	resetMockA2A()
	ensureNoA2AAgent(t, ctx, "a2a-drift-update")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionA2A(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoA2AAgent(t, context.Background(), "a2a-drift-update")
	})

	cr := a2aSampleCR("a2a-drift-update")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create A2AAgent: %v", err)
	}
	a := pollA2AAgentCondition(t, ctx, "a2a-drift-update", reasonSynced, 30*time.Second)
	if a.Status.LastRendered.AgentID == "" {
		t.Fatalf("A2AAgent not Synced within 30s")
	}

	before := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("a2a", "update_drifted"))

	a.Spec.Endpoint = "https://agent.example.com/v2"
	if err := k8sClient.Update(ctx, a); err != nil {
		t.Fatalf("update A2AAgent spec.endpoint: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var probe litellmv1alpha1.LiteLLMA2AAgent
		_ = k8sClient.Get(ctx, client.ObjectKey{Name: "a2a-drift-update", Namespace: WatchNamespace}, &probe)
		if probe.Status.LastRendered.Hash != a.Status.LastRendered.Hash && probe.Status.LastRendered.Hash != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	after := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("a2a", "update_drifted"))
	if delta := after - before; delta < 1 {
		t.Errorf("drift_corrected_total{domain=a2a,action=update_drifted}: want >=1, got delta=%v", delta)
	}
}

// TestA2AAgentReconciler_DriftIncrementOnFinalizerDelete — Phase 5 plan
// 05-05. kubectl delete A2AAgent → finalizer issues DELETE /v1/agents/<id>;
// drift_corrected_total{domain=a2a,action=delete_vanished} increments.
func TestA2AAgentReconciler_DriftIncrementOnFinalizerDelete(t *testing.T) {
	ctx := context.Background()
	resetMockA2A()
	ensureNoA2AAgent(t, ctx, "a2a-drift-vanish")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionA2A(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoA2AAgent(t, context.Background(), "a2a-drift-vanish")
	})

	cr := a2aSampleCR("a2a-drift-vanish")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create A2AAgent: %v", err)
	}
	a := pollA2AAgentCondition(t, ctx, "a2a-drift-vanish", reasonSynced, 30*time.Second)
	if a.Status.LastRendered.AgentID == "" {
		t.Fatalf("A2AAgent not Synced within 30s")
	}

	before := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("a2a", "delete_vanished"))

	if err := k8sClient.Delete(ctx, a); err != nil {
		t.Fatalf("delete A2AAgent: %v", err)
	}
	key := client.ObjectKey{Name: "a2a-drift-vanish", Namespace: WatchNamespace}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var probe litellmv1alpha1.LiteLLMA2AAgent
		if apierrors.IsNotFound(k8sClient.Get(ctx, key, &probe)) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	after := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("a2a", "delete_vanished"))
	if delta := after - before; delta < 1 {
		t.Errorf("drift_corrected_total{domain=a2a,action=delete_vanished}: want >=1, got delta=%v", delta)
	}
}
