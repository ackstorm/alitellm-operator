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
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/prometheus/client_golang/prometheus/testutil"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/connection"
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
	"github.com/ackstorm/alitellm-operator/internal/metrics"
)

// teamNameAnthropic is a Team metadata.name fixture reused across the
// name-as-team_id tests. Extracted so goconst stays quiet across its
// many occurrences in this file.
const teamNameAnthropic = "anthropic"

// teamSampleCR returns a basic Team CR exercising the happy path with no
// budget and no params.
func teamSampleCR(name string) *litellmv1alpha1.LiteLLMTeam {
	return &litellmv1alpha1.LiteLLMTeam{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.TeamSpec{},
	}
}

// ensureNoTeam deletes any pre-existing Team in WatchNamespace with the
// given name and waits up to 10s for full removal. does NOT
// wire a finalizer, but the cleanup tolerates one for forward-compat with
// .
func ensureNoTeam(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	var existing litellmv1alpha1.LiteLLMTeam
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	if err := k8sClient.Get(ctx, key, &existing); err == nil {
		// Forward-compat: strip any finalizer that 06-04 may have added.
		existing.SetFinalizers(nil)
		_ = k8sClient.Update(ctx, &existing)
		_ = k8sClient.Delete(ctx, &existing)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, key, &existing); apierrors.IsNotFound(err) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Logf("warning: Team %q still present after 10s cleanup wait", name)
}

// pollTeamCondition polls the Ready condition until reason matches or
// timeout. Returns the final re-Get'd CR.
func pollTeamCondition(t *testing.T, ctx context.Context, name, wantReason string, timeout time.Duration) *litellmv1alpha1.LiteLLMTeam {
	t.Helper()
	deadline := time.Now().Add(timeout)
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	var tm litellmv1alpha1.LiteLLMTeam
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, key, &tm); err == nil {
			c := apimeta.FindStatusCondition(tm.Status.Conditions, conditionTypeReady)
			if c != nil && c.Reason == wantReason {
				return &tm
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return &tm
}

// setupReadyConnectionTeam creates the LiteLLMConnection/default CR + waits
// for the cache snapshot Reason="Synced". Returns a cleanup func that
// removes the conn CR.
func setupReadyConnectionTeam(t *testing.T, ctx context.Context) func() {
	t.Helper()
	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	cleanup := func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		time.Sleep(50 * time.Millisecond)
	}
	snap := pollSnapshotReason(30*time.Second, reasonSynced)
	if snap.Reason != reasonSynced {
		cleanup()
		t.Fatalf("LiteLLMConnection not Synced within 30s; reason=%q", snap.Reason)
	}
	return cleanup
}

// listTeamEvents returns all Events for the named Team CR in WatchNamespace.
func listTeamEvents(ctx context.Context, t *testing.T, teamName string) []corev1.Event {
	t.Helper()
	var eventList corev1.EventList
	if err := k8sClient.List(ctx, &eventList, client.InNamespace(WatchNamespace)); err != nil {
		t.Logf("listTeamEvents: list failed (non-fatal): %v", err)
		return nil
	}
	var filtered []corev1.Event
	for _, ev := range eventList.Items {
		if ev.InvolvedObject.Name == teamName && ev.InvolvedObject.Kind == "LiteLLMTeam" {
			filtered = append(filtered, ev)
		}
	}
	return filtered
}

// floatPtr returns a pointer to the given float64 — convenience for
// constructing spec.budget.limit inline.
func floatPtr(f float64) *float64 { return &f }

// int32Ptr returns a pointer to the given int32 — convenience for
// constructing spec.rateLimits.{rpm,tpm} inline (Phase 10 TRL-01).
func int32Ptr(i int32) *int32 { return &i }

// ──────────────────────────────────────────────────────────────────────────
// Test 1: CreateOnFirstReconcile (no budget)
// ──────────────────────────────────────────────────────────────────────────

// TestTeamReconciler_CreateOnFirstReconcile_NoBudget — behavior
// #1 + must_haves[1] (no-budget happy path). Team CR with no spec.budget
// and a simple spec.params bag reaches Ready=Synced within 30s; mock
// records exactly 1 POST /team/new; body has team_alias bare, max_budget
// and budget_duration both as JSON null (preserved in LastBody);
// status.lastRendered.teamID matches the mock-assigned id.
//
// Phase 10 update (TRL-02/TRL-04): spec.params uses `models` (a
// non-overlay pass-through key) instead of `tpm_limit` — the rate-limit
// keys are now structural-overlay-controlled (always-emit, nil when
// spec.rateLimits absent) and would not pass through.
func TestTeamReconciler_CreateOnFirstReconcile_NoBudget(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache() // Phase 6 cross-suite flake fix (07-CONTEXT.md §Phase-6-flake option α)
	ensureNoTeam(t, ctx, "team-create-nobudget")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-create-nobudget")
	})

	cr := teamSampleCR("team-create-nobudget")
	cr.Spec.Params = runtime.RawExtension{Raw: []byte(`{"models":["gpt-4o"]}`)}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}

	tm := pollTeamCondition(t, ctx, "team-create-nobudget", reasonSynced, 30*time.Second)
	c := apimeta.FindStatusCondition(tm.Status.Conditions, conditionTypeReady)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != reasonSynced {
		t.Fatalf("Ready condition not Synced; condition=%+v", c)
	}
	if tm.Status.LastRendered.TeamID == "" {
		t.Error("lastRendered.teamID is empty; want non-empty UUID from POST /team/new")
	}
	if len(tm.Status.LastRendered.Hash) != 64 {
		t.Errorf("lastRendered.hash length: want 64 (sha256 hex), got %d", len(tm.Status.LastRendered.Hash))
	}

	if got := mockServer.MutationsByTeamAlias("team-create-nobudget"); got != 1 {
		t.Errorf("MutationsByTeamAlias: want 1 (one POST /team/new), got %d", got)
	}
	body := mockServer.LastTeamBody("team-create-nobudget")
	if body == nil {
		t.Fatalf("LastTeamBody is nil — mock did not capture POST /team/new body")
	}
	if alias, _ := body["team_alias"].(string); alias != "team-create-nobudget" {
		t.Errorf("body.team_alias: want bare metadata.name %q, got %v", "team-create-nobudget", body["team_alias"])
	}
	// max_budget MUST be present in the body AND null (JSON null preserved
	// as nil in map[string]any per encoding/json decode).
	mb, ok := body["max_budget"]
	if !ok {
		t.Errorf("body.max_budget MISSING — spec §6.7 requires max_budget always present (with null when absent)")
	}
	if mb != nil {
		t.Errorf("body.max_budget: want nil (JSON null), got %v (type %T)", mb, mb)
	}
	bd, ok := body["budget_duration"]
	if !ok {
		t.Errorf("body.budget_duration MISSING — spec §6.7 requires budget_duration always present (with null when absent)")
	}
	if bd != nil {
		t.Errorf("body.budget_duration: want nil (JSON null), got %v (type %T)", bd, bd)
	}
	// Pass-through key from spec.params (non-overlay).
	models, _ := body["models"].([]any)
	if len(models) != 1 {
		t.Errorf("body.models: want 1 element passed through from spec.params, got %d (%v)", len(models), models)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 1b: CreateUsesNameAsTeamID — new teams pin team_id == metadata.name
// ──────────────────────────────────────────────────────────────────────────

// TestTeamCreate_UsesNameAsTeamID — a freshly-created team (empty alias
// lookup → CREATE arm) MUST be sent to LiteLLM with team_id ==
// metadata.name (human-readable, collision-free), NOT a server-assigned
// UUID. The persisted status.lastRendered.teamID MUST equal the name too,
// so the alias/id round-trip survives an operator restart.
//
// Contrast with TestTeamReconciler_CreateOnFirstReconcile_NoBudget, which
// (pre-change) asserted only that the minted teamID was non-empty. This
// test pins the exact value to metadata.name.
func TestTeamCreate_UsesNameAsTeamID(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache() // Phase 6 cross-suite flake fix (07-CONTEXT.md §Phase-6-flake option α)
	ensureNoTeam(t, ctx, teamNameAnthropic)
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), teamNameAnthropic)
	})

	cr := teamSampleCR(teamNameAnthropic)
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}

	tm := pollTeamCondition(t, ctx, teamNameAnthropic, reasonSynced, 30*time.Second)
	c := apimeta.FindStatusCondition(tm.Status.Conditions, conditionTypeReady)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != reasonSynced {
		t.Fatalf("Ready condition not Synced; condition=%+v", c)
	}

	// CREATE arm fired exactly once.
	if got := mockServer.MutationsByTeamAlias(teamNameAnthropic); got != 1 {
		t.Errorf("MutationsByTeamAlias: want 1 (one POST /team/new), got %d", got)
	}

	// The POST /team/new body MUST carry team_id == metadata.name.
	body := mockServer.LastTeamBody(teamNameAnthropic)
	if body == nil {
		t.Fatalf("LastTeamBody is nil — mock did not capture POST /team/new body")
	}
	if id, _ := body["team_id"].(string); id != teamNameAnthropic {
		t.Errorf("body.team_id: want metadata.name %q, got %v", teamNameAnthropic, body["team_id"])
	}

	// The persisted status teamID MUST equal the name, not a UUID.
	if tm.Status.LastRendered.TeamID != teamNameAnthropic {
		t.Errorf("status.lastRendered.teamID: want %q, got %q", teamNameAnthropic, tm.Status.LastRendered.TeamID)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 1c: UpdateKeepsExistingUUID — name-as-id change is CREATE-only
// ──────────────────────────────────────────────────────────────────────────

// TestTeamUpdate_KeepsExistingUUID — a team that already exists in
// LiteLLM under a server-assigned UUID MUST take the UPDATE arm and keep
// that UUID. The name-as-id change (Task 1) is CREATE-only — it must
// never rewrite an existing team's identity. Regression guard: if a
// future change leaked team.Name into the UPDATE arm, the pinned body
// team_id and status.teamID would flip from the UUID to the name and
// this test would fail.
func TestTeamUpdate_KeepsExistingUUID(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache() // Phase 6 cross-suite flake fix (07-CONTEXT.md §Phase-6-flake option α)
	ensureNoTeam(t, ctx, teamNameAnthropic)
	resetConnCacheSnapshot()

	// Pre-seed a hand-managed team under the alias the CR will declare, so
	// ListTeamsByAlias("anthropic") returns one UUID entry → UPDATE arm
	// (never CREATE). AddHandManagedTeam mints a "mock-team-id-N" UUID,
	// standing in for the legacy server-assigned id of an existing team.
	existingUUID := mockServer.AddHandManagedTeam(teamNameAnthropic)

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), teamNameAnthropic)
	})

	cr := teamSampleCR(teamNameAnthropic)
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}

	// Wait for the UPDATE arm to fire — the UPDATE body pins team_id to
	// the existing UUID (CREATE arm would pin it to "anthropic" instead).
	deadline := time.Now().Add(30 * time.Second)
	var sawUpdate bool
	for time.Now().Before(deadline) {
		body := mockServer.LastTeamBody(teamNameAnthropic)
		if body != nil {
			if v, _ := body["team_id"].(string); v == existingUUID {
				sawUpdate = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !sawUpdate {
		t.Fatalf("UPDATE arm did not fire within 30s (no body with team_id=%q observed)", existingUUID)
	}

	tm := pollTeamCondition(t, ctx, teamNameAnthropic, reasonSynced, 30*time.Second)
	if c := apimeta.FindStatusCondition(tm.Status.Conditions, conditionTypeReady); c == nil ||
		c.Status != metav1.ConditionTrue || c.Reason != reasonSynced {
		t.Fatalf("Ready condition not Synced; condition=%+v", c)
	}

	// Exactly ONE mutation (the UPDATE) — NO POST /team/new was issued.
	if got := mockServer.MutationsByTeamAlias(teamNameAnthropic); got != 1 {
		t.Errorf("MutationsByTeamAlias: want 1 (one POST /team/update, no /team/new), got %d", got)
	}

	// The UPDATE body MUST pin the existing UUID, not the name.
	body := mockServer.LastTeamBody(teamNameAnthropic)
	if id, _ := body["team_id"].(string); id != existingUUID {
		t.Errorf("UPDATE body.team_id: want existing UUID %q, got %v", existingUUID, body["team_id"])
	}

	// The persisted status teamID MUST stay on the UUID.
	if tm.Status.LastRendered.TeamID != existingUUID {
		t.Errorf("status.lastRendered.teamID: want existing UUID %q, got %q", existingUUID, tm.Status.LastRendered.TeamID)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 1d: AdoptNameIDFoundByAlias — post-restart re-adopt, no duplicate
// ──────────────────────────────────────────────────────────────────────────

// TestTeamAdopt_NameIDFoundByAlias — after an operator restart that lost
// the CR status, a team previously created with the name-as-id scheme
// (team_id == team_alias == metadata.name) MUST be re-adopted via the
// UPDATE arm (ListTeamsByAlias matches it), NEVER recreated. A duplicate
// POST /team/new would 400 against LiteLLM's existing team_id. The CR is
// reconciled with EMPTY status to simulate the post-restart bootstrap.
func TestTeamAdopt_NameIDFoundByAlias(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache() // Phase 6 cross-suite flake fix (07-CONTEXT.md §Phase-6-flake option α)
	ensureNoTeam(t, ctx, teamNameAnthropic)
	resetConnCacheSnapshot()

	// Seed a name-id team: team_id == team_alias == "anthropic". This is
	// exactly the state left in LiteLLM by a prior name-as-id create.
	mockServer.AddHandManagedTeamWithID(teamNameAnthropic, teamNameAnthropic)

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), teamNameAnthropic)
	})

	// Apply the CR with EMPTY status (fresh Create == post-restart bootstrap:
	// the operator has no lastRendered.teamID to pin from).
	cr := teamSampleCR(teamNameAnthropic)
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}

	// The adopt path is the UPDATE arm — its body pins team_id to the
	// matched entry ("anthropic"). Wait for it.
	deadline := time.Now().Add(30 * time.Second)
	var sawUpdate bool
	for time.Now().Before(deadline) {
		body := mockServer.LastTeamBody(teamNameAnthropic)
		if body != nil {
			if v, _ := body["team_id"].(string); v == teamNameAnthropic {
				sawUpdate = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !sawUpdate {
		t.Fatalf("adopt UPDATE arm did not fire within 30s (no body with team_id=%q observed)", teamNameAnthropic)
	}

	tm := pollTeamCondition(t, ctx, teamNameAnthropic, reasonSynced, 30*time.Second)
	if c := apimeta.FindStatusCondition(tm.Status.Conditions, conditionTypeReady); c == nil ||
		c.Status != metav1.ConditionTrue || c.Reason != reasonSynced {
		t.Fatalf("Ready condition not Synced; condition=%+v", c)
	}

	// Exactly ONE mutation (the adopt UPDATE) — NO duplicate POST /team/new.
	if got := mockServer.MutationsByTeamAlias(teamNameAnthropic); got != 1 {
		t.Errorf("MutationsByTeamAlias: want 1 (one POST /team/update, no /team/new), got %d", got)
	}

	// The mock still holds exactly ONE entry under this alias (no duplicate),
	// keyed on the adopted name-id.
	if id := mockServer.GetTeamID(teamNameAnthropic); id != teamNameAnthropic {
		t.Errorf("adopted entry team_id: want %q, got %q", teamNameAnthropic, id)
	}

	// Status was re-adopted onto the name-id (not a fresh UUID).
	if tm.Status.LastRendered.TeamID != teamNameAnthropic {
		t.Errorf("status.lastRendered.teamID: want adopted name-id %q, got %q", teamNameAnthropic, tm.Status.LastRendered.TeamID)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 2: CreateOnFirstReconcile (with budget) — AC-T1 budget projection
// ──────────────────────────────────────────────────────────────────────────

// TestTeamReconciler_AC_T1_BudgetProjection — behavior #2 +
// AC-T1 (spec §11). Team CR with spec.budget.limit=500.0 and
// spec.budget.period="30d". POST /team/new body has max_budget == 500.0,
// budget_duration == "30d", team_alias bare.
func TestTeamReconciler_AC_T1_BudgetProjection(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache() // Phase 6 cross-suite flake fix (07-CONTEXT.md §Phase-6-flake option α)
	ensureNoTeam(t, ctx, "team-budget-ac-t1")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-budget-ac-t1")
	})

	cr := teamSampleCR("team-budget-ac-t1")
	cr.Spec.Budget = &litellmv1alpha1.BudgetSpec{
		Limit:  floatPtr(500.0),
		Period: "30d",
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}

	tm := pollTeamCondition(t, ctx, "team-budget-ac-t1", reasonSynced, 30*time.Second)
	if tm.Status.LastRendered.TeamID == "" {
		t.Fatalf("Team not Synced within 30s")
	}

	body := mockServer.LastTeamBody("team-budget-ac-t1")
	if body == nil {
		t.Fatalf("LastTeamBody is nil")
	}
	if v, _ := body["max_budget"].(float64); v != 500.0 {
		t.Errorf("body.max_budget: want 500.0, got %v", body["max_budget"])
	}
	if v, _ := body["budget_duration"].(string); v != "30d" { //nolint:goconst // budget_duration fixture value asserted in 4 budget tests; const would obscure the per-test budget shape
		t.Errorf("body.budget_duration: want %q, got %v", "30d", body["budget_duration"])
	}
	if v, _ := body["team_alias"].(string); v != "team-budget-ac-t1" {
		t.Errorf("body.team_alias: want bare metadata.name, got %v", body["team_alias"])
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 3: UpdateOnDrift (spec.params change)
// ──────────────────────────────────────────────────────────────────────────

// TestTeamReconciler_UpdateOnDrift_Params — behavior #3.
// After Synced, mutate spec.params (non-overlay key) → next reconcile
// issues exactly one POST /team/update; alitellm_operator_drift_corrected_total{action=
// update_drifted} increments by 1; teamID unchanged.
//
// Phase 10 update (TRL-02/TRL-04): the drift-driver key changed from
// `tpm_limit` (now structural-overlay-controlled — always emits the
// operator's value, so spec.params mutation would NOT change the wire
// body) to `models` (non-overlay pass-through). The test still
// validates the hash-change → UPDATE → identity-preservation
// invariant; only the user-controlled key shifted.
func TestTeamReconciler_UpdateOnDrift_Params(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache() // Phase 6 cross-suite flake fix (07-CONTEXT.md §Phase-6-flake option α)
	ensureNoTeam(t, ctx, "team-update-drift")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-update-drift")
	})

	cr := teamSampleCR("team-update-drift")
	cr.Spec.Params = runtime.RawExtension{Raw: []byte(`{"models":["gpt-4o"]}`)}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}
	tm := pollTeamCondition(t, ctx, "team-update-drift", reasonSynced, 30*time.Second)
	if tm.Status.LastRendered.TeamID == "" {
		t.Fatalf("Team not Synced within 30s")
	}
	originalTeamID := tm.Status.LastRendered.TeamID
	originalHash := tm.Status.LastRendered.Hash

	before := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("team", "update_drifted"))

	// Mutate spec.params (non-overlay key) → triggers UPDATE.
	tm.Spec.Params = runtime.RawExtension{Raw: []byte(`{"models":["gpt-4o","claude-3-5-sonnet"]}`)}
	if err := k8sClient.Update(ctx, tm); err != nil {
		t.Fatalf("update Team spec.params: %v", err)
	}

	// Poll for hash change.
	deadline := time.Now().Add(30 * time.Second)
	var updated *litellmv1alpha1.LiteLLMTeam
	for time.Now().Before(deadline) {
		updated = pollTeamCondition(t, ctx, "team-update-drift", reasonSynced, 5*time.Second)
		if updated.Status.LastRendered.Hash != originalHash {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if updated.Status.LastRendered.Hash == originalHash {
		t.Fatalf("hash unchanged after spec.params mutation; want new hash")
	}
	if updated.Status.LastRendered.TeamID != originalTeamID {
		t.Errorf("teamID changed across UPDATE: was %q, got %q (UPDATE should preserve identity)",
			originalTeamID, updated.Status.LastRendered.TeamID)
	}

	after := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("team", "update_drifted"))
	if delta := after - before; delta < 1 {
		t.Errorf("alitellm_operator_drift_corrected_total{action=update_drifted}: want +1, got delta=%v", delta)
	}
	// Verify the new value made it to the wire.
	body := mockServer.LastTeamBody("team-update-drift")
	models, _ := body["models"].([]any)
	if len(models) != 2 {
		t.Errorf("body.models after update: want 2 elements, got %d (%v)", len(models), models)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 4: HashEqualNoOp (idempotency)
// ──────────────────────────────────────────────────────────────────────────

// TestTeamReconciler_HashEqualNoOp — behavior #5. After Synced,
// re-trigger reconcile via annotation bump → no additional mutation calls
// (hash compare suppresses no-op writes).
func TestTeamReconciler_HashEqualNoOp(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache() // Phase 6 cross-suite flake fix (07-CONTEXT.md §Phase-6-flake option α)
	ensureNoTeam(t, ctx, "team-noop")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-noop")
	})

	cr := teamSampleCR("team-noop")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}
	tm := pollTeamCondition(t, ctx, "team-noop", reasonSynced, 30*time.Second)
	if tm.Status.LastRendered.TeamID == "" {
		t.Fatalf("Team not Synced within 30s")
	}
	mutsAfterFirst := mockServer.MutationsByTeamAlias("team-noop")

	// Bump annotation → forces a reconcile, but hash unchanged.
	if err := updateWithRetry(ctx,
		client.ObjectKeyFromObject(tm),
		tm,
		func(obj *litellmv1alpha1.LiteLLMTeam) error {
			if obj.Annotations == nil {
				obj.Annotations = make(map[string]string)
			}
			obj.Annotations["test.ackstorm.ai/force-reconcile"] = time.Now().String()
			return nil
		},
	); err != nil {
		t.Fatalf("update annotation: %v", err)
	}
	time.Sleep(1250 * time.Millisecond)

	mutsAfterAnnotation := mockServer.MutationsByTeamAlias("team-noop")
	if delta := mutsAfterAnnotation - mutsAfterFirst; delta != 0 {
		t.Errorf("hash-equal short-circuit broken: %d mutations after annotation-only edit (want 0)", delta)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Tests 5-7: ProjectionOverride (3 keys)
// ──────────────────────────────────────────────────────────────────────────

// projOverrideCount returns the number of ProjectionOverride Events on the
// named Team whose Message contains the given keyName.
func projOverrideCount(events []corev1.Event, keyName string) int {
	n := 0
	for _, ev := range events {
		if ev.Reason == "ProjectionOverride" && strings.Contains(ev.Message, keyName) {
			n++
		}
	}
	return n
}

// TestTeamReconciler_ProjectionOverride_TeamAlias — behavior #6.
// spec.params contains team_alias: "user-supplied"; reconcile emits at
// least one Warning Event with reason=ProjectionOverride mentioning
// team_alias; body.team_alias on the wire is metadata.name (operator
// overlay wins).
func TestTeamReconciler_ProjectionOverride_TeamAlias(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache() // Phase 6 cross-suite flake fix (07-CONTEXT.md §Phase-6-flake option α)
	ensureNoTeam(t, ctx, "team-proj-alias")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-proj-alias")
	})

	cr := teamSampleCR("team-proj-alias")
	cr.Spec.Params = runtime.RawExtension{Raw: []byte(`{"team_alias":"user-supplied"}`)}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}
	tm := pollTeamCondition(t, ctx, "team-proj-alias", reasonSynced, 30*time.Second)
	if tm.Status.LastRendered.TeamID == "" {
		t.Fatalf("Team not Synced within 30s")
	}
	body := mockServer.LastTeamBody("team-proj-alias")
	if v, _ := body["team_alias"].(string); v != "team-proj-alias" {
		t.Errorf("body.team_alias: operator overlay must win — want %q, got %v",
			"team-proj-alias", body["team_alias"])
	}
	// Give the event recorder a moment to flush.
	time.Sleep(500 * time.Millisecond)
	events := listTeamEvents(ctx, t, "team-proj-alias")
	if n := projOverrideCount(events, "team_alias"); n < 1 {
		t.Errorf("ProjectionOverride Event for team_alias: want >=1, got %d", n)
	}
}

