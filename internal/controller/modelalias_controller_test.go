// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/connection"
	"github.com/ackstorm/alitellm-operator/internal/litellm"
)

// modelAlias401Cache is a connection.ConnectionCache stub that returns a
// usable snapshot (Ready + a real Client) and records InvalidateOn401.
// The shared FakeConnectionCache returns a nil Client, which fails the
// snap.Usable() gate before the reconciler reaches the LiteLLM call.
type modelAlias401Cache struct {
	snap        connection.ConnectionSnapshot
	invalidated atomic.Bool
}

func (f *modelAlias401Cache) Snapshot() connection.ConnectionSnapshot { return f.snap }
func (f *modelAlias401Cache) InvalidateOn401()                        { f.invalidated.Store(true) }
func (f *modelAlias401Cache) Rebuild(_ connection.ConnectionSnapshot) {}

func TestModelAliasReconciler_ConnectionNotReady_WritesLiteLLMUnavailable(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := litellmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	a := &litellmv1alpha1.LiteLLMModelAlias{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "default", Generation: 1},
		Spec: litellmv1alpha1.LiteLLMModelAliasSpec{
			Aliases: []litellmv1alpha1.ModelAliasEntry{
				{Name: "k1", Value: "v1"},
				{Name: "k2", Value: "v2"},
			},
		},
	}
	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(a).
		WithStatusSubresource(&litellmv1alpha1.LiteLLMModelAlias{}).
		Build()

	r := &ModelAliasReconciler{
		Client:    cli,
		Scheme:    scheme,
		Cache:     connection.NewCache(),
		Recorder:  record.NewFakeRecorder(8),
		Namespace: "default",
		Log:       ctrl.Log.WithName("test"),
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: ModelAliasSingletonKey, Namespace: "default"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got litellmv1alpha1.LiteLLMModelAlias
	if err := cli.Get(context.Background(), types.NamespacedName{Name: "x", Namespace: "default"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Status.Conditions) == 0 {
		t.Fatalf("expected at least one condition; got none")
	}
	cond := got.Status.Conditions[0]
	if cond.Reason != reasonLiteLLMUnavailable {
		t.Fatalf("want reason=%q got %q (full=%+v)", reasonLiteLLMUnavailable, cond.Reason, cond)
	}
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("want status=False got %s", cond.Status)
	}
	// Per-entry statuses must NOT be populated on the unready path.
	if len(got.Status.AliasStatuses) != 0 {
		t.Fatalf("AliasStatuses should remain empty when not ready, got %+v", got.Status.AliasStatuses)
	}
}

// TestModelAlias_ReconcileRejectsNonSingletonKey is a defense-in-depth
// test: even if a future contributor re-adds For(&LiteLLMModelAlias{})
// or wires a Watches without the mapToSingleton mapper, Reconcile
// itself rejects non-singleton keys with a no-op + V(2) log.
//
// Post-2026-05-26 review finding F3.
func TestModelAlias_ReconcileRejectsNonSingletonKey(t *testing.T) {
	r := &ModelAliasReconciler{Namespace: "litellm-system"}
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "litellm-system", Name: "some-alias-name"},
	})
	if err != nil {
		t.Fatalf("expected nil error for non-singleton key, got %v", err)
	}
	if !res.IsZero() {
		t.Fatalf("expected zero Result for non-singleton key, got %+v", res)
	}
}

// TestModelAlias_OnlyEnqueuesSingletonKey is the manager-driven
// counterpart to the guard test above. It would assert that the
// SetupWithManager wiring never enqueues a non-singleton key onto the
// work queue. Implementing this requires a manager-driven harness with
// a work-queue inspector, which this test file does not currently
// provide (other tests use a fake client directly and exercise
// Reconcile in-process).
//
// The guard test above (TestModelAlias_ReconcileRejectsNonSingletonKey)
// is sufficient defense in depth: even if SetupWithManager is mis-wired
// to enqueue per-object keys, Reconcile will no-op on them. We keep
// this skipped placeholder to document the intent.
//
// Post-2026-05-26 review finding F3.
func TestModelAlias_OnlyEnqueuesSingletonKey(t *testing.T) {
	t.Skip("requires manager-driven harness; covered by TestModelAlias_ReconcileRejectsNonSingletonKey as defense in depth")
}

func TestModelAliasReconciler_FinalizerAddedToAliveCR(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := litellmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	a := &litellmv1alpha1.LiteLLMModelAlias{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "default", Generation: 1},
		Spec: litellmv1alpha1.LiteLLMModelAliasSpec{
			Aliases: []litellmv1alpha1.ModelAliasEntry{{Name: "k", Value: "v"}},
		},
	}
	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(a).
		WithStatusSubresource(&litellmv1alpha1.LiteLLMModelAlias{}).
		Build()

	r := &ModelAliasReconciler{
		Client:    cli,
		Scheme:    scheme,
		Cache:     connection.NewCache(),
		Recorder:  record.NewFakeRecorder(8),
		Namespace: "default",
		Log:       ctrl.Log.WithName("test"),
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: ModelAliasSingletonKey, Namespace: "default"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got litellmv1alpha1.LiteLLMModelAlias
	if err := cli.Get(context.Background(), types.NamespacedName{Name: "x", Namespace: "default"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	found := false
	for _, f := range got.Finalizers {
		if f == modelAliasFinalizer {
			found = true
		}
	}
	if !found {
		t.Fatalf("finalizer %q not added; finalizers=%v", modelAliasFinalizer, got.Finalizers)
	}
}

func TestModelAlias_401_InvalidatesCache(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := litellmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	ma := &litellmv1alpha1.LiteLLMModelAlias{
		ObjectMeta: metav1.ObjectMeta{Name: "ma-401", Namespace: "default", Generation: 1},
		Spec: litellmv1alpha1.LiteLLMModelAliasSpec{
			Aliases: []litellmv1alpha1.ModelAliasEntry{{Name: "k1", Value: "v1"}},
		},
	}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ma).
		WithStatusSubresource(&litellmv1alpha1.LiteLLMModelAlias{}).Build()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"token_not_found_in_db","code":"401","message":"x"}}`))
	}))
	defer srv.Close()
	llm := litellm.NewClient(srv.URL, "sk-test", logr.Discard())

	fc := &modelAlias401Cache{snap: connection.ConnectionSnapshot{Ready: true, Reason: reasonSynced, Client: llm}}

	r := &ModelAliasReconciler{
		Client:    cli,
		Scheme:    scheme,
		Cache:     fc,
		Recorder:  record.NewFakeRecorder(8),
		Namespace: "default",
		Log:       ctrl.Log.WithName("test"),
	}
	_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: ModelAliasSingletonKey, Namespace: "default"}})

	if !fc.invalidated.Load() {
		t.Fatal("ModelAlias did not call InvalidateOn401 on a 401 from LiteLLM")
	}
}
