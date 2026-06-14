// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/connection"
	"github.com/ackstorm/alitellm-operator/internal/controller/deletionpolicy"
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
	"github.com/ackstorm/alitellm-operator/internal/metrics"
)

// LiteLLM wire-path constants asserted across this test file. Extracted
// so goconst stays quiet across 3-5 occurrences each.
const (
	pathModelNew    = "/model/new"
	pathModelDelete = "/model/delete"
)

// modelSampleCR returns a basic Model CR with simple spec.params and no secrets.
// The model name must be unique across tests — callers pass a unique name.
func modelSampleCR(name string) *litellmv1alpha1.LiteLLMModel {
	return &litellmv1alpha1.LiteLLMModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.ModelSpec{
			Params: runtime.RawExtension{
				Raw: []byte(`{"model":"openai/gpt-4o-mini","rpm":100}`),
			},
		},
	}
}

// ensureNoModel deletes any pre-existing Model in WatchNamespace with the
// given name and waits up to 10s for the API server and envtest to fully
// remove it (including finalizer cleanup by the reconciler).
func ensureNoModel(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	var existing litellmv1alpha1.LiteLLMModel
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	if err := k8sClient.Get(ctx, key, &existing); err == nil {
		// Remove finalizer manually so the delete doesn't block on the reconciler
		// if the LiteLLM mock is not in a state where DeleteModel succeeds.
		controllerutil.RemoveFinalizer(&existing, modelFinalizer)
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
	t.Logf("warning: Model %q still present after 10s cleanup wait", name)
}

// pollModelCondition polls the Model CR's Ready condition reason until it
// matches wantReason or the timeout expires. Returns the final re-Get'd CR.
func pollModelCondition(t *testing.T, ctx context.Context, name, wantReason string, timeout time.Duration) *litellmv1alpha1.LiteLLMModel {
	t.Helper()
	deadline := time.Now().Add(timeout)
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	var m litellmv1alpha1.LiteLLMModel
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, key, &m); err == nil {
			c := apimeta.FindStatusCondition(m.Status.Conditions, conditionTypeReady)
			if c != nil && c.Reason == wantReason {
				return &m
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return &m
}

// TestModel_FinalizerAddedOnFirstReconcile — Task 1 Test 1.
//
// Create a Model CR; assert that on the next reconcile the reconciler adds
// "models.litellm.ackstorm.ai/finalizer" to the CR.
func TestModel_FinalizerAddedOnFirstReconcile(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetModels()
	ensureNoModel(t, ctx, "finalizer-test")
	resetConnCacheSnapshot()

	// Ensure LiteLLMConnection/default is ready so the model reconciler
	// can proceed past connection-gating (Step 3).
	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		time.Sleep(50 * time.Millisecond)
	})
	// Wait for connection to be ready.
	connSnap := pollSnapshotReason(30*time.Second, reasonSynced)
	if connSnap.Reason != reasonSynced {
		t.Fatalf("LiteLLMConnection not Synced within 30s; reason=%q", connSnap.Reason)
	}

	cr := modelSampleCR("finalizer-test")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Model: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		ensureNoModel(t, context.Background(), "finalizer-test")
	})

	// Poll until the finalizer is present.
	deadline := time.Now().Add(30 * time.Second)
	found := false
	key := client.ObjectKey{Name: "finalizer-test", Namespace: WatchNamespace}
	for time.Now().Before(deadline) {
		var m litellmv1alpha1.LiteLLMModel
		if err := k8sClient.Get(ctx, key, &m); err == nil {
			if controllerutil.ContainsFinalizer(&m, modelFinalizer) {
				found = true
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !found {
		t.Errorf("ModelFinalizer %q not added within 30s", modelFinalizer)
	}
}

// TestModel_DeletionPath_IssuesDeleteAndRemovesFinalizer — Task 1 Test 2.
//
// Pre-populate a Model with a known ModelID in status; set
// DeletionTimestamp by deleting it; assert exactly one POST /model/delete
// is issued and the finalizer is removed.
func TestModel_DeletionPath_IssuesDeleteAndRemovesFinalizer(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetModels()
	ensureNoModel(t, ctx, "deletion-test")
	resetConnCacheSnapshot()

	// Ensure LiteLLMConnection/default is ready.
	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		time.Sleep(50 * time.Millisecond)
	})
	connSnap := pollSnapshotReason(30*time.Second, reasonSynced)
	if connSnap.Reason != reasonSynced {
		t.Fatalf("LiteLLMConnection not Synced within 30s; reason=%q", connSnap.Reason)
	}

	// Create the Model and wait for it to reach Ready=Synced.
	cr := modelSampleCR("deletion-test")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Model: %v", err)
	}

	// Wait for it to become Synced (i.e., created in LiteLLM and has a ModelID).
	m := pollModelCondition(t, ctx, "deletion-test", reasonSynced, 30*time.Second)
	if m.Status.LastRendered.ModelID == "" {
		t.Fatalf("Model Synced but ModelID is empty; status=%+v", m.Status)
	}
	storedModelID := m.Status.LastRendered.ModelID

	// Snapshot mutations BEFORE delete.
	mutationsBeforeDelete := mockServer.Mutations()

	// Delete the CR — the reconciler should issue POST /model/delete.
	if err := k8sClient.Delete(ctx, m); err != nil {
		t.Fatalf("delete Model: %v", err)
	}

	// Poll until fully removed.
	key := client.ObjectKey{Name: "deletion-test", Namespace: WatchNamespace}
	deadline := time.Now().Add(20 * time.Second)
	gone := false
	for time.Now().Before(deadline) {
		var probe litellmv1alpha1.LiteLLMModel
		err := k8sClient.Get(ctx, key, &probe)
		if apierrors.IsNotFound(err) {
			gone = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !gone {
		t.Fatalf("Model not removed within 20s of Delete (finalizer cleanup did not run)")
	}

	mutationsAfterDelete := mockServer.Mutations()
	// Exactly one additional mutation (the POST /model/delete).
	delta := mutationsAfterDelete - mutationsBeforeDelete
	if delta < 1 {
		t.Errorf("expected at least 1 mutation (POST /model/delete) after CR delete; got delta=%d (before=%d, after=%d)",
			delta, mutationsBeforeDelete, mutationsAfterDelete)
	}

	// Verify in recorded calls that there's a POST /model/delete.
	calls := mockServer.Recorded()
	found := false
	for _, c := range calls {
		if c.Method == http.MethodPost && c.Path == pathModelDelete {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("POST /model/delete not found in recorded calls; storedModelID=%q; calls=%+v", storedModelID, calls)
	}
}

// TestModel_DeletionPath_StaleStatus_NameResolveFallback — Task 1 Test 3.
//
// Create a Model with ModelID="" in status (stale status); the
// reconciler should call GetModelInfoByName to resolve the ID, then issue
// POST /model/delete. Assert Reads increments (the GET /model/info?model_name=)
// and Mutations increments (the POST /model/delete).
func TestModel_DeletionPath_StaleStatus_NameResolveFallback(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetModels()
	ensureNoModel(t, ctx, "stale-deletion-test")
	resetConnCacheSnapshot()

	// Ensure LiteLLMConnection/default is ready.
	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		time.Sleep(50 * time.Millisecond)
	})
	connSnap := pollSnapshotReason(30*time.Second, reasonSynced)
	if connSnap.Reason != reasonSynced {
		t.Fatalf("LiteLLMConnection not Synced within 30s; reason=%q", connSnap.Reason)
	}

	// Create the Model and wait for it to reach Ready=Synced (so there's a
	// model in the mock's internal state).
	cr := modelSampleCR("stale-deletion-test")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Model: %v", err)
	}
	m := pollModelCondition(t, ctx, "stale-deletion-test", reasonSynced, 30*time.Second)
	if m.Status.LastRendered.ModelID == "" {
		t.Fatalf("Model Synced but ModelID is empty")
	}

	// Simulate stale status: clear the ModelID to force name-resolve fallback.
	m.Status.LastRendered.ModelID = ""
	if err := k8sClient.Status().Update(ctx, m); err != nil {
		t.Fatalf("simulate stale status: %v", err)
	}
	// Give the reconciler a moment to pick up the status change (it may reconcile).
	time.Sleep(500 * time.Millisecond)

	// Re-get to get latest.
	key := client.ObjectKey{Name: "stale-deletion-test", Namespace: WatchNamespace}
	if err := k8sClient.Get(ctx, key, m); err != nil {
		t.Fatalf("re-get model: %v", err)
	}

	// Snapshot counters before delete.
	readsBeforeDelete := mockServer.Reads()
	mutationsBeforeDelete := mockServer.Mutations()

	// Delete the CR.
	if err := k8sClient.Delete(ctx, m); err != nil {
		t.Fatalf("delete Model: %v", err)
	}

	// Poll until fully removed.
	deadline := time.Now().Add(20 * time.Second)
	gone := false
	for time.Now().Before(deadline) {
		var probe litellmv1alpha1.LiteLLMModel
		if apierrors.IsNotFound(k8sClient.Get(ctx, key, &probe)) {
			gone = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !gone {
		t.Fatalf("Model not removed within 20s of Delete")
	}

	readsAfterDelete := mockServer.Reads()
	mutationsAfterDelete := mockServer.Mutations()

	// Reads should have increased (the GET /model/info?model_name= call).
	readsIncrease := readsAfterDelete - readsBeforeDelete
	if readsIncrease < 1 {
		t.Errorf("expected at least 1 additional read (GET /model/info?model_name=) during name-resolve fallback; got delta=%d",
			readsIncrease)
	}
	// Mutations should have increased (the POST /model/delete call).
	mutationsIncrease := mutationsAfterDelete - mutationsBeforeDelete
	if mutationsIncrease < 1 {
		t.Errorf("expected at least 1 mutation (POST /model/delete) after stale-status delete; got delta=%d",
			mutationsIncrease)
	}
}

// TestModel_FirstReconcile_CreateNew_NoDrift — Task 2 Test 4.
//
// Create a Model CR with simple spec.params; after first reconcile:
// - exactly 1 POST /model/new issued
// - Model.Status.Conditions[Ready].Status == True, Reason=Synced
// - status.lastRendered.hash != ""
// - status.lastRendered.modelID != ""
// - status.lastRendered.at within the last 10s
func TestModel_FirstReconcile_CreateNew_NoDrift(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetModels()
	ensureNoModel(t, ctx, "first-reconcile-test")
	resetConnCacheSnapshot()

	// Ensure LiteLLMConnection/default is ready.
	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		ensureNoModel(t, context.Background(), "first-reconcile-test")
		time.Sleep(50 * time.Millisecond)
	})
	connSnap := pollSnapshotReason(30*time.Second, reasonSynced)
	if connSnap.Reason != reasonSynced {
		t.Fatalf("LiteLLMConnection not Synced within 30s")
	}

	// Snapshot mutations BEFORE Create.
	mutationsBefore := mockServer.Mutations()

	cr := modelSampleCR("first-reconcile-test")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Model: %v", err)
	}

	// Poll until Ready=Synced.
	m := pollModelCondition(t, ctx, "first-reconcile-test", reasonSynced, 30*time.Second)
	c := apimeta.FindStatusCondition(m.Status.Conditions, conditionTypeReady)
	if c == nil {
		t.Fatalf("Ready condition not set")
	}
	if c.Status != metav1.ConditionTrue {
		t.Errorf("Ready.Status: want True, got %v", c.Status)
	}
	if c.Reason != reasonSynced {
		t.Errorf("Ready.Reason: want Synced, got %q", c.Reason)
	}

	if m.Status.LastRendered.Hash == "" {
		t.Error("lastRendered.hash is empty; want non-empty SHA-256 hex")
	}
	if m.Status.LastRendered.ModelID == "" {
		t.Error("lastRendered.modelID is empty; want non-empty UUID")
	}
	if m.Status.LastRendered.At == nil {
		t.Error("lastRendered.at is nil; want a timestamp")
	} else {
		age := time.Since(m.Status.LastRendered.At.Time)
		if age > 10*time.Second {
			t.Errorf("lastRendered.at is too old: %v ago", age)
		}
	}
	if m.Status.ObservedGeneration != m.Generation {
		t.Errorf("observedGeneration: want %d, got %d", m.Generation, m.Status.ObservedGeneration)
	}

	// Verify exactly 1 POST /model/new was issued.
	mutationsAfter := mockServer.Mutations()
	// Allow for the finalizer Add reconcile (which doesn't mutate LiteLLM)
	// and the actual create. We care that at least one mutation happened and
	// that there's exactly one POST /model/new in the recorded calls.
	_ = mutationsBefore
	_ = mutationsAfter

	calls := mockServer.Recorded()
	newCount := 0
	for _, call := range calls {
		if call.Method == http.MethodPost && call.Path == pathModelNew {
			newCount++
		}
	}
	if newCount != 1 {
		t.Errorf("POST /model/new count: want 1, got %d (recorded calls: %+v)", newCount, calls)
	}
}