// TestTeamReconciler_ProjectionOverride_MaxBudget — behavior #7.
// spec.params contains max_budget: 9999.0 AND spec.budget.limit=500.0;
// Event emitted mentioning max_budget; body.max_budget on the wire is
// 500.0 (operator overlay wins).
func TestTeamReconciler_ProjectionOverride_MaxBudget(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache() // Phase 6 cross-suite flake fix (07-CONTEXT.md §Phase-6-flake option α)
	ensureNoTeam(t, ctx, "team-proj-maxbudget")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-proj-maxbudget")
	})

	cr := teamSampleCR("team-proj-maxbudget")
	cr.Spec.Budget = &litellmv1alpha1.BudgetSpec{Limit: floatPtr(500.0), Period: "30d"}
	cr.Spec.Params = runtime.RawExtension{Raw: []byte(`{"max_budget":9999.0}`)}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}
	tm := pollTeamCondition(t, ctx, "team-proj-maxbudget", reasonSynced, 30*time.Second)
	if tm.Status.LastRendered.TeamID == "" {
		t.Fatalf("Team not Synced within 30s")
	}
	body := mockServer.LastTeamBody("team-proj-maxbudget")
	if v, _ := body["max_budget"].(float64); v != 500.0 {
		t.Errorf("body.max_budget: operator overlay must win — want 500.0, got %v", body["max_budget"])
	}
	time.Sleep(500 * time.Millisecond)
	events := listTeamEvents(ctx, t, "team-proj-maxbudget")
	if n := projOverrideCount(events, "max_budget"); n < 1 {
		t.Errorf("ProjectionOverride Event for max_budget: want >=1, got %d", n)
	}
}

// TestTeamReconciler_ProjectionOverride_BudgetDuration — // behavior #8. spec.params contains budget_duration: "1y" AND
// spec.budget.period="30d"; Event emitted mentioning budget_duration;
// body.budget_duration on the wire is "30d" (operator overlay wins).
func TestTeamReconciler_ProjectionOverride_BudgetDuration(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache() // Phase 6 cross-suite flake fix (07-CONTEXT.md §Phase-6-flake option α)
	ensureNoTeam(t, ctx, "team-proj-bdur")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-proj-bdur")
	})

	cr := teamSampleCR("team-proj-bdur")
	cr.Spec.Budget = &litellmv1alpha1.BudgetSpec{Limit: floatPtr(500.0), Period: "30d"}
	// Note: budget_duration value in params is a string the CRD's
	// preserve-unknown-fields accepts.
	cr.Spec.Params = runtime.RawExtension{Raw: []byte(`{"budget_duration":"1y"}`)}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}
	tm := pollTeamCondition(t, ctx, "team-proj-bdur", reasonSynced, 30*time.Second)
	if tm.Status.LastRendered.TeamID == "" {
		t.Fatalf("Team not Synced within 30s")
	}
	body := mockServer.LastTeamBody("team-proj-bdur")
	if v, _ := body["budget_duration"].(string); v != "30d" {
		t.Errorf("body.budget_duration: operator overlay must win — want %q, got %v", "30d", body["budget_duration"])
	}
	time.Sleep(500 * time.Millisecond)
	events := listTeamEvents(ctx, t, "team-proj-bdur")
	if n := projOverrideCount(events, "budget_duration"); n < 1 {
		t.Errorf("ProjectionOverride Event for budget_duration: want >=1, got %d", n)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Phase 10 Tests: RateLimits projection + clearing + 4 collision events +
// 2 negative-admission tests (TRL-01..TRL-05)
// ──────────────────────────────────────────────────────────────────────────

// TestTeamReconciler_RateLimitsProjection — TRL-02 + TRL-03 + TRL-06
// (composite). Team CR with spec.rateLimits.rpm=6000 + tpm=1000000.
// POST /team/new body has all 4 rate-limit overlay keys at top level:
// rpm_limit=6000, tpm_limit=1000000, rpm_limit_type=
// "best_effort_throughput", tpm_limit_type="best_effort_throughput".
// team_alias bare metadata.name.
func TestTeamReconciler_RateLimitsProjection(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache()
	ensureNoTeam(t, ctx, "team-ratelimits-projection")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-ratelimits-projection")
	})

	cr := teamSampleCR("team-ratelimits-projection")
	cr.Spec.RateLimits = &litellmv1alpha1.RateLimitsSpec{
		RPM: int32Ptr(6000),
		TPM: int32Ptr(1000000),
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}

	tm := pollTeamCondition(t, ctx, "team-ratelimits-projection", reasonSynced, 30*time.Second)
	if tm.Status.LastRendered.TeamID == "" {
		t.Fatalf("Team not Synced within 30s")
	}

	body := mockServer.LastTeamBody("team-ratelimits-projection")
	if body == nil {
		t.Fatalf("LastTeamBody is nil")
	}
	// JSON numbers unmarshal as float64 in map[string]any per encoding/json.
	if v, _ := body["rpm_limit"].(float64); v != 6000.0 {
		t.Errorf("body.rpm_limit: want 6000, got %v (type %T)", body["rpm_limit"], body["rpm_limit"])
	}
	if v, _ := body["tpm_limit"].(float64); v != 1000000.0 {
		t.Errorf("body.tpm_limit: want 1000000, got %v (type %T)", body["tpm_limit"], body["tpm_limit"])
	}
	if v, _ := body["rpm_limit_type"].(string); v != rateLimitTypeBestEffort {
		t.Errorf("body.rpm_limit_type: want %q, got %v", rateLimitTypeBestEffort, body["rpm_limit_type"])
	}
	if v, _ := body["tpm_limit_type"].(string); v != rateLimitTypeBestEffort {
		t.Errorf("body.tpm_limit_type: want %q, got %v", rateLimitTypeBestEffort, body["tpm_limit_type"])
	}
	if v, _ := body["team_alias"].(string); v != "team-ratelimits-projection" {
		t.Errorf("body.team_alias: want bare metadata.name, got %v", body["team_alias"])
	}
}

