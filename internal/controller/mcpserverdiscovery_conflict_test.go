// SPDX-License-Identifier: Apache-2.0

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
	"github.com/ackstorm/alitellm-operator/internal/toolhive"
)

// TestMCPServerDiscoveryReconciler_AlphaLastWins_IntraDiscovery pins the
// alpha-last-wins conflict-resolution policy for the intra-discovery
// surface: when two upstream ToolHive objects in DIFFERENT namespaces
// share the same metadata.name within a single MCPServerDiscovery, the
// occurrence with the alpha-LAST `<namespace>/<name>` key must win;
// earlier occurrences are skipped with Reason=NameCollision.
//
// Setup: discovery `alw-disc` consumes namespaces [ns-a, ns-b]. Both
// namespaces hold a ToolHive MCPServer named `shared-name`. Under
// alpha-last-wins (sorting by `(namespace, name)` ASC and picking the
// LAST), the survivor is `ns-b/shared-name`; the skipped entry points
// at `ns-a/shared-name`.
//
// Pre-flip behavior: the FIRST entry (`ns-a/shared-name`) wins and
// `ns-b/shared-name` is skipped — this test fails with that policy.
func TestMCPServerDiscoveryReconciler_AlphaLastWins_IntraDiscovery(t *testing.T) {
	ctx := context.Background()
	const mdName = "alw-intra-disc"
	const sharedSourceName = "shared-name"
	const nsA = "alw-ns-a"
	const nsB = "alw-ns-b"

	ensureNoMCPServerDiscovery(t, ctx, mdName)
	for _, ns := range []string{nsA, nsB} {
		if err := k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create namespace %s: %v", ns, err)
		}
	}
	ensureNoToolhiveObject(t, ctx, toolhive.MCPServerGVK, nsA, sharedSourceName)
	ensureNoToolhiveObject(t, ctx, toolhive.MCPServerGVK, nsB, sharedSourceName)
	t.Cleanup(func() {
		ensureNoMCPServerDiscovery(t, context.Background(), mdName)
		ensureNoToolhiveObject(t, context.Background(), toolhive.MCPServerGVK, nsA, sharedSourceName)
		ensureNoToolhiveObject(t, context.Background(), toolhive.MCPServerGVK, nsB, sharedSourceName)
	})

	// Two upstream objects with the SAME metadata.name in different
	// namespaces. Different status.url so we can detect which one was
	// chosen via the child's spec.endpoint.
	createToolhiveMCPServer(t, ctx, nsA, sharedSourceName, "https://ns-a.example.com", "http")
	createToolhiveMCPServer(t, ctx, nsB, sharedSourceName, "https://ns-b.example.com", "http")

	md := msDiscSampleCR(mdName, []string{nsA, nsB})
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("create MCPServerDiscovery: %v", err)
	}

	// Wait for the single surviving child to land.
	children := pollMCPServerDiscoveryChildren(t, ctx, mdName, 1, 30*time.Second)
	if len(children) != 1 {
		t.Fatalf("expected exactly 1 child after intra-discovery collision, got %d", len(children))
	}
	wantChildName := mdName + "-" + sharedSourceName
	if children[0].Name != wantChildName {
		t.Errorf("child name: got %q, want %q", children[0].Name, wantChildName)
	}

	// Alpha-last-wins: the child must carry the spec.endpoint of the
	// alpha-LAST source (ns-b/shared-name → https://ns-b.example.com).
	// Under the pre-flip first-wins policy this would be ns-a's URL.
	const wantEndpoint = "https://ns-b.example.com"
	if got := children[0].Spec.Endpoint; got != wantEndpoint {
		t.Errorf("alpha-last-wins: child spec.endpoint got %q, want %q (alpha-FIRST ns-a would emit https://ns-a.example.com)",
			got, wantEndpoint)
	}

	// The skipped entry must point at the LOSER (ns-a/shared-name)
	// under alpha-last-wins. Under first-wins the skipped OwnedBy would
	// be "ns-b/shared-name".
	skip := pollMCPServerDiscoverySkipReason(t, ctx, mdName, "NameCollision", 10*time.Second)
	if skip == nil {
		t.Fatalf("no skippedCandidate with Reason=NameCollision recorded")
	}
	const wantOwnedBy = nsA + "/" + sharedSourceName
	if skip.OwnedBy != wantOwnedBy {
		t.Errorf("alpha-last-wins: skipped candidate OwnedBy got %q, want %q (the loser must be the alpha-FIRST entry)",
			skip.OwnedBy, wantOwnedBy)
	}

	// Parent's NameCollision condition continues to fire — the surface
	// is preserved; only the choice of survivor flipped.
	var md2 litellmv1alpha1.LiteLLMMCPServerDiscovery
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: mdName, Namespace: WatchNamespace}, &md2); err != nil {
		t.Fatalf("re-get MCPServerDiscovery: %v", err)
	}
	var sawCondition bool
	for _, c := range md2.Status.Conditions {
		if c.Type == ConditionTypeNameCollision && c.Status == metav1.ConditionTrue && c.Reason == "NameCollision" {
			sawCondition = true
			break
		}
	}
	if !sawCondition {
		t.Errorf("parent-level NameCollision condition must still be True; conditions=%+v", md2.Status.Conditions)
	}
}
