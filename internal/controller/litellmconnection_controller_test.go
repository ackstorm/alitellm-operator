// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/connection"
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
	"github.com/ackstorm/alitellm-operator/internal/metrics"
)

// connDefaultCR returns a LiteLLMConnection CR named 'default' in
// WatchNamespace pointing at the in-process mock server with the
// suite-managed master-key Secret. The CEL singleton-by-name rule
// (CONN-02) requires name="default"; tests serialize on the singleton
// via t.Cleanup deletion.
func connDefaultCR() *litellmv1alpha1.LiteLLMConnection {
	return &litellmv1alpha1.LiteLLMConnection{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default",
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.LiteLLMConnectionSpec{
			Endpoint: mockServer.URL(),
			MasterKeySecretRef: litellmv1alpha1.SecretKeyRef{
				Name: "litellm-master-key",
				Key:  "masterKey",
			},
			// FIX2.txt M-10 (2026-05-22): effectively disable rate
			// limiting in envtest by pinning to a high value. The
			// production default of 5 rps + burst 10 throttles
			// cross-suite tests that fire many mutations in quick
			// succession; the rate limiter delays past assertion
			// windows and bleeds artifacts across t.Cleanup boundaries
			// via the shared mockServer counter. 10000 rps is well
			// above any envtest workload but stays inside the CRD
			// Maximum=1000... — bump to 1000 (max allowed) which is
			// effectively unlimited for the envtest workload.
			MaxRequestsPerSecond: 1000,
			MaxBurst:             1000,
		},
	}
}

// ensureNoConnectionDefault deletes any pre-existing LiteLLMConnection/default
// in WatchNamespace and waits up to 5s for the API server to clear it. The
// singleton CEL constraint means only one CR named 'default' may exist at
// a time; previous tests leaving the CR behind would cause downstream
// Create calls to fail.
func ensureNoConnectionDefault(t *testing.T, ctx context.Context) {
	t.Helper()
	var existing litellmv1alpha1.LiteLLMConnection
	key := client.ObjectKey{Name: "default", Namespace: WatchNamespace}
	if err := k8sClient.Get(ctx, key, &existing); err == nil {
		_ = k8sClient.Delete(ctx, &existing)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, key, &existing); apierrors.IsNotFound(err) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("warning: LiteLLMConnection/default still present after 5s cleanup wait")
}

// ensureMasterKeySecret idempotently re-creates the master-key Secret
// after a test that deletes it (e.g. TestConnectionSecretNotFound).
func ensureMasterKeySecret(t *testing.T, ctx context.Context) {
	t.Helper()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "litellm-master-key",
			Namespace: WatchNamespace,
		},
		Data: map[string][]byte{"masterKey": []byte("sk-test-master-key")},
	}
	if err := k8sClient.Create(ctx, sec); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Logf("warning: ensureMasterKeySecret create failed: %v", err)
	}
}

// resetConnCacheSnapshot rebuilds the shared connCache with a zero-value
// snapshot so the next test's poll loop reads from a known baseline. The
// cache is a manager-level singleton — without this reset, the previous
// test's terminal snapshot bleeds into the next test's setup phase.
func resetConnCacheSnapshot() {
	connCache.Rebuild(connection.ConnectionSnapshot{})
}

// setConnCacheReady restores the shared connCache to a VALID Ready
// snapshot — Ready=true backed by the suite's mock-wired *litellm.Client.
//
// Test cleanup that needs the cache Ready (so an in-flight finalizer can
// drain through the real reconciler) MUST use this instead of hand-rolling
// connCache.Rebuild(ConnectionSnapshot{Ready: true}). A Ready snapshot with
// a nil Client violates the ConnectionSnapshot.Usable() invariant: it
// poisons the manager-level singleton and the next dependent reconcile
// (e.g. the always-on ModelAlias singleton) dereferences snap.Client and
// panics. That cross-test bleed is the root cause of issue #74's
// shuffle-dependent envtest flakes.
func setConnCacheReady() {
	connCache.Rebuild(connection.ConnectionSnapshot{
		Ready:  true,
		Reason: reasonSynced,
		Client: suiteLLMClient,
	})
}

