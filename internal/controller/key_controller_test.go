// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/litellm"
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
)

const (
	keyPollTimeout = 30 * time.Second
	keyTestPeriod  = "30d"
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
			Budget:     &litellmv1alpha1.BudgetSpec{Limit: &limit, Period: keyTestPeriod},
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
	_ = k8sClient.Delete(ctx, &k)
	deadline := time.Now().Add(keyPollTimeout)
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, key, &k); apierrors.IsNotFound(err) {
			return
		}
		// Retried: the reconciler's status write can conflict with the strip.
		k.Finalizers = nil
		_ = k8sClient.Update(ctx, &k)
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

func keyTestSetup(t *testing.T, names ...string) {
	t.Helper()
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetKeys()
	if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: keySecretNS}}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	for _, n := range names {
		ensureNoKey(t, ctx, n)
	}
	resetConnCacheSnapshot()
	cleanupConn := setupReadyConnectionAccessGroup(t, ctx)
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		for _, n := range names {
			ensureNoKey(t, context.Background(), n)
		}
		cleanupConn()
		setConnCacheReady()
		mockServer.ResetKeys()
	})
}

func pollKeyReason(t *testing.T, ctx context.Context, name, want string) *litellmv1alpha1.LiteLLMKey {
	t.Helper()
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	deadline := time.Now().Add(keyPollTimeout)
	var k litellmv1alpha1.LiteLLMKey
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, key, &k); err == nil {
			if c := apimeta.FindStatusCondition(k.Status.Conditions, conditionTypeReady); c != nil && c.Reason == want &&
				k.Status.ObservedGeneration == k.Generation {
				return &k
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("key %s never reached reason %s; conditions=%v", name, want, k.Status.Conditions)
	return nil
}

func keySecret(ctx context.Context, name string) (*corev1.Secret, error) {
	var s corev1.Secret
	err := k8sClient.Get(ctx, client.ObjectKey{Name: name + "-secret", Namespace: keySecretNS}, &s)
	return &s, err
}

func keyValue(t *testing.T, ctx context.Context, name string) string {
	t.Helper()
	s, err := keySecret(ctx, name)
	if err != nil {
		t.Fatalf("key secret: %v", err)
	}
	return string(s.Data["api-key"])
}

// pokeKey triggers a reconcile without changing the spec. Tests use it in
// place of the safety relist, which the suite keeps off.
func pokeKey(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	for i := 0; i < 20; i++ {
		var k litellmv1alpha1.LiteLLMKey
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: WatchNamespace}, &k); err != nil {
			t.Fatal(err)
		}
		if k.Annotations == nil {
			k.Annotations = map[string]string{}
		}
		k.Annotations["test/poke"] = fmt.Sprint(time.Now().UnixNano())
		err := k8sClient.Update(ctx, &k)
		if err == nil {
			return
		}
		if !apierrors.IsConflict(err) {
			t.Fatal(err)
		}
	}
	t.Fatal("pokeKey: conflict retries exhausted")
}

