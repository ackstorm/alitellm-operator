// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
)

// TestRenderAccessGroup_ResolvesNamesAndKeepsModelsVerbatim pins the three
// dimensions' differing treatment: models pass through, MCP servers and agents
// are resolved name→id because LiteLLM matches those on ids and silently
// ignores names.
func TestRenderAccessGroup_ResolvesNamesAndKeepsModelsVerbatim(t *testing.T) {
	spec := litellmv1alpha1.AccessGroupSpec{
		Models:     []string{"gpt-4", "claude-opus"},
		MCPServers: []string{"slack"},
		Agents:     []string{"finops"},
	}
	serverIDs := map[string]string{"slack": "srv-1"}
	agentIDs := map[string]string{"finops": "agt-1"}

	got, missing := renderAccessGroup(spec, serverIDs, agentIDs)
	if len(missing.MCPServers)+len(missing.Agents) != 0 {
		t.Fatalf("unexpected missing: %+v", missing)
	}
	if len(got.Models) != 2 || got.Models[0] != "claude-opus" {
		t.Errorf("Models = %v, want the two spec names (sorted)", got.Models)
	}
	if len(got.MCPServerIDs) != 1 || got.MCPServerIDs[0] != "srv-1" {
		t.Errorf("MCPServerIDs = %v, want [srv-1]", got.MCPServerIDs)
	}
	if len(got.AgentIDs) != 1 || got.AgentIDs[0] != "agt-1" {
		t.Errorf("AgentIDs = %v, want [agt-1]", got.AgentIDs)
	}
}

// TestRenderAccessGroup_EmptySpecRendersNonNilLists guards the CLEAR contract:
// nil slices would serialize as null/absent and KEEP a stale grant upstream.
func TestRenderAccessGroup_EmptySpecRendersNonNilLists(t *testing.T) {
	got, _ := renderAccessGroup(litellmv1alpha1.AccessGroupSpec{}, nil, nil)
	if got.Models == nil || got.MCPServerIDs == nil || got.AgentIDs == nil {
		t.Fatalf("nil slice in %+v — an omitted list KEEPS the stale value upstream", got)
	}
}

// TestRenderAccessGroup_ReportsUnresolvedNames drives the parking path: an
// unresolved name must be reported, never silently dropped (a dropped name is
// a silent authorization gap).
func TestRenderAccessGroup_ReportsUnresolvedNames(t *testing.T) {
	spec := litellmv1alpha1.AccessGroupSpec{
		MCPServers: []string{"slack", "ghost"},
		Agents:     []string{"nobody"},
	}
	_, missing := renderAccessGroup(spec, map[string]string{"slack": "srv-1"}, nil)
	if len(missing.MCPServers) != 1 || missing.MCPServers[0] != "ghost" {
		t.Errorf("missing.MCPServers = %v, want [ghost]", missing.MCPServers)
	}
	if len(missing.Agents) != 1 || missing.Agents[0] != "nobody" {
		t.Errorf("missing.Agents = %v, want [nobody]", missing.Agents)
	}
}

// TestAccessGroupHash_StableAcrossDeclarationOrder guards the steady-state
// short-circuit: a reordered spec must not look like drift and trigger a PUT.
func TestAccessGroupHash_StableAcrossDeclarationOrder(t *testing.T) {
	a, _ := renderAccessGroup(litellmv1alpha1.AccessGroupSpec{
		Models: []string{"m1", "m2"}}, nil, nil)
	b, _ := renderAccessGroup(litellmv1alpha1.AccessGroupSpec{
		Models: []string{"m2", "m1"}}, nil, nil)
	if accessGroupHash(a) != accessGroupHash(b) {
		t.Error("hash differs on declaration order — every reconcile would PUT")
	}
}

// ── envtest helpers ───────────────────────────────────────────────────────

func resetMockAccessGroup() {
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetAccessGroups()
}

func accessGroupSampleCR(name string, spec litellmv1alpha1.AccessGroupSpec) *litellmv1alpha1.LiteLLMAccessGroup {
	return &litellmv1alpha1.LiteLLMAccessGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: WatchNamespace},
		Spec:       spec,
	}
}

