// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/connection"
	"github.com/ackstorm/alitellm-operator/internal/controller/deletionpolicy"
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
)

// TestModelDiscoveryChildForcedOrphan — Issue #23 envtest.
//
// A Model CR owned by a LiteLLMModelDiscovery parent (controller:true)
// must always resolve to Orphan regardless of spec.deletionPolicy=Delete
// or annotation override=Delete. Otherwise vanish-detection on the
// Discovery side could deadlock waiting for a stuck child's LiteLLM ack.
//
// The test creates the Model directly with a synthetic ownerReference
// (no live Discovery parent needed) so it exercises only the resolver
// branch — independent of the Discovery reconciler's plumbing.
func TestModelDiscoveryChildForcedOrphan(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetModels()

	const name = "discovery-child-forced-orphan"
	ensureNoModel(t, ctx, name)
	resetConnCacheSnapshot()

	// Connection Ready so the finalizer-add path runs cleanly.
	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	t.Cleanup(func() {
		ensureNoConnectionDefault(t, context.Background())
	})

	ctrlTrue := true
	cr := &litellmv1alpha1.LiteLLMModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: WatchNamespace,
			Annotations: map[string]string{
				// Annotation says Delete — resolver MUST still force Orphan
				// because of the Discovery owner.
				deletionpolicy.AnnotationOverride: string(deletionpolicy.Delete),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: litellmv1alpha1.GroupVersion.String(),
				Kind:       "LiteLLMModelDiscovery",
				Name:       "synthetic-parent",
				// Stable synthetic UID. The reconciler only inspects
				// ref.Controller + ref.Kind — UID validity does not matter
				// for this code path.
				UID:        types.UID("00000000-0000-0000-0000-000000000023"),
				Controller: &ctrlTrue,
			}},
		},
		Spec: litellmv1alpha1.ModelSpec{
			// Spec also says Delete — Discovery rule must still win.
			DeletionPolicy: string(deletionpolicy.Delete),
			Params: runtime.RawExtension{
				Raw: []byte(`{"model":"openai/gpt-4o-mini","rpm":100}`),
			},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create child Model: %v", err)
	}
	t.Cleanup(func() {
		// Restore a VALID Ready snapshot (Client-backed) so the finalizer
		// can drain. A Ready+nil-Client snapshot poisons the shared
		// singleton and panics the next reconcile (issue #74).
		setConnCacheReady()
		ensureNoModel(t, context.Background(), name)
	})

	// Wait for finalizer.
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	var got litellmv1alpha1.LiteLLMModel
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, key, &got); err == nil {
			for _, f := range got.Finalizers {
				if f == modelFinalizer {
					goto FINALIZED
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("finalizer never added")
FINALIZED:

	// Flip cache NotReady so the deletion path hits ack-missing.
	connCache.Rebuild(connection.ConnectionSnapshot{Ready: false, Reason: "Unreachable"})

	// Delete the child.
	if err := k8sClient.Delete(ctx, &got); err != nil {
		t.Fatalf("delete child: %v", err)
	}

	// Despite spec=Delete + annotation=Delete, the Discovery owner forces
	// Orphan — finalizer must drain.
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, key, &got); apierrors.IsNotFound(err) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("Discovery-owned child did not drain within 10s — resolver did NOT force Orphan (Issue #23 regression)")
}
