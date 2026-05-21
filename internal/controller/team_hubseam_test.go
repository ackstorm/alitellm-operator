// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
)

// TestTeamHubSeam_AC_DC1_VirtualKeysCoexist exercises the TEAM-09 + AC-DC1
// Hub seam invariant. Apply Team/engineering (max_budget=500.0,
// budget_duration="30d"); wait Ready. Construct a test-local "Hub ledger"
// of virtualKeyID → team_id (NOT touched by the operator). Trigger a
// spurious operator reconcile via annotation bump. Assert:
//
// 1. mock.LastTeamBody("engineering").max_budget == 500.0 (budget
// preserved across the re-reconcile).
// 2. The test-local hubLedger is structurally unchanged (the operator
// has no code path that mutates it; this is the structural guarantee).
// 3. mock.PathCallCount("/user/") == 0 (zero /user/* calls).
// 4. mock.PathCallCount("/key/") == 0 (zero /key/* calls).
func TestTeamHubSeam_AC_DC1_VirtualKeysCoexist(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache() // Phase 6 cross-suite flake fix (07-CONTEXT.md §Phase-6-flake option α)
	ensureNoTeam(t, ctx, "engineering")
	ensureNoTeam(t, ctx, "hubseam-trigger")
	resetConnCacheSnapshot()

	// Capture baseline PathCallCount before the LiteLLMConnection setup
	// (which probes GET /models — separate from the /user/* and /key/*
	// surfaces we care about here).
	priorUserCalls := mockServer.PathCallCount("/user/")
	priorKeyCalls := mockServer.PathCallCount("/key/")

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		cleanupConn()
		ensureNoTeam(t, context.Background(), "engineering")
		ensureNoTeam(t, context.Background(), "hubseam-trigger")
	})

	// ─── Step 1: Apply Team/engineering with budget ──────────────────────
	cr := &litellmv1alpha1.LiteLLMTeam{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "engineering",
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.TeamSpec{
			Budget: &litellmv1alpha1.BudgetSpec{
				Limit:  floatPtr(500.0),
				Period: "30d",
			},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team/engineering: %v", err)
	}

	tm := pollTeamCondition(t, ctx, "engineering", reasonSynced, 30*time.Second)
	if tm.Status.LastRendered.TeamID == "" {
		t.Fatalf("Team/engineering not Synced within 30s; conditions=%+v", tm.Status.Conditions)
	}
	engineeringTeamID := tm.Status.LastRendered.TeamID

	// Verify the budget was projected to LiteLLM.
	bodyAfterCreate := mockServer.LastTeamBody("engineering")
	if bodyAfterCreate == nil {
		t.Fatalf("mock.LastTeamBody(\"engineering\") is nil after Synced")
	}
	if v, _ := bodyAfterCreate["max_budget"].(float64); v != 500.0 {
		t.Fatalf("budget projection: want max_budget=500.0, got %v", bodyAfterCreate["max_budget"])
	}
	if v, _ := bodyAfterCreate["budget_duration"].(string); v != "30d" {
		t.Fatalf("budget projection: want budget_duration=\"30d\", got %v", bodyAfterCreate["budget_duration"])
	}

	// ─── Step 2: Construct a test-local Hub ledger ────────────────────────
	//
	// The operator NEVER touches this map. It lives in test memory only.
	// This is the structural guarantee: TEAM-09 says external identity system owns
	// User/VirtualKey/team-membership and the operator does not enumerate
	// it. The test memorializes that "structurally untouchable" property
	// by holding the ledger entirely outside the operator's reach.
	hubLedger := map[string]string{
		"vk_test_1": engineeringTeamID,
		"vk_test_2": engineeringTeamID,
	}
	hubLedgerSizeBefore := len(hubLedger)

	// ─── Step 3: Trigger a spurious operator reconcile ───────────────────
	//
	// Apply a DIFFERENT Team CR (hubseam-trigger) to force the operator
	// to run its reconcile loop without trivially involving engineering.
	// This is more aggressive than a self-annotation bump — it proves
	// cross-CR reconciliation events don't bleed into engineering's
	// state.
	trigger := &litellmv1alpha1.LiteLLMTeam{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "hubseam-trigger",
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.TeamSpec{},
	}
	if err := k8sClient.Create(ctx, trigger); err != nil {
		t.Fatalf("create Team/hubseam-trigger: %v", err)
	}
	triggerCR := pollTeamCondition(t, ctx, "hubseam-trigger", reasonSynced, 30*time.Second)
	if triggerCR.Status.LastRendered.TeamID == "" {
		t.Fatalf("Team/hubseam-trigger not Synced within 30s")
	}

	// Also annotation-bump engineering directly so the engineering
	// reconciler runs the full Step-1-through-11 path (including the
	// hash-equal short-circuit check, which would re-write status if
	// anything had drifted).
	if tm.Annotations == nil {
		tm.Annotations = map[string]string{}
	}
	tm.Annotations["test.litellm.ackstorm.ai/hubseam-trigger"] = time.Now().Format(time.RFC3339Nano)
	if err := k8sClient.Update(ctx, tm); err != nil {
		t.Fatalf("annotation-bump engineering: %v", err)
	}

	// Safety margin for any cross-CR cascade.
	time.Sleep(2 * time.Second)

	// ─── Step 4: Assert budget preserved ──────────────────────────────────
	bodyAfterReconcile := mockServer.LastTeamBody("engineering")
	if bodyAfterReconcile == nil {
		t.Fatalf("mock.LastTeamBody(\"engineering\") is nil after re-reconcile")
	}
	if v, _ := bodyAfterReconcile["max_budget"].(float64); v != 500.0 {
		t.Errorf("budget MUTATED across re-reconcile: max_budget was 500.0, now %v (operator should NOT drop CR spec on Hub-side virtual-key creation)",
			bodyAfterReconcile["max_budget"])
	}
	if v, _ := bodyAfterReconcile["budget_duration"].(string); v != "30d" {
		t.Errorf("budget MUTATED across re-reconcile: budget_duration was \"30d\", now %v",
			bodyAfterReconcile["budget_duration"])
	}

	// Verify the engineering team_id did NOT change (the operator MUST
	// preserve the LiteLLM-assigned UUID across re-reconciles per Phase
	// 3 D-04 / Phase 5 D-02 ID-pin family).
	var post litellmv1alpha1.LiteLLMTeam
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "engineering", Namespace: WatchNamespace}, &post); err != nil {
		t.Fatalf("re-Get engineering: %v", err)
	}
	if post.Status.LastRendered.TeamID != engineeringTeamID {
		t.Errorf("engineering team_id CHANGED across reconcile: was %q, now %q (ID pin broken)",
			engineeringTeamID, post.Status.LastRendered.TeamID)
	}
	c := apimeta.FindStatusCondition(post.Status.Conditions, "Ready")
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("engineering Ready condition not True after re-reconcile: %+v", c)
	}

	// ─── Step 5: Assert hubLedger structurally unchanged ──────────────────
	//
	// Trivially true since the operator has no handle to hubLedger — the
	// assertion documents the invariant rather than proving it. The
	// LOAD-BEARING assertions are Step 6 below (zero /user/ + /key/
	// calls); this one is for narrative clarity.
	if len(hubLedger) != hubLedgerSizeBefore {
		t.Errorf("hubLedger size changed across reconcile: was %d, now %d (impossible — operator has no handle)",
			hubLedgerSizeBefore, len(hubLedger))
	}
	if hubLedger["vk_test_1"] != engineeringTeamID || hubLedger["vk_test_2"] != engineeringTeamID {
		t.Errorf("hubLedger entries mutated: %+v (impossible)", hubLedger)
	}

	// ─── Step 6: LOAD-BEARING zero /user/* and /key/* call assertion ─────
	//
	// TEAM-09 + SCOPE-03 team slice + AC-DC1 Hub seam negative invariant.
	// The operator's reconciliation path MUST NOT generate any traffic
	// to externally-owned routes. Phase 7 will sweep this
	// invariant across every CRD; this test is the Team slice.
	if got := mockServer.PathCallCount("/user/") - priorUserCalls; got != 0 {
		t.Errorf("AC-DC1 + TEAM-09 violation: operator issued %d new /user/* call(s) during Team reconciliation (want 0)",
			got)
	}
	if got := mockServer.PathCallCount("/key/") - priorKeyCalls; got != 0 {
		t.Errorf("AC-DC1 + TEAM-09 violation: operator issued %d new /key/* call(s) during Team reconciliation (want 0)",
			got)
	}
}
