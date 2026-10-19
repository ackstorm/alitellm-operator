// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/controller/conflict"
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
)

// TestMCPServer_ConflictResolution_SanitizationCollapse_Loser exercises the
// alpha-last-wins conflict resolver on a sanitization-collapse collision:
// two LiteLLMMCPServer CRs in the same namespace whose metadata.name values
// sanitize to the same LiteLLM server_name. With separator=".", "foo.bar"
// and "foo-bar" both sanitize to "foo-bar". The CR whose <namespace>/<name>
// sorts LAST wins (zzz-mcp), the other (aaa-mcp) is short-circuited with
// Ready=False, Reason=Conflict, Message=~"zzz-mcp".
//
// Pre-resolver behaviour: both CRs reach Ready=True/Synced and silently
// race on the same LiteLLM server_name entry. After the resolver lands,
// aaa-mcp's Ready condition pivots to Conflict and never issues a LiteLLM
// HTTP mutation.
func TestMCPServer_ConflictResolution_SanitizationCollapse_Loser(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetMCPServers()

	// ASCII: '-' (0x2D) < '.' (0x2E). Alpha-last-wins sorts
	// <namespace>/<name> ASC and returns the LAST → "foo.bar" wins.
	// Both sanitize to "foo-bar" when separator is ".".
	const loserName = "foo-bar"  // sorts FIRST → loser
	const winnerName = "foo.bar" // sorts LAST  → winner (sanitizes to foo-bar)

	ensureNoMCPServer(t, ctx, loserName)
	ensureNoMCPServer(t, ctx, winnerName)
	resetConnCacheSnapshot()
	cleanupConn := setupReadyConnectionMCPWithSeparator(t, ctx, ".")
	t.Cleanup(func() {
		cleanupConn()
		ensureNoMCPServer(t, context.Background(), loserName)
		ensureNoMCPServer(t, context.Background(), winnerName)
	})

	// Re-Get cleanup is appended AFTER cleanupConn intentionally — the
	// connection is the gating dependency for the reconciler.

	loserCR := &litellmv1alpha1.LiteLLMMCPServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      loserName,
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.MCPServerSpec{
			Endpoint:  "https://mcp.example.com",
			Transport: "http",
			Params: runtime.RawExtension{
				Raw: []byte(`{"mcp_info":{"description":"loser"}}`),
			},
		},
	}
	winnerCR := &litellmv1alpha1.LiteLLMMCPServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      winnerName,
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.MCPServerSpec{
			Endpoint:  "https://mcp.example.com",
			Transport: "http",
			Params: runtime.RawExtension{
				Raw: []byte(`{"mcp_info":{"description":"winner"}}`),
			},
		},
	}
	// Create the winner FIRST. The informer delivers events in order, so
	// the loser's first reconcile is guaranteed to see the winner. Created
	// the other way round, the loser can reconcile alone, create foo-bar in
	// LiteLLM and keep lastRendered.serverID after it turns Conflict, which
	// is not the short-circuit this test asserts.
	if err := k8sClient.Create(ctx, winnerCR); err != nil {
		t.Fatalf("create winner MCPServer %q: %v", winnerName, err)
	}
	if err := k8sClient.Create(ctx, loserCR); err != nil {
		t.Fatalf("create loser MCPServer %q: %v", loserName, err)
	}

	// Winner reaches Ready/Synced.
	winner := pollMCPServerCondition(t, ctx, winnerName, reasonSynced, 30*time.Second)
	wc := apimeta.FindStatusCondition(winner.Status.Conditions, conditionTypeReady)
	if wc == nil || wc.Status != metav1.ConditionTrue || wc.Reason != reasonSynced {
		t.Fatalf("winner %q Ready not Synced; condition=%+v", winnerName, wc)
	}

	// Loser reaches Ready=False, Reason=Conflict, Message mentions winner.
	loser := pollMCPServerCondition(t, ctx, loserName, conflict.ConditionReasonConflict, 30*time.Second)
	lc := apimeta.FindStatusCondition(loser.Status.Conditions, conditionTypeReady)
	if lc == nil {
		t.Fatalf("loser %q has no Ready condition; conditions=%+v", loserName, loser.Status.Conditions)
	}
	if lc.Status != metav1.ConditionFalse {
		t.Errorf("loser %q Ready.Status: want False, got %v", loserName, lc.Status)
	}
	if lc.Reason != conflict.ConditionReasonConflict {
		t.Errorf("loser %q Ready.Reason: want %q, got %q", loserName,
			conflict.ConditionReasonConflict, lc.Reason)
	}
	if !strings.Contains(lc.Message, winnerName) {
		t.Errorf("loser %q Ready.Message=%q must reference winner key containing %q",
			loserName, lc.Message, winnerName)
	}

	// Loser must NOT carry a populated ServerID (resolver short-circuits
	// BEFORE any LiteLLM HTTP call).
	if loser.Status.LastRendered.ServerID != "" {
		t.Errorf("loser %q must not have lastRendered.serverID set (resolver should short-circuit before HTTP); got %q",
			loserName, loser.Status.LastRendered.ServerID)
	}

	// Winner has no Conflict condition.
	if wc.Reason == conflict.ConditionReasonConflict {
		t.Errorf("winner %q must not carry Conflict reason; condition=%+v", winnerName, wc)
	}
}