func waitKey(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(keyPollTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestKey_Create(t *testing.T) {
	ctx := context.Background()
	keyTestSetup(t, "key-create")
	if err := k8sClient.Create(ctx, keyCR("key-create")); err != nil {
		t.Fatal(err)
	}
	got := pollKeyReason(t, ctx, "key-create", reasonSynced)

	s, err := keySecret(ctx, "key-create")
	if err != nil {
		t.Fatal(err)
	}
	value := string(s.Data["api-key"])
	if !strings.HasPrefix(value, "sk-") || len(value) < 16 {
		t.Fatalf("bad key value shape (length %d)", len(value))
	}
	if s.Labels["app.kubernetes.io/managed-by"] != "alitellm-operator" ||
		s.Annotations["litellm.ackstorm.ai/owner-uid"] != string(got.UID) {
		t.Errorf("secret marks: labels=%v annotations=%v", s.Labels, s.Annotations)
	}
	row, ok := mockServer.Key(litellm.HashKey(value))
	if !ok {
		t.Fatal("LiteLLM holds no key for the Secret's value")
	}
	meta, _ := row["metadata"].(map[string]any)
	if row["key_alias"] != "key-create" || fmt.Sprint(row["models"]) != "[m-a m-b]" ||
		row["max_budget"] != 5.0 || row["budget_duration"] != keyTestPeriod || row["rpm_limit"] != 60.0 ||
		row["tpm_limit"] != nil || meta["managed-by"] != "alitellm-operator" || meta["owner-uid"] != string(got.UID) {
		t.Errorf("stored key: %v", row)
	}
	if lr := got.Status.LastRendered; lr == nil || lr.KeyAlias != "key-create" || lr.Hash == "" {
		t.Errorf("lastRendered: %+v", lr)
	}
}

func TestKey_SpecChange_UpdatesAndClears(t *testing.T) {
	ctx := context.Background()
	keyTestSetup(t, "key-update")
	if err := k8sClient.Create(ctx, keyCR("key-update")); err != nil {
		t.Fatal(err)
	}
	got := pollKeyReason(t, ctx, "key-update", reasonSynced)
	hash := litellm.HashKey(keyValue(t, ctx, "key-update"))

	got.Spec.Models = []string{"m-c"}
	got.Spec.Budget = nil
	if err := k8sClient.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	waitKey(t, "key updated in LiteLLM", func() bool {
		row, _ := mockServer.Key(hash)
		return fmt.Sprint(row["models"]) == "[m-c]" && row["max_budget"] == nil && row["budget_duration"] == nil
	})
	if litellm.HashKey(keyValue(t, ctx, "key-update")) != hash {
		t.Error("a spec change must not rotate the key value")
	}
}

func TestKey_LostInLiteLLM_RegeneratedWithSameValue(t *testing.T) {
	ctx := context.Background()
	keyTestSetup(t, "key-lost")
	if err := k8sClient.Create(ctx, keyCR("key-lost")); err != nil {
		t.Fatal(err)
	}
	pollKeyReason(t, ctx, "key-lost", reasonSynced)
	hash := litellm.HashKey(keyValue(t, ctx, "key-lost"))

	mockServer.DeleteKeyOutOfBand(hash)
	pokeKey(t, ctx, "key-lost")
	waitKey(t, "key regenerated", func() bool { _, ok := mockServer.Key(hash); return ok })
	if litellm.HashKey(keyValue(t, ctx, "key-lost")) != hash {
		t.Error("the Secret value changed; pods would lose access")
	}
}

func TestKey_SecretDeleted_Rotates(t *testing.T) {
	ctx := context.Background()
	keyTestSetup(t, "key-rotate")
	if err := k8sClient.Create(ctx, keyCR("key-rotate")); err != nil {
		t.Fatal(err)
	}
	pollKeyReason(t, ctx, "key-rotate", reasonSynced)
	old := litellm.HashKey(keyValue(t, ctx, "key-rotate"))

	s, _ := keySecret(ctx, "key-rotate")
	if err := k8sClient.Delete(ctx, s); err != nil {
		t.Fatal(err)
	}
	pokeKey(t, ctx, "key-rotate")
	waitKey(t, "rotated key", func() bool {
		s, err := keySecret(ctx, "key-rotate")
		if err != nil {
			return false
		}
		_, ok := mockServer.Key(litellm.HashKey(string(s.Data["api-key"])))
		return ok && litellm.HashKey(string(s.Data["api-key"])) != old
	})
	if _, ok := mockServer.Key(old); ok {
		t.Error("the old key is still valid in LiteLLM")
	}
	if n := len(mockServer.KeyHashesByAlias("key-rotate")); n != 1 {
		t.Errorf("want one key for the alias, got %d", n)
	}
}

func TestKey_AliasHeldByForeignKey_Conflict(t *testing.T) {
	ctx := context.Background()
	keyTestSetup(t, "key-foreign")
	const foreign = "f00df00df00df00df00df00df00df00df00df00df00df00df00df00df00df00d"
	mockServer.SeedKey(foreign, map[string]any{"key_alias": "key-foreign", "models": []any{"x"}})

	if err := k8sClient.Create(ctx, keyCR("key-foreign")); err != nil {
		t.Fatal(err)
	}
	pollKeyReason(t, ctx, "key-foreign", reasonKeyAliasConflict)
	if _, ok := mockServer.Key(foreign); !ok {
		t.Error("a key the operator did not create was deleted")
	}
}

func TestKey_UnownedSecret_NotTouched(t *testing.T) {
	ctx := context.Background()
	keyTestSetup(t, "key-unowned")
	pre := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "key-unowned-secret", Namespace: keySecretNS},
		Data:       map[string][]byte{"api-key": []byte("sk-someone-elses-secret")},
	}
	if err := k8sClient.Create(ctx, pre); err != nil {
		t.Fatal(err)
	}
	before := mockServer.PathCallCount("/key/generate") + mockServer.PathCallCount("/key/info")

	if err := k8sClient.Create(ctx, keyCR("key-unowned")); err != nil {
		t.Fatal(err)
	}
	pollKeyReason(t, ctx, "key-unowned", reasonSecretNotOwned)
	if after := mockServer.PathCallCount("/key/generate") + mockServer.PathCallCount("/key/info"); after != before {
		t.Errorf("LiteLLM was called for a Secret the operator does not own (%d calls)", after-before)
	}
	if v := keyValue(t, ctx, "key-unowned"); v != "sk-someone-elses-secret" {
		t.Error("the unowned Secret was modified")
	}
}

