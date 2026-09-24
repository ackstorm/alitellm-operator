// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

	got, missing := renderAccessGroup(spec, serverIDs, agentIDs, nil, nil)
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
	got, _ := renderAccessGroup(litellmv1alpha1.AccessGroupSpec{}, nil, nil, nil, nil)
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
	_, missing := renderAccessGroup(spec, map[string]string{"slack": "srv-1"}, nil, nil, nil)
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
		Models: []string{"m1", "m2"}}, nil, nil, nil, nil)
	b, _ := renderAccessGroup(litellmv1alpha1.AccessGroupSpec{
		Models: []string{"m2", "m1"}}, nil, nil, nil, nil)
	if accessGroupHash(a) != accessGroupHash(b) {
		t.Error("hash differs on declaration order — every reconcile would PUT")
	}
}

// TestAccessGroupHash_ModelsToModelGroupsIsNoOp pins the gitops migration:
// moving tags from models to modelGroups renders the same projection, so the
// operator sees no drift and issues no PUT.
func TestAccessGroupHash_ModelsToModelGroupsIsNoOp(t *testing.T) {
	a, _ := renderAccessGroup(litellmv1alpha1.AccessGroupSpec{Models: []string{"openai", "a2a"}}, nil, nil, nil, nil)
	b, _ := renderAccessGroup(litellmv1alpha1.AccessGroupSpec{ModelGroups: []string{"a2a", "openai"}}, nil, nil, nil, nil)
	if accessGroupHash(a) != accessGroupHash(b) {
		t.Error("moving tags to modelGroups changes the hash")
	}
}

func mcpWithTags(name, serverID, params string, deleting bool) litellmv1alpha1.LiteLLMMCPServer {
	s := litellmv1alpha1.LiteLLMMCPServer{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: WatchNamespace}}
	s.Status.LastRendered.ServerID = serverID
	if params != "" {
		s.Spec.Params = runtime.RawExtension{Raw: []byte(params)}
	}
	if deleting {
		now := metav1.Now()
		s.DeletionTimestamp = &now
	}
	return s
}

// TestTaggedMCPServerIDs pins the selection rule: mcp_access_groups wins over
// the access_groups alias, registered only, not deleting.
func TestTaggedMCPServerIDs(t *testing.T) {
	servers := []litellmv1alpha1.LiteLLMMCPServer{
		mcpWithTags("a", "s-a", `{"access_groups":["default"]}`, false),
		mcpWithTags("b", "s-b", `{"mcp_access_groups":["default","ro"]}`, false),
		mcpWithTags("c", "s-c", `{"access_groups":["other"]}`, false),
		mcpWithTags("d", "", `{"access_groups":["default"]}`, false),
		mcpWithTags("e", "s-e", `{"access_groups":["default"]}`, true),
		mcpWithTags("f", "s-f", ``, false),
		mcpWithTags("g", "s-g", `{"mcp_access_groups":["x"],"access_groups":["default"]}`, false),
	}
	if got, want := taggedMCPServerIDs(servers, []string{"default"}), []string{"s-a", "s-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("taggedMCPServerIDs = %v, want %v", got, want)
	}
	if got := taggedMCPServerIDs(servers, nil); len(got) != 0 {
		t.Fatalf("no groups must select nothing, got %v", got)
	}
}

// TestRenderAccessGroup_UnionsModelGroupsAndTaggedServers: modelGroups join
// models; tagged server ids join resolved names; sorted, deduped, never missing.
func TestRenderAccessGroup_UnionsModelGroupsAndTaggedServers(t *testing.T) {
	spec := litellmv1alpha1.AccessGroupSpec{
		Models: []string{"gpt-x", "openai"}, ModelGroups: []string{"openai", "a2a"},
		MCPServers: []string{"slack"}, MCPServerGroups: []string{"default"},
	}
	got, missing := renderAccessGroup(spec, map[string]string{"slack": "s-1"}, nil, []string{"s-2", "s-1"}, nil)
	if len(missing.MCPServers) != 0 {
		t.Fatalf("missing = %v", missing.MCPServers)
	}
	if want := []string{"a2a", "gpt-x", "openai"}; !reflect.DeepEqual(got.Models, want) {
		t.Fatalf("Models = %v, want %v", got.Models, want)
	}
	if want := []string{"s-1", "s-2"}; !reflect.DeepEqual(got.MCPServerIDs, want) {
		t.Fatalf("MCPServerIDs = %v, want %v", got.MCPServerIDs, want)
	}
}