// ensureNoAccessGroup deletes any pre-existing access-group CR with the given
// name and waits for full removal (finalizer drain included).
func ensureNoAccessGroup(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	var existing litellmv1alpha1.LiteLLMAccessGroup
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	if err := k8sClient.Get(ctx, key, &existing); err == nil {
		controllerutil.RemoveFinalizer(&existing, accessGroupFinalizer)
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
	t.Logf("warning: LiteLLMAccessGroup %q still present after 10s cleanup wait", name)
}

// accessGroupPollTimeout bounds every Ready-condition poll in this file.
const accessGroupPollTimeout = 30 * time.Second

// pollAccessGroupCondition polls the Ready condition until reason matches or
// accessGroupPollTimeout elapses. Returns the final re-Get'd CR either way.
func pollAccessGroupCondition(t *testing.T, ctx context.Context, name, wantReason string) *litellmv1alpha1.LiteLLMAccessGroup {
	t.Helper()
	deadline := time.Now().Add(accessGroupPollTimeout)
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	var ag litellmv1alpha1.LiteLLMAccessGroup
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, key, &ag); err == nil {
			c := apimeta.FindStatusCondition(ag.Status.Conditions, conditionTypeReady)
			if c != nil && c.Reason == wantReason {
				return &ag
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return &ag
}

// mockAccessGroupByName returns the mock's stored group with that name, or nil.
func mockAccessGroupByName(name string) *mock.AccessGroupSnapshot {
	for _, g := range mockServer.AccessGroups() {
		if g.AccessGroupName == name {
			out := g
			return &out
		}
	}
	return nil
}

// setupReadyConnectionAccessGroup delegates to setupReadyConnectionToolset:
// that helper creates only the LiteLLMConnection/default CR and polls the
// snapshot — it is entirely kind-agnostic despite its name, so a sixth
// byte-identical copy would be pure duplication.
func setupReadyConnectionAccessGroup(t *testing.T, ctx context.Context) func() {
	t.Helper()
	return setupReadyConnectionToolset(t, ctx)
}

// ── envtests ──────────────────────────────────────────────────────────────

// TestAccessGroup_CreateOnFirstReconcile asserts the CREATE arm: a fresh CR
// produces one upstream group named after the CR, and the SERVER-MINTED id
// lands in status (never derived from metadata.name).
func TestAccessGroup_CreateOnFirstReconcile(t *testing.T) {
	ctx := context.Background()
	name := "ag-create-test"
	resetMockAccessGroup()
	ensureNoAccessGroup(t, ctx, name)
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionAccessGroup(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		setConnCacheReady()
		ensureNoAccessGroup(t, context.Background(), name)
	})

	cr := accessGroupSampleCR(name, litellmv1alpha1.AccessGroupSpec{
		Models: []string{"gpt-3.5-turbo"},
	})
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create access group CR: %v", err)
	}

	got := pollAccessGroupCondition(t, ctx, name, reasonSynced)
	c := apimeta.FindStatusCondition(got.Status.Conditions, conditionTypeReady)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != reasonSynced {
		t.Fatalf("Ready condition = %+v, want True/Synced", c)
	}
	id := got.Status.LastRendered.AccessGroupID
	if id == "" {
		t.Fatal("status.lastRendered.accessGroupID is empty; it must carry the server-minted id")
	}
	if id == name {
		t.Errorf("accessGroupID = %q == metadata.name; the id is SERVER-MINTED and must "+
			"be read from the POST response, not derived from the name", id)
	}

	groups := mockServer.AccessGroups()
	if len(groups) != 1 {
		t.Fatalf("mock has %d groups, want exactly 1 (a duplicate means adoption failed)", len(groups))
	}
	if groups[0].AccessGroupName != name {
		t.Errorf("access_group_name = %q, want %q", groups[0].AccessGroupName, name)
	}
	if len(groups[0].AccessModelNames) != 1 || groups[0].AccessModelNames[0] != "gpt-3.5-turbo" {
		t.Errorf("access_model_names = %v, want [gpt-3.5-turbo]", groups[0].AccessModelNames)
	}
	// The operator must never write this face — a team-side write does not
	// propagate here, and writing it would make us a second mirror writer.
	if len(groups[0].AssignedTeamIDs) != 0 {
		t.Errorf("assigned_team_ids = %v, want [] — the operator must not write it",
			groups[0].AssignedTeamIDs)
	}
}