// pollSnapshotReason polls cache.Snapshot up to `timeout` for
// snap.Reason == want. Returns the final snapshot regardless. The poll
// interval is 100ms — fast enough that the LiteLLMConnection probe loop
// (which has no RequeueAfter on error paths) is observed reliably.
func pollSnapshotReason(timeout time.Duration, want string) connection.ConnectionSnapshot {
	deadline := time.Now().Add(timeout)
	var snap connection.ConnectionSnapshot
	for time.Now().Before(deadline) {
		snap = connCache.Snapshot()
		if snap.Reason == want {
			return snap
		}
		time.Sleep(25 * time.Millisecond)
	}
	return snap
}

// TestConnectionProbeLoop_AC_C1 — CONN-03 + CONN-04 (happy path).
//
// Procedure:
//
// 1. Reset mock to ModeHappy + counters.
// 2. Ensure no pre-existing CR.
// 3. Create LiteLLMConnection/default with endpoint=mockServer.URL
// and masterKeySecretRef={litellm-master-key, masterKey}.
// 4. Poll up to 30s for cache.Snapshot.Reason == reasonSynced.
// 5. Assert cache.Snapshot.Ready == true AND .Client != nil.
// 6. Re-Get the CR; assert apimeta.IsStatusConditionTrue(Ready) +
// Reason="Synced" + ObservedGeneration matches.
//
// AC-C1: a LiteLLMConnection CR named 'default' with valid Secret reaches
// Ready=Synced within 30s on a happy mock.
func TestConnectionProbeLoop_AC_C1(t *testing.T) {
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
		// Wait briefly for delete to land so the next test's
		// ensureNoConnectionDefault is fast.
		time.Sleep(50 * time.Millisecond)
	})

	snap := pollSnapshotReason(30*time.Second, reasonSynced)
	if snap.Reason != reasonSynced {
		t.Fatalf("cache.Snapshot().Reason = %q, want %q within 30s", snap.Reason, reasonSynced)
	}
	if !snap.Ready {
		t.Errorf("cache.Snapshot().Ready = false, want true")
	}
	if snap.Client == nil {
		t.Errorf("cache.Snapshot().Client = nil, want non-nil after Synced rebuild (D-03 fresh client)")
	}

	// Re-Get the CR and assert status condition + observedGeneration.
	var got litellmv1alpha1.LiteLLMConnection
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(cr), &got); err != nil {
		t.Fatalf("re-get CR: %v", err)
	}
	if !apimeta.IsStatusConditionTrue(got.Status.Conditions, conditionTypeReady) {
		t.Errorf("status.conditions[Ready].Status is not True; conditions=%+v", got.Status.Conditions)
	}
	if c := apimeta.FindStatusCondition(got.Status.Conditions, conditionTypeReady); c == nil || c.Reason != reasonSynced {
		t.Errorf("status.conditions[Ready].Reason = %v, want Synced", c)
	}
	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("status.observedGeneration = %d, want %d (metadata.generation)", got.Status.ObservedGeneration, got.Generation)
	}
}

// TestConnectionProbeLoop_BadMasterKey — CONN-04 (401 path) + REL-06
// (anti-storm).
//
// Procedure:
//
// 1. Reset mock to Mode401 BEFORE creating the CR.
// 2. Create LiteLLMConnection/default.
// 3. Poll up to 15s for cache.Snapshot.Reason == "BadMasterKey".
// 4. Assert cache.Snapshot.Ready == false.
// 5. Observe mockServer.Mutations over a 5s anti-storm window; assert
// it stays at 0 (the reconciler returned nil, NOT err — no
// exponential-backoff storm).
// 6. Re-Get CR; assert condition Reason == "BadMasterKey".
func TestConnectionProbeLoop_BadMasterKey(t *testing.T) {
	if connCache == nil {
		t.Fatal("suite_test.go did not initialize connCache")
	}

	ctx := context.Background()
	mockServer.SetMode(mock.Mode401)
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

	snap := pollSnapshotReason(15*time.Second, "BadMasterKey")
	if snap.Reason != "BadMasterKey" {
		t.Fatalf("cache.Snapshot().Reason = %q, want BadMasterKey within 15s", snap.Reason)
	}
	if snap.Ready {
		t.Errorf("cache.Snapshot().Ready = true, want false on BadMasterKey")
	}

	// REL-06 anti-storm: 401 returns nil (NOT err), so controller-runtime
	// must NOT enqueue exponential-backoff retries. The probe is POST
	// /key/health (counted as a Mutation by the mock); after BadMasterKey
	// the reconciler returns nil with no RequeueAfter, so the
	// /key/health-mutation delta in a 2.5s window should be 0. We
	// specifically check the probe path here instead of all Mutations
	// because /key/health is the only mutation the connection reconciler
	// emits — non-zero delta indicates a runaway reconcile.
	probeBefore := mockServer.PathCallCount("/key/health")
	time.Sleep(2500 * time.Millisecond)
	deltaProbes := mockServer.PathCallCount("/key/health") - probeBefore
	if deltaProbes != 0 {
		t.Errorf("REL-06 anti-storm FAIL: %d /key/health probes during 2.5s window after BadMasterKey (expected 0)", deltaProbes)
	}

	// Re-Get and assert condition.
	var got litellmv1alpha1.LiteLLMConnection
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(cr), &got); err != nil {
		t.Fatalf("re-get CR: %v", err)
	}
	if c := apimeta.FindStatusCondition(got.Status.Conditions, conditionTypeReady); c == nil || c.Reason != "BadMasterKey" {
		t.Errorf("status.conditions[Ready].Reason = %v, want BadMasterKey", c)
	}
}

