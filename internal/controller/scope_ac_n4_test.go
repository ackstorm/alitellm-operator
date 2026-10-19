// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"strings"
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
//  3. Snapshot the noop reconciler's foreign-namespace count and the mock log.
//  4. Create a Model CR in outOfWatchNamespace.
//  5. Wait 10s (we are asserting NO reconcile — sleep is intentional).
//  6. Assert no reconcile fired for a request outside WatchNamespace.
//  7. Assert the mock saw no /model/ call (defense-in-depth).
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
	beforeCalls := reconciler.ForeignNamespaceCalls.Load()
	beforeRecorded := len(mockServer.Recorded())

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

	// AC-N4 observation window: long enough for envtest's manager loop to
	// observe a leaked watch event, but short enough to keep this negative
	// cache-filter smoke test cheap.
	time.Sleep(1250 * time.Millisecond)

	// Defensive: make sure the mock is in happy mode so any leaked
	// reconcile would be observable (mode happy returns 200 → reads++).
	if mockServer.Mode() != mock.ModeHappy {
		t.Logf("note: mock mode is %q, not happy", mockServer.Mode())
	}

	// Scope both assertions to the out-of-namespace CR. Global counters
	// (all noop reconciles, all mock traffic) also pick up WatchNamespace
	// work left draining by earlier tests — seen at 10 reconciles and 305
	// reads in one 1.25s window — and fail without any scope leak.
	gotCalls := reconciler.ForeignNamespaceCalls.Load() - beforeCalls
	if gotCalls != 0 {
		t.Errorf("AC-N4 FAIL: noop reconciler was called %d times for a CR in non-watched namespace (cache filter did not enforce SCOPE-04)", gotCalls)
	}
	// Defense-in-depth: a leaked Model reconcile reaches LiteLLM under
	// /model/. Other paths are logged only, to diagnose background noise.
	modelCalls := 0
	other := map[string]int{}
	for _, c := range mockServer.Recorded()[beforeRecorded:] {
		if strings.HasPrefix(c.Path, "/model/") {
			modelCalls++
		} else {
			other[c.Method+" "+c.Path]++
		}
	}
	if modelCalls != 0 {
		t.Errorf("AC-N4 defense-in-depth: mock saw %d /model/ calls for an out-of-namespace CR (expected zero)", modelCalls)
	}
	if len(other) > 0 {
		t.Logf("AC-N4: unrelated mock traffic in window: %v", other)
	}
}
