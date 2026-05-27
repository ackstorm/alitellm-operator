// SPDX-License-Identifier: Apache-2.0

package connection

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/ackstorm/alitellm-operator/internal/litellm"
)

// TestNewCacheZeroValue — Task 1 behavior 1.
// NewCache returns a non-nil *Cache; Snapshot on a fresh cache returns
// the zero-value ConnectionSnapshot (Ready=false, Client=nil) — D-04
// "do not mutate" gate.
func TestNewCacheZeroValue(t *testing.T) {
	c := NewCache(logr.Discard())
	if c == nil {
		t.Fatal("NewCache returned nil")
	}
	snap := c.Snapshot()
	if snap.Ready {
		t.Errorf("fresh cache Snapshot().Ready = true, want false (D-04 zero-value gate)")
	}
	if snap.Client != nil {
		t.Errorf("fresh cache Snapshot().Client = %v, want nil", snap.Client)
	}
	if snap.Reason != "" {
		t.Errorf("fresh cache Snapshot().Reason = %q, want \"\"", snap.Reason)
	}
	if snap.Generation != 0 {
		t.Errorf("fresh cache Snapshot().Generation = %d, want 0", snap.Generation)
	}
}

// TestRebuildSnapshotRoundtrip — Task 1 behavior 2.
// Rebuild(snap) followed by Snapshot returns the same value (lock-free read).
func TestRebuildSnapshotRoundtrip(t *testing.T) {
	c := NewCache(logr.Discard())
	in := ConnectionSnapshot{Ready: true, Reason: "Synced", Generation: 7}
	c.Rebuild(in)
	out := c.Snapshot()
	if out.Ready != in.Ready || out.Reason != in.Reason || out.Generation != in.Generation {
		t.Errorf("Snapshot() = %+v, want %+v", out, in)
	}
}

// TestInvalidateOn401StormGate — Task 1 behavior 3.
// InvalidateOn401 called five times in tight succession sends only ONE
// event on Channel (CAS storm gate per D-10).
func TestInvalidateOn401StormGate(t *testing.T) {
	c := NewCache(logr.Discard())
	for i := 0; i < 5; i++ {
		c.InvalidateOn401()
	}
	ch := c.Channel()
	// First read should succeed quickly.
	select {
	case <-ch:
	case <-time.After(1 * time.Second):
		t.Fatal("expected at least 1 event on Channel() after InvalidateOn401, got none")
	}
	// Second read must time out — storm gate prevented duplicate events.
	select {
	case <-ch:
		t.Error("storm gate FAIL: got a second event from 5 tight InvalidateOn401 calls (D-10 CAS gate broken)")
	case <-time.After(100 * time.Millisecond):
		// Expected: no second event.
	}
}

// TestStormGateResetsOnRebuild — Task 1 behavior 4.
// After InvalidateOn401 then Rebuild, the storm gate resets — a subsequent
// InvalidateOn401 sends a new event.
func TestStormGateResetsOnRebuild(t *testing.T) {
	c := NewCache(logr.Discard())
	c.InvalidateOn401()
	ch := c.Channel()
	// Drain the first event.
	select {
	case <-ch:
	case <-time.After(1 * time.Second):
		t.Fatal("expected first event")
	}
	// Rebuild resets the storm gate (D-10).
	c.Rebuild(ConnectionSnapshot{Ready: true, Reason: "Synced"})
	// New InvalidateOn401 should send a new event.
	c.InvalidateOn401()
	select {
	case <-ch:
		// Expected.
	case <-time.After(1 * time.Second):
		t.Error("storm gate did not reset after Rebuild: second InvalidateOn401 produced no event")
	}
}

// TestStartBlocksUntilContextCancelled — Task 1 behavior 5.
// Start(ctx) blocks until ctx is cancelled, then closes the channel and returns nil
// (manager.Runnable contract).
func TestStartBlocksUntilContextCancelled(t *testing.T) {
	c := NewCache(logr.Discard())
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- c.Start(ctx) }()

	// Verify Start has NOT returned yet.
	select {
	case err := <-done:
		t.Fatalf("Start returned before ctx cancel: %v", err)
	case <-time.After(100 * time.Millisecond):
		// Expected: still running.
	}

	cancel()

	// Verify Start returns nil after ctx cancel.
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start returned err=%v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return within 2s of ctx cancel")
	}

	// Verify channel is closed.
	ch := c.Channel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("Channel() received a value after Start returned; expected closed channel")
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("Channel() did not yield a closed-channel read after Start returned")
	}
}