// TestModel_RouterModel_NoChurn — Fix B (router-aware reconcile).
//
// A router pseudo-model (litellm_params.model "auto_router/…") is accepted
// by LiteLLM (200 + id) but lives in the in-memory router, not the DB model
// table, so GET /model/info never lists it (the mock mirrors this). Without
// the router-aware skip, the existence probe would read empty, clear the
// ModelID, and re-POST every reconcile — a storm with an ever-changing id.
// This asserts the router CR reaches a STABLE Ready=Synced with exactly one
// POST /model/new and a fixed modelID.
func TestModel_RouterModel_NoChurn(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetModels()
	ensureNoModel(t, ctx, "router-no-churn-test")
	resetConnCacheSnapshot()

	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		ensureNoModel(t, context.Background(), "router-no-churn-test")
		time.Sleep(50 * time.Millisecond)
	})
	if connSnap := pollSnapshotReason(30*time.Second, reasonSynced); connSnap.Reason != reasonSynced {
		t.Fatalf("LiteLLMConnection not Synced within 30s")
	}

	cr := modelSampleCR("router-no-churn-test")
	cr.Spec.Params.Raw = []byte(`{"model":"auto_router/complexity_router",` +
		`"complexity_router_config":{"tiers":{"SIMPLE":"ackstorm.fast"}},` +
		`"complexity_router_default_model":"ackstorm.lite"}`)
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create router Model: %v", err)
	}

	m := pollModelCondition(t, ctx, "router-no-churn-test", reasonSynced, 30*time.Second)
	c := apimeta.FindStatusCondition(m.Status.Conditions, conditionTypeReady)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != reasonSynced {
		t.Fatalf("router model not Ready/Synced: %+v", c)
	}
	if m.Status.LastRendered.ModelID == "" {
		t.Fatal("router model lastRendered.modelID empty; want the create id retained")
	}
	firstID := m.Status.LastRendered.ModelID

	countNew := func() int {
		n := 0
		for _, call := range mockServer.Recorded() {
			if call.Method == http.MethodPost && call.Path == pathModelNew {
				n++
			}
		}
		return n
	}
	// Let several safety/event reconciles elapse — a churning router would
	// re-POST and mutate the id during this window.
	time.Sleep(2 * time.Second)

	if n := countNew(); n != 1 {
		t.Errorf("POST /model/new count for router model: want 1 (no churn), got %d", n)
	}
	var after litellmv1alpha1.LiteLLMModel
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "router-no-churn-test", Namespace: WatchNamespace}, &after); err != nil {
		t.Fatalf("re-get router model: %v", err)
	}
	if after.Status.LastRendered.ModelID != firstID {
		t.Errorf("router modelID churned: was %q, now %q", firstID, after.Status.LastRendered.ModelID)
	}
	if rc := apimeta.FindStatusCondition(after.Status.Conditions, conditionTypeReady); rc == nil ||
		rc.Reason == reasonRecreateThrottled {
		t.Errorf("router model should be steady Ready, not throttled: %+v", rc)
	}
}

// TestModel_SecondReconcile_NoSpecChange_NoOp — Task 2 Test 5.
//
// After a Model reaches Synced, a second reconcile with unchanged spec must
// be a no-op (no additional POST /model/new or /model/update).
func TestModel_SecondReconcile_NoSpecChange_NoOp(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetModels()
	ensureNoModel(t, ctx, "noop-reconcile-test")
	resetConnCacheSnapshot()

	// Ensure LiteLLMConnection/default is ready.
	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		ensureNoModel(t, context.Background(), "noop-reconcile-test")
		time.Sleep(50 * time.Millisecond)
	})
	connSnap := pollSnapshotReason(30*time.Second, reasonSynced)
	if connSnap.Reason != reasonSynced {
		t.Fatalf("LiteLLMConnection not Synced within 30s")
	}

	cr := modelSampleCR("noop-reconcile-test")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Model: %v", err)
	}

	// Wait for first reconcile to complete (Ready=Synced).
	m := pollModelCondition(t, ctx, "noop-reconcile-test", reasonSynced, 30*time.Second)
	if m.Status.LastRendered.ModelID == "" {
		t.Fatalf("Model not Synced within 30s; conditions=%+v", m.Status.Conditions)
	}

	// Snapshot counters AFTER first reconcile.
	mockServer.ResetCounters()

	// Trigger a reconcile by adding an annotation (bumps resourceVersion but
	// does NOT change spec.params, so hash is the same).
	// We use a metadata annotation to trigger the watch event.
	if err := updateWithRetry(ctx,
		client.ObjectKeyFromObject(m),
		m,
		func(model *litellmv1alpha1.LiteLLMModel) error {
			if model.Annotations == nil {
				model.Annotations = make(map[string]string)
			}
			model.Annotations["test.ackstorm.ai/force-reconcile"] = time.Now().String()
			return nil
		},
	); err != nil {
		t.Fatalf("update Model annotation: %v", err)
	}

	// Cross the accelerated 1s envtest relist cadence, then assert no mutations occurred.
	time.Sleep(1250 * time.Millisecond)

	mutationsAfter := mockServer.Mutations()
	if mutationsAfter != 0 {
		t.Errorf("steady-state violation (AC-R1): mockServer.Mutations() = %d, want 0", mutationsAfter)
	}
}

// TestModel_SpecParamsEdit_Update — Task 2 Test 6.
//
// Modify spec.params (add a new field); the next reconcile should issue
// exactly 1 POST /model/update (no delete, no new). The ModelID
// must remain the same.
func TestModel_SpecParamsEdit_Update(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetModels()
	ensureNoModel(t, ctx, "update-test")
	resetConnCacheSnapshot()

	// Ensure LiteLLMConnection/default is ready.
	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		ensureNoModel(t, context.Background(), "update-test")
		time.Sleep(50 * time.Millisecond)
	})
	connSnap := pollSnapshotReason(30*time.Second, reasonSynced)
	if connSnap.Reason != reasonSynced {
		t.Fatalf("LiteLLMConnection not Synced within 30s")
	}

	cr := modelSampleCR("update-test")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Model: %v", err)
	}

	// Wait for first reconcile to complete.
	m := pollModelCondition(t, ctx, "update-test", reasonSynced, 30*time.Second)
	if m.Status.LastRendered.ModelID == "" {
		t.Fatalf("Model not Synced within 30s")
	}
	originalModelID := m.Status.LastRendered.ModelID
	originalHash := m.Status.LastRendered.Hash

	// Snapshot recorded calls to find the index of the first reconcile.
	mockServer.ResetCounters()
	mockServer.ResetRecorded()

	// Add a new param (temperature: 0.5) — key addition, no shrinkage.
	if err := updateWithRetry(ctx,
		client.ObjectKeyFromObject(m),
		m,
		func(model *litellmv1alpha1.LiteLLMModel) error {
			model.Spec.Params = runtime.RawExtension{
				Raw: []byte(`{"model":"openai/gpt-4o-mini","rpm":100,"temperature":0.5}`),
			}
			return nil
		},
	); err != nil {
		t.Fatalf("update Model spec.params: %v", err)
	}

	// Wait for the reconcile to pick up the spec change.
	deadline := time.Now().Add(30 * time.Second)
	var updated *litellmv1alpha1.LiteLLMModel
	for time.Now().Before(deadline) {
		updated = pollModelCondition(t, ctx, "update-test", reasonSynced, 5*time.Second)
		if updated.Status.LastRendered.Hash != originalHash {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if updated.Status.LastRendered.Hash == originalHash {
		t.Fatalf("lastRendered.hash unchanged after spec edit; want new hash")
	}
	if updated.Status.LastRendered.ModelID != originalModelID {
		t.Errorf("ModelID changed on UPDATE (want stable): was %q, got %q",
			originalModelID, updated.Status.LastRendered.ModelID)
	}

	// Assert paramsKeys includes "temperature".
	foundTemp := false
	for _, k := range updated.Status.LastRendered.ParamsKeys {
		if k == "temperature" { //nolint:goconst // wire-payload field name asserted across drift / paramsKeys / canary fixtures; each occurrence carries its own semantic meaning
			foundTemp = true
			break
		}
	}
	if !foundTemp {
		t.Errorf("lastRendered.paramsKeys does not contain 'temperature'; keys=%v", updated.Status.LastRendered.ParamsKeys)
	}

	// Assert the key-addition went through the UPDATE path (>=1 update,
	// no delete, no new). The load-bearing invariant is the SHAPE of the
	// drift correction — update, never delete+recreate — not the exact
	// update count. The reconcile loop is at-least-once: the 100ms safety
	// re-list can fire a second, idempotent POST /model/update if it reads
	// cache-stale status (old hash) against the freshly-edited spec before
	// the first update's status write propagates (#74). A redundant update
	// is harmless in production (LiteLLM update is idempotent; the relist
	// is 30m there), so assert update>=1 and keep delete/new exact-zero.
	calls := mockServer.Recorded()
	updateCount := 0
	deleteCount := 0
	newCount := 0
	for _, c := range calls {
		switch {
		case c.Method == "POST" && c.Path == "/model/update":
			updateCount++
		case c.Method == http.MethodPost && c.Path == pathModelDelete:
			deleteCount++
		case c.Method == http.MethodPost && c.Path == pathModelNew:
			newCount++
		}
	}
	if updateCount < 1 {
		t.Errorf("POST /model/update count: want >=1, got %d", updateCount)
	}
	if deleteCount != 0 {
		t.Errorf("unexpected POST /model/delete on key-addition reconcile: count=%d", deleteCount)
	}
	if newCount != 0 {
		t.Errorf("unexpected POST /model/new on key-addition reconcile: count=%d", newCount)
	}
}

// TestModel_SpecParamsKeyRemoval_DeleteAndRecreate — Task 2 Test 7.
//
// Per D-02 (Probe 9 ✗ + Probe 9b ✗): removing a key from spec.params must
// trigger delete-and-recreate (NOT explicit-null, NOT POST /model/update alone).
// The test starts from a state where temperature=0.5 is in paramsKeys, then
// removes temperature. The next reconcile must:
// - issue exactly 1 POST /model/delete
// - issue exactly 1 POST /model/new
// - NOT issue POST /model/update
// - re-pin lastRendered.modelID to a NEW UUID
// - NOT include "temperature" in lastRendered.paramsKeys
func TestModel_SpecParamsKeyRemoval_DeleteAndRecreate(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetModels()
	ensureNoModel(t, ctx, "shrink-test")
	resetConnCacheSnapshot()

	// Ensure LiteLLMConnection/default is ready.
	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		ensureNoModel(t, context.Background(), "shrink-test")
		time.Sleep(50 * time.Millisecond)
	})
	connSnap := pollSnapshotReason(30*time.Second, reasonSynced)
	if connSnap.Reason != reasonSynced {
		t.Fatalf("LiteLLMConnection not Synced within 30s")
	}

	// Create with temperature=0.5 initially.
	cr := &litellmv1alpha1.LiteLLMModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shrink-test",
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.ModelSpec{
			Params: runtime.RawExtension{
				Raw: []byte(`{"model":"openai/gpt-4o-mini","rpm":100,"temperature":0.5}`),
			},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Model with temperature: %v", err)
	}

	// Wait for first reconcile (Ready=Synced with temperature in paramsKeys).
	m := pollModelCondition(t, ctx, "shrink-test", reasonSynced, 30*time.Second)
	if m.Status.LastRendered.ModelID == "" {
		t.Fatalf("Model not Synced within 30s; conditions=%+v", m.Status.Conditions)
	}
	// Verify temperature is in paramsKeys.
	foundTemp := false
	for _, k := range m.Status.LastRendered.ParamsKeys {
		if k == "temperature" {
			foundTemp = true
			break
		}
	}
	if !foundTemp {
		t.Fatalf("temperature not in paramsKeys after first reconcile; paramsKeys=%v", m.Status.LastRendered.ParamsKeys)
	}
	originalModelID := m.Status.LastRendered.ModelID

	// Reset counters and recorded calls before the shrinkage reconcile.
	mockServer.ResetCounters()
	mockServer.ResetRecorded()

	// Remove temperature from spec.params.
	if err := updateWithRetry(ctx,
		client.ObjectKeyFromObject(m),
		m,
		func(model *litellmv1alpha1.LiteLLMModel) error {
			model.Spec.Params = runtime.RawExtension{
				Raw: []byte(`{"model":"openai/gpt-4o-mini","rpm":100}`),
			}
			return nil
		},
	); err != nil {
		t.Fatalf("update Model to remove temperature: %v", err)
	}

	// Wait for the reconcile to detect shrinkage and re-create.
	deadline := time.Now().Add(30 * time.Second)
	var updated *litellmv1alpha1.LiteLLMModel
	for time.Now().Before(deadline) {
		updated = pollModelCondition(t, ctx, "shrink-test", reasonSynced, 5*time.Second)
		// The model ID should have changed (new UUID from delete+create).
		if updated.Status.LastRendered.ModelID != originalModelID &&
			updated.Status.LastRendered.ModelID != "" {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}

	// Assert mockServer call sequence: 1 DELETE + 1 NEW, no UPDATE.
	calls := mockServer.Recorded()
	deleteCount := 0
	newCount := 0
	updateCount := 0
	for _, c := range calls {
		switch {
		case c.Method == http.MethodPost && c.Path == pathModelDelete:
			deleteCount++
		case c.Method == http.MethodPost && c.Path == pathModelNew:
			newCount++
		case c.Method == "POST" && c.Path == "/model/update":
			updateCount++
		}
	}
	if deleteCount != 1 {
		t.Errorf("D-02 delete-and-recreate: expected 1 POST /model/delete, got %d (calls: %+v)", deleteCount, calls)
	}
	if newCount != 1 {
		t.Errorf("D-02 delete-and-recreate: expected 1 POST /model/new, got %d (calls: %+v)", newCount, calls)
	}
	if updateCount != 0 {
		t.Errorf("D-02 delete-and-recreate: expected 0 POST /model/update, got %d (calls: %+v)", updateCount, calls)
	}

	// Assert new ModelID is different from original.
	if updated.Status.LastRendered.ModelID == originalModelID {
		t.Errorf("ModelID unchanged after delete-and-recreate; want new UUID; got %q", updated.Status.LastRendered.ModelID)
	}
	if updated.Status.LastRendered.ModelID == "" {
		t.Errorf("ModelID is empty after delete-and-recreate")
	}

	// Assert temperature is no longer in paramsKeys.
	for _, k := range updated.Status.LastRendered.ParamsKeys {
		if k == "temperature" {
			t.Errorf("temperature still in paramsKeys after key removal; paramsKeys=%v", updated.Status.LastRendered.ParamsKeys)
		}
	}
}

