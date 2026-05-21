// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/connection"
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
)

// waitForMockTeamDefault polls mockServer.HasTeam("default") up to timeout.
// Returns true if the team was observed, false otherwise.
func waitForMockTeamDefault(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if mockServer.HasTeam("default") {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return mockServer.HasTeam("default")
}

// ──────────────────────────────────────────────────────────────────────────
// Test 1: BootstrapAfterConnectionReady
// ──────────────────────────────────────────────────────────────────────────

// TestTeamDefaultRunnable_BootstrapAfterConnectionReady — // behavior #1 + AC-T2 first half. On manager start with the connection
// reaching Ready, the runnable enqueues a synthetic Team/default reconcile.
// The reconciler's NotFound-on-default branch fires reconcileImplicitDefault
// which posts to POST /team/new with the implicit empty body. The K8s API
// server NEVER sees a Team/default CR (per spec §7.4 line 1313).
func TestTeamDefaultRunnable_BootstrapAfterConnectionReady(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache() // Phase 6 cross-suite flake fix (07-CONTEXT.md §Phase-6-flake option α)
	resetConnCacheSnapshot()

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(cleanupConn)

	if !waitForMockTeamDefault(15 * time.Second) {
		t.Fatalf("synthetic bootstrap did not create LiteLLM team aliased `default` within 15s")
	}

	// Verify the body's structural shape: empty implicit body — team_alias
	// bare, max_budget=null, budget_duration=null.
	body := mockServer.LastTeamBody("default")
	if body == nil {
		t.Fatalf("LastTeamBody is nil for `default` — mock did not capture POST /team/new body")
	}
	if alias, _ := body["team_alias"].(string); alias != "default" {
		t.Errorf("body.team_alias: want %q, got %v", "default", body["team_alias"])
	}
	if v, ok := body["max_budget"]; !ok {
		t.Errorf("body.max_budget MISSING — synthetic body must always include max_budget key")
	} else if v != nil {
		t.Errorf("body.max_budget: want nil (JSON null), got %v (type %T)", v, v)
	}
	if v, ok := body["budget_duration"]; !ok {
		t.Errorf("body.budget_duration MISSING — synthetic body must always include budget_duration key")
	} else if v != nil {
		t.Errorf("body.budget_duration: want nil (JSON null), got %v (type %T)", v, v)
	}

	// Spec §7.4 line 1313: the operator MUST NOT create a Kubernetes
	// Team/default CR. Verify via direct k8sClient Get.
	var team litellmv1alpha1.LiteLLMTeam
	err := k8sClient.Get(ctx, client.ObjectKey{Namespace: WatchNamespace, Name: "default"}, &team)
	if !apierrors.IsNotFound(err) {
		t.Errorf("Team/default CR exists in K8s (got err=%v) — spec §7.4 forbids operator-created Team/default CR", err)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 2: BootstrapWhenLiteLLMTeamAlreadyExists
// ──────────────────────────────────────────────────────────────────────────

// TestTeamDefaultRunnable_BootstrapWhenLiteLLMTeamAlreadyExists — plan
// 06-03 behavior #2. Pre-populate the mock with a hand-managed team
// aliased `default`. The synthetic reconcile resolves the existing
// team_id via ListTeamsByAlias and takes the UPDATE arm with the empty
// body. After the first reconcile, subsequent ticker fires hit the
// hash-equal short-circuit — no spurious UPDATE issued.
func TestTeamDefaultRunnable_BootstrapWhenLiteLLMTeamAlreadyExists(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetTeams()
	teamReconciler.ResetImplicitDefaultCache() // Phase 6 cross-suite flake fix (07-CONTEXT.md §Phase-6-flake option α)
	resetConnCacheSnapshot()

	preExistingID := mockServer.AddHandManagedTeam("default")
	t.Logf("pre-existing LiteLLM team aliased `default` with team_id=%q", preExistingID)

	cleanupConn := setupReadyConnectionTeam(t, ctx)
	t.Cleanup(cleanupConn)

	// Wait for at least one UPDATE to fire (initial bootstrap UPDATE
	// against the pre-existing team_id).
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if mockServer.MutationsByTeamAlias("default") >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	muts := mockServer.MutationsByTeamAlias("default")
	if muts < 1 {
		t.Fatalf("synthetic bootstrap did not fire any /team/update against pre-existing `default` within 15s; muts=%d", muts)
	}

	// Let several ticker fires pass; the hash-equal short-circuit must
	// keep MutationsByTeamAlias at 1 (no spurious updates).
	mutsAfterFirst := muts
	time.Sleep(500 * time.Millisecond)
	mutsAfterIdle := mockServer.MutationsByTeamAlias("default")
	if mutsAfterIdle != mutsAfterFirst {
		t.Errorf("hash-equal short-circuit broken: muts grew from %d to %d during 500ms idle",
			mutsAfterFirst, mutsAfterIdle)
	}

	// Verify the team_id is preserved (operator did NOT recreate the team
	// with a new team_id — UPDATE arm fired against the existing one).
	if got := mockServer.GetTeamID("default"); got != preExistingID {
		t.Errorf("team_id changed across UPDATE: was %q, got %q (UPDATE must preserve identity)",
			preExistingID, got)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 3: Bootstrap_GatedOnConnectionReady
// ──────────────────────────────────────────────────────────────────────────

// TestTeamDefaultRunnable_GatedOnConnectionReady — behavior #3.
// With the connection cache snapshot Ready=false, the runnable's Phase-1
// wait loop holds — NO reconcile fires, NO LiteLLM call. Flipping the
// cache to Ready=true allows the runnable to enqueue. We cannot directly
// influence the suite-level TeamDefaultRunnable (it is already running);
// instead this test instantiates a fresh runnable + standalone channel +
// fake cache and observes Phase 1 → Phase 2 transition synchronously.
func TestTeamDefaultRunnable_GatedOnConnectionReady(t *testing.T) {
	rf := &readyFlag{}
	rf.ready.Store(false)
	gated := &gatedCache{rf: rf}

	ch := make(chan reconcile.Request, 4)
	runnable := &TeamDefaultRunnable{
		Cache:             gated,
		Namespace:         WatchNamespace,
		Interval:          100 * time.Millisecond,
		ReadyPollInterval: 20 * time.Millisecond,
		Log:               logr.Discard(),
		RequeueCh:         ch,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = runnable.Start(ctx)
		close(done)
	}()

	// Phase-1 gate: with Ready=false, no enqueue should occur for ≥250ms.
	select {
	case req := <-ch:
		t.Fatalf("runnable enqueued %+v before Ready=true — Phase-1 gate broken", req)
	case <-time.After(250 * time.Millisecond):
	}

	// Flip Ready=true.
	rf.ready.Store(true)

	// Phase-2: initial enqueue should fire within 200ms.
	select {
	case req := <-ch:
		if req.Name != "default" || req.Namespace != WatchNamespace {
			t.Errorf("unexpected request: got %+v, want {%s, default}", req, WatchNamespace)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("runnable did not enqueue Team/default within 500ms of Ready=true")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Errorf("runnable did not exit within 1s after ctx cancel")
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Test 4: Cadence_30MinReList
// ──────────────────────────────────────────────────────────────────────────

// TestTeamDefaultRunnable_Cadence_30MinReList — behavior #4.
// With Interval=100ms (test-scaled), the runnable fires the synthetic
// reconcile at least 4 times in 500ms. Verified via the runnable's
// internal atomic tickCount counter (TickCount).
func TestTeamDefaultRunnable_Cadence_30MinReList(t *testing.T) {
	// Fresh isolated runnable so we don't perturb suite state.
	rf := &readyFlag{}
	rf.ready.Store(true)
	gated := &gatedCache{rf: rf}
	ch := make(chan reconcile.Request, 256)
	runnable := &TeamDefaultRunnable{
		Cache:             gated,
		Namespace:         WatchNamespace,
		Interval:          100 * time.Millisecond,
		ReadyPollInterval: 20 * time.Millisecond,
		Log:               logr.Discard(),
		RequeueCh:         ch,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = runnable.Start(ctx)
		close(done)
	}()

	// Wait 500ms. With 100ms interval + initial enqueue, expect ≥4 ticks.
	time.Sleep(500 * time.Millisecond)
	got := runnable.TickCount()
	if got < 4 {
		t.Errorf("tick cadence: want ≥4 enqueues in 500ms (Interval=100ms), got %d", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Errorf("runnable did not exit within 1s after ctx cancel")
	}

	// Drain channel to avoid leaking the producer (ch is local; GC handles it).
	_ = ch
}

// ──────────────────────────────────────────────────────────────────────────
// Local helpers
// ──────────────────────────────────────────────────────────────────────────

// readyFlag is a tiny atomic.Bool container used by gatedCache to expose
// a flippable Ready state to TeamDefaultRunnable.Phase-1 polling. The
// atomic.Bool keeps the test data-race-free under -race.
type readyFlag struct {
	ready atomic.Bool
}

// gatedCache implements connection.ConnectionCache with a Snapshot whose
// Ready value is read live from an external readyFlag. Used by the
// Bootstrap_GatedOnConnectionReady test to flip the Ready state from
// outside the runnable. The Client field is intentionally nil — the
// gated test exercises only the Phase-1 wait loop, not LiteLLM calls.
type gatedCache struct {
	rf *readyFlag
}

func (g *gatedCache) Snapshot() connection.ConnectionSnapshot {
	return connection.ConnectionSnapshot{Ready: g.rf.ready.Load(), Client: nil}
}

func (g *gatedCache) InvalidateOn401() {}

func (g *gatedCache) Rebuild(_ connection.ConnectionSnapshot) {}

// Compile-time assertion that gatedCache satisfies the interface.
var _ connection.ConnectionCache = (*gatedCache)(nil)