// TestCacheSatisfiesInterface — Task 1 behavior 6.
// Compile-time check that *Cache implements ConnectionCache (D-12).
func TestCacheSatisfiesInterface(t *testing.T) {
	var _ ConnectionCache = (*Cache)(nil)
}

// TestSubscribeEmitsOnInitialReady — issue #44 close (PR #45 follow-up).
// First Rebuild with Ready=true emits exactly ONE event on a Subscribe()
// channel.
func TestSubscribeEmitsOnInitialReady(t *testing.T) {
	c := NewCache(logr.Discard())
	sub := c.Subscribe()
	c.Rebuild(ConnectionSnapshot{Ready: true, Reason: "Synced"})
	select {
	case <-sub:
	case <-time.After(1 * time.Second):
		t.Fatal("expected event on Subscribe() channel after initial Ready=true Rebuild")
	}
	// No spurious second event on Ready=true→Ready=true.
	c.Rebuild(ConnectionSnapshot{Ready: true, Reason: "Synced"})
	select {
	case <-sub:
		t.Error("got a duplicate event on Ready=true→Ready=true; transition gate broken")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestSubscribeSilentOnNotReady — issue #44 close (PR #45 follow-up).
// Rebuild with Ready=false MUST NOT emit; subsequent Ready=true transitions
// MUST emit.
func TestSubscribeSilentOnNotReady(t *testing.T) {
	c := NewCache(logr.Discard())
	sub := c.Subscribe()
	c.Rebuild(ConnectionSnapshot{Ready: false, Reason: "Connecting"})
	select {
	case <-sub:
		t.Fatal("got event on Ready=false Rebuild; want silent")
	case <-time.After(100 * time.Millisecond):
	}
	c.Rebuild(ConnectionSnapshot{Ready: true, Reason: "Synced"})
	select {
	case <-sub:
	case <-time.After(1 * time.Second):
		t.Fatal("expected event on false→true transition")
	}
}

// TestSubscribeReFiresAfterReadyFlap — issue #44 close (PR #45 follow-up).
// Ready=true → Ready=false → Ready=true MUST emit on both Ready=true events.
func TestSubscribeReFiresAfterReadyFlap(t *testing.T) {
	c := NewCache(logr.Discard())
	sub := c.Subscribe()
	c.Rebuild(ConnectionSnapshot{Ready: true})
	<-sub // drain initial
	c.Rebuild(ConnectionSnapshot{Ready: false, Reason: "Unreachable"})
	c.Rebuild(ConnectionSnapshot{Ready: true})
	select {
	case <-sub:
	case <-time.After(1 * time.Second):
		t.Fatal("expected event on second false→true transition after flap")
	}
}

// TestSubscribeClosedOnShutdown — issue #44 close (PR #45 follow-up).
// Cache.Start exit closes every Subscribe() channel so source.Channel
// consumers see EOF.
func TestSubscribeClosedOnShutdown(t *testing.T) {
	c := NewCache(logr.Discard())
	sub := c.Subscribe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Start(ctx) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after cancel")
	}
	select {
	case _, ok := <-sub:
		if ok {
			t.Error("subscriber channel yielded a value after shutdown; want closed")
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("subscriber channel did not return closed-channel read after shutdown")
	}
}

// TestSubscribeAfterShutdownNoPanic — issue #44 + CR-02 (PR #45 follow-up).
// Calling Rebuild after Start has closed every subscriber channel must not
// panic (the emit path's closed-flag check + defer-recover keep operator
// alive).
func TestSubscribeAfterShutdownNoPanic(t *testing.T) {
	c := NewCache(logr.Discard())
	_ = c.Subscribe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = c.Start(ctx); close(done) }()
	cancel()
	<-done
	// Should not panic.
	c.Rebuild(ConnectionSnapshot{Ready: true})
}

// TestSubscribeFanOutToAllSubscribers — CR-01 (PR #45 follow-up).
//
// EVERY subscriber receives EVERY false→true Ready transition emit.
//
// Before this fix the cache exposed a single shared Rebuilt() channel
// and Go's exactly-one-receiver semantics turned the fan-out into a
// 1-in-N lottery per emit: N-1 reconcilers stayed stuck after the cold-
// start Ready transition the #44 fix was meant to handle. The
// per-subscriber slice + per-channel non-blocking send (Cache.Subscribe
// / emitRebuilt) is the correct fan-out — same pattern BootSweeper
// uses for its per-kind channels. This test would have caught the
// regression in PR #45 before merge.
//
// 6 subscribers matches production wiring (Model, MCPServer, A2AAgent,
// Team, GuardRail, ModelAlias) but the property under test is "every
// subscriber receives every emit", not the count itself.
func TestSubscribeFanOutToAllSubscribers(t *testing.T) {
	c := NewCache(logr.Discard())
	const n = 6
	subs := make([]<-chan event.GenericEvent, n)
	for i := range subs {
		subs[i] = c.Subscribe()
	}
	c.Rebuild(ConnectionSnapshot{Ready: true})
	for i, sub := range subs {
		select {
		case <-sub:
		case <-time.After(1 * time.Second):
			t.Fatalf("subscriber %d did not receive event; fan-out broken", i)
		}
	}
}

// TestSubscribeFanOutAcrossFlap — CR-01 (PR #45 follow-up).
//
// On a Ready=true → Ready=false → Ready=true cycle every subscriber MUST
// receive BOTH Ready=true emits (initial + post-flap recovery). Stress
// case for the per-subscriber fan-out across multiple transitions.
func TestSubscribeFanOutAcrossFlap(t *testing.T) {
	c := NewCache(logr.Discard())
	const n = 6
	subs := make([]<-chan event.GenericEvent, n)
	for i := range subs {
		subs[i] = c.Subscribe()
	}
	c.Rebuild(ConnectionSnapshot{Ready: true})
	for i, sub := range subs {
		select {
		case <-sub:
		case <-time.After(1 * time.Second):
			t.Fatalf("subscriber %d missed initial emit", i)
		}
	}
	c.Rebuild(ConnectionSnapshot{Ready: false, Reason: "Unreachable"})
	c.Rebuild(ConnectionSnapshot{Ready: true})
	for i, sub := range subs {
		select {
		case <-sub:
		case <-time.After(1 * time.Second):
			t.Fatalf("subscriber %d missed post-flap emit", i)
		}
	}
}

// TestCache_InvalidateOn401_AfterShutdown_NoPanic — CR-02
// close.
//
// Procedure:
//
// 1. Construct a fresh Cache.
// 2. Start it in a goroutine and wait for it to take ownership of the
// channel.
// 3. Cancel the context; wait for Start to return (so c.closed is set
// AND c.ch is closed).
// 4. Rebuild with a non-zero snapshot so InvalidateOn401's CAS gate is
// re-armed (Rebuild resets c.invalidated to false).
// 5. Call InvalidateOn401 — per the CR-02 fix this MUST NOT panic.
// The closed-flag guard at entry short-circuits the method; the
// deferred recover is defense-in-depth for the TOCTOU window.
//
// Without the CR-02 fix this test panics with `send on closed channel`
// (the reproduction documented in 02-VERIFICATION.md).
func TestCache_InvalidateOn401_AfterShutdown_NoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("CR-02 FAIL: InvalidateOn401 panicked after shutdown: %v", r)
		}
	}()

	cache := NewCache(logr.Discard())
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- cache.Start(ctx) }()

	// Give Start a moment to begin blocking on ctx.Done.
	time.Sleep(50 * time.Millisecond)

	cancel()

	// Wait for Start to return (c.closed.Store(true) BEFORE close(c.ch)).
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned error: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("Start did not return within 1s after ctx cancel")
	}

	// Reset the storm gate so InvalidateOn401's CAS does not short-circuit
	// before we reach the (closed-channel-send) defect site. Rebuild stores
	// into the snapshot atomic.Pointer even though c.closed is set — this
	// is fine; Rebuild does not touch the channel.
	cache.Rebuild(ConnectionSnapshot{Ready: true, Reason: "Synced"})

	// The actual test: this must NOT panic. The CR-02 closed-flag guard
	// at entry to InvalidateOn401 returns early; even if the guard misses
	// due to TOCTOU, the deferred recover catches the panic silently.
	cache.InvalidateOn401()
}