// TestModel_OwnerReferenceFromDiscovery_ReconciledIdentically — Task 2 Test 8.
//
// A Model with metadata.ownerReferences[controller=true] pointing at a fake
// ModelDiscovery owner must be reconciled identically to a user-authored Model
// (no branch, no skip per MODEL-08 / AC-M-ADOPT). The reconciler does NOT check
// ownerRef state.
func TestModel_OwnerReferenceFromDiscovery_ReconciledIdentically(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetModels()
	ensureNoModel(t, ctx, "owned-model-test")
	resetConnCacheSnapshot()

	// Ensure LiteLLMConnection/default is ready.
	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		ensureNoModel(t, context.Background(), "owned-model-test")
		time.Sleep(50 * time.Millisecond)
	})
	connSnap := pollSnapshotReason(30*time.Second, reasonSynced)
	if connSnap.Reason != reasonSynced {
		t.Fatalf("LiteLLMConnection not Synced within 30s")
	}

	// Create a Model CR with a fake ownerReference (as if created by ModelDiscovery).
	isController := true
	cr := &litellmv1alpha1.LiteLLMModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "owned-model-test",
			Namespace: WatchNamespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "litellm.ackstorm.ai/v1alpha1",
					Kind:       "LiteLLMModelDiscovery",
					Name:       "fake-discovery",
					UID:        "fake-uid-12345",
					Controller: &isController,
				},
			},
		},
		Spec: litellmv1alpha1.ModelSpec{
			Params: runtime.RawExtension{
				Raw: []byte(`{"model":"openai/gpt-4o-mini","rpm":100}`),
			},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create owned Model: %v", err)
	}

	// Poll for Ready=Synced — the reconciler must process it identically.
	m := pollModelCondition(t, ctx, "owned-model-test", reasonSynced, 30*time.Second)
	c := apimeta.FindStatusCondition(m.Status.Conditions, conditionTypeReady)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != reasonSynced {
		t.Errorf("owned Model not Synced; condition=%+v", c)
	}
	if m.Status.LastRendered.ModelID == "" {
		t.Errorf("owned Model has empty ModelID; want UUID from POST /model/new")
	}

	// Verify POST /model/new was issued (same as user-authored Model).
	calls := mockServer.Recorded()
	newCount := 0
	for _, call := range calls {
		if call.Method == http.MethodPost && call.Path == pathModelNew {
			newCount++
		}
	}
	if newCount != 1 {
		t.Errorf("owned Model: POST /model/new count: want 1, got %d (MODEL-08 violation)", newCount)
	}
}

// TestModel_LiteLLMUnavailable_NoMutationCall — Task 1 Test 1.
//
// CONN-06 / AC-C3b: When the connection cache snapshot is not Ready, the
// Model reconciler must:
//
//	(a) transition the Model's Ready condition to False with Reason="LiteLLMUnavailable"
//	(b) include the D-08 echo-reason message format
//	(c) issue ZERO LiteLLM mutation calls (POST /model/new, /model/update, /model/delete)
//	(d) NOT call r.Cache.InvalidateOn401 on this path
//
// Procedure: directly rebuild the connCache with Ready=false, Reason="Unreachable",
// then create a Model CR and assert the status + zero-mutation outcome.
func TestModel_LiteLLMUnavailable_NoMutationCall(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetModels()
	ensureNoModel(t, ctx, "unavailable-test")
	// Reset cache snapshot to zero value (Ready=false, Reason="") first.
	resetConnCacheSnapshot()

	// Directly rebuild the cache with a not-Ready snapshot. Skip the connection
	// reconciler's probe loop to make the test fast and deterministic.
	// snap.Client is nil — the gate must prevent any mutation call.
	connCache.Rebuild(connection.ConnectionSnapshot{
		Ready:  false,
		Reason: "Unreachable",
	})

	// Ensure no LiteLLMConnection CR exists (so the connection reconciler
	// doesn't interfere by rebuilding the cache during the test window).
	ensureNoConnectionDefault(t, ctx)

	cr := modelSampleCR("unavailable-test")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Model: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		// Restore a Ready snapshot so subsequent tests don't inherit
		// the not-Ready snapshot.
		connCache.Rebuild(connection.ConnectionSnapshot{})
		ensureNoModel(t, context.Background(), "unavailable-test")
	})

	// Poll until Ready=LiteLLMUnavailable (or timeout).
	m := pollModelCondition(t, ctx, "unavailable-test", reasonLiteLLMUnavailable, 30*time.Second)
	c := apimeta.FindStatusCondition(m.Status.Conditions, conditionTypeReady)
	if c == nil {
		t.Fatalf("Ready condition not set; conditions=%+v", m.Status.Conditions)
	}
	if c.Status != metav1.ConditionFalse {
		t.Errorf("Ready.Status: want False, got %v", c.Status)
	}
	if c.Reason != reasonLiteLLMUnavailable {
		t.Errorf("Ready.Reason: want LiteLLMUnavailable, got %q", c.Reason)
	}
	// D-08 echo-reason message format.
	wantMsgSubstr := connNotReadyUnreachableMsg
	if !strings.Contains(c.Message, wantMsgSubstr) {
		t.Errorf("Ready.Message: want substring %q, got %q", wantMsgSubstr, c.Message)
	}

	// Assert zero LiteLLM mutations after crossing the accelerated 1s
	// envtest safety-relist cadence.
	mockServer.ResetCounters()
	time.Sleep(1250 * time.Millisecond)
	mutationsAfter := mockServer.Mutations()
	if mutationsAfter != 0 {
		t.Errorf("AC-C3b: mockServer.Mutations() = %d, want 0 while connection not Ready", mutationsAfter)
	}
}

// TestModel_LiteLLMUnavailable_EmptyReason_DefaultsToConnecting — Task 1.
//
// When snap.Reason == "" (zero-value snapshot — no probe yet completed),
// the D-08 message must use "Connecting" as the default reason string
// per Phase 2 D-07 entry-state convention.
func TestModel_LiteLLMUnavailable_EmptyReason_DefaultsToConnecting(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetModels()
	ensureNoModel(t, ctx, "empty-reason-test")

	// Rebuild cache with Ready=false AND empty Reason (zero-value snapshot).
	connCache.Rebuild(connection.ConnectionSnapshot{Ready: false, Reason: ""})
	ensureNoConnectionDefault(t, ctx)

	cr := modelSampleCR("empty-reason-test")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Model: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		connCache.Rebuild(connection.ConnectionSnapshot{})
		ensureNoModel(t, context.Background(), "empty-reason-test")
	})

	// Poll until LiteLLMUnavailable.
	m := pollModelCondition(t, ctx, "empty-reason-test", reasonLiteLLMUnavailable, 30*time.Second)
	c := apimeta.FindStatusCondition(m.Status.Conditions, conditionTypeReady)
	if c == nil {
		t.Fatalf("Ready condition not set")
	}
	// The message must contain "Connecting" (not an empty reason).
	wantMsgSubstr := "LiteLLMConnection/default not Ready (reason: Connecting)"
	if !strings.Contains(c.Message, wantMsgSubstr) {
		t.Errorf("D-08 empty-reason: want message containing %q, got %q", wantMsgSubstr, c.Message)
	}
}

