// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"reflect"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
)

// TestModelAlias_Envtest_HappyPath_MultiEntry creates a CR with multiple
// spec.aliases entries and asserts:
//   - status.conditions[Ready] = True, reason=Synced.
//   - status.aliasStatuses has one row per entry, all Applied=true with
//     AppliedValue matching the spec.
//   - mock recorded the merged map under
//     router_settings.model_group_alias.
//
// NOTE: requires LiteLLMConnection/default Synced before the alias is
// created. We bootstrap the connection by creating the CR and waiting on
// connCache.Snapshot via pollSnapshotReason (the same helper the other
// happy-path envtests use).
func TestModelAlias_Envtest_HappyPath_MultiEntry(t *testing.T) {
	ctx := context.Background()
	ns := WatchNamespace

	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		time.Sleep(50 * time.Millisecond)
	})
	snap := pollSnapshotReason(30*time.Second, reasonSynced)
	if snap.Reason != reasonSynced {
		t.Fatalf("LiteLLMConnection not Synced within 30s; got reason=%q", snap.Reason)
	}

	a := &litellmv1alpha1.LiteLLMModelAlias{
		ObjectMeta: metav1.ObjectMeta{Name: "ack-bundle", Namespace: ns},
		Spec: litellmv1alpha1.LiteLLMModelAliasSpec{
			Aliases: []litellmv1alpha1.ModelAliasEntry{
				{Name: "ackstorm.smart", Value: "GEMINI.gemini-3-pro-preview"},
				{Name: "ackstorm.fast", Value: "GEMINI.gemini-3-flash"},
			},
		},
	}
	if err := k8sClient.Create(ctx, a); err != nil {
		t.Fatalf("create LiteLLMModelAlias: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), a)
	})

	deadline := time.After(30 * time.Second)
	for {
		select {
		case <-deadline:
			var got litellmv1alpha1.LiteLLMModelAlias
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: a.Name, Namespace: ns}, &got)
			t.Fatalf("timed out waiting for Ready=True; last status=%+v", got.Status)
		case <-time.After(500 * time.Millisecond):
			var got litellmv1alpha1.LiteLLMModelAlias
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: a.Name, Namespace: ns}, &got); err != nil {
				continue
			}
			ready := false
			for _, c := range got.Status.Conditions {
				if c.Type == conditionTypeReady && c.Status == metav1.ConditionTrue && c.Reason == reasonSynced {
					ready = true
				}
			}
			if !ready {
				continue
			}
			if len(got.Status.AliasStatuses) != 2 {
				t.Fatalf("expected 2 alias statuses, got %d (full=%+v)", len(got.Status.AliasStatuses), got.Status.AliasStatuses)
			}
			for _, s := range got.Status.AliasStatuses {
				if !s.Applied {
					t.Fatalf("entry %q not applied: %+v", s.Name, s)
				}
				if s.AppliedValue == "" {
					t.Fatalf("entry %q missing AppliedValue: %+v", s.Name, s)
				}
			}
			got2 := mockServer.ModelGroupAlias()
			if got2["ackstorm.smart"] != "GEMINI.gemini-3-pro-preview" {
				t.Fatalf("mock alias map missing ackstorm.smart: %v", got2)
			}
			if got2["ackstorm.fast"] != "GEMINI.gemini-3-flash" {
				t.Fatalf("mock alias map missing ackstorm.fast: %v", got2)
			}
			return
		}
	}
}

