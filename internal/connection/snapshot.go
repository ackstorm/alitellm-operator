// SPDX-License-Identifier: Apache-2.0

package connection

import (
	"github.com/ackstorm/alitellm-operator/internal/litellm"
)

// ConnectionSnapshot is the immutable value returned by Cache.Snapshot
// (and by *controller.FakeConnectionCache.Snapshot per D-12).
//
// # Value semantics (D-02)
//
// Snapshot returns ConnectionSnapshot BY VALUE — not a pointer. This
// is a deliberate, type-level guarantee that callers cannot mutate
// cached state through the returned snapshot. The cache holds the
// canonical pointer internally;
// every caller sees a fresh, read-only copy of the struct.
//
// The embedded *litellm.Client pointer IS shared across many readers —
// that is safe because the Client is read-only after construction (D-03).
// The cache rebuilds the entire Client on every probe (Secret rotation,
// generation change, 401 fast-path); there is no in-place mutation of
// the Client. When the cache replaces the snapshot, the previous
// *litellm.Client is GC'd as soon as the last in-flight reconcile drops
// its reference.
//
// # Zero-value semantics (D-04)
//
// The zero value ConnectionSnapshot{} has Ready=false, Reason="",
// Client=nil, Generation=0. This is the universal "do not mutate"
// signal — whether the CR is absent, deleted, mid-probe, Unreachable,
// BadMasterKey, or SecretNotFound. Dependents need only check
// `snap.Ready` to gate any LiteLLM mutation call; the Reason field
// (when non-empty) carries the underlying cause for the dependent's
// own Ready=False, reason=LiteLLMUnavailable status propagation per
// §6.0.
type ConnectionSnapshot struct {
	// Ready is the single boolean gate. true iff the most recent probe
	// succeeded AND the underlying
	// CR is not being deleted AND no in-flight 401 invalidation is
	// pending. Dependents do:
	//
	// snap := cache.Snapshot
	// if !snap.Ready {
	// // set Ready=False, reason=LiteLLMUnavailable; return nil
	// }
	//
	// Per D-04 this is a single boolean gate — sufficient because §6.0
	// dependents emit Ready=False, reason=LiteLLMUnavailable regardless
	// of underlying cause; the Reason field is for diagnostic message
	// composition, not for branching.
	Ready bool

	// Reason carries the most recent §6.0 condition reason, one of:
	//
	// - "Synced" — probe succeeded; Ready=true; Client is the
	// fresh *litellm.Client to use for mutations.
	// - "Connecting" — probe in flight or queued; Ready=false.
	// - "Unreachable" — last probe failed with a transient error
	// (5xx, TCP reset, context deadline).
	// - "BadMasterKey" — last probe returned 401 (typed
	// *litellm.Auth401Error per REL-06).
	// - "SecretNotFound" — the masterKeySecretRef Secret or its key
	// is missing.
	//
	// The empty string (zero value) is treated as "no probe yet" —
	// Maps this on entry to the "Connecting" status surface
	// per D-07 (write Connecting on entry, then overwrite with the
	// final reason after the probe completes).
	Reason string

	// Client is the *litellm.Client to use for mutations IFF Ready==true.
	// On any not-ready snapshot Client is nil (and dependents MUST NOT
	// dereference it). enforces this invariant in
	// Cache.rebuild — every not-ready snapshot it stores has Client=nil.
	//
	// Shared pointer is intentional and safe: *litellm.Client is
	// read-only after construction (D-03). Multiple Phase 3+ reconcilers
	// concurrently call methods on the same *Client across reconciles —
	// the Client's internal *http.Client and redacting RoundTripper are
	// already concurrency-safe.
	Client *litellm.Client

	// Generation is the metadata.generation of the LiteLLMConnection CR
	// the snapshot was built from. Dependents may compare against their
	// own status.observedConnectionGeneration to detect a fresh
	// connection rebuild (not required for v1alpha1 — surfaced here so
	// reconciler can set it correctly and Phase 3+ can
	// rely on the field existing).
	Generation int64
}