// TestMCPServer_ConflictResolution_SanitizationCollapse_LoserPromoted
// asserts the promotion path: after a winner exists, deleting the winner
// causes the loser to clear its Conflict condition and reconcile through
// to Ready=True/Synced.
func TestMCPServer_ConflictResolution_SanitizationCollapse_LoserPromoted(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetMCPServers()

	// Same ASCII ordering as the first scenario: foo-bar sorts FIRST
	// (loser), foo.bar sorts LAST (winner). Both sanitize to "foo-bar"
	// under separator=".".
	const loserName = "foo-bar"
	const winnerName = "foo.bar"

	ensureNoMCPServer(t, ctx, loserName)
	ensureNoMCPServer(t, ctx, winnerName)
	resetConnCacheSnapshot()
	cleanupConn := setupReadyConnectionMCPWithSeparator(t, ctx, ".")
	t.Cleanup(func() {
		cleanupConn()
		ensureNoMCPServer(t, context.Background(), loserName)
		ensureNoMCPServer(t, context.Background(), winnerName)
	})

	loserCR := &litellmv1alpha1.LiteLLMMCPServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      loserName,
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.MCPServerSpec{
			Endpoint:  "https://mcp.example.com",
			Transport: "http",
			Params: runtime.RawExtension{
				Raw: []byte(`{"mcp_info":{"description":"loser"}}`),
			},
		},
	}
	if err := k8sClient.Create(ctx, loserCR); err != nil {
		t.Fatalf("create loser MCPServer %q: %v", loserName, err)
	}
	winnerCR := &litellmv1alpha1.LiteLLMMCPServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      winnerName,
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.MCPServerSpec{
			Endpoint:  "https://mcp.example.com",
			Transport: "http",
			Params: runtime.RawExtension{
				Raw: []byte(`{"mcp_info":{"description":"winner"}}`),
			},
		},
	}
	if err := k8sClient.Create(ctx, winnerCR); err != nil {
		t.Fatalf("create winner MCPServer %q: %v", winnerName, err)
	}

	// Wait for the loser to land on Conflict.
	loser := pollMCPServerCondition(t, ctx, loserName, conflict.ConditionReasonConflict, 30*time.Second)
	if c := apimeta.FindStatusCondition(loser.Status.Conditions, conditionTypeReady); c == nil || c.Reason != conflict.ConditionReasonConflict {
		t.Fatalf("loser %q never reached Conflict; condition=%+v", loserName, c)
	}

	// Delete the winner. The loser must promote: Conflict condition
	// cleared, Ready=True/Synced reached.
	ensureNoMCPServer(t, ctx, winnerName)

	promoted := pollMCPServerCondition(t, ctx, loserName, reasonSynced, 30*time.Second)
	pc := apimeta.FindStatusCondition(promoted.Status.Conditions, conditionTypeReady)
	if pc == nil {
		t.Fatalf("promoted loser %q has no Ready condition; conditions=%+v", loserName, promoted.Status.Conditions)
	}
	if pc.Status != metav1.ConditionTrue || pc.Reason != reasonSynced {
		t.Errorf("promoted loser %q Ready: want True/%s, got Status=%v Reason=%q",
			loserName, reasonSynced, pc.Status, pc.Reason)
	}
	if pc.Reason == conflict.ConditionReasonConflict {
		t.Errorf("promoted loser %q still carries Conflict reason after winner deletion", loserName)
	}
}