// TestConnectionSecretNotFound — CONN-04 (SecretNotFound path) + §9.1
// redaction canary.
//
// Procedure:
//
// 1. Delete the master-key Secret created in TestMain.
// 2. SetMode(ModeHappy).
// 3. Create LiteLLMConnection/default.
// 4. Poll for cache.Snapshot.Reason == "SecretNotFound".
// 5. Re-Get; assert condition Reason == "SecretNotFound" AND message
// contains the literal "default/litellm-master-key" (the §9.1
// diagnostic coordinates) but does NOT contain "sk-" (the master
// key value redaction canary).
// 6. t.Cleanup recreates the Secret so the next test's
// ensureMasterKeySecret is a no-op.
func TestConnectionSecretNotFound(t *testing.T) {
	if connCache == nil {
		t.Fatal("suite_test.go did not initialize connCache")
	}

	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	ensureNoConnectionDefault(t, ctx)
	resetConnCacheSnapshot()

	// Delete the master-key Secret BEFORE creating the CR so the first
	// reconcile sees the missing Secret.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "litellm-master-key",
			Namespace: WatchNamespace,
		},
	}
	if err := k8sClient.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("delete master-key Secret: %v", err)
	}
	// Wait briefly for delete to propagate through the informer cache so
	// the reconciler's r.Get sees NotFound.
	time.Sleep(500 * time.Millisecond)

	cr := connDefaultCR()
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create LiteLLMConnection/default: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), cr, &client.DeleteOptions{})
		// Recreate the Secret so subsequent tests have it.
		ensureMasterKeySecret(t, context.Background())
		time.Sleep(50 * time.Millisecond)
	})

	snap := pollSnapshotReason(15*time.Second, reasonSecretNotFound)
	if snap.Reason != reasonSecretNotFound {
		t.Fatalf("cache.Snapshot().Reason = %q, want SecretNotFound within 15s", snap.Reason)
	}
	if snap.Ready {
		t.Errorf("cache.Snapshot().Ready = true, want false on SecretNotFound")
	}

	// Re-Get and assert condition + §9.1 redaction.
	var got litellmv1alpha1.LiteLLMConnection
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(cr), &got); err != nil {
		t.Fatalf("re-get CR: %v", err)
	}
	c := apimeta.FindStatusCondition(got.Status.Conditions, conditionTypeReady)
	if c == nil {
		t.Fatalf("status.conditions[Ready] missing")
	}
	if c.Reason != reasonSecretNotFound {
		t.Errorf("status.conditions[Ready].Reason = %q, want SecretNotFound", c.Reason)
	}

	// §9.1 redaction canary: message must contain the diagnostic
	// coordinates but never the master key value (sk-.).
	expectedCoords := WatchNamespace + "/litellm-master-key"
	if !strings.Contains(c.Message, expectedCoords) {
		t.Errorf("status.conditions[Ready].Message = %q does not contain %q (diagnostic coordinates)", c.Message, expectedCoords)
	}
	if strings.Contains(c.Message, "sk-") {
		t.Errorf("§9.1 FAIL: status.conditions[Ready].Message contains 'sk-' prefix (master key value leaked): %q", c.Message)
	}
}

