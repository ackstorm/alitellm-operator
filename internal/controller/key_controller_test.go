// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

const (
	keyPollTimeout = 30 * time.Second
	// keySecretNS is outside WatchNamespace on purpose: the key Secret is
	// written to the service's namespace, which the manager cache never sees.
	keySecretNS = "key-secrets"
)

func keyCR(name string) *litellmv1alpha1.LiteLLMKey {
	limit, rpm := 5.0, int32(60)
	return &litellmv1alpha1.LiteLLMKey{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: WatchNamespace},
		Spec: litellmv1alpha1.KeySpec{
			Models:     []string{"m-b", "m-a"},
			Budget:     &litellmv1alpha1.BudgetSpec{Limit: &limit, Period: "30d"},
			RateLimits: &litellmv1alpha1.RateLimitsSpec{RPM: &rpm},
			SecretRef:  litellmv1alpha1.KeySecretRef{Namespace: keySecretNS, Name: name + "-secret", Key: "api-key"},
		},
	}
}

// ensureNoKey removes a LiteLLMKey and its Secret whatever state they are in.
func ensureNoKey(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	// Last, so a reconcile racing the CR delete cannot leave a Secret behind.
	defer func() {
		_ = k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name + "-secret", Namespace: keySecretNS}})
	}()
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	var k litellmv1alpha1.LiteLLMKey
	if err := k8sClient.Get(ctx, key, &k); apierrors.IsNotFound(err) {
		return
	}
	k.Finalizers = nil
	_ = k8sClient.Update(ctx, &k)
	_ = k8sClient.Delete(ctx, &k)
	deadline := time.Now().Add(keyPollTimeout)
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, key, &k); apierrors.IsNotFound(err) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("key %s not removed", name)
}

func TestKey_CRDValidation(t *testing.T) {
	ctx := context.Background()
	ensureNoKey(t, ctx, "key-validation")
	t.Cleanup(func() { ensureNoKey(t, context.Background(), "key-validation") })

	noModels := keyCR("key-validation")
	noModels.Spec.Models = nil
	if err := k8sClient.Create(ctx, noModels); !apierrors.IsInvalid(err) {
		t.Fatalf("empty models must be rejected, got %v", err)
	}

	ok := keyCR("key-validation")
	if err := k8sClient.Create(ctx, ok); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(keyPollTimeout)
	for {
		var k litellmv1alpha1.LiteLLMKey
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(ok), &k); err != nil {
			t.Fatal(err)
		}
		k.Spec.SecretRef.Name = "elsewhere"
		err := k8sClient.Update(ctx, &k)
		if apierrors.IsInvalid(err) {
			return
		}
		if !apierrors.IsConflict(err) || time.Now().After(deadline) {
			t.Fatalf("secretRef change must be rejected as invalid, got %v", err)
		}
	}
}