func TestKey_Delete_RemovesKeyAndSecret(t *testing.T) {
	for _, policy := range []string{"Delete", "Orphan"} {
		t.Run(policy, func(t *testing.T) {
			ctx := context.Background()
			name := "key-del-" + strings.ToLower(policy)
			keyTestSetup(t, name)
			cr := keyCR(name)
			cr.Spec.DeletionPolicy = policy
			if err := k8sClient.Create(ctx, cr); err != nil {
				t.Fatal(err)
			}
			got := pollKeyReason(t, ctx, name, reasonSynced)
			hash := litellm.HashKey(keyValue(t, ctx, name))

			if err := k8sClient.Delete(ctx, got); err != nil {
				t.Fatal(err)
			}
			waitKey(t, "CR, key and Secret gone", func() bool {
				var k litellmv1alpha1.LiteLLMKey
				crGone := apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(cr), &k))
				_, keyLive := mockServer.Key(hash)
				_, serr := keySecret(ctx, name)
				return crGone && !keyLive && apierrors.IsNotFound(serr)
			})
		})
	}
}

func TestKey_ValueNeverLeaks(t *testing.T) {
	ctx := context.Background()
	keyTestSetup(t, "key-leak")
	if err := k8sClient.Create(ctx, keyCR("key-leak")); err != nil {
		t.Fatal(err)
	}
	got := pollKeyReason(t, ctx, "key-leak", reasonSynced)
	value := keyValue(t, ctx, "key-leak")

	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), value) {
		t.Error("key value found in the LiteLLMKey object")
	}
	var events corev1.EventList
	if err := k8sClient.List(ctx, &events, client.InNamespace(WatchNamespace)); err != nil {
		t.Fatal(err)
	}
	for _, e := range events.Items {
		if strings.Contains(e.Message, value) {
			t.Errorf("key value found in event %s", e.Name)
		}
	}
	sawGenerate := false
	for _, c := range mockServer.Recorded() {
		if strings.Contains(c.Path+"?"+c.Query, value) {
			t.Errorf("key value found in URL of %s %s", c.Method, c.Path)
		}
		if strings.Contains(string(c.Body), value) {
			if c.Path != "/key/generate" {
				t.Errorf("key value found in body of %s %s", c.Method, c.Path)
			}
			sawGenerate = true
		}
	}
	if !sawGenerate {
		t.Error("expected the value in the /key/generate body")
	}
}