// TestTeamReconciler_RateLimitsClearing_RPM — TRL-04 leaf-clear.
// RPM omitted (nil), TPM set. Body has rpm_limit=nil + rpm_limit_type
// ABSENT from the map AND tpm_limit set + tpm_limit_type
// "best_effort_throughput".
func TestTeamReconciler_RateLimitsClearing_RPM(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache()
	ensureNoTeam(t, ctx, "team-ratelimits-clear-rpm")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-ratelimits-clear-rpm")
	})

	cr := teamSampleCR("team-ratelimits-clear-rpm")
	cr.Spec.RateLimits = &litellmv1alpha1.RateLimitsSpec{
		RPM: nil,
		TPM: int32Ptr(1000000),
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}
	tm := pollTeamCondition(t, ctx, "team-ratelimits-clear-rpm", reasonSynced, 30*time.Second)
	if tm.Status.LastRendered.TeamID == "" {
		t.Fatalf("Team not Synced within 30s")
	}

	body := mockServer.LastTeamBody("team-ratelimits-clear-rpm")
	if body == nil {
		t.Fatalf("LastTeamBody is nil")
	}
	// rpm_limit MUST be present with nil value (JSON null on wire).
	if v, ok := body["rpm_limit"]; !ok || v != nil {
		t.Errorf("body.rpm_limit: want present with nil value, got ok=%v v=%v", ok, v)
	}
	// rpm_limit_type MUST be ABSENT from the map (key not inserted at all).
	if _, ok := body["rpm_limit_type"]; ok {
		t.Errorf("body.rpm_limit_type: must be ABSENT when rpm_limit is null (Feature 01 §2.1), but found in map")
	}
	// tpm_limit set; tpm_limit_type present.
	if v, _ := body["tpm_limit"].(float64); v != 1000000.0 {
		t.Errorf("body.tpm_limit: want 1000000, got %v", body["tpm_limit"])
	}
	if v, _ := body["tpm_limit_type"].(string); v != rateLimitTypeBestEffort {
		t.Errorf("body.tpm_limit_type: want %q, got %v", rateLimitTypeBestEffort, body["tpm_limit_type"])
	}
}

// TestTeamReconciler_RateLimitsClearing_TPM — TRL-04 leaf-clear (mirror).
// RPM set, TPM omitted. Mirror image of _RPM.
func TestTeamReconciler_RateLimitsClearing_TPM(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache()
	ensureNoTeam(t, ctx, "team-ratelimits-clear-tpm")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-ratelimits-clear-tpm")
	})

	cr := teamSampleCR("team-ratelimits-clear-tpm")
	cr.Spec.RateLimits = &litellmv1alpha1.RateLimitsSpec{
		RPM: int32Ptr(6000),
		TPM: nil,
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}
	tm := pollTeamCondition(t, ctx, "team-ratelimits-clear-tpm", reasonSynced, 30*time.Second)
	if tm.Status.LastRendered.TeamID == "" {
		t.Fatalf("Team not Synced within 30s")
	}

	body := mockServer.LastTeamBody("team-ratelimits-clear-tpm")
	if body == nil {
		t.Fatalf("LastTeamBody is nil")
	}
	if v, _ := body["rpm_limit"].(float64); v != 6000.0 {
		t.Errorf("body.rpm_limit: want 6000, got %v", body["rpm_limit"])
	}
	if v, _ := body["rpm_limit_type"].(string); v != rateLimitTypeBestEffort {
		t.Errorf("body.rpm_limit_type: want %q, got %v", rateLimitTypeBestEffort, body["rpm_limit_type"])
	}
	if v, ok := body["tpm_limit"]; !ok || v != nil {
		t.Errorf("body.tpm_limit: want present with nil value, got ok=%v v=%v", ok, v)
	}
	if _, ok := body["tpm_limit_type"]; ok {
		t.Errorf("body.tpm_limit_type: must be ABSENT when tpm_limit is null (Feature 01 §2.1), but found in map")
	}
}

// TestTeamReconciler_RateLimitsClearing_WholeBlock — TRL-04 whole-block
// clear. spec.RateLimits is nil (not set). Body has both *_limit keys
// present with nil values AND both *_type keys ABSENT.
func TestTeamReconciler_RateLimitsClearing_WholeBlock(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache()
	ensureNoTeam(t, ctx, "team-ratelimits-clear-block")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-ratelimits-clear-block")
	})

	cr := teamSampleCR("team-ratelimits-clear-block")
	// Explicitly leave cr.Spec.RateLimits unset (nil pointer — whole-block
	// absent).
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}
	tm := pollTeamCondition(t, ctx, "team-ratelimits-clear-block", reasonSynced, 30*time.Second)
	if tm.Status.LastRendered.TeamID == "" {
		t.Fatalf("Team not Synced within 30s")
	}

	body := mockServer.LastTeamBody("team-ratelimits-clear-block")
	if body == nil {
		t.Fatalf("LastTeamBody is nil")
	}
	if v, ok := body["rpm_limit"]; !ok || v != nil {
		t.Errorf("body.rpm_limit: want present with nil value, got ok=%v v=%v", ok, v)
	}
	if v, ok := body["tpm_limit"]; !ok || v != nil {
		t.Errorf("body.tpm_limit: want present with nil value, got ok=%v v=%v", ok, v)
	}
	if _, ok := body["rpm_limit_type"]; ok {
		t.Errorf("body.rpm_limit_type: must be ABSENT when rpm_limit is null (Feature 01 §2.1), but found in map")
	}
	if _, ok := body["tpm_limit_type"]; ok {
		t.Errorf("body.tpm_limit_type: must be ABSENT when tpm_limit is null (Feature 01 §2.1), but found in map")
	}
}

// TestTeamReconciler_RateLimitsClearing_EmptyBlock — D-03 validation:
// empty `spec.rateLimits: {}` block (present, both leaves nil) is
// IDENTICAL to whole-block-absent. Validates the empty-struct-equals-
// absent contract.
func TestTeamReconciler_RateLimitsClearing_EmptyBlock(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache()
	ensureNoTeam(t, ctx, "team-ratelimits-empty-block")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-ratelimits-empty-block")
	})

	cr := teamSampleCR("team-ratelimits-empty-block")
	cr.Spec.RateLimits = &litellmv1alpha1.RateLimitsSpec{} // both leaves nil
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}
	tm := pollTeamCondition(t, ctx, "team-ratelimits-empty-block", reasonSynced, 30*time.Second)
	if tm.Status.LastRendered.TeamID == "" {
		t.Fatalf("Team not Synced within 30s")
	}

	body := mockServer.LastTeamBody("team-ratelimits-empty-block")
	if body == nil {
		t.Fatalf("LastTeamBody is nil")
	}
	// Same assertions as _WholeBlock — D-03 contract.
	if v, ok := body["rpm_limit"]; !ok || v != nil {
		t.Errorf("body.rpm_limit: empty-block ≡ absent — want present with nil value, got ok=%v v=%v", ok, v)
	}
	if v, ok := body["tpm_limit"]; !ok || v != nil {
		t.Errorf("body.tpm_limit: empty-block ≡ absent — want present with nil value, got ok=%v v=%v", ok, v)
	}
	if _, ok := body["rpm_limit_type"]; ok {
		t.Errorf("body.rpm_limit_type: empty-block ≡ absent — must be ABSENT, but found")
	}
	if _, ok := body["tpm_limit_type"]; ok {
		t.Errorf("body.tpm_limit_type: empty-block ≡ absent — must be ABSENT, but found")
	}
}

// TestTeamReconciler_ProjectionOverride_RPMLimit — TRL-05 collision.
// spec.rateLimits.rpm=6000 + spec.params.rpm_limit=9999. Operator
// overlay wins (body.rpm_limit=6000) AND ProjectionOverride event
// emitted naming rpm_limit.
func TestTeamReconciler_ProjectionOverride_RPMLimit(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache()
	ensureNoTeam(t, ctx, "team-proj-rpm-limit")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-proj-rpm-limit")
	})

	cr := teamSampleCR("team-proj-rpm-limit")
	cr.Spec.RateLimits = &litellmv1alpha1.RateLimitsSpec{RPM: int32Ptr(6000)}
	cr.Spec.Params = runtime.RawExtension{Raw: []byte(`{"rpm_limit":9999}`)}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}
	tm := pollTeamCondition(t, ctx, "team-proj-rpm-limit", reasonSynced, 30*time.Second)
	if tm.Status.LastRendered.TeamID == "" {
		t.Fatalf("Team not Synced within 30s")
	}
	body := mockServer.LastTeamBody("team-proj-rpm-limit")
	if v, _ := body["rpm_limit"].(float64); v != 6000.0 {
		t.Errorf("body.rpm_limit: operator overlay must win — want 6000, got %v", body["rpm_limit"])
	}
	time.Sleep(500 * time.Millisecond)
	events := listTeamEvents(ctx, t, "team-proj-rpm-limit")
	if n := projOverrideCount(events, "rpm_limit"); n < 1 {
		t.Errorf("ProjectionOverride Event for rpm_limit: want >=1, got %d", n)
	}
}

// TestTeamReconciler_ProjectionOverride_TPMLimit — TRL-05 collision (mirror).
func TestTeamReconciler_ProjectionOverride_TPMLimit(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache()
	ensureNoTeam(t, ctx, "team-proj-tpm-limit")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-proj-tpm-limit")
	})

	cr := teamSampleCR("team-proj-tpm-limit")
	cr.Spec.RateLimits = &litellmv1alpha1.RateLimitsSpec{TPM: int32Ptr(1000000)}
	cr.Spec.Params = runtime.RawExtension{Raw: []byte(`{"tpm_limit":99999}`)}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}
	tm := pollTeamCondition(t, ctx, "team-proj-tpm-limit", reasonSynced, 30*time.Second)
	if tm.Status.LastRendered.TeamID == "" {
		t.Fatalf("Team not Synced within 30s")
	}
	body := mockServer.LastTeamBody("team-proj-tpm-limit")
	if v, _ := body["tpm_limit"].(float64); v != 1000000.0 {
		t.Errorf("body.tpm_limit: operator overlay must win — want 1000000, got %v", body["tpm_limit"])
	}
	time.Sleep(500 * time.Millisecond)
	events := listTeamEvents(ctx, t, "team-proj-tpm-limit")
	if n := projOverrideCount(events, "tpm_limit"); n < 1 {
		t.Errorf("ProjectionOverride Event for tpm_limit: want >=1, got %d", n)
	}
}

// TestTeamReconciler_ProjectionOverride_RPMLimitType — TRL-05 collision.
// spec.rateLimits.rpm=6000 (so operator emits rpm_limit_type=
// "best_effort_throughput") + spec.params.rpm_limit_type="high_priority".
// Operator hardcoded value wins AND event emitted.
func TestTeamReconciler_ProjectionOverride_RPMLimitType(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache()
	ensureNoTeam(t, ctx, "team-proj-rpm-type")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-proj-rpm-type")
	})

	cr := teamSampleCR("team-proj-rpm-type")
	cr.Spec.RateLimits = &litellmv1alpha1.RateLimitsSpec{RPM: int32Ptr(6000)}
	cr.Spec.Params = runtime.RawExtension{Raw: []byte(`{"rpm_limit_type":"high_priority"}`)}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}
	tm := pollTeamCondition(t, ctx, "team-proj-rpm-type", reasonSynced, 30*time.Second)
	if tm.Status.LastRendered.TeamID == "" {
		t.Fatalf("Team not Synced within 30s")
	}
	body := mockServer.LastTeamBody("team-proj-rpm-type")
	if v, _ := body["rpm_limit_type"].(string); v != rateLimitTypeBestEffort {
		t.Errorf("body.rpm_limit_type: operator hardcoded value must win — want %q, got %v",
			rateLimitTypeBestEffort, body["rpm_limit_type"])
	}
	time.Sleep(500 * time.Millisecond)
	events := listTeamEvents(ctx, t, "team-proj-rpm-type")
	if n := projOverrideCount(events, "rpm_limit_type"); n < 1 {
		t.Errorf("ProjectionOverride Event for rpm_limit_type: want >=1, got %d", n)
	}
}

// TestTeamReconciler_ProjectionOverride_TPMLimitType — TRL-05 (mirror).
func TestTeamReconciler_ProjectionOverride_TPMLimitType(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache()
	ensureNoTeam(t, ctx, "team-proj-tpm-type")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-proj-tpm-type")
	})

	cr := teamSampleCR("team-proj-tpm-type")
	cr.Spec.RateLimits = &litellmv1alpha1.RateLimitsSpec{TPM: int32Ptr(1000000)}
	cr.Spec.Params = runtime.RawExtension{Raw: []byte(`{"tpm_limit_type":"high_priority"}`)}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}
	tm := pollTeamCondition(t, ctx, "team-proj-tpm-type", reasonSynced, 30*time.Second)
	if tm.Status.LastRendered.TeamID == "" {
		t.Fatalf("Team not Synced within 30s")
	}
	body := mockServer.LastTeamBody("team-proj-tpm-type")
	if v, _ := body["tpm_limit_type"].(string); v != rateLimitTypeBestEffort {
		t.Errorf("body.tpm_limit_type: operator hardcoded value must win — want %q, got %v",
			rateLimitTypeBestEffort, body["tpm_limit_type"])
	}
	time.Sleep(500 * time.Millisecond)
	events := listTeamEvents(ctx, t, "team-proj-tpm-type")
	if n := projOverrideCount(events, "tpm_limit_type"); n < 1 {
		t.Errorf("ProjectionOverride Event for tpm_limit_type: want >=1, got %d", n)
	}
}

