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
	"github.com/ackstorm/alitellm-operator/internal/connection"
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
)

// ── helpers ───────────────────────────────────────────────────────────────

// toolsetRawServerUUID is a raw LiteLLM server_id — the shape a user supplies
// for an adopted MCP server that has no LiteLLMMCPServer CR. It must reach
// LiteLLM verbatim (no sanitization, no drop).
const toolsetRawServerUUID = "6d071d99-39d2-44f9-8182-8917827b7c45"

func toolsetSampleCR(name string, from ...litellmv1alpha1.MCPToolsetServerTools) *litellmv1alpha1.LiteLLMMCPToolset {
	return &litellmv1alpha1.LiteLLMMCPToolset{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: WatchNamespace},
		Spec: litellmv1alpha1.MCPToolsetSpec{
			Description: "envtest toolset",
			From:        from,
		},
	}
}

// ensureNoToolset deletes any pre-existing toolset CR with the given name and
// waits for full removal (finalizer drain included).
func ensureNoToolset(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	var existing litellmv1alpha1.LiteLLMMCPToolset
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	if err := k8sClient.Get(ctx, key, &existing); err == nil {
		controllerutil.RemoveFinalizer(&existing, mcpToolsetFinalizer)
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
	t.Logf("warning: LiteLLMMCPToolset %q still present after 10s cleanup wait", name)
}

// toolsetPollTimeout bounds every Ready-condition poll in this file.
const toolsetPollTimeout = 30 * time.Second

// pollToolsetCondition polls the Ready condition until reason matches or
// toolsetPollTimeout elapses. Returns the final re-Get'd CR either way.
func pollToolsetCondition(t *testing.T, ctx context.Context, name, wantReason string) *litellmv1alpha1.LiteLLMMCPToolset {
	t.Helper()
	deadline := time.Now().Add(toolsetPollTimeout)
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	var ts litellmv1alpha1.LiteLLMMCPToolset
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, key, &ts); err == nil {
			c := apimeta.FindStatusCondition(ts.Status.Conditions, conditionTypeReady)
			if c != nil && c.Reason == wantReason {
				return &ts
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return &ts
}

func resetMockToolset() {
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetToolsets()
}

// setupReadyConnectionToolset mirrors setupReadyConnectionA2A.
func setupReadyConnectionToolset(t *testing.T, ctx context.Context) func() {
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

// toolPairs extracts the {server_id, tool_name} pairs from a captured body.
func toolPairs(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	if body == nil {
		return nil
	}
	raw, present := body["tools"]
	if !present {
		t.Fatalf("body has NO `tools` key; ALWAYS-EMIT requires it to be present: %v", body)
	}
	arr, ok := raw.([]any)
	if !ok {
		t.Fatalf("`tools` is %T, want a JSON array: %v", raw, raw)
	}
	out := make([]map[string]any, 0, len(arr))
	for _, e := range arr {
		m, _ := e.(map[string]any)
		out = append(out, m)
	}
	return out
}

// ── tests ─────────────────────────────────────────────────────────────────

// The toolset_id is MINTED by LiteLLM — it must come from the POST response,
// never from metadata.name (unlike team_id / MCP server_id).
func TestMCPToolset_CreateOnFirstReconcile(t *testing.T) {
	ctx := context.Background()
	name := "ts-create-test"
	resetMockToolset()
	ensureNoToolset(t, ctx, name)
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionToolset(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		setConnCacheReady()
		ensureNoToolset(t, context.Background(), name)
	})

	cr := toolsetSampleCR(name, litellmv1alpha1.MCPToolsetServerTools{
		Server: "some-server", Tools: []string{"web_search", "fetch_page"},
	})
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create toolset CR: %v", err)
	}

	got := pollToolsetCondition(t, ctx, name, reasonSynced)
	c := apimeta.FindStatusCondition(got.Status.Conditions, conditionTypeReady)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != reasonSynced {
		t.Fatalf("Ready condition = %+v, want True/Synced", c)
	}
	id := got.Status.LastRendered.ToolsetID
	if id == "" {
		t.Fatal("status.lastRendered.toolsetID is empty; it must carry the server-minted id")
	}
	if id == name {
		t.Errorf("toolsetID = %q == metadata.name; the id is SERVER-MINTED and must "+
			"be read from the POST response, not derived from the name", id)
	}
	if mockID := mockServer.GetToolsetID(name); mockID != id {
		t.Errorf("toolsetID = %q, want the mock-minted %q", id, mockID)
	}
	// Mutation SHAPE, not an exact count — the reconcile loop is at-least-once.
	if n := mockServer.MutationsByToolsetName(name); n < 1 {
		t.Errorf("toolset mutations = %d, want >= 1", n)
	}
	pairs := toolPairs(t, mockServer.LastToolsetBody(name))
	if len(pairs) != 2 || pairs[0]["tool_name"] != "web_search" || pairs[1]["tool_name"] != "fetch_page" {
		t.Errorf("tools = %v, want [web_search fetch_page] in declaration order", pairs)
	}
}

// A spec.from[].server naming a LiteLLMMCPServer CR resolves to that CR's
// status.lastRendered.serverID.
func TestMCPToolset_ServerNameResolvedFromCR(t *testing.T) {
	ctx := context.Background()
	name := "ts-resolve-test"
	srvName := "ts-resolve-backing-server"
	resetMockToolset()
	ensureNoToolset(t, ctx, name)
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionToolset(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		setConnCacheReady()
		ensureNoToolset(t, context.Background(), name)
	})

	// Seed an out-of-band MCP server FIRST so the MCPServer reconciler takes
	// its ADOPTION path (resolveServerIDByName → UPDATE arm), which keeps the
	// server-minted UUID. That is the only way status.lastRendered.serverID
	// differs from metadata.name — the CREATE arm pins server_id to the
	// sanitized name, which would make this assertion vacuous.
	mintedID := mockServer.AddHandManagedMCPServer(srvName, "https://mcp.example.com/mcp", "http")
	if mintedID == srvName {
		t.Fatalf("precondition: mock minted %q == the CR name; the test cannot "+
			"distinguish a resolved id from a verbatim fallback", mintedID)
	}

	srv := &litellmv1alpha1.LiteLLMMCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: srvName, Namespace: WatchNamespace},
		Spec: litellmv1alpha1.MCPServerSpec{
			Endpoint:  "https://mcp.example.com/mcp",
			Transport: "http",
		},
	}
	if err := k8sClient.Create(ctx, srv); err != nil {
		t.Fatalf("create backing MCPServer CR: %v", err)
	}
	t.Cleanup(func() {
		var s litellmv1alpha1.LiteLLMMCPServer
		bg := context.Background()
		if err := k8sClient.Get(bg, client.ObjectKeyFromObject(srv), &s); err == nil {
			controllerutil.RemoveFinalizer(&s, mcpServerFinalizer)
			_ = k8sClient.Update(bg, &s)
			_ = k8sClient.Delete(bg, &s)
		}
	})

	// Wait for the MCPServer reconciler to adopt and publish the minted id.
	srvKey := client.ObjectKeyFromObject(srv)
	adoptDeadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(adoptDeadline) {
		var s litellmv1alpha1.LiteLLMMCPServer
		if err := k8sClient.Get(ctx, srvKey, &s); err == nil &&
			s.Status.LastRendered.ServerID == mintedID {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	var backing litellmv1alpha1.LiteLLMMCPServer
	if err := k8sClient.Get(ctx, srvKey, &backing); err != nil {
		t.Fatalf("get backing MCPServer: %v", err)
	}
	if backing.Status.LastRendered.ServerID != mintedID {
		t.Fatalf("precondition: backing MCPServer serverID = %q, want the adopted %q",
			backing.Status.LastRendered.ServerID, mintedID)
	}

	cr := toolsetSampleCR(name, litellmv1alpha1.MCPToolsetServerTools{
		Server: srvName, Tools: []string{"a_tool"},
	})
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create toolset CR: %v", err)
	}

	var pairs []map[string]any
	resolveDeadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(resolveDeadline) {
		pairs = toolPairs(t, mockServer.LastToolsetBody(name))
		if len(pairs) == 1 && pairs[0]["server_id"] == mintedID {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(pairs) != 1 || pairs[0]["server_id"] != mintedID {
		t.Errorf("tools = %v, want server_id resolved to the CR's status serverID %q "+
			"(got the verbatim name instead if it reads %q)", pairs, mintedID, srvName)
	}
	got := pollToolsetCondition(t, ctx, name, reasonSynced)
	if c := apimeta.FindStatusCondition(got.Status.Conditions, conditionTypeReady); c == nil || c.Reason != reasonSynced {
		t.Errorf("Ready = %+v, want Synced", c)
	}
}

// An unresolvable server name is forwarded VERBATIM and the CR is never
// parked — this is the raw-UUID / adopted-server path.
func TestMCPToolset_UnknownServerPassesThroughVerbatim(t *testing.T) {
	ctx := context.Background()
	name := "ts-verbatim-test"
	resetMockToolset()
	ensureNoToolset(t, ctx, name)
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionToolset(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		setConnCacheReady()
		ensureNoToolset(t, context.Background(), name)
	})

	cr := toolsetSampleCR(name, litellmv1alpha1.MCPToolsetServerTools{
		Server: toolsetRawServerUUID, Tools: []string{"raw_tool"},
	})
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create toolset CR: %v", err)
	}

	got := pollToolsetCondition(t, ctx, name, reasonSynced)
	if c := apimeta.FindStatusCondition(got.Status.Conditions, conditionTypeReady); c == nil || c.Reason != reasonSynced {
		t.Fatalf("Ready = %+v, want Synced — an unresolvable server must NEVER park the CR", c)
	}
	pairs := toolPairs(t, mockServer.LastToolsetBody(name))
	if len(pairs) != 1 || pairs[0]["server_id"] != toolsetRawServerUUID {
		t.Errorf("tools = %v, want the verbatim UUID %q (no sanitization)", pairs, toolsetRawServerUUID)
	}
}

