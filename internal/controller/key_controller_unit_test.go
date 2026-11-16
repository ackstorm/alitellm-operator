// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/litellm"
)

func keyUnitScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(litellmv1alpha1.AddToScheme(s))
	return s
}

func TestKeyInSync(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	i32 := func(v int32) *int32 { return &v }
	i64 := func(v int64) *int64 { return &v }
	str := func(v string) *string { return &v }
	spec := &litellmv1alpha1.KeySpec{
		Models:     []string{"b", "a"},
		Budget:     &litellmv1alpha1.BudgetSpec{Limit: f(5), Period: keyTestPeriod},
		RateLimits: &litellmv1alpha1.RateLimitsSpec{RPM: i32(60)},
	}
	same := &litellm.KeyInfo{Models: []string{"a", "b"}, MaxBudget: f(5), BudgetDuration: str(keyTestPeriod), RPMLimit: i64(60)}
	if !keyInSync(spec, same) {
		t.Error("model order must not count as drift")
	}
	for name, got := range map[string]*litellm.KeyInfo{
		"models":    {Models: []string{"a"}, MaxBudget: f(5), BudgetDuration: str(keyTestPeriod), RPMLimit: i64(60)},
		"budget":    {Models: []string{"a", "b"}, MaxBudget: f(6), BudgetDuration: str(keyTestPeriod), RPMLimit: i64(60)},
		"period":    {Models: []string{"a", "b"}, MaxBudget: f(5), RPMLimit: i64(60)},
		"rpm":       {Models: []string{"a", "b"}, MaxBudget: f(5), BudgetDuration: str(keyTestPeriod)},
		"extra tpm": {Models: []string{"a", "b"}, MaxBudget: f(5), BudgetDuration: str(keyTestPeriod), RPMLimit: i64(60), TPMLimit: i64(1)},
	} {
		if keyInSync(spec, got) {
			t.Errorf("%s drift not detected", name)
		}
	}
	if !keyInSync(&litellmv1alpha1.KeySpec{Models: []string{"a"}}, &litellm.KeyInfo{Models: []string{"a"}}) {
		t.Error("nil budget and limits must match a key with none")
	}
}

func TestKey_EnsureSecret_Forbidden(t *testing.T) {
	denied := interceptor.NewClient(fake.NewClientBuilder().WithScheme(keyUnitScheme()).Build(), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, key.Name, errors.New("denied"))
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	r := &KeyReconciler{Secrets: denied}
	value, reason, _, err := r.ensureSecret(context.Background(), keyCR("key-forbidden"))
	if err != nil || value != "" || reason != reasonSecretAccessDenied {
		t.Fatalf("value set=%v reason=%q err=%v", value != "", reason, err)
	}
}

func TestKey_Finalize_LiteLLMUnavailable_KeepsFinalizer(t *testing.T) {
	for _, policy := range []string{"Delete", "Orphan"} {
		t.Run(policy, func(t *testing.T) {
			cr := keyCR("key-outage")
			cr.Spec.DeletionPolicy = policy
			controllerutil.AddFinalizer(cr, keyFinalizer)
			c := fake.NewClientBuilder().WithScheme(keyUnitScheme()).WithObjects(cr).Build()
			cache := NewFakeConnectionCache()
			cache.Invalidated.Store(true) // snapshot not Usable, and not Absent
			r := &KeyReconciler{Client: c, Secrets: c, Cache: cache, Recorder: record.NewFakeRecorder(10)}

			if err := r.finalize(context.Background(), log.Log, cr); err == nil {
				t.Fatal("finalize must fail while LiteLLM is unavailable")
			}
			var after litellmv1alpha1.LiteLLMKey
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(cr), &after); err != nil {
				t.Fatal(err)
			}
			if !controllerutil.ContainsFinalizer(&after, keyFinalizer) {
				t.Error("finalizer dropped during an outage")
			}
		})
	}
}