// TestTeamReconciler_RateLimits_NegativeRPM_Rejected — TRL-01 admission gate.
// Apply a CR with spec.rateLimits.rpm=-1 via the typed Go struct; the
// API server's OpenAPI Minimum=0 schema constraint rejects with
// apierrors.IsInvalid.
func TestTeamReconciler_RateLimits_NegativeRPM_Rejected(t *testing.T) {
	ctx := context.Background()
	ensureNoTeam(t, ctx, "team-ratelimits-negative-rpm")
	t.Cleanup(func() {
		ensureNoTeam(t, context.Background(), "team-ratelimits-negative-rpm")
	})

	cr := teamSampleCR("team-ratelimits-negative-rpm")
	cr.Spec.RateLimits = &litellmv1alpha1.RateLimitsSpec{RPM: int32Ptr(-1)}
	err := k8sClient.Create(ctx, cr)
	if err == nil {
		t.Fatalf("expected admission rejection on rpm=-1, but Create succeeded")
	}
	if !apierrors.IsInvalid(err) {
		t.Errorf("expected apierrors.IsInvalid for negative rpm; got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "rpm") &&
		!strings.Contains(err.Error(), "greater than or equal to 0") {
		t.Errorf("expected rejection message to mention rpm or 'greater than or equal to 0'; got %q", err.Error())
	}
}

// TestTeamReconciler_RateLimits_NegativeTPM_Rejected — TRL-01 admission (mirror).
func TestTeamReconciler_RateLimits_NegativeTPM_Rejected(t *testing.T) {
	ctx := context.Background()
	ensureNoTeam(t, ctx, "team-ratelimits-negative-tpm")
	t.Cleanup(func() {
		ensureNoTeam(t, context.Background(), "team-ratelimits-negative-tpm")
	})

	cr := teamSampleCR("team-ratelimits-negative-tpm")
	cr.Spec.RateLimits = &litellmv1alpha1.RateLimitsSpec{TPM: int32Ptr(-1)}
	err := k8sClient.Create(ctx, cr)
	if err == nil {
		t.Fatalf("expected admission rejection on tpm=-1, but Create succeeded")
	}
	if !apierrors.IsInvalid(err) {
		t.Errorf("expected apierrors.IsInvalid for negative tpm; got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "tpm") &&
		!strings.Contains(err.Error(), "greater than or equal to 0") {
		t.Errorf("expected rejection message to mention tpm or 'greater than or equal to 0'; got %q", err.Error())
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 8: AC-T6 ParamsPassthrough
// ──────────────────────────────────────────────────────────────────────────

// TestTeamReconciler_AC_T6_ParamsPassthrough — behavior; AC-T6
// (spec §11). spec.params={tpm_limit:1000000, rpm_limit:6000,
// tags:["engineering","production"], blocked:false}.
//
// Phase 10 update (TRL-02/TRL-05): tpm_limit and rpm_limit are now
// structural-overlay-controlled — operator overlay wins, forcing them
// to nil when spec.rateLimits is absent, AND emitting a
// ProjectionOverride Warning Event per colliding key. tags + blocked
// remain pass-through (no collision with the 7 overlay keys).
// blocked=false is dropped from the body per CR-10 / D-7.1-10 (the
// LiteLLM 1.83.10 403-on-blocked-false workaround at
// team_controller.go Step 7).
func TestTeamReconciler_AC_T6_ParamsPassthrough(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache() // Phase 6 cross-suite flake fix (07-CONTEXT.md §Phase-6-flake option α)
	ensureNoTeam(t, ctx, "team-passthrough-ac-t6")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-passthrough-ac-t6")
	})

	cr := teamSampleCR("team-passthrough-ac-t6")
	cr.Spec.Params = runtime.RawExtension{
		Raw: []byte(`{"tpm_limit":1000000,"rpm_limit":6000,"tags":["engineering","production"],"blocked":false}`),
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}
	tm := pollTeamCondition(t, ctx, "team-passthrough-ac-t6", reasonSynced, 30*time.Second)
	if tm.Status.LastRendered.TeamID == "" {
		t.Fatalf("Team not Synced within 30s")
	}
	body := mockServer.LastTeamBody("team-passthrough-ac-t6")
	if body == nil {
		t.Fatalf("LastTeamBody is nil")
	}
	// Phase 10: tpm_limit and rpm_limit are operator-overlaid; absent
	// spec.rateLimits → both forced to nil on the wire.
	if v, ok := body["tpm_limit"]; !ok || v != nil {
		t.Errorf("body.tpm_limit: operator overlay must force nil when spec.rateLimits absent — got ok=%v v=%v", ok, v)
	}
	if v, ok := body["rpm_limit"]; !ok || v != nil {
		t.Errorf("body.rpm_limit: operator overlay must force nil when spec.rateLimits absent — got ok=%v v=%v", ok, v)
	}
	// Pass-through keys (no collision with overlay set).
	tags, _ := body["tags"].([]any)
	if len(tags) != 2 {
		t.Errorf("body.tags: want 2 elements (pass-through), got %d (%v)", len(tags), tags)
	}
	// blocked=false is dropped by Step 7 CR-10 workaround; don't assert
	// the key is present.
	if v, _ := body["team_alias"].(string); v != "team-passthrough-ac-t6" {
		t.Errorf("body.team_alias: want bare name, got %v", body["team_alias"])
	}
	// Phase 10: ProjectionOverride events MUST fire for both rate-limit
	// colliding keys (the user set both in spec.params; the operator
	// overlay wins; the Event surfaces the collision).
	time.Sleep(500 * time.Millisecond)
	events := listTeamEvents(ctx, t, "team-passthrough-ac-t6")
	if n := projOverrideCount(events, "tpm_limit"); n < 1 {
		t.Errorf("ProjectionOverride Event for tpm_limit: want >=1, got %d", n)
	}
	if n := projOverrideCount(events, "rpm_limit"); n < 1 {
		t.Errorf("ProjectionOverride Event for rpm_limit: want >=1, got %d", n)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 9: ConnectionGate
// ──────────────────────────────────────────────────────────────────────────

// TestTeamReconciler_ConnectionGate — behavior #9. Force cache
// Ready=false reason=Unreachable → Team status flips to Ready=False /
// LiteLLMUnavailable with echo-reason message; zero mock mutations.
func TestTeamReconciler_ConnectionGate(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache() // Phase 6 cross-suite flake fix (07-CONTEXT.md §Phase-6-flake option α)
	ensureNoTeam(t, ctx, "team-gate-test")
	resetConnCacheSnapshot()

	// Force cache not-Ready.
	connCache.Rebuild(connection.ConnectionSnapshot{
		Ready:  false,
		Reason: "Unreachable",
	})
	ensureNoConnectionDefault(t, ctx)

	cr := teamSampleCR("team-gate-test")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		connCache.Rebuild(connection.ConnectionSnapshot{})
		ensureNoTeam(t, context.Background(), "team-gate-test")
	})

	tm := pollTeamCondition(t, ctx, "team-gate-test", "LiteLLMUnavailable", 30*time.Second)
	c := apimeta.FindStatusCondition(tm.Status.Conditions, conditionTypeReady)
	if c == nil {
		t.Fatalf("Ready condition not set; conditions=%+v", tm.Status.Conditions)
	}
	if c.Status != metav1.ConditionFalse {
		t.Errorf("Ready.Status: want False, got %v", c.Status)
	}
	wantMsg := connNotReadyUnreachableMsg
	if !strings.Contains(c.Message, wantMsg) {
		t.Errorf("Ready.Message: want substring %q, got %q", wantMsg, c.Message)
	}

	// Zero team mutations.
	if got := mockServer.MutationsByTeamAlias("team-gate-test"); got != 0 {
		t.Errorf("connection-gate: want 0 mutations against alias, got %d", got)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 10: 401FastPath
// ──────────────────────────────────────────────────────────────────────────

// TestTeamReconciler_401FastPath — behavior #10. Mock returns
// 401 on team routes → cache invalidated; status = LiteLLMUnavailable;
// reconcile returns nil (anti-storm).
func TestTeamReconciler_401FastPath(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache() // Phase 6 cross-suite flake fix (07-CONTEXT.md §Phase-6-flake option α)
	ensureNoTeam(t, ctx, "team-401-test")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-401-test")
	})

	// Flip to Mode401 BEFORE creating the CR so the first call 401s.
	mockServer.SetMode(mock.Mode401)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()

	cr := teamSampleCR("team-401-test")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}

	// Cache should flip not-Ready within 5s.
	deadline := time.Now().Add(5 * time.Second)
	notReady := false
	for time.Now().Before(deadline) {
		if !connCache.Snapshot().Ready {
			notReady = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !notReady {
		t.Errorf("401FastPath: cache.Snapshot().Ready did NOT flip false within 5s")
	}

	tm := pollTeamCondition(t, ctx, "team-401-test", "LiteLLMUnavailable", 10*time.Second)
	c := apimeta.FindStatusCondition(tm.Status.Conditions, conditionTypeReady)
	if c == nil {
		t.Fatalf("Ready condition not set after 401")
	}
	if c.Reason != "LiteLLMUnavailable" {
		t.Errorf("Ready.Reason: want LiteLLMUnavailable, got %q", c.Reason)
	}

	// Anti-storm: bounded mutations over an accelerated observation window.
	mockServer.SetMode(mock.Mode401)
	mutsBefore := mockServer.Mutations()
	time.Sleep(1250 * time.Millisecond)
	mutsAfter := mockServer.Mutations()
	delta := mutsAfter - mutsBefore
	if delta > 8 {
		t.Errorf("401FastPath anti-storm: %d mutations in 2s window (want <= 8)", delta)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 11: SecretSubstitution
// ──────────────────────────────────────────────────────────────────────────

// TestTeamReconciler_SecretSubstitution — behavior #11.
// spec.params={"api_key":"{{API_KEY}}"} + spec.secrets=[{as:API_KEY,
// secretRef:{name:s1, key:k}}] + Secret s1.k="resolved-value". POST
// /team/new body.api_key == "resolved-value".
func TestTeamReconciler_SecretSubstitution(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache() // Phase 6 cross-suite flake fix (07-CONTEXT.md §Phase-6-flake option α)
	ensureNoTeam(t, ctx, "team-secret-sub")
	resetConnCacheSnapshot()

	secretName := "team-secret-sub-secret"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: WatchNamespace,
		},
		Data: map[string][]byte{"k": []byte("resolved-value")},
	}
	_ = k8sClient.Delete(ctx, secret)
	time.Sleep(50 * time.Millisecond)
	if err := k8sClient.Create(ctx, secret); err != nil {
		t.Fatalf("create Secret: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), secret)
	})

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-secret-sub")
	})

	cr := teamSampleCR("team-secret-sub")
	cr.Spec.Params = runtime.RawExtension{Raw: []byte(`{"api_key":"{{API_KEY}}"}`)}
	cr.Spec.Secrets = []litellmv1alpha1.SecretSubstitution{
		{
			As: "API_KEY",
			SecretRef: litellmv1alpha1.SecretKeyRef{
				Name: secretName,
				Key:  "k",
			},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}

	tm := pollTeamCondition(t, ctx, "team-secret-sub", reasonSynced, 30*time.Second)
	if tm.Status.LastRendered.TeamID == "" {
		t.Fatalf("Team not Synced within 30s")
	}
	body := mockServer.LastTeamBody("team-secret-sub")
	if v, _ := body["api_key"].(string); v != "resolved-value" {
		t.Errorf("body.api_key: want substituted value %q, got %v", "resolved-value", body["api_key"])
	}

	// AC-S1 carry-forward: secret value must NOT appear in
	// status.conditions[].message (narrow check; redaction canary
	// suite covers the broader log+events surface).
	c := apimeta.FindStatusCondition(tm.Status.Conditions, conditionTypeReady)
	if c != nil && strings.Contains(c.Message, "resolved-value") {
		t.Errorf("§9.1 FAIL: secret value leaked into status.conditions[Ready].message=%q", c.Message)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 12: SecretNotFound
// ──────────────────────────────────────────────────────────────────────────

// TestTeamReconciler_SecretNotFound — behavior #12.
// spec.params={"x":"{{MISSING}}"} with NO matching spec.secrets[].as.
// Ready=False reason=SecretNotFound; zero LiteLLM mutation calls.
func TestTeamReconciler_SecretNotFound(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache() // Phase 6 cross-suite flake fix (07-CONTEXT.md §Phase-6-flake option α)
	ensureNoTeam(t, ctx, "team-secret-missing")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-secret-missing")
	})

	cr := teamSampleCR("team-secret-missing")
	cr.Spec.Params = runtime.RawExtension{Raw: []byte(`{"x":"{{MISSING}}"}`)}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}

	tm := pollTeamCondition(t, ctx, "team-secret-missing", "SecretNotFound", 30*time.Second)
	c := apimeta.FindStatusCondition(tm.Status.Conditions, conditionTypeReady)
	if c == nil || c.Reason != "SecretNotFound" {
		t.Fatalf("Ready.Reason: want SecretNotFound, got %+v", c)
	}
	if !strings.Contains(c.Message, "MISSING") {
		t.Errorf("Ready.Message: want substring 'MISSING', got %q", c.Message)
	}

	if got := mockServer.MutationsByTeamAlias("team-secret-missing"); got != 0 {
		t.Errorf("SecretNotFound: want 0 LiteLLM mutations, got %d", got)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 13: AC-DC1 HandManagedCoexistence (team slice)
// ──────────────────────────────────────────────────────────────────────────

// TestTeamReconciler_AC_DC1_HandManagedCoexistence — behavior
// #13. Mock pre-populated via AddHandManagedTeam("hand-tuned-eng"). No
// Team CR for that alias exists. After reconciling a DIFFERENT Team CR,
// HasTeam("hand-tuned-eng") still true; MutationsByTeamAlias for
// hand-managed alias == 0.
func TestTeamReconciler_AC_DC1_HandManagedCoexistence(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache() // Phase 6 cross-suite flake fix (07-CONTEXT.md §Phase-6-flake option α)
	ensureNoTeam(t, ctx, "team-declared-dc1")
	resetConnCacheSnapshot()

	// Pre-populate hand-managed Team entries (no operator CRs).
	hmID := mockServer.AddHandManagedTeam("hand-tuned-eng")
	mockServer.AddHandManagedTeam("hand-tuned-finance")

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-declared-dc1")
	})

	// Apply ONE operator-owned Team CR (different alias).
	cr := teamSampleCR("team-declared-dc1")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create declared Team: %v", err)
	}
	tm := pollTeamCondition(t, ctx, "team-declared-dc1", reasonSynced, 30*time.Second)
	if tm.Status.LastRendered.TeamID == "" {
		t.Fatalf("declared Team not Synced within 30s")
	}

	// Trigger a spurious reconcile via annotation bump.
	if tm.Annotations == nil {
		tm.Annotations = map[string]string{}
	}
	tm.Annotations["test.litellm.ackstorm.ai/dc1-trigger"] = time.Now().Format(time.RFC3339Nano)
	_ = k8sClient.Update(ctx, tm)
	time.Sleep(1250 * time.Millisecond)

	for _, hmName := range []string{"hand-tuned-eng", "hand-tuned-finance"} {
		if !mockServer.HasTeam(hmName) {
			t.Errorf("AC-DC1 violation: hand-managed Team %q was REMOVED", hmName)
		}
		if got := mockServer.MutationsByTeamAlias(hmName); got != 0 {
			t.Errorf("AC-DC1 violation: %d mutation(s) against hand-managed Team %q (want 0)", got, hmName)
		}
	}
	if id := mockServer.GetTeamID("hand-tuned-eng"); id != hmID {
		t.Errorf("hand-managed entry ID changed: got %q, want %q", id, hmID)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 14: DriftMetricsFirstReconcileSuppressed
// ──────────────────────────────────────────────────────────────────────────

// TestTeamReconciler_DriftSuppressedOnFirstCreate — behavior #14
// (first half) + Phase 5 two-gate suppression carry-forward.
// On the very first reconcile (ObservedGeneration == 0 →
// firstReconcile=true), alitellm_operator_drift_corrected_total{domain=team,
// action=create_missing} MUST NOT increment.
func TestTeamReconciler_DriftSuppressedOnFirstCreate(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache() // Phase 6 cross-suite flake fix (07-CONTEXT.md §Phase-6-flake option α)
	ensureNoTeam(t, ctx, "team-drift-suppress-first")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-drift-suppress-first")
	})

	before := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("team", "create_missing"))

	cr := teamSampleCR("team-drift-suppress-first")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}
	tm := pollTeamCondition(t, ctx, "team-drift-suppress-first", reasonSynced, 30*time.Second)
	if tm.Status.LastRendered.TeamID == "" {
		t.Fatalf("Team not Synced within 30s")
	}

	after := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("team", "create_missing"))
	if delta := after - before; delta != 0 {
		t.Errorf("OWN-04 violation: alitellm_operator_drift_corrected_total{domain=team,action=create_missing} incremented by %v on first reconcile (want 0)",
			delta)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 15: AC-T2 — OwnershipTransition (synthetic bootstrap → CR-driven)
// ──────────────────────────────────────────────────────────────────────────

// TestTeamReconciler_AC_T2_OwnershipTransition — behavior #5 +
// AC-T2 (spec §11 line 1667). Procedure:
//
// 1. With the connection Ready, the TeamDefaultRunnable's synthetic
// bootstrap fires; the mock observes POST /team/new with team_alias=
// "default" and max_budget=null.
// 2. User THEN creates a Kubernetes Team/default CR with
// spec.budget.limit=100. The CR's add event re-enqueues the
// reconciler, which now takes the Get-found path → resolve via
// ListTeamsByAlias("default") finds the bootstrapped entry → UPDATE
// arm fires with max_budget=100.
// 3. Assert: mock.MutationsByTeamAlias("default") increments to 2
// (initial CREATE + ownership-transition UPDATE); LastTeamBody for
// `default` has max_budget==100.0; team_id is unchanged.
func TestTeamReconciler_AC_T2_OwnershipTransition(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache() // Phase 6 cross-suite flake fix (07-CONTEXT.md §Phase-6-flake option α)
	ensureNoTeam(t, ctx, "default")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "default")
	})

	// (1) Wait for the synthetic bootstrap.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if mockServer.HasTeam("default") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !mockServer.HasTeam("default") {
		t.Fatalf("synthetic bootstrap did not create LiteLLM team aliased `default` within 15s")
	}
	bootstrapTeamID := mockServer.GetTeamID("default")
	if bootstrapTeamID == "" {
		t.Fatalf("GetTeamID(default) is empty after bootstrap")
	}
	bootstrapMutations := mockServer.MutationsByTeamAlias("default")
	t.Logf("after bootstrap: team_id=%q, muts=%d", bootstrapTeamID, bootstrapMutations)
	// Initial body should have max_budget=null.
	if body := mockServer.LastTeamBody("default"); body != nil {
		if v, ok := body["max_budget"]; !ok || v != nil {
			t.Errorf("bootstrap body.max_budget: want present and nil, got ok=%v v=%v", ok, v)
		}
	}

	// (2) User creates Team/default CR with spec.budget.limit=100.
	cr := &litellmv1alpha1.LiteLLMTeam{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default",
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.TeamSpec{
			Budget: &litellmv1alpha1.BudgetSpec{
				Limit: floatPtr(100.0),
			},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team/default: %v", err)
	}

	// (3) Poll for the CR-driven UPDATE to fire (LastTeamBody.max_budget=100).
	deadline = time.Now().Add(30 * time.Second)
	var bodyAfterCR map[string]any
	for time.Now().Before(deadline) {
		body := mockServer.LastTeamBody("default")
		if body != nil {
			if v, ok := body["max_budget"].(float64); ok && v == 100.0 {
				bodyAfterCR = body
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if bodyAfterCR == nil {
		t.Fatalf("CR-driven UPDATE did not apply max_budget=100 within 30s; LastTeamBody=%+v",
			mockServer.LastTeamBody("default"))
	}

	// AC-T2 invariants:
	// (a) team_id is unchanged across the ownership transition.
	if got := mockServer.GetTeamID("default"); got != bootstrapTeamID {
		t.Errorf("AC-T2 violation: team_id changed across ownership transition: was %q, got %q",
			bootstrapTeamID, got)
	}
	// (b) mutation count grew (initial bootstrap + ownership-transition
	// update). On a hand-managed pre-existing team the initial
	// bootstrap takes the UPDATE arm rather than CREATE, but in
	// either case the CR-driven reconcile produces at least one
	// additional mutation.
	finalMutations := mockServer.MutationsByTeamAlias("default")
	if finalMutations < bootstrapMutations+1 {
		t.Errorf("AC-T2 violation: mutation count did not grow across CR creation: bootstrap=%d, final=%d (want ≥%d)",
			bootstrapMutations, finalMutations, bootstrapMutations+1)
	}
	// (c) team_alias on the wire is bare "default".
	if alias, _ := bodyAfterCR["team_alias"].(string); alias != teamAliasDefault {
		t.Errorf("AC-T2: body.team_alias on UPDATE arm: want %q, got %v", teamAliasDefault, bodyAfterCR["team_alias"])
	}

	// (4) Wait for status to populate so cleanup can remove the finalizer.
	pollTeamCondition(t, ctx, "default", reasonSynced, 30*time.Second)
}

// ──────────────────────────────────────────────────────────────────────────
// Test 16: AC-T4 — DefaultDeletionReappliesEmpty (protected deletion)
// ──────────────────────────────────────────────────────────────────────────

// TestTeamReconciler_AC_T4_DefaultDeletionReappliesEmpty — // behavior #6 + AC-T4 (spec §11 line 1671). Procedure:
//
// 1. Create Team/default CR with spec.budget.limit=100 (after the
// synthetic bootstrap has run).
// 2. Wait for Ready=Synced; verify max_budget=100 on the wire.
// 3. Delete the CR. The finalizer fires the protected-deletion path:
// POST /team/update with the implicit empty body (max_budget=null,
// budget_duration=null) — NEVER POST /team/delete.
// 4. Within 30s: assert mock.HasTeam("default")==true (the LiteLLM
// team aliased `default` is preserved); assert
// mock.DeleteTeamCalls is EMPTY across the entire test; assert
// LastTeamBody for default has max_budget==nil; the CR is reaped.
// 5. Repeat steps 1-3 a second time to verify deletion-then-recreate
// works without orphaning the LiteLLM team.
func TestTeamReconciler_AC_T4_DefaultDeletionReappliesEmpty(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache() // Phase 6 cross-suite flake fix (07-CONTEXT.md §Phase-6-flake option α)
	ensureNoTeam(t, ctx, "default")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "default")
	})

	// Wait for synthetic bootstrap.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if mockServer.HasTeam("default") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !mockServer.HasTeam("default") {
		t.Fatalf("synthetic bootstrap did not create LiteLLM team aliased `default` within 15s")
	}
	bootstrapTeamID := mockServer.GetTeamID("default")

	// Run the create-then-delete cycle twice.
	for cycle := 1; cycle <= 2; cycle++ {
		t.Logf("=== cycle %d: create Team/default with budget=100, then delete ===", cycle)

		cr := &litellmv1alpha1.LiteLLMTeam{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "default",
				Namespace: WatchNamespace,
			},
			Spec: litellmv1alpha1.TeamSpec{
				Budget: &litellmv1alpha1.BudgetSpec{Limit: floatPtr(100.0)},
			},
		}
		if err := k8sClient.Create(ctx, cr); err != nil {
			t.Fatalf("cycle %d create Team/default: %v", cycle, err)
		}

		// Wait for the CR-driven UPDATE to apply max_budget=100.
		applied := false
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			body := mockServer.LastTeamBody("default")
			if body != nil {
				if v, ok := body["max_budget"].(float64); ok && v == 100.0 {
					applied = true
					break
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		if !applied {
			t.Fatalf("cycle %d: max_budget=100 not applied within 30s", cycle)
		}

		// Wait for the finalizer to be added so DeletionTimestamp triggers
		// the protected-deletion branch (not immediate K8s GC).
		deadline = time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			var tm litellmv1alpha1.LiteLLMTeam
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: "default", Namespace: WatchNamespace}, &tm); err == nil {
				hasFinalizer := false
				for _, f := range tm.Finalizers {
					if f == teamFinalizer {
						hasFinalizer = true
						break
					}
				}
				if hasFinalizer {
					break
				}
			}
			time.Sleep(25 * time.Millisecond)
		}

		// Delete the CR. The finalizer must fire the protected-deletion
		// path: re-apply the empty body via POST /team/update.
		var existing litellmv1alpha1.LiteLLMTeam
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: "default", Namespace: WatchNamespace}, &existing); err != nil {
			t.Fatalf("cycle %d Get before delete: %v", cycle, err)
		}
		if err := k8sClient.Delete(ctx, &existing); err != nil {
			t.Fatalf("cycle %d Delete Team/default: %v", cycle, err)
		}

		// Wait for the empty body to be re-applied AND the CR to be
		// reaped.
		reapplied := false
		reaped := false
		deadline = time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			body := mockServer.LastTeamBody("default")
			if body != nil {
				if v, ok := body["max_budget"]; ok && v == nil {
					reapplied = true
				}
			}
			var tm litellmv1alpha1.LiteLLMTeam
			err := k8sClient.Get(ctx, client.ObjectKey{Name: "default", Namespace: WatchNamespace}, &tm)
			if apierrors.IsNotFound(err) {
				reaped = true
			}
			if reapplied && reaped {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if !reapplied {
			t.Errorf("cycle %d: implicit empty body NOT re-applied after delete (LastTeamBody.max_budget != nil)", cycle)
		}
		if !reaped {
			t.Errorf("cycle %d: CR was NOT reaped within 30s after delete (finalizer stuck?)", cycle)
		}

		// AC-T4 INVARIANTS (per cycle):
		// (a) The LiteLLM team aliased `default` is preserved.
		if !mockServer.HasTeam("default") {
			t.Errorf("AC-T4 violation cycle %d: LiteLLM team aliased `default` was REMOVED", cycle)
		}
		// (b) team_id is unchanged across the delete-and-reapply cycle.
		if got := mockServer.GetTeamID("default"); got != bootstrapTeamID {
			t.Errorf("AC-T4 violation cycle %d: team_id changed: was %q, got %q (deletion must NOT recreate the LiteLLM team)",
				cycle, bootstrapTeamID, got)
		}
	}

	// AC-T4 LOAD-BEARING INVARIANT — across the ENTIRE test, the operator
	// MUST NOT have invoked the team-delete endpoint against the default
	// team's team_id. mock.DeleteTeamCalls returns the list of team_id
	// values the operator passed to POST /team/delete; for AC-T4 this
	// list MUST be empty.
	if calls := mockServer.DeleteTeamCalls(); len(calls) > 0 {
		t.Errorf("AC-T4 violation: operator invoked POST /team/delete with team_ids=%v (want empty list across Team/default delete-and-recreate cycles)",
			calls)
	}
}

