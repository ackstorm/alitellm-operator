// SPDX-License-Identifier: Apache-2.0

package connection

import (
	"context"
	"sync"
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

	// rebuiltSubs is the slice of per-subscriber cap=1 channels feeding
	// dependent reconcilers (guardrail, model, mcpserver, a2aagent, team,
	// modelalias). Subscribe() appends one fresh channel and returns it;
	// emitRebuilt fans-out a single GenericEvent to EVERY subscriber.
	//
	// Each emit closes the boot-time race where a dependent reconciler's
	// Connection-watch CreateFunc enqueues a child CR BEFORE the
	// connection reconciler's first probe has populated the snapshot —
	// without this signal the child reconcile would read Snapshot()==zero,
	// write Ready=False/LiteLLMUnavailable, and stay stuck until the next
	// spec edit (the connectionReadyTransition predicate also stays
	// silent because Ready=True→Ready=True is not a transition).
	//
	// Per-subscriber cap=1 + non-blocking select-default send: a slow
	// consumer just drops the duplicate; the next genuine transition
	// re-fires for every subscriber.
	//
	// CR-01 (PR #45 follow-up): the earlier implementation exposed a
	// single shared `Rebuilt()` channel and Go's exactly-one-receiver
	// semantics turned the fan-out into a 1-in-N lottery per emit.
	// Mirrors BootSweeper's per-kind channel design.
	subsMu      sync.RWMutex
	rebuiltSubs []chan event.GenericEvent

	// lastReady tracks the most-recent snapshot's Ready value so Rebuild
	// emits on rebuiltSubs only when the snapshot transitions FROM
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
		ch:  make(chan event.GenericEvent, 1),
		log: log,
		// snapshot: zero-value atomic.Pointer — Load returns nil until
		// the first Rebuild. Snapshot handles the nil case explicitly.
		// rebuiltSubs: nil — Subscribe() lazy-appends on each call.
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

// emitRebuilt fans-out a single GenericEvent to every registered
// subscriber via per-subscriber non-blocking sends. CR-02 shutdown
// defenses mirror InvalidateOn401's send path: the closed-flag
// short-circuit handles the steady case, and the `defer recover`
// catches the TOCTOU window where Start fires between the check and
// the send.
//
// Snapshot subs under RLock then release before sending so a slow
// subscriber cannot block other emit cycles or a concurrent Subscribe.
// Per-channel cap=1 + select-default keeps any single subscriber from
// blocking the fan-out loop — duplicates collapse into "one event
// pending".
func (c *Cache) emitRebuilt() {
	if c.closed.Load() {
		return
	}
	c.subsMu.RLock()
	subs := c.rebuiltSubs
	c.subsMu.RUnlock()
	defer func() { _ = recover() }()
	for _, ch := range subs {
		select {
		case ch <- event.GenericEvent{}:
		default:
		}
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

// Subscribe registers a fresh per-subscriber cap=1 event channel and
// returns its read-only end. Every Rebuild that transitions the
// snapshot to Ready=true fans-out exactly one GenericEvent to EVERY
// channel ever returned by Subscribe.
//
// Dependent reconcilers (guardrail, model, mcpserver, a2aagent, team,
// modelalias) call Subscribe once at SetupWithManager time and wire the
// resulting channel through ConnectionRebuiltSource so a re-enqueue
// happens as soon as the cache is populated — closing the boot-time
// race the connectionReadyTransition predicate cannot catch (a
// Ready=True→Ready=True status write is not a transition).
//
// Why per-subscriber and not one shared channel: Go channel semantics
// deliver each send to EXACTLY ONE receiver. With N reconcilers each
// running a goroutine doing `<-sharedCh`, only 1-in-N would receive
// any given Ready emit (the runtime scheduler picks the winner
// non-deterministically) and N-1 reconcilers would stay stuck — the
// exact bug PR #45 was meant to fix. This is CR-01 in
// .planning/reviews/PR-45-REVIEW.md. The per-subscriber slice +
// per-channel fan-out is the same pattern BootSweeper uses
// (internal/controller/bootsweep.go:43-75).
//
// Subscribe MUST be called before mgr.Start (no add-after-start
// support); registration order is irrelevant. The returned channel is
// closed by Cache.Start during graceful shutdown, so source.Channel
// consumers observe EOF.
func (c *Cache) Subscribe() <-chan event.GenericEvent {
	ch := make(chan event.GenericEvent, 1)
	c.subsMu.Lock()
	c.rebuiltSubs = append(c.rebuiltSubs, ch)
	c.subsMu.Unlock()
	return ch
}

// Start implements manager.Runnable so the cache participates in the
// manager's graceful-shutdown lifecycle (D-08 / D-66).
//
// cmd/main.go calls mgr.Add(cache) before mgr.Start; Task 3's
// suite_test.go extension calls the same.
//
// Behavior: block until ctx is cancelled (manager shutdown), then close
// every owned channel so any source.Channel consumer sees the close as
// its exit signal. Returns nil unconditionally — this Runnable has no
// failure mode beyond cancellation.
//
// Per CR-02, c.closed is set BEFORE the channel closes so
// InvalidateOn401 / emitRebuilt can short-circuit instead of attempting
// a send on the now-closed channel. The ordering is load-bearing: a
// concurrent caller that observes closed==true via the entry-guard
// returns without touching the channel; one that races past the load
// (TOCTOU) is caught by the `defer recover` defense-in-depth inside
// the send paths.
func (c *Cache) Start(ctx context.Context) error {
	<-ctx.Done()
	c.closed.Store(true)
	close(c.ch)
	c.subsMu.Lock()
	for _, ch := range c.rebuiltSubs {
		close(ch)
	}
	c.rebuiltSubs = nil
	c.subsMu.Unlock()
	return nil
}

// Compile-time assertion: *Cache satisfies the ConnectionCache interface
// (D-12). Without this line, an unintended signature change to any of
// Snapshot/InvalidateOn401 would only surface at the first call site,
// which may be in a downstream phase. The assertion fails the build of
// THIS package, surfacing the breakage at the source.
var _ ConnectionCache = (*Cache)(nil)