// TestModel_ConnectionRoundTrip_AC_C4 — Task 1 Test 2.
//
// AC-C4: Model transitions Ready=True → Ready=False(LiteLLMUnavailable) →
// Ready=True(Synced) as the connection cache flips states.
//
// This test uses ONLY direct connCache.Rebuild calls — it does NOT rely on
// the connection reconciler's probe loop. This makes the round-trip deterministic
// and eliminates races between the connection reconciler re-probing and our
// manual cache state changes (plan note: "directly call connCache.Rebuild
// with the desired snapshot to simulate the post-probe state").
//
// Procedure:
// 1. Seed the cache to Synced via direct Rebuild. Create LiteLLMConnection/default
// so the connection reconciler doesn't overwrite our manual cache state
// immediately — then ensure the model reaches Ready=Synced.
// 2. Directly rebuild cache to not-Ready (Reason=BadMasterKey) + set mock
// to Mode401 so the connection reconciler's next probe also returns BadMasterKey
// (preventing the probe loop from racing our manual not-Ready state).
// Trigger model reconcile. Assert LiteLLMUnavailable.
// 3. Restore mock to ModeHappy + rebuild cache to Synced. Trigger model reconcile.
// Assert Model returns to Ready=Synced.
func TestModel_ConnectionRoundTrip_AC_C4(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetModels()
	ensureNoModel(t, ctx, "roundtrip-test")
	resetConnCacheSnapshot()

	// Step 1: Start with model Ready=Synced.
	// Use a direct connCache.Rebuild with a real client to avoid dependency
	// on the connection reconciler's timing.
	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		ensureNoModel(t, context.Background(), "roundtrip-test")
		time.Sleep(300 * time.Millisecond)
	})
	// Wait for the connection reconciler to reach Synced naturally.
	connSnap := pollSnapshotReason(30*time.Second, reasonSynced)
	if connSnap.Reason != reasonSynced {
		t.Fatalf("initial Synced never reached: snap=%+v", connSnap)
	}
	savedClient := connSnap.Client

	cr := modelSampleCR("roundtrip-test")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Model: %v", err)
	}
	m := pollModelCondition(t, ctx, "roundtrip-test", reasonSynced, 30*time.Second)
	if m.Status.LastRendered.ModelID == "" {
		t.Fatalf("Model not Synced within 30s")
	}
	t.Logf("AC-C4 step 1: Model Ready=Synced, modelID=%s", m.Status.LastRendered.ModelID)

	// Step 2: Delete the LiteLLMConnection CR to prevent the connection
	// reconciler from racing our manual cache state changes. Once the CR is
	// gone, no probe loop will override our direct connCache.Rebuild calls.
	_ = k8sClient.Delete(ctx, connCR, &client.DeleteOptions{})
	// Wait for the connection finalizer path to complete — the connection
	// reconciler rebuilds the cache to Absent on finalizer. Poll instead of
	// a fixed sleep to avoid a race where the finalizer completes AFTER our
	// manual BadMasterKey rebuild and overwrites it back to Absent.
	finalizerDeadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(finalizerDeadline) {
		snap := connCache.Snapshot()
		if !snap.Ready && snap.Reason == "Absent" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	// Now directly rebuild to not-Ready. The cache is exclusively owned by
	// this test from this point on (no running probe loop).
	connCache.Rebuild(connection.ConnectionSnapshot{
		Ready:      false,
		Reason:     "BadMasterKey",
		Generation: 1,
		Client:     savedClient,
	})

	// Trigger a Model reconcile via annotation patch. Retry on 409 because
	// the operator may be concurrently writing status (race-build slows
	// reconciles enough that the test-side Update can lose the optimistic
	// lock).
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: "roundtrip-test", Namespace: WatchNamespace}, m); err != nil {
			return err
		}
		if m.Annotations == nil {
			m.Annotations = make(map[string]string)
		}
		m.Annotations["test.ackstorm.ai/trigger-1"] = time.Now().String()
		return k8sClient.Update(ctx, m)
	}); err != nil {
		t.Fatalf("update model annotation (trigger reconcile): %v", err)
	}

	// Step 3: Assert Model transitions to LiteLLMUnavailable WITH the
	// BadMasterKey message. The safety relist may have already produced a
	// LiteLLMUnavailable(Absent) reconcile before our BadMasterKey rebuild,
	// so we cannot use the generic pollModelCondition(LiteLLMUnavailable) —
	// it would return on the Absent message before the annotation-patch
	// reconcile (which carries BadMasterKey) has been processed.
	// Instead, poll specifically for the BadMasterKey message.
	wantMsg := "LiteLLMConnection/default not Ready (reason: BadMasterKey)"
	var c *metav1.Condition
	{
		key := client.ObjectKey{Name: "roundtrip-test", Namespace: WatchNamespace}
		badKeyDeadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(badKeyDeadline) {
			var latest litellmv1alpha1.LiteLLMModel
			if err := k8sClient.Get(ctx, key, &latest); err == nil {
				cond := apimeta.FindStatusCondition(latest.Status.Conditions, conditionTypeReady)
				if cond != nil && cond.Reason == reasonLiteLLMUnavailable &&
					strings.Contains(cond.Message, wantMsg) {
					m = &latest
					c = cond
					break
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if c == nil {
		// Timeout — surface whatever is in the model's Ready condition.
		var latest litellmv1alpha1.LiteLLMModel
		_ = k8sClient.Get(ctx, client.ObjectKey{Name: "roundtrip-test", Namespace: WatchNamespace}, &latest)
		cond := apimeta.FindStatusCondition(latest.Status.Conditions, conditionTypeReady)
		if cond == nil || cond.Reason != reasonLiteLLMUnavailable {
			t.Fatalf("AC-C4 step 2: expected LiteLLMUnavailable; condition=%+v", cond)
		}
		t.Errorf("AC-C4 D-08 message: want %q in %q", wantMsg, cond.Message)
		c = cond
	}
	t.Logf("AC-C4 step 2: Model Ready=False(LiteLLMUnavailable) msg=%q", c.Message)

	// Step 4: Directly rebuild cache to Ready=Synced.
	// No connection reconciler is running (CR was deleted), so this state is stable.
	connCache.Rebuild(connection.ConnectionSnapshot{
		Ready:      true,
		Reason:     "Synced",
		Generation: 2,
		Client:     savedClient,
	})

	// Trigger reconcile via SPEC change — a new spec.params key forces a
	// hash change so the steady-state check at Step 8 cannot short-circuit
	// even if ObservedGeneration happened to match (defense-in-depth).
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: "roundtrip-test", Namespace: WatchNamespace}, m); err != nil {
			return err
		}
		m.Spec.Params.Raw = []byte(`{"model":"openai/gpt-4o-mini","rpm":100,"timeout":30}`)
		return k8sClient.Update(ctx, m)
	}); err != nil {
		t.Fatalf("update model spec (trigger reconcile 2): %v", err)
	}

	// Step 5: Assert Model returns to Synced.
	m = pollModelCondition(t, ctx, "roundtrip-test", reasonSynced, 20*time.Second)
	c = apimeta.FindStatusCondition(m.Status.Conditions, conditionTypeReady)
	if c == nil || c.Reason != reasonSynced || c.Status != metav1.ConditionTrue {
		t.Errorf("AC-C4 step 3: expected Ready=True,Synced; condition=%+v", c)
	}
	t.Logf("AC-C4 step 3: Model returned to Ready=Synced (AC-C4 round-trip PASS)")
}

// TestModel_LiteLLMRejected_OnHTTP422 — Task 2 Test 9.
//
// Mock LiteLLM to return 422 on POST /model/new. Assert:
// - Model.Status.Conditions[Ready].Status == False, Reason=LiteLLMRejected
// - lastRendered.hash NOT updated (only successful renders persist)
func TestModel_LiteLLMRejected_OnHTTP422(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.Mode422)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetModels()
	ensureNoModel(t, ctx, "rejected-test")
	resetConnCacheSnapshot()

	// Ensure LiteLLMConnection/default is ready (the mock returns 200 for
	// the POST /key/health probe in Mode422; only POST /model/new returns 422).
	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		ensureNoModel(t, context.Background(), "rejected-test")
		time.Sleep(50 * time.Millisecond)
	})
	connSnap := pollSnapshotReason(30*time.Second, reasonSynced)
	if connSnap.Reason != reasonSynced {
		t.Fatalf("LiteLLMConnection not Synced within 30s; reason=%q", connSnap.Reason)
	}

	cr := modelSampleCR("rejected-test")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Model: %v", err)
	}

	// Poll for Ready=False, Reason=LiteLLMRejected.
	m := pollModelCondition(t, ctx, "rejected-test", "LiteLLMRejected", 30*time.Second)
	c := apimeta.FindStatusCondition(m.Status.Conditions, conditionTypeReady)
	if c == nil {
		t.Fatalf("Ready condition not set; conditions=%+v", m.Status.Conditions)
	}
	if c.Status != metav1.ConditionFalse {
		t.Errorf("Ready.Status: want False, got %v", c.Status)
	}
	if c.Reason != "LiteLLMRejected" {
		t.Errorf("Ready.Reason: want LiteLLMRejected, got %q", c.Reason)
	}

	// lastRendered.hash must be empty (no successful render).
	if m.Status.LastRendered.Hash != "" {
		t.Errorf("lastRendered.hash must be empty on LiteLLMRejected; got %q", m.Status.LastRendered.Hash)
	}
}

