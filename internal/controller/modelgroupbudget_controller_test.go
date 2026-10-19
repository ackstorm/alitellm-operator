// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"net/http"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
)

const budgetPollTimeout = 30 * time.Second

func resetMockBudgets() {
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetModelGroupBudgets()
}

func budgetCR(name, group string, limit float64) *litellmv1alpha1.LiteLLMModelGroupBudget {
	return &litellmv1alpha1.LiteLLMModelGroupBudget{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: WatchNamespace},
		Spec:       litellmv1alpha1.ModelGroupBudgetSpec{ModelGroup: group, Limit: &limit, Period: "30d"},
	}
}

func ensureNoBudget(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	var b litellmv1alpha1.LiteLLMModelGroupBudget
	if err := k8sClient.Get(ctx, key, &b); apierrors.IsNotFound(err) {
		return
	}
	b.Finalizers = nil
	_ = k8sClient.Update(ctx, &b)
	_ = k8sClient.Delete(ctx, &b)
	deadline := time.Now().Add(budgetPollTimeout)
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, key, &b); apierrors.IsNotFound(err) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("budget %s not removed", name)
}

func pollBudgetReason(t *testing.T, ctx context.Context, name, want string) *litellmv1alpha1.LiteLLMModelGroupBudget {
	t.Helper()
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	deadline := time.Now().Add(budgetPollTimeout)
	var b litellmv1alpha1.LiteLLMModelGroupBudget
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, key, &b); err == nil {
			if c := apimeta.FindStatusCondition(b.Status.Conditions, conditionTypeReady); c != nil && c.Reason == want &&
				b.Status.ObservedGeneration == b.Generation {
				return &b
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("budget %s never reached reason %s; conditions=%v", name, want, b.Status.Conditions)
	return nil
}

func budgetTestSetup(t *testing.T, names ...string) {
	ctx := context.Background()
	resetMockBudgets()
	for _, n := range names {
		ensureNoBudget(t, ctx, n)
	}
	resetConnCacheSnapshot()
	cleanupConn := setupReadyConnectionAccessGroup(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		setConnCacheReady()
		for _, n := range names {
			ensureNoBudget(t, context.Background(), n)
		}
		mockServer.ResetModelGroupBudgets()
	})
}

func TestModelGroupBudget_CreateUpdateDelete(t *testing.T) {
	ctx := context.Background()
	budgetTestSetup(t, "mgb-crud")
	mockServer.SeedModelGroup("tag-crud")

	soft := 4.0
	cr := budgetCR("mgb-crud", "tag-crud", 5)
	cr.Spec.SoftLimit = &soft
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatal(err)
	}
	got := pollBudgetReason(t, ctx, "mgb-crud", reasonSynced)
	b, ok := mockServer.ModelGroupBudget("tag-crud")
	if !ok || b["max_budget"] != 5.0 || b["soft_budget"] != 4.0 || b["budget_duration"] != "30d" { //nolint:goconst // fixture value shared with team budget tests
		t.Fatalf("stored budget: %v", b)
	}

	// Removing softLimit must clear it in LiteLLM (DELETE-then-PUT).
	got.Spec.SoftLimit = nil
	if err := k8sClient.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	got = pollBudgetReason(t, ctx, "mgb-crud", reasonSynced)
	if b, _ := mockServer.ModelGroupBudget("tag-crud"); b["soft_budget"] != nil {
		t.Errorf("soft_budget not cleared: %v", b)
	}

	if err := k8sClient.Delete(ctx, got); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(budgetPollTimeout)
	for time.Now().Before(deadline) {
		if _, ok := mockServer.ModelGroupBudget("tag-crud"); !ok {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("budget still present in LiteLLM after CR delete")
}

func TestModelGroupBudget_UnknownTagParks(t *testing.T) {
	ctx := context.Background()
	budgetTestSetup(t, "mgb-unknown")
	if err := k8sClient.Create(ctx, budgetCR("mgb-unknown", "tag-nobody", 1)); err != nil {
		t.Fatal(err)
	}
	pollBudgetReason(t, ctx, "mgb-unknown", reasonModelGroupNotFound)
}

func TestModelGroupBudget_DuplicateParksAndDeleteSendsNothing(t *testing.T) {
	ctx := context.Background()
	budgetTestSetup(t, "mgb-owner", "mgb-dup")
	mockServer.SeedModelGroup("tag-dup")

	if err := k8sClient.Create(ctx, budgetCR("mgb-owner", "tag-dup", 5)); err != nil {
		t.Fatal(err)
	}
	pollBudgetReason(t, ctx, "mgb-owner", reasonSynced)
	time.Sleep(1100 * time.Millisecond) // creationTimestamp has 1s resolution
	if err := k8sClient.Create(ctx, budgetCR("mgb-dup", "tag-dup", 99)); err != nil {
		t.Fatal(err)
	}
	dup := pollBudgetReason(t, ctx, "mgb-dup", reasonDuplicateModelGroup)
	if b, _ := mockServer.ModelGroupBudget("tag-dup"); b["max_budget"] != 5.0 {
		t.Fatalf("duplicate overwrote the owner's budget: %v", b)
	}

	if err := k8sClient.Delete(ctx, dup); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(dup)
	deadline := time.Now().Add(budgetPollTimeout)
	for {
		var check litellmv1alpha1.LiteLLMModelGroupBudget
		if err := k8sClient.Get(ctx, key, &check); apierrors.IsNotFound(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("duplicate CR still present %s after delete", budgetPollTimeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, ok := mockServer.ModelGroupBudget("tag-dup"); !ok {
		t.Fatal("deleting the duplicate removed the owner's budget")
	}
	for _, c := range mockServer.Recorded() {
		if c.Method == http.MethodDelete && c.Path == "/access_group/tag-dup/budget" {
			t.Fatalf("deleting the duplicate sent %s %s", c.Method, c.Path)
		}
	}
}

func TestModelGroupBudget_HealsOutOfBandEdit(t *testing.T) {
	ctx := context.Background()
	enableSuiteRelist(t)
	budgetTestSetup(t, "mgb-drift")
	mockServer.SeedModelGroup("tag-drift")
	if err := k8sClient.Create(ctx, budgetCR("mgb-drift", "tag-drift", 5)); err != nil {
		t.Fatal(err)
	}
	pollBudgetReason(t, ctx, "mgb-drift", reasonSynced)
	mockServer.SetModelGroupBudgetOutOfBand("tag-drift", map[string]any{"max_budget": 1000.0})
	deadline := time.Now().Add(budgetPollTimeout)
	for time.Now().Before(deadline) {
		if b, _ := mockServer.ModelGroupBudget("tag-drift"); b["max_budget"] == 5.0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("safety relist never restored max_budget=5")
}