// LiteLLM accepts a bogus tool name with 201 and grants nothing. The operator
// forwards it and reports Synced — no validation anywhere.
func TestMCPToolset_BogusToolNameAccepted(t *testing.T) {
	ctx := context.Background()
	name := "ts-bogus-tool-test"
	resetMockToolset()
	ensureNoToolset(t, ctx, name)
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionToolset(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		setConnCacheReady()
		ensureNoToolset(t, context.Background(), name)
	})

	cr := toolsetSampleCR(name, litellmv1alpha1.MCPToolsetServerTools{
		Server: "nonexistent-server", Tools: []string{"no_such_tool_anywhere"},
	})
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create toolset CR: %v", err)
	}

	got := pollToolsetCondition(t, ctx, name, reasonSynced)
	if c := apimeta.FindStatusCondition(got.Status.Conditions, conditionTypeReady); c == nil || c.Reason != reasonSynced {
		t.Fatalf("Ready = %+v, want Synced — bogus refs are inert, not fatal", c)
	}
	pairs := toolPairs(t, mockServer.LastToolsetBody(name))
	if len(pairs) != 1 || pairs[0]["tool_name"] != "no_such_tool_anywhere" {
		t.Errorf("tools = %v, want the bogus name forwarded verbatim", pairs)
	}
}