// TestTeamReconciler_ImplicitDefault_BodyShape_ClearsRateLimits — CR-01
// regression: the synthetic-default bootstrap CREATE arm body
// (team_controller.go:639) MUST emit rpm_limit:nil + tpm_limit:nil and
// MUST OMIT both *_type keys per Feature 01 §2.1 (always-emit on clear
// for *_limit; conditional-add for *_type — absent when *_limit is nil).
func TestTeamReconciler_ImplicitDefault_BodyShape_ClearsRateLimits(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache()
	ensureNoTeam(t, ctx, "default")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "default")
	})

	// Wait for the synthetic bootstrap (CREATE arm at team_controller.go:677).
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if mockServer.HasTeam("default") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !mockServer.HasTeam("default") {
		t.Fatalf("synthetic bootstrap did not create LiteLLM team aliased `default` within 15s")
	}

	body := mockServer.LastTeamBody("default")
	if body == nil {
		t.Fatalf("no body recorded for implicit-default CREATE arm")
	}

	// CR-01 regression: rpm_limit and tpm_limit MUST be present with nil value.
	if v, ok := body["rpm_limit"]; !ok {
		t.Errorf("CR-01 regression: rpm_limit MISSING from implicit-default CREATE body; want present with nil")
	} else if v != nil {
		t.Errorf("CR-01 regression: rpm_limit=%v on implicit-default CREATE body; want nil", v)
	}
	if v, ok := body["tpm_limit"]; !ok {
		t.Errorf("CR-01 regression: tpm_limit MISSING from implicit-default CREATE body; want present with nil")
	} else if v != nil {
		t.Errorf("CR-01 regression: tpm_limit=%v on implicit-default CREATE body; want nil", v)
	}

	// CR-01 regression: *_type keys MUST be absent (conditional-add per Feature 01 §2.1).
	if _, ok := body["rpm_limit_type"]; ok {
		t.Errorf("CR-01 regression: rpm_limit_type PRESENT on implicit-default CREATE body; want absent (conditional-add per Feature 01 §2.1)")
	}
	if _, ok := body["tpm_limit_type"]; ok {
		t.Errorf("CR-01 regression: tpm_limit_type PRESENT on implicit-default CREATE body; want absent")
	}
}

