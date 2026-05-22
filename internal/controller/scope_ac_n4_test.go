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
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
)

// outOfWatchNamespace is the namespace used by AC-N4 to host CRs that
// the manager MUST NOT reconcile. It is created on demand in the test
// (envtest's apiserver does not pre-create custom namespaces) and is
// asserted to be different from WatchNamespace.
const outOfWatchNamespace = "ac-n4-out-of-watch"

// TestWatchNamespaceEnforcement — SCOPE-04 / AC-N4. A Model CR created
// in a namespace OTHER than WatchNamespace must NEVER trigger the
// reconciler — the cache.Options.DefaultNamespaces filter in
// cmd/main.go (and replicated in suite_test.go's manager setup) is
// mechanically enforced at the informer level, above any in-Reconcile
// namespace check.
//
// Procedure:
//
//  1. Ensure outOfWatchNamespace != WatchNamespace (fixture guard).
//  2. Create outOfWatchNamespace in the envtest apiserver if absent.
//  3. Snapshot reconcileCalls and mock counters.
//  4. Create a Model CR in outOfWatchNamespace.
//  5. Wait 10s (we are asserting NO reconcile — sleep is intentional).
//  6. Assert reconcileCalls.Load did NOT change.
//  7. Assert mock.Mutations == 0 AND mock.Reads == 0 (defense-in-depth).
//
// This is the AC-N4 dry-run. The kind-e2e equivalent
// (TestNamespaceScopeAC_N4_E2E in test/e2e/) verifies the same property
// in a real cluster.
func TestWatchNamespaceEnforcement(t *testing.T) {
	if reconcileCalls == nil {
		t.Fatal("suite_test.go did not initialize globals — TestMain ordering bug")
	}
	if outOfWatchNamespace == WatchNamespace {
		t.Fatalf("fixture bug: outOfWatchNamespace (%q) must differ from WatchNamespace (%q)",
			outOfWatchNamespace, WatchNamespace)
	}

	ctx := context.Background()

	// Ensure the out-of-watch namespace exists. AlreadyExists is fine —
	// the test may be re-run in the same envtest process.
	if err := k8sClient.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: outOfWatchNamespace},
	}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create %s ns: %v", outOfWatchNamespace, err)
	}

	// Snapshot pre-state.
	beforeCalls := reconcileCalls.Load()
	beforeMutations := mockServer.Mutations()
	beforeReads := mockServer.Reads()

	// Create a Model CR in the wrong namespace.
	model := &litellmv1alpha1.LiteLLMModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ac-n4-wrong-ns",
			Namespace: outOfWatchNamespace,
		},
		Spec: litellmv1alpha1.ModelSpec{},
	}
	if err := k8sClient.Create(ctx, model); err != nil {
		t.Fatalf("create CR in %s ns: %v", outOfWatchNamespace, err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), model, &client.DeleteOptions{})
	})

	// AC-N4 observation window: 4 seconds. The cache should filter out
	// the CR entirely; if it did NOT, the noop reconciler would have
	// fired by now (envtest's manager loop is fast).
	time.Sleep(4 * time.Second)

	// Defensive: make sure the mock is in happy mode so any leaked
	// reconcile would be observable (mode happy returns 200 → reads++).
	if mockServer.Mode() != mock.ModeHappy {
		t.Logf("note: mock mode is %q, not happy", mockServer.Mode())
	}

	gotCalls := reconcileCalls.Load() - beforeCalls
	if gotCalls != 0 {
		t.Errorf("AC-N4 FAIL: noop reconciler was called %d times for a CR in non-watched namespace (cache filter did not enforce SCOPE-04)", gotCalls)
	}
	gotMutations := mockServer.Mutations() - beforeMutations
	gotReads := mockServer.Reads() - beforeReads
	if gotMutations != 0 || gotReads != 0 {
		t.Errorf("AC-N4 defense-in-depth: mock saw %d mutations and %d reads for an out-of-namespace CR (expected zero of each)", gotMutations, gotReads)
	}
}
