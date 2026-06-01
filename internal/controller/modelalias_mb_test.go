// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/connection"
)

func mbModelAliasScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := litellmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return scheme
}

// TestModelAliasReconciler_RejectionRequeues is the M-B2 regression: the
// rejection / connection-not-ready path went through broadcastNotReady which
// returned ctrl.Result{} with NO RequeueAfter, so a transient failure stalled
// until the 15m resync. It must now requeue with a bounded interval.
func TestModelAliasReconciler_RejectionRequeues(t *testing.T) {
	scheme := mbModelAliasScheme(t)
	a := &litellmv1alpha1.LiteLLMModelAlias{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "default", Generation: 1},
		Spec: litellmv1alpha1.LiteLLMModelAliasSpec{
			Aliases: []litellmv1alpha1.ModelAliasEntry{{Name: "k1", Value: "v1"}},
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
		Cache:     connection.NewCache(zap.New()), // default snapshot: not Ready
		Recorder:  record.NewFakeRecorder(8),
		Namespace: "default",
		Log:       ctrl.Log.WithName("test"),
	}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: ModelAliasSingletonKey, Namespace: "default"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Fatalf("M-B2: rejection path must requeue with a bounded interval; got RequeueAfter=%v", res.RequeueAfter)
	}
}

// TestModelAlias_ApplyStatus_UsesFreshGeneration is the M-B1 regression:
// applyStatus set ObservedGeneration from the stale list-snapshot copy
// (item.Generation) instead of the object it actually re-read
// (fresh.Generation).
func TestModelAlias_ApplyStatus_UsesFreshGeneration(t *testing.T) {
	scheme := mbModelAliasScheme(t)
	live := &litellmv1alpha1.LiteLLMModelAlias{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "default", Generation: 1},
		Spec: litellmv1alpha1.LiteLLMModelAliasSpec{
			Aliases: []litellmv1alpha1.ModelAliasEntry{{Name: "k1", Value: "v1"}},
		},
	}
	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(live).
		WithStatusSubresource(&litellmv1alpha1.LiteLLMModelAlias{}).
		Build()
	r := &ModelAliasReconciler{Client: cli, Scheme: scheme, Log: ctrl.Log.WithName("test")}

	// Stale list-snapshot copy carrying a deliberately wrong Generation.
	stale := *live.DeepCopy()
	stale.Generation = 999
	cond := metav1.Condition{
		Type:    conditionTypeReady,
		Status:  metav1.ConditionTrue,
		Reason:  "Test",
		Message: "m",
	}
	if err := r.applyStatus(context.Background(), stale, cond, nil); err != nil {
		t.Fatalf("applyStatus: %v", err)
	}
	var got litellmv1alpha1.LiteLLMModelAlias
	if err := cli.Get(context.Background(), types.NamespacedName{Name: "x", Namespace: "default"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.ObservedGeneration != 1 {
		t.Fatalf("M-B1: want observedGeneration=1 (fresh.Generation), got %d (stale was 999)", got.Status.ObservedGeneration)
	}
}
