// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// suiteCatalogConfig is the catalog the suite's ModelAliasReconciler writes.
// Same namespace as the CRs: the envtest cache knows only WatchNamespace, and
// the cross-namespace write is what e2e CAT-01 covers.
var suiteCatalogConfig = CatalogConfig{
	ConfigMapNamespace: WatchNamespace,
	ConfigMapName:      "opencode-catalog",
	APIBase:            "https://api.test.invalid/v1",
}

// catalogNSOverride, when set, redirects every catalog ConfigMap write to
// that namespace. It is the test seam behind catalogTestWriter.
var catalogNSOverride atomic.Pointer[string]

// catalogTestWriter is the suite's CatalogWriter: the uncached client, with
// Get/Create/Update retargeted at catalogNSOverride when one is set, so a
// test can point the write at a namespace that does not exist without
// racing the reconcile goroutine on r.Catalog.
type catalogTestWriter struct{ client.Client }

func (w catalogTestWriter) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if ns := catalogNSOverride.Load(); ns != nil {
		key.Namespace = *ns
	}
	return w.Client.Get(ctx, key, obj, opts...)
}

func (w catalogTestWriter) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if ns := catalogNSOverride.Load(); ns != nil {
		obj.SetNamespace(*ns)
	}
	return w.Client.Create(ctx, obj, opts...)
}

func (w catalogTestWriter) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if ns := catalogNSOverride.Load(); ns != nil {
		obj.SetNamespace(*ns)
	}
	return w.Client.Update(ctx, obj, opts...)
}

// catalogAliasSetup brings LiteLLMConnection/default to Synced and creates
// one alias CR pointing at target; both are cleaned up with t.
func catalogAliasSetup(t *testing.T, ctx context.Context, crName, alias, target string) *litellmv1alpha1.LiteLLMModelAlias {
	t.Helper()
	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), connCR)
		time.Sleep(50 * time.Millisecond)
	})
	if snap := pollSnapshotReason(30*time.Second, reasonSynced); snap.Reason != reasonSynced {
		t.Fatalf("LiteLLMConnection not Synced within 30s; got reason=%q", snap.Reason)
	}

	a := &litellmv1alpha1.LiteLLMModelAlias{
		ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: WatchNamespace},
		Spec: litellmv1alpha1.LiteLLMModelAliasSpec{
			Aliases: []litellmv1alpha1.ModelAliasEntry{{Name: alias, Value: target}},
		},
	}
	if err := k8sClient.Create(ctx, a); err != nil {
		t.Fatalf("create LiteLLMModelAlias: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), a) })
	return a
}

// pollAliasReady blocks until the alias CR reports Ready=True/Synced.
func pollAliasReady(t *testing.T, ctx context.Context, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var got litellmv1alpha1.LiteLLMModelAlias
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: WatchNamespace}, &got); err == nil {
			for _, c := range got.Status.Conditions {
				if c.Type == conditionTypeReady && c.Status == metav1.ConditionTrue && c.Reason == reasonSynced {
					return
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("alias %q not Ready=True/Synced within %s", name, timeout)
}

// TestModelAlias_Envtest_CatalogConfigMapWritten — an alias whose target
// is a vision-capable chat model lands in the catalog ConfigMap with
// attachment=true, read from the TARGET's model_info as served by the mock.
func TestModelAlias_Envtest_CatalogConfigMapWritten(t *testing.T) {
	ctx := context.Background()
	const alias, target = "ackstorm.vision", "GEMINI.gemini-3-pro-preview"

	mockServer.ResetModels()
	mockServer.SeedModel(target, map[string]any{
		"mode":                      "chat",
		"supports_vision":           true,
		"supports_function_calling": true,
	})
	t.Cleanup(mockServer.ResetModels)

	// A ConfigMap left by an earlier pass must not satisfy this test.
	_ = k8sClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: suiteCatalogConfig.ConfigMapName, Namespace: WatchNamespace}})

	a := catalogAliasSetup(t, ctx, "catalog-vision", alias, target)
	pollAliasReady(t, ctx, a.Name, 30*time.Second)

	// The write happens AFTER the Ready status lands, so poll for the key.
	var doc map[string]map[string]any
	deadline := time.Now().Add(15 * time.Second)
	for {
		var cm corev1.ConfigMap
		err := k8sClient.Get(ctx, types.NamespacedName{
			Name: suiteCatalogConfig.ConfigMapName, Namespace: WatchNamespace}, &cm)
		if err == nil {
			if raw, ok := cm.Data[catalogConfigMapKey]; ok {
				if err := json.Unmarshal([]byte(raw), &doc); err != nil {
					t.Fatalf("api.json is not valid JSON: %v\n%s", err, raw)
				}
				if models, ok := doc[catalogProviderID]["models"].(map[string]any); ok && models[alias] != nil {
					break
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("catalog ConfigMap missing %q within 15s (get err=%v, data=%v)", alias, err, doc)
		}
		time.Sleep(200 * time.Millisecond)
	}

	provider := doc[catalogProviderID]
	if provider["api"] != suiteCatalogConfig.APIBase {
		t.Errorf("provider.api = %v, want %q", provider["api"], suiteCatalogConfig.APIBase)
	}
	model := provider["models"].(map[string]any)[alias].(map[string]any)
	if model["attachment"] != true {
		t.Errorf("%s.attachment = %v, want true (target supports_vision)", alias, model["attachment"])
	}
	if model["tool_call"] != true {
		t.Errorf("%s.tool_call = %v, want true (target supports_function_calling)", alias, model["tool_call"])
	}
}

// TestModelAlias_Envtest_CatalogWriteFailed_KeepsReady — a failed catalog
// write (ConfigMap aimed at a namespace that does not exist) emits a
// Warning/CatalogWriteFailed Event on the alias and leaves Ready=True/Synced:
// the aliases were already written to LiteLLM, so a secondary artifact
// must not blame them.
func TestModelAlias_Envtest_CatalogWriteFailed_KeepsReady(t *testing.T) {
	ctx := context.Background()
	const alias, target = "ackstorm.orphan", "GEMINI.gemini-3-flash"

	mockServer.ResetModels()
	mockServer.SeedModel(target, map[string]any{"mode": "chat"})
	t.Cleanup(mockServer.ResetModels)

	bogus := "catalog-ns-does-not-exist"
	catalogNSOverride.Store(&bogus)
	t.Cleanup(func() { catalogNSOverride.Store(nil) })

	a := catalogAliasSetup(t, ctx, "catalog-orphan", alias, target)
	pollAliasReady(t, ctx, a.Name, 30*time.Second)

	deadline := time.Now().Add(15 * time.Second)
	for {
		var events corev1.EventList
		if err := k8sClient.List(ctx, &events, client.InNamespace(WatchNamespace)); err != nil {
			t.Fatalf("list events: %v", err)
		}
		for _, e := range events.Items {
			if e.InvolvedObject.Name == a.Name && e.Reason == "CatalogWriteFailed" {
				if e.Type != corev1.EventTypeWarning {
					t.Errorf("CatalogWriteFailed Event type: want Warning, got %q", e.Type)
				}
				if !strings.Contains(e.Message, bogus) {
					t.Errorf("Event message should name the failed namespace %q: %s", bogus, e.Message)
				}
				// Ready must have survived the failed write.
				pollAliasReady(t, ctx, a.Name, 5*time.Second)
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no CatalogWriteFailed Event on %q within 15s", a.Name)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