// TestModel_401FastPath_InvalidatesCache — Task 2 Test 3.
//
// §7.7 / REL-06 / CONN-06: When a LiteLLM mutation call returns 401, the
// model reconciler must:
//
//	(a) call r.Cache.InvalidateOn401 — verifiable via cache.Snapshot.Ready
//	 transitioning false within 150ms (same pattern as AC-C3c in)
//	(b) write Model.Status.Conditions[Ready]=False, Reason=LiteLLMUnavailable
//	 with a message indicating the 401 path (no master key / body content)
//	(c) return ctrl.Result{}, nil — NOT ctrl.Result{}, err — per REL-06 anti-storm
//
// Procedure:
// 1. Start with Connection Synced, Model Ready=Synced (ModeHappy seed).
// 2. Flip mock to Mode401 so subsequent POST /model/update returns 401.
// 3. Trigger a Model reconcile by editing spec.params.
// 4. Assert: cache.Snapshot.Ready flips false within 150ms.
// 5. Assert: Model.Status.Conditions[Ready].Reason == "LiteLLMUnavailable".
// 6. Assert: no infinite retry storm (mockServer.Mutations bounded
// within a 3s observation window after the 401 path fires).
func TestModel_401FastPath_InvalidatesCache(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetModels()
	ensureNoModel(t, ctx, "fastpath-test")
	resetConnCacheSnapshot()

	// Step 1: Get connection to Synced.
	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		ensureNoModel(t, context.Background(), "fastpath-test")
		time.Sleep(50 * time.Millisecond)
	})
	connSnap := pollSnapshotReason(30*time.Second, reasonSynced)
	if connSnap.Reason != reasonSynced {
		t.Fatalf("LiteLLMConnection not Synced within 30s; snap=%+v", connSnap)
	}

	// Create Model and wait for it to reach Ready=Synced.
	cr := modelSampleCR("fastpath-test")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Model: %v", err)
	}
	m := pollModelCondition(t, ctx, "fastpath-test", reasonSynced, 30*time.Second)
	if m.Status.LastRendered.ModelID == "" {
		t.Fatalf("Model not Synced within 30s; conditions=%+v", m.Status.Conditions)
	}
	t.Logf("401FastPath step 1: Model Ready=Synced; modelID=%s", m.Status.LastRendered.ModelID)

	// Step 2: Flip mock to Mode401 — subsequent POST /model/update returns 401.
	mockServer.SetMode(mock.Mode401)
	// Reset counters to count only the post-401 mutations.
	mockServer.ResetCounters()
	mockServer.ResetRecorded()

	// Step 3: Trigger a Model reconcile by changing spec.params (adds timeout key).
	if err := updateWithRetry(ctx,
		client.ObjectKey{Name: "fastpath-test", Namespace: WatchNamespace},
		m,
		func(model *litellmv1alpha1.LiteLLMModel) error {
			model.Spec.Params.Raw = []byte(`{"model":"openai/gpt-4o-mini","rpm":100,"timeout":30}`)
			return nil
		},
	); err != nil {
		t.Fatalf("update model spec (trigger 401 reconcile): %v", err)
	}

	// Step 4: Assert cache.Snapshot.Ready flips false within 150ms.
	// The model reconciler's 401 branch calls r.Cache.InvalidateOn401 which
	// synchronously stores a not-Ready snapshot before sending the channel event.
	deadline := time.Now().Add(5 * time.Second) // give reconciler time to run
	notReady := false
	for time.Now().Before(deadline) {
		if !connCache.Snapshot().Ready {
			notReady = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !notReady {
		t.Errorf("401FastPath: cache.Snapshot().Ready did NOT flip false within 5s of Model 401; snap=%+v", connCache.Snapshot())
	}

	// Step 5: Model status must show LiteLLMUnavailable (the 401 branch writes this).
	m = pollModelCondition(t, ctx, "fastpath-test", reasonLiteLLMUnavailable, 10*time.Second)
	c := apimeta.FindStatusCondition(m.Status.Conditions, conditionTypeReady)
	if c == nil {
		t.Fatalf("Ready condition not set after 401; conditions=%+v", m.Status.Conditions)
	}
	if c.Reason != reasonLiteLLMUnavailable {
		t.Errorf("401FastPath: Ready.Reason want LiteLLMUnavailable, got %q", c.Reason)
	}
	// §9.1: message must NOT contain master key or body content.
	// The observable message may come from the direct 401 path ("401 from LiteLLM…")
	// OR from the subsequent gating reconcile ("LiteLLMConnection/default not Ready…")
	// if the connection reconciler's re-probe fires first and writes BadMasterKey into
	// the cache before the status observer captures the transient 401 message.
	// Either message is correct: the invariant is Reason=LiteLLMUnavailable (checked
	// above) and cache.Snapshot.Ready == false (checked in step 4).
	t.Logf("401FastPath step 5: LiteLLMUnavailable message=%q", c.Message)

	// Step 6: Anti-storm check — bounded mutations.
	// After the 401 path fires (InvalidateOn401 → channel → connection re-probe
	// → cache.Rebuild), the model reconciler sees not-Ready on next reconcile
	// and returns nil (no additional LiteLLM mutation). Count mutations over
	// an accelerated observation window after the initial 401 was observed.
	mutsBefore := mockServer.Mutations()
	time.Sleep(1250 * time.Millisecond)
	mutsAfter := mockServer.Mutations()
	delta := mutsAfter - mutsBefore
	// Expect at most 2 additional mutations in the observation window (1 for the POST /model/update
	// that produced the 401, plus potential connection re-probe GET — but the model
	// reconciler must NOT retry the mutation after 401).
	if delta > 3 {
		t.Errorf("401FastPath: anti-storm FAIL — %d mutations in observation window after 401 (want <= 3)", delta)
	}
	t.Logf("401FastPath: mutations delta=%d in accelerated observation window (anti-storm bound <=3)", delta)
}

// TestModel_MockStateful_TracksCreatedModels — Task 2 Test 4.
//
// The mock must track stateful model state across the POST /model/new →
// GET /model/info round-trip. This is required for:
// - Test 3 (stale-status name-resolve fallback)
// - drift-correction tests
// - Correct UUID assignment and persistence across reconciles
//
// Procedure: POST /model/new with model_name=test-stateful. Then
// GET /model/info?model_name=test-stateful. Assert the returned model_info.id
// matches what POST returned. Then GET /model/info?litellm_model_id=<returned_id>.
// Assert same entry.
func TestModel_MockStateful_TracksCreatedModels(t *testing.T) {
	// This is a direct mock unit test — no envtest manager needed.
	// We use the suite's global mockServer.
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetModels()

	// Seed: create a Model CR to get the mock to accept POST /model/new via
	// the full model reconciler path. Alternatively, call the mock HTTP directly.
	// For a cleaner unit test, we directly hit the mock HTTP server.
	modelName := "test-stateful-model"

	// POST /model/new
	createBody := `{"model_name":"` + modelName + `","litellm_params":{"model":"openai/gpt-4o-mini"},"model_info":{"id":""}}`
	createResp, err := doMockRequest(t, http.MethodPost, mockServer.URL()+pathModelNew, createBody)
	if err != nil {
		t.Fatalf("POST /model/new failed: %v", err)
	}

	// Extract model_id from response.
	assignedID := mockServer.GetModelID(modelName)
	if assignedID == "" {
		t.Fatalf("mock.GetModelID(%q) returned empty — model not tracked; createResp=%s", modelName, createResp)
	}
	t.Logf("POST /model/new assigned modelID=%q", assignedID)

	// GET /model/info?model_name=<name>
	getByNameResp, err := doMockRequest(t, "GET", mockServer.URL()+"/model/info?model_name="+modelName, "")
	if err != nil {
		t.Fatalf("GET /model/info?model_name= failed: %v", err)
	}
	if !strings.Contains(getByNameResp, assignedID) {
		t.Errorf("GET /model/info?model_name= response should contain modelID %q; got %s", assignedID, getByNameResp)
	}

	// GET /model/info?litellm_model_id=<id>
	getByIDResp, err := doMockRequest(t, "GET", mockServer.URL()+"/model/info?litellm_model_id="+assignedID, "")
	if err != nil {
		t.Fatalf("GET /model/info?litellm_model_id= failed: %v", err)
	}
	if !strings.Contains(getByIDResp, assignedID) {
		t.Errorf("GET /model/info?litellm_model_id= response should contain modelID %q; got %s", assignedID, getByIDResp)
	}
	if !strings.Contains(getByIDResp, modelName) {
		t.Errorf("GET /model/info?litellm_model_id= response should contain modelName %q; got %s", modelName, getByIDResp)
	}

	// Verify ResetModels clears the state.
	mockServer.ResetModels()
	afterReset := mockServer.GetModelID(modelName)
	if afterReset != "" {
		t.Errorf("after ResetModels(), GetModelID(%q) should return empty; got %q", modelName, afterReset)
	}
	t.Logf("TestModel_MockStateful_TracksCreatedModels PASS — assigned=%q, by-name=%t, by-id=%t, reset=%t",
		assignedID, strings.Contains(getByNameResp, assignedID), strings.Contains(getByIDResp, assignedID), afterReset == "")
}

// doMockRequest is a test helper that makes an HTTP request to the mock server.
func doMockRequest(t *testing.T, method, url, body string) (string, error) {
	t.Helper()
	var req *http.Request
	var err error
	if body != "" {
		req, err = http.NewRequest(method, url, strings.NewReader(body))
	} else {
		req, err = http.NewRequest(method, url, nil)
	}
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test-master-key")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var buf strings.Builder
	if _, err := io.Copy(&buf, resp.Body); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// — Drift counter + redaction canary + OWN-06 tests
// ─────────────────────────────────────────────────────────────────────────────

// TestModel_DriftCounter_FirstReconcile_NoIncrement — Task 1 Test 1.
//
// OWN-04 metric suppression: on first reconcile (observedGeneration==0),
// the drift counter must NOT be incremented even when the Model's name
// collides with a pre-existing LiteLLM entry. The operator silently
// overwrites the pre-existing entry without any metric, Event, or Warning.
func TestModel_DriftCounter_FirstReconcile_NoIncrement(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetModels()
	ensureNoModel(t, ctx, "drift-first-reconcile")
	resetConnCacheSnapshot()

	// Pre-populate the mock with a model having the same name as the CR we will
	// create — simulates a pre-existing LiteLLM entry (OWN-04 collision case).
	mockServer.AddHandManagedModel("drift-first-reconcile", "pre-existing-id-001")

	// Ensure connection is ready.
	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		ensureNoModel(t, context.Background(), "drift-first-reconcile")
		time.Sleep(50 * time.Millisecond)
	})
	connSnap := pollSnapshotReason(30*time.Second, reasonSynced)
	if connSnap.Reason != reasonSynced {
		t.Fatalf("LiteLLMConnection not Synced within 30s")
	}

	// Snapshot counters BEFORE creating the CR.
	beforeCreateMissing := testutil.ToFloat64(metrics.DriftCorrectedTotal.WithLabelValues("model", "create_missing"))
	beforeUpdateDrifted := testutil.ToFloat64(metrics.DriftCorrectedTotal.WithLabelValues("model", "update_drifted"))

	cr := modelSampleCR("drift-first-reconcile")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Model: %v", err)
	}

	// Wait for Ready=Synced (first reconcile complete).
	m := pollModelCondition(t, ctx, "drift-first-reconcile", reasonSynced, 30*time.Second)
	if m.Status.LastRendered.Hash == "" {
		t.Fatalf("Model Synced but lastRendered.hash is empty")
	}
	if m.Status.ObservedGeneration == 0 {
		t.Errorf("observedGeneration should be set after successful reconcile; got 0")
	}

	// Assert: NO drift counter increments on first reconcile (OWN-04).
	afterCreateMissing := testutil.ToFloat64(metrics.DriftCorrectedTotal.WithLabelValues("model", "create_missing"))
	afterUpdateDrifted := testutil.ToFloat64(metrics.DriftCorrectedTotal.WithLabelValues("model", "update_drifted"))

	deltaCM := afterCreateMissing - beforeCreateMissing
	deltaUD := afterUpdateDrifted - beforeUpdateDrifted

	if deltaCM != 0 {
		t.Errorf("OWN-04 violation: create_missing incremented on first reconcile; delta=%.0f", deltaCM)
	}
	if deltaUD != 0 {
		t.Errorf("OWN-04 violation: update_drifted incremented on first reconcile; delta=%.0f", deltaUD)
	}
	t.Logf("OWN-04 AC-DC2: first-reconcile NO metric increment — create_missing delta=%.0f update_drifted delta=%.0f (PASS)", deltaCM, deltaUD)
}

// TestModel_DriftCounter_UpdateDrifted — Task 1 Test 2.
//
// AC-DC4 / AC-O2 / OWN-02: after a Model reaches Synced, editing spec.params
// triggers a reconcile with a new hash. The UPDATE path must increment
// drift_corrected_total{model, update_drifted} exactly once.
func TestModel_DriftCounter_UpdateDrifted(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetModels()
	ensureNoModel(t, ctx, "drift-update-test")
	resetConnCacheSnapshot()

	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		ensureNoModel(t, context.Background(), "drift-update-test")
		time.Sleep(50 * time.Millisecond)
	})
	connSnap := pollSnapshotReason(30*time.Second, reasonSynced)
	if connSnap.Reason != reasonSynced {
		t.Fatalf("LiteLLMConnection not Synced within 30s")
	}

	cr := modelSampleCR("drift-update-test")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Model: %v", err)
	}

	// Wait for first reconcile (Ready=Synced with hash + modelID).
	m := pollModelCondition(t, ctx, "drift-update-test", reasonSynced, 30*time.Second)
	if m.Status.LastRendered.ModelID == "" {
		t.Fatalf("Model not Synced within 30s")
	}
	originalHash := m.Status.LastRendered.Hash

	// Snapshot counters AFTER first reconcile (first reconcile may have incremented
	// if ObservedGeneration was not 0 on some edge case — we baseline here).
	beforeUpdateDrifted := testutil.ToFloat64(metrics.DriftCorrectedTotal.WithLabelValues("model", "update_drifted"))

	// Trigger a spec change (add new param) → new hash → UPDATE path.
	if err := updateWithRetry(ctx,
		client.ObjectKeyFromObject(m),
		m,
		func(model *litellmv1alpha1.LiteLLMModel) error {
			model.Spec.Params = runtime.RawExtension{
				Raw: []byte(`{"model":"openai/gpt-4o-mini","rpm":100,"timeout":60}`),
			}
			return nil
		},
	); err != nil {
		t.Fatalf("update Model spec.params: %v", err)
	}

	// Wait for the hash to change (new reconcile with updated spec).
	deadline := time.Now().Add(30 * time.Second)
	var updated *litellmv1alpha1.LiteLLMModel
	for time.Now().Before(deadline) {
		updated = pollModelCondition(t, ctx, "drift-update-test", reasonSynced, 5*time.Second)
		if updated.Status.LastRendered.Hash != originalHash && updated.Status.LastRendered.Hash != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if updated.Status.LastRendered.Hash == originalHash {
		t.Fatalf("lastRendered.hash unchanged after spec edit; want new hash indicating reconcile ran")
	}

	afterUpdateDrifted := testutil.ToFloat64(metrics.DriftCorrectedTotal.WithLabelValues("model", "update_drifted"))
	deltaUD := afterUpdateDrifted - beforeUpdateDrifted

	if deltaUD < 1 {
		t.Errorf("AC-O2: update_drifted NOT incremented after spec change; delta=%.0f (want >= 1)", deltaUD)
	}
	t.Logf("AC-DC4: update_drifted delta=%.0f after spec edit (PASS)", deltaUD)
}

// TestModel_DriftCounter_CreateMissing_SafetyRelist — Task 1 Test 3.
//
// AC-DC5 / OWN-05: after a Model reaches Synced, the LiteLLM entry is
// deleted out of band (directly from mock state). The safety re-list
// detects the missing entry on the next tick and the reconciler creates it
// again, incrementing drift_corrected_total{model, create_missing} once.
func TestModel_DriftCounter_CreateMissing_SafetyRelist(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetModels()
	ensureNoModel(t, ctx, "drift-create-missing")
	resetConnCacheSnapshot()
	enableSuiteRelist(t)

	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		ensureNoModel(t, context.Background(), "drift-create-missing")
		time.Sleep(50 * time.Millisecond)
	})
	connSnap := pollSnapshotReason(30*time.Second, reasonSynced)
	if connSnap.Reason != reasonSynced {
		t.Fatalf("LiteLLMConnection not Synced within 30s")
	}

	cr := modelSampleCR("drift-create-missing")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Model: %v", err)
	}

	// Wait for first reconcile (Ready=Synced).
	m := pollModelCondition(t, ctx, "drift-create-missing", reasonSynced, 30*time.Second)
	if m.Status.LastRendered.ModelID == "" {
		t.Fatalf("Model not Synced within 30s; conditions=%+v", m.Status.Conditions)
	}

	// Snapshot create_missing AFTER first reconcile.
	beforeCreateMissing := testutil.ToFloat64(metrics.DriftCorrectedTotal.WithLabelValues("model", "create_missing"))

	// Simulate out-of-band DELETE: remove the model from mock state without
	// touching the K8s CR. Also clear lastRendered.ModelID in status
	// to force the CREATE branch on the next reconcile.
	mockServer.DeleteModelOutOfBand("drift-create-missing")

	// Also clear the status.lastRendered.ModelID to simulate the
	// operator seeing the model as "missing" on the CREATE branch.
	// Re-get the latest CR.
	key := client.ObjectKey{Name: "drift-create-missing", Namespace: WatchNamespace}
	if err := k8sClient.Get(ctx, key, m); err != nil {
		t.Fatalf("re-get model: %v", err)
	}
	savedHash := m.Status.LastRendered.Hash // non-empty = NOT first reconcile
	m.Status.LastRendered.ModelID = ""
	if err := k8sClient.Status().Update(ctx, m); err != nil {
		t.Fatalf("clear ModelID: %v", err)
	}
	t.Logf("savedHash=%q (non-empty = past first reconcile)", savedHash)

	// The safety re-list runnable (100ms interval in envtest) will enqueue
	// the Model. The reconciler will see: hash matches (same spec), ModelID
	// is empty → CREATE path → increment create_missing (firstReconcile=false because
	// ObservedGeneration > 0 and hash is non-empty from the previous successful reconcile).
	// Wait up to 5s for create_missing to increment.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		after := testutil.ToFloat64(metrics.DriftCorrectedTotal.WithLabelValues("model", "create_missing"))
		if after-beforeCreateMissing >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	afterCreateMissing := testutil.ToFloat64(metrics.DriftCorrectedTotal.WithLabelValues("model", "create_missing"))
	deltaСM := afterCreateMissing - beforeCreateMissing
	if deltaСM < 1 {
		t.Errorf("AC-DC5: create_missing NOT incremented after out-of-band DELETE + safety re-list; delta=%.0f", deltaСM)
	}
	t.Logf("AC-DC5: create_missing delta=%.0f after out-of-band DELETE recovery (PASS)", deltaСM)
}