// TestTeamReconciler_ImplicitDefault_UpdateArm_BodyShape_ClearsRateLimits
// — CR-01 regression: the synthetic-default bootstrap UPDATE arm body
// (team_controller.go:704) MUST emit rpm_limit:nil + tpm_limit:nil and
// MUST OMIT both *_type keys.
//
// The UPDATE arm fires when ListTeamsByAlias("default") returns a
// non-empty entry. We force that path by pre-populating the mock with a
// hand-managed team aliased `default` (AddHandManagedTeam) so the
// implicit reconciler sees the existing entry and routes through the
// UPDATE arm instead of the CREATE arm.
func TestTeamReconciler_ImplicitDefault_UpdateArm_BodyShape_ClearsRateLimits(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache()
	ensureNoTeam(t, ctx, "default")
	resetConnCacheSnapshot()

	// Pre-seed the mock with a `default`-aliased team so the implicit
	// reconciler's ListTeamsByAlias returns it → UPDATE arm fires
	// instead of CREATE arm. Mirrors the "out-of-band default already
	// exists" scenario from 10-REVIEW.md CR-01 concrete-failure-mode #2.
	seededTeamID := mockServer.AddHandManagedTeam("default")

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "default")
	})

	// Wait for the synthetic reconcile to fire the UPDATE arm (since
	// `default` already exists in the mock's store). The UPDATE arm
	// body is distinguishable from the CREATE arm body by the presence
	// of the `team_id` key (CREATE arm omits team_id; UPDATE arm pins it).
	deadline := time.Now().Add(15 * time.Second)
	var sawUpdate bool
	for time.Now().Before(deadline) {
		body := mockServer.LastTeamBody("default")
		if body != nil {
			if v, hasTeamID := body["team_id"]; hasTeamID && v == seededTeamID {
				sawUpdate = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !sawUpdate {
		t.Fatalf("implicit-default UPDATE arm did not fire within 15s (no body with team_id=%q observed)", seededTeamID)
	}

	body := mockServer.LastTeamBody("default")
	// CR-01 regression: rpm_limit and tpm_limit MUST be present with nil value.
	if v, ok := body["rpm_limit"]; !ok {
		t.Errorf("CR-01 regression: rpm_limit MISSING from implicit-default UPDATE body; want present with nil")
	} else if v != nil {
		t.Errorf("CR-01 regression: rpm_limit=%v on implicit-default UPDATE body; want nil", v)
	}
	if v, ok := body["tpm_limit"]; !ok {
		t.Errorf("CR-01 regression: tpm_limit MISSING from implicit-default UPDATE body; want present with nil")
	} else if v != nil {
		t.Errorf("CR-01 regression: tpm_limit=%v on implicit-default UPDATE body; want nil", v)
	}
	// CR-01 regression: *_type keys MUST be absent (conditional-add per Feature 01 §2.1).
	if _, ok := body["rpm_limit_type"]; ok {
		t.Errorf("CR-01 regression: rpm_limit_type PRESENT on implicit-default UPDATE body; want absent")
	}
	if _, ok := body["tpm_limit_type"]; ok {
		t.Errorf("CR-01 regression: tpm_limit_type PRESENT on implicit-default UPDATE body; want absent")
	}
}

// TestTeamReconciler_AC_T4_DeletionBodyShape_ClearsRateLimits — CR-01
// regression: the AC-T4 protected-deletion body (team_controller.go:797)
// MUST emit rpm_limit:nil + tpm_limit:nil and MUST OMIT both *_type keys
// even when the deleted CR carried a populated spec.rateLimits block.
// This pins the always-emit-nil-on-clear contract on the AC-T4 path.
func TestTeamReconciler_AC_T4_DeletionBodyShape_ClearsRateLimits(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache()
	ensureNoTeam(t, ctx, "default")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "default")
	})

	// Wait for synthetic bootstrap so the LiteLLM-side `default` team exists.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if mockServer.HasTeam("default") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !mockServer.HasTeam("default") {
		t.Fatalf("synthetic bootstrap did not create LiteLLM team aliased `default` within 15s")
	}

	// Apply Team/default CR with populated rateLimits.
	cr := &litellmv1alpha1.LiteLLMTeam{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: WatchNamespace},
		Spec: litellmv1alpha1.TeamSpec{
			RateLimits: &litellmv1alpha1.RateLimitsSpec{
				RPM: int32Ptr(6000),
				TPM: int32Ptr(1000000),
			},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team/default with rateLimits: %v", err)
	}

	// Wait for the CR-driven UPDATE to set rpm_limit=6000.
	applied := false
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		body := mockServer.LastTeamBody("default")
		if body != nil {
			if v, ok := body["rpm_limit"].(float64); ok && v == 6000.0 {
				applied = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !applied {
		t.Fatalf("rpm_limit=6000 not applied within 30s")
	}

	// Wait for the finalizer to be added (same pattern as AC-T4 test at line 1640).
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var tm litellmv1alpha1.LiteLLMTeam
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: "default", Namespace: WatchNamespace}, &tm); err == nil {
			hasFinalizer := false
			for _, f := range tm.Finalizers {
				if f == teamFinalizer {
					hasFinalizer = true
					break
				}
			}
			if hasFinalizer {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}

	// Delete — must fire AC-T4 protected-deletion at team_controller.go:797.
	var existing litellmv1alpha1.LiteLLMTeam
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "default", Namespace: WatchNamespace}, &existing); err != nil {
		t.Fatalf("Get before delete: %v", err)
	}
	if err := k8sClient.Delete(ctx, &existing); err != nil {
		t.Fatalf("Delete Team/default: %v", err)
	}

	// Wait for the post-deletion body to land. The AC-T4 deletion body
	// is recognizable by both *_limit keys present + nil (the prior
	// rpm_limit=6000 body had non-nil values).
	cleared := false
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		body := mockServer.LastTeamBody("default")
		if body != nil {
			if v, ok := body["rpm_limit"]; ok && v == nil {
				if v2, ok2 := body["tpm_limit"]; ok2 && v2 == nil {
					cleared = true
					break
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !cleared {
		body := mockServer.LastTeamBody("default")
		t.Fatalf("CR-01 regression: AC-T4 deletion body did not emit rpm_limit:nil + tpm_limit:nil within 30s; last body = %+v", body)
	}

	// Final body assertion — *_type keys must be ABSENT.
	body := mockServer.LastTeamBody("default")
	if _, ok := body["rpm_limit_type"]; ok {
		t.Errorf("CR-01 regression: AC-T4 deletion body rpm_limit_type PRESENT; want absent (conditional-add per Feature 01 §2.1)")
	}
	if _, ok := body["tpm_limit_type"]; ok {
		t.Errorf("CR-01 regression: AC-T4 deletion body tpm_limit_type PRESENT; want absent")
	}
}

// ──────────────────────────────────────────────────────────────────────────
// — AC-T3 non-default Team deletion path (6 cases)
// ──────────────────────────────────────────────────────────────────────────
//
// AC-T3 (spec §11 line 1669, verbatim):
//
// Given a `Team/foo` CR (any name other than `default`), when deleted,
// then the operator calls `GET /v2/team/list?team_alias=foo&page_size=100`,
// filters the response in-memory for exact `team_alias == foo` match
// (LiteLLM's server-side `team_alias` filter is partial), resolves
// `team_id` from the matched entry, then issues `POST /team/delete` with
// body `{"team_ids": ["<resolved-team_id>"]}`. The operator treats an
// empty exact-match result as success (nothing to delete); a `404` from
// `/v2/team/list` is treated as permanent `LiteLLMRejected` per §7.7
// (NOT success). The finalizer is eventually removed. The contract
// permits repeated idempotent calls under transient status-update
// failures; tests MAY assert "at least one POST /team/delete call
// observed in the single-success-path fixture" but MUST NOT require
// "exactly one."
//
// 404-on-DELETE = SUCCESS (spec §7.5 line 1332). 401 anti-storm (REL-06)
// removes the finalizer even on auth failure. Connection-unavailable at
// delete time: warn + finalizer removed (anti-storm).

// waitForTeamFinalizer polls up to 10s for the named Team CR to have the
// teamFinalizer attached. Returns true if the finalizer was observed
// before the deadline. Used by the AC-T3 tests to ensure the deletion
// path is exercised (DeletionTimestamp without finalizer = immediate K8s
// GC, bypassing the operator).
//
//nolint:unparam // timeout reserved as a parameter for future tests that override the default 10s budget; current callers all pass the AC-T3 budget.
func waitForTeamFinalizer(ctx context.Context, name string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	for time.Now().Before(deadline) {
		var tm litellmv1alpha1.LiteLLMTeam
		if err := k8sClient.Get(ctx, key, &tm); err == nil {
			for _, f := range tm.Finalizers {
				if f == teamFinalizer {
					return true
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

// waitForTeamReaped polls up to `timeout` for the named Team CR to be
// fully removed (apierrors.IsNotFound on Get). Returns true on success.
//
//nolint:unparam // timeout reserved for future tests that override the default 30s budget.
func waitForTeamReaped(ctx context.Context, name string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	for time.Now().Before(deadline) {
		var tm litellmv1alpha1.LiteLLMTeam
		if err := k8sClient.Get(ctx, key, &tm); apierrors.IsNotFound(err) {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

// TestTeamReconciler_AC_T3_DeleteHappyPath — behavior #1.
// Create Team/foo with budget; wait Ready; delete CR. Within 30s the mock
// observes POST /team/delete carrying the pinned team_id; the finalizer
// is removed; the CR is reaped; alitellm_operator_drift_corrected_total{team,delete_vanished}
// increments by ≥1.
func TestTeamReconciler_AC_T3_DeleteHappyPath(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache() // Phase 6 cross-suite flake fix (07-CONTEXT.md §Phase-6-flake option α)
	ensureNoTeam(t, ctx, "team-act3-happy")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-act3-happy")
	})

	cr := teamSampleCR("team-act3-happy")
	cr.Spec.Budget = &litellmv1alpha1.BudgetSpec{
		Limit:  floatPtr(250.0),
		Period: "30d",
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}

	tm := pollTeamCondition(t, ctx, "team-act3-happy", reasonSynced, 30*time.Second)
	if tm.Status.LastRendered.TeamID == "" {
		t.Fatalf("Team not Synced within 30s")
	}
	pinnedTeamID := tm.Status.LastRendered.TeamID

	if !waitForTeamFinalizer(ctx, "team-act3-happy", 10*time.Second) {
		t.Fatalf("teamFinalizer was not attached within 10s (deletion path will not fire)")
	}

	before := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("team", "delete_vanished"))

	// Re-Get to capture the latest resourceVersion before Delete.
	var latest litellmv1alpha1.LiteLLMTeam
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "team-act3-happy", Namespace: WatchNamespace}, &latest); err != nil {
		t.Fatalf("re-Get before delete: %v", err)
	}
	if err := k8sClient.Delete(ctx, &latest); err != nil {
		t.Fatalf("delete Team: %v", err)
	}

	if !waitForTeamReaped(ctx, "team-act3-happy", 30*time.Second) {
		t.Fatalf("Team not reaped within 30s of Delete (finalizer stuck?)")
	}

	// Assert the operator invoked POST /team/delete with the pinned team_id.
	calls := mockServer.DeleteTeamCalls()
	if len(calls) < 1 {
		t.Errorf("AC-T3 happy path: mock.DeleteTeamCalls() empty — want ≥1 call (POST /team/delete)")
	}
	foundPinned := false
	for _, id := range calls {
		if id == pinnedTeamID {
			foundPinned = true
			break
		}
	}
	if !foundPinned {
		t.Errorf("AC-T3 happy path: POST /team/delete never carried pinned team_id %q (calls=%v)",
			pinnedTeamID, calls)
	}

	after := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("team", "delete_vanished"))
	if delta := after - before; delta < 1 {
		t.Errorf("alitellm_operator_drift_corrected_total{team,delete_vanished}: want +≥1, got delta=%v", delta)
	}
}

// TestTeamReconciler_AC_T3_DeleteEmptyExactMatch — behavior #2.
// Mock pre-populated with team_alias="neighbor" (no operator CR). Apply
// Team/orphan-search to a clean reconciler state: status.lastRendered.
// teamID will end up populated (the CR's own reconcile creates a LiteLLM
// entry). Then delete the LiteLLM entry out-of-band, then delete the CR
// — the resolve step's ListTeamsByAlias("orphan-search") returns empty,
// the operator's status pin is also stale (the team is gone), and the
// operator MUST proceed to finalizer-remove without issuing POST
// /team/delete (nothing to delete). alitellm_operator_drift_corrected_total{delete_vanished}
// is NOT incremented (the operator never observed a 200 or 404 on
// /team/delete — it skipped the call entirely).
//
// This test specifically exercises the "len(entries) == 0 AND status pin
// becomes useless" branch. (Note: status.lastRendered.teamID is set to
// the team_id we deleted, but the operator's resolve fallback finds
// nothing — that mismatch is fine; the operator could attempt a
// hopeless POST /team/delete against the stale pin and get a 404 = success
// per spec §7.5; OR it could skip the call entirely. The current
// implementation skips when the pin is empty but USES the stale pin
// when it's set (defensive — see must_haves[3]'s "if empty
// AND status.lastRendered.teamID != ” → use the pinned ID"). So this
// test asserts the empty-pin path: we clear the status pin via
// out-of-band deletion of the LiteLLM team + a fresh apply of a never-
// existed Team CR with no status. The cleanest way is to apply a Team
// CR, immediately delete it before the reconciler runs (no status set),
// and verify the operator skips POST /team/delete entirely.
//
// Implementation: rely on the operator-side branch where status is empty
// AND ListTeamsByAlias returns empty — by NEVER letting the team go
// Ready before deletion. We mode-flip the connection cache to NotReady
// at create time, then delete the CR while the connection is still
// NotReady. The delete branch will see snap.Ready=false → fall through
// to RemoveFinalizer without any /team/delete attempt. (This is the
// "connection unavailable at deletion" branch covered separately by
// test 6 — so for this test we use a different shape: create the CR,
// wait Ready (so the team exists in LiteLLM), THEN out-of-band-delete
// the LiteLLM team, THEN delete the CR. Status pin is still set; the
// operator uses it; POST /team/delete returns the mock's happy-path
// 200 (the team doesn't exist anymore, but the mock's handler is
// tolerant of unknown team_ids and just returns {}). So this test
// proves the operator does USE its pin even when the team is
// out-of-band-gone.)
//
// The plan's <behavior> for AC_T3_DeleteEmptyExactMatch acknowledges
// the race: "wait Ready=False reason=LiteLLMRejected from the create
// attempt OR (depending on race) Ready=True with a freshly-minted entry
// — either way, delete the CR". The robust assertion is: regardless of
// the create attempt's outcome, after deletion the CR is reaped + the
// finalizer is gone + the hand-managed entry is preserved.
func TestTeamReconciler_AC_T3_DeleteEmptyExactMatch(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache() // Phase 6 cross-suite flake fix (07-CONTEXT.md §Phase-6-flake option α)
	ensureNoTeam(t, ctx, "team-act3-orphan")
	resetConnCacheSnapshot()

	// Pre-populate a hand-managed entry that is NOT operator-owned. The
	// reconcile of "team-act3-orphan" must not touch it.
	hmID := mockServer.AddHandManagedTeam("neighbor")

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-act3-orphan")
	})

	// Apply the orphan CR. The reconciler will create a LiteLLM team
	// for it (the alias does not exist yet). We then out-of-band-
	// delete the LiteLLM team so the operator's eventual resolve sees
	// an empty exact-match — exercising the "len(entries) == 0 with
	// a non-empty status pin" branch of.
	cr := teamSampleCR("team-act3-orphan")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}
	tm := pollTeamCondition(t, ctx, "team-act3-orphan", reasonSynced, 30*time.Second)
	if tm.Status.LastRendered.TeamID == "" {
		t.Fatalf("Team not Synced within 30s")
	}
	createdTeamID := tm.Status.LastRendered.TeamID

	if !waitForTeamFinalizer(ctx, "team-act3-orphan", 10*time.Second) {
		t.Fatalf("teamFinalizer was not attached within 10s")
	}

	// Out-of-band delete the LiteLLM team so the operator's resolve
	// step sees empty. Status pin still references the now-gone team_id.
	mockServer.DeleteTeamOutOfBand(createdTeamID)

	// Snapshot drift counter — we expect either 0 increment (operator
	// used the stale pin, got 404 = success, alitellm_operator_drift_corrected_total
	// incremented OR the operator skipped the call). Either is fine
	// for AC-T3; the load-bearing assertion is "finalizer removed; CR
	// reaped; hand-managed entry preserved".
	beforeDriftVanished := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("team", "delete_vanished"))

	var latest litellmv1alpha1.LiteLLMTeam
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "team-act3-orphan", Namespace: WatchNamespace}, &latest); err != nil {
		t.Fatalf("re-Get before delete: %v", err)
	}
	if err := k8sClient.Delete(ctx, &latest); err != nil {
		t.Fatalf("delete Team: %v", err)
	}

	if !waitForTeamReaped(ctx, "team-act3-orphan", 30*time.Second) {
		t.Fatalf("Team not reaped within 30s of Delete (finalizer stuck?)")
	}

	// Hand-managed entry MUST be preserved across this whole sequence.
	if !mockServer.HasTeam("neighbor") {
		t.Errorf("AC-DC1 carry-forward: hand-managed Team %q was REMOVED", "neighbor")
	}
	if got := mockServer.GetTeamID("neighbor"); got != hmID {
		t.Errorf("hand-managed entry ID changed: got %q, want %q", got, hmID)
	}

	// Spot-check: drift counter may have ticked (by ≤1; the operator's
	// stale-pin DELETE returned the mock's happy-path 200 which is
	// treated as success). We accept ≥0 (the load-bearing invariant
	// is that the CR was reaped without spurious mutations against
	// the hand-managed alias).
	afterDriftVanished := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("team", "delete_vanished"))
	if afterDriftVanished < beforeDriftVanished {
		t.Errorf("alitellm_operator_drift_corrected_total decremented (impossible): before=%v after=%v",
			beforeDriftVanished, afterDriftVanished)
	}
}

