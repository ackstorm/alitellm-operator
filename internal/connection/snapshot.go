// SPDX-License-Identifier: Apache-2.0

package connection

import (
	"time"

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

	// MCPToolPrefixSeparator mirrors LiteLLM's `MCP_TOOL_PREFIX_SEPARATOR`
	// env var on the target instance (LiteLLMConnection.spec.
	// mcpToolPrefixSeparator). Carried on the snapshot so MCPServer and
	// MCPServerDiscovery reconcilers can sanitize wire-side server_name +
	// alias without re-reading the Connection CR. Valid values: "." or
	// "-"; empty string is treated as the operator-side default ("." per
	// FIX2.txt HIGH-1, 2026-05-22).
	MCPToolPrefixSeparator string

	// RequeueOnRejectedAfter controls how often each dependent reconciler
	// retries CRs that hit a deterministic upstream error (LiteLLMRejected,
	// SecretNotFound). Surfaced from LiteLLMConnection.spec.
	// requeueOnRejectedAfter (default 5m, range [1m, 1h] enforced via CEL
	// on the spec field). FIX2.txt HIGH-2 (2026-05-22).
	//
	// Dependents pattern at every deterministic-error return site:
	//   return ctrl.Result{RequeueAfter: snap.RequeueOnRejectedAfter}, nil
	//
	// Zero value (when no Connection has loaded yet OR snapshot is the
	// not-Ready zero value) means callers fall back to
	// DefaultRequeueOnRejectedAfter via NormalizedRequeueOnRejectedAfter.
	RequeueOnRejectedAfter time.Duration
}

// DefaultRequeueOnRejectedAfter is the fallback retry cadence applied
// when a snapshot's RequeueOnRejectedAfter is zero — i.e. the operator
// has not yet loaded a Connection CR, or the dependent is acting on the
// zero-value snapshot returned for not-Ready cases. Matches the
// kubebuilder default of "5m" on
// LiteLLMConnection.spec.requeueOnRejectedAfter (FIX2.txt H-2).
const (
	DefaultRequeueOnRejectedAfter = 5 * time.Minute
	MinRequeueOnRejectedAfter     = 1 * time.Minute
	MaxRequeueOnRejectedAfter     = 1 * time.Hour
)

// NormalizedRequeueOnRejectedAfter returns RequeueOnRejectedAfter
// clamped to [MinRequeueOnRejectedAfter, MaxRequeueOnRejectedAfter].
// Zero or non-positive values resolve to DefaultRequeueOnRejectedAfter.
// Used at every reconciler return site so the requeue cadence is
// always bounded, regardless of what the user set on the spec.
func (s ConnectionSnapshot) NormalizedRequeueOnRejectedAfter() time.Duration {
	d := s.RequeueOnRejectedAfter
	if d <= 0 {
		return DefaultRequeueOnRejectedAfter
	}
	if d < MinRequeueOnRejectedAfter {
		return MinRequeueOnRejectedAfter
	}
	if d > MaxRequeueOnRejectedAfter {
		return MaxRequeueOnRejectedAfter
	}
	return d
}
