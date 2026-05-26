// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

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

	// Winner reaches Ready/Synced.
	winner := pollMCPServerCondition(t, ctx, winnerName, reasonSynced, 30*time.Second)
	wc := apimeta.FindStatusCondition(winner.Status.Conditions, "Ready")
	if wc == nil || wc.Status != metav1.ConditionTrue || wc.Reason != reasonSynced {
		t.Fatalf("winner %q Ready not Synced; condition=%+v", winnerName, wc)
	}

	// Loser reaches Ready=False, Reason=Conflict, Message mentions winner.
	loser := pollMCPServerCondition(t, ctx, loserName, conflict.ConditionReasonConflict, 30*time.Second)
	lc := apimeta.FindStatusCondition(loser.Status.Conditions, "Ready")
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
	if c := apimeta.FindStatusCondition(loser.Status.Conditions, "Ready"); c == nil || c.Reason != conflict.ConditionReasonConflict {
		t.Fatalf("loser %q never reached Conflict; condition=%+v", loserName, c)
	}

	// Delete the winner. The loser must promote: Conflict condition
	// cleared, Ready=True/Synced reached.
	ensureNoMCPServer(t, ctx, winnerName)

	promoted := pollMCPServerCondition(t, ctx, loserName, reasonSynced, 30*time.Second)
	pc := apimeta.FindStatusCondition(promoted.Status.Conditions, "Ready")
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