// newCollapseMCPServer returns a minimal LiteLLMMCPServer for the
// sanitization-collapse tests (foo-bar / foo.bar under separator ".").
func newCollapseMCPServer(name string) *litellmv1alpha1.LiteLLMMCPServer {
	return &litellmv1alpha1.LiteLLMMCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: WatchNamespace},
		Spec: litellmv1alpha1.MCPServerSpec{
			Endpoint:  "https://mcp.example.com",
			Transport: "http",
			Params:    runtime.RawExtension{Raw: []byte(`{"mcp_info":{"description":"` + name + `"}}`)},
		},
	}
}

// deleteMCPServerAndWaitGone deletes the CR WITHOUT stripping its finalizer
// (unlike ensureNoMCPServer), so the reconciler's deletion path runs, and
// waits until the finalizer has drained.
func deleteMCPServerAndWaitGone(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	if err := k8sClient.Delete(ctx, newCollapseMCPServer(name)); err != nil {
		t.Fatalf("delete MCPServer %q: %v", name, err)
	}
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var m litellmv1alpha1.LiteLLMMCPServer
		if err := k8sClient.Get(ctx, key, &m); apierrors.IsNotFound(err) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("MCPServer %q not gone within 30s (finalizer never drained)", name)
}

// mcpDeletesOf counts DELETE /v1/mcp/server/<serverID> calls the mock saw.
func mcpDeletesOf(serverID string) int {
	n := 0
	for _, c := range mockServer.Recorded() {
		if c.Method == http.MethodDelete && c.Path == "/v1/mcp/server/"+serverID {
			n++
		}
	}
	return n
}