// TestCache_InvalidateOn401_BeforeStart_NoPanic — corner case where the
// cache is constructed but Start was never called.
//
// In this state c.closed is false (zero value) and c.ch is open (cap=1).
// The CAS storm gate fires once, the cache snapshot updates, and the
// non-blocking send into the cap=1 channel succeeds. No panic.
//
// This codepath would be exercised by a misconfigured test that
// constructs a *Cache without adding it to a manager. The Phase 2
// envtests do not hit this — TestMain calls mgr.Add(connCache) before
// mgr.Start — but the property is worth locking.
func TestCache_InvalidateOn401_BeforeStart_NoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("InvalidateOn401 panicked before Start: %v", r)
		}
	}()

	cache := NewCache(logr.Discard())
	cache.InvalidateOn401()
}

// TestCache_InvalidateOn401_PreservesGenerationAndClient_WR04 — Plan
// 02-04 WR-04 close.
//
// Before this plan, InvalidateOn401's placeholder snapshot was
// ConnectionSnapshot{Ready: false, Reason: "Unreachable"}, dropping
// Generation and Client. Phase 3+ logic comparing snap.Generation against
// observedConnectionGeneration would see a spurious zero in the one-
// reconcile window between InvalidateOn401 and the next Rebuild.
//
// The fix preserves Generation + Client from the current snapshot and
// uses Reason "BadMasterKey" (the §6.0 vocabulary entry matching a 401).
//
// Procedure:
//
// 1. Construct a fresh Cache.
// 2. Construct a non-nil *litellm.Client via NewClient (no network call
// happens — the client is just a struct that holds the endpoint).
// 3. Seed the cache via Rebuild with a snapshot {Ready:true,
// Reason:"Synced", Generation:42, Client:client}.
// 4. Call InvalidateOn401.
// 5. Read Snapshot. Assert:
// - Ready == false (flipped)
// - Reason == "BadMasterKey" (NOT "Unreachable" — WR-04 fix)
// - Generation == 42 (PRESERVED — WR-04 fix)
// - Client == client (PRESERVED, pointer identity — WR-04 fix)
func TestCache_InvalidateOn401_PreservesGenerationAndClient_WR04(t *testing.T) {
	cache := NewCache(logr.Discard())

	// Construct a non-nil sentinel *litellm.Client. No network call is
	// issued by NewClient; the endpoint is only used at probe time.
	client := litellm.NewClient("http://example.invalid:4000", "sk-test", logr.Discard())
	if client == nil {
		t.Fatalf("NewClient returned nil; cannot exercise Client preservation")
	}

	seed := ConnectionSnapshot{
		Ready:      true,
		Reason:     "Synced",
		Generation: 42,
		Client:     client,
	}
	cache.Rebuild(seed)

	cache.InvalidateOn401()

	snap := cache.Snapshot()

	if snap.Ready {
		t.Errorf("WR-04 FAIL: snap.Ready = true after InvalidateOn401(); want false")
	}
	if snap.Reason != "BadMasterKey" {
		t.Errorf("WR-04 FAIL: snap.Reason = %q after InvalidateOn401(); want %q (§6.0 vocabulary entry for a 401 outcome)",
			snap.Reason, "BadMasterKey")
	}
	if snap.Generation != 42 {
		t.Errorf("WR-04 FAIL: Generation dropped after InvalidateOn401() (was 42, now %d) — Phase 3+ observedConnectionGeneration comparison will see a spurious zero",
			snap.Generation)
	}
	if snap.Client != client {
		t.Errorf("WR-04 FAIL: Client pointer changed after InvalidateOn401() (want pointer identity to seed Client) — dependents holding a *litellm.Client reference may break under the placeholder write")
	}
}
