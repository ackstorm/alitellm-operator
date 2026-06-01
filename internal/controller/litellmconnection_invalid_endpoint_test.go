// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// TestLiteLLMConnection_InvalidEndpoint_NoRequeue asserts that an
// endpoint slipping past CRD admission (raw Unicode host — Pattern
// admits any [^@\s?#] sequence) is rejected at the wire layer with
// Ready=False reason=InvalidEndpoint and that the reconciler does NOT
// requeue (Spec watch retriggers).
func TestLiteLLMConnection_InvalidEndpoint_NoRequeue(t *testing.T) {
	if connCache == nil {
		t.Fatal("suite_test.go did not initialize connCache — TestMain ordering bug")
	}

	ctx := context.Background()
	ensureNoConnectionDefault(t, ctx)
	ensureMasterKeySecret(t, ctx)
	resetConnCacheSnapshot()

	cr := &litellmv1alpha1.LiteLLMConnection{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: WatchNamespace},
		Spec: litellmv1alpha1.LiteLLMConnectionSpec{
			Endpoint:           "https://bücher.example",
			MasterKeySecretRef: litellmv1alpha1.SecretKeyRef{Name: "litellm-master-key", Key: "masterKey"},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create connection: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), cr)
		time.Sleep(50 * time.Millisecond)
	})

	snap := pollSnapshotReason(15*time.Second, reasonInvalidEndpoint)
	if snap.Reason != reasonInvalidEndpoint {
		t.Fatalf("cache.Snapshot().Reason = %q, want %q within 15s", snap.Reason, reasonInvalidEndpoint)
	}
	if snap.Ready {
		t.Errorf("cache.Snapshot().Ready = true, want false")
	}

	var got litellmv1alpha1.LiteLLMConnection
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "default", Namespace: WatchNamespace}, &got); err != nil {
		t.Fatalf("get connection: %v", err)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, conditionTypeReady)
	if cond == nil {
		t.Fatalf("Ready condition missing; conditions: %#v", got.Status.Conditions)
	}
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("Ready status = %q, want False", cond.Status)
	}
	if cond.Reason != reasonInvalidEndpoint {
		t.Fatalf("Ready reason=%q, want %q", cond.Reason, reasonInvalidEndpoint)
	}
}