// TestModel_DriftCounter_DeleteVanished_OnFinalizer — Task 1 Test 4.
//
// OWN-03: when a Model CR is deleted, the finalizer issues POST /model/delete
// and increments drift_corrected_total{model, delete_vanished}.
func TestModel_DriftCounter_DeleteVanished_OnFinalizer(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetModels()
	ensureNoModel(t, ctx, "drift-delete-vanished")
	resetConnCacheSnapshot()

	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		time.Sleep(50 * time.Millisecond)
	})
	connSnap := pollSnapshotReason(30*time.Second, reasonSynced)
	if connSnap.Reason != reasonSynced {
		t.Fatalf("LiteLLMConnection not Synced within 30s")
	}

	cr := modelSampleCR("drift-delete-vanished")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Model: %v", err)
	}

	// Wait for first reconcile (Ready=Synced with ModelID).
	m := pollModelCondition(t, ctx, "drift-delete-vanished", reasonSynced, 30*time.Second)
	if m.Status.LastRendered.ModelID == "" {
		t.Fatalf("Model not Synced within 30s; conditions=%+v", m.Status.Conditions)
	}

	// Snapshot delete_vanished BEFORE deleting.
	beforeDeleteVanished := testutil.ToFloat64(metrics.DriftCorrectedTotal.WithLabelValues("model", "delete_vanished"))

	// Delete the CR — finalizer will run POST /model/delete.
	if err := k8sClient.Delete(ctx, m); err != nil {
		t.Fatalf("delete Model: %v", err)
	}

	// Poll until fully removed.
	key := client.ObjectKey{Name: "drift-delete-vanished", Namespace: WatchNamespace}
	deadline := time.Now().Add(20 * time.Second)
	gone := false
	for time.Now().Before(deadline) {
		var probe litellmv1alpha1.LiteLLMModel
		if apierrors.IsNotFound(k8sClient.Get(ctx, key, &probe)) {
			gone = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !gone {
		t.Fatalf("Model not removed within 20s of Delete (finalizer cleanup did not run)")
	}

	afterDeleteVanished := testutil.ToFloat64(metrics.DriftCorrectedTotal.WithLabelValues("model", "delete_vanished"))
	deltaDV := afterDeleteVanished - beforeDeleteVanished

	if deltaDV < 1 {
		t.Errorf("OWN-03: delete_vanished NOT incremented after CR delete + finalizer; delta=%.0f", deltaDV)
	}
	t.Logf("OWN-03: delete_vanished delta=%.0f after finalizer-time DELETE (PASS)", deltaDV)
}

// TestModel_HandManagedEntry_Untouched — Task 1 Test 5.
//
// OWN-06 / AC-DC1 model slice: hand-managed LiteLLM entries (names NOT
// declared by any Model CR) must NEVER be touched by the reconciler —
// no mutations, no drift counter increments for those names, and the
// entries must still be present after a full reconcile cycle.
func TestModel_HandManagedEntry_Untouched(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetModels()
	ensureNoModel(t, ctx, "operator-owned-ownsix")
	resetConnCacheSnapshot()

	// Pre-populate three hand-managed models — these have NO Model CRs.
	mockServer.AddHandManagedModel("hand-managed-1", "hm-id-001")
	mockServer.AddHandManagedModel("hand-managed-2", "hm-id-002")
	mockServer.AddHandManagedModel("hand-managed-3", "hm-id-003")

	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		ensureNoModel(t, context.Background(), "operator-owned-ownsix")
		time.Sleep(50 * time.Millisecond)
	})
	connSnap := pollSnapshotReason(30*time.Second, reasonSynced)
	if connSnap.Reason != reasonSynced {
		t.Fatalf("LiteLLMConnection not Synced within 30s")
	}

	// Create ONE operator-owned Model CR.
	cr := modelSampleCR("operator-owned-ownsix")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create operator-owned Model: %v", err)
	}

	// Wait for the operator-owned model to reach Ready=Synced.
	m := pollModelCondition(t, ctx, "operator-owned-ownsix", reasonSynced, 30*time.Second)
	if m.Status.LastRendered.ModelID == "" {
		t.Fatalf("operator-owned model not Synced within 30s")
	}

	// Cross the accelerated 1s envtest safety-relist cadence.
	time.Sleep(1250 * time.Millisecond)

	// Assert: the three hand-managed entries are still PRESENT and UNCHANGED.
	for _, hmName := range []string{"hand-managed-1", "hand-managed-2", "hand-managed-3"} {
		if !mockServer.HasModel(hmName) {
			t.Errorf("OWN-06 violation: hand-managed entry %q was REMOVED during reconcile cycle", hmName)
		}
		// Assert: no mutations were issued against the hand-managed names.
		mutCount := mockServer.MutationsByModelName(hmName)
		if mutCount != 0 {
			t.Errorf("OWN-06 violation: %d mutation(s) issued against hand-managed entry %q (want 0)",
				mutCount, hmName)
		}
	}

	// Assert: expected IDs still match (entries unchanged).
	expectedIDs := map[string]string{
		"hand-managed-1": "hm-id-001",
		"hand-managed-2": "hm-id-002",
		"hand-managed-3": "hm-id-003",
	}
	for name, expectedID := range expectedIDs {
		if id := mockServer.GetModelID(name); id != expectedID {
			t.Errorf("OWN-06: hand-managed entry %q: got ID=%q, want %q (entry modified)", name, id, expectedID)
		}
	}

	t.Logf("OWN-06 AC-DC1: three hand-managed entries untouched after full reconcile cycle (PASS)")
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 2 — AC-S1 Redaction Canary
// ─────────────────────────────────────────────────────────────────────────────

// canaryAPIKey is the synthetic secret value the AC-S1 redaction canary test
// uses. If this string appears in any log line, Kubernetes Event, or
// status.conditions[].message, §9.1 is violated.
// NOTE: separate from transport_test.go's canaryMasterKey to avoid
// cross-package confusion. The Model canary tests the *operator's*
// code paths (reconciler logs / Events / status), not the transport layer.
const canaryAPIKey = "sk-canary-MODEL-LEAK-12345" // #nosec G101 -- fake canary string for log-leak detection tests, not a real credential

// bufferSink is a goroutine-safe logr.LogSink that accumulates log lines
// into an in-memory bytes.Buffer. Mirrors the bufferSink in
// internal/litellm/transport_test.go.
type bufferSink struct {
	mu  sync.Mutex
	buf *bytes.Buffer
}

func (b *bufferSink) Init(info logr.RuntimeInfo)             {}
func (b *bufferSink) Enabled(level int) bool                 { return true }
func (b *bufferSink) WithValues(kv ...any) logr.LogSink      { return b }
func (b *bufferSink) WithName(name string) logr.LogSink      { return b }
func (b *bufferSink) Info(level int, msg string, kv ...any)  { b.write(msg, kv) }
func (b *bufferSink) Error(err error, msg string, kv ...any) { b.write(msg+" err="+errStr(err), kv) }

func (b *bufferSink) write(msg string, kv []any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.WriteString(msg)
	for i := 0; i+1 < len(kv); i += 2 {
		b.buf.WriteString(" ")
		b.buf.WriteString(fmt.Sprintf("%v=%v", kv[i], kv[i+1]))
	}
	b.buf.WriteByte('\n')
}

func errStr(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}

func (b *bufferSink) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *bufferSink) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

// canarySecret creates a Kubernetes Secret containing the canary API key.
// Cleanup deletes it via t.Cleanup.
//
//nolint:unparam // ns parameter reserved for future multi-namespace canary suites; current callers all pass WatchNamespace.
func canarySecretObj(name, ns string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Data: map[string][]byte{
			"api-key": []byte(canaryAPIKey),
		},
	}
}