// TestModelAlias_Envtest_NameCharset_AdmissionValidation exercises the
// CRD `spec.aliases[].name` pattern at the apiserver admission layer
// (no reconcile / connection needed — validation runs before the
// controller sees the object). Real LiteLLM model identifiers must be
// accepted as alias names (square brackets for context-window variants,
// colons for tags, at-signs for version pins); whitespace must stay
// rejected. Guards the relaxed pattern
// `^[A-Za-z0-9][A-Za-z0-9._:/@+\[\]-]{0,252}$` against regression.
func TestModelAlias_Envtest_NameCharset_AdmissionValidation(t *testing.T) {
	ctx := context.Background()
	ns := WatchNamespace

	mkCR := func(objName, aliasName string) *litellmv1alpha1.LiteLLMModelAlias {
		return &litellmv1alpha1.LiteLLMModelAlias{
			ObjectMeta: metav1.ObjectMeta{Name: objName, Namespace: ns},
			Spec: litellmv1alpha1.LiteLLMModelAliasSpec{
				Aliases: []litellmv1alpha1.ModelAliasEntry{
					{Name: aliasName, Value: "ANTHROPIC.claude-opus-4-8"},
				},
			},
		}
	}

	accepted := []struct{ objName, aliasName string }{
		{"charset-brackets", "claude-opus-4-8[1m]"}, // the reported real model name
		{"charset-colon", "ollama/llama3:8b"},       // provider tag
		{"charset-at", "gpt-4@2024-08-06"},          // version pin
		{"charset-plain", "ackstorm.smart"},         // unchanged classic form
	}
	for _, tc := range accepted {
		cr := mkCR(tc.objName, tc.aliasName)
		if err := k8sClient.Create(ctx, cr); err != nil {
			t.Errorf("alias name %q should be ACCEPTED, got admission error: %v", tc.aliasName, err)
			continue
		}
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), cr) })
	}

	rejected := []struct{ objName, aliasName string }{
		{"charset-space", "bad name"},          // whitespace
		{"charset-leadingdot", ".leading-dot"}, // must start alphanumeric
	}
	for _, tc := range rejected {
		cr := mkCR(tc.objName, tc.aliasName)
		err := k8sClient.Create(ctx, cr)
		if err == nil {
			_ = k8sClient.Delete(context.Background(), cr)
			t.Errorf("alias name %q should be REJECTED by the pattern, but admission accepted it", tc.aliasName)
			continue
		}
		if !apierrors.IsInvalid(err) {
			t.Errorf("alias name %q rejected with non-validation error: %v", tc.aliasName, err)
		}
	}
}

