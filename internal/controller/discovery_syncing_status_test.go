// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// recordReadyWrites returns an interceptor that appends the Ready-condition
// reason of every status update to *out (in write order). Used to prove the
// Syncing-on-entry write lands BEFORE the terminal write, since both happen
// in a single reconcile and only the last survives in the store.
func recordReadyWrites(out *[]string) interceptor.Funcs {
	return interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if sub == "status" {
				if conds := readyConditions(obj); conds != nil {
					if rc := apimeta.FindStatusCondition(conds, conditionTypeReady); rc != nil {
						*out = append(*out, rc.Reason)
					}
				}
			}
			return c.Status().Update(ctx, obj, opts...)
		},
	}
}

func readyConditions(obj client.Object) []metav1.Condition {
	switch d := obj.(type) {
	case *litellmv1alpha1.LiteLLMModelDiscovery:
		return d.Status.Conditions
	case *litellmv1alpha1.LiteLLMMCPServerDiscovery:
		return d.Status.Conditions
	default:
		return nil
	}
}

// TestModelDiscovery_SyncingOnEntry: a fresh CR's first Ready write must be
// False/Syncing (before credential/list work), and an already-Synced CR of
// the same generation must NOT flap back to Syncing on a re-reconcile.
func TestModelDiscovery_SyncingOnEntry(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := litellmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil { // Secret resolution in Step 4
		t.Fatalf("corev1 AddToScheme: %v", err)
	}
	newMD := func() *litellmv1alpha1.LiteLLMModelDiscovery {
		return &litellmv1alpha1.LiteLLMModelDiscovery{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "prov",
				Namespace:  "default",
				Generation: 1,
				Finalizers: []string{modelDiscoveryFinalizer},
			},
			// anthropic + a ref to a Secret that does not exist → the reconcile
			// reaches SecretNotFound right after the Syncing-on-entry write.
			Spec: litellmv1alpha1.ModelDiscoverySpec{
				Type:                 providerTypeAnthropic,
				CredentialsSecretRef: &litellmv1alpha1.SecretObjectRef{Name: "missing"},
			},
		}
	}

	t.Run("fresh CR writes Syncing first", func(t *testing.T) {
		var writes []string
		md := newMD()
		cli := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(md).
			WithStatusSubresource(&litellmv1alpha1.LiteLLMModelDiscovery{}).
			WithInterceptorFuncs(recordReadyWrites(&writes)).
			Build()
		r := &ModelDiscoveryReconciler{Client: cli, Scheme: scheme, Recorder: record.NewFakeRecorder(8), Namespace: "default", Log: ctrl.Log.WithName("test")}
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "prov", Namespace: "default"}}); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if len(writes) == 0 || writes[0] != reasonSyncing {
			t.Fatalf("first Ready write must be %q; got sequence %v", reasonSyncing, writes)
		}
	})

	t.Run("already-Synced CR does not flap to Syncing", func(t *testing.T) {
		var writes []string
		md := newMD()
		apimeta.SetStatusCondition(&md.Status.Conditions, metav1.Condition{
			Type: conditionTypeReady, Status: metav1.ConditionTrue, Reason: reasonSynced,
			ObservedGeneration: 1,
		})
		md.Status.ObservedGeneration = 1 // == Generation → guard must skip Syncing
		cli := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(md).
			WithStatusSubresource(&litellmv1alpha1.LiteLLMModelDiscovery{}).
			WithInterceptorFuncs(recordReadyWrites(&writes)).
			Build()
		r := &ModelDiscoveryReconciler{Client: cli, Scheme: scheme, Recorder: record.NewFakeRecorder(8), Namespace: "default", Log: ctrl.Log.WithName("test")}
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "prov", Namespace: "default"}}); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		for _, w := range writes {
			if w == reasonSyncing {
				t.Fatalf("already-Synced CR (matching generation) must not flap to Syncing; got %v", writes)
			}
		}
	})
}

// TestMCPServerDiscovery_SyncingOnEntry mirrors the model test: fresh CR's
// first Ready write is Syncing; an already-Synced CR does not flap.
func TestMCPServerDiscovery_SyncingOnEntry(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := litellmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	newMD := func() *litellmv1alpha1.LiteLLMMCPServerDiscovery {
		return &litellmv1alpha1.LiteLLMMCPServerDiscovery{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "mcp-prov",
				Namespace:  "default",
				Generation: 1,
				Finalizers: []string{mcpServerDiscoveryFinalizer},
			},
		}
	}

	t.Run("fresh CR writes Syncing first", func(t *testing.T) {
		var writes []string
		md := newMD()
		cli := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(md).
			WithStatusSubresource(&litellmv1alpha1.LiteLLMMCPServerDiscovery{}).
			WithInterceptorFuncs(recordReadyWrites(&writes)).
			Build()
		// nil ToolHiveInformer → source gate writes SourceUnreachable after
		// the Syncing-on-entry write.
		r := &MCPServerDiscoveryReconciler{Client: cli, Scheme: scheme, Recorder: record.NewFakeRecorder(8), Namespace: "default", Log: ctrl.Log.WithName("test")}
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "mcp-prov", Namespace: "default"}}); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if len(writes) == 0 || writes[0] != reasonSyncing {
			t.Fatalf("first Ready write must be %q; got sequence %v", reasonSyncing, writes)
		}
	})

	t.Run("already-Synced CR does not flap to Syncing", func(t *testing.T) {
		var writes []string
		md := newMD()
		apimeta.SetStatusCondition(&md.Status.Conditions, metav1.Condition{
			Type: conditionTypeReady, Status: metav1.ConditionTrue, Reason: reasonSynced,
			ObservedGeneration: 1,
		})
		md.Status.ObservedGeneration = 1
		cli := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(md).
			WithStatusSubresource(&litellmv1alpha1.LiteLLMMCPServerDiscovery{}).
			WithInterceptorFuncs(recordReadyWrites(&writes)).
			Build()
		r := &MCPServerDiscoveryReconciler{Client: cli, Scheme: scheme, Recorder: record.NewFakeRecorder(8), Namespace: "default", Log: ctrl.Log.WithName("test")}
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "mcp-prov", Namespace: "default"}}); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		for _, w := range writes {
			if w == reasonSyncing {
				t.Fatalf("already-Synced CR (matching generation) must not flap to Syncing; got %v", writes)
			}
		}
	})
}