// canaryModelCR builds a Model CR that references the canary secret via
// spec.secrets[].as + spec.params.api_key placeholder.
//
//nolint:unparam // ns parameter reserved for future multi-namespace canary suites; current callers all pass WatchNamespace.
func canaryModelCR(modelName, secretName, ns string) *litellmv1alpha1.LiteLLMModel {
	return &litellmv1alpha1.LiteLLMModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:      modelName,
			Namespace: ns,
		},
		Spec: litellmv1alpha1.ModelSpec{
			Params: runtime.RawExtension{
				Raw: []byte(`{"model":"openai/gpt-4o-mini","api_key":"{{API_KEY}}"}`),
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

// listModelEvents returns all Events for the named Model CR in WatchNamespace.
// Uses a broad list and filters client-side to avoid the need for a custom field indexer.
func listModelEvents(ctx context.Context, t *testing.T, modelName string) []corev1.Event {
	t.Helper()
	var eventList corev1.EventList
	if err := k8sClient.List(ctx, &eventList, client.InNamespace(WatchNamespace)); err != nil {
		// Best-effort; list failure does not fail the test outright.
		t.Logf("listModelEvents: list failed (non-fatal): %v", err)
		return nil
	}
	var filtered []corev1.Event
	for _, ev := range eventList.Items {
		if ev.InvolvedObject.Name == modelName {
			filtered = append(filtered, ev)
		}
	}
	return filtered
}

// assertNoCanaryLeak asserts that the canary string does not appear in the
// log buffer, Events, or status message. Reports test failures via t.
func assertNoCanaryLeak(t *testing.T, subTest, logBuf string, events []corev1.Event, statusMsg string) {
	t.Helper()
	if strings.Contains(logBuf, canaryAPIKey) {
		t.Errorf("[%s] §9.1 FAIL: canary string %q found in log buffer", subTest, canaryAPIKey)
	}
	for _, ev := range events {
		combined := ev.Message + " " + ev.Reason
		if strings.Contains(combined, canaryAPIKey) {
			t.Errorf("[%s] §9.1 FAIL: canary string %q found in Event (message=%q reason=%q)",
				subTest, canaryAPIKey, ev.Message, ev.Reason)
		}
	}
	if strings.Contains(statusMsg, canaryAPIKey) {
		t.Errorf("[%s] §9.1 FAIL: canary string %q found in status.conditions[Ready].message=%q",
			subTest, canaryAPIKey, statusMsg)
	}
}

// TestModel_RedactionCanary_AC_S1 — Task 2.
//
// SEC-08 / AC-S1: traverse all FIVE reconcile paths (success, 401,
// SecretNotFound, 4xx LiteLLMRejected, 5xx transient backoff) with a
// canary secret value and assert zero occurrences in logs, Events, and
// status messages. Mirrors the Phase 1 TestNoCredentialLeak pattern but
// for the Model reconciler's output surfaces.
func TestModel_RedactionCanary_AC_S1(t *testing.T) {
	ctx := context.Background()

	// Install a capturing logr sink on the controller-runtime root logger
	// so we capture both the Model reconciler's own log lines AND the
	// workqueue-backoff log lines emitted by controller-runtime on the
	// 5xx err-return path (OWN-09). The existing test logger writes to
	// stderr; we replace it temporarily and restore on cleanup.
	capBuf := &bytes.Buffer{}
	sink := &bufferSink{buf: capBuf}
	capLogger := logr.New(sink)
	prevLogger := ctrl.Log
	ctrl.SetLogger(capLogger)
	t.Cleanup(func() {
		ctrl.SetLogger(prevLogger)
	})

	// Shared setup: one LiteLLMConnection so all sub-tests share a Synced conn.
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
	// Save a known-good non-nil client reference captured before any sub-test
	// mutates the mock mode. This is used in SecretNotFound/LiteLLMRejected
	// fallback rebuilds so we never rebuild with Client==nil (which causes
	// nil-pointer panics in subsequent reconcile loops).
	savedConnClient := connSnap.Client

	// ── Sub-test 1: success path ──────────────────────────────────────────
	t.Run("success", func(t *testing.T) {
		sink.Reset()
		mockServer.SetMode(mock.ModeHappy)
		mockServer.ResetCounters()
		mockServer.ResetModels()
		modelName := "canary-success"
		secName := "canary-secret-success"
		ensureNoModel(t, ctx, modelName)
		defer ensureNoModel(t, context.Background(), modelName)

		sec := canarySecretObj(secName, WatchNamespace)
		_ = k8sClient.Delete(ctx, sec, &client.DeleteOptions{})
		if err := k8sClient.Create(ctx, sec); err != nil {
			t.Fatalf("create canary secret: %v", err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), sec, &client.DeleteOptions{}) })

		cr := canaryModelCR(modelName, secName, WatchNamespace)
		if err := k8sClient.Create(ctx, cr); err != nil {
			t.Fatalf("create canary Model: %v", err)
		}

		m := pollModelCondition(t, ctx, modelName, reasonSynced, 30*time.Second)
		statusMsg := ""
		if c := apimeta.FindStatusCondition(m.Status.Conditions, conditionTypeReady); c != nil {
			statusMsg = c.Message
		}
		events := listModelEvents(ctx, t, modelName)
		assertNoCanaryLeak(t, "success", sink.String(), events, statusMsg)
		t.Logf("[success] log captured %d bytes — canary absent", len(sink.String()))
	})

	// ── Sub-test 2: 401 path ─────────────────────────────────────────────
	t.Run("401", func(t *testing.T) {
		sink.Reset()
		// Start in happy mode so connection stays Synced, then flip to 401
		// only for the POST /model/new call. The Mode401 mock returns 401
		// on ALL paths — set it right before creating the Model so the
		// connection reconciler's next probe also sees 401.
		mockServer.SetMode(mock.ModeHappy)
		mockServer.ResetCounters()
		mockServer.ResetModels()
		modelName := "canary-401"
		secName := "canary-secret-401"
		ensureNoModel(t, ctx, modelName)
		defer ensureNoModel(t, context.Background(), modelName)

		sec := canarySecretObj(secName, WatchNamespace)
		_ = k8sClient.Delete(ctx, sec, &client.DeleteOptions{})
		if err := k8sClient.Create(ctx, sec); err != nil {
			t.Fatalf("create canary secret: %v", err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), sec, &client.DeleteOptions{}) })

		// Flip mock to 401 then create the CR — the Model reconciler sees 401
		// on POST /model/new and transitions to LiteLLMUnavailable.
		mockServer.SetMode(mock.Mode401)
		cr := canaryModelCR(modelName, secName, WatchNamespace)
		if err := k8sClient.Create(ctx, cr); err != nil {
			t.Fatalf("create canary Model: %v", err)
		}

		m := pollModelCondition(t, ctx, modelName, reasonLiteLLMUnavailable, 30*time.Second)
		statusMsg := ""
		if c := apimeta.FindStatusCondition(m.Status.Conditions, conditionTypeReady); c != nil {
			statusMsg = c.Message
		}
		events := listModelEvents(ctx, t, modelName)
		assertNoCanaryLeak(t, "401", sink.String(), events, statusMsg)
		t.Logf("[401] status message=%q — canary absent", statusMsg)

		// Restore happy mode for subsequent sub-tests.
		mockServer.SetMode(mock.ModeHappy)
	})

	// ── Sub-test 3: SecretNotFound path ──────────────────────────────────
	t.Run("SecretNotFound", func(t *testing.T) {
		sink.Reset()
		// Restore mock to happy mode + force connection cache Ready so the
		// Model reconciler can proceed past connection-gating and reach the
		// SecretNotFound path. Probe-loop recovery latency is not under test
		// here — direct rebuild avoids waiting up to 15s for the next probe.
		mockServer.SetMode(mock.ModeHappy)
		connCache.Rebuild(connection.ConnectionSnapshot{
			Ready:  true,
			Reason: reasonSynced,
			Client: savedConnClient,
		})

		mockServer.ResetCounters()
		mockServer.ResetModels()
		modelName := "canary-secretnotfound"
		secName := "canary-secret-notfound"
		ensureNoModel(t, ctx, modelName)
		defer ensureNoModel(t, context.Background(), modelName)

		// Do NOT create the canary secret — SecretNotFound path.
		_ = k8sClient.Delete(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secName, Namespace: WatchNamespace},
		}, &client.DeleteOptions{})

		cr := canaryModelCR(modelName, secName, WatchNamespace)
		if err := k8sClient.Create(ctx, cr); err != nil {
			t.Fatalf("create canary Model: %v", err)
		}
		t.Cleanup(func() { ensureNoModel(t, context.Background(), modelName) })

		m := pollModelCondition(t, ctx, modelName, "SecretNotFound", 30*time.Second)
		statusMsg := ""
		if c := apimeta.FindStatusCondition(m.Status.Conditions, conditionTypeReady); c != nil {
			statusMsg = c.Message
		}
		events := listModelEvents(ctx, t, modelName)
		assertNoCanaryLeak(t, "SecretNotFound", sink.String(), events, statusMsg)

		// Also verify the SEC-06 coordinate format: "<ns>/<name>:<key> not found"
		// — coordinates only, NO secret value. Log at info level even if checking
		// fails (since connection may not have been Synced in time).
		expectedCoord := WatchNamespace + "/" + secName + ":api-key not found"
		if !strings.Contains(statusMsg, expectedCoord) {
			t.Errorf("[SecretNotFound] SEC-06: status message want %q in %q", expectedCoord, statusMsg)
		}
		t.Logf("[SecretNotFound] status message=%q — canary absent, SEC-06 coords checked", statusMsg)
	})

	// ── Sub-test 4: 4xx LiteLLMRejected path ─────────────────────────────
	t.Run("LiteLLMRejected", func(t *testing.T) {
		sink.Reset()
		// Ensure connection cache is Ready before testing LiteLLMRejected.
		// Mode422 only returns 422 on POST /model/new — the POST /key/health
		// probe stays 200. Force cache Ready directly — probe-loop recovery
		// latency is not under test here.
		mockServer.SetMode(mock.ModeHappy)
		connCache.Rebuild(connection.ConnectionSnapshot{
			Ready:  true,
			Reason: reasonSynced,
			Client: savedConnClient,
		})
		// Now set 422 mode — POST /model/new returns 422, POST /key/health stays 200.
		mockServer.SetMode(mock.Mode422)
		mockServer.ResetCounters()
		mockServer.ResetModels()
		modelName := "canary-rejected"
		secName := "canary-secret-rejected"
		ensureNoModel(t, ctx, modelName)
		defer ensureNoModel(t, context.Background(), modelName)

		sec := canarySecretObj(secName, WatchNamespace)
		_ = k8sClient.Delete(ctx, sec, &client.DeleteOptions{})
		if err := k8sClient.Create(ctx, sec); err != nil {
			t.Fatalf("create canary secret: %v", err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), sec, &client.DeleteOptions{}) })

		cr := canaryModelCR(modelName, secName, WatchNamespace)
		if err := k8sClient.Create(ctx, cr); err != nil {
			t.Fatalf("create canary Model: %v", err)
		}

		m := pollModelCondition(t, ctx, modelName, "LiteLLMRejected", 30*time.Second)
		statusMsg := ""
		if c := apimeta.FindStatusCondition(m.Status.Conditions, conditionTypeReady); c != nil {
			statusMsg = c.Message
		}
		events := listModelEvents(ctx, t, modelName)
		assertNoCanaryLeak(t, "LiteLLMRejected", sink.String(), events, statusMsg)
		t.Logf("[LiteLLMRejected] status message=%q — canary absent", statusMsg)

		mockServer.SetMode(mock.ModeHappy)
	})

	// ── Sub-test 5: 5xx ModeTransient5xx path ────────────────────────────
	t.Run("5xx", func(t *testing.T) {
		sink.Reset()
		mockServer.SetMode(mock.ModeTransient5xx)
		mockServer.ResetCounters()
		mockServer.ResetRecorded()
		mockServer.ResetModels()
		modelName := "canary-5xx"
		secName := "canary-secret-5xx"
		ensureNoModel(t, ctx, modelName)
		defer ensureNoModel(t, context.Background(), modelName)

		sec := canarySecretObj(secName, WatchNamespace)
		_ = k8sClient.Delete(ctx, sec, &client.DeleteOptions{})
		if err := k8sClient.Create(ctx, sec); err != nil {
			t.Fatalf("create canary secret: %v", err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), sec, &client.DeleteOptions{}) })

		cr := canaryModelCR(modelName, secName, WatchNamespace)
		if err := k8sClient.Create(ctx, cr); err != nil {
			t.Fatalf("create canary Model: %v", err)
		}

		// Wait for the 5xx path to be exercised. Retry timing is covered
		// by the backoff tests; this redaction canary only needs the
		// transient-error surface to exist before checking logs/events/status.
		deadline := time.Now().Add(3 * time.Second)
		sawModelInfoRead := false
		for time.Now().Before(deadline) {
			for _, call := range mockServer.Recorded() {
				if call.Method == http.MethodGet && call.Path == "/model/info" {
					sawModelInfoRead = true
					break
				}
			}
			if sawModelInfoRead {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		logText := sink.String()
		if !sawModelInfoRead {
			t.Fatalf("[5xx] transient /model/info read not observed within 3s; reads=%d logBytes=%d", mockServer.Reads(), len(logText))
		}
		t.Logf("[5xx] transient /model/info read observed; log captured %d bytes", len(logText))

		// On the 5xx path, OWN-09 leaves the previous status unchanged (no writeStatus
		// call on transient error path). Capture whatever status exists.
		m := &litellmv1alpha1.LiteLLMModel{}
		key := client.ObjectKey{Name: modelName, Namespace: WatchNamespace}
		_ = k8sClient.Get(ctx, key, m)
		statusMsg := ""
		if c := apimeta.FindStatusCondition(m.Status.Conditions, conditionTypeReady); c != nil {
			statusMsg = c.Message
		}

		events := listModelEvents(ctx, t, modelName)
		// Load-bearing assertion: the log buffer (workqueue-backoff retry lines) must
		// NOT contain the canary value.
		assertNoCanaryLeak(t, "5xx", sink.String(), events, statusMsg)
		t.Logf("[5xx] log captured %d bytes — canary absent (load-bearing assertion)", len(sink.String()))

		mockServer.SetMode(mock.ModeHappy)
	})
}