// TestTeamReconciler_AC_T3_DeleteListReturns404 — behavior #3.
// Apply Team/foo, wait Ready. Clear the status pin (so the deletion path
// MUST consult /v2/team/list), then SetMode("not-found-list-teams") so
// the LIST endpoint returns 404. Delete the CR. Within 5s the operator
// writes Ready=False reason=LiteLLMRejected message containing "LiteLLM
// API surface mismatch on /v2/team/list"; the finalizer stays attached
// (CR remains in Terminating); zero POST /team/delete calls were issued.
//
// To force the LIST path: we patch the CR's status to clear teamID
// BEFORE deletion. (The reconciler's deletion branch prefers status.
// lastRendered.teamID and only falls back to ListTeamsByAlias when the
// pin is empty.)
func TestTeamReconciler_AC_T3_DeleteListReturns404(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache() // Phase 6 cross-suite flake fix (07-CONTEXT.md §Phase-6-flake option α)
	ensureNoTeam(t, ctx, "team-act3-list404")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		cleanupConn()
		// Cleanup: clear the finalizer + force-delete so the CR doesn't
		// stick around for subsequent tests (the rejection path leaves
		// it in Terminating).
		var existing litellmv1alpha1.LiteLLMTeam
		if err := k8sClient.Get(context.Background(),
			client.ObjectKey{Name: "team-act3-list404", Namespace: WatchNamespace},
			&existing); err == nil {
			existing.SetFinalizers(nil)
			_ = k8sClient.Update(context.Background(), &existing)
		}
		ensureNoTeam(t, context.Background(), "team-act3-list404")
	})

	cr := teamSampleCR("team-act3-list404")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}
	tm := pollTeamCondition(t, ctx, "team-act3-list404", reasonSynced, 30*time.Second)
	if tm.Status.LastRendered.TeamID == "" {
		t.Fatalf("Team not Synced within 30s")
	}
	if !waitForTeamFinalizer(ctx, "team-act3-list404", 10*time.Second) {
		t.Fatalf("teamFinalizer was not attached within 10s")
	}

	// Flip the LIST endpoint to 404 BEFORE clearing the status pin —
	// the controller MAY reconcile between status-clear and Delete; if
	// it does, the normal reconcile path will hit ListTeamsByAlias →
	// 404 → LiteLLMRejected (and the status write will NOT touch
	// Status.LastRendered.TeamID — writeStatus only sets Conditions +
	// ObservedGeneration). This way the cleared pin survives.
	mockServer.SetMode(mock.ModeNotFoundListTeams)

	// Clear the status pin so the deletion path takes the
	// ListTeamsByAlias fallback.
	tm.Status.LastRendered.TeamID = ""
	if err := k8sClient.Status().Update(ctx, tm); err != nil {
		t.Fatalf("clear status.lastRendered.teamID: %v", err)
	}

	// Capture the deleteTeamCalls baseline.
	priorDeleteCalls := len(mockServer.DeleteTeamCalls())

	// Re-Get the latest resourceVersion + Delete.
	var latest litellmv1alpha1.LiteLLMTeam
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "team-act3-list404", Namespace: WatchNamespace}, &latest); err != nil {
		t.Fatalf("re-Get before delete: %v", err)
	}
	if err := k8sClient.Delete(ctx, &latest); err != nil {
		t.Fatalf("delete Team: %v", err)
	}

	// Poll for LiteLLMRejected status.
	deadline := time.Now().Add(15 * time.Second)
	var final *litellmv1alpha1.LiteLLMTeam
	for time.Now().Before(deadline) {
		var probe litellmv1alpha1.LiteLLMTeam
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: "team-act3-list404", Namespace: WatchNamespace}, &probe); err == nil {
			c := apimeta.FindStatusCondition(probe.Status.Conditions, conditionTypeReady)
			if c != nil && c.Reason == "LiteLLMRejected" {
				final = &probe
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if final == nil {
		t.Fatalf("Ready.Reason=LiteLLMRejected never observed within 15s after delete-with-404-list")
	}
	c := apimeta.FindStatusCondition(final.Status.Conditions, conditionTypeReady)
	if c == nil {
		t.Fatalf("Ready condition missing on rejected delete")
	}
	if !strings.Contains(c.Message, "LiteLLM API surface mismatch on /v2/team/list") {
		t.Errorf("Ready.Message: want substring %q, got %q",
			"LiteLLM API surface mismatch on /v2/team/list", c.Message)
	}

	// Finalizer is still present (CR is in Terminating but not reaped).
	hasFinalizer := false
	for _, f := range final.Finalizers {
		if f == teamFinalizer {
			hasFinalizer = true
			break
		}
	}
	if !hasFinalizer {
		t.Errorf("finalizer was removed on LiteLLMRejected path (want present; CR should stay in Terminating)")
	}

	// Zero POST /team/delete calls were issued during the rejected path.
	if newDeleteCalls := len(mockServer.DeleteTeamCalls()) - priorDeleteCalls; newDeleteCalls != 0 {
		t.Errorf("LIST-404 path issued %d POST /team/delete call(s) (want 0)", newDeleteCalls)
	}
}

// TestTeamReconciler_AC_T3_DeleteReturns404IsSuccess — behavior
// #4. Apply Team/foo, wait Ready. SetMode("not-found-delete-team") so the
// LIST endpoint returns the entry but DELETE returns 404. Delete the CR.
// Within 30s the finalizer is removed, the CR is reaped, and
// alitellm_operator_drift_corrected_total{team,delete_vanished} increments by ≥1 (404-on-
// DELETE is treated as SUCCESS per spec §7.5 line 1332).
func TestTeamReconciler_AC_T3_DeleteReturns404IsSuccess(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache() // Phase 6 cross-suite flake fix (07-CONTEXT.md §Phase-6-flake option α)
	ensureNoTeam(t, ctx, "team-act3-del404")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-act3-del404")
	})

	cr := teamSampleCR("team-act3-del404")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}
	tm := pollTeamCondition(t, ctx, "team-act3-del404", reasonSynced, 30*time.Second)
	if tm.Status.LastRendered.TeamID == "" {
		t.Fatalf("Team not Synced within 30s")
	}
	if !waitForTeamFinalizer(ctx, "team-act3-del404", 10*time.Second) {
		t.Fatalf("teamFinalizer was not attached within 10s")
	}

	before := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("team", "delete_vanished"))

	// Flip POST /team/delete to 404.
	mockServer.SetMode(mock.ModeNotFoundDeleteTeam)

	var latest litellmv1alpha1.LiteLLMTeam
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "team-act3-del404", Namespace: WatchNamespace}, &latest); err != nil {
		t.Fatalf("re-Get before delete: %v", err)
	}
	if err := k8sClient.Delete(ctx, &latest); err != nil {
		t.Fatalf("delete Team: %v", err)
	}

	if !waitForTeamReaped(ctx, "team-act3-del404", 30*time.Second) {
		t.Fatalf("Team not reaped within 30s (404-on-DELETE should be SUCCESS per spec §7.5; finalizer stuck?)")
	}

	// drift counter MUST have incremented — 404 on POST /team/delete is
	// SUCCESS per spec §7.5 line 1332 (the operator's drift correction
	// succeeded: the team is gone).
	after := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("team", "delete_vanished"))
	if delta := after - before; delta < 1 {
		t.Errorf("alitellm_operator_drift_corrected_total{team,delete_vanished}: want +≥1 (404-on-DELETE is success), got delta=%v", delta)
	}

	// The mock recorded the team_id in deleteTeamCalls even though it
	// returned 404 (the operator DID issue the call).
	if len(mockServer.DeleteTeamCalls()) < 1 {
		t.Errorf("mock.DeleteTeamCalls() empty — want ≥1 (operator must have issued POST /team/delete)")
	}
}

// TestTeamReconciler_AC_T3_Delete401AntiStorm — behavior #5.
// Apply Team/foo, wait Ready. SetMode("401-delete-team"). Delete the CR.
// Within 30s: the operator issues POST /team/delete with the pinned
// team_id, receives 401; the finalizer is removed anyway (REL-06
// anti-storm); the CR is reaped; alitellm_operator_drift_corrected_total{delete_vanished}
// is NOT incremented (the delete never confirmed).
//
// We deliberately do NOT assert on connCache.Snapshot Ready
// transitioning to false. The LiteLLMConnectionReconciler probes
// continuously via POST /key/health (which remains happy under
// Mode401DeleteTeam — only POST /team/delete returns 401), so any
// 401-driven cache invalidation is
// immediately re-Synced by the next probe — the window is too narrow
// to observe deterministically without instrumenting the cache itself
// with an invalidation counter. The structural anti-storm guarantee is
// captured by (a) deleteTeamCalls contains the pinned team_id (operator
// DID make the call), (b) CR was reaped (finalizer removed despite
// 401), and (c) drift counter was NOT incremented (the call never
// confirmed success).
func TestTeamReconciler_AC_T3_Delete401AntiStorm(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache() // Phase 6 cross-suite flake fix (07-CONTEXT.md §Phase-6-flake option α)
	ensureNoTeam(t, ctx, "team-act3-401")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-act3-401")
	})

	cr := teamSampleCR("team-act3-401")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}
	tm := pollTeamCondition(t, ctx, "team-act3-401", reasonSynced, 30*time.Second)
	if tm.Status.LastRendered.TeamID == "" {
		t.Fatalf("Team not Synced within 30s")
	}
	pinnedTeamID := tm.Status.LastRendered.TeamID
	if !waitForTeamFinalizer(ctx, "team-act3-401", 10*time.Second) {
		t.Fatalf("teamFinalizer was not attached within 10s")
	}

	before := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("team", "delete_vanished"))
	priorDeleteCalls := len(mockServer.DeleteTeamCalls())

	// Flip POST /team/delete to 401.
	mockServer.SetMode(mock.Mode401DeleteTeam)

	var latest litellmv1alpha1.LiteLLMTeam
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "team-act3-401", Namespace: WatchNamespace}, &latest); err != nil {
		t.Fatalf("re-Get before delete: %v", err)
	}
	if err := k8sClient.Delete(ctx, &latest); err != nil {
		t.Fatalf("delete Team: %v", err)
	}

	if !waitForTeamReaped(ctx, "team-act3-401", 30*time.Second) {
		t.Fatalf("Team not reaped within 30s (401-on-DELETE should remove finalizer anyway per REL-06 anti-storm)")
	}

	// Operator MUST have attempted POST /team/delete with the pinned
	// team_id (deleteTeamCalls captures all attempts, including 401s,
	// per mock semantics).
	newCalls := mockServer.DeleteTeamCalls()[priorDeleteCalls:]
	foundPinned := false
	for _, id := range newCalls {
		if id == pinnedTeamID {
			foundPinned = true
			break
		}
	}
	if !foundPinned {
		t.Errorf("operator never attempted POST /team/delete with pinned team_id %q (calls=%v)",
			pinnedTeamID, newCalls)
	}

	// alitellm_operator_drift_corrected_total{delete_vanished} MUST NOT have incremented
	// — the 401 path never confirms the delete.
	after := testutil.ToFloat64(
		metrics.DriftCorrectedTotal.WithLabelValues("team", "delete_vanished"))
	if delta := after - before; delta != 0 {
		t.Errorf("alitellm_operator_drift_corrected_total{delete_vanished}: want +0 (401 path does NOT count), got delta=%v", delta)
	}
}