// TestAccessGroup_UpdateOnSpecChange asserts the UPDATE arm: adding a model
// pushes it upstream via PUT on the SAME id (no delete+recreate). Shape, not
// an exact mutation count — the reconcile loop is at-least-once.
func TestAccessGroup_UpdateOnSpecChange(t *testing.T) {
	ctx := context.Background()
	name := "ag-update-test"
	resetMockAccessGroup()
	ensureNoAccessGroup(t, ctx, name)
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionAccessGroup(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		setConnCacheReady()
		ensureNoAccessGroup(t, context.Background(), name)
	})

	cr := accessGroupSampleCR(name, litellmv1alpha1.AccessGroupSpec{
		Models: []string{"m-one"},
	})
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create access group CR: %v", err)
	}
	synced := pollAccessGroupCondition(t, ctx, name, reasonSynced)
	firstID := synced.Status.LastRendered.AccessGroupID
	if firstID == "" {
		t.Fatal("precondition: access group never reached Synced with an id")
	}

	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	var fresh litellmv1alpha1.LiteLLMAccessGroup
	if err := k8sClient.Get(ctx, key, &fresh); err != nil {
		t.Fatalf("get access group: %v", err)
	}
	fresh.Spec.Models = []string{"m-one", "m-two"}
	if err := k8sClient.Update(ctx, &fresh); err != nil {
		t.Fatalf("add model to spec: %v", err)
	}

	deadline := time.Now().Add(accessGroupPollTimeout)
	var g *mock.AccessGroupSnapshot
	for time.Now().Before(deadline) {
		g = mockAccessGroupByName(name)
		if g != nil && len(g.AccessModelNames) == 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if g == nil || len(g.AccessModelNames) != 2 {
		t.Fatalf("access_model_names = %+v, want both models after the spec change", g)
	}
	if g.AccessGroupID != firstID {
		t.Errorf("access_group_id = %q, want the original %q — an UPDATE must PUT in "+
			"place, never delete+recreate", g.AccessGroupID, firstID)
	}
	if n := len(mockServer.AccessGroups()); n != 1 {
		t.Errorf("mock has %d groups, want 1 — an UPDATE must not leave a duplicate row", n)
	}
}

// TestAccessGroup_ShrinkToEmptyClears is the regression test for the omitempty
// trap: LiteLLM's PUT reads an OMITTED list as KEEP, so shrinking a list to
// empty must send an explicit `[]`.
func TestAccessGroup_ShrinkToEmptyClears(t *testing.T) {
	ctx := context.Background()
	name := "ag-clear-test"
	resetMockAccessGroup()
	ensureNoAccessGroup(t, ctx, name)
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionAccessGroup(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		setConnCacheReady()
		ensureNoAccessGroup(t, context.Background(), name)
	})

	cr := accessGroupSampleCR(name, litellmv1alpha1.AccessGroupSpec{
		Models: []string{"m-doomed"},
	})
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create access group CR: %v", err)
	}
	pollAccessGroupCondition(t, ctx, name, reasonSynced)
	if g := mockAccessGroupByName(name); g == nil || len(g.AccessModelNames) != 1 {
		t.Fatalf("precondition: access_model_names = %+v, want [m-doomed]", g)
	}

	// Shrink to empty — the revocation case.
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	var fresh litellmv1alpha1.LiteLLMAccessGroup
	if err := k8sClient.Get(ctx, key, &fresh); err != nil {
		t.Fatalf("get access group: %v", err)
	}
	fresh.Spec.Models = nil
	if err := k8sClient.Update(ctx, &fresh); err != nil {
		t.Fatalf("clear spec.models: %v", err)
	}

	deadline := time.Now().Add(accessGroupPollTimeout)
	var g *mock.AccessGroupSnapshot
	for time.Now().Before(deadline) {
		g = mockAccessGroupByName(name)
		if g != nil && len(g.AccessModelNames) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("access_model_names = %+v, want EMPTY after clearing spec.models — an "+
		"omitted list KEEPS the stale grant in LiteLLM (silent revocation failure)", g)
}