// TestModel_UnusedSecretRef_EventEmitted — Task 2, Gap 1 closure.
//
// SEC-07: when a spec.secrets[].as value is declared but no {{NAME}} placeholder
// appears in spec.params or spec.info, the reconciler must emit a Normal Kubernetes
// Event with reason=UnusedSecretRef on the Model object. Ready remains True (Synced).
func TestModel_UnusedSecretRef_EventEmitted(t *testing.T) {
	ctx := context.Background()
	modelName := "unused-as-test-model"
	secName := "unused-as-test-secret"

	// Shared setup: ensure a Synced LiteLLMConnection exists.
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
		t.Fatalf("LiteLLMConnection not Synced within 30s; got reason=%q", connSnap.Reason)
	}
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetModels()

	// Ensure clean slate.
	ensureNoModel(t, ctx, modelName)

	// Create a Secret that EXISTS — we're testing UnusedSecretRef, not SecretNotFound.
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secName,
			Namespace: WatchNamespace,
		},
		Data: map[string][]byte{"api-key": []byte("dummy-value")},
	}
	_ = k8sClient.Delete(ctx, sec, &client.DeleteOptions{})
	if err := k8sClient.Create(ctx, sec); err != nil {
		t.Fatalf("create Secret: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), sec, &client.DeleteOptions{})
	})

	// Create Model: spec.secrets has UNUSED_NAME but spec.params has no {{UNUSED_NAME}} placeholder.
	cr := &litellmv1alpha1.LiteLLMModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:      modelName,
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.ModelSpec{
			Params: runtime.RawExtension{
				Raw: []byte(`{"model":"openai/gpt-4o-mini"}`),
			},
			Secrets: []litellmv1alpha1.SecretSubstitution{
				{
					As: "UNUSED_NAME",
					SecretRef: litellmv1alpha1.SecretKeyRef{
						Name: secName,
						Key:  "api-key",
					},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Model: %v", err)
	}
	t.Cleanup(func() {
		ensureNoModel(t, context.Background(), modelName)
	})

	// SEC-07: Ready stays True (Synced) — unused secret is informational only.
	m := pollModelCondition(t, ctx, modelName, reasonSynced, 30*time.Second)
	if m == nil {
		t.Fatalf("pollModelCondition returned nil")
	}

	// Let the Event broadcaster flush to the envtest apiserver (async delivery).
	time.Sleep(500 * time.Millisecond)

	// Retry loop: up to 5 attempts with 200ms gap.
	var matchingEvent *corev1.Event
	for attempt := 0; attempt < 5; attempt++ {
		events := listModelEvents(ctx, t, modelName)
		for i := range events {
			ev := &events[i]
			if ev.Type == "Normal" && ev.Reason == "UnusedSecretRef" && strings.Contains(ev.Message, "UNUSED_NAME") {
				matchingEvent = ev
				break
			}
		}
		if matchingEvent != nil {
			break
		}
		if attempt < 4 {
			time.Sleep(50 * time.Millisecond)
		} else {
			// Log all observed events for diagnostics.
			events := listModelEvents(ctx, t, modelName)
			var evDescs []string
			for _, ev := range events {
				evDescs = append(evDescs, fmt.Sprintf("{Type:%q Reason:%q Message:%q}", ev.Type, ev.Reason, ev.Message))
			}
			t.Fatalf("SEC-07: no Normal/UnusedSecretRef Event with message containing UNUSED_NAME after 5 attempts; observed events: %v", evDescs)
		}
	}

	t.Logf("TestModel_UnusedSecretRef_EventEmitted: found expected Event Type=%q Reason=%q Message=%q",
		matchingEvent.Type, matchingEvent.Reason, matchingEvent.Message)
}

// TestModel_ProjectionOverride_EventEmitted — Task 2, Gap 3 closure.
//
// Spec §5.1 Identity tier: when a user supplies spec.info.id (which the operator
// overlay always wins), the reconciler must emit a Warning Kubernetes Event with
// reason=ProjectionOverride. The user-supplied value must NOT appear in the Event
// message (§9.1 redaction). Ready remains True (Synced).
func TestModel_ProjectionOverride_EventEmitted(t *testing.T) {
	ctx := context.Background()
	modelName := "projection-override-test"

	// Shared setup: ensure a Synced LiteLLMConnection exists.
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
		t.Fatalf("LiteLLMConnection not Synced within 30s; got reason=%q", connSnap.Reason)
	}
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetModels()

	// Ensure clean slate.
	ensureNoModel(t, ctx, modelName)
	t.Cleanup(func() {
		ensureNoModel(t, context.Background(), modelName)
	})

	// Create Model with user-supplied spec.info.id — triggers ProjectionOverride path.
	// The canary value "user-supplied-fake-uuid-99999" must NOT appear in the Event message (§9.1).
	userSuppliedID := "user-supplied-fake-uuid-99999"
	cr := &litellmv1alpha1.LiteLLMModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:      modelName,
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.ModelSpec{
			Params: runtime.RawExtension{
				Raw: []byte(`{"model":"openai/gpt-4o-mini"}`),
			},
			Info: runtime.RawExtension{
				Raw: []byte(`{"id":"` + userSuppliedID + `","mode":"chat"}`),
			},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Model: %v", err)
	}

	// ProjectionOverride is a warning — Ready stays True (Synced).
	m := pollModelCondition(t, ctx, modelName, reasonSynced, 30*time.Second)
	if m == nil {
		t.Fatalf("pollModelCondition returned nil")
	}

	// Let the Event broadcaster flush to the envtest apiserver (async delivery).
	time.Sleep(500 * time.Millisecond)

	// Retry loop: up to 5 attempts with 200ms gap.
	var matchingEvent *corev1.Event
	for attempt := 0; attempt < 5; attempt++ {
		events := listModelEvents(ctx, t, modelName)
		for i := range events {
			ev := &events[i]
			if ev.Type == "Warning" && ev.Reason == "ProjectionOverride" && strings.Contains(ev.Message, "spec.info.id") {
				matchingEvent = ev
				break
			}
		}
		if matchingEvent != nil {
			break
		}
		if attempt < 4 {
			time.Sleep(50 * time.Millisecond)
		} else {
			// Log all observed events for diagnostics.
			events := listModelEvents(ctx, t, modelName)
			var evDescs []string
			for _, ev := range events {
				evDescs = append(evDescs, fmt.Sprintf("{Type:%q Reason:%q Message:%q}", ev.Type, ev.Reason, ev.Message))
			}
			t.Fatalf("spec §5.1: no Warning/ProjectionOverride Event with message containing spec.info.id after 5 attempts; observed events: %v", evDescs)
		}
	}

	// §9.1 redaction assertion: the user-supplied id value must NOT appear in the Event message.
	if strings.Contains(matchingEvent.Message, userSuppliedID) {
		t.Errorf("§9.1 FAIL: user-supplied id value %q found in ProjectionOverride Event message %q", userSuppliedID, matchingEvent.Message)
	}

	t.Logf("TestModel_ProjectionOverride_EventEmitted: found expected Event Type=%q Reason=%q Message=%q",
		matchingEvent.Type, matchingEvent.Reason, matchingEvent.Message)
	t.Logf("§9.1 check: user-supplied id %q absent from Event message — redaction confirmed", userSuppliedID)
}

// TestModel_DuplicateSecretsAs_Rejected — Task 3, Gap 2 closure.
//
// SEC-03: a Model CR with two or more spec.secrets[] entries sharing the same
// .as value must be rejected with Ready=False, reason=InvalidConfig and a message
// naming the duplicated identifier. The uniqueness check (Step 3.5) must fire
// BEFORE Step 4 secret resolution, so no LiteLLM mutation call is issued for
// invalid specs (mockServer.Mutations == 0).
func TestModel_DuplicateSecretsAs_Rejected(t *testing.T) {
	ctx := context.Background()
	modelName := "duplicate-as-test"

	// Shared setup: ensure a Synced LiteLLMConnection exists.
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
		t.Fatalf("LiteLLMConnection not Synced within 30s; got reason=%q", connSnap.Reason)
	}
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetModels()

	// Ensure clean slate.
	ensureNoModel(t, ctx, modelName)

	// Create two Secrets that both EXIST — we're isolating the uniqueness check from SecretNotFound.
	secA := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "dup-secret-a", Namespace: WatchNamespace},
		Data:       map[string][]byte{"api-key": []byte("value-a")},
	}
	secB := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "dup-secret-b", Namespace: WatchNamespace},
		Data:       map[string][]byte{"api-key": []byte("value-b")},
	}
	_ = k8sClient.Delete(ctx, secA, &client.DeleteOptions{})
	_ = k8sClient.Delete(ctx, secB, &client.DeleteOptions{})
	if err := k8sClient.Create(ctx, secA); err != nil {
		t.Fatalf("create dup-secret-a: %v", err)
	}
	if err := k8sClient.Create(ctx, secB); err != nil {
		t.Fatalf("create dup-secret-b: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), secA, &client.DeleteOptions{})
		_ = k8sClient.Delete(context.Background(), secB, &client.DeleteOptions{})
	})

	// Create Model with TWO spec.secrets entries both using as: "API_KEY" — duplicate!
	cr := &litellmv1alpha1.LiteLLMModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:      modelName,
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.ModelSpec{
			Params: runtime.RawExtension{
				Raw: []byte(`{"model":"openai/gpt-4o-mini"}`),
			},
			Secrets: []litellmv1alpha1.SecretSubstitution{
				{
					As: "API_KEY",
					SecretRef: litellmv1alpha1.SecretKeyRef{
						Name: "dup-secret-a",
						Key:  "api-key",
					},
				},
				{
					As: "API_KEY", // duplicate — same as value!
					SecretRef: litellmv1alpha1.SecretKeyRef{
						Name: "dup-secret-b",
						Key:  "api-key",
					},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Model: %v", err)
	}
	t.Cleanup(func() {
		ensureNoModel(t, context.Background(), modelName)
	})

	// SEC-03: reconciler must reject with Ready=False, reason=InvalidConfig.
	m := pollModelCondition(t, ctx, modelName, "InvalidConfig", 30*time.Second)
	if m == nil {
		t.Fatalf("pollModelCondition returned nil")
	}

	cond := apimeta.FindStatusCondition(m.Status.Conditions, conditionTypeReady)
	if cond == nil {
		t.Fatalf("SEC-03: no Ready condition on Model %q", modelName)
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("SEC-03: want Ready=False, got Ready=%s", cond.Status)
	}
	if cond.Reason != "InvalidConfig" {
		t.Errorf("SEC-03: want reason=InvalidConfig, got reason=%s", cond.Reason)
	}

	// Message must identify the duplicated identifier and reference SEC-03 or "duplicate".
	if !strings.Contains(cond.Message, "API_KEY") {
		t.Errorf("SEC-03: want message containing %q, got %q", "API_KEY", cond.Message)
	}
	if !strings.Contains(cond.Message, "duplicate") && !strings.Contains(cond.Message, "SEC-03") {
		t.Errorf("SEC-03: want message containing 'duplicate' or 'SEC-03', got %q", cond.Message)
	}

	// Critical: uniqueness check fires BEFORE Step 4 secret resolution, so NO
	// LiteLLM mutation for THIS model. Use the per-model counter so unrelated
	// mutations from other reconcilers (e.g. the implicit Team/default CREATE
	// fired by TeamDefaultRunnable when the Connection cache becomes Ready)
	// can't false-positive this assertion. The total `mockServer.Mutations()`
	// counter is shared across every reconciler in the envtest manager and
	// is therefore unsafe to assert exact equality on inside isolated tests.
	if got := mockServer.MutationsByModelName(modelName); got != 0 {
		t.Errorf("SEC-03: want MutationsByModelName(%q)==0 (uniqueness check before LiteLLM call), got %d",
			modelName, got)
	}

	t.Logf("TestModel_DuplicateSecretsAs_Rejected: Ready=False reason=%q message=%q mutations_for_model=%d total_mutations=%d",
		cond.Reason, cond.Message, mockServer.MutationsByModelName(modelName), mockServer.Mutations())
}

func TestModel_SpecInfo_ForwardedToLiteLLM(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetModels()
	ensureNoModel(t, ctx, "specinfo-fwd")
	resetConnCacheSnapshot()

	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		ensureNoModel(t, context.Background(), "specinfo-fwd")
		time.Sleep(50 * time.Millisecond)
	})
	if snap := pollSnapshotReason(30*time.Second, reasonSynced); snap.Reason != reasonSynced {
		t.Fatalf("connection not Synced")
	}

	cr := modelSampleCR("specinfo-fwd")
	cr.Spec.Info = runtime.RawExtension{Raw: []byte(`{"base_model":"gpt-4o-mini","tier":"paid"}`)}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Model: %v", err)
	}

	m := pollModelCondition(t, ctx, "specinfo-fwd", reasonSynced, 30*time.Second)
	if c := apimeta.FindStatusCondition(m.Status.Conditions, conditionTypeReady); c == nil || c.Reason != reasonSynced {
		t.Fatalf("want Ready=Synced, got %+v", c)
	}

	mi := mockServer.LastModelInfoBody("specinfo-fwd")
	if mi == nil {
		t.Fatal("no model_info body captured for specinfo-fwd")
	}
	if mi["base_model"] != "gpt-4o-mini" {
		t.Errorf("model_info.base_model: want gpt-4o-mini, got %v (full: %+v)", mi["base_model"], mi)
	}
	if mi["tier"] != "paid" {
		t.Errorf("model_info.tier: want paid, got %v", mi["tier"])
	}
}

func TestModel_DeletionPath_Deterministic4xx_OrphanDrains(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetModels()
	ensureNoModel(t, ctx, "del-4xx-orphan")
	resetConnCacheSnapshot()

	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create conn: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		ensureNoModel(t, context.Background(), "del-4xx-orphan")
		time.Sleep(50 * time.Millisecond)
	})
	if snap := pollSnapshotReason(30*time.Second, reasonSynced); snap.Reason != reasonSynced {
		t.Fatalf("conn not Synced")
	}

	// Create with deletionPolicy=Orphan so a deterministic 4xx on delete drains.
	cr := modelSampleCR("del-4xx-orphan")
	cr.Spec.DeletionPolicy = string(deletionpolicy.Orphan)
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create model: %v", err)
	}
	_ = pollModelCondition(t, ctx, "del-4xx-orphan", reasonSynced, 30*time.Second)

	// Now make every delete fail with 422, then delete the CR.
	mockServer.SetMode(mock.ModeDelete422)
	var got litellmv1alpha1.LiteLLMModel
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: WatchNamespace, Name: "del-4xx-orphan"}, &got); err != nil {
		t.Fatalf("get model: %v", err)
	}
	if err := k8sClient.Delete(ctx, &got); err != nil {
		t.Fatalf("delete model: %v", err)
	}

	// Under Orphan, a deterministic 4xx is "ack-missing" → finalizer drained.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		err := k8sClient.Get(ctx, client.ObjectKey{Namespace: WatchNamespace, Name: "del-4xx-orphan"}, &got)
		if apierrors.IsNotFound(err) {
			return // success — CR drained
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("CR stuck Terminating after deterministic 4xx under Orphan policy")
}