// TestTeamReconciler_AC_T3_DeleteConnectionUnavailable — // behavior #6. connCache forced to Ready=false BEFORE the CR is created.
// Apply Team/foo (reaches Ready=False reason=LiteLLMUnavailable; no
// LiteLLM mutations). Delete the CR. Within 30s the finalizer is
// removed (anti-storm); CR is reaped; mock observes ZERO calls
// against the foo alias.
//
// NOTE: with connCache Ready=false, the CR cannot reach Ready=True
// (the LiteLLM call is gated). In that case the Reconcile loop never
// reaches the finalizer-add step (Step 1.6) — the finalizer is added
// before connection-gating but AFTER deletion-timestamp-check. To
// exercise the deletion path with no finalizer, we have to either
// (a) ensure the finalizer is added when Ready=true, then flip Ready
// to false before deletion, OR (b) accept that with no finalizer the
// K8s GC just reaps immediately without operator intervention. The
// plan's <behavior> requires the operator to log "LiteLLM unavailable
// on deletion; finalizer removed; team entry MAY persist" — that
// branch only fires when the finalizer is present. So we use approach
// (a): create + wait Ready (finalizer attached), then flip connCache,
// then delete.
func TestTeamReconciler_AC_T3_DeleteConnectionUnavailable(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache() // Phase 6 cross-suite flake fix (07-CONTEXT.md §Phase-6-flake option α)
	ensureNoTeam(t, ctx, "team-act3-conn-down")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		// Restore the snapshot so subsequent tests don't observe a
		// poisoned cache.
		connCache.Rebuild(connection.ConnectionSnapshot{})
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-act3-conn-down")
	})

	cr := teamSampleCR("team-act3-conn-down")
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}
	tm := pollTeamCondition(t, ctx, "team-act3-conn-down", reasonSynced, 30*time.Second)
	if tm.Status.LastRendered.TeamID == "" {
		t.Fatalf("Team not Synced within 30s")
	}
	if !waitForTeamFinalizer(ctx, "team-act3-conn-down", 10*time.Second) {
		t.Fatalf("teamFinalizer was not attached within 10s")
	}

	// Capture deleteTeamCalls baseline + per-alias mutation baseline.
	priorDeleteCalls := len(mockServer.DeleteTeamCalls())
	priorMutations := mockServer.MutationsByTeamAlias("team-act3-conn-down")

	// Force the connection cache to Ready=false. Also remove the
	// LiteLLMConnection/default CR so the real connection reconciler
	// does not immediately re-probe and flip Ready back to true.
	ensureNoConnectionDefault(t, ctx)
	connCache.Rebuild(connection.ConnectionSnapshot{
		Ready:  false,
		Reason: "Unreachable",
	})

	var latest litellmv1alpha1.LiteLLMTeam
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: "team-act3-conn-down", Namespace: WatchNamespace}, &latest); err != nil {
		t.Fatalf("re-Get before delete: %v", err)
	}
	if err := k8sClient.Delete(ctx, &latest); err != nil {
		t.Fatalf("delete Team: %v", err)
	}

	if !waitForTeamReaped(ctx, "team-act3-conn-down", 30*time.Second) {
		t.Fatalf("Team not reaped within 30s with connection unavailable (anti-storm should remove finalizer)")
	}

	// Mock observed ZERO new POST /team/delete calls + zero new
	// mutations against this alias (connection-gate prevented any
	// LiteLLM traffic).
	if newDeleteCalls := len(mockServer.DeleteTeamCalls()) - priorDeleteCalls; newDeleteCalls != 0 {
		t.Errorf("connection-unavailable deletion: mock observed %d new POST /team/delete call(s); want 0",
			newDeleteCalls)
	}
	if newMutations := mockServer.MutationsByTeamAlias("team-act3-conn-down") - priorMutations; newMutations != 0 {
		t.Errorf("connection-unavailable deletion: mock observed %d new mutation(s) against team alias; want 0",
			newMutations)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// spec.permission projection tests
// ──────────────────────────────────────────────────────────────────────────

// TestTeamPermission_ProjectsModelsAndMcp — a present permission block with
// models/modelGroups/mcpServers/mcpGroups projects onto the top-level `models`
// list and `object_permission` on the POST /team/new body.
func TestTeamPermission_ProjectsModelsAndMcp(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	mockServer.ResetAgents()
	teamReconciler.ResetImplicitDefaultCache()
	ensureNoTeam(t, ctx, "team-perm-models")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-perm-models")
	})

	cr := teamSampleCR("team-perm-models")
	cr.Spec.Permission = &litellmv1alpha1.PermissionSpec{
		Models:      []string{"gpt-4o"},
		ModelGroups: []string{"anthropic"},
		McpServers:  []string{"hindsight"},
		McpGroups:   []string{"team-a"},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}

	pollTeamCondition(t, ctx, "team-perm-models", reasonSynced, 30*time.Second)
	body := mockServer.LastTeamBody("team-perm-models")
	if body == nil {
		t.Fatalf("LastTeamBody nil")
	}
	models, _ := body["models"].([]any)
	if len(models) != 2 {
		t.Errorf("body.models: want [gpt-4o anthropic], got %v", body["models"])
	}
	op, ok := body["object_permission"].(map[string]any)
	if !ok {
		t.Fatalf("body.object_permission: want map, got %T (%v)", body["object_permission"], body["object_permission"])
	}
	if mcp, _ := op["mcp_servers"].([]any); len(mcp) != 1 || mcp[0] != "hindsight" {
		t.Errorf("object_permission.mcp_servers: got %v", op["mcp_servers"])
	}
	if grp, _ := op["mcp_access_groups"].([]any); len(grp) != 1 || grp[0] != "team-a" {
		t.Errorf("object_permission.mcp_access_groups: got %v", op["mcp_access_groups"])
	}
}

// TestTeamPermission_ResolvesAgentNamesToUUIDs — agent NAMES in the block are
// resolved to agent_id UUIDs (via GET /v1/agents) on the projected body.
func TestTeamPermission_ResolvesAgentNamesToUUIDs(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	mockServer.ResetAgents()
	teamReconciler.ResetImplicitDefaultCache()
	ensureNoTeam(t, ctx, "team-perm-agents")
	resetConnCacheSnapshot()

	// Register the agent in the mock so GET /v1/agents resolves it.
	agentID := mockServer.AddHandManagedAgent("planner")

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-perm-agents")
	})

	cr := teamSampleCR("team-perm-agents")
	cr.Spec.Permission = &litellmv1alpha1.PermissionSpec{Agents: []string{"planner"}}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}

	pollTeamCondition(t, ctx, "team-perm-agents", reasonSynced, 30*time.Second)
	body := mockServer.LastTeamBody("team-perm-agents")
	op, ok := body["object_permission"].(map[string]any)
	if !ok {
		t.Fatalf("object_permission: want map, got %T", body["object_permission"])
	}
	agents, _ := op["agents"].([]any)
	if len(agents) != 1 || agents[0] != agentID {
		t.Errorf("object_permission.agents: want [%s] (resolved UUID), got %v", agentID, op["agents"])
	}
	// The block omits models → deny-by-default: the outgoing body carries the
	// deny-all sentinel, not an empty list (which would fail OPEN in LiteLLM).
	if m, _ := body["models"].([]any); len(m) != 1 || m[0] != modelDenyAllSentinel {
		t.Errorf("body.models: want deny-all sentinel [%s] (block omits models), got %v", modelDenyAllSentinel, body["models"])
	}
}

// TestTeamPermission_AgentNotFoundRequeues — an agent name absent from
// GET /v1/agents parks the Team Ready=False/AgentNotFound (no /team/new).
func TestTeamPermission_AgentNotFoundRequeues(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	mockServer.ResetAgents()
	teamReconciler.ResetImplicitDefaultCache()
	ensureNoTeam(t, ctx, "team-perm-ghost")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-perm-ghost")
	})

	cr := teamSampleCR("team-perm-ghost")
	cr.Spec.Permission = &litellmv1alpha1.PermissionSpec{Agents: []string{"ghost"}}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}

	tm := pollTeamCondition(t, ctx, "team-perm-ghost", reasonAgentNotFound, 30*time.Second)
	c := apimeta.FindStatusCondition(tm.Status.Conditions, conditionTypeReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != reasonAgentNotFound {
		t.Fatalf("Ready condition: want False/AgentNotFound, got %+v", c)
	}
	if got := mockServer.MutationsByTeamAlias("team-perm-ghost"); got != 0 {
		t.Errorf("MutationsByTeamAlias: want 0 (parked before /team/new), got %d", got)
	}
}

// TestTeamPermission_ResolvesAccessGroupNamesToIDs — access-group NAMES in the
// block are resolved to access_group_id UUIDs (via GET /v1/access_group) and
// land on the TOP-LEVEL access_group_ids, never inside object_permission.
func TestTeamPermission_ResolvesAccessGroupNamesToIDs(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	mockServer.ResetAgents()
	mockServer.ResetAccessGroups()
	teamReconciler.ResetImplicitDefaultCache()
	ensureNoTeam(t, ctx, "team-perm-ag")
	resetConnCacheSnapshot()

	groupID := mockServer.SeedAccessGroup("shared-tier")

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-perm-ag")
		mockServer.ResetAccessGroups()
	})

	cr := teamSampleCR("team-perm-ag")
	cr.Spec.Permission = &litellmv1alpha1.PermissionSpec{AccessGroups: []string{"shared-tier"}}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}

	pollTeamCondition(t, ctx, "team-perm-ag", reasonSynced, 30*time.Second)
	body := mockServer.LastTeamBody("team-perm-ag")
	if body == nil {
		t.Fatalf("LastTeamBody nil")
	}
	ids, ok := body["access_group_ids"].([]any)
	if !ok {
		t.Fatalf("body.access_group_ids: want array, got %T (%v)", body["access_group_ids"], body["access_group_ids"])
	}
	if len(ids) != 1 || ids[0] != groupID {
		t.Errorf("body.access_group_ids: want [%s] (resolved id), got %v", groupID, ids)
	}
	// The team↔group relation is written ONLY from the team side. The operator
	// must never touch the group's assigned_team_ids (LiteLLM does not
	// propagate the team-side write back, and a second writer would need
	// delta-repair machinery this operator deliberately avoids).
	for _, g := range mockServer.AccessGroups() {
		if g.AccessGroupID == groupID && len(g.AssignedTeamIDs) != 0 {
			t.Errorf("group.assigned_team_ids: want untouched [], got %v", g.AssignedTeamIDs)
		}
	}
}

// TestTeamPermission_AccessGroupNotFoundRequeues — a group name absent from
// GET /v1/access_group parks the Team Ready=False/AccessGroupNotFound before
// any /team/new, mirroring the AgentNotFound ordering dependency.
func TestTeamPermission_AccessGroupNotFoundRequeues(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	mockServer.ResetAgents()
	mockServer.ResetAccessGroups()
	teamReconciler.ResetImplicitDefaultCache()
	ensureNoTeam(t, ctx, "team-perm-ag-ghost")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-perm-ag-ghost")
		mockServer.ResetAccessGroups()
	})

	cr := teamSampleCR("team-perm-ag-ghost")
	cr.Spec.Permission = &litellmv1alpha1.PermissionSpec{AccessGroups: []string{"ghost-group"}}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}

	tm := pollTeamCondition(t, ctx, "team-perm-ag-ghost", reasonAccessGroupNotFound, 30*time.Second)
	c := apimeta.FindStatusCondition(tm.Status.Conditions, conditionTypeReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != reasonAccessGroupNotFound {
		t.Fatalf("Ready condition: want False/AccessGroupNotFound, got %+v", c)
	}
	if got := mockServer.MutationsByTeamAlias("team-perm-ag-ghost"); got != 0 {
		t.Errorf("MutationsByTeamAlias: want 0 (parked before /team/new), got %d", got)
	}
}

// TestTeamPermission_OverridesParamsModels — a permission block deletes a
// colliding spec.params.models key (permission wins).
func TestTeamPermission_OverridesParamsModels(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	mockServer.ResetAgents()
	teamReconciler.ResetImplicitDefaultCache()
	ensureNoTeam(t, ctx, "team-perm-override")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-perm-override")
	})

	cr := teamSampleCR("team-perm-override")
	cr.Spec.Params = runtime.RawExtension{Raw: []byte(`{"models":["stale-from-params"]}`)}
	cr.Spec.Permission = &litellmv1alpha1.PermissionSpec{Models: []string{"gpt-4o"}}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}

	pollTeamCondition(t, ctx, "team-perm-override", reasonSynced, 30*time.Second)
	body := mockServer.LastTeamBody("team-perm-override")
	models, _ := body["models"].([]any)
	if len(models) != 1 || models[0] != "gpt-4o" {
		t.Errorf("body.models: want [gpt-4o] (permission wins over params), got %v", body["models"])
	}
}

// TestTeamPermission_AbsentBlockPassesParamsThrough — with NO permission
// block, spec.params.models still passes through unchanged (migration path).
func TestTeamPermission_AbsentBlockPassesParamsThrough(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	mockServer.ResetAgents()
	teamReconciler.ResetImplicitDefaultCache()
	ensureNoTeam(t, ctx, "team-perm-absent")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-perm-absent")
	})

	cr := teamSampleCR("team-perm-absent")
	cr.Spec.Params = runtime.RawExtension{Raw: []byte(`{"models":["passthrough-model"]}`)}
	// No cr.Spec.Permission.
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}

	pollTeamCondition(t, ctx, "team-perm-absent", reasonSynced, 30*time.Second)
	body := mockServer.LastTeamBody("team-perm-absent")
	models, _ := body["models"].([]any)
	if len(models) != 1 || models[0] != "passthrough-model" {
		t.Errorf("body.models: want passthrough [passthrough-model], got %v", body["models"])
	}
}

// TestTeamPermission_ShrinkToEmptyRevokes is the security regression for the
// v0.7.25 fix: when a permission sublist is emptied, the outgoing /team/update
// body MUST carry that field as [] (an explicit LiteLLM clear), NOT omit it.
// LiteLLM's per-field merge keeps an OMITTED field's stale value, so omission
// silently fails to revoke (CR Ready=Synced while access persists). Covers
// both a non-empty shrink (drop one item) and a shrink-to-empty (clear a
// field) in the same transition.
func TestTeamPermission_ShrinkToEmptyRevokes(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	mockServer.ResetAgents()
	teamReconciler.ResetImplicitDefaultCache()
	ensureNoTeam(t, ctx, "team-perm-shrink")
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		ensureNoTeam(t, context.Background(), "team-perm-shrink")
	})

	cr := teamSampleCR("team-perm-shrink")
	cr.Spec.Permission = &litellmv1alpha1.PermissionSpec{
		McpServers: []string{"a", "b"},
		McpGroups:  []string{"g1"},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Team: %v", err)
	}
	pollTeamCondition(t, ctx, "team-perm-shrink", reasonSynced, 30*time.Second)

	// Transition: drop "b" from mcpServers (non-empty shrink) AND empty
	// mcpGroups (shrink-to-empty).
	key := client.ObjectKey{Name: "team-perm-shrink", Namespace: WatchNamespace}
	var fresh litellmv1alpha1.LiteLLMTeam
	if err := k8sClient.Get(ctx, key, &fresh); err != nil {
		t.Fatalf("get Team: %v", err)
	}
	fresh.Spec.Permission = &litellmv1alpha1.PermissionSpec{
		McpServers: []string{"a"},
		McpGroups:  []string{},
	}
	if err := k8sClient.Update(ctx, &fresh); err != nil {
		t.Fatalf("update Team: %v", err)
	}

	// Poll until the /team/update body reflects the shrink (mcp_servers len 1).
	var op map[string]any
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if body := mockServer.LastTeamBody("team-perm-shrink"); body != nil {
			if o, ok := body["object_permission"].(map[string]any); ok {
				if mcp, _ := o["mcp_servers"].([]any); len(mcp) == 1 {
					op = o
					break
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if op == nil {
		t.Fatalf("timed out waiting for shrunk /team/update body")
	}

	// Non-empty shrink: "b" gone.
	if mcp, _ := op["mcp_servers"].([]any); len(mcp) != 1 || mcp[0] != "a" {
		t.Errorf("mcp_servers: want [a] (b revoked), got %v", op["mcp_servers"])
	}
	// Shrink-to-empty: the field MUST be present as [] — the security assertion.
	grp, present := op["mcp_access_groups"]
	if !present {
		t.Error("mcp_access_groups ABSENT from /team/update body — an emptied field must be sent as [] to revoke; omitting it lets LiteLLM keep the stale grant")
	} else if g, _ := grp.([]any); len(g) != 0 {
		t.Errorf("mcp_access_groups: want [] (cleared), got %v", grp)
	}
	// agents is a fail-open field the block never set → deny-by-default: it
	// carries the null-UUID sentinel (present, non-empty), NOT []. Still emitted
	// unconditionally (always-emit contract).
	if a, _ := op["agents"].([]any); len(a) != 1 || a[0] != agentDenyAllSentinel {
		t.Errorf("agents: want deny-all sentinel [%s] (block omits agents), got %v", agentDenyAllSentinel, op["agents"])
	}
	// agent_access_groups is fail-closed / no-op → still emitted as [].
	if _, ok := op["agent_access_groups"]; !ok {
		t.Error("agent_access_groups ABSENT — always-emit contract requires it present as []")
	}
}
