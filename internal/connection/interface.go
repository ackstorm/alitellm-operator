// SPDX-License-Identifier: Apache-2.0

package connection

// ConnectionCache is the shared read API every Phase 3+ domain
// reconciler accepts (D-12). It is implemented by BOTH:
//
// - The real *connection.Cache (atomic.Pointer-backed,
// manager.Runnable, owns the Source.Channel for the 401 fast-path).
// - The Phase 1 *controller.FakeConnectionCache test double (after
// Adds a thin Snapshot method that returns a value
// derived from the existing Invalidated atomic.Bool — Phase 1's
// fastpath_test.go / idempotency_test.go / idempotency_long_test.go /
// metrics_scrape_test.go continue to compile and pass).
//
// Every Phase 3+ reconciler MUST type its cache field as ConnectionCache
// (the interface) — NEVER as *connection.Cache (the concrete type) — so
// the test double can be substituted without code change. The bbdsoftware
// upstream's instance-typed cache was the failure mode this contract
// avoids.
type ConnectionCache interface {
	// Snapshot returns the current ConnectionSnapshot as a value copy.
	//
	// Per D-02, the return type is the value (not a pointer); callers
	// cannot mutate cached state through the returned snapshot. The
	// underlying implementation does a single
	// atomic.Pointer.Load then a dereference — a zero-alloc, lock-free
	// hot path safe for arbitrary concurrent callers. When no probe has
	// yet completed, the zero-value ConnectionSnapshot{} (Ready=false,
	// Client=nil) is returned — the universal "do not mutate" signal
	// per D-04.
	Snapshot() ConnectionSnapshot

	// InvalidateOn401 is called by any Phase 3+ domain reconciler that
	// receives a *litellm.Auth401Error from a LiteLLM HTTP call. It
	// atomically:
	//
	// 1. Marks the cache as not-Ready (: stores a snapshot
	// with Ready=false; Phase 1 fake: sets Invalidated=true).
	// 2. Enqueues one out-of-band probe of LiteLLMConnection/default
	// so the connection reconciler re-validates the master key
	// and rebuilds the cache (: sends one event on a
	// buffered channel feeding source.Channel).
	//
	// Per D-09 the channel mechanism is internal to the real Cache; the
	// interface deliberately exposes no channel, no event, no return
	// value — callers don't need to know how the enqueue happens, only
	// that calling this method is enough to trigger the fast-path.
	//
	// Per D-10 the implementation is CAS-gated and idempotent under
	// concurrent callers: a 401-storm where N domain reconcilers all
	// see 401 simultaneously results in AT MOST ONE enqueued probe per
	// invalidation cycle (the flag resets when the next probe
	// completes — success or terminal failure). The no-arg, no-return
	// shape is intentional: the cache owns the storm gate internally,
	// callers don't.
	InvalidateOn401()

	// Rebuild atomically replaces the cached snapshot. It is the
	// cache-write entry point used by the LiteLLMConnection reconciler
	// on every probe outcome (Synced, Connecting, Unreachable,
	// BadMasterKey, SecretNotFound) AND on the finalizer cache-clear
	// path (Reason="Absent").
	//
	// Per D-12, promoting Rebuild to the interface (rather than typing
	// the reconciler's Cache field as the concrete *Cache) is the
	// load-bearing change that lets *FakeConnectionCache substitute for
	// *Cache without runtime type-assertion panics. Phase 2's
	// (CR-01 close) replaced six `r.Cache.(*connection.Cache).Rebuild(.)`
	// type assertions in the reconciler with `r.Cache.Rebuild(.)`,
	// closing the gap 02-VERIFICATION.md flagged as CR-01.
	//
	// Implementations MUST also reset any storm gate they maintain so
	// the next InvalidateOn401 cycle can fire (this is what *Cache's
	// concrete implementation does — see cache.go).
	Rebuild(snapshot ConnectionSnapshot)
}