// TestConnectionSecretRotation_AC_C3a — CONN-05 part (b) + D-03.
//
// Procedure:
//
// 1. SetMode(ModeHappy). Create CR. Wait for Ready=Synced.
// 2. Snapshot cache.Snapshot.Client pointer (clientV1).
// 3. Update the master-key Secret's data to a new value (still a valid
// "sk-." prefix so happy mode keeps accepting).
// 4. Poll up to 15s for cache.Snapshot.Client != clientV1 (a NEW
// pointer — D-03 fresh-client-per-rebuild empirical proof).
// 5. Assert cache.Snapshot.Ready == true AND Reason == reasonSynced
// (mock still in happy mode; the rotated key still validates).
//
// AC-C3a: Secret rotation propagates within 10s (allow 15s envtest
// slack); fresh-client-per-rebuild empirically observable.
func TestConnectionSecretRotation_AC_C3a(t *testing.T) {
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
		// Restore the master-key Secret's original value so subsequent
		// tests get a known fixture.
		var sec corev1.Secret
		_ = k8sClient.Get(context.Background(), client.ObjectKey{Name: "litellm-master-key", Namespace: WatchNamespace}, &sec)
		if sec.Name != "" {
			sec.Data = map[string][]byte{"masterKey": []byte("sk-test-master-key")}
			_ = k8sClient.Update(context.Background(), &sec)
		}
		time.Sleep(50 * time.Millisecond)
	})

	// Wait for the first Synced reconcile.
	snap := pollSnapshotReason(30*time.Second, reasonSynced)
	if snap.Reason != reasonSynced || snap.Client == nil {
		t.Fatalf("initial Synced never reached: snap=%+v", snap)
	}
	clientV1 := snap.Client

	// Rotate the Secret's data — should trigger the Secret watch +
	// secretToConnection mapper + a fresh reconcile.
	var sec corev1.Secret
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "litellm-master-key", Namespace: WatchNamespace}, &sec); err != nil {
		t.Fatalf("get master-key Secret: %v", err)
	}
	sec.Data = map[string][]byte{"masterKey": []byte("sk-test-rotated")}
	if err := k8sClient.Update(ctx, &sec); err != nil {
		t.Fatalf("update master-key Secret: %v", err)
	}

	// Poll up to 15s for cache.Snapshot.Client != clientV1.
	deadline := time.Now().Add(15 * time.Second)
	var newSnap connection.ConnectionSnapshot
	for time.Now().Before(deadline) {
		newSnap = connCache.Snapshot()
		if newSnap.Client != nil && newSnap.Client != clientV1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if newSnap.Client == clientV1 {
		t.Fatalf("D-03 FAIL: cache.Snapshot().Client did not change after Secret rotation within 15s — fresh-client-per-rebuild invariant broken")
	}
	if newSnap.Client == nil {
		t.Fatalf("cache.Snapshot().Client = nil after rotation; want non-nil fresh client")
	}
	if !newSnap.Ready {
		t.Errorf("cache.Snapshot().Ready = false after rotation; want true (mock still in happy mode)")
	}
	if newSnap.Reason != reasonSynced {
		t.Errorf("cache.Snapshot().Reason = %q after rotation; want Synced", newSnap.Reason)
	}
}

func TestConnectionReasonAll_IncludesInvalidEndpoint(t *testing.T) {
	t.Parallel()
	found := false
	for _, r := range connectionReasonAll {
		if r == reasonInvalidEndpoint {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("connectionReasonAll missing %q; metrics one-hot gauge will not reset it", reasonInvalidEndpoint)
	}
}

// recordingConnectionCache is a thin connection.ConnectionCache fake
// that records every Rebuild invocation in order. Used by the F2
// regression test (TestConnection_GenChangeRebuildsCacheBeforeProbe)
// to assert that on a generation-change reconcile the cache is
// Rebuilt to Ready=false reason=Connecting BEFORE the terminal Rebuild
// (SecretNotFound / BadMasterKey / Unreachable) — closing the gap
// where dependents observed the previous Ready=true snapshot with the
// stale client during the probe window.
//
// Kept local to this test file (not a package-level helper) per the
// scope of the F2 fix; other tests use FakeConnectionCache (Snapshot
// only) or the real *connection.Cache.
type recordingConnectionCache struct {
	mu       sync.Mutex
	rebuilds []connection.ConnectionSnapshot
}

func (r *recordingConnectionCache) Snapshot() connection.ConnectionSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.rebuilds) == 0 {
		return connection.ConnectionSnapshot{}
	}
	return r.rebuilds[len(r.rebuilds)-1]
}

func (r *recordingConnectionCache) InvalidateOn401() {}

func (r *recordingConnectionCache) Rebuild(snap connection.ConnectionSnapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rebuilds = append(r.rebuilds, snap)
}