func a2aWithTags(name, agentID string, tags string, deleting bool) litellmv1alpha1.LiteLLMA2AAgent {
	a := litellmv1alpha1.LiteLLMA2AAgent{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: WatchNamespace}}
	if tags != "" {
		a.Spec.Params = runtime.RawExtension{Raw: []byte(tags)}
	}
	a.Status.LastRendered.AgentID = agentID
	if deleting {
		now := metav1.Now()
		a.DeletionTimestamp = &now
	}
	return a
}

// TestTaggedAgentIDs pins the selection rule: tag match, registered, not deleting.
func TestTaggedAgentIDs(t *testing.T) {
	agents := []litellmv1alpha1.LiteLLMA2AAgent{
		a2aWithTags("a", "id-a", `{"access_groups":["agents"]}`, false),
		a2aWithTags("b", "id-b", `{"agent_access_groups":["agents","sec"]}`, false),
		a2aWithTags("c", "id-c", `{"access_groups":["other"]}`, false),
		a2aWithTags("d", "", `{"access_groups":["agents"]}`, false),
		a2aWithTags("e", "id-e", `{"access_groups":["agents"]}`, true),
		a2aWithTags("f", "id-f", ``, false),
		a2aWithTags("g", "id-g", `{"agent_access_groups":["x"],"access_groups":["agents"]}`, false),
	}
	got := taggedAgentIDs(agents, []string{"agents"})
	want := []string{"id-a", "id-b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("taggedAgentIDs = %v, want %v", got, want)
	}
	if got := taggedAgentIDs(agents, nil); len(got) != 0 {
		t.Fatalf("no groups must select nothing, got %v", got)
	}
}

