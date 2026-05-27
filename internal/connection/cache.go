// SPDX-License-Identifier: Apache-2.0

package connection

import (
	"context"
	"sync/atomic"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// Cache is the manager-level LiteLLMConnection cache.
//
// It stores the most recent ConnectionSnapshot under an atomic.Pointer so
// every Phase 3+ domain reconciler reads it lock-free on its hot path
// (D-02). A separate atomic.Bool prevents 401-storm event amplification
// (D-10) — a Cartesian flood of 401s across many CRs results in AT MOST
// ONE enqueued probe per invalidation cycle.
//
// The cache implements manager.Runnable (Start) so its event channel is
// owned by controller-runtime's manager lifecycle (D-08 / D-66): a graceful
// shutdown closes the channel, and any in-flight Source.Channel consumers
// see the close as their exit signal.
//
// The cache itself NEVER constructs a *litellm.Client. Rebuild's caller
// builds a fresh client per probe (D-03 —
// new http.Client, new redacting RoundTripper) and hands the resulting
// ConnectionSnapshot to Rebuild.
type Cache struct {
	// snapshot is the lock-free read store (D-02). Loads are an atomic
	// pointer dereference; Snapshot returns a value copy.
	snapshot atomic.Pointer[ConnectionSnapshot]

	// invalidated is the D-10 storm gate. CompareAndSwap(false, true) on
	// the InvalidateOn401 hot path; reset to false on every Rebuild so the
	// next cycle can fire again.
	invalidated atomic.Bool

	// closed is the CR-02 shutdown guard. Start sets this to true BEFORE
	// close(c.ch) so InvalidateOn401 can short-circuit instead of panicking
	// with `send on closed channel`. Per VERIFICATION.md (Phase 2),
	// graceful-shutdown-after-concurrent-401 was a reproducible panic
	// before this flag was added.
	closed atomic.Bool

	// ch is the D-09 fast-path channel fed into source.Channel by Plan
	// 02-03's cmd/main.go (and Task 3's suite_test.go extension). cap=1 is
	// defense-in-depth — combined with the CAS storm gate, sends never
	// block (a full buffer means an event is already pending; a non-blocking
	// `select default` drops the duplicate).
	ch chan event.GenericEvent

	// rebuiltCh fires a single event on every Rebuild that transitions
	// the cached snapshot into Ready=true (false→true OR initial). Closes
	// the boot-time race where a dependent reconciler's Connection-watch
	// CreateFunc enqueues a child CR BEFORE the connection reconciler's
	// first probe has populated the snapshot — without this signal the
	// child reconcile would read Snapshot()==zero, write
	// Ready=False/LiteLLMUnavailable, and stay stuck until the next spec
	// edit (the connectionReadyTransition predicate also stays silent
	// because Ready=True→Ready=True is not a transition).
	//
	// cap=1 buffer + non-blocking select-default send: a slow consumer
	// just drops the duplicate; the next genuine transition re-fires.
	rebuiltCh chan event.GenericEvent

	// lastReady tracks the most-recent snapshot's Ready value so Rebuild
	// emits on rebuiltCh only when the snapshot transitions FROM
	// not-Ready INTO Ready. Steady-state probe ticks (Ready→Ready,
	// Connecting→Connecting) generate no churn on dependent reconcilers.
	lastReady atomic.Bool

	// log is the per-cache logger. Reserved for future diagnostic use; the
	// hot paths (Snapshot / Rebuild / InvalidateOn401) intentionally do
	// not log to avoid alloc + lock contention.
	log logr.Logger
}

// NewCache constructs a *Cache with an empty (nil) snapshot pointer and a
// cap=1 event channel. No probe has yet completed; Snapshot on the
// returned Cache returns the zero-value ConnectionSnapshot (D-04 "do not
// mutate" signal). Task 2's reconciler calls Rebuild on its
// first reconcile to populate the cache.
func NewCache(log logr.Logger) *Cache {
	return &Cache{
		ch:        make(chan event.GenericEvent, 1),
		rebuiltCh: make(chan event.GenericEvent, 1),
		log:       log,
		// snapshot: zero-value atomic.Pointer — Load returns nil until
		// the first Rebuild. Snapshot handles the nil case explicitly.
	}
}

// Snapshot returns the current ConnectionSnapshot as a value copy (D-02).
//
// The implementation is a single atomic.Pointer.Load + a pointer
// dereference. No locks, no allocations on the hot path (the
// ConnectionSnapshot struct is stack-allocated by the caller). When no
// probe has completed yet, the zero-value ConnectionSnapshot{} is
// returned — the universal "do not mutate" signal per D-04 (Ready=false,
// Client=nil, Reason="", Generation=0).
//
// Safe for arbitrary concurrent callers: Phase 3+ reconcilers may call
// this from many goroutines simultaneously.
func (c *Cache) Snapshot() ConnectionSnapshot {
	if snap := c.snapshot.Load(); snap != nil {
		return *snap
	}
	return ConnectionSnapshot{}
}

// Rebuild atomically replaces the cached snapshot and resets the storm
// gate so the next InvalidateOn401 cycle can fire.
//
// The caller is responsible for
// constructing the ConnectionSnapshot with a FRESH *litellm.Client per
// D-03 — new http.Client, new transport, new redacting RoundTripper.
// The cache itself never calls litellm.NewClient; that would couple this
// package to the wire layer and inflate the test surface.
//
// Rebuild is called on every probe outcome (Synced, Connecting,
// Unreachable, BadMasterKey, SecretNotFound) and on the finalizer
// cache-clear path. The storm gate reset
// per D-10 fires regardless of outcome: success, terminal failure, or
// transient failure all bound the gate to "at most one event per
// invalidation cycle".
func (c *Cache) Rebuild(snap ConnectionSnapshot) {
	// Store a pointer to a defensive copy. Even though snap is passed by
	// value (so the caller cannot mutate the cached state via aliasing),
	// taking &snap inside Rebuild ensures the stored pointer references
	// the function-local copy — not a stack-allocated value the caller
	// might overwrite on return.
	c.snapshot.Store(&snap)
	// D-10: reset the storm gate so the next 401 fast-path can fire.
	c.invalidated.Store(false)

	// Emit a rebuilt event on false→true (or initial) Ready transitions
	// so dependent reconcilers can re-enqueue their CRs and close the
	// boot-time race against the Connection-watch CreateFunc. Bounded:
	// steady-state Ready→Ready probe ticks do NOT fire.
	switch {
	case snap.Ready:
		if !c.lastReady.Swap(true) {
			c.emitRebuilt()
		}
	default:
		c.lastReady.Store(false)
	}
}

// emitRebuilt performs a non-blocking send on the rebuiltCh, with
// CR-02 shutdown defenses identical to InvalidateOn401's send path:
// the closed-flag short-circuit handles the steady case, and the
// `defer recover` catches the TOCTOU window where Start fires between
// the check and the send.
func (c *Cache) emitRebuilt() {
	if c.closed.Load() {
		return
	}
	defer func() { _ = recover() }()
	select {
	case c.rebuiltCh <- event.GenericEvent{}:
	default:
	}
}

// InvalidateOn401 is called by any Phase 3+ domain reconciler that
// receives a *litellm.Auth401Error from a LiteLLM HTTP call (CONN-05 part
// (c), D-09).
//
// Behavior (CAS gate per D-10):
//
// 1. CR-02 shutdown guard: if c.closed.Load is true (Start has set it
// during graceful shutdown), return immediately without touching the
// channel. Without this guard, a send on the closed channel panics
// with `send on closed channel` — the reproducible CR-02 defect.
// 2. CompareAndSwap(false, true) on the invalidated gate. If the swap
// fails (gate already raised), this is a duplicate caller in the same
// storm and we return without side effects.
// 3. If the swap succeeded, load the current snapshot and construct a
// placeholder PRESERVING Generation + Client (WR-04). Only Ready
// flips to false and Reason becomes "BadMasterKey" — the §6.0
// vocabulary entry matching a 401 outcome. Phase 3+ logic comparing
// snap.Generation against observedConnectionGeneration no longer
// sees a spurious zero. The next Connecting-on-entry write (D-07)
// overwrites this placeholder when the reconciler re-probes.
// 4. Non-blocking send on c.ch via select default. Combined with the
// cap=1 channel buffer and the CAS gate, a 401-storm across N CRs
// produces AT MOST ONE enqueued probe per invalidation cycle. The
// send is wrapped in `defer recover` as defense-in-depth for the
// TOCTOU window between the closed.Load check at entry and this
// send — the closed-flag check is the primary guard; recover is
// silent and catches the narrow race where Start fires between the
// check and the send.
//
// The gate resets in Rebuild after the connection reconciler completes
// the next probe (success or terminal failure).
//
// §9.1: never logs the master key, never logs the Auth401Error.Body. The
// caller's logger already redacts on the wire layer.
func (c *Cache) InvalidateOn401() {
	// CR-02 shutdown guard — primary defense.
	if c.closed.Load() {
		return
	}
	if c.invalidated.CompareAndSwap(false, true) {
		// WR-04: preserve Generation and Client across the 401
		// placeholder. Loading the current snapshot via atomic.Pointer
		// is the same lock-free read Snapshot uses; this is the hot
		// path so we accept the small extra dereference.
		placeholder := ConnectionSnapshot{
			Ready:  false,
			Reason: "BadMasterKey",
		}
		if cur := c.snapshot.Load(); cur != nil {
			placeholder.Generation = cur.Generation
			placeholder.Client = cur.Client
		}
		c.snapshot.Store(&placeholder)
		// CR-02 defense-in-depth: recover from a TOCTOU `send on closed
		// channel` panic if Start fires between the closed.Load above
		// and the send below. Silent — the primary guard already handled
		// the expected case; this just keeps the operator alive on the
		// narrow race window.
		defer func() { _ = recover() }()
		// Non-blocking send — see method doc. The buffered channel + CAS
		// gate enforce the storm bound; this default is defense-in-depth.
		select {
		case c.ch <- event.GenericEvent{}:
		default:
		}
	}
}

// Channel returns the read-only event channel feeding the Source.Channel
// the LiteLLMConnection reconciler installs in its SetupWithManager
// (D-09). cmd/main.go and Task 3's suite_test.go
// extension both call this and pass the result through to the reconciler's
// SetupWithManager(mgr, ch).
//
// The channel direction is read-only — callers cannot send into the
// cache's invalidation pipe. All sends originate inside InvalidateOn401.
func (c *Cache) Channel() <-chan event.GenericEvent {
	return c.ch
}

// Rebuilt returns the read-only event channel that fires every time
// Rebuild transitions the cached snapshot into Ready=true. Dependent
// reconcilers (guardrail, model, mcpserver, a2aagent, team, modelalias)
// wire this through ConnectionRebuiltSource in their SetupWithManager
// so a re-enqueue happens as soon as the cache is populated — closing
// the boot-time race the connectionReadyTransition predicate cannot
// catch (a Ready=True→Ready=True status write is not a transition).
//
// Read-only by direction; all sends originate inside Cache.Rebuild.
func (c *Cache) Rebuilt() <-chan event.GenericEvent {
	return c.rebuiltCh
}

// Start implements manager.Runnable so the cache participates in the
// manager's graceful-shutdown lifecycle (D-08 / D-66).// cmd/main.go calls mgr.Add(cache) before mgr.Start; Task 3's
// suite_test.go extension calls the same.
//
// Behavior: block until ctx is cancelled (manager shutdown), then close
// the channel so any source.Channel consumer sees the close as its exit
// signal. Returns nil unconditionally — this Runnable has no failure
// mode beyond cancellation.
//
// Per CR-02, c.closed is set BEFORE close(c.ch) so InvalidateOn401 can
// short-circuit instead of attempting a send on the now-closed channel.
// The ordering is load-bearing: a concurrent InvalidateOn401 caller that
// observes closed==true via the entry-guard will return without touching
// the channel; one that races past the load (TOCTOU) is caught by the
// `defer recover` defense-in-depth inside InvalidateOn401.
func (c *Cache) Start(ctx context.Context) error {
	<-ctx.Done()
	c.closed.Store(true)
	close(c.ch)
	close(c.rebuiltCh)
	return nil
}

// Compile-time assertion: *Cache satisfies the ConnectionCache interface
// (D-12). Without this line, an unintended signature change to any of
// Snapshot/InvalidateOn401 would only surface at the first call site,
// which may be in a downstream phase. The assertion fails the build of
// THIS package, surfacing the breakage at the source.
var _ ConnectionCache = (*Cache)(nil)