func (r *recordingConnectionCache) calls() []connection.ConnectionSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]connection.ConnectionSnapshot, len(r.rebuilds))
	copy(out, r.rebuilds)
	return out
}

// Compile-time assertion: *recordingConnectionCache satisfies the
// connection.ConnectionCache interface.
var _ connection.ConnectionCache = (*recordingConnectionCache)(nil)

// TestConnection_GenChangeRebuildsCacheBeforeProbe asserts that when a
// LiteLLMConnection's generation advances (e.g., endpoint or
// masterKeySecretRef rotation), the cache snapshot is rebuilt to
// Ready=false reason=Connecting BEFORE the Secret fetch + probe runs,
// so dependent reconcilers reading r.Cache.Snapshot() during the probe
// window do NOT observe the previous Ready=true snapshot with the OLD
// *litellm.Client.
//
// Without the fix, only ONE Rebuild is recorded (the terminal one —
// here SecretNotFound), and the cache stays at its pre-existing
// Ready=true Synced snapshot during the probe window. With the fix,
// TWO Rebuilds are recorded in order: first {Ready=false,
// Reason=Connecting, Client=nil}, then {Ready=false,
// Reason=SecretNotFound, Client=nil}.
//
// The Secret-not-found path is chosen deliberately because it
// short-circuits before any HTTP probe (no mock server required) yet
// still exercises the Step 3 generation-change guard. The fix is
// orthogonal to which terminal reason follows.
//
// Post-2026-05-26 review finding F2 (cache invalidation gap).
func TestConnection_GenChangeRebuildsCacheBeforeProbe(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := litellmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(litellm): %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(corev1): %v", err)
	}

	const ns = "default"

	// Build a Connection CR at Generation=2 with status reflecting the
	// PREVIOUS generation's terminal Synced outcome (ObservedGeneration=1,
	// Ready=True, Reason=Synced). Finalizer is pre-attached so the
	// reconciler skips the Step 2b finalizer-add early return and falls
	// through to the Step 3 Connecting-on-entry block.
	prevReady := metav1.Condition{
		Type:               conditionTypeReady,
		Status:             metav1.ConditionTrue,
		Reason:             reasonSynced,
		Message:            "probe ok",
		ObservedGeneration: 1,
		LastTransitionTime: metav1.Now(),
	}
	cr := &litellmv1alpha1.LiteLLMConnection{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "default",
			Namespace:  ns,
			Generation: 2,
			Finalizers: []string{connectionFinalizer},
		},
		Spec: litellmv1alpha1.LiteLLMConnectionSpec{
			Endpoint: "http://example.invalid",
			MasterKeySecretRef: litellmv1alpha1.SecretKeyRef{
				Name: "missing-master-key", // intentionally absent — drives SecretNotFound
				Key:  "masterKey",
			},
		},
		Status: litellmv1alpha1.LiteLLMConnectionStatus{
			ObservedGeneration: 1,
			Conditions:         []metav1.Condition{prevReady},
		},
	}

	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cr).
		WithStatusSubresource(&litellmv1alpha1.LiteLLMConnection{}).
		Build()

	cache := &recordingConnectionCache{}

	r := &LiteLLMConnectionReconciler{
		Client:    cli,
		Scheme:    scheme,
		Cache:     cache,
		Namespace: ns,
		Log:       ctrl.Log.WithName("test"),
	}

	// Reconcile drives Step 3 (gen-change Connecting write) → Step 4
	// (Secret GET → NotFound → terminal SecretNotFound write + Rebuild).
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "default", Namespace: ns},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	calls := cache.calls()
	if len(calls) < 2 {
		t.Fatalf(
			"F2 FAIL: expected at least 2 Rebuild calls (Connecting then SecretNotFound), got %d: %+v\n"+
				"pre-fix bug: only the terminal Rebuild fires, leaving the previous Ready=true snapshot with the stale client visible to dependents during the probe window",
			len(calls), calls,
		)
	}

	// First Rebuild must be the Connecting-on-entry invalidation:
	// Ready=false, Reason=Connecting, Client=nil.
	first := calls[0]
	if first.Ready {
		t.Errorf("first Rebuild Ready=true, want false (cache must invalidate before probe)")
	}
	if first.Reason != reasonConnecting {
		t.Errorf("first Rebuild Reason=%q, want %q", first.Reason, reasonConnecting)
	}
	if first.Client != nil {
		t.Errorf("first Rebuild Client=%p, want nil (stale client must not leak through probe window)", first.Client)
	}

	// Terminal Rebuild must be SecretNotFound (Ready=false, Reason=SecretNotFound).
	last := calls[len(calls)-1]
	if last.Reason != reasonSecretNotFound {
		t.Errorf("terminal Rebuild Reason=%q, want %q", last.Reason, reasonSecretNotFound)
	}
	if last.Ready {
		t.Errorf("terminal Rebuild Ready=true, want false on SecretNotFound")
	}
}

