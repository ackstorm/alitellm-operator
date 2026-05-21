// SPDX-License-Identifier: Apache-2.0

// Package connection owns the manager-level LiteLLMConnection cache. It
// is the single, lock-free source of truth that every Phase 3+ domain
// reconciler reads before issuing any LiteLLM mutation call.
//
// # Scope of this skeleton
//
// This package SHIPS ONLY the contract types other plans depend on:
//
// - ConnectionSnapshot — immutable value struct returned by Snapshot.
// - ConnectionCache — the read API every dependent reconciler accepts.
//
// No concrete Cache type, no NewCache constructor, no probe loop, no
// Source.Channel wiring. lands all of that.
//
// # Load-bearing decisions
//
// - D-01 — Package separation. internal/connection/ is distinct from
// internal/controller/ (reconciler) and internal/litellm/ (wire-shape
// REST client). Every Phase 3+ domain reconciler imports this package
// for the Snapshot API; no other package owns the cache.
//
// - D-02 — Snapshot returns a VALUE (not a pointer). Callers cannot
// mutate cached state through the returned snapshot. The embedded
// *litellm.Client pointer is shared and safe because the Client is
// read-only after construction (per D-03: the cache rebuilds the
// entire Client on every probe; there is no in-place mutation).
//
// - D-04 — Zero-value ConnectionSnapshot{} has Ready=false and
// Client=nil. This is the universal "do not mutate" signal — whether
// the CR is absent, deleted, mid-probe (Connecting), Unreachable,
// BadMasterKey, or SecretNotFound. Dependents need only check
// snap.Ready; the Reason field carries the underlying cause for
// their Ready=False, reason=LiteLLMUnavailable propagation
// (§6.0 dependent rule).
//
// - D-12 — Shared interface. ConnectionCache is implemented by BOTH
// the real *Cache AND Phase 1's
// *controller.FakeConnectionCache (after adds Snapshot
// to the fake). Every Phase 3+ domain reconciler accepts this
// interface — NEVER the concrete type — so the 4 Phase 1 envtests
// (fastpath_test.go, idempotency_test.go, idempotency_long_test.go,
// metrics_scrape_test.go) keep working when swaps the
// real cache into cmd/main.go.
package connection