// TestMCPServer_ConflictLoserDelete_LateWinner_KeepsWinnerEntry: the loser
// reconciles before the winner exists, so it CREATEs server_id=foo-bar and
// keeps lastRendered.serverID after turning Conflict. Deleting that loser
// must NOT DELETE foo-bar — the live winner owns it.
func TestMCPServer_ConflictLoserDelete_LateWinner_KeepsWinnerEntry(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetMCPServers()

	const loserName, winnerName, serverID = "foo-bar", "foo.bar", "foo-bar"
	ensureNoMCPServer(t, ctx, loserName)
	ensureNoMCPServer(t, ctx, winnerName)
	resetConnCacheSnapshot()
	cleanupConn := setupReadyConnectionMCPWithSeparator(t, ctx, ".")
	t.Cleanup(func() {
		cleanupConn()
		ensureNoMCPServer(t, context.Background(), loserName)
		ensureNoMCPServer(t, context.Background(), winnerName)
	})

	if err := k8sClient.Create(ctx, newCollapseMCPServer(loserName)); err != nil {
		t.Fatalf("create loser: %v", err)
	}
	l := pollMCPServerCondition(t, ctx, loserName, reasonSynced, 30*time.Second)
	if l.Status.LastRendered.ServerID != serverID || !mockServer.HasMCPServer(serverID) {
		t.Fatalf("loser did not create %q first; serverID=%q", serverID, l.Status.LastRendered.ServerID)
	}

	if err := k8sClient.Create(ctx, newCollapseMCPServer(winnerName)); err != nil {
		t.Fatalf("create winner: %v", err)
	}
	pollMCPServerCondition(t, ctx, loserName, conflict.ConditionReasonConflict, 30*time.Second)
	if w := pollMCPServerCondition(t, ctx, winnerName, reasonSynced, 30*time.Second); w.Status.LastRendered.ServerID != serverID {
		t.Fatalf("winner not Synced on %q; status=%+v", serverID, w.Status)
	}

	mockServer.ResetRecorded()
	deleteMCPServerAndWaitGone(t, ctx, loserName)

	if n := mcpDeletesOf(serverID); n != 0 {
		t.Errorf("deleting the conflict loser issued %d DELETE /v1/mcp/server/%s — that is the winner's entry", n, serverID)
	}
	if !mockServer.HasMCPServer(serverID) {
		t.Errorf("winner's LiteLLM entry %q is gone after deleting the loser", serverID)
	}
	w := pollMCPServerCondition(t, ctx, winnerName, reasonSynced, 5*time.Second)
	if c := apimeta.FindStatusCondition(w.Status.Conditions, conditionTypeReady); c == nil || c.Reason != reasonSynced {
		t.Errorf("winner not Ready/Synced after loser delete; condition=%+v", c)
	}
}

// TestMCPServer_ConflictWinnerDelete_LoserInherits_LastOwnerDeletes: deleting
// the winner while a live loser shares the server_name must leave the entry
// for the promoted loser; the DELETE happens only when that last owning CR
// goes away.
func TestMCPServer_ConflictWinnerDelete_LoserInherits_LastOwnerDeletes(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetMCPServers()

	const loserName, winnerName, serverID = "foo-bar", "foo.bar", "foo-bar"
	ensureNoMCPServer(t, ctx, loserName)
	ensureNoMCPServer(t, ctx, winnerName)
	resetConnCacheSnapshot()
	cleanupConn := setupReadyConnectionMCPWithSeparator(t, ctx, ".")
	t.Cleanup(func() {
		cleanupConn()
		ensureNoMCPServer(t, context.Background(), loserName)
		ensureNoMCPServer(t, context.Background(), winnerName)
	})

	if err := k8sClient.Create(ctx, newCollapseMCPServer(winnerName)); err != nil {
		t.Fatalf("create winner: %v", err)
	}
	if err := k8sClient.Create(ctx, newCollapseMCPServer(loserName)); err != nil {
		t.Fatalf("create loser: %v", err)
	}
	pollMCPServerCondition(t, ctx, winnerName, reasonSynced, 30*time.Second)
	pollMCPServerCondition(t, ctx, loserName, conflict.ConditionReasonConflict, 30*time.Second)

	mockServer.ResetRecorded()
	deleteMCPServerAndWaitGone(t, ctx, winnerName)
	if n := mcpDeletesOf(serverID); n != 0 {
		t.Errorf("deleting the winner while loser %q is live issued %d DELETE /v1/mcp/server/%s", loserName, n, serverID)
	}
	pollMCPServerCondition(t, ctx, loserName, reasonSynced, 30*time.Second)
	if !mockServer.HasMCPServer(serverID) {
		t.Fatalf("entry %q missing after the loser was promoted", serverID)
	}

	deleteMCPServerAndWaitGone(t, ctx, loserName)
	if n := mcpDeletesOf(serverID); n == 0 {
		t.Errorf("deleting the last owning CR issued no DELETE /v1/mcp/server/%s", serverID)
	}
	if mockServer.HasMCPServer(serverID) {
		t.Errorf("entry %q still present after its last owning CR was deleted", serverID)
	}
}