// A second reconcile with an unchanged spec must issue no further mutation.
func TestMCPToolset_SteadyStateNoMutation(t *testing.T) {
	ctx := context.Background()
	name := "ts-steady-test"
	resetMockToolset()
	ensureNoToolset(t, ctx, name)
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionToolset(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		setConnCacheReady()
		ensureNoToolset(t, context.Background(), name)
	})

	cr := toolsetSampleCR(name, litellmv1alpha1.MCPToolsetServerTools{
		Server: "s1", Tools: []string{"t1"},
	})
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create toolset CR: %v", err)
	}
	pollToolsetCondition(t, ctx, name, reasonSynced)

	settled := mockServer.MutationsByToolsetName(name)
	// Nudge the reconciler with a no-op metadata change; the rendered hash is
	// unchanged, so the steady-state short-circuit must hold. Retry on
	// conflict — the reconciler writes status concurrently.
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	nudged := false
	nudgeDeadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(nudgeDeadline) {
		var fresh litellmv1alpha1.LiteLLMMCPToolset
		if err := k8sClient.Get(ctx, key, &fresh); err != nil {
			t.Fatalf("get toolset: %v", err)
		}
		if fresh.Annotations == nil {
			fresh.Annotations = map[string]string{}
		}
		fresh.Annotations["envtest.ackstorm.ai/nudge"] = "1"
		if err := k8sClient.Update(ctx, &fresh); err == nil {
			nudged = true
			break
		} else if !apierrors.IsConflict(err) {
			t.Fatalf("annotate toolset: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !nudged {
		t.Fatal("could not annotate the toolset within 10s (persistent conflict)")
	}
	time.Sleep(2 * time.Second)

	if after := mockServer.MutationsByToolsetName(name); after != settled {
		t.Errorf("mutations went %d → %d across a no-op update; steady state must not mutate",
			settled, after)
	}
}

// Issue #102: a stale Ready=False with a matching hash + id + generation must
// be HEALED by the steady-state short-circuit, not short-circuited past.
func TestMCPToolset_SteadyStateHealsStaleReadyFalse(t *testing.T) {
	ctx := context.Background()
	name := "ts-heal-test"
	resetMockToolset()
	ensureNoToolset(t, ctx, name)
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionToolset(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		setConnCacheReady()
		ensureNoToolset(t, context.Background(), name)
	})

	cr := toolsetSampleCR(name, litellmv1alpha1.MCPToolsetServerTools{
		Server: "s1", Tools: []string{"t1"},
	})
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create toolset CR: %v", err)
	}
	synced := pollToolsetCondition(t, ctx, name, reasonSynced)
	if synced.Status.LastRendered.ToolsetID == "" {
		t.Fatal("precondition: toolset never reached Synced with an id")
	}

	// Stamp a stale Ready=False while KEEPING lastRendered + observedGeneration
	// intact — exactly what a Step 3 connection-gate write leaves behind.
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	var stale litellmv1alpha1.LiteLLMMCPToolset
	if err := k8sClient.Get(ctx, key, &stale); err != nil {
		t.Fatalf("get toolset: %v", err)
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
		var nudge litellmv1alpha1.LiteLLMMCPToolset
		if err := k8sClient.Get(ctx, key, &nudge); err != nil {
			t.Fatalf("get toolset: %v", err)
		}
		if nudge.Annotations == nil {
			nudge.Annotations = map[string]string{}
		}
		nudge.Annotations["envtest.ackstorm.ai/heal-nudge"] = "1"
		if err := k8sClient.Update(ctx, &nudge); err == nil {
			nudged = true
			break
		} else if !apierrors.IsConflict(err) {
			t.Fatalf("annotate toolset: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !nudged {
		t.Fatal("could not annotate the toolset within 10s (persistent conflict)")
	}

	healed := pollToolsetCondition(t, ctx, name, reasonSynced)
	c := apimeta.FindStatusCondition(healed.Status.Conditions, conditionTypeReady)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != reasonSynced {
		t.Errorf("Ready = %+v, want True/Synced — the steady-state short-circuit "+
			"MUST heal a stale Ready=False (issue #102)", c)
	}
}

// ALWAYS-EMIT: shrinking spec.from to empty must send `tools: []` — a present
// empty array, an explicit clear. An omitted field would keep the stale list.
func TestMCPToolset_EmptyFromClearsTools(t *testing.T) {
	ctx := context.Background()
	name := "ts-clear-test"
	resetMockToolset()
	ensureNoToolset(t, ctx, name)
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionToolset(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		setConnCacheReady()
		ensureNoToolset(t, context.Background(), name)
	})

	cr := toolsetSampleCR(name, litellmv1alpha1.MCPToolsetServerTools{
		Server: "s1", Tools: []string{"t1", "t2"},
	})
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create toolset CR: %v", err)
	}
	pollToolsetCondition(t, ctx, name, reasonSynced)
	if got := toolPairs(t, mockServer.LastToolsetBody(name)); len(got) != 2 {
		t.Fatalf("precondition: tools = %v, want 2 pairs", got)
	}

	// Shrink to empty — the revocation case.
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	var fresh litellmv1alpha1.LiteLLMMCPToolset
	if err := k8sClient.Get(ctx, key, &fresh); err != nil {
		t.Fatalf("get toolset: %v", err)
	}
	fresh.Spec.From = nil
	if err := k8sClient.Update(ctx, &fresh); err != nil {
		t.Fatalf("clear spec.from: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	var pairs []map[string]any
	for time.Now().Before(deadline) {
		pairs = toolPairs(t, mockServer.LastToolsetBody(name))
		if len(pairs) == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(pairs) != 0 {
		t.Errorf("tools = %v, want an EMPTY array after clearing spec.from", pairs)
	}
	// toolPairs already fatals when the key is absent; assert it explicitly
	// too, because "absent" is the exact revocation-leak failure mode.
	body := mockServer.LastToolsetBody(name)
	if _, present := body["tools"]; !present {
		t.Errorf("`tools` key ABSENT from the PUT body; an omitted field keeps the "+
			"stale tool list in LiteLLM. Body: %v", body)
	}
}

// toolset_name is unique server-side. A 409 on CREATE means the toolset
// already exists (restart / out-of-band create) → adopt it by name.
func TestMCPToolset_AdoptsOn409(t *testing.T) {
	ctx := context.Background()
	name := "ts-adopt-test"
	resetMockToolset()
	ensureNoToolset(t, ctx, name)
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionToolset(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		setConnCacheReady()
		ensureNoToolset(t, context.Background(), name)
	})

	// Pre-existing toolset in LiteLLM with the CR's name.
	preexistingID := mockServer.SeedToolset(name)

	cr := toolsetSampleCR(name, litellmv1alpha1.MCPToolsetServerTools{
		Server: "s1", Tools: []string{"t1"},
	})
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create toolset CR: %v", err)
	}

	got := pollToolsetCondition(t, ctx, name, reasonSynced)
	c := apimeta.FindStatusCondition(got.Status.Conditions, conditionTypeReady)
	if c == nil || c.Reason != reasonSynced {
		t.Fatalf("Ready = %+v, want Synced — a 409 must adopt, not park", c)
	}
	if got.Status.LastRendered.ToolsetID != preexistingID {
		t.Errorf("toolsetID = %q, want the adopted %q", got.Status.LastRendered.ToolsetID, preexistingID)
	}
	// The adopted toolset must have received our rendered state via PUT.
	pairs := toolPairs(t, mockServer.LastToolsetBody(name))
	if len(pairs) != 1 || pairs[0]["tool_name"] != "t1" {
		t.Errorf("adopted toolset tools = %v, want the CR's rendered [s1/t1]", pairs)
	}
}

