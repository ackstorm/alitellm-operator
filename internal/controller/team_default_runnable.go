// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/ackstorm/alitellm-operator/internal/connection"
)

// TeamDefaultRunnable is the manager.Runnable that satisfies spec §7.4
// line 1313 (the synthetic `Team/default` reconcile contract) + the §7.6
// 30-min safety re-list for the implicit-default key. Mirrors
// ModelSafetyRelistRunnable structurally — the
// difference is that the Team variant has a single static key
// ({Namespace, "default"}) rather than enumerating CRs in the namespace.
//
// Lifecycle:
//
// 1. Start waits for connection.ConnectionCache.Snapshot.Ready to first
// become true. Polling happens at ReadyPollInterval (production 5s;
// tests 50ms) and honors ctx cancellation — no busy-loop, no goroutine
// leak.
// 2. After Ready, the runnable enqueues a synthetic
// reconcile.Request{NamespacedName: {Namespace, "default"}} onto
// RequeueCh. The K8s API server is NEVER asked to create a Team CR —
// this is purely an in-memory event that drives the TeamReconciler's
// NotFound-on-default bootstrap branch (see team_controller.go
// reconcileImplicitDefault).
// 3. The runnable then enters a time.Ticker loop at Interval (production
// 30m; tests 100ms). Each tick re-enqueues the same Request.
// 4. ctx.Done terminates the loop cleanly.
//
// Channel back-pressure is non-blocking: `select default` skips the tick
// if RequeueCh is full. The next tick retries — preserves liveness under
// a slow LiteLLM (T-06-03-01 mitigation).
//
// Leader election: manager.Add treats Runnables as leader-only by default
// (controller-runtime convention). Standby replicas never call Start —
// at-most-one-leader semantics hold (T-06-03-05 mitigation).
type TeamDefaultRunnable struct {
	// Cache is the connection cache the runnable polls for the
	// LiteLLMConnection/default Ready transition gate. Typed as the
	// interface (per D-12) so tests substitute *FakeConnectionCache.
	Cache connection.ConnectionCache

	// Namespace is the operator's WATCH_NAMESPACE — the synthetic
	// Request always targets {Namespace, "default"}.
	Namespace string

	// Interval is the safety re-list cadence (production 30m;
	// tests 100ms).
	Interval time.Duration

	// ReadyPollInterval is the cadence at which Start polls
	// Cache.Snapshot.Ready before the first enqueue. Defaults to 5s
	// when zero (defensive — production code MUST set it; tests use
	// 50*time.Millisecond).
	ReadyPollInterval time.Duration

	// Log receives Start lifecycle messages at V(1).
	Log logr.Logger

	// RequeueCh receives the synthetic reconcile.Request. The Team
	// reconciler's SetupWithManager wires this as a source.TypedFunc
	// watch (mirrors ModelReconciler.SetupWithManager(mgr,
	// safetyRelistCh)).
	RequeueCh chan<- reconcile.Request

	// tickCount is a test-observable counter incremented on every
	// enqueue (initial Ready-gated + per-tick). Zero allocation in
	// production; tests assert delta via TickCount.
	tickCount atomic.Int64
}

// TickCount returns the total number of enqueues this runnable has
// performed since Start was called. Used by Cadence_30MinReList envtests
// to assert the ticker fired the expected number of times even when
// hash-equal short-circuit prevents new MutationsByTeamAlias deltas.
func (r *TeamDefaultRunnable) TickCount() int64 {
	return r.tickCount.Load()
}

// Start implements manager.Runnable. Blocks until ctx is cancelled.
//
// Phase 1: poll Cache.Snapshot.Ready, honoring ctx cancellation.
// Phase 2: enqueue the synthetic Team/default reconcile.Request.
// Phase 3: enter a time.Ticker(Interval) loop, enqueuing on every tick.
func (r *TeamDefaultRunnable) Start(ctx context.Context) error {
	pollInterval := r.ReadyPollInterval
	if pollInterval <= 0 {
		// Defensive default. Production code in cmd/main.go sets 5s
		// explicitly; tests set 50ms.
		pollInterval = 5 * time.Second
	}

	r.Log.V(1).Info("TeamDefaultRunnable: waiting for LiteLLMConnection/default Ready",
		"pollInterval", pollInterval, "namespace", r.Namespace)

	// ─── Phase 1: wait for Cache.Snapshot.Ready ─────────────────────────
	for {
		if r.Cache != nil && r.Cache.Snapshot().Ready {
			break
		}
		select {
		case <-ctx.Done():
			r.Log.V(1).Info("TeamDefaultRunnable: ctx cancelled before Ready; exiting")
			return nil
		case <-time.After(pollInterval):
			// retry
		}
	}

	// ─── Phase 2: initial enqueue ─────────────────────────────────────────
	req := reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: r.Namespace, Name: "default"},
	}
	r.enqueue(req)
	r.Log.V(1).Info("TeamDefaultRunnable: connection Ready; initial Team/default reconcile enqueued",
		"namespace", r.Namespace)

	// ─── Phase 3: 30-min ticker loop ──────────────────────────────────────
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.Log.V(1).Info("TeamDefaultRunnable: ctx cancelled; exiting tick loop")
			return nil
		case <-ticker.C:
			r.enqueue(req)
		}
	}
}

// enqueue performs a non-blocking send on RequeueCh and increments the
// tick counter. Back-pressure (full channel) is logged at V(1) and
// silently skipped — the next tick will retry.
func (r *TeamDefaultRunnable) enqueue(req reconcile.Request) {
	select {
	case r.RequeueCh <- req:
		r.tickCount.Add(1)
	default:
		r.Log.V(1).Info("TeamDefaultRunnable: requeue channel full; skipping tick",
			"namespace", req.NamespacedName.Namespace)
	}
}