// TestRenderAccessGroup_UnionsTaggedAgents: explicit names and tag matches
// merge, sorted and deduped; tag matches never count as missing.
func TestRenderAccessGroup_UnionsTaggedAgents(t *testing.T) {
	spec := litellmv1alpha1.AccessGroupSpec{Agents: []string{"finops"}, AgentGroups: []string{"agents"}}
	got, missing := renderAccessGroup(spec, nil, map[string]string{"finops": "id-f"}, nil, []string{"id-z", "id-f"})
	if len(missing.Agents) != 0 {
		t.Fatalf("missing = %v", missing.Agents)
	}
	if want := []string{"id-f", "id-z"}; !reflect.DeepEqual(got.AgentIDs, want) {
		t.Fatalf("AgentIDs = %v, want %v", got.AgentIDs, want)
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

// declaredMockAccessGroups is the mock's group list minus the implicit
// `default` row the always-on default runnable keeps present.
func declaredMockAccessGroups() []mock.AccessGroupSnapshot {
	var out []mock.AccessGroupSnapshot
	for _, g := range mockServer.AccessGroups() {
		if g.AccessGroupName != implicitDefaultAccessGroup {
			out = append(out, g)
		}
	}
	return out
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

	groups := declaredMockAccessGroups()
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
	if n := len(declaredMockAccessGroups()); n != 1 {
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
	if n := len(declaredMockAccessGroups()); n != 1 {
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

// TestAccessGroup_AgentGroupsFollowAgentTags: tagging an agent adds it to the
// group, untagging removes it — no edit to the group.
func TestAccessGroup_AgentGroupsFollowAgentTags(t *testing.T) {
	ctx := context.Background()
	name, agentName := "ag-tags-test", "a2a-tags-test"
	resetMockAccessGroup()
	resetMockA2A()
	ensureNoAccessGroup(t, ctx, name)
	ensureNoA2AAgent(t, ctx, agentName)
	resetConnCacheSnapshot()
	cleanupConn := setupReadyConnectionAccessGroup(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		setConnCacheReady()
		ensureNoAccessGroup(t, context.Background(), name)
		ensureNoA2AAgent(t, context.Background(), agentName)
	})

	if err := k8sClient.Create(ctx, accessGroupSampleCR(name, litellmv1alpha1.AccessGroupSpec{AgentGroups: []string{"agents"}})); err != nil {
		t.Fatalf("create group: %v", err)
	}
	pollAccessGroupCondition(t, ctx, name, reasonSynced)

	agent := a2aSampleCR(agentName)
	agent.Spec.Params = runtime.RawExtension{Raw: []byte(`{"access_groups":["agents"]}`)}
	if err := k8sClient.Create(ctx, agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	var agentID string
	waitFor(t, func() bool {
		var a litellmv1alpha1.LiteLLMA2AAgent
		if k8sClient.Get(ctx, client.ObjectKey{Name: agentName, Namespace: WatchNamespace}, &a) != nil {
			return false
		}
		agentID = a.Status.LastRendered.AgentID
		g := mockAccessGroupByName(name)
		return agentID != "" && g != nil && slices.Contains(g.AccessAgentIDs, agentID)
	}, "tagged agent never reached access_agent_ids")

	if body := mockServer.LastAgentBody(agentName); body["agent_access_groups"] == nil {
		t.Errorf("agent body lacks agent_access_groups: %v", body)
	}

	var a litellmv1alpha1.LiteLLMA2AAgent
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: agentName, Namespace: WatchNamespace}, &a); err != nil {
		t.Fatal(err)
	}
	a.Spec.Params = runtime.RawExtension{Raw: []byte(`{"access_groups":["other"]}`)}
	if err := k8sClient.Update(ctx, &a); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		g := mockAccessGroupByName(name)
		return g != nil && !slices.Contains(g.AccessAgentIDs, agentID)
	}, "untagged agent was not removed from access_agent_ids")
}

func TestAccessGroup_MCPServerGroupsFollowServerTags(t *testing.T) {
	ctx := context.Background()
	name, serverName := "ag-mcp-tags-test", "mcp-tags-test"
	resetMockAccessGroup()
	ensureNoAccessGroup(t, ctx, name)
	ensureNoMCPServer(t, ctx, serverName)
	resetConnCacheSnapshot()
	cleanupConn := setupReadyConnectionAccessGroup(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		setConnCacheReady()
		ensureNoAccessGroup(t, context.Background(), name)
		ensureNoMCPServer(t, context.Background(), serverName)
	})

	if err := k8sClient.Create(ctx, accessGroupSampleCR(name, litellmv1alpha1.AccessGroupSpec{MCPServerGroups: []string{"default"}})); err != nil {
		t.Fatalf("create group: %v", err)
	}
	pollAccessGroupCondition(t, ctx, name, reasonSynced)

	srv := mcpServerSampleCR(serverName)
	srv.Spec.Params = runtime.RawExtension{Raw: []byte(`{"mcp_info":{"description":"sample"},"access_groups":["default"]}`)}
	if err := k8sClient.Create(ctx, srv); err != nil {
		t.Fatalf("create server: %v", err)
	}
	var serverID string
	waitFor(t, func() bool {
		var s litellmv1alpha1.LiteLLMMCPServer
		if k8sClient.Get(ctx, client.ObjectKey{Name: serverName, Namespace: WatchNamespace}, &s) != nil {
			return false
		}
		serverID = s.Status.LastRendered.ServerID
		g := mockAccessGroupByName(name)
		return serverID != "" && g != nil && slices.Contains(g.AccessMCPServerIDs, serverID)
	}, "tagged server never reached access_mcp_server_ids")

	var s litellmv1alpha1.LiteLLMMCPServer
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: serverName, Namespace: WatchNamespace}, &s); err != nil {
		t.Fatal(err)
	}
	s.Spec.Params = runtime.RawExtension{Raw: []byte(`{"mcp_info":{"description":"sample"},"access_groups":["other"]}`)}
	if err := k8sClient.Update(ctx, &s); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		g := mockAccessGroupByName(name)
		return g != nil && !slices.Contains(g.AccessMCPServerIDs, serverID)
	}, "untagged server was not removed from access_mcp_server_ids")
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(accessGroupPollTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal(msg)
}