// An out-of-band delete must be detected by the vanish probe and recreated.
func TestMCPToolset_VanishProbeRecreates(t *testing.T) {
	ctx := context.Background()
	name := "ts-vanish-test"
	resetMockToolset()
	ensureNoToolset(t, ctx, name)
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionToolset(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		setConnCacheReady()
		ensureNoToolset(t, context.Background(), name)
	})

	cr := toolsetSampleCR(name, litellmv1alpha1.MCPToolsetServerTools{
		Server: "s1", Tools: []string{"t1"},
	})
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create toolset CR: %v", err)
	}
	synced := pollToolsetCondition(t, ctx, name, reasonSynced)
	firstID := synced.Status.LastRendered.ToolsetID
	if firstID == "" {
		t.Fatal("precondition: toolset never reached Synced with an id")
	}

	// Background relist is OFF by default in this suite (#74); this test is
	// one of the few that genuinely needs the drift tick.
	enableSuiteRelist(t)
	mockServer.DeleteToolsetOutOfBand(firstID)

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if mockServer.HasToolset(name) && mockServer.GetToolsetID(name) != firstID {
			return // recreated under a fresh id
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("toolset %q was not recreated after an out-of-band delete (first id %q)", name, firstID)
}

// Deleting the CR must issue DELETE /v1/mcp/toolset/<id> and drain the
// finalizer.
func TestMCPToolset_FinalizerDeletes(t *testing.T) {
	ctx := context.Background()
	name := "ts-finalizer-test"
	resetMockToolset()
	ensureNoToolset(t, ctx, name)
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionToolset(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		setConnCacheReady()
		ensureNoToolset(t, context.Background(), name)
	})

	cr := toolsetSampleCR(name, litellmv1alpha1.MCPToolsetServerTools{
		Server: "s1", Tools: []string{"t1"},
	})
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create toolset CR: %v", err)
	}
	synced := pollToolsetCondition(t, ctx, name, reasonSynced)
	if synced.Status.LastRendered.ToolsetID == "" {
		t.Fatal("precondition: toolset never reached Synced with an id")
	}
	if !mockServer.HasToolset(name) {
		t.Fatal("precondition: mock has no toolset with that name")
	}

	if err := k8sClient.Delete(ctx, synced); err != nil {
		t.Fatalf("delete toolset CR: %v", err)
	}

	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	deadline := time.Now().Add(30 * time.Second)
	gone := false
	for time.Now().Before(deadline) {
		var check litellmv1alpha1.LiteLLMMCPToolset
		if err := k8sClient.Get(ctx, key, &check); apierrors.IsNotFound(err) {
			gone = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !gone {
		t.Error("CR still present after 30s; the finalizer never drained")
	}
	if mockServer.HasToolset(name) {
		t.Error("toolset still present in LiteLLM; DELETE /v1/mcp/toolset/<id> was not issued")
	}
}

// Issue #74: a Ready snapshot with a nil Client must take the not-Ready path,
// never nil-deref. Cache.Rebuild does not enforce Ready ⇒ Client != nil.
func TestMCPToolset_NotUsableSnapshotParks(t *testing.T) {
	ctx := context.Background()
	name := "ts-notusable-test"
	resetMockToolset()
	ensureNoToolset(t, ctx, name)

	t.Cleanup(func() {
		setConnCacheReady()
		ensureNoToolset(t, context.Background(), name)
	})

	// Ready=true but Client=nil — the poisoned-singleton shape from #74.
	connCache.Rebuild(connection.ConnectionSnapshot{
		Ready:  true,
		Reason: reasonSynced,
	})

	cr := toolsetSampleCR(name, litellmv1alpha1.MCPToolsetServerTools{
		Server: "s1", Tools: []string{"t1"},
	})
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create toolset CR: %v", err)
	}

	got := pollToolsetCondition(t, ctx, name, reasonLiteLLMUnavailable)
	c := apimeta.FindStatusCondition(got.Status.Conditions, conditionTypeReady)
	if c == nil || c.Reason != reasonLiteLLMUnavailable {
		t.Errorf("Ready = %+v, want LiteLLMUnavailable — a Ready+nil-Client snapshot "+
			"must take the not-Usable path (issue #74), not panic", c)
	}
	if mockServer.HasToolset(name) {
		t.Error("toolset was created despite an unusable connection snapshot")
	}
}