// TestAccessGroup_ParksOnUnresolvedServer: an unresolvable spec.mcpServers name
// parks the CR rather than silently narrowing the grant, and creates NOTHING
// upstream.
func TestAccessGroup_ParksOnUnresolvedServer(t *testing.T) {
	ctx := context.Background()
	name := "ag-ghost-test"
	resetMockAccessGroup()
	ensureNoAccessGroup(t, ctx, name)
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionAccessGroup(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		setConnCacheReady()
		ensureNoAccessGroup(t, context.Background(), name)
	})

	cr := accessGroupSampleCR(name, litellmv1alpha1.AccessGroupSpec{
		MCPServers: []string{"ghost-server-nobody-registered"},
	})
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create access group CR: %v", err)
	}

	got := pollAccessGroupCondition(t, ctx, name, reasonMCPServerNotFound)
	c := apimeta.FindStatusCondition(got.Status.Conditions, conditionTypeReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != reasonMCPServerNotFound {
		t.Fatalf("Ready = %+v, want False/MCPServerNotFound", c)
	}
	if g := mockAccessGroupByName(name); g != nil {
		t.Errorf("mock has group %+v; an unresolved name must create NOTHING upstream "+
			"(a partial group is a silent authorization gap)", g)
	}
}

// TestAccessGroup_AdoptsExistingByName: access_group_name is unique
// server-side, so a duplicate CREATE answers 409. The operator adopts the
// existing group by name — that is how it re-attaches after a restart.
func TestAccessGroup_AdoptsExistingByName(t *testing.T) {
	ctx := context.Background()
	name := "ag-adopt-test"
	resetMockAccessGroup()
	ensureNoAccessGroup(t, ctx, name)
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionAccessGroup(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		setConnCacheReady()
		ensureNoAccessGroup(t, context.Background(), name)
	})

	preexistingID := mockServer.SeedAccessGroup(name)

	cr := accessGroupSampleCR(name, litellmv1alpha1.AccessGroupSpec{
		Models: []string{"m-adopted"},
	})
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create access group CR: %v", err)
	}

	got := pollAccessGroupCondition(t, ctx, name, reasonSynced)
	c := apimeta.FindStatusCondition(got.Status.Conditions, conditionTypeReady)
	if c == nil || c.Reason != reasonSynced {
		t.Fatalf("Ready = %+v, want Synced — a 409 must adopt, not park", c)
	}
	if got.Status.LastRendered.AccessGroupID != preexistingID {
		t.Errorf("accessGroupID = %q, want the adopted %q",
			got.Status.LastRendered.AccessGroupID, preexistingID)
	}
	if n := len(mockServer.AccessGroups()); n != 1 {
		t.Errorf("mock has %d groups, want 1 — adoption must not create a duplicate", n)
	}
	// The adopted group must have received our rendered state via PUT.
	g := mockAccessGroupByName(name)
	if g == nil || len(g.AccessModelNames) != 1 || g.AccessModelNames[0] != "m-adopted" {
		t.Errorf("adopted group models = %+v, want [m-adopted]", g)
	}
}

