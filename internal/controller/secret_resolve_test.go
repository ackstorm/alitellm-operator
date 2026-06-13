// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

func secretResolveScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme: %v", err)
	}
	if err := litellmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return scheme
}

func sub(name, key, as string) litellmv1alpha1.SecretSubstitution {
	return litellmv1alpha1.SecretSubstitution{
		SecretRef: litellmv1alpha1.SecretKeyRef{Name: name, Key: key},
		As:        as,
	}
}

func TestResolveSecretMap(t *testing.T) {
	scheme := secretResolveScheme(t)
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"},
		Data:       map[string][]byte{"k": []byte("v")},
	}

	t.Run("success", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sec).Build()
		m, miss, err := resolveSecretMap(context.Background(), c, "ns", []litellmv1alpha1.SecretSubstitution{sub("s", "k", "VAR")})
		if err != nil || miss != "" {
			t.Fatalf("want success; got miss=%q err=%v", miss, err)
		}
		if m["VAR"] != "v" {
			t.Fatalf("want VAR=v; got %v", m)
		}
	})

	t.Run("missing secret", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		m, miss, err := resolveSecretMap(context.Background(), c, "ns", []litellmv1alpha1.SecretSubstitution{sub("absent", "k", "MISSING_SECRET")})
		if err != nil || m != nil {
			t.Fatalf("want soft miss; got m=%v err=%v", m, err)
		}
		if miss != "ns/absent:k not found" {
			t.Fatalf("unexpected miss message: %q", miss)
		}
	})

	t.Run("missing key", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sec).Build()
		m, miss, err := resolveSecretMap(context.Background(), c, "ns", []litellmv1alpha1.SecretSubstitution{sub("s", "absent", "MISSING_KEY")})
		if err != nil || m != nil {
			t.Fatalf("want soft miss; got m=%v err=%v", m, err)
		}
		if miss != "ns/s:absent not found" {
			t.Fatalf("unexpected miss message: %q", miss)
		}
	})

	t.Run("transient error", func(t *testing.T) {
		boom := errors.New("boom")
		c := fake.NewClientBuilder().WithScheme(scheme).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
					return boom
				},
			}).Build()
		m, miss, err := resolveSecretMap(context.Background(), c, "ns", []litellmv1alpha1.SecretSubstitution{sub("s", "k", "VAR")})
		if !errors.Is(err, boom) || m != nil || miss != "" {
			t.Fatalf("want transient error propagated; got m=%v miss=%q err=%v", m, miss, err)
		}
	})
}

func TestResolveStringKey_TransientError_NotMissing(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	boom := errors.New("apiserver throttled")
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return boom
			},
		}).Build()

	r := &ModelDiscoveryReconciler{Client: c, Scheme: scheme}
	ref := &litellmv1alpha1.SecretObjectRef{Name: "creds"}
	_, missing, err := r.resolveStringKey(context.Background(), WatchNamespace, ref, "ANTHROPIC_API_KEY")
	if err == nil {
		t.Fatal("expected transient error to be returned")
	}
	if missing {
		t.Error("transient error must NOT be classified as missing (SecretNotFound)")
	}
}

func TestResolveStringKey_NotFound_IsMissing(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build() // no secret → IsNotFound
	r := &ModelDiscoveryReconciler{Client: c, Scheme: scheme}
	ref := &litellmv1alpha1.SecretObjectRef{Name: "creds"}
	_, missing, err := r.resolveStringKey(context.Background(), WatchNamespace, ref, "ANTHROPIC_API_KEY")
	if !missing {
		t.Errorf("a genuinely-absent secret must be missing=true (err=%v)", err)
	}
}