// A reconcile that finds LiteLLM's alias map already equal to the desired
// one issues no POST /config/update.
func TestModelAlias_Envtest_SteadyState_NoConfigUpdate(t *testing.T) {
	ctx := context.Background()

	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		time.Sleep(50 * time.Millisecond)
	})
	if snap := pollSnapshotReason(30*time.Second, reasonSynced); snap.Reason != reasonSynced {
		t.Fatalf("LiteLLMConnection not Synced within 30s; got reason=%q", snap.Reason)
	}

	a := &litellmv1alpha1.LiteLLMModelAlias{
		ObjectMeta: metav1.ObjectMeta{Name: "ack-steady", Namespace: WatchNamespace},
		Spec: litellmv1alpha1.LiteLLMModelAliasSpec{
			Aliases: []litellmv1alpha1.ModelAliasEntry{{Name: "ackstorm.steady", Value: "GEMINI.gemini-3-flash"}},
		},
	}
	if err := k8sClient.Create(ctx, a); err != nil {
		t.Fatalf("create LiteLLMModelAlias: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), a) })
	waitFor(t, func() bool { return mockServer.ModelGroupAlias()["ackstorm.steady"] != "" },
		"alias never written to LiteLLM")

	posts := func() int64 { return mockServer.PathCallCount("/config/update") }
	gets := func() int64 { return mockServer.PathCallCount("/get/config/callbacks") }
	before := waitQuiet(t, posts, time.Second, 20*time.Second)
	getsBefore := gets()

	key := types.NamespacedName{Name: a.Name, Namespace: WatchNamespace}
	var latest litellmv1alpha1.LiteLLMModelAlias
	if err := updateWithRetry(ctx, key, &latest, func(a *litellmv1alpha1.LiteLLMModelAlias) error {
		if a.Annotations == nil {
			a.Annotations = map[string]string{}
		}
		a.Annotations["test.litellm.ackstorm.ai/trigger"] = "1"
		return nil
	}); err != nil {
		t.Fatalf("annotate to trigger reconcile: %v", err)
	}
	waitFor(t, func() bool { return gets() > getsBefore }, "no alias reconcile after the touch")
	if after := waitQuiet(t, posts, time.Second, 20*time.Second); after != before {
		t.Fatalf("POST /config/update on an unchanged alias map: %d -> %d", before, after)
	}
}

// An alias's fallbacks land in router_settings.fallbacks keyed by the alias
// name, replace an entry no CR declares, and go away with the CR.
func TestModelAlias_Envtest_Fallbacks(t *testing.T) {
	ctx := context.Background()

	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		time.Sleep(50 * time.Millisecond)
	})
	if snap := pollSnapshotReason(30*time.Second, reasonSynced); snap.Reason != reasonSynced {
		t.Fatalf("LiteLLMConnection not Synced within 30s; got reason=%q", snap.Reason)
	}

	foreign := map[string]any{"real-model": []any{"other-model"}}
	setFallbacks := func(v []any) {
		rs, err := suiteLLMClient.GetRouterSettings(context.Background())
		if err != nil {
			t.Fatalf("GET router settings: %v", err)
		}
		rs.Extra["fallbacks"] = v
		if err := suiteLLMClient.UpdateRouterSettings(context.Background(), rs); err != nil {
			t.Fatalf("POST router settings: %v", err)
		}
	}
	setFallbacks([]any{foreign})
	t.Cleanup(func() { setFallbacks([]any{}) })
	fallbacksAre := func(want ...any) func() bool {
		return func() bool {
			rs, err := suiteLLMClient.GetRouterSettings(ctx)
			got, _ := rs.Extra["fallbacks"].([]any)
			return err == nil && len(got) == len(want) && (len(want) == 0 || reflect.DeepEqual(got, want))
		}
	}

	a := &litellmv1alpha1.LiteLLMModelAlias{
		ObjectMeta: metav1.ObjectMeta{Name: "ack-fallback", Namespace: WatchNamespace},
		Spec: litellmv1alpha1.LiteLLMModelAliasSpec{
			Aliases: []litellmv1alpha1.ModelAliasEntry{{
				Name: "ackstorm.super", Value: "gemini.argon", Fallbacks: []string{"gemini.carbon", "anthropic.sonnet"},
			}},
		},
	}
	if err := k8sClient.Create(ctx, a); err != nil {
		t.Fatalf("create LiteLLMModelAlias: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), a) })
	waitFor(t, fallbacksAre(map[string]any{"ackstorm.super": []any{"gemini.carbon", "anthropic.sonnet"}}),
		"router_settings.fallbacks is not exactly the alias's entry")

	posts := func() int64 { return mockServer.PathCallCount("/config/update") }
	gets := func() int64 { return mockServer.PathCallCount("/get/config/callbacks") }
	before := waitQuiet(t, posts, time.Second, 20*time.Second)
	getsBefore := gets()
	var latest litellmv1alpha1.LiteLLMModelAlias
	if err := updateWithRetry(ctx, client.ObjectKeyFromObject(a), &latest, func(a *litellmv1alpha1.LiteLLMModelAlias) error {
		a.Annotations = map[string]string{"test.litellm.ackstorm.ai/trigger": "1"}
		return nil
	}); err != nil {
		t.Fatalf("annotate to trigger reconcile: %v", err)
	}
	waitFor(t, func() bool { return gets() > getsBefore }, "no alias reconcile after the touch")
	if after := waitQuiet(t, posts, time.Second, 20*time.Second); after != before {
		t.Fatalf("POST /config/update with fallbacks already in place: %d -> %d", before, after)
	}

	if err := k8sClient.Delete(ctx, a); err != nil {
		t.Fatalf("delete LiteLLMModelAlias: %v", err)
	}
	waitFor(t, fallbacksAre(), "alias fallbacks not removed with the CR")
}