// TestConnection_LoggingHealthy_NoCallbacksReported — UAT LOW-01.
// When LiteLLM /key/health returns logging_callbacks: null, the
// LoggingHealthy condition must be Unknown with reason
// NoCallbacksReported and a self-explanatory message (not the empty
// "logging callbacks: " of pre-LOW-01).
//
// Mirrors TestConnectionProbeLoop_AC_C1 scaffolding (shared mockServer,
// ensureNoConnectionDefault, ensureMasterKeySecret,
// resetConnCacheSnapshot, connDefaultCR, pollSnapshotReason). The only
// behavioral difference is the toggle on mockServer's /key/health
// response shape.
func TestConnection_LoggingHealthy_NoCallbacksReported(t *testing.T) {
	if connCache == nil {
		t.Fatal("suite_test.go did not initialize connCache — TestMain ordering bug")
	}

	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.SetLoggingCallbacksNull(true)
	t.Cleanup(func() { mockServer.SetLoggingCallbacksNull(false) })
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

	snap := pollSnapshotReason(30*time.Second, reasonSynced)
	if snap.Reason != reasonSynced {
		t.Fatalf("cache.Snapshot().Reason = %q, want %q within 30s", snap.Reason, reasonSynced)
	}

	// Re-Get CR; assert LoggingHealthy condition shape.
	var got litellmv1alpha1.LiteLLMConnection
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(cr), &got); err != nil {
		t.Fatalf("re-get CR: %v", err)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, "LoggingHealthy")
	if cond == nil {
		t.Fatal("LoggingHealthy condition missing")
	}
	if cond.Status != metav1.ConditionUnknown {
		t.Fatalf("LoggingHealthy status: want Unknown, got %v", cond.Status)
	}
	if cond.Reason != reasonNoCallbacksReported {
		t.Fatalf("LoggingHealthy reason: want NoCallbacksReported, got %q", cond.Reason)
	}
	if cond.Message == "" || strings.HasSuffix(cond.Message, ": ") {
		t.Fatalf("LoggingHealthy message must be non-trivial, got %q", cond.Message)
	}
}

// TestConnectionReadyGauge_ReassertedWhenStatusUnchanged guards finding
// #10: after an operator restart the in-memory gauge is 0 but the CR's
// Ready condition already equals what writeStatus is about to write. The
// skip-when-equal path must STILL re-assert the gauge.
func TestConnectionReadyGauge_ReassertedWhenStatusUnchanged(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := litellmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	conn := &litellmv1alpha1.LiteLLMConnection{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default", Generation: 1},
	}
	// Pre-set Ready=False/Connecting at ObservedGeneration=1 so writeStatus sees "unchanged".
	apimeta.SetStatusCondition(&conn.Status.Conditions, metav1.Condition{
		Type: conditionTypeReady, Status: metav1.ConditionFalse, Reason: reasonConnecting,
		Message: "msg", ObservedGeneration: 1,
	})
	conn.Status.ObservedGeneration = 1

	cli := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(conn).
		WithStatusSubresource(&litellmv1alpha1.LiteLLMConnection{}).Build()
	r := &LiteLLMConnectionReconciler{Client: cli, Scheme: scheme, Namespace: "default", Log: ctrl.Log.WithName("test")}

	// Reset the one-hot gauge to 0 across all reasons (simulates post-restart).
	for _, rk := range connectionReasonAll {
		metrics.ConnectionReady.WithLabelValues(rk).Set(0)
	}

	if err := r.writeStatus(context.Background(), conn, reasonConnecting, "msg"); err != nil {
		t.Fatalf("writeStatus: %v", err)
	}

	if v := testutil.ToFloat64(metrics.ConnectionReady.WithLabelValues(reasonConnecting)); v != 1 {
		t.Errorf("connection_ready{Connecting}: want 1 after skip-when-equal, got %v", v)
	}
}