// TestAccessGroup_DeleteRemovesGroup: deleting the CR must issue
// DELETE /v1/access_group/<id> and drain the finalizer.
func TestAccessGroup_DeleteRemovesGroup(t *testing.T) {
	ctx := context.Background()
	name := "ag-delete-test"
	resetMockAccessGroup()
	ensureNoAccessGroup(t, ctx, name)
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionAccessGroup(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		setConnCacheReady()
		ensureNoAccessGroup(t, context.Background(), name)
	})

	cr := accessGroupSampleCR(name, litellmv1alpha1.AccessGroupSpec{
		Models: []string{"m-one"},
	})
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create access group CR: %v", err)
	}
	synced := pollAccessGroupCondition(t, ctx, name, reasonSynced)
	if synced.Status.LastRendered.AccessGroupID == "" {
		t.Fatal("precondition: access group never reached Synced with an id")
	}
	if mockAccessGroupByName(name) == nil {
		t.Fatal("precondition: mock has no group with that name")
	}

	if err := k8sClient.Delete(ctx, synced); err != nil {
		t.Fatalf("delete access group CR: %v", err)
	}

	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	deadline := time.Now().Add(accessGroupPollTimeout)
	gone := false
	for time.Now().Before(deadline) {
		var check litellmv1alpha1.LiteLLMAccessGroup
		if err := k8sClient.Get(ctx, key, &check); apierrors.IsNotFound(err) {
			gone = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !gone {
		t.Error("CR still present after 30s; the finalizer never drained")
	}
	if g := mockAccessGroupByName(name); g != nil {
		t.Errorf("group %+v still present in LiteLLM; DELETE /v1/access_group/<id> "+
			"was not issued", g)
	}
}

// TestAccessGroup_HealsStaleReadyFalse — issue #102: a stale Ready=False with a
// matching hash + id + generation must be HEALED by the steady-state
// short-circuit, not short-circuited past.
func TestAccessGroup_HealsStaleReadyFalse(t *testing.T) {
	ctx := context.Background()
	name := "ag-heal-test"
	resetMockAccessGroup()
	ensureNoAccessGroup(t, ctx, name)
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionAccessGroup(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		setConnCacheReady()
		ensureNoAccessGroup(t, context.Background(), name)
	})

	cr := accessGroupSampleCR(name, litellmv1alpha1.AccessGroupSpec{
		Models: []string{"m-one"},
	})
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create access group CR: %v", err)
	}
	synced := pollAccessGroupCondition(t, ctx, name, reasonSynced)
	if synced.Status.LastRendered.AccessGroupID == "" {
		t.Fatal("precondition: access group never reached Synced with an id")
	}

	// Stamp a stale Ready=False while KEEPING lastRendered + observedGeneration
	// intact — exactly what a Step 3 connection-gate write leaves behind.
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	var stale litellmv1alpha1.LiteLLMAccessGroup
	if err := k8sClient.Get(ctx, key, &stale); err != nil {
		t.Fatalf("get access group: %v", err)
	}
	apimeta.SetStatusCondition(&stale.Status.Conditions, metav1.Condition{
		Type:               conditionTypeReady,
		Status:             metav1.ConditionFalse,
		Reason:             reasonLiteLLMUnavailable,
		Message:            "LiteLLMConnection/default not Ready (reason: Connecting)",
		ObservedGeneration: stale.Generation,
	})
	if err := k8sClient.Status().Update(ctx, &stale); err != nil {
		t.Fatalf("stamp stale Ready=False: %v", err)
	}

	// Trigger a reconcile that lands in the steady-state block. Retry on
	// conflict — the reconciler writes status concurrently.
	nudged := false
	nudgeDeadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(nudgeDeadline) {
		var nudge litellmv1alpha1.LiteLLMAccessGroup
		if err := k8sClient.Get(ctx, key, &nudge); err != nil {
			t.Fatalf("get access group: %v", err)
		}
		if nudge.Annotations == nil {
			nudge.Annotations = map[string]string{}
		}
		nudge.Annotations["envtest.ackstorm.ai/heal-nudge"] = "1"
		if err := k8sClient.Update(ctx, &nudge); err == nil {
			nudged = true
			break
		} else if !apierrors.IsConflict(err) {
			t.Fatalf("annotate access group: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !nudged {
		t.Fatal("could not annotate the access group within 10s (persistent conflict)")
	}

	healed := pollAccessGroupCondition(t, ctx, name, reasonSynced)
	c := apimeta.FindStatusCondition(healed.Status.Conditions, conditionTypeReady)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != reasonSynced {
		t.Errorf("Ready = %+v, want True/Synced — the steady-state short-circuit "+
			"MUST heal a stale Ready=False (issue #102)", c)
	}
}