// ── implicit access group `default` ───────────────────────────────────────

// TestAccessGroup_ImplicitDefaultCreatedEmpty — with no CR, the operator keeps
// a group named `default` in LiteLLM, and it grants nothing.
func TestAccessGroup_ImplicitDefaultCreatedEmpty(t *testing.T) {
	ctx := context.Background()
	resetMockAccessGroup()
	ensureNoAccessGroup(t, ctx, implicitDefaultAccessGroup)
	resetConnCacheSnapshot()
	cleanupConn := setupReadyConnectionAccessGroup(t, ctx)
	t.Cleanup(cleanupConn)

	waitFor(t, func() bool { return mockAccessGroupByName(implicitDefaultAccessGroup) != nil },
		"implicit access group default was never created")
	g := mockAccessGroupByName(implicitDefaultAccessGroup)
	if len(g.AccessModelNames)+len(g.AccessMCPServerIDs)+len(g.AccessAgentIDs) != 0 {
		t.Errorf("implicit default must be empty, got %+v", g)
	}
}

// TestAccessGroup_ImplicitDefaultEmptiedWhenStale — grants left on `default`
// with no CR declaring it are cleared, and the row keeps its id.
func TestAccessGroup_ImplicitDefaultEmptiedWhenStale(t *testing.T) {
	ctx := context.Background()
	resetMockAccessGroup()
	ensureNoAccessGroup(t, ctx, implicitDefaultAccessGroup)
	resetConnCacheSnapshot()
	id := mockServer.SeedAccessGroup(implicitDefaultAccessGroup)
	mockServer.SetAccessGroupModels(implicitDefaultAccessGroup, []string{"stale-model"})
	cleanupConn := setupReadyConnectionAccessGroup(t, ctx)
	t.Cleanup(cleanupConn)

	waitFor(t, func() bool {
		g := mockAccessGroupByName(implicitDefaultAccessGroup)
		return g != nil && len(g.AccessModelNames) == 0
	}, "stale grant on implicit default was never cleared")
	if g := mockAccessGroupByName(implicitDefaultAccessGroup); g.AccessGroupID != id {
		t.Errorf("implicit default must keep its id %q, got %q", id, g.AccessGroupID)
	}
}

// TestAccessGroup_DefaultCRWins — a declared LiteLLMAccessGroup/default owns
// the content; the implicit ticks must not empty it.
func TestAccessGroup_DefaultCRWins(t *testing.T) {
	ctx := context.Background()
	resetMockAccessGroup()
	ensureNoAccessGroup(t, ctx, implicitDefaultAccessGroup)
	resetConnCacheSnapshot()
	cleanupConn := setupReadyConnectionAccessGroup(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		setConnCacheReady()
		ensureNoAccessGroup(t, context.Background(), implicitDefaultAccessGroup)
	})

	cr := accessGroupSampleCR(implicitDefaultAccessGroup, litellmv1alpha1.AccessGroupSpec{Models: []string{"m1"}})
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create access group CR: %v", err)
	}
	pollAccessGroupCondition(t, ctx, implicitDefaultAccessGroup, reasonSynced)
	time.Sleep(500 * time.Millisecond) // several 100ms implicit ticks
	g := mockAccessGroupByName(implicitDefaultAccessGroup)
	if g == nil || len(g.AccessModelNames) != 1 || g.AccessModelNames[0] != "m1" {
		t.Errorf("declared CR content must survive implicit ticks, got %+v", g)
	}
}

