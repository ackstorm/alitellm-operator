// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// TestModelDiscovery_DrainSetsDeletingStatus locks the fix for the
// "deleting a large discovery still reads Ready=True/Synced" report: while
// the finalizer waits for owned children to drain, one reconcile must flip
// the parent's Ready condition to False/Deleting (and requeue). The
// envtest cascade test drains children in milliseconds, so this uses a fake
// client with a child that keeps its finalizer — the drain window stays
// open and the assertion is deterministic.
func TestModelDiscovery_DrainSetsDeletingStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := litellmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	dt := metav1.Now()
	md := &litellmv1alpha1.LiteLLMModelDiscovery{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "big-provider",
			Namespace:         "default",
			Generation:        1,
			Finalizers:        []string{modelDiscoveryFinalizer},
			DeletionTimestamp: &dt,
		},
	}
	// Pre-set the stale Ready=True/Synced the CR held before deletion, so
	// the test proves the transition, not just an initial write.
	apimeta.SetStatusCondition(&md.Status.Conditions, metav1.Condition{
		Type: conditionTypeReady, Status: metav1.ConditionTrue, Reason: reasonSynced,
	})

	// One owned child that will NOT drain: it carries a finalizer, so the
	// reconciler's r.Delete only sets its DeletionTimestamp and it stays in
	// the list — keeping the parent in the drain-wait branch.
	child := &litellmv1alpha1.LiteLLMModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "big-provider.model-0001",
			Namespace:  "default",
			Labels:     map[string]string{generatedByLabel: md.Name},
			Finalizers: []string{"test/keep"},
		},
	}

	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(md, child).
		WithStatusSubresource(&litellmv1alpha1.LiteLLMModelDiscovery{}).
		Build()

	r := &ModelDiscoveryReconciler{
		Client:    cli,
		Scheme:    scheme,
		Recorder:  record.NewFakeRecorder(8),
		Namespace: "default",
		Log:       ctrl.Log.WithName("test"),
	}

	res, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Name: md.Name, Namespace: "default"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 5*time.Second {
		t.Errorf("drain-wait must requeue at 5s; got RequeueAfter=%v", res.RequeueAfter)
	}

	var got litellmv1alpha1.LiteLLMModelDiscovery
	if err := cli.Get(context.Background(), types.NamespacedName{Name: md.Name, Namespace: "default"}, &got); err != nil {
		t.Fatalf("get parent: %v", err)
	}
	c := apimeta.FindStatusCondition(got.Status.Conditions, conditionTypeReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != reasonDeleting {
		t.Fatalf("parent Ready must be False/Deleting while children drain; got %+v", c)
	}
}
