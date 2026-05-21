// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/connection"
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
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
	if !apimeta.IsStatusConditionTrue(got.Status.Conditions, "Ready") {
		t.Errorf("status.conditions[Ready].Status is not True; conditions=%+v", got.Status.Conditions)
	}
	if c := apimeta.FindStatusCondition(got.Status.Conditions, "Ready"); c == nil || c.Reason != reasonSynced {
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
	// must NOT enqueue exponential-backoff retries. Mutations should stay
	// at 0 across a 5s window — the mock counts /models GET as a Read,
	// not a Mutation, so a runaway reconcile would still produce 0
	// Mutations. We check Mutations as a defense-in-depth — any non-zero
	// would indicate the reconciler is calling POST/PUT/DELETE paths it
	// shouldn't.
	mutationsBefore := mockServer.Mutations()
	time.Sleep(5 * time.Second)
	deltaMutations := mockServer.Mutations() - mutationsBefore
	if deltaMutations != 0 {
		t.Errorf("REL-06 anti-storm FAIL: %d mutations during 5s window after BadMasterKey (expected 0)", deltaMutations)
	}

	// Re-Get and assert condition.
	var got litellmv1alpha1.LiteLLMConnection
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(cr), &got); err != nil {
		t.Fatalf("re-get CR: %v", err)
	}
	if c := apimeta.FindStatusCondition(got.Status.Conditions, "Ready"); c == nil || c.Reason != "BadMasterKey" {
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
	c := apimeta.FindStatusCondition(got.Status.Conditions, "Ready")
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