// TestAccessGroup_DefaultCRDeleteKeepsRowEmpty — deleting the `default` CR
// empties the LiteLLM row instead of deleting it, so teams keep a valid id.
func TestAccessGroup_DefaultCRDeleteKeepsRowEmpty(t *testing.T) {
	ctx := context.Background()
	resetMockAccessGroup()
	ensureNoAccessGroup(t, ctx, implicitDefaultAccessGroup)
	resetConnCacheSnapshot()
	cleanupConn := setupReadyConnectionAccessGroup(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		setConnCacheReady()
		ensureNoAccessGroup(t, context.Background(), implicitDefaultAccessGroup)
	})

	cr := accessGroupSampleCR(implicitDefaultAccessGroup, litellmv1alpha1.AccessGroupSpec{Models: []string{"m1"}})
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create access group CR: %v", err)
	}
	synced := pollAccessGroupCondition(t, ctx, implicitDefaultAccessGroup, reasonSynced)
	id := synced.Status.LastRendered.AccessGroupID
	if id == "" {
		t.Fatal("precondition: default CR never reached Synced with an id")
	}
	if err := k8sClient.Delete(ctx, synced); err != nil {
		t.Fatalf("delete access group CR: %v", err)
	}
	key := client.ObjectKey{Name: implicitDefaultAccessGroup, Namespace: WatchNamespace}
	waitFor(t, func() bool {
		var check litellmv1alpha1.LiteLLMAccessGroup
		return apierrors.IsNotFound(k8sClient.Get(ctx, key, &check))
	}, "default CR finalizer never drained")

	g := mockAccessGroupByName(implicitDefaultAccessGroup)
	if g == nil {
		t.Fatal("LiteLLM row for default was deleted; want kept and emptied")
	}
	if g.AccessGroupID != id || len(g.AccessModelNames) != 0 {
		t.Errorf("want row %q kept and empty, got %+v", id, g)
	}
}

// TestAccessGroup_DefaultCRDeleteWhileUnavailableNeverLeaksGrants — deleting
// the `default` CR while LiteLLM is unusable must not drain the finalizer with
// the old grants still live: the CR only disappears once the row is emptied.
func TestAccessGroup_DefaultCRDeleteWhileUnavailableNeverLeaksGrants(t *testing.T) {
	ctx := context.Background()
	resetMockAccessGroup()
	ensureNoAccessGroup(t, ctx, implicitDefaultAccessGroup)
	resetConnCacheSnapshot()
	cleanupConn := setupReadyConnectionAccessGroup(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		setConnCacheReady()
		ensureNoAccessGroup(t, context.Background(), implicitDefaultAccessGroup)
	})

	cr := accessGroupSampleCR(implicitDefaultAccessGroup, litellmv1alpha1.AccessGroupSpec{Models: []string{"m1"}})
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create access group CR: %v", err)
	}
	synced := pollAccessGroupCondition(t, ctx, implicitDefaultAccessGroup, reasonSynced)
	if synced.Status.LastRendered.AccessGroupID == "" {
		t.Fatal("precondition: default CR never reached Synced")
	}

	resetConnCacheSnapshot() // LiteLLM unusable
	if err := k8sClient.Delete(ctx, synced); err != nil {
		t.Fatalf("delete access group CR: %v", err)
	}
	key := client.ObjectKey{Name: implicitDefaultAccessGroup, Namespace: WatchNamespace}
	gone := func() bool {
		var check litellmv1alpha1.LiteLLMAccessGroup
		return apierrors.IsNotFound(k8sClient.Get(ctx, key, &check))
	}
	// The connection probe may restore the cache at any moment, so assert the
	// invariant rather than a timing: a gone CR implies an emptied row.
	for i := 0; i < 10; i++ {
		if gone() {
			if g := mockAccessGroupByName(implicitDefaultAccessGroup); g != nil && len(g.AccessModelNames) != 0 {
				t.Fatalf("default CR drained with grants still live: %+v", g)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	setConnCacheReady()
	waitFor(t, gone, "default CR finalizer never drained after LiteLLM recovered")
	if g := mockAccessGroupByName(implicitDefaultAccessGroup); g == nil || len(g.AccessModelNames) != 0 {
		t.Errorf("want default row kept and empty, got %+v", g)
	}
}

