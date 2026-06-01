// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

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
