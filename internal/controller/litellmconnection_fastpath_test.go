// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ackstorm/alitellm-operator/internal/connection"
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
)

// TestConnectionFastPathEnqueue_AC_C3c — CONN-05 part (c) + D-09.
//
// The 401 fast-path is the cache→channel→Source.Channel→reconciler
// pipeline. Calling cache.InvalidateOn401 must:
//
// 1. Synchronously store a not-Ready snapshot (cache.Snapshot.Ready
// flips to false within 100ms — the InvalidateOn401 implementation
// in cache.go stores a placeholder ConnectionSnapshot before sending
// the channel event).
// 2. Fire exactly ONE event on cache.Channel (D-10 storm gate).
// 3. Cause the LiteLLMConnection reconciler's WatchesRawSource handler
// to enqueue a reconcile of `<watchNs>/default` — observed via
// PathCallCount("/key/health") incrementing (the reconciler probes
// via POST /key/health) and cache.Snapshot.Ready transitioning back
// to true.
//
// Procedure:
//
// 1. SetMode(ModeHappy); ResetCounters.
// 2. Ensure CR/default exists and reaches Ready=Synced first.
// 3. Snapshot baselineReads after Synced.
// 4. Call connCache.InvalidateOn401 — DIRECTLY (a future Phase 3
// dependent reconciler will be the caller in production).
// 5. Within 100ms: assert cache.Snapshot.Ready == false.
// 6. Within 5s: assert cache.Snapshot.Ready == true AND Reason ==
// "Synced" (probe re-ran via the channel-driven enqueue).
// 7. Assert mockServer.Reads incremented (at least one re-probe).
func TestConnectionFastPathEnqueue_AC_C3c(t *testing.T) {
	if connCache == nil {
		t.Fatal("suite_test.go did not initialize connCache — TestMain ordering bug")
	}

	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	ensureNoConnectionDefault(t, ctx)
	ensureMasterKeySecret(t, ctx)
	resetConnCacheSnapshot()

	cr := connDefaultCR()
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create LiteLLMConnection/default: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), cr, &client.DeleteOptions{})
		time.Sleep(50 * time.Millisecond)
	})

	// Reach Ready=Synced first.
	snap := pollSnapshotReason(30*time.Second, reasonSynced)
	if snap.Reason != reasonSynced || !snap.Ready {
		t.Fatalf("initial Synced never reached: snap=%+v", snap)
	}

	// Snapshot reads AFTER initial Synced so the post-Invalidate delta
	// reflects only the fast-path-triggered probe.
	baselineReads := mockServer.PathCallCount("/key/health")

	// Trigger the 401 fast-path.
	connCache.InvalidateOn401()

	// Within 100ms: cache must be not-Ready (synchronous store in
	// InvalidateOn401 — D-09 step 2).
	deadline := time.Now().Add(150 * time.Millisecond)
	notReady := false
	for time.Now().Before(deadline) {
		if !connCache.Snapshot().Ready {
			notReady = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !notReady {
		t.Fatalf("AC-C3c FAIL: cache.Snapshot().Ready did NOT flip to false within 150ms of InvalidateOn401()")
	}

	// Within 5s: cache must return to Ready=Synced (proves channel→
	// WatchesRawSource→reconciler→probe→Rebuild pipeline works).
	deadline = time.Now().Add(5 * time.Second)
	var afterSnap connection.ConnectionSnapshot
	for time.Now().Before(deadline) {
		afterSnap = connCache.Snapshot()
		if afterSnap.Ready && afterSnap.Reason == reasonSynced {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !afterSnap.Ready || afterSnap.Reason != reasonSynced {
		t.Fatalf("AC-C3c FAIL: cache did not return to Ready=Synced within 5s of InvalidateOn401(); final snap=%+v", afterSnap)
	}

	// The reconciler must have probed at least once.
	postReads := mockServer.PathCallCount("/key/health")
	if postReads <= baselineReads {
		t.Errorf("AC-C3c FAIL: PathCallCount(/key/health) did not increment after InvalidateOn401() (before=%d, after=%d) — the channel-driven enqueue did not reach the reconciler",
			baselineReads, postReads)
	}
	t.Logf("AC-C3c: reads before=%d, after=%d (delta=%d); cache returned to Synced in <5s", baselineReads, postReads, postReads-baselineReads)
}

// TestConnectionFastPathStormGate_D_10 — D-10 CAS storm gate.
//
// Without the gate, calling InvalidateOn401 five times in a tight loop
// would fire five channel events and enqueue five reconciles, producing
// at least five probe (POST /key/health) calls on the mock. With the gate,
// the five rapid calls collapse to AT MOST ONE channel event AND ONE
// enqueued reconcile per invalidation cycle (the gate resets on the
// next Rebuild).
//
// Procedure:
//
// 1. SetMode(ModeHappy); ResetCounters.
// 2. Ensure CR is Synced.
// 3. Snapshot baselineReads.
// 4. Call connCache.InvalidateOn401 five times back-to-back (no sleeps).
// 5. Within 5s, assert mockServer.Reads - baselineReads <= 3 (storm
// gate bound — without the gate the delta would be >= 5).
// 6. After the cache returns to Ready=Synced, wait 1s to ensure
// Rebuild has reset the gate.
// 7. Call InvalidateOn401 once more; assert the cache returns to
// Ready=Synced again (proves the gate resets after Rebuild).
func TestConnectionFastPathStormGate_D_10(t *testing.T) {
	if connCache == nil {
		t.Fatal("suite_test.go did not initialize connCache")
	}

	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	ensureNoConnectionDefault(t, ctx)
	ensureMasterKeySecret(t, ctx)
	resetConnCacheSnapshot()

	cr := connDefaultCR()
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create LiteLLMConnection/default: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), cr, &client.DeleteOptions{})
		time.Sleep(50 * time.Millisecond)
	})

	// Reach Ready=Synced.
	snap := pollSnapshotReason(30*time.Second, reasonSynced)
	if snap.Reason != reasonSynced {
		t.Fatalf("initial Synced never reached: snap=%+v", snap)
	}
	baselineReads := mockServer.PathCallCount("/key/health")

	// Storm: 5 InvalidateOn401 calls back-to-back, no sleeps.
	for i := 0; i < 5; i++ {
		connCache.InvalidateOn401()
	}

	// Within 5s, the cache should return to Ready=Synced once.
	deadline := time.Now().Add(5 * time.Second)
	var afterSnap connection.ConnectionSnapshot
	for time.Now().Before(deadline) {
		afterSnap = connCache.Snapshot()
		if afterSnap.Ready && afterSnap.Reason == reasonSynced {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !afterSnap.Ready || afterSnap.Reason != reasonSynced {
		t.Fatalf("D-10 storm-gate FAIL: cache did not return to Ready=Synced within 5s of 5x InvalidateOn401(); final snap=%+v", afterSnap)
	}

	// D-10 strict bound: the read delta must be <= 3. Without the gate,
	// 5 channel events ⇒ 5 enqueued reconciles ⇒ delta would be 5+
	// (the asymmetry is the regression catch).
	postReads := mockServer.PathCallCount("/key/health")
	delta := postReads - baselineReads
	if delta > 3 {
		t.Errorf("D-10 FAIL: 5 successive InvalidateOn401() produced %d reads (delta from baseline %d); storm gate should bound this to <= 3",
			postReads, baselineReads)
	}
	t.Logf("D-10 storm gate: 5 InvalidateOn401() calls produced read-delta=%d (bound: <=3)", delta)

	// Wait an additional 1s past Synced to ensure Rebuild has reset the
	// gate. (Rebuild resets c.invalidated to false unconditionally per
	// cache.go.)
	time.Sleep(1 * time.Second)

	// Second invalidation — gate must be re-armed.
	preSecondReads := mockServer.PathCallCount("/key/health")
	connCache.InvalidateOn401()

	deadline = time.Now().Add(5 * time.Second)
	var secondSnap connection.ConnectionSnapshot
	for time.Now().Before(deadline) {
		secondSnap = connCache.Snapshot()
		if secondSnap.Ready && secondSnap.Reason == reasonSynced {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !secondSnap.Ready || secondSnap.Reason != reasonSynced {
		t.Fatalf("D-10 reset FAIL: cache did not return to Ready=Synced within 5s of post-reset InvalidateOn401(); the storm gate was not re-armed by the previous Rebuild")
	}
	postSecondReads := mockServer.PathCallCount("/key/health")
	if postSecondReads <= preSecondReads {
		t.Errorf("D-10 reset FAIL: post-reset InvalidateOn401() did not trigger a probe (reads before=%d, after=%d)",
			preSecondReads, postSecondReads)
	}
	t.Logf("D-10 gate reset: second InvalidateOn401() produced read-delta=%d (expected >=1)", postSecondReads-preSecondReads)
}

// TestConnectionFastPathEnqueue_NonDefaultWatchNamespace_CR03 — Plan
// 02-04 CR-03 close.
//
// The 401 fast-path channel handler used to enqueue a reconcile of
// `default/default` via a hardcoded `connectionWatchNamespace`
// constant. Production deployments setting WATCH_NAMESPACE to anything
// other than "default" would queue an unresolvable request
// (`default/default` while the informer cache was scoped to a
// different namespace), breaking CONN-05 part (c) — the "within 5
// seconds" SLA — for every non-default WATCH_NAMESPACE.
//
// The fix added a Namespace field to LiteLLMConnectionReconciler and a
// fastPathRequest helper that reads r.Namespace. cmd/main.go now
// passes `Namespace: watchNS` (sourced from the WATCH_NAMESPACE env
// var); suite_test.go passes `Namespace: WatchNamespace` so the
// existing 11 Phase-2 envtests keep working.
//
// This test exercises the helper directly with three Namespace values:
//
// - "litellm-ops" — non-default production-style value; would
// fail under the original CR-03 defect.
// - "" — empty (misconfigured suite); helper produces
// an empty namespace (which the informer cache cannot match), but
// does NOT default to "default" — proving the hardcode is gone.
// - "production-llm" — another non-default value; covers a second
// deployment scenario.
//
// The test stands alone — it does not exercise the envtest manager —
// because suite_test.go is single-manager-per-binary and the fast-path
// closure is private to SetupWithManager. The helper-extraction refactor
// is the load-bearing decoupling that lets this assertion run at the
// unit level.
func TestConnectionFastPathEnqueue_NonDefaultWatchNamespace_CR03(t *testing.T) {
	cases := []struct {
		name        string
		nsField     string
		wantNS      string
		wantName    string
		description string
	}{
		{
			name:        "non-default production WATCH_NAMESPACE",
			nsField:     "litellm-ops",
			wantNS:      "litellm-ops",
			wantName:    "default",
			description: "CR-03 primary case: WATCH_NAMESPACE != default must enqueue the matching namespace",
		},
		{
			name:        "empty Namespace (misconfigured suite)",
			nsField:     "",
			wantNS:      "",
			wantName:    "default",
			description: "Empty field must NOT default to default (proves the hardcoded constant is gone)",
		},
		{
			name:        "second non-default value",
			nsField:     "production-llm",
			wantNS:      "production-llm",
			wantName:    "default",
			description: "Second case covering an alternate production deployment",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &LiteLLMConnectionReconciler{Namespace: tc.nsField}
			req := r.fastPathRequest()

			if req.NamespacedName.Namespace != tc.wantNS {
				t.Errorf("CR-03 FAIL (%s): fastPathRequest enqueues namespace=%q (expected %q); reconciler.Namespace=%q",
					tc.description, req.NamespacedName.Namespace, tc.wantNS, r.Namespace)
			}
			if req.NamespacedName.Name != tc.wantName {
				t.Errorf("CR-03 FAIL (%s): fastPathRequest enqueues name=%q (expected %q — the CEL singleton)",
					tc.description, req.NamespacedName.Name, tc.wantName)
			}
		})
	}
}
